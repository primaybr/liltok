package router

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/primaybr/liltok/internal/provider"
)

// Live streaming (routes.live_streaming): instead of waiting for a whole reply, an attempt reads
// the provider's stream and holds it back until it has clearly passed the checks that would fail
// it over (see liveGate.shouldCommit). From that commit point the text streams to the client as
// it arrives. The usual checks still run on the complete reply; a reply that fails them after
// the commit cannot fail over, because the client already has part of it, so the turn ends with
// an error instead. A reply that never commits (tool calls only, short answers, text that may hide
// a tool call) is returned whole and replayed exactly as without live streaming.

// LiveSink receives a committed reply. Commit is called once, followed by the held-back thinking
// and text; Text then receives each later delta. After the router's checks, exactly one of Finish
// (the validated reply, to send whatever was held back plus tool calls and the stop) or Fail is
// called. Implementations write to the client and must return an error once it has gone.
type LiveSink interface {
	Commit() error
	Thinking(text string) error
	Text(delta string) error
	Finish(resp *provider.UnifiedChatResponse, providerName string) error
	Fail(err error)
}

type liveSinkKey struct{}

// WithLiveSink returns a context under which DispatchChat streams committed replies to sink.
func WithLiveSink(ctx context.Context, sink LiveSink) context.Context {
	return context.WithValue(ctx, liveSinkKey{}, sink)
}

func liveSinkFrom(ctx context.Context) LiveSink {
	sink, _ := ctx.Value(liveSinkKey{}).(LiveSink)
	return sink
}

// CommittedStreamError is returned when an attempt failed after its reply had started streaming
// to the client. The sink has already been told (Fail), and there is no failover.
type CommittedStreamError struct {
	Provider string
	Err      error
}

func (e *CommittedStreamError) Error() string {
	return fmt.Sprintf("live stream from %s failed after output started: %v", e.Provider, e.Err)
}

func (e *CommittedStreamError) Unwrap() error { return e.Err }

// IsCommittedStreamError reports whether err means the reply was partly sent before failing.
func IsCommittedStreamError(err error) bool {
	var c *CommittedStreamError
	return errors.As(err, &c)
}

// liveMarkers are lowercase fragments that can start text the router later turns into tool calls
// or strips (DSML, <tool_call>, fenced JSON/bash blocks, call:Name{...}, legacy "tool call:",
// inline <think> reasoning). Text from the first marker on is held back until the reply ends.
var liveMarkers = []string{"```", "<tool_call", "dsml", "call:", "tool call", "<function", "<think"}

// liveHoldback keeps the last bytes unsent so a marker split across deltas is seen whole.
var liveHoldback = func() int {
	n := 0
	for _, m := range liveMarkers {
		if len(m) > n {
			n = len(m)
		}
	}
	return n - 1
}()

type liveGate struct {
	sink      LiveSink
	prevText  string // previous assistant turn, trimmed: a reply repeating it may be a loop
	thinking  strings.Builder
	text      strings.Builder
	sent      int // bytes of text already passed to the sink
	committed bool
}

func newLiveGate(req *provider.UnifiedChatRequest, sink LiveSink) *liveGate {
	g := &liveGate{sink: sink}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "assistant" {
			g.prevText = strings.TrimSpace(req.Messages[i].Content)
			break
		}
	}
	return g
}

func firstMarker(s string) int {
	lower := strings.ToLower(s)
	first := -1
	for _, m := range liveMarkers {
		if i := strings.Index(lower, m); i >= 0 && (first < 0 || i < first) {
			first = i
		}
	}
	return first
}

// shouldCommit reports whether the text so far can no longer be failed over by the post-stream
// checks: longer than any stall, no tool-call or reasoning markup yet, and not a replay of the
// previous assistant turn.
func (g *liveGate) shouldCommit() bool {
	text := strings.TrimSpace(g.text.String())
	if len(text) <= maxStallTextLen {
		return false
	}
	if firstMarker(text) >= 0 {
		return false
	}
	if g.prevText != "" && strings.HasPrefix(g.prevText, text) {
		return false
	}
	return true
}

func (g *liveGate) addThinking(delta string) {
	if !g.committed {
		g.thinking.WriteString(delta)
	}
}

func (g *liveGate) addText(delta string) error {
	g.text.WriteString(delta)
	if !g.committed {
		if !g.shouldCommit() {
			return nil
		}
		g.committed = true
		if err := g.sink.Commit(); err != nil {
			return err
		}
		if t := g.thinking.String(); t != "" {
			if err := g.sink.Thinking(t); err != nil {
				return err
			}
		}
	}
	return g.flush()
}

