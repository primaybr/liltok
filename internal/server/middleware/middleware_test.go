package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestIDMiddleware(t *testing.T) {
	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := GetRequestID(r.Context())
		if reqID == "" {
			t.Errorf("expected request ID in context, got empty")
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	respID := rec.Header().Get("X-Liltok-Request-Id")
	if respID == "" {
		t.Errorf("expected X-Liltok-Request-Id header in response")
	}
}

func TestRecoveryMiddleware(t *testing.T) {
	panicHandler := Recovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("simulated critical crash")
	}))

	req := httptest.NewRequest("GET", "/panic", nil)
	rec := httptest.NewRecorder()

	panicHandler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500 on panic, got %d", rec.Code)
	}

	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to unmarshal recovery error: %v", err)
	}

	if errResp.Error.Code != "LILTOK_INTERNAL_ERROR" {
		t.Errorf("expected error code LILTOK_INTERNAL_ERROR, got %s", errResp.Error.Code)
	}
}

func TestAuthMiddleware(t *testing.T) {
	testCases := []struct {
		name       string
		headers    map[string]string
		expectKey  string
		expectVirt bool
	}{
		{
			name:       "Standard Bearer Key",
			headers:    map[string]string{"Authorization": "Bearer sk-openai-12345"},
			expectKey:  "sk-openai-12345",
			expectVirt: false,
		},
		{
			name:       "Anthropic x-api-key",
			headers:    map[string]string{"x-api-key": "sk-ant-claude-999"},
			expectKey:  "sk-ant-claude-999",
			expectVirt: false,
		},
		{
			name:       "Liltok Virtual Key",
			headers:    map[string]string{"Authorization": "Bearer lt-live-team-key"},
			expectKey:  "lt-live-team-key",
			expectVirt: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			handler := Auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				key := GetAuthKey(r.Context())
				isVirt := IsVirtualToken(r.Context())

				if key != tc.expectKey {
					t.Errorf("expected key %s, got %s", tc.expectKey, key)
				}
				if isVirt != tc.expectVirt {
					t.Errorf("expected isVirt %v, got %v", tc.expectVirt, isVirt)
				}
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)
		})
	}
}
