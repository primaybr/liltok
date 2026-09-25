package ledger_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/ledger"
)

func setupTestDB(t *testing.T) *db.DB {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory db: %v", err)
	}
	return database
}

func TestKeyManagerLifecycle(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	km := ledger.NewKeyManager(database)
	ctx := context.Background()

	// 1. Create virtual key
	rawKey, keyObj, err := km.CreateKey(ctx, "dev-cursor", 25.00, 100, 50000)
	if err != nil {
		t.Fatalf("failed to create key: %v", err)
	}
	if len(rawKey) < 20 {
		t.Errorf("unexpected raw key format: %s", rawKey)
	}
	if keyObj.MonthlyBudgetUSD != 25.00 || keyObj.RPM != 100 {
		t.Errorf("unexpected key object properties: %+v", keyObj)
	}

	// 2. Validate key
	validated, err := km.ValidateKey(ctx, rawKey)
	if err != nil {
		t.Fatalf("failed to validate key: %v", err)
	}
	if validated.ID != keyObj.ID || !validated.IsActive {
		t.Errorf("mismatched validated key: %+v", validated)
	}

	// 3. List keys
	keys, err := km.ListKeys(ctx)
	if err != nil {
		t.Fatalf("failed to list keys: %v", err)
	}
	if len(keys) != 1 {
		t.Errorf("expected 1 key in list, got %d", len(keys))
	}

	// 4. Update spend
	err = km.UpdateSpend(ctx, keyObj.ID, 5.50)
	if err != nil {
		t.Fatalf("failed to update spend: %v", err)
	}
	updated, _ := km.ValidateKey(ctx, rawKey)
	if updated.CurrentSpendUSD != 5.50 {
		t.Errorf("expected spend $5.50, got %f", updated.CurrentSpendUSD)
	}

	// 5. Revoke key
	err = km.RevokeKey(ctx, keyObj.ID)
	if err != nil {
		t.Fatalf("failed to revoke key: %v", err)
	}
	_, err = km.ValidateKey(ctx, rawKey)
	if err == nil {
		t.Errorf("expected error validating revoked key")
	}
}

func TestQuotaEnforcer_RateLimits(t *testing.T) {
	qe := ledger.NewQuotaEnforcer()

	key := &ledger.APIKey{
		ID:  "key_test",
		RPM: 2,
		TPM: 100,
	}

	// First 2 requests should be allowed
	if allowed, _ := qe.CheckRateLimit(key, 10); !allowed {
		t.Errorf("expected req 1 allowed")
	}
	if allowed, _ := qe.CheckRateLimit(key, 10); !allowed {
		t.Errorf("expected req 2 allowed")
	}

	// 3rd request in same minute should be rejected (RPM)
	if allowed, reason := qe.CheckRateLimit(key, 10); allowed {
		t.Errorf("expected req 3 rejected by RPM limit, got allowed")
	} else if reason == "" {
		t.Errorf("expected rejection reason")
	}
}

func TestQuotaEnforcer_Budget(t *testing.T) {
	qe := ledger.NewQuotaEnforcer()

	withinBudget := &ledger.APIKey{
		ID:               "key_1",
		MonthlyBudgetUSD: 10.00,
		CurrentSpendUSD:  8.50,
	}
	if allowed, _ := qe.CheckBudget(withinBudget); !allowed {
		t.Errorf("expected key within budget to be allowed")
	}

	exceededBudget := &ledger.APIKey{
		ID:               "key_2",
		MonthlyBudgetUSD: 10.00,
		CurrentSpendUSD:  10.50,
	}
	if allowed, _ := qe.CheckBudget(exceededBudget); allowed {
		t.Errorf("expected key exceeding budget to be blocked for upstream")
	}
}

