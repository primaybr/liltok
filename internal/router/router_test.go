package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/provider"
)

type mockProvider struct {
	name      string
	tier      provider.ProviderTier
	fail      bool
	failCount int
	response  *provider.UnifiedChatResponse
	lastModel string
}

func (m *mockProvider) Name() string { return m.name }
func (m *mockProvider) Tier() provider.ProviderTier { return m.tier }
func (m *mockProvider) CheckHealth(ctx context.Context) (bool, error) { return !m.fail, nil }

func (m *mockProvider) SendChat(ctx context.Context, req *provider.UnifiedChatRequest) (*provider.UnifiedChatResponse, error) {
	if req != nil {
		m.lastModel = req.Model
	}
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

	r.SetProvider("anthropic", primaryMock)
	r.SetProvider("groq", secondaryMock)

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
	r.SetProvider("groq", &mockProvider{
		name: "groq",
		fail: true,
	})
	r.SetProvider("gemini", &mockProvider{
		name: "gemini",
		fail: true,
	})
	r.SetProvider("nvidianim", &mockProvider{
		name: "nvidianim",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "nim-winner-1",
			Content: "hello from NVIDIA NIM rolling winner",
		},
	})

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
	r.SetProvider("groq", &mockProvider{
		name: "groq",
		fail: true,
	})
	r.SetProvider("gemini", &mockProvider{
		name: "gemini",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "gemini-large-ctx",
			Content: "handled large prompt",
		},
	})

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
	r.SetProvider("openrouter", &mockProvider{
		name: "openrouter",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "or-test-id",
			Model:   "openrouter/free",
			Content: "hello from openrouter",
		},
	})

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

func TestRouterGroqActiveModelResolutionAndRemapping(t *testing.T) {
	cfg := config.DefaultConfig()
	r := NewRouter(cfg)

	// 1. Test deprecated model remapping in ResolveTargets
	targets := r.ResolveTargets("llama-3.3-70b-versatile", "")
	if len(targets) == 0 {
		t.Fatalf("expected targets for deprecated llama-3.3-70b-versatile")
	}
	if targets[0].ProviderName != "groq" || targets[0].UpstreamModel != "openai/gpt-oss-120b" {
		t.Errorf("expected llama-3.3-70b-versatile to remap to groq/openai/gpt-oss-120b, got %v", targets[0])
	}

	targets8b := r.ResolveTargets("groq/llama-3.1-8b-instant", "")
	if len(targets8b) == 0 {
		t.Fatalf("expected targets for deprecated groq/llama-3.1-8b-instant")
	}
	if targets8b[0].ProviderName != "groq" || targets8b[0].UpstreamModel != "openai/gpt-oss-20b" {
		t.Errorf("expected groq/llama-3.1-8b-instant to remap to groq/openai/gpt-oss-20b, got %v", targets8b[0])
	}

	// 2. Test active model resolution directly
	targetsCompound := r.ResolveTargets("groq/compound", "")
	if len(targetsCompound) == 0 || targetsCompound[0].UpstreamModel != "groq/compound" {
		t.Errorf("expected groq/compound target, got %v", targetsCompound)
	}

	// 3. Test IsActiveModel check
	if !r.IsActiveModel("groq", "openai/gpt-oss-120b") {
		t.Errorf("expected openai/gpt-oss-120b to be active on groq")
	}
	if !r.IsActiveModel("groq", "qwen/qwen3.8-27b") {
		t.Errorf("expected qwen/qwen3.8-27b to be active on groq")
	}
	if r.IsActiveModel("groq", "llama-3.3-70b-versatile") {
		t.Errorf("expected decommissioned llama-3.3-70b-versatile to NOT be active on groq")
	}

	// 4. Test GetAllActiveModels
	allModels := r.GetAllActiveModels(context.Background())
	if len(allModels) == 0 {
		t.Errorf("expected GetAllActiveModels to return active models")
	}
	foundGPT120B := false
	for _, m := range allModels {
		if m.Provider == "groq" && m.ID == "openai/gpt-oss-120b" {
			foundGPT120B = true
			break
		}
	}
	if !foundGPT120B {
		t.Errorf("expected groq/openai/gpt-oss-120b in GetAllActiveModels")
	}
}

func TestRouterNVIDIANIMActiveModelResolutionAndRemapping(t *testing.T) {
	cfg := config.DefaultConfig()
	r := NewRouter(cfg)

	// 1. Test deprecated model remapping in ResolveTargets
	targetsLlama33 := r.ResolveTargets("meta/llama-3.3-70b-instruct", "")
	if len(targetsLlama33) == 0 {
		t.Fatalf("expected targets for deprecated meta/llama-3.3-70b-instruct")
	}
	if targetsLlama33[0].ProviderName != "nvidianim" || targetsLlama33[0].UpstreamModel != "nvidia/nemotron-3.5-lightning-30b-a3b" {
		t.Errorf("expected meta/llama-3.3-70b-instruct to remap to nvidia/nemotron-3.5-lightning-30b-a3b, got %v", targetsLlama33[0])
	}

	targets70b := r.ResolveTargets("meta/llama-3.1-70b-instruct", "")
	if len(targets70b) == 0 {
		t.Fatalf("expected targets for deprecated meta/llama-3.1-70b-instruct")
	}
	if targets70b[0].ProviderName != "nvidianim" || targets70b[0].UpstreamModel != "nvidia/nemotron-3.5-lightning-30b-a3b" {
		t.Errorf("expected meta/llama-3.1-70b-instruct to remap to nvidia/nemotron-3.5-lightning-30b-a3b, got %v", targets70b[0])
	}

	targetsR1 := r.ResolveTargets("deepseek-r1", "")
	if len(targetsR1) == 0 {
		t.Fatalf("expected targets for deepseek-r1")
	}
	if targetsR1[0].ProviderName != "nvidianim" || targetsR1[0].UpstreamModel != "deepseek-ai/deepseek-v4-flash-0731" {
		t.Errorf("expected deepseek-r1 to remap to deepseek-ai/deepseek-v4-flash-0731, got %v", targetsR1[0])
	}

	// 2. Test active model resolution directly
	targetsReasoning := r.ResolveTargets("google/gemma-4-31b-it", "")
	if len(targetsReasoning) == 0 || targetsReasoning[0].UpstreamModel != "google/gemma-4-31b-it" {
		t.Errorf("expected google/gemma-4-31b-it target, got %v", targetsReasoning)
	}

	// 3. Test IsActiveModel check
	if !r.IsActiveModel("nvidianim", "deepseek-ai/deepseek-v4-flash-0731") {
		t.Errorf("expected deepseek-ai/deepseek-v4-flash-0731 to be active on nvidianim")
	}
	if !r.IsActiveModel("nvidianim", "google/gemma-4-31b-it") {
		t.Errorf("expected google/gemma-4-31b-it to be active on nvidianim")
	}
	if !r.IsActiveModel("nvidianim", "nvidia/nemotron-3.5-lightning-30b-a3b") {
		t.Errorf("expected nvidia/nemotron-3.5-lightning-30b-a3b to be active on nvidianim")
	}
	if r.IsActiveModel("nvidianim", "meta/llama-3.1-70b-instruct") {
		t.Errorf("expected decommissioned meta/llama-3.1-70b-instruct to NOT be active on nvidianim")
	}
	if r.IsActiveModel("nvidianim", "meta/llama-3.3-70b-instruct") {
		t.Errorf("expected decommissioned meta/llama-3.3-70b-instruct to NOT be active on nvidianim")
	}

	// 4. Test GetAllActiveModels contains both Groq and NVIDIA NIM models
	allModels := r.GetAllActiveModels(context.Background())
	foundDeepSeek := false
	foundGemma := false
	for _, m := range allModels {
		if m.Provider == "nvidianim" && m.ID == "deepseek-ai/deepseek-v4-flash-0731" {
			foundDeepSeek = true
		}
		if m.Provider == "nvidianim" && m.ID == "google/gemma-4-31b-it" {
			foundGemma = true
		}
	}
	if !foundDeepSeek {
		t.Errorf("expected nvidianim/deepseek-ai/deepseek-v4-flash-0731 in GetAllActiveModels")
	}
	if !foundGemma {
		t.Errorf("expected nvidianim/google/gemma-4-31b-it in GetAllActiveModels")
	}
}

