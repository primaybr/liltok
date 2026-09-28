package router

import (
	"context"
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/provider"
)

// continuingProvider streams like streamingProvider and answers SendChat (the continuation
// request) from a per-model script, recording the requests it received.
type continuingProvider struct {
	*streamingProvider
	replies map[string]*provider.UnifiedChatResponse
	got     []*provider.UnifiedChatRequest
}

func (p *continuingProvider) SendChat(ctx context.Context, req *provider.UnifiedChatRequest) (*provider.UnifiedChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sendCalls++
	p.got = append(p.got, req)
	if r, ok := p.replies[req.Model]; ok {
		cp := *r
		return &cp, nil
	}
	return &provider.UnifiedChatResponse{Model: req.Model, Role: "assistant", Content: "Let me read the file.", FinishReason: "stop"}, nil
}

func readCall(path string) provider.UnifiedToolCall {
	c := provider.UnifiedToolCall{ID: "c1", Type: "function"}
	c.Function.Name = "Read"
	c.Function.Arguments = `{"file_path":"` + path + `"}`
	return c
}

func continuationRouter(t *testing.T, stalled string, replies map[string]*provider.UnifiedChatResponse, models ...string) (*Router, *continuingProvider) {
	t.Helper()
	scripts := map[string]streamScript{}
	for _, m := range models {
		scripts[m] = streamScript{events: textEvents(stalled[:450], stalled[450:])}
	}
	r, sp := liveRouter(t, scripts, models...)
	cp := &continuingProvider{streamingProvider: sp, replies: replies}
	r.SetProvider("streamer", cp)
	return r, cp
}

func toolRequest() *provider.UnifiedChatRequest {
	return &provider.UnifiedChatRequest{Model: "live", Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "fix the gate"}},
		Tools: []interface{}{map[string]interface{}{"name": "Read"}}}
}

// A plan long enough to commit, ending by announcing its first step.
var stalledPlan = longText(600) + "\n\nLet's read `internal/share/gate_test.go` around line 100 to find where to add the cases."

func TestLiveStallAfterCommitIsContinuedWithAToolCall(t *testing.T) {
	r, cp := continuationRouter(t, stalledPlan, map[string]*provider.UnifiedChatResponse{
		"m1": {Content: "Reading it.", ToolCalls: []provider.UnifiedToolCall{readCall("internal/share/gate_test.go")}, FinishReason: "tool_calls"},
	}, "m1", "m2")
	sink := &recordingSink{}

	resp, winner, err := liveDispatch(r, sink, toolRequest())
	if err != nil || winner != "streamer" || sink.failed != nil {
		t.Fatalf("a stalled committed turn must be continued, not failed: err %v failed %v", err, sink.failed)
	}
	if sink.commits != 1 || sink.finished == nil || len(sink.finished.ToolCalls) != 1 || sink.finished.ToolCalls[0].Function.Name != "Read" {
		t.Fatalf("Finish must carry the continuation's tool call: %+v", sink.finished)
	}
	if resp.Content != stalledPlan || resp.FinishReason != "tool_calls" {
		t.Fatalf("the reply must keep the streamed text only (no repeated announcement): %q %s", resp.Content, resp.FinishReason)
	}
	if cp.streamed["m2"] != 0 || len(cp.got) != 1 {
		t.Fatalf("the stalling target is asked first and nothing else is needed: streamed %v sends %d", cp.streamed, len(cp.got))
	}
	msgs := cp.got[0].Messages
	if len(msgs) != 3 || msgs[1].Role != "assistant" || msgs[1].Content != stalledPlan || msgs[2].Role != "user" || !strings.Contains(msgs[2].Content, "tool call") || cp.got[0].RawPayload != nil {
		t.Fatalf("continuation request must be conversation + stalled text + reminder, built from Messages: %+v", msgs)
	}
}

func TestLiveStallContinuationMovesToTheNextTarget(t *testing.T) {
	r, cp := continuationRouter(t, stalledPlan, map[string]*provider.UnifiedChatResponse{
		"m2": {ToolCalls: []provider.UnifiedToolCall{readCall("a.go")}, FinishReason: "tool_calls"},
	}, "m1", "m2")
	sink := &recordingSink{}

	resp, _, err := liveDispatch(r, sink, toolRequest())
	if err != nil || len(resp.ToolCalls) != 1 || len(cp.got) != 2 || cp.got[1].Model != "m2" {
		t.Fatalf("when the stalling model stalls again, the next target continues: err %v sends %d", err, len(cp.got))
	}
}

func TestLiveStallWithNoContinuationStillFailsTheTurn(t *testing.T) {
	r, _ := continuationRouter(t, stalledPlan, nil, "m1")
	sink := &recordingSink{}

	_, _, err := liveDispatch(r, sink, toolRequest())
	if !IsCommittedStreamError(err) || sink.failed == nil {
		t.Fatalf("with no usable continuation the committed turn must fail as before: %v", err)
	}
}

func TestReasoningOnlyReplyFailsOver(t *testing.T) {
	// The auto-mode classifier case: max_tokens 64 spent on reasoning, no verdict.
	thought := "Let me analyze this transcript carefully to determine the severity"
	r, cp := continuationRouter(t, longText(500), map[string]*provider.UnifiedChatResponse{
		"m1": {Content: thought, ReasoningContent: thought, FinishReason: "length"},
		"m2": {Content: "<severity>0</severity>", FinishReason: "stop"},
	}, "m1", "m2")

	req := &provider.UnifiedChatRequest{Model: "live", MaxTokens: 64, Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "score it"}}}
	resp, _, err := r.DispatchChat(context.Background(), req, "live")
	if err != nil || resp.Content != "<severity>0</severity>" || len(cp.got) != 2 {
		t.Fatalf("a reply with only reasoning must fail over: err %v resp %+v sends %d", err, resp, len(cp.got))
	}
}
