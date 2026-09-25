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
	return t.ConvertOpenAIToAnthropicResponseForRequest(resp, nil, targetModel)
}

// ConvertOpenAIToAnthropicResponseForRequest is ConvertOpenAIToAnthropicResponse with the originating
// request, so request-dependent rewrites (Claude Code plan mode) can use the conversation history.
func (t *Translator) ConvertOpenAIToAnthropicResponseForRequest(resp *provider.UnifiedChatResponse, req *provider.UnifiedChatRequest, targetModel string) ([]byte, error) {
	msgID := fmt.Sprintf("msg_%s", resp.ID)
	if resp.ID == "" {
		msgID = fmt.Sprintf("msg_%x", time.Now().UnixMilli())
	}

	toolCalls := resp.ToolCalls
	textContent := resp.Content

	// 1. Extract thinking / reasoning blocks (<think>...</think> and resp.ReasoningContent)
	thinkingText, cleanText := extractThinkingBlocks(textContent)
	textContent = cleanText
	if resp.ReasoningContent != "" {
		if strings.TrimSpace(textContent) == strings.TrimSpace(resp.ReasoningContent) {
			textContent = ""
		}
		if thinkingText != "" {
			thinkingText = resp.ReasoningContent + "\n" + thinkingText
		} else {
			thinkingText = resp.ReasoningContent
		}
	}

	// 2. Fallback Interceptor: If model generated plain-text tool calls, extract them into structured tool calls
	if len(toolCalls) == 0 && textContent != "" {
		if cleanText2, extracted := extractTextToolCalls(textContent); len(extracted) > 0 {
			textContent = cleanText2
			toolCalls = extracted
		}
	}

	// 2.5. Claude Code Plan Mode Interceptor: Ensure implementation plan is presented in chat text
	textContent, toolCalls = handlePlanMode(textContent, thinkingText, toolCalls, extractPlanModeContext(req))

	// 3. Determine Stop Reason: Tool use MUST take priority over max_tokens to prevent Claude Code pause loops
	stopReason := "end_turn"
	if len(toolCalls) > 0 || resp.FinishReason == "tool_calls" {
		stopReason = "tool_use"
	} else if resp.FinishReason == "length" {
		stopReason = "max_tokens"
	}

	contentBlocks := make([]map[string]interface{}, 0, 2+len(toolCalls))

	// Prepend thinking block if present (Anthropic requires thinking before text / tool_use)
	if thinkingText != "" {
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type":     "thinking",
			"thinking": thinkingText,
		})
	}

	if textContent != "" {
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type": "text",
			"text": textContent,
		})
	}

	for _, tc := range toolCalls {
		var inputObj interface{}
		argsStr := strings.TrimSpace(tc.Function.Arguments)
		if err := json.Unmarshal([]byte(argsStr), &inputObj); err != nil {
			repaired := RepairJSON(argsStr)
			if err2 := json.Unmarshal([]byte(repaired), &inputObj); err2 != nil {
				inputObj = map[string]interface{}{}
			}
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

	// Matches unclosed DSML invoke tags terminating at end of string: <[|｜]DSML[|｜]invoke ...>...
	dsmlUnclosedInvokeRegex = regexp.MustCompile(`(?s)<[|｜]+DSML[|｜]+invoke\s+([^>]*?)>(.*)$`)

	// Matches DSML parameter tags with contents: <[|｜]DSML[|｜]parameter ...>...</[|｜]DSML[|｜]parameter>
	dsmlParamRegex = regexp.MustCompile(`(?s)<[|｜]+DSML[|｜]+parameter\s+([^>]*?)>(.*?)<\/[|｜]+DSML[|｜]+parameter>`)

	// Matches self-closing DSML parameter tags: <[|｜]DSML[|｜]parameter .../>
	dsmlParamSelfClosingRegex = regexp.MustCompile(`<[|｜]+DSML[|｜]+parameter\s+([^>]*?)\/>`)

	// Matches unclosed DSML parameter tag at end of string: <[|｜]DSML[|｜]parameter ...>...
	dsmlUnclosedParamRegex = regexp.MustCompile(`(?s)<[|｜]+DSML[|｜]+parameter\s+([^>]*?)>(.*)$`)

	// Extracts XML/DSML tag attributes: key="value", key='value', or key=value
	dsmlAttrRegex = regexp.MustCompile(`([a-zA-Z_:][-a-zA-Z0-9_:.]*)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)

	// Matches outer wrapper tags: <[|｜]DSML[|｜]tool_calls> and </[|｜]DSML[|｜]tool_calls>
	dsmlWrapperRegex = regexp.MustCompile(`<\/?(?:\||｜)+DSML(?:\||｜)+tool_calls?>`)

	// Matches any orphan or residual DSML tags: opening, closing, or self-closing
	dsmlOrphanTagRegex = regexp.MustCompile(`<\/?(?:\||｜)+DSML(?:\||｜)+[^>]*>`)

	// Matches standard closed <tool_call> JSON blocks (e.g. Qwen / Hermes / Llama)
	toolCallBlockRegex = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*<\/tool_call>`)

	// Matches unclosed <tool_call> block terminating at end of string
	unclosedToolCallBlockRegex = regexp.MustCompile(`(?s)<tool_call>\s*(.*)$`)

	// Matches markdown JSON or tool_call code blocks: ```json {...} ``` or unclosed
	markdownJSONBlockRegex = regexp.MustCompile("(?s)```(?:json|tool_call)?\\s*([{\\[].*?)(?:```|$)")

	// Matches markdown bash / sh code blocks: ```bash ... ``` or unclosed
	markdownBashRegex = regexp.MustCompile("(?s)```(?:bash|sh)\\s*(.*?)(?:```|$)")

	// Matches closed <think>...</think> blocks
	thinkBlockRegex = regexp.MustCompile(`(?s)<think>(.*?)<\/think>`)

	// Matches unclosed <think>... up to end of string
	unclosedThinkRegex = regexp.MustCompile(`(?s)<think>(.*)$`)
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
		// Fallback: detect unclosed DSML invoke tag terminating at end of string
		unclosedMatches := dsmlUnclosedInvokeRegex.FindStringSubmatchIndex(content)
		if len(unclosedMatches) >= 6 {
			attrStr := content[unclosedMatches[2]:unclosedMatches[3]]
			body := content[unclosedMatches[4]:unclosedMatches[5]]

			attrs := parseDSMLAttributes(attrStr)
			toolName := strings.TrimSpace(attrs["name"])
			if toolName != "" {
				argsMap := make(map[string]interface{})
				paramMatches := dsmlParamRegex.FindAllStringSubmatch(body, -1)
				for _, pm := range paramMatches {
					if len(pm) >= 3 {
						pAttrs := parseDSMLAttributes(pm[1])
						pName := strings.TrimSpace(pAttrs["name"])
						if pName != "" {
							argsMap[pName] = parseDSMLParamValue(pm[2], pAttrs["string"])
						}
					}
				}
				// Also check unclosed parameter tag at the end of body
				if upm := dsmlUnclosedParamRegex.FindStringSubmatch(body); len(upm) >= 3 {
					pAttrs := parseDSMLAttributes(upm[1])
					pName := strings.TrimSpace(pAttrs["name"])
					if pName != "" {
						if _, exists := argsMap[pName]; !exists {
							argsMap[pName] = parseDSMLParamValue(upm[2], pAttrs["string"])
						}
					}
				}
				argsBytes, err := json.Marshal(argsMap)
				argsStr := "{}"
				if err == nil {
					argsStr = string(argsBytes)
				}
				if toolName == "Bash" {
					if cmd, ok := argsMap["command"].(string); ok {
						repairedCmd := repairBashCommand(cmd)
						if repairedCmd != cmd {
							argsMap["command"] = repairedCmd
							if b, err := json.Marshal(argsMap); err == nil {
								argsStr = string(b)
							}
						}
					}
				}
				callID := fmt.Sprintf("call_dsml_%x_0", time.Now().UnixNano())
				tc := provider.UnifiedToolCall{
					ID:   callID,
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{
						Name:      toolName,
						Arguments: argsStr,
					},
				}
				cleaned := strings.TrimSpace(content[:unclosedMatches[0]])
				return cleaned, []provider.UnifiedToolCall{tc}
			}
		}
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
		if toolName == "Bash" {
			if cmd, ok := argsMap["command"].(string); ok {
				repairedCmd := repairBashCommand(cmd)
				if repairedCmd != cmd {
					argsMap["command"] = repairedCmd
					if b, err := json.Marshal(argsMap); err == nil {
						argsStr = string(b)
					}
				}
			}
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
	cleaned = dsmlUnclosedInvokeRegex.ReplaceAllString(cleaned, "")
	cleaned = dsmlParamRegex.ReplaceAllString(cleaned, "")
	cleaned = dsmlUnclosedParamRegex.ReplaceAllString(cleaned, "")
	cleaned = dsmlParamSelfClosingRegex.ReplaceAllString(cleaned, "")
	cleaned = dsmlWrapperRegex.ReplaceAllString(cleaned, "")
	cleaned = dsmlOrphanTagRegex.ReplaceAllString(cleaned, "")
	cleaned = strings.TrimSpace(cleaned)

	return cleaned, toolCalls
}

// StripDSMLTags removes any leaked or orphan DSML tags from output text.
func StripDSMLTags(content string) string {
	cleaned := dsmlInvokeRegex.ReplaceAllString(content, "")
	cleaned = dsmlUnclosedInvokeRegex.ReplaceAllString(cleaned, "")
	cleaned = dsmlParamRegex.ReplaceAllString(cleaned, "")
	cleaned = dsmlUnclosedParamRegex.ReplaceAllString(cleaned, "")
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
	// 1. Check for DSML markup (both closed and unclosed)
	if strings.Contains(content, "DSML") {
		if cleanText, tc := parseDSMLToolCalls(content); len(tc) > 0 {
			return cleanText, tc
		}
		// If DSML markup was detected but no valid calls could be extracted,
		// strip orphan tags so corrupted fragments do not leak to client
		cleaned := stripDSMLTags(content)
		return cleaned, nil
	}

	// 2. Check for <tool_call> blocks (both closed and unclosed)
	if strings.Contains(content, "<tool_call>") {
		if cleanText, tc := parseToolCallBlocks(content); len(tc) > 0 {
			return cleanText, tc
		}
	}

	// 3. Check for Markdown JSON tool calls (```json {"name": "Bash", ...} ```)
	if cleanText, tc := parseMarkdownJSONToolCalls(content); len(tc) > 0 {
		return cleanText, tc
	}

	// 4. Legacy fallback: "tool call: name(args)"
	if cleanText, tc := extractStandardTextToolCalls(content); len(tc) > 0 {
		return cleanText, tc
	}

	// 5. Fallback for Markdown Bash/Shell execution blocks (e.g. cat << 'EOF' > ... or commands)
	if cleanText, tc := parseMarkdownBashToolCalls(content); len(tc) > 0 {
		return cleanText, tc
	}

	return content, nil
}

func parseToolCallBlocks(content string) (string, []provider.UnifiedToolCall) {
	matches := toolCallBlockRegex.FindAllStringSubmatch(content, -1)
	var toolCalls []provider.UnifiedToolCall

	if len(matches) > 0 {
		for i, m := range matches {
			if len(m) < 2 {
				continue
			}
			rawJSON := strings.TrimSpace(m[1])
			repaired := RepairJSON(rawJSON)
			var parsed struct {
				Name      string      `json:"name"`
				Arguments interface{} `json:"arguments"`
				Input     interface{} `json:"input"`
			}
			if err := json.Unmarshal([]byte(repaired), &parsed); err != nil || parsed.Name == "" {
				continue
			}

			argsStr := "{}"
			argsObj := parsed.Arguments
			if argsObj == nil && parsed.Input != nil {
				argsObj = parsed.Input
			}
			switch a := argsObj.(type) {
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

		if len(toolCalls) > 0 {
			cleaned := toolCallBlockRegex.ReplaceAllString(content, "")
			return strings.TrimSpace(cleaned), toolCalls
		}
	}

	// Fallback: check unclosed <tool_call>... terminating at end of string
	if strings.Contains(content, "<tool_call>") {
		idx := strings.Index(content, "<tool_call>")
		raw := strings.TrimSpace(content[idx+len("<tool_call>"):])
		repaired := RepairJSON(raw)
		var parsed struct {
			Name      string      `json:"name"`
			Arguments interface{} `json:"arguments"`
			Input     interface{} `json:"input"`
		}
		if err := json.Unmarshal([]byte(repaired), &parsed); err == nil && parsed.Name != "" {
			argsStr := "{}"
			argsObj := parsed.Arguments
			if argsObj == nil && parsed.Input != nil {
				argsObj = parsed.Input
			}
			switch a := argsObj.(type) {
			case string:
				argsStr = a
			case map[string]interface{}:
				if b, err := json.Marshal(a); err == nil {
					argsStr = string(b)
				}
			}
			callID := fmt.Sprintf("call_tc_%x_0", time.Now().UnixNano())
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
			cleaned := strings.TrimSpace(content[:idx])
			return cleaned, toolCalls
		}
	}

	return content, nil
}

func parseMarkdownJSONToolCalls(content string) (string, []provider.UnifiedToolCall) {
	matches := markdownJSONBlockRegex.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return content, nil
	}

	var toolCalls []provider.UnifiedToolCall
	var removeRanges [][2]int

	for i, m := range matches {
		if len(m) < 4 {
			continue
		}
		raw := strings.TrimSpace(content[m[2]:m[3]])
		repaired := RepairJSON(raw)

		var single struct {
			Name       string      `json:"name"`
			Tool       string      `json:"tool"`
			Arguments  interface{} `json:"arguments"`
			Parameters interface{} `json:"parameters"`
			Input      interface{} `json:"input"`
		}
		if err := json.Unmarshal([]byte(repaired), &single); err == nil && (single.Name != "" || single.Tool != "") {
			tName := single.Name
			if tName == "" {
				tName = single.Tool
			}
			argsObj := single.Arguments
			if argsObj == nil {
				argsObj = single.Parameters
			}
			if argsObj == nil {
				argsObj = single.Input
			}
			argsStr := "{}"
			switch a := argsObj.(type) {
			case string:
				argsStr = a
			case map[string]interface{}:
				if b, err := json.Marshal(a); err == nil {
					argsStr = string(b)
				}
			}
			callID := fmt.Sprintf("call_mdjson_%x_%d", time.Now().UnixNano(), i)
			toolCalls = append(toolCalls, provider.UnifiedToolCall{
				ID:   callID,
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      tName,
					Arguments: argsStr,
				},
			})
			removeRanges = append(removeRanges, [2]int{m[0], m[1]})
		}
	}

	if len(toolCalls) == 0 {
		return content, nil
	}

	cleaned := content
	for i := len(removeRanges) - 1; i >= 0; i-- {
		start, end := removeRanges[i][0], removeRanges[i][1]
		cleaned = cleaned[:start] + cleaned[end:]
	}
	return strings.TrimSpace(cleaned), toolCalls
}

func parseMarkdownBashToolCalls(content string) (string, []provider.UnifiedToolCall) {
	lower := strings.ToLower(content)
	if strings.Contains(lower, "implementation plan") ||
		strings.Contains(lower, "exitplanmode") ||
		strings.Contains(lower, "exit_plan_mode") ||
		strings.Contains(lower, "plan mode") ||
		strings.Contains(lower, "# plan") {
		return content, nil
	}

	matches := markdownBashRegex.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return content, nil
	}

	var toolCalls []provider.UnifiedToolCall
	var removeRanges [][2]int

	for i, m := range matches {
		if len(m) < 4 {
			continue
		}
		cmd := strings.TrimSpace(content[m[2]:m[3]])
		if cmd == "" {
			continue
		}

		isExecutableCmd := strings.Contains(cmd, "cat <<") ||
			strings.Contains(cmd, "#!/") ||
			strings.Contains(cmd, "npm ") ||
			strings.Contains(cmd, "go ") ||
			strings.Contains(cmd, "chmod ") ||
			strings.Contains(cmd, "python") ||
			strings.Contains(cmd, "git ") ||
			strings.Contains(cmd, "./") ||
			strings.Contains(cmd, "bash ") ||
			strings.Contains(cmd, "sh ") ||
			strings.Contains(cmd, "curl ") ||
			strings.Contains(cmd, "make ") ||
			strings.Contains(cmd, "cargo ") ||
			strings.Contains(cmd, "pip ") ||
			strings.Contains(cmd, "echo ") ||
			strings.Contains(cmd, "mkdir ") ||
			strings.Contains(cmd, "rm ")

		if isExecutableCmd {
			repairedCmd := repairBashCommand(cmd)
			argsMap := map[string]string{
				"command": repairedCmd,
			}
			argsBytes, _ := json.Marshal(argsMap)
			callID := fmt.Sprintf("call_bash_%x_%d", time.Now().UnixNano(), i)
			toolCalls = append(toolCalls, provider.UnifiedToolCall{
				ID:   callID,
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      "Bash",
					Arguments: string(argsBytes),
				},
			})
			removeRanges = append(removeRanges, [2]int{m[0], m[1]})
		}
	}

	if len(toolCalls) == 0 {
		return content, nil
	}

	cleaned := content
	for i := len(removeRanges) - 1; i >= 0; i-- {
		start, end := removeRanges[i][0], removeRanges[i][1]
		cleaned = cleaned[:start] + cleaned[end:]
	}
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

func repairBashCommand(cmd string) string {
	heredocRegex := regexp.MustCompile(`(?m)cat\s*<<-?\s*['"]?([a-zA-Z0-9_]+)['"]?`)
	matches := heredocRegex.FindAllStringSubmatchIndex(cmd, -1)
	if len(matches) == 0 {
		return cmd
	}

	for _, m := range matches {
		if len(m) < 4 {
			continue
		}
		delim := cmd[m[2]:m[3]]
		afterHeredoc := cmd[m[1]:]
		delimRegex := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(delim) + `\s*$`)
		if !delimRegex.MatchString(afterHeredoc) {
			if !strings.HasSuffix(cmd, "\n") {
				cmd += "\n"
			}
			cmd += delim + "\n"
		}
	}
	return cmd
}