func TestRouterOpenRouterActiveModelResolutionAndRemapping(t *testing.T) {
	cfg := config.DefaultConfig()
	r := NewRouter(cfg)

	// 1. Test deprecated/shorthand alias remapping in ResolveTargets
	targetsR1 := r.ResolveTargets("deepseek-r1:free", "")
	if len(targetsR1) == 0 {
		t.Fatalf("expected targets for deepseek-r1:free")
	}
	if targetsR1[0].ProviderName != "openrouter" || targetsR1[0].UpstreamModel != "deepseek/deepseek-v4-flash-0731:free" {
		t.Errorf("expected deepseek-r1:free to remap to deepseek/deepseek-v4-flash-0731:free, got %v", targetsR1[0])
	}

	targetsLlama := r.ResolveTargets("meta-llama/llama-3.3-70b-instruct:free", "")
	if len(targetsLlama) == 0 {
		t.Fatalf("expected targets for meta-llama/llama-3.3-70b-instruct:free")
	}
	if targetsLlama[0].ProviderName != "openrouter" || targetsLlama[0].UpstreamModel != "nvidia/nemotron-3.5-lightning:free" {
		t.Errorf("expected meta-llama/llama-3.3-70b-instruct:free to remap to nvidia/nemotron-3.5-lightning:free, got %v", targetsLlama[0])
	}

	targetsAuto := r.ResolveTargets("openrouter/auto", "")
	if len(targetsAuto) == 0 {
		t.Fatalf("expected targets for openrouter/auto")
	}
	if targetsAuto[0].ProviderName != "openrouter" || targetsAuto[0].UpstreamModel != "openrouter/free" {
		t.Errorf("expected openrouter/auto to remap to openrouter/free, got %v", targetsAuto[0])
	}

	// 2. Test active model resolution directly
	targetsDirect := r.ResolveTargets("deepseek/deepseek-v4-flash-0731:free", "")
	if len(targetsDirect) == 0 || targetsDirect[0].ProviderName != "openrouter" || targetsDirect[0].UpstreamModel != "deepseek/deepseek-v4-flash-0731:free" {
		t.Errorf("expected direct resolution for deepseek/deepseek-v4-flash-0731:free, got %v", targetsDirect)
	}

	targetsGemma := r.ResolveTargets("openrouter/google/gemma-4-31b-it:free", "")
	if len(targetsGemma) == 0 || targetsGemma[0].ProviderName != "openrouter" || targetsGemma[0].UpstreamModel != "google/gemma-4-31b-it:free" {
		t.Errorf("expected direct resolution for openrouter/google/gemma-4-31b-it:free, got %v", targetsGemma)
	}

	// 3. Test IsActiveModel check
	if !r.IsActiveModel("openrouter", "deepseek/deepseek-v4-flash-0731:free") {
		t.Errorf("expected deepseek/deepseek-v4-flash-0731:free to be active on openrouter")
	}
	if !r.IsActiveModel("openrouter", "google/gemma-4-31b-it:free") {
		t.Errorf("expected google/gemma-4-31b-it:free to be active on openrouter")
	}
	if !r.IsActiveModel("openrouter", "openrouter/free") {
		t.Errorf("expected openrouter/free to be active on openrouter")
	}
	if !r.IsActiveModel("openrouter", "nvidia/nemotron-3.5-lightning:free") {
		t.Errorf("expected nvidia/nemotron-3.5-lightning:free to be active on openrouter")
	}
	if r.IsActiveModel("openrouter", "deepseek-r1:free") {
		t.Errorf("expected decommissioned deepseek-r1:free to NOT be directly in active catalog")
	}
	if r.IsActiveModel("openrouter", "meta-llama/llama-3.3-70b-instruct:free") {
		t.Errorf("expected decommissioned meta-llama/llama-3.3-70b-instruct:free to NOT be directly in active catalog")
	}

	// 4. Test GetAllActiveModels contains OpenRouter models
	allModels := r.GetAllActiveModels(context.Background())
	foundORDeepSeek := false
	foundORGemma := false
	for _, m := range allModels {
		if m.Provider == "openrouter" && m.ID == "deepseek/deepseek-v4-flash-0731:free" {
			foundORDeepSeek = true
		}
		if m.Provider == "openrouter" && m.ID == "google/gemma-4-31b-it:free" {
			foundORGemma = true
		}
	}
	if !foundORDeepSeek {
		t.Errorf("expected openrouter/deepseek/deepseek-v4-flash-0731:free in GetAllActiveModels")
	}
	if !foundORGemma {
		t.Errorf("expected openrouter/google/gemma-4-31b-it:free in GetAllActiveModels")
	}

	// 5. Test DispatchChat auto-remaps deprecated model
	mockOR := &mockProvider{
		name: "openrouter",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "mock-or-id",
			Model:   "deepseek/deepseek-v4-flash-0731:free",
			Content: "remapped openrouter response",
		},
	}
	r.SetProvider("openrouter", mockOR)

	req := &provider.UnifiedChatRequest{
		Model: "openrouter/deepseek-r1:free",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "hello"},
		},
	}
	resp, winProv, err := r.DispatchChat(context.Background(), req, "")
	if err != nil {
		t.Fatalf("unexpected dispatch error: %v", err)
	}
	if winProv != "openrouter" {
		t.Errorf("expected winning provider openrouter, got %s", winProv)
	}
	if resp.Content != "remapped openrouter response" {
		t.Errorf("expected content 'remapped openrouter response', got %s", resp.Content)
	}
	if mockOR.lastModel != "deepseek/deepseek-v4-flash-0731:free" {
		t.Errorf("expected provider to receive remapped model deepseek/deepseek-v4-flash-0731:free, got %s", mockOR.lastModel)
	}
}

