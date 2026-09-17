package prefix_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/liltok/liltok/internal/cache/prefix"
)

func TestEstimateTokens(t *testing.T) {
	text := "Hello, world! This is a test sentence for token estimation."
	tokens := prefix.EstimateTokens(text)
	if tokens < 10 || tokens > 25 {
		t.Errorf("Unexpected token count estimate: %d for text length %d", tokens, len(text))
	}

	if empty := prefix.EstimateTokens(""); empty != 0 {
		t.Errorf("Expected 0 tokens for empty string, got %d", empty)
	}
}

func TestInjectAnthropicCacheControl_BelowThreshold(t *testing.T) {
	shortPayload := `{
		"model": "claude-3-5-sonnet-20241022",
		"system": "Short system prompt",
		"messages": [{"role": "user", "content": "Hi"}]
	}`

	modified, injected, err := prefix.InjectAnthropicCacheControl([]byte(shortPayload), 1024)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if injected {
		t.Errorf("Expected injected=false for short payload")
	}
	if string(modified) != shortPayload {
		t.Errorf("Expected payload to remain unchanged")
	}
}

func TestInjectAnthropicCacheControl_AboveThreshold(t *testing.T) {
	longInstructions := strings.Repeat("Detailed instructions for code generation and refactoring. ", 100)
	longPayload := `{
		"model": "claude-3-5-sonnet-20241022",
		"system": "` + longInstructions + `",
		"tools": [
			{
				"name": "read_file",
				"description": "Reads a file from disk"
			},
			{
				"name": "write_file",
				"description": "Writes code to disk"
			}
		],
		"messages": [{"role": "user", "content": "Hello"}]
	}`

	opt := prefix.NewPrefixOptimizer(100)
	modified, injected, err := opt.OptimizeAnthropicPayload([]byte(longPayload))
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !injected {
		t.Fatalf("Expected injected=true for long payload")
	}

	var root map[string]interface{}
	if err := json.Unmarshal(modified, &root); err != nil {
		t.Fatalf("Failed to unmarshal modified JSON: %v", err)
	}

	// Verify system prompt is converted to array with ephemeral cache_control
	sysArray, ok := root["system"].([]interface{})
	if !ok || len(sysArray) == 0 {
		t.Fatalf("Expected system to be an array of blocks")
	}
	firstBlock := sysArray[0].(map[string]interface{})
	cc, hasCC := firstBlock["cache_control"].(map[string]interface{})
	if !hasCC || cc["type"] != "ephemeral" {
		t.Errorf("Expected ephemeral cache_control in system block, got: %v", firstBlock)
	}

	// Verify last tool has cache_control
	tools := root["tools"].([]interface{})
	lastTool := tools[len(tools)-1].(map[string]interface{})
	toolCC, hasToolCC := lastTool["cache_control"].(map[string]interface{})
	if !hasToolCC || toolCC["type"] != "ephemeral" {
		t.Errorf("Expected ephemeral cache_control in last tool, got: %v", lastTool)
	}
}

func TestParseAnthropicUsage(t *testing.T) {
	respJSON := `{
		"id": "msg_01",
		"type": "message",
		"role": "assistant",
		"content": [{"type": "text", "text": "Hello"}],
		"model": "claude-3-5-sonnet-20241022",
		"usage": {
			"input_tokens": 150,
			"output_tokens": 25,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens": 2048
		}
	}`

	usage, err := prefix.ParseAnthropicUsage([]byte(respJSON))
	if err != nil {
		t.Fatalf("Failed to parse usage: %v", err)
	}

	if usage.InputTokens != 150 || usage.OutputTokens != 25 {
		t.Errorf("Unexpected standard tokens: %+v", usage)
	}
	if usage.CacheReadInputTokens != 2048 {
		t.Errorf("Expected 2048 cache read tokens, got: %d", usage.CacheReadInputTokens)
	}
	if !usage.HasCacheHit() {
		t.Errorf("Expected HasCacheHit to be true")
	}
}

func TestInjectAnthropicCacheControl_MaxLimit(t *testing.T) {
	// Payload that already has 4 cache_control blocks (e.g. from Claude Code)
	payloadWith4 := `{
		"model": "claude-3-5-sonnet-20241022",
		"system": [
			{"type": "text", "text": "system 1", "cache_control": {"type": "ephemeral"}},
			{"type": "text", "text": "system 2", "cache_control": {"type": "ephemeral"}}
		],
		"tools": [
			{"name": "tool1", "description": "desc", "cache_control": {"type": "ephemeral"}},
			{"name": "tool2", "description": "desc", "cache_control": {"type": "ephemeral"}}
		],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "` + strings.Repeat("test ", 500) + `"}]}
		]
	}`

	modified, injected, err := prefix.InjectAnthropicCacheControl([]byte(payloadWith4), 100)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if injected {
		t.Errorf("Expected injected=false when 4 cache_controls already present")
	}
	if string(modified) != payloadWith4 {
		t.Errorf("Payload should not have been modified")
	}
}

