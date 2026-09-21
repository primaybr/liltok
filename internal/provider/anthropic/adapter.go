package anthropic

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

// Adapter implements provider.ProviderClient for Anthropic Claude.
type Adapter struct {
	baseURL    string
	apiKey     string
	mu         sync.RWMutex
	httpClient *http.Client
}

// NewAdapter creates an Anthropic adapter.
func NewAdapter(baseURL, apiKey string) *Adapter {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &Adapter{
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
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	a.baseURL = strings.TrimRight(baseURL, "/")
}

func (a *Adapter) Name() string {
	return "anthropic"
}

func (a *Adapter) Tier() provider.ProviderTier {
	return provider.TierPremium
}

func (a *Adapter) CheckHealth(ctx context.Context) (bool, error) {
	a.mu.RLock()
	baseURL := a.baseURL
	apiKey := a.apiKey
	a.mu.RUnlock()

	if apiKey == "" {
		return false, fmt.Errorf("anthropic api key is not configured")
	}

	url := baseURL + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false, err
	}
	a.setHeaders(req)
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("anthropic connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBytes, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("anthropic returned status %d: %s", resp.StatusCode, string(respBytes))
	}
	return true, nil
}

// ListModels queries Anthropic /v1/models.
func (a *Adapter) ListModels(ctx context.Context) ([]provider.ModelInfo, error) {
	a.mu.RLock()
	baseURL := a.baseURL
	apiKey := a.apiKey
	a.mu.RUnlock()

	if apiKey == "" {
		return nil, fmt.Errorf("anthropic api key is not configured")
	}

	url := baseURL + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	a.setHeaders(req)
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic list models failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("anthropic returned status %d: %s", resp.StatusCode, string(respBytes))
	}

	var anthResp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&anthResp); err != nil {
		return nil, fmt.Errorf("failed to decode anthropic models: %w", err)
	}

	var results []provider.ModelInfo
	for _, m := range anthResp.Data {
		results = append(results, provider.ModelInfo{
			ID:       m.ID,
			Provider: "anthropic",
			Active:   true,
			OwnedBy:  "anthropic",
		})
	}
	return results, nil
}