func TestRouterKiloActiveModelResolutionAndRemapping(t *testing.T) {
	cfg := config.DefaultConfig()
	r := NewRouter(cfg)

	// 1. Test RemapKiloModel
	tests := []struct {
		input    string
		expected string
	}{
		{"kilo/free", "kilo-auto/free"},
		{"free", "kilo-auto/free"},
		{"kilo-auto", "kilo-auto/free"},
		{"kilo/deepseek-r1", "deepseek/deepseek-v4-flash-0731:free"},
		{"deepseek/deepseek-v4-flash-0731:free", "deepseek/deepseek-v4-flash-0731:free"},
		{"qwen/qwen3.8-27b:free", "qwen/qwen3.8-27b:free"},
	}

	for _, tc := range tests {
		got, _ := RemapKiloModel(tc.input)
		if got != tc.expected {
			t.Errorf("RemapKiloModel(%q) = %q, expected %q", tc.input, got, tc.expected)
		}
	}

	// 2. Test ResolveTargets with kilo prefix
	targets := r.ResolveTargets("kilo/kilo-auto/free", "")
	if len(targets) == 0 {
		t.Fatalf("expected at least 1 target for kilo/kilo-auto/free")
	}
	if targets[0].ProviderName != "kilo" {
		t.Errorf("expected primary provider kilo, got %s", targets[0].ProviderName)
	}
	if targets[0].UpstreamModel != "kilo-auto/free" {
		t.Errorf("expected model kilo-auto/free, got %s", targets[0].UpstreamModel)
	}

	aliasTargets := r.ResolveTargets("kilo/free", "")
	if len(aliasTargets) == 0 {
		t.Fatalf("expected at least 1 target for kilo/free")
	}
	if aliasTargets[0].ProviderName != "kilo" {
		t.Errorf("expected primary provider kilo, got %s", aliasTargets[0].ProviderName)
	}
	if aliasTargets[0].UpstreamModel != "kilo-auto/free" {
		t.Errorf("expected remapped model kilo-auto/free, got %s", aliasTargets[0].UpstreamModel)
	}

	// 3. Test IsActiveModel
	if !r.IsActiveModel("kilo", "kilo-auto/free") {
		t.Errorf("expected kilo-auto/free to be active on kilo")
	}
	if !r.IsActiveModel("kilo", "deepseek/deepseek-v4-flash-0731:free") {
		t.Errorf("expected deepseek/deepseek-v4-flash-0731:free to be active on kilo")
	}
	if !r.IsActiveModel("kilo", "nvidia/nemotron-3.5-lightning:free") {
		t.Errorf("expected nvidia/nemotron-3.5-lightning:free to be active on kilo")
	}
	if r.IsActiveModel("kilo", "deepseek-r1:free") {
		t.Errorf("expected decommissioned deepseek-r1:free to NOT be in active catalog")
	}

	// 4. Test GetAllActiveModels contains Kilo models
	allModels := r.GetAllActiveModels(context.Background())
	foundKiloAuto := false
	for _, m := range allModels {
		if m.Provider == "kilo" && m.ID == "kilo-auto/free" {
			foundKiloAuto = true
			break
		}
	}
	if !foundKiloAuto {
		t.Errorf("expected kilo/kilo-auto/free in GetAllActiveModels")
	}

	// 5. Test DispatchChat auto-remaps deprecated/alias model
	mockKilo := &mockProvider{
		name: "kilo",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "mock-kilo-id",
			Model:   "kilo-auto/free",
			Content: "kilo free response",
		},
	}
	r.SetProvider("kilo", mockKilo)

	req := &provider.UnifiedChatRequest{
		Model: "kilo/free",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "ping"},
		},
	}
	resp, winProv, err := r.DispatchChat(context.Background(), req, "")
	if err != nil {
		t.Fatalf("unexpected dispatch error: %v", err)
	}
	if winProv != "kilo" {
		t.Errorf("expected winning provider kilo, got %s", winProv)
	}
	if resp.Content != "kilo free response" {
		t.Errorf("expected content 'kilo free response', got %s", resp.Content)
	}
	if mockKilo.lastModel != "kilo-auto/free" {
		t.Errorf("expected provider to receive remapped model kilo-auto/free, got %s", mockKilo.lastModel)
	}
}

