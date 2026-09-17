package proxy

import (
	"encoding/json"
	"testing"
)

func TestAnthropicStreamCollector_TextStream(t *testing.T) {
	c := NewAnthropicStreamCollector()

	// Simulate Anthropic SSE events
	c.FeedLine(`{"type":"message_start","message":{"id":"msg_test123","type":"message","role":"assistant","content":[],"model":"claude-3-5-sonnet-20241022","usage":{"input_tokens":10,"output_tokens":0}}}`)
	c.FeedLine(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	c.FeedLine(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}`)
	c.FeedLine(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world!"}}`)
	c.FeedLine(`{"type":"content_block_stop","index":0}`)
	c.FeedLine(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`)
	c.FeedLine(`{"type":"message_stop"}`)

	if !c.HasContent() {
		t.Fatalf("expected HasContent to be true")
	}

	payload, inTokens, outTokens := c.BuildMessage("claude-3-5-sonnet-20241022")
	if inTokens != 10 || outTokens != 5 {
		t.Errorf("expected 10 in, 5 out tokens; got %d in, %d out", inTokens, outTokens)
	}

	var msg struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Role       string `json:"role"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}

	if err := json.Unmarshal(payload, &msg); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if msg.ID != "msg_test123" {
		t.Errorf("expected ID msg_test123, got %s", msg.ID)
	}
	if msg.Type != "message" || msg.Role != "assistant" {
		t.Errorf("unexpected type or role: %s, %s", msg.Type, msg.Role)
	}
	if msg.StopReason != "end_turn" {
		t.Errorf("expected stop_reason end_turn, got %s", msg.StopReason)
	}
	if len(msg.Content) != 1 || msg.Content[0].Text != "Hello world!" {
		t.Errorf("unexpected content: %+v", msg.Content)
	}
}

func TestAnthropicStreamCollector_ToolUseStream(t *testing.T) {
	c := NewAnthropicStreamCollector()

	c.FeedLine(`{"type":"message_start","message":{"id":"msg_tool456","type":"message","role":"assistant","content":[],"model":"claude-opus-5","usage":{"input_tokens":50,"output_tokens":0}}}`)
	c.FeedLine(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_abc","name":"read_file"}}`)
	c.FeedLine(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"file\":\"main.go\""}}`)
	c.FeedLine(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"}"}}`)
	c.FeedLine(`{"type":"content_block_stop","index":0}`)
	c.FeedLine(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":25}}`)
	c.FeedLine(`{"type":"message_stop"}`)

	if !c.HasContent() {
		t.Fatalf("expected HasContent to be true")
	}

	payload, _, _ := c.BuildMessage("claude-opus-5")

	var msg struct {
		ID         string `json:"id"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string                 `json:"type"`
			ID    string                 `json:"id"`
			Name  string                 `json:"name"`
			Input map[string]interface{} `json:"input"`
		} `json:"content"`
	}

	if err := json.Unmarshal(payload, &msg); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if msg.StopReason != "tool_use" {
		t.Errorf("expected stop_reason tool_use, got %s", msg.StopReason)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(msg.Content))
	}
	block := msg.Content[0]
	if block.Type != "tool_use" || block.Name != "read_file" || block.ID != "toolu_abc" {
		t.Errorf("unexpected block metadata: %+v", block)
	}
	if block.Input["file"] != "main.go" {
		t.Errorf("expected input.file == main.go, got %v", block.Input["file"])
	}
}

func TestAnthropicStreamCollector_EmptyStream(t *testing.T) {
	c := NewAnthropicStreamCollector()

	// Empty stream (no content blocks, e.g. aborted connection)
	c.FeedLine(`{"type":"message_start","message":{"id":"msg_empty","type":"message","role":"assistant","content":[]}}`)
	c.FeedLine(`{"type":"message_stop"}`)

	if c.HasContent() {
		t.Errorf("expected HasContent to be false for empty stream")
	}
}
