package router

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/provider"
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

	// Test Claude 5 resolution
	opusTargets := r.ResolveTargets("claude-opus-5", "")
	if len(opusTargets) < 2 || opusTargets[0].ProviderName != "anthropic" || opusTargets[0].UpstreamModel != "claude-opus-5" {
		t.Errorf("expected anthropic claude-opus-5, got %v", opusTargets)
	}
	if opusTargets[1].UpstreamModel != "qwen/qwen3.8-27b" {
		t.Errorf("expected qwen fallback, got %s", opusTargets[1].UpstreamModel)
	}

	// Test auto-resilient route resolution
	resilientTargets := r.ResolveTargets("auto-resilient", "")
	if len(resilientTargets) == 0 || resilientTargets[0].UpstreamModel != "claude-sonnet-5" {
		t.Errorf("expected claude-sonnet-5 primary for auto-resilient, got %v", resilientTargets)
	}

	// Test premium-only route resolution
	premiumTargets := r.ResolveTargets("premium-only", "")
	if len(premiumTargets) == 0 || premiumTargets[0].UpstreamModel != "claude-opus-5" {
		t.Errorf("expected claude-opus-5 primary for premium-only, got %v", premiumTargets)
	}

	// Test free-first route resolution
	freeTargets := r.ResolveTargets("gpt-4o", "free-first")
	if len(freeTargets) == 0 || freeTargets[0].ProviderName != "groq" {
		t.Errorf("expected groq primary for free-first route, got %v", freeTargets)
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
		name: "groq",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "mock-groq-1",
			Content: "hello from Groq fallback",
		},
	}

	r.providers["anthropic"] = primaryMock
	r.providers["groq"] = secondaryMock

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

	if winningProvider != "groq" {
		t.Errorf("expected winning provider groq, got %s", winningProvider)
	}

	if resp.Content != "hello from Groq fallback" {
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

func TestTranslatorOpenAIToAnthropicWithToolCalls(t *testing.T) {
	tr := NewTranslator()
	oaiResp := &provider.UnifiedChatResponse{
		ID:           "test-tool-id",
		Content:      "I will run the command",
		FinishReason: "tool_calls",
		ToolCalls: []provider.UnifiedToolCall{
			{
				ID:   "call_999",
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      "Bash",
					Arguments: `{"command":"git status"}`,
				},
			},
		},
		Usage: provider.UnifiedUsage{
			PromptTokens:     120,
			CompletionTokens: 30,
		},
	}

	anthJSON, err := tr.ConvertOpenAIToAnthropicResponse(oaiResp, "claude-sonnet-5")
	if err != nil {
		t.Fatalf("conversion failed: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(anthJSON, &parsed); err != nil {
		t.Fatalf("invalid json generated: %v", err)
	}

	if parsed["stop_reason"] != "tool_use" {
		t.Errorf("expected stop_reason tool_use, got %v", parsed["stop_reason"])
	}

	contentArr, ok := parsed["content"].([]interface{})
	if !ok || len(contentArr) != 2 {
		t.Fatalf("expected 2 content blocks (text + tool_use), got %v", contentArr)
	}

	toolBlock := contentArr[1].(map[string]interface{})
	if toolBlock["type"] != "tool_use" {
		t.Errorf("expected type tool_use, got %v", toolBlock["type"])
	}
	if toolBlock["name"] != "Bash" {
		t.Errorf("expected tool Bash, got %v", toolBlock["name"])
	}
	inputMap, ok := toolBlock["input"].(map[string]interface{})
	if !ok || inputMap["command"] != "git status" {
		t.Errorf("expected command 'git status', got %v", inputMap)
	}
}

func TestTranslatorOpenAIToAnthropic_TextToolCallFallback(t *testing.T) {
	tr := NewTranslator()
	rawText := `I will resume the Task 1 implementer to finish running its tests, commit the changes, and write the report.
Tool Call: SendMessage({"message":"Continue where you left off. Run the tests to confirm they pass, run the full Widget suite, commit per Step 8 of the brief with the required Co-Authored-By trailer, write the full report to X:\work\www\example\.superpowers\sdd\2026-09-17-feature-overhaul-phase-4\task-1-report.md, and reply with your final status.","summary":"Resume to finish tests, commit, and report","to":"a139b43b450f80250"})`

	oaiResp := &provider.UnifiedChatResponse{
		ID:           "test-gemini-fallback",
		Content:      rawText,
		FinishReason: "stop",
		Usage: provider.UnifiedUsage{
			PromptTokens:     30000,
			CompletionTokens: 150,
		},
	}

	anthJSON, err := tr.ConvertOpenAIToAnthropicResponse(oaiResp, "claude-sonnet-5")
	if err != nil {
		t.Fatalf("conversion failed: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(anthJSON, &parsed); err != nil {
		t.Fatalf("invalid json generated: %v", err)
	}

	if parsed["stop_reason"] != "tool_use" {
		t.Errorf("expected stop_reason tool_use, got %v", parsed["stop_reason"])
	}

	contentArr, ok := parsed["content"].([]interface{})
	if !ok || len(contentArr) != 2 {
		t.Fatalf("expected 2 content blocks (clean text + tool_use), got %v", contentArr)
	}

	textBlock := contentArr[0].(map[string]interface{})
	if textBlock["type"] != "text" || !strings.Contains(textBlock["text"].(string), "I will resume the Task 1 implementer") {
		t.Errorf("unexpected text block: %v", textBlock)
	}

	toolBlock := contentArr[1].(map[string]interface{})
	if toolBlock["type"] != "tool_use" {
		t.Errorf("expected type tool_use, got %v", toolBlock["type"])
	}
	if toolBlock["name"] != "SendMessage" {
		t.Errorf("expected tool SendMessage, got %v", toolBlock["name"])
	}

	inputMap, ok := toolBlock["input"].(map[string]interface{})
	if !ok || inputMap["to"] != "a139b43b450f80250" {
		t.Errorf("expected input to 'a139b43b450f80250', got %v", inputMap)
	}
}

func TestRouterRollingFallbackSequence(t *testing.T) {
	cfg := config.DefaultConfig()
	r := NewRouter(cfg)

	// Register mock groq (failing with 429), mock gemini (failing with 429), mock nvidianim (succeeding)
	r.providers["groq"] = &mockProvider{
		name: "groq",
		fail: true,
	}
	r.providers["gemini"] = &mockProvider{
		name: "gemini",
		fail: true,
	}
	r.providers["nvidianim"] = &mockProvider{
		name: "nvidianim",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "nim-winner-1",
			Content: "hello from NVIDIA NIM rolling winner",
		},
	}

	req := &provider.UnifiedChatRequest{
		Model: "free-first",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "test rolling fallback"},
		},
	}

	resp, winningProvider, err := r.DispatchChat(context.Background(), req, "free-first")
	if err != nil {
		t.Fatalf("dispatch failed unexpectedly: %v", err)
	}

	if winningProvider != "nvidianim" {
		t.Errorf("expected winning provider nvidianim after groq and gemini 429s, got %s", winningProvider)
	}

	if resp.Content != "hello from NVIDIA NIM rolling winner" {
		t.Errorf("unexpected content: %s", resp.Content)
	}
}

