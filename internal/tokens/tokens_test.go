package tokens_test

import (
	"testing"

	"github.com/liltok/liltok/internal/db"
	"github.com/liltok/liltok/internal/tokens"
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
