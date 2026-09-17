package router

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/liltok/liltok/internal/provider"
)

// Translator provides cross-protocol conversions between Anthropic Messages API and OpenAI ChatCompletions.
type Translator struct{}

// NewTranslator creates a new Translator instance.
func NewTranslator() *Translator {
	return &Translator{}
}

// ConvertOpenAIToAnthropicResponse transforms a standard OpenAI response into an Anthropic Messages response.
func (t *Translator) ConvertOpenAIToAnthropicResponse(resp *provider.UnifiedChatResponse, targetModel string) ([]byte, error) {
	msgID := fmt.Sprintf("msg_%s", resp.ID)
	if resp.ID == "" {
		msgID = fmt.Sprintf("msg_%x", time.Now().UnixMilli())
	}

	stopReason := "end_turn"
	if resp.FinishReason == "length" {
		stopReason = "max_tokens"
	} else if resp.FinishReason == "tool_calls" {
		stopReason = "tool_use"
	}

	anthropicPayload := map[string]interface{}{
		"id":          msgID,
		"type":        "message",
		"role":        "assistant",
		"model":       targetModel,
		"stop_reason": stopReason,
		"content": []map[string]interface{}{
			{
				"type": "text",
				"text": resp.Content,
			},
		},
		"usage": map[string]int{
			"input_tokens":  resp.Usage.PromptTokens,
			"output_tokens": resp.Usage.CompletionTokens,
		},
	}

	return json.Marshal(anthropicPayload)
}

// StreamOpenAIToAnthropicEvents converts incoming UnifiedSSEEvents into Anthropic-compliant SSE byte chunks.
func (t *Translator) StreamOpenAIToAnthropicEvents(in <-chan provider.UnifiedSSEEvent, out chan<- []byte, targetModel string) {
	defer close(out)

	msgID := fmt.Sprintf("msg_%x", time.Now().UnixMilli())

	// 1. Emit message_start
	startEvent := fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":%q,\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":%q,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		msgID, targetModel)
	out <- []byte(startEvent)

	// 2. Emit content_block_start
	blockStart := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"
	out <- []byte(blockStart)

	totalOutputTokens := 0

	for ev := range in {
		if ev.Type == "text_delta" && ev.DeltaText != "" {
			totalOutputTokens++
			escapedText, _ := json.Marshal(ev.DeltaText)
			deltaEvent := fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%s}}\n\n",
				string(escapedText))
			out <- []byte(deltaEvent)
		} else if ev.Type == "finish" {
			// 3. Emit content_block_stop
			out <- []byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")

			// 4. Emit message_delta
			stopReason := "end_turn"
			if ev.FinishReason == "length" {
				stopReason = "max_tokens"
			}
			msgDelta := fmt.Sprintf("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%q},\"usage\":{\"output_tokens\":%d}}\n\n",
				stopReason, totalOutputTokens)
			out <- []byte(msgDelta)
		}
	}

	// 5. Emit final message_stop
	out <- []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
}
