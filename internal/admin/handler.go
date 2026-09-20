package admin

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/primaybr/liltok/internal/cache"
	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/ledger"
	"github.com/primaybr/liltok/internal/miner"
	"github.com/primaybr/liltok/internal/router"
)

// AdminHandler serves REST APIs for the developer dashboard.
type AdminHandler struct {
	cfg         *config.Config
	configPath  string
	database    *db.DB
	ledger      *ledger.Ledger
	keyManager  *ledger.KeyManager
	router      *router.Router
	cacheStore    cache.Store
	broadcaster   *Broadcaster
	startTime     time.Time
	miningManager *miner.MiningManager
}

// NewAdminHandler initializes administrative API endpoints.
func NewAdminHandler(cfg *config.Config, database *db.DB, led *ledger.Ledger, km *ledger.KeyManager, rtr *router.Router, cs cache.Store, b *Broadcaster) *AdminHandler {
	if b == nil {
		b = NewBroadcaster()
	}
	ah := &AdminHandler{
		cfg:         cfg,
		database:    database,
		ledger:      led,
		keyManager:  km,
		router:      rtr,
		cacheStore:  cs,
		broadcaster: b,
		startTime:   time.Now(),
	}
	if database != nil {
		ah.miningManager = miner.NewMiningManager(database, func(ev miner.MiningProgressEvent) {
			b.Broadcast(TelemetryEvent{
				Type:      ev.Type,
				Timestamp: ev.Timestamp,
				Data:      ev,
			})
		})
	}
	return ah
}

// SetConfigPath overrides the config file path (useful for test isolation).
func (h *AdminHandler) SetConfigPath(path string) {
	h.configPath = path
}

// RegisterRoutes registers all admin REST endpoints onto the Chi router.
func (h *AdminHandler) RegisterRoutes(r chi.Router) {
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/overview", h.HandleOverview)
		r.Get("/analytics", h.HandleAnalytics)
		r.Get("/logs", h.HandleLogs)
		r.Get("/logs/query", h.HandleQueryLogs)
		r.Get("/logs/export", h.HandleExportLogs)
		r.Post("/logs/clear", h.HandleClearLogs)
		r.Get("/system", h.HandleSystemDiagnostics)
		r.Post("/system/vacuum", h.HandleSystemVacuum)
		r.Get("/cache", h.HandleListCache)
		r.Delete("/cache/{hash}", h.HandleDeleteCacheEntry)
		r.Post("/cache/purge", h.HandlePurgeCache)
		r.Post("/cache/pack", h.HandlePackStarterCache)
		r.Get("/routes", h.HandleRoutes)
		r.Post("/routes/strategy", h.HandleSetRouteStrategy)
		r.Post("/routes/reset-breakers", h.HandleResetCircuitBreakers)
		r.Get("/providers", h.HandleListProviders)
		r.Post("/providers", h.HandleUpdateProviders)
		r.Post("/providers/test", h.HandleTestProvider)
		r.Get("/providers/stats", h.HandleProviderStats)
		r.Post("/providers/{name}/sync-models", h.HandleSyncProviderModels)
		r.Get("/models", h.HandleModels)
		r.Get("/keys", h.HandleListKeys)
		r.Post("/keys", h.HandleCreateKey)
		r.Delete("/keys/{id}", h.HandleRevokeKey)
		r.Get("/events", h.broadcaster.ServeHTTP)

		// Maintainer Moderation Endpoints
		r.Get("/moderate/status", h.HandleModerateStatus)
		r.Post("/moderate/upload", h.HandleModerateUpload)
		r.Get("/moderate/entries", h.HandleModerateEntries)
		r.Post("/moderate/approve", h.HandleModerateApprove)
		r.Post("/moderate/reject", h.HandleModerateReject)
		r.Delete("/moderate/clear", h.HandleModerateClear)

		// Synthetic Cache Mining Endpoints
		r.Get("/miner/status", h.HandleMinerStatus)
		r.Get("/miner/prompts", h.HandleMinerPrompts)
		r.Post("/miner/start", h.HandleMinerStart)
		r.Post("/miner/stop", h.HandleMinerStop)
	})
}

// Broadcaster returns the underlying SSE event broadcaster.
func (h *AdminHandler) Broadcaster() *Broadcaster {
	return h.broadcaster
}

// HandleOverview provides aggregated runtime statistics.
func (h *AdminHandler) HandleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var overview ledger.OverviewStats
	var err error

	if h.ledger != nil {
		overview, err = h.ledger.GetOverviewStats(ctx)
		if err != nil {
			overview = ledger.OverviewStats{}
		}
	}

	var totalEntries int64
	if h.database != nil {
		row := h.database.QueryRowContext(ctx, "SELECT COUNT(*) FROM cache_entries")
		_ = row.Scan(&totalEntries)
	}

	resp := map[string]interface{}{
		"status":                    "healthy",
		"version":                   "0.1.6-beta",
		"uptime_seconds":            int64(time.Since(h.startTime).Seconds()),
		"total_requests":            overview.TotalRequests,
		"total_hits":                overview.TotalHits,
		"local_hits":                overview.LocalHits,
		"model_cache_hits":          overview.ModelCacheHits,
		"hit_rate_percent":          overview.HitRatePercent,
		"tier1_exact_hits":          overview.Tier1ExactHits,
		"tier2_prefix_hits":         overview.Tier2PrefixHits,
		"tier3_semantic_hits":       overview.Tier3SemanticHits,
		"misses":                    overview.Misses,
		"provider_counts":           overview.ProviderCounts,
		"total_tokens_in":           overview.TotalTokensIn,
		"total_tokens_out":          overview.TotalTokensOut,
		"total_cost_usd":            overview.TotalCostUSD,
		"total_prompt_cost_usd":     overview.TotalPromptCostUSD,
		"total_completion_cost_usd": overview.TotalCompletionCostUSD,
		"gross_token_spend_usd":     overview.GrossTokenSpendUSD,
		"total_saved_usd":           overview.TotalSavedUSD,
		"total_pruned_bytes":        overview.TotalPrunedBytes,
		"total_pruned_tokens":       overview.TotalPrunedTokens,
		"avg_latency_ms":            overview.AvgLatencyMs,
		"cache_entries":             totalEntries,
		"active_listeners":          h.broadcaster.ClientCount(),
		"maintainer_mode":           h.cfg != nil && h.cfg.Maintainer.Enabled,
	}

	writeJSON(w, http.StatusOK, resp)
}

