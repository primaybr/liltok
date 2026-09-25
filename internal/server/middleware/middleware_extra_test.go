package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/ledger"
	"github.com/primaybr/liltok/internal/telemetry"
)

// captureLog swaps the global logger for one writing JSON into a buffer.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := telemetry.Log
	telemetry.Log = telemetry.NewLogger("info", "json", &buf)
	t.Cleanup(func() { telemetry.Log = prev })
	return &buf
}

func newKeyManager(t *testing.T) *ledger.KeyManager {
	t.Helper()
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return ledger.NewKeyManager(database)
}

type authSeen struct {
	called         bool
	key, authType  string
	virtual        bool
	keyID          string
	budgetExceeded bool
}

func authProbe(seen *authSeen) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		*seen = authSeen{
			called:         true,
			key:            GetAuthKey(ctx),
			authType:       GetAuthType(ctx),
			virtual:        IsVirtualToken(ctx),
			keyID:          GetAPIKeyID(ctx),
			budgetExceeded: IsBudgetExceeded(ctx),
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func TestAuth_HeaderParsing(t *testing.T) {
	tests := []struct {
		name         string
		headers      map[string]string
		wantKey      string
		wantAuthType string
	}{
		{"no headers", nil, "", ""},
		{"lowercase bearer with padding", map[string]string{"Authorization": "bearer   test-key  "}, "test-key", "bearer"},
		{"non-bearer scheme falls back to x-api-key", map[string]string{"Authorization": "Basic abc", "x-api-key": " test-anthropic "}, "test-anthropic", "anthropic"},
		{"bearer wins over x-api-key", map[string]string{"Authorization": "Bearer test-bearer", "x-api-key": "test-anthropic"}, "test-bearer", "bearer"},
		{"malformed authorization only", map[string]string{"Authorization": "Bearer"}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen authSeen
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			Auth(authProbe(&seen)).ServeHTTP(rec, req)

			if !seen.called || rec.Code != http.StatusNoContent {
				t.Fatalf("next handler not reached, code %d", rec.Code)
			}
			if seen.key != tt.wantKey || seen.authType != tt.wantAuthType {
				t.Errorf("got key %q type %q, want %q %q", seen.key, seen.authType, tt.wantKey, tt.wantAuthType)
			}
			if seen.virtual || seen.keyID != "" || seen.budgetExceeded {
				t.Errorf("non-virtual request must not carry virtual key state: %+v", seen)
			}
		})
	}
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q, want application/json", ct)
	}
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid error body %q: %v", rec.Body.String(), err)
	}
	return resp
}

