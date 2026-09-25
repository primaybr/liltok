package router

import (
	"regexp"
	"strings"

	"github.com/primaybr/liltok/internal/provider"
)

// stallRegex matches a closing sentence that announces a tool action ("Let me read adapter.go.",
// "I'll run the tests now:") rather than reporting a result.
var stallRegex = regexp.MustCompile(`(?i)(?:^|[.!?\n]\s*)(?:now,?\s+|next,?\s+|first,?\s+)?(?:let me|let's|let us|i will|i'll|i am going to|i'm going to)\s+(?:now\s+)?(?:read|open|check|look at|look into|inspect|examine|search|grep|find|list|view|explore|run|execute|edit|update|modify|write|create|fix)\b(?:[^.!?\n]|[.!?]\S)*[.:]?\s*$`)

// maxStallTextLen bounds the replies treated as stalls; a long answer that happens to end with
// "let me check" is more likely a real answer.
const maxStallTextLen = 400

// isStalledAgentTurn reports a turn in a tool-using request that announces its next tool action
// in text but makes no tool call. Clients such as Claude Code treat a turn without tool calls as
// the final answer, so the agent run would end without doing the announced work.
func isStalledAgentTurn(req *provider.UnifiedChatRequest, resp *provider.UnifiedChatResponse) bool {
	if req == nil || resp == nil || len(req.Tools) == 0 || len(resp.ToolCalls) > 0 {
		return false
	}
	_, visible := extractThinkingBlocks(resp.Content)
	visible = strings.TrimSpace(visible)
	if visible == "" || len(visible) > maxStallTextLen {
		return false
	}
	return stallRegex.MatchString(visible)
}
