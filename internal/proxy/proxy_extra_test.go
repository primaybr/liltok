package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/primaybr/liltok/internal/admin"
	"github.com/primaybr/liltok/internal/cache"
	"github.com/primaybr/liltok/internal/cache/exact"
	"github.com/primaybr/liltok/internal/cache/semantic"
	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/ledger"
	"github.com/primaybr/liltok/internal/provider"
	"github.com/primaybr/liltok/internal/router"
	"github.com/primaybr/liltok/internal/server/middleware"
)

// offlineConfig returns a config whose providers all point at a local server that answers 503,
// so routers built from it never reach a real provider.
func offlineConfig(t *testing.T) *config.Config {
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
	cfg.Routes.CaptureDir = ""
	return cfg
}

func openExtraStore(t *testing.T) (*db.DB, *exact.TieredStore) {
	t.Helper()
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store, err := exact.NewTieredStore(database, 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return database, store
}

// extraProvider serves every model with reply, or fails with err when set.
type extraProvider struct {
	name  string
	reply *provider.UnifiedChatResponse
	err   error
	calls atomic.Int32
}

func (p *extraProvider) Name() string                                  { return p.name }
func (p *extraProvider) Tier() provider.ProviderTier                   { return provider.TierFree }
func (p *extraProvider) CheckHealth(ctx context.Context) (bool, error) { return true, nil }
func (p *extraProvider) StreamChat(ctx context.Context, req *provider.UnifiedChatRequest) (<-chan provider.UnifiedSSEEvent, <-chan error, error) {
	return nil, nil, errors.New("not used")
}

func (p *extraProvider) SendChat(ctx context.Context, req *provider.UnifiedChatRequest) (*provider.UnifiedChatResponse, error) {
	p.calls.Add(1)
	if p.err != nil {
		return nil, p.err
	}
	cp := *p.reply
	return &cp, nil
}

// subscribe connects to the broadcaster's SSE endpoint and returns decoded events after the
// initial "connected" event.
func subscribe(t *testing.T, b *admin.Broadcaster) <-chan admin.TelemetryEvent {
	t.Helper()
	srv := httptest.NewServer(b)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); srv.Close() })
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan admin.TelemetryEvent, 32)
	connected := make(chan struct{})
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		first := true
		for sc.Scan() {
			line := strings.TrimPrefix(sc.Text(), "data: ")
			if line == "" || line == sc.Text() {
				continue
			}
			var ev admin.TelemetryEvent
			if json.Unmarshal([]byte(line), &ev) != nil {
				continue
			}
			if first {
				first = false
				close(connected)
				continue
			}
			events <- ev
		}
	}()
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("broadcaster subscription did not connect")
	}
	return events
}

func nextEvent(t *testing.T, events <-chan admin.TelemetryEvent) (string, map[string]interface{}) {
	t.Helper()
	select {
	case ev := <-events:
		data, _ := ev.Data.(map[string]interface{})
		return ev.Type, data
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a broadcast event")
		return "", nil
	}
}

func serve(handler http.HandlerFunc, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	middleware.RequestID(middleware.Auth(handler)).ServeHTTP(rec, req)
	return rec
}

type upstreamHit struct {
	path, query, auth, apiKey, custom string
}