// HandleLogs returns paginated request logs.
func (h *AdminHandler) HandleLogs(w http.ResponseWriter, r *http.Request) {
	limit := 50
	offset := 0
	if l := r.URL.Query().Get("limit"); l != "" {
		if val, err := strconv.Atoi(l); err == nil && val > 0 {
			limit = val
		}
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		if val, err := strconv.Atoi(o); err == nil && val >= 0 {
			offset = val
		}
	}

	if h.database == nil {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}

	rows, err := h.database.QueryContext(r.Context(), `
		SELECT request_id, timestamp, COALESCE(api_key_id, ''), model, COALESCE(requested_model, ''), provider,
		       cache_status, cache_tier, prompt_tokens, completion_tokens,
		       cached_tokens, latency_ms, cost_usd, COALESCE(prompt_cost_usd, 0.0), COALESCE(completion_cost_usd, 0.0), saved_usd,
		       COALESCE(pruned_bytes, 0), COALESCE(pruned_tokens, 0),
		       status_code, COALESCE(error_message, '')
		FROM request_logs
		ORDER BY timestamp DESC
		LIMIT ? OFFSET ?
	`, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var logs []*ledger.RequestLog
	for rows.Next() {
		var l ledger.RequestLog
		var ts string
		if err := rows.Scan(
			&l.RequestID, &ts, &l.APIKeyID, &l.Model, &l.RequestedModel, &l.Provider,
			&l.CacheStatus, &l.CacheTier, &l.PromptTokens, &l.CompletionTokens,
			&l.CachedTokens, &l.LatencyMs, &l.CostUSD, &l.PromptCostUSD, &l.CompletionCostUSD, &l.SavedUSD,
			&l.PrunedBytes, &l.PrunedTokens,
			&l.StatusCode, &l.ErrorMessage,
		); err == nil {
			if l.RequestedModel == "" {
				l.RequestedModel = l.Model
			}
			l.Timestamp = parseLogTimestamp(ts)
			logs = append(logs, &l)
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"limit":  limit,
		"offset": offset,
		"logs":   logs,
	})
}

// HandleClearLogs purges all request logs from SQLite and broadcasts an SSE event.
func (h *AdminHandler) HandleClearLogs(w http.ResponseWriter, r *http.Request) {
	if h.database == nil {
		writeError(w, http.StatusInternalServerError, "database connection unavailable")
		return
	}

	ctx := r.Context()
	res, err := h.database.ExecContext(ctx, "DELETE FROM request_logs")
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to clear logs: %v", err))
		return
	}

	rowsDeleted, _ := res.RowsAffected()

	if r.URL.Query().Get("reset_spends") == "true" {
		_, _ = h.database.ExecContext(ctx, "UPDATE api_keys SET current_spend_usd = 0.0")
	}

	// Broadcast SSE event so connected browser dashboards update instantly
	if h.broadcaster != nil {
		h.broadcaster.Broadcast(TelemetryEvent{
			Type:      "logs_cleared",
			Timestamp: time.Now(),
			Data: map[string]interface{}{
				"cleared_count": rowsDeleted,
			},
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":        "success",
		"message":       "Request logs successfully cleared",
		"cleared_count": rowsDeleted,
	})
}

// HandleAnalytics returns time-series telemetry, unit economics, and latency percentiles.
func (h *AdminHandler) HandleAnalytics(w http.ResponseWriter, r *http.Request) {
	timeRange := r.URL.Query().Get("range")
	if timeRange == "" {
		timeRange = "24h"
	}

	var timeFilterSQL string
	var bucketFormat string
	switch timeRange {
	case "24h":
		timeFilterSQL = "timestamp >= datetime('now', '-24 hours')"
		bucketFormat = "%Y-%m-%d %H:00"
	case "7d":
		timeFilterSQL = "timestamp >= datetime('now', '-7 days')"
		bucketFormat = "%Y-%m-%d"
	case "30d":
		timeFilterSQL = "timestamp >= datetime('now', '-30 days')"
		bucketFormat = "%Y-%m-%d"
	case "all":
		timeFilterSQL = "1=1"
		bucketFormat = "%Y-%m-%d"
	default:
		timeFilterSQL = "timestamp >= datetime('now', '-24 hours')"
		bucketFormat = "%Y-%m-%d %H:00"
	}

	if h.database == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{})
		return
	}
	ctx := r.Context()

	// 1. Time Series Buckets
	type TimeBucket struct {
		Timestamp         string  `json:"timestamp"`
		Requests          int64   `json:"requests"`
		TokensIn          int64   `json:"tokens_in"`
		TokensOut         int64   `json:"tokens_out"`
		CostUSD           float64 `json:"cost_usd"`
		PromptCostUSD     float64 `json:"prompt_cost_usd"`
		CompletionCostUSD float64 `json:"completion_cost_usd"`
		SavedUSD          float64 `json:"saved_usd"`
		AvgLatencyMs      float64 `json:"avg_latency_ms"`
		CacheHits         int64   `json:"cache_hits"`
	}

	bucketQuery := fmt.Sprintf(`
		SELECT 
			strftime('%s', timestamp) AS bucket,
			COUNT(*),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(SUM(cost_usd), 0.0),
			COALESCE(SUM(prompt_cost_usd), 0.0),
			COALESCE(SUM(completion_cost_usd), 0.0),
			COALESCE(SUM(saved_usd), 0.0),
			COALESCE(AVG(latency_ms), 0.0),
			SUM(CASE WHEN cache_status = 'HIT' OR cache_tier = 'TIER2_PREFIX' OR cached_tokens > 0 THEN 1 ELSE 0 END)
		FROM request_logs
		WHERE %s
		GROUP BY bucket
		ORDER BY bucket ASC
	`, bucketFormat, timeFilterSQL)

	var buckets []TimeBucket
	rows, err := h.database.QueryContext(ctx, bucketQuery)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var b TimeBucket
			var hits sql.NullInt64
			if err := rows.Scan(
				&b.Timestamp, &b.Requests, &b.TokensIn, &b.TokensOut,
				&b.CostUSD, &b.PromptCostUSD, &b.CompletionCostUSD, &b.SavedUSD,
				&b.AvgLatencyMs, &hits,
			); err == nil {
				b.CacheHits = hits.Int64
				buckets = append(buckets, b)
			}
		}
	}

	// 2. Provider Unit Economics Matrix
	type ProviderMetric struct {
		Provider      string  `json:"provider"`
		Requests      int64   `json:"requests"`
		TokensIn      int64   `json:"tokens_in"`
		TokensOut     int64   `json:"tokens_out"`
		TotalCostUSD  float64 `json:"total_cost_usd"`
		TotalSavedUSD float64 `json:"total_saved_usd"`
		AvgLatencyMs  float64 `json:"avg_latency_ms"`
		ExactHits     int64   `json:"exact_hits"`
		PrefixHits    int64   `json:"prefix_hits"`
		Misses        int64   `json:"misses"`
	}

	provQuery := fmt.Sprintf(`
		SELECT 
			provider,
			COUNT(*),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(SUM(cost_usd), 0.0),
			COALESCE(SUM(saved_usd), 0.0),
			COALESCE(AVG(latency_ms), 0.0),
			SUM(CASE WHEN cache_status = 'HIT' AND cache_tier = 'TIER1_EXACT' THEN 1 ELSE 0 END),
			SUM(CASE WHEN (cache_tier = 'TIER2_PREFIX' OR cached_tokens > 0) AND cache_status != 'HIT' THEN 1 ELSE 0 END),
			SUM(CASE WHEN cache_status != 'HIT' AND cache_tier != 'TIER2_PREFIX' AND (cached_tokens IS NULL OR cached_tokens = 0) THEN 1 ELSE 0 END)
		FROM request_logs
		WHERE %s
		GROUP BY provider
		ORDER BY COUNT(*) DESC
	`, timeFilterSQL)

	var providers []ProviderMetric
	pRows, err := h.database.QueryContext(ctx, provQuery)
	if err == nil {
		defer pRows.Close()
		for pRows.Next() {
			var p ProviderMetric
			var exact, prefix, misses sql.NullInt64
			if err := pRows.Scan(
				&p.Provider, &p.Requests, &p.TokensIn, &p.TokensOut,
				&p.TotalCostUSD, &p.TotalSavedUSD, &p.AvgLatencyMs,
				&exact, &prefix, &misses,
			); err == nil {
				p.ExactHits = exact.Int64
				p.PrefixHits = prefix.Int64
				p.Misses = misses.Int64
				providers = append(providers, p)
			}
		}
	}

	// 3. Model Unit Economics Matrix
	type ModelMetric struct {
		Model          string  `json:"model"`
		Requests       int64   `json:"requests"`
		TokensIn       int64   `json:"tokens_in"`
		TokensOut      int64   `json:"tokens_out"`
		TotalCostUSD   float64 `json:"total_cost_usd"`
		TotalSavedUSD  float64 `json:"total_saved_usd"`
		AvgLatencyMs   float64 `json:"avg_latency_ms"`
		CostPerMillion float64 `json:"cost_per_million"`
	}

	modelQuery := fmt.Sprintf(`
		SELECT 
			model,
			COUNT(*),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(SUM(cost_usd), 0.0),
			COALESCE(SUM(saved_usd), 0.0),
			COALESCE(AVG(latency_ms), 0.0)
		FROM request_logs
		WHERE %s
		GROUP BY model
		ORDER BY COUNT(*) DESC
	`, timeFilterSQL)

	var models []ModelMetric
	mRows, err := h.database.QueryContext(ctx, modelQuery)
	if err == nil {
		defer mRows.Close()
		for mRows.Next() {
			var m ModelMetric
			if err := mRows.Scan(
				&m.Model, &m.Requests, &m.TokensIn, &m.TokensOut,
				&m.TotalCostUSD, &m.TotalSavedUSD, &m.AvgLatencyMs,
			); err == nil {
				totTokens := m.TokensIn + m.TokensOut
				if totTokens > 0 {
					m.CostPerMillion = (m.TotalCostUSD / float64(totTokens)) * 1000000.0
				}
				models = append(models, m)
			}
		}
	}

	// 4. Latency Percentiles (p50, p75, p90, p95, p99)
	latQuery := fmt.Sprintf(`SELECT latency_ms FROM request_logs WHERE %s AND latency_ms > 0 ORDER BY latency_ms ASC`, timeFilterSQL)
	var latencies []float64
	lRows, err := h.database.QueryContext(ctx, latQuery)
	if err == nil {
		defer lRows.Close()
		for lRows.Next() {
			var lat float64
			if err := lRows.Scan(&lat); err == nil {
				latencies = append(latencies, lat)
			}
		}
	}

	percentile := func(p float64) float64 {
		if len(latencies) == 0 {
			return 0
		}
		idx := int(float64(len(latencies)-1) * p)
		if idx < 0 {
			idx = 0
		}
		if idx >= len(latencies) {
			idx = len(latencies) - 1
		}
		return latencies[idx]
	}

	percentiles := map[string]float64{
		"p50": percentile(0.50),
		"p75": percentile(0.75),
		"p90": percentile(0.90),
		"p95": percentile(0.95),
		"p99": percentile(0.99),
	}

	// 5. Aggregate Summary
	var totReqs, totTokensIn, totTokensOut int64
	var totCost, totPromptCost, totCompCost, totSaved, avgLat float64
	summaryQuery := fmt.Sprintf(`
		SELECT 
			COUNT(*),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(SUM(cost_usd), 0.0),
			COALESCE(SUM(prompt_cost_usd), 0.0),
			COALESCE(SUM(completion_cost_usd), 0.0),
			COALESCE(SUM(saved_usd), 0.0),
			COALESCE(AVG(latency_ms), 0.0)
		FROM request_logs
		WHERE %s
	`, timeFilterSQL)
	_ = h.database.QueryRowContext(ctx, summaryQuery).Scan(
		&totReqs, &totTokensIn, &totTokensOut,
		&totCost, &totPromptCost, &totCompCost, &totSaved, &avgLat,
	)

	roiMultiplier := 0.0
	if totCost > 0 {
		roiMultiplier = (totCost + totSaved) / totCost
	} else if totSaved > 0 {
		roiMultiplier = 999.0
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"range":       timeRange,
		"time_series": buckets,
		"providers":   providers,
		"models":      models,
		"percentiles": percentiles,
		"summary": map[string]interface{}{
			"total_requests":            totReqs,
			"total_tokens_in":           totTokensIn,
			"total_tokens_out":          totTokensOut,
			"total_cost_usd":            totCost,
			"total_prompt_cost_usd":     totPromptCost,
			"total_completion_cost_usd": totCompCost,
			"total_saved_usd":           totSaved,
			"gross_token_spend_usd":     totCost + totSaved,
			"roi_multiplier":            roiMultiplier,
			"avg_latency_ms":            avgLat,
		},
	})
}

