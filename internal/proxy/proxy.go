package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/primaybr/liltok/internal/admin"
	"github.com/primaybr/liltok/internal/cache"
	"github.com/primaybr/liltok/internal/cache/prefix"
	"github.com/primaybr/liltok/internal/cache/prune"
	"github.com/primaybr/liltok/internal/cache/semantic"
	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/ledger"
	"github.com/primaybr/liltok/internal/provider"
	"github.com/primaybr/liltok/internal/router"
	"github.com/primaybr/liltok/internal/server/middleware"
	"github.com/primaybr/liltok/internal/telemetry"
	"github.com/primaybr/liltok/internal/tokens"
)

// Proxy handles reverse proxying between clients and upstream providers with caching, resilient routing, and ledger tracking.
type Proxy struct {
	cfg             *config.Config
	cacheStore      cache.Store
	semanticCache   *semantic.SemanticCache
	pruner          *prune.Pruner
	prefixOptimizer *prefix.PrefixOptimizer
	router          *router.Router
	ledger          *ledger.Ledger
	pricingReg      *tokens.PricingRegistry
	httpClient      *http.Client
	broadcaster     *admin.Broadcaster
	coalescer       *InFlightCoalescer
}

// NewProxy creates a new Proxy instance with multi-tier caching, routing, and accounting capabilities.
func NewProxy(cfg *config.Config, cacheStore cache.Store, semCache *semantic.SemanticCache, r *router.Router, led *ledger.Ledger, pr *tokens.PricingRegistry) *Proxy {
	if pr == nil {
		pr = tokens.NewPricingRegistry(nil)
	}

	pruneOpts := prune.DefaultOptions()
	pruneOpts.EnableDiff = cfg.Cache.PruneDiffs
	pruneOpts.EnableSessionCompactor = cfg.Cache.SessionCompactorEnabled
	if cfg.Cache.RecentTurnsToKeep > 0 {
		pruneOpts.RecentTurnsToKeep = cfg.Cache.RecentTurnsToKeep
	}
	if cfg.Cache.CompactorHeadBytes > 0 {
		pruneOpts.CompactorHeadBytes = cfg.Cache.CompactorHeadBytes
	}
	if cfg.Cache.CompactorTailBytes > 0 {
		pruneOpts.CompactorTailBytes = cfg.Cache.CompactorTailBytes
	}

	return &Proxy{
		cfg:             cfg,
		cacheStore:      cacheStore,
		semanticCache:   semCache,
		pruner:          prune.NewPruner(pruneOpts),
		prefixOptimizer: prefix.NewPrefixOptimizer(prefix.DefaultMinTokensForAnthropicCache),
		router:          r,
		ledger:          led,
		pricingReg:      pr,
		coalescer:       NewInFlightCoalescer(),
		httpClient: &http.Client{
			Timeout: time.Duration(cfg.Server.WriteTimeoutSeconds) * time.Second,
		},
	}
}

// SetBroadcaster attaches an SSE telemetry broadcaster to the proxy.
func (p *Proxy) SetBroadcaster(b *admin.Broadcaster) {
	p.broadcaster = b
}

// SetRouter attaches a router to the proxy.
func (p *Proxy) SetRouter(r *router.Router) {
	p.router = r
}

func (p *Proxy) recordLog(item *ledger.RequestLog) {
	if item == nil {
		return
	}
	if item.Timestamp.IsZero() {
		item.Timestamp = time.Now()
	}
	if p.ledger != nil {
		p.ledger.Record(item)
	}
	if p.broadcaster != nil {
		p.broadcaster.Broadcast(admin.TelemetryEvent{
			Type:      "request",
			Timestamp: item.Timestamp,
			Data:      item,
		})
	}
}

// CommonChatRequest carries common fields for payload inspection.
type CommonChatRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// HandleChatCompletions proxies standard OpenAI /v1/chat/completions requests.
func (p *Proxy) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	p.proxyToTarget(w, r, "openai", "/v1/chat/completions")
}

// HandleCompletions proxies legacy OpenAI /v1/completions requests.
func (p *Proxy) HandleCompletions(w http.ResponseWriter, r *http.Request) {
	p.proxyToTarget(w, r, "openai", "/v1/completions")
}

// HandleEmbeddings proxies OpenAI /v1/embeddings requests.
func (p *Proxy) HandleEmbeddings(w http.ResponseWriter, r *http.Request) {
	p.proxyToTarget(w, r, "openai", "/v1/embeddings")
}

