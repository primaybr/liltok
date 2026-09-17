package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/liltok/liltok/internal/config"
	"github.com/liltok/liltok/internal/db"
	"github.com/liltok/liltok/internal/metrics"
	"github.com/liltok/liltok/internal/router"
)

func TestPrometheusExporter(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open memory db: %v", err)
	}
	defer database.Close()

	// Clear cold-start starter cache entries for clean test isolation
	_, _ = database.Exec(`DELETE FROM cache_entries`)

	// Seed some metrics rows
	_, _ = database.Exec(`
		INSERT INTO cache_entries (hash, model, normalized_prompt, response_payload)
		VALUES ('hash_1', 'gpt-4o', 'prompt', 'resp')
	`)
	_, _ = database.Exec(`
		INSERT INTO request_logs (request_id, model, provider, cache_status, cache_tier, prompt_tokens, completion_tokens, cached_tokens, latency_ms, cost_usd, saved_usd)
		VALUES ('req_prom_1', 'gpt-4o', 'openai', 'HIT', 'TIER1_EXACT', 1500, 250, 0, 2, 0.0, 0.005)
	`)

	cfg := config.DefaultConfig()
	rtr := router.NewRouter(cfg)

	exporter := metrics.NewPrometheusExporter(database, rtr)

	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()

	exporter.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	body := rec.Body.String()

	// Verify standard Prometheus metric lines
	if !strings.Contains(body, "liltok_uptime_seconds") {
		t.Errorf("expected liltok_uptime_seconds in output")
	}
	if !strings.Contains(body, "liltok_cache_entries_total 1") {
		t.Errorf("expected liltok_cache_entries_total 1 in output: %s", body)
	}
	if !strings.Contains(body, `liltok_requests_total{model="gpt-4o",cache_status="HIT",cache_tier="TIER1_EXACT"} 1`) {
		t.Errorf("expected liltok_requests_total metric in output: %s", body)
	}
	if !strings.Contains(body, `liltok_tokens_total{model="gpt-4o",type="prompt"} 1500`) {
		t.Errorf("expected prompt tokens count in output: %s", body)
	}
	if !strings.Contains(body, `liltok_savings_usd_total{model="gpt-4o"} 0.005000`) {
		t.Errorf("expected savings total in output: %s", body)
	}
	if !strings.Contains(body, `liltok_circuit_breaker_state{provider="openai"} 0`) {
		t.Errorf("expected circuit breaker state in output: %s", body)
	}
}
