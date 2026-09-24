package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/primaybr/liltok/internal/provider"
)

// Adapter implements provider.ProviderClient for any OpenAI-compatible API (OpenAI, NVIDIA NIM, Groq, DeepSeek, Ollama).
type Adapter struct {
	name       string
	tier       provider.ProviderTier
	baseURL    string
	apiKey     string
	mu         sync.RWMutex
	httpClient *http.Client
}

// NewAdapter creates an OpenAI-compatible adapter.
func NewAdapter(name string, tier provider.ProviderTier, baseURL, apiKey string) *Adapter {
	baseURL = strings.TrimRight(baseURL, "/")
	return &Adapter{
		name:    name,
		tier:    tier,
		baseURL: baseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// SetAPIKey updates the API key at runtime.
func (a *Adapter) SetAPIKey(apiKey string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.apiKey = apiKey
}

// SetBaseURL updates the base URL at runtime.
func (a *Adapter) SetBaseURL(baseURL string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.baseURL = strings.TrimRight(baseURL, "/")
}

// NewNVIDIANIMAdapter creates an adapter for NVIDIA NIM's free endpoints.
func NewNVIDIANIMAdapter(apiKey string) *Adapter {
	return NewAdapter("nvidianim", provider.TierFree, "https://integrate.api.nvidia.com/v1", apiKey)
}

// NewGroqAdapter creates an adapter for Groq's high-speed free endpoints.
func NewGroqAdapter(apiKey string) *Adapter {
	return NewAdapter("groq", provider.TierFree, "https://api.groq.com/openai/v1", apiKey)
}

// NewOpenRouterAdapter creates an adapter for OpenRouter's free and routing endpoints.
func NewOpenRouterAdapter(apiKey, baseURL string) *Adapter {
	if baseURL == "" {
		baseURL = "https://openrouter.ai/api/v1"
	}
	return NewAdapter("openrouter", provider.TierFree, baseURL, apiKey)
}

// NewOllamaAdapter creates an adapter for local offline Ollama endpoints.
func NewOllamaAdapter(baseURL string) *Adapter {
	if baseURL == "" {
		baseURL = "http://localhost:11434/v1"
	}
	return NewAdapter("ollama", provider.TierFree, baseURL, "")
}

// NewKiloAdapter creates an adapter for Kilo AI Gateway's free models.
func NewKiloAdapter(apiKey, baseURL string) *Adapter {
	if baseURL == "" {
		baseURL = "https://api.kilo.ai/api/gateway"
	}
	return NewAdapter("kilo", provider.TierFree, baseURL, apiKey)
}

// NewClineAdapter creates an adapter for Cline's free and reasoning models.
func NewClineAdapter(apiKey, baseURL string) *Adapter {
	if baseURL == "" {
		baseURL = "https://api.cline.bot/api/v1"
	}
	return NewAdapter("cline", provider.TierFree, baseURL, apiKey)
}

func (a *Adapter) Name() string {
	return a.name
}

func (a *Adapter) Tier() provider.ProviderTier {
	return a.tier
}

func (a *Adapter) CheckHealth(ctx context.Context) (bool, error) {
	a.mu.RLock()
	baseURL := a.baseURL
	apiKey := a.apiKey
	name := a.name
	a.mu.RUnlock()

	if name != "ollama" && name != "kilo" && apiKey == "" {
		return false, fmt.Errorf("%s api key is not configured", name)
	}

	if name == "ollama" && (baseURL == "" || baseURL == "disabled") {
		return false, fmt.Errorf("ollama local engine is disabled")
	}

	url := baseURL + "/models"
	if name == "ollama" && !strings.HasSuffix(baseURL, "/v1") {
		url = baseURL + "/api/tags"
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if name == "openrouter" {
		req.Header.Set("HTTP-Referer", "https://github.com/primaybr/liltok")
		req.Header.Set("X-Title", "liltok")
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("%s connection failed: %w", name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBytes, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("%s returned status %d: %s", name, resp.StatusCode, string(respBytes))
	}
	return true, nil
}

// ListModels queries the provider's models endpoint and returns active models.
func (a *Adapter) ListModels(ctx context.Context) ([]provider.ModelInfo, error) {
	a.mu.RLock()
	baseURL := a.baseURL
	apiKey := a.apiKey
	name := a.name
	a.mu.RUnlock()

	if name != "ollama" && name != "kilo" && apiKey == "" {
		return nil, fmt.Errorf("%s api key is not configured", name)
	}

	if name == "ollama" && (baseURL == "" || baseURL == "disabled") {
		return nil, fmt.Errorf("ollama local engine is disabled")
	}

	url := baseURL + "/models"
	isOllamaTags := name == "ollama" && !strings.HasSuffix(baseURL, "/v1")
	if isOllamaTags {
		url = baseURL + "/api/tags"
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if name == "openrouter" {
		req.Header.Set("HTTP-Referer", "https://github.com/primaybr/liltok")
		req.Header.Set("X-Title", "liltok")
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s list models failed: %w", name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%s returned status %d: %s", name, resp.StatusCode, string(respBytes))
	}

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var results []provider.ModelInfo

	if isOllamaTags {
		var ollamaResp struct {
			Models []struct {
				Name  string `json:"name"`
				Model string `json:"model"`
			} `json:"models"`
		}
		if err := json.Unmarshal(respBytes, &ollamaResp); err != nil {
			return nil, fmt.Errorf("failed to decode ollama tags: %w", err)
		}
		for _, m := range ollamaResp.Models {
			id := m.Name
			if id == "" {
				id = m.Model
			}
			if id != "" {
				results = append(results, provider.ModelInfo{
					ID:       id,
					Provider: name,
					Active:   true,
				})
			}
		}
		return results, nil
	}

	var openAIResp struct {
		Data []struct {
			ID               string `json:"id"`
			Active           *bool  `json:"active"`
			ContextWindow    int    `json:"context_window"`
			ContextLength    int    `json:"context_length"`
			MaxContextLength int    `json:"max_context_length"`
			OwnedBy          string `json:"owned_by"`
			Capabilities     struct {
				CompletionChat bool `json:"completion_chat"`
				Reasoning      bool `json:"reasoning"`
			} `json:"capabilities"`
			Pricing struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
			Architecture struct {
				OutputModalities []string `json:"output_modalities"`
			} `json:"architecture"`
			SupportedParameters []string `json:"supported_parameters"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBytes, &openAIResp); err != nil {
		return nil, fmt.Errorf("failed to decode models list: %w", err)
	}

	for _, item := range openAIResp.Data {
		isActive := true
		if item.Active != nil {
			isActive = *item.Active
		}
		// Strict active check: ignore non-active models
		if !isActive {
			continue
		}

		// NVIDIA NIM filtering: exclude non-chat/embeddings and known decommissioned/404 models
		if name == "nvidianim" && !isNVIDIANIMChatModel(item.ID) {
			continue
		}

		// OpenRouter filtering: strictly retain free chat and reasoning models (exclude paid, audio, safety guards)
		if name == "openrouter" && !isOpenRouterFreeChatModel(item.ID, item.Pricing.Prompt, item.Pricing.Completion, item.Architecture.OutputModalities) {
			continue
		}

		// Kilo filtering: strictly retain free chat and reasoning models (exclude audio, safety guards)
		if name == "kilo" && !isKiloFreeChatModel(item.ID, item.Pricing.Prompt, item.Pricing.Completion, item.Architecture.OutputModalities) {
			continue
		}

		// Cline filtering: strictly retain free chat and reasoning models (exclude paid, audio, safety guards)
		if name == "cline" && !isClineFreeChatModel(item.ID) {
			continue
		}

		ctxWindow := item.ContextWindow
		if ctxWindow <= 0 {
			if item.ContextLength > 0 {
				ctxWindow = item.ContextLength
			} else if item.MaxContextLength > 0 {
				ctxWindow = item.MaxContextLength
			} else if name == "nvidianim" {
				ctxWindow = getNVIDIANIMContextWindow(item.ID)
			} else if name == "openrouter" || name == "kilo" || name == "cline" {
				ctxWindow = 262144
			}
		}

		ownedBy := item.OwnedBy
		if ownedBy == "" && strings.Contains(item.ID, "/") {
			ownedBy = strings.Split(item.ID, "/")[0]
		}

		results = append(results, provider.ModelInfo{
			ID:            item.ID,
			Provider:      name,
			Active:        true,
			ContextWindow: ctxWindow,
			OwnedBy:       ownedBy,
		})
	}

	return results, nil
}

// nvidianimDecommissionedModels lists known 404 or decommissioned endpoints on NVIDIA NIM.
var nvidianimDecommissionedModels = map[string]bool{
	"01-ai/yi-large":                              true,
	"adept/fuyu-8b":                               true,
	"ai21labs/jamba-1.5-large-instruct":           true,
	"aisingapore/sea-lion-7b-instruct":            true,
	"bigcode/starcoder2-15b":                      true,
	"databricks/dbrx-instruct":                    true,
	"deepseek-ai/deepseek-coder-6.7b-instruct":    true,
	"google/codegemma-1.1-7b":                     true,
	"google/codegemma-7b":                         true,
	"google/deplot":                               true,
	"google/gemma-2b":                             true,
	"google/gemma-3-12b-it":                       true,
	"google/gemma-3-4b-it":                        true,
	"google/recurrentgemma-2b":                    true,
	"ibm/granite-3.0-3b-a800m-instruct":           true,
	"ibm/granite-3.0-8b-instruct":                 true,
	"ibm/granite-34b-code-instruct":               true,
	"ibm/granite-8b-code-instruct":                true,
	"meta/codellama-70b":                          true,
	"meta/llama-3.1-70b-instruct":                 true,
	"meta/llama-3.1-8b-instruct":                  true,
	"meta/llama-3.1-405b-instruct":                true,
	"meta/llama2-70b":                             true,
	"meta/muse-glimmer-30b":                       true,
	"microsoft/kosmos-2":                          true,
	"microsoft/phi-3-vision-128k-instruct":        true,
	"microsoft/phi-3.5-moe-instruct":              true,
	"mistralai/codestral-22b-instruct-v0.1":       true,
	"mistralai/mistral-7b-instruct-v0.3":          true,
	"mistralai/mistral-large":                     true,
	"mistralai/mistral-large-2-instruct":          true,
	"mistralai/mixtral-8x22b-v0.1":                true,
	"moonshotai/kimi-k2.6":                        true,
	"nv-mistralai/mistral-nemo-12b-instruct":      true,
	"nvidia/ai-synthetic-video-detector":          true,
	"nvidia/cosmos-reason2-8b":                    true,
	"nvidia/llama-3.1-nemotron-51b-instruct":      true,
	"nvidia/llama-3.1-nemotron-70b-instruct":      true,
	"nvidia/llama-3.1-nemotron-ultra-253b-v1":     true,
	"nvidia/llama3-chatqa-1.5-70b":                true,
	"nvidia/mistral-nemo-minitron-8b-8k-instruct": true,
	"nvidia/nemotron-4-340b-instruct":             true,
	"nvidia/nemotron-nano-3-30b-a3b":              true,
	"nvidia/neva-22b":                             true,
	"nvidia/vila":                                 true,
	"writer/palmyra-creative-122b":                true,
	"writer/palmyra-fin-70b-32k":                  true,
	"writer/palmyra-med-70b":                      true,
	"writer/palmyra-med-70b-32k":                  true,
	"zyphra/zamba2-7b-instruct":                   true,
}

// isNVIDIANIMChatModel validates that a model on NVIDIA NIM is an active chat/reasoning model.
func isNVIDIANIMChatModel(modelID string) bool {
	lower := strings.ToLower(strings.TrimSpace(modelID))

	// Exclude non-generative / non-chat model classes
	if strings.Contains(lower, "embed") ||
		strings.Contains(lower, "clip") ||
		strings.Contains(lower, "guard") ||
		strings.Contains(lower, "safety") ||
		strings.Contains(lower, "topic-control") ||
		strings.Contains(lower, "synthetic-video-detector") ||
		strings.Contains(lower, "-reward") ||
		strings.Contains(lower, "nemotron-parse") ||
		strings.Contains(lower, "riva-translate") {
		return false
	}

	// Exclude known decommissioned / 404 endpoints
	if nvidianimDecommissionedModels[lower] {
		return false
	}

	return true
}

func getNVIDIANIMContextWindow(modelID string) int {
	lower := strings.ToLower(modelID)
	switch {
	case strings.Contains(lower, "deepseek-v4-flash") || strings.Contains(lower, "nemotron-3.5") || strings.Contains(lower, "gemma-4-31b") || strings.Contains(lower, "nemotron-3"):
		return 131072
	case strings.Contains(lower, "llama-3.2") || strings.Contains(lower, "glm-5.3") || strings.Contains(lower, "gpt-oss-20b") || strings.Contains(lower, "mistral-nemotron"):
		return 131072
	case strings.Contains(lower, "laguna-xs"):
		return 65536
	case strings.Contains(lower, "diffusiongemma"):
		return 32768
	default:
		return 32768
	}
}

// isOpenRouterFreeChatModel validates that a model on OpenRouter is a free chat/reasoning model.
func isOpenRouterFreeChatModel(id, promptPrice, compPrice string, outputModalities []string) bool {
	lower := strings.ToLower(strings.TrimSpace(id))

	// Exclude non-text/audio models
	for _, mod := range outputModalities {
		if strings.ToLower(mod) == "audio" {
			return false
		}
	}
	if strings.Contains(lower, "lyria") || strings.Contains(lower, "music") {
		return false
	}

	// Exclude safety guards
	if strings.Contains(lower, "safety") || strings.Contains(lower, "guard") {
		return false
	}

	// Must be free tier: pricing zero or :free suffix or openrouter/free
	isFree := (promptPrice == "0" && compPrice == "0") || strings.HasSuffix(lower, ":free") || lower == "openrouter/free"
	return isFree
}

// isKiloFreeChatModel determines whether a model from Kilo Gateway is a free chat/reasoning model.
func isKiloFreeChatModel(id, promptPrice, compPrice string, outputModalities []string) bool {
	lower := strings.ToLower(strings.TrimSpace(id))

	// Exclude non-text/audio models
	for _, mod := range outputModalities {
		if strings.ToLower(mod) == "audio" {
			return false
		}
	}
	if strings.Contains(lower, "lyria") || strings.Contains(lower, "music") {
		return false
	}

	// Exclude safety guards
	if strings.Contains(lower, "safety") || strings.Contains(lower, "guard") {
		return false
	}

	// Check free pricing or :free / /free suffix or kilo-auto/free
	isFreePricing := (promptPrice == "0" || strings.HasPrefix(promptPrice, "0.000000000000")) &&
		(compPrice == "0" || strings.HasPrefix(compPrice, "0.000000000000"))
	isFreeID := strings.HasSuffix(lower, ":free") || strings.HasSuffix(lower, "/free") || lower == "kilo-auto/free"

	return isFreePricing || isFreeID
}

// isClineFreeChatModel determines whether a model from Cline is a free chat/reasoning model.
func isClineFreeChatModel(id string) bool {
	lower := strings.ToLower(strings.TrimSpace(id))

	// Exclude safety guards, embeddings, moderations
	if strings.Contains(lower, "safety") || strings.Contains(lower, "guard") || strings.Contains(lower, "embed") || strings.Contains(lower, "moderation") {
		return false
	}

	// Must have :free or /free suffix or contain /free
	return strings.HasSuffix(lower, ":free") || strings.Contains(lower, "/free")
}

// SendChat executes a non-streaming chat completion.
func (a *Adapter) SendChat(ctx context.Context, req *provider.UnifiedChatRequest) (*provider.UnifiedChatResponse, error) {
	payload, err := a.buildPayload(req, false)
	if err != nil {
		return nil, err
	}

	a.mu.RLock()
	baseURL := a.baseURL
	apiKey := a.apiKey
	a.mu.RUnlock()

	url := baseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if a.name == "openrouter" {
		httpReq.Header.Set("HTTP-Referer", "https://github.com/primaybr/liltok")
		httpReq.Header.Set("X-Title", "liltok")
	}

	start := time.Now()
	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	duration := time.Since(start)
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read upstream response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("upstream error status %d: %s", resp.StatusCode, string(respBytes))
	}

	var oaiResp struct {
		Success *bool  `json:"success"`
		Error   any    `json:"error"`
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
				Thought          string `json:"thought"`
				Refusal          string `json:"refusal"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
		Data *struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Choices []struct {
				Index   int `json:"index"`
				Message struct {
					Role             string `json:"role"`
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
					Thought          string `json:"thought"`
					Refusal          string `json:"refusal"`
					ToolCalls        []struct {
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		} `json:"data"`
	}

	if err := json.Unmarshal(respBytes, &oaiResp); err != nil {
		return nil, fmt.Errorf("failed to parse upstream json: %w", err)
	}

	if oaiResp.Success != nil && !*oaiResp.Success {
		return nil, fmt.Errorf("upstream error: %v", oaiResp.Error)
	}
	if oaiResp.Error != nil {
		switch e := oaiResp.Error.(type) {
		case string:
			if e != "" {
				return nil, fmt.Errorf("upstream error: %s", e)
			}
		case map[string]any:
			if msg, ok := e["message"].(string); ok && msg != "" {
				return nil, fmt.Errorf("upstream error: %s", msg)
			}
			return nil, fmt.Errorf("upstream error: %v", e)
		}
	}

	// Unwrap Cline payload if response wrapped inside top-level data field
	if len(oaiResp.Choices) == 0 && oaiResp.Data != nil {
		oaiResp.ID = oaiResp.Data.ID
		oaiResp.Model = oaiResp.Data.Model
		oaiResp.Choices = oaiResp.Data.Choices
		oaiResp.Usage = oaiResp.Data.Usage
	}

	if len(oaiResp.Choices) == 0 {
		return nil, fmt.Errorf("upstream returned no choices: %s", string(respBytes))
	}

	content := ""
	reasoningContent := ""
	role := "assistant"
	finishReason := "stop"
	var toolCalls []provider.UnifiedToolCall
	if len(oaiResp.Choices) > 0 {
		c := oaiResp.Choices[0]
		content = c.Message.Content
		if c.Message.ReasoningContent != "" {
			reasoningContent = c.Message.ReasoningContent
		} else if c.Message.Reasoning != "" {
			reasoningContent = c.Message.Reasoning
		} else if c.Message.Thought != "" {
			reasoningContent = c.Message.Thought
		}
		if strings.TrimSpace(content) == "" {
			if reasoningContent != "" {
				content = reasoningContent
			} else if c.Message.Refusal != "" {
				content = c.Message.Refusal
			}
		}
		role = c.Message.Role
		finishReason = c.FinishReason
		for _, tc := range c.Message.ToolCalls {
			toolCalls = append(toolCalls, provider.UnifiedToolCall{
				ID:   tc.ID,
				Type: tc.Type,
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
		}
	}

	rawResp := respBytes
	if oaiResp.Data != nil {
		if norm, err := json.Marshal(map[string]interface{}{
			"id":      oaiResp.ID,
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   oaiResp.Model,
			"choices": oaiResp.Choices,
			"usage":   oaiResp.Usage,
		}); err == nil {
			rawResp = norm
		}
	}

	return &provider.UnifiedChatResponse{
		ID:               oaiResp.ID,
		Model:            oaiResp.Model,
		Role:             role,
		Content:          content,
		ReasoningContent: reasoningContent,
		ToolCalls:        toolCalls,
		FinishReason:     finishReason,
		Usage: provider.UnifiedUsage{
			PromptTokens:     oaiResp.Usage.PromptTokens,
			CompletionTokens: oaiResp.Usage.CompletionTokens,
			TotalTokens:      oaiResp.Usage.TotalTokens,
		},
		RawResponse: rawResp,
		Latency:     duration,
	}, nil
}

// StreamChat initiates a streaming chat completion.
func (a *Adapter) StreamChat(ctx context.Context, req *provider.UnifiedChatRequest) (<-chan provider.UnifiedSSEEvent, <-chan error, error) {
	payload, err := a.buildPayload(req, true)
	if err != nil {
		return nil, nil, err
	}

	url := a.baseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if a.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
	}
	if a.name == "openrouter" {
		httpReq.Header.Set("HTTP-Referer", "https://github.com/primaybr/liltok")
		httpReq.Header.Set("X-Title", "liltok")
	}

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("upstream stream request failed: %w", err)
	}

	if resp.StatusCode >= 400 {
		respBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, nil, fmt.Errorf("upstream error status %d: %s", resp.StatusCode, string(respBytes))
	}

	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "application/json") && !strings.Contains(contentType, "text/event-stream") {
		respBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		var errResp struct {
			Success *bool `json:"success"`
			Error   any   `json:"error"`
		}
		if err := json.Unmarshal(respBytes, &errResp); err == nil {
			if errResp.Success != nil && !*errResp.Success {
				return nil, nil, fmt.Errorf("upstream stream error: %v", errResp.Error)
			}
			if errResp.Error != nil {
				return nil, nil, fmt.Errorf("upstream stream error: %v", errResp.Error)
			}
		}
		return nil, nil, fmt.Errorf("upstream returned json error status %d: %s", resp.StatusCode, string(respBytes))
	}

	eventChan := make(chan provider.UnifiedSSEEvent, 100)
	errChan := make(chan error, 1)

	go func() {
		defer resp.Body.Close()
		defer close(eventChan)
		defer close(errChan)

		scanner := bufio.NewScanner(resp.Body)
		buf := make([]byte, 64*1024)
		scanner.Buffer(buf, 512*1024)

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data: ") {
				continue
			}

			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				eventChan <- provider.UnifiedSSEEvent{
					Type: "done",
				}
				return
			}

			var chunk struct {
				ID      string `json:"id"`
				Model   string `json:"model"`
				Choices []struct {
					Delta struct {
						Role    string `json:"role"`
						Content string `json:"content"`
					} `json:"delta"`
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
			}

			if err := json.Unmarshal([]byte(data), &chunk); err == nil && len(chunk.Choices) > 0 {
				c := chunk.Choices[0]
				ev := provider.UnifiedSSEEvent{
					Type:         "text_delta",
					DeltaText:    c.Delta.Content,
					Role:         c.Delta.Role,
					FinishReason: c.FinishReason,
					RawChunk:     []byte(line + "\n\n"),
				}
				if c.FinishReason != "" {
					ev.Type = "finish"
				}
				eventChan <- ev
			}
		}

		if err := scanner.Err(); err != nil && err != io.EOF {
			errChan <- err
		}
	}()

	return eventChan, errChan, nil
}

func (a *Adapter) buildPayload(req *provider.UnifiedChatRequest, stream bool) ([]byte, error) {
	// If already in standard OpenAI format and not needing translation
	if !req.IsAnthropicSource && len(req.RawPayload) > 0 {
		var rawMap map[string]interface{}
		if err := json.Unmarshal(req.RawPayload, &rawMap); err == nil {
			rawMap["stream"] = stream
			rawMap["model"] = req.Model
			return json.Marshal(rawMap)
		}
	}

	// Construct standardized OpenAI payload
	messages := make([]map[string]interface{}, 0, len(req.Messages)+1)
	if req.SystemPrompt != "" {
		messages = append(messages, map[string]interface{}{
			"role":    "system",
			"content": req.SystemPrompt,
		})
	}
	for i, m := range req.Messages {
		role := m.Role

		// Normalize system messages:
		// 1. If identical to top-level system prompt at message index 0, skip to avoid duplicate.
		// 2. If trailing or mid-conversation system message (e.g. Claude Code environment update),
		//    convert to role "user" with [System Reminder] prefix to comply with OpenAI/DeepSeek chat templates.
		if role == "system" {
			if i == 0 && req.SystemPrompt == m.Content {
				continue
			}
			if len(messages) == 0 {
				role = "system"
			} else {
				role = "user"
			}
		}

		msg := map[string]interface{}{
			"role": role,
		}
		if m.Role == "system" && role == "user" {
			msg["content"] = "[System Reminder]\n" + m.Content
		} else if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			var tcList []map[string]interface{}
			for _, tc := range m.ToolCalls {
				tcList = append(tcList, map[string]interface{}{
					"id":   tc.ID,
					"type": "function",
					"function": map[string]interface{}{
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					},
				})
			}
			msg["tool_calls"] = tcList
			if m.Content != "" {
				msg["content"] = m.Content
			} else {
				msg["content"] = nil
			}
		} else if m.Role == "tool" {
			msg["content"] = m.Content
			if m.ToolCallID != "" {
				msg["tool_call_id"] = m.ToolCallID
			}
		} else {
			msg["content"] = m.Content
		}
		messages = append(messages, msg)
	}

	payload := map[string]interface{}{
		"model":       req.Model,
		"messages":    messages,
		"temperature": req.Temperature,
		"stream":      stream,
	}

	if req.TopP > 0 {
		payload["top_p"] = req.TopP
	}
	maxTokens := req.MaxTokens
	if maxTokens > 0 {
		if a.name == "groq" && maxTokens > 8192 {
			maxTokens = 8192
		} else if a.name == "nvidianim" && maxTokens > 16384 {
			maxTokens = 16384
		}
		if len(req.Tools) > 0 && maxTokens < 4096 {
			maxTokens = 4096
		}
		payload["max_tokens"] = maxTokens
	} else if len(req.Tools) > 0 {
		payload["max_tokens"] = 4096
	}
	if len(req.Tools) > 0 {
		payload["tools"] = convertToolsToOpenAI(req.Tools)
	}
	if req.ToolChoice != nil {
		payload["tool_choice"] = convertToolChoiceToOpenAI(req.ToolChoice)
	}

	return json.Marshal(payload)
}

func convertToolsToOpenAI(tools []interface{}) []interface{} {
	out := make([]interface{}, 0, len(tools))
	for _, t := range tools {
		tMap, ok := t.(map[string]interface{})
		if !ok {
			out = append(out, t)
			continue
		}
		// If already in OpenAI format with type "function" and "function" map
		if _, hasFunc := tMap["function"]; hasFunc {
			out = append(out, t)
			continue
		}
		// Anthropic schema: {name: "...", description: "...", input_schema: {...}}
		name, _ := tMap["name"].(string)
		desc, _ := tMap["description"].(string)
		schema := tMap["input_schema"]
		if schema == nil {
			schema = map[string]interface{}{
				"type": "object",
			}
		}
		fnMap := map[string]interface{}{
			"name":       name,
			"parameters": schema,
		}
		if desc != "" {
			fnMap["description"] = desc
		}
		out = append(out, map[string]interface{}{
			"type":     "function",
			"function": fnMap,
		})
	}
	return out
}

func convertToolChoiceToOpenAI(choice interface{}) interface{} {
	cMap, ok := choice.(map[string]interface{})
	if !ok {
		return choice
	}
	tType, _ := cMap["type"].(string)
	switch tType {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "tool":
		name, _ := cMap["name"].(string)
		return map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name": name,
			},
		}
	default:
		return choice
	}
}
