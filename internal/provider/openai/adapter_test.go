package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/liltok/liltok/internal/provider"
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
