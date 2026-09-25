package provider

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// ProviderTier classifies providers by operational cost.
type ProviderTier string

const (
	TierPremium ProviderTier = "premium"
	TierBudget  ProviderTier = "budget"
	TierFree    ProviderTier = "free"
)

// UnifiedToolCall represents a function/tool invocation request.
type UnifiedToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // e.g. "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// UnifiedChatMessage represents a normalized chat message.
type UnifiedChatMessage struct {
	Role       string            `json:"role"` // "system", "user", "assistant", "tool"
	Content    string            `json:"content"`
	Name       string            `json:"name,omitempty"`
	ToolCalls  []UnifiedToolCall `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
}

// UnifiedChatRequest represents a normalized chat completion request.
type UnifiedChatRequest struct {
	Model        string               `json:"model"`
	Messages     []UnifiedChatMessage `json:"messages"`
	SystemPrompt string               `json:"system_prompt,omitempty"`
	Temperature  float64              `json:"temperature"`
	TopP         float64              `json:"top_p"`
	// HasTemperature and HasTopP record that the client set the value, so an explicit 0 is kept.
	HasTemperature    bool          `json:"has_temperature,omitempty"`
	HasTopP           bool          `json:"has_top_p,omitempty"`
	MaxTokens         int           `json:"max_tokens,omitempty"`
	Stream            bool          `json:"stream"`
	Tools             []interface{} `json:"tools,omitempty"`
	ToolChoice        interface{}   `json:"tool_choice,omitempty"`
	IsAnthropicSource bool          `json:"is_anthropic_source"`
	RawPayload        []byte        `json:"raw_payload,omitempty"`
}

// UnifiedUsage details prompt, completion, and cached token consumption.
type UnifiedUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CachedTokens     int `json:"cached_tokens"`
}

// UnifiedChatResponse represents a normalized completion response.
type UnifiedChatResponse struct {
	ID               string            `json:"id"`
	Model            string            `json:"model"`
	Role             string            `json:"role"`
	Content          string            `json:"content"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ToolCalls        []UnifiedToolCall `json:"tool_calls,omitempty"`
	FinishReason     string            `json:"finish_reason"`
	Usage            UnifiedUsage      `json:"usage"`
	RawResponse      []byte            `json:"raw_response,omitempty"`
	Latency          time.Duration     `json:"latency"`
}

// UnifiedSSEEvent represents a streaming event chunk.
type UnifiedSSEEvent struct {
	// Type is "text_delta" or "thinking_delta" (DeltaText), "tool_call" (one complete call in
	// ToolCalls), "finish" (FinishReason and, when known, Usage) or "done".
	Type         string            `json:"type"`
	DeltaText    string            `json:"delta_text,omitempty"`
	Role         string            `json:"role,omitempty"`
	ToolCalls    []UnifiedToolCall `json:"tool_calls,omitempty"`
	FinishReason string            `json:"finish_reason,omitempty"`
	Usage        *UnifiedUsage     `json:"usage,omitempty"`
	RawChunk     []byte            `json:"raw_chunk,omitempty"`
}

// ProviderClient defines the interface implemented by all upstream LLM adapters.
type ProviderClient interface {
	Name() string
	Tier() ProviderTier
	SendChat(ctx context.Context, req *UnifiedChatRequest) (*UnifiedChatResponse, error)
	StreamChat(ctx context.Context, req *UnifiedChatRequest) (<-chan UnifiedSSEEvent, <-chan error, error)
	CheckHealth(ctx context.Context) (bool, error)
}

// ModelInfo represents metadata about an available model from a provider.
type ModelInfo struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	Active        bool   `json:"active"`
	ContextWindow int    `json:"context_window,omitempty"`
	OwnedBy       string `json:"owned_by,omitempty"`
}

