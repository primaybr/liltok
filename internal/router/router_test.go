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
Tool Call: SendMessage({"message":"Continue where you left off. Run the tests to confirm they pass, run the full ProductMatching suite, commit per Step 8 of the brief with the required Co-Authored-By trailer, write the full report to F:\laragon\www\carikno\.superpowers\sdd\2026-09-17-product-matching-overhaul-phase-4\task-1-report.md, and reply with your final status.","summary":"Resume to finish tests, commit, and report","to":"a139b43b450f80250"})`

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

func TestRouterMistralActiveModelResolutionAndRemapping(t *testing.T) {
	cfg := config.DefaultConfig()
	r := NewRouter(cfg)

	// 1. Test RemapMistralModel
	tests := []struct {
		input    string
		expected string
	}{
		{"mistral/codestral", "codestral-latest"},
		{"codestral", "codestral-latest"},
		{"mistral/ministral-8b", "ministral-8b-latest"},
		{"mistral/ministral-3b", "ministral-3b-latest"},
		{"mistral/mistral-small-latest", "ministral-8b-latest"},
		{"mistral/codestral-latest", "codestral-latest"},
		{"ministral-14b-latest", "ministral-14b-latest"},
	}

	for _, tc := range tests {
		got, _ := RemapMistralModel(tc.input)
		if got != tc.expected {
			t.Errorf("RemapMistralModel(%q) = %q, expected %q", tc.input, got, tc.expected)
		}
	}

	// 2. Test ResolveTargets with mistral prefix
	targets := r.ResolveTargets("mistral/codestral-latest", "")
	if len(targets) == 0 {
		t.Fatalf("expected at least 1 target for mistral/codestral-latest")
	}
	if targets[0].ProviderName != "mistral" {
		t.Errorf("expected primary provider mistral, got %s", targets[0].ProviderName)
	}
	if targets[0].UpstreamModel != "codestral-latest" {
		t.Errorf("expected model codestral-latest, got %s", targets[0].UpstreamModel)
	}

	aliasTargets := r.ResolveTargets("mistral/codestral", "")
	if len(aliasTargets) == 0 {
		t.Fatalf("expected at least 1 target for mistral/codestral")
	}
	if aliasTargets[0].ProviderName != "mistral" {
		t.Errorf("expected primary provider mistral, got %s", aliasTargets[0].ProviderName)
	}
	if aliasTargets[0].UpstreamModel != "codestral-latest" {
		t.Errorf("expected remapped model codestral-latest, got %s", aliasTargets[0].UpstreamModel)
	}

	// 3. Test IsActiveModel
	if !r.IsActiveModel("mistral", "codestral-latest") {
		t.Errorf("expected codestral-latest to be active on mistral")
	}
	if !r.IsActiveModel("mistral", "ministral-8b-latest") {
		t.Errorf("expected ministral-8b-latest to be active on mistral")
	}
	if !r.IsActiveModel("mistral", "ministral-3b-latest") {
		t.Errorf("expected ministral-3b-latest to be active on mistral")
	}
	if r.IsActiveModel("mistral", "mistral-small-latest") {
		t.Errorf("expected rate-limited mistral-small-latest to NOT be in active catalog")
	}

	// 4. Test GetAllActiveModels contains Mistral models
	allModels := r.GetAllActiveModels(context.Background())
	foundCodestral := false
	for _, m := range allModels {
		if m.Provider == "mistral" && m.ID == "codestral-latest" {
			foundCodestral = true
			break
		}
	}
	if !foundCodestral {
		t.Errorf("expected mistral/codestral-latest in GetAllActiveModels")
	}

	// 5. Test DispatchChat auto-remaps alias model
	mockMistral := &mockProvider{
		name: "mistral",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "mock-mistral-id",
			Model:   "codestral-latest",
			Content: "mistral code response",
		},
	}
	r.SetProvider("mistral", mockMistral)

	req := &provider.UnifiedChatRequest{
		Model: "mistral/codestral",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "write a function"},
		},
	}
	resp, winProv, err := r.DispatchChat(context.Background(), req, "")
	if err != nil {
		t.Fatalf("unexpected dispatch error: %v", err)
	}
	if winProv != "mistral" {
		t.Errorf("expected winning provider mistral, got %s", winProv)
	}
	if resp.Content != "mistral code response" {
		t.Errorf("expected content 'mistral code response', got %s", resp.Content)
	}
	if mockMistral.lastModel != "codestral-latest" {
		t.Errorf("expected provider to receive remapped model codestral-latest, got %s", mockMistral.lastModel)
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
		{"cline/deepseek-r1:free", "deepseek/deepseek-v4-flash-0731:free"},
		{"deepseek-r1:free", "deepseek/deepseek-v4-flash-0731:free"},
		{"meta-llama/llama-3.3-70b-instruct:free", "nvidia/nemotron-3.5-lightning:free"},
		{"qwen/qwen-2.5-72b-instruct:free", "qwen/qwen3.8-27b:free"},
		{"deepseek/deepseek-v4-flash-0731:free", "deepseek/deepseek-v4-flash-0731:free"},
	}

	for _, tc := range tests {
		got, _ := RemapClineModel(tc.input)
		if got != tc.expected {
			t.Errorf("RemapClineModel(%q) = %q, expected %q", tc.input, got, tc.expected)
		}
	}

	// 2. Test ResolveTargets with cline prefix
	targets := r.ResolveTargets("cline/deepseek/deepseek-v4-flash-0731:free", "")
	if len(targets) == 0 {
		t.Fatalf("expected at least 1 target for cline/deepseek/deepseek-v4-flash-0731:free")
	}
	if targets[0].ProviderName != "cline" {
		t.Errorf("expected primary provider cline, got %s", targets[0].ProviderName)
	}
	if targets[0].UpstreamModel != "deepseek/deepseek-v4-flash-0731:free" {
		t.Errorf("expected model deepseek/deepseek-v4-flash-0731:free, got %s", targets[0].UpstreamModel)
	}

	aliasTargets := r.ResolveTargets("cline/deepseek-r1:free", "")
	if len(aliasTargets) == 0 {
		t.Fatalf("expected at least 1 target for cline/deepseek-r1:free")
	}
	if aliasTargets[0].ProviderName != "cline" {
		t.Errorf("expected primary provider cline, got %s", aliasTargets[0].ProviderName)
	}
	if aliasTargets[0].UpstreamModel != "deepseek/deepseek-v4-flash-0731:free" {
		t.Errorf("expected remapped model deepseek/deepseek-v4-flash-0731:free, got %s", aliasTargets[0].UpstreamModel)
	}

	// 3. Test IsActiveModel
	if !r.IsActiveModel("cline", "deepseek/deepseek-v4-flash-0731:free") {
		t.Errorf("expected deepseek/deepseek-v4-flash-0731:free to be active on cline")
	}
	if !r.IsActiveModel("cline", "qwen/qwen3.8-27b:free") {
		t.Errorf("expected qwen/qwen3.8-27b:free to be active on cline")
	}

	// 4. Test GetAllActiveModels contains Cline models
	allModels := r.GetAllActiveModels(context.Background())
	foundCline := false
	for _, m := range allModels {
		if m.Provider == "cline" && m.ID == "deepseek/deepseek-v4-flash-0731:free" {
			foundCline = true
			break
		}
	}
	if !foundCline {
		t.Errorf("expected cline/deepseek/deepseek-v4-flash-0731:free in GetAllActiveModels")
	}

	// 5. Test DispatchChat auto-remaps alias model
	mockCline := &mockProvider{
		name: "cline",
		fail: false,
		response: &provider.UnifiedChatResponse{
			ID:      "mock-cline-id",
			Model:   "deepseek/deepseek-v4-flash-0731:free",
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
	if mockCline.lastModel != "deepseek/deepseek-v4-flash-0731:free" {
		t.Errorf("expected provider to receive remapped model deepseek/deepseek-v4-flash-0731:free, got %s", mockCline.lastModel)
	}
}

