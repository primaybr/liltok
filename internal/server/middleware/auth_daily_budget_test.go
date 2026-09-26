package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/primaybr/liltok/internal/ledger"
)

func TestNewAuth_DailyBudgetExceededFlagged(t *testing.T) {
	km := newKeyManager(t)
	ctx := context.Background()
	// No monthly cap, so only the daily cap can trip the flag.
	raw, key, err := km.CreateKeyWithOptions(ctx, ledger.KeyOptions{Name: "daily", DailyBudgetUSD: 1.0, RPM: 100, TPM: 100000})
	if err != nil {
		t.Fatalf("CreateKeyWithOptions: %v", err)
	}
	h := NewAuth(km, ledger.NewQuotaEnforcer())

	probe := func() authSeen {
		var seen authSeen
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set("Authorization", "Bearer "+raw)
		h(authProbe(&seen)).ServeHTTP(httptest.NewRecorder(), req)
		return seen
	}

	if err := km.UpdateSpend(ctx, key.ID, 0.4); err != nil {
		t.Fatalf("UpdateSpend: %v", err)
	}
	if seen := probe(); !seen.called || seen.budgetExceeded {
		t.Fatalf("key under its daily cap = %+v, want no budget flag", seen)
	}

	if err := km.UpdateSpend(ctx, key.ID, 0.6); err != nil {
		t.Fatalf("UpdateSpend: %v", err)
	}
	seen := probe()
	if !seen.called {
		t.Fatalf("over-daily-budget key must still reach the next handler (cache hits allowed)")
	}
	if !seen.budgetExceeded {
		t.Errorf("expected budget exceeded flag once the daily cap is reached")
	}
}
