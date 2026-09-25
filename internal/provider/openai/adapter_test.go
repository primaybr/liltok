package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
		Model:  "gpt-4o",
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

func TestOpenAIAdapterListModelsGroq(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"object": "list",
			"data": [
				{
					"id": "openai/gpt-oss-120b",
					"object": "model",
					"active": true,
					"context_window": 131072,
					"owned_by": "openai"
				},
				{
					"id": "qwen/qwen3.8-27b",
					"object": "model",
					"active": true,
					"context_window": 131042,
					"owned_by": "qwen"
				},
				{
					"id": "llama-3.3-70b-versatile",
					"object": "model",
					"active": false,
					"context_window": 131072,
					"owned_by": "meta"
				}
			]
		}`))
	}))
	defer mockServer.Close()

	adapter := NewAdapter("groq", provider.TierFree, mockServer.URL, "gsk_test_key")
	models, err := adapter.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}

	if len(models) != 2 {
		t.Fatalf("expected 2 active models (1 inactive filtered out), got %d", len(models))
	}

	for _, m := range models {
		if m.ID == "llama-3.3-70b-versatile" {
			t.Errorf("inactive model llama-3.3-70b-versatile was not filtered out")
		}
		if !m.Active {
			t.Errorf("expected model %s to be active", m.ID)
		}
	}
}

func TestOpenAIAdapterListModelsNVIDIANIM(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"object": "list",
			"data": [
				{
					"id": "deepseek-ai/deepseek-v4-flash-0731",
					"object": "model",
					"owned_by": "deepseek-ai"
				},
				{
					"id": "google/gemma-4-31b-it",
					"object": "model",
					"owned_by": "google"
				},
				{
					"id": "nvidia/embed-qa-4",
					"object": "model",
					"owned_by": "nvidia"
				},
				{
					"id": "meta/llama-3.1-70b-instruct",
					"object": "model",
					"owned_by": "meta"
				},
				{
					"id": "nvidia/ai-synthetic-video-detector",
					"object": "model",
					"owned_by": "nvidia"
				},
				{
					"id": "nvidia/nemotron-3.5-lightning-30b-a3b",
					"object": "model",
					"owned_by": "nvidia"
				}
			]
		}`))
	}))
	defer mockServer.Close()

	adapter := NewAdapter("nvidianim", provider.TierFree, mockServer.URL, "nvapi-test-key")
	models, err := adapter.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}

	if len(models) != 3 {
		t.Fatalf("expected 3 valid chat/reasoning models, got %d", len(models))
	}

	expected := map[string]int{
		"deepseek-ai/deepseek-v4-flash-0731":    131072,
		"google/gemma-4-31b-it":                 131072,
		"nvidia/nemotron-3.5-lightning-30b-a3b": 131072,
	}

	for _, m := range models {
		expectedCtx, exists := expected[m.ID]
		if !exists {
			t.Errorf("unexpected model kept: %s", m.ID)
		}
		if m.ContextWindow != expectedCtx {
			t.Errorf("expected context window %d for %s, got %d", expectedCtx, m.ID, m.ContextWindow)
		}
		if !m.Active {
			t.Errorf("expected model %s to be active", m.ID)
		}
	}
}

