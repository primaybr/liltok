package ledger_test

import (
	"context"
	"testing"
	"time"

	"github.com/liltok/liltok/internal/db"
	"github.com/liltok/liltok/internal/ledger"
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

	// Allow background worker to process queue
	time.Sleep(150 * time.Millisecond)

	stats, err := led.GetOverviewStats(context.Background())
	if err != nil {
		t.Fatalf("failed to get overview stats: %v", err)
	}

	if stats.TotalRequests != 2 {
		t.Errorf("expected 2 total requests, got %d", stats.TotalRequests)
	}
	if stats.TotalHits != 1 {
		t.Errorf("expected 1 hit, got %d", stats.TotalHits)
	}
	if stats.HitRatePercent != 50.0 {
		t.Errorf("expected 50%% hit rate, got %f", stats.HitRatePercent)
	}
	if stats.TotalSavedUSD != 0.0045 {
		t.Errorf("expected $0.0045 saved, got %f", stats.TotalSavedUSD)
	}
	if stats.TotalCostUSD != 0.008 {
		t.Errorf("expected $0.008 cost, got %f", stats.TotalCostUSD)
	}

	// Verify key spend was updated
	updatedKey, _ := km.ValidateKey(context.Background(), rawKey)
	if updatedKey.CurrentSpendUSD != 0.008 {
		t.Errorf("expected key current spend $0.008, got %f", updatedKey.CurrentSpendUSD)
	}
}
