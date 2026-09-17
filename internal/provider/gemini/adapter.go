package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/primaybr/liltok/internal/provider"
)

// Adapter implements provider.ProviderClient for Google Gemini with multi-key rotation.
type Adapter struct {
	mu         sync.RWMutex
	baseURL    string
	apiKey     string
	apiKeys    []string
	counter    uint64
	httpClient *http.Client
}

// NewAdapter creates a Google Gemini adapter supporting one or more API keys (comma or newline separated).
func NewAdapter(baseURL, apiKey string) *Adapter {
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	keys := parseKeys(apiKey)

	return &Adapter{
		baseURL:    baseURL,
		apiKey:     apiKey,
		apiKeys:    keys,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

func parseKeys(apiKey string) []string {
	var keys []string
	for _, raw := range strings.FieldsFunc(apiKey, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ';'
	}) {
		k := strings.TrimSpace(raw)
		if k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// SetAPIKey dynamically updates the API key(s).
func (a *Adapter) SetAPIKey(apiKey string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.apiKey = apiKey
	a.apiKeys = parseKeys(apiKey)
}

// SetBaseURL dynamically updates the upstream base URL.
func (a *Adapter) SetBaseURL(baseURL string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com"
	}
	a.baseURL = strings.TrimRight(baseURL, "/")
}

// KeyCount returns the number of active API keys configured.
func (a *Adapter) KeyCount() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.apiKeys)
}

func (a *Adapter) Name() string {
	return "gemini"
}

func (a *Adapter) Tier() provider.ProviderTier {
	return provider.TierFree
}

// CheckHealth verifies connection to the Gemini API using configured keys.
func (a *Adapter) CheckHealth(ctx context.Context) (bool, error) {
	a.mu.RLock()
	baseURL := a.baseURL
	keys := append([]string(nil), a.apiKeys...)
	a.mu.RUnlock()

	if len(keys) == 0 {
		return false, fmt.Errorf("gemini api key is not configured")
	}

	key := keys[0]
	url := fmt.Sprintf("%s/v1beta/models?key=%s", baseURL, key)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false, err
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("gemini connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBytes, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("gemini returned status %d: %s", resp.StatusCode, string(respBytes))
	}

	return true, nil
}

// SendChat executes a Gemini generateContent request with automatic multi-key rotation and 429 failover.
func (a *Adapter) SendChat(ctx context.Context, req *provider.UnifiedChatRequest) (*provider.UnifiedChatResponse, error) {
	a.mu.RLock()
	baseURL := a.baseURL
	keys := append([]string(nil), a.apiKeys...)
	a.mu.RUnlock()

	if len(keys) == 0 {
		return nil, fmt.Errorf("gemini api key is not configured")
	}

	model := req.Model
	if !strings.HasPrefix(model, "models/") {
		model = "models/" + model
	}

	payload, err := a.buildPayload(req)
	if err != nil {
		return nil, err
	}

	// Determine starting key using atomic round-robin counter (0-indexed)
	startIdx := int((atomic.AddUint64(&a.counter, 1) - 1) % uint64(len(keys)))
	var lastErr error

	for i := 0; i < len(keys); i++ {
		key := keys[(startIdx+i)%len(keys)]
		url := fmt.Sprintf("%s/v1beta/%s:generateContent?key=%s", baseURL, model, key)

		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")

		start := time.Now()
		resp, err := a.httpClient.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("gemini request failed: %w", err)
			continue
		}

		duration := time.Since(start)
		respBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("failed to read gemini body: %w", err)
			continue
		}

		// If rate limit (429) or quota exceeded, try next available key
		if resp.StatusCode == http.StatusTooManyRequests || strings.Contains(string(respBytes), "RESOURCE_EXHAUSTED") {
			lastErr = fmt.Errorf("gemini key rate limited (status %d): %s", resp.StatusCode, string(respBytes))
			continue
		}

		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("gemini error %d: %s", resp.StatusCode, string(respBytes))
		}

		var geminiResp struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
					Role string `json:"role"`
				} `json:"content"`
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
			UsageMetadata struct {
				PromptTokenCount     int `json:"promptTokenCount"`
				CandidatesTokenCount int `json:"candidatesTokenCount"`
				TotalTokenCount      int `json:"totalTokenCount"`
			} `json:"usageMetadata"`
		}

		if err := json.Unmarshal(respBytes, &geminiResp); err != nil {
			return nil, fmt.Errorf("failed to parse gemini json: %w", err)
		}

		content := ""
		finishReason := "stop"
		if len(geminiResp.Candidates) > 0 {
			c := geminiResp.Candidates[0]
			for _, p := range c.Content.Parts {
				content += p.Text
			}
			if c.FinishReason != "" {
				finishReason = strings.ToLower(c.FinishReason)
			}
		}

		return &provider.UnifiedChatResponse{
			ID:           fmt.Sprintf("gemini-%x", time.Now().UnixMilli()),
			Model:        req.Model,
			Role:         "assistant",
			Content:      content,
			FinishReason: finishReason,
			Usage: provider.UnifiedUsage{
				PromptTokens:     geminiResp.UsageMetadata.PromptTokenCount,
				CompletionTokens: geminiResp.UsageMetadata.CandidatesTokenCount,
				TotalTokens:      geminiResp.UsageMetadata.TotalTokenCount,
			},
			RawResponse: respBytes,
			Latency:     duration,
		}, nil
	}

	return nil, fmt.Errorf("all gemini api keys exhausted: %w", lastErr)
}

// StreamChat is provided for completeness; for Gemini, falls back to buffered generateContent.
func (a *Adapter) StreamChat(ctx context.Context, req *provider.UnifiedChatRequest) (<-chan provider.UnifiedSSEEvent, <-chan error, error) {
	resp, err := a.SendChat(ctx, req)
	if err != nil {
		return nil, nil, err
	}

	eventChan := make(chan provider.UnifiedSSEEvent, 2)
	errChan := make(chan error, 1)

	go func() {
		defer close(eventChan)
		defer close(errChan)

		eventChan <- provider.UnifiedSSEEvent{
			Type:      "text_delta",
			DeltaText: resp.Content,
			Role:      "assistant",
		}
		eventChan <- provider.UnifiedSSEEvent{
			Type:         "finish",
			FinishReason: resp.FinishReason,
			Usage:        &resp.Usage,
		}
	}()

	return eventChan, errChan, nil
}

func (a *Adapter) buildPayload(req *provider.UnifiedChatRequest) ([]byte, error) {
	contents := make([]map[string]interface{}, 0, len(req.Messages))

	for _, m := range req.Messages {
		if m.Role == "system" {
			continue
		}
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		contents = append(contents, map[string]interface{}{
			"role": role,
			"parts": []map[string]string{
				{"text": m.Content},
			},
		})
	}

	payload := map[string]interface{}{
		"contents": contents,
	}

	if req.SystemPrompt != "" {
		payload["systemInstruction"] = map[string]interface{}{
			"parts": []map[string]string{
				{"text": req.SystemPrompt},
			},
		}
	}

	generationConfig := map[string]interface{}{}
	if req.Temperature > 0 {
		generationConfig["temperature"] = req.Temperature
	}
	if req.TopP > 0 {
		generationConfig["topP"] = req.TopP
	}
	if req.MaxTokens > 0 {
		generationConfig["maxOutputTokens"] = req.MaxTokens
	}
	if len(generationConfig) > 0 {
		payload["generationConfig"] = generationConfig
	}

	return json.Marshal(payload)
}