func buildLogsFilter(r *http.Request) (string, []interface{}) {
	var clauses []string
	var args []interface{}

	q := r.URL.Query()

	if provider := strings.TrimSpace(q.Get("provider")); provider != "" && provider != "all" {
		clauses = append(clauses, "provider = ?")
		args = append(args, provider)
	}

	if model := strings.TrimSpace(q.Get("model")); model != "" && model != "all" {
		clauses = append(clauses, "(model = ? OR requested_model = ?)")
		args = append(args, model, model)
	}

	if cacheStatus := strings.TrimSpace(q.Get("cache_status")); cacheStatus != "" && cacheStatus != "all" {
		switch cacheStatus {
		case "HIT":
			clauses = append(clauses, "cache_status = 'HIT'")
		case "MISS":
			clauses = append(clauses, "cache_status != 'HIT' AND cache_tier != 'TIER2_PREFIX' AND (cached_tokens IS NULL OR cached_tokens = 0)")
		case "TIER1_EXACT":
			clauses = append(clauses, "cache_tier = 'TIER1_EXACT'")
		case "TIER2_PREFIX":
			clauses = append(clauses, "(cache_tier = 'TIER2_PREFIX' OR cached_tokens > 0)")
		case "TIER3_SEMANTIC":
			clauses = append(clauses, "cache_tier = 'TIER3_SEMANTIC'")
		}
	}

	if timeRange := strings.TrimSpace(q.Get("time_range")); timeRange != "" && timeRange != "all" {
		switch timeRange {
		case "1h":
			clauses = append(clauses, "timestamp >= datetime('now', '-1 hour')")
		case "24h":
			clauses = append(clauses, "timestamp >= datetime('now', '-24 hours')")
		case "7d":
			clauses = append(clauses, "timestamp >= datetime('now', '-7 days')")
		case "30d":
			clauses = append(clauses, "timestamp >= datetime('now', '-30 days')")
		}
	}

	if statusCode := strings.TrimSpace(q.Get("status_code")); statusCode != "" && statusCode != "all" {
		switch statusCode {
		case "200":
			clauses = append(clauses, "status_code = 200")
		case "4xx":
			clauses = append(clauses, "status_code >= 400 AND status_code < 500")
		case "5xx":
			clauses = append(clauses, "status_code >= 500")
		}
	}

	if search := strings.TrimSpace(q.Get("search")); search != "" {
		like := "%" + search + "%"
		clauses = append(clauses, "(request_id LIKE ? OR model LIKE ? OR requested_model LIKE ? OR error_message LIKE ?)")
		args = append(args, like, like, like, like)
	}

	if len(clauses) == 0 {
		return "1=1", args
	}
	return strings.Join(clauses, " AND "), args
}

// HandleQueryLogs returns filtered and paginated request logs with aggregate query summaries.
func (h *AdminHandler) HandleQueryLogs(w http.ResponseWriter, r *http.Request) {
	if h.database == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"page":        1,
			"page_size":   25,
			"total_count": 0,
			"total_pages": 0,
			"logs":        []interface{}{},
		})
		return
	}

	ctx := r.Context()
	whereSQL, args := buildLogsFilter(r)

	// Summary and count
	var totalCount, tokensIn, tokensOut int64
	var totalCost, totalSaved float64

	countQuery := fmt.Sprintf(`
		SELECT 
			COUNT(*),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(SUM(cost_usd), 0.0),
			COALESCE(SUM(saved_usd), 0.0)
		FROM request_logs
		WHERE %s
	`, whereSQL)

	err := h.database.QueryRowContext(ctx, countQuery, args...).Scan(
		&totalCount, &tokensIn, &tokensOut, &totalCost, &totalSaved,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to count logs: %v", err))
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if pageSize < 1 || pageSize > 100 {
		pageSize = 25
	}
	offset := (page - 1) * pageSize

	sortBy := r.URL.Query().Get("sort_by")
	sortDir := strings.ToUpper(r.URL.Query().Get("sort_dir"))
	if sortDir != "ASC" {
		sortDir = "DESC"
	}

	var orderClause string
	switch sortBy {
	case "latency":
		orderClause = "ORDER BY latency_ms " + sortDir
	case "tokens":
		orderClause = "ORDER BY (prompt_tokens + completion_tokens) " + sortDir
	case "cost":
		orderClause = "ORDER BY cost_usd " + sortDir
	case "saved":
		orderClause = "ORDER BY saved_usd " + sortDir
	default:
		orderClause = "ORDER BY timestamp " + sortDir
	}

	querySQL := fmt.Sprintf(`
		SELECT request_id, timestamp, COALESCE(api_key_id, ''), model, COALESCE(requested_model, ''), provider,
		       cache_status, cache_tier, prompt_tokens, completion_tokens,
		       cached_tokens, latency_ms, cost_usd, COALESCE(prompt_cost_usd, 0.0), COALESCE(completion_cost_usd, 0.0), saved_usd,
		       COALESCE(pruned_bytes, 0), COALESCE(pruned_tokens, 0),
		       status_code, COALESCE(error_message, '')
		FROM request_logs
		WHERE %s
		%s
		LIMIT ? OFFSET ?
	`, whereSQL, orderClause)

	queryArgs := append(args, pageSize, offset)
	rows, err := h.database.QueryContext(ctx, querySQL, queryArgs...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to query logs: %v", err))
		return
	}
	defer rows.Close()

	var logs []*ledger.RequestLog
	for rows.Next() {
		var l ledger.RequestLog
		var ts string
		if err := rows.Scan(
			&l.RequestID, &ts, &l.APIKeyID, &l.Model, &l.RequestedModel, &l.Provider,
			&l.CacheStatus, &l.CacheTier, &l.PromptTokens, &l.CompletionTokens,
			&l.CachedTokens, &l.LatencyMs, &l.CostUSD, &l.PromptCostUSD, &l.CompletionCostUSD, &l.SavedUSD,
			&l.PrunedBytes, &l.PrunedTokens,
			&l.StatusCode, &l.ErrorMessage,
		); err == nil {
			if l.RequestedModel == "" {
				l.RequestedModel = l.Model
			}
			l.Timestamp = parseLogTimestamp(ts)
			logs = append(logs, &l)
		}
	}

	totalPages := 0
	if totalCount > 0 {
		totalPages = int((totalCount + int64(pageSize) - 1) / int64(pageSize))
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"page":        page,
		"page_size":   pageSize,
		"total_count": totalCount,
		"total_pages": totalPages,
		"summary": map[string]interface{}{
			"tokens_in":  tokensIn,
			"tokens_out": tokensOut,
			"cost_usd":   totalCost,
			"saved_usd":  totalSaved,
		},
		"logs": logs,
	})
}

