package router

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/provider"
	"github.com/primaybr/liltok/internal/provider/anthropic"
	"github.com/primaybr/liltok/internal/provider/gemini"
	"github.com/primaybr/liltok/internal/provider/openai"
	"github.com/primaybr/liltok/internal/telemetry"
)

// TargetSpec defines a provider and specific upstream model in a fallback chain.
type TargetSpec struct {
	ProviderName  string
	UpstreamModel string
}

// Route defines an ordered fallback sequence of provider targets.
type Route struct {
	ID       string
	Strategy string
	Targets  []TargetSpec
}

// Router orchestrates multi-provider dispatching, circuit breaking, and resilient fallbacks.
type Router struct {
	mu         sync.RWMutex
	cfg        *config.Config
	providers  map[string]provider.ProviderClient
	breakers   map[string]*CircuitBreaker
	routes     map[string]Route
	translator *Translator
}

// NewRouter initializes the router with configured provider clients and default fallback routes.
func NewRouter(cfg *config.Config) *Router {
	r := &Router{
		cfg:        cfg,
		providers:  make(map[string]provider.ProviderClient),
		breakers:   make(map[string]*CircuitBreaker),
		routes:     make(map[string]Route),
		translator: NewTranslator(),
	}

	// Register Standard Providers
	r.registerProvider(openai.NewAdapter("openai", provider.TierPremium, cfg.Providers.OpenAI.BaseURL, cfg.Providers.OpenAI.APIKey))
	r.registerProvider(anthropic.NewAdapter(cfg.Providers.Anthropic.BaseURL, cfg.Providers.Anthropic.APIKey))

	nimURL := cfg.Providers.NVIDIANIM.BaseURL
	if nimURL == "" {
		nimURL = "https://integrate.api.nvidia.com/v1"
	}
	r.registerProvider(openai.NewAdapter("nvidianim", provider.TierFree, nimURL, cfg.Providers.NVIDIANIM.APIKey))

	groqURL := cfg.Providers.Groq.BaseURL
	if groqURL == "" {
		groqURL = "https://api.groq.com/openai/v1"
	}
	r.registerProvider(openai.NewAdapter("groq", provider.TierFree, groqURL, cfg.Providers.Groq.APIKey))

	r.registerProvider(gemini.NewAdapter(cfg.Providers.Gemini.BaseURL, cfg.Providers.Gemini.APIKey))

	orURL := cfg.Providers.OpenRouter.BaseURL
	if orURL == "" {
		orURL = "https://openrouter.ai/api/v1"
	}
	r.registerProvider(openai.NewOpenRouterAdapter(cfg.Providers.OpenRouter.APIKey, orURL))

	r.registerProvider(openai.NewOllamaAdapter(cfg.Providers.Ollama.BaseURL))

	// Register Default Fallback Routes
	r.initDefaultRoutes()

	return r
}

func (r *Router) registerProvider(client provider.ProviderClient) {
	name := client.Name()
	r.providers[name] = client
	r.breakers[name] = NewCircuitBreaker(name)
}

