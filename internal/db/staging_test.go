package db

import (
	"context"
	"testing"
)

func TestStagedEntriesLifecycle(t *testing.T) {
	database, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer database.Close()

	items := []StarterCacheItem{
		{
			Hash:             "hash_test_1",
			Model:            "gpt-4o",
			NormalizedPrompt: "What is binary search?",
			ResponsePayload:  "Binary search is O(log n)...",
			PromptTokens:     10,
			CompletionTokens: 20,
			TTLSeconds:       86400,
		},
		{
			Hash:             "hash_test_2",
			Model:            "claude-3-5-sonnet",
			NormalizedPrompt: "Explain SQLite WAL mode",
			ResponsePayload:  "SQLite WAL mode allows concurrent readers...",
			PromptTokens:     15,
			CompletionTokens: 35,
			TTLSeconds:       86400,
		},
	}

	inserted, dups, err := database.InsertStagedEntries(items, "submission1.enc")
	if err != nil {
		t.Fatalf("InsertStagedEntries failed: %v", err)
	}
	if inserted != 2 || dups != 0 {
		t.Fatalf("expected 2 inserted, 0 dups, got %d / %d", inserted, dups)
	}

	entries, err := database.GetStagedEntries("pending", 10, 0)
	if err != nil {
		t.Fatalf("GetStagedEntries failed: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 staged entries, got %d", len(entries))
	}

	// Approve entry 1
	ctx := context.Background()
	merged, err := database.ApproveStagedEntries(ctx, []int64{entries[0].ID})
	if err != nil {
		t.Fatalf("ApproveStagedEntries failed: %v", err)
	}
	if merged != 1 {
		t.Fatalf("expected 1 merged, got %d", merged)
	}

	// Verify entry 1 is in cache_entries
	var cacheCount int
	_ = database.QueryRow("SELECT COUNT(*) FROM cache_entries WHERE hash = ?", entries[0].Hash).Scan(&cacheCount)
	if cacheCount != 1 {
		t.Fatalf("expected hash_test_1 in cache_entries, got %d", cacheCount)
	}

	// Reject entry 2
	err = database.RejectStagedEntries(ctx, []int64{entries[1].ID})
	if err != nil {
		t.Fatalf("RejectStagedEntries failed: %v", err)
	}

	pending, approved, rejected, err := database.CountStagedEntries()
	if err != nil {
		t.Fatalf("CountStagedEntries failed: %v", err)
	}
	if pending != 0 || approved != 1 || rejected != 1 {
		t.Fatalf("expected 0 pending, 1 approved, 1 rejected, got %d/%d/%d", pending, approved, rejected)
	}
}
