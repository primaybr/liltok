package miner_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	dbPath := filepath.Join(tempDir, "test.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	defer database.Close()

	// Clear default seeded entries for test isolation
	_, _ = database.Exec("DELETE FROM cache_entries")

	// Insert an entry containing sensitive home path and api key
	_, err = database.Exec(`
		INSERT INTO cache_entries (
			hash, model, normalized_prompt, response_payload,
			prompt_tokens, completion_tokens, hit_count,
			created_at, last_accessed_at, ttl_seconds, is_pinned, is_semantic
		) VALUES (
			'hash-test-pack-1', 'gpt-4o',
			'{"messages":[{"content":"Read C:\\Users\\Developer\\secret.txt with key sk-123456789012345678901234","role":"user"}]}',
			'{"choices":[{"message":{"content":"Found key sk-987654321098765432109876 in C:\\Users\\Developer\\secret.txt"}}]}',
			20, 20, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 3600, 0, 0
		)
	`)
	if err != nil {
		t.Fatalf("failed to insert test entry: %v", err)
	}

	targetGz := filepath.Join(tempDir, "out_starter.json.gz")
	res, err := miner.PackStarterCache(database, targetGz, miner.PackOptions{
		MinHits:  0,
		Sanitize: true,
	})
	if err != nil {
		t.Fatalf("PackStarterCache failed: %v", err)
	}

	if res.TotalEntries != 1 {
		t.Errorf("expected 1 total entry, got %d", res.TotalEntries)
	}
	if res.MergedFromDB != 1 {
		t.Errorf("expected 1 merged entry, got %d", res.MergedFromDB)
	}
	if res.SizeBytes <= 0 {
		t.Errorf("expected non-zero size, got %d", res.SizeBytes)
	}

	// Verify imported sanitized content
	verifyDB, err := db.Open(filepath.Join(tempDir, "verify.db"))
	if err != nil {
		t.Fatalf("failed to open verify db: %v", err)
	}
	defer verifyDB.Close()

	data, err := os.ReadFile(targetGz)
	if err != nil {
		t.Fatalf("failed to read targetGz: %v", err)
	}

	imported, err := miner.ImportCacheFromGz(verifyDB, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("failed to import packed cache: %v", err)
	}
	if imported != 1 {
		t.Errorf("expected 1 imported entry, got %d", imported)
	}

	var prompt, payload string
	err = verifyDB.QueryRow("SELECT normalized_prompt, response_payload FROM cache_entries WHERE hash='hash-test-pack-1'").Scan(&prompt, &payload)
	if err != nil {
		t.Fatalf("failed to query imported entry: %v", err)
	}

	if bytes.Contains([]byte(prompt), []byte("Developer")) {
		t.Errorf("prompt was not sanitized, found 'Developer': %s", prompt)
	}
	if bytes.Contains([]byte(prompt), []byte("sk-123456789012345678901234")) {
		t.Errorf("prompt was not sanitized, found raw API key: %s", prompt)
	}
	if bytes.Contains([]byte(payload), []byte("Developer")) {
		t.Errorf("payload was not sanitized, found 'Developer': %s", payload)
	}
}

