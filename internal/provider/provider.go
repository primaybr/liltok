package provider

import (
	"context"
	"encoding/json"
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
	Model             string               `json:"model"`
	Messages          []UnifiedChatMessage `json:"messages"`
	SystemPrompt      string               `json:"system_prompt,omitempty"`
	Temperature       float64              `json:"temperature"`
	TopP              float64              `json:"top_p"`
	MaxTokens         int                  `json:"max_tokens,omitempty"`
	Stream            bool                 `json:"stream"`
	Tools             []interface{}        `json:"tools,omitempty"`
	ToolChoice        interface{}          `json:"tool_choice,omitempty"`
	IsAnthropicSource bool                 `json:"is_anthropic_source"`
	RawPayload        []byte               `json:"raw_payload,omitempty"`
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
	ID           string            `json:"id"`
	Model        string            `json:"model"`
	Role         string            `json:"role"`
	Content      string            `json:"content"`
	ToolCalls    []UnifiedToolCall `json:"tool_calls,omitempty"`
	FinishReason string            `json:"finish_reason"`
	Usage        UnifiedUsage      `json:"usage"`
	RawResponse  []byte            `json:"raw_response,omitempty"`
	Latency      time.Duration     `json:"latency"`
}

// UnifiedSSEEvent represents a streaming event chunk.
type UnifiedSSEEvent struct {
	Type         string            `json:"type"` // "text_delta", "tool_delta", "finish", "done"
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
	}
	if p, ok := rawMap["top_p"].(float64); ok {
		req.TopP = p
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
					switch c := mMap["content"].(type) {
					case string:
						content = c
					case []interface{}:
						for _, part := range c {
							if partMap, ok := part.(map[string]interface{}); ok {
								if txt, ok := partMap["text"].(string); ok {
									content += txt
								} else if pType, ok := partMap["type"].(string); ok {
									if pType == "tool_result" {
										if res, ok := partMap["content"].(string); ok {
											content += res
										} else if resArr, ok := partMap["content"].([]interface{}); ok {
											for _, rItem := range resArr {
												if rMap, ok := rItem.(map[string]interface{}); ok {
													if rTxt, ok := rMap["text"].(string); ok {
														content += rTxt
													}
												}
											}
										}
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
					}
					req.Messages = append(req.Messages, UnifiedChatMessage{
						Role:      role,
						Content:   content,
						ToolCalls: msgToolCalls,
					})
				}
			}
		}
	} else {
		// Standard OpenAI Schema
		if msgs, ok := rawMap["messages"].([]interface{}); ok {
			for _, m := range msgs {
				if mMap, ok := m.(map[string]interface{}); ok {
					role, _ := mMap["role"].(string)
					content, _ := mMap["content"].(string)
					if role == "system" && req.SystemPrompt == "" {
						req.SystemPrompt = content
					}
					req.Messages = append(req.Messages, UnifiedChatMessage{
						Role:    role,
						Content: content,
					})
				}
			}
		}
	}

	return req, nil
}
