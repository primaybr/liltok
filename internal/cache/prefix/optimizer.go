package prefix

import (
	"encoding/json"
	"math"
)

const (
	// DefaultMinTokensForAnthropicCache is the minimum threshold required by Anthropic
	// for prompt caching on Claude 3.5 Sonnet / Claude 3.7.
	DefaultMinTokensForAnthropicCache = 1024
)

// EstimateTokens calculates an approximate token count for text using standard ~3.8 chars/token ratio.
func EstimateTokens(text string) int {
	if len(text) == 0 {
		return 0
	}
	tokens := int(math.Ceil(float64(len(text)) / 3.8))
	if tokens < 1 {
		return 1
	}
	return tokens
}

// EstimatePayloadTokens extracts approximate total prompt token count across system, messages, and tools.
func EstimatePayloadTokens(payload []byte) int {
	if len(payload) == 0 {
		return 0
	}

	var root map[string]interface{}
	if err := json.Unmarshal(payload, &root); err != nil {
		// Fallback to simple byte-length estimation if JSON is unparseable
		return len(payload) / 4
	}

	totalTokens := 0

	// System tokens
	if sys, ok := root["system"]; ok {
		switch s := sys.(type) {
		case string:
			totalTokens += EstimateTokens(s)
		case []interface{}:
			for _, b := range s {
				if bMap, ok := b.(map[string]interface{}); ok {
					if t, ok := bMap["text"].(string); ok {
						totalTokens += EstimateTokens(t)
					}
				}
			}
		}
	}

	// Messages tokens
	if msgs, ok := root["messages"].([]interface{}); ok {
		for _, m := range msgs {
			if msgMap, ok := m.(map[string]interface{}); ok {
				if c, ok := msgMap["content"]; ok {
					switch content := c.(type) {
					case string:
						totalTokens += EstimateTokens(content)
					case []interface{}:
						for _, p := range content {
							if pMap, ok := p.(map[string]interface{}); ok {
								if t, ok := pMap["text"].(string); ok {
									totalTokens += EstimateTokens(t)
								}
							}
						}
					}
				}
			}
		}
	}

	// Tools tokens
	if tools, ok := root["tools"].([]interface{}); ok {
		for _, tool := range tools {
			if toolBytes, err := json.Marshal(tool); err == nil {
				totalTokens += EstimateTokens(string(toolBytes))
			}
		}
	}

	return totalTokens
}

// PrefixOptimizer coordinates Tier-2 prefix and prompt caching injections.
type PrefixOptimizer struct {
	minTokens int
}

// NewPrefixOptimizer creates a new PrefixOptimizer.
func NewPrefixOptimizer(minTokens int) *PrefixOptimizer {
	if minTokens <= 0 {
		minTokens = DefaultMinTokensForAnthropicCache
	}
	return &PrefixOptimizer{minTokens: minTokens}
}

// OptimizeAnthropicPayload checks if the payload exceeds the minimum token threshold
// and injects ephemeral cache controls if applicable.
func (po *PrefixOptimizer) OptimizeAnthropicPayload(payload []byte) ([]byte, bool, error) {
	return InjectAnthropicCacheControl(payload, po.minTokens)
}
