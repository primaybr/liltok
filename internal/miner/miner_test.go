package miner_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/miner"
)

func TestCacheMiner_MockExecution(t *testing.T) {
	// Mock free-tier provider server (Groq / NIM compatible)
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}

		resp := map[string]interface{}{
			"id":      "chatcmpl-mock-free-tier",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "llama-3.3-70b-versatile",
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"message": map[string]string{
						"role":    "assistant",
						"content": "To fix a nil pointer dereference in Go, check if the pointer or interface is nil before accessing its fields.",
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]int{
				"prompt_tokens":     45,
				"completion_tokens": 28,
				"total_tokens":      73,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open memory db: %v", err)
	}
	defer database.Close()
	_, _ = database.Exec("DELETE FROM cache_entries")

	cfg := miner.MinerConfig{
		Provider:     "groq",
		APIKey:       "gsk-test-key",
		BaseURL:      mockServer.URL,
		Model:        "llama-3.3-70b-versatile",
		Workers:      2,
		RateLimitRPM: 300, // fast for testing
		TargetModels: []string{"claude-3-5-sonnet-20241022", "gpt-4o"},
	}

	m := miner.NewCacheMiner(cfg, database, nil)

	prompts := []miner.PromptItem{
		{
			ID:           "test-nil-pointer",
			Category:     "errors",
			SystemPrompt: "You are a Go expert.",
			UserPrompt:   "How do I fix nil pointer dereference in Go?",
		},
		{
			ID:           "test-map-concurrency",
			Category:     "errors",
			SystemPrompt: "You are a Go expert.",
			UserPrompt:   "How do I fix concurrent map writes in Go?",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stats, err := m.MinePrompts(ctx, prompts)
	if err != nil {
		t.Fatalf("unexpected error mining prompts: %v", err)
	}

	if stats.Completed != 2 {
		t.Errorf("expected 2 completed prompts, got %d", stats.Completed)
	}
	if stats.Errors != 0 {
		t.Errorf("expected 0 errors, got %d", stats.Errors)
	}
	if stats.TokensGenerated != 146 { // 73 * 2
		t.Errorf("expected 146 tokens generated, got %d", stats.TokensGenerated)
	}

	// Verify entries in database: 2 prompts * 2 target models * 2 schemas (OpenAI & Anthropic) = 8 entries
	var count int
	_ = database.QueryRowContext(ctx, "SELECT COUNT(*) FROM cache_entries").Scan(&count)
	if count < 4 {
		t.Errorf("expected at least 4 cache entries, got %d", count)
	}

	// Verify Export & Import round-trip
	var buf bytes.Buffer
	exportedCount, err := miner.ExportCacheToGz(database, &buf)
	if err != nil {
		t.Fatalf("failed to export cache: %v", err)
	}
	if exportedCount != count {
		t.Errorf("exported count %d != db count %d", exportedCount, count)
	}

	// Import into clean database
	cleanDBPath := filepath.Join(t.TempDir(), "clean.db")
	cleanDB, err := db.Open(cleanDBPath)
	if err != nil {
		t.Fatalf("failed to open second memory db: %v", err)
	}
	defer cleanDB.Close()
	_, _ = cleanDB.Exec("DELETE FROM cache_entries")

	importedCount, err := miner.ImportCacheFromGz(cleanDB, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("failed to import cache: %v", err)
	}
	if importedCount != int(stats.CacheEntries) {
		t.Errorf("imported count %d != mined count %d", importedCount, stats.CacheEntries)
	}

	var totalCleanCount int
	_ = cleanDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM cache_entries").Scan(&totalCleanCount)
	if totalCleanCount != count {
		t.Errorf("totalCleanCount %d != count %d", totalCleanCount, count)
	}

	// Second import should be idempotent (0 new entries due to INSERT OR IGNORE)
	idempotentCount, err := miner.ImportCacheFromGz(cleanDB, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("second import failed: %v", err)
	}
	if idempotentCount != 0 {
		t.Errorf("expected 0 new entries on second import, got %d", idempotentCount)
	}
}

func TestPackStarterCache(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	database, err := db.Open(filepath.Join(tempDir, "test.db"))
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	defer database.Close()

	// A mined answer to a curated prompt, and a user request carrying a home path and an API key.
	mined := minedEntry(t, testPrompts[0], "gpt-4o", true, "Use slices.Reverse(s).")
	insertEntry(t, database, mined.Hash, mined.Model, mined.NormalizedPrompt, mined.ResponsePayload, 1, false)
	insertEntry(t, database, "hash-user-traffic", "gpt-4o",
		`{"messages":[{"content":"Read C:\\Users\\Developer\\secret.txt with key sk-123456789012345678901234","role":"user"}],"model":"gpt-4o"}`,
		`{"choices":[{"message":{"content":"Found key sk-987654321098765432109876"}}]}`, 1, false)

	targetGz := filepath.Join(tempDir, "out_starter.json.gz")
	res, err := miner.PackStarterCache(database, targetGz, miner.PackOptions{Prompts: testPrompts})
	if err != nil {
		t.Fatalf("PackStarterCache failed: %v", err)
	}
	if res.TotalEntries != 1 || res.MergedFromDB != 1 {
		t.Errorf("expected only the mined entry packed, got %+v", res)
	}
	if res.Rejected != 1 || res.RejectReasons[miner.RejectNotCurated] != 1 {
		t.Errorf("expected the user entry rejected as not curated, got %+v", res)
	}

	data, err := os.ReadFile(targetGz)
	if err != nil {
		t.Fatalf("failed to read targetGz: %v", err)
	}
	items := decodeGz(t, data)
	if len(items) != 1 || items[0].Hash != mined.Hash {
		t.Fatalf("packed items = %+v, want only the mined entry", items)
	}
	for _, leak := range []string{"Developer", "sk-123456789012345678901234", "hash-user-traffic"} {
		if strings.Contains(items[0].NormalizedPrompt+items[0].ResponsePayload, leak) {
			t.Errorf("packed archive contains %q from user traffic", leak)
		}
	}
}

func TestDefaultTargetModels_MatchClientModelIDs(t *testing.T) {
	models := miner.DefaultTargetModels()
	want := map[string]bool{"claude-opus-5-5": false, "claude-haiku-4-5-20251001": false}
	for _, m := range models {
		if m == "claude-opus-5" || m == "claude-haiku-4-5" {
			t.Errorf("default targets contain %q, which Claude Code does not send", m)
		}
		if _, ok := want[m]; ok {
			want[m] = true
		}
	}
	for m, found := range want {
		if !found {
			t.Errorf("default targets missing %q", m)
		}
	}

	// Callers append to the result; each call must return an independent slice.
	models[0] = "mutated"
	if miner.DefaultTargetModels()[0] == "mutated" {
		t.Error("DefaultTargetModels returned a shared slice")
	}
}
