package provider

import (
	"encoding/json"
	"testing"
)

func TestParseUnifiedRequest_InvalidJSON(t *testing.T) {
	if _, err := ParseUnifiedRequest([]byte(`{not json`), false); err == nil {
		t.Errorf("expected an error for invalid JSON")
	}
	if _, err := ParseUnifiedRequest([]byte(`[1,2]`), true); err == nil {
		t.Errorf("expected an error for a non-object body")
	}
}

func TestParseUnifiedRequest_CommonFields(t *testing.T) {
	raw := []byte(`{
		"model": "m",
		"temperature": 0,
		"top_p": 0,
		"tool_choice": {"type": "auto"},
		"tools": [{"name": "a"}, {"name": "b"}],
		"messages": []
	}`)
	req, err := ParseUnifiedRequest(raw, false)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if !req.HasTemperature || req.Temperature != 0 || !req.HasTopP || req.TopP != 0 {
		t.Errorf("explicit zero sampling values must be recorded as set: %+v", req)
	}
	if tc, ok := req.ToolChoice.(map[string]interface{}); !ok || tc["type"] != "auto" {
		t.Errorf("tool_choice = %#v", req.ToolChoice)
	}
	if len(req.Tools) != 2 {
		t.Errorf("tools = %v", req.Tools)
	}
	if req.IsAnthropicSource || string(req.RawPayload) != string(raw) {
		t.Errorf("raw payload and source flag not kept")
	}

	req, err = ParseUnifiedRequest([]byte(`{"model":"m","tool_choice":null}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if req.ToolChoice != nil || req.HasTemperature || req.HasTopP || req.Stream {
		t.Errorf("absent or null fields must stay unset: %+v", req)
	}
}

func TestParseUnifiedRequest_AnthropicSystemArrayAndResultBlocks(t *testing.T) {
	raw := []byte(`{
		"system": [{"type": "text", "text": "part one. "}, {"type": "text", "text": "part two."}, "ignored"],
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t1", "content": [{"type": "text", "text": "line1 "}, {"type": "image"}, {"type": "text", "text": "line2"}]},
				{"type": "tool_result", "tool_use_id": "t2"}
			]},
			{"role": "assistant", "content": [
				{"type": "text", "text": "answer"},
				{"type": "thinking", "thinking": "ignored because text came first"},
				{"type": "tool_use", "id": "t3", "name": "Run"}
			]},
			{"role": "user"},
			"not an object"
		]
	}`)
	req, err := ParseUnifiedRequest(raw, true)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if req.SystemPrompt != "part one. part two." {
		t.Errorf("system prompt = %q", req.SystemPrompt)
	}
	if len(req.Messages) != 4 {
		t.Fatalf("expected 4 messages (2 tool results, assistant, empty user), got %d: %+v", len(req.Messages), req.Messages)
	}
	if m := req.Messages[0]; m.Role != "tool" || m.ToolCallID != "t1" || m.Content != "line1 line2" {
		t.Errorf("first tool result = %+v", m)
	}
	if m := req.Messages[1]; m.Role != "tool" || m.ToolCallID != "t2" || m.Content != "" {
		t.Errorf("second tool result = %+v", m)
	}
	a := req.Messages[2]
	if a.Role != "assistant" || a.Content != "answer" || len(a.ToolCalls) != 1 {
		t.Fatalf("assistant message = %+v", a)
	}
	if tc := a.ToolCalls[0]; tc.ID != "t3" || tc.Type != "function" || tc.Function.Name != "Run" || tc.Function.Arguments != "null" {
		t.Errorf("tool call = %+v", tc)
	}
	if m := req.Messages[3]; m.Role != "user" || m.Content != "" {
		t.Errorf("user message without content must still be kept, got %+v", m)
	}
}

func TestParseUnifiedRequest_OpenAIContentPartsAndToolCalls(t *testing.T) {
	raw := []byte(`{
		"messages": [
			{"role": "system", "content": [{"type": "text", "text": "sys "}, {"type": "text", "text": "prompt"}]},
			{"role": "system", "content": "second system is not the prompt"},
			{"role": "user", "name": "alice", "content": [{"type": "text", "text": "look"}, {"type": "image_url", "image_url": {"url": "x"}}, "junk"]},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "c1", "type": "function", "function": {"name": "a", "arguments": "{\"x\":1}"}},
				{"id": "c2", "function": {"name": "b", "arguments": {"y": 2}}},
				{"id": "c3", "function": {"name": "c"}},
				"skip me"
			]},
			{"role": "tool", "tool_call_id": "c1", "content": 42},
			7
		]
	}`)
	req, err := ParseUnifiedRequest(raw, false)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if req.SystemPrompt != "sys prompt" {
		t.Errorf("system prompt = %q, want first system message text", req.SystemPrompt)
	}
	if len(req.Messages) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(req.Messages))
	}
	if m := req.Messages[2]; m.Content != "look" || m.Name != "alice" {
		t.Errorf("user message = %+v", m)
	}

	calls := req.Messages[3].ToolCalls
	if len(calls) != 3 {
		t.Fatalf("expected 3 tool calls, got %+v", calls)
	}
	want := []struct{ id, name, args string }{
		{"c1", "a", `{"x":1}`},
		{"c2", "b", `{"y":2}`},
		{"c3", "c", "{}"},
	}
	for i, w := range want {
		c := calls[i]
		if c.ID != w.id || c.Type != "function" || c.Function.Name != w.name || c.Function.Arguments != w.args {
			t.Errorf("call %d = %+v, want %+v", i, c, w)
		}
	}
	if m := req.Messages[4]; m.Role != "tool" || m.ToolCallID != "c1" || m.Content != "" {
		t.Errorf("tool message with non-string content = %+v", m)
	}
}

func TestParseOpenAIToolCalls_MissingFunction(t *testing.T) {
	var calls []interface{}
	if err := json.Unmarshal([]byte(`[{"id":"only-id"}]`), &calls); err != nil {
		t.Fatal(err)
	}
	got := parseOpenAIToolCalls(calls)
	if len(got) != 1 || got[0].ID != "only-id" || got[0].Function.Name != "" || got[0].Function.Arguments != "{}" {
		t.Errorf("got %+v", got)
	}
}
