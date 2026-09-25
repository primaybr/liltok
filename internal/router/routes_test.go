package router

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/provider"
)

func openRouteDB(t *testing.T) *db.DB {
	t.Helper()
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	database, err := db.Open(filepath.Join(t.TempDir(), "routes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func TestUpsertRouteValidation(t *testing.T) {
	r := NewRouter(config.DefaultConfig())
	groq := []TargetSpec{{ProviderName: "groq", UpstreamModel: "qwen/qwen3.8-27b"}}
	cases := []struct {
		name  string
		route Route
		want  string
	}{
		{"bad id", Route{ID: "Has Spaces", Targets: groq}, "route id"},
		{"empty id", Route{ID: "", Targets: groq}, "route id"},
		{"unknown strategy", Route{ID: "x", Strategy: "random", Targets: groq}, "unknown strategy"},
		{"no targets", Route{ID: "x"}, "at least one target"},
		{"unknown provider", Route{ID: "x", Targets: []TargetSpec{{ProviderName: "nope", UpstreamModel: "m"}}}, "unknown provider"},
		{"missing model", Route{ID: "x", Targets: []TargetSpec{{ProviderName: "groq"}}}, "model is required"},
	}
	for _, tc := range cases {
		err := r.UpsertRoute(context.Background(), tc.route)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
	if len(r.Routes()) != len(builtInRouteOrder) {
		t.Fatalf("rejected routes must not be added; have %d routes", len(r.Routes()))
	}
}

func TestRouteEditsPersistAndReset(t *testing.T) {
	database := openRouteDB(t)
	ctx := context.Background()

	r := NewRouter(config.DefaultConfig())
	if err := r.SetRouteStore(ctx, NewSQLRouteStore(database.DB)); err != nil {
		t.Fatal(err)
	}
	custom := Route{ID: "cheap-first", Description: "cheapest first", Strategy: StrategyLeastCost, Targets: []TargetSpec{
		{ProviderName: "Anthropic", UpstreamModel: " claude-sonnet-5 "},
		{ProviderName: "groq", UpstreamModel: "qwen/qwen3.8-27b"},
	}}
	if err := r.UpsertRoute(ctx, custom); err != nil {
		t.Fatal(err)
	}
	premium := Route{ID: "premium-only", Targets: []TargetSpec{{ProviderName: "openai", UpstreamModel: "gpt-4o"}}}
	if err := r.UpsertRoute(ctx, premium); err != nil {
		t.Fatal(err)
	}

	// A fresh router loads both edits from the database.
	reloaded := NewRouter(config.DefaultConfig())
	if err := reloaded.SetRouteStore(ctx, NewSQLRouteStore(database.DB)); err != nil {
		t.Fatal(err)
	}
	infos := map[string]RouteInfo{}
	var order []string
	for _, info := range reloaded.Routes() {
		infos[info.ID] = info
		order = append(order, info.ID)
	}
	if want := []string{"auto-resilient", "free-first", "premium-only", "cheap-first"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("route order = %v, want %v", order, want)
	}
	got := infos["cheap-first"]
	wantTargets := []TargetSpec{{ProviderName: "anthropic", UpstreamModel: "claude-sonnet-5"}, {ProviderName: "groq", UpstreamModel: "qwen/qwen3.8-27b"}}
	if got.BuiltIn || got.Strategy != StrategyLeastCost || got.Description != "cheapest first" || !reflect.DeepEqual(got.Targets, wantTargets) {
		t.Fatalf("reloaded custom route = %+v", got)
	}
	if p := infos["premium-only"]; !p.BuiltIn || !p.Customized || p.Strategy != StrategyFallback || len(p.Targets) != 1 {
		t.Fatalf("reloaded premium-only = %+v, want the saved single-target override", p)
	}
	if infos["auto-resilient"].Customized {
		t.Fatal("an unedited built-in route must not be marked customized")
	}

	// The saved least_cost strategy orders the free Groq target before Anthropic.
	targets, id, strategy := reloaded.resolveRoute("cheap-first", "")
	if ordered := reloaded.orderTargets(id, strategy, targets); ordered[0].ProviderName != "groq" {
		t.Fatalf("least_cost route tried %v first, want groq", ordered[0])
	}

	// Reset restores the built-in and deletes the custom route, in memory and in the database.
	if found, err := reloaded.ResetRoute(ctx, "premium-only"); !found || err != nil {
		t.Fatalf("reset premium-only = %v, %v", found, err)
	}
	if found, err := reloaded.ResetRoute(ctx, "cheap-first"); !found || err != nil {
		t.Fatalf("reset cheap-first = %v, %v", found, err)
	}
	if found, _ := reloaded.ResetRoute(ctx, "no-such-route"); found {
		t.Fatal("resetting an unknown route must report not found")
	}
	third := NewRouter(config.DefaultConfig())
	if err := third.SetRouteStore(ctx, NewSQLRouteStore(database.DB)); err != nil {
		t.Fatal(err)
	}
	routes := third.Routes()
	if len(routes) != len(builtInRouteOrder) {
		t.Fatalf("after reset, %d routes remain, want only the built-ins", len(routes))
	}
	for _, info := range routes {
		if info.Customized {
			t.Fatalf("route %s still customized after reset", info.ID)
		}
	}
}

func TestSavedRouteWithRemovedProviderIsSkipped(t *testing.T) {
	database := openRouteDB(t)
	ctx := context.Background()
	store := NewSQLRouteStore(database.DB)
	if err := store.SaveRoute(ctx, Route{ID: "stale", Strategy: StrategyFallback, Targets: []TargetSpec{{ProviderName: "retired-provider", UpstreamModel: "m"}}}); err != nil {
		t.Fatal(err)
	}
	r := NewRouter(config.DefaultConfig())
	if err := r.SetRouteStore(ctx, store); err != nil {
		t.Fatal(err)
	}
	for _, info := range r.Routes() {
		if info.ID == "stale" {
			t.Fatal("a saved route naming an unknown provider must be skipped at load")
		}
	}
}

func TestDispatchUsesEditedRoute(t *testing.T) {
	r := NewRouter(config.DefaultConfig())
	p := &catalogProvider{errs: map[string]error{}, calls: map[string]int{}, answer: "edited route"}
	r.SetProvider("alpha", p)
	if err := r.UpsertRoute(context.Background(), Route{ID: "mine", Targets: []TargetSpec{{ProviderName: "alpha", UpstreamModel: "m1"}}}); err != nil {
		t.Fatal(err)
	}
	req := &provider.UnifiedChatRequest{Model: "claude-sonnet-5", Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "hi"}}}
	resp, _, err := r.DispatchChat(context.Background(), req, "mine")
	if err != nil || resp.Content != "edited route" || p.callCount("m1") != 1 {
		t.Fatalf("dispatch via X-Liltok-Route: resp %v err %v calls %d", resp, err, p.callCount("m1"))
	}
}