// flush sends the text that can no longer turn out to be markup.
func (g *liveGate) flush() error {
	text := g.text.String()
	end := len(text) - liveHoldback
	if m := firstMarker(text); m >= 0 && m < end {
		end = m
	}
	for end > g.sent && end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	if end <= g.sent {
		return nil
	}
	if err := g.sink.Text(text[g.sent:end]); err != nil {
		return err
	}
	g.sent = end
	return nil
}

// streamAttempt runs one target through its stream. It returns the assembled reply (the same
// shape SendChat returns), whether output was committed to the sink, and the attempt error. idle
// bounds the wait for each stream event on non-premium targets, so a stalled stream fails over
// instead of hanging; a reply that keeps producing tokens is never cut off.
func (r *Router) streamAttempt(ctx context.Context, p provider.ProviderClient, target TargetSpec, targetReq, req *provider.UnifiedChatRequest, sink LiveSink, idle time.Duration) (*provider.UnifiedChatResponse, bool, error) {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	gate := newLiveGate(req, sink)
	resp := &provider.UnifiedChatResponse{Model: targetReq.Model, Role: "assistant"}

	var idleC <-chan time.Time
	var idleTimer *time.Timer
	if idle > 0 {
		idleTimer = time.NewTimer(idle)
		defer idleTimer.Stop()
		idleC = idleTimer.C
	}
	timeout := func() error {
		return fmt.Errorf("upstream provider %s model %s did not respond within %s", target.ProviderName, target.UpstreamModel, idle)
	}

	type opened struct {
		events <-chan provider.UnifiedSSEEvent
		errs   <-chan error
		err    error
	}
	openCh := make(chan opened, 1)
	go func() {
		ev, er, err := p.StreamChat(sctx, targetReq)
		openCh <- opened{ev, er, err}
	}()
	var events <-chan provider.UnifiedSSEEvent
	var errs <-chan error
	select {
	case o := <-openCh:
		if o.err != nil {
			return nil, false, o.err
		}
		events, errs = o.events, o.errs
	case <-idleC:
		return nil, false, timeout()
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}

	for events != nil || errs != nil {
		select {
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if idleTimer != nil {
				if !idleTimer.Stop() {
					select {
					case <-idleTimer.C:
					default:
					}
				}
				idleTimer.Reset(idle)
			}
			switch ev.Type {
			case "thinking_delta":
				resp.ReasoningContent += ev.DeltaText
				gate.addThinking(ev.DeltaText)
			case "text_delta":
				if ev.DeltaText != "" {
					if err := gate.addText(ev.DeltaText); err != nil {
						return resp, gate.committed, fmt.Errorf("client stopped receiving the live stream: %w", err)
					}
				}
			case "tool_call":
				resp.ToolCalls = append(resp.ToolCalls, ev.ToolCalls...)
			case "finish":
				if ev.DeltaText != "" {
					if err := gate.addText(ev.DeltaText); err != nil {
						return resp, gate.committed, fmt.Errorf("client stopped receiving the live stream: %w", err)
					}
				}
				resp.FinishReason = ev.FinishReason
				if ev.Usage != nil {
					resp.Usage = *ev.Usage
				}
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err != nil {
				// Providers send their last events and then the error; select does not keep that
				// order, so take the events still queued before reporting the failure.
				if events != nil {
					for ev := range events {
						if ev.Type == "text_delta" && ev.DeltaText != "" {
							if werr := gate.addText(ev.DeltaText); werr != nil {
								break
							}
						}
					}
				}
				resp.Content = gate.text.String()
				return resp, gate.committed, err
			}
		case <-idleC:
			resp.Content = gate.text.String()
			return resp, gate.committed, timeout()
		case <-ctx.Done():
			resp.Content = gate.text.String()
			return resp, gate.committed, ctx.Err()
		}
	}

	resp.Content = gate.text.String()
	if resp.FinishReason == "" {
		resp.FinishReason = "stop"
		if len(resp.ToolCalls) > 0 {
			resp.FinishReason = "tool_calls"
		}
	}
	if resp.Usage.TotalTokens == 0 {
		resp.Usage.TotalTokens = resp.Usage.PromptTokens + resp.Usage.CompletionTokens
	}
	return resp, gate.committed, nil
}