func (r *Router) initDefaultRoutes() {
	// 1. auto-resilient: Claude -> Groq (Qwen -> GPT-120B -> GPT-20B) -> Gemini (3.8 -> 3.7 -> 3.6 -> 3.5-lite) -> NVIDIA NIM (Llama-11B -> Nemotron 30B/120B -> Poolside -> GPT-20B -> Nemotron Omni/550B)
	r.routes["auto-resilient"] = Route{
		ID:       "auto-resilient",
		Strategy: "fallback",
		Targets: []TargetSpec{
			{ProviderName: "anthropic", UpstreamModel: "claude-sonnet-5"},
			{ProviderName: "groq", UpstreamModel: "qwen/qwen3.8-27b"},
			{ProviderName: "groq", UpstreamModel: "openai/gpt-oss-120b"},
			{ProviderName: "groq", UpstreamModel: "openai/gpt-oss-20b"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.8-flash"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.7-flash"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.6-flash"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.5-flash-lite"},
			{ProviderName: "nvidianim", UpstreamModel: "meta/llama-3.2-11b-vision-instruct"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3.5-lightning-30b-a3b"},
			{ProviderName: "nvidianim", UpstreamModel: "poolside/laguna-xs-2.1"},
			{ProviderName: "nvidianim", UpstreamModel: "google/diffusiongemma-26b-a4b-it"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3-super-120b-a12b"},
			{ProviderName: "nvidianim", UpstreamModel: "openai/gpt-oss-20b"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning"},
			{ProviderName: "nvidianim", UpstreamModel: "meta/muse-glimmer-30b"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3-ultra-550b-a55b"},
			{ProviderName: "openrouter", UpstreamModel: "openrouter/free"},
		},
	}

	// 2. free-first: Groq -> Gemini Free (3 Keys) -> NVIDIA NIM -> OpenRouter Free
	r.routes["free-first"] = Route{
		ID:       "free-first",
		Strategy: "free_first",
		Targets: []TargetSpec{
			{ProviderName: "groq", UpstreamModel: "qwen/qwen3.8-27b"},
			{ProviderName: "groq", UpstreamModel: "openai/gpt-oss-120b"},
			{ProviderName: "groq", UpstreamModel: "openai/gpt-oss-20b"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.8-flash"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.7-flash"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.6-flash"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.5-flash-lite"},
			{ProviderName: "nvidianim", UpstreamModel: "meta/llama-3.2-11b-vision-instruct"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3.5-lightning-30b-a3b"},
			{ProviderName: "nvidianim", UpstreamModel: "poolside/laguna-xs-2.1"},
			{ProviderName: "nvidianim", UpstreamModel: "google/diffusiongemma-26b-a4b-it"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3-super-120b-a12b"},
			{ProviderName: "nvidianim", UpstreamModel: "openai/gpt-oss-20b"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning"},
			{ProviderName: "nvidianim", UpstreamModel: "meta/muse-glimmer-30b"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3-ultra-550b-a55b"},
			{ProviderName: "openrouter", UpstreamModel: "openrouter/free"},
		},
	}

	// 3. premium-only: Direct Frontier API
	r.routes["premium-only"] = Route{
		ID:       "premium-only",
		Strategy: "fallback",
		Targets: []TargetSpec{
			{ProviderName: "anthropic", UpstreamModel: "claude-opus-5"},
			{ProviderName: "openai", UpstreamModel: "gpt-4o"},
		},
	}

	// Initialize target-level circuit breakers for all targets in routes
	for _, route := range r.routes {
		for _, target := range route.Targets {
			key := target.ProviderName + "/" + target.UpstreamModel
			if _, exists := r.breakers[key]; !exists {
				r.breakers[key] = NewCircuitBreaker(key)
			}
		}
	}
}

// DefaultStrategy returns the active default routing strategy.
func (r *Router) DefaultStrategy() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.cfg != nil && r.cfg.Routes.DefaultStrategy != "" {
		return r.cfg.Routes.DefaultStrategy
	}
	return "auto-resilient"
}

// SetDefaultStrategy dynamically updates the default routing strategy.
func (r *Router) SetDefaultStrategy(strategy string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.routes[strategy]; !exists {
		return fmt.Errorf("unknown routing strategy: %q", strategy)
	}

	if r.cfg != nil {
		r.cfg.Routes.DefaultStrategy = strategy
	}
	return nil
}

// ResolveTargets determines the ordered target list based on requested model and route alias.
func (r *Router) ResolveTargets(requestedModel, routeAlias string) []TargetSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// 1. Check explicit route alias header
	if routeAlias != "" {
		if route, exists := r.routes[routeAlias]; exists {
			return route.Targets
		}
	}

	// 2. Check if requested model matches a route name (e.g. "free-first", "premium-only", "auto-resilient")
	if route, exists := r.routes[requestedModel]; exists {
		return route.Targets
	}

	strategy := "auto-resilient"
	if r.cfg != nil && r.cfg.Routes.DefaultStrategy != "" {
		strategy = r.cfg.Routes.DefaultStrategy
	}

	// 3. If default strategy is explicitly free-first, always use free-first targets to reduce token consumption
	if strategy == "free-first" {
		return r.routes["free-first"].Targets
	}

	// 4. If default strategy is premium-only, return premium-only targets
	if strategy == "premium-only" {
		return r.routes["premium-only"].Targets
	}

	// 5. Specific model matching heuristics for auto-resilient mode
	lowerModel := strings.ToLower(requestedModel)
	if strings.Contains(lowerModel, "claude") {
		// Target Anthropic first, fallback to all rolling free targets
		targets := []TargetSpec{
			{ProviderName: "anthropic", UpstreamModel: requestedModel},
		}
		targets = append(targets, r.routes["free-first"].Targets...)
		return targets
	}
	if strings.HasPrefix(lowerModel, "openrouter/") {
		targets := []TargetSpec{
			{ProviderName: "openrouter", UpstreamModel: requestedModel},
		}
		targets = append(targets, r.routes["free-first"].Targets...)
		return targets
	}
	if strings.Contains(lowerModel, "llama") || strings.Contains(lowerModel, "free") {
		return r.routes["free-first"].Targets
	}

	// 6. Default to OpenAI target + fallback
	return []TargetSpec{
		{ProviderName: "openai", UpstreamModel: requestedModel},
		{ProviderName: "nvidianim", UpstreamModel: "meta/llama-3.2-11b-vision-instruct"},
		{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3.5-lightning-30b-a3b"},
		{ProviderName: "nvidianim", UpstreamModel: "poolside/laguna-xs-2.1"},
		{ProviderName: "nvidianim", UpstreamModel: "google/diffusiongemma-26b-a4b-it"},
		{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3-super-120b-a12b"},
		{ProviderName: "nvidianim", UpstreamModel: "openai/gpt-oss-20b"},
	}
}

// getTargetBreaker returns or initializes a circuit breaker for a specific provider/model target.
func (r *Router) getTargetBreaker(providerName, upstreamModel string) *CircuitBreaker {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := providerName
	if upstreamModel != "" {
		key = providerName + "/" + upstreamModel
	}

	cb, exists := r.breakers[key]
	if !exists {
		cb = NewCircuitBreaker(key)
		r.breakers[key] = cb
	}
	return cb
}

// DispatchChat executes non-streaming chat with automatic failover across target specifications.
func (r *Router) DispatchChat(ctx context.Context, req *provider.UnifiedChatRequest, routeAlias string) (*provider.UnifiedChatResponse, string, error) {
	targets := r.ResolveTargets(req.Model, routeAlias)
	approxTokens := len(req.RawPayload) / 4
	if approxTokens == 0 {
		promptChars := len(req.SystemPrompt)
		for _, m := range req.Messages {
			promptChars += len(m.Content)
		}
		approxTokens = promptChars / 4
	}

	// Filter and prioritize targets based on token context requirements
	var candidateTargets []TargetSpec
	if approxTokens > 25000 {
		// Prompts > 25k tokens: prioritize 1M-context Gemini models first
		for _, t := range targets {
			if t.ProviderName == "gemini" {
				candidateTargets = append(candidateTargets, t)
			}
		}
		// If prompt fits within 120k tokens, include Groq/NVIDIA as secondary fallback
		if approxTokens <= 120000 {
			for _, t := range targets {
				if t.ProviderName != "gemini" && t.ProviderName != "anthropic" {
					candidateTargets = append(candidateTargets, t)
				}
			}
		}
		// If no candidates selected, default to original targets
		if len(candidateTargets) == 0 {
			candidateTargets = targets
		}
	} else {
		// Prompts <= 25k tokens: use default sequence (Groq fast tier first, then Gemini, then NVIDIA NIM)
		candidateTargets = targets
	}

	var lastErr error
	for _, target := range candidateTargets {
		// Strictly bypass providers whose physical context window cannot accommodate prompt
		if approxTokens > 120000 && (target.ProviderName == "groq" || (target.ProviderName == "nvidianim" && target.UpstreamModel != "nvidia/nemotron-3-ultra-550b-a55b")) {
			telemetry.Log.Debug().
				Str("provider", target.ProviderName).
				Str("model", target.UpstreamModel).
				Int("approx_tokens", approxTokens).
				Msg("Prompt exceeds provider context window; bypassing to large-context target")
			continue
		}

		p, exists := r.providers[target.ProviderName]
		if !exists {
			continue
		}

		cb := r.getTargetBreaker(target.ProviderName, target.UpstreamModel)
		if !cb.Allow() {
			telemetry.Log.Warn().
				Str("provider", target.ProviderName).
				Str("model", target.UpstreamModel).
				Msg("Circuit breaker OPEN, skipping target in fallback chain")
			continue
		}

		// Adjust request model for the specific target
		targetReq := *req
		targetReq.Model = target.UpstreamModel

		resp, err := p.SendChat(ctx, &targetReq)
		if err == nil {
			cb.RecordSuccess()
			return resp, target.ProviderName, nil
		}

		if isCircuitBreakerError(err) {
			cb.RecordFailure()
			// If rate limited or quota exhausted (429/RESOURCE_EXHAUSTED), trip immediately to allow rapid rolling failover
			errLower := strings.ToLower(err.Error())
			if strings.Contains(errLower, "429") || strings.Contains(errLower, "resource_exhausted") || strings.Contains(errLower, "quota") {
				cb.TripImmediate()
			}
		}
		lastErr = err
		telemetry.Log.Warn().
			Str("failed_provider", target.ProviderName).
			Str("failed_model", target.UpstreamModel).
			Err(err).
			Msg("Provider target failed, failing over to next target in rolling sequence")
	}

	return nil, "", fmt.Errorf("all providers in fallback chain failed: %w", lastErr)
}

// ResetCircuitBreakers resets all circuit breakers to CLOSED.
func (r *Router) ResetCircuitBreakers() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, cb := range r.breakers {
		cb.Reset()
	}
}

// ResetCircuitBreaker resets a single named circuit breaker to CLOSED.
func (r *Router) ResetCircuitBreaker(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cb, exists := r.breakers[strings.ToLower(name)]; exists && cb != nil {
		cb.Reset()
		return true
	}
	if cb, exists := r.breakers[name]; exists && cb != nil {
		cb.Reset()
		return true
	}
	return false
}

// isCircuitBreakerError returns true if the error indicates a downstream server outage,
// network timeout, or rate-limit exhaustion that should contribute to tripping the circuit breaker.
// Client errors (HTTP 400 Bad Request, 401 Unauthorized, 403 Forbidden, 404 Not Found)
// are client-side or payload issues, not provider infrastructure outages.
func isCircuitBreakerError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	if strings.Contains(errStr, "status 400") ||
		strings.Contains(errStr, "error 400") ||
		strings.Contains(errStr, "status 401") ||
		strings.Contains(errStr, "status 403") ||
		strings.Contains(errStr, "status 404") ||
		strings.Contains(errStr, "invalid_argument") {
		return false
	}
	return true
}


// GetProvider retrieves a registered provider client.
func (r *Router) GetProvider(name string) (provider.ProviderClient, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

// GetBreaker retrieves a provider's circuit breaker.
func (r *Router) GetBreaker(name string) (*CircuitBreaker, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.breakers[name]
	return b, ok
}

// CircuitBreakers returns a snapshot map of all registered circuit breakers.
func (r *Router) CircuitBreakers() map[string]*CircuitBreaker {
	r.mu.RLock()
	defer r.mu.RUnlock()
	res := make(map[string]*CircuitBreaker, len(r.breakers))
	for k, v := range r.breakers {
		res[k] = v
	}
	return res
}

// CircuitBreakerSnapshots returns an ordered snapshot slice of all active circuit breakers.
func (r *Router) CircuitBreakerSnapshots() []CircuitBreakerSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	res := make([]CircuitBreakerSnapshot, 0, len(r.breakers))
	for _, cb := range r.breakers {
		if cb != nil {
			res = append(res, cb.Snapshot())
		}
	}
	sort.Slice(res, func(i, j int) bool {
		iHasSlash := strings.Contains(res[i].Name, "/")
		jHasSlash := strings.Contains(res[j].Name, "/")
		if iHasSlash != jHasSlash {
			return !iHasSlash
		}
		return res[i].Name < res[j].Name
	})
	return res
}

// UpdateProvider dynamically updates credentials and base URL for a named provider.
func (r *Router) UpdateProvider(name string, creds config.ProviderCreds) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	name = strings.ToLower(name)
	if r.cfg != nil {
		switch name {
		case "openai":
			r.cfg.Providers.OpenAI = creds
		case "anthropic":
			r.cfg.Providers.Anthropic = creds
		case "nvidianim":
			r.cfg.Providers.NVIDIANIM = creds
		case "groq":
			r.cfg.Providers.Groq = creds
		case "gemini":
			r.cfg.Providers.Gemini = creds
		case "openrouter":
			r.cfg.Providers.OpenRouter = creds
		case "ollama":
			r.cfg.Providers.Ollama = creds
		}
	}

	p, exists := r.providers[name]
	if !exists {
		return fmt.Errorf("unknown provider: %s", name)
	}

	type keySetter interface {
		SetAPIKey(string)
	}
	type urlSetter interface {
		SetBaseURL(string)
	}

	if ks, ok := p.(keySetter); ok {
		ks.SetAPIKey(creds.APIKey)
	}
	if us, ok := p.(urlSetter); ok && creds.BaseURL != "" {
		us.SetBaseURL(creds.BaseURL)
	}

	// Reset circuit breaker to CLOSED when credentials are updated
	if cb, exists := r.breakers[name]; exists {
		cb.Reset()
	}

	return nil
}

// TestProvider runs an active health check against a provider and returns round-trip latency.
func (r *Router) TestProvider(ctx context.Context, name string) (bool, int64, error) {
	r.mu.RLock()
	p, exists := r.providers[strings.ToLower(name)]
	r.mu.RUnlock()

	if !exists {
		return false, 0, fmt.Errorf("unknown provider: %s", name)
	}

	start := time.Now()
	ok, err := p.CheckHealth(ctx)
	latencyMs := time.Since(start).Milliseconds()

	if ok && err == nil {
		if cb, ok := r.GetBreaker(name); ok {
			cb.RecordSuccess()
		}
		return true, latencyMs, nil
	}

	return false, latencyMs, err
}

// Translator returns the cross-protocol translator instance.
func (r *Router) Translator() *Translator {
	return r.translator
}
