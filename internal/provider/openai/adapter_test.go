package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/primaybr/liltok/internal/provider"
)

func TestOpenAIAdapterSendChat(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-test",
			"model": "gpt-4o",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "mocked completion"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 12, "completion_tokens": 8, "total_tokens": 20}
		}`))
	}))
	defer mockServer.Close()

	adapter := NewAdapter("openai", provider.TierPremium, mockServer.URL, "test-key")

	req := &provider.UnifiedChatRequest{
		Model: "gpt-4o",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "hello"},
		},
	}

	resp, err := adapter.SendChat(context.Background(), req)
	if err != nil {
		t.Fatalf("send chat failed: %v", err)
	}

	if resp.Content != "mocked completion" {
		t.Errorf("expected content 'mocked completion', got %s", resp.Content)
	}
	if resp.Usage.TotalTokens != 20 {
		t.Errorf("expected 20 total tokens, got %d", resp.Usage.TotalTokens)
	}
}

func TestOpenAIAdapterStreamChat(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"streamed chunk\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer mockServer.Close()

	adapter := NewAdapter("openai", provider.TierPremium, mockServer.URL, "test-key")

	req := &provider.UnifiedChatRequest{
		Model: "gpt-4o",
		Stream: true,
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "stream test"},
		},
	}

	eventChan, errChan, err := adapter.StreamChat(context.Background(), req)
	if err != nil {
		t.Fatalf("stream chat failed: %v", err)
	}

	receivedChunk := false
	for ev := range eventChan {
		if ev.DeltaText == "streamed chunk" {
			receivedChunk = true
		}
	}

	if err := <-errChan; err != nil {
		t.Fatalf("stream error: %v", err)
	}

	if !receivedChunk {
		t.Errorf("failed to receive expected streamed chunk")
	}
}

func TestAnthropicToolConversion(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-tool",
			"model": "qwen/qwen3.8-27b",
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"content": "Running command",
					"tool_calls": [{
						"id": "call_abc123",
						"type": "function",
						"function": {
							"name": "Bash",
							"arguments": "{\"command\":\"ls\"}"
						}
					}]
				},
				"finish_reason": "tool_calls"
			}],
			"usage": {"prompt_tokens": 15, "completion_tokens": 10, "total_tokens": 25}
		}`))
	}))
	defer mockServer.Close()

	adapter := NewAdapter("groq", provider.TierFree, mockServer.URL, "test-key")

	req := &provider.UnifiedChatRequest{
		Model:             "qwen/qwen3.8-27b",
		IsAnthropicSource: true,
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "List directory"},
		},
		Tools: []interface{}{
			map[string]interface{}{
				"name":        "Bash",
				"description": "Execute shell command",
				"input_schema": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"command": map[string]interface{}{"type": "string"},
					},
				},
			},
		},
		ToolChoice: map[string]interface{}{
			"type": "auto",
		},
	}

	resp, err := adapter.SendChat(context.Background(), req)
	if err != nil {
		t.Fatalf("send chat with tools failed: %v", err)
	}

	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Function.Name != "Bash" {
		t.Errorf("expected tool Bash, got %s", resp.ToolCalls[0].Function.Name)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("expected finish reason tool_calls, got %s", resp.FinishReason)
	}
}
