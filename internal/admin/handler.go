package admin

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
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
	cacheStore  cache.Store
	broadcaster *Broadcaster
	startTime   time.Time
}

// NewAdminHandler initializes administrative API endpoints.
func NewAdminHandler(cfg *config.Config, database *db.DB, led *ledger.Ledger, km *ledger.KeyManager, rtr *router.Router, cs cache.Store, b *Broadcaster) *AdminHandler {
	if b == nil {
		b = NewBroadcaster()
	}
	return &AdminHandler{
		cfg:         cfg,
		database:    database,
		ledger:      led,
		keyManager:  km,
		router:      rtr,
		cacheStore:  cs,
		broadcaster: b,
		startTime:   time.Now(),
	}
}

// SetConfigPath overrides the config file path (useful for test isolation).
func (h *AdminHandler) SetConfigPath(path string) {
	h.configPath = path
}

// RegisterRoutes registers all admin REST endpoints onto the Chi router.
func (h *AdminHandler) RegisterRoutes(r chi.Router) {
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/overview", h.HandleOverview)
		r.Get("/logs", h.HandleLogs)
		r.Get("/cache", h.HandleListCache)
		r.Delete("/cache/{hash}", h.HandleDeleteCacheEntry)
		r.Post("/cache/purge", h.HandlePurgeCache)
		r.Post("/cache/pack", h.HandlePackStarterCache)
		r.Get("/routes", h.HandleRoutes)
		r.Post("/routes/strategy", h.HandleSetRouteStrategy)
		r.Get("/providers", h.HandleListProviders)
		r.Post("/providers", h.HandleUpdateProviders)
		r.Post("/providers/test", h.HandleTestProvider)
		r.Get("/providers/stats", h.HandleProviderStats)
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
		"status":           "healthy",
		"version":          "0.1.0",
		"uptime_seconds":   int64(time.Since(h.startTime).Seconds()),
		"total_requests":   overview.TotalRequests,
		"total_hits":       overview.TotalHits,
		"local_hits":       overview.LocalHits,
		"model_cache_hits": overview.ModelCacheHits,
		"hit_rate_percent": overview.HitRatePercent,
		"total_cost_usd":   overview.TotalCostUSD,
		"total_saved_usd":  overview.TotalSavedUSD,
		"avg_latency_ms":   overview.AvgLatencyMs,
		"cache_entries":    totalEntries,
		"active_listeners": h.broadcaster.ClientCount(),
		"maintainer_mode":  h.cfg != nil && h.cfg.Maintainer.Enabled,
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
		SELECT request_id, timestamp, COALESCE(api_key_id, ''), model, provider,
		       cache_status, cache_tier, prompt_tokens, completion_tokens,
		       cached_tokens, latency_ms, cost_usd, saved_usd, status_code,
		       COALESCE(error_message, '')
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
			&l.RequestID, &ts, &l.APIKeyID, &l.Model, &l.Provider,
			&l.CacheStatus, &l.CacheTier, &l.PromptTokens, &l.CompletionTokens,
			&l.CachedTokens, &l.LatencyMs, &l.CostUSD, &l.SavedUSD,
			&l.StatusCode, &l.ErrorMessage,
		); err == nil {
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
			"description": "Frontier models with automatic failover to budget and free tiers",
			"targets":     []string{"anthropic/claude-sonnet-5", "groq/qwen/qwen3.8-27b", "gemini/gemini-flash-latest", "nvidianim/meta/llama-3.2-11b-vision-instruct"},
		},
		{
			"id":          "free-first",
			"description": "Free AI coding agents (Groq, Gemini Free with 3 keys, NVIDIA NIM) for $0.00 spend",
			"targets":     []string{"groq/qwen/qwen3.8-27b", "gemini/gemini-flash-latest", "nvidianim/meta/llama-3.2-11b-vision-instruct"},
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
	Name                string `json:"name"`
	DisplayName         string `json:"display_name"`
	Tier                string `json:"tier"`
	BaseURL             string `json:"base_url"`
	APIKeyMasked        string `json:"api_key_masked"`
	KeyCount            int    `json:"key_count"`
	HasKey              bool   `json:"has_key"`
	CircuitBreakerState string `json:"circuit_breaker_state"`
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

	if h.cfg != nil {
		p := h.cfg.Providers
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
			},
			{
				Name:                "ollama",
				DisplayName:         "Ollama Local",
				Tier:                "free",
				BaseURL:             p.Ollama.BaseURL,
				APIKeyMasked:        "",
				KeyCount:            0,
				HasKey:              true,
				CircuitBreakerState: getBreakerState("ollama"),
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