func TestRouterClineActiveModelResolutionAndRemapping(t *testing.T) {
	cfg := config.DefaultConfig()
	r := NewRouter(cfg)

	// 1. Test RemapClineModel
	tests := []struct {
		input    string
		expected string
	}{
		{"cline/deepseek-r1:free", "nvidia/nemotron-3.5-lightning:free"},
		{"deepseek-r1:free", "nvidia/nemotron-3.5-lightning:free"},
		{"meta-llama/llama-3.3-70b-instruct:free", "nvidia/nemotron-3.5-lightning:free"},
		{"qwen/qwen-2.5-72b-instruct:free", "qwen/qwen3.8-27b:free"},
		{"deepseek/deepseek-v4-flash-0731:free", "nvidia/nemotron-3.5-lightning:free"},
		{"deepseek-v4:free", "nvidia/nemotron-3.5-lightning:free"},
	}

	for _, tc := range tests {
		got, _ := RemapClineModel(tc.input)
		if got != tc.expected {
			t.Errorf("RemapClineModel(%q) = %q, expected %q", tc.input, got, tc.expected)
		}
	}

	// 2. Test ResolveTargets with cline prefix and remapping
	targets := r.ResolveTargets("cline/deepseek/deepseek-v4-flash-0731:free", "")
	if len(targets) == 0 {
		t.Fatalf("expected at least 1 target for cline/deepseek/deepseek-v4-flash-0731:free")
	}
	if targets[0].ProviderName != "cline" {
		t.Errorf("expected primary provider cline, got %s", targets[0].ProviderName)
	}
	if targets[0].UpstreamModel != "nvidia/nemotron-3.5-lightning:free" {
		t.Errorf("expected model nvidia/nemotron-3.5-lightning:free, got %s", targets[0].UpstreamModel)
	}

	aliasTargets := r.ResolveTargets("cline/deepseek-r1:free", "")
	if len(aliasTargets) == 0 {
		t.Fatalf("expected at least 1 target for cline/deepseek-r1:free")
	}
	if aliasTargets[0].ProviderName != "cline" {
		t.Errorf("expected primary provider cline, got %s", aliasTargets[0].ProviderName)
	}
	if aliasTargets[0].UpstreamModel != "nvidia/nemotron-3.5-lightning:free" {
		t.Errorf("expected remapped model nvidia/nemotron-3.5-lightning:free, got %s", aliasTargets[0].UpstreamModel)
	}

	// 3. Test IsActiveModel
	if !r.IsActiveModel("cline", "nvidia/nemotron-3.5-lightning:free") {
		t.Errorf("expected nvidia/nemotron-3.5-lightning:free to be active on cline")
	}
	if !r.IsActiveModel("cline", "google/gemma-4-31b-it:free") {
		t.Errorf("expected google/gemma-4-31b-it:free to be active on cline")
	}
	if !r.IsActiveModel("cline", "qwen/qwen3.8-27b:free") {
		t.Errorf("expected qwen/qwen3.8-27b:free to be active on cline")
	}
	if r.IsActiveModel("cline", "deepseek/deepseek-v4-flash-0731:free") {
		t.Errorf("expected deepseek/deepseek-v4-flash-0731:free to be inactive on cline")
	}

	// 4. Test GetAllActiveModels contains Cline models
	allModels := r.GetAllActiveModels(context.Background())
	foundCline := false
	for _, m := range allModels {
		if m.Provider == "cline" && m.ID == "nvidia/nemotron-3.5-lightning:free" {
			foundCline = true
			break
		}
	}
	if !foundCline {
		t.Errorf("expected cline/nvidia/nemotron-3.5-lightning:free in GetAllActiveModels")
	}

	// 5. Test DispatchChat auto-remaps alias model
	mockCline := &mockProvider{
		name: "cline",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "mock-cline-id",
			Model:   "nvidia/nemotron-3.5-lightning:free",
			Content: "cline free response",
		},
	}
	r.SetProvider("cline", mockCline)

	req := &provider.UnifiedChatRequest{
		Model: "cline/deepseek-r1:free",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "hello"},
		},
	}
	resp, winProv, err := r.DispatchChat(context.Background(), req, "")
	if err != nil {
		t.Fatalf("unexpected dispatch error: %v", err)
	}
	if winProv != "cline" {
		t.Errorf("expected winning provider cline, got %s", winProv)
	}
	if resp.Content != "cline free response" {
		t.Errorf("expected content 'cline free response', got %s", resp.Content)
	}
	if mockCline.lastModel != "nvidia/nemotron-3.5-lightning:free" {
		t.Errorf("expected provider to receive remapped model nvidia/nemotron-3.5-lightning:free, got %s", mockCline.lastModel)
	}
}

func TestRouterGetModelContextWindow(t *testing.T) {
	cfg := config.DefaultConfig()
	r := NewRouter(cfg)

	tests := []struct {
		provider string
		model    string
		expected int
	}{
		{"gemini", "gemini-3.8-flash", 1048576},
		{"openrouter", "deepseek/deepseek-v4-flash-0731:free", 1048576},
		{"openrouter", "nvidia/nemotron-3.5-lightning:free", 1000000},
		{"openrouter", "dots-studio/dots-3-note-preview:free", 512000},
		{"openrouter", "google/gemma-4-31b-it:free", 262144},
		{"openrouter", "qwen/qwen3.8-27b:free", 262144},
		{"openrouter", "cohere/north-mini-code:free", 256000},
		{"kilo", "kilo-auto/free", 256000},
		{"kilo", "deepseek/deepseek-v4-flash-0731:free", 1048576},
		{"cline", "nvidia/nemotron-3.5-lightning:free", 1000000},
		{"groq", "openai/gpt-oss-120b", 131072},
		{"groq", "qwen/qwen3.8-27b", 131042},
		{"anthropic", "claude-sonnet-5", 200000},
		{"openai", "gpt-4o", 128000},
	}

	for _, tc := range tests {
		cw := r.GetModelContextWindow(tc.provider, tc.model)
		if cw != tc.expected {
			t.Errorf("[%s/%s] expected context window %d, got %d", tc.provider, tc.model, tc.expected, cw)
		}
	}
}

func TestRouterHighContext256KFreePrioritization(t *testing.T) {
	cfg := config.DefaultConfig()
	r := NewRouter(cfg)

	mockGemini := &mockProvider{
		name: "gemini",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "gemini-resp",
			Content: "gemini 1M handled",
		},
	}
	mockAnthropic := &mockProvider{
		name: "anthropic",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "anthropic-resp",
			Content: "anthropic paid handled",
		},
	}
	mockOpenRouter := &mockProvider{
		name: "openrouter",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "openrouter-resp",
			Content: "openrouter free handled",
		},
	}

	r.SetProvider("gemini", mockGemini)
	r.SetProvider("anthropic", mockAnthropic)
	r.SetProvider("openrouter", mockOpenRouter)

	// Simulate a 200,000 token prompt (800,000 bytes) within Gemini Free Tier TPM limit (250K)
	largePayload := make([]byte, 800000)
	req := &provider.UnifiedChatRequest{
		Model:      "claude-sonnet-5",
		RawPayload: largePayload,
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "200k prompt"},
		},
	}

	resp, winProv, err := r.DispatchChat(context.Background(), req, "")
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	// Free Gemini 1M tier must win over Anthropic for 200K prompt
	if winProv != "gemini" {
		t.Errorf("expected winning provider gemini for 200K prompt, got %s", winProv)
	}
	if resp.Content != "gemini 1M handled" {
		t.Errorf("unexpected response content: %s", resp.Content)
	}
	if mockAnthropic.failCount > 0 {
		t.Errorf("anthropic was called unexpectedly for 200K prompt")
	}
}