func recordingUpstream(t *testing.T, status int, body string, extraHeaders map[string]string) (*httptest.Server, func() []upstreamHit) {
	t.Helper()
	var (
		mu   sync.Mutex
		hits []upstreamHit
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, upstreamHit{r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("x-api-key"), r.Header.Get("X-Custom")})
		mu.Unlock()
		for k, v := range extraHeaders {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []upstreamHit {
		mu.Lock()
		defer mu.Unlock()
		return append([]upstreamHit(nil), hits...)
	}
}

func TestProxyLegacyEndpointsForwardToUpstream(t *testing.T) {
	upstream, hits := recordingUpstream(t, http.StatusOK, `{"object":"list","data":[{"embedding":[0.1]}]}`, nil)
	cfg := offlineConfig(t)
	cfg.Providers.OpenAI = config.ProviderCreds{BaseURL: upstream.URL + "/v1", APIKey: "cfg-key"}
	p := NewProxy(cfg, nil, nil, nil, nil, nil)

	cases := []struct {
		name    string
		handler http.HandlerFunc
		method  string
		target  string
		body    string
		want    string
	}{
		{"completions", p.HandleCompletions, http.MethodPost, "/v1/completions", `{"model":"gpt-3.5-turbo-instruct","prompt":"hi"}`, "/v1/completions"},
		{"embeddings", p.HandleEmbeddings, http.MethodPost, "/v1/embeddings?dims=8", `{"model":"text-embedding-3-small","input":"hi"}`, "/v1/embeddings"},
		{"models without router", p.HandleModels, http.MethodGet, "/v1/models", "", "/v1/models"},
	}
	for i, tc := range cases {
		rec := serve(tc.handler, tc.method, tc.target, tc.body, map[string]string{"X-Custom": "kept"})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", tc.name, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("X-Liltok-Provider") != "openai" || rec.Header().Get("X-Liltok-Cache-Status") != "MISS" {
			t.Errorf("%s: headers %v", tc.name, rec.Header())
		}
		got := hits()
		if len(got) != i+1 {
			t.Fatalf("%s: %d upstream hits, want %d", tc.name, len(got), i+1)
		}
		h := got[i]
		if h.path != tc.want || h.auth != "Bearer cfg-key" || h.custom != "kept" {
			t.Errorf("%s: upstream saw %+v", tc.name, h)
		}
		if tc.name == "embeddings" && h.query != "dims=8" {
			t.Errorf("query string not forwarded: %q", h.query)
		}
	}
}

func TestProxyDirectUpstreamAuthSelection(t *testing.T) {
	cases := []struct {
		name       string
		anthropic  bool
		headers    map[string]string
		wantAuth   string
		wantAPIKey string
	}{
		{"anthropic bearer client key", true, map[string]string{"Authorization": "Bearer client-key"}, "Bearer client-key", ""},
		{"anthropic x-api-key client key", true, map[string]string{"x-api-key": "client-key"}, "", "client-key"},
		{"anthropic configured key", true, nil, "", "cfg-ant-key"},
		{"openai client key", false, map[string]string{"Authorization": "Bearer client-key"}, "Bearer client-key", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream, hits := recordingUpstream(t, http.StatusOK, `{"id":"x","type":"message","content":[{"type":"text","text":"hello"}],"usage":{"input_tokens":3,"output_tokens":1}}`, nil)
			cfg := offlineConfig(t)
			cfg.Providers.Anthropic = config.ProviderCreds{BaseURL: upstream.URL, APIKey: "cfg-ant-key"}
			cfg.Providers.OpenAI = config.ProviderCreds{BaseURL: upstream.URL, APIKey: "cfg-oai-key"}
			p := NewProxy(cfg, nil, nil, nil, nil, nil)
			handler, path := p.HandleChatCompletions, "/v1/chat/completions"
			if tc.anthropic {
				handler, path = p.HandleAnthropicMessages, "/v1/messages"
			}
			rec := serve(handler, http.MethodPost, path, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, tc.headers)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			got := hits()
			if len(got) != 1 || got[0].path != path || got[0].auth != tc.wantAuth || got[0].apiKey != tc.wantAPIKey {
				t.Fatalf("upstream saw %+v; want auth %q x-api-key %q", got, tc.wantAuth, tc.wantAPIKey)
			}
		})
	}
}

