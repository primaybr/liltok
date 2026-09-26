package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/config"
)

func TestServerHealthz(t *testing.T) {
	cfg := config.DefaultConfig()
	s := NewServer(ServerConfig{
		Config: cfg,
	})

	req := httptest.NewRequest("GET", "http://localhost:8080/healthz", nil)
	rec := httptest.NewRecorder()

	s.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), `"status":"healthy"`) {
		t.Errorf("expected healthy status in body: %s", rec.Body.String())
	}
}

func TestServerDashboardAndMetrics(t *testing.T) {
	cfg := config.DefaultConfig()
	s := NewServer(ServerConfig{
		Config: cfg,
	})

	// Test GET /metrics
	req := httptest.NewRequest("GET", "http://localhost:8080/metrics", nil)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected metrics 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "liltok_uptime_seconds") {
		t.Errorf("expected liltok_uptime_seconds in metrics output")
	}

	// Test GET /dashboard
	req = httptest.NewRequest("GET", "http://localhost:8080/dashboard", nil)
	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected dashboard 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<title>Liltok") {
		t.Errorf("expected Liltok title in dashboard HTML: %s", rec.Body.String())
	}

	// Test GET / (redirects to /dashboard)
	req = httptest.NewRequest("GET", "http://localhost:8080/", nil)
	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Errorf("expected redirect 307, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/dashboard" {
		t.Errorf("expected Location /dashboard, got %s", loc)
	}

	// Test GET /api/v1/overview
	req = httptest.NewRequest("GET", "http://localhost:8080/api/v1/overview", nil)
	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected overview 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "total_requests") {
		t.Errorf("expected total_requests in overview response: %s", rec.Body.String())
	}
}

// TestServerRefusesRebindingHost checks the LocalGuard wiring: a gateway listening on loopback
// refuses a request whose Host is another name, as a DNS-rebinding page would send.
func TestServerRefusesRebindingHost(t *testing.T) {
	cfg := config.DefaultConfig()
	srv := NewServer(ServerConfig{Config: cfg})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest("GET", "http://rebind.example:8080/healthz", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("rebinding host got %d, want 403", rec.Code)
	}
}

// TestServerAdminTokenWiring checks that server.admin_token protects the admin API end to end.
func TestServerAdminTokenWiring(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Server.AdminToken = "s3cret"
	srv := NewServer(ServerConfig{Config: cfg})
	get := func(header string) int {
		req := httptest.NewRequest("GET", "http://localhost:8080/api/v1/overview", nil)
		if header != "" {
			req.Header.Set("X-Liltok-Admin-Token", header)
		}
		rec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rec, req)
		return rec.Code
	}
	if code := get(""); code != http.StatusUnauthorized {
		t.Fatalf("without the token = %d, want 401", code)
	}
	if code := get("s3cret"); code != http.StatusOK {
		t.Fatalf("with the token = %d, want 200", code)
	}
}