func TestRouterHighContextPromptExceedingGeminiTPM(t *testing.T) {
	cfg := config.DefaultConfig()
	r := NewRouter(cfg)

	mockGemini := &mockProvider{
		name: "gemini",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "gemini-resp",
			Content: "gemini should be bypassed",
		},
	}
	mockOpenRouter := &mockProvider{
		name: "openrouter",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "openrouter-resp",
			Content: "openrouter 1M handled",
		},
	}

	r.SetProvider("gemini", mockGemini)
	r.SetProvider("openrouter", mockOpenRouter)

	// Simulate a 500,000 token prompt (2,000,000 bytes) exceeding Gemini Free Tier TPM limit (250K)
	hugePayload := make([]byte, 2000000)
	req := &provider.UnifiedChatRequest{
		Model:      "claude-sonnet-5",
		RawPayload: hugePayload,
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "500k prompt"},
		},
	}

	resp, winProv, err := r.DispatchChat(context.Background(), req, "")
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	// Gemini must be bypassed because prompt > 250K TPM, routing to OpenRouter 1M
	if winProv != "openrouter" {
		t.Errorf("expected winning provider openrouter for 500K prompt exceeding Gemini TPM, got %s", winProv)
	}
	if resp.Content != "openrouter 1M handled" {
		t.Errorf("unexpected response content: %s", resp.Content)
	}
}

func TestRouterHighContextFailoverAcrossFreeProviders(t *testing.T) {
	cfg := config.DefaultConfig()
	r := NewRouter(cfg)

	// Gemini fails (e.g. rate-limited 429)
	mockGemini := &mockProvider{
		name: "gemini",
		fail: true,
	}
	// OpenRouter succeeds with 1M model
	mockOpenRouter := &mockProvider{
		name: "openrouter",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "openrouter-1m",
			Content: "openrouter 1m free handled",
		},
	}
	mockAnthropic := &mockProvider{
		name: "anthropic",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "anthropic-paid",
			Content: "anthropic paid",
		},
	}

	r.SetProvider("gemini", mockGemini)
	r.SetProvider("openrouter", mockOpenRouter)
	r.SetProvider("anthropic", mockAnthropic)

	// 256,000 token prompt
	largePayload := make([]byte, 1024000)
	req := &provider.UnifiedChatRequest{
		Model:      "claude-sonnet-5",
		RawPayload: largePayload,
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "256k prompt with failover"},
		},
	}

	resp, winProv, err := r.DispatchChat(context.Background(), req, "")
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	// OpenRouter 1M should succeed on Gemini failover without hitting Anthropic
	if winProv != "openrouter" {
		t.Errorf("expected openrouter to win on Gemini failover, got %s", winProv)
	}
	if resp.Content != "openrouter 1m free handled" {
		t.Errorf("unexpected content: %s", resp.Content)
	}
	if mockAnthropic.failCount > 0 {
		t.Errorf("anthropic was called unexpectedly when openrouter 1M free was available")
	}
}

func TestRouterEmptyResponseTriggersFailover(t *testing.T) {
	cfg := config.DefaultConfig()
	r := NewRouter(cfg)

	// First provider returns HTTP 200 with empty text and no tool calls
	mockSilentFail := &mockProvider{
		name: "groq",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "groq-empty",
			Content: "",
		},
	}
	// Second provider returns valid text
	mockSuccess := &mockProvider{
		name: "gemini",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "gemini-valid",
			Content: "hello from gemini",
		},
	}

	r.SetProvider("groq", mockSilentFail)
	r.SetProvider("gemini", mockSuccess)

	req := &provider.UnifiedChatRequest{
		Model: "claude-sonnet-5",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "hello"},
		},
	}

	resp, winProv, err := r.DispatchChat(context.Background(), req, "")
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	if winProv != "gemini" {
		t.Errorf("expected failover to gemini when groq returns empty content, got %s", winProv)
	}
	if resp.Content != "hello from gemini" {
		t.Errorf("unexpected content: %s", resp.Content)
	}
}

func TestTranslatorOpenAIToAnthropic_DSMLToolCall(t *testing.T) {
	tr := NewTranslator()
	dsmlContent := `I will check the repo status.
<｜DSML｜tool_calls>
<｜DSML｜invoke name="Bash">
<｜DSML｜parameter name="command" string="true">git status</｜DSML｜parameter>
<｜DSML｜parameter name="description" string="true">Check repo status</｜DSML｜parameter>
</｜DSML｜invoke>
</｜DSML｜tool_calls>`

	oaiResp := &provider.UnifiedChatResponse{
		ID:           "test-dsml-call",
		Content:      dsmlContent,
		FinishReason: "stop",
		Usage: provider.UnifiedUsage{
			PromptTokens:     500,
			CompletionTokens: 40,
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

	textBlock := contentArr[0].(map[string]interface{})
	if textBlock["text"] != "I will check the repo status." {
		t.Errorf("unexpected text content: %v", textBlock["text"])
	}

	toolBlock := contentArr[1].(map[string]interface{})
	if toolBlock["type"] != "tool_use" || toolBlock["name"] != "Bash" {
		t.Errorf("expected tool_use for Bash, got %v", toolBlock)
	}
	inputMap, ok := toolBlock["input"].(map[string]interface{})
	if !ok || inputMap["command"] != "git status" || inputMap["description"] != "Check repo status" {
		t.Errorf("expected command and description in tool input, got %v", inputMap)
	}
}

func TestTranslatorOpenAIToAnthropic_OrphanDSMLStripping(t *testing.T) {
	rawOrphan := ` <｜DSML｜parameter name="description" string="true">Check repo location, recent commits, branch, and status</｜DSML｜parameter>
</｜DSML｜invoke>
</｜DSML｜tool_calls>`

	cleanText, toolCalls := ExtractTextToolCalls(rawOrphan)
	if cleanText != "" {
		t.Errorf("expected cleanText to be empty after stripping orphan DSML, got %q", cleanText)
	}
	if len(toolCalls) != 0 {
		t.Errorf("expected 0 tool calls from orphan DSML markup, got %d", len(toolCalls))
	}
}

func TestRouter_DSMLOrphanFailover(t *testing.T) {
	cfg := config.DefaultConfig()
	r := NewRouter(cfg)

	rawOrphan := ` <｜DSML｜parameter name="description" string="true">Check repo location, recent commits, branch, and status</｜DSML｜parameter>
</｜DSML｜invoke>
</｜DSML｜tool_calls>`

	// First provider returns orphan DSML
	mockDSMLFail := &mockProvider{
		name: "openrouter",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "or-dsml-fail",
			Content: rawOrphan,
		},
	}
	// Second provider returns valid response
	mockValid := &mockProvider{
		name: "gemini",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "gemini-recovered",
			Content: "Recovered successfully from DSML corruption",
		},
	}

	r.SetProvider("openrouter", mockDSMLFail)
	r.SetProvider("gemini", mockValid)

	req := &provider.UnifiedChatRequest{
		Model: "claude-sonnet-5",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "status report"},
		},
	}

	resp, winProv, err := r.DispatchChat(context.Background(), req, "")
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	if winProv != "gemini" {
		t.Errorf("expected failover to gemini, got %s", winProv)
	}
	if resp.Content != "Recovered successfully from DSML corruption" {
		t.Errorf("unexpected content: %s", resp.Content)
	}
}

