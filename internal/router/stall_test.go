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

const longAnswer = "The router resolves targets from the route alias or model name, filters them by context window, " +
	"then tries each in order. Every attempt is bounded by the per-attempt timeout, and failures are recorded on the " +
	"target's circuit breaker. Empty completions, undeclared tools, empty plan exits and repetition loops all count as " +
	"failures and move the request to the next target, so a client only sees a reply that passed every check."
