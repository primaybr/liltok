package tokens

import (
	"math"
	"strings"

	"github.com/pkoukk/tiktoken-go"
)

// CountTokens counts the exact tokens for text using OpenAI BPE encoding when possible,
// falling back to character-ratio heuristics for non-OpenAI or unknown models.
func CountTokens(model, text string) int {
	if len(text) == 0 {
		return 0
	}

	// 1. Try exact BPE encoding via tiktoken
	encoding, err := tiktoken.EncodingForModel(model)
	if err != nil {
		// Fallback to cl100k_base or o200k_base for generic gpt-style models
		if strings.HasPrefix(model, "gpt-4o") {
			encoding, _ = tiktoken.GetEncoding("o200k_base")
		} else {
			encoding, _ = tiktoken.GetEncoding("cl100k_base")
		}
	}

	if encoding != nil {
		tokenList := encoding.Encode(text, nil, nil)
		return len(tokenList)
	}

	// 2. Fallback heuristic for Claude, Gemini, and other models (~3.8 characters per token)
	return int(math.Ceil(float64(len(text)) / 3.8))
}

// TokenMessage represents an individual message for token estimation.
type TokenMessage struct {
	Role    string
	Content string
}

// CountMessagesTokens estimates token overhead for structured chat messages.
func CountMessagesTokens(model string, messages []TokenMessage) int {
	total := 0
	// Base priming tokens
	total += 3

	for _, m := range messages {
		// Per-message framing overhead (~3 tokens for <|start|>, role, <|end|>)
		total += 3
		total += CountTokens(model, m.Role)
		total += CountTokens(model, m.Content)
	}

	return total
}
