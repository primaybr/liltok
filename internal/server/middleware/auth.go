package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/liltok/liltok/internal/ledger"
)

const (
	AuthKeyCtx          contextKey = "liltok_auth_key"
	AuthTypeCtx         contextKey = "liltok_auth_type"
	IsVirtualKey        contextKey = "liltok_is_virtual"
	APIKeyIDCtx         contextKey = "liltok_api_key_id"
	IsBudgetExceededCtx contextKey = "liltok_budget_exceeded"
)

// Auth provides basic authentication parsing when no KeyManager is specified.
func Auth(next http.Handler) http.Handler {
	return NewAuth(nil, nil)(next)
}

// NewAuth creates an authentication middleware validating virtual keys and enforcing quotas.
func NewAuth(km *ledger.KeyManager, qe *ledger.QuotaEnforcer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var token string
			var authType string

			// 1. Check standard Authorization: Bearer <token>
			authHeader := r.Header.Get("Authorization")
			if authHeader != "" {
				parts := strings.SplitN(authHeader, " ", 2)
				if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
					token = strings.TrimSpace(parts[1])
					authType = "bearer"
				}
			}

			// 2. Check Anthropic x-api-key header (used by Claude Code)
			if token == "" {
				anthropicKey := r.Header.Get("x-api-key")
				if anthropicKey != "" {
					token = strings.TrimSpace(anthropicKey)
					authType = "anthropic"
				}
			}

			isVirtual := strings.HasPrefix(token, "lt-live-")
			var apiKeyID string
			var budgetExceeded bool

			if isVirtual && km != nil {
				keyObj, err := km.ValidateKey(r.Context(), token)
				if err != nil {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					_ = json.NewEncoder(w).Encode(ErrorResponse{
						Error: ErrorDetail{
							Message: "Invalid or revoked virtual API key",
							Type:    "authentication_error",
							Code:    "invalid_api_key",
						},
					})
					return
				}

				apiKeyID = keyObj.ID

				if qe != nil {
					// Check Rate limits (RPM/TPM)
					if allowed, reason := qe.CheckRateLimit(keyObj, 1); !allowed {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusTooManyRequests)
						_ = json.NewEncoder(w).Encode(ErrorResponse{
							Error: ErrorDetail{
								Message: reason,
								Type:    "rate_limit_error",
								Code:    "rate_limit_exceeded",
							},
						})
						return
					}

					// Check Monthly Spend Budget
					if allowed, _ := qe.CheckBudget(keyObj); !allowed {
						budgetExceeded = true
					}
				}
			}

			ctx := context.WithValue(r.Context(), AuthKeyCtx, token)
			ctx = context.WithValue(ctx, AuthTypeCtx, authType)
			ctx = context.WithValue(ctx, IsVirtualKey, isVirtual)
			ctx = context.WithValue(ctx, APIKeyIDCtx, apiKeyID)
			ctx = context.WithValue(ctx, IsBudgetExceededCtx, budgetExceeded)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// GetAuthKey returns the extracted authorization key from context.
func GetAuthKey(ctx context.Context) string {
	if val, ok := ctx.Value(AuthKeyCtx).(string); ok {
		return val
	}
	return ""
}

// GetAuthType returns the authorization type ("bearer" or "anthropic").
func GetAuthType(ctx context.Context) string {
	if val, ok := ctx.Value(AuthTypeCtx).(string); ok {
		return val
	}
	return ""
}

// IsVirtualToken returns whether the token is a liltok virtual key.
func IsVirtualToken(ctx context.Context) bool {
	if val, ok := ctx.Value(IsVirtualKey).(bool); ok {
		return val
	}
	return false
}

// GetAPIKeyID returns the verified virtual key ID or empty string.
func GetAPIKeyID(ctx context.Context) string {
	if val, ok := ctx.Value(APIKeyIDCtx).(string); ok {
		return val
	}
	return ""
}

// IsBudgetExceeded returns whether the virtual key has exhausted its dollar spend budget.
func IsBudgetExceeded(ctx context.Context) bool {
	if val, ok := ctx.Value(IsBudgetExceededCtx).(bool); ok {
		return val
	}
	return false
}
