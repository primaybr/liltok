package openai

import (
	"context"
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/provider"
)

func parseStream(t *testing.T, lines ...string) ([]provider.UnifiedSSEEvent, []error) {
	t.Helper()
	events := make(chan provider.UnifiedSSEEvent, 64)
	errs := make(chan error, 4)
	readOpenAIStream(context.Background(), strings.NewReader(strings.Join(lines, "\n\n")+"\n\n"), events, errs)
	close(events)
	close(errs)
	var evs []provider.UnifiedSSEEvent
	for ev := range events {
		evs = append(evs, ev)
	}
	var es []error
	for e := range errs {
		es = append(es, e)
	}
	return evs, es
}

func TestReadOpenAIStreamAssemblesToolCallsReasoningAndUsage(t *testing.T) {
	evs, errs := parseStream(t,
		`data: {"choices":[{"delta":{"role":"assistant","reasoning_content":"thinking "}}]}`,
		`data: {"choices":[{"delta":{"reasoning":"more"}}]}`,
		`data: {"choices":[{"delta":{"content":"Checking."}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"Read","arguments":"{\"file_"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"Grep","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"path\":\"a.go\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":30,"total_tokens":150}}`,
		`data: [DONE]`,
	)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	var kinds []string
	var thinking, text string
	var calls []provider.UnifiedToolCall
	var finish provider.UnifiedSSEEvent
	for _, ev := range evs {
		kinds = append(kinds, ev.Type)
		switch ev.Type {
		case "thinking_delta":
			thinking += ev.DeltaText
		case "text_delta":
			text += ev.DeltaText
		case "tool_call":
			calls = append(calls, ev.ToolCalls...)
		case "finish":
			finish = ev
		}
	}
	if got := strings.Join(kinds, ","); got != "thinking_delta,thinking_delta,text_delta,tool_call,tool_call,finish,done" {
		t.Fatalf("event order = %s", got)
	}
	if thinking != "thinking more" || text != "Checking." {
		t.Errorf("thinking %q text %q", thinking, text)
	}
	if len(calls) != 2 || calls[0].ID != "call_a" || calls[0].Function.Name != "Read" || calls[0].Function.Arguments != `{"file_path":"a.go"}` ||
		calls[1].Function.Name != "Grep" || calls[1].Function.Arguments != "{}" {
		t.Errorf("tool calls = %+v", calls)
	}
	if finish.FinishReason != "tool_calls" || finish.Usage == nil || finish.Usage.PromptTokens != 120 || finish.Usage.CompletionTokens != 30 {
		t.Errorf("finish = %+v usage %+v; usage sent after finish_reason must still be reported", finish, finish.Usage)
	}
}

func TestReadOpenAIStreamGroqUsageAndMissingDone(t *testing.T) {
	evs, errs := parseStream(t,
		`data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}],"x_groq":{"usage":{"prompt_tokens":9,"completion_tokens":1}}}`,
	)
	if len(errs) != 0 || len(evs) != 3 {
		t.Fatalf("events %+v errors %v", evs, errs)
	}
	if evs[1].Type != "finish" || evs[1].FinishReason != "stop" || evs[1].Usage == nil || evs[1].Usage.PromptTokens != 9 || evs[2].Type != "done" {
		t.Errorf("a stream that ends without [DONE] must still finish with Groq's x_groq usage: %+v", evs)
	}
}

func TestReadOpenAIStreamMidStreamError(t *testing.T) {
	evs, errs := parseStream(t,
		`data: {"choices":[{"delta":{"content":"par"}}]}`,
		`data: {"error":{"message":"Provider returned error","code":502}}`,
	)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "Provider returned error") {
		t.Fatalf("errors = %v, want the mid-stream error", errs)
	}
	for _, ev := range evs {
		if ev.Type == "finish" || ev.Type == "done" {
			t.Fatalf("a failed stream must not report finish/done: %+v", evs)
		}
	}
}
