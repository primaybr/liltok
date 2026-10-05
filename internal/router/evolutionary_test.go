package router

import (
	"context"
	"testing"

	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/ledger"
)

func TestEvolutionary_FitnessCalculation(t *testing.T) {
	weights := DefaultFitnessWeights()
	target := TargetSpec{ProviderName: "groq", UpstreamModel: "llama-3-8b"}

	// 1. Untested target (no calls recorded)
	tfUntested := CalculateTargetFitness(target, nil, "CLOSED", weights)
	if tfUntested.Score != 0.70 {
		t.Errorf("expected untested score 0.70, got %f", tfUntested.Score)
	}

	// 2. High performing model (100% success, 50ms latency, $0 cost)
	perfGreat := &ledger.ModelPerformance{
		Provider:     "groq",
		Model:        "llama-3-8b",
		TotalCalls:   100,
		SuccessCalls: 100,
		SuccessRate:  1.0,
		AvgLatencyMs: 50.0,
		AvgCostUSD:   0.0,
	}
	tfGreat := CalculateTargetFitness(target, perfGreat, "CLOSED", weights)
	if tfGreat.Score < 0.95 {
		t.Errorf("expected high fitness score > 0.95, got %f", tfGreat.Score)
	}

	// 3. Degraded model (50% success, 4000ms latency, $0.04 cost)
	perfPoor := &ledger.ModelPerformance{
		Provider:     "groq",
		Model:        "llama-3-8b",
		TotalCalls:   100,
		SuccessCalls: 50,
		SuccessRate:  0.50,
		AvgLatencyMs: 4000.0,
		AvgCostUSD:   0.04,
	}
	tfPoor := CalculateTargetFitness(target, perfPoor, "CLOSED", weights)
	if tfPoor.Score > 0.50 {
		t.Errorf("expected poor fitness score < 0.50, got %f", tfPoor.Score)
	}

	// 4. Open circuit breaker (heavily penalized)
	tfOpen := CalculateTargetFitness(target, perfGreat, "OPEN", weights)
	if tfOpen.Score != 0.01 {
		t.Errorf("expected open breaker score 0.01, got %f", tfOpen.Score)
	}

	// 5. Half-open breaker (halved score)
	tfHalf := CalculateTargetFitness(target, perfGreat, "HALF_OPEN", weights)
	if tfHalf.Score > tfGreat.Score/2+0.05 || tfHalf.Score < tfGreat.Score/2-0.05 {
		t.Errorf("expected half-open score ~%f, got %f", tfGreat.Score/2, tfHalf.Score)
	}
}

func TestEvolutionary_RankTargetsByFitness(t *testing.T) {
	weights := FitnessWeights{
		SuccessWeight: 0.50,
		LatencyWeight: 0.30,
		CostWeight:    0.20,
		MutationRate:  0.0, // disable random exploration for deterministic ranking test
	}

	targets := []TargetSpec{
		{ProviderName: "provider_a", UpstreamModel: "slow-costly-model"},
		{ProviderName: "provider_b", UpstreamModel: "fast-cheap-model"},
		{ProviderName: "provider_c", UpstreamModel: "failing-model"},
	}

	stats := map[string]*ledger.ModelPerformance{
		"provider_a/slow-costly-model": {
			Provider:     "provider_a",
			Model:        "slow-costly-model",
			TotalCalls:   50,
			SuccessCalls: 50,
			SuccessRate:  1.0,
			AvgLatencyMs: 3500.0,
			AvgCostUSD:   0.03,
		},
		"provider_b/fast-cheap-model": {
			Provider:     "provider_b",
			Model:        "fast-cheap-model",
			TotalCalls:   100,
			SuccessCalls: 100,
			SuccessRate:  1.0,
			AvgLatencyMs: 120.0,
			AvgCostUSD:   0.00,
		},
		"provider_c/failing-model": {
			Provider:     "provider_c",
			Model:        "failing-model",
			TotalCalls:   50,
			SuccessCalls: 10,
			SuccessRate:  0.20,
			AvgLatencyMs: 2000.0,
			AvgCostUSD:   0.01,
		},
	}

	ranked := RankTargetsByFitness(targets, stats, nil, weights, false)
	if len(ranked) != 3 {
		t.Fatalf("expected 3 ranked targets, got %d", len(ranked))
	}

	// Expect fast-cheap-model to be ranked #1
	if ranked[0].Target.UpstreamModel != "fast-cheap-model" {
		t.Errorf("expected rank 1 to be fast-cheap-model, got %s (score: %f)", ranked[0].Target.UpstreamModel, ranked[0].Score)
	}

	// Expect failing-model to be ranked #3
	if ranked[2].Target.UpstreamModel != "failing-model" {
		t.Errorf("expected rank 3 to be failing-model, got %s (score: %f)", ranked[2].Target.UpstreamModel, ranked[2].Score)
	}
}

func TestEvolutionary_StrategyOrderTargets(t *testing.T) {
	cfg := config.DefaultConfig()
	r := NewRouter(cfg)

	route := Route{
		ID:       "custom-evolved",
		Strategy: StrategyEvolutionary,
		Targets: []TargetSpec{
			{ProviderName: "groq", UpstreamModel: "model-1"},
			{ProviderName: "groq", UpstreamModel: "model-2"},
		},
	}
	_ = r.UpsertRoute(context.Background(), route)

	// Configure mock evolutionary ranker reversing target order
	r.SetEvolutionaryRanker(func(targets []TargetSpec) []TargetSpec {
		reversed := make([]TargetSpec, len(targets))
		for i, t := range targets {
			reversed[len(targets)-1-i] = t
		}
		return reversed
	})

	ordered := r.orderTargets("custom-evolved", StrategyEvolutionary, route.Targets)
	if len(ordered) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(ordered))
	}
	if ordered[0].UpstreamModel != "model-2" || ordered[1].UpstreamModel != "model-1" {
		t.Errorf("expected reversed order [model-2, model-1], got [%s, %s]", ordered[0].UpstreamModel, ordered[1].UpstreamModel)
	}
}

func TestEvolutionary_OptimizeRouteMethod(t *testing.T) {
	cfg := config.DefaultConfig()
	r := NewRouter(cfg)

	// Optimize built-in free-first route
	ranked, err := r.OptimizeRoute(context.Background(), "free-first", nil)
	if err != nil {
		t.Fatalf("OptimizeRoute failed: %v", err)
	}

	if len(ranked) == 0 {
		t.Fatal("expected ranked targets for free-first route, got empty")
	}

	// Untested default targets should have valid scores
	for _, rt := range ranked {
		if rt.Score <= 0 || rt.Score > 1.0 {
			t.Errorf("invalid score for target %s: %f", rt.Target.UpstreamModel, rt.Score)
		}
	}
}