func TestProxyDirectUpstreamFailures(t *testing.T) {
	t.Run("missing upstream key", func(t *testing.T) {
		upstream, hits := recordingUpstream(t, http.StatusOK, `{}`, nil)
		cfg := offlineConfig(t)
		cfg.Providers.Anthropic = config.ProviderCreds{BaseURL: upstream.URL}
		p := NewProxy(cfg, nil, nil, nil, nil, nil)
		rec := serve(p.HandleAnthropicMessages, http.MethodPost, "/v1/messages", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, nil)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "missing_upstream_api_key") {
			t.Fatalf("status %d body %s; want 401 missing_upstream_api_key", rec.Code, rec.Body.String())
		}
		if len(hits()) != 0 {
			t.Fatal("upstream must not be called without a key")
		}
	})

	t.Run("unreachable upstream", func(t *testing.T) {
		gone := httptest.NewServer(http.NotFoundHandler())
		gone.Close()
		cfg := offlineConfig(t)
		cfg.Providers.OpenAI = config.ProviderCreds{BaseURL: gone.URL, APIKey: "k"}
		p := NewProxy(cfg, nil, nil, nil, nil, nil)
		rec := serve(p.HandleChatCompletions, http.MethodPost, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, nil)
		if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "LILTOK_UPSTREAM_FAILED") {
			t.Fatalf("status %d body %s; want 502 LILTOK_UPSTREAM_FAILED", rec.Code, rec.Body.String())
		}
	})

	t.Run("upstream error status is passed through", func(t *testing.T) {
		upstream, _ := recordingUpstream(t, http.StatusBadRequest, `{"error":"bad model"}`, map[string]string{"X-Upstream-Trace": "abc"})
		cfg := offlineConfig(t)
		cfg.Providers.OpenAI = config.ProviderCreds{BaseURL: upstream.URL, APIKey: "k"}
		p := NewProxy(cfg, nil, nil, nil, nil, nil)
		rec := serve(p.HandleChatCompletions, http.MethodPost, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, nil)
		if rec.Code != http.StatusBadRequest || rec.Body.String() != `{"error":"bad model"}` || rec.Header().Get("X-Upstream-Trace") != "abc" {
			t.Fatalf("status %d body %s headers %v", rec.Code, rec.Body.String(), rec.Header())
		}
	})

	t.Run("unreadable request body", func(t *testing.T) {
		p := NewProxy(offlineConfig(t), nil, nil, nil, nil, nil)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", io.NopCloser(errReader{}))
		rec := httptest.NewRecorder()
		p.HandleChatCompletions(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "LILTOK_BAD_PAYLOAD") {
			t.Fatalf("status %d body %s; want 400 LILTOK_BAD_PAYLOAD", rec.Code, rec.Body.String())
		}
	})
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

// brokenRouteRouter returns a router with a "broken" route whose only target fails, and the
// free providers replaced by fallback.
func brokenRouteRouter(t *testing.T, cfg *config.Config, fallback *extraProvider) (*router.Router, *extraProvider) {
	t.Helper()
	r := router.NewRouter(cfg)
	alpha := &extraProvider{name: "alpha", err: errors.New("alpha returned status 500: down")}
	r.SetProvider("alpha", alpha)
	r.SetRoute(router.Route{ID: "broken", Strategy: router.StrategyFallback, Targets: []router.TargetSpec{{ProviderName: "alpha", UpstreamModel: "a1"}}})
	for _, name := range []string{"groq", "gemini", "nvidianim", "openrouter", "kilo", "cline"} {
		if fallback != nil && name == fallback.name {
			r.SetProvider(name, fallback)
			continue
		}
		r.SetProvider(name, &extraProvider{name: name, err: errors.New(name + " returned status 503: unavailable")})
	}
	return r, alpha
}

