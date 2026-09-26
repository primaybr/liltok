package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminToken(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	cases := []struct {
		name   string
		token  string
		path   string
		header string
		cookie string
		want   int
	}{
		{"no token configured leaves the API open", "", "/api/v1/logs", "", "", 200},
		{"admin API without the token", "s3cret", "/api/v1/logs", "", "", 401},
		{"admin API with the header", "s3cret", "/api/v1/logs", "s3cret", "", 200},
		{"admin API with the login cookie", "s3cret", "/api/v1/events", "", "s3cret", 200},
		{"wrong header", "s3cret", "/api/v1/keys", "guess", "", 401},
		{"wrong cookie", "s3cret", "/api/v1/keys", "", "guess", 401},
		{"login endpoint is reachable without it", "s3cret", "/api/v1/auth", "", "", 200},
		{"model API is not affected", "s3cret", "/v1/messages", "", "", 200},
		{"health is not affected", "s3cret", "/health", "", "", 200},
		{"dashboard page is not affected", "s3cret", "/dashboard", "", "", 200},
	}
	for _, tc := range cases {
		req := httptest.NewRequest("GET", tc.path, nil)
		if tc.header != "" {
			req.Header.Set(AdminTokenHeader, tc.header)
		}
		if tc.cookie != "" {
			req.AddCookie(&http.Cookie{Name: AdminTokenCookie, Value: tc.cookie})
		}
		rec := httptest.NewRecorder()
		AdminToken(tc.token)(ok).ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}