// HandleExportLogs exports filtered request logs as CSV or JSON.
func (h *AdminHandler) HandleExportLogs(w http.ResponseWriter, r *http.Request) {
	if h.database == nil {
		writeError(w, http.StatusInternalServerError, "database unavailable")
		return
	}

	ctx := r.Context()
	whereSQL, args := buildLogsFilter(r)
	format := strings.ToLower(r.URL.Query().Get("format"))

	querySQL := fmt.Sprintf(`
		SELECT request_id, timestamp, COALESCE(api_key_id, ''), model, COALESCE(requested_model, ''), provider,
		       cache_status, cache_tier, prompt_tokens, completion_tokens,
		       cached_tokens, latency_ms, cost_usd, COALESCE(prompt_cost_usd, 0.0), COALESCE(completion_cost_usd, 0.0), saved_usd,
		       COALESCE(pruned_bytes, 0), COALESCE(pruned_tokens, 0),
		       status_code, COALESCE(error_message, '')
		FROM request_logs
		WHERE %s
		ORDER BY timestamp DESC
		LIMIT 5000
	`, whereSQL)

	rows, err := h.database.QueryContext(ctx, querySQL, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to fetch export logs: %v", err))
		return
	}
	defer rows.Close()

	var logs []*ledger.RequestLog
	for rows.Next() {
		var l ledger.RequestLog
		var ts string
		if err := rows.Scan(
			&l.RequestID, &ts, &l.APIKeyID, &l.Model, &l.RequestedModel, &l.Provider,
			&l.CacheStatus, &l.CacheTier, &l.PromptTokens, &l.CompletionTokens,
			&l.CachedTokens, &l.LatencyMs, &l.CostUSD, &l.PromptCostUSD, &l.CompletionCostUSD, &l.SavedUSD,
			&l.PrunedBytes, &l.PrunedTokens,
			&l.StatusCode, &l.ErrorMessage,
		); err == nil {
			if l.RequestedModel == "" {
				l.RequestedModel = l.Model
			}
			l.Timestamp = parseLogTimestamp(ts)
			logs = append(logs, &l)
		}
	}

	if format == "json" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", "attachment; filename=\"liltok_logs_export.json\"")
		_ = json.NewEncoder(w).Encode(logs)
		return
	}

	// Default CSV
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=\"liltok_logs_export.csv\"")
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"Timestamp", "Request ID", "API Key", "Appointed Model", "Requested Model",
		"Provider", "Cache Status", "Cache Tier", "Prompt Tokens", "Comp Tokens",
		"Total Tokens", "Cached Tokens", "Cost USD", "Prompt Cost USD", "Comp Cost USD",
		"Saved USD", "Latency MS", "Status Code", "Error",
	})
	for _, l := range logs {
		_ = cw.Write([]string{
			l.Timestamp.UTC().Format(time.RFC3339),
			l.RequestID,
			l.APIKeyID,
			l.Model,
			l.RequestedModel,
			l.Provider,
			l.CacheStatus,
			l.CacheTier,
			strconv.Itoa(l.PromptTokens),
			strconv.Itoa(l.CompletionTokens),
			strconv.Itoa(l.PromptTokens + l.CompletionTokens),
			strconv.Itoa(l.CachedTokens),
			fmt.Sprintf("%.6f", l.CostUSD),
			fmt.Sprintf("%.6f", l.PromptCostUSD),
			fmt.Sprintf("%.6f", l.CompletionCostUSD),
			fmt.Sprintf("%.6f", l.SavedUSD),
			strconv.FormatInt(l.LatencyMs, 10),
			strconv.Itoa(l.StatusCode),
			l.ErrorMessage,
		})
	}
	cw.Flush()
}

// HandleSystemDiagnostics returns Go runtime memory stats, SQLite database footprint, and circuit breaker telemetry.
func (h *AdminHandler) HandleSystemDiagnostics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	dbPath := ""
	var dbSize, walSize, shmSize int64
	if h.database != nil {
		dbPath = h.database.Path()
		if fi, err := os.Stat(dbPath); err == nil {
			dbSize = fi.Size()
		}
		if fi, err := os.Stat(dbPath + "-wal"); err == nil {
			walSize = fi.Size()
		}
		if fi, err := os.Stat(dbPath + "-shm"); err == nil {
			shmSize = fi.Size()
		}
	}

	var cacheCount, logCount, keyCount, routeCount int64
	var journalMode string
	var pageSize, pageCount int64
	if h.database != nil {
		_ = h.database.QueryRowContext(ctx, "SELECT COUNT(*) FROM cache_entries").Scan(&cacheCount)
		_ = h.database.QueryRowContext(ctx, "SELECT COUNT(*) FROM request_logs").Scan(&logCount)
		_ = h.database.QueryRowContext(ctx, "SELECT COUNT(*) FROM api_keys").Scan(&keyCount)
		_ = h.database.QueryRowContext(ctx, "SELECT COUNT(*) FROM provider_routes").Scan(&routeCount)
		_ = h.database.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode)
		_ = h.database.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize)
		_ = h.database.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount)
	}

	var breakerSnapshots []router.CircuitBreakerSnapshot
	if h.router != nil {
		breakerSnapshots = h.router.CircuitBreakerSnapshots()
	}

	resp := map[string]interface{}{
		"runtime": map[string]interface{}{
			"go_version":        runtime.Version(),
			"os":                runtime.GOOS,
			"arch":              runtime.GOARCH,
			"num_cpu":           runtime.NumCPU(),
			"goroutines":        runtime.NumGoroutine(),
			"uptime_seconds":    int64(time.Since(h.startTime).Seconds()),
			"alloc_bytes":       m.Alloc,
			"alloc_mb":          float64(m.Alloc) / (1024 * 1024),
			"total_alloc_bytes": m.TotalAlloc,
			"sys_bytes":         m.Sys,
			"sys_mb":            float64(m.Sys) / (1024 * 1024),
			"heap_inuse_bytes":  m.HeapInuse,
			"heap_inuse_mb":     float64(m.HeapInuse) / (1024 * 1024),
			"stack_inuse_bytes": m.StackInuse,
			"num_gc":            m.NumGC,
			"gc_pause_total_ms": float64(m.PauseTotalNs) / 1e6,
		},
		"database": map[string]interface{}{
			"path":                dbPath,
			"db_size_bytes":       dbSize,
			"db_size_mb":          float64(dbSize) / (1024 * 1024),
			"wal_size_bytes":      walSize,
			"wal_size_mb":         float64(walSize) / (1024 * 1024),
			"shm_size_bytes":      shmSize,
			"cache_entries_count": cacheCount,
			"request_logs_count":  logCount,
			"virtual_keys_count":  keyCount,
			"routes_count":        routeCount,
			"journal_mode":        journalMode,
			"page_size":           pageSize,
			"page_count":          pageCount,
		},
		"circuit_breakers": breakerSnapshots,
	}

	writeJSON(w, http.StatusOK, resp)
}

