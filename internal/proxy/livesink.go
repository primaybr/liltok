package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/primaybr/liltok/internal/provider"
	"github.com/primaybr/liltok/internal/router"
	"github.com/primaybr/liltok/internal/telemetry"
)

// anthropicLiveSink writes a routed reply to an Anthropic-format streaming client as the router
// commits it (routes.live_streaming). The reply's ending (text held back by the router, tool
// calls, stop reason, usage) comes from the same translator output the non-live replay uses, so
// both paths send the client the same content; finalBytes keeps that output for logging and the
// cache.
type anthropicLiveSink struct {
	w          http.ResponseWriter
	ka         *keepAliveWriter // nil when the response is not wrapped
	translator *router.Translator
	req        *provider.UnifiedChatRequest
	reqID      string
	inputEst   int

	committed  bool
	nextIndex  int
	textOpen   bool
	textIndex  int
	sent       strings.Builder
	finalBytes []byte
}

func newAnthropicLiveSink(w http.ResponseWriter, translator *router.Translator, req *provider.UnifiedChatRequest, reqID string) *anthropicLiveSink {
	s := &anthropicLiveSink{w: w, translator: translator, req: req, reqID: reqID}
	if ka, ok := w.(*keepAliveWriter); ok {
		s.ka = ka
	}
	s.inputEst = len(req.RawPayload) / 4
	return s
}

func (s *anthropicLiveSink) event(name string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, data); err != nil {
		return err
	}
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (s *anthropicLiveSink) Commit() error {
	s.committed = true
	headersSent := false
	if s.ka != nil {
		s.ka.stop()
		headersSent = s.ka.committed
	}
	if !headersSent {
		h := s.w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Liltok-Request-Id", s.reqID)
		h.Set("X-Liltok-Cache-Status", "MISS")
		h.Set("Trailer", "X-Liltok-Provider, X-Liltok-Cache-Tier")
		s.w.WriteHeader(http.StatusOK)
	}
	return s.event("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id": "msg_live_" + s.reqID, "type": "message", "role": "assistant", "content": []interface{}{},
			"model": s.req.Model, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]int{"input_tokens": s.inputEst, "output_tokens": 0},
		},
	})
}

func (s *anthropicLiveSink) Thinking(text string) error {
	i := s.nextIndex
	s.nextIndex++
	if err := s.event("content_block_start", map[string]interface{}{"type": "content_block_start", "index": i, "content_block": map[string]string{"type": "thinking", "thinking": ""}}); err != nil {
		return err
	}
	if err := s.event("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": i, "delta": map[string]string{"type": "thinking_delta", "thinking": text}}); err != nil {
		return err
	}
	return s.event("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": i})
}

func (s *anthropicLiveSink) openText() error {
	if s.textOpen {
		return nil
	}
	s.textOpen, s.textIndex = true, s.nextIndex
	s.nextIndex++
	return s.event("content_block_start", map[string]interface{}{"type": "content_block_start", "index": s.textIndex, "content_block": map[string]string{"type": "text", "text": ""}})
}

func (s *anthropicLiveSink) textDelta(text string) error {
	if err := s.openText(); err != nil {
		return err
	}
	return s.event("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": s.textIndex, "delta": map[string]string{"type": "text_delta", "text": text}})
}

func (s *anthropicLiveSink) closeText() error {
	if !s.textOpen {
		return nil
	}
	s.textOpen = false
	return s.event("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": s.textIndex})
}

func (s *anthropicLiveSink) Text(delta string) error {
	if err := s.textDelta(delta); err != nil {
		return err
	}
	s.sent.WriteString(delta)
	return nil
}

// Finish sends the rest of the validated reply: the text the router held back, any further
// blocks (tool calls), and the stop with final usage.
func (s *anthropicLiveSink) Finish(resp *provider.UnifiedChatResponse, providerName string) error {
	final, err := s.translator.ConvertOpenAIToAnthropicResponseForRequest(resp, s.req, s.req.Model)
	if err != nil {
		s.Fail(err)
		return err
	}
	s.finalBytes = final
	var msg struct {
		StopReason string                   `json:"stop_reason"`
		Content    []map[string]interface{} `json:"content"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(final, &msg); err != nil {
		s.Fail(err)
		return err
	}

	sent := s.sent.String()
	textDone := false
	for _, block := range msg.Content {
		typ, _ := block["type"].(string)
		switch {
		case typ == "thinking" || typ == "redacted_thinking":
			// Thinking is sent at commit; reasoning that arrives later is kept in the cached reply.
		case typ == "text" && !textDone:
			textDone = true
			text, _ := block["text"].(string)
			rest, ok := strings.CutPrefix(text, sent)
			if !ok {
				rest, ok = strings.CutPrefix(strings.TrimLeft(text, " \t\r\n"), strings.TrimLeft(sent, " \t\r\n"))
			}
			if !ok {
				telemetry.Log.Warn().Str("request_id", s.reqID).Msg("Live stream: translated text does not continue the streamed text; tail not sent")
				rest = ""
			}
			if rest != "" {
				if err := s.textDelta(rest); err != nil {
					return err
				}
			}
			if err := s.closeText(); err != nil {
				return err
			}
		default:
			if err := s.closeText(); err != nil {
				return err
			}
			if err := s.block(block); err != nil {
				return err
			}
		}
	}
	if err := s.closeText(); err != nil {
		return err
	}

	stop := msg.StopReason
	if stop == "" {
		stop = "end_turn"
	}
	s.w.Header().Set("X-Liltok-Provider", providerName)
	s.w.Header().Set("X-Liltok-Cache-Tier", "NONE")
	if err := s.event("message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]int{"input_tokens": msg.Usage.InputTokens, "output_tokens": msg.Usage.OutputTokens},
	}); err != nil {
		return err
	}
	return s.event("message_stop", map[string]string{"type": "message_stop"})
}

// block sends one complete content block: tool_use input goes out as a single input_json_delta,
// the way the non-live replay sends it.
func (s *anthropicLiveSink) block(block map[string]interface{}) error {
	i := s.nextIndex
	s.nextIndex++
	start := make(map[string]interface{}, len(block))
	for k, v := range block {
		start[k] = v
	}
	var delta map[string]interface{}
	switch block["type"] {
	case "tool_use":
		input, _ := json.Marshal(block["input"])
		start["input"] = map[string]interface{}{}
		delta = map[string]interface{}{"type": "input_json_delta", "partial_json": string(input)}
	case "text":
		text, _ := block["text"].(string)
		start["text"] = ""
		delta = map[string]interface{}{"type": "text_delta", "text": text}
	}
	if err := s.event("content_block_start", map[string]interface{}{"type": "content_block_start", "index": i, "content_block": start}); err != nil {
		return err
	}
	if delta != nil {
		if err := s.event("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": i, "delta": delta}); err != nil {
			return err
		}
	}
	return s.event("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": i})
}

// Fail ends a committed stream with an Anthropic error event.
func (s *anthropicLiveSink) Fail(err error) {
	_ = s.closeText()
	_ = s.event("error", map[string]interface{}{
		"type":  "error",
		"error": map[string]string{"type": "api_error", "message": "upstream reply failed after streaming started: " + err.Error()},
	})
}