func TestRouter_IsRepetitionLoop(t *testing.T) {
	bashCall := []provider.UnifiedToolCall{
		{
			ID:   "call_bash_1",
			Type: "function",
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{
				Name:      "Bash",
				Arguments: `{"command": "cd /path/to/example && ls Config/"}`,
			},
		},
	}

	diffBashCall := []provider.UnifiedToolCall{
		{
			ID:   "call_bash_2",
			Type: "function",
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{
				Name:      "Bash",
				Arguments: `{"command": "cd /path/to/example && ls App/"}`,
			},
		},
	}

	t.Run("ExactMatchWithToolCall", func(t *testing.T) {
		req := &provider.UnifiedChatRequest{
			Messages: []provider.UnifiedChatMessage{
				{Role: "user", Content: "investigate repo structure"},
				{
					Role:      "assistant",
					Content:   "Let me explore the project structure more carefully.",
					ToolCalls: bashCall,
				},
				{
					Role:    "tool",
					Content: "Exit code 2\nls: Config/: No such file or directory",
				},
			},
		}
		resp := &provider.UnifiedChatResponse{
			Content:   "Let me explore the project structure more carefully.",
			ToolCalls: bashCall,
		}
		if !isRepetitionLoop(req, resp) {
			t.Errorf("expected isRepetitionLoop to return true for identical text and tool calls")
		}
	})

	t.Run("IdenticalToolCallAfterErrorWithDifferentText", func(t *testing.T) {
		req := &provider.UnifiedChatRequest{
			Messages: []provider.UnifiedChatMessage{
				{Role: "user", Content: "investigate repo structure"},
				{
					Role:      "assistant",
					Content:   "Let me explore the project structure.",
					ToolCalls: bashCall,
				},
				{
					Role:    "tool",
					Content: "Exit code 2\nls: Config/: No such file or directory",
				},
			},
		}
		resp := &provider.UnifiedChatResponse{
			Content:   "Trying to run the command again:",
			ToolCalls: bashCall,
		}
		if !isRepetitionLoop(req, resp) {
			t.Errorf("expected isRepetitionLoop to return true for identical tool call after tool error")
		}
	})

	t.Run("HumanInterventionBypassesLoop", func(t *testing.T) {
		req := &provider.UnifiedChatRequest{
			Messages: []provider.UnifiedChatMessage{
				{Role: "user", Content: "investigate repo structure"},
				{
					Role:      "assistant",
					Content:   "Let me explore the project structure more carefully.",
					ToolCalls: bashCall,
				},
				{
					Role:    "tool",
					Content: "Exit code 2\nls: Config/: No such file or directory",
				},
				{
					Role:    "user",
					Content: "Please run the exact same command again anyway.",
				},
			},
		}
		resp := &provider.UnifiedChatResponse{
			Content:   "Let me explore the project structure more carefully.",
			ToolCalls: bashCall,
		}
		if isRepetitionLoop(req, resp) {
			t.Errorf("expected isRepetitionLoop to return false when a human user prompts between turns")
		}
	})

	t.Run("DifferentArgumentsAllowed", func(t *testing.T) {
		req := &provider.UnifiedChatRequest{
			Messages: []provider.UnifiedChatMessage{
				{Role: "user", Content: "investigate repo structure"},
				{
					Role:      "assistant",
					Content:   "Let me explore the project structure more carefully.",
					ToolCalls: bashCall,
				},
				{
					Role:    "tool",
					Content: "Exit code 2\nls: Config/: No such file or directory",
				},
			},
		}
		resp := &provider.UnifiedChatResponse{
			Content:   "Config does not exist, let me explore App directory.",
			ToolCalls: diffBashCall,
		}
		if isRepetitionLoop(req, resp) {
			t.Errorf("expected isRepetitionLoop to return false when tool call arguments differ")
		}
	})

	t.Run("TextOnlyLoopInAgent", func(t *testing.T) {
		req := &provider.UnifiedChatRequest{
			Messages: []provider.UnifiedChatMessage{
				{Role: "user", Content: "start investigation"},
				{
					Role:    "assistant",
					Content: "Let me explore the project structure more carefully.",
				},
				{
					Role:    "tool",
					Content: "done",
				},
			},
		}
		resp := &provider.UnifiedChatResponse{
			Content: "Let me explore the project structure more carefully.",
		}
		if !isRepetitionLoop(req, resp) {
			t.Errorf("expected isRepetitionLoop to return true for repeated text in autonomous tool loop")
		}
	})

	t.Run("ThreeTurnIdenticalToolCall", func(t *testing.T) {
		req := &provider.UnifiedChatRequest{
			Messages: []provider.UnifiedChatMessage{
				{Role: "user", Content: "status check"},
				{
					Role:      "assistant",
					Content:   "Checking status 1",
					ToolCalls: bashCall,
				},
				{
					Role:    "tool",
					Content: "ok",
				},
				{
					Role:      "assistant",
					Content:   "Checking status 2",
					ToolCalls: bashCall,
				},
				{
					Role:    "tool",
					Content: "ok",
				},
			},
		}
		resp := &provider.UnifiedChatResponse{
			Content:   "Checking status 3",
			ToolCalls: bashCall,
		}
		if !isRepetitionLoop(req, resp) {
			t.Errorf("expected isRepetitionLoop to return true for 3rd identical tool invocation in history")
		}
	})
}

