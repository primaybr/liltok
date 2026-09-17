package router

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/liltok/liltok/internal/config"
	"github.com/liltok/liltok/internal/provider"
)

type mockProvider struct {
	name      string
	tier      provider.ProviderTier
	fail      bool
	failCount int
	response  *provider.UnifiedChatResponse
}

func (m *mockProvider) Name() string { return m.name }
func (m *mockProvider) Tier() provider.ProviderTier { return m.tier }
func (m *mockProvider) CheckHealth(ctx context.Context) (bool, error) { return !m.fail, nil }

func (m *mockProvider) SendChat(ctx context.Context, req *provider.UnifiedChatRequest) (*provider.UnifiedChatResponse, error) {
	if m.fail {
		m.failCount++
		return nil, errors.New("simulated provider 429 rate limit")
	}
	return m.response, nil
}

func (m *mockProvider) StreamChat(ctx context.Context, req *provider.UnifiedChatRequest) (<-chan provider.UnifiedSSEEvent, <-chan error, error) {
	if m.fail {
		return nil, nil, errors.New("simulated stream failure")
	}
	ch := make(chan provider.UnifiedSSEEvent, 1)
	errCh := make(chan error, 1)
	ch <- provider.UnifiedSSEEvent{Type: "text_delta", DeltaText: m.response.Content}
	close(ch)
	close(errCh)
	return ch, errCh, nil
}

func TestRouterTargetResolution(t *testing.T) {
	cfg := config.DefaultConfig()
	r := NewRouter(cfg)

	// Test Claude resolution
	targets := r.ResolveTargets("claude-3-5-sonnet-20241022", "")
	if len(targets) < 2 || targets[0].ProviderName != "anthropic" {
		t.Errorf("expected anthropic primary for claude, got %v", targets)
	}

	// Test free-first route resolution
	freeTargets := r.ResolveTargets("gpt-4o", "free-first")
	if len(freeTargets) == 0 || freeTargets[0].ProviderName != "nvidianim" {
		t.Errorf("expected nvidianim primary for free-first route, got %v", freeTargets)
	}
}

func TestRouterFailoverExecution(t *testing.T) {
	cfg := config.DefaultConfig()
	r := NewRouter(cfg)

	// Register mock primary (failing) and mock secondary (succeeding)
	primaryMock := &mockProvider{
		name: "anthropic",
		fail: true,
	}
	secondaryMock := &mockProvider{
		name: "nvidianim",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "mock-nim-1",
			Content: "hello from NVIDIA NIM fallback",
		},
	}

	r.providers["anthropic"] = primaryMock
	r.providers["nvidianim"] = secondaryMock

	req := &provider.UnifiedChatRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "refactor code"},
		},
	}

	resp, winningProvider, err := r.DispatchChat(context.Background(), req, "")
	if err != nil {
		t.Fatalf("dispatch failed unexpectedly: %v", err)
	}

	if winningProvider != "nvidianim" {
		t.Errorf("expected winning provider nvidianim, got %s", winningProvider)
	}

	if resp.Content != "hello from NVIDIA NIM fallback" {
		t.Errorf("unexpected content: %s", resp.Content)
	}

	if primaryMock.failCount != 1 {
		t.Errorf("expected 1 failure on primary mock, got %d", primaryMock.failCount)
	}
}

func TestTranslatorOpenAIToAnthropic(t *testing.T) {
	tr := NewTranslator()
	oaiResp := &provider.UnifiedChatResponse{
		ID:           "test-id",
		Content:      "translated answer",
		FinishReason: "stop",
		Usage: provider.UnifiedUsage{
			PromptTokens:     100,
			CompletionTokens: 50,
		},
	}

	anthJSON, err := tr.ConvertOpenAIToAnthropicResponse(oaiResp, "claude-3-5-sonnet")
	if err != nil {
		t.Fatalf("conversion failed: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(anthJSON, &parsed); err != nil {
		t.Fatalf("invalid json generated: %v", err)
	}

	if parsed["type"] != "message" {
		t.Errorf("expected type message, got %v", parsed["type"])
	}
	if parsed["stop_reason"] != "end_turn" {
		t.Errorf("expected stop_reason end_turn, got %v", parsed["stop_reason"])
	}

	contentArr, ok := parsed["content"].([]interface{})
	if !ok || len(contentArr) == 0 {
		t.Fatalf("missing content array: %v", parsed)
	}

	contentObj := contentArr[0].(map[string]interface{})
	if contentObj["text"] != "translated answer" {
		t.Errorf("expected text 'translated answer', got %v", contentObj["text"])
	}
}