// HandleSystemVacuum executes SQLite optimize and VACUUM to reclaim disk space.
func (h *AdminHandler) HandleSystemVacuum(w http.ResponseWriter, r *http.Request) {
	if h.database == nil {
		writeError(w, http.StatusInternalServerError, "database unavailable")
		return
	}

	ctx := r.Context()
	_, _ = h.database.ExecContext(ctx, "PRAGMA optimize;")
	_, err := h.database.ExecContext(ctx, "VACUUM;")
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("vacuum failed: %v", err))
		return
	}

	var newSize int64
	if fi, err := os.Stat(h.database.Path()); err == nil {
		newSize = fi.Size()
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":        "success",
		"message":       "Database optimized and vacuumed successfully",
		"db_size_bytes": newSize,
		"db_size_mb":    float64(newSize) / (1024 * 1024),
	})
}

// HandleListCache returns active cache entries with hit counts and metadata.
func (h *AdminHandler) HandleListCache(w http.ResponseWriter, r *http.Request) {
	if h.database == nil {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}

	query := r.URL.Query().Get("q")
	var rows *sql.Rows
	var err error

	if query != "" {
		rows, err = h.database.QueryContext(r.Context(), `
			SELECT hash, model, normalized_prompt, hit_count, created_at, last_accessed_at, ttl_seconds, is_semantic
			FROM cache_entries
			WHERE normalized_prompt LIKE ?
			ORDER BY hit_count DESC
			LIMIT 100
		`, "%"+query+"%")
	} else {
		rows, err = h.database.QueryContext(r.Context(), `
			SELECT hash, model, normalized_prompt, hit_count, created_at, last_accessed_at, ttl_seconds, is_semantic
			FROM cache_entries
			ORDER BY last_accessed_at DESC
			LIMIT 100
		`)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	type cacheItem struct {
		Hash           string `json:"hash"`
		Model          string `json:"model"`
		PromptPreview  string `json:"prompt_preview"`
		HitCount       int    `json:"hit_count"`
		CreatedAt      string `json:"created_at"`
		LastAccessedAt string `json:"last_accessed_at"`
		TTLSeconds     int    `json:"ttl_seconds"`
		IsSemantic     bool   `json:"is_semantic"`
	}

	var items []cacheItem
	for rows.Next() {
		var item cacheItem
		var prompt string
		var isSem int
		if err := rows.Scan(&item.Hash, &item.Model, &prompt, &item.HitCount, &item.CreatedAt, &item.LastAccessedAt, &item.TTLSeconds, &isSem); err == nil {
			item.IsSemantic = isSem == 1
			if len(prompt) > 120 {
				item.PromptPreview = prompt[:120] + "..."
			} else {
				item.PromptPreview = prompt
			}
			items = append(items, item)
		}
	}

	writeJSON(w, http.StatusOK, items)
}

// HandleDeleteCacheEntry purges an individual cache record by hash.
func (h *AdminHandler) HandleDeleteCacheEntry(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	if hash == "" {
		writeError(w, http.StatusBadRequest, "missing cache hash parameter")
		return
	}

	if h.database != nil {
		_, _ = h.database.ExecContext(r.Context(), "DELETE FROM cache_entries WHERE hash = ?", hash)
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "deleted",
		"message": "Cache entry deleted successfully",
		"hash":    hash,
	})
}

// HandlePurgeCache flushes all cache entries or entries matching a specific model.
func (h *AdminHandler) HandlePurgeCache(w http.ResponseWriter, r *http.Request) {
	model := r.URL.Query().Get("model")
	all := r.URL.Query().Get("all") == "true"

	if h.database == nil {
		writeError(w, http.StatusInternalServerError, "database unavailable")
		return
	}

	var deletedCount int64
	if all {
		res, err := h.database.ExecContext(r.Context(), "DELETE FROM cache_entries")
		if err == nil {
			deletedCount, _ = res.RowsAffected()
		}
	} else if model != "" {
		res, err := h.database.ExecContext(r.Context(), "DELETE FROM cache_entries WHERE model = ?", model)
		if err == nil {
			deletedCount, _ = res.RowsAffected()
		}
	} else {
		writeError(w, http.StatusBadRequest, "must specify ?all=true or ?model=<name>")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":        "purged",
		"deleted_count": deletedCount,
	})
}

