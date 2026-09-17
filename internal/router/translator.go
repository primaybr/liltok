package router

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/primaybr/liltok/internal/provider"
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

	toolCalls := resp.ToolCalls
	textContent := resp.Content

	// Fallback Interceptor: If model generated plain-text tool calls, extract them into structured tool calls
	if len(toolCalls) == 0 && textContent != "" {
		if cleanText, extracted := extractTextToolCalls(textContent); len(extracted) > 0 {
			textContent = cleanText
			toolCalls = extracted
		}
	}

	stopReason := "end_turn"
	if resp.FinishReason == "length" {
		stopReason = "max_tokens"
	} else if resp.FinishReason == "tool_calls" || len(toolCalls) > 0 {
		stopReason = "tool_use"
	}

	contentBlocks := make([]map[string]interface{}, 0, 1+len(toolCalls))
	if textContent != "" {
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type": "text",
			"text": textContent,
		})
	}
	for _, tc := range toolCalls {
		var inputObj interface{}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &inputObj); err != nil {
			inputObj = map[string]interface{}{}
		}
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type":  "tool_use",
			"id":    tc.ID,
			"name":  tc.Function.Name,
			"input": inputObj,
		})
	}
	if len(contentBlocks) == 0 {
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type": "text",
			"text": "",
		})
	}

	anthropicPayload := map[string]interface{}{
		"id":          msgID,
		"type":        "message",
		"role":        "assistant",
		"model":       targetModel,
		"stop_reason": stopReason,
		"content":     contentBlocks,
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

func extractTextToolCalls(content string) (string, []provider.UnifiedToolCall) {
	lower := strings.ToLower(content)
	tag := "tool call:"
	idx := strings.Index(lower, tag)
	if idx == -1 {
		return content, nil
	}

	afterTag := content[idx+len(tag):]
	openParen := strings.Index(afterTag, "(")
	if openParen == -1 {
		return content, nil
	}

	toolName := strings.TrimSpace(afterTag[:openParen])
	if toolName == "" {
		return content, nil
	}

	inside := afterTag[openParen+1:]
	lastParen := strings.LastIndex(inside, ")")
	if lastParen == -1 {
		return content, nil
	}

	rawJSON := strings.TrimSpace(inside[:lastParen])
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(rawJSON), &parsed); err != nil {
		sanitized := sanitizeJSON(rawJSON)
		if err := json.Unmarshal([]byte(sanitized), &parsed); err != nil {
			return content, nil
		}
		rawJSON = sanitized
	}

	cleanText := strings.TrimSpace(content[:idx])
	callID := fmt.Sprintf("call_%x", time.Now().UnixNano())

	tc := provider.UnifiedToolCall{
		ID:   callID,
		Type: "function",
		Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{
			Name:      toolName,
			Arguments: rawJSON,
		},
	}

	return cleanText, []provider.UnifiedToolCall{tc}
}

func sanitizeJSON(raw string) string {
	var sb strings.Builder
	runes := []rune(raw)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == '\\' && i+1 < len(runes) {
			next := runes[i+1]
			switch next {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't', 'u':
				sb.WriteRune(r)
				sb.WriteRune(next)
				i++
			default:
				sb.WriteString("\\\\")
			}
		} else {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
