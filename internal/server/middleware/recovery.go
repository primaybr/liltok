package middleware

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"

	"github.com/liltok/liltok/internal/telemetry"
)

type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Message string      `json:"message"`
	Type    string      `json:"type"`
	Param   interface{} `json:"param"`
	Code    string      `json:"code"`
}

// Recovery middleware recovers from panics, logs the stack trace, and returns an RFC-compliant 500 error.
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rvr := recover(); rvr != nil {
				reqID := GetRequestID(r.Context())
				stack := string(debug.Stack())

				telemetry.Log.Error().
					Str("request_id", reqID).
					Interface("panic", rvr).
					Str("stack", stack).
					Msg("Unhandled panic recovered in HTTP handler")

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)

				resp := ErrorResponse{
					Error: ErrorDetail{
						Message: fmt.Sprintf("Internal gateway server error: %v", rvr),
						Type:    "internal_server_error",
						Param:   nil,
						Code:    "LILTOK_INTERNAL_ERROR",
					},
				}
				_ = json.NewEncoder(w).Encode(resp)
			}
		}()

		next.ServeHTTP(w, r)
	})
}