func TestProxyAnthropicLimitEmergencyFailover(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "json"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			upstream, hits := recordingUpstream(t, http.StatusTooManyRequests, `{"type":"error","error":{"type":"rate_limit_error"}}`, map[string]string{"Retry-After": "60"})
			cfg := offlineConfig(t)
			cfg.Providers.Anthropic = config.ProviderCreds{BaseURL: upstream.URL, APIKey: "cfg-ant-key"}
			groq := &extraProvider{name: "groq", reply: &provider.UnifiedChatResponse{ID: "g1", Model: "qwen/qwen3.8-27b", Content: "rescued by groq", Usage: provider.UnifiedUsage{PromptTokens: 12, CompletionTokens: 4}}}
			r, alpha := brokenRouteRouter(t, cfg, groq)
			p := NewProxy(cfg, nil, nil, r, nil, nil)
			var failovers []*ledger.RequestLog
			p.onFailover = func(item *ledger.RequestLog) { failovers = append(failovers, item) }
			b := admin.NewBroadcaster()
			p.SetBroadcaster(b)
			events := subscribe(t, b)

			body := `{"model":"claude-sonnet-5","max_tokens":64,"messages":[{"role":"user","content":"hello"}]` + map[bool]string{true: `,"stream":true}`, false: `}`}[stream]
			rec := serve(p.HandleAnthropicMessages, http.MethodPost, "/v1/messages", body, map[string]string{"X-Liltok-Route": "broken"})
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			// The router's background model sync may also list models on the same server, so
			// only count message requests.
			messageHits := 0
			for _, h := range hits() {
				if h.path == "/v1/messages" {
					messageHits++
				}
			}
			if messageHits != 1 || alpha.calls.Load() != 1 || groq.calls.Load() != 1 {
				t.Fatalf("calls: upstream %d alpha %d groq %d; want 1 each", messageHits, alpha.calls.Load(), groq.calls.Load())
			}
			if rec.Header().Get("Retry-After") != "" {
				t.Error("Retry-After from the rate-limited upstream must be dropped after a successful failover")
			}
			if !strings.Contains(rec.Body.String(), "rescued by groq") {
				t.Fatalf("body = %s, want the groq reply", rec.Body.String())
			}
			if stream {
				if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
					t.Errorf("Content-Type = %q, want text/event-stream", ct)
				}
			} else if rec.Header().Get("X-Liltok-Provider") != "groq" {
				t.Errorf("X-Liltok-Provider = %q, want groq", rec.Header().Get("X-Liltok-Provider"))
			}
			if len(failovers) != 1 || failovers[0].Provider != "alpha" || failovers[0].CacheStatus != "FAILOVER" || failovers[0].StatusCode != http.StatusBadGateway {
				t.Fatalf("failover reports = %+v, want one for alpha", failovers)
			}

			typ, data := nextEvent(t, events)
			if typ != "failover" || data["provider"] != "alpha" {
				t.Fatalf("first event = %s %v, want the alpha failover", typ, data)
			}
			typ, data = nextEvent(t, events)
			if typ != "request" || data["provider"] != "groq" || data["cache_status"] != "MISS" || data["prompt_tokens"] != float64(12) {
				t.Fatalf("second event = %s %v, want the groq request log", typ, data)
			}
		})
	}
}

func TestProxyAnthropicLimitFailoverExhaustedPassesThrough(t *testing.T) {
	upstream, _ := recordingUpstream(t, 529, `{"type":"error","error":{"type":"overloaded_error"}}`, nil)
	cfg := offlineConfig(t)
	cfg.Providers.Anthropic = config.ProviderCreds{BaseURL: upstream.URL, APIKey: "cfg-ant-key"}
	r, _ := brokenRouteRouter(t, cfg, nil)
	p := NewProxy(cfg, nil, nil, r, nil, nil)
	rec := serve(p.HandleAnthropicMessages, http.MethodPost, "/v1/messages", `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hello"}]}`, map[string]string{"X-Liltok-Route": "broken"})
	if rec.Code != 529 || !strings.Contains(rec.Body.String(), "overloaded_error") {
		t.Fatalf("status %d body %s; want the upstream 529 passed through", rec.Code, rec.Body.String())
	}
}

