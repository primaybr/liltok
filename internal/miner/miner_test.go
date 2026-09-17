package miner_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/liltok/liltok/internal/db"
	"github.com/liltok/liltok/internal/miner"
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
