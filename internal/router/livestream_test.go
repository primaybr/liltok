package router

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/provider"
)

// streamScript is one scripted streamed reply: its events, an optional error after them, and an
// optional pause before each event (to exercise the idle timeout).
type streamScript struct {
	events []provider.UnifiedSSEEvent
	err    error
	pause  time.Duration
}

type streamingProvider struct {
	mu        sync.Mutex
	scripts   map[string]streamScript
	streamed  map[string]int
	sendCalls int
}

func (p *streamingProvider) Name() string                                  { return "streamer" }
func (p *streamingProvider) Tier() provider.ProviderTier                   { return provider.TierFree }
func (p *streamingProvider) CheckHealth(ctx context.Context) (bool, error) { return true, nil }
func (p *streamingProvider) SendChat(ctx context.Context, req *provider.UnifiedChatRequest) (*provider.UnifiedChatResponse, error) {
	p.mu.Lock()
	p.sendCalls++
	p.mu.Unlock()
	return &provider.UnifiedChatResponse{Model: req.Model, Role: "assistant", Content: "non-streamed reply", FinishReason: "stop"}, nil
}
func (p *streamingProvider) StreamChat(ctx context.Context, req *provider.UnifiedChatRequest) (<-chan provider.UnifiedSSEEvent, <-chan error, error) {
	p.mu.Lock()
	sc := p.scripts[req.Model]
	p.streamed[req.Model]++
	p.mu.Unlock()
	events := make(chan provider.UnifiedSSEEvent)
	errs := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(errs)
		for _, ev := range sc.events {
			if sc.pause > 0 {
				select {
				case <-time.After(sc.pause):
				case <-ctx.Done():
					return
				}
			}
			select {
			case events <- ev:
			case <-ctx.Done():
				return
			}
		}
		if sc.err != nil {
			errs <- sc.err
		}
	}()
	return events, errs, nil
}

// recordingSink records what a client would have been sent.
type recordingSink struct {
	mu        sync.Mutex
	commits   int
	thinking  string
	text      strings.Builder
	finished  *provider.UnifiedChatResponse
	finishedP string
	failed    error
}

func (s *recordingSink) Commit() error { s.mu.Lock(); defer s.mu.Unlock(); s.commits++; return nil }
func (s *recordingSink) Thinking(t string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.thinking += t
	return nil
}
func (s *recordingSink) Text(d string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.text.WriteString(d)
	return nil
}
func (s *recordingSink) Finish(resp *provider.UnifiedChatResponse, providerName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished, s.finishedP = resp, providerName
	return nil
}
func (s *recordingSink) Fail(err error) { s.mu.Lock(); defer s.mu.Unlock(); s.failed = err }

func textEvents(chunks ...string) []provider.UnifiedSSEEvent {
	evs := make([]provider.UnifiedSSEEvent, 0, len(chunks)+1)
	for _, c := range chunks {
		evs = append(evs, provider.UnifiedSSEEvent{Type: "text_delta", DeltaText: c})
	}
	return append(evs, provider.UnifiedSSEEvent{Type: "finish", FinishReason: "stop", Usage: &provider.UnifiedUsage{PromptTokens: 50, CompletionTokens: 120}})
}

// longText returns n bytes of plain prose with no markup.
func longText(n int) string {
	return strings.Repeat("Live streaming sends prose as it arrives. ", n/41+1)[:n]
}

func liveRouter(t *testing.T, scripts map[string]streamScript, models ...string) (*Router, *streamingProvider) {
	t.Helper()
	r := NewRouter(config.DefaultConfig())
	sp := &streamingProvider{scripts: scripts, streamed: map[string]int{}}
	r.SetProvider("streamer", sp)
	targets := make([]TargetSpec, len(models))
	for i, m := range models {
		targets[i] = TargetSpec{ProviderName: "streamer", UpstreamModel: m}
	}
	r.SetRoute(Route{ID: "live", Strategy: StrategyFallback, Targets: targets})
	return r, sp
}

func liveDispatch(r *Router, sink LiveSink, req *provider.UnifiedChatRequest) (*provider.UnifiedChatResponse, string, error) {
	if req == nil {
		req = &provider.UnifiedChatRequest{Model: "live", Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "explain"}}}
	}
	return r.DispatchChat(WithLiveSink(context.Background(), sink), req, "live")
}

func TestLiveStreamCommitsLongProseAndStreamsTheRest(t *testing.T) {
	body := longText(900)
	chunks := []string{body[:300], body[300:600], body[600:]}
	events := append([]provider.UnifiedSSEEvent{{Type: "thinking_delta", DeltaText: "plan the answer"}}, textEvents(chunks...)...)
	r, _ := liveRouter(t, map[string]streamScript{"m1": {events: events}}, "m1")
	sink := &recordingSink{}

	resp, winner, err := liveDispatch(r, sink, nil)
	if err != nil || winner != "streamer" {
		t.Fatalf("dispatch: %v %s", err, winner)
	}
	if sink.commits != 1 || sink.thinking != "plan the answer" {
		t.Fatalf("commits %d thinking %q; the held-back thinking must be sent at commit", sink.commits, sink.thinking)
	}
	sent := sink.text.String()
	if !strings.HasPrefix(body, sent) || len(sent) < len(body)-liveHoldback || len(sent) >= len(body) {
		t.Fatalf("sent %d of %d bytes; all but the holdback tail must stream before Finish", len(sent), len(body))
	}
	if sink.finished == nil || sink.finished.Content != body || resp.Content != body || resp.Usage.CompletionTokens != 120 {
		t.Fatalf("Finish/return must carry the whole validated reply and its usage: %+v", sink.finished)
	}
}