// SendChat sends a non-streaming request to Anthropic /v1/messages.
func (a *Adapter) SendChat(ctx context.Context, req *provider.UnifiedChatRequest) (*provider.UnifiedChatResponse, error) {
	payload, err := a.buildPayload(req, false)
	if err != nil {
		return nil, err
	}

	a.mu.RLock()
	baseURL := a.baseURL
	a.mu.RUnlock()

	url := baseURL + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}

	a.setHeaders(httpReq)

	start := time.Now()
	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic request failed: %w", err)
	}
	defer resp.Body.Close()

	duration := time.Since(start)
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("anthropic error status %d: %s", resp.StatusCode, string(respBytes))
	}

	var anthResp struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Role    string `json:"role"`
		Content []struct {
			Type     string                 `json:"type"`
			Text     string                 `json:"text"`
			Thinking string                 `json:"thinking"`
			ID       string                 `json:"id"`
			Name     string                 `json:"name"`
			Input    map[string]interface{} `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(respBytes, &anthResp); err != nil {
		return nil, fmt.Errorf("failed to parse anthropic json: %w", err)
	}

	content := ""
	reasoningContent := ""
	var toolCalls []provider.UnifiedToolCall
	for _, c := range anthResp.Content {
		switch c.Type {
		case "text":
			content += c.Text
		case "thinking":
			reasoningContent += c.Thinking
		case "tool_use":
			inputBytes, _ := json.Marshal(c.Input)
			toolCalls = append(toolCalls, provider.UnifiedToolCall{
				ID:   c.ID,
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      c.Name,
					Arguments: string(inputBytes),
				},
			})
		}
	}

	return &provider.UnifiedChatResponse{
		ID:               anthResp.ID,
		Model:            anthResp.Model,
		Role:             anthResp.Role,
		Content:          content,
		ReasoningContent: reasoningContent,
		ToolCalls:        toolCalls,
		FinishReason:     anthResp.StopReason,
		Usage: provider.UnifiedUsage{
			PromptTokens:     anthResp.Usage.InputTokens,
			CompletionTokens: anthResp.Usage.OutputTokens,
			TotalTokens:      anthResp.Usage.InputTokens + anthResp.Usage.OutputTokens,
			CachedTokens:     anthResp.Usage.CacheReadInputTokens,
		},
		RawResponse: respBytes,
		Latency:     duration,
	}, nil
}

// StreamChat initiates a streaming connection to Anthropic /v1/messages.
func (a *Adapter) StreamChat(ctx context.Context, req *provider.UnifiedChatRequest) (<-chan provider.UnifiedSSEEvent, <-chan error, error) {
	payload, err := a.buildPayload(req, true)
	if err != nil {
		return nil, nil, err
	}

	url := a.baseURL + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, err
	}

	a.setHeaders(httpReq)

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("anthropic stream failed: %w", err)
	}

	if resp.StatusCode >= 400 {
		respBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, nil, fmt.Errorf("anthropic error status %d: %s", resp.StatusCode, string(respBytes))
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

		var currentEvent string
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if strings.HasPrefix(line, "event: ") {
				currentEvent = strings.TrimPrefix(line, "event: ")
				continue
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}

			data := strings.TrimPrefix(line, "data: ")

			switch currentEvent {
			case "content_block_delta":
				var blockDelta struct {
					Delta struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"delta"`
				}
				if err := json.Unmarshal([]byte(data), &blockDelta); err == nil {
					eventChan <- provider.UnifiedSSEEvent{
						Type:      "text_delta",
						DeltaText: blockDelta.Delta.Text,
						RawChunk:  []byte(fmt.Sprintf("event: content_block_delta\ndata: %s\n\n", data)),
					}
				}
			case "message_delta":
				var msgDelta struct {
					Delta struct {
						StopReason string `json:"stop_reason"`
					} `json:"delta"`
					Usage struct {
						OutputTokens int `json:"output_tokens"`
					} `json:"usage"`
				}
				if err := json.Unmarshal([]byte(data), &msgDelta); err == nil {
					eventChan <- provider.UnifiedSSEEvent{
						Type:         "finish",
						FinishReason: msgDelta.Delta.StopReason,
						RawChunk:     []byte(fmt.Sprintf("event: message_delta\ndata: %s\n\n", data)),
					}
				}
			case "message_stop":
				eventChan <- provider.UnifiedSSEEvent{
					Type:     "done",
					RawChunk: []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"),
				}
				return
			}
		}

		if err := scanner.Err(); err != nil && err != io.EOF {
			errChan <- err
		}
	}()

	return eventChan, errChan, nil
}

func (a *Adapter) setHeaders(r *http.Request) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("anthropic-version", "2023-06-01")
	r.Header.Set("anthropic-beta", "prompt-caching-2024-07-25")
	a.mu.RLock()
	apiKey := a.apiKey
	a.mu.RUnlock()
	if apiKey != "" {
		r.Header.Set("x-api-key", apiKey)
	}
}

func (a *Adapter) buildPayload(req *provider.UnifiedChatRequest, stream bool) ([]byte, error) {
	// If incoming request was already an Anthropic payload, reuse
	if req.IsAnthropicSource && len(req.RawPayload) > 0 {
		var rawMap map[string]interface{}
		if err := json.Unmarshal(req.RawPayload, &rawMap); err == nil {
			rawMap["stream"] = stream
			if _, hasTokens := rawMap["max_tokens"]; !hasTokens {
				rawMap["max_tokens"] = 4096
			}
			return json.Marshal(rawMap)
		}
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}

	// Translate messages
	messages := make([]map[string]interface{}, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := m.Role
		if role == "system" {
			continue // Anthropic uses top-level system parameter
		}
		messages = append(messages, map[string]interface{}{
			"role":    role,
			"content": m.Content,
		})
	}

	payload := map[string]interface{}{
		"model":      req.Model,
		"messages":   messages,
		"max_tokens": maxTokens,
		"stream":     stream,
	}

	if req.SystemPrompt != "" {
		payload["system"] = req.SystemPrompt
	}
	if req.Temperature > 0 {
		payload["temperature"] = req.Temperature
	}
	if req.TopP > 0 {
		payload["top_p"] = req.TopP
	}

	return json.Marshal(payload)
}
