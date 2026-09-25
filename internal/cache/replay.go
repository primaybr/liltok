package cache

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ReplayCacheHit writes a cached completion response to the downstream client with default TIER1_EXACT tier.
func ReplayCacheHit(w http.ResponseWriter, entry *CacheEntry, stream bool, isAnthropic bool, latencySavedMs int64) error {
	return ReplayCacheHitWithTier(w, entry, stream, isAnthropic, "TIER1_EXACT", latencySavedMs)
}

// ReplayCacheHitWithTier writes a cached completion response specifying the exact cache tier.
func ReplayCacheHitWithTier(w http.ResponseWriter, entry *CacheEntry, stream bool, isAnthropic bool, tier string, latencySavedMs int64) error {
	if tier == "" {
		tier = "TIER1_EXACT"
	}
	w.Header().Set("X-Liltok-Cache-Status", "HIT")
	w.Header().Set("X-Liltok-Cache-Tier", tier)
	w.Header().Set("X-Liltok-Provider", "cache-local")
	w.Header().Set("X-Liltok-Latency-Saved-Ms", fmt.Sprintf("%d", latencySavedMs))

	if !stream {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, err := w.Write(entry.ResponsePayload)
		return err
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("response writer does not support flushing")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	if isAnthropic {
		return replayAnthropicSSE(w, flusher, entry)
	}
	return replayOpenAISSE(w, flusher, entry)
}

func replayOpenAISSE(w http.ResponseWriter, flusher http.Flusher, entry *CacheEntry) error {
	var respObj struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	content := ""
	id := fmt.Sprintf("chatcmpl-cached-%x", time.Now().UnixMilli())
	model := entry.Model

	if err := json.Unmarshal(entry.ResponsePayload, &respObj); err == nil {
		if len(respObj.Choices) > 0 {
			content = respObj.Choices[0].Message.Content
		}
		if respObj.ID != "" {
			id = "cached-" + respObj.ID
		}
	} else {
		content = string(entry.ResponsePayload)
	}

	// 1. Role Delta
	chunkRole := fmt.Sprintf(`data: {"id":%q,"object":"chat.completion.chunk","created":%d,"model":%q,"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`+"\n\n",
		id, time.Now().Unix(), model)
	if _, err := w.Write([]byte(chunkRole)); err != nil {
		return err
	}
	flusher.Flush()

	// 2. Content Delta (Burst emit)
	escapedContent, _ := json.Marshal(content)
	chunkContent := fmt.Sprintf(`data: {"id":%q,"object":"chat.completion.chunk","created":%d,"model":%q,"choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`+"\n\n",
		id, time.Now().Unix(), model, string(escapedContent))
	if _, err := w.Write([]byte(chunkContent)); err != nil {
		return err
	}
	flusher.Flush()

	// 3. Stop Delta
	chunkStop := fmt.Sprintf(`data: {"id":%q,"object":"chat.completion.chunk","created":%d,"model":%q,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n",
		id, time.Now().Unix(), model)
	if _, err := w.Write([]byte(chunkStop)); err != nil {
		return err
	}
	flusher.Flush()

	// 4. DONE marker
	if _, err := w.Write([]byte("data: [DONE]\n\n")); err != nil {
		return err
	}
	flusher.Flush()

	return nil
}

func replayAnthropicSSE(w http.ResponseWriter, flusher http.Flusher, entry *CacheEntry) error {
	var respObj struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Content []struct {
			Type     string                 `json:"type"`
			Text     string                 `json:"text"`
			Thinking string                 `json:"thinking"`
			ID       string                 `json:"id"`
			Name     string                 `json:"name"`
			Input    map[string]interface{} `json:"input"`
		} `json:"content"`
	}

	id := fmt.Sprintf("msg-cached-%x", time.Now().UnixMilli())
	model := entry.Model
	stopReason := "end_turn"

	_ = json.Unmarshal(entry.ResponsePayload, &respObj)
	if respObj.ID != "" {
		id = "cached-" + respObj.ID
	}
	if respObj.Model != "" {
		model = respObj.Model
	}
	if respObj.StopReason != "" {
		stopReason = respObj.StopReason
	}
	// Claude Code sizes its context window from this usage, so report the payload's counts
	// and fall back to the entry's.
	inputTokens, outputTokens := respObj.Usage.InputTokens, respObj.Usage.OutputTokens
	if inputTokens == 0 {
		inputTokens = entry.PromptTokens
	}
	if outputTokens == 0 {
		outputTokens = entry.CompletionTokens
	}

	// 1. message_start
	eventStart := fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":%q,\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":%q,\"usage\":{\"input_tokens\":%d,\"output_tokens\":%d}}}\n\n",
		id, model, inputTokens, outputTokens)
	if _, err := w.Write([]byte(eventStart)); err != nil {
		return err
	}
	flusher.Flush()

	// 2. Iterate through content blocks
	for i, c := range respObj.Content {
		switch c.Type {
		case "thinking":
			eventBlockStart := fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n", i)
			if _, err := w.Write([]byte(eventBlockStart)); err != nil {
				return err
			}
			flusher.Flush()

			escapedThinking, _ := json.Marshal(c.Thinking)
			eventDelta := fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":%s}}\n\n",
				i, string(escapedThinking))
			if _, err := w.Write([]byte(eventDelta)); err != nil {
				return err
			}
			flusher.Flush()

			eventBlockStop := fmt.Sprintf("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", i)
			if _, err := w.Write([]byte(eventBlockStop)); err != nil {
				return err
			}
			flusher.Flush()

		case "text":
			eventBlockStart := fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n", i)
			if _, err := w.Write([]byte(eventBlockStart)); err != nil {
				return err
			}
			flusher.Flush()

			escapedText, _ := json.Marshal(c.Text)
			eventDelta := fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"text_delta\",\"text\":%s}}\n\n",
				i, string(escapedText))
			if _, err := w.Write([]byte(eventDelta)); err != nil {
				return err
			}
			flusher.Flush()

			eventBlockStop := fmt.Sprintf("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", i)
			if _, err := w.Write([]byte(eventBlockStop)); err != nil {
				return err
			}
			flusher.Flush()

		case "tool_use":
			eventBlockStart := fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"tool_use\",\"id\":%q,\"name\":%q,\"input\":{}}}\n\n",
				i, c.ID, c.Name)
			if _, err := w.Write([]byte(eventBlockStart)); err != nil {
				return err
			}
			flusher.Flush()

			inputJSONBytes, _ := json.Marshal(c.Input)
			escapedJSON, _ := json.Marshal(string(inputJSONBytes))
			eventDelta := fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":%s}}\n\n",
				i, string(escapedJSON))
			if _, err := w.Write([]byte(eventDelta)); err != nil {
				return err
			}
			flusher.Flush()

			eventBlockStop := fmt.Sprintf("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", i)
			if _, err := w.Write([]byte(eventBlockStop)); err != nil {
				return err
			}
			flusher.Flush()
		}
	}

	// 3. message_delta
	eventMsgDelta := fmt.Sprintf("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%q},\"usage\":{\"output_tokens\":%d}}\n\n",
		stopReason, outputTokens)
	if _, err := w.Write([]byte(eventMsgDelta)); err != nil {
		return err
	}
	flusher.Flush()

	// 4. message_stop
	eventStop := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	if _, err := w.Write([]byte(eventStop)); err != nil {
		return err
	}
	flusher.Flush()

	return nil
}
