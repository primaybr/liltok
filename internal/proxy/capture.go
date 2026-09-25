package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/primaybr/liltok/internal/router"
)

// Replay fixtures pair a client /v1/messages request with the raw upstream reply of each fallback
// attempt and the response the client should receive. replay_test.go replays every fixture in
// testdata/replay; routes.capture_dir records new ones from live traffic.

type replayFixture struct {
	Description string            `json:"description"`
	Origin      string            `json:"origin"`
	Request     json.RawMessage   `json:"request"`
	Attempts    []replayAttempt   `json:"attempts"`
	Expect      replayExpectation `json:"expect"`
}

type replayAttempt struct {
	Content          string           `json:"content,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []replayToolCall `json:"tool_calls,omitempty"`
	FinishReason     string           `json:"finish_reason,omitempty"`
	Error            string           `json:"error,omitempty"`
}

type replayToolCall struct {
	Name string `json:"name"`
	// Arguments is a JSON object, or a JSON string holding raw (possibly malformed) argument text.
	Arguments json.RawMessage `json:"arguments"`
}

type replayExpectation struct {
	Status         int           `json:"status,omitempty"`
	Attempts       int           `json:"attempts,omitempty"`
	DirectUpstream bool          `json:"direct_upstream,omitempty"`
	StopReason     string        `json:"stop_reason,omitempty"`
	Blocks         []replayBlock `json:"blocks,omitempty"`
	// UpstreamContains lists raw substrings every upstream attempt's system prompt or message text must contain.
	UpstreamContains []string `json:"upstream_contains,omitempty"`
}

type replayBlock struct {
	Type          string                 `json:"type"`
	Name          string                 `json:"name,omitempty"`
	Contains      []string               `json:"contains,omitempty"`
	Excludes      []string               `json:"excludes,omitempty"`
	Input         map[string]interface{} `json:"input,omitempty"`
	InputContains map[string]string      `json:"input_contains,omitempty"`
}

// replayMessage is the Anthropic message a client reconstructs from the response.
type replayMessage struct {
	StopReason   string
	InputTokens  int
	OutputTokens int
	Blocks       []replayOutBlock
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type replayOutBlock struct {
	Type  string
	Text  string
	Name  string
	Input map[string]interface{}
}

func parseAnthropicJSON(body []byte) (replayMessage, error) {
	var resp struct {
		Type       string         `json:"type"`
		StopReason string         `json:"stop_reason"`
		Usage      anthropicUsage `json:"usage"`
		Content    []struct {
			Type     string                 `json:"type"`
			Text     string                 `json:"text"`
			Thinking string                 `json:"thinking"`
			Name     string                 `json:"name"`
			Input    map[string]interface{} `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return replayMessage{}, err
	}
	if resp.Type != "message" {
		return replayMessage{}, fmt.Errorf("type = %q, want message", resp.Type)
	}
	msg := replayMessage{StopReason: resp.StopReason, InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	for _, c := range resp.Content {
		text := c.Text
		if c.Type == "thinking" {
			text = c.Thinking
		}
		msg.Blocks = append(msg.Blocks, replayOutBlock{Type: c.Type, Text: text, Name: c.Name, Input: c.Input})
	}
	return msg, nil
}

// fixtureCapture collects the raw upstream attempts of one routed request.
type fixtureCapture struct {
	mu       sync.Mutex
	attempts []replayAttempt
}

func (c *fixtureCapture) observe(res router.AttemptResult) {
	// Keep the raw reply even when an interceptor rejected it, so replaying the fixture
	// reproduces the rejection; only attempts with no reply are recorded as errors.
	var a replayAttempt
	resp := res.Response
	switch {
	case resp == nil && res.Err != nil:
		a.Error = res.Err.Error()
	case resp == nil:
		a.Error = "no response"
	default:
		a.Content = resp.Content
		a.ReasoningContent = resp.ReasoningContent
		a.FinishReason = resp.FinishReason
		for _, tc := range resp.ToolCalls {
			args := json.RawMessage(tc.Function.Arguments)
			if !json.Valid(args) {
				args, _ = json.Marshal(tc.Function.Arguments)
			}
			a.ToolCalls = append(a.ToolCalls, replayToolCall{Name: tc.Function.Name, Arguments: args})
		}
	}
	c.mu.Lock()
	c.attempts = append(c.attempts, a)
	c.mu.Unlock()
}

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// write stores the capture as a fixture whose expectations snapshot the response actually sent.
// Edit the expectations to the correct behaviour before adding the file to testdata/replay.
func (c *fixtureCapture) write(dir, reqID string, request, response []byte) (string, error) {
	msg, err := parseAnthropicJSON(response)
	if err != nil {
		return "", fmt.Errorf("parse translated response: %w", err)
	}
	c.mu.Lock()
	attempts := append([]replayAttempt(nil), c.attempts...)
	c.mu.Unlock()

	now := time.Now().UTC()
	fx := replayFixture{
		Description: "Captured " + now.Format(time.RFC3339) + ". Describe the behaviour under test and correct the expectations.",
		Origin:      "capture " + reqID,
		Request:     json.RawMessage(request),
		Attempts:    attempts,
		Expect: replayExpectation{
			Attempts:   len(attempts),
			StopReason: msg.StopReason,
		},
	}
	for _, b := range msg.Blocks {
		block := replayBlock{Type: b.Type, Name: b.Name, Input: b.Input}
		if b.Text != "" {
			block.Contains = []string{b.Text}
		}
		fx.Expect.Blocks = append(fx.Expect.Blocks, block)
	}
	if !json.Valid(fx.Request) {
		return "", fmt.Errorf("request body is not valid JSON")
	}
	out, err := json.MarshalIndent(fx, "", "  ")
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := now.Format("20060102-150405.000") + "-" + unsafeFileChars.ReplaceAllString(reqID, "_") + ".json"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
