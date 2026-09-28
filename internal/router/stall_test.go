package router

import (
	"testing"

	"github.com/primaybr/liltok/internal/provider"
)

func TestIsStalledAgentTurn(t *testing.T) {
	withTools := &provider.UnifiedChatRequest{Tools: []interface{}{map[string]interface{}{"name": "Read"}}}
	noTools := &provider.UnifiedChatRequest{}

	cases := []struct {
		name  string
		req   *provider.UnifiedChatRequest
		text  string
		calls int
		want  bool
	}{
		{"announces read", withTools, "Let me read `internal/provider/anthropic/adapter.go` and `internal/provider/provider.go`.", 0, true},
		{"announces run after result text", withTools, "The build passed. Now I'll run the tests:", 0, true},
		{"going to inspect", withTools, "I'm going to inspect the router next", 0, true},
		{"think block then announce", withTools, "<think>need file</think>Let me check the config loader.", 0, true},
		{"let me know closing", withTools, "Done. The test passes. Let me know if you want more cases.", 0, false},
		{"final answer", withTools, "The fix is in adapter.go: temperature 0 is now sent.", 0, false},
		{"announce with tool call", withTools, "Let me read the file.", 1, false},
		{"no tools declared", noTools, "Let me read the file.", 0, false},
		{"long answer ending in announce", withTools, longAnswer + " Let me check one more thing.", 0, false},
		{"announcement mid-text only", withTools, "Let me explain. The router retries each target in order.", 0, false},
		// Replies that ended a real agent run early (2026-09-28 free-first session).
		{"long plan ending in an announcement", withTools, longAnswer + "\n\n---\n\nLet's examine `internal/share/gate.go` around `isLoopbackHost`, `hasForeignURL`, and `credentialPattern` to make the targeted edits.", 0, true},
		{"announces continue", withTools, "Let's continue checking the rest of the tests to verify everything passes.", 0, true},
		{"announces perform", withTools, "Let's enhance the pattern.\n\nLet's perform the edit on `internal/share/gate.go`.", 0, true},
		{"modifier words before the verb", withTools, "Now let me also update the credential pattern to catch camelCase names.", 0, true},
		{"waits on purpose", withTools, "The plan is above. I'll wait for your go-ahead before editing.", 0, false},
		{"long closing paragraph", withTools, "Summary.\n\n" + longAnswer + " Let me check one more thing.", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &provider.UnifiedChatResponse{Content: tc.text}
			for i := 0; i < tc.calls; i++ {
				resp.ToolCalls = append(resp.ToolCalls, provider.UnifiedToolCall{})
			}
			if got := isStalledAgentTurn(tc.req, resp); got != tc.want {
				t.Errorf("isStalledAgentTurn(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestVisibleReplyTextDropsCopiedReasoning(t *testing.T) {
	// The OpenAI adapter copies reasoning into the content when a model sent none (a reply cut off
	// at max_tokens mid-thought); the translator then emits only a thinking block.
	resp := &provider.UnifiedChatResponse{Content: "Let me analyze this transcript.", ReasoningContent: "Let me analyze this transcript."}
	if got := visibleReplyText(resp); got != "" {
		t.Fatalf("copied reasoning must not count as visible text, got %q", got)
	}
	resp = &provider.UnifiedChatResponse{Content: "<severity>0</severity>", ReasoningContent: "Scoring it."}
	if got := visibleReplyText(resp); got != "<severity>0</severity>" {
		t.Fatalf("a real answer next to reasoning must stay visible, got %q", got)
	}
}

const longAnswer = "The router resolves targets from the route alias or model name, filters them by context window, " +
	"then tries each in order. Every attempt is bounded by the per-attempt timeout, and failures are recorded on the " +
	"target's circuit breaker. Empty completions, undeclared tools, empty plan exits and repetition loops all count as " +
	"failures and move the request to the next target, so a client only sees a reply that passed every check."