func TestRouter_RepetitionLoopFailover(t *testing.T) {
	cfg := config.DefaultConfig()
	r := NewRouter(cfg)

	bashCall := []provider.UnifiedToolCall{
		{
			ID:   "call_bash_fail",
			Type: "function",
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{
				Name:      "Bash",
				Arguments: `{"command": "cd /path/to/example && ls Config/"}`,
			},
		},
	}

	// First provider is stuck repeating identical text & tool call
	mockLoopingProvider := &mockProvider{
		name: "openrouter",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:        "or-loop-resp",
			Content:   "Let me explore the project structure more carefully.",
			ToolCalls: bashCall,
		},
	}

	// Second provider breaks out with a different, constructive command
	mockRecoveredProvider := &mockProvider{
		name: "gemini",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "gemini-break-resp",
			Content: "Let me inspect the App directory instead since Config does not exist.",
			ToolCalls: []provider.UnifiedToolCall{
				{
					ID:   "call_bash_ok",
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{
						Name:      "Bash",
						Arguments: `{"command": "cd /path/to/example && ls App/"}`,
					},
				},
			},
		},
	}

	r.SetProvider("openrouter", mockLoopingProvider)
	r.SetProvider("gemini", mockRecoveredProvider)

	req := &provider.UnifiedChatRequest{
		Model: "claude-sonnet-5",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "help explore repository"},
			{
				Role:      "assistant",
				Content:   "Let me explore the project structure more carefully.",
				ToolCalls: bashCall,
			},
			{
				Role:    "tool",
				Content: "Exit code 2\nls: Config/: No such file or directory",
			},
		},
	}

	resp, winProv, err := r.DispatchChat(context.Background(), req, "")
	if err != nil {
		t.Fatalf("dispatch failed unexpectedly: %v", err)
	}

	if winProv != "gemini" {
		t.Errorf("expected failover to gemini to break repetition loop, got %s", winProv)
	}
	if !strings.Contains(resp.Content, "App directory") {
		t.Errorf("unexpected content from recovered provider: %s", resp.Content)
	}
}


func TestRouterThinkingOnlyResponseTriggersFailover(t *testing.T) {
	r := NewRouter(config.DefaultConfig())
	r.SetProvider("groq", &mockProvider{
		name:     "groq",
		response: &provider.UnifiedChatResponse{ID: "groq-think-only", Content: "<think>I should edit the file next.</think>"},
	})
	r.SetProvider("gemini", &mockProvider{
		name:     "gemini",
		response: &provider.UnifiedChatResponse{ID: "gemini-valid", Content: "hello from gemini"},
	})

	req := &provider.UnifiedChatRequest{
		Model:    "claude-sonnet-5",
		Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "hello"}},
	}
	_, winProv, err := r.DispatchChat(context.Background(), req, "")
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if winProv != "gemini" {
		t.Errorf("expected failover when the only output is a <think> block, got %s", winProv)
	}
}

func TestRouterEmptyExitPlanModeTriggersFailover(t *testing.T) {
	r := NewRouter(config.DefaultConfig())
	r.SetProvider("groq", &mockProvider{
		name: "groq",
		response: &provider.UnifiedChatResponse{
			ID:           "groq-empty-exit",
			FinishReason: "tool_calls",
			ToolCalls:    []provider.UnifiedToolCall{makeToolCall("call_exit", "ExitPlanMode", `{}`)},
		},
	})
	r.SetProvider("gemini", &mockProvider{
		name:     "gemini",
		response: &provider.UnifiedChatResponse{ID: "gemini-plan", Content: "# Plan\n1. Add adapter"},
	})

	_, winProv, err := r.DispatchChat(context.Background(), planModeRequest(), "")
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if winProv != "gemini" {
		t.Errorf("expected failover for ExitPlanMode with no plan text and no plan file, got %s", winProv)
	}
}

func TestRouter_IsRepetitionLoop_SystemReminderIsNotHumanInput(t *testing.T) {
	editCall := []provider.UnifiedToolCall{makeToolCall("call_edit_1", "Edit",
		`{"file_path":"internal/config/default.go","old_string":"\t\t\t\tMistral","new_string":"\t\t\t\tOmniRoute"}`)}

	req := &provider.UnifiedChatRequest{
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "remove mistral"},
			{Role: "assistant", ToolCalls: editCall},
			{Role: "tool", Content: "<tool_use_error>String to replace not found in file.</tool_use_error>"},
			{Role: "user", Content: "<system-reminder>\nThe user hasn't heard from you in a while.\n</system-reminder>"},
		},
	}
	resp := &provider.UnifiedChatResponse{ToolCalls: editCall}
	if !isRepetitionLoop(req, resp) {
		t.Errorf("a Claude Code <system-reminder> must not count as human input and disable loop detection")
	}

	req.Messages[3].Content = "try that edit again please"
	if isRepetitionLoop(req, resp) {
		t.Errorf("a real human message must still exempt a repeated call")
	}
}

