package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/liltok/liltok/internal/provider"
)

// Adapter implements provider.ProviderClient for Google Gemini.
type Adapter struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewAdapter creates a Google Gemini adapter.
func NewAdapter(baseURL, apiKey string) *Adapter {
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com"
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

func (a *Adapter) Name() string {
	return "gemini"
}

func (a *Adapter) Tier() provider.ProviderTier {
	return provider.TierFree
}

func (a *Adapter) CheckHealth(ctx context.Context) (bool, error) {
	return true, nil
}

// SendChat executes a Gemini generateContent request.
func (a *Adapter) SendChat(ctx context.Context, req *provider.UnifiedChatRequest) (*provider.UnifiedChatResponse, error) {
	model := req.Model
	if !strings.HasPrefix(model, "models/") {
		model = "models/" + model
	}

	url := fmt.Sprintf("%s/v1beta/%s:generateContent?key=%s", a.baseURL, model, a.apiKey)
	payload, err := a.buildPayload(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini request failed: %w", err)
	}
	defer resp.Body.Close()

	duration := time.Since(start)
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read gemini body: %w", err)
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
