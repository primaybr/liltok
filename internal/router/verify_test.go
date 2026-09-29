package router

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/provider"
)

func verifyCall(id, name, args string) provider.UnifiedToolCall {
	c := provider.UnifiedToolCall{ID: id, Type: "function"}
	c.Function.Name = name
	c.Function.Arguments = args
	return c
}

func verifyShell(id, command string) provider.UnifiedToolCall {
	return verifyCall(id, "Bash", fmt.Sprintf(`{"command":%q}`, command))
}

func verifyAssistant(calls ...provider.UnifiedToolCall) provider.UnifiedChatMessage {
	return provider.UnifiedChatMessage{Role: "assistant", ToolCalls: calls}
}

func verifyResult(id, content string) provider.UnifiedChatMessage {
	return provider.UnifiedChatMessage{Role: "tool", ToolCallID: id, Content: content}
}

func verifyUser(content string) provider.UnifiedChatMessage {
	return provider.UnifiedChatMessage{Role: "user", Content: content}
}

func verifyTools() []interface{} {
	return []interface{}{map[string]interface{}{"name": "Bash"}, map[string]interface{}{"name": "Read"}}
}

func claimResp(text string) *provider.UnifiedChatResponse {
	return &provider.UnifiedChatResponse{Content: text, FinishReason: "stop"}
}