// RepairJSON attempts to repair truncated or malformed JSON payloads.
func RepairJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "{}"
	}

	var dummy interface{}
	if err := json.Unmarshal([]byte(raw), &dummy); err == nil {
		if objMap, ok := dummy.(map[string]interface{}); ok {
			if cmdVal, hasCmd := objMap["command"].(string); hasCmd {
				repairedCmd := repairBashCommand(cmdVal)
				if repairedCmd != cmdVal {
					objMap["command"] = repairedCmd
					if b, err := json.Marshal(objMap); err == nil {
						return string(b)
					}
				}
			}
		}
		return raw
	}

	// 1. Sanitize invalid escapes
	sanitized := sanitizeJSON(raw)
	if err := json.Unmarshal([]byte(sanitized), &dummy); err == nil {
		if objMap, ok := dummy.(map[string]interface{}); ok {
			if cmdVal, hasCmd := objMap["command"].(string); hasCmd {
				repairedCmd := repairBashCommand(cmdVal)
				if repairedCmd != cmdVal {
					objMap["command"] = repairedCmd
					if b, err := json.Marshal(objMap); err == nil {
						return string(b)
					}
				}
			}
		}
		return sanitized
	}

	// 2. State-machine tracking string quotes, escapes, and bracket/brace nesting
	var stack []rune
	inString := false
	isEscaped := false

	runes := []rune(sanitized)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if isEscaped {
			isEscaped = false
			continue
		}
		if r == '\\' {
			isEscaped = true
			continue
		}
		if r == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if r == '{' || r == '[' {
			stack = append(stack, r)
		} else if r == '}' {
			if len(stack) > 0 && stack[len(stack)-1] == '{' {
				stack = stack[:len(stack)-1]
			}
		} else if r == ']' {
			if len(stack) > 0 && stack[len(stack)-1] == '[' {
				stack = stack[:len(stack)-1]
			}
		}
	}

	var sb strings.Builder
	sb.WriteString(sanitized)
	if inString {
		if isEscaped {
			sb.WriteString(`\`)
		}
		sb.WriteString(`"`)
	}

	trimmed := strings.TrimRight(sb.String(), " \t\r\n")
	trimmed = strings.TrimSuffix(trimmed, ",")

	var closeSb strings.Builder
	closeSb.WriteString(trimmed)
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i] == '{' {
			closeSb.WriteString("}")
		} else if stack[i] == '[' {
			closeSb.WriteString("]")
		}
	}

	repaired := closeSb.String()
	if err := json.Unmarshal([]byte(repaired), &dummy); err == nil {
		if objMap, ok := dummy.(map[string]interface{}); ok {
			if cmdVal, hasCmd := objMap["command"].(string); hasCmd {
				repairedCmd := repairBashCommand(cmdVal)
				if repairedCmd != cmdVal {
					objMap["command"] = repairedCmd
					if b, err := json.Marshal(objMap); err == nil {
						return string(b)
					}
				}
			}
		}
		return repaired
	}

	return sanitized
}

