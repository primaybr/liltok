package metrics

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/liltok/liltok/internal/db"
	"github.com/liltok/liltok/internal/router"
)

// PrometheusExporter formats runtime gateway telemetry into standard Prometheus text representation.
type PrometheusExporter struct {
	database  *db.DB
	router    *router.Router
	startTime time.Time
}

// NewPrometheusExporter creates a new Prometheus metrics exporter.
func NewPrometheusExporter(database *db.DB, rtr *router.Router) *PrometheusExporter {
	return &PrometheusExporter{
		database:  database,
		router:    rtr,
		startTime: time.Now(),
	}
}

// ServeHTTP handles requests to GET /metrics.
func (pe *PrometheusExporter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var b strings.Builder

	// Uptime
	uptime := time.Since(pe.startTime).Seconds()
	b.WriteString("# HELP liltok_uptime_seconds Uptime of the liltok gateway in seconds.\n")
	b.WriteString("# TYPE liltok_uptime_seconds gauge\n")
	b.WriteString(fmt.Sprintf("liltok_uptime_seconds %.2f\n\n", uptime))

	if pe.database != nil {
		// Cache entries count
		var cacheEntries int64
		row := pe.database.QueryRowContext(ctx, "SELECT COUNT(*) FROM cache_entries")
		_ = row.Scan(&cacheEntries)

		b.WriteString("# HELP liltok_cache_entries_total Total number of active entries in the local cache.\n")
		b.WriteString("# TYPE liltok_cache_entries_total gauge\n")
		b.WriteString(fmt.Sprintf("liltok_cache_entries_total %d\n\n", cacheEntries))

		// Requests total by model, cache_status, and cache_tier
		b.WriteString("# HELP liltok_requests_total Total number of chat requests processed.\n")
		b.WriteString("# TYPE liltok_requests_total counter\n")
		rows, err := pe.database.QueryContext(ctx, `
			SELECT model, cache_status, cache_tier, COUNT(*)
			FROM request_logs
			GROUP BY model, cache_status, cache_tier
		`)
		if err == nil {
			for rows.Next() {
				var model, status, tier string
				var count int64
				if err := rows.Scan(&model, &status, &tier, &count); err == nil {
					b.WriteString(fmt.Sprintf("liltok_requests_total{model=%q,cache_status=%q,cache_tier=%q} %d\n", model, status, tier, count))
				}
			}
			rows.Close()
		}
		b.WriteString("\n")

		// Tokens total by model and type
		b.WriteString("# HELP liltok_tokens_total Total tokens processed broken down by type.\n")
		b.WriteString("# TYPE liltok_tokens_total counter\n")
		tokenRows, err := pe.database.QueryContext(ctx, `
			SELECT model, 
			       COALESCE(SUM(prompt_tokens), 0),
			       COALESCE(SUM(completion_tokens), 0),
			       COALESCE(SUM(cached_tokens), 0)
			FROM request_logs
			GROUP BY model
		`)
		if err == nil {
			for tokenRows.Next() {
				var model string
				var prompt, completion, cached int64
				if err := tokenRows.Scan(&model, &prompt, &completion, &cached); err == nil {
					b.WriteString(fmt.Sprintf("liltok_tokens_total{model=%q,type=\"prompt\"} %d\n", model, prompt))
					b.WriteString(fmt.Sprintf("liltok_tokens_total{model=%q,type=\"completion\"} %d\n", model, completion))
					b.WriteString(fmt.Sprintf("liltok_tokens_total{model=%q,type=\"cached\"} %d\n", model, cached))
				}
			}
			tokenRows.Close()
		}
		b.WriteString("\n")

		// Savings USD total by model
		b.WriteString("# HELP liltok_savings_usd_total Estimated dollar savings achieved through local caching.\n")
		b.WriteString("# TYPE liltok_savings_usd_total counter\n")
		savingsRows, err := pe.database.QueryContext(ctx, `
			SELECT model, COALESCE(SUM(saved_usd), 0.0)
			FROM request_logs
			GROUP BY model
		`)
		if err == nil {
			for savingsRows.Next() {
				var model string
				var saved float64
				if err := savingsRows.Scan(&model, &saved); err == nil {
					b.WriteString(fmt.Sprintf("liltok_savings_usd_total{model=%q} %.6f\n", model, saved))
				}
			}
			savingsRows.Close()
		}
		b.WriteString("\n")

		// Cost USD total by model
		b.WriteString("# HELP liltok_cost_usd_total Gross expenditure incurred across upstream providers.\n")
		b.WriteString("# TYPE liltok_cost_usd_total counter\n")
		costRows, err := pe.database.QueryContext(ctx, `
			SELECT model, COALESCE(SUM(cost_usd), 0.0)
			FROM request_logs
			GROUP BY model
		`)
		if err == nil {
			for costRows.Next() {
				var model string
				var cost float64
				if err := costRows.Scan(&model, &cost); err == nil {
					b.WriteString(fmt.Sprintf("liltok_cost_usd_total{model=%q} %.6f\n", model, cost))
				}
			}
			costRows.Close()
		}
		b.WriteString("\n")
	}

	// Circuit breaker status
	if pe.router != nil {
		b.WriteString("# HELP liltok_circuit_breaker_state Current state of upstream provider circuit breaker (0=closed, 1=half-open, 2=open).\n")
		b.WriteString("# TYPE liltok_circuit_breaker_state gauge\n")
		for name, cb := range pe.router.CircuitBreakers() {
			val := 0
			st, _ := cb.State()
			switch st {
			case router.StateHalfOpen:
				val = 1
			case router.StateOpen:
				val = 2
			}
			b.WriteString(fmt.Sprintf("liltok_circuit_breaker_state{provider=%q} %d\n", name, val))
		}
		b.WriteString("\n")
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}