// HandlePackStarterCache merges local database cache entries into starter_cache.json.gz with sanitization.
func (h *AdminHandler) HandlePackStarterCache(w http.ResponseWriter, r *http.Request) {
	if h.database == nil {
		writeError(w, http.StatusInternalServerError, "database unavailable")
		return
	}

	var req struct {
		MinHits        int    `json:"min_hits"`
		MaxPromptBytes int    `json:"max_prompt_bytes"`
		Sanitize       *bool  `json:"sanitize"`
		TargetPath     string `json:"target_path"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	sanitize := true
	if req.Sanitize != nil {
		sanitize = *req.Sanitize
	}

	targetPath := req.TargetPath
	if targetPath == "" {
		targetPath = filepath.Join("internal", "db", "starter_cache.json.gz")
	}

	res, err := miner.PackStarterCache(h.database, targetPath, miner.PackOptions{
		MinHits:        req.MinHits,
		MaxPromptBytes: req.MaxPromptBytes,
		Sanitize:       sanitize,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":           "success",
		"total_entries":    res.TotalEntries,
		"existing_entries": res.ExistingEntries,
		"merged_from_db":   res.MergedFromDB,
		"size_bytes":       res.SizeBytes,
		"target_path":      res.TargetPath,
	})
}

// HandleRoutes reports configured routing strategies and live circuit breaker states.
func (h *AdminHandler) HandleRoutes(w http.ResponseWriter, r *http.Request) {
	type breakerInfo struct {
		Provider string `json:"provider"`
		State    string `json:"state"`
	}

	var breakers []breakerInfo
	if h.router != nil {
		for name, cb := range h.router.CircuitBreakers() {
			st, _ := cb.State()
			breakers = append(breakers, breakerInfo{
				Provider: name,
				State:    string(st),
			})
		}
	}

	routes := []map[string]interface{}{
		{
			"id":          "auto-resilient",
			"description": "Frontier Claude with rolling failover across Groq (Qwen/GPT), Gemini 1M (3.8/3.7/3.6/3.5-lite), Kilo Gateway, Mistral AI, Cline Free, NVIDIA NIM, and OpenRouter Free",
			"targets": []string{
				"anthropic/claude-sonnet-5",
				"groq/qwen/qwen3.8-27b",
				"groq/openai/gpt-oss-120b",
				"groq/openai/gpt-oss-20b",
				"gemini/gemini-3.8-flash",
				"gemini/gemini-3.7-flash",
				"gemini/gemini-3.6-flash",
				"gemini/gemini-3.5-flash-lite",
				"kilo/kilo-auto/free",
				"kilo/deepseek/deepseek-v4-flash-0731:free",
				"mistral/codestral-latest",
				"mistral/ministral-8b-latest",
				"cline/deepseek/deepseek-v4-flash-0731:free",
				"cline/qwen/qwen3.8-27b:free",
				"nvidianim/meta/llama-3.2-11b-vision-instruct",
				"nvidianim/nvidia/nemotron-3.5-lightning-30b-a3b",
				"nvidianim/poolside/laguna-xs-2.1",
				"nvidianim/google/diffusiongemma-26b-a4b-it",
				"nvidianim/nvidia/nemotron-3-super-120b-a12b",
				"nvidianim/openai/gpt-oss-20b",
				"nvidianim/nvidia/nemotron-3-nano-omni-30b-a3b-reasoning",
				"nvidianim/meta/muse-glimmer-30b",
				"nvidianim/nvidia/nemotron-3-ultra-550b-a55b",
				"openrouter/openrouter/free",
			},
		},
		{
			"id":          "free-first",
			"description": "Rolling multi-model free tier sequence: Groq (300ms) -> Gemini 1M Context -> Kilo Gateway -> Mistral AI -> Cline Free -> NVIDIA NIM -> OpenRouter Free for $0.00 spend",
			"targets": []string{
				"groq/qwen/qwen3.8-27b",
				"groq/openai/gpt-oss-120b",
				"groq/openai/gpt-oss-20b",
				"gemini/gemini-3.8-flash",
				"gemini/gemini-3.7-flash",
				"gemini/gemini-3.6-flash",
				"gemini/gemini-3.5-flash-lite",
				"kilo/kilo-auto/free",
				"kilo/deepseek/deepseek-v4-flash-0731:free",
				"mistral/codestral-latest",
				"mistral/ministral-8b-latest",
				"cline/deepseek/deepseek-v4-flash-0731:free",
				"cline/qwen/qwen3.8-27b:free",
				"nvidianim/meta/llama-3.2-11b-vision-instruct",
				"nvidianim/nvidia/nemotron-3.5-lightning-30b-a3b",
				"nvidianim/poolside/laguna-xs-2.1",
				"nvidianim/google/diffusiongemma-26b-a4b-it",
				"nvidianim/nvidia/nemotron-3-super-120b-a12b",
				"nvidianim/openai/gpt-oss-20b",
				"nvidianim/nvidia/nemotron-3-nano-omni-30b-a3b-reasoning",
				"nvidianim/meta/muse-glimmer-30b",
				"nvidianim/nvidia/nemotron-3-ultra-550b-a55b",
				"openrouter/openrouter/free",
			},
		},
		{
			"id":          "premium-only",
			"description": "Frontier intelligence models (Claude Opus 5, GPT-4o) with prompt caching",
			"targets":     []string{"anthropic/claude-opus-5", "openai/gpt-4o"},
		},
	}

	defaultStrategy := "auto-resilient"
	if h.router != nil {
		defaultStrategy = h.router.DefaultStrategy()
	} else if h.cfg != nil && h.cfg.Routes.DefaultStrategy != "" {
		defaultStrategy = h.cfg.Routes.DefaultStrategy
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"default_strategy": defaultStrategy,
		"routes":           routes,
		"circuit_breakers": breakers,
	})
}

// HandleSetRouteStrategy dynamically updates the default routing strategy and persists it.
func (h *AdminHandler) HandleSetRouteStrategy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Strategy string `json:"strategy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	strategy := strings.ToLower(strings.TrimSpace(req.Strategy))
	if strategy != "auto-resilient" && strategy != "free-first" && strategy != "premium-only" {
		writeError(w, http.StatusBadRequest, "Strategy must be one of: auto-resilient, free-first, premium-only")
		return
	}

	if h.router != nil {
		if err := h.router.SetDefaultStrategy(strategy); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		h.router.ResetCircuitBreakers()
	}
	if h.cfg != nil {
		h.cfg.Routes.DefaultStrategy = strategy
		_ = config.PersistDefaultStrategy(h.configPath, strategy)
	}

	if h.broadcaster != nil {
		h.broadcaster.Broadcast(TelemetryEvent{
			Type:      "strategy_change",
			Timestamp: time.Now(),
			Data: map[string]string{
				"strategy": strategy,
			},
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":   "success",
		"strategy": strategy,
	})
}

// HandleResetCircuitBreakers manually resets all circuit breakers or a single named circuit breaker to CLOSED.
func (h *AdminHandler) HandleResetCircuitBreakers(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	msg := "All circuit breakers reset to CLOSED"
	if h.router != nil {
		if name != "" {
			h.router.ResetCircuitBreaker(name)
			msg = fmt.Sprintf("Circuit breaker %s reset to CLOSED", name)
		} else {
			h.router.ResetCircuitBreakers()
		}
	}

	if h.broadcaster != nil {
		h.broadcaster.Broadcast(TelemetryEvent{
			Type:      "breakers_reset",
			Timestamp: time.Now(),
			Data: map[string]string{
				"status": "closed",
				"name":   name,
			},
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "success",
		"message": msg,
	})
}

// HandleListKeys returns all virtual API keys.
func (h *AdminHandler) HandleListKeys(w http.ResponseWriter, r *http.Request) {
	if h.keyManager == nil {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}

	keys, err := h.keyManager.ListKeys(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, keys)
}

// HandleCreateKey generates a new virtual API key.
func (h *AdminHandler) HandleCreateKey(w http.ResponseWriter, r *http.Request) {
	if h.keyManager == nil {
		writeError(w, http.StatusInternalServerError, "key manager unavailable")
		return
	}

	var req struct {
		Name   string  `json:"name"`
		Budget float64 `json:"budget"`
		RPM    int     `json:"rpm"`
		TPM    int     `json:"tpm"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	rawKey, keyObj, err := h.keyManager.CreateKey(r.Context(), req.Name, req.Budget, req.RPM, req.TPM)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"secret_key": rawKey,
		"key":        keyObj,
	})
}

// HandleRevokeKey revokes a virtual API key.
func (h *AdminHandler) HandleRevokeKey(w http.ResponseWriter, r *http.Request) {
	keyID := chi.URLParam(r, "id")
	if keyID == "" {
		writeError(w, http.StatusBadRequest, "missing key id")
		return
	}

	if h.keyManager == nil {
		writeError(w, http.StatusInternalServerError, "key manager unavailable")
		return
	}

	if err := h.keyManager.RevokeKey(r.Context(), keyID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "revoked",
		"message": "Key revoked successfully",
		"id":      keyID,
	})
}

// ProviderSummary represents public provider configuration and health details.
type ProviderSummary struct {
	Name                string   `json:"name"`
	DisplayName         string   `json:"display_name"`
	Tier                string   `json:"tier"`
	BaseURL             string   `json:"base_url"`
	APIKeyMasked        string   `json:"api_key_masked"`
	KeyCount            int      `json:"key_count"`
	HasKey              bool     `json:"has_key"`
	CircuitBreakerState string   `json:"circuit_breaker_state"`
	ActiveModelsCount   int      `json:"active_models_count"`
	ActiveModels        []string `json:"active_models,omitempty"`
}

func maskKey(k string) string {
	k = strings.TrimSpace(k)
	if k == "" {
		return ""
	}
	if strings.Contains(k, ",") {
		parts := strings.Split(k, ",")
		var maskedParts []string
		for _, p := range parts {
			trimmed := strings.TrimSpace(p)
			if trimmed != "" {
				maskedParts = append(maskedParts, maskSingleKey(trimmed))
			}
		}
		return strings.Join(maskedParts, ", ")
	}
	return maskSingleKey(k)
}

func maskSingleKey(k string) string {
	if len(k) <= 8 {
		return "••••••••"
	}
	return k[:4] + "••••••••" + k[len(k)-4:]
}

func countKeys(k string) int {
	k = strings.TrimSpace(k)
	if k == "" {
		return 0
	}
	parts := strings.FieldsFunc(k, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ';'
	})
	count := 0
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			count++
		}
	}
	if count == 0 && k != "" {
		return 1
	}
	return count
}

// HandleListProviders reports active provider configuration with masked credentials.
func (h *AdminHandler) HandleListProviders(w http.ResponseWriter, r *http.Request) {
	var providers []ProviderSummary

	getBreakerState := func(name string) string {
		if h.router != nil {
			if b, ok := h.router.GetBreaker(name); ok {
				st, _ := b.State()
				return string(st)
			}
		}
		return "CLOSED"
	}

	getActiveModels := func(name string) []string {
		if h.router != nil {
			models := h.router.GetProviderActiveModels(name)
			var ids []string
			for _, m := range models {
				if m.Active {
					ids = append(ids, m.ID)
				}
			}
			return ids
		}
		return nil
	}

	if h.cfg != nil {
		p := h.cfg.Providers
		groqModels := getActiveModels("groq")
		geminiModels := getActiveModels("gemini")
		nimModels := getActiveModels("nvidianim")
		anthropicModels := getActiveModels("anthropic")
		openaiModels := getActiveModels("openai")
		openrouterModels := getActiveModels("openrouter")
		kiloModels := getActiveModels("kilo")
		mistralModels := getActiveModels("mistral")
		clineModels := getActiveModels("cline")
		ollamaModels := getActiveModels("ollama")

		providers = []ProviderSummary{
			{
				Name:                "groq",
				DisplayName:         "Groq Cloud",
				Tier:                "free",
				BaseURL:             p.Groq.BaseURL,
				APIKeyMasked:        maskKey(p.Groq.APIKey),
				KeyCount:            countKeys(p.Groq.APIKey),
				HasKey:              strings.TrimSpace(p.Groq.APIKey) != "",
				CircuitBreakerState: getBreakerState("groq"),
				ActiveModelsCount:   len(groqModels),
				ActiveModels:        groqModels,
			},
			{
				Name:                "gemini",
				DisplayName:         "Google Gemini",
				Tier:                "free",
				BaseURL:             p.Gemini.BaseURL,
				APIKeyMasked:        maskKey(p.Gemini.APIKey),
				KeyCount:            countKeys(p.Gemini.APIKey),
				HasKey:              strings.TrimSpace(p.Gemini.APIKey) != "",
				CircuitBreakerState: getBreakerState("gemini"),
				ActiveModelsCount:   len(geminiModels),
				ActiveModels:        geminiModels,
			},
			{
				Name:                "nvidianim",
				DisplayName:         "NVIDIA NIM",
				Tier:                "free",
				BaseURL:             p.NVIDIANIM.BaseURL,
				APIKeyMasked:        maskKey(p.NVIDIANIM.APIKey),
				KeyCount:            countKeys(p.NVIDIANIM.APIKey),
				HasKey:              strings.TrimSpace(p.NVIDIANIM.APIKey) != "",
				CircuitBreakerState: getBreakerState("nvidianim"),
				ActiveModelsCount:   len(nimModels),
				ActiveModels:        nimModels,
			},
			{
				Name:                "anthropic",
				DisplayName:         "Anthropic Claude",
				Tier:                "premium",
				BaseURL:             p.Anthropic.BaseURL,
				APIKeyMasked:        maskKey(p.Anthropic.APIKey),
				KeyCount:            countKeys(p.Anthropic.APIKey),
				HasKey:              strings.TrimSpace(p.Anthropic.APIKey) != "",
				CircuitBreakerState: getBreakerState("anthropic"),
				ActiveModelsCount:   len(anthropicModels),
				ActiveModels:        anthropicModels,
			},
			{
				Name:                "openai",
				DisplayName:         "OpenAI",
				Tier:                "premium",
				BaseURL:             p.OpenAI.BaseURL,
				APIKeyMasked:        maskKey(p.OpenAI.APIKey),
				KeyCount:            countKeys(p.OpenAI.APIKey),
				HasKey:              strings.TrimSpace(p.OpenAI.APIKey) != "",
				CircuitBreakerState: getBreakerState("openai"),
				ActiveModelsCount:   len(openaiModels),
				ActiveModels:        openaiModels,
			},
			{
				Name:                "openrouter",
				DisplayName:         "OpenRouter (Free Tier)",
				Tier:                "free",
				BaseURL:             p.OpenRouter.BaseURL,
				APIKeyMasked:        maskKey(p.OpenRouter.APIKey),
				KeyCount:            countKeys(p.OpenRouter.APIKey),
				HasKey:              strings.TrimSpace(p.OpenRouter.APIKey) != "",
				CircuitBreakerState: getBreakerState("openrouter"),
				ActiveModelsCount:   len(openrouterModels),
				ActiveModels:        openrouterModels,
			},
			{
				Name:                "kilo",
				DisplayName:         "Kilo Gateway (Free Tier)",
				Tier:                "free",
				BaseURL:             p.Kilo.BaseURL,
				APIKeyMasked:        maskKey(p.Kilo.APIKey),
				KeyCount:            countKeys(p.Kilo.APIKey),
				HasKey:              true,
				CircuitBreakerState: getBreakerState("kilo"),
				ActiveModelsCount:   len(kiloModels),
				ActiveModels:        kiloModels,
			},
			{
				Name:                "mistral",
				DisplayName:         "Mistral AI",
				Tier:                "free",
				BaseURL:             p.Mistral.BaseURL,
				APIKeyMasked:        maskKey(p.Mistral.APIKey),
				KeyCount:            countKeys(p.Mistral.APIKey),
				HasKey:              strings.TrimSpace(p.Mistral.APIKey) != "",
				CircuitBreakerState: getBreakerState("mistral"),
				ActiveModelsCount:   len(mistralModels),
				ActiveModels:        mistralModels,
			},
			{
				Name:                "cline",
				DisplayName:         "Cline (Free Tier)",
				Tier:                "free",
				BaseURL:             p.Cline.BaseURL,
				APIKeyMasked:        maskKey(p.Cline.APIKey),
				KeyCount:            countKeys(p.Cline.APIKey),
				HasKey:              strings.TrimSpace(p.Cline.APIKey) != "",
				CircuitBreakerState: getBreakerState("cline"),
				ActiveModelsCount:   len(clineModels),
				ActiveModels:        clineModels,
			},
			{
				Name:                "ollama",
				DisplayName:         "Ollama Local",
				Tier:                "free",
				BaseURL:             p.Ollama.BaseURL,
				APIKeyMasked:        "",
				KeyCount:            0,
				HasKey:              strings.TrimSpace(p.Ollama.BaseURL) != "" && strings.TrimSpace(p.Ollama.BaseURL) != "disabled",
				CircuitBreakerState: getBreakerState("ollama"),
				ActiveModelsCount:   len(ollamaModels),
				ActiveModels:        ollamaModels,
			},
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"providers": providers,
	})
}

// HandleUpdateProviders applies new credentials to runtime memory and persists to YAML.
func (h *AdminHandler) HandleUpdateProviders(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Providers map[string]struct {
			APIKey  *string `json:"api_key,omitempty"`
			BaseURL *string `json:"base_url,omitempty"`
		} `json:"providers"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	if h.cfg == nil {
		writeError(w, http.StatusInternalServerError, "Config unavailable")
		return
	}

	for name, item := range req.Providers {
		name = strings.ToLower(name)
		var creds *config.ProviderCreds
		switch name {
		case "openai":
			creds = &h.cfg.Providers.OpenAI
		case "anthropic":
			creds = &h.cfg.Providers.Anthropic
		case "nvidianim":
			creds = &h.cfg.Providers.NVIDIANIM
		case "groq":
			creds = &h.cfg.Providers.Groq
		case "gemini":
			creds = &h.cfg.Providers.Gemini
		case "openrouter":
			creds = &h.cfg.Providers.OpenRouter
		case "kilo":
			creds = &h.cfg.Providers.Kilo
		case "mistral":
			creds = &h.cfg.Providers.Mistral
		case "cline":
			creds = &h.cfg.Providers.Cline
		case "ollama":
			creds = &h.cfg.Providers.Ollama
		}

		if creds != nil {
			if item.APIKey != nil {
				creds.APIKey = strings.TrimSpace(*item.APIKey)
			}
			if item.BaseURL != nil && strings.TrimSpace(*item.BaseURL) != "" {
				creds.BaseURL = strings.TrimSpace(*item.BaseURL)
			}
			if h.router != nil {
				_ = h.router.UpdateProvider(name, *creds)
			}
		}
	}

	// Persist to YAML
	_ = config.PersistProviders(h.configPath, h.cfg.Providers)

	if h.broadcaster != nil {
		h.broadcaster.Broadcast(TelemetryEvent{
			Type:      "providers_updated",
			Timestamp: time.Now(),
			Data: map[string]string{
				"status": "updated",
			},
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "success",
		"message": "Provider settings saved and applied to runtime successfully",
	})
}

// HandleTestProvider runs an active connectivity ping against a named provider.
func (h *AdminHandler) HandleTestProvider(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Provider == "" {
		writeError(w, http.StatusBadRequest, "Must specify 'provider'")
		return
	}

	if h.router == nil {
		writeError(w, http.StatusInternalServerError, "Router unavailable")
		return
	}

	ok, latencyMs, err := h.router.TestProvider(r.Context(), req.Provider)
	resp := map[string]interface{}{
		"provider":   req.Provider,
		"ok":         ok,
		"latency_ms": latencyMs,
	}
	if err != nil {
		resp["error"] = err.Error()
	}

	writeJSON(w, http.StatusOK, resp)
}

// HandleProviderStats aggregates request metrics and costs per provider.
func (h *AdminHandler) HandleProviderStats(w http.ResponseWriter, r *http.Request) {
	type provStat struct {
		Provider      string  `json:"provider"`
		TotalReqs     int64   `json:"total_requests"`
		TotalTokens   int64   `json:"total_tokens"`
		TotalCostUSD  float64 `json:"total_cost_usd"`
		TotalSavedUSD float64 `json:"total_saved_usd"`
		AvgLatencyMs  float64 `json:"avg_latency_ms"`
	}

	var stats []provStat
	if h.database != nil {
		rows, err := h.database.QueryContext(r.Context(), `
			SELECT provider, 
			       COUNT(*), 
			       COALESCE(SUM(prompt_tokens + completion_tokens), 0),
			       COALESCE(SUM(cost_usd), 0.0),
			       COALESCE(SUM(saved_usd), 0.0),
			       COALESCE(AVG(latency_ms), 0.0)
			FROM request_logs
			GROUP BY provider
		`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var s provStat
				if err := rows.Scan(&s.Provider, &s.TotalReqs, &s.TotalTokens, &s.TotalCostUSD, &s.TotalSavedUSD, &s.AvgLatencyMs); err == nil {
					stats = append(stats, s)
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"stats": stats,
	})
}

// HandleModels returns all active models discovered across providers.
func (h *AdminHandler) HandleModels(w http.ResponseWriter, r *http.Request) {
	if h.router != nil {
		models := h.router.GetAllActiveModels(r.Context())
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"models": models,
			"total":  len(models),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"models": []interface{}{},
		"total":  0,
	})
}

// HandleSyncProviderModels triggers live discovery and synchronization of active models for a provider.
func (h *AdminHandler) HandleSyncProviderModels(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "Provider name is required")
		return
	}

	if h.router == nil {
		writeError(w, http.StatusInternalServerError, "Router unavailable")
		return
	}

	models, err := h.router.SyncProviderModels(r.Context(), name)
	if err != nil && len(models) == 0 {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("Failed to sync models for %s: %v", name, err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"provider":      name,
		"active_models": models,
		"total":         len(models),
		"status":        "synced",
	})
}

// HandleMinerStatus returns the current status and metrics of the synthetic cache mining worker.
func (h *AdminHandler) HandleMinerStatus(w http.ResponseWriter, r *http.Request) {
	if h.miningManager == nil {
		writeJSON(w, http.StatusOK, miner.MiningStatusResponse{
			Status: miner.MiningStatusIdle,
		})
		return
	}
	status := h.miningManager.Status()
	writeJSON(w, http.StatusOK, status)
}

// HandleMinerPrompts audits the canonical prompt corpus against the current database cache.
func (h *AdminHandler) HandleMinerPrompts(w http.ResponseWriter, r *http.Request) {
	if h.database == nil {
		writeError(w, http.StatusInternalServerError, "Database unavailable")
		return
	}
	category := r.URL.Query().Get("category")
	prompts, summary, err := miner.AuditCorpusStatus(h.database, category)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"prompts": prompts,
		"summary": summary,
	})
}

// MinerStartRequest defines the payload for initiating a synthetic mining session.
type MinerStartRequest struct {
	Provider     string   `json:"provider"`
	Model        string   `json:"model"`
	APIKey       string   `json:"api_key"`
	BaseURL      string   `json:"base_url"`
	Category     string   `json:"category"`
	PromptIDs    []string `json:"prompt_ids"`
	Workers      int      `json:"workers"`
	RateLimitRPM int      `json:"rate_limit_rpm"`
	TargetModels []string `json:"target_models"`
}

// HandleMinerStart starts a synthetic cache mining session in the background.
func (h *AdminHandler) HandleMinerStart(w http.ResponseWriter, r *http.Request) {
	if h.miningManager == nil {
		writeError(w, http.StatusInternalServerError, "Mining manager unavailable")
		return
	}

	var req MinerStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON payload")
		return
	}

	if req.Provider == "" {
		req.Provider = "groq"
	}
	req.Provider = strings.ToLower(req.Provider)

	// Resolve API key if not explicitly provided
	apiKey := strings.TrimSpace(req.APIKey)
	if apiKey == "" && h.cfg != nil {
		switch req.Provider {
		case "groq":
			apiKey = h.cfg.Providers.Groq.APIKey
			if apiKey == "" {
				apiKey = os.Getenv("GROQ_API_KEY")
			}
		case "nvidianim":
			apiKey = h.cfg.Providers.NVIDIANIM.APIKey
			if apiKey == "" {
				apiKey = os.Getenv("NVIDIA_NIM_API_KEY")
			}
		case "openrouter":
			apiKey = h.cfg.Providers.OpenRouter.APIKey
			if apiKey == "" {
				apiKey = os.Getenv("OPENROUTER_API_KEY")
			}
		case "kilo":
			apiKey = h.cfg.Providers.Kilo.APIKey
			if apiKey == "" {
				apiKey = os.Getenv("KILO_API_KEY")
			}
			if apiKey == "" {
				apiKey = "anonymous"
			}
		case "mistral":
			apiKey = h.cfg.Providers.Mistral.APIKey
			if apiKey == "" {
				apiKey = os.Getenv("MISTRAL_API_KEY")
			}
		case "cline":
			apiKey = h.cfg.Providers.Cline.APIKey
			if apiKey == "" {
				apiKey = os.Getenv("CLINE_API_KEY")
			}
		}
	}

	if apiKey == "" && req.Provider != "ollama" && req.Provider != "kilo" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Missing API key for provider '%s'. Please provide an API key or configure it in Providers settings.", req.Provider))
		return
	}

	if req.Workers <= 0 {
		req.Workers = 2
	}
	if req.RateLimitRPM <= 0 {
		req.RateLimitRPM = 30
	}
	if len(req.TargetModels) == 0 {
		req.TargetModels = []string{
			"claude-3-5-sonnet-20241022",
			"claude-3-5-haiku-20241022",
			"claude-3-7-sonnet-20250219",
			"gpt-4o",
			"gpt-4o-mini",
			"deepseek-chat",
			"claude-sonnet-5",
			"claude-opus-5",
		}
	}

	// Filter prompts
	var promptsToMine []miner.PromptItem
	if len(req.PromptIDs) > 0 {
		allPrompts := miner.GetCuratedPrompts("all")
		idMap := make(map[string]bool)
		for _, id := range req.PromptIDs {
			idMap[id] = true
		}
		for _, p := range allPrompts {
			if idMap[p.ID] {
				promptsToMine = append(promptsToMine, p)
			}
		}
	} else {
		promptsToMine = miner.GetCuratedPrompts(req.Category)
	}

	if len(promptsToMine) == 0 {
		writeError(w, http.StatusBadRequest, "No matching prompts found in corpus to mine")
		return
	}

	minerCfg := miner.MinerConfig{
		Provider:     req.Provider,
		APIKey:       apiKey,
		BaseURL:      req.BaseURL,
		Model:        req.Model,
		Workers:      req.Workers,
		RateLimitRPM: req.RateLimitRPM,
		TargetModels: req.TargetModels,
	}

	if err := h.miningManager.Start(minerCfg, req.Category, promptsToMine); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":        "started",
		"provider":      req.Provider,
		"category":      req.Category,
		"total_prompts": len(promptsToMine),
	})
}

// HandleMinerStop stops any active synthetic cache mining session.
func (h *AdminHandler) HandleMinerStop(w http.ResponseWriter, r *http.Request) {
	if h.miningManager == nil {
		writeError(w, http.StatusInternalServerError, "Mining manager unavailable")
		return
	}
	if err := h.miningManager.Stop(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "stopping",
	})
}

// SetMiningManager overrides the default mining manager (useful for testing).
func (h *AdminHandler) SetMiningManager(mm *miner.MiningManager) {
	h.miningManager = mm
}

// MiningManager returns the current MiningManager.
func (h *AdminHandler) MiningManager() *miner.MiningManager {
	return h.miningManager
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{
		"error": msg,
	})
}

func parseLogTimestamp(ts string) time.Time {
	formats := []string{
		time.RFC3339,
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, ts); err == nil {
			return t
		}
	}
	return time.Now()
}

