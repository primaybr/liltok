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
