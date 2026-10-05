package router

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"

	"github.com/primaybr/liltok/internal/ledger"
	"github.com/primaybr/liltok/internal/telemetry"
)

// FitnessWeights parameterizes the evolutionary route optimization algorithm (Hermes / Nous GEPA pattern).
type FitnessWeights struct {
	SuccessWeight float64 `json:"success_weight"` // default: 0.50
	LatencyWeight float64 `json:"latency_weight"` // default: 0.30
	CostWeight    float64 `json:"cost_weight"`    // default: 0.20
	MutationRate  float64 `json:"mutation_rate"`  // default: 0.10 (chance of exploratory rank perturbation)
}

// DefaultFitnessWeights returns balanced weights prioritizing reliability and responsiveness.
func DefaultFitnessWeights() FitnessWeights {
	return FitnessWeights{
		SuccessWeight: 0.50,
		LatencyWeight: 0.30,
		CostWeight:    0.20,
		MutationRate:  0.10,
	}
}

// TargetFitness details the telemetry-grounded score and ranking for a target specification.
type TargetFitness struct {
	Target       TargetSpec `json:"target"`
	TotalCalls   int64      `json:"total_calls"`
	SuccessCalls int64      `json:"success_calls"`
	FailedCalls  int64      `json:"failed_calls"`
	SuccessRate  float64    `json:"success_rate"`
	AvgLatencyMs float64    `json:"avg_latency_ms"`
	AvgCostUSD   float64    `json:"avg_cost_usd"`
	Score        float64    `json:"score"`
	BreakerState string     `json:"breaker_state"`
}

// CalculateTargetFitness calculates the fitness score [0.0, 1.0] for a target given its telemetry and breaker state.
func CalculateTargetFitness(target TargetSpec, perf *ledger.ModelPerformance, breakerState string, weights FitnessWeights) TargetFitness {
	tf := TargetFitness{
		Target:       target,
		BreakerState: breakerState,
	}

	if breakerState == "OPEN" {
		tf.Score = 0.01 // heavily penalize broken targets
		if perf != nil {
			tf.TotalCalls = perf.TotalCalls
			tf.SuccessCalls = perf.SuccessCalls
			tf.FailedCalls = perf.FailedCalls
			tf.SuccessRate = perf.SuccessRate
			tf.AvgLatencyMs = perf.AvgLatencyMs
			tf.AvgCostUSD = perf.AvgCostUSD
		}
		return tf
	}

	if perf == nil || perf.TotalCalls == 0 {
		// Untested target prior: neutral score to prevent traffic starvation
		tf.Score = 0.70
		return tf
	}

	tf.TotalCalls = perf.TotalCalls
	tf.SuccessCalls = perf.SuccessCalls
	tf.FailedCalls = perf.FailedCalls
	tf.SuccessRate = perf.SuccessRate
	tf.AvgLatencyMs = perf.AvgLatencyMs
	tf.AvgCostUSD = perf.AvgCostUSD

	successScore := perf.SuccessRate

	// Normalized latency score (0ms = 1.0, 5000ms+ = 0.0)
	latencyScore := 1.0 - (perf.AvgLatencyMs / 5000.0)
	if latencyScore < 0 {
		latencyScore = 0
	}

	// Normalized cost score (0.00 = 1.0, 0.05+ = 0.0)
	costScore := 1.0 - (perf.AvgCostUSD / 0.05)
	if costScore < 0 {
		costScore = 0
	}

	raw := (weights.SuccessWeight * successScore) +
		(weights.LatencyWeight * latencyScore) +
		(weights.CostWeight * costScore)

	if breakerState == "HALF_OPEN" {
		raw *= 0.50
	}

	if raw < 0.01 {
		raw = 0.01
	} else if raw > 1.0 {
		raw = 1.0
	}

	tf.Score = raw
	return tf
}

// RankTargetsByFitness evaluates each target, sorts by fitness score descending, and applies exploration mutation.
func RankTargetsByFitness(targets []TargetSpec, stats map[string]*ledger.ModelPerformance, breakers map[string]*CircuitBreaker, weights FitnessWeights, explore bool) []TargetFitness {
	results := make([]TargetFitness, 0, len(targets))

	for _, t := range targets {
		key := strings.ToLower(t.ProviderName) + "/" + strings.ToLower(t.UpstreamModel)
		var perf *ledger.ModelPerformance
		if stats != nil {
			perf = stats[key]
		}
		breakerState := "CLOSED"
		if breakers != nil {
			if cb, exists := breakers[key]; exists && cb != nil {
				st, _ := cb.State()
				breakerState = string(st)
			}
		}

		tf := CalculateTargetFitness(t, perf, breakerState, weights)
		results = append(results, tf)
	}

	// Sort descending by fitness score (stable to preserve tie order)
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	// Evolutionary Mutation / Exploration: with probability weights.MutationRate,
	// swap two adjacent targets so under-sampled models receive trial requests
	if explore && len(results) > 1 && weights.MutationRate > 0 {
		if rand.Float64() < weights.MutationRate {
			// Swap index 0 with index 1 to sample the alternative candidate
			results[0], results[1] = results[1], results[0]
			telemetry.Log.Debug().
				Str("promoted", results[0].Target.UpstreamModel).
				Str("demoted", results[1].Target.UpstreamModel).
				Msg("Evolutionary route mutation applied")
		}
	}

	return results
}

// SetEvolutionaryRanker sets the target ranker invoked for evolutionary / gepa routes.
func (r *Router) SetEvolutionaryRanker(fn func(targets []TargetSpec) []TargetSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evolutionaryRanker = fn
}

// OptimizeRoute evaluates and ranks the targets of a route given historical telemetry stats.
func (r *Router) OptimizeRoute(ctx context.Context, routeID string, stats map[string]*ledger.ModelPerformance) ([]TargetFitness, error) {
	r.mu.RLock()
	route, exists := r.routes[routeID]
	breakers := make(map[string]*CircuitBreaker, len(r.breakers))
	for k, v := range r.breakers {
		breakers[k] = v
	}
	r.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("route %q not found", routeID)
	}

	weights := DefaultFitnessWeights()
	ranked := RankTargetsByFitness(route.Targets, stats, breakers, weights, false)
	return ranked, nil
}
