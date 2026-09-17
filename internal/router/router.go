package router

import (
	"context"
	"fmt"
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
	// 1. auto-resilient: Claude -> Groq -> Gemini Free -> NVIDIA NIM
	r.routes["auto-resilient"] = Route{
		ID:       "auto-resilient",
		Strategy: "fallback",
		Targets: []TargetSpec{
			{ProviderName: "anthropic", UpstreamModel: "claude-sonnet-5"},
			{ProviderName: "groq", UpstreamModel: "qwen/qwen3.8-27b"},
			{ProviderName: "gemini", UpstreamModel: "gemini-flash-latest"},
			{ProviderName: "nvidianim", UpstreamModel: "meta/llama-3.2-11b-vision-instruct"},
		},
	}

	// 2. free-first: Groq -> Gemini Free (3 Keys) -> NVIDIA NIM
	r.routes["free-first"] = Route{
		ID:       "free-first",
		Strategy: "free_first",
		Targets: []TargetSpec{
			{ProviderName: "groq", UpstreamModel: "qwen/qwen3.8-27b"},
			{ProviderName: "gemini", UpstreamModel: "gemini-flash-latest"},
			{ProviderName: "nvidianim", UpstreamModel: "meta/llama-3.2-11b-vision-instruct"},
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
		// Target Anthropic first, fallback to free targets
		return []TargetSpec{
			{ProviderName: "anthropic", UpstreamModel: requestedModel},
			{ProviderName: "groq", UpstreamModel: "qwen/qwen3.8-27b"},
			{ProviderName: "gemini", UpstreamModel: "gemini-flash-latest"},
			{ProviderName: "nvidianim", UpstreamModel: "meta/llama-3.2-11b-vision-instruct"},
		}
	}
	if strings.Contains(lowerModel, "llama") || strings.Contains(lowerModel, "free") {
		return r.routes["free-first"].Targets
	}

	// 6. Default to OpenAI target + fallback
	return []TargetSpec{
		{ProviderName: "openai", UpstreamModel: requestedModel},
		{ProviderName: "nvidianim", UpstreamModel: "meta/llama-3.3-70b-instruct"},
	}
}

// DispatchChat executes non-streaming chat with automatic failover across target specifications.
func (r *Router) DispatchChat(ctx context.Context, req *provider.UnifiedChatRequest, routeAlias string) (*provider.UnifiedChatResponse, string, error) {
	targets := r.ResolveTargets(req.Model, routeAlias)

	var lastErr error
	for _, target := range targets {
		p, exists := r.providers[target.ProviderName]
		if !exists {
			continue
		}

		cb := r.breakers[target.ProviderName]
		if !cb.Allow() {
			telemetry.Log.Warn().
				Str("provider", target.ProviderName).
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

		cb.RecordFailure()
		lastErr = err
		telemetry.Log.Warn().
			Str("failed_provider", target.ProviderName).
			Err(err).
			Msg("Provider failed, failing over to next target")
	}

	return nil, "", fmt.Errorf("all providers in fallback chain failed: %w", lastErr)
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
