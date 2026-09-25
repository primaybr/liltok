package router

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/provider"
)

// scriptProvider answers per upstream model: an error from errs, a reply from replies, or a
// default "ok from <model>" text reply. It records the order of the models it was asked for.
type scriptProvider struct {
	mu        sync.Mutex
	name      string
	tier      provider.ProviderTier
	replies   map[string]*provider.UnifiedChatResponse
	errs      map[string]error
	calls     []string
	healthErr error
}

func (p *scriptProvider) Name() string                { return p.name }
func (p *scriptProvider) Tier() provider.ProviderTier { return p.tier }
func (p *scriptProvider) CheckHealth(ctx context.Context) (bool, error) {
	if p.healthErr != nil {
		return false, p.healthErr
	}
	return true, nil
}

func (p *scriptProvider) SendChat(ctx context.Context, req *provider.UnifiedChatRequest) (*provider.UnifiedChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, req.Model)
	if err := p.errs[req.Model]; err != nil {
		return nil, err
	}
	if r, ok := p.replies[req.Model]; ok {
		cp := *r
		cp.ToolCalls = append([]provider.UnifiedToolCall(nil), r.ToolCalls...)
		return &cp, nil
	}
	return &provider.UnifiedChatResponse{ID: "r-" + req.Model, Content: "ok from " + req.Model}, nil
}

func (p *scriptProvider) StreamChat(ctx context.Context, req *provider.UnifiedChatRequest) (<-chan provider.UnifiedSSEEvent, <-chan error, error) {
	return nil, nil, errors.New("streaming not scripted")
}

func (p *scriptProvider) callLog() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

// listerProvider is a scriptProvider that also lists models.
type listerProvider struct {
	scriptProvider
	lmu     sync.Mutex
	models  []provider.ModelInfo
	listErr error
}

func (p *listerProvider) ListModels(ctx context.Context) ([]provider.ModelInfo, error) {
	p.lmu.Lock()
	defer p.lmu.Unlock()
	if p.listErr != nil {
		return nil, p.listErr
	}
	return append([]provider.ModelInfo(nil), p.models...), nil
}

func (p *listerProvider) setModels(models []provider.ModelInfo, err error) {
	p.lmu.Lock()
	defer p.lmu.Unlock()
	p.models, p.listErr = models, err
}

// newOfflineRouter builds a router whose built-in adapters all point at a local server that
// answers 503, so the background model sync never reaches a real provider.
func newOfflineRouter(t *testing.T, mutate func(cfg *config.Config)) *Router {
	t.Helper()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(dead.Close)
	cfg := config.DefaultConfig()
	creds := config.ProviderCreds{BaseURL: dead.URL}
	cfg.Providers = config.ProvidersConfig{
		OpenAI: creds, Anthropic: creds, NVIDIANIM: creds, Groq: creds, Gemini: creds,
		OpenRouter: creds, Ollama: creds, Kilo: creds, Cline: creds,
	}
	cfg.Routes.DefaultStrategy = ""
	if mutate != nil {
		mutate(cfg)
	}
	return NewRouter(cfg)
}

func userReq(model string) *provider.UnifiedChatRequest {
	return &provider.UnifiedChatRequest{Model: model, Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "hi"}}}
}

func targetModels(targets []TargetSpec) []string {
	out := make([]string, len(targets))
	for i, t := range targets {
		out[i] = t.ProviderName + "/" + t.UpstreamModel
	}
	return out
}

func TestCircuitBreakerStringResetSnapshot(t *testing.T) {
	cb := NewCircuitBreaker("alpha/model")
	if s := cb.Snapshot(); s.State != StateClosed || s.CooldownRemaining != 0 || s.Name != "alpha/model" {
		t.Fatalf("fresh snapshot = %+v", s)
	}
	for i := 0; i < 5; i++ {
		cb.RecordFailure()
	}
	snap := cb.Snapshot()
	if snap.State != StateOpen || snap.ConsecutiveFailures != 5 {
		t.Fatalf("after 5 failures snapshot = %+v, want OPEN with 5 failures", snap)
	}
	if snap.CooldownRemaining <= 0 || snap.CooldownRemaining > 30 {
		t.Fatalf("cooldown remaining = %v, want within (0, 30]", snap.CooldownRemaining)
	}
	if got := cb.String(); got != "[alpha/model] State: OPEN (Failures: 5)" {
		t.Fatalf("String() = %q", got)
	}

	cb.Reset()
	if state, fails := cb.State(); state != StateClosed || fails != 0 {
		t.Fatalf("after Reset state = %s/%d, want CLOSED/0", state, fails)
	}
	if !cb.Allow() {
		t.Fatal("reset breaker must allow requests")
	}
}

func TestCircuitBreakerHalfOpenFailureBacksOff(t *testing.T) {
	cb := NewCircuitBreaker("beta")
	cb.currentCooldown = time.Millisecond
	cb.TripImmediate()
	time.Sleep(3 * time.Millisecond)
	if !cb.Allow() {
		t.Fatal("expected a probe after the cooldown")
	}
	if state, _ := cb.State(); state != StateHalfOpen {
		t.Fatalf("state = %s, want HALF_OPEN", state)
	}
	cb.RecordFailure()
	if state, _ := cb.State(); state != StateOpen {
		t.Fatalf("failed probe left state %s, want OPEN", state)
	}
	if cb.currentCooldown != 2*time.Millisecond {
		t.Fatalf("cooldown = %s, want doubled to 2ms", cb.currentCooldown)
	}

	// The backoff is capped at five minutes.
	cb.state = StateHalfOpen
	cb.currentCooldown = 4 * time.Minute
	cb.RecordFailure()
	if cb.currentCooldown != 5*time.Minute {
		t.Fatalf("cooldown = %s, want capped at 5m", cb.currentCooldown)
	}
	cb.Reset()
	if cb.currentCooldown != 30*time.Second {
		t.Fatalf("Reset cooldown = %s, want the 30s default", cb.currentCooldown)
	}
}

