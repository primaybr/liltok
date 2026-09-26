package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

const (
	// AdminTokenHeader carries the admin token on API calls from the CLI, MCP bridge and scripts.
	AdminTokenHeader = "X-Liltok-Admin-Token"
	// AdminTokenCookie carries it for the dashboard, including its event stream (EventSource cannot
	// set headers). It is set by POST /api/v1/auth.
	AdminTokenCookie = "liltok_admin"
	// AdminAuthPath is the login endpoint; it is reachable without the token.
	AdminAuthPath = "/api/v1/auth"
)

// AdminToken requires token on every /api/v1 admin API request when token is set. The model API
// (/v1), health checks, metrics and the dashboard page itself are not affected. An empty token
// leaves the admin API open, as before.
func AdminToken(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if token == "" {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/v1/") && r.URL.Path != AdminAuthPath && !AdminTokenMatches(r, token) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"admin token required: send the X-Liltok-Admin-Token header or sign in on the dashboard"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// AdminTokenMatches reports whether the request carries token in the header or the login cookie.
func AdminTokenMatches(r *http.Request, token string) bool {
	if token == "" {
		return true
	}
	given := r.Header.Get(AdminTokenHeader)
	if given == "" {
		if c, err := r.Cookie(AdminTokenCookie); err == nil {
			given = c.Value
		}
	}
	return given != "" && subtle.ConstantTimeCompare([]byte(given), []byte(token)) == 1
}
