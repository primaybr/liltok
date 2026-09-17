package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"
)

type contextKey string

const (
	RequestIDKey contextKey = "liltok_request_id"
	StartTimeKey contextKey = "liltok_start_time"
)

// GenerateRequestID creates a unique identifier for each intercepted request.
func GenerateRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("req_%x%s", time.Now().UnixMilli(), hex.EncodeToString(b))
}

// RequestID middleware injects a unique request ID into context and response headers.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Liltok-Request-Id")
		if reqID == "" {
			reqID = GenerateRequestID()
		}

		ctx := context.WithValue(r.Context(), RequestIDKey, reqID)
		ctx = context.WithValue(ctx, StartTimeKey, time.Now())

		w.Header().Set("X-Liltok-Request-Id", reqID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetRequestID extracts the request ID from context.
func GetRequestID(ctx context.Context) string {
	if val, ok := ctx.Value(RequestIDKey).(string); ok {
		return val
	}
	return ""
}