func TestProxyRouterSuccessOpenAIEndpoint(t *testing.T) {
	database, store := openExtraStore(t)
	semCache := semantic.NewSemanticCache(database, semantic.NewFastLocalEmbedder(256), 0.80)
	cfg := offlineConfig(t)
	r := router.NewRouter(cfg)
	alpha := &extraProvider{name: "alpha", reply: &provider.UnifiedChatResponse{ID: "a-1", Model: "alpha-model", Role: "assistant", Content: "routed answer", FinishReason: "stop"}}
	r.SetProvider("alpha", alpha)
	r.SetRoute(router.Route{ID: "alpha-route", Strategy: router.StrategyFallback, Targets: []router.TargetSpec{{ProviderName: "alpha", UpstreamModel: "alpha-model"}}})
	p := NewProxy(cfg, store, semCache, r, nil, nil)

	body := `{"model":"alpha-route","temperature":0,"messages":[{"role":"user","content":"explain goroutines"}]}`
	rec := serve(p.HandleChatCompletions, http.MethodPost, "/v1/chat/completions", body, nil)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Liltok-Provider") != "alpha" {
		t.Fatalf("status %d provider %q: %s", rec.Code, rec.Header().Get("X-Liltok-Provider"), rec.Body.String())
	}
	var out struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, rec.Body.String())
	}
	if out.ID != "a-1" || out.Object != "chat.completion" || out.Model != "alpha-model" || len(out.Choices) != 1 ||
		out.Choices[0].Message.Content != "routed answer" || out.Choices[0].Message.Role != "assistant" || out.Choices[0].FinishReason != "stop" {
		t.Fatalf("synthesized completion = %+v", out)
	}

	// The routed reply was cached: the same request is an exact hit.
	rec = serve(p.HandleChatCompletions, http.MethodPost, "/v1/chat/completions", body, nil)
	if rec.Header().Get("X-Liltok-Cache-Tier") != "TIER1_EXACT" || alpha.calls.Load() != 1 {
		t.Fatalf("repeat request tier %q with %d upstream calls; want TIER1_EXACT and 1 call", rec.Header().Get("X-Liltok-Cache-Tier"), alpha.calls.Load())
	}

	// And stored semantically: the same question with a different max_tokens misses Tier-1 but
	// hits Tier-3.
	body2 := `{"model":"alpha-route","temperature":0,"max_tokens":99,"messages":[{"role":"user","content":"explain goroutines"}]}`
	rec = serve(p.HandleChatCompletions, http.MethodPost, "/v1/chat/completions", body2, nil)
	if rec.Header().Get("X-Liltok-Cache-Tier") != "TIER3_SEMANTIC" || alpha.calls.Load() != 1 {
		t.Fatalf("similar request tier %q with %d upstream calls; want TIER3_SEMANTIC and 1 call", rec.Header().Get("X-Liltok-Cache-Tier"), alpha.calls.Load())
	}
}

