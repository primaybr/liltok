package metrics_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/metrics"
)

// TestPrometheusDurationHistogramAndStatus checks the latency histogram built from request_logs
// (cumulative buckets, sum and count per model and cache status) and the status label on
// liltok_requests_total.
func TestPrometheusDurationHistogramAndStatus(t *testing.T) {
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	database, err := db.Open(t.TempDir() + "/metrics.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	for _, row := range []struct {
		id, status string
		latencyMs  int
		code       int
	}{
		{"h1", "HIT", 3, 200},
		{"h2", "HIT", 40, 200},
		{"m1", "MISS", 1200, 200},
		{"m2", "MISS", 45000, 502},
	} {
		if _, err := database.Exec(`INSERT INTO request_logs (request_id, model, provider, cache_status, cache_tier, latency_ms, status_code)
			VALUES (?, 'claude-sonnet-5', 'gemini', ?, 'NONE', ?, ?)`, row.id, row.status, row.latencyMs, row.code); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	metrics.NewPrometheusExporter(database, nil).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	want := []string{
		"# TYPE liltok_request_duration_seconds histogram",
		`liltok_request_duration_seconds_bucket{model="claude-sonnet-5",cache_status="HIT",le="0.005"} 1`,
		`liltok_request_duration_seconds_bucket{model="claude-sonnet-5",cache_status="HIT",le="0.05"} 2`,
		`liltok_request_duration_seconds_bucket{model="claude-sonnet-5",cache_status="HIT",le="+Inf"} 2`,
		`liltok_request_duration_seconds_sum{model="claude-sonnet-5",cache_status="HIT"} 0.043`,
		`liltok_request_duration_seconds_bucket{model="claude-sonnet-5",cache_status="MISS",le="1"} 0`,
		`liltok_request_duration_seconds_bucket{model="claude-sonnet-5",cache_status="MISS",le="2.5"} 1`,
		`liltok_request_duration_seconds_bucket{model="claude-sonnet-5",cache_status="MISS",le="30"} 1`,
		`liltok_request_duration_seconds_bucket{model="claude-sonnet-5",cache_status="MISS",le="60"} 2`,
		`liltok_request_duration_seconds_count{model="claude-sonnet-5",cache_status="MISS"} 2`,
		`liltok_requests_total{model="claude-sonnet-5",cache_status="MISS",cache_tier="NONE",status="502"} 1`,
		`liltok_requests_total{model="claude-sonnet-5",cache_status="MISS",cache_tier="NONE",status="200"} 1`,
	}
	for _, line := range want {
		if !strings.Contains(body, line) {
			t.Errorf("metrics output is missing %q", line)
		}
	}
}
