package gemini

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/primaybr/liltok/internal/provider"
)

func TestGeminiAdapterSendChat(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {
					"parts": [{"text": "gemini response text"}],
					"role": "model"
				},
				"finishReason": "STOP"
			}],
			"usageMetadata": {
				"promptTokenCount": 30,
				"candidatesTokenCount": 15,
				"totalTokenCount": 45
			}
		}`))
	}))
	defer mockServer.Close()

	adapter := NewAdapter(mockServer.URL, "gem-key")

	req := &provider.UnifiedChatRequest{
		Model: "gemini-1.5-flash",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "hello gemini"},
		},
	}

	resp, err := adapter.SendChat(context.Background(), req)
	if err != nil {
		t.Fatalf("send chat failed: %v", err)
	}

	if resp.Content != "gemini response text" {
		t.Errorf("expected gemini response text, got %s", resp.Content)
	}
	if resp.Usage.TotalTokens != 45 {
		t.Errorf("expected 45 total tokens, got %d", resp.Usage.TotalTokens)
	}
}
