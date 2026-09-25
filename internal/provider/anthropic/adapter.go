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

	a.mu.RLock()
	baseURL := a.baseURL
	a.mu.RUnlock()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/v1/messages", bytes.NewReader(payload))
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
		(&streamState{ctx: ctx, events: eventChan, errs: errChan}).run(resp.Body)
	}()

	return eventChan, errChan, nil
}

// streamState converts an Anthropic Messages event stream into UnifiedSSEEvents. Text and
// thinking deltas are forwarded as they arrive; tool_use input arrives as input_json_delta
// fragments, which are assembled and emitted as one complete tool_call when the block stops.
type streamState struct {
	ctx    context.Context
	events chan<- provider.UnifiedSSEEvent
	errs   chan<- error

	usage provider.UnifiedUsage
	tools map[int]*streamTool
}

type streamTool struct {
	id, name string
	input    strings.Builder
}

// streamEvent is the union of the Anthropic stream event fields this adapter reads.
type streamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		Usage streamUsage `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *streamUsage `json:"usage"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type streamUsage struct {
	InputTokens          int `json:"input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
}

// emit sends an event unless the caller has gone away; it reports whether to keep reading.
func (s *streamState) emit(ev provider.UnifiedSSEEvent) bool {
	select {
	case s.events <- ev:
		return true
	case <-s.ctx.Done():
		return false
	}
}

func (s *streamState) run(body io.Reader) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 512*1024)

	var eventName string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var ev streamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		// The data's own type is authoritative; the preceding event: line covers senders that omit it.
		if ev.Type == "" {
			ev.Type = eventName
		}
		if !s.handle(ev, data) {
			return
		}
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		s.errs <- err
	}
}

// handle processes one event and reports whether to keep reading the stream.
func (s *streamState) handle(ev streamEvent, data string) bool {
	switch ev.Type {
	case "message_start":
		s.usage.PromptTokens = ev.Message.Usage.InputTokens
		s.usage.CompletionTokens = ev.Message.Usage.OutputTokens
		s.usage.CachedTokens = ev.Message.Usage.CacheReadInputTokens
	case "content_block_start":
		if ev.ContentBlock.Type == "tool_use" {
			if s.tools == nil {
				s.tools = map[int]*streamTool{}
			}
			s.tools[ev.Index] = &streamTool{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
		}
	case "content_block_delta":
		raw := []byte(fmt.Sprintf("event: content_block_delta\ndata: %s\n\n", data))
		switch ev.Delta.Type {
		case "text_delta", "": // untyped deltas carrying text are treated as text
			if ev.Delta.Type == "" && ev.Delta.Text == "" {
				return true
			}
			return s.emit(provider.UnifiedSSEEvent{Type: "text_delta", DeltaText: ev.Delta.Text, RawChunk: raw})
		case "thinking_delta":
			return s.emit(provider.UnifiedSSEEvent{Type: "thinking_delta", DeltaText: ev.Delta.Thinking, RawChunk: raw})
		case "input_json_delta":
			if tool := s.tools[ev.Index]; tool != nil {
				tool.input.WriteString(ev.Delta.PartialJSON)
			}
		}
	case "content_block_stop":
		tool := s.tools[ev.Index]
		if tool == nil {
			return true
		}
		delete(s.tools, ev.Index)
		args := tool.input.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		var call provider.UnifiedToolCall
		call.ID, call.Type = tool.id, "function"
		call.Function.Name, call.Function.Arguments = tool.name, args
		return s.emit(provider.UnifiedSSEEvent{Type: "tool_call", ToolCalls: []provider.UnifiedToolCall{call}})
	case "message_delta":
		if ev.Usage != nil {
			// message_delta usage is cumulative; input counts appear here only on newer API versions.
			if ev.Usage.InputTokens > 0 {
				s.usage.PromptTokens = ev.Usage.InputTokens
			}
			s.usage.CompletionTokens = ev.Usage.OutputTokens
		}
		usage := s.usage
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		return s.emit(provider.UnifiedSSEEvent{
			Type:         "finish",
			FinishReason: ev.Delta.StopReason,
			Usage:        &usage,
			RawChunk:     []byte(fmt.Sprintf("event: message_delta\ndata: %s\n\n", data)),
		})
	case "message_stop":
		s.emit(provider.UnifiedSSEEvent{
			Type:     "done",
			RawChunk: []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"),
		})
		return false
	case "error":
		s.errs <- fmt.Errorf("anthropic stream error (%s): %s", ev.Error.Type, ev.Error.Message)
		return false
	}
	return true
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

	// Translate messages. Assistant tool calls become tool_use blocks and role "tool" results become
	// tool_result blocks in a user turn; consecutive turns with the same role are merged.
	system := []string{}
	if req.SystemPrompt != "" {
		system = append(system, req.SystemPrompt)
	}
	type turn struct {
		role   string
		blocks []map[string]interface{}
	}
	var turns []turn
	add := func(role string, blocks ...map[string]interface{}) {
		if len(blocks) == 0 {
			return
		}
		if n := len(turns); n > 0 && turns[n-1].role == role {
			turns[n-1].blocks = append(turns[n-1].blocks, blocks...)
			return
		}
		turns = append(turns, turn{role: role, blocks: blocks})
	}
	textBlock := func(text string) map[string]interface{} {
		return map[string]interface{}{"type": "text", "text": text}
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			if m.Content != "" && m.Content != req.SystemPrompt {
				system = append(system, m.Content)
			}
		case "assistant":
			var blocks []map[string]interface{}
			if m.Content != "" {
				blocks = append(blocks, textBlock(m.Content))
			}
			for _, tc := range m.ToolCalls {
				input := map[string]interface{}{}
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil || input == nil {
					input = map[string]interface{}{}
				}
				blocks = append(blocks, map[string]interface{}{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": input,
				})
			}
			add("assistant", blocks...)
		case "tool":
			if m.ToolCallID == "" {
				add("user", textBlock(m.Content))
				continue
			}
			add("user", map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     m.Content,
			})
		default:
			if m.Content != "" {
				add("user", textBlock(m.Content))
			}
		}
	}

	messages := make([]map[string]interface{}, 0, len(turns))
	for _, t := range turns {
		var content interface{} = t.blocks
		if len(t.blocks) == 1 && t.blocks[0]["type"] == "text" {
			content = t.blocks[0]["text"]
		}
		messages = append(messages, map[string]interface{}{"role": t.role, "content": content})
	}

	payload := map[string]interface{}{
		"model":      req.Model,
		"messages":   messages,
		"max_tokens": maxTokens,
		"stream":     stream,
	}

	if len(system) > 0 {
		payload["system"] = strings.Join(system, "\n\n")
	}
	if req.HasTemperature || req.Temperature > 0 {
		payload["temperature"] = req.Temperature
	}
	if req.HasTopP || req.TopP > 0 {
		payload["top_p"] = req.TopP
	}
	if len(req.Tools) > 0 {
		payload["tools"] = convertToolsToAnthropic(req.Tools)
	}
	if choice := convertToolChoiceToAnthropic(req.ToolChoice); choice != nil {
		payload["tool_choice"] = choice
	}

	return json.Marshal(payload)
}

// convertToolsToAnthropic maps OpenAI function tools to Anthropic tool definitions; tools already
// in Anthropic form pass through.
func convertToolsToAnthropic(tools []interface{}) []interface{} {
	out := make([]interface{}, 0, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		fn, isOpenAI := tm["function"].(map[string]interface{})
		if !isOpenAI {
			out = append(out, tm)
			continue
		}
		schema := fn["parameters"]
		if schema == nil {
			schema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		def := map[string]interface{}{"name": fn["name"], "input_schema": schema}
		if desc, ok := fn["description"].(string); ok && desc != "" {
			def["description"] = desc
		}
		out = append(out, def)
	}
	return out
}

// convertToolChoiceToAnthropic maps an OpenAI tool_choice ("auto", "none", "required", or a named
// function) to Anthropic's form. Anthropic-form choices pass through; nil means leave it unset.
func convertToolChoiceToAnthropic(choice interface{}) interface{} {
	switch c := choice.(type) {
	case string:
		switch c {
		case "auto":
			return map[string]interface{}{"type": "auto"}
		case "none":
			return map[string]interface{}{"type": "none"}
		case "required":
			return map[string]interface{}{"type": "any"}
		}
	case map[string]interface{}:
		if fn, ok := c["function"].(map[string]interface{}); ok {
			return map[string]interface{}{"type": "tool", "name": fn["name"]}
		}
		if _, ok := c["type"].(string); ok {
			return c
		}
	}
	return nil
}
