package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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

// defaultGroqActiveModels provides the verified baseline active models from https://console.groq.com/docs/models
var defaultGroqActiveModels = []provider.ModelInfo{
	{ID: "openai/gpt-oss-120b", Provider: "groq", Active: true, ContextWindow: 131072, OwnedBy: "openai"},
	{ID: "openai/gpt-oss-20b", Provider: "groq", Active: true, ContextWindow: 131072, OwnedBy: "openai"},
	{ID: "qwen/qwen3.8-27b", Provider: "groq", Active: true, ContextWindow: 131042, OwnedBy: "qwen"},
	{ID: "groq/compound", Provider: "groq", Active: true, ContextWindow: 131072, OwnedBy: "groq"},
	{ID: "groq/compound-mini", Provider: "groq", Active: true, ContextWindow: 131072, OwnedBy: "groq"},
	{ID: "openai/gpt-oss-safeguard-20b", Provider: "groq", Active: true, ContextWindow: 131072, OwnedBy: "openai"},
	{ID: "allam-2-7b", Provider: "groq", Active: true, ContextWindow: 4096, OwnedBy: "allam"},
	{ID: "canopylabs/orpheus-v1-english", Provider: "groq", Active: true, ContextWindow: 4000, OwnedBy: "canopylabs"},
	{ID: "canopylabs/orpheus-arabic-saudi", Provider: "groq", Active: true, ContextWindow: 4000, OwnedBy: "canopylabs"},
	{ID: "meta-llama/llama-prompt-guard-2-22m", Provider: "groq", Active: true, ContextWindow: 512, OwnedBy: "meta-llama"},
	{ID: "meta-llama/llama-prompt-guard-2-86m", Provider: "groq", Active: true, ContextWindow: 512, OwnedBy: "meta-llama"},
	{ID: "whisper-large-v3", Provider: "groq", Active: true, ContextWindow: 448, OwnedBy: "openai"},
	{ID: "whisper-large-v3-turbo", Provider: "groq", Active: true, ContextWindow: 448, OwnedBy: "openai"},
}

