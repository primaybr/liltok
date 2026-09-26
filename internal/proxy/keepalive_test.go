package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/primaybr/liltok/internal/provider"
	"github.com/primaybr/liltok/internal/router"
)

// slowProvider answers (or fails) after a delay, standing in for a slow free-tier model.
type slowProvider struct {
	delay time.Duration
	reply *provider.UnifiedChatResponse
	err   error
}

func (p *slowProvider) Name() string                                  { return "slow" }
func (p *slowProvider) Tier() provider.ProviderTier                   { return provider.TierFree }
func (p *slowProvider) CheckHealth(ctx context.Context) (bool, error) { return true, nil }
func (p *slowProvider) StreamChat(ctx context.Context, req *provider.UnifiedChatRequest) (<-chan provider.UnifiedSSEEvent, <-chan error, error) {
	return nil, nil, errors.New("not used")
}
func (p *slowProvider) SendChat(ctx context.Context, req *provider.UnifiedChatRequest) (*provider.UnifiedChatResponse, error) {
	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if p.err != nil {
		return nil, p.err
	}
	r := *p.reply
	r.Model = req.Model
	return &r, nil
}

func slowRouteProxy(t *testing.T, routeID string, sp *slowProvider) *Proxy {
	t.Helper()
	cfg := offlineConfig(t)
	r := router.NewRouter(cfg)
	r.SetProvider("slow", sp)
	r.SetRoute(router.Route{ID: routeID, Strategy: router.StrategyFallback, Targets: []router.TargetSpec{{ProviderName: "slow", UpstreamModel: "slow-model"}}})
	p := NewProxy(cfg, nil, nil, r, nil, nil)
	p.keepAliveDelay, p.keepAliveInterval = 20*time.Millisecond, 20*time.Millisecond
	return p
}

const streamedMessage = `{"model":"claude-sonnet-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`

func TestKeepAliveCommitsEarlyForSlowRoutedStream(t *testing.T) {
	sp := &slowProvider{delay: 150 * time.Millisecond, reply: &provider.UnifiedChatResponse{ID: "r1", Role: "assistant", Content: "slow answer", FinishReason: "stop",
		Usage: provider.UnifiedUsage{PromptTokens: 12, CompletionTokens: 3}}}
	p := slowRouteProxy(t, "slow-route", sp)

	rec := serve(p.HandleAnthropicMessages, http.MethodPost, "/v1/messages", streamedMessage, map[string]string{"X-Liltok-Route": "slow-route"})
	res := rec.Result()
	body := rec.Body.String()

	if rec.Code != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d content-type %q", rec.Code, res.Header.Get("Content-Type"))
	}
	if !strings.HasPrefix(body, ": liltok routing\n\n") || !strings.Contains(body, ": keep-alive\n\n") {
		t.Fatalf("expected an early comment and keep-alives before the reply, got:\n%s", body)
	}
	start := strings.Index(body, "event: message_start")
	if start < 0 || strings.Count(body, "event: message_start") != 1 || !strings.Contains(body[start:], "slow answer") || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("expected one complete message after the keep-alives, got:\n%s", body)
	}
	if res.Header.Get("X-Liltok-Cache-Status") != "MISS" {
		t.Errorf("cache status header = %q, want MISS", res.Header.Get("X-Liltok-Cache-Status"))
	}
	if got := res.Trailer.Get("X-Liltok-Provider"); got != "slow" {
		t.Errorf("X-Liltok-Provider trailer = %q, want slow (the header was sent before the winner was known)", got)
	}
}

func TestKeepAliveReportsLateFailureAsSSEError(t *testing.T) {
	// Only the free-first route skips the paid direct-upstream fallback, so its exhaustion is the
	// failure a committed stream must report.
	sp := &slowProvider{delay: 80 * time.Millisecond, err: errors.New("slow returned status 503: overloaded")}
	p := slowRouteProxy(t, "free-first", sp)

	rec := serve(p.HandleAnthropicMessages, http.MethodPost, "/v1/messages", streamedMessage, map[string]string{"X-Liltok-Route": "free-first"})
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d; a committed stream keeps its 200 and reports the failure in-band", rec.Code)
	}
	if !strings.HasPrefix(body, ": liltok routing") || !strings.Contains(body, "event: error\ndata: {\"type\":\"error\",\"error\":{") ||
		!strings.Contains(body, "All free providers in fallback chain failed (status 502)") {
		t.Fatalf("expected an SSE error event carrying the gateway error, got:\n%s", body)
	}
	if strings.Contains(body, `"LILTOK_ROUTER_EXHAUSTED"`) {
		t.Fatalf("the raw JSON error body must not be written into the event stream:\n%s", body)
	}
}

func TestKeepAliveNotUsedForFastOrNonStreamingReplies(t *testing.T) {
	sp := &slowProvider{delay: 0, reply: &provider.UnifiedChatResponse{ID: "r2", Role: "assistant", Content: "fast", FinishReason: "stop"}}
	p := slowRouteProxy(t, "fast-route", sp)
	p.keepAliveDelay = time.Second

	rec := serve(p.HandleAnthropicMessages, http.MethodPost, "/v1/messages", streamedMessage, map[string]string{"X-Liltok-Route": "fast-route"})
	if body := rec.Body.String(); strings.Contains(body, ": liltok routing") || !strings.HasPrefix(body, "event: message_start") {
		t.Fatalf("a reply faster than the delay must stream exactly as before, got:\n%s", body)
	}
	if rec.Header().Get("X-Liltok-Provider") != "slow" {
		t.Errorf("provider header = %q, want slow as a normal header", rec.Header().Get("X-Liltok-Provider"))
	}

	sp.delay = 80 * time.Millisecond
	p.keepAliveDelay = 10 * time.Millisecond
	nonStream := strings.Replace(streamedMessage, `"stream":true`, `"stream":false`, 1)
	rec = serve(p.HandleAnthropicMessages, http.MethodPost, "/v1/messages", nonStream, map[string]string{"X-Liltok-Route": "fast-route"})
	if body := rec.Body.String(); strings.Contains(body, ": liltok") || !strings.HasPrefix(strings.TrimSpace(body), "{") {
		t.Fatalf("non-streaming replies must never get keep-alives, got:\n%s", body)
	}
}