func extractThinkingBlocks(content string) (string, string) {
	if !strings.Contains(content, "<think>") {
		return "", content
	}

	var thinkParts []string

	// 1. Extract closed <think>...</think>
	matches := thinkBlockRegex.FindAllStringSubmatch(content, -1)
	for _, m := range matches {
		if len(m) > 1 && strings.TrimSpace(m[1]) != "" {
			thinkParts = append(thinkParts, strings.TrimSpace(m[1]))
		}
	}
	cleaned := thinkBlockRegex.ReplaceAllString(content, "")

	// 2. Extract unclosed <think>... up to end of string
	if strings.Contains(cleaned, "<think>") {
		if m := unclosedThinkRegex.FindStringSubmatch(cleaned); len(m) > 1 {
			if strings.TrimSpace(m[1]) != "" {
				thinkParts = append(thinkParts, strings.TrimSpace(m[1]))
			}
		}
		cleaned = unclosedThinkRegex.ReplaceAllString(cleaned, "")
	}

	cleaned = strings.TrimSpace(cleaned)
	thinkingText := strings.Join(thinkParts, "\n\n")
	return thinkingText, cleaned
}

var (
	planHeaderRegex       = regexp.MustCompile(`(?is)(?:^|\n)(#{1,4}\s*(?:Implementation\s+)?Plan[^\n]*\n.*)`)
	numberedStepRegex     = regexp.MustCompile(`(?is)(?:^|\n)(1\.\s+[^\n]+(?:\n\s*\d+\.\s+[^\n]+)+.*)`)
	trailingThoughtsRegex = regexp.MustCompile(`(?i)\n+(?:now\s+i\s+(?:will|should|can)\s+call|calling\s+exitplanmode|i\s+am\s+ready\s+to\s+exit).*$`)
)