func TestNewAuth_InvalidVirtualKeyRejected(t *testing.T) {
	km := newKeyManager(t)
	var seen authSeen
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer lt-live-unknown")
	rec := httptest.NewRecorder()
	NewAuth(km, ledger.NewQuotaEnforcer())(authProbe(&seen)).ServeHTTP(rec, req)

	if seen.called {
		t.Fatalf("next handler must not run for an invalid virtual key")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	resp := decodeError(t, rec)
	if resp.Error.Code != "invalid_api_key" || resp.Error.Type != "authentication_error" {
		t.Errorf("unexpected error body: %+v", resp.Error)
	}
}

func TestNewAuth_ValidVirtualKeyAndRateLimit(t *testing.T) {
	km := newKeyManager(t)
	raw, key, err := km.CreateKey(context.Background(), "test", 0, 1, 1000)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	h := NewAuth(km, ledger.NewQuotaEnforcer())

	var seen authSeen
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("x-api-key", raw)
	rec := httptest.NewRecorder()
	h(authProbe(&seen)).ServeHTTP(rec, req)
	if !seen.called {
		t.Fatalf("valid key must reach the next handler, code %d body %s", rec.Code, rec.Body.String())
	}
	if !seen.virtual || seen.keyID != key.ID || seen.authType != "anthropic" || seen.budgetExceeded {
		t.Errorf("unexpected context state: %+v", seen)
	}

	// RPM limit is 1, so an immediate second request is throttled.
	seen = authSeen{}
	rec = httptest.NewRecorder()
	h(authProbe(&seen)).ServeHTTP(rec, req)
	if seen.called {
		t.Fatalf("rate limited request must not reach the next handler")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
	resp := decodeError(t, rec)
	if resp.Error.Code != "rate_limit_exceeded" || !strings.Contains(resp.Error.Message, "RPM") {
		t.Errorf("unexpected error body: %+v", resp.Error)
	}
}

func TestNewAuth_BudgetExceededFlagged(t *testing.T) {
	km := newKeyManager(t)
	ctx := context.Background()
	raw, key, err := km.CreateKey(ctx, "test", 1.0, 100, 100000)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := km.UpdateSpend(ctx, key.ID, 2.0); err != nil {
		t.Fatalf("UpdateSpend: %v", err)
	}

	var seen authSeen
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	NewAuth(km, ledger.NewQuotaEnforcer())(authProbe(&seen)).ServeHTTP(rec, req)
	if !seen.called {
		t.Fatalf("over-budget key must still reach the next handler (cache hits allowed)")
	}
	if !seen.budgetExceeded {
		t.Errorf("expected budget exceeded flag in context")
	}
}

func TestNewAuth_NoQuotaEnforcer(t *testing.T) {
	km := newKeyManager(t)
	raw, key, err := km.CreateKey(context.Background(), "test", 0, 1, 1)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	h := NewAuth(km, nil)
	for i := 0; i < 3; i++ {
		var seen authSeen
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set("Authorization", "Bearer "+raw)
		rec := httptest.NewRecorder()
		h(authProbe(&seen)).ServeHTTP(rec, req)
		if !seen.called || seen.keyID != key.ID {
			t.Fatalf("request %d: without a quota enforcer no limit applies, got %+v code %d", i, seen, rec.Code)
		}
	}
}

func TestContextGettersOnEmptyContext(t *testing.T) {
	ctx := context.Background()
	if GetAuthKey(ctx) != "" || GetAuthType(ctx) != "" || GetAPIKeyID(ctx) != "" || GetRequestID(ctx) != "" {
		t.Errorf("string getters must return empty on a bare context")
	}
	if IsVirtualToken(ctx) || IsBudgetExceeded(ctx) {
		t.Errorf("bool getters must return false on a bare context")
	}
}

func TestRequestID_ReusesIncomingHeader(t *testing.T) {
	var got string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = GetRequestID(r.Context())
		if _, ok := r.Context().Value(StartTimeKey).(interface{ IsZero() bool }); !ok {
			t.Errorf("expected start time in context")
		}
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Liltok-Request-Id", "req_fixed")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got != "req_fixed" || rec.Header().Get("X-Liltok-Request-Id") != "req_fixed" {
		t.Errorf("incoming request ID must be reused, ctx %q header %q", got, rec.Header().Get("X-Liltok-Request-Id"))
	}
}

func TestGenerateRequestID_UniqueAndPrefixed(t *testing.T) {
	a, b := GenerateRequestID(), GenerateRequestID()
	if !strings.HasPrefix(a, "req_") || a == b {
		t.Errorf("expected unique req_ prefixed IDs, got %q and %q", a, b)
	}
}

func TestLogger_RecordsStatusAndBytes(t *testing.T) {
	buf := captureLog(t)
	h := RequestID(Logger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello"))
		_, _ = w.Write([]byte(" world"))
		w.(http.Flusher).Flush()
	})))
	req := httptest.NewRequest(http.MethodPut, "/some/path", nil)
	req.Header.Set("X-Liltok-Request-Id", "req_log")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot || rec.Body.String() != "hello world" {
		t.Errorf("response not passed through: %d %q", rec.Code, rec.Body.String())
	}
	if !rec.Flushed {
		t.Errorf("Flush must reach the underlying flusher")
	}

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatalf("expected one JSON log line, got %q: %v", buf.String(), err)
	}
	if entry["status"] != float64(http.StatusTeapot) || entry["bytes"] != float64(11) ||
		entry["method"] != http.MethodPut || entry["path"] != "/some/path" || entry["request_id"] != "req_log" {
		t.Errorf("unexpected log entry: %v", entry)
	}
}

func TestLogger_DefaultStatusOK(t *testing.T) {
	buf := captureLog(t)
	h := Logger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(buf.String(), `"status":200`) {
		t.Errorf("handler writing nothing must be logged as 200, got %q", buf.String())
	}
}

// plainWriter implements only http.ResponseWriter, not http.Flusher.
type plainWriter struct {
	h http.Header
}

func (p *plainWriter) Header() http.Header         { return p.h }
func (p *plainWriter) Write(b []byte) (int, error) { return len(b), nil }
func (p *plainWriter) WriteHeader(int)             {}

func TestResponseWriterWrapper_FlushWithoutFlusher(t *testing.T) {
	rw := &responseWriterWrapper{ResponseWriter: &plainWriter{h: http.Header{}}, statusCode: http.StatusOK}
	rw.Flush() // must not panic
	if n, err := rw.Write([]byte("abc")); n != 3 || err != nil || rw.bytesWritten != 3 {
		t.Errorf("write = %d %v, bytes %d", n, err, rw.bytesWritten)
	}
}

func TestRecovery_LogsPanicWithRequestID(t *testing.T) {
	buf := captureLog(t)
	h := RequestID(Recovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Liltok-Request-Id", "req_panic")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	resp := decodeError(t, rec)
	if !strings.Contains(resp.Error.Message, "boom") || resp.Error.Type != "internal_server_error" {
		t.Errorf("unexpected error body: %+v", resp.Error)
	}
	if !strings.Contains(buf.String(), "req_panic") || !strings.Contains(buf.String(), "boom") {
		t.Errorf("panic log must carry the request ID and panic value, got %q", buf.String())
	}
}

func TestRecovery_NoPanicPassesThrough(t *testing.T) {
	h := Recovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", rec.Code)
	}
}
