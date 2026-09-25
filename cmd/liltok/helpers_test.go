package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/db"
)

// testEnv is an isolated CLI workspace: a temp home directory, a temp config file, and a temp
// database path, so no test ever reads or writes the developer's real ~/.liltok.
type testEnv struct {
	home    string
	cfgPath string
	dbPath  string
}

// isolatedEnvVars are cleared for every CLI test because config.Load and the MCP client read them
// and a developer shell may have them set.
var isolatedEnvVars = []string{
	"LILTOK_HOST", "LILTOK_PORT", "LILTOK_DB_PATH", "LILTOK_LOG_LEVEL", "LILTOK_LOG_FORMAT",
	"LILTOK_API_KEY", "OPENAI_API_KEY", "OPENAI_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL",
	"NVIDIA_API_KEY", "GROQ_API_KEY", "GEMINI_API_KEY", "OPENROUTER_API_KEY",
	"LILTOK_SYNC_URL", "LILTOK_AUTO_SYNC", "LILTOK_CAPTURE_DIR",
	"LILTOK_MAINTAINER_MODE", "LILTOK_MAINTAINER_KEY_FILE", "LILTOK_MAINTAINER_PUBKEY",
}

// newTestEnv creates a temp workspace and writes a config whose db_path points inside it.
// extraYAML is appended to the generated config verbatim.
func newTestEnv(t *testing.T, extraYAML string) *testEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	for _, k := range isolatedEnvVars {
		t.Setenv(k, "")
	}

	env := &testEnv{
		home:    home,
		cfgPath: filepath.Join(home, "test-config.yaml"),
		// Forward slashes keep the YAML value and the path printed by commands identical.
		dbPath: filepath.ToSlash(filepath.Join(home, "data", "liltok.db")),
	}
	yaml := "storage:\n  db_path: '" + filepath.ToSlash(env.dbPath) + "'\nlog:\n  level: 'error'\n" + extraYAML
	if err := os.WriteFile(env.cfgPath, []byte(yaml), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return env
}

// run executes the CLI with --config pointing at the temp config and returns captured stdout.
func (e *testEnv) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return runCLI(t, append([]string{"--config", e.cfgPath}, args...)...)
}

// openDB opens the workspace database for fixture setup or assertions.
func (e *testEnv) openDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(e.dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return database
}

// insertEntry writes one cache_entries row directly and closes the database again, so the CLI
// command under test is the only open handle while it runs.
func (e *testEnv) insertEntry(t *testing.T, hash, model, prompt, payload string, hits int, semantic bool) {
	t.Helper()
	database := e.openDB(t)
	defer database.Close()
	isSem := 0
	if semantic {
		isSem = 1
	}
	_, err := database.Exec(`
		INSERT INTO cache_entries (hash, model, normalized_prompt, response_payload, hit_count, is_semantic)
		VALUES (?, ?, ?, ?, ?, ?)`, hash, model, prompt, []byte(payload), hits, isSem)
	if err != nil {
		t.Fatalf("insert cache entry: %v", err)
	}
}

// countEntries returns the number of cache_entries rows, optionally filtered by model.
func (e *testEnv) countEntries(t *testing.T, model string) int {
	t.Helper()
	database := e.openDB(t)
	defer database.Close()
	var n int
	var err error
	if model == "" {
		err = database.QueryRow("SELECT COUNT(*) FROM cache_entries").Scan(&n)
	} else {
		err = database.QueryRow("SELECT COUNT(*) FROM cache_entries WHERE model = ?", model).Scan(&n)
	}
	if err != nil {
		t.Fatalf("count entries: %v", err)
	}
	return n
}

// runCLI builds a fresh command tree, runs it with args, and returns everything the command
// printed to stdout. Cobra's own usage and error output is discarded.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCommand()
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	var runErr error
	out := captureStdout(t, func() { runErr = root.Execute() })
	return out, runErr
}

// captureStdout redirects os.Stdout while fn runs, because the commands print with fmt.Print*.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w

	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	defer func() {
		os.Stdout = orig
	}()
	fn()
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// withStdin replaces os.Stdin with a file holding input while fn runs.
func withStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin.txt")
	if err := os.WriteFile(path, []byte(input), 0644); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open stdin: %v", err)
	}
	defer f.Close()
	orig := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = orig }()
	fn()
}

// assertContains fails the test for each want substring missing from got.
func assertContains(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q\n--- output ---\n%s", w, got)
		}
	}
}

// fieldAfter returns the first whitespace-separated token after label on the line containing it.
func fieldAfter(t *testing.T, out, label string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if idx := strings.Index(line, label); idx >= 0 {
			fields := strings.Fields(line[idx+len(label):])
			if len(fields) > 0 {
				return fields[0]
			}
		}
	}
	t.Fatalf("label %q not found in output:\n%s", label, out)
	return ""
}
