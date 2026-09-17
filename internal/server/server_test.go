package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/liltok/liltok/internal/config"
)

func TestServerHealthz(t *testing.T) {
	cfg := config.DefaultConfig()
	s := NewServer(ServerConfig{
		Config: cfg,
	})

	req := httptest.NewRequest("GET", "/healthz", nil)
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
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected metrics 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "liltok_uptime_seconds") {
		t.Errorf("expected liltok_uptime_seconds in metrics output")
	}

	// Test GET /dashboard
	req = httptest.NewRequest("GET", "/dashboard", nil)
	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected dashboard 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<title>Liltok") {
		t.Errorf("expected Liltok title in dashboard HTML: %s", rec.Body.String())
	}

	// Test GET / (redirects to /dashboard)
	req = httptest.NewRequest("GET", "/", nil)
	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Errorf("expected redirect 307, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/dashboard" {
		t.Errorf("expected Location /dashboard, got %s", loc)
	}

	// Test GET /api/v1/overview
	req = httptest.NewRequest("GET", "/api/v1/overview", nil)
	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected overview 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "total_requests") {
		t.Errorf("expected total_requests in overview response: %s", rec.Body.String())
	}
}
