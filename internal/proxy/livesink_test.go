package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/provider"
	"github.com/primaybr/liltok/internal/router"
)

// liveProvider streams scripted events and ends with an optional error.
type liveProvider struct {
	events []provider.UnifiedSSEEvent
	err    error
}

func (p *liveProvider) Name() string                                  { return "live" }
func (p *liveProvider) Tier() provider.ProviderTier                   { return provider.TierFree }
func (p *liveProvider) CheckHealth(ctx context.Context) (bool, error) { return true, nil }
func (p *liveProvider) SendChat(ctx context.Context, req *provider.UnifiedChatRequest) (*provider.UnifiedChatResponse, error) {
	return nil, errors.New("live streaming must use StreamChat")
}
func (p *liveProvider) StreamChat(ctx context.Context, req *provider.UnifiedChatRequest) (<-chan provider.UnifiedSSEEvent, <-chan error, error) {
	events := make(chan provider.UnifiedSSEEvent, len(p.events))
	errs := make(chan error, 1)
	for _, ev := range p.events {
		events <- ev
	}
	if p.err != nil {
		errs <- p.err
	}
	close(events)
	close(errs)
	return events, errs, nil
}

func liveProxy(t *testing.T, lp *liveProvider) *Proxy {
	t.Helper()
	cfg := offlineConfig(t)
	cfg.Routes.LiveStreaming = true
	r := router.NewRouter(cfg)
	r.SetProvider("live", lp)
	r.SetRoute(router.Route{ID: "live-route", Strategy: router.StrategyFallback, Targets: []router.TargetSpec{{ProviderName: "live", UpstreamModel: "live-model"}}})
	return NewProxy(cfg, nil, nil, r, nil, nil)
}

type sseEvent struct {
	name string
	data map[string]interface{}
}

func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	var name string
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var d map[string]interface{}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &d); err != nil {
				t.Fatalf("bad SSE data %q: %v", line, err)
			}
			out = append(out, sseEvent{name, d})
		}
	}
	return out
}

// streamedText concatenates every text_delta, the way a client assembles the reply.
func streamedText(evs []sseEvent) string {
	var b strings.Builder
	for _, ev := range evs {
		if ev.name != "content_block_delta" {
			continue
		}
		if d, ok := ev.data["delta"].(map[string]interface{}); ok && d["type"] == "text_delta" {
			b.WriteString(d["text"].(string))
		}
	}
	return b.String()
}

func TestLiveSinkStreamsCommittedReply(t *testing.T) {
	body := strings.Repeat("Streaming live keeps the client busy. ", 20)
	lp := &liveProvider{events: []provider.UnifiedSSEEvent{
		{Type: "text_delta", DeltaText: body[:400]},
		{Type: "text_delta", DeltaText: body[400:600]},
		{Type: "text_delta", DeltaText: body[600:]},
		{Type: "finish", FinishReason: "stop", Usage: &provider.UnifiedUsage{PromptTokens: 80, CompletionTokens: 150}},
	}}
	p := liveProxy(t, lp)
	rec := serve(p.HandleAnthropicMessages, http.MethodPost, "/v1/messages", streamedMessage, map[string]string{"X-Liltok-Route": "live-route"})
	evs := parseSSE(t, rec.Body.String())

	var names []string
	for _, ev := range evs {
		names = append(names, ev.name)
	}
	if names[0] != "message_start" || names[len(names)-1] != "message_stop" || strings.Count(strings.Join(names, ","), "message_start") != 1 {
		t.Fatalf("event sequence = %v", names)
	}
	if got := streamedText(evs); got != body {
		t.Fatalf("assembled text differs from the reply:\n got %q\nwant %q", got, body)
	}
	if !strings.HasPrefix(evs[0].data["message"].(map[string]interface{})["id"].(string), "msg_live_") {
		t.Errorf("message_start = %v, want a live message id", evs[0].data)
	}
	md := evs[len(evs)-2]
	if md.name != "message_delta" || md.data["usage"].(map[string]interface{})["output_tokens"].(float64) != 150 {
		t.Errorf("message_delta = %v, want the reply's usage", md.data)
	}
	if rec.Result().Trailer.Get("X-Liltok-Provider") != "live" {
		t.Errorf("provider trailer = %q, want live", rec.Result().Trailer.Get("X-Liltok-Provider"))
	}
}

func TestLiveSinkLeavesToolRepliesToTheReplay(t *testing.T) {
	call := provider.UnifiedToolCall{ID: "call_1", Type: "function"}
	call.Function.Name = "Read"
	call.Function.Arguments = `{"file_path":"main.go"}`
	lp := &liveProvider{events: []provider.UnifiedSSEEvent{
		{Type: "text_delta", DeltaText: "Reading main.go."},
		{Type: "tool_call", ToolCalls: []provider.UnifiedToolCall{call}},
		{Type: "finish", FinishReason: "tool_calls"},
	}}
	p := liveProxy(t, lp)
	req := `{"model":"claude-sonnet-5","max_tokens":64,"stream":true,"tools":[{"name":"Read","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"read it"}]}`
	rec := serve(p.HandleAnthropicMessages, http.MethodPost, "/v1/messages", req, map[string]string{"X-Liltok-Route": "live-route"})
	body := rec.Body.String()
	if strings.Contains(body, "msg_live_") || !strings.Contains(body, `"name":"Read"`) || !strings.Contains(body, `"stop_reason":"tool_use"`) {
		t.Fatalf("a reply that never commits must be replayed whole with its tool call, got:\n%s", body)
	}
}

func TestLiveSinkFailureAfterCommitEndsWithError(t *testing.T) {
	lp := &liveProvider{
		events: []provider.UnifiedSSEEvent{{Type: "text_delta", DeltaText: strings.Repeat("Partial answer text. ", 30)}},
		err:    errors.New("connection reset by upstream"),
	}
	p := liveProxy(t, lp)
	rec := serve(p.HandleAnthropicMessages, http.MethodPost, "/v1/messages", streamedMessage, map[string]string{"X-Liltok-Route": "live-route"})
	evs := parseSSE(t, rec.Body.String())
	starts, last := 0, evs[len(evs)-1]
	for _, ev := range evs {
		if ev.name == "message_start" {
			starts++
		}
	}
	if starts != 1 || last.name != "error" || !strings.Contains(last.data["error"].(map[string]interface{})["message"].(string), "connection reset") {
		t.Fatalf("expected one message then an error event, got %d starts, last %v", starts, last)
	}
}
