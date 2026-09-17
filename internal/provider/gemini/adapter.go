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
	cleanModel := strings.TrimPrefix(model, "models/")
	if cleanModel == "gemini-flash-latest" || cleanModel == "gemini-1.5-flash" || cleanModel == "gemini-2.5-flash" || cleanModel == "gemini-flash" {
		model = "gemini-3.6-flash"
	}
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

		// If rate limit (429), temporary service unavailable (503), or quota exceeded, try next available key
		if resp.StatusCode == http.StatusTooManyRequests || 
		   resp.StatusCode == http.StatusServiceUnavailable || 
		   strings.Contains(string(respBytes), "RESOURCE_EXHAUSTED") || 
		   strings.Contains(string(respBytes), "UNAVAILABLE") {
			lastErr = fmt.Errorf("gemini key %d unavailable (status %d): %s", i+1, resp.StatusCode, string(respBytes))
			continue
		}

		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("gemini error %d: %s", resp.StatusCode, string(respBytes))
		}

		var geminiResp struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text         string `json:"text"`
						FunctionCall *struct {
							Name string                 `json:"name"`
							Args map[string]interface{} `json:"args"`
							ID   string                 `json:"id"`
						} `json:"functionCall"`
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
		var toolCalls []provider.UnifiedToolCall
		if len(geminiResp.Candidates) > 0 {
			c := geminiResp.Candidates[0]
			for _, p := range c.Content.Parts {
				if p.Text != "" {
					content += p.Text
				}
				if p.FunctionCall != nil {
					argsBytes, _ := json.Marshal(p.FunctionCall.Args)
					callID := p.FunctionCall.ID
					if callID == "" {
						callID = fmt.Sprintf("call_%x", time.Now().UnixNano())
					}
					toolCalls = append(toolCalls, provider.UnifiedToolCall{
						ID:   callID,
						Type: "function",
						Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{
							Name:      p.FunctionCall.Name,
							Arguments: string(argsBytes),
						},
					})
					finishReason = "tool_calls"
				}
			}
			if len(toolCalls) == 0 && c.FinishReason != "" {
				finishReason = strings.ToLower(c.FinishReason)
			}
		}

		return &provider.UnifiedChatResponse{
			ID:           fmt.Sprintf("gemini-%x", time.Now().UnixMilli()),
			Model:        req.Model,
			Role:         "assistant",
			Content:      content,
			ToolCalls:    toolCalls,
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

		var parts []map[string]interface{}
		if m.Content != "" {
			parts = append(parts, map[string]interface{}{
				"text": m.Content,
			})
		}
		for _, tc := range m.ToolCalls {
			var args map[string]interface{}
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			if args == nil {
				args = map[string]interface{}{}
			}
			parts = append(parts, map[string]interface{}{
				"functionCall": map[string]interface{}{
					"name": tc.Function.Name,
					"args": args,
				},
			})
		}
		if len(parts) == 0 {
			parts = append(parts, map[string]interface{}{
				"text": "",
			})
		}

		contents = append(contents, map[string]interface{}{
			"role":  role,
			"parts": parts,
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

	if len(req.Tools) > 0 {
		if gemTools := convertToolsToGemini(req.Tools); gemTools != nil {
			payload["tools"] = gemTools
		}
	}

	return json.Marshal(payload)
}

func convertToolsToGemini(tools []interface{}) []map[string]interface{} {
	var decls []map[string]interface{}
	for _, t := range tools {
		tMap, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := tMap["name"].(string)
		desc, _ := tMap["description"].(string)
		var schema interface{}

		if s, ok := tMap["input_schema"]; ok {
			schema = s
		} else if fn, ok := tMap["function"].(map[string]interface{}); ok {
			name, _ = fn["name"].(string)
			desc, _ = fn["description"].(string)
			schema = fn["parameters"]
		}

		if name == "" {
			continue
		}

		decl := map[string]interface{}{
			"name": name,
		}
		if desc != "" {
			decl["description"] = desc
		}
		if schema != nil {
			if cleaned := cleanGeminiSchema(schema); cleaned != nil {
				decl["parameters"] = cleaned
			}
		}
		decls = append(decls, decl)
	}
	if len(decls) == 0 {
		return nil
	}
	return []map[string]interface{}{
		{"functionDeclarations": decls},
	}
}

// cleanGeminiSchema recursively sanitizes JSON Schema definitions to conform strictly
// with Google Gemini's OpenAPI 3.0 Schema protobuf specification, stripping keywords
// that trigger Gemini HTTP 400 INVALID_ARGUMENT errors.
func cleanGeminiSchema(val interface{}) interface{} {
	if val == nil {
		return nil
	}

	switch v := val.(type) {
	case map[string]interface{}:
		cleaned := make(map[string]interface{})

		// Handle const -> enum: [constVal]
		if constVal, ok := v["const"]; ok {
			if _, hasEnum := v["enum"]; !hasEnum {
				cleaned["enum"] = []string{fmt.Sprintf("%v", constVal)}
			}
		}

		// Handle exclusiveMinimum -> minimum
		if exMin, ok := v["exclusiveMinimum"]; ok {
			if _, hasMin := v["minimum"]; !hasMin {
				cleaned["minimum"] = exMin
			}
		}

		// Handle exclusiveMaximum -> maximum
		if exMax, ok := v["exclusiveMaximum"]; ok {
			if _, hasMax := v["maximum"]; !hasMax {
				cleaned["maximum"] = exMax
			}
		}

		// Handle prefixItems -> items
		if prefixItems, ok := v["prefixItems"].([]interface{}); ok && len(prefixItems) > 0 {
			if _, hasItems := v["items"]; !hasItems {
				cleaned["items"] = cleanGeminiSchema(prefixItems[0])
			}
		}

		// Strictly process only fields supported by Google Gemini Schema protobuf
		for k, child := range v {
			switch k {
			case "type":
				switch t := child.(type) {
				case string:
					cleaned["type"] = strings.ToLower(t)
				case []interface{}:
					// Convert type union such as ["string", "null"]
					for _, item := range t {
						if s, ok := item.(string); ok {
							if strings.ToLower(s) == "null" {
								cleaned["nullable"] = true
							} else {
								cleaned["type"] = strings.ToLower(s)
							}
						}
					}
				}

			case "format":
				if s, ok := child.(string); ok && s != "" {
					cleaned["format"] = s
				}

			case "description":
				if s, ok := child.(string); ok && s != "" {
					cleaned["description"] = s
				}

			case "nullable":
				if b, ok := child.(bool); ok {
					cleaned["nullable"] = b
				}

			case "enum":
				if enumList, ok := child.([]interface{}); ok {
					var strEnum []string
					for _, e := range enumList {
						if es, ok := e.(string); ok {
							strEnum = append(strEnum, es)
						} else {
							strEnum = append(strEnum, fmt.Sprintf("%v", e))
						}
					}
					cleaned["enum"] = strEnum
				} else if strList, ok := child.([]string); ok {
					cleaned["enum"] = strList
				}

			case "properties":
				if propMap, ok := child.(map[string]interface{}); ok {
					cleanedProps := make(map[string]interface{})
					for pName, pVal := range propMap {
						cleanedProps[pName] = cleanGeminiSchema(pVal)
					}
					cleaned["properties"] = cleanedProps
				}

			case "required":
				if reqList, ok := child.([]interface{}); ok {
					var strReq []string
					for _, r := range reqList {
						if rs, ok := r.(string); ok {
							strReq = append(strReq, rs)
						}
					}
					cleaned["required"] = strReq
				} else if strList, ok := child.([]string); ok {
					cleaned["required"] = strList
				}

			case "items":
				if itemMap, ok := child.(map[string]interface{}); ok {
					cleaned["items"] = cleanGeminiSchema(itemMap)
				} else if itemSlice, ok := child.([]interface{}); ok && len(itemSlice) > 0 {
					cleaned["items"] = cleanGeminiSchema(itemSlice[0])
				}

			case "minItems", "min_items":
				cleaned["minItems"] = child
			case "maxItems", "max_items":
				cleaned["maxItems"] = child
			case "minLength", "min_length":
				cleaned["minLength"] = child
			case "maxLength", "max_length":
				cleaned["maxLength"] = child
			case "minimum":
				cleaned["minimum"] = child
			case "maximum":
				cleaned["maximum"] = child

			case "anyOf", "any_of":
				if anySlice, ok := child.([]interface{}); ok {
					var nonNullSchemas []interface{}
					hasNull := false
					for _, item := range anySlice {
						if m, ok := item.(map[string]interface{}); ok {
							if t, ok := m["type"].(string); ok && strings.ToLower(t) == "null" {
								hasNull = true
								continue
							}
						}
						nonNullSchemas = append(nonNullSchemas, cleanGeminiSchema(item))
					}

					if hasNull && len(nonNullSchemas) == 1 {
						if singleMap, ok := nonNullSchemas[0].(map[string]interface{}); ok {
							for sk, sv := range singleMap {
								cleaned[sk] = sv
							}
							cleaned["nullable"] = true
						} else {
							cleaned["anyOf"] = nonNullSchemas
							cleaned["nullable"] = true
						}
					} else if len(nonNullSchemas) > 0 {
						cleaned["anyOf"] = nonNullSchemas
					}
				}

			default:
				// Intentionally drop $schema, additionalProperties, propertyNames, default,
				// title, pattern, $defs, definitions, $ref, $id, examples, etc.
			}
		}

		// Ensure object type when properties are defined
		if _, hasProps := cleaned["properties"]; hasProps {
			if _, hasType := cleaned["type"]; !hasType {
				cleaned["type"] = "object"
			}
		}

		// Default primitive type to string if property has enum
		if _, hasType := cleaned["type"]; !hasType {
			if _, hasProps := cleaned["properties"]; !hasProps {
				if _, hasItems := cleaned["items"]; !hasItems {
					if _, hasEnum := cleaned["enum"]; hasEnum {
						cleaned["type"] = "string"
					}
				}
			}
		}

		return cleaned

	case []interface{}:
		var cleanedList []interface{}
		for _, item := range v {
			cleanedList = append(cleanedList, cleanGeminiSchema(item))
		}
		return cleanedList

	default:
		return val
	}
}
