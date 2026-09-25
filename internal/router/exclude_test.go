package router

import (
	"context"
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/provider"
)

func TestNormalizeModelID(t *testing.T) {
	cases := map[string]string{
		"gemini-3.5-flash-lite":              "gemini-3.5-flash-lite",
		"  Gemini-3.5-Flash-Lite ":           "gemini-3.5-flash-lite",
		"google/gemini-3.5-flash-lite:free":  "gemini-3.5-flash-lite",
		"models/gemini-3.5-flash-lite":       "gemini-3.5-flash-lite",
		"openrouter/google/gemini-3.5-flash": "gemini-3.5-flash",
		"":                                   "",
	}
	for in, want := range cases {
		if got := normalizeModelID(in); got != want {
			t.Errorf("normalizeModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

// Flash-Lite stays in the default chains but is tried only after every other target.
func TestDefaultRoutesDemoteFlashLite(t *testing.T) {
	r := NewRouter(config.DefaultConfig())
	for _, id := range []string{"auto-resilient", "free-first"} {
		chain := r.demoteLastResort(r.routes[id].Targets)
		last := chain[len(chain)-1]
		if last.UpstreamModel != "gemini-3.5-flash-lite" {
			t.Errorf("route %s: last target = %s/%s, want gemini-3.5-flash-lite", id, last.ProviderName, last.UpstreamModel)
		}
		for _, target := range chain[:len(chain)-1] {
			if normalizeModelID(target.UpstreamModel) == "gemini-3.5-flash-lite" {
				t.Errorf("route %s: flash-lite appears before the end of the chain", id)
			}
		}
	}
	if r.isExcludedModel("gemini-3.5-flash-lite") {
		t.Error("flash-lite must not be excluded by default")
	}
}

// A last-resort model is tried only after every other target failed, even when the route lists
// it earlier, and it serves the request when nothing else can.
func TestDispatchTriesLastResortModelLast(t *testing.T) {
	r := NewRouter(config.DefaultConfig())
	lite := &mockProvider{name: "lrlite", tier: provider.TierFree,
		response: &provider.UnifiedChatResponse{Content: "lite answer"}}
	failing := &mockProvider{name: "lrfail", tier: provider.TierFree, fail: true}
	good := &mockProvider{name: "lrgood", tier: provider.TierFree,
		response: &provider.UnifiedChatResponse{Content: "good answer"}}
	r.SetProvider("lrlite", lite)
	r.SetProvider("lrfail", failing)
	r.SetProvider("lrgood", good)
	req := func() *provider.UnifiedChatRequest {
		return &provider.UnifiedChatRequest{Model: "claude-sonnet-5", Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "hi"}}}
	}

	r.SetRoute(Route{ID: "lr-healthy", Targets: []TargetSpec{
		{ProviderName: "lrlite", UpstreamModel: "google/gemini-3.5-flash-lite:free"},
		{ProviderName: "lrfail", UpstreamModel: "m-fail"},
		{ProviderName: "lrgood", UpstreamModel: "m-good"},
	}})
	_, winner, err := r.DispatchChat(context.Background(), req(), "lr-healthy")
	if err != nil || winner != "lrgood" {
		t.Fatalf("with a healthy target left, winner = %s (%v), want lrgood", winner, err)
	}
	if lite.lastModel != "" {
		t.Errorf("last-resort model was tried while a better target was healthy")
	}

	r.SetRoute(Route{ID: "lr-only", Targets: []TargetSpec{
		{ProviderName: "lrlite", UpstreamModel: "gemini-3.5-flash-lite"},
		{ProviderName: "lrfail", UpstreamModel: "m-fail-2"},
	}})
	resp, winner, err := r.DispatchChat(context.Background(), req(), "lr-only")
	if err != nil || winner != "lrlite" || resp.Content != "lite answer" {
		t.Fatalf("when every other target fails, winner = %s (%v), want lrlite", winner, err)
	}
}

// Excluded models are skipped whichever provider offers them, and a reply from an auto-routing
// upstream that reports an excluded model fails over.
func TestDispatchSkipsExcludedModels(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Routes.ExcludedModels = []string{"gemini-3.5-flash-lite"}
	r := NewRouter(cfg)

	excludedTarget := &mockProvider{name: "exgem", tier: provider.TierFree,
		response: &provider.UnifiedChatResponse{Content: "should never be asked"}}
	autoRouter := &mockProvider{name: "exauto", tier: provider.TierFree,
		response: &provider.UnifiedChatResponse{Model: "google/gemini-3.5-flash-lite:free", Content: "answered by the excluded model"}}
	good := &mockProvider{name: "exgood", tier: provider.TierFree,
		response: &provider.UnifiedChatResponse{Model: "qwen/qwen3.8-27b", Content: "good answer"}}
	r.SetProvider("exgem", excludedTarget)
	r.SetProvider("exauto", autoRouter)
	r.SetProvider("exgood", good)
	r.SetRoute(Route{ID: "exclude-test", Strategy: "fallback", Targets: []TargetSpec{
		{ProviderName: "exgem", UpstreamModel: "gemini-3.5-flash-lite"},
		{ProviderName: "exauto", UpstreamModel: "openrouter/free"},
		{ProviderName: "exgood", UpstreamModel: "qwen/qwen3.8-27b"},
	}})

	var attempts []AttemptResult
	ctx := WithAttemptObserver(context.Background(), func(res AttemptResult) { attempts = append(attempts, res) })
	resp, winner, err := r.DispatchChat(ctx, &provider.UnifiedChatRequest{
		Model:    "claude-sonnet-5",
		Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "hi"}},
	}, "exclude-test")
	if err != nil {
		t.Fatalf("DispatchChat error: %v", err)
	}
	if winner != "exgood" || resp.Content != "good answer" {
		t.Fatalf("winner = %s (%q), want exgood", winner, resp.Content)
	}
	if excludedTarget.lastModel != "" {
		t.Errorf("excluded target was called with model %q", excludedTarget.lastModel)
	}
	if len(attempts) != 2 || attempts[0].Err == nil || !strings.Contains(attempts[0].Err.Error(), "excluded model") {
		t.Errorf("want the auto-router reply rejected as an excluded model, got %+v", attempts)
	}
}

func TestLastResortModelsCanBeCleared(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Routes.LastResortModels = nil
	r := NewRouter(cfg)
	targets := []TargetSpec{{ProviderName: "gemini", UpstreamModel: "gemini-3.5-flash-lite"}, {ProviderName: "groq", UpstreamModel: "x"}}
	if got := r.demoteLastResort(targets); got[0].UpstreamModel != "gemini-3.5-flash-lite" {
		t.Error("clearing routes.last_resort_models must keep the route's own order")
	}
}
