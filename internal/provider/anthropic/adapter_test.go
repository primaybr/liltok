package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/primaybr/liltok/internal/provider"
)

func TestAnthropicAdapterSendChat(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "ant-key" {
			t.Errorf("unexpected anthropic key: %s", r.Header.Get("x-api-key"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "msg-test",
			"model": "claude-3-5-sonnet-20241022",
			"role": "assistant",
			"content": [{"type": "text", "text": "claude response"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 50, "output_tokens": 25, "cache_read_input_tokens": 40}
		}`))
	}))
	defer mockServer.Close()

	adapter := NewAdapter(mockServer.URL, "ant-key")

	req := &provider.UnifiedChatRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "claude test"},
		},
	}

	resp, err := adapter.SendChat(context.Background(), req)
	if err != nil {
		t.Fatalf("send chat failed: %v", err)
	}

	if resp.Content != "claude response" {
		t.Errorf("expected claude response, got %s", resp.Content)
	}
	if resp.Usage.CachedTokens != 40 {
		t.Errorf("expected 40 cached tokens, got %d", resp.Usage.CachedTokens)
	}
}

func TestAnthropicAdapterStreamChat(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"streamed claude\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer mockServer.Close()

	adapter := NewAdapter(mockServer.URL, "ant-key")

	req := &provider.UnifiedChatRequest{
		Model:  "claude-3-5-sonnet-20241022",
		Stream: true,
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "stream claude"},
		},
	}

	eventChan, errChan, err := adapter.StreamChat(context.Background(), req)
	if err != nil {
		t.Fatalf("stream chat failed: %v", err)
	}

	receivedChunk := false
	for ev := range eventChan {
		if ev.DeltaText == "streamed claude" {
			receivedChunk = true
		}
	}

	if err := <-errChan; err != nil {
		t.Fatalf("stream error: %v", err)
	}

	if !receivedChunk {
		t.Errorf("failed to receive expected claude chunk")
	}
}
