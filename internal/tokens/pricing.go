package tokens

import (
	"math"
	"regexp"
	"strings"
	"sync"

	"github.com/primaybr/liltok/internal/db"
)

// ModelPricing defines token rates per 1 Million (1M) tokens in USD.
type ModelPricing struct {
	ModelPattern        string  `json:"model_pattern"`
	Provider            string  `json:"provider"`
	Tier                string  `json:"tier"`
	InputCostPerM       float64 `json:"input_cost_per_m"`
	CachedInputCostPerM float64 `json:"cached_input_cost_per_m"`
	OutputCostPerM      float64 `json:"output_cost_per_m"`

	regex *regexp.Regexp
}

// PricingRegistry manages model rates and calculates exact costs and savings.
type PricingRegistry struct {
	mu    sync.RWMutex
	rules []ModelPricing
	db    *db.DB
}

// DefaultPricingRules provides fallback pricing when database entries are unavailable.
func DefaultPricingRules() []ModelPricing {
	raw := []ModelPricing{
		{ModelPattern: `^claude-opus-5.*`, Provider: "anthropic", Tier: "premium", InputCostPerM: 15.00, CachedInputCostPerM: 1.50, OutputCostPerM: 75.00},
		{ModelPattern: `^claude-sonnet-5.*`, Provider: "anthropic", Tier: "premium", InputCostPerM: 3.00, CachedInputCostPerM: 0.30, OutputCostPerM: 15.00},
		{ModelPattern: `^claude-haiku-5.*`, Provider: "anthropic", Tier: "budget", InputCostPerM: 0.80, CachedInputCostPerM: 0.08, OutputCostPerM: 4.00},
		{ModelPattern: `^claude-5.*`, Provider: "anthropic", Tier: "premium", InputCostPerM: 3.00, CachedInputCostPerM: 0.30, OutputCostPerM: 15.00},
		{ModelPattern: `^claude-3-5-sonnet.*`, Provider: "anthropic", Tier: "premium", InputCostPerM: 3.00, CachedInputCostPerM: 0.30, OutputCostPerM: 15.00},
		{ModelPattern: `^claude-3-7-sonnet.*`, Provider: "anthropic", Tier: "premium", InputCostPerM: 3.00, CachedInputCostPerM: 0.30, OutputCostPerM: 15.00},
		{ModelPattern: `^gpt-4o$`, Provider: "openai", Tier: "premium", InputCostPerM: 2.50, CachedInputCostPerM: 1.25, OutputCostPerM: 10.00},
		{ModelPattern: `^gpt-4o-mini.*`, Provider: "openai", Tier: "budget", InputCostPerM: 0.15, CachedInputCostPerM: 0.075, OutputCostPerM: 0.60},
		{ModelPattern: `^deepseek-chat.*`, Provider: "deepseek", Tier: "budget", InputCostPerM: 0.27, CachedInputCostPerM: 0.07, OutputCostPerM: 1.10},
		{ModelPattern: `^deepseek-reasoner.*`, Provider: "deepseek", Tier: "budget", InputCostPerM: 0.55, CachedInputCostPerM: 0.14, OutputCostPerM: 2.19},
		{ModelPattern: `^meta/llama-3.3-70b-instruct.*`, Provider: "nvidianim", Tier: "free", InputCostPerM: 0.00, CachedInputCostPerM: 0.00, OutputCostPerM: 0.00},
		{ModelPattern: `^deepseek-ai/deepseek-r1.*`, Provider: "nvidianim", Tier: "free", InputCostPerM: 0.00, CachedInputCostPerM: 0.00, OutputCostPerM: 0.00},
		{ModelPattern: `^llama-3.3-70b-versatile.*`, Provider: "groq", Tier: "free", InputCostPerM: 0.00, CachedInputCostPerM: 0.00, OutputCostPerM: 0.00},
		{ModelPattern: `^gemini-1.5-flash.*`, Provider: "gemini", Tier: "free", InputCostPerM: 0.00, CachedInputCostPerM: 0.00, OutputCostPerM: 0.00},
		{ModelPattern: `^ollama/.*`, Provider: "ollama", Tier: "free", InputCostPerM: 0.00, CachedInputCostPerM: 0.00, OutputCostPerM: 0.00},
	}

	for i := range raw {
		raw[i].regex, _ = regexp.Compile(raw[i].ModelPattern)
	}
	return raw
}

// NewPricingRegistry creates a new pricing registry from SQLite or defaults.
func NewPricingRegistry(database *db.DB) *PricingRegistry {
	r := &PricingRegistry{
		rules: DefaultPricingRules(),
		db:    database,
	}

	if database != nil {
		_ = r.LoadFromDB()
	}

	return r
}