func declaredTools(names ...string) []interface{} {
	tools := make([]interface{}, 0, len(names))
	for i, n := range names {
		if i%2 == 0 {
			// Anthropic tool schema
			tools = append(tools, map[string]interface{}{"name": n, "input_schema": map[string]interface{}{"type": "object"}})
		} else {
			// OpenAI function schema
			tools = append(tools, map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": n}})
		}
	}
	return tools
}

func TestReconcileToolNames(t *testing.T) {
	tools := declaredTools("Glob", "Read", "ExitPlanMode", "mcp__liltok__liltok_ask")

	calls := []provider.UnifiedToolCall{
		makeToolCall("c1", "glob", `{}`),
		makeToolCall("c2", "exit_plan_mode", `{}`),
		makeToolCall("c3", "Read", `{}`),
		makeToolCall("c4", "mcp__liltok__liltok_ask", `{}`),
	}
	if bad := reconcileToolNames(tools, calls); bad != "" {
		t.Fatalf("expected all calls to reconcile, got undeclared %q", bad)
	}
	for i, want := range []string{"Glob", "ExitPlanMode", "Read", "mcp__liltok__liltok_ask"} {
		if calls[i].Function.Name != want {
			t.Errorf("call %d: expected name %q, got %q", i, want, calls[i].Function.Name)
		}
	}

	if bad := reconcileToolNames(tools, []provider.UnifiedToolCall{makeToolCall("c5", "Global", `{}`)}); bad != "Global" {
		t.Errorf("expected Global to be reported as undeclared, got %q", bad)
	}

	if bad := reconcileToolNames(nil, []provider.UnifiedToolCall{makeToolCall("c6", "Anything", `{}`)}); bad != "" {
		t.Errorf("requests without declared tools must not be checked, got %q", bad)
	}
}

func TestRouterUndeclaredToolTriggersFailover(t *testing.T) {
	r := NewRouter(config.DefaultConfig())
	r.SetProvider("groq", &mockProvider{
		name: "groq",
		response: &provider.UnifiedChatResponse{
			ID:           "groq-bad-tool",
			FinishReason: "tool_calls",
			ToolCalls:    []provider.UnifiedToolCall{makeToolCall("c1", "Global", `{"pattern":"**/*.go"}`)},
		},
	})
	r.SetProvider("gemini", &mockProvider{
		name: "gemini",
		response: &provider.UnifiedChatResponse{
			ID:           "gemini-good-tool",
			FinishReason: "tool_calls",
			ToolCalls:    []provider.UnifiedToolCall{makeToolCall("c2", "Glob", `{"pattern":"**/*.go"}`)},
		},
	})

	req := &provider.UnifiedChatRequest{
		Model:    "claude-sonnet-5",
		Tools:    declaredTools("Glob", "Read"),
		Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "find go files"}},
	}
	resp, winProv, err := r.DispatchChat(context.Background(), req, "")
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if winProv != "gemini" || resp.ToolCalls[0].Function.Name != "Glob" {
		t.Errorf("expected failover to a model calling a declared tool, got %s calling %s", winProv, resp.ToolCalls[0].Function.Name)
	}
}

// hangingProvider blocks until its context ends, like an upstream that accepted the request and never replied.
type hangingProvider struct {
	name  string
	calls int
}

func (h *hangingProvider) Name() string                                  { return h.name }
func (h *hangingProvider) Tier() provider.ProviderTier                   { return provider.TierFree }
func (h *hangingProvider) CheckHealth(ctx context.Context) (bool, error) { return true, nil }
func (h *hangingProvider) SendChat(ctx context.Context, req *provider.UnifiedChatRequest) (*provider.UnifiedChatResponse, error) {
	h.calls++
	<-ctx.Done()
	return nil, fmt.Errorf("upstream request failed: Post \"https://example.invalid/v1/chat/completions\": %w", ctx.Err())
}
func (h *hangingProvider) StreamChat(ctx context.Context, req *provider.UnifiedChatRequest) (<-chan provider.UnifiedSSEEvent, <-chan error, error) {
	return nil, nil, errors.New("not implemented")
}

func TestRouterAttemptTimeoutFailsOverFromHungProvider(t *testing.T) {
	r := NewRouter(config.DefaultConfig())
	r.attemptTimeout = 50 * time.Millisecond

	r.SetProvider("anthropic", &mockProvider{name: "anthropic", fail: true})
	hung := &hangingProvider{name: "groq"}
	r.SetProvider("groq", hung)
	r.SetProvider("gemini", &mockProvider{
		name:     "gemini",
		response: &provider.UnifiedChatResponse{ID: "gemini-ok", Content: "hello from gemini"},
	})

	req := &provider.UnifiedChatRequest{
		Model:    "claude-sonnet-5",
		Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "hello"}},
	}

	start := time.Now()
	_, winProv, err := r.DispatchChat(context.Background(), req, "")
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if winProv != "gemini" {
		t.Errorf("expected failover to gemini after groq attempts time out, got %s", winProv)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("per-attempt timeout not applied: dispatch took %s", elapsed)
	}
	if hung.calls == 0 {
		t.Errorf("expected the hung provider to be attempted")
	}
}

func TestRouterStopsFailoverWhenClientCancels(t *testing.T) {
	r := NewRouter(config.DefaultConfig())
	r.attemptTimeout = 0

	r.SetProvider("anthropic", &mockProvider{name: "anthropic", fail: true})
	hung := &hangingProvider{name: "groq"}
	next := &mockProvider{name: "gemini", response: &provider.UnifiedChatResponse{ID: "gemini-ok", Content: "late"}}
	r.SetProvider("groq", hung)
	r.SetProvider("gemini", next)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	req := &provider.UnifiedChatRequest{
		Model:    "claude-sonnet-5",
		Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "hello"}},
	}
	_, _, err := r.DispatchChat(ctx, req, "")
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected a context.Canceled error once the client disconnects, got %v", err)
	}
	if hung.calls != 1 {
		t.Errorf("expected exactly one attempt before the cancel, got %d", hung.calls)
	}
	if next.lastModel != "" {
		t.Errorf("failover continued after the client canceled (gemini was called with %q)", next.lastModel)
	}
	if cb, ok := r.GetBreaker("groq/qwen/qwen3.8-27b"); ok {
		if state, failures := cb.State(); state != StateClosed || failures != 0 {
			t.Errorf("a client cancel must not count as a provider failure, breaker is %v with %d failures", state, failures)
		}
	}
}

func TestIsCircuitBreakerError_IgnoresCancellation(t *testing.T) {
	wrapped := fmt.Errorf("upstream request failed: Post \"https://api.cline.bot/api/v1/chat/completions\": %w", context.Canceled)
	if isCircuitBreakerError(wrapped) {
		t.Errorf("context.Canceled must not count against a provider's circuit breaker")
	}
	if isCircuitBreakerError(errors.New(`upstream request failed: Post "https://x": context canceled`)) {
		t.Errorf("unwrapped 'context canceled' text must not count against a provider's circuit breaker")
	}
	if !isCircuitBreakerError(errors.New("upstream error status 503")) {
		t.Errorf("real upstream failures must still count")
	}
}
