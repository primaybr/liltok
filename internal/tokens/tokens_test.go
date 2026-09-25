package tokens_test

import (
	"testing"

	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/tokens"
)

func TestCountTokens(t *testing.T) {
	text := "Hello world, this is a test prompt for tiktoken tokenization."
	count4o := tokens.CountTokens("gpt-4o", text)
	if count4o < 5 || count4o > 20 {
		t.Errorf("Unexpected token count for gpt-4o: %d", count4o)
	}

	countFallback := tokens.CountTokens("claude-3-5-sonnet-20241022", text)
	if countFallback < 5 || countFallback > 25 {
		t.Errorf("Unexpected token count for claude: %d", countFallback)
	}

	if empty := tokens.CountTokens("gpt-4o", ""); empty != 0 {
		t.Errorf("Expected 0 tokens for empty string, got %d", empty)
	}
}

func TestCountMessagesTokens(t *testing.T) {
	messages := []tokens.TokenMessage{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "Hello!"},
	}
	total := tokens.CountMessagesTokens("gpt-4o", messages)
	if total < 10 || total > 30 {
		t.Errorf("Unexpected message tokens total: %d", total)
	}
}

func TestPricingRegistry_ExactHit(t *testing.T) {
	reg := tokens.NewPricingRegistry(nil)

	// gpt-4o: $2.50 / 1M input, $10.00 / 1M output
	// Exact Cache Hit on 10,000 prompt tokens + 1,000 completion tokens
	cost, saved := reg.Calculate("gpt-4o", 10_000, 1_000, 0, "HIT", "TIER1_EXACT")
	if cost != 0.0 {
		t.Errorf("Expected $0.00 cost on exact cache hit, got %f", cost)
	}

	// Expected saved: (10000 * 2.50 + 1000 * 10.00) / 1,000,000 = 0.025 + 0.010 = 0.035 USD
	if saved != 0.035 {
		t.Errorf("Expected $0.035 saved on exact hit, got %f", saved)
	}
}

func TestPricingRegistry_PrefixDiscount(t *testing.T) {
	reg := tokens.NewPricingRegistry(nil)

	// claude-3-5-sonnet: $3.00 / 1M input, $0.30 / 1M cached input, $15.00 / 1M output
	// 5,000 prompt tokens total, of which 4,000 read from cache (1,000 regular input), 500 completion tokens
	cost, saved := reg.Calculate("claude-3-5-sonnet-20241022", 5_000, 500, 4_000, "MISS", "TIER2_PREFIX")

	// Regular prompt cost: 1000 * 3.00 / 1M = 0.003
	// Cached prompt cost: 4000 * 0.30 / 1M = 0.0012
	// Completion cost: 500 * 15.00 / 1M = 0.0075
	// Total cost = 0.003 + 0.0012 + 0.0075 = 0.0117
	if cost != 0.0117 {
		t.Errorf("Expected cost $0.0117, got %f", cost)
	}

	// Saved: 4000 * (3.00 - 0.30) / 1M = 4000 * 2.70 / 1M = 0.0108
	if saved != 0.0108 {
		t.Errorf("Expected saved $0.0108, got %f", saved)
	}
}

func TestPricingRegistry_ClaudeOpus5(t *testing.T) {
	reg := tokens.NewPricingRegistry(nil)

	// claude-opus-5: $5.00 / 1M input, $0.50 / 1M cached input, $25.00 / 1M output
	// 1,000,000 prompt tokens total, of which 900,000 read from cache, 1,000 completion tokens
	cost, saved := reg.Calculate("claude-opus-5", 1_000_000, 1_000, 900_000, "MISS", "TIER2_PREFIX")

	// Regular prompt cost: 100,000 * 5.00 / 1M = 0.50 USD
	// Cached prompt cost: 900,000 * 0.50 / 1M = 0.45 USD
	// Completion cost: 1,000 * 25.00 / 1M = 0.025 USD
	// Total cost = 0.50 + 0.45 + 0.025 = 0.975 USD
	if cost != 0.975 {
		t.Errorf("Expected cost $0.975, got %f", cost)
	}

	// Saved: 900,000 * (5.00 - 0.50) / 1M = 4.05 USD
	if saved != 4.05 {
		t.Errorf("Expected saved $4.05, got %f", saved)
	}
}

// TestPricingRegistry_CurrentClaudeRates pins the list prices and checks that a specific rule wins
// over the general rule that also matches (claude-opus-5-5 vs claude-opus-5).
func TestPricingRegistry_CurrentClaudeRates(t *testing.T) {
	reg := tokens.NewPricingRegistry(nil)
	cases := []struct {
		model               string
		input, cached, outp float64
	}{
		{"claude-opus-5-5", 4.00, 0.20, 20.00},
		{"claude-opus-5", 5.00, 0.50, 25.00},
		{"claude-sonnet-5", 2.00, 0.20, 10.00},
		{"claude-haiku-4-5-20251001", 1.00, 0.10, 5.00},
		{"claude-fable-5-1", 10.00, 0.25, 50.00},
		{"claude-fable-5", 10.00, 1.00, 50.00},
		{"claude-opus-4-8", 5.00, 0.50, 25.00},
		{"claude-opus-4-1", 15.00, 1.50, 75.00},
		{"claude-sonnet-4-6", 3.00, 0.30, 15.00},
		{"gpt-4o-mini", 0.15, 0.075, 0.60},
		{"gpt-4o", 2.50, 1.25, 10.00},
	}
	for _, tc := range cases {
		p := reg.FindPricing(tc.model)
		if p.InputCostPerM != tc.input || p.CachedInputCostPerM != tc.cached || p.OutputCostPerM != tc.outp {
			t.Errorf("%s priced %v/%v/%v (pattern %s), want %v/%v/%v", tc.model, p.InputCostPerM, p.CachedInputCostPerM, p.OutputCostPerM, p.ModelPattern, tc.input, tc.cached, tc.outp)
		}
	}
}