func isExitPlanMode(name string) bool {
	clean := strings.ToLower(strings.ReplaceAll(name, "_", ""))
	return clean == "exitplanmode"
}

func extractPlanFromThinking(thinking string) string {
	thinking = strings.TrimSpace(thinking)
	if thinking == "" {
		return ""
	}

	if m := planHeaderRegex.FindStringSubmatch(thinking); len(m) > 1 {
		plan := trailingThoughtsRegex.ReplaceAllString(m[1], "")
		return strings.TrimSpace(plan)
	}

	if m := numberedStepRegex.FindStringSubmatch(thinking); len(m) > 1 {
		plan := trailingThoughtsRegex.ReplaceAllString(m[1], "")
		return strings.TrimSpace(plan)
	}

	return thinking
}

func extractPlanFromArguments(argsStr string) (string, map[string]interface{}) {
	argsMap := make(map[string]interface{})
	argsStr = strings.TrimSpace(argsStr)
	if argsStr == "" {
		return "", argsMap
	}

	if err := json.Unmarshal([]byte(argsStr), &argsMap); err != nil {
		repaired := RepairJSON(argsStr)
		if err2 := json.Unmarshal([]byte(repaired), &argsMap); err2 != nil {
			var rawStr string
			if err3 := json.Unmarshal([]byte(argsStr), &rawStr); err3 == nil && strings.TrimSpace(rawStr) != "" {
				argsMap["plan"] = rawStr
				return strings.TrimSpace(rawStr), argsMap
			}
			return "", argsMap
		}
	}

	candidates := []string{
		"plan",
		"plan_content",
		"implementation_plan",
		"proposal",
		"content",
		"text",
		"description",
		"summary",
		"details",
		"response",
		"message",
	}

	for _, k := range candidates {
		if val, exists := argsMap[k]; exists {
			if s, ok := val.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s), argsMap
			}
		}
	}

	if stepsVal, exists := argsMap["steps"]; exists {
		switch steps := stepsVal.(type) {
		case string:
			if strings.TrimSpace(steps) != "" {
				return strings.TrimSpace(steps), argsMap
			}
		case []interface{}:
			var sb strings.Builder
			sb.WriteString("### Implementation Steps\n")
			for i, step := range steps {
				switch s := step.(type) {
				case string:
					sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, s))
				case map[string]interface{}:
					if title, ok := s["title"].(string); ok {
						sb.WriteString(fmt.Sprintf("%d. **%s**", i+1, title))
						if desc, ok := s["description"].(string); ok {
							sb.WriteString(fmt.Sprintf(": %s", desc))
						}
						sb.WriteString("\n")
					} else if desc, ok := s["description"].(string); ok {
						sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, desc))
					} else if st, ok := s["step"].(string); ok {
						sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, st))
					}
				}
			}
			result := strings.TrimSpace(sb.String())
			if result != "" && result != "### Implementation Steps" {
				return result, argsMap
			}
		}
	}

	// Fallback: search for any string value with length >= 20 or containing newlines/markdown
	for _, val := range argsMap {
		if s, ok := val.(string); ok {
			trimmed := strings.TrimSpace(s)
			if len(trimmed) >= 20 || strings.Contains(trimmed, "\n") || strings.Contains(trimmed, "#") {
				return trimmed, argsMap
			}
		}
	}

	return "", argsMap
}