func TestLedger_RecordAndStats(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	km := ledger.NewKeyManager(database)
	rawKey, key, _ := km.CreateKey(context.Background(), "test-app", 50.00, 60, 100000)
	_ = rawKey

	led := ledger.NewLedger(database, km)
	defer led.Close()

	// Record an exact cache hit
	led.Record(&ledger.RequestLog{
		RequestID:        "req_hit_1",
		APIKeyID:         key.ID,
		Model:            "gpt-4o",
		Provider:         "cache-local",
		CacheStatus:      "HIT",
		CacheTier:        "TIER1_EXACT",
		PromptTokens:     1000,
		CompletionTokens: 200,
		CachedTokens:     0,
		LatencyMs:        1,
		CostUSD:          0.0,
		SavedUSD:         0.0045,
		StatusCode:       200,
	})

	// Record an upstream miss with cost
	led.Record(&ledger.RequestLog{
		RequestID:        "req_miss_1",
		APIKeyID:         key.ID,
		Model:            "gpt-4o",
		Provider:         "openai",
		CacheStatus:      "MISS",
		CacheTier:        "NONE",
		PromptTokens:     2000,
		CompletionTokens: 300,
		CachedTokens:     0,
		LatencyMs:        450,
		CostUSD:          0.008,
		SavedUSD:         0.0,
		StatusCode:       200,
	})

	// Record an upstream request that hit model KV-cache
	led.Record(&ledger.RequestLog{
		RequestID:        "req_prefix_1",
		APIKeyID:         key.ID,
		Model:            "claude-3-5-sonnet-20241022",
		Provider:         "anthropic",
		CacheStatus:      "MISS",
		CacheTier:        "TIER2_PREFIX",
		PromptTokens:     10000,
		CompletionTokens: 50,
		CachedTokens:     9000,
		LatencyMs:        650,
		CostUSD:          0.0035,
		SavedUSD:         0.0243,
		StatusCode:       200,
	})

	// Allow background worker to process queue
	time.Sleep(150 * time.Millisecond)

	stats, err := led.GetOverviewStats(context.Background())
	if err != nil {
		t.Fatalf("failed to get overview stats: %v", err)
	}

	if stats.TotalRequests != 3 {
		t.Errorf("expected 3 total requests, got %d", stats.TotalRequests)
	}
	if stats.LocalHits != 1 {
		t.Errorf("expected 1 local hit, got %d", stats.LocalHits)
	}
	if stats.ModelCacheHits != 1 {
		t.Errorf("expected 1 model cache hit, got %d", stats.ModelCacheHits)
	}
	if stats.TotalHits != 2 {
		t.Errorf("expected 2 total hits, got %d", stats.TotalHits)
	}
	if stats.Tier1ExactHits != 1 {
		t.Errorf("expected 1 tier1 exact hit, got %d", stats.Tier1ExactHits)
	}
	if stats.Tier2PrefixHits != 1 {
		t.Errorf("expected 1 tier2 prefix hit, got %d", stats.Tier2PrefixHits)
	}
	if stats.Misses != 1 {
		t.Errorf("expected 1 miss, got %d", stats.Misses)
	}
	if stats.ProviderCounts["openai"] != 1 {
		t.Errorf("expected 1 openai request, got %d", stats.ProviderCounts["openai"])
	}
	if stats.ProviderCounts["anthropic"] != 1 {
		t.Errorf("expected 1 anthropic request, got %d", stats.ProviderCounts["anthropic"])
	}
	if stats.ProviderCounts["cache-local"] != 1 {
		t.Errorf("expected 1 cache-local request, got %d", stats.ProviderCounts["cache-local"])
	}
	expectedHitRate := (2.0 / 3.0) * 100.0
	if stats.HitRatePercent < expectedHitRate-0.1 || stats.HitRatePercent > expectedHitRate+0.1 {
		t.Errorf("expected ~%.2f%% hit rate, got %f", expectedHitRate, stats.HitRatePercent)
	}
	expectedSaved := 0.0045 + 0.0243
	if stats.TotalSavedUSD < expectedSaved-0.0001 || stats.TotalSavedUSD > expectedSaved+0.0001 {
		t.Errorf("expected $%.4f saved, got %f", expectedSaved, stats.TotalSavedUSD)
	}
	expectedCost := 0.008 + 0.0035
	if stats.TotalCostUSD < expectedCost-0.0001 || stats.TotalCostUSD > expectedCost+0.0001 {
		t.Errorf("expected $%.4f cost, got %f", expectedCost, stats.TotalCostUSD)
	}

	// Verify key spend was updated
	updatedKey, _ := km.ValidateKey(context.Background(), rawKey)
	if updatedKey.CurrentSpendUSD < expectedCost-0.0001 || updatedKey.CurrentSpendUSD > expectedCost+0.0001 {
		t.Errorf("expected key current spend $%.4f, got %f", expectedCost, updatedKey.CurrentSpendUSD)
	}
}

// TestLedger_BatchedWritesPersistOnClose queues more records than one batch holds and checks that
// Close drains all of them and applies key spend after the batch commits.
func TestLedger_BatchedWritesPersistOnClose(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	ctx := context.Background()
	km := ledger.NewKeyManager(database)
	_, key, err := km.CreateKey(ctx, "batch-app", 50.00, 60, 100000)
	if err != nil {
		t.Fatal(err)
	}

	led := ledger.NewLedger(database, km)
	const total = 1000
	for i := 0; i < total; i++ {
		item := &ledger.RequestLog{
			RequestID:   fmt.Sprintf("req_batch_%d", i),
			Model:       "gpt-4o",
			Provider:    "cache-local",
			CacheStatus: "HIT",
			CacheTier:   "TIER1_EXACT",
			StatusCode:  200,
		}
		if i%250 == 0 {
			item.APIKeyID = key.ID
			item.CostUSD = 0.25
		}
		led.Record(item)
	}
	if err := led.Close(); err != nil {
		t.Fatal(err)
	}

	var rows int
	if err := database.QueryRow("SELECT COUNT(*) FROM request_logs WHERE request_id LIKE 'req_batch_%'").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != total {
		t.Fatalf("persisted %d request logs, want %d", rows, total)
	}

	keys, err := km.ListKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k.ID == key.ID && math.Abs(k.CurrentSpendUSD-1.00) > 1e-9 {
			t.Fatalf("key spend = %v, want 1.00 (4 records x 0.25)", k.CurrentSpendUSD)
		}
	}
}