// HandleModels returns active models across providers in OpenAI-compatible format.
func (p *Proxy) HandleModels(w http.ResponseWriter, r *http.Request) {
	if p.router != nil {
		models := p.router.GetAllActiveModels(r.Context())
		if len(models) > 0 {
			type modelEntry struct {
				ID         string        `json:"id"`
				Object     string        `json:"object"`
				Created    int64         `json:"created"`
				OwnedBy    string        `json:"owned_by"`
				Permission []interface{} `json:"permission"`
				Root       string        `json:"root"`
				Parent     interface{}   `json:"parent"`
			}
			type modelListResponse struct {
				Object string       `json:"object"`
				Data   []modelEntry `json:"data"`
			}

			resp := modelListResponse{
				Object: "list",
				Data:   make([]modelEntry, 0, len(models)),
			}
			now := time.Now().Unix()
			for _, m := range models {
				if !m.Active {
					continue
				}
				resp.Data = append(resp.Data, modelEntry{
					ID:         m.ID,
					Object:     "model",
					Created:    now,
					OwnedBy:    m.Provider,
					Permission: []interface{}{},
					Root:       m.ID,
					Parent:     nil,
				})
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
	}
	p.proxyToTarget(w, r, "openai", "/v1/models")
}

// HandleAnthropicMessages proxies native Anthropic /v1/messages requests (Claude Code).
func (p *Proxy) HandleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	p.proxyToTarget(w, r, "anthropic", "/v1/messages")
}

func (p *Proxy) proxyToTarget(w http.ResponseWriter, r *http.Request, targetProvider, defaultPath string) {
	startTime := time.Now()
	reqID := middleware.GetRequestID(r.Context())
	authKey := middleware.GetAuthKey(r.Context())
	apiKeyID := middleware.GetAPIKeyID(r.Context())
	isVirtual := middleware.IsVirtualToken(r.Context())

	// Read body and clone for inspection
	var bodyBytes []byte
	if r.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		if err != nil {
			p.writeError(w, http.StatusBadRequest, "Failed to read request body", "LILTOK_BAD_PAYLOAD")
			return
		}
		_ = r.Body.Close()
	}

	// 0. Tier-0 Pre-Flight Token Pruner
	var prunedBytesCount int
	var prunedTokensCount int
	bypassPrune := strings.EqualFold(r.Header.Get("X-Liltok-Prune"), "false")
	if !bypassPrune && p.pruner != nil && len(bodyBytes) > 0 {
		if prunedBytes, stats, err := p.pruner.PruneJSONPayload(bodyBytes); err == nil && stats.SavedBytes > 0 {
			prunedBytesCount = stats.SavedBytes
			prunedTokensCount = stats.SavedBytes / 4
			if prunedTokensCount == 0 && stats.SavedBytes > 0 {
				prunedTokensCount = 1
			}
			telemetry.Log.Debug().
				Str("request_id", reqID).
				Int("saved_bytes", stats.SavedBytes).
				Int("pruned_tokens", prunedTokensCount).
				Float64("reduction_ratio", stats.ReductionRatio).
				Msg("Tier-0 Pre-flight Token Pruning applied")
			bodyBytes = prunedBytes
		}
	}

	recordLog := func(item *ledger.RequestLog) {
		if item != nil {
			item.PrunedBytes = prunedBytesCount
			item.PrunedTokens = prunedTokensCount
			p.recordLog(item)
		}
	}

	// Extract prompt details for guardrails and semantic caching
	systemPrompt, toolsJSON, userQuery := extractPromptDetails(bodyBytes)

	// Detect if stream is requested
	var chatReq CommonChatRequest
	if len(bodyBytes) > 0 {
		_ = json.Unmarshal(bodyBytes, &chatReq)
	}

	// Check for client-side cache bypass
	bypassCache := strings.EqualFold(r.Header.Get("X-Liltok-Cache-Bypass"), "true")

	// 1. Evaluate Tier-1 Exact Match Cache
	var normReq *cache.NormalizedRequest
	if len(bodyBytes) > 0 && !bypassCache {
		normOpts := cache.NormalizationOptions{
			CacheNonzeroTemperature: p.cfg.Cache.CacheNonzeroTemperature,
		}
		if n, err := cache.NormalizePayload(bodyBytes, normOpts); err == nil {
			normReq = n
			if normReq.IsCacheable && p.cacheStore != nil {
				if entry, hit, err := p.cacheStore.Get(r.Context(), normReq.Hash); err == nil && hit {
					telemetry.Log.Info().
						Str("request_id", reqID).
						Str("hash", normReq.Hash).
						Str("model", normReq.Model).
						Str("provider", targetProvider).
						Msg("Tier-1 Exact Match Cache HIT")

					pTokens := tokens.CountTokens(normReq.Model, normReq.CanonicalJSON)
					_, cTokens, _ := extractUsage(entry.ResponsePayload, normReq.Model)
					costBD := p.pricingReg.CalculateDetailed(normReq.Model, pTokens, cTokens, 0, "HIT", "TIER1_EXACT")

					recordLog(&ledger.RequestLog{
						RequestID:         reqID,
						APIKeyID:          apiKeyID,
						Model:             normReq.Model,
						RequestedModel:    normReq.Model,
						Provider:          "cache-local",
						CacheStatus:       "HIT",
						CacheTier:         "TIER1_EXACT",
						PromptTokens:      pTokens,
						CompletionTokens:  cTokens,
						CachedTokens:      0,
						LatencyMs:         time.Since(startTime).Milliseconds(),
						CostUSD:           costBD.TotalCostUSD,
						PromptCostUSD:     costBD.PromptCostUSD,
						CompletionCostUSD: costBD.CompletionCostUSD,
						SavedUSD:          costBD.SavedUSD,
						StatusCode:        http.StatusOK,
					})

					_ = cache.ReplayCacheHit(w, entry, chatReq.Stream, targetProvider == "anthropic", 850)
					return
				}

				// In-Flight Request Coalescing: check if an identical request is already running
				if p.coalescer != nil {
					isFirst, waitCh := p.coalescer.Start(normReq.Hash)
					if !isFirst {
						select {
						case <-waitCh:
							// The in-flight request finished; check if it populated the cache
							if entry, hit, err := p.cacheStore.Get(r.Context(), normReq.Hash); err == nil && hit {
								telemetry.Log.Info().
									Str("request_id", reqID).
									Str("hash", normReq.Hash).
									Str("model", normReq.Model).
									Str("provider", targetProvider).
									Msg("Tier-1 Exact Match Cache HIT (Coalesced)")

								pTokens := tokens.CountTokens(normReq.Model, normReq.CanonicalJSON)
								_, cTokens, _ := extractUsage(entry.ResponsePayload, normReq.Model)
								costBD := p.pricingReg.CalculateDetailed(normReq.Model, pTokens, cTokens, 0, "HIT", "TIER1_EXACT")

								recordLog(&ledger.RequestLog{
									RequestID:         reqID,
									APIKeyID:          apiKeyID,
									Model:             normReq.Model,
									RequestedModel:    normReq.Model,
									Provider:          "cache-local",
									CacheStatus:       "HIT",
									CacheTier:         "TIER1_EXACT",
									PromptTokens:      pTokens,
									CompletionTokens:  cTokens,
									CachedTokens:      0,
									LatencyMs:         time.Since(startTime).Milliseconds(),
									CostUSD:           costBD.TotalCostUSD,
									PromptCostUSD:     costBD.PromptCostUSD,
									CompletionCostUSD: costBD.CompletionCostUSD,
									SavedUSD:          costBD.SavedUSD,
									StatusCode:        http.StatusOK,
								})

								_ = cache.ReplayCacheHit(w, entry, chatReq.Stream, targetProvider == "anthropic", 850)
								return
							}
						case <-r.Context().Done():
							return
						case <-time.After(60 * time.Second):
							// In-flight request timed out; fall through to upstream
						}
					} else {
						defer p.coalescer.Done(normReq.Hash)
					}
				}
			}

			// 2. Evaluate Tier-3 Semantic Similarity Cache (on Tier-1 Miss)
			if normReq.IsCacheable && p.semanticCache != nil && userQuery != "" {
				if semEntry, sim, semHit := p.semanticCache.Lookup(r.Context(), normReq.Model, systemPrompt, toolsJSON, userQuery); semHit && semEntry != nil {
					telemetry.Log.Info().
						Str("request_id", reqID).
						Str("model", normReq.Model).
						Float32("similarity", sim).
						Msg("Tier-3 Semantic Similarity Cache HIT")

					w.Header().Set("X-Liltok-Request-Id", reqID)
					w.Header().Set("X-Liltok-Cache-Status", "HIT")
					w.Header().Set("X-Liltok-Cache-Tier", "TIER3_SEMANTIC")
					w.Header().Set("X-Liltok-Semantic-Score", fmt.Sprintf("%.4f", sim))

					cachedEntry := &cache.CacheEntry{
						Hash:            semEntry.Hash,
						Model:           semEntry.Model,
						ResponsePayload: semEntry.ResponsePayload,
					}

					pTokens := tokens.CountTokens(normReq.Model, normReq.CanonicalJSON)
					_, cTokens, _ := extractUsage(cachedEntry.ResponsePayload, normReq.Model)
					costBD := p.pricingReg.CalculateDetailed(normReq.Model, pTokens, cTokens, 0, "HIT", "TIER3_SEMANTIC")

					recordLog(&ledger.RequestLog{
						RequestID:         reqID,
						APIKeyID:          apiKeyID,
						Model:             normReq.Model,
						RequestedModel:    normReq.Model,
						Provider:          "cache-local",
						CacheStatus:       "HIT",
						CacheTier:         "TIER3_SEMANTIC",
						PromptTokens:      pTokens,
						CompletionTokens:  cTokens,
						CachedTokens:      0,
						LatencyMs:         time.Since(startTime).Milliseconds(),
						CostUSD:           costBD.TotalCostUSD,
						PromptCostUSD:     costBD.PromptCostUSD,
						CompletionCostUSD: costBD.CompletionCostUSD,
						SavedUSD:          costBD.SavedUSD,
						StatusCode:        http.StatusOK,
					})

					_ = cache.ReplayCacheHitWithTier(w, cachedEntry, chatReq.Stream, targetProvider == "anthropic", "TIER3_SEMANTIC", 850)
					return
				}
			}
		}
	}

	// 2.5. Check Monthly Budget Quota (Cache hits were permitted above, upstream blocked below)
	if middleware.IsBudgetExceeded(r.Context()) {
		telemetry.Log.Warn().
			Str("request_id", reqID).
			Str("api_key_id", apiKeyID).
			Msg("Upstream request rejected: monthly dollar budget exceeded")
		p.writeError(w, http.StatusTooManyRequests, "Monthly budget quota exceeded for this API key. Cache hits remain available.", "insufficient_quota")
		return
	}

	// 3. Evaluate Tier-2 Prefix Prompt Caching (for Anthropic targets)
	if targetProvider == "anthropic" && p.prefixOptimizer != nil && len(bodyBytes) > 0 {
		if optBytes, injected, err := p.prefixOptimizer.OptimizeAnthropicPayload(bodyBytes); err == nil && injected {
			bodyBytes = optBytes
			telemetry.Log.Debug().
				Str("request_id", reqID).
				Msg("Tier-2 Anthropic Ephemeral Prompt Caching injected")
		}
	}

	// 4. Cache MISS: Router Fallback Dispatching (streaming & non-streaming)
	routeAlias := r.Header.Get("X-Liltok-Route")
	hasClientAuth := authKey != "" && !isVirtual
	shouldRoute := routeAlias != "" || !hasClientAuth || (p.router != nil && p.router.DefaultStrategy() != "")

	if p.router != nil && len(bodyBytes) > 0 && shouldRoute {
		unifiedReq, err := provider.ParseUnifiedRequest(bodyBytes, targetProvider == "anthropic")
		if err == nil {
			resp, winningProvider, err := p.router.DispatchChat(r.Context(), unifiedReq, routeAlias)

			if err == nil {
				w.Header().Set("X-Liltok-Request-Id", reqID)
				w.Header().Set("X-Liltok-Cache-Status", "MISS")
				w.Header().Set("X-Liltok-Cache-Tier", "NONE")
				w.Header().Set("X-Liltok-Provider", winningProvider)

				var finalBytes []byte
				if targetProvider == "anthropic" && winningProvider != "anthropic" {
					finalBytes, _ = p.router.Translator().ConvertOpenAIToAnthropicResponse(resp, unifiedReq.Model)
				} else {
					finalBytes = resp.RawResponse
					if len(finalBytes) == 0 {
						finalBytes, _ = json.Marshal(map[string]interface{}{
							"id":      resp.ID,
							"object":  "chat.completion",
							"model":   resp.Model,
							"choices": []map[string]interface{}{{"index": 0, "message": map[string]string{"role": resp.Role, "content": resp.Content}, "finish_reason": resp.FinishReason}},
						})
					}
				}

				modelName := unifiedReq.Model
				if normReq != nil {
					modelName = normReq.Model
				}

				if chatReq.Stream {
					entry := &cache.CacheEntry{
						Model:           modelName,
						ResponsePayload: finalBytes,
					}
					if normReq != nil {
						entry.Hash = normReq.Hash
					}
					_ = cache.ReplayCacheHitWithTier(w, entry, true, targetProvider == "anthropic", "NONE", 0)
				} else {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(finalBytes)
				}
				pTokens, cTokens, cachedTokens := extractUsage(finalBytes, modelName)
				if pTokens == 0 && normReq != nil {
					pTokens = tokens.CountTokens(modelName, normReq.CanonicalJSON)
				}

				tier := "NONE"
				if cachedTokens > 0 {
					tier = "TIER2_PREFIX"
				}
				costBD := p.pricingReg.CalculateDetailedForRouting(modelName, winningProvider, pTokens, cTokens, cachedTokens, "MISS", tier)

				appointedModel := resp.Model
				if appointedModel == "" {
					appointedModel = modelName
				}

				recordLog(&ledger.RequestLog{
					RequestID:         reqID,
					APIKeyID:          apiKeyID,
					Model:             appointedModel,
					RequestedModel:    modelName,
					Provider:          winningProvider,
					CacheStatus:       "MISS",
					CacheTier:         tier,
					PromptTokens:      pTokens,
					CompletionTokens:  cTokens,
					CachedTokens:      cachedTokens,
					LatencyMs:         time.Since(startTime).Milliseconds(),
					CostUSD:           costBD.TotalCostUSD,
					PromptCostUSD:     costBD.PromptCostUSD,
					CompletionCostUSD: costBD.CompletionCostUSD,
					SavedUSD:          costBD.SavedUSD,
					StatusCode:        http.StatusOK,
				})

				if normReq != nil && normReq.IsCacheable && len(finalBytes) > 0 {
					if p.cacheStore != nil {
						entry := &cache.CacheEntry{
							Hash:             normReq.Hash,
							Model:            normReq.Model,
							NormalizedPrompt: normReq.CanonicalJSON,
							ResponsePayload:  finalBytes,
							TTLSeconds:       p.cfg.Cache.DefaultTTLSeconds,
						}
						_ = p.cacheStore.Set(context.Background(), entry)
					}
					if p.semanticCache != nil && userQuery != "" {
						_ = p.semanticCache.Store(context.Background(), normReq.Hash, normReq.Model, systemPrompt, toolsJSON, userQuery, finalBytes, time.Duration(p.cfg.Cache.DefaultTTLSeconds)*time.Second)
					}
				}
				return
			}
		}
	}

	// 5. Direct Upstream Fallback
	if p.router != nil && (routeAlias == "free-first" || p.router.DefaultStrategy() == "free-first") {
		telemetry.Log.Warn().
			Str("request_id", reqID).
			Msg("Router fallback exhausted under free-first strategy; direct upstream bypassed to prevent paid token spend")
		p.writeError(w, http.StatusBadGateway, "All free providers in fallback chain failed", "LILTOK_ROUTER_EXHAUSTED")
		return
	}

	var upstreamBaseURL string
	var upstreamAuthHeader string
	var upstreamAuthValue string

	authType := middleware.GetAuthType(r.Context())

	switch targetProvider {
	case "anthropic":
		upstreamBaseURL = strings.TrimRight(p.cfg.Providers.Anthropic.BaseURL, "/")
		if authKey != "" && !isVirtual {
			if authType == "bearer" {
				upstreamAuthHeader = "Authorization"
				upstreamAuthValue = "Bearer " + authKey
			} else {
				upstreamAuthHeader = "x-api-key"
				upstreamAuthValue = authKey
			}
		} else {
			upstreamAuthHeader = "x-api-key"
			upstreamAuthValue = p.cfg.Providers.Anthropic.APIKey
		}
	default: // "openai"
		upstreamBaseURL = strings.TrimRight(p.cfg.Providers.OpenAI.BaseURL, "/")
		upstreamAuthHeader = "Authorization"
		if authKey != "" && !isVirtual {
			upstreamAuthValue = "Bearer " + authKey
		} else {
			upstreamAuthValue = "Bearer " + p.cfg.Providers.OpenAI.APIKey
		}
	}

	targetURL := joinURLPath(upstreamBaseURL, defaultPath)
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		p.writeError(w, http.StatusInternalServerError, "Failed to create upstream request", "LILTOK_INTERNAL_ERROR")
		return
	}

	for k, v := range r.Header {
		if strings.EqualFold(k, "Authorization") || 
		   strings.EqualFold(k, "x-api-key") || 
		   strings.EqualFold(k, "Host") || 
		   strings.EqualFold(k, "Accept-Encoding") {
			continue
		}
		upstreamReq.Header[k] = v
	}

	if chatReq.Stream {
		upstreamReq.Header.Set("Accept-Encoding", "identity")
	}

	if upstreamAuthValue != "" {
		upstreamReq.Header.Set(upstreamAuthHeader, upstreamAuthValue)
	} else if targetProvider == "anthropic" || targetProvider == "openai" {
		telemetry.Log.Warn().
			Str("request_id", reqID).
			Str("provider", targetProvider).
			Msg("Upstream request rejected: missing upstream API key")
		p.writeError(w, http.StatusUnauthorized, fmt.Sprintf("Upstream %s API key is not configured. Please set 'providers.%s.api_key' in ~/.liltok/liltok.yaml or provide an upstream API key.", targetProvider, targetProvider), "missing_upstream_api_key")
		return
	}

	resp, err := p.httpClient.Do(upstreamReq)
	if err != nil {
		telemetry.Log.Error().
			Str("request_id", reqID).
			Str("provider", targetProvider).
			Err(err).
			Msg("Upstream HTTP request failed")
		p.writeError(w, http.StatusBadGateway, fmt.Sprintf("Upstream provider %s failed: %v", targetProvider, err), "LILTOK_UPSTREAM_FAILED")
		return
	}
	defer resp.Body.Close()

	duration := time.Since(startTime)

	w.Header().Set("X-Liltok-Request-Id", reqID)
	w.Header().Set("X-Liltok-Cache-Status", "MISS")
	w.Header().Set("X-Liltok-Cache-Tier", "NONE")
	w.Header().Set("X-Liltok-Provider", targetProvider)

	for k, v := range resp.Header {
		if (strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Content-Encoding")) && chatReq.Stream && resp.StatusCode == http.StatusOK {
			continue
		}
		w.Header()[k] = v
	}

	if resp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(resp.Body)

		// Check if Anthropic returned rate limit or 5-hour quota exhaustion (429), overloaded (529), or temporary outage (503)
		if targetProvider == "anthropic" && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529 || resp.StatusCode == http.StatusServiceUnavailable) && p.router != nil {
			telemetry.Log.Warn().
				Str("request_id", reqID).
				Int("status_code", resp.StatusCode).
				Msg("Anthropic limit reached (429/529/503); engaging automatic emergency failover to free providers")

			if unifiedReq, uErr := provider.ParseUnifiedRequest(bodyBytes, true); uErr == nil {
				if fbResp, winningProvider, fbErr := p.router.DispatchChat(r.Context(), unifiedReq, "free-first"); fbErr == nil {
					telemetry.Log.Info().
						Str("request_id", reqID).
						Str("winning_provider", winningProvider).
						Msg("Emergency failover from Anthropic limit succeeded")

					w.Header().Del("Content-Length")
					w.Header().Del("Content-Encoding")
					w.Header().Del("Retry-After")
					w.Header().Set("X-Liltok-Request-Id", reqID)
					w.Header().Set("X-Liltok-Cache-Status", "MISS")
					w.Header().Set("X-Liltok-Cache-Tier", "NONE")
					w.Header().Set("X-Liltok-Provider", winningProvider)

					finalBytes, _ := p.router.Translator().ConvertOpenAIToAnthropicResponse(fbResp, unifiedReq.Model)
					if chatReq.Stream {
						entry := &cache.CacheEntry{
							Model:           unifiedReq.Model,
							ResponsePayload: finalBytes,
						}
						_ = cache.ReplayCacheHitWithTier(w, entry, true, true, "NONE", 0)
					} else {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						_, _ = w.Write(finalBytes)
					}

					appointedModel := fbResp.Model
					if appointedModel == "" {
						appointedModel = unifiedReq.Model
					}
					costBD := p.pricingReg.CalculateDetailedForRouting(unifiedReq.Model, winningProvider, fbResp.Usage.PromptTokens, fbResp.Usage.CompletionTokens, 0, "MISS", "NONE")

					recordLog(&ledger.RequestLog{
						RequestID:         reqID,
						APIKeyID:          apiKeyID,
						Model:             appointedModel,
						RequestedModel:    unifiedReq.Model,
						Provider:          winningProvider,
						CacheStatus:       "MISS",
						CacheTier:         "NONE",
						PromptTokens:      fbResp.Usage.PromptTokens,
						CompletionTokens:  fbResp.Usage.CompletionTokens,
						LatencyMs:         time.Since(startTime).Milliseconds(),
						CostUSD:           costBD.TotalCostUSD,
						PromptCostUSD:     costBD.PromptCostUSD,
						CompletionCostUSD: costBD.CompletionCostUSD,
						SavedUSD:          costBD.SavedUSD,
						StatusCode:        http.StatusOK,
					})
					return
				} else {
					telemetry.Log.Error().
						Str("request_id", reqID).
						Err(fbErr).
						Msg("Emergency failover from Anthropic limit failed")
				}
			}
		}

		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBytes)
		return
	}

	if chatReq.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(resp.StatusCode)

	modelName := "unknown"
	if normReq != nil {
		modelName = normReq.Model
	} else if chatReq.Model != "" {
		modelName = chatReq.Model
	}

	if chatReq.Stream {
		var anthropicCollector *AnthropicStreamCollector
		var openaiCollector *OpenAIStreamCollector

		if targetProvider == "anthropic" {
			anthropicCollector = NewAnthropicStreamCollector()
		} else {
			openaiCollector = NewOpenAIStreamCollector()
		}

		err := StreamSSE(w, resp.Body, func(chunk []byte) {
			lines := strings.Split(string(chunk), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "data: ") {
					data := strings.TrimPrefix(line, "data: ")
					if targetProvider == "anthropic" {
						anthropicCollector.FeedLine(data)
					} else {
						openaiCollector.FeedLine(data)
					}
				}
			}
		})

		if err != nil {
			telemetry.Log.Warn().
				Str("request_id", reqID).
				Err(err).
				Msg("Streaming client connection disconnected or aborted")
		} else if resp.StatusCode == http.StatusOK {
			var payloadBytes []byte
			var pTokens, cTokens int

			if targetProvider == "anthropic" {
				if anthropicCollector.HasContent() {
					payloadBytes, pTokens, cTokens = anthropicCollector.BuildMessage(modelName)
				}
			} else {
				if openaiCollector.HasContent() {
					payloadBytes = openaiCollector.BuildCompletion(modelName)
				}
			}

			var cachedTokens int
			extPrompt, extComp, extCached := extractUsage(payloadBytes, modelName)
			if pTokens == 0 {
				pTokens = extPrompt
			}
			if cTokens == 0 {
				cTokens = extComp
			}
			cachedTokens = extCached

			if pTokens == 0 && normReq != nil {
				pTokens = tokens.CountTokens(modelName, normReq.CanonicalJSON)
			}
			tier := "NONE"
			if cachedTokens > 0 {
				tier = "TIER2_PREFIX"
			}
			costBD := p.pricingReg.CalculateDetailedForRouting(modelName, targetProvider, pTokens, cTokens, cachedTokens, "MISS", tier)

			recordLog(&ledger.RequestLog{
				RequestID:         reqID,
				APIKeyID:          apiKeyID,
				Model:             modelName,
				RequestedModel:    modelName,
				Provider:          targetProvider,
				CacheStatus:       "MISS",
				CacheTier:         tier,
				PromptTokens:      pTokens,
				CompletionTokens:  cTokens,
				CachedTokens:      cachedTokens,
				LatencyMs:         duration.Milliseconds(),
				CostUSD:           costBD.TotalCostUSD,
				PromptCostUSD:     costBD.PromptCostUSD,
				CompletionCostUSD: costBD.CompletionCostUSD,
				SavedUSD:          costBD.SavedUSD,
				StatusCode:        resp.StatusCode,
			})

			if normReq != nil && normReq.IsCacheable && len(payloadBytes) > 0 {
				if p.cacheStore != nil {
					entry := &cache.CacheEntry{
						Hash:             normReq.Hash,
						Model:            normReq.Model,
						NormalizedPrompt: normReq.CanonicalJSON,
						ResponsePayload:  payloadBytes,
						PromptTokens:     pTokens,
						CompletionTokens: cTokens,
						TTLSeconds:       p.cfg.Cache.DefaultTTLSeconds,
					}
					_ = p.cacheStore.Set(context.Background(), entry)
				}
				if p.semanticCache != nil && userQuery != "" {
					_ = p.semanticCache.Store(context.Background(), normReq.Hash, normReq.Model, systemPrompt, toolsJSON, userQuery, payloadBytes, time.Duration(p.cfg.Cache.DefaultTTLSeconds)*time.Second)
				}
			} else if len(payloadBytes) == 0 {
				telemetry.Log.Warn().
					Str("request_id", reqID).
					Str("model", modelName).
					Msg("Stream response contained no valid content blocks; skipped caching empty response")
			}
		}
	} else {
		respBytes, err := io.ReadAll(resp.Body)
		if err == nil {
			_, _ = w.Write(respBytes)

			pTokens, cTokens, cachedTokens := extractUsage(respBytes, modelName)
			if pTokens == 0 && normReq != nil {
				pTokens = tokens.CountTokens(modelName, normReq.CanonicalJSON)
			}
			tier := "NONE"
			if cachedTokens > 0 {
				tier = "TIER2_PREFIX"
			}
			costBD := p.pricingReg.CalculateDetailedForRouting(modelName, targetProvider, pTokens, cTokens, cachedTokens, "MISS", tier)

			recordLog(&ledger.RequestLog{
				RequestID:         reqID,
				APIKeyID:          apiKeyID,
				Model:             modelName,
				RequestedModel:    modelName,
				Provider:          targetProvider,
				CacheStatus:       "MISS",
				CacheTier:         tier,
				PromptTokens:      pTokens,
				CompletionTokens:  cTokens,
				CachedTokens:      cachedTokens,
				LatencyMs:         duration.Milliseconds(),
				CostUSD:           costBD.TotalCostUSD,
				PromptCostUSD:     costBD.PromptCostUSD,
				CompletionCostUSD: costBD.CompletionCostUSD,
				SavedUSD:          costBD.SavedUSD,
				StatusCode:        resp.StatusCode,
			})

			if resp.StatusCode == http.StatusOK && normReq != nil && normReq.IsCacheable {
				if p.cacheStore != nil {
					entry := &cache.CacheEntry{
						Hash:             normReq.Hash,
						Model:            normReq.Model,
						NormalizedPrompt: normReq.CanonicalJSON,
						ResponsePayload:  respBytes,
						TTLSeconds:       p.cfg.Cache.DefaultTTLSeconds,
					}
					_ = p.cacheStore.Set(context.Background(), entry)
				}
				if p.semanticCache != nil && userQuery != "" {
					_ = p.semanticCache.Store(context.Background(), normReq.Hash, normReq.Model, systemPrompt, toolsJSON, userQuery, respBytes, time.Duration(p.cfg.Cache.DefaultTTLSeconds)*time.Second)
				}
			}
		}
	}

	telemetry.Log.Debug().
		Str("request_id", reqID).
		Str("provider", targetProvider).
		Int("upstream_status", resp.StatusCode).
		Dur("upstream_duration", duration).
		Bool("stream", chatReq.Stream).
		Msg("Upstream request completed")
}