func TestProxyRouterSuccessRawAndStreamed(t *testing.T) {
	cfg := offlineConfig(t)
	r := router.NewRouter(cfg)
	raw := []byte(`{"id":"raw-1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"raw body"}}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
	alpha := &extraProvider{name: "alpha", reply: &provider.UnifiedChatResponse{ID: "raw-1", Content: "raw body", RawResponse: raw}}
	r.SetProvider("alpha", alpha)
	r.SetRoute(router.Route{ID: "alpha-route", Strategy: router.StrategyFallback, Targets: []router.TargetSpec{{ProviderName: "alpha", UpstreamModel: "m"}}})
	p := NewProxy(cfg, nil, nil, r, nil, nil)
	b := admin.NewBroadcaster()
	p.SetBroadcaster(b)
	events := subscribe(t, b)

	rec := serve(p.HandleChatCompletions, http.MethodPost, "/v1/chat/completions", `{"model":"alpha-route","messages":[{"role":"user","content":"q"}]}`, nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), raw) {
		t.Fatalf("status %d body %s; want the provider's raw response", rec.Code, rec.Body.String())
	}
	typ, data := nextEvent(t, events)
	if typ != "request" || data["provider"] != "alpha" || data["prompt_tokens"] != float64(7) || data["completion_tokens"] != float64(2) {
		t.Fatalf("event = %s %v, want the alpha request with usage from the raw body", typ, data)
	}

	rec = serve(p.HandleChatCompletions, http.MethodPost, "/v1/chat/completions", `{"model":"alpha-route","stream":true,"messages":[{"role":"user","content":"q2"}]}`, nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream status %d content type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "data: ") || !strings.Contains(rec.Body.String(), "raw body") {
		t.Fatalf("stream body = %q, want SSE chunks with the reply", rec.Body.String())
	}
}

func TestProxyStreamedRouterMissReportsMissHeaders(t *testing.T) {
	cfg := offlineConfig(t)
	r := router.NewRouter(cfg)
	r.SetProvider("alpha", &extraProvider{name: "alpha", reply: &provider.UnifiedChatResponse{ID: "s", Content: "streamed"}})
	r.SetRoute(router.Route{ID: "alpha-route", Strategy: router.StrategyFallback, Targets: []router.TargetSpec{{ProviderName: "alpha", UpstreamModel: "m"}}})
	p := NewProxy(cfg, nil, nil, r, nil, nil)
	rec := serve(p.HandleChatCompletions, http.MethodPost, "/v1/chat/completions", `{"model":"alpha-route","stream":true,"messages":[{"role":"user","content":"q"}]}`, nil)
	if rec.Header().Get("X-Liltok-Cache-Status") != "MISS" || rec.Header().Get("X-Liltok-Provider") != "alpha" {
		t.Fatalf("headers = %v, want MISS from alpha", rec.Header())
	}
}

func sseUpstream(t *testing.T, chunks []string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "identity")
		w.WriteHeader(http.StatusOK)
		for _, c := range chunks {
			_, _ = w.Write([]byte(c + "\n\n"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestProxyDirectStreamingIsRelayedAndCached(t *testing.T) {
	cases := []struct {
		name      string
		anthropic bool
		chunks    []string
		wantText  string
	}{
		{
			name: "openai",
			chunks: []string{
				`data: {"id":"c1","choices":[{"delta":{"role":"assistant","content":"Hello"}}]}`,
				`data: {"id":"c1","choices":[{"delta":{"content":" world"},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			},
			wantText: "Hello world",
		},
		{
			name:      "anthropic",
			anthropic: true,
			chunks: []string{
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"claude-x\",\"usage\":{\"input_tokens\":11}}}",
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}",
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi there\"}}",
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}",
				"event: message_stop\ndata: {\"type\":\"message_stop\"}",
			},
			wantText: "Hi there",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream, calls := sseUpstream(t, tc.chunks)
			_, store := openExtraStore(t)
			cfg := offlineConfig(t)
			cfg.Providers.OpenAI = config.ProviderCreds{BaseURL: upstream.URL, APIKey: "k"}
			cfg.Providers.Anthropic = config.ProviderCreds{BaseURL: upstream.URL, APIKey: "k"}
			p := NewProxy(cfg, store, nil, nil, nil, nil)
			b := admin.NewBroadcaster()
			p.SetBroadcaster(b)
			events := subscribe(t, b)
			handler, path := p.HandleChatCompletions, "/v1/chat/completions"
			if tc.anthropic {
				handler, path = p.HandleAnthropicMessages, "/v1/messages"
			}
			body := `{"model":"m-stream","stream":true,"temperature":0,"messages":[{"role":"user","content":"say hi"}]}`

			rec := serve(handler, http.MethodPost, path, body, nil)
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
				t.Fatalf("status %d content type %q", rec.Code, rec.Header().Get("Content-Type"))
			}
			if rec.Header().Get("Content-Encoding") != "" {
				t.Error("Content-Encoding must not be forwarded on a streamed reply")
			}
			for _, c := range tc.chunks {
				if !strings.Contains(rec.Body.String(), c) {
					t.Fatalf("relayed stream is missing chunk %q", c)
				}
			}
			typ, data := nextEvent(t, events)
			if typ != "request" || data["cache_status"] != "MISS" || data["status_code"] != float64(200) {
				t.Fatalf("event = %s %v, want the MISS request log", typ, data)
			}
			if tc.anthropic && (data["prompt_tokens"] != float64(11) || data["completion_tokens"] != float64(3)) {
				t.Fatalf("anthropic usage = %v/%v, want 11/3 from the stream", data["prompt_tokens"], data["completion_tokens"])
			}

			rec = serve(handler, http.MethodPost, path, body, nil)
			if rec.Header().Get("X-Liltok-Cache-Tier") != "TIER1_EXACT" || calls.Load() != 1 {
				t.Fatalf("repeat tier %q with %d upstream calls; want a TIER1_EXACT hit", rec.Header().Get("X-Liltok-Cache-Tier"), calls.Load())
			}
			if !strings.Contains(rec.Body.String(), strings.Split(tc.wantText, " ")[0]) {
				t.Fatalf("cached replay = %q, want the collected text", rec.Body.String())
			}
		})
	}
}