func TestLiveStreamDoesNotCommitShortOrToolReplies(t *testing.T) {
	call := provider.UnifiedToolCall{ID: "c1", Type: "function"}
	call.Function.Name = "Read"
	call.Function.Arguments = `{"file_path":"a.go"}`
	scripts := map[string]streamScript{
		"short": {events: textEvents("A short answer.")},
		"tools": {events: []provider.UnifiedSSEEvent{
			{Type: "text_delta", DeltaText: "Reading it."},
			{Type: "tool_call", ToolCalls: []provider.UnifiedToolCall{call}},
			{Type: "finish", FinishReason: "tool_calls"},
		}},
	}
	for _, model := range []string{"short", "tools"} {
		r, _ := liveRouter(t, scripts, model)
		sink := &recordingSink{}
		req := &provider.UnifiedChatRequest{Model: "live", Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "go"}},
			Tools: []interface{}{map[string]interface{}{"name": "Read"}}}
		resp, _, err := liveDispatch(r, sink, req)
		if err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		if sink.commits != 0 || sink.finished != nil {
			t.Fatalf("%s: a reply that never passed the commit rule must not reach the sink", model)
		}
		if model == "tools" && (len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Name != "Read") {
			t.Fatalf("tool reply must be returned whole for the normal replay: %+v", resp)
		}
	}
}

func TestLiveStreamHoldsBackTextFromAMarker(t *testing.T) {
	prose := longText(500)
	fence := "\n```bash\nls -la\n```\n"
	r, _ := liveRouter(t, map[string]streamScript{"m1": {events: textEvents(prose, fence, "Done.")}}, "m1")
	sink := &recordingSink{}
	if _, _, err := liveDispatch(r, sink, nil); err != nil {
		t.Fatal(err)
	}
	if sent := sink.text.String(); strings.Contains(sent, "```") || !strings.HasPrefix(prose+fence, sent) || len(sent) < len(prose)-liveHoldback {
		t.Fatalf("text from the fence on must be held for the translator, sent %q", sent[max(0, len(sent)-40):])
	}
	if sink.finished == nil || !strings.Contains(sink.finished.Content, "Done.") {
		t.Fatalf("the full reply, including the held-back part, must reach Finish")
	}
}

func TestLiveStreamSkipsCommitForRepeatedTurn(t *testing.T) {
	prev := longText(700)
	r, _ := liveRouter(t, map[string]streamScript{"m1": {events: textEvents(prev[:450], prev[450:])}}, "m1")
	sink := &recordingSink{}
	req := &provider.UnifiedChatRequest{Model: "live", Messages: []provider.UnifiedChatMessage{
		{Role: "user", Content: "q"}, {Role: "assistant", Content: prev}, {Role: "user", Content: "again"},
	}}
	_, _, _ = liveDispatch(r, sink, req)
	if sink.commits != 0 {
		t.Fatal("a reply that is still a prefix of the previous assistant turn must not commit (it may be a loop)")
	}
}

func TestLiveStreamFailoverBeforeCommitAndNoneAfter(t *testing.T) {
	// Before commit: m1 fails early, m2 serves it live.
	body := longText(600)
	scripts := map[string]streamScript{
		"m1": {events: textEvents("Partial")[:1], err: errors.New("streamer returned status 503: overloaded")},
		"m2": {events: textEvents(body[:450], body[450:])},
	}
	r, sp := liveRouter(t, scripts, "m1", "m2")
	sink := &recordingSink{}
	resp, _, err := liveDispatch(r, sink, nil)
	if err != nil || resp.Content != body || sp.streamed["m2"] != 1 || sink.failed != nil {
		t.Fatalf("an uncommitted failure must fail over: err %v streamed %v failed %v", err, sp.streamed, sink.failed)
	}

	// After commit: m1 fails mid-stream; the turn ends with an error and m2 is never tried.
	scripts = map[string]streamScript{
		"m1": {events: textEvents(body[:450])[:1], err: errors.New("connection reset")},
		"m2": {events: textEvents("unused")},
	}
	r, sp = liveRouter(t, scripts, "m1", "m2")
	sink = &recordingSink{}
	_, _, err = liveDispatch(r, sink, nil)
	if !IsCommittedStreamError(err) || sink.failed == nil || sp.streamed["m2"] != 0 {
		t.Fatalf("a failure after commit must not fail over: err %v failed %v streamed %v", err, sink.failed, sp.streamed)
	}
}

func TestLiveStreamOffInPlanModeAndStalledStreamTimesOut(t *testing.T) {
	r, sp := liveRouter(t, map[string]streamScript{"m1": {events: textEvents(longText(600))}}, "m1")
	sink := &recordingSink{}
	req := planModeRequest()
	req.Model = "live"
	resp, _, err := liveDispatch(r, sink, req)
	if err != nil || sp.sendCalls != 1 || sp.streamed["m1"] != 0 || resp.Content != "non-streamed reply" {
		t.Fatalf("plan mode must use the whole-reply path: err %v send %d streamed %v", err, sp.sendCalls, sp.streamed)
	}

	scripts := map[string]streamScript{
		"slow": {events: textEvents("never arrives"), pause: time.Second},
		"fast": {events: textEvents("fast answer")},
	}
	r, _ = liveRouter(t, scripts, "slow", "fast")
	r.attemptTimeout = 50 * time.Millisecond
	resp, _, err = liveDispatch(r, &recordingSink{}, nil)
	if err != nil || resp.Content != "fast answer" {
		t.Fatalf("a stalled stream must time out and fail over: %v %+v", err, resp)
	}
}
