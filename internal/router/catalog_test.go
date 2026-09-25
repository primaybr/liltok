package router

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/provider"
)

func TestModelGoneReason(t *testing.T) {
	cases := []struct {
		name string
		err  error
		gone bool
	}{
		{"groq model does not exist", errors.New("groq returned status 404: {\"error\":{\"message\":\"The model `llama3-8b-8192` does not exist or you do not have access to it.\",\"code\":\"model_not_found\"}}"), true},
		{"groq decommissioned as 400", errors.New("groq returned status 400: {\"error\":{\"message\":\"The model `mixtral-8x7b-32768` has been decommissioned and is no longer supported.\",\"code\":\"model_decommissioned\"}}"), true},
		{"openrouter no endpoints", errors.New("openrouter returned status 404: {\"error\":{\"message\":\"No endpoints found for meta-llama/llama-3.1-8b-instruct:free.\"}}"), true},
		{"gemini model not found", errors.New("gemini error 404: {\"error\":{\"message\":\"models/gemini-1.5-flash is not found for API version v1beta\"}}"), true},
		{"410 gone", errors.New("nvidianim returned status 410: gone"), true},
		{"wrong base url 404", errors.New("openai returned status 404: 404 page not found"), false},
		{"400 payload issue", errors.New("groq returned status 400: tool calling is not supported by this model"), false},
		{"rate limit", errors.New("groq returned status 429: model rate limit reached"), false},
		{"server error", errors.New("kilo returned status 500: model not found in cache"), false},
		{"no status", errors.New("dial tcp: connection refused"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		reason, gone := modelGoneReason(tc.err)
		if gone != tc.gone {
			t.Errorf("%s: gone = %v (reason %q), want %v", tc.name, gone, reason, tc.gone)
		}
	}
}

func TestResolveModelAliasExactOnly(t *testing.T) {
	cases := []struct {
		provider, in, want string
		ok                 bool
	}{
		{"openrouter", "auto", "openrouter/free", true},
		{"openrouter", "openrouter/auto", "openrouter/free", true},
		{"OpenRouter", "AUTO", "openrouter/free", true},
		// The old tables matched loosely, so "free" was rewritten to a DeepSeek model on OpenRouter.
		{"openrouter", "free", "free", false},
		{"openrouter", "deepseek/deepseek-r1:free", "deepseek/deepseek-r1:free", false},
		{"kilo", "kilo/auto", "kilo-auto/free", true},
		{"groq", "llama-3.3-70b-versatile", "llama-3.3-70b-versatile", false},
	}
	for _, tc := range cases {
		got, ok := ResolveModelAlias(tc.provider, tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ResolveModelAlias(%q, %q) = %q, %v; want %q, %v", tc.provider, tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestModelCatalogRecheckTTL(t *testing.T) {
	c := NewModelCatalog(time.Hour)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	if !c.Usable("groq", "m1") {
		t.Fatal("an unknown model must be usable")
	}
	c.MarkInactive("groq", "m1", "status 404: does not exist")
	if c.Usable("groq", "M1") {
		t.Fatal("an inactive model must be skipped (matching ignores case)")
	}

	now = now.Add(59 * time.Minute)
	if c.Usable("groq", "m1") {
		t.Fatal("model must stay skipped before the recheck TTL")
	}

	now = now.Add(2 * time.Minute)
	if !c.Usable("groq", "m1") {
		t.Fatal("one request must be let through after the recheck TTL")
	}
	if c.Usable("groq", "m1") {
		t.Fatal("the recheck is claimed by one request; others keep skipping")
	}

	c.MarkActive("groq", "m1")
	if !c.Usable("groq", "m1") {
		t.Fatal("a successful reply must reactivate the model")
	}

	c.MarkInactive("groq", "m1", "status 410 gone")
	entries := c.Entries()
	if len(entries) != 1 || entries[0].Status != CatalogInactive || !entries[0].RecheckAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("entries = %+v, want one inactive entry rechecking at %v", entries, now.Add(time.Hour))
	}
	if !c.Reactivate("groq", "m1") || !c.Usable("groq", "m1") {
		t.Fatal("Reactivate must clear the inactive mark")
	}
	if c.Reactivate("groq", "m1") {
		t.Fatal("Reactivate on an active model must report false")
	}
}

// catalogProvider fails with a scripted error for listed models and counts calls per model.
type catalogProvider struct {
	mu     sync.Mutex
	errs   map[string]error
	calls  map[string]int
	answer string
}

func (p *catalogProvider) Name() string                                  { return "alpha" }
func (p *catalogProvider) Tier() provider.ProviderTier                   { return provider.TierFree }
func (p *catalogProvider) CheckHealth(ctx context.Context) (bool, error) { return true, nil }
func (p *catalogProvider) StreamChat(ctx context.Context, req *provider.UnifiedChatRequest) (<-chan provider.UnifiedSSEEvent, <-chan error, error) {
	return nil, nil, errors.New("not scripted")
}

func (p *catalogProvider) SendChat(ctx context.Context, req *provider.UnifiedChatRequest) (*provider.UnifiedChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls[req.Model]++
	if err := p.errs[req.Model]; err != nil {
		return nil, err
	}
	return &provider.UnifiedChatResponse{ID: "ok", Model: req.Model, Role: "assistant", Content: p.answer, FinishReason: "stop"}, nil
}

func (p *catalogProvider) callCount(model string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[model]
}

func TestDispatchSkipsModelsReportedGone(t *testing.T) {
	r := NewRouter(config.DefaultConfig())
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	r.catalog.now = func() time.Time { return now }

	p := &catalogProvider{
		errs:   map[string]error{"retired-model": errors.New("alpha returned status 404: The model `retired-model` does not exist")},
		calls:  map[string]int{},
		answer: "served by the fallback",
	}
	r.SetProvider("alpha", p)
	r.SetRoute(Route{ID: "catalog-test", Strategy: "fallback", Targets: []TargetSpec{
		{ProviderName: "alpha", UpstreamModel: "retired-model"},
		{ProviderName: "alpha", UpstreamModel: "live-model"},
	}})
	dispatch := func() {
		t.Helper()
		req := &provider.UnifiedChatRequest{Model: "catalog-test", Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "hi"}}}
		resp, _, err := r.DispatchChat(context.Background(), req, "catalog-test")
		if err != nil || resp.Content != "served by the fallback" {
			t.Fatalf("dispatch = %v, %v; want the fallback reply", resp, err)
		}
	}

	dispatch()
	dispatch()
	if n := p.callCount("retired-model"); n != 1 {
		t.Fatalf("retired-model called %d times, want 1 (skipped after its 404)", n)
	}

	// After the recheck TTL one request re-checks it; the model has come back.
	now = now.Add(25 * time.Hour)
	delete(p.errs, "retired-model")
	dispatch()
	if n := p.callCount("retired-model"); n != 2 {
		t.Fatalf("retired-model called %d times after the TTL, want 2", n)
	}
	for _, e := range r.Catalog().Entries() {
		if e.Model == "retired-model" && e.Status != CatalogActive {
			t.Fatalf("retired-model status = %s after a successful recheck, want active", e.Status)
		}
	}
}

func TestSQLCatalogStorePersists(t *testing.T) {
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	database, err := db.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	c := NewModelCatalog(time.Hour)
	if err := c.SetStore(context.Background(), NewSQLCatalogStore(database.DB)); err != nil {
		t.Fatal(err)
	}
	c.MarkInactive("Groq", "openai/GPT-OSS-120b", "status 404: does not exist")
	c.MarkInactive("groq", "openai/gpt-oss-120b", "status 404: does not exist")

	reloaded := NewModelCatalog(time.Hour)
	if err := reloaded.SetStore(context.Background(), NewSQLCatalogStore(database.DB)); err != nil {
		t.Fatal(err)
	}
	entries := reloaded.Entries()
	if len(entries) != 1 {
		t.Fatalf("reloaded %d entries, want 1 (provider and model match ignoring case)", len(entries))
	}
	if e := entries[0]; e.Status != CatalogInactive || e.FailCount != 2 || e.Reason == "" {
		t.Fatalf("reloaded entry = %+v, want inactive with 2 failures and a reason", e)
	}
	if reloaded.Usable("groq", "openai/gpt-oss-120b") {
		t.Fatal("a persisted inactive model must stay skipped after reload")
	}
}
