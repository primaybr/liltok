package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeGenerator is an OpenAI-compatible chat completions server that records every request.
type fakeGenerator struct {
	mu       sync.Mutex
	models   []string
	prompts  []string
	auth     []string
	srv      *httptest.Server
	response string
}

func newFakeGenerator(t *testing.T) *fakeGenerator {
	t.Helper()
	g := &fakeGenerator{response: `{"choices":[{"message":{"role":"assistant","content":"mined answer"}}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.mu.Lock()
		g.models = append(g.models, body.Model)
		if n := len(body.Messages); n > 0 {
			g.prompts = append(g.prompts, body.Messages[n-1].Content)
		}
		g.auth = append(g.auth, r.Header.Get("Authorization"))
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(g.response))
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func TestMineFromFileWithExport(t *testing.T) {
	gen := newFakeGenerator(t)
	env := newTestEnv(t, "")

	promptFile := filepath.Join(env.home, "prompts.txt")
	if err := os.WriteFile(promptFile, []byte("# comment\nhow do I reverse a slice in go?\n\nwhat is a mutex?\n"), 0644); err != nil {
		t.Fatal(err)
	}
	exportPath := filepath.Join(env.home, "mined.json.gz")

	out, err := env.run(t, "mine",
		"--provider", "ollama",
		"--base-url", gen.srv.URL+"/v1",
		"--model", "local-coder",
		"--file", promptFile,
		"--target-models", "gpt-4o, ,claude-x",
		"--rate-limit", "60000",
		"--workers", "1",
		"--export", exportPath,
	)
	if err != nil {
		t.Fatalf("mine: %v", err)
	}
	assertContains(t, out,
		"Loaded 2 prompts from file: "+promptFile,
		"Liltok Cache Miner (ollama)",
		"Generator Model: local-coder",
		"Target Models:   gpt-4o, claude-x",
		"Prompts Processed: 2 / 2",
		"New Cache Entries: 8",
		"Tokens Generated:  14",
		"Errors / Retries:  0",
		"Successfully exported 8 cache entries to "+exportPath,
	)

	gen.mu.Lock()
	defer gen.mu.Unlock()
	if len(gen.models) != 2 {
		t.Fatalf("generator calls = %d, want 2", len(gen.models))
	}
	for i, m := range gen.models {
		if m != "local-coder" {
			t.Errorf("call %d model = %q, want local-coder", i, m)
		}
		if gen.auth[i] != "" {
			t.Errorf("ollama call sent Authorization %q, want none", gen.auth[i])
		}
	}
	joined := strings.Join(gen.prompts, "|")
	if !strings.Contains(joined, "reverse a slice") || !strings.Contains(joined, "what is a mutex?") || strings.Contains(joined, "# comment") {
		t.Errorf("prompts sent = %v", gen.prompts)
	}

	if n := env.countEntries(t, "gpt-4o"); n != 4 {
		t.Errorf("gpt-4o entries = %d, want 4", n)
	}
	if n := env.countEntries(t, "claude-x"); n != 4 {
		t.Errorf("claude-x entries = %d, want 4", n)
	}
	if items := readPack(t, exportPath); len(items) != 8 {
		t.Errorf("exported pack has %d items, want 8", len(items))
	}
}

func TestMineWorkspaceUsesConfigKey(t *testing.T) {
	gen := newFakeGenerator(t)
	env := newTestEnv(t, "providers:\n  groq:\n    api_key: 'cfg-groq-key'\n")

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module example.com/x\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := env.run(t, "mine", "--base-url", gen.srv.URL+"/v1", "--workspace", ws,
		"--target-models", "gpt-4o", "-r", "60000", "-w", "2")
	if err != nil {
		t.Fatalf("mine: %v", err)
	}
	assertContains(t, out, "Synthesized 2 contextual prompts from workspace: "+ws, "Liltok Cache Miner (groq)", "Prompts Processed: 2 / 2")

	gen.mu.Lock()
	defer gen.mu.Unlock()
	if len(gen.auth) != 2 {
		t.Fatalf("generator calls = %d, want 2", len(gen.auth))
	}
	for _, a := range gen.auth {
		if a != "Bearer cfg-groq-key" {
			t.Errorf("Authorization = %q, want the configured groq key", a)
		}
	}
}

func TestMineProviderKeyFromEnv(t *testing.T) {
	cases := []struct {
		provider string
		envVar   string
	}{
		{"openrouter", "OPENROUTER_API_KEY"},
		{"nvidianim", "NVIDIA_API_KEY"},
		{"groq", "GROQ_API_KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			gen := newFakeGenerator(t)
			env := newTestEnv(t, "")
			t.Setenv(tc.envVar, "env-key-"+tc.provider)
			promptFile := filepath.Join(env.home, "p.txt")
			if err := os.WriteFile(promptFile, []byte("one prompt\n"), 0644); err != nil {
				t.Fatal(err)
			}

			out, err := env.run(t, "mine", "-p", tc.provider, "-u", gen.srv.URL+"/v1", "-m", "gen-model",
				"-f", promptFile, "--target-models", "gpt-4o", "-r", "60000", "-w", "1")
			if err != nil {
				t.Fatalf("mine: %v", err)
			}
			assertContains(t, out, "Prompts Processed: 1 / 1")
			gen.mu.Lock()
			defer gen.mu.Unlock()
			if len(gen.auth) != 1 || gen.auth[0] != "Bearer env-key-"+tc.provider {
				t.Errorf("Authorization = %v, want Bearer env-key-%s", gen.auth, tc.provider)
			}
		})
	}
}

func TestMineErrors(t *testing.T) {
	env := newTestEnv(t, "")

	out, err := env.run(t, "mine", "--provider", "groq")
	if err == nil || !strings.Contains(err.Error(), "missing API key for free provider groq") {
		t.Errorf("missing key: err = %v", err)
	}
	assertContains(t, out, "No API key found for groq")

	_, err = env.run(t, "mine", "-p", "ollama", "-f", filepath.Join(env.home, "missing.txt"))
	if err == nil || !strings.Contains(err.Error(), "failed to load prompts from file") {
		t.Errorf("missing prompt file: err = %v", err)
	}

	// An empty workspace yields no prompts, so the miner returns before any network call.
	out, err = env.run(t, "mine", "-p", "ollama", "-u", closedServerURL(t), "--workspace", t.TempDir())
	if err != nil {
		t.Fatalf("empty workspace: %v", err)
	}
	assertContains(t, out, "Synthesized 0 contextual prompts", "No prompts to mine.")
}

func TestMineGeneratorFailureCountsErrors(t *testing.T) {
	gen := newFakeGenerator(t)
	gen.response = `{"choices":[]}`
	env := newTestEnv(t, "")
	promptFile := filepath.Join(env.home, "p.txt")
	if err := os.WriteFile(promptFile, []byte("one prompt\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// An export path inside a missing directory fails after mining completes.
	badExport := filepath.Join(env.home, "no-dir", "out.json.gz")
	out, err := env.run(t, "mine", "-p", "ollama", "-u", gen.srv.URL+"/v1", "-f", promptFile,
		"--target-models", "gpt-4o", "-r", "60000", "-w", "1", "-e", badExport)
	if err == nil || !strings.Contains(err.Error(), "failed to create export file") {
		t.Errorf("bad export path: err = %v", err)
	}
	assertContains(t, out, "Prompts Processed: 0 / 1", "New Cache Entries: 0", "Errors / Retries:  1")
	if n := env.countEntries(t, ""); n != 0 {
		t.Errorf("failed generation stored %d entries", n)
	}
}