// newPlanFileWrite builds the Write call that stores a plan in the Claude Code plan file.
func newPlanFileWrite(planPath, plan string) provider.UnifiedToolCall {
	argsBytes, _ := json.Marshal(map[string]string{"file_path": planPath, "content": plan})
	tc := provider.UnifiedToolCall{ID: fmt.Sprintf("call_plan_write_%x", time.Now().UnixNano()), Type: "function"}
	tc.Function.Name = "Write"
	tc.Function.Arguments = string(argsBytes)
	return tc
}

// handlePlanMode keeps Claude Code plan mode intact for models that skip steps: the plan is always
// shown as chat text, and an ExitPlanMode issued before the plan file exists is replaced with a Write
// of that file, because Claude Code reads the plan from the file and not from ExitPlanMode arguments.
func handlePlanMode(textContent, thinkingText string, toolCalls []provider.UnifiedToolCall, ctx PlanModeContext) (string, []provider.UnifiedToolCall) {
	needsPlanFile := ctx.Active && !ctx.PlanWritten && ctx.PlanPath != ""
	hasExitPlan := false
	for i, tc := range toolCalls {
		if isExitPlanMode(tc.Function.Name) {
			hasExitPlan = true
			toolCalls[i].Function.Name = "ExitPlanMode"

			planFromArgs, argsMap := extractPlanFromArguments(tc.Function.Arguments)
			if planFromArgs == "" && strings.TrimSpace(textContent) == "" && thinkingText != "" {
				planFromArgs = extractPlanFromThinking(thinkingText)
			}

			finalPlan := planFromArgs
			if finalPlan == "" && strings.TrimSpace(textContent) != "" {
				finalPlan = strings.TrimSpace(textContent)
			}

			if finalPlan != "" {
				if strings.TrimSpace(textContent) == "" {
					textContent = finalPlan
				} else if !strings.Contains(textContent, finalPlan) {
					textContent = strings.TrimSpace(textContent) + "\n\n" + finalPlan
				}
				if needsPlanFile {
					toolCalls[i] = newPlanFileWrite(ctx.PlanPath, finalPlan)
					continue
				}
				argsMap["plan"] = finalPlan
				if b, err := json.Marshal(argsMap); err == nil {
					toolCalls[i].Function.Arguments = string(b)
				}
			}
		}
	}

	// Only synthesize plan-mode calls in plan mode: outside it, text that merely discusses
	// ExitPlanMode or plan headers is an ordinary answer.
	if ctx.Active && !hasExitPlan && len(toolCalls) == 0 && textContent != "" {
		lower := strings.ToLower(textContent)
		hasExitPlanMention := strings.Contains(lower, "exitplanmode") ||
			strings.Contains(lower, "exit_plan_mode") ||
			strings.Contains(lower, "exit plan mode") ||
			strings.Contains(lower, "exiting plan mode")
		hasPlanStructure := strings.Contains(lower, "# plan") ||
			strings.Contains(lower, "implementation plan") ||
			strings.Contains(lower, "## plan") ||
			strings.Contains(lower, "### plan") ||
			strings.Contains(lower, "plan:")

		if hasExitPlanMention && hasPlanStructure && needsPlanFile {
			toolCalls = append(toolCalls, newPlanFileWrite(ctx.PlanPath, textContent))
		} else if hasExitPlanMention && hasPlanStructure {
			argsMap := map[string]interface{}{
				"plan": textContent,
			}
			argsBytes, _ := json.Marshal(argsMap)
			synthesizedTC := provider.UnifiedToolCall{
				ID:   fmt.Sprintf("call_exit_plan_%x", time.Now().UnixNano()),
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      "ExitPlanMode",
					Arguments: string(argsBytes),
				},
			}
			toolCalls = append(toolCalls, synthesizedTC)
		}
	}

	return textContent, toolCalls
}