func TestPricingRegistry_FreeTier(t *testing.T) {
	reg := tokens.NewPricingRegistry(nil)

	cost, saved := reg.Calculate("meta/llama-3.3-70b-instruct", 50_000, 2_000, 0, "MISS", "NONE")
	if cost != 0.0 || saved != 0.0 {
		t.Errorf("Expected $0.00 cost and saved for free tier model, got cost=%f saved=%f", cost, saved)
	}
}

func TestPricingRegistry_DBLoad(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open memory db: %v", err)
	}
	defer database.Close()

	reg := tokens.NewPricingRegistry(database)
	p := reg.FindPricing("claude-3-7-sonnet-20250219")
	if p.Provider != "anthropic" || p.InputCostPerM != 3.00 {
		t.Errorf("Expected seeded pricing from DB, got %+v", p)
	}
}

func TestPricingRegistry_CalculateForRouting(t *testing.T) {
	reg := tokens.NewPricingRegistry(nil)

	// claude-sonnet-5 routed to gemini (free tier)
	// 29,975 prompt tokens + 113 completion tokens
	cost, saved := reg.CalculateForRouting("claude-sonnet-5", "gemini", 29_975, 113, 0, "MISS", "NONE")
	if cost != 0.0 {
		t.Errorf("Expected $0.00 cost when fulfilled by gemini, got %f", cost)
	}
	// Expected saved: (29975 * 2.00 + 113 * 10.00) / 1,000,000 = 0.05995 + 0.00113 = 0.06108
	if saved != 0.06108 {
		t.Errorf("Expected $0.06108 saved when fulfilled by free tier, got %f", saved)
	}

	// claude-sonnet-5 served by anthropic (paid tier)
	costPaid, savedPaid := reg.CalculateForRouting("claude-sonnet-5", "anthropic", 29_975, 113, 0, "MISS", "NONE")
	if costPaid != 0.06108 {
		t.Errorf("Expected $0.06108 cost when fulfilled by anthropic, got %f", costPaid)
	}
	if savedPaid != 0.0 {
		t.Errorf("Expected $0.00 saved when fulfilled by anthropic without cache, got %f", savedPaid)
	}

	// claude-sonnet-5 routed to openrouter (free tier)
	costOR, savedOR := reg.CalculateForRouting("claude-sonnet-5", "openrouter", 29_975, 113, 0, "MISS", "NONE")
	if costOR != 0.0 {
		t.Errorf("Expected $0.00 cost when fulfilled by openrouter free tier, got %f", costOR)
	}
	if savedOR != 0.06108 {
		t.Errorf("Expected $0.06108 saved when fulfilled by openrouter, got %f", savedOR)
	}

	// openrouter/free direct pricing check
	costDirect, savedDirect := reg.Calculate("openrouter/free", 10_000, 1_000, 0, "MISS", "NONE")
	if costDirect != 0.0 || savedDirect != 0.0 {
		t.Errorf("Expected $0.00 cost and saved for openrouter/free direct, got cost=%f saved=%f", costDirect, savedDirect)
	}
}

// TestPricingRegistry_DBMergesWithDefaults checks the migrated table's corrected rates, that a
// model the table does not list still gets its built-in rate instead of the generic fallback, and
// that an edited table row wins over the built-in rule.
func TestPricingRegistry_DBMergesWithDefaults(t *testing.T) {
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	database, err := db.Open(t.TempDir() + "/pricing.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`UPDATE model_pricing SET input_cost_per_m = 1.50 WHERE model_pattern = '^claude-haiku-4-5.*'`); err != nil {
		t.Fatal(err)
	}

	reg := tokens.NewPricingRegistry(database)
	cases := []struct {
		model string
		input float64
	}{
		{"claude-opus-5-5", 4.00},    // specific row beats 'claude-opus-5.*'
		{"claude-opus-5", 5.00},      // seeded row corrected by migration 007
		{"claude-sonnet-5", 2.00},    // seeded row corrected by migration 007
		{"gpt-4o-mini", 0.15},        // no longer swallowed by the gpt-4o row
		{"claude-haiku-4-5", 1.50},   // edited row wins over the built-in rule
		{"moonshotai/kimi-k3", 0.00}, // not in the table: built-in free rule, not generic $1
		{"unknown-model-xyz", 1.00},  // no rule at all: generic fallback
	}
	for _, tc := range cases {
		if p := reg.FindPricing(tc.model); p.InputCostPerM != tc.input {
			t.Errorf("%s input = %v (pattern %s), want %v", tc.model, p.InputCostPerM, p.ModelPattern, tc.input)
		}
	}
}