func TestRouterDefaultStrategy(t *testing.T) {
	r := newOfflineRouter(t, nil)
	if got := r.DefaultStrategy(); got != "auto-resilient" {
		t.Fatalf("DefaultStrategy() = %q, want auto-resilient", got)
	}
	if err := r.SetDefaultStrategy("no-such-route"); err == nil || !strings.Contains(err.Error(), "unknown routing strategy") {
		t.Fatalf("SetDefaultStrategy(unknown) err = %v", err)
	}
	if err := r.SetDefaultStrategy("free-first"); err != nil {
		t.Fatal(err)
	}
	if got := r.DefaultStrategy(); got != "free-first" {
		t.Fatalf("DefaultStrategy() = %q after set, want free-first", got)
	}
	if got := r.ResolveTargets("gpt-4o", ""); got[0].ProviderName != "groq" {
		t.Fatalf("free-first default should route gpt-4o to groq first, got %v", got[0])
	}
	if err := r.SetDefaultStrategy("premium-only"); err != nil {
		t.Fatal(err)
	}
	if got := targetModels(r.ResolveTargets("gpt-4o", "")); !reflect.DeepEqual(got, []string{"anthropic/claude-opus-5-5", "openai/gpt-4o"}) {
		t.Fatalf("premium-only default targets = %v", got)
	}
}

func TestResolveTargetsProviderPrefixes(t *testing.T) {
	r := newOfflineRouter(t, nil)
	cases := []struct {
		model     string
		wantFirst string
	}{
		{"groq/QWEN/qwen3.8-27b", "groq/qwen/qwen3.8-27b"},
		{"groq/unlisted-model", "groq/groq/unlisted-model"},
		{"nvidianim/Google/Gemma-4-31b-it", "nvidianim/google/gemma-4-31b-it"},
		{"cline/qwen/qwen3.8-27b:free", "cline/qwen/qwen3.8-27b:free"},
		{"cline/unlisted", "cline/cline/unlisted"},
		{"kilo/kilo-auto/free", "kilo/kilo-auto/free"},
		{"kilo/auto", "kilo/kilo-auto/free"},
		{"kilo-auto/free", "kilo/kilo-auto/free"},
		{"openrouter/google/gemma-4-31b-it:free", "openrouter/google/gemma-4-31b-it:free"},
		{"some-vendor/model:free", "openrouter/some-vendor/model:free"},
		{"openai/gpt-oss-120b", "groq/openai/gpt-oss-120b"},
		{"deepseek-ai/deepseek-v4-flash-0731", "nvidianim/deepseek-ai/deepseek-v4-flash-0731"},
		{"claude-haiku-5", "anthropic/claude-haiku-5"},
	}
	for _, tc := range cases {
		targets := targetModels(r.ResolveTargets(tc.model, ""))
		if len(targets) < 2 || targets[0] != tc.wantFirst {
			t.Errorf("ResolveTargets(%q) first = %v, want %s", tc.model, targets, tc.wantFirst)
			continue
		}
		for _, later := range targets[1:] {
			if later == tc.wantFirst && !strings.HasPrefix(tc.model, "claude") {
				t.Errorf("ResolveTargets(%q) repeats its primary target %s in the fallback tail", tc.model, later)
			}
		}
	}

	if got := r.ResolveTargets("meta-llama-3", ""); !reflect.DeepEqual(got, r.routes["free-first"].Targets) {
		t.Errorf("llama models should use the free-first route, got %v", targetModels(got))
	}
	def := targetModels(r.ResolveTargets("gpt-4o", "unknown-alias"))
	if def[0] != "openai/gpt-4o" || def[1] != "nvidianim/deepseek-ai/deepseek-v4-flash-0731" {
		t.Errorf("unknown model default targets = %v, want openai first then nvidianim", def)
	}
}

func TestResolveTargetsActiveModelsFromListings(t *testing.T) {
	r := newOfflineRouter(t, nil)
	r.modelsMu.Lock()
	r.activeModels["openrouter"] = []provider.ModelInfo{{ID: "vendor/or-special", Provider: "openrouter", Active: true}}
	r.activeModels["kilo"] = []provider.ModelInfo{{ID: "vendor/kilo-special", Provider: "kilo", Active: true}}
	r.activeModels["cline"] = []provider.ModelInfo{{ID: "vendor/cline-special", Provider: "cline", Active: true}}
	r.modelsMu.Unlock()

	for model, want := range map[string]string{
		"vendor/or-special":    "openrouter/vendor/or-special",
		"vendor/kilo-special":  "kilo/vendor/kilo-special",
		"vendor/cline-special": "cline/vendor/cline-special",
	} {
		got := targetModels(r.ResolveTargets(model, ""))
		if got[0] != want {
			t.Errorf("ResolveTargets(%q) first = %s, want %s", model, got[0], want)
		}
	}
}