func TestRouterContextAwareTargetOrdering(t *testing.T) {
	cfg := config.DefaultConfig()
	r := NewRouter(cfg)

	var attemptedOrder []string
	r.providers["groq"] = &mockProvider{
		name: "groq",
		fail: true,
	}
	r.providers["gemini"] = &mockProvider{
		name: "gemini",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "gemini-large-ctx",
			Content: "handled large prompt",
		},
	}

	// Create a large payload simulating > 25,000 tokens
	largePayload := make([]byte, 120000) // 120,000 bytes / 4 = 30,000 tokens
	req := &provider.UnifiedChatRequest{
		Model:      "free-first",
		RawPayload: largePayload,
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "large prompt"},
		},
	}

	resp, winningProvider, err := r.DispatchChat(context.Background(), req, "free-first")
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	// For > 25,000 tokens, Gemini should be prioritized first
	if winningProvider != "gemini" {
		t.Errorf("expected gemini to win for large prompt, got %s (attempted: %v)", winningProvider, attemptedOrder)
	}
	if resp.Content != "handled large prompt" {
		t.Errorf("unexpected response: %s", resp.Content)
	}
}

func TestRouterOpenRouterResolutionAndDispatch(t *testing.T) {
	cfg := config.DefaultConfig()
	r := NewRouter(cfg)

	// Verify openrouter is registered in providers
	if _, ok := r.GetProvider("openrouter"); !ok {
		t.Fatalf("expected openrouter provider to be registered")
	}

	// Verify openrouter/free targets
	targets := r.ResolveTargets("openrouter/free", "")
	if len(targets) == 0 || targets[0].ProviderName != "openrouter" {
		t.Fatalf("expected openrouter primary target for openrouter/free, got %v", targets)
	}

	// Verify openrouter is part of free-first fallback targets
	freeTargets := r.ResolveTargets("free-first", "")
	foundOR := false
	for _, tgt := range freeTargets {
		if tgt.ProviderName == "openrouter" && tgt.UpstreamModel == "openrouter/free" {
			foundOR = true
			break
		}
	}
	if !foundOR {
		t.Errorf("expected openrouter/free in free-first targets, got %v", freeTargets)
	}

	// Test dispatch to mock openrouter
	r.providers["openrouter"] = &mockProvider{
		name: "openrouter",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "or-test-id",
			Model:   "openrouter/free",
			Content: "hello from openrouter",
		},
	}

	req := &provider.UnifiedChatRequest{
		Model: "openrouter/free",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "hi"},
		},
	}
	resp, winProv, err := r.DispatchChat(context.Background(), req, "")
	if err != nil {
		t.Fatalf("unexpected dispatch error: %v", err)
	}
	if winProv != "openrouter" {
		t.Errorf("expected winning provider openrouter, got %s", winProv)
	}
	if resp.Content != "hello from openrouter" {
		t.Errorf("expected content 'hello from openrouter', got %s", resp.Content)
	}
}

