package ledger

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/primaybr/liltok/internal/db"
)

// keyManagerAt returns a KeyManager on a fresh database whose clock reads *now.
func keyManagerAt(t *testing.T, start time.Time) (*KeyManager, *time.Time) {
	t.Helper()
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	database, err := db.Open(filepath.Join(t.TempDir(), "budget.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	km := NewKeyManager(database)
	now := start
	km.now = func() time.Time { return now }
	return km, &now
}

type storedSpend struct {
	monthly, daily float64
	month, day     string
}

func readSpend(t *testing.T, km *KeyManager, id string) storedSpend {
	t.Helper()
	var s storedSpend
	if err := km.db.QueryRow(`SELECT current_spend_usd, spend_month, daily_spend_usd, daily_spend_date FROM api_keys WHERE id = ?`, id).
		Scan(&s.monthly, &s.month, &s.daily, &s.day); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCreateKeyWithOptionsStoresDailyBudget(t *testing.T) {
	km, _ := keyManagerAt(t, time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()

	raw, key, err := km.CreateKeyWithOptions(ctx, KeyOptions{Name: "daily", MonthlyBudgetUSD: 20, DailyBudgetUSD: 2.5})
	if err != nil {
		t.Fatal(err)
	}
	if key.DailyBudgetUSD != 2.5 || key.SpendMonth != "2026-09" || key.RPM != 60 || key.TPM != 100000 {
		t.Fatalf("created key = %+v", key)
	}
	got, err := km.ValidateKey(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.DailyBudgetUSD != 2.5 || got.MonthlyBudgetUSD != 20 || got.DailySpendUSD != 0 {
		t.Fatalf("validated key = %+v", got)
	}

	_, neg, err := km.CreateKeyWithOptions(ctx, KeyOptions{Name: "neg", MonthlyBudgetUSD: -1, DailyBudgetUSD: -1})
	if err != nil {
		t.Fatal(err)
	}
	if neg.DailyBudgetUSD != 0 || neg.MonthlyBudgetUSD != 0 {
		t.Fatalf("negative budgets must clamp to 0, got %+v", neg)
	}
}

func TestUpdateSpendDailyRolloverAtUTCMidnight(t *testing.T) {
	// 23:30 UTC on Sept 25 is already Sept 26 in UTC+7, so the day must follow UTC, not local time.
	km, now := keyManagerAt(t, time.Date(2026, 9, 25, 23, 30, 0, 0, time.FixedZone("UTC+7", 7*3600)).UTC())
	ctx := context.Background()
	_, key, err := km.CreateKeyWithOptions(ctx, KeyOptions{Name: "k", DailyBudgetUSD: 5})
	if err != nil {
		t.Fatal(err)
	}

	for _, amt := range []float64{1.25, 0.75} {
		if err := km.UpdateSpend(ctx, key.ID, amt); err != nil {
			t.Fatal(err)
		}
	}
	if s := readSpend(t, km, key.ID); s.daily != 2.0 || s.day != "2026-09-25" || s.monthly != 2.0 {
		t.Fatalf("before midnight = %+v, want 2.0 on 2026-09-25", s)
	}

	*now = time.Date(2026, 9, 26, 0, 0, 1, 0, time.UTC)
	if err := km.UpdateSpend(ctx, key.ID, 0.5); err != nil {
		t.Fatal(err)
	}
	s := readSpend(t, km, key.ID)
	if s.daily != 0.5 || s.day != "2026-09-26" {
		t.Fatalf("after midnight daily = %v on %s, want 0.5 on 2026-09-26", s.daily, s.day)
	}
	if s.monthly != 2.5 || s.month != "2026-09" {
		t.Fatalf("monthly spend must keep accumulating within the month, got %v in %s", s.monthly, s.month)
	}

	// Zero or negative amounts are ignored.
	if err := km.UpdateSpend(ctx, key.ID, 0); err != nil {
		t.Fatal(err)
	}
	if got := readSpend(t, km, key.ID); got != s {
		t.Fatalf("zero spend changed the row: %+v -> %+v", s, got)
	}
}

func TestUpdateSpendMonthlyRollover(t *testing.T) {
	km, now := keyManagerAt(t, time.Date(2026, 9, 30, 23, 59, 0, 0, time.UTC))
	ctx := context.Background()
	raw, key, err := km.CreateKey(ctx, "monthly", 10, 60, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := km.UpdateSpend(ctx, key.ID, 10); err != nil {
		t.Fatal(err)
	}
	qe, qnow := enforcerAt(*now)
	k, err := km.ValidateKey(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if ok, reason := qe.CheckBudget(k); ok || !strings.Contains(reason, "monthly") {
		t.Fatalf("key at its monthly cap = %v %q, want a monthly rejection", ok, reason)
	}

	// A new UTC month lifts the cap before any new spend is recorded...
	*now = time.Date(2026, 10, 1, 0, 0, 30, 0, time.UTC)
	*qnow = *now
	k, err = km.ValidateKey(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if k.CurrentSpendUSD != 0 || k.SpendMonth != "2026-10" {
		t.Fatalf("stale month read as spend %v in %s, want 0 in 2026-10", k.CurrentSpendUSD, k.SpendMonth)
	}
	if ok, reason := qe.CheckBudget(k); !ok {
		t.Fatalf("new month must lift the monthly cap, got %q", reason)
	}

	// ...and the first spend of the month restarts the counter.
	if err := km.UpdateSpend(ctx, key.ID, 1.5); err != nil {
		t.Fatal(err)
	}
	if s := readSpend(t, km, key.ID); s.monthly != 1.5 || s.month != "2026-10" {
		t.Fatalf("after rollover = %+v, want 1.5 in 2026-10", s)
	}
}

func TestListKeysReportsCurrentPeriodSpend(t *testing.T) {
	km, now := keyManagerAt(t, time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC))
	ctx := context.Background()
	_, key, err := km.CreateKeyWithOptions(ctx, KeyOptions{Name: "list", DailyBudgetUSD: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := km.UpdateSpend(ctx, key.ID, 1); err != nil {
		t.Fatal(err)
	}
	keys, err := km.ListKeys(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListKeys = %v, %v", keys, err)
	}
	if k := keys[0]; k.DailySpendUSD != 1 || k.DailyBudgetUSD != 3 || k.DailySpendDate != "2026-09-25" {
		t.Fatalf("listed key = %+v", k)
	}

	*now = now.Add(24 * time.Hour)
	keys, err = km.ListKeys(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListKeys = %v, %v", keys, err)
	}
	if k := keys[0]; k.DailySpendUSD != 0 || k.DailySpendDate != "2026-09-26" || k.CurrentSpendUSD != 1 {
		t.Fatalf("next day listed key = %+v, want no daily spend and monthly spend kept", k)
	}
}

func TestUpdateSpendConcurrentAddsAreNotLost(t *testing.T) {
	km, _ := keyManagerAt(t, time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC))
	ctx := context.Background()
	_, key, err := km.CreateKeyWithOptions(ctx, KeyOptions{Name: "race", DailyBudgetUSD: 100})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = km.UpdateSpend(ctx, key.ID, 0.25)
		}()
	}
	wg.Wait()
	if s := readSpend(t, km, key.ID); s.daily != 5 || s.monthly != 5 {
		t.Fatalf("after 20 x 0.25 = %+v, want 5 daily and monthly", s)
	}
}

func TestCheckBudgetDailyCap(t *testing.T) {
	at := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	qe, _ := enforcerAt(at)
	cases := []struct {
		name   string
		key    APIKey
		wantOK bool
	}{
		{"no daily cap", APIKey{DailySpendUSD: 50, DailySpendDate: "2026-09-25"}, true},
		{"under cap", APIKey{DailyBudgetUSD: 5, DailySpendUSD: 4.99, DailySpendDate: "2026-09-25"}, true},
		{"cap reached", APIKey{DailyBudgetUSD: 5, DailySpendUSD: 5, DailySpendDate: "2026-09-25"}, false},
		{"cap exceeded", APIKey{DailyBudgetUSD: 5, DailySpendUSD: 7, DailySpendDate: "2026-09-25"}, false},
		{"stale date ignored", APIKey{DailyBudgetUSD: 5, DailySpendUSD: 7, DailySpendDate: "2026-09-24"}, true},
		{"undated spend counts", APIKey{DailyBudgetUSD: 5, DailySpendUSD: 7}, false},
		{"monthly stale, daily ok", APIKey{MonthlyBudgetUSD: 10, CurrentSpendUSD: 20, SpendMonth: "2026-08", DailyBudgetUSD: 5, DailySpendUSD: 1, DailySpendDate: "2026-09-25"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := tc.key
			ok, reason := qe.CheckBudget(&key)
			if ok != tc.wantOK {
				t.Fatalf("CheckBudget = %v %q, want %v", ok, reason, tc.wantOK)
			}
			if !ok && !strings.Contains(reason, "daily budget") {
				t.Fatalf("reason = %q, want a daily budget message", reason)
			}
		})
	}
}
