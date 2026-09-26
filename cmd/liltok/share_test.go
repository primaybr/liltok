package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/miner"
	"github.com/primaybr/liltok/internal/share"
)

func countLine(label string, n int) string { return fmt.Sprintf("   %-22s %d", label+":", n) }

func shareReq(t *testing.T, msgs ...map[string]interface{}) string {
	t.Helper()
	b, err := json.Marshal(map[string]interface{}{"model": "claude-sonnet-5", "max_tokens": 4096, "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestShareScan(t *testing.T) {
	env := newTestEnv(t, "share:\n  deny_terms: ['acme-billing']\n")
	orig := currentDenyEnv
	currentDenyEnv = func() share.DenyEnv { return share.DenyEnv{} }
	t.Cleanup(func() { currentDenyEnv = orig })

	answer := `{"type":"message","content":[{"type":"text","text":"An answer."}],"stop_reason":"end_turn"}`
	u := func(c interface{}) map[string]interface{} {
		return map[string]interface{}{"role": "user", "content": c}
	}

	env.insertEntry(t, "clean", "claude-sonnet-5", shareReq(t, u("How do I reverse a slice in Go without allocating a new one?")), answer, 0, false)
	env.insertEntry(t, "clean-dup", "claude-opus-5-5", shareReq(t, u("how do I reverse a slice in Go  without allocating a new one?")), answer, 0, false)
	env.insertEntry(t, "email", "claude-sonnet-5", shareReq(t, u("Why does dev@corp.example.com not receive the reset email from our service?")), answer, 0, false)
	env.insertEntry(t, "deny", "claude-sonnet-5", shareReq(t, u("How does acme-billing retry failed invoices after a timeout?")), answer, 0, false)
	env.insertEntry(t, "tool", "claude-sonnet-5", shareReq(t, u([]interface{}{map[string]interface{}{"type": "tool_result", "tool_use_id": "t", "content": "ok"}})), answer, 0, false)
	// A mined answer to a curated prompt that would otherwise pass: pick one that survives Extract and the gate.
	strict := share.NewGate(share.GateConfig{})
	var minedHash, minedPrompt string
	for _, p := range miner.GetCuratedPrompts("all") {
		h, prompt, _ := minedCorpusEntry(t, p, "mined answer")
		if _, skip := share.Extract(share.ExtractInput{NormalizedPrompt: prompt, ResponsePayload: answer}); skip == "" && strict.Check(p.UserPrompt).Passed() {
			minedHash, minedPrompt = h, prompt
			break
		}
	}
	if minedHash == "" {
		t.Fatal("no curated prompt passes Extract and the gate")
	}
	env.insertEntry(t, minedHash, "gpt-4o", minedPrompt, answer, 0, false)

	out, err := env.run(t, "share", "scan")
	if err != nil {
		t.Fatalf("share scan: %v", err)
	}
	assertContains(t, out,
		"liltok Share Scan (nothing leaves this machine)",
		" Deny terms:              1 configured, 0 automatic",
		"Cache entries scanned:   6",
		countLine("skip_tool_turn", 1),
		countLine("skip_curated", 1),
		"Rejected by gate:        2  (gate v1)",
		countLine("pii", 1),
		countLine("deny_term", 1),
		"Unique candidates:       1",
		countLine("added", 1),
		"Pending review:          1",
	)
	if strings.Contains(out, "reverse a slice") {
		t.Error("scan output must not print question text")
	}
	if strings.Contains(out, "reset email") {
		t.Error("scan output must not print the rejected email question's text")
	}
	if strings.Contains(out, "retry failed invoices") {
		t.Error("scan output must not print the rejected deny-term question's text")
	}

	// A second scan finds the same candidate and changes nothing.
	out, err = env.run(t, "share", "scan")
	if err != nil {
		t.Fatalf("second share scan: %v", err)
	}
	assertContains(t, out, countLine("added", 0), countLine("dropped", 0), "Pending review:          1")
}

// TestShareScanMissingDatabase covers a storage.db_path that does not point at an existing file:
// that must be refused before db.Open, which would otherwise silently create and seed a new, empty
// database there and report zero pending candidates. This config's db_path is a separate path from
// newTestEnv's own (which TestShareScan already creates via insertEntry before scanning), so this
// test starts from a path nothing has ever opened.
func TestShareScanMissingDatabase(t *testing.T) {
	env := newTestEnv(t, "")
	missing := filepath.ToSlash(filepath.Join(env.home, "never-opened", "liltok.db"))
	cfg := "storage:\n  db_path: '" + missing + "'\nlog:\n  level: 'error'\n"
	if err := os.WriteFile(env.cfgPath, []byte(cfg), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := env.run(t, "share", "scan")
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("share scan: err = %v, want an error mentioning %q", err, missing)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Errorf("share scan must create nothing at %s; stat err = %v", missing, statErr)
	}
	if _, statErr := os.Stat(filepath.Dir(missing)); !os.IsNotExist(statErr) {
		t.Errorf("share scan must not even create the parent directory of %s; stat err = %v", missing, statErr)
	}
}

// TestShareScanEmptyDBPath covers an empty storage.db_path: it must be refused with a clear message
// rather than reaching os.Stat (which, given "", would error in a way that does not name the
// problem) or db.Open.
func TestShareScanEmptyDBPath(t *testing.T) {
	env := newTestEnv(t, "")
	cfg := "storage:\n  db_path: ''\nlog:\n  level: 'error'\n"
	if err := os.WriteFile(env.cfgPath, []byte(cfg), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := env.run(t, "share", "scan")
	if err == nil || !strings.Contains(err.Error(), "storage.db_path is empty") {
		t.Fatalf("share scan: err = %v, want an error saying storage.db_path is empty", err)
	}
}

// TestShareScanZeroByteDatabase covers a storage.db_path that names an existing but zero-byte file:
// db.Open would silently initialize and seed it as a brand-new database, exactly like a missing
// path would, so it must be refused the same way, and the file itself must be left untouched.
func TestShareScanZeroByteDatabase(t *testing.T) {
	env := newTestEnv(t, "")
	empty := filepath.ToSlash(filepath.Join(env.home, "empty-liltok.db"))
	if err := os.WriteFile(filepath.FromSlash(empty), nil, 0644); err != nil {
		t.Fatalf("create zero-byte database: %v", err)
	}
	cfg := "storage:\n  db_path: '" + empty + "'\nlog:\n  level: 'error'\n"
	if err := os.WriteFile(env.cfgPath, []byte(cfg), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := env.run(t, "share", "scan")
	if err == nil || !strings.Contains(err.Error(), empty) {
		t.Fatalf("share scan: err = %v, want an error mentioning %q", err, empty)
	}
	info, statErr := os.Stat(filepath.FromSlash(empty))
	if statErr != nil {
		t.Fatalf("stat %s: %v", empty, statErr)
	}
	if info.Size() != 0 {
		t.Errorf("share scan must not modify %s; size = %d, want 0", empty, info.Size())
	}
}
