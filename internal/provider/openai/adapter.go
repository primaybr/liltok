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

// NewOllamaAdapter creates an adapter for local offline Ollama endpoints.
func NewOllamaAdapter(baseURL string) *Adapter {
	if baseURL == "" {
		baseURL = "http://localhost:11434/v1"
	}
	return NewAdapter("ollama", provider.TierFree, baseURL, "")
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

	if name != "ollama" && apiKey == "" {
		return false, fmt.Errorf("%s api key is not configured", name)
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
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
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
	}

	if err := json.Unmarshal(respBytes, &oaiResp); err != nil {
		return nil, fmt.Errorf("failed to parse upstream json: %w", err)
	}

	content := ""
	role := "assistant"
	finishReason := "stop"
	var toolCalls []provider.UnifiedToolCall
	if len(oaiResp.Choices) > 0 {
		c := oaiResp.Choices[0]
		content = c.Message.Content
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

	return &provider.UnifiedChatResponse{
		ID:           oaiResp.ID,
		Model:        oaiResp.Model,
		Role:         role,
		Content:      content,
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
		Usage: provider.UnifiedUsage{
			PromptTokens:     oaiResp.Usage.PromptTokens,
			CompletionTokens: oaiResp.Usage.CompletionTokens,
			TotalTokens:      oaiResp.Usage.TotalTokens,
		},
		RawResponse: respBytes,
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

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("upstream stream request failed: %w", err)
	}

	if resp.StatusCode >= 400 {
		respBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, nil, fmt.Errorf("upstream error status %d: %s", resp.StatusCode, string(respBytes))
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
	for _, m := range req.Messages {
		messages = append(messages, map[string]interface{}{
			"role":    m.Role,
			"content": m.Content,
		})
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
	if req.MaxTokens > 0 {
		payload["max_tokens"] = req.MaxTokens
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