func TestOpenAIAdapterListModelsOpenRouter(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"data": [
				{
					"id": "deepseek/deepseek-v4-flash-0731:free",
					"context_length": 163840,
					"pricing": {
						"prompt": "0",
						"completion": "0"
					},
					"architecture": {
						"modality": "text->text",
						"output_modalities": ["text"]
					}
				},
				{
					"id": "openrouter/free",
					"context_length": 200000,
					"pricing": {
						"prompt": "0",
						"completion": "0"
					},
					"architecture": {
						"modality": "text->text",
						"output_modalities": ["text"]
					}
				},
				{
					"id": "openai/gpt-4o",
					"context_length": 128000,
					"pricing": {
						"prompt": "0.0000025",
						"completion": "0.00001"
					},
					"architecture": {
						"modality": "text->text",
						"output_modalities": ["text"]
					}
				},
				{
					"id": "google/lyria-3-pro-preview",
					"context_length": 32768,
					"pricing": {
						"prompt": "0",
						"completion": "0"
					},
					"architecture": {
						"modality": "text->audio",
						"output_modalities": ["text", "audio"]
					}
				},
				{
					"id": "meta-llama/llama-guard-3-8b",
					"context_length": 8192,
					"pricing": {
						"prompt": "0",
						"completion": "0"
					},
					"architecture": {
						"modality": "text->text",
						"output_modalities": ["text"]
					}
				}
			]
		}`))
	}))
	defer mockServer.Close()

	adapter := NewAdapter("openrouter", provider.TierFree, mockServer.URL, "sk-or-test-key")
	models, err := adapter.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}

	if len(models) != 2 {
		t.Fatalf("expected 2 free chat/reasoning models, got %d", len(models))
	}

	expected := map[string]int{
		"deepseek/deepseek-v4-flash-0731:free": 163840,
		"openrouter/free":                      200000,
	}

	for _, m := range models {
		expectedCtx, exists := expected[m.ID]
		if !exists {
			t.Errorf("unexpected model kept: %s", m.ID)
		}
		if m.ContextWindow != expectedCtx {
			t.Errorf("expected context window %d for %s, got %d", expectedCtx, m.ID, m.ContextWindow)
		}
		if !m.Active {
			t.Errorf("expected model %s to be active", m.ID)
		}
		if m.Provider != "openrouter" {
			t.Errorf("expected provider openrouter, got %s", m.Provider)
		}
	}
}

func TestOpenAIAdapterListModelsKilo(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"data": [
				{
					"id": "kilo-auto/free",
					"context_length": 262144,
					"pricing": {
						"prompt": "0",
						"completion": "0"
					},
					"architecture": {
						"modality": "text->text",
						"output_modalities": ["text"]
					}
				},
				{
					"id": "deepseek/deepseek-v4-flash-0731:free",
					"context_length": 163840,
					"pricing": {
						"prompt": "0",
						"completion": "0"
					},
					"architecture": {
						"modality": "text->text",
						"output_modalities": ["text"]
					}
				},
				{
					"id": "openai/gpt-4o",
					"context_length": 128000,
					"pricing": {
						"prompt": "0.000005",
						"completion": "0.000015"
					},
					"architecture": {
						"modality": "text->text",
						"output_modalities": ["text"]
					}
				},
				{
					"id": "google/lyria:free",
					"context_length": 4096,
					"pricing": {
						"prompt": "0",
						"completion": "0"
					},
					"architecture": {
						"modality": "text->audio",
						"output_modalities": ["text", "audio"]
					}
				},
				{
					"id": "meta-llama/llama-guard-3-8b:free",
					"context_length": 8192,
					"pricing": {
						"prompt": "0",
						"completion": "0"
					},
					"architecture": {
						"modality": "text->text",
						"output_modalities": ["text"]
					}
				}
			]
		}`))
	}))
	defer mockServer.Close()

	adapter := NewKiloAdapter("", mockServer.URL)
	models, err := adapter.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}

	if len(models) != 2 {
		t.Fatalf("expected 2 free chat/reasoning models for Kilo, got %d", len(models))
	}

	expected := map[string]int{
		"kilo-auto/free":                       262144,
		"deepseek/deepseek-v4-flash-0731:free": 163840,
	}

	for _, m := range models {
		expectedCtx, exists := expected[m.ID]
		if !exists {
			t.Errorf("unexpected model kept: %s", m.ID)
		}
		if m.ContextWindow != expectedCtx {
			t.Errorf("expected context window %d for %s, got %d", expectedCtx, m.ID, m.ContextWindow)
		}
		if !m.Active {
			t.Errorf("expected model %s to be active", m.ID)
		}
		if m.Provider != "kilo" {
			t.Errorf("expected provider kilo, got %s", m.Provider)
		}
	}
}

