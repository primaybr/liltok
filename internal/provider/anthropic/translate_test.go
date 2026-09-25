package anthropic

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/primaybr/liltok/internal/provider"
)

// An OpenAI-format tool conversation routed to Anthropic must arrive as a valid Messages payload:
// tool calls as tool_use blocks, tool results as tool_result blocks in one user turn, tool
// definitions and tool_choice converted, and an explicit temperature of 0 kept.
func TestBuildPayloadTranslatesOpenAIToolConversation(t *testing.T) {
	body := `{
		"model": "claude-opus-5-5",
		"temperature": 0,
		"tool_choice": "required",
		"tools": [{"type": "function", "function": {"name": "get_weather", "description": "Weather by city",
			"parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}}],
		"messages": [
			{"role": "system", "content": "You are terse."},
			{"role": "user", "content": [{"type": "text", "text": "Weather in Oslo and Rome?"}]},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"Oslo\"}"}},
				{"id": "call_2", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"Rome\"}"}}]},
			{"role": "tool", "tool_call_id": "call_1", "content": "Oslo: 4C"},
			{"role": "tool", "tool_call_id": "call_2", "content": "Rome: 19C"},
			{"role": "user", "content": "Which is warmer?"}
		]
	}`
	req, err := provider.ParseUnifiedRequest([]byte(body), false)
	if err != nil {
		t.Fatal(err)
	}
	got := decodePayload(t, NewAdapter("", "test-key"), req, false)

	want := map[string]interface{}{
		"model":       "claude-opus-5-5",
		"system":      "You are terse.",
		"temperature": float64(0),
		"max_tokens":  float64(4096),
		"stream":      false,
		"tool_choice": map[string]interface{}{"type": "any"},
		"tools": []interface{}{map[string]interface{}{
			"name":        "get_weather",
			"description": "Weather by city",
			"input_schema": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"city": map[string]interface{}{"type": "string"}},
				"required":   []interface{}{"city"},
			},
		}},
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "Weather in Oslo and Rome?"},
			map[string]interface{}{"role": "assistant", "content": []interface{}{
				map[string]interface{}{"type": "tool_use", "id": "call_1", "name": "get_weather", "input": map[string]interface{}{"city": "Oslo"}},
				map[string]interface{}{"type": "tool_use", "id": "call_2", "name": "get_weather", "input": map[string]interface{}{"city": "Rome"}},
			}},
			map[string]interface{}{"role": "user", "content": []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "call_1", "content": "Oslo: 4C"},
				map[string]interface{}{"type": "tool_result", "tool_use_id": "call_2", "content": "Rome: 19C"},
				map[string]interface{}{"type": "text", "text": "Which is warmer?"},
			}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("payload mismatch:\n%s", gotJSON)
	}
}

func TestBuildPayloadAssistantTextAndToolCall(t *testing.T) {
	var call provider.UnifiedToolCall
	call.ID = "call_9"
	call.Function.Name = "search"
	call.Function.Arguments = "not json"
	got := decodePayload(t, NewAdapter("", "test-key"), &provider.UnifiedChatRequest{
		Model: "claude-test",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "find it"},
			{Role: "assistant", Content: "Searching.", ToolCalls: []provider.UnifiedToolCall{call}},
			{Role: "tool", Content: "orphan result"},
			{Role: "assistant", Content: ""},
		},
	}, false)

	msgs := got["messages"].([]interface{})
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages (empty assistant turn dropped), got %d: %v", len(msgs), msgs)
	}
	assistant := msgs[1].(map[string]interface{})["content"].([]interface{})
	if assistant[0].(map[string]interface{})["text"] != "Searching." {
		t.Errorf("assistant text block missing: %v", assistant)
	}
	toolUse := assistant[1].(map[string]interface{})
	if toolUse["type"] != "tool_use" || !reflect.DeepEqual(toolUse["input"], map[string]interface{}{}) {
		t.Errorf("invalid JSON arguments must become an empty input object, got %v", toolUse)
	}
	// A tool result without a tool_call_id cannot be linked, so it is sent as text.
	if msgs[2].(map[string]interface{})["content"] != "orphan result" {
		t.Errorf("unlinked tool result should be plain user text, got %v", msgs[2])
	}
}

func TestConvertToolChoiceToAnthropic(t *testing.T) {
	cases := []struct {
		in   interface{}
		want interface{}
	}{
		{"auto", map[string]interface{}{"type": "auto"}},
		{"none", map[string]interface{}{"type": "none"}},
		{"required", map[string]interface{}{"type": "any"}},
		{map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "f"}}, map[string]interface{}{"type": "tool", "name": "f"}},
		{map[string]interface{}{"type": "tool", "name": "g"}, map[string]interface{}{"type": "tool", "name": "g"}},
		{nil, nil},
		{"bogus", nil},
	}
	for _, c := range cases {
		if got := convertToolChoiceToAnthropic(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("convertToolChoiceToAnthropic(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestConvertToolsToAnthropicPassthroughAndDefaults(t *testing.T) {
	anth := map[string]interface{}{"name": "Read", "input_schema": map[string]interface{}{"type": "object"}}
	openaiNoParams := map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "ping"}}
	got := convertToolsToAnthropic([]interface{}{anth, openaiNoParams, "junk"})
	want := []interface{}{
		anth,
		map[string]interface{}{"name": "ping", "input_schema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("convertToolsToAnthropic = %v, want %v", got, want)
	}
}