func TestRouterBreakerManagement(t *testing.T) {
	r := newOfflineRouter(t, nil)
	groq, _ := r.GetBreaker("groq")
	groq.TripImmediate()
	mixed := r.getTargetBreaker("alpha", "Model-X")
	mixed.TripImmediate()

	if !r.ResetCircuitBreaker("GROQ") {
		t.Fatal("ResetCircuitBreaker should match provider names case-insensitively")
	}
	if state, _ := groq.State(); state != StateClosed {
		t.Fatalf("groq breaker = %s after reset", state)
	}
	if !r.ResetCircuitBreaker("alpha/Model-X") {
		t.Fatal("ResetCircuitBreaker should find a mixed-case target key by exact name")
	}
	if r.ResetCircuitBreaker("does-not-exist") {
		t.Fatal("ResetCircuitBreaker reported success for an unknown breaker")
	}

	groq.TripImmediate()
	mixed.TripImmediate()
	all := r.CircuitBreakers()
	if all["groq"] != groq || all["alpha/Model-X"] != mixed {
		t.Fatal("CircuitBreakers() must return the live breaker instances")
	}
	delete(all, "groq")
	if _, ok := r.GetBreaker("groq"); !ok {
		t.Fatal("mutating the CircuitBreakers() map must not affect the router")
	}

	snaps := r.CircuitBreakerSnapshots()
	seenSlash := false
	for i, s := range snaps {
		hasSlash := strings.Contains(s.Name, "/")
		if seenSlash && !hasSlash {
			t.Fatalf("provider breaker %q listed after target breakers", s.Name)
		}
		seenSlash = seenSlash || hasSlash
		if i > 0 && strings.Contains(snaps[i-1].Name, "/") == hasSlash && snaps[i-1].Name > s.Name {
			t.Fatalf("snapshots not sorted: %q before %q", snaps[i-1].Name, s.Name)
		}
		if s.Name == "groq" && s.State != StateOpen {
			t.Fatalf("groq snapshot state = %s, want OPEN", s.State)
		}
	}

	r.ResetCircuitBreakers()
	for name, cb := range r.CircuitBreakers() {
		if state, _ := cb.State(); state != StateClosed {
			t.Fatalf("breaker %s = %s after ResetCircuitBreakers", name, state)
		}
	}
}