func TestClineAdapter_ListModels_FiltersFreeChatModels(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-cline-key" {
			t.Errorf("missing or invalid authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"data": [
				{
					"id": "deepseek/deepseek-v4-flash-0731:free",
					"owned_by": "deepseek"
				},
				{
					"id": "qwen/qwen3.8-27b:free",
					"owned_by": "qwen"
				},
				{
					"id": "minimax/minimax-m2.5",
					"owned_by": "minimax"
				},
				{
					"id": "meta-llama/llama-guard-4-12b:free",
					"owned_by": "meta-llama"
				},
				{
					"id": "openai/text-embedding-3-small:free",
					"owned_by": "openai"
				}
			]
		}`))
	}))
	defer mockServer.Close()

	adapter := NewClineAdapter("test-cline-key", mockServer.URL)
	models, err := adapter.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}

	if len(models) != 2 {
		t.Fatalf("expected 2 free chat models for Cline, got %d", len(models))
	}

	for _, m := range models {
		if !strings.HasSuffix(m.ID, ":free") {
			t.Errorf("expected free model suffix: %s", m.ID)
		}
		if m.ContextWindow != 262144 {
			t.Errorf("expected 262144 context window, got %d", m.ContextWindow)
		}
		if m.Provider != "cline" {
			t.Errorf("expected provider cline, got %s", m.Provider)
		}
	}
}

func TestClineAdapter_SendChat_UnwrapsData(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"data": {
				"id": "chatcmpl-cline-123",
				"model": "deepseek/deepseek-v4-flash-0731:free",
				"choices": [
					{
						"index": 0,
						"message": {
							"role": "assistant",
							"content": "Hello from Cline!"
						},
						"finish_reason": "stop"
					}
				],
				"usage": {
					"prompt_tokens": 10,
					"completion_tokens": 5,
					"total_tokens": 15
				}
			},
			"success": true
		}`))
	}))
	defer mockServer.Close()

	adapter := NewClineAdapter("test-cline-key", mockServer.URL)
	resp, err := adapter.SendChat(context.Background(), &provider.UnifiedChatRequest{
		Model: "deepseek/deepseek-v4-flash-0731:free",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "Hi"},
		},
	})
	if err != nil {
		t.Fatalf("SendChat failed: %v", err)
	}

	if resp.Content != "Hello from Cline!" {
		t.Errorf("expected 'Hello from Cline!', got %q", resp.Content)
	}
	if resp.Usage.TotalTokens != 15 {
		t.Errorf("expected 15 total tokens, got %d", resp.Usage.TotalTokens)
	}
	if resp.ID != "chatcmpl-cline-123" {
		t.Errorf("expected ID chatcmpl-cline-123, got %q", resp.ID)
	}

	var stdOAI struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(resp.RawResponse, &stdOAI); err != nil || len(stdOAI.Choices) == 0 {
		t.Fatalf("expected valid standard OpenAI RawResponse with choices, got %s (err: %v)", string(resp.RawResponse), err)
	}
	if stdOAI.Choices[0].Message.Content != "Hello from Cline!" {
		t.Errorf("expected RawResponse content 'Hello from Cline!', got %q", stdOAI.Choices[0].Message.Content)
	}
}

func TestClineAdapter_SendChat_HandlesErrors(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"success": false,
			"error": "rate limit exceeded: please slow down"
		}`))
	}))
	defer mockServer.Close()

	adapter := NewClineAdapter("test-cline-key", mockServer.URL)
	_, err := adapter.SendChat(context.Background(), &provider.UnifiedChatRequest{
		Model: "deepseek/deepseek-v4-flash-0731:free",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "Hi"},
		},
	})
	if err == nil {
		t.Fatalf("expected error from failed response, got nil")
	}
	if !strings.Contains(err.Error(), "rate limit exceeded") {
		t.Errorf("expected rate limit exceeded in error message, got %v", err)
	}
}

func TestClineAdapter_StreamChat_HandlesJSONError(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"success": false,
			"error": "upstream model overloaded"
		}`))
	}))
	defer mockServer.Close()

	adapter := NewClineAdapter("test-cline-key", mockServer.URL)
	_, _, err := adapter.StreamChat(context.Background(), &provider.UnifiedChatRequest{
		Model: "deepseek/deepseek-v4-flash-0731:free",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "Hi"},
		},
	})
	if err == nil {
		t.Fatalf("expected error from stream json error, got nil")
	}
	if !strings.Contains(err.Error(), "upstream model overloaded") {
		t.Errorf("expected overloaded in error message, got %v", err)
	}
}

