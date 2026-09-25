package prefix

import (
	"encoding/json"
	"fmt"
)

// AnthropicUsage captures standard Anthropic token metrics including prompt caching tokens.
type AnthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// HasCacheHit returns true if any prompt tokens were read from Anthropic's prefix cache.
func (u AnthropicUsage) HasCacheHit() bool {
	return u.CacheReadInputTokens > 0
}

// CountExistingCacheControls counts total cache_control blocks in system, tools, and messages.
func CountExistingCacheControls(root map[string]interface{}) int {
	count := 0

	// Check system blocks
	if sys, ok := root["system"].([]interface{}); ok {
		for _, b := range sys {
			if bMap, ok := b.(map[string]interface{}); ok {
				if _, hasCC := bMap["cache_control"]; hasCC {
					count++
				}
			}
		}
	}

	// Check tools
	if tools, ok := root["tools"].([]interface{}); ok {
		for _, t := range tools {
			if tMap, ok := t.(map[string]interface{}); ok {
				if _, hasCC := tMap["cache_control"]; hasCC {
					count++
				}
			}
		}
	}

	// Check messages content blocks
	if msgs, ok := root["messages"].([]interface{}); ok {
		for _, m := range msgs {
			if mMap, ok := m.(map[string]interface{}); ok {
				if contentBlocks, ok := mMap["content"].([]interface{}); ok {
					for _, cb := range contentBlocks {
						if cbMap, ok := cb.(map[string]interface{}); ok {
							if _, hasCC := cbMap["cache_control"]; hasCC {
								count++
							}
						}
					}
				}
			}
		}
	}

	return count
}

// InjectAnthropicCacheControl inspects an Anthropic Messages API JSON payload.
// If estimated total tokens exceeds minTokens (default 1024), it attaches
// cache_control: {"type": "ephemeral"} without exceeding Anthropic's hard limit of 4 blocks.
func InjectAnthropicCacheControl(payload []byte, minTokens int) ([]byte, bool, error) {
	if len(payload) == 0 {
		return payload, false, nil
	}

	estTokens := EstimatePayloadTokens(payload)
	if estTokens < minTokens {
		return payload, false, nil
	}

	var root map[string]interface{}
	if err := json.Unmarshal(payload, &root); err != nil {
		return payload, false, fmt.Errorf("failed to parse anthropic payload: %w", err)
	}

	// If the payload already contains any cache_control blocks (e.g. from Claude Code or Cursor),
	// preserve the client's explicit caching strategy and TTL configurations without modification.
	if CountExistingCacheControls(root) > 0 {
		return payload, false, nil
	}
	remainingSlots := 4

	injected := false
	ephemeralControl := map[string]string{"type": "ephemeral"}

	// 1. Inject into System prompt
	if remainingSlots > 0 {
		if sys, hasSys := root["system"]; hasSys {
			switch v := sys.(type) {
			case string:
				if len(v) > 0 {
					root["system"] = []map[string]interface{}{
						{
							"type":          "text",
							"text":          v,
							"cache_control": ephemeralControl,
						},
					}
					injected = true
					remainingSlots--
				}
			case []interface{}:
				if len(v) > 0 {
					lastIdx := len(v) - 1
					if lastBlock, ok := v[lastIdx].(map[string]interface{}); ok {
						if _, already := lastBlock["cache_control"]; !already {
							lastBlock["cache_control"] = ephemeralControl
							v[lastIdx] = lastBlock
							injected = true
							remainingSlots--
						}
					}
				}
			}
		}
	}

	// 2. Inject into Tools
	if remainingSlots > 0 {
		if tools, hasTools := root["tools"].([]interface{}); hasTools && len(tools) > 0 {
			lastIdx := len(tools) - 1
			if lastTool, ok := tools[lastIdx].(map[string]interface{}); ok {
				if _, already := lastTool["cache_control"]; !already {
					lastTool["cache_control"] = ephemeralControl
					tools[lastIdx] = lastTool
					injected = true
				}
			}
		}
	}

	if !injected {
		return payload, false, nil
	}

	modifiedJSON, err := json.Marshal(root)
	if err != nil {
		return payload, false, err
	}

	return modifiedJSON, true, nil
}

// ParseAnthropicUsage extracts usage statistics from an Anthropic JSON response.
func ParseAnthropicUsage(raw []byte) (AnthropicUsage, error) {
	var root struct {
		Usage AnthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		return AnthropicUsage{}, err
	}
	return root.Usage, nil
}
