package provider

import (
	"testing"
)

func TestParseUnifiedRequestOpenAI(t *testing.T) {
	raw := []byte(`{
		"model": "gpt-4o",
		"temperature": 0.2,
		"top_p": 0.9,
		"max_tokens": 1024,
		"stream": true,
		"messages": [
			{"role": "system", "content": "system instruction"},
			{"role": "user", "content": "user query"}
		]
	}`)

	req, err := ParseUnifiedRequest(raw, false)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	if req.Model != "gpt-4o" {
		t.Errorf("expected model gpt-4o, got %s", req.Model)
	}
	if req.SystemPrompt != "system instruction" {
		t.Errorf("expected system instruction, got %s", req.SystemPrompt)
	}
	if len(req.Messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(req.Messages))
	}
	if !req.Stream {
		t.Errorf("expected stream true")
	}
}

func TestParseUnifiedRequestAnthropic(t *testing.T) {
	raw := []byte(`{
		"model": "claude-3-5-sonnet-20241022",
		"system": "You are Claude Code",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hello claude"}]}
		],
		"max_tokens": 2048
	}`)

	req, err := ParseUnifiedRequest(raw, true)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	if req.Model != "claude-3-5-sonnet-20241022" {
		t.Errorf("expected claude model, got %s", req.Model)
	}
	if req.SystemPrompt != "You are Claude Code" {
		t.Errorf("expected system prompt 'You are Claude Code', got %s", req.SystemPrompt)
	}
	if len(req.Messages) != 1 || req.Messages[0].Content != "hello claude" {
		t.Errorf("unexpected message content: %v", req.Messages)
	}
}

func TestParseUnifiedRequestAnthropicToolResultsAndThinking(t *testing.T) {
	raw := []byte(`{
		"model": "claude-3-7-sonnet-20250219",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "thinking", "thinking": "Let me read the file."},
					{"type": "tool_use", "id": "toolu_01ABC", "name": "ReadFile", "input": {"path": "main.go"}}
				]
			},
			{
				"role": "user",
				"content": [
					{"type": "tool_result", "tool_use_id": "toolu_01ABC", "content": "package main"},
					{"type": "text", "text": "Now run it."}
				]
			}
		]
	}`)

	req, err := ParseUnifiedRequest(raw, true)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	if len(req.Messages) != 3 {
		t.Fatalf("expected 3 messages (assistant with tool_calls, tool result, and user text), got %d", len(req.Messages))
	}

	// 1. Assistant message
	if req.Messages[0].Role != "assistant" {
		t.Errorf("expected role assistant, got %s", req.Messages[0].Role)
	}
	if len(req.Messages[0].ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(req.Messages[0].ToolCalls))
	}
	if req.Messages[0].ToolCalls[0].ID != "toolu_01ABC" {
		t.Errorf("expected tool call ID toolu_01ABC, got %s", req.Messages[0].ToolCalls[0].ID)
	}
	if req.Messages[0].Content != "Let me read the file." {
		t.Errorf("expected thinking extracted into content, got %s", req.Messages[0].Content)
	}

	// 2. Tool result message
	if req.Messages[1].Role != "tool" {
		t.Errorf("expected role tool, got %s", req.Messages[1].Role)
	}
	if req.Messages[1].ToolCallID != "toolu_01ABC" {
		t.Errorf("expected tool_call_id toolu_01ABC, got %s", req.Messages[1].ToolCallID)
	}
	if req.Messages[1].Content != "package main" {
		t.Errorf("expected content 'package main', got %s", req.Messages[1].Content)
	}

	// 3. User message
	if req.Messages[2].Role != "user" {
		t.Errorf("expected role user, got %s", req.Messages[2].Role)
	}
	if req.Messages[2].Content != "Now run it." {
		t.Errorf("expected content 'Now run it.', got %s", req.Messages[2].Content)
	}
}
