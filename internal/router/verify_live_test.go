package router

import (
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/provider"
)

// A reply long enough to commit, ending with a success claim.
var claimingStream = longText(600) + "\n\nI fixed the gate. The build passes and all tests pass."

func TestLiveFalseClaimIsContinuedWithAVerificationCall(t *testing.T) {
	r, cp := continuationRouter(t, claimingStream, map[string]*provider.UnifiedChatResponse{
		"m1": {Content: "Running the build.", ToolCalls: []provider.UnifiedToolCall{verifyShell("n1", "go build ./...")}, FinishReason: "tool_calls"},
	}, "m1", "m2")
	sink := &recordingSink{}

	resp, winner, err := liveDispatch(r, sink, unverifiedRequest())
	if err != nil || winner != "streamer" || sink.failed != nil {
		t.Fatalf("a false claim after commit must be continued, not failed: err %v failed %v", err, sink.failed)
	}
	if sink.finished == nil || len(sink.finished.ToolCalls) != 1 || sink.finished.ToolCalls[0].Function.Name != "Bash" {
		t.Fatalf("Finish must carry the continuation's tool call: %+v", sink.finished)
	}
	if resp.Content != claimingStream || resp.FinishReason != "tool_calls" {
		t.Fatalf("the reply keeps the streamed text and ends in a tool call: %q %s", resp.Content, resp.FinishReason)
	}
	if len(cp.got) != 1 || cp.streamed["m2"] != 0 {
		t.Fatalf("the claiming target is asked first and alone: sends %d, streamed %v", len(cp.got), cp.streamed)
	}
	msgs := cp.got[0].Messages
	last := msgs[len(msgs)-1]
	if last.Role != "user" || !strings.Contains(last.Content, "no build or test command has run") || msgs[len(msgs)-2].Role != "assistant" {
		t.Fatalf("continuation request must end with the claim and the verification reminder: %+v", msgs[len(msgs)-2:])
	}
}

func TestLiveFalseClaimWithNoVerificationEndsWithANotice(t *testing.T) {
	// continuingProvider answers "Let me read the file." by default, which is a stalled turn and
	// makes no tool call, so no target can verify the claim.
	r, _ := continuationRouter(t, claimingStream, nil, "m1", "m2")
	sink := &recordingSink{}

	resp, _, err := liveDispatch(r, sink, unverifiedRequest())
	if err != nil || sink.failed != nil || sink.finished == nil {
		t.Fatalf("the turn must end normally, not fail: err %v failed %v finished %v", err, sink.failed, sink.finished)
	}
	if !strings.HasSuffix(sink.finished.Content, "[liltok] unverified: no build or test ran this turn.") || resp.Content != sink.finished.Content {
		t.Fatalf("the streamed reply must end with the notice: %q", sink.finished.Content)
	}
}

// A target that declines the verification reminder has not failed; only a transport error may
// count against its circuit breaker.
func TestLiveDeclinedVerificationDoesNotChargeTheBreaker(t *testing.T) {
	decline := &provider.UnifiedChatResponse{Content: "Nothing more to run.", FinishReason: "stop"}
	r, _ := continuationRouter(t, claimingStream, map[string]*provider.UnifiedChatResponse{"m1": decline, "m2": decline}, "m1", "m2")
	sink := &recordingSink{}

	if _, _, err := liveDispatch(r, sink, unverifiedRequest()); err != nil || sink.failed != nil {
		t.Fatalf("the turn must end normally: err %v failed %v", err, sink.failed)
	}
	for _, name := range []string{"streamer/m1", "streamer/m2"} {
		cb, ok := r.GetBreaker(name)
		if !ok {
			continue
		}
		if _, failures := cb.State(); failures != 0 {
			t.Errorf("breaker %s has %d failures after declining a verification reminder; want 0", name, failures)
		}
	}
}
