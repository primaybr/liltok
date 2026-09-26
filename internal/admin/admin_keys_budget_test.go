package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

type listedKey struct {
	ID               string  `json:"id"`
	MonthlyBudgetUSD float64 `json:"monthly_budget_usd"`
	CurrentSpendUSD  float64 `json:"current_spend_usd"`
	DailyBudgetUSD   float64 `json:"daily_budget_usd"`
	DailySpendUSD    float64 `json:"daily_spend_usd"`
	DailySpendDate   string  `json:"daily_spend_date"`
}

func listKeys(t *testing.T, env *adminEnv) []listedKey {
	t.Helper()
	rec, _ := do(t, env.mux, "GET", "/api/v1/keys", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list keys status %d: %s", rec.Code, rec.Body.String())
	}
	var keys []listedKey
	if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
		t.Fatalf("decode key list: %v", err)
	}
	return keys
}

func TestAdminKeysDailyBudget(t *testing.T) {
	env := newAdminEnv(t)

	rec, out := do(t, env.mux, "POST", "/api/v1/keys", map[string]interface{}{
		"name": "daily-capped", "budget": 30, "daily_budget": 2.5, "rpm": 10, "tpm": 1000,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status %d: %s", rec.Code, rec.Body.String())
	}
	created, _ := out["key"].(map[string]interface{})
	if created["daily_budget_usd"] != 2.5 || created["monthly_budget_usd"] != float64(30) {
		t.Fatalf("created key = %v, want daily_budget_usd 2.5 and monthly_budget_usd 30", created)
	}
	id, _ := created["id"].(string)

	// Omitting daily_budget keeps the old request shape working and means no daily cap.
	if rec, _ := do(t, env.mux, "POST", "/api/v1/keys", map[string]interface{}{"name": "uncapped", "budget": 5}); rec.Code != http.StatusCreated {
		t.Fatalf("create without daily_budget status %d", rec.Code)
	}

	if err := env.keys.UpdateSpend(context.Background(), id, 1.25); err != nil {
		t.Fatal(err)
	}
	var found *listedKey
	keys := listKeys(t, env)
	for i := range keys {
		if keys[i].ID == id {
			found = &keys[i]
		} else if keys[i].DailyBudgetUSD != 0 {
			t.Errorf("key created without daily_budget has daily_budget_usd %v", keys[i].DailyBudgetUSD)
		}
	}
	if found == nil {
		t.Fatalf("created key %s missing from list %+v", id, keys)
	}
	if found.DailyBudgetUSD != 2.5 || found.DailySpendUSD != 1.25 || found.CurrentSpendUSD != 1.25 || found.DailySpendDate == "" {
		t.Fatalf("listed key = %+v, want daily budget 2.5 and 1.25 spent today", *found)
	}

	if rec, _ := do(t, env.mux, "POST", "/api/v1/logs/clear?reset_spends=true", nil); rec.Code != http.StatusOK {
		t.Fatalf("clear status %d", rec.Code)
	}
	for _, k := range listKeys(t, env) {
		if k.DailySpendUSD != 0 || k.CurrentSpendUSD != 0 {
			t.Errorf("reset_spends left spend on %s: %+v", k.ID, k)
		}
	}
}