// defaultNVIDIANIMActiveModels provides the verified baseline active free reasoning and chat models from build.nvidia.com
var defaultNVIDIANIMActiveModels = []provider.ModelInfo{
	{ID: "deepseek-ai/deepseek-v4-flash-0731", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "deepseek-ai"},
	{ID: "google/gemma-4-31b-it", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "google"},
	{ID: "nvidia/nemotron-3.5-lightning-30b-a3b", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "nvidia"},
	{ID: "nvidia/nemotron-3-super-120b-a12b", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "nvidia"},
	{ID: "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "nvidia"},
	{ID: "poolside/laguna-xs-2.1", Provider: "nvidianim", Active: true, ContextWindow: 65536, OwnedBy: "poolside"},
	{ID: "meta/llama-3.2-11b-vision-instruct", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "meta"},
	{ID: "meta/llama-3.2-90b-vision-instruct", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "meta"},
	{ID: "google/diffusiongemma-26b-a4b-it", Provider: "nvidianim", Active: true, ContextWindow: 32768, OwnedBy: "google"},
	{ID: "nvidia/nemotron-3-ultra-550b-a55b", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "nvidia"},
	{ID: "openai/gpt-oss-20b", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "openai"},
	{ID: "mistralai/mistral-nemotron", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "mistralai"},
	{ID: "z-ai/glm-5.3-flash", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "z-ai"},
	{ID: "z-ai/glm-5.3", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "z-ai"},
	{ID: "moonshotai/kimi-k3", Provider: "nvidianim", Active: true, ContextWindow: 32768, OwnedBy: "moonshotai"},
}

// defaultOpenRouterActiveModels provides the verified baseline active free reasoning and chat models from openrouter.ai/models
var defaultOpenRouterActiveModels = []provider.ModelInfo{
	{ID: "deepseek/deepseek-v4-flash-0731:free", Provider: "openrouter", Active: true, ContextWindow: 1048576, OwnedBy: "deepseek"},
	{ID: "google/gemma-4-31b-it:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "google"},
	{ID: "qwen/qwen3.8-27b:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "qwen"},
	{ID: "nvidia/nemotron-3.5-lightning:free", Provider: "openrouter", Active: true, ContextWindow: 1000000, OwnedBy: "nvidia"},
	{ID: "nvidia/nemotron-3-super-120b-a12b:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "nvidia"},
	{ID: "nvidia/nemotron-3-ultra-550b-a55b:free", Provider: "openrouter", Active: true, ContextWindow: 1000000, OwnedBy: "nvidia"},
	{ID: "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free", Provider: "openrouter", Active: true, ContextWindow: 256000, OwnedBy: "nvidia"},
	{ID: "poolside/laguna-xs-2.1:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "poolside"},
	{ID: "poolside/laguna-s-2.1:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "poolside"},
	{ID: "thinkingmachines/inkling:free", Provider: "openrouter", Active: true, ContextWindow: 1048576, OwnedBy: "thinkingmachines"},
	{ID: "thinkingmachines/inkling-small:free", Provider: "openrouter", Active: true, ContextWindow: 1048576, OwnedBy: "thinkingmachines"},
	{ID: "nex-agi/nex-n2.5-pro:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "nex-agi"},
	{ID: "nex-agi/nex-n2.5-mini:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "nex-agi"},
	{ID: "cohere/north-mini-code:free", Provider: "openrouter", Active: true, ContextWindow: 256000, OwnedBy: "cohere"},
	{ID: "dots-studio/dots-3-note-preview:free", Provider: "openrouter", Active: true, ContextWindow: 512000, OwnedBy: "dots-studio"},
	{ID: "liquid/lfm-2.5-2.6b:free", Provider: "openrouter", Active: true, ContextWindow: 65536, OwnedBy: "liquid"},
	{ID: "inclusionai/ling-3.0-flash-vl:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "inclusionai"},
	{ID: "inclusionai/ling-3.0-flash-sante:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "inclusionai"},
	{ID: "inclusionai/ling-3.0-flash-fin:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "inclusionai"},
	{ID: "google/gemma-4-26b-a4b-it:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "google"},
	{ID: "z-ai/glm-5.2:free", Provider: "openrouter", Active: true, ContextWindow: 32768, OwnedBy: "z-ai"},
	{ID: "openrouter/free", Provider: "openrouter", Active: true, ContextWindow: 200000, OwnedBy: "openrouter"},
}

// defaultKiloActiveModels provides the verified baseline active free models from api.kilo.ai/api/gateway/models
var defaultKiloActiveModels = []provider.ModelInfo{
	{ID: "kilo-auto/free", Provider: "kilo", Active: true, ContextWindow: 256000, OwnedBy: "kilo"},
	{ID: "deepseek/deepseek-v4-flash-0731:free", Provider: "kilo", Active: true, ContextWindow: 1048576, OwnedBy: "deepseek"},
	{ID: "nvidia/nemotron-3.5-lightning:free", Provider: "kilo", Active: true, ContextWindow: 1000000, OwnedBy: "nvidia"},
	{ID: "qwen/qwen3.8-27b:free", Provider: "kilo", Active: true, ContextWindow: 262144, OwnedBy: "qwen"},
	{ID: "nvidia/nemotron-3-ultra-550b-a55b:free", Provider: "kilo", Active: true, ContextWindow: 1000000, OwnedBy: "nvidia"},
	{ID: "cohere/north-mini-code:free", Provider: "kilo", Active: true, ContextWindow: 256000, OwnedBy: "cohere"},
	{ID: "poolside/laguna-xs-2.1:free", Provider: "kilo", Active: true, ContextWindow: 262144, OwnedBy: "poolside"},
	{ID: "poolside/laguna-s-2.1:free", Provider: "kilo", Active: true, ContextWindow: 262144, OwnedBy: "poolside"},
	{ID: "dots-studio/dots-3-note-preview:free", Provider: "kilo", Active: true, ContextWindow: 512000, OwnedBy: "dots-studio"},
	{ID: "thinkingmachines/inkling-small:free", Provider: "kilo", Active: true, ContextWindow: 1048576, OwnedBy: "thinkingmachines"},
	{ID: "stepfun/step-3.7-flash:free", Provider: "kilo", Active: true, ContextWindow: 262144, OwnedBy: "stepfun"},
	{ID: "nex-agi/nex-n2.5-pro:free", Provider: "kilo", Active: true, ContextWindow: 262144, OwnedBy: "nex-agi"},
	{ID: "nex-agi/nex-n2.5-mini:free", Provider: "kilo", Active: true, ContextWindow: 262144, OwnedBy: "nex-agi"},
	{ID: "inclusionai/ling-3.0-flash-vl:free", Provider: "kilo", Active: true, ContextWindow: 262144, OwnedBy: "inclusionai"},
	{ID: "openrouter/free", Provider: "kilo", Active: true, ContextWindow: 200000, OwnedBy: "openrouter"},
}

// defaultClineActiveModels provides the verified baseline active free reasoning and chat models from api.cline.bot/api/v1/models
var defaultClineActiveModels = []provider.ModelInfo{
	{ID: "nvidia/nemotron-3.5-lightning:free", Provider: "cline", Active: true, ContextWindow: 1000000, OwnedBy: "nvidia"},
	{ID: "google/gemma-4-31b-it:free", Provider: "cline", Active: true, ContextWindow: 262144, OwnedBy: "google"},
	{ID: "qwen/qwen3.8-27b:free", Provider: "cline", Active: true, ContextWindow: 262144, OwnedBy: "qwen"},
	{ID: "nvidia/nemotron-3-super-120b-a12b:free", Provider: "cline", Active: true, ContextWindow: 262144, OwnedBy: "nvidia"},
	{ID: "nvidia/nemotron-3-ultra-550b-a55b:free", Provider: "cline", Active: true, ContextWindow: 1000000, OwnedBy: "nvidia"},
	{ID: "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free", Provider: "cline", Active: true, ContextWindow: 256000, OwnedBy: "nvidia"},
	{ID: "poolside/laguna-xs-2.1:free", Provider: "cline", Active: true, ContextWindow: 262144, OwnedBy: "poolside"},
	{ID: "poolside/laguna-s-2.1:free", Provider: "cline", Active: true, ContextWindow: 262144, OwnedBy: "poolside"},
	{ID: "thinkingmachines/inkling:free", Provider: "cline", Active: true, ContextWindow: 1048576, OwnedBy: "thinkingmachines"},
	{ID: "thinkingmachines/inkling-small:free", Provider: "cline", Active: true, ContextWindow: 1048576, OwnedBy: "thinkingmachines"},
	{ID: "nex-agi/nex-n2.5-pro:free", Provider: "cline", Active: true, ContextWindow: 262144, OwnedBy: "nex-agi"},
	{ID: "nex-agi/nex-n2.5-mini:free", Provider: "cline", Active: true, ContextWindow: 262144, OwnedBy: "nex-agi"},
	{ID: "cohere/north-mini-code:free", Provider: "cline", Active: true, ContextWindow: 256000, OwnedBy: "cohere"},
	{ID: "dots-studio/dots-3-note-preview:free", Provider: "cline", Active: true, ContextWindow: 512000, OwnedBy: "dots-studio"},
	{ID: "liquid/lfm-2.5-2.6b:free", Provider: "cline", Active: true, ContextWindow: 65536, OwnedBy: "liquid"},
	{ID: "inclusionai/ling-3.0-flash-vl:free", Provider: "cline", Active: true, ContextWindow: 262144, OwnedBy: "inclusionai"},
	{ID: "inclusionai/ling-3.0-flash-sante:free", Provider: "cline", Active: true, ContextWindow: 262144, OwnedBy: "inclusionai"},
	{ID: "inclusionai/ling-3.0-flash-fin:free", Provider: "cline", Active: true, ContextWindow: 262144, OwnedBy: "inclusionai"},
	{ID: "google/gemma-4-26b-a4b-it:free", Provider: "cline", Active: true, ContextWindow: 262144, OwnedBy: "google"},
	{ID: "z-ai/glm-5.2:free", Provider: "cline", Active: true, ContextWindow: 32768, OwnedBy: "z-ai"},
	{ID: "deepseek/deepseek-v4-flash-0731:free", Provider: "cline", Active: false, ContextWindow: 1048576, OwnedBy: "deepseek"},
}

// catalogedProviders list their models; targets on them are dispatched only for models the
// listing reports as active.
var catalogedProviders = map[string]bool{"groq": true, "nvidianim": true, "openrouter": true, "kilo": true, "cline": true}

// Route defines an ordered fallback sequence of provider targets.
type Route struct {
	ID          string
	Strategy    string
	Description string
	Targets     []TargetSpec
}

// builtInRouteOrder lists the routes liltok ships with, in display order.
var builtInRouteOrder = []string{"auto-resilient", "free-first", "premium-only"}

// Router orchestrates multi-provider dispatching, circuit breaking, and resilient fallbacks.
type Router struct {
	mu           sync.RWMutex
	cfg          *config.Config
	providers    map[string]provider.ProviderClient
	breakers     map[string]*CircuitBreaker
	routes       map[string]Route
	translator   *Translator
	activeModels map[string][]provider.ModelInfo
	modelsMu     sync.RWMutex

	// attemptTimeout bounds each non-premium upstream attempt; 0 means no per-attempt limit.
	attemptTimeout time.Duration
	// excluded and lastResort hold normalized routes.excluded_models and routes.last_resort_models entries.
	excluded   map[string]bool
	lastResort map[string]bool
	// catalog holds model health learned from upstream "model not found" replies.
	catalog *ModelCatalog
	// targetCost prices targets for least_cost routes; nil uses provider tiers.
	targetCost TargetCostFunc
	// roundRobin maps a round_robin route ID to its *atomic.Uint64 request counter.
	roundRobin sync.Map
	// builtInRoutes keeps the shipped route definitions so an edited built-in route can be reset.
	builtInRoutes map[string]Route
	// routeStore persists routes edited at runtime; nil keeps edits in memory only.
	routeStore RouteStore
}

// NewRouter initializes the router with configured provider clients and default fallback routes.
func NewRouter(cfg *config.Config) *Router {
	r := &Router{
		cfg:          cfg,
		providers:    make(map[string]provider.ProviderClient),
		breakers:     make(map[string]*CircuitBreaker),
		routes:       make(map[string]Route),
		translator:   NewTranslator(),
		activeModels: make(map[string][]provider.ModelInfo),
	}
	if cfg != nil && cfg.Routes.AttemptTimeoutSeconds > 0 {
		r.attemptTimeout = time.Duration(cfg.Routes.AttemptTimeoutSeconds) * time.Second
	}
	r.excluded, r.lastResort = make(map[string]bool), make(map[string]bool)
	recheck := time.Duration(0)
	if cfg != nil && cfg.Routes.ModelRecheckHours > 0 {
		recheck = time.Duration(cfg.Routes.ModelRecheckHours) * time.Hour
	}
	r.catalog = NewModelCatalog(recheck)
	if cfg != nil {
		for _, m := range cfg.Routes.ExcludedModels {
			if n := normalizeModelID(m); n != "" {
				r.excluded[n] = true
			}
		}
		for _, m := range cfg.Routes.LastResortModels {
			if n := normalizeModelID(m); n != "" {
				r.lastResort[n] = true
			}
		}
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

	kiloURL := cfg.Providers.Kilo.BaseURL
	if kiloURL == "" {
		kiloURL = "https://api.kilo.ai/api/gateway"
	}
	r.registerProvider(openai.NewKiloAdapter(cfg.Providers.Kilo.APIKey, kiloURL))

	clineURL := cfg.Providers.Cline.BaseURL
	if clineURL == "" {
		clineURL = "https://api.cline.bot/api/v1"
	}
	r.registerProvider(openai.NewClineAdapter(cfg.Providers.Cline.APIKey, clineURL))

	// Register Default Fallback Routes
	r.initDefaultRoutes()

	// Seed Groq active models and start non-blocking discovery
	r.seedActiveModels()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = r.SyncAllProviderModels(ctx)
	}()

	return r
}

func (r *Router) registerProvider(client provider.ProviderClient) {
	name := client.Name()
	r.providers[name] = client
	r.breakers[name] = NewCircuitBreaker(name)
}

func (r *Router) initDefaultRoutes() {
	// 1. auto-resilient: Claude -> Groq (Qwen -> GPT-120B -> GPT-20B) -> Gemini (3.8 -> 3.7 -> 3.6; 3.5-lite as last resort) -> NVIDIA NIM (Llama-11B -> Nemotron 30B/120B -> Poolside -> GPT-20B -> Nemotron Omni/550B) -> OpenRouter -> Kilo
	r.routes["auto-resilient"] = Route{
		ID:          "auto-resilient",
		Strategy:    StrategyFallback,
		Description: "Claude Sonnet 5 first, then the free tiers: Groq, Gemini (3.5 Flash-Lite as last resort), NVIDIA NIM, OpenRouter, Kilo and Cline",
		Targets: []TargetSpec{
			{ProviderName: "anthropic", UpstreamModel: "claude-sonnet-5"},
			{ProviderName: "groq", UpstreamModel: "qwen/qwen3.8-27b"},
			{ProviderName: "groq", UpstreamModel: "openai/gpt-oss-120b"},
			{ProviderName: "groq", UpstreamModel: "openai/gpt-oss-20b"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.8-flash"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.7-flash"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.6-flash"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.5-flash-lite"}, // last resort, see routes.last_resort_models
			{ProviderName: "nvidianim", UpstreamModel: "deepseek-ai/deepseek-v4-flash-0731"},
			{ProviderName: "nvidianim", UpstreamModel: "google/gemma-4-31b-it"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3.5-lightning-30b-a3b"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3-super-120b-a12b"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning"},
			{ProviderName: "nvidianim", UpstreamModel: "meta/llama-3.2-11b-vision-instruct"},
			{ProviderName: "nvidianim", UpstreamModel: "poolside/laguna-xs-2.1"},
			{ProviderName: "nvidianim", UpstreamModel: "google/diffusiongemma-26b-a4b-it"},
			{ProviderName: "nvidianim", UpstreamModel: "openai/gpt-oss-20b"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3-ultra-550b-a55b"},
			{ProviderName: "openrouter", UpstreamModel: "deepseek/deepseek-v4-flash-0731:free"},
			{ProviderName: "openrouter", UpstreamModel: "nvidia/nemotron-3.5-lightning:free"},
			{ProviderName: "openrouter", UpstreamModel: "google/gemma-4-31b-it:free"},
			{ProviderName: "openrouter", UpstreamModel: "openrouter/free"},
			{ProviderName: "kilo", UpstreamModel: "kilo-auto/free"},
			{ProviderName: "kilo", UpstreamModel: "deepseek/deepseek-v4-flash-0731:free"},
			{ProviderName: "kilo", UpstreamModel: "nvidia/nemotron-3.5-lightning:free"},
			{ProviderName: "cline", UpstreamModel: "deepseek/deepseek-v4-flash-0731:free"},
			{ProviderName: "cline", UpstreamModel: "nvidia/nemotron-3.5-lightning:free"},
			{ProviderName: "cline", UpstreamModel: "qwen/qwen3.8-27b:free"},
		},
	}

	// 2. free-first: Groq -> Gemini Free (3 Keys) -> NVIDIA NIM -> OpenRouter Free -> Kilo Free -> Cline Free
	r.routes["free-first"] = Route{
		ID:          "free-first",
		Strategy:    StrategyFreeFirst,
		Description: "Free tiers only: Groq, Gemini (3.5 Flash-Lite as last resort), NVIDIA NIM, OpenRouter, Kilo and Cline",
		Targets: []TargetSpec{
			{ProviderName: "groq", UpstreamModel: "qwen/qwen3.8-27b"},
			{ProviderName: "groq", UpstreamModel: "openai/gpt-oss-120b"},
			{ProviderName: "groq", UpstreamModel: "openai/gpt-oss-20b"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.8-flash"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.7-flash"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.6-flash"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.5-flash-lite"}, // last resort, see routes.last_resort_models
			{ProviderName: "nvidianim", UpstreamModel: "deepseek-ai/deepseek-v4-flash-0731"},
			{ProviderName: "nvidianim", UpstreamModel: "google/gemma-4-31b-it"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3.5-lightning-30b-a3b"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3-super-120b-a12b"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning"},
			{ProviderName: "nvidianim", UpstreamModel: "meta/llama-3.2-11b-vision-instruct"},
			{ProviderName: "nvidianim", UpstreamModel: "poolside/laguna-xs-2.1"},
			{ProviderName: "nvidianim", UpstreamModel: "google/diffusiongemma-26b-a4b-it"},
			{ProviderName: "nvidianim", UpstreamModel: "openai/gpt-oss-20b"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3-ultra-550b-a55b"},
			{ProviderName: "openrouter", UpstreamModel: "deepseek/deepseek-v4-flash-0731:free"},
			{ProviderName: "openrouter", UpstreamModel: "nvidia/nemotron-3.5-lightning:free"},
			{ProviderName: "openrouter", UpstreamModel: "google/gemma-4-31b-it:free"},
			{ProviderName: "openrouter", UpstreamModel: "openrouter/free"},
			{ProviderName: "kilo", UpstreamModel: "kilo-auto/free"},
			{ProviderName: "kilo", UpstreamModel: "deepseek/deepseek-v4-flash-0731:free"},
			{ProviderName: "kilo", UpstreamModel: "nvidia/nemotron-3.5-lightning:free"},
			{ProviderName: "cline", UpstreamModel: "deepseek/deepseek-v4-flash-0731:free"},
			{ProviderName: "cline", UpstreamModel: "nvidia/nemotron-3.5-lightning:free"},
			{ProviderName: "cline", UpstreamModel: "qwen/qwen3.8-27b:free"},
		},
	}

	// 3. premium-only: Direct Frontier API
	r.routes["premium-only"] = Route{
		ID:          "premium-only",
		Strategy:    StrategyFallback,
		Description: "Paid frontier models: Claude Opus 5.5, then GPT-4o",
		Targets: []TargetSpec{
			{ProviderName: "anthropic", UpstreamModel: "claude-opus-5-5"},
			{ProviderName: "openai", UpstreamModel: "gpt-4o"},
		},
	}

	r.builtInRoutes = make(map[string]Route, len(r.routes))
	for id, route := range r.routes {
		r.builtInRoutes[id] = copyRoute(route)
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

	lowerModel := strings.ToLower(requestedModel)

	// 3. Explicit provider prefix matching (e.g. "groq/...", "nvidianim/...", "openrouter/...")
	if strings.HasPrefix(lowerModel, "groq/") {
		actualModel := requestedModel
		if strings.HasPrefix(lowerModel, "groq/") {
			candidate := strings.TrimPrefix(requestedModel, "groq/")
			for _, m := range r.GetProviderActiveModels("groq") {
				if strings.EqualFold(m.ID, candidate) {
					actualModel = m.ID
					break
				}
			}
		}
		targets := []TargetSpec{
			{ProviderName: "groq", UpstreamModel: actualModel},
		}
		for _, t := range r.routes["free-first"].Targets {
			if t.ProviderName == "groq" && t.UpstreamModel == actualModel {
				continue
			}
			targets = append(targets, t)
		}
		return targets
	}

	if strings.HasPrefix(lowerModel, "nvidianim/") {
		actualModel := requestedModel
		if strings.HasPrefix(lowerModel, "nvidianim/") {
			candidate := strings.TrimPrefix(requestedModel, "nvidianim/")
			for _, m := range r.GetProviderActiveModels("nvidianim") {
				if strings.EqualFold(m.ID, candidate) {
					actualModel = m.ID
					break
				}
			}
		}
		targets := []TargetSpec{
			{ProviderName: "nvidianim", UpstreamModel: actualModel},
		}
		for _, t := range r.routes["free-first"].Targets {
			if t.ProviderName == "nvidianim" && t.UpstreamModel == actualModel {
				continue
			}
			targets = append(targets, t)
		}
		return targets
	}

	if strings.HasPrefix(lowerModel, "cline/") {
		actualModel := requestedModel
		candidate := strings.TrimPrefix(requestedModel, "cline/")
		for _, m := range r.GetProviderActiveModels("cline") {
			if strings.EqualFold(m.ID, candidate) {
				actualModel = m.ID
				break
			}
		}
		targets := []TargetSpec{
			{ProviderName: "cline", UpstreamModel: actualModel},
		}
		for _, t := range r.routes["free-first"].Targets {
			if t.ProviderName == "cline" && t.UpstreamModel == actualModel {
				continue
			}
			targets = append(targets, t)
		}
		return targets
	}

	if strings.HasPrefix(lowerModel, "openrouter/") || strings.HasSuffix(lowerModel, ":free") || isModelAlias("openrouter", requestedModel) {
		actualModel := requestedModel
		if strings.HasPrefix(lowerModel, "openrouter/") {
			candidate := strings.TrimPrefix(requestedModel, "openrouter/")
			for _, m := range r.GetProviderActiveModels("openrouter") {
				if strings.EqualFold(m.ID, candidate) {
					actualModel = m.ID
					break
				}
			}
		}
		if repl, isAlias := ResolveModelAlias("openrouter", actualModel); isAlias {
			actualModel = repl
		}
		targets := []TargetSpec{
			{ProviderName: "openrouter", UpstreamModel: actualModel},
		}
		for _, t := range r.routes["free-first"].Targets {
			if t.ProviderName == "openrouter" && t.UpstreamModel == actualModel {
				continue
			}
			targets = append(targets, t)
		}
		return targets
	}

	if strings.HasPrefix(lowerModel, "kilo/") || lowerModel == "kilo-auto/free" || isModelAlias("kilo", requestedModel) {
		actualModel := requestedModel
		if strings.HasPrefix(lowerModel, "kilo/") {
			candidate := strings.TrimPrefix(requestedModel, "kilo/")
			for _, m := range r.GetProviderActiveModels("kilo") {
				if strings.EqualFold(m.ID, candidate) {
					actualModel = m.ID
					break
				}
			}
		}
		if repl, isAlias := ResolveModelAlias("kilo", actualModel); isAlias {
			actualModel = repl
		}
		targets := []TargetSpec{
			{ProviderName: "kilo", UpstreamModel: actualModel},
		}
		for _, t := range r.routes["free-first"].Targets {
			if t.ProviderName == "kilo" && t.UpstreamModel == actualModel {
				continue
			}
			targets = append(targets, t)
		}
		return targets
	}

	// 4. If default strategy is explicitly free-first, use free-first targets to reduce token consumption
	if strategy == "free-first" {
		return r.routes["free-first"].Targets
	}

	// 5. If default strategy is premium-only, return premium-only targets
	if strategy == "premium-only" {
		return r.routes["premium-only"].Targets
	}

	// 6. Specific model matching heuristics for auto-resilient mode
	if strings.Contains(lowerModel, "claude") {
		// Target Anthropic first, fallback to all rolling free targets
		targets := []TargetSpec{
			{ProviderName: "anthropic", UpstreamModel: requestedModel},
		}
		targets = append(targets, r.routes["free-first"].Targets...)
		return targets
	}

	if r.IsActiveModel("groq", requestedModel) {
		targets := []TargetSpec{
			{ProviderName: "groq", UpstreamModel: requestedModel},
		}
		for _, t := range r.routes["free-first"].Targets {
			if t.ProviderName == "groq" && t.UpstreamModel == requestedModel {
				continue
			}
			targets = append(targets, t)
		}
		return targets
	}

	if r.IsActiveModel("nvidianim", requestedModel) {
		targets := []TargetSpec{
			{ProviderName: "nvidianim", UpstreamModel: requestedModel},
		}
		for _, t := range r.routes["free-first"].Targets {
			if t.ProviderName == "nvidianim" && t.UpstreamModel == requestedModel {
				continue
			}
			targets = append(targets, t)
		}
		return targets
	}

	if r.IsActiveModel("openrouter", requestedModel) {
		actualModel := requestedModel
		if repl, isAlias := ResolveModelAlias("openrouter", actualModel); isAlias {
			actualModel = repl
		}
		targets := []TargetSpec{
			{ProviderName: "openrouter", UpstreamModel: actualModel},
		}
		for _, t := range r.routes["free-first"].Targets {
			if t.ProviderName == "openrouter" && t.UpstreamModel == actualModel {
				continue
			}
			targets = append(targets, t)
		}
		return targets
	}

	if r.IsActiveModel("kilo", requestedModel) {
		actualModel := requestedModel
		if repl, isAlias := ResolveModelAlias("kilo", actualModel); isAlias {
			actualModel = repl
		}
		targets := []TargetSpec{
			{ProviderName: "kilo", UpstreamModel: actualModel},
		}
		for _, t := range r.routes["free-first"].Targets {
			if t.ProviderName == "kilo" && t.UpstreamModel == actualModel {
				continue
			}
			targets = append(targets, t)
		}
		return targets
	}

	if r.IsActiveModel("cline", requestedModel) {
		actualModel := requestedModel
		targets := []TargetSpec{
			{ProviderName: "cline", UpstreamModel: actualModel},
		}
		for _, t := range r.routes["free-first"].Targets {
			if t.ProviderName == "cline" && t.UpstreamModel == actualModel {
				continue
			}
			targets = append(targets, t)
		}
		return targets
	}

	if strings.Contains(lowerModel, "llama") || strings.Contains(lowerModel, "free") {
		return r.routes["free-first"].Targets
	}

	// 6. Default to OpenAI target + fallback
	return []TargetSpec{
		{ProviderName: "openai", UpstreamModel: requestedModel},
		{ProviderName: "nvidianim", UpstreamModel: "deepseek-ai/deepseek-v4-flash-0731"},
		{ProviderName: "nvidianim", UpstreamModel: "google/gemma-4-31b-it"},
		{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3.5-lightning-30b-a3b"},
		{ProviderName: "nvidianim", UpstreamModel: "meta/llama-3.2-11b-vision-instruct"},
		{ProviderName: "nvidianim", UpstreamModel: "poolside/laguna-xs-2.1"},
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
	targets, routeID, strategy := r.resolveRoute(req.Model, routeAlias)
	targets = r.orderTargets(routeID, strategy, targets)
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
		// 1. High-Context Tier 1 (1M Context Windows): Gemini (if prompt fits free TPM limit), OpenRouter 1M, Kilo 1M, Cline 1M
		for _, t := range targets {
			if t.ProviderName != "anthropic" && t.ProviderName != "openai" {
				// Google Gemini Free Tier strictly limits input tokens to 250,000 per minute.
				// Prompts exceeding 250,000 tokens will immediately return 429 RESOURCE_EXHAUSTED.
				if t.ProviderName == "gemini" && approxTokens > 250000 {
					continue
				}
				ctxWin := r.GetModelContextWindow(t.ProviderName, t.UpstreamModel)
				if ctxWin >= 1000000 {
					candidateTargets = append(candidateTargets, t)
				}
			}
		}

		// 2. High-Context Tier 2 (>= 256K Context Windows): OpenRouter, Kilo, Cline
		for _, t := range targets {
			if t.ProviderName != "anthropic" && t.ProviderName != "openai" {
				ctxWin := r.GetModelContextWindow(t.ProviderName, t.UpstreamModel)
				if ctxWin >= approxTokens && ctxWin < 1000000 && ctxWin >= 256000 {
					candidateTargets = append(candidateTargets, t)
				}
			}
		}

		// 3. Medium-Context Tier 3 (128K Context Windows): Groq, NVIDIA NIM
		if approxTokens <= 120000 {
			for _, t := range targets {
				if t.ProviderName != "anthropic" && t.ProviderName != "openai" {
					ctxWin := r.GetModelContextWindow(t.ProviderName, t.UpstreamModel)
					if ctxWin >= approxTokens && ctxWin < 256000 {
						candidateTargets = append(candidateTargets, t)
					}
				}
			}
		}

		// 4. Paid Frontier Fallback: append Anthropic or OpenAI only if prompt fits physical context window
		for _, t := range targets {
			if t.ProviderName == "anthropic" || t.ProviderName == "openai" {
				ctxWin := r.GetModelContextWindow(t.ProviderName, t.UpstreamModel)
				if ctxWin >= approxTokens {
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
	candidateTargets = r.demoteLastResort(candidateTargets)

	var lastErr error
	for _, target := range candidateTargets {
		// Stop the chain once the client has gone: later attempts would fail instantly with
		// "context canceled" and nobody is left to receive a reply.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, "", fmt.Errorf("request canceled before reaching %s: %w", target.ProviderName, ctxErr)
		}

		// Strictly bypass providers whose physical context window cannot accommodate prompt
		ctxWin := r.GetModelContextWindow(target.ProviderName, target.UpstreamModel)
		if approxTokens > ctxWin {
			telemetry.Log.Debug().
				Str("provider", target.ProviderName).
				Str("model", target.UpstreamModel).
				Int("approx_tokens", approxTokens).
				Int("context_window", ctxWin).
				Msg("Prompt exceeds provider context window; bypassing to large-context target")
			continue
		}

		// Providers with a model listing: resolve shorthand names, then dispatch only models the
		// listing (or the seed list, when the listing is unavailable) reports as active.
		if catalogedProviders[target.ProviderName] {
			if actual, ok := ResolveModelAlias(target.ProviderName, target.UpstreamModel); ok {
				target.UpstreamModel = actual
			}
			if !r.IsActiveModel(target.ProviderName, target.UpstreamModel) {
				telemetry.Log.Debug().
					Str("provider", target.ProviderName).
					Str("model", target.UpstreamModel).
					Msg("Model is not in the provider's active models, skipping target")
				continue
			}
		}

		if !r.catalog.Usable(target.ProviderName, target.UpstreamModel) {
			telemetry.Log.Debug().
				Str("provider", target.ProviderName).
				Str("model", target.UpstreamModel).
				Msg("Model was reported unavailable by its provider, skipping target until recheck")
			continue
		}

		if r.isExcludedModel(target.UpstreamModel) {
			telemetry.Log.Debug().
				Str("provider", target.ProviderName).
				Str("model", target.UpstreamModel).
				Msg("Model is in routes.excluded_models, skipping target")
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

		// Bound non-premium attempts so a provider that accepts the request and never answers
		// hands over to the next target instead of stalling the whole chain.
		attemptCtx, cancelAttempt := ctx, context.CancelFunc(func() {})
		if r.attemptTimeout > 0 && p.Tier() != provider.TierPremium {
			attemptCtx, cancelAttempt = context.WithTimeout(ctx, r.attemptTimeout)
		}
		attemptStart := time.Now()
		resp, err := p.SendChat(attemptCtx, &targetReq)
		timedOut := err != nil && ctx.Err() == nil && errors.Is(attemptCtx.Err(), context.DeadlineExceeded)
		cancelAttempt()

		if err != nil && ctx.Err() != nil {
			return nil, "", fmt.Errorf("request canceled while waiting on %s: %w", target.ProviderName, ctx.Err())
		}
		if timedOut {
			err = fmt.Errorf("upstream provider %s model %s did not respond within %s: %w", target.ProviderName, target.UpstreamModel, r.attemptTimeout, err)
		}
		raw := snapshotResponse(resp)
		// Auto-routing upstreams (openrouter/free, kilo-auto/free) pick the model themselves;
		// reject replies that report an excluded model.
		if err == nil && resp != nil && r.isExcludedModel(resp.Model) {
			err = fmt.Errorf("upstream provider %s routed to excluded model %s", target.ProviderName, resp.Model)
		}
		if err == nil {
			// Fallback Interceptor: Convert text/DSML tool calls to structured ToolCalls
			if len(resp.ToolCalls) == 0 && resp.Content != "" {
				cleanText, extracted := ExtractTextToolCalls(resp.Content)
				if len(extracted) > 0 {
					resp.Content = cleanText
					resp.ToolCalls = extracted
					resp.FinishReason = "tool_calls"
				} else {
					// Clean any orphan DSML fragments from text
					resp.Content = cleanText
				}
			} else if len(resp.ToolCalls) > 0 && resp.Content != "" {
				// Strip leaked DSML tags if model returned both structured calls and raw DSML text
				if strings.Contains(resp.Content, "DSML") {
					resp.Content = StripDSMLTags(resp.Content)
				}
			}

			// Recover call:Name{...} tool calls written as text; unparseable ones would otherwise reach the
			// client as a final answer and end an agent run early.
			if len(resp.ToolCalls) == 0 {
				cleanText, calls, leaked := extractLeakedCalls(resp.Content, req.Tools)
				if len(calls) > 0 {
					resp.Content = cleanText
					resp.ToolCalls = calls
					resp.FinishReason = "tool_calls"
				} else if leaked {
					err = fmt.Errorf("upstream provider %s model %s wrote an unparseable tool call as text", target.ProviderName, target.UpstreamModel)
				}
			}

			// Reject silent empty or corrupted completions (0 visible text and 0 tool calls) to trigger failover.
			// Visible text excludes <think> blocks, which the Anthropic translator moves out of the text block.
			_, visibleText := extractThinkingBlocks(resp.Content)
			if strings.TrimSpace(visibleText) == "" && len(resp.ToolCalls) == 0 {
				err = fmt.Errorf("upstream provider %s returned empty or corrupted completion with no content and no tool calls", target.ProviderName)
			}

			// Reject calls to tools the client never declared (e.g. "Global" for "Glob"); repair case-only mismatches
			if err == nil {
				if bad := reconcileToolNames(req.Tools, resp.ToolCalls); bad != "" {
					err = fmt.Errorf("upstream provider %s model %s called undeclared tool %q", target.ProviderName, target.UpstreamModel, bad)
				}
			}

			// Reject turns that announce a tool action without calling it; the client would end the run
			if err == nil && isStalledAgentTurn(req, resp) {
				err = fmt.Errorf("upstream provider %s model %s announced a tool action without calling a tool", target.ProviderName, target.UpstreamModel)
			}

			// Reject ExitPlanMode calls that skip writing the plan and carry no plan text to write
			if err == nil && isEmptyExitPlanMode(req, resp) {
				err = fmt.Errorf("upstream provider %s model %s called ExitPlanMode without writing or providing a plan", target.ProviderName, target.UpstreamModel)
			}

			// Reject completions stuck in an in-context repetition loop to trigger rolling failover
			if err == nil && isRepetitionLoop(req, resp) {
				err = fmt.Errorf("upstream provider %s model %s stuck in repetition loop with identical content/tool calls", target.ProviderName, target.UpstreamModel)
			}
		}

		observeAttempt(ctx, AttemptResult{
			Provider: target.ProviderName,
			Model:    target.UpstreamModel,
			Response: raw,
			Err:      err,
			Latency:  time.Since(attemptStart),
		})

		if err == nil {
			cb.RecordSuccess()
			r.catalog.MarkActive(target.ProviderName, target.UpstreamModel)
			return resp, target.ProviderName, nil
		}
		if reason, gone := modelGoneReason(err); gone {
			r.catalog.MarkInactive(target.ProviderName, target.UpstreamModel, reason)
		}

		if isCircuitBreakerError(err) {
			cb.RecordFailure()
			// If rate limited, quota exhausted (429/RESOURCE_EXHAUSTED), or stuck in repetition loop, trip immediately to allow rapid rolling failover
			errLower := strings.ToLower(err.Error())
			if strings.Contains(errLower, "429") || strings.Contains(errLower, "resource_exhausted") || strings.Contains(errLower, "quota") || strings.Contains(errLower, "repetition loop") {
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
	// A canceled request is the client leaving, not the provider failing. Attempt timeouts are
	// wrapped in a "did not respond within" error, which still counts.
	if errors.Is(err, context.Canceled) {
		return false
	}
	errStr := strings.ToLower(err.Error())
	if strings.Contains(errStr, "context canceled") && !strings.Contains(errStr, "did not respond within") {
		return false
	}
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

// isRepetitionLoop inspects conversational history to detect if an upstream model
// is trapped in an autoregressive in-context repetition loop during autonomous agent execution.
func isRepetitionLoop(req *provider.UnifiedChatRequest, resp *provider.UnifiedChatResponse) bool {
	if req == nil || resp == nil || len(req.Messages) == 0 {
		return false
	}

	// Locate the most recent assistant turn in req.Messages
	var lastAssistant *provider.UnifiedChatMessage
	onlyToolResultsSinceLastAssistant := true
	toolResultHasError := false

	for i := len(req.Messages) - 1; i >= 0; i-- {
		msg := &req.Messages[i]
		if msg.Role == "assistant" {
			lastAssistant = msg
			break
		}
		if msg.Role == "tool" {
			contentLower := strings.ToLower(msg.Content)
			if strings.Contains(contentLower, "exit code") ||
				strings.Contains(contentLower, "error") ||
				strings.Contains(contentLower, "failed") ||
				strings.Contains(contentLower, "not found") ||
				strings.Contains(contentLower, "cannot find") ||
				strings.Contains(contentLower, "no such") ||
				strings.Contains(contentLower, "exception") {
				toolResultHasError = true
			}
		} else if msg.Role == "user" {
			// Client-injected <system-reminder> updates are not human input.
			if !isAutomatedUserMessage(msg.Content) {
				onlyToolResultsSinceLastAssistant = false
			}
		}
	}

	if lastAssistant == nil {
		return false
	}

	// Zero false positives for human interactions:
	// If a human user explicitly sent a message between the last assistant turn and now,
	// do not treat repeated responses as an autonomous agent loop.
	if !onlyToolResultsSinceLastAssistant {
		return false
	}

	respContent := strings.TrimSpace(resp.Content)
	lastContent := strings.TrimSpace(lastAssistant.Content)

	// Check 1: Tool call repetition
	if len(resp.ToolCalls) > 0 && len(lastAssistant.ToolCalls) > 0 {
		if areToolCallsEqual(resp.ToolCalls, lastAssistant.ToolCalls) {
			// Case 1a: Exact text match (including both empty text) + identical tool calls
			if strings.EqualFold(respContent, lastContent) {
				return true
			}
			// Case 1b: Tool execution failed and model repeated identical tool calls
			if toolResultHasError {
				return true
			}
			// Case 1c: The exact same tool calls appeared in an earlier assistant turn as well (3rd repeat)
			if hasPriorIdenticalToolCall(req.Messages, resp.ToolCalls) {
				return true
			}
		}
	}

	// Check 2: Pure text repetition in an autonomous agent tool loop (>= 15 chars)
	if len(resp.ToolCalls) == 0 && len(lastAssistant.ToolCalls) == 0 && len(respContent) >= 15 {
		if strings.EqualFold(respContent, lastContent) {
			return true
		}
	}

	return false
}

// areToolCallsEqual returns true if two slices of UnifiedToolCall have matching names and arguments.
func areToolCallsEqual(tc1, tc2 []provider.UnifiedToolCall) bool {
	if len(tc1) != len(tc2) {
		return false
	}
	matched := make([]bool, len(tc2))
	for _, call1 := range tc1 {
		found := false
		for j, call2 := range tc2 {
			if matched[j] {
				continue
			}
			if strings.EqualFold(call1.Function.Name, call2.Function.Name) &&
				areArgumentsEqual(call1.Function.Arguments, call2.Function.Arguments) {
				matched[j] = true
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// areArgumentsEqual canonically compares two JSON argument strings.
func areArgumentsEqual(arg1, arg2 string) bool {
	trimmed1 := strings.TrimSpace(arg1)
	trimmed2 := strings.TrimSpace(arg2)
	if trimmed1 == trimmed2 {
		return true
	}
	if trimmed1 == "" || trimmed2 == "" {
		return false
	}
	var v1, v2 interface{}
	if err := json.Unmarshal([]byte(trimmed1), &v1); err == nil {
		if err := json.Unmarshal([]byte(trimmed2), &v2); err == nil {
			return reflect.DeepEqual(v1, v2)
		}
	}
	return false
}

// hasPriorIdenticalToolCall checks if an assistant message prior to the last assistant turn
// already invoked the identical tool calls.
func hasPriorIdenticalToolCall(messages []provider.UnifiedChatMessage, targetCalls []provider.UnifiedToolCall) bool {
	assistantCount := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			assistantCount++
			if assistantCount > 1 {
				// Prior assistant message
				if areToolCallsEqual(messages[i].ToolCalls, targetCalls) {
					return true
				}
			}
		}
	}
	return false
}

type attemptObserverKey struct{}

// AttemptResult describes one upstream attempt in a fallback chain. Response is a copy of the raw
// provider reply, taken before failover interceptors repair it; Err is the attempt's final error,
// including rejections by those interceptors (empty turn, undeclared tool, stall, loop).
type AttemptResult struct {
	Provider string
	Model    string
	Response *provider.UnifiedChatResponse
	Err      error
	Latency  time.Duration
}

// AttemptObserver receives the result of every upstream attempt.
type AttemptObserver func(AttemptResult)

// WithAttemptObserver returns a context whose DispatchChat calls report every attempt to obs.
func WithAttemptObserver(ctx context.Context, obs AttemptObserver) context.Context {
	return context.WithValue(ctx, attemptObserverKey{}, obs)
}

func observeAttempt(ctx context.Context, res AttemptResult) {
	if obs, ok := ctx.Value(attemptObserverKey{}).(AttemptObserver); ok && obs != nil {
		obs(res)
	}
}

func snapshotResponse(resp *provider.UnifiedChatResponse) *provider.UnifiedChatResponse {
	if resp == nil {
		return nil
	}
	snapshot := *resp
	snapshot.ToolCalls = append([]provider.UnifiedToolCall(nil), resp.ToolCalls...)
	return &snapshot
}

// normalizeModelID reduces a model ID to its bare name for exclusion matching:
// "google/gemini-3.5-flash-lite:free" and "models/gemini-3.5-flash-lite" become "gemini-3.5-flash-lite".
func normalizeModelID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	if i := strings.Index(id, ":"); i >= 0 {
		id = id[:i]
	}
	return id
}

// demoteLastResort moves targets whose model is in routes.last_resort_models to the end of the
// chain, keeping the relative order within each group.
func (r *Router) demoteLastResort(targets []TargetSpec) []TargetSpec {
	if len(r.lastResort) == 0 {
		return targets
	}
	ordered := make([]TargetSpec, 0, len(targets))
	var demoted []TargetSpec
	for _, t := range targets {
		if r.lastResort[normalizeModelID(t.UpstreamModel)] {
			demoted = append(demoted, t)
			continue
		}
		ordered = append(ordered, t)
	}
	return append(ordered, demoted...)
}

// isExcludedModel reports whether routes.excluded_models covers the model.
func (r *Router) isExcludedModel(model string) bool {
	return len(r.excluded) > 0 && r.excluded[normalizeModelID(model)]
}

// SetRoute registers or replaces a named route. Requests select it by model name or X-Liltok-Route.
func (r *Router) SetRoute(route Route) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[route.ID] = route
}

// SetProvider registers or overrides a provider client thread-safely.
func (r *Router) SetProvider(name string, client provider.ProviderClient) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[strings.ToLower(name)] = client
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
		case "kilo":
			r.cfg.Providers.Kilo = creds
		case "cline":
			r.cfg.Providers.Cline = creds
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

	// Trigger asynchronous resync of provider models if applicable
	go func(pName string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = r.SyncProviderModels(ctx, pName)
	}(name)

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

// Catalog returns the model health catalog.
func (r *Router) Catalog() *ModelCatalog {
	return r.catalog
}

// Translator returns the cross-protocol translator instance.
func (r *Router) Translator() *Translator {
	return r.translator
}

func (r *Router) seedActiveModels() {
	r.modelsMu.Lock()
	defer r.modelsMu.Unlock()
	r.activeModels["groq"] = append([]provider.ModelInfo(nil), defaultGroqActiveModels...)
	r.activeModels["nvidianim"] = append([]provider.ModelInfo(nil), defaultNVIDIANIMActiveModels...)
	r.activeModels["openrouter"] = append([]provider.ModelInfo(nil), defaultOpenRouterActiveModels...)
	r.activeModels["kilo"] = append([]provider.ModelInfo(nil), defaultKiloActiveModels...)
	r.activeModels["cline"] = append([]provider.ModelInfo(nil), defaultClineActiveModels...)
}

// SyncProviderModels retrieves the current active models from the specified provider client.
func (r *Router) SyncProviderModels(ctx context.Context, providerName string) ([]provider.ModelInfo, error) {
	providerName = strings.ToLower(providerName)
	r.mu.RLock()
	p, exists := r.providers[providerName]
	r.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("unknown provider %q", providerName)
	}

	lister, ok := p.(provider.ModelLister)
	if !ok {
		return r.GetProviderActiveModels(providerName), nil
	}

	models, err := lister.ListModels(ctx)
	if err != nil {
		telemetry.Log.Warn().
			Str("provider", providerName).
			Err(err).
			Msg("Failed to dynamically sync provider models, using cached/default catalog")
		return r.GetProviderActiveModels(providerName), err
	}

	// Strictly filter active models
	var filtered []provider.ModelInfo
	for _, m := range models {
		if providerName == "groq" && !m.Active {
			continue
		}
		filtered = append(filtered, m)
	}

	if len(filtered) > 0 {
		r.modelsMu.Lock()
		r.activeModels[providerName] = filtered
		r.modelsMu.Unlock()

		// Ensure circuit breakers exist for discovered active models
		for _, m := range filtered {
			key := providerName + "/" + m.ID
			r.mu.Lock()
			if _, cbExists := r.breakers[key]; !cbExists {
				r.breakers[key] = NewCircuitBreaker(key)
			}
			r.mu.Unlock()
		}

		telemetry.Log.Info().
			Str("provider", providerName).
			Int("active_models", len(filtered)).
			Msg("Provider active models dynamically synchronized")
		return filtered, nil
	}

	return r.GetProviderActiveModels(providerName), nil
}

// SyncAllProviderModels queries all registered providers for active models.
func (r *Router) SyncAllProviderModels(ctx context.Context) map[string][]provider.ModelInfo {
	r.mu.RLock()
	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	r.mu.RUnlock()

	result := make(map[string][]provider.ModelInfo)
	for _, name := range names {
		models, _ := r.SyncProviderModels(ctx, name)
		if len(models) > 0 {
			result[name] = models
		}
	}
	return result
}

// GetProviderActiveModels returns the cached active models for a provider.
func (r *Router) GetProviderActiveModels(providerName string) []provider.ModelInfo {
	providerName = strings.ToLower(providerName)
	r.modelsMu.RLock()
	defer r.modelsMu.RUnlock()

	if list, ok := r.activeModels[providerName]; ok && len(list) > 0 {
		out := make([]provider.ModelInfo, len(list))
		copy(out, list)
		return out
	}

	if providerName == "groq" {
		out := make([]provider.ModelInfo, len(defaultGroqActiveModels))
		copy(out, defaultGroqActiveModels)
		return out
	}
	if providerName == "nvidianim" {
		out := make([]provider.ModelInfo, len(defaultNVIDIANIMActiveModels))
		copy(out, defaultNVIDIANIMActiveModels)
		return out
	}
	if providerName == "openrouter" {
		out := make([]provider.ModelInfo, len(defaultOpenRouterActiveModels))
		copy(out, defaultOpenRouterActiveModels)
		return out
	}
	if providerName == "kilo" {
		out := make([]provider.ModelInfo, len(defaultKiloActiveModels))
		copy(out, defaultKiloActiveModels)
		return out
	}
	if providerName == "cline" {
		out := make([]provider.ModelInfo, len(defaultClineActiveModels))
		copy(out, defaultClineActiveModels)
		return out
	}

	return nil
}

// IsActiveModel checks whether a given model is actively supported by the provider.
func (r *Router) IsActiveModel(providerName, modelID string) bool {
	providerName = strings.ToLower(providerName)
	models := r.GetProviderActiveModels(providerName)
	for _, m := range models {
		if !m.Active {
			continue
		}
		if strings.EqualFold(m.ID, modelID) {
			return true
		}
		trimmedModel := strings.TrimPrefix(strings.ToLower(modelID), providerName+"/")
		trimmedMID := strings.TrimPrefix(strings.ToLower(m.ID), providerName+"/")
		if trimmedModel == trimmedMID {
			return true
		}
	}
	return false
}

// GetModelContextWindow returns the maximum physical context window in tokens for a given provider and model.
func (r *Router) GetModelContextWindow(providerName, modelID string) int {
	providerName = strings.ToLower(providerName)
	models := r.GetProviderActiveModels(providerName)
	for _, m := range models {
		if m.ContextWindow > 0 {
			if strings.EqualFold(m.ID, modelID) {
				return m.ContextWindow
			}
			trimmedModel := strings.TrimPrefix(strings.ToLower(modelID), providerName+"/")
			trimmedMID := strings.TrimPrefix(strings.ToLower(m.ID), providerName+"/")
			if trimmedModel == trimmedMID {
				return m.ContextWindow
			}
		}
	}
	switch providerName {
	case "gemini":
		return 1048576
	case "anthropic":
		return 200000
	case "openai":
		return 128000
	case "groq":
		return 131072
	case "nvidianim":
		return 131072
	case "openrouter", "kilo", "cline":
		return 262144
	default:
		return 131072
	}
}

// GetAllActiveModels returns a flat list of all active models across all configured providers.
func (r *Router) GetAllActiveModels(ctx context.Context) []provider.ModelInfo {
	r.modelsMu.RLock()
	var all []provider.ModelInfo
	for _, list := range r.activeModels {
		all = append(all, list...)
	}
	r.modelsMu.RUnlock()

	hasGroq := false
	hasNvidia := false
	hasOpenRouter := false
	hasKilo := false
	hasCline := false
	for _, m := range all {
		if m.Provider == "groq" {
			hasGroq = true
		}
		if m.Provider == "nvidianim" {
			hasNvidia = true
		}
		if m.Provider == "openrouter" {
			hasOpenRouter = true
		}
		if m.Provider == "kilo" {
			hasKilo = true
		}
		if m.Provider == "cline" {
			hasCline = true
		}
	}
	if !hasGroq {
		all = append(all, defaultGroqActiveModels...)
	}
	if !hasNvidia {
		all = append(all, defaultNVIDIANIMActiveModels...)
	}
	if !hasOpenRouter {
		all = append(all, defaultOpenRouterActiveModels...)
	}
	if !hasKilo {
		all = append(all, defaultKiloActiveModels...)
	}
	if !hasCline {
		all = append(all, defaultClineActiveModels...)
	}
	return all
}
