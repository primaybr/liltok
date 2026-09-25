package tokens

import (
	"math"
	"strings"
	"sync"
	"time"

	"github.com/pkoukk/tiktoken-go"
)

// encodingRetryAfter bounds how often a failed encoding load (the BPE ranks are downloaded on first
// use) is retried, so an offline gateway does not attempt the download on every request.
const encodingRetryAfter = time.Minute

type encoderEntry struct {
	enc      *tiktoken.Tiktoken
	failedAt time.Time
}

var (
	encodersMu sync.Mutex
	encoders   = map[string]encoderEntry{}
)

// encodingNameForModel mirrors tiktoken.EncodingForModel, falling back to o200k_base for gpt-4o
// variants and cl100k_base for every other model.
func encodingNameForModel(model string) string {
	if name, ok := tiktoken.MODEL_TO_ENCODING[model]; ok {
		return name
	}
	for prefix, name := range tiktoken.MODEL_PREFIX_TO_ENCODING {
		if strings.HasPrefix(model, prefix) {
			return name
		}
	}
	if strings.HasPrefix(model, "gpt-4o") {
		return "o200k_base"
	}
	return "cl100k_base"
}

// encoderFor returns a shared encoder for the model's encoding. Building one sorts the full
// vocabulary, so it is done once per encoding; Encode on the shared value is safe for concurrent use.
func encoderFor(model string) *tiktoken.Tiktoken {
	name := encodingNameForModel(model)
	encodersMu.Lock()
	defer encodersMu.Unlock()
	if e, ok := encoders[name]; ok && (e.enc != nil || time.Since(e.failedAt) < encodingRetryAfter) {
		return e.enc
	}
	enc, err := tiktoken.GetEncoding(name)
	if err != nil {
		encoders[name] = encoderEntry{failedAt: time.Now()}
		return nil
	}
	encoders[name] = encoderEntry{enc: enc}
	return enc
}

// CountTokens counts the exact tokens for text using OpenAI BPE encoding when possible,
// falling back to character-ratio heuristics when no encoding can be loaded.
func CountTokens(model, text string) int {
	if len(text) == 0 {
		return 0
	}

	// 1. Exact BPE encoding via a shared tiktoken encoder
	if encoding := encoderFor(model); encoding != nil {
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