func extractUsage(body []byte, model string) (promptTokens, completionTokens, cachedTokens int) {
	if len(body) == 0 {
		return 0, 0, 0
	}

	var obj struct {
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			InputTokens         int `json:"input_tokens"`
			OutputTokens        int `json:"output_tokens"`
			CacheReadTokens     int `json:"cache_read_input_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}

	if err := json.Unmarshal(body, &obj); err == nil {
		pTok := obj.Usage.PromptTokens
		if pTok == 0 {
			pTok = obj.Usage.InputTokens
		}
		cTok := obj.Usage.CompletionTokens
		if cTok == 0 {
			cTok = obj.Usage.OutputTokens
		}
		cachedTok := obj.Usage.CacheReadTokens
		if cachedTok == 0 {
			cachedTok = obj.Usage.PromptTokensDetails.CachedTokens
		}

		if cTok == 0 {
			var text string
			if len(obj.Choices) > 0 {
				text = obj.Choices[0].Message.Content
			} else if len(obj.Content) > 0 {
				text = obj.Content[0].Text
			}
			if text != "" {
				cTok = tokens.CountTokens(model, text)
			}
		}

		return pTok, cTok, cachedTok
	}
	return 0, 0, 0
}

func extractPromptDetails(body []byte) (systemPrompt, toolsJSON, userQuery string) {
	if len(body) == 0 {
		return "", "", ""
	}
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return "", "", ""
	}

	if tools, ok := root["tools"]; ok {
		if toolsBytes, err := json.Marshal(tools); err == nil {
			toolsJSON = string(toolsBytes)
		}
	}

	if sys, ok := root["system"]; ok {
		switch s := sys.(type) {
		case string:
			systemPrompt = s
		case []interface{}:
			var sb strings.Builder
			for _, b := range s {
				if bMap, ok := b.(map[string]interface{}); ok {
					if t, ok := bMap["text"].(string); ok {
						sb.WriteString(t)
						sb.WriteByte('\n')
					}
				}
			}
			systemPrompt = strings.TrimSpace(sb.String())
		}
	}

	if msgs, ok := root["messages"].([]interface{}); ok {
		for _, m := range msgs {
			msgMap, ok := m.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := msgMap["role"].(string)
			if (role == "system" || role == "developer") && systemPrompt == "" {
				if c, ok := msgMap["content"].(string); ok {
					systemPrompt = c
				}
			}
			if role == "user" {
				switch c := msgMap["content"].(type) {
				case string:
					userQuery = c
				case []interface{}:
					var sb strings.Builder
					for _, part := range c {
						if pMap, ok := part.(map[string]interface{}); ok {
							if t, ok := pMap["text"].(string); ok {
								sb.WriteString(t)
								sb.WriteByte('\n')
							}
						}
					}
					userQuery = strings.TrimSpace(sb.String())
				}
			}
		}
	}

	return systemPrompt, toolsJSON, userQuery
}

func (p *Proxy) writeError(w http.ResponseWriter, status int, msg, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	resp := middleware.ErrorResponse{
		Error: middleware.ErrorDetail{
			Message: msg,
			Type:    "gateway_error",
			Param:   nil,
			Code:    code,
		},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func joinURLPath(baseURL, path string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(baseURL, "/v1") && strings.HasPrefix(path, "/v1") {
		baseURL = strings.TrimSuffix(baseURL, "/v1")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return baseURL + path
}
