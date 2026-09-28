package router

import (
	"errors"
	"regexp"
	"strings"

	"github.com/primaybr/liltok/internal/provider"
)

// stallRegex matches a closing sentence that announces a next action ("Let me read adapter.go.",
// "I'll run the tests now:", "Let's continue with the edit.") rather than reporting a result. The
// announced verb is captured so a closing courtesy ("Let me know if ...") can be told apart; see
// nonActionVerbs.
var stallRegex = regexp.MustCompile(`(?i)(?:^|[.!?\n]\s*)(?:(?:now|next|first|then|so|okay|ok|alright|actually),?\s+)?(?:let me|let's|let us|i will|i'll|i am going to|i'm going to)\s+(?:(?:now|also|first|then|next|just|quickly|finally)\s+)*([a-z]+)\b(?:[^.!?\n]|[.!?]\S)*[.:]?\s*$`)

// nonActionVerbs follow "let me" or "I'll" in a closing line that ends a turn on purpose ("Let me
// know if ...", "I'll wait for your answer.", "I'll stop here.").
var nonActionVerbs = map[string]bool{"know": true, "wait": true, "stop": true, "leave": true, "be": true}

// maxStallTextLen bounds the closing paragraph treated as a stall; a long closing paragraph that
// happens to end with "let me check" is more likely a real answer. Earlier paragraphs are not
// counted, so a long plan that ends by announcing its first step is still a stall.
const maxStallTextLen = 400

// errStalledTurn marks a reply rejected by isStalledAgentTurn, so a live stream that already
// committed its text can recover it with a continuation instead of failing the turn.
var errStalledTurn = errors.New("announced a tool action without calling a tool")

// visibleReplyText returns the text a client would show for resp: the content without <think>
// blocks, and empty when the content only repeats the reasoning. Some adapters copy the reasoning
// into the content when the model sent no content, and the Anthropic translator then emits the
// reply as a thinking block alone, so such a reply has no visible text at all.
func visibleReplyText(resp *provider.UnifiedChatResponse) string {
	_, visible := extractThinkingBlocks(resp.Content)
	visible = strings.TrimSpace(visible)
	if resp.ReasoningContent != "" && visible == strings.TrimSpace(resp.ReasoningContent) {
		return ""
	}
	return visible
}

// lastParagraph returns the text after the last blank line.
func lastParagraph(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if i := strings.LastIndex(text, "\n\n"); i >= 0 {
		return strings.TrimSpace(text[i+2:])
	}
	return text
}

// isStalledAgentTurn reports a turn in a tool-using request that announces its next tool action
// in text but makes no tool call. Clients such as Claude Code treat a turn without tool calls as
// the final answer, so the agent run would end without doing the announced work.
func isStalledAgentTurn(req *provider.UnifiedChatRequest, resp *provider.UnifiedChatResponse) bool {
	if req == nil || resp == nil || len(req.Tools) == 0 || len(resp.ToolCalls) > 0 {
		return false
	}
	tail := lastParagraph(visibleReplyText(resp))
	if tail == "" || len(tail) > maxStallTextLen {
		return false
	}
	m := stallRegex.FindStringSubmatch(tail)
	return m != nil && !nonActionVerbs[strings.ToLower(m[1])]
}
