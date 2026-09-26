package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocalGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	cases := []struct {
		name     string
		bind     string
		method   string
		host     string
		headers  map[string]string
		wantCode int
	}{
		{"localhost host", "127.0.0.1", "POST", "localhost:8080", nil, 200},
		{"loopback ip host", "127.0.0.1", "POST", "127.0.0.1:8080", nil, 200},
		{"ipv6 loopback host", "::1", "GET", "[::1]:8080", nil, 200},
		{"rebinding host refused", "127.0.0.1", "GET", "evil.example:8080", nil, 403},
		{"rebinding host refused on api write", "localhost", "POST", "attacker.test", nil, 403},
		{"any host when listening on all interfaces", "0.0.0.0", "GET", "gateway.lan:8080", nil, 200},
		{"sdk client without browser headers", "127.0.0.1", "POST", "localhost:8080", nil, 200},
		{"dashboard same-origin write", "127.0.0.1", "POST", "localhost:8080", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://localhost:8080"}, 200},
		{"cross-site write refused", "127.0.0.1", "POST", "localhost:8080", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, 403},
		{"same-site write refused", "127.0.0.1", "DELETE", "localhost:8080", map[string]string{"Sec-Fetch-Site": "same-site"}, 403},
		{"foreign origin without fetch metadata refused", "127.0.0.1", "PUT", "localhost:8080", map[string]string{"Origin": "https://evil.example"}, 403},
		{"null origin refused", "127.0.0.1", "POST", "localhost:8080", map[string]string{"Origin": "null"}, 403},
		{"matching origin without fetch metadata allowed", "127.0.0.1", "POST", "localhost:8080", map[string]string{"Origin": "http://localhost:8080"}, 200},
		{"cross-site read allowed (browser CORS blocks the response)", "127.0.0.1", "GET", "localhost:8080", map[string]string{"Sec-Fetch-Site": "cross-site"}, 200},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, "/api/v1/cache/purge", nil)
		req.Host = tc.host
		for k, v := range tc.headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		LocalGuard(tc.bind)(ok).ServeHTTP(rec, req)
		if rec.Code != tc.wantCode {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, rec.Code, tc.wantCode, rec.Body.String())
		}
	}
}
