package exact

import (
	"context"
	"testing"
	"time"

	"github.com/primaybr/liltok/internal/cache"
	"github.com/primaybr/liltok/internal/db"
)

func TestTieredStoreLifecycle(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	defer database.Close()

	store, err := NewTieredStore(database, 100)
	if err != nil {
		t.Fatalf("failed to create tiered store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	hash := "a1b2c3d4e5f67890"

	entry := &cache.CacheEntry{
		Hash:             hash,
		Model:            "gpt-4o",
		NormalizedPrompt: "test prompt",
		ResponsePayload:  []byte(`{"id":"chatcmpl-test","choices":[{"message":{"content":"cached response"}}]}`),
		PromptTokens:     10,
		CompletionTokens: 5,
		TTLSeconds:       60,
	}

	// 1. Initial Get should be a MISS
	_, ok, err := store.Get(ctx, hash)
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if ok {
		t.Errorf("expected cache miss on initial get")
	}

	// 2. Set entry
	if err := store.Set(ctx, entry); err != nil {
		t.Fatalf("set failed: %v", err)
	}

	// 3. Immediate Get should be a HIT from L1 memory
	hitEntry, ok, err := store.Get(ctx, hash)
	if err != nil {
		t.Fatalf("get hit failed: %v", err)
	}
	if !ok {
		t.Fatalf("expected cache hit after set")
	}
	if string(hitEntry.ResponsePayload) != string(entry.ResponsePayload) {
		t.Errorf("payload mismatch: %s", string(hitEntry.ResponsePayload))
	}

	// Allow async persistence worker to flush to L2 SQLite
	time.Sleep(50 * time.Millisecond)

	// 4. Evict from L1 memory to verify L2 fallback
	store.l1.Remove(hash)
	if _, inL1 := store.l1.Get(hash); inL1 {
		t.Errorf("expected hash to be removed from L1")
	}

	// 5. Get again should fetch from L2 SQLite and repopulate L1
	l2HitEntry, ok, err := store.Get(ctx, hash)
	if err != nil {
		t.Fatalf("get from L2 failed: %v", err)
	}
	if !ok {
		t.Fatalf("expected cache hit from L2 SQLite store")
	}
	if string(l2HitEntry.ResponsePayload) != string(entry.ResponsePayload) {
		t.Errorf("l2 payload mismatch: %s", string(l2HitEntry.ResponsePayload))
	}

	// Confirm it repopulated L1
	if _, inL1 := store.l1.Get(hash); !inL1 {
		t.Errorf("expected entry to be promoted back into L1 memory")
	}

	// 6. Test Purge
	deleted, err := store.Purge(ctx, "gpt-4o")
	if err != nil {
		t.Fatalf("purge failed: %v", err)
	}
	if deleted == 0 {
		t.Errorf("expected at least 1 row purged, got %d", deleted)
	}

	// 7. Verify it is now a MISS
	_, ok, err = store.Get(ctx, hash)
	if err != nil {
		t.Fatalf("get after purge failed: %v", err)
	}
	if ok {
		t.Errorf("expected miss after purge")
	}
}

func TestTTLExpiration(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	defer database.Close()

	store, err := NewTieredStore(database, 100)
	if err != nil {
		t.Fatalf("failed to create tiered store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	hash := "expire-hash"

	entry := &cache.CacheEntry{
		Hash:             hash,
		Model:            "gpt-4o",
		NormalizedPrompt: "test",
		ResponsePayload:  []byte(`{"res":"expired"}`),
		TTLSeconds:       1, // 1 second TTL
		CreatedAt:        time.Now().Add(-2 * time.Second), // Already expired
	}

	if err := store.Set(ctx, entry); err != nil {
		t.Fatalf("set failed: %v", err)
	}

	// Get should detect expiration and return miss
	_, ok, err := store.Get(ctx, hash)
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if ok {
		t.Errorf("expected expired entry to return cache miss")
	}
}

func TestHitCountSemantics(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	defer database.Close()

	store, err := NewTieredStore(database, 100)
	if err != nil {
		t.Fatalf("failed to create tiered store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	hash := "hitcount-test-hash"

	entry := &cache.CacheEntry{
		Hash:             hash,
		Model:            "claude-opus-5",
		NormalizedPrompt: "test prompt",
		ResponsePayload:  []byte(`{"response":"test"}`),
		TTLSeconds:       3600,
	}

	// 1. Initial Set (New Entry)
	if err := store.Set(ctx, entry); err != nil {
		t.Fatalf("set failed: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // flush async L2 write

	var hitCount int
	err = database.QueryRow("SELECT hit_count FROM cache_entries WHERE hash = ?", hash).Scan(&hitCount)
	if err != nil {
		t.Fatalf("query hit_count failed: %v", err)
	}
	if hitCount != 0 {
		t.Errorf("expected hit_count = 0 on initial creation, got %d", hitCount)
	}

	// 2. Duplicate Set (e.g. concurrent in-flight write) - MUST NOT increment hit_count
	if err := store.Set(ctx, entry); err != nil {
		t.Fatalf("duplicate set failed: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	err = database.QueryRow("SELECT hit_count FROM cache_entries WHERE hash = ?", hash).Scan(&hitCount)
	if err != nil {
		t.Fatalf("query hit_count failed: %v", err)
	}
	if hitCount != 0 {
		t.Errorf("expected hit_count to remain 0 after duplicate Set(), got %d", hitCount)
	}

	// 3. Cache HIT via Get - MUST increment hit_count to 1
	_, ok, err := store.Get(ctx, hash)
	if err != nil || !ok {
		t.Fatalf("expected cache hit on Get")
	}
	time.Sleep(50 * time.Millisecond)

	err = database.QueryRow("SELECT hit_count FROM cache_entries WHERE hash = ?", hash).Scan(&hitCount)
	if err != nil {
		t.Fatalf("query hit_count failed: %v", err)
	}
	if hitCount != 1 {
		t.Errorf("expected hit_count = 1 after first cache hit, got %d", hitCount)
	}

	// 4. Second Cache HIT via Get - MUST increment hit_count to 2
	_, ok, err = store.Get(ctx, hash)
	if err != nil || !ok {
		t.Fatalf("expected cache hit on 2nd Get")
	}
	time.Sleep(50 * time.Millisecond)

	err = database.QueryRow("SELECT hit_count FROM cache_entries WHERE hash = ?", hash).Scan(&hitCount)
	if err != nil {
		t.Fatalf("query hit_count failed: %v", err)
	}
	if hitCount != 2 {
		t.Errorf("expected hit_count = 2 after second cache hit, got %d", hitCount)
	}
}