func TestProxyEmptyStreamIsNotCached(t *testing.T) {
	upstream, calls := sseUpstream(t, []string{`data: [DONE]`})
	_, store := openExtraStore(t)
	cfg := offlineConfig(t)
	cfg.Providers.OpenAI = config.ProviderCreds{BaseURL: upstream.URL, APIKey: "k"}
	p := NewProxy(cfg, store, nil, nil, nil, nil)
	body := `{"model":"m","stream":true,"temperature":0,"messages":[{"role":"user","content":"nothing"}]}`
	for i := 0; i < 2; i++ {
		rec := serve(p.HandleChatCompletions, http.MethodPost, "/v1/chat/completions", body, nil)
		if rec.Header().Get("X-Liltok-Cache-Status") != "MISS" {
			t.Fatalf("request %d cache status %q, want MISS", i+1, rec.Header().Get("X-Liltok-Cache-Status"))
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2 (an empty stream must not be cached)", calls.Load())
	}
}

func TestProxyNonStreamDirectMissStoresSemantic(t *testing.T) {
	upstream, hits := recordingUpstream(t, http.StatusOK, `{"id":"d1","choices":[{"message":{"role":"assistant","content":"direct answer"}}]}`, nil)
	database, store := openExtraStore(t)
	semCache := semantic.NewSemanticCache(database, semantic.NewFastLocalEmbedder(256), 0.80)
	cfg := offlineConfig(t)
	cfg.Providers.OpenAI = config.ProviderCreds{BaseURL: upstream.URL, APIKey: "k"}
	p := NewProxy(cfg, store, semCache, nil, nil, nil)

	rec := serve(p.HandleChatCompletions, http.MethodPost, "/v1/chat/completions", `{"model":"gpt-4o","temperature":0,"messages":[{"role":"user","content":"what is a mutex"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	rec = serve(p.HandleChatCompletions, http.MethodPost, "/v1/chat/completions", `{"model":"gpt-4o","temperature":0,"max_tokens":50,"messages":[{"role":"user","content":"what is a mutex"}]}`, nil)
	if rec.Header().Get("X-Liltok-Cache-Tier") != "TIER3_SEMANTIC" || len(hits()) != 1 {
		t.Fatalf("tier %q with %d upstream calls; want TIER3_SEMANTIC and 1 call", rec.Header().Get("X-Liltok-Cache-Tier"), len(hits()))
	}
}

func TestProxyCoalescedWaiterLeavesWhenClientCancels(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{}, 1)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		arrived <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"slow","choices":[{"message":{"role":"assistant","content":"slow"}}]}`))
	}))
	defer upstream.Close()
	_, store := openExtraStore(t)
	cfg := offlineConfig(t)
	cfg.Providers.OpenAI = config.ProviderCreds{BaseURL: upstream.URL, APIKey: "k"}
	p := NewProxy(cfg, store, nil, nil, nil, nil)
	body := `{"model":"gpt-4o","temperature":0,"messages":[{"role":"user","content":"slow question"}]}`

	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- serve(p.HandleChatCompletions, http.MethodPost, "/v1/chat/completions", body, nil) }()
	<-arrived

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	waiterDone := make(chan struct{})
	go func() { p.HandleChatCompletions(rec, req); close(waiterDone) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-waiterDone:
	case <-time.After(5 * time.Second):
		t.Fatal("coalesced waiter did not return after its client canceled")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("canceled waiter wrote a body: %q", rec.Body.String())
	}

	close(release)
	first := <-done
	if first.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("first request status %d, upstream calls %d; want 200 and 1", first.Code, calls.Load())
	}
}