func TestClaimsSuccess(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"I fixed the gate. The build passes and all tests pass.", true},
		{"Everything compiles cleanly now.", true},
		{"go vet is clean and the tests are passing.", true},
		{"All tests pass.", true},
		{"The build succeeded with no errors.", true},
		{"Ran the tests: 0 failures.", true},
		{"The build does not pass yet; I still need to fix the import.", false},
		{"I haven't run the tests.", false},
		{"Once you run the build it should pass.", false},
		{"Make sure the tests pass before merging.", false},
		{"The tests failed with three errors.", false},
		{"The build passes but one test failed.", false},
		{"This function returns clean output when the build flag is set.", false},
		{"There are no errors in the log format spec.", false},
		// Found by the transcript replay: "check" and "passes" far apart in a sentence about something else.
		{"A mistake in this check affects every free-model reply that passes through the gateway.", false},
		{"The test helper passes the request to the router.", false},
		{"All checks pass.", true},
		{"The whole build now passes.", true},
		{"", false},
	}
	for _, tc := range cases {
		if got := claimsSuccess(tc.text); got != tc.want {
			t.Errorf("claimsSuccess(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestShellCommandAndVerification(t *testing.T) {
	extra := []*regexp.Regexp{regexp.MustCompile(`\bjust\s+test\b`)}
	cases := []struct {
		name string
		call provider.UnifiedToolCall
		want bool
	}{
		{"go test", verifyShell("1", "go test ./internal/..."), true},
		{"go build after cd", verifyShell("1", "cd x && go build ./..."), true},
		{"go vet", verifyShell("1", "go vet ./..."), true},
		{"npm test", verifyShell("1", "npm test"), true},
		{"npm run build", verifyShell("1", "npm run build"), true},
		{"pytest", verifyCall("1", "PowerShell", `{"command":"pytest -q"}`), true},
		{"cargo test", verifyShell("1", "cargo test --all"), true},
		{"make target", verifyShell("1", "make build"), true},
		{"make after separator", verifyShell("1", "cd x && make"), true},
		{"custom pattern", verifyShell("1", "just test"), true},
		{"plain listing", verifyShell("1", "ls -la"), false},
		{"make as a word in prose", verifyShell("1", "echo make me a sandwich"), false},
		{"not a shell tool", verifyCall("1", "Read", `{"file_path":"go.mod"}`), false},
		{"non-JSON arguments are used as the command", verifyCall("1", "Bash", "go vet ./..."), true},
		{"unknown argument shape", verifyCall("1", "Bash", `{"weird":1}`), false},
		{"empty arguments", verifyCall("1", "Bash", ""), false},
	}
	for _, tc := range cases {
		cmd, isShell := shellCommand(tc.call)
		got := isShell && isVerificationCommand(cmd, extra)
		if got != tc.want {
			t.Errorf("%s: verification = %v, want %v (command %q, shell %v)", tc.name, got, tc.want, cmd, isShell)
		}
	}
}

func TestToolResultFailed(t *testing.T) {
	cases := []struct {
		content string
		want    bool
	}{
		{"Exit code 1\nFAIL github.com/example/pkg 0.2s", true},
		{"Exit code: 2\nsomething", true},
		{"--- FAIL: TestGate (0.00s)", true},
		{"FAIL\tgithub.com/example/pkg\t0.2s", true},
		{"BUILD FAILED in 3s", true},
		{"2 tests FAILED", true},
		{"ok  \tgithub.com/example/pkg\t0.3s", false},
		{"Exit code 0\nok", false},
		{"Tests: 0 failed, 12 passed", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := toolResultFailed(tc.content); got != tc.want {
			t.Errorf("toolResultFailed(%q) = %v, want %v", tc.content, got, tc.want)
		}
	}
}

const claimText = "I fixed the gate. The build passes and all tests pass."

func assessWith(msgs []provider.UnifiedChatMessage, reply *provider.UnifiedChatResponse) claimVerdict {
	return assessClaim(&provider.UnifiedChatRequest{Tools: verifyTools(), Messages: msgs}, reply, nil)
}

func TestAssessClaimVerdicts(t *testing.T) {
	read := verifyAssistant(verifyCall("r1", "Read", `{"file_path":"gate.go"}`))
	build := verifyAssistant(verifyShell("b1", "go build ./..."))

	cases := []struct {
		name string
		msgs []provider.UnifiedChatMessage
		want claimVerdict
	}{
		{"claim after only a read", []provider.UnifiedChatMessage{verifyUser("fix it"), read, verifyResult("r1", "package share")}, claimUnverified},
		{"claim with no tool history", []provider.UnifiedChatMessage{verifyUser("fix it")}, claimUnverified},
		{"claim after a passing build", []provider.UnifiedChatMessage{verifyUser("fix it"), build, verifyResult("b1", "ok")}, claimNone},
		{"claim after a failing build", []provider.UnifiedChatMessage{verifyUser("fix it"), build, verifyResult("b1", "Exit code 1\nFAIL example")}, claimContradicted},
		{"a later passing run wins over an earlier failing one", []provider.UnifiedChatMessage{
			verifyUser("fix it"),
			build, verifyResult("b1", "Exit code 1\nFAIL example"),
			verifyAssistant(verifyShell("b2", "go test ./...")), verifyResult("b2", "ok"),
		}, claimNone},
		{"a build before the user's latest message does not count", []provider.UnifiedChatMessage{
			verifyUser("fix it"), build, verifyResult("b1", "ok"), verifyUser("now summarise"),
		}, claimUnverified},
	}
	for _, tc := range cases {
		if got := assessWith(tc.msgs, claimResp(claimText)); got != tc.want {
			t.Errorf("%s: verdict = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAssessClaimSkips(t *testing.T) {
	msgs := []provider.UnifiedChatMessage{verifyUser("fix it")}
	if got := assessWith(msgs, claimResp("Here is the summary of the change.")); got != claimNone {
		t.Errorf("a reply that claims nothing must not be flagged, got %v", got)
	}
	withCall := claimResp(claimText)
	withCall.ToolCalls = []provider.UnifiedToolCall{verifyShell("n1", "go test ./...")}
	if got := assessWith(msgs, withCall); got != claimNone {
		t.Errorf("a reply that makes a tool call is not a final answer, got %v", got)
	}
	if got := assessClaim(&provider.UnifiedChatRequest{Messages: msgs}, claimResp(claimText), nil); got != claimNone {
		t.Errorf("a request without tools is not an agent turn, got %v", got)
	}
	long := claimResp("The build passes. " + strings.Repeat("x", maxClaimTextLen))
	if got := assessWith(msgs, long); got != claimNone {
		t.Errorf("a closing paragraph over %d characters is more likely a report, got %v", maxClaimTextLen, got)
	}
	if got := assessClaim(nil, claimResp(claimText), nil); got != claimNone {
		t.Errorf("nil request, got %v", got)
	}
	if got := assessClaim(&provider.UnifiedChatRequest{Tools: verifyTools()}, nil, nil); got != claimNone {
		t.Errorf("nil reply, got %v", got)
	}
}

// Claude Code adds reminder text to the same user turn that carries a tool result, and the parser
// emits it as its own user message; it must not restart the evidence window.
func TestAssessClaimReminderOnlyUserTurnKeepsWindow(t *testing.T) {
	msgs := []provider.UnifiedChatMessage{
		verifyUser("fix it"),
		verifyAssistant(verifyShell("b1", "go build ./...")),
		verifyResult("b1", "ok"),
		verifyUser("<system-reminder>\nToday's date is 2026-09-29.\n</system-reminder>"),
	}
	if got := assessWith(msgs, claimResp(claimText)); got != claimNone {
		t.Errorf("a reminder-only user turn must keep the build in the window, got %v", got)
	}
	msgs = append(msgs, verifyUser("<system-reminder>\nx\n</system-reminder>\nplease also update the docs"))
	if got := assessWith(msgs, claimResp(claimText)); got != claimUnverified {
		t.Errorf("a user turn with real text after the reminder restarts the window, got %v", got)
	}
}

func TestAssessClaimOpenAIShapedHistory(t *testing.T) {
	msgs := []provider.UnifiedChatMessage{
		verifyUser("fix it"),
		{Role: "assistant", ToolCalls: []provider.UnifiedToolCall{verifyCall("call_1", "bash", `{"cmd":"go test ./..."}`)}},
		{Role: "tool", ToolCallID: "call_1", Content: "ok  \texample\t0.1s"},
	}
	if got := assessWith(msgs, claimResp(claimText)); got != claimNone {
		t.Errorf("OpenAI-shaped history with a passing test run, got %v", got)
	}
	msgs[2].Content = "FAIL\texample\t0.1s"
	if got := assessWith(msgs, claimResp(claimText)); got != claimContradicted {
		t.Errorf("OpenAI-shaped history with a failing test run, got %v", got)
	}
}

func TestClaimVerdictString(t *testing.T) {
	if claimNone.String() != "none" || claimUnverified.String() != "unverified" || claimContradicted.String() != "contradicted" {
		t.Errorf("verdict names: %s %s %s", claimNone, claimUnverified, claimContradicted)
	}
}