// ModelLister is an optional interface for providers that can list available models.
type ModelLister interface {
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// ParseUnifiedRequest extracts a UnifiedChatRequest from either OpenAI or Anthropic raw JSON.
func ParseUnifiedRequest(bodyBytes []byte, isAnthropic bool) (*UnifiedChatRequest, error) {
	req := &UnifiedChatRequest{
		RawPayload:        bodyBytes,
		IsAnthropicSource: isAnthropic,
	}

	var rawMap map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &rawMap); err != nil {
		return nil, err
	}

	if m, ok := rawMap["model"].(string); ok {
		req.Model = m
	}
	if s, ok := rawMap["stream"].(bool); ok {
		req.Stream = s
	}
	if t, ok := rawMap["temperature"].(float64); ok {
		req.Temperature = t
		req.HasTemperature = true
	}
	if p, ok := rawMap["top_p"].(float64); ok {
		req.TopP = p
		req.HasTopP = true
	}
	if tc, ok := rawMap["tool_choice"]; ok && tc != nil {
		req.ToolChoice = tc
	}
	if mt, ok := rawMap["max_tokens"].(float64); ok {
		req.MaxTokens = int(mt)
	}
	if tools, ok := rawMap["tools"].([]interface{}); ok {
		req.Tools = tools
	}

	if isAnthropic {
		// Anthropic Messages Schema: system can be string or array
		if sys, exists := rawMap["system"]; exists {
			switch s := sys.(type) {
			case string:
				req.SystemPrompt = s
			case []interface{}:
				for _, item := range s {
					if itemMap, ok := item.(map[string]interface{}); ok {
						if txt, ok := itemMap["text"].(string); ok {
							req.SystemPrompt += txt
						}
					}
				}
			}
		}

		if msgs, ok := rawMap["messages"].([]interface{}); ok {
			for _, m := range msgs {
				if mMap, ok := m.(map[string]interface{}); ok {
					role, _ := mMap["role"].(string)
					content := ""
					var msgToolCalls []UnifiedToolCall
					var toolResultMsgs []UnifiedChatMessage

					switch c := mMap["content"].(type) {
					case string:
						content = c
					case []interface{}:
						for _, part := range c {
							if partMap, ok := part.(map[string]interface{}); ok {
								pType, _ := partMap["type"].(string)
								if txt, ok := partMap["text"].(string); ok {
									content += txt
								} else if pType == "thinking" {
									if thTxt, ok := partMap["thinking"].(string); ok && content == "" {
										content = thTxt
									}
								} else if pType == "tool_result" {
									toolUseID, _ := partMap["tool_use_id"].(string)
									resContent := ""
									if res, ok := partMap["content"].(string); ok {
										resContent = res
									} else if resArr, ok := partMap["content"].([]interface{}); ok {
										for _, rItem := range resArr {
											if rMap, ok := rItem.(map[string]interface{}); ok {
												if rTxt, ok := rMap["text"].(string); ok {
													resContent += rTxt
												}
											}
										}
									}
									toolResultMsgs = append(toolResultMsgs, UnifiedChatMessage{
										Role:       "tool",
										ToolCallID: toolUseID,
										Content:    resContent,
									})
								} else if pType == "tool_use" {
									name, _ := partMap["name"].(string)
									id, _ := partMap["id"].(string)
									inputBytes, _ := json.Marshal(partMap["input"])
									msgToolCalls = append(msgToolCalls, UnifiedToolCall{
										ID:   id,
										Type: "function",
										Function: struct {
											Name      string `json:"name"`
											Arguments string `json:"arguments"`
										}{
											Name:      name,
											Arguments: string(inputBytes),
										},
									})
								}
							}
						}
					}

					// Append any tool results extracted from this turn (in OpenAI protocol, tool results are individual messages)
					if len(toolResultMsgs) > 0 {
						req.Messages = append(req.Messages, toolResultMsgs...)
					}
					// If there is regular text or tool calls (e.g. user prompt or assistant response), append as corresponding role message
					if content != "" || len(msgToolCalls) > 0 || len(toolResultMsgs) == 0 {
						req.Messages = append(req.Messages, UnifiedChatMessage{
							Role:      role,
							Content:   content,
							ToolCalls: msgToolCalls,
						})
					}
				}
			}
		}
	} else {
		// Standard OpenAI Schema
		if msgs, ok := rawMap["messages"].([]interface{}); ok {
			for _, m := range msgs {
				if mMap, ok := m.(map[string]interface{}); ok {
					role, _ := mMap["role"].(string)
					content := openAIContentText(mMap["content"])
					if role == "system" && req.SystemPrompt == "" {
						req.SystemPrompt = content
					}
					msg := UnifiedChatMessage{Role: role, Content: content}
					msg.Name, _ = mMap["name"].(string)
					msg.ToolCallID, _ = mMap["tool_call_id"].(string)
					if calls, ok := mMap["tool_calls"].([]interface{}); ok {
						msg.ToolCalls = parseOpenAIToolCalls(calls)
					}
					req.Messages = append(req.Messages, msg)
				}
			}
		}
	}

	return req, nil
}

// openAIContentText returns an OpenAI message's text, from a plain string or from the text parts
// of a content array. Non-text parts (images, audio) are dropped.
func openAIContentText(content interface{}) string {
	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		var sb strings.Builder
		for _, part := range c {
			if pm, ok := part.(map[string]interface{}); ok {
				if txt, ok := pm["text"].(string); ok {
					sb.WriteString(txt)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// parseOpenAIToolCalls converts an assistant message's OpenAI tool_calls. Arguments sent as a JSON
// object instead of the usual string are re-encoded as a string.
func parseOpenAIToolCalls(calls []interface{}) []UnifiedToolCall {
	var out []UnifiedToolCall
	for _, c := range calls {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		fn, _ := cm["function"].(map[string]interface{})
		var tc UnifiedToolCall
		tc.ID, _ = cm["id"].(string)
		tc.Type = "function"
		tc.Function.Name, _ = fn["name"].(string)
		switch args := fn["arguments"].(type) {
		case string:
			tc.Function.Arguments = args
		case nil:
			tc.Function.Arguments = "{}"
		default:
			b, _ := json.Marshal(args)
			tc.Function.Arguments = string(b)
		}
		out = append(out, tc)
	}
	return out
}
