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

func TestDefaultRoutesOmitFlashLite(t *testing.T) {
	r := NewRouter(config.DefaultConfig())
	for id, route := range r.routes {
		for _, target := range route.Targets {
			if normalizeModelID(target.UpstreamModel) == "gemini-3.5-flash-lite" {
				t.Errorf("route %s still targets %s/%s", id, target.ProviderName, target.UpstreamModel)
			}
		}
	}
}

// Excluded models are skipped whichever provider offers them, and a reply from an auto-routing
// upstream that reports an excluded model fails over.
func TestDispatchSkipsExcludedModels(t *testing.T) {
	cfg := config.DefaultConfig()
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

func TestExcludedModelsCanBeCleared(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Routes.ExcludedModels = nil
	if NewRouter(cfg).isExcludedModel("gemini-3.5-flash-lite") {
		t.Error("clearing routes.excluded_models must re-enable the model")
	}
}