// LoadFromDB refreshes rates from the SQLite model_pricing table.
func (pr *PricingRegistry) LoadFromDB() error {
	if pr.db == nil {
		return nil
	}

	rows, err := pr.db.Query(`
		SELECT model_pattern, provider, tier, input_cost_per_m, cached_input_cost_per_m, output_cost_per_m
		FROM model_pricing
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var loaded []ModelPricing
	for rows.Next() {
		var p ModelPricing
		if err := rows.Scan(&p.ModelPattern, &p.Provider, &p.Tier, &p.InputCostPerM, &p.CachedInputCostPerM, &p.OutputCostPerM); err == nil {
			p.regex, _ = regexp.Compile(p.ModelPattern)
			loaded = append(loaded, p)
		}
	}

	if len(loaded) > 0 {
		pr.mu.Lock()
		pr.rules = loaded
		pr.mu.Unlock()
	}

	return nil
}

// FindPricing returns matching model pricing or fallback default.
func (pr *PricingRegistry) FindPricing(model string) ModelPricing {
	pr.mu.RLock()
	defer pr.mu.RUnlock()

	for _, rule := range pr.rules {
		if rule.regex != nil && rule.regex.MatchString(model) {
			return rule
		}
	}

	// Fallback generic pricing
	return ModelPricing{
		ModelPattern:        model,
		Provider:            "generic",
		Tier:                "standard",
		InputCostPerM:       1.00,
		CachedInputCostPerM: 0.50,
		OutputCostPerM:      3.00,
	}
}

// Calculate computes exact financial cost and dollars saved based on token consumption and cache status.
func (pr *PricingRegistry) Calculate(model string, promptTokens, completionTokens, cachedTokens int, cacheStatus, cacheTier string) (costUSD float64, savedUSD float64) {
	pricing := pr.FindPricing(model)

	// Free tier models cost $0.00
	if pricing.Tier == "free" || pricing.InputCostPerM == 0 && pricing.OutputCostPerM == 0 {
		return 0.0, 0.0
	}

	// 1. Exact or Semantic Cache Hit: Upstream cost is $0.00, saved is 100% of input + completion cost
	if cacheStatus == "HIT" && (cacheTier == "TIER1_EXACT" || cacheTier == "TIER3_SEMANTIC") {
		costUSD = 0.0
		savedUSD = (float64(promptTokens)*pricing.InputCostPerM + float64(completionTokens)*pricing.OutputCostPerM) / 1_000_000.0
		return round6(costUSD), round6(savedUSD)
	}

	// 2. Standard Upstream Execution (with potential Tier-2 Prefix Cache Hit)
	regularPromptTokens := promptTokens - cachedTokens
	if regularPromptTokens < 0 {
		regularPromptTokens = 0
	}

	promptCost := (float64(regularPromptTokens)*pricing.InputCostPerM + float64(cachedTokens)*pricing.CachedInputCostPerM) / 1_000_000.0
	completionCost := (float64(completionTokens) * pricing.OutputCostPerM) / 1_000_000.0
	costUSD = promptCost + completionCost

	// Dollars saved from prefix cache discount
	if cachedTokens > 0 && pricing.InputCostPerM > pricing.CachedInputCostPerM {
		savedUSD = (float64(cachedTokens) * (pricing.InputCostPerM - pricing.CachedInputCostPerM)) / 1_000_000.0
	}

	return round6(costUSD), round6(savedUSD)
}

// CalculateForRouting computes cost and savings when a requested model is fulfilled by a specific provider.
// If the fulfilling provider is a free-tier provider (gemini, groq, nvidianim, ollama), actual cost is $0.00
// and saved USD is the baseline commercial cost of executing that requested model.
func (pr *PricingRegistry) CalculateForRouting(requestedModel, fulfillingProvider string, promptTokens, completionTokens, cachedTokens int, cacheStatus, cacheTier string) (costUSD float64, savedUSD float64) {
	prov := strings.ToLower(fulfillingProvider)
	isFree := prov == "gemini" || prov == "groq" || prov == "nvidianim" || prov == "ollama" || prov == "free"

	if isFree {
		costUSD = 0.0
		pricing := pr.FindPricing(requestedModel)
		if pricing.Tier != "free" {
			baselineCost := (float64(promptTokens)*pricing.InputCostPerM + float64(completionTokens)*pricing.OutputCostPerM) / 1_000_000.0
			savedUSD = baselineCost
		}
		return round6(costUSD), round6(savedUSD)
	}

	return pr.Calculate(requestedModel, promptTokens, completionTokens, cachedTokens, cacheStatus, cacheTier)
}

func round6(val float64) float64 {
	return math.Round(val*1_000_000) / 1_000_000
}
