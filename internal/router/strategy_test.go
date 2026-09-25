package router

import (
	"context"
	"reflect"
	"testing"

	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/provider"
)

func specs(models ...string) []TargetSpec {
	out := make([]TargetSpec, len(models))
	for i, m := range models {
		out[i] = TargetSpec{ProviderName: "alpha", UpstreamModel: m}
	}
	return out
}

func models(targets []TargetSpec) []string {
	out := make([]string, len(targets))
	for i, t := range targets {
		out[i] = t.UpstreamModel
	}
	return out
}

func TestOrderTargetsRoundRobin(t *testing.T) {
	r := NewRouter(config.DefaultConfig())
	route := specs("a", "b", "c")

	want := [][]string{{"a", "b", "c"}, {"b", "c", "a"}, {"c", "a", "b"}, {"a", "b", "c"}}
	for i, w := range want {
		if got := models(r.orderTargets("rr", StrategyRoundRobin, route)); !reflect.DeepEqual(got, w) {
			t.Fatalf("request %d order = %v, want %v", i+1, got, w)
		}
	}
	if got := models(r.orderTargets("other-rr", StrategyRoundRobin, route)); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("a second route must keep its own counter, got %v", got)
	}
	if got := models(route); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("orderTargets modified the route's own slice: %v", got)
	}
}

func TestOrderTargetsLeastCost(t *testing.T) {
	r := NewRouter(config.DefaultConfig())
	prices := map[string]float64{"pricey": 90, "cheap": 0.5, "free-a": 0, "free-b": 0, "mid": 12}
	r.SetTargetCost(func(_, model string) float64 { return prices[model] })

	got := models(r.orderTargets("lc", StrategyLeastCost, specs("pricey", "free-a", "mid", "cheap", "free-b")))
	want := []string{"free-a", "free-b", "cheap", "mid", "pricey"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("least_cost order = %v, want %v (equal costs keep listed order)", got, want)
	}
}

func TestOrderTargetsLeastCostDefaultsToProviderTier(t *testing.T) {
	r := NewRouter(config.DefaultConfig())
	targets := []TargetSpec{
		{ProviderName: "anthropic", UpstreamModel: "claude-sonnet-5"},
		{ProviderName: "groq", UpstreamModel: "qwen/qwen3.8-27b"},
		{ProviderName: "openai", UpstreamModel: "gpt-4o"},
		{ProviderName: "gemini", UpstreamModel: "gemini-3.8-flash"},
	}
	got := r.orderTargets("lc", StrategyLeastCost, targets)
	if got[0].ProviderName != "groq" || got[1].ProviderName != "gemini" || got[2].ProviderName != "anthropic" || got[3].ProviderName != "openai" {
		t.Fatalf("without a price lookup, free-tier providers must come first in listed order; got %v", got)
	}
}

func TestOrderTargetsFallbackKeepsOrder(t *testing.T) {
	r := NewRouter(config.DefaultConfig())
	for _, s := range []string{"", StrategyFallback, StrategyFreeFirst} {
		if got := models(r.orderTargets("f", s, specs("a", "b", "c"))); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
			t.Errorf("strategy %q reordered targets: %v", s, got)
		}
	}
}

func TestDispatchRoundRobinSpreadsRequests(t *testing.T) {
	r := NewRouter(config.DefaultConfig())
	p := &catalogProvider{errs: map[string]error{}, calls: map[string]int{}, answer: "ok"}
	r.SetProvider("alpha", p)
	r.SetRoute(Route{ID: "spread", Strategy: StrategyRoundRobin, Targets: specs("m1", "m2")})

	for i := 0; i < 4; i++ {
		req := &provider.UnifiedChatRequest{Model: "spread", Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "hi"}}}
		if _, _, err := r.DispatchChat(context.Background(), req, ""); err != nil {
			t.Fatalf("dispatch %d: %v", i+1, err)
		}
	}
	if p.callCount("m1") != 2 || p.callCount("m2") != 2 {
		t.Fatalf("calls m1=%d m2=%d, want 2 each", p.callCount("m1"), p.callCount("m2"))
	}
}

func TestIsKnownStrategy(t *testing.T) {
	for _, s := range []string{"", "fallback", "free_first", "round_robin", "least_cost"} {
		if !IsKnownStrategy(s) {
			t.Errorf("IsKnownStrategy(%q) = false", s)
		}
	}
	for _, s := range []string{"random", "Least_Cost", "weighted"} {
		if IsKnownStrategy(s) {
			t.Errorf("IsKnownStrategy(%q) = true", s)
		}
	}
}