func TestRouterUpdateProviderAndTestProvider(t *testing.T) {
	var (
		mu    sync.Mutex
		auths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		auths = append(auths, req.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"synced-model","context_window":4242}]}`))
	}))
	defer srv.Close()

	r := newOfflineRouter(t, nil)
	cb, _ := r.GetBreaker("groq")
	cb.TripImmediate()

	if err := r.UpdateProvider("GROQ", config.ProviderCreds{APIKey: "k-new", BaseURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	if got := r.cfg.Providers.Groq; got.APIKey != "k-new" || got.BaseURL != srv.URL {
		t.Fatalf("config not updated: %+v", got)
	}
	if state, _ := cb.State(); state != StateClosed {
		t.Fatalf("breaker = %s, want reset to CLOSED on credential update", state)
	}

	ok, _, err := r.TestProvider(context.Background(), "groq")
	if !ok || err != nil {
		t.Fatalf("TestProvider = %v, %v", ok, err)
	}
	mu.Lock()
	gotAuth := append([]string(nil), auths...)
	mu.Unlock()
	if len(gotAuth) == 0 || gotAuth[len(gotAuth)-1] != "Bearer k-new" {
		t.Fatalf("health check auth headers = %v, want the new key", gotAuth)
	}

	// The update triggers an asynchronous model resync against the new base URL.
	deadline := time.Now().Add(5 * time.Second)
	for !r.IsActiveModel("groq", "synced-model") {
		if time.Now().After(deadline) {
			t.Fatal("groq models were not resynced after UpdateProvider")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if w := r.GetModelContextWindow("groq", "synced-model"); w != 4242 {
		t.Fatalf("context window = %d, want 4242 from the listing", w)
	}

	// Every known provider name updates its config slot.
	for _, name := range []string{"openai", "anthropic", "nvidianim", "gemini", "openrouter", "ollama", "kilo", "cline"} {
		if err := r.UpdateProvider(name, config.ProviderCreds{APIKey: "key-" + name}); err != nil {
			t.Fatalf("UpdateProvider(%s): %v", name, err)
		}
	}
	p := r.cfg.Providers
	for name, got := range map[string]string{
		"openai": p.OpenAI.APIKey, "anthropic": p.Anthropic.APIKey, "nvidianim": p.NVIDIANIM.APIKey,
		"gemini": p.Gemini.APIKey, "openrouter": p.OpenRouter.APIKey, "ollama": p.Ollama.APIKey,
		"kilo": p.Kilo.APIKey, "cline": p.Cline.APIKey,
	} {
		if got != "key-"+name {
			t.Errorf("config key for %s = %q", name, got)
		}
	}

	if err := r.UpdateProvider("nope", config.ProviderCreds{}); err == nil {
		t.Fatal("UpdateProvider(unknown) should fail")
	}
	if _, _, err := r.TestProvider(context.Background(), "nope"); err == nil {
		t.Fatal("TestProvider(unknown) should fail")
	}
	r.SetProvider("broken", &scriptProvider{name: "broken", healthErr: errors.New("down")})
	if ok, _, err := r.TestProvider(context.Background(), "broken"); ok || err == nil || err.Error() != "down" {
		t.Fatalf("TestProvider(broken) = %v, %v; want false with the health error", ok, err)
	}
	if r.Translator() == nil || r.Translator() != r.translator {
		t.Fatal("Translator() must return the router's translator")
	}
}

func TestSyncProviderModels(t *testing.T) {
	r := newOfflineRouter(t, nil)
	if _, err := r.SyncProviderModels(context.Background(), "nope"); err == nil {
		t.Fatal("expected an error for an unknown provider")
	}

	// A provider without a listing returns its cached models.
	r.SetProvider("groq", &scriptProvider{name: "groq"})
	got, err := r.SyncProviderModels(context.Background(), "groq")
	if err != nil || len(got) != len(defaultGroqActiveModels) {
		t.Fatalf("non-lister sync = %d models, %v; want the %d seeded groq models", len(got), err, len(defaultGroqActiveModels))
	}

	lp := &listerProvider{scriptProvider: scriptProvider{name: "groq"}}
	lp.setModels([]provider.ModelInfo{
		{ID: "live-a", Provider: "groq", Active: true, ContextWindow: 8000},
		{ID: "retired-b", Provider: "groq", Active: false},
	}, nil)
	r.SetProvider("groq", lp)
	got, err = r.SyncProviderModels(context.Background(), "GROQ")
	if err != nil || len(got) != 1 || got[0].ID != "live-a" {
		t.Fatalf("sync = %+v, %v; want only the active groq model", got, err)
	}
	if _, ok := r.GetBreaker("groq/live-a"); !ok {
		t.Fatal("sync should create a breaker for each discovered model")
	}
	if r.IsActiveModel("groq", "retired-b") {
		t.Fatal("inactive groq models must be filtered out")
	}

	// An empty listing keeps the previous models.
	lp.setModels(nil, nil)
	got, err = r.SyncProviderModels(context.Background(), "groq")
	if err != nil || len(got) != 1 || got[0].ID != "live-a" {
		t.Fatalf("empty listing sync = %+v, %v; want the cached live-a", got, err)
	}

	// A listing error returns the cached models and the error.
	lp.setModels(nil, errors.New("listing unavailable"))
	got, err = r.SyncProviderModels(context.Background(), "groq")
	if err == nil || len(got) != 1 {
		t.Fatalf("failed listing sync = %+v, %v; want cached models and an error", got, err)
	}

	all := r.SyncAllProviderModels(context.Background())
	if len(all["groq"]) != 1 {
		t.Fatalf("SyncAllProviderModels groq = %+v", all["groq"])
	}
}

func TestActiveModelDefaultsAndContextWindows(t *testing.T) {
	r := newOfflineRouter(t, nil)
	r.modelsMu.Lock()
	r.activeModels = map[string][]provider.ModelInfo{}
	r.modelsMu.Unlock()

	defaults := map[string][]provider.ModelInfo{
		"groq": defaultGroqActiveModels, "nvidianim": defaultNVIDIANIMActiveModels,
		"openrouter": defaultOpenRouterActiveModels, "kilo": defaultKiloActiveModels, "cline": defaultClineActiveModels,
	}
	total := 0
	for name, want := range defaults {
		got := r.GetProviderActiveModels(strings.ToUpper(name))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("GetProviderActiveModels(%s) did not fall back to the seed list", name)
		}
		if len(got) > 0 {
			got[0].ID = "mutated"
			if r.GetProviderActiveModels(name)[0].ID == "mutated" {
				t.Errorf("GetProviderActiveModels(%s) returned the shared seed slice", name)
			}
		}
		total += len(want)
	}
	if got := r.GetProviderActiveModels("gemini"); got != nil {
		t.Errorf("unlisted provider models = %v, want nil", got)
	}
	if got := len(r.GetAllActiveModels(context.Background())); got != total {
		t.Errorf("GetAllActiveModels = %d models, want the %d seeded ones", got, total)
	}

	if !r.IsActiveModel("groq", "groq/allam-2-7b") {
		t.Error("IsActiveModel should accept a provider-prefixed model ID")
	}
	if r.IsActiveModel("cline", "deepseek/deepseek-v4-flash-0731:free") {
		t.Error("IsActiveModel must skip models listed as inactive")
	}
	windows := []struct {
		provider, model string
		want            int
	}{
		{"groq", "groq/allam-2-7b", 4096},
		{"gemini", "anything", 1048576},
		{"anthropic", "claude", 200000},
		{"openai", "gpt", 128000},
		{"groq", "unknown", 131072},
		{"nvidianim", "unknown", 131072},
		{"openrouter", "unknown", 262144},
		{"kilo", "unknown", 262144},
		{"cline", "unknown", 262144},
		{"custom", "unknown", 131072},
	}
	for _, w := range windows {
		if got := r.GetModelContextWindow(w.provider, w.model); got != w.want {
			t.Errorf("GetModelContextWindow(%s, %s) = %d, want %d", w.provider, w.model, got, w.want)
		}
	}
}

func TestDispatchAllTargetsFail(t *testing.T) {
	r := newOfflineRouter(t, nil)
	p := &scriptProvider{name: "alpha", errs: map[string]error{
		"first":  errors.New("alpha returned status 500: boom"),
		"second": errors.New("alpha returned status 502: bad gateway"),
	}}
	r.SetProvider("alpha", p)
	r.SetRoute(Route{ID: "two", Strategy: StrategyFallback, Targets: []TargetSpec{
		{ProviderName: "alpha", UpstreamModel: "first"},
		{ProviderName: "alpha", UpstreamModel: "second"},
	}})

	_, winner, err := r.DispatchChat(context.Background(), userReq("two"), "")
	if err == nil || winner != "" {
		t.Fatalf("expected failure, got winner %q err %v", winner, err)
	}
	if !strings.Contains(err.Error(), "all providers in fallback chain failed") || !strings.Contains(err.Error(), "bad gateway") {
		t.Fatalf("error = %v, want the chain failure wrapping the last error", err)
	}
	if got := p.callLog(); !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("calls = %v, want first then second", got)
	}
	cb, _ := r.GetBreaker("alpha/first")
	if _, fails := cb.State(); fails != 1 {
		t.Fatalf("alpha/first failures = %d, want 1 (5xx counts toward the breaker)", fails)
	}
}

func TestDispatchCanceledBeforeFirstAttempt(t *testing.T) {
	r := newOfflineRouter(t, nil)
	p := &scriptProvider{name: "alpha"}
	r.SetProvider("alpha", p)
	r.SetRoute(Route{ID: "one", Strategy: StrategyFallback, Targets: []TargetSpec{{ProviderName: "alpha", UpstreamModel: "m"}}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := r.DispatchChat(ctx, userReq("one"), "one")
	if err == nil || !strings.Contains(err.Error(), "request canceled before reaching alpha") || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a cancellation before the first attempt", err)
	}
	if len(p.callLog()) != 0 {
		t.Fatal("no upstream call should be made for a canceled request")
	}
}

func TestDispatchSkipsUnusableTargets(t *testing.T) {
	r := newOfflineRouter(t, func(cfg *config.Config) {
		cfg.Routes.ExcludedModels = []string{"vendor/banned-model:free"}
	})
	alpha := &scriptProvider{name: "alpha"}
	groq := &scriptProvider{name: "groq"}
	openrouter := &scriptProvider{name: "openrouter"}
	r.SetProvider("alpha", alpha)
	r.SetProvider("groq", groq)
	r.SetProvider("openrouter", openrouter)
	r.getTargetBreaker("alpha", "tripped").TripImmediate()
	r.SetRoute(Route{ID: "skips", Strategy: StrategyFallback, Targets: []TargetSpec{
		{ProviderName: "ghost", UpstreamModel: "unregistered-provider"},
		{ProviderName: "alpha", UpstreamModel: "tripped"},
		{ProviderName: "alpha", UpstreamModel: "banned-model"},
		{ProviderName: "groq", UpstreamModel: "allam-2-7b"},            // 4096-token window, prompt is larger
		{ProviderName: "openrouter", UpstreamModel: "not-listed"},      // not an active listed model
		{ProviderName: "openrouter", UpstreamModel: "openrouter/auto"}, // alias for openrouter/free
	}})

	req := userReq("skips")
	req.Messages[0].Content = strings.Repeat("x", 20000) // about 5000 tokens
	resp, winner, err := r.DispatchChat(context.Background(), req, "")
	if err != nil {
		t.Fatal(err)
	}
	if winner != "openrouter" || resp.Content != "ok from openrouter/free" {
		t.Fatalf("winner %q content %q; want openrouter serving the resolved alias", winner, resp.Content)
	}
	if len(alpha.callLog()) != 0 || len(groq.callLog()) != 0 {
		t.Fatalf("skipped targets were called: alpha %v groq %v", alpha.callLog(), groq.callLog())
	}
	if got := openrouter.callLog(); !reflect.DeepEqual(got, []string{"openrouter/free"}) {
		t.Fatalf("openrouter calls = %v, want only the alias target", got)
	}
}

func TestDispatchNoUsableTargetReportsClearError(t *testing.T) {
	r := newOfflineRouter(t, nil)
	p := &scriptProvider{name: "alpha"}
	r.SetProvider("alpha", p)
	r.SetRoute(Route{ID: "tiny", Strategy: StrategyFallback, Targets: []TargetSpec{{ProviderName: "alpha", UpstreamModel: "small"}}})

	// About 150k tokens: no tier fits the 131k default window, so the chain falls back to the
	// original targets and then bypasses each one.
	req := userReq("tiny")
	req.RawPayload = make([]byte, 600000)
	_, _, err := r.DispatchChat(context.Background(), req, "")
	if err == nil {
		t.Fatal("expected an error when every target is bypassed")
	}
	if strings.Contains(err.Error(), "%!") {
		t.Fatalf("error message has a formatting artifact: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "all providers in fallback chain failed") {
		t.Fatalf("err = %v", err)
	}
	if len(p.callLog()) != 0 {
		t.Fatal("an oversized prompt must not be sent to a small-window target")
	}
}

func grepTool() []interface{} {
	return []interface{}{map[string]interface{}{
		"name": "Grep",
		"input_schema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":    map[string]interface{}{"type": "string"},
				"pattern": map[string]interface{}{"type": "string"},
				"limit":   map[string]interface{}{"type": "integer"},
			},
		},
	}}
}

func TestDispatchResponseInterceptors(t *testing.T) {
	bash := provider.UnifiedToolCall{ID: "c1", Type: "function"}
	bash.Function.Name = "Bash"
	bash.Function.Arguments = `{"command":"ls"}`

	cases := []struct {
		name        string
		tools       []interface{}
		reply       *provider.UnifiedChatResponse
		wantTool    string
		wantArgs    map[string]interface{}
		wantContent string
	}{
		{
			name:     "tool_call block becomes a structured call",
			reply:    &provider.UnifiedChatResponse{Content: `<tool_call>{"name":"Bash","arguments":{"command":"ls -la"}}</tool_call>`},
			wantTool: "Bash",
			wantArgs: map[string]interface{}{"command": "ls -la"},
		},
		{
			name:        "leaked DSML tags next to structured calls are stripped",
			reply:       &provider.UnifiedChatResponse{Content: "Running it now <｜DSML｜tool_calls></｜DSML｜tool_calls>", ToolCalls: []provider.UnifiedToolCall{bash}},
			wantTool:    "Bash",
			wantArgs:    map[string]interface{}{"command": "ls"},
			wantContent: "Running it now",
		},
		{
			name:     "gemini call:Name{...} text becomes a structured call",
			tools:    grepTool(),
			reply:    &provider.UnifiedChatResponse{Content: "call:default_api:Grep{path:internal/proxy/,pattern:Foo, bar,limit:5}"},
			wantTool: "Grep",
			wantArgs: map[string]interface{}{"path": "internal/proxy/", "pattern": "Foo, bar", "limit": float64(5)},
		},
		{
			name:     "call:Name with JSON arguments",
			tools:    grepTool(),
			reply:    &provider.UnifiedChatResponse{Content: `call:grep{"pattern":"x"}`},
			wantTool: "Grep",
			wantArgs: map[string]interface{}{"pattern": "x"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newOfflineRouter(t, nil)
			r.SetProvider("alpha", &scriptProvider{name: "alpha", replies: map[string]*provider.UnifiedChatResponse{"m": tc.reply}})
			r.SetRoute(Route{ID: "one", Strategy: StrategyFallback, Targets: []TargetSpec{{ProviderName: "alpha", UpstreamModel: "m"}}})
			req := userReq("one")
			req.Tools = tc.tools
			resp, _, err := r.DispatchChat(context.Background(), req, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Name != tc.wantTool {
				t.Fatalf("tool calls = %+v, want one %s call", resp.ToolCalls, tc.wantTool)
			}
			var args map[string]interface{}
			if err := json.Unmarshal([]byte(resp.ToolCalls[0].Function.Arguments), &args); err != nil {
				t.Fatalf("arguments %q: %v", resp.ToolCalls[0].Function.Arguments, err)
			}
			if !reflect.DeepEqual(args, tc.wantArgs) {
				t.Fatalf("arguments = %v, want %v", args, tc.wantArgs)
			}
			if resp.Content != tc.wantContent {
				t.Fatalf("content = %q, want %q", resp.Content, tc.wantContent)
			}
		})
	}
}

func TestDispatchRejectsBadTurnsAndFailsOver(t *testing.T) {
	loopReq := func() *provider.UnifiedChatRequest {
		return &provider.UnifiedChatRequest{Model: "pair", Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "run the build"},
			{Role: "assistant", Content: "The build output looks identical to before."},
			{Role: "tool", Content: "build ok"},
		}}
	}
	cases := []struct {
		name    string
		req     func() *provider.UnifiedChatRequest
		reply   string
		wantErr string
		tripped bool
	}{
		{
			name:    "unparseable leaked call",
			req:     func() *provider.UnifiedChatRequest { r := userReq("pair"); r.Tools = grepTool(); return r },
			reply:   "call:Unknown{x}",
			wantErr: "unparseable tool call",
		},
		{
			name:    "stalled agent turn",
			req:     func() *provider.UnifiedChatRequest { r := userReq("pair"); r.Tools = grepTool(); return r },
			reply:   "Let me read the adapter file.",
			wantErr: "announced a tool action",
		},
		{
			name:    "text repetition loop",
			req:     loopReq,
			reply:   "The build output looks identical to before.",
			wantErr: "repetition loop",
			tripped: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newOfflineRouter(t, nil)
			p := &scriptProvider{name: "alpha", replies: map[string]*provider.UnifiedChatResponse{
				"bad": {Content: tc.reply},
			}}
			r.SetProvider("alpha", p)
			r.SetRoute(Route{ID: "pair", Strategy: StrategyFallback, Targets: []TargetSpec{
				{ProviderName: "alpha", UpstreamModel: "bad"},
				{ProviderName: "alpha", UpstreamModel: "good"},
			}})
			var attempts []AttemptResult
			ctx := WithAttemptObserver(context.Background(), func(a AttemptResult) { attempts = append(attempts, a) })
			resp, _, err := r.DispatchChat(ctx, tc.req(), "")
			if err != nil {
				t.Fatal(err)
			}
			if resp.Content != "ok from good" {
				t.Fatalf("content = %q, want the fallback reply", resp.Content)
			}
			if len(attempts) != 2 || attempts[0].Err == nil || !strings.Contains(attempts[0].Err.Error(), tc.wantErr) {
				t.Fatalf("attempts = %+v, want the first rejected with %q", attempts, tc.wantErr)
			}
			if attempts[0].Response == nil || attempts[0].Response.Content != tc.reply {
				t.Fatalf("observer should see the raw reply, got %+v", attempts[0].Response)
			}
			cb, _ := r.GetBreaker("alpha/bad")
			if state, _ := cb.State(); (state == StateOpen) != tc.tripped {
				t.Fatalf("alpha/bad breaker = %s, tripped want %v", state, tc.tripped)
			}
		})
	}
}

func TestParseLeakedArgsRejectsUnknownLeadingKey(t *testing.T) {
	types := map[string]string{"path": "string"}
	if _, ok := parseLeakedArgs("other:1,path:x", types); ok {
		t.Fatal("arguments that do not start with a declared parameter must be rejected")
	}
	if _, ok := parseLeakedArgs("path x", nil); ok {
		t.Fatal("non-JSON arguments without declared parameters must be rejected")
	}
	if args, ok := parseLeakedArgs("  ", types); !ok || len(args) != 0 {
		t.Fatalf("empty body = %v, %v; want an empty argument map", args, ok)
	}
	if args, ok := parseLeakedArgs(`path:"quoted value"`, types); !ok || args["path"] != "quoted value" {
		t.Fatalf("quoted value = %v, %v", args, ok)
	}
	if _, calls, leaked := extractLeakedCalls("call:Grep{path:x", grepTool()); !leaked || calls != nil {
		t.Fatal("an unterminated call body must be reported as leaked")
	}
	if clean, calls, leaked := extractLeakedCalls("plain answer", grepTool()); clean != "plain answer" || calls != nil || leaked {
		t.Fatal("text without call syntax must pass through")
	}

	openAITools := []interface{}{
		"not-a-map",
		map[string]interface{}{"type": "function", "function": map[string]interface{}{
			"name":       "Read",
			"parameters": map[string]interface{}{"properties": map[string]interface{}{"file_path": map[string]interface{}{"type": "string"}, "raw": true}},
		}},
		map[string]interface{}{"input_schema": map[string]interface{}{}},
	}
	params := declaredToolParams(openAITools)
	if !reflect.DeepEqual(params, map[string]map[string]string{"Read": {"file_path": "string", "raw": ""}}) {
		t.Fatalf("declaredToolParams = %v", params)
	}
}

type failingRouteStore struct {
	loadErr, saveErr, deleteErr error
}

func (s failingRouteStore) LoadRoutes(ctx context.Context) ([]Route, error) { return nil, s.loadErr }
func (s failingRouteStore) SaveRoute(ctx context.Context, route Route) error {
	return s.saveErr
}
func (s failingRouteStore) DeleteRoute(ctx context.Context, id string) error { return s.deleteErr }

func TestRouteStoreErrors(t *testing.T) {
	r := newOfflineRouter(t, nil)
	if err := r.SetRouteStore(context.Background(), failingRouteStore{loadErr: errors.New("load failed")}); err == nil {
		t.Fatal("SetRouteStore should return the load error")
	}

	store := failingRouteStore{saveErr: errors.New("disk full"), deleteErr: errors.New("locked")}
	if err := r.SetRouteStore(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	route := Route{ID: "custom", Targets: []TargetSpec{{ProviderName: "groq", UpstreamModel: "qwen/qwen3.8-27b"}}}
	if err := r.UpsertRoute(context.Background(), route); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("UpsertRoute err = %v, want the save error", err)
	}
	if _, ok := r.routes["custom"]; ok {
		t.Fatal("a route whose save failed must not go live")
	}
	found, err := r.ResetRoute(context.Background(), "free-first")
	if !found || err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("ResetRoute = %v, %v; want found with the delete error", found, err)
	}

	many := Route{ID: "huge"}
	for i := 0; i <= maxRouteTargets; i++ {
		many.Targets = append(many.Targets, TargetSpec{ProviderName: "groq", UpstreamModel: "m"})
	}
	r.routeStore = nil
	if err := r.UpsertRoute(context.Background(), many); err == nil || !strings.Contains(err.Error(), "the limit is") {
		t.Fatalf("UpsertRoute(too many targets) err = %v", err)
	}

	// Editing only the description marks a built-in route as customized.
	edited := copyRoute(r.routes["premium-only"])
	edited.Description = "changed"
	if err := r.UpsertRoute(context.Background(), edited); err != nil {
		t.Fatal(err)
	}
	for _, info := range r.Routes() {
		if info.ID == "premium-only" && !info.Customized {
			t.Fatal("premium-only should be reported as customized")
		}
	}
}

func openRawDB(t *testing.T) *sql.DB {
	t.Helper()
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	database, err := db.Open(filepath.Join(t.TempDir(), "extra.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database.DB
}

func TestSQLStoresReportDatabaseErrors(t *testing.T) {
	ctx := context.Background()

	raw := openRawDB(t)
	if _, err := raw.Exec(`DROP TABLE route_targets`); err != nil {
		t.Fatal(err)
	}
	routes := NewSQLRouteStore(raw)
	route := Route{ID: "r1", Strategy: StrategyFallback, Targets: []TargetSpec{{ProviderName: "groq", UpstreamModel: "m"}}}
	if err := routes.SaveRoute(ctx, route); err == nil {
		t.Fatal("SaveRoute should fail when route_targets is missing")
	}
	if err := routes.DeleteRoute(ctx, "r1"); err == nil {
		t.Fatal("DeleteRoute should fail when route_targets is missing")
	}
	if _, err := routes.LoadRoutes(ctx); err == nil || !strings.Contains(err.Error(), "load routes") {
		t.Fatalf("LoadRoutes err = %v", err)
	}

	closed := openRawDB(t)
	_ = closed.Close()
	if err := NewSQLRouteStore(closed).SaveRoute(ctx, route); err == nil {
		t.Fatal("SaveRoute on a closed database should fail")
	}
	if err := NewSQLRouteStore(closed).DeleteRoute(ctx, "r1"); err == nil {
		t.Fatal("DeleteRoute on a closed database should fail")
	}
	if _, err := NewSQLCatalogStore(closed).LoadCatalog(ctx); err == nil || !strings.Contains(err.Error(), "load model catalog") {
		t.Fatalf("LoadCatalog err = %v", err)
	}
}

type failingCatalogStore struct {
	loadErr error
	mu      sync.Mutex
	saves   int
}

func (s *failingCatalogStore) LoadCatalog(ctx context.Context) ([]CatalogEntry, error) {
	return nil, s.loadErr
}

func (s *failingCatalogStore) SaveCatalogEntry(ctx context.Context, e CatalogEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	return errors.New("write failed")
}

func TestModelCatalogStoreErrors(t *testing.T) {
	c := NewModelCatalog(0)
	if c.recheck != defaultModelRecheck {
		t.Fatalf("recheck = %s, want the default for a non-positive value", c.recheck)
	}
	if err := c.SetStore(context.Background(), &failingCatalogStore{loadErr: errors.New("no table")}); err == nil {
		t.Fatal("SetStore should return the load error")
	}

	store := &failingCatalogStore{}
	if err := c.SetStore(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	// A failed write is logged, not fatal: the in-memory status still changes.
	c.MarkInactive("alpha", "gone", "status 404")
	if c.Usable("alpha", "gone") {
		t.Fatal("model should be unusable after MarkInactive even when persisting fails")
	}
	store.mu.Lock()
	saves := store.saves
	store.mu.Unlock()
	if saves == 0 {
		t.Fatal("MarkInactive should try to persist the entry")
	}
}

func TestResolvePlanTextSources(t *testing.T) {
	cases := []struct {
		name, text, thinking, args, want string
	}{
		{"arguments first", "visible", "", `{"plan":"from args"}`, "from args"},
		{"visible text", "  visible plan  ", "", "", "visible plan"},
		{"plan header in thinking", "", "musing\n## Plan\n1. do a\n2. do b\nNow I will call ExitPlanMode", "", "## Plan\n1. do a\n2. do b"},
		{"numbered steps in thinking", "", "hmm\n1. first\n2. second", "", "1. first\n2. second"},
		{"unstructured thinking is not a plan", "", "just thinking out loud", "", ""},
	}
	for _, tc := range cases {
		if got := resolvePlanText(tc.text, tc.thinking, tc.args); got != tc.want {
			t.Errorf("%s: resolvePlanText = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestExtractPlanFromArgumentsShapes(t *testing.T) {
	longText := "This is a sufficiently long free-form value"
	cases := []struct {
		name, args, want string
	}{
		{"empty", "", ""},
		{"json string", `"the whole plan"`, "the whole plan"},
		{"garbage", `not json at all`, ""},
		{"truncated object repaired", `{"plan":"partial plan`, "partial plan"},
		{"summary key", `{"summary":"short summary"}`, "short summary"},
		{"steps string", `{"steps":"1. a\n2. b"}`, "1. a\n2. b"},
		{"steps objects", `{"steps":[{"title":"A","description":"do a"},{"description":"do b"},{"step":"do c"},"do d"]}`,
			"### Implementation Steps\n1. **A**: do a\n2. do b\n3. do c\n4. do d"},
		{"empty steps", `{"steps":[]}`, ""},
		{"long fallback value", `{"x":"` + longText + `"}`, longText},
		{"markdown fallback value", `{"x":"# H"}`, "# H"},
		{"nothing usable", `{"x":"short","n":3}`, ""},
	}
	for _, tc := range cases {
		if got, _ := extractPlanFromArguments(tc.args); got != tc.want {
			t.Errorf("%s: extractPlanFromArguments = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestExtractPlanFromThinking(t *testing.T) {
	cases := map[string]string{
		"":                                  "",
		"### Implementation Plan\nstep one": "### Implementation Plan\nstep one",
		"intro\n1. alpha\n2. beta\ncalling ExitPlanMode now": "1. alpha\n2. beta",
		"free-form reasoning":                                "free-form reasoning",
	}
	for in, want := range cases {
		if got := extractPlanFromThinking(in); got != want {
			t.Errorf("extractPlanFromThinking(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStreamOpenAIToAnthropicEvents(t *testing.T) {
	for _, tc := range []struct {
		finish, wantStop string
	}{{"stop", "end_turn"}, {"length", "max_tokens"}} {
		in := make(chan provider.UnifiedSSEEvent, 4)
		in <- provider.UnifiedSSEEvent{Type: "text_delta", DeltaText: "He said \"hi\""}
		in <- provider.UnifiedSSEEvent{Type: "text_delta", DeltaText: ""}
		in <- provider.UnifiedSSEEvent{Type: "text_delta", DeltaText: " again"}
		in <- provider.UnifiedSSEEvent{Type: "finish", FinishReason: tc.finish}
		close(in)
		out := make(chan []byte, 16)
		NewTranslator().StreamOpenAIToAnthropicEvents(in, out, "model-x")

		var events []string
		for chunk := range out {
			events = append(events, string(chunk))
		}
		if len(events) != 7 {
			t.Fatalf("got %d events, want 7: %q", len(events), events)
		}
		wantPrefixes := []string{"event: message_start", "event: content_block_start", "event: content_block_delta",
			"event: content_block_delta", "event: content_block_stop", "event: message_delta", "event: message_stop"}
		for i, p := range wantPrefixes {
			if !strings.HasPrefix(events[i], p) {
				t.Fatalf("event %d = %q, want prefix %q", i, events[i], p)
			}
		}
		if !strings.Contains(events[0], `"model":"model-x"`) {
			t.Errorf("message_start = %q, want the target model", events[0])
		}
		if !strings.Contains(events[2], `"text":"He said \"hi\""`) {
			t.Errorf("delta = %q, want JSON-escaped text", events[2])
		}
		if !strings.Contains(events[5], `"stop_reason":"`+tc.wantStop+`"`) || !strings.Contains(events[5], `"output_tokens":2`) {
			t.Errorf("message_delta = %q, want stop %s and 2 output tokens", events[5], tc.wantStop)
		}
	}
}

func TestParseDSMLParamValueTypes(t *testing.T) {
	cases := []struct {
		raw, isString string
		want          interface{}
	}{
		{" 42 ", "true", "42"},
		{"null", "", nil},
		{"true", "", true},
		{"false", "", false},
		{"[1,2]", "", []interface{}{float64(1), float64(2)}},
		{"&lt;tag&gt;", "", "<tag>"},
		{"plain words", "", "plain words"},
	}
	for _, tc := range cases {
		if got := parseDSMLParamValue(tc.raw, tc.isString); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseDSMLParamValue(%q, %q) = %#v, want %#v", tc.raw, tc.isString, got, tc.want)
		}
	}
}

func TestParseToolCallBlocksVariants(t *testing.T) {
	cases := []struct {
		name, content, wantName, wantArgs, wantClean string
	}{
		{"input key", `pre <tool_call>{"name":"Read","input":{"file_path":"a.go"}}</tool_call>`, "Read", `{"file_path":"a.go"}`, "pre"},
		{"string arguments", `<tool_call>{"name":"Bash","arguments":"{\"command\":\"ls\"}"}</tool_call>`, "Bash", `{"command":"ls"}`, ""},
		{"unclosed block", `text <tool_call>{"name":"Glob","arguments":{"pattern":"*.go"}`, "Glob", `{"pattern":"*.go"}`, "text"},
		{"unclosed with input", `<tool_call>{"name":"Read","input":{"file_path":"b.go"}}`, "Read", `{"file_path":"b.go"}`, ""},
		{"unclosed string args", `<tool_call>{"name":"Bash","arguments":"raw"}`, "Bash", "raw", ""},
	}
	for _, tc := range cases {
		clean, calls := parseToolCallBlocks(tc.content)
		if len(calls) != 1 || calls[0].Function.Name != tc.wantName || calls[0].Function.Arguments != tc.wantArgs || clean != tc.wantClean {
			t.Errorf("%s: parseToolCallBlocks = %q, %+v", tc.name, clean, calls)
		}
	}
	if clean, calls := parseToolCallBlocks(`<tool_call>{"arguments":{}}</tool_call>`); calls != nil || clean == "" {
		t.Errorf("a block without a name must not produce calls, got %q %+v", clean, calls)
	}
}
