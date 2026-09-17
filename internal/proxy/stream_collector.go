package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// AnthropicStreamCollector accumulates streaming SSE events into a spec-compliant Anthropic Message object.
type AnthropicStreamCollector struct {
	msgID        string
	model        string
	stopReason   string
	inputTokens  int
	outputTokens int
	blocks       []*anthropicCollectedBlock
	blockIndex   map[int]*anthropicCollectedBlock
}

type anthropicCollectedBlock struct {
	Type     string
	Text     strings.Builder
	ID       string
	Name     string
	InputJSON strings.Builder
	Thinking strings.Builder
}

// NewAnthropicStreamCollector creates a new collector for Anthropic streams.
func NewAnthropicStreamCollector() *AnthropicStreamCollector {
	return &AnthropicStreamCollector{
		blockIndex: make(map[int]*anthropicCollectedBlock),
		stopReason: "end_turn",
	}
}

// FeedLine processes a single SSE data line (with "data: " prefix already trimmed).
func (c *AnthropicStreamCollector) FeedLine(data string) {
	if data == "[DONE]" || strings.TrimSpace(data) == "" {
		return
	}

	var raw struct {
		Type         string `json:"type"`
		Index        int    `json:"index"`
		Message      struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
			Text string `json:"text"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			Thinking    string `json:"thinking"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return
	}

	switch raw.Type {
	case "message_start":
		if raw.Message.ID != "" {
			c.msgID = raw.Message.ID
		}
		if raw.Message.Model != "" {
			c.model = raw.Message.Model
		}
		if raw.Message.Usage.InputTokens > 0 {
			c.inputTokens = raw.Message.Usage.InputTokens
		}
		if raw.Message.Usage.OutputTokens > 0 {
			c.outputTokens = raw.Message.Usage.OutputTokens
		}

	case "content_block_start":
		b := &anthropicCollectedBlock{
			Type: raw.ContentBlock.Type,
			ID:   raw.ContentBlock.ID,
			Name: raw.ContentBlock.Name,
		}
		if raw.ContentBlock.Text != "" {
			b.Text.WriteString(raw.ContentBlock.Text)
		}
		c.blockIndex[raw.Index] = b
		c.blocks = append(c.blocks, b)

	case "content_block_delta":
		b, exists := c.blockIndex[raw.Index]
		if !exists {
			// Fallback: create block if start was missed
			b = &anthropicCollectedBlock{Type: "text"}
			c.blockIndex[raw.Index] = b
			c.blocks = append(c.blocks, b)
		}

		if raw.Delta.Text != "" {
			b.Text.WriteString(raw.Delta.Text)
		}
		if raw.Delta.PartialJSON != "" {
			b.InputJSON.WriteString(raw.Delta.PartialJSON)
		}
		if raw.Delta.Thinking != "" {
			b.Thinking.WriteString(raw.Delta.Thinking)
		}

	case "message_delta":
		if raw.Delta.StopReason != "" {
			c.stopReason = raw.Delta.StopReason
		}
		if raw.Usage.OutputTokens > 0 {
			c.outputTokens = raw.Usage.OutputTokens
		}
	}
}

// HasContent returns true if the stream contained valid, non-empty content blocks.
func (c *AnthropicStreamCollector) HasContent() bool {
	for _, b := range c.blocks {
		if b.Type == "text" && b.Text.Len() > 0 {
			return true
		}
		if b.Type == "tool_use" && b.Name != "" {
			return true
		}
		if b.Type == "thinking" && b.Thinking.Len() > 0 {
			return true
		}
	}
	return false
}

// BuildMessage constructs a spec-compliant Anthropic Message JSON payload.
func (c *AnthropicStreamCollector) BuildMessage(defaultModel string) ([]byte, int, int) {
	msgID := c.msgID
	if msgID == "" {
		msgID = fmt.Sprintf("msg-stream-%x", time.Now().UnixMilli())
	}
	model := c.model
	if model == "" {
		model = defaultModel
	}

	var contentList []map[string]interface{}
	for _, b := range c.blocks {
		switch b.Type {
		case "text":
			textStr := b.Text.String()
			if textStr != "" {
				contentList = append(contentList, map[string]interface{}{
					"type": "text",
					"text": textStr,
				})
			}
		case "tool_use":
			var inputObj map[string]interface{}
			rawJSON := strings.TrimSpace(b.InputJSON.String())
			if rawJSON != "" {
				_ = json.Unmarshal([]byte(rawJSON), &inputObj)
			}
			if inputObj == nil {
				inputObj = make(map[string]interface{})
			}
			contentList = append(contentList, map[string]interface{}{
				"type":  "tool_use",
				"id":    b.ID,
				"name":  b.Name,
				"input": inputObj,
			})
		case "thinking":
			contentList = append(contentList, map[string]interface{}{
				"type":     "thinking",
				"thinking": b.Thinking.String(),
			})
		}
	}

	stopReason := c.stopReason
	if stopReason == "" {
		stopReason = "end_turn"
	}

	msg := map[string]interface{}{
		"id":            msgID,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       contentList,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]interface{}{
			"input_tokens":  c.inputTokens,
			"output_tokens": c.outputTokens,
		},
	}

	data, _ := json.Marshal(msg)
	return data, c.inputTokens, c.outputTokens
}

// OpenAIStreamCollector accumulates streaming SSE chunks into a standard ChatCompletion JSON object.
type OpenAIStreamCollector struct {
	id          string
	model       string
	accumulated strings.Builder
}

func NewOpenAIStreamCollector() *OpenAIStreamCollector {
	return &OpenAIStreamCollector{}
}

func (c *OpenAIStreamCollector) FeedLine(data string) {
	if data == "[DONE]" || strings.TrimSpace(data) == "" {
		return
	}

	var chunkObj struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}

	if err := json.Unmarshal([]byte(data), &chunkObj); err == nil {
		if chunkObj.ID != "" && c.id == "" {
			c.id = chunkObj.ID
		}
		if chunkObj.Model != "" && c.model == "" {
			c.model = chunkObj.Model
		}
		if len(chunkObj.Choices) > 0 {
			c.accumulated.WriteString(chunkObj.Choices[0].Delta.Content)
		}
	}
}

func (c *OpenAIStreamCollector) HasContent() bool {
	return c.accumulated.Len() > 0
}

func (c *OpenAIStreamCollector) BuildCompletion(defaultModel string) []byte {
	id := c.id
	if id == "" {
		id = fmt.Sprintf("chatcmpl-stream-%x", time.Now().UnixMilli())
	}
	model := c.model
	if model == "" {
		model = defaultModel
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"id":     id,
		"object": "chat.completion",
		"model":  model,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]string{
					"role":    "assistant",
					"content": c.accumulated.String(),
				},
				"finish_reason": "stop",
			},
		},
	})
	return payload
}
