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

func TestGeminiMultiKeyFailover(t *testing.T) {
	key1Hit := false
	key2Hit := false

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := r.URL.Query().Get("key")
		if k == "key-1" {
			key1Hit = true
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error": "RESOURCE_EXHAUSTED"}`))
			return
		}
		if k == "key-2" {
			key2Hit = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"candidates": [{
					"content": {"parts": [{"text": "recovered on key-2"}], "role": "model"},
					"finishReason": "STOP"
				}],
				"usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 5, "totalTokenCount": 15}
			}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer mockServer.Close()

	adapter := NewAdapter(mockServer.URL, "key-1, key-2")
	if adapter.KeyCount() != 2 {
		t.Fatalf("expected 2 keys, got %d", adapter.KeyCount())
	}

	req := &provider.UnifiedChatRequest{
		Model: "gemini-1.5-flash",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "test failover"},
		},
	}

	resp, err := adapter.SendChat(context.Background(), req)
	if err != nil {
		t.Fatalf("expected failover to succeed, got error: %v", err)
	}
	if resp.Content != "recovered on key-2" {
		t.Errorf("expected 'recovered on key-2', got: %q", resp.Content)
	}
	if !key1Hit || !key2Hit {
		t.Errorf("expected both key1 and key2 to be hit, key1=%v, key2=%v", key1Hit, key2Hit)
	}
}

func TestGeminiCheckHealth(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1beta/models" && r.URL.Query().Get("key") == "valid-key" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"models": []}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error": "API_KEY_INVALID"}`))
	}))
	defer mockServer.Close()

	// 1. Unconfigured
	a0 := NewAdapter(mockServer.URL, "")
	if ok, err := a0.CheckHealth(context.Background()); ok || err == nil {
		t.Errorf("expected unconfigured adapter health to fail")
	}

	// 2. Valid key
	a1 := NewAdapter(mockServer.URL, "valid-key")
	if ok, err := a1.CheckHealth(context.Background()); !ok || err != nil {
		t.Errorf("expected valid key to pass health check: %v", err)
	}

	// 3. Invalid key
	a2 := NewAdapter(mockServer.URL, "invalid-key")
	if ok, err := a2.CheckHealth(context.Background()); ok || err == nil {
		t.Errorf("expected invalid key to fail health check")
	}
}

func TestGeminiFunctionCalling(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {
					"parts": [{
						"functionCall": {
							"name": "SendMessage",
							"args": {
								"to": "a139b43b450f80250",
								"message": "Continue"
							}
						}
					}],
					"role": "model"
				},
				"finishReason": "STOP"
			}],
			"usageMetadata": {
				"promptTokenCount": 50,
				"candidatesTokenCount": 20,
				"totalTokenCount": 70
			}
		}`))
	}))
	defer mockServer.Close()

	adapter := NewAdapter(mockServer.URL, "gem-key")

	req := &provider.UnifiedChatRequest{
		Model: "gemini-flash-latest",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "Resume agent"},
		},
		Tools: []interface{}{
			map[string]interface{}{
				"name":        "SendMessage",
				"description": "Send a message to another agent",
				"input_schema": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"to":      map[string]interface{}{"type": "string"},
						"message": map[string]interface{}{"type": "string"},
					},
					"required": []string{"to", "message"},
				},
			},
		},
	}

	resp, err := adapter.SendChat(context.Background(), req)
	if err != nil {
		t.Fatalf("send chat with gemini tools failed: %v", err)
	}

	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Function.Name != "SendMessage" {
		t.Errorf("expected function SendMessage, got %s", resp.ToolCalls[0].Function.Name)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("expected finish_reason tool_calls, got %s", resp.FinishReason)
	}
}
