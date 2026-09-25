package gemini

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/primaybr/liltok/internal/provider"
)

// A Claude Code turn history must reach Gemini with structured function history: the tool result
// as a functionResponse matched by name to the earlier functionCall, merged with the reminder text
// that follows it into a single user content, and an explicit temperature of 0 kept.
func TestBuildPayloadSendsToolResultsAsFunctionResponses(t *testing.T) {
	body := `{
		"model": "claude-sonnet-5",
		"max_tokens": 1024,
		"temperature": 0,
		"system": "You are Claude Code.",
		"tools": [{"name": "Read", "description": "Read a file", "input_schema": {"type": "object", "properties": {"file_path": {"type": "string"}}}}],
		"messages": [
			{"role": "user", "content": "Open main.go"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "Reading it."},
				{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": {"file_path": "main.go"}}]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "package main"},
				{"type": "text", "text": "<system-reminder>todo list empty</system-reminder>"}]}
		]
	}`
	req, err := provider.ParseUnifiedRequest([]byte(body), true)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := NewAdapter("", "test-key").buildPayload(req)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	wantContents := []interface{}{
		map[string]interface{}{"role": "user", "parts": []interface{}{
			map[string]interface{}{"text": "Open main.go"},
		}},
		map[string]interface{}{"role": "model", "parts": []interface{}{
			map[string]interface{}{"text": "Reading it."},
			map[string]interface{}{
				"functionCall":     map[string]interface{}{"name": "Read", "args": map[string]interface{}{"file_path": "main.go"}},
				"thoughtSignature": geminiExternalThoughtSignature,
			},
		}},
		map[string]interface{}{"role": "user", "parts": []interface{}{
			map[string]interface{}{"functionResponse": map[string]interface{}{"name": "Read", "response": map[string]interface{}{"content": "package main"}}},
			map[string]interface{}{"text": "<system-reminder>todo list empty</system-reminder>"},
		}},
	}
	if !reflect.DeepEqual(got["contents"], wantContents) {
		gotJSON, _ := json.MarshalIndent(got["contents"], "", "  ")
		t.Fatalf("contents mismatch:\n%s", gotJSON)
	}
	gen, _ := got["generationConfig"].(map[string]interface{})
	if temp, ok := gen["temperature"]; !ok || temp != float64(0) {
		t.Errorf("explicit temperature 0 must be sent, generationConfig = %v", gen)
	}
}

func TestBuildPayloadUnmatchedToolResultAndEmptyTurns(t *testing.T) {
	raw, err := NewAdapter("", "test-key").buildPayload(&provider.UnifiedChatRequest{
		Model: "gemini-test",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: ""},
			{Role: "tool", ToolCallID: "unknown", Content: "stray output"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Contents []struct {
			Role  string                   `json:"role"`
			Parts []map[string]interface{} `json:"parts"`
		} `json:"contents"`
		GenerationConfig map[string]interface{} `json:"generationConfig"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	// The empty assistant turn is dropped, so the unmatched tool result (sent as text) merges into
	// the first user content instead of creating an empty model turn.
	if len(got.Contents) != 1 || got.Contents[0].Role != "user" || len(got.Contents[0].Parts) != 2 {
		t.Fatalf("want one user content with 2 parts, got %+v", got.Contents)
	}
	if got.Contents[0].Parts[1]["text"] != "stray output" {
		t.Errorf("unmatched tool result should be text, got %v", got.Contents[0].Parts[1])
	}
	if _, ok := got.GenerationConfig["temperature"]; ok {
		t.Errorf("unset temperature must not be sent, got %v", got.GenerationConfig)
	}
}
