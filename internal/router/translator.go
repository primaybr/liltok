package router

import (
	"encoding/json"
	"fmt"
	"html"
	"regexp"
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

var (
	// Matches full DSML invoke blocks: <[|｜]DSML[|｜]invoke ...>...</[|｜]DSML[|｜]invoke>
	dsmlInvokeRegex = regexp.MustCompile(`(?s)<[|｜]+DSML[|｜]+invoke\s+([^>]*?)>(.*?)<\/[|｜]+DSML[|｜]+invoke>`)

	// Matches DSML parameter tags with contents: <[|｜]DSML[|｜]parameter ...>...</[|｜]DSML[|｜]parameter>
	dsmlParamRegex = regexp.MustCompile(`(?s)<[|｜]+DSML[|｜]+parameter\s+([^>]*?)>(.*?)<\/[|｜]+DSML[|｜]+parameter>`)

	// Matches self-closing DSML parameter tags: <[|｜]DSML[|｜]parameter .../>
	dsmlParamSelfClosingRegex = regexp.MustCompile(`<[|｜]+DSML[|｜]+parameter\s+([^>]*?)\/>`)

	// Extracts XML/DSML tag attributes: key="value", key='value', or key=value
	dsmlAttrRegex = regexp.MustCompile(`([a-zA-Z_:][-a-zA-Z0-9_:.]*)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)

	// Matches outer wrapper tags: <[|｜]DSML[|｜]tool_calls> and </[|｜]DSML[|｜]tool_calls>
	dsmlWrapperRegex = regexp.MustCompile(`<\/?(?:\||｜)+DSML(?:\||｜)+tool_calls?>`)

	// Matches any orphan or residual DSML tags: opening, closing, or self-closing
	dsmlOrphanTagRegex = regexp.MustCompile(`<\/?(?:\||｜)+DSML(?:\||｜)+[^>]*>`)

	// Matches standard <tool_call> JSON blocks (e.g. Qwen / Hermes / Llama)
	toolCallBlockRegex = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*<\/tool_call>`)
)

func parseDSMLAttributes(attrStr string) map[string]string {
	attrs := make(map[string]string)
	matches := dsmlAttrRegex.FindAllStringSubmatch(attrStr, -1)
	for _, m := range matches {
		if len(m) >= 5 {
			k := strings.ToLower(m[1])
			val := m[2]
			if val == "" {
				val = m[3]
			}
			if val == "" {
				val = m[4]
			}
			attrs[k] = val
		}
	}
	return attrs
}

func parseDSMLParamValue(rawVal, isStringAttr string) interface{} {
	val := html.UnescapeString(rawVal)
	val = strings.TrimSpace(val)

	if strings.ToLower(isStringAttr) == "true" {
		return val
	}

	if val == "null" {
		return nil
	}
	if val == "true" {
		return true
	}
	if val == "false" {
		return false
	}

	var jsonVal interface{}
	if err := json.Unmarshal([]byte(val), &jsonVal); err == nil {
		return jsonVal
	}

	return val
}

func parseDSMLToolCalls(content string) (string, []provider.UnifiedToolCall) {
	invokeMatches := dsmlInvokeRegex.FindAllStringSubmatchIndex(content, -1)
	if len(invokeMatches) == 0 {
		return "", nil
	}

	var toolCalls []provider.UnifiedToolCall
	for i, idx := range invokeMatches {
		attrStr := content[idx[2]:idx[3]]
		body := content[idx[4]:idx[5]]

		attrs := parseDSMLAttributes(attrStr)
		toolName := strings.TrimSpace(attrs["name"])
		if toolName == "" {
			continue
		}

		argsMap := make(map[string]interface{})

		// Extract standard parameters
		paramMatches := dsmlParamRegex.FindAllStringSubmatch(body, -1)
		for _, pm := range paramMatches {
			if len(pm) < 3 {
				continue
			}
			pAttrs := parseDSMLAttributes(pm[1])
			pName := strings.TrimSpace(pAttrs["name"])
			if pName == "" {
				continue
			}
			argsMap[pName] = parseDSMLParamValue(pm[2], pAttrs["string"])
		}

		// Extract self-closing parameters
		selfMatches := dsmlParamSelfClosingRegex.FindAllStringSubmatch(body, -1)
		for _, scm := range selfMatches {
			if len(scm) < 2 {
				continue
			}
			pAttrs := parseDSMLAttributes(scm[1])
			pName := strings.TrimSpace(pAttrs["name"])
			if pName == "" {
				continue
			}
			argsMap[pName] = parseDSMLParamValue("", pAttrs["string"])
		}

		argsBytes, err := json.Marshal(argsMap)
		argsStr := "{}"
		if err == nil {
			argsStr = string(argsBytes)
		}

		callID := fmt.Sprintf("call_dsml_%x_%d", time.Now().UnixNano(), i)
		toolCalls = append(toolCalls, provider.UnifiedToolCall{
			ID:   callID,
			Type: "function",
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{
				Name:      toolName,
				Arguments: argsStr,
			},
		})
	}

	if len(toolCalls) == 0 {
		return "", nil
	}

	cleaned := dsmlInvokeRegex.ReplaceAllString(content, "")
	cleaned = dsmlParamRegex.ReplaceAllString(cleaned, "")
	cleaned = dsmlParamSelfClosingRegex.ReplaceAllString(cleaned, "")
	cleaned = dsmlWrapperRegex.ReplaceAllString(cleaned, "")
	cleaned = dsmlOrphanTagRegex.ReplaceAllString(cleaned, "")
	cleaned = strings.TrimSpace(cleaned)

	return cleaned, toolCalls
}

// StripDSMLTags removes any leaked or orphan DSML tags from output text.
func StripDSMLTags(content string) string {
	cleaned := dsmlInvokeRegex.ReplaceAllString(content, "")
	cleaned = dsmlParamRegex.ReplaceAllString(cleaned, "")
	cleaned = dsmlParamSelfClosingRegex.ReplaceAllString(cleaned, "")
	cleaned = dsmlWrapperRegex.ReplaceAllString(cleaned, "")
	cleaned = dsmlOrphanTagRegex.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}

func stripDSMLTags(content string) string {
	return StripDSMLTags(content)
}

// ExtractTextToolCalls extracts embedded plain-text or DSML tool calls from model output.
func ExtractTextToolCalls(content string) (string, []provider.UnifiedToolCall) {
	return extractTextToolCalls(content)
}

func extractTextToolCalls(content string) (string, []provider.UnifiedToolCall) {
	// 1. Check for DSML markup
	if strings.Contains(content, "DSML") {
		if cleanText, tc := parseDSMLToolCalls(content); len(tc) > 0 {
			return cleanText, tc
		}
		// If DSML markup was detected but no valid calls could be extracted,
		// strip orphan tags so corrupted fragments do not leak to client
		cleaned := stripDSMLTags(content)
		return cleaned, nil
	}

	// 2. Check for <tool_call> blocks (e.g. Qwen / Hermes / Llama)
	if strings.Contains(content, "<tool_call>") {
		if cleanText, tc := parseToolCallBlocks(content); len(tc) > 0 {
			return cleanText, tc
		}
	}

	// 3. Legacy fallback: "tool call: name(args)"
	return extractStandardTextToolCalls(content)
}

func parseToolCallBlocks(content string) (string, []provider.UnifiedToolCall) {
	matches := toolCallBlockRegex.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return content, nil
	}

	var toolCalls []provider.UnifiedToolCall
	for i, m := range matches {
		if len(m) < 2 {
			continue
		}
		rawJSON := strings.TrimSpace(m[1])
		var parsed struct {
			Name      string      `json:"name"`
			Arguments interface{} `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(rawJSON), &parsed); err != nil || parsed.Name == "" {
			continue
		}

		argsStr := "{}"
		switch a := parsed.Arguments.(type) {
		case string:
			argsStr = a
		case map[string]interface{}:
			if b, err := json.Marshal(a); err == nil {
				argsStr = string(b)
			}
		}

		callID := fmt.Sprintf("call_tc_%x_%d", time.Now().UnixNano(), i)
		toolCalls = append(toolCalls, provider.UnifiedToolCall{
			ID:   callID,
			Type: "function",
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{
				Name:      parsed.Name,
				Arguments: argsStr,
			},
		})
	}

	if len(toolCalls) == 0 {
		return content, nil
	}

	cleaned := toolCallBlockRegex.ReplaceAllString(content, "")
	return strings.TrimSpace(cleaned), toolCalls
}

func extractStandardTextToolCalls(content string) (string, []provider.UnifiedToolCall) {
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
