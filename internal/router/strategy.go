package router

import (
	"sort"
	"strings"
	"sync/atomic"

	"github.com/primaybr/liltok/internal/provider"
)

// Route strategies decide the order a named route's targets are tried in. Every strategy still
// fails over through the whole list; they differ only in which target is tried first.
const (
	// StrategyFallback tries targets in the order they are listed.
	StrategyFallback = "fallback"
	// StrategyFreeFirst is the listed order too; routes use it to mark a list that starts with
	// free targets.
	StrategyFreeFirst = "free_first"
	// StrategyRoundRobin starts each request one target further along the list, spreading load
	// across targets while keeping the rest of the list as fallback.
	StrategyRoundRobin = "round_robin"
	// StrategyLeastCost tries the cheapest targets first (stable, so equal-cost targets keep
	// their listed order).
	StrategyLeastCost = "least_cost"
)

// KnownStrategies lists the accepted Route.Strategy values.
var KnownStrategies = []string{StrategyFallback, StrategyFreeFirst, StrategyRoundRobin, StrategyLeastCost}

// IsKnownStrategy reports whether s is an accepted Route.Strategy value. Empty means fallback.
func IsKnownStrategy(s string) bool {
	if s == "" {
		return true
	}
	for _, k := range KnownStrategies {
		if s == k {
			return true
		}
	}
	return false
}

// TargetCostFunc returns a target's price in USD per million tokens, used by least_cost.
type TargetCostFunc func(providerName, model string) float64

// SetTargetCost sets the price lookup least_cost ordering uses. Without one, targets on free-tier
// providers cost 0 and all others cost the same.
func (r *Router) SetTargetCost(fn TargetCostFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.targetCost = fn
}

// resolveRoute is ResolveTargets plus the route the targets came from. Strategy only applies to
// named routes; routeID and strategy are empty when the targets were resolved from the model name.
func (r *Router) resolveRoute(requestedModel, routeAlias string) (targets []TargetSpec, routeID, strategy string) {
	r.mu.RLock()
	route, named := r.routes[routeAlias]
	if routeAlias == "" || !named {
		route, named = r.routes[requestedModel]
	}
	r.mu.RUnlock()
	if named {
		return route.Targets, route.ID, route.Strategy
	}
	return r.ResolveTargets(requestedModel, routeAlias), "", ""
}

// orderTargets returns targets in the order the strategy tries them. It never modifies targets,
// which may be a route's own slice.
func (r *Router) orderTargets(routeID, strategy string, targets []TargetSpec) []TargetSpec {
	if len(targets) < 2 {
		return targets
	}
	switch strategy {
	case StrategyRoundRobin:
		counter, _ := r.roundRobin.LoadOrStore(routeID, new(atomic.Uint64))
		start := int((counter.(*atomic.Uint64).Add(1) - 1) % uint64(len(targets)))
		out := make([]TargetSpec, 0, len(targets))
		out = append(out, targets[start:]...)
		return append(out, targets[:start]...)
	case StrategyLeastCost:
		out := append([]TargetSpec(nil), targets...)
		costs := make(map[TargetSpec]float64, len(out))
		for _, t := range out {
			costs[t] = r.costOf(t)
		}
		sort.SliceStable(out, func(i, j int) bool { return costs[out[i]] < costs[out[j]] })
		return out
	default:
		return targets
	}
}

func (r *Router) costOf(t TargetSpec) float64 {
	r.mu.RLock()
	fn := r.targetCost
	p, ok := r.providers[strings.ToLower(t.ProviderName)]
	r.mu.RUnlock()
	if fn != nil {
		return fn(t.ProviderName, t.UpstreamModel)
	}
	if ok && p.Tier() == provider.TierFree {
		return 0
	}
	return 1
}