func TestOpenAIAdapter_BuildPayload_ToolCallsAndToolRole(t *testing.T) {
	adapter := NewAdapter("openai", provider.TierPremium, "https://api.openai.com/v1", "test-key")

	req := &provider.UnifiedChatRequest{
		Model: "gpt-4o",
		Messages: []provider.UnifiedChatMessage{
			{
				Role:    "assistant",
				Content: "",
				ToolCalls: []provider.UnifiedToolCall{
					{
						ID:   "call_abc123",
						Type: "function",
						Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{
							Name:      "read_file",
							Arguments: `{"path":"main.go"}`,
						},
					},
				},
			},
			{
				Role:       "tool",
				ToolCallID: "call_abc123",
				Content:    "package main\n\nfunc main() {}",
			},
			{
				Role:    "user",
				Content: "now run it",
			},
		},
	}

	payloadBytes, err := adapter.buildPayload(req, false)
	if err != nil {
		t.Fatalf("buildPayload failed: %v", err)
	}

	var payload struct {
		Messages []struct {
			Role       string  `json:"role"`
			Content    *string `json:"content"`
			ToolCallID string  `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}

	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("unmarshal payload failed: %v", err)
	}

	if len(payload.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(payload.Messages))
	}

	// Message 1: assistant with tool_calls and null content
	asst := payload.Messages[0]
	if asst.Role != "assistant" {
		t.Errorf("expected role assistant, got %s", asst.Role)
	}
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "call_abc123" {
		t.Errorf("expected tool call call_abc123, got %+v", asst.ToolCalls)
	}
	if asst.Content != nil {
		t.Errorf("expected null content for tool-calling assistant message, got %v", asst.Content)
	}

	// Message 2: tool role with tool_call_id
	toolMsg := payload.Messages[1]
	if toolMsg.Role != "tool" {
		t.Errorf("expected role tool, got %s", toolMsg.Role)
	}
	if toolMsg.ToolCallID != "call_abc123" {
		t.Errorf("expected tool_call_id call_abc123, got %s", toolMsg.ToolCallID)
	}
	if toolMsg.Content == nil || *toolMsg.Content != "package main\n\nfunc main() {}" {
		t.Errorf("unexpected tool content: %v", toolMsg.Content)
	}

	// Message 3: user message
	userMsg := payload.Messages[2]
	if userMsg.Role != "user" {
		t.Errorf("expected role user, got %s", userMsg.Role)
	}
}

func TestOpenAIAdapter_SendChat_ReasoningFallback(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"id": "chatcmpl-test-r1",
			"model": "deepseek-r1",
			"choices": [
				{
					"index": 0,
					"message": {
						"role": "assistant",
						"content": "",
						"reasoning_content": "The solution is to use binary search."
					},
					"finish_reason": "stop"
				}
			],
			"usage": {
				"prompt_tokens": 10,
				"completion_tokens": 8,
				"total_tokens": 18
			}
		}`))
	}))
	defer mockServer.Close()

	adapter := NewAdapter("openrouter", provider.TierFree, mockServer.URL, "sk-or-test")
	resp, err := adapter.SendChat(context.Background(), &provider.UnifiedChatRequest{
		Model: "deepseek/deepseek-r1",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "How do I solve this?"},
		},
	})
	if err != nil {
		t.Fatalf("SendChat failed: %v", err)
	}

	if resp.Content != "The solution is to use binary search." {
		t.Errorf("expected reasoning_content fallback, got %q", resp.Content)
	}
}

func TestOpenAIAdapter_BuildPayload_TrailingSystemMessage(t *testing.T) {
	adapter := NewAdapter("openrouter", provider.TierFree, "https://openrouter.ai/api/v1", "sk-test")

	req := &provider.UnifiedChatRequest{
		Model:        "deepseek/deepseek-v4-flash-0731:free",
		SystemPrompt: "You are an expert developer.",
		Messages: []provider.UnifiedChatMessage{
			{
				Role:    "user",
				Content: "Check repo",
			},
			{
				Role: "assistant",
				ToolCalls: []provider.UnifiedToolCall{
					{
						ID:   "call_1",
						Type: "function",
						Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{
							Name:      "Bash",
							Arguments: `{"command":"pwd"}`,
						},
					},
				},
			},
			{
				Role:       "tool",
				ToolCallID: "call_1",
				Content:    "/workspace",
			},
			{
				Role:    "system",
				Content: "<system-reminder>\nEnvironment update: directory changed\n</system-reminder>",
			},
		},
	}

	payloadBytes, err := adapter.buildPayload(req, false)
	if err != nil {
		t.Fatalf("buildPayload failed: %v", err)
	}

	var payload struct {
		Messages []struct {
			Role    string  `json:"role"`
			Content *string `json:"content"`
		} `json:"messages"`
	}

	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	// Expected order:
	// 0: system (SystemPrompt)
	// 1: user
	// 2: assistant
	// 3: tool
	// 4: user (normalized from trailing system message)
	if len(payload.Messages) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(payload.Messages))
	}

	sysInitial := payload.Messages[0]
	if sysInitial.Role != "system" || sysInitial.Content == nil || *sysInitial.Content != "You are an expert developer." {
		t.Errorf("expected initial system prompt, got %+v", sysInitial)
	}

	lastMsg := payload.Messages[4]
	if lastMsg.Role != "user" {
		t.Errorf("expected trailing system message to be converted to role user, got %s", lastMsg.Role)
	}
	if lastMsg.Content == nil || !strings.HasPrefix(*lastMsg.Content, "[System Reminder]\n<system-reminder>") {
		t.Errorf("expected [System Reminder] prefix in converted user message, got %v", lastMsg.Content)
	}
}