func TestExtractPromptDetailsShapes(t *testing.T) {
	cases := []struct {
		name, body, sys, tools, user string
	}{
		{"empty", ``, "", "", ""},
		{"invalid json", `{`, "", "", ""},
		{"system blocks and user parts", `{"system":[{"type":"text","text":"be brief"},{"type":"text","text":"be kind"},"skip"],
			"tools":[{"name":"Bash"}],
			"messages":[{"role":"user","content":[{"type":"text","text":"part one"},{"type":"image"},{"type":"text","text":"part two"}]}]}`,
			"be brief\nbe kind", `[{"name":"Bash"}]`, "part one\npart two"},
		{"developer message and last user wins", `{"messages":["junk",{"role":"developer","content":"dev rules"},{"role":"user","content":"first"},{"role":"user","content":"second"}]}`,
			"dev rules", "", "second"},
		{"top-level system wins over a system message", `{"system":"top","messages":[{"role":"system","content":"inner"}]}`, "top", "", ""},
	}
	for _, tc := range cases {
		sys, tools, user := extractPromptDetails([]byte(tc.body))
		if sys != tc.sys || tools != tc.tools || user != tc.user {
			t.Errorf("%s: got (%q, %q, %q), want (%q, %q, %q)", tc.name, sys, tools, user, tc.sys, tc.tools, tc.user)
		}
	}
}

func TestProxySmallHelpers(t *testing.T) {
	if got := joinURLPath("http://h/v1/", "v1/models"); got != "http://h/v1/v1/models" {
		t.Errorf("joinURLPath without a leading slash = %q", got)
	}
	if got := joinURLPath("http://h/v1", "/v1/models"); got != "http://h/v1/models" {
		t.Errorf("joinURLPath dedupes /v1: %q", got)
	}

	norm := &cache.NormalizedRequest{Model: "gpt-4o", CanonicalJSON: `{"messages":[{"role":"user","content":"count these tokens please"}]}`}
	if got := exactHitPromptTokens(&cache.CacheEntry{PromptTokens: 42}, norm); got != 42 {
		t.Errorf("stored prompt tokens = %d, want 42", got)
	}
	if got := exactHitPromptTokens(&cache.CacheEntry{}, norm); got <= 0 {
		t.Errorf("entries without a stored count must be re-counted, got %d", got)
	}

	if p, c, cached := extractUsage([]byte("not json"), "m"); p != 0 || c != 0 || cached != 0 {
		t.Errorf("extractUsage(invalid) = %d %d %d", p, c, cached)
	}
	p, c, cached := extractUsage([]byte(`{"usage":{"input_tokens":9,"prompt_tokens_details":{"cached_tokens":4}},"content":[{"text":"some anthropic text"}]}`), "claude")
	if p != 9 || c <= 0 || cached != 4 {
		t.Errorf("extractUsage(anthropic) = %d %d %d; want 9, counted completion, 4", p, c, cached)
	}

	var nilProxy Proxy
	nilProxy.recordLog(nil) // must not panic
	nilProxy.recordLog(&ledger.RequestLog{RequestID: "no-sinks"})
}

func TestAnthropicCollectorHasContentBlockTypes(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"tool use only", []string{`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"Bash"}}`}, true},
		{"thinking only", []string{
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
		}, true},
		{"empty text block", []string{`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`}, false},
	}
	for _, tc := range cases {
		c := NewAnthropicStreamCollector()
		for _, l := range tc.lines {
			c.FeedLine(l)
		}
		if got := c.HasContent(); got != tc.want {
			t.Errorf("%s: HasContent = %v, want %v", tc.name, got, tc.want)
		}
	}
}
