package router

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/primaybr/liltok/internal/provider"
)

// claimVerdict says whether a final reply's success claim is supported by the conversation.
type claimVerdict int

const (
	claimNone         claimVerdict = iota
	claimUnverified                // no build or test command ran since the user's last message
	claimContradicted              // the most recent build or test command failed
)

func (v claimVerdict) String() string {
	switch v {
	case claimUnverified:
		return "unverified"
	case claimContradicted:
		return "contradicted"
	}
	return "none"
}

// maxClaimTextLen bounds the closing paragraph checked for a success claim; a longer paragraph is
// more likely a real report than a bare "the build passes".
const maxClaimTextLen = 600

var (
	claimSentenceSplit = regexp.MustCompile(`[.!?]+\s+|\n+`)

	// claimOutcome is what a build or test run is said to have done.
	claimOutcome = `(?:pass\w*|succe\w+|green|clean|(?:no|zero|0)\s+(?:errors?|failures?))`

	// claimPatterns match a sentence that says a build, compile, test, vet or lint run succeeded.
	claimPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:build\w*|compil\w*|tests?|test suite|vet|lint\w*|checks?)\b[^.!?\n]{0,40}\b` + claimOutcome + `\b`),
		regexp.MustCompile(`\b(?:builds?|compiles?)\s+(?:cleanly|successfully|without\s+errors?)`),
		regexp.MustCompile(`\ball\s+(?:the\s+)?tests?\s+(?:now\s+)?pass`),
	}

	// claimHedge marks a sentence that negates, hedges or predicts instead of reporting.
	claimHedge = regexp.MustCompile(`\b(?:not|never|yet|unable|cannot|should|would|will|once|until|whether|ensure|fail(?:s|ed|ing)?)\b|n't|make sure|need to|needs to|to verify|to check|\bif\b`)
)

// claimsSuccess reports whether text states, in a sentence that is not hedged or negated, that a
// build, compile, test, vet or lint run passed.
func claimsSuccess(text string) bool {
	for _, s := range claimSentenceSplit.Split(strings.ToLower(text), -1) {
		if s = strings.TrimSpace(s); s == "" || claimHedge.MatchString(s) {
			continue
		}
		for _, p := range claimPatterns {
			if p.MatchString(s) {
				return true
			}
		}
	}
	return false
}

// shellToolNames are the tool names, lower-cased, that run a shell command.
var shellToolNames = map[string]bool{
	"bash": true, "powershell": true, "shell": true, "sh": true,
	"run_command": true, "execute_command": true, "run_terminal_cmd": true, "terminal": true,
}

// builtinVerifyCommands match a shell command that builds, tests, vets or lints a project.
var builtinVerifyCommands = []*regexp.Regexp{
	regexp.MustCompile(`\bgo\s+(?:build|test|vet)\b`),
	regexp.MustCompile(`\b(?:npm|yarn|pnpm)\s+(?:run\s+)?(?:build|test|lint|typecheck)\b`),
	regexp.MustCompile(`\b(?:pytest|py\.test)\b`),
	regexp.MustCompile(`\bpython3?\s+-m\s+(?:pytest|unittest|mypy)\b`),
	regexp.MustCompile(`\bcargo\s+(?:build|test|check|clippy)\b`),
	regexp.MustCompile(`(?:^|[;&|]\s*)make(?:\s|$)`),
	regexp.MustCompile(`\btsc\b`),
	regexp.MustCompile(`\bdotnet\s+(?:build|test)\b`),
	regexp.MustCompile(`\b(?:mvn|gradle|gradlew)\b`),
	regexp.MustCompile(`\bgolangci-lint\b`),
}

// shellCommand returns the command a shell tool call runs. isShell is false for any other tool. A
// shell call whose arguments are not a JSON object with a command field returns the raw arguments,
// or an empty command when the object has no recognised field.
func shellCommand(call provider.UnifiedToolCall) (command string, isShell bool) {
	if !shellToolNames[strings.ToLower(call.Function.Name)] {
		return "", false
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return call.Function.Arguments, true
	}
	for _, k := range []string{"command", "cmd", "script"} {
		if s, ok := args[k].(string); ok {
			return s, true
		}
	}
	return "", true
}

// isVerificationCommand reports whether command builds, tests or lints a project, by the built-in
// list or by one of the extra patterns from routes.verify_commands.
func isVerificationCommand(command string, extra []*regexp.Regexp) bool {
	for _, re := range builtinVerifyCommands {
		if re.MatchString(command) {
			return true
		}
	}
	for _, re := range extra {
		if re.MatchString(command) {
			return true
		}
	}
	return false
}

// toolFailureMarkers find a failed build or test run in a tool result. The client's is_error flag
// is not kept when a request is parsed, so failure is read from the text: a non-zero exit code, a
// Go test failure line, or a build tool's failure banner.
var toolFailureMarkers = regexp.MustCompile(`(?m)^Exit code:? *[1-9]\d*|^--- FAIL|^FAIL\s|(?i:build failed)|\bFAILED\b`)

func toolResultFailed(content string) bool {
	return toolFailureMarkers.MatchString(content)
}

// evidenceWindow returns the messages after the last real user message. A user message holding
// only client-injected reminder blocks is not real: Claude Code puts them next to a tool result,
// and the parser emits them as their own user message.
func evidenceWindow(msgs []provider.UnifiedChatMessage) []provider.UnifiedChatMessage {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == "user" && strings.TrimSpace(m.Content) != "" && !isAutomatedUserMessage(m.Content) {
			return msgs[i+1:]
		}
	}
	return msgs
}

// lastVerification reports whether the window holds a build or test command and, for the most
// recent one, whether its result looks failed. A command with no result message counts as passed.
func lastVerification(window []provider.UnifiedChatMessage, extra []*regexp.Regexp) (found, failed bool) {
	lastID := ""
	for _, m := range window {
		if m.Role != "assistant" {
			continue
		}
		for _, c := range m.ToolCalls {
			if cmd, ok := shellCommand(c); ok && isVerificationCommand(cmd, extra) {
				found, lastID = true, c.ID
			}
		}
	}
	if !found {
		return false, false
	}
	for _, m := range window {
		if m.Role == "tool" && m.ToolCallID == lastID {
			failed = toolResultFailed(m.Content)
		}
	}
	return found, failed
}

// assessClaim judges a final reply from a tool-using request: claimUnverified when it says a
// build or test run passed and none ran since the user's last message, claimContradicted when the
// most recent run failed, claimNone otherwise (including any reply that is not a final answer).
func assessClaim(req *provider.UnifiedChatRequest, resp *provider.UnifiedChatResponse, extra []*regexp.Regexp) claimVerdict {
	if req == nil || resp == nil || len(req.Tools) == 0 || len(resp.ToolCalls) > 0 {
		return claimNone
	}
	tail := lastParagraph(visibleReplyText(resp))
	if tail == "" || len(tail) > maxClaimTextLen || !claimsSuccess(tail) {
		return claimNone
	}
	found, failed := lastVerification(evidenceWindow(req.Messages), extra)
	switch {
	case !found:
		return claimUnverified
	case failed:
		return claimContradicted
	}
	return claimNone
}
