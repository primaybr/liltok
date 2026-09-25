package middleware

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/ledger"
)

// TestAuthChargesTPMByRequestSize checks that a virtual key's TPM limit is charged with the
// estimated prompt size (it used to be charged 1 per request) and that the body still reaches the
// next handler intact.
func TestAuthChargesTPMByRequestSize(t *testing.T) {
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	km := ledger.NewKeyManager(database)
	rawKey, _, err := km.CreateKey(context.Background(), "tpm-test", 0, 100, 1000)
	if err != nil {
		t.Fatal(err)
	}

	var seen []string
	handler := NewAuth(km, ledger.NewQuotaEnforcer())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, string(body))
		w.WriteHeader(http.StatusOK)
	}))
	send := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("x-api-key", rawKey)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	body := `{"messages":[{"role":"user","content":"` + strings.Repeat("x", 2400) + `"}]}` // about 600 tokens
	if code := send(body); code != http.StatusOK {
		t.Fatalf("first request returned %d, want 200", code)
	}
	if len(seen) != 1 || seen[0] != body {
		t.Fatal("the next handler must receive the full request body")
	}
	if code := send(body); code != http.StatusTooManyRequests {
		t.Fatalf("second ~600-token request against a 1000 TPM key returned %d, want 429", code)
	}
	if code := send(`{"messages":[]}`); code != http.StatusOK {
		t.Fatalf("a small request that still fits the remaining budget returned %d, want 200", code)
	}
}
