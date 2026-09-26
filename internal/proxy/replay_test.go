package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/ledger"
	"github.com/primaybr/liltok/internal/provider"
	"github.com/primaybr/liltok/internal/router"
	"github.com/primaybr/liltok/internal/server/middleware"
	"github.com/primaybr/liltok/internal/telemetry"
	"github.com/rs/zerolog"
)

// Replay harness: each testdata/replay/*.json fixture is an Anthropic /v1/messages request plus the
// scripted upstream replies for each attempt in the fallback chain. The request runs through the real
// handler (pruner, router validators, translator, SSE replay) in both non-streaming and streaming mode,
// and the Anthropic response Claude Code would receive is checked against the fixture's expectations.
// To cover a new translation bug, add a fixture that reproduces it; replayFixture documents the fields.

const replayRoute = "replay"

// TestMain silences the package logger once, before any test builds a router. NewRouter starts a
// model-sync goroutine that outlives its test and logs through telemetry.Log, so swapping the logger
// inside a test would race with those goroutines.
func TestMain(m *testing.M) {
	telemetry.Log = zerolog.Nop()
	os.Exit(m.Run())
}

// scriptedProvider returns the fixture's attempts in order, one per SendChat call.
type scriptedProvider struct {
	mu       sync.Mutex
	attempts []replayAttempt
	requests []*provider.UnifiedChatRequest
}

func (s *scriptedProvider) Name() string                { return replayRoute }
func (s *scriptedProvider) Tier() provider.ProviderTier { return provider.TierFree }
func (s *scriptedProvider) CheckHealth(ctx context.Context) (bool, error) {
	return true, nil
}

// StreamChat streams the next scripted attempt the way the adapters do: reasoning as thinking
// deltas, the text in small chunks, then each tool call, a finish carrying usage, and done. It is
// used by the live-streaming replay mode; the script is the same one SendChat returns whole.
func (s *scriptedProvider) StreamChat(ctx context.Context, req *provider.UnifiedChatRequest) (<-chan provider.UnifiedSSEEvent, <-chan error, error) {
	resp, err := s.SendChat(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	var evs []provider.UnifiedSSEEvent
	if resp.ReasoningContent != "" {
		evs = append(evs, provider.UnifiedSSEEvent{Type: "thinking_delta", DeltaText: resp.ReasoningContent})
	}
	for text := resp.Content; text != ""; {
		n := min(len(text), 48)
		for n < len(text) && !utf8.RuneStart(text[n]) {
			n++
		}
		evs = append(evs, provider.UnifiedSSEEvent{Type: "text_delta", DeltaText: text[:n]})
		text = text[n:]
	}
	for _, tc := range resp.ToolCalls {
		evs = append(evs, provider.UnifiedSSEEvent{Type: "tool_call", ToolCalls: []provider.UnifiedToolCall{tc}})
	}
	usage := resp.Usage
	evs = append(evs, provider.UnifiedSSEEvent{Type: "finish", FinishReason: resp.FinishReason, Usage: &usage}, provider.UnifiedSSEEvent{Type: "done"})

	events := make(chan provider.UnifiedSSEEvent, len(evs))
	for _, ev := range evs {
		events <- ev
	}
	close(events)
	errs := make(chan error)
	close(errs)
	return events, errs, nil
}

func (s *scriptedProvider) SendChat(ctx context.Context, req *provider.UnifiedChatRequest) (*provider.UnifiedChatResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := *req
	s.requests = append(s.requests, &clone)
	n := len(s.requests)
	if n > len(s.attempts) {
		return nil, fmt.Errorf("replay: attempt %d is not scripted (fixture has %d)", n, len(s.attempts))
	}
	a := s.attempts[n-1]
	if a.Error != "" {
		return nil, errors.New(a.Error)
	}
	resp := &provider.UnifiedChatResponse{
		ID:               fmt.Sprintf("chatcmpl-replay-%d", n),
		Model:            req.Model,
		Role:             "assistant",
		Content:          a.Content,
		ReasoningContent: a.ReasoningContent,
		FinishReason:     a.FinishReason,
		Usage:            provider.UnifiedUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
	}
	for i, tc := range a.ToolCalls {
		call := provider.UnifiedToolCall{ID: fmt.Sprintf("call_replay_%d_%d", n, i), Type: "function"}
		call.Function.Name = tc.Name
		var raw string
		if err := json.Unmarshal(tc.Arguments, &raw); err == nil {
			call.Function.Arguments = raw
		} else {
			call.Function.Arguments = string(tc.Arguments)
		}
		resp.ToolCalls = append(resp.ToolCalls, call)
	}
	if resp.FinishReason == "" {
		resp.FinishReason = "stop"
		if len(resp.ToolCalls) > 0 {
			resp.FinishReason = "tool_calls"
		}
	}
	resp.RawResponse = rawOpenAIResponse(resp)
	return resp, nil
}

// rawOpenAIResponse builds the RawResponse the OpenAI-compatible adapter attaches: the upstream
// chat completion as received, before any router interceptor rewrites the reply. Only the openai
// ingress reads it; the Anthropic ingress translates the unified response instead.
func rawOpenAIResponse(resp *provider.UnifiedChatResponse) []byte {
	message := map[string]interface{}{"role": resp.Role, "content": resp.Content}
	if resp.ReasoningContent != "" {
		message["reasoning_content"] = resp.ReasoningContent
	}
	if len(resp.ToolCalls) > 0 {
		message["tool_calls"] = append([]provider.UnifiedToolCall(nil), resp.ToolCalls...)
	}
	raw, _ := json.Marshal(map[string]interface{}{
		"id":      resp.ID,
		"object":  "chat.completion",
		"model":   resp.Model,
		"choices": []map[string]interface{}{{"index": 0, "message": message, "finish_reason": resp.FinishReason}},
		"usage": map[string]int{
			"prompt_tokens":     resp.Usage.PromptTokens,
			"completion_tokens": resp.Usage.CompletionTokens,
			"total_tokens":      resp.Usage.TotalTokens,
		},
	})
	return raw
}

func TestReplayFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "replay", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no replay fixtures found in testdata/replay")
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var fx replayFixture
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&fx); err != nil {
			t.Fatalf("%s: invalid fixture: %v", path, err)
		}
		if fx.Ingress != "" && fx.Ingress != ingressOpenAI {
			t.Fatalf("%s: unknown ingress %q", path, fx.Ingress)
		}
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		// Every fixture must give the same response whole (json), replayed as SSE (sse), and with
		// routes.live_streaming on (live), where attempts stream and long prose commits early.
		for _, mode := range []replayMode{modeJSON, modeSSE, modeLive} {
			// Live streaming only serves Anthropic-format clients; the openai ingress never uses a
			// live sink, so its live run would repeat the sse run.
			if mode == modeLive && fx.Ingress == ingressOpenAI {
				continue
			}
			t.Run(name+"/"+string(mode), func(t *testing.T) {
				if fx.KnownBug != "" {
					t.Skip("known bug: " + fx.KnownBug)
				}
				runReplayFixture(t, fx, mode, "")
			})
		}
	}
}

type replayMode string

const (
	modeJSON replayMode = "json"
	modeSSE  replayMode = "sse"
	modeLive replayMode = "live"
)

// ingressOpenAI marks a fixture that drives POST /v1/chat/completions instead of /v1/messages.
const ingressOpenAI = "openai"

func runReplayFixture(t *testing.T, fx replayFixture, mode replayMode, captureDir string) {
	t.Helper()
	stream := mode != modeJSON
	if fx.Description != "" {
		t.Log(fx.Description)
	}

	// Any request that escapes the router to a direct upstream lands here instead of the internet.
	var directHits int
	var directMu sync.Mutex
	// NewRouter's background model-catalog sync also lands here as GET requests; only POSTs count.
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			directMu.Lock()
			directHits++
			directMu.Unlock()
		}
		http.Error(w, `{"error":{"message":"replay: direct upstream reached"}}`, http.StatusBadGateway)
	}))
	defer direct.Close()

	cfg := config.DefaultConfig()
	cfg.Providers.Anthropic.BaseURL = direct.URL
	cfg.Providers.Anthropic.APIKey = "replay-anthropic-key"
	cfg.Providers.OpenAI.BaseURL = direct.URL
	cfg.Providers.OpenAI.APIKey = "replay-openai-key"
	cfg.Routes.CaptureDir = captureDir
	cfg.Routes.LiveStreaming = mode == modeLive

	rtr := router.NewRouter(cfg)
	scripted := &scriptedProvider{attempts: fx.Attempts}
	rtr.SetProvider(replayRoute, scripted)
	// One target per scripted attempt; distinct models keep each target's circuit breaker separate.
	targets := make([]router.TargetSpec, len(fx.Attempts))
	for i := range targets {
		targets[i] = router.TargetSpec{ProviderName: replayRoute, UpstreamModel: fmt.Sprintf("replay-model-%d", i+1)}
	}
	rtr.SetRoute(router.Route{ID: replayRoute, Strategy: "fallback", Targets: targets})

	body := setStreamFlag(t, fx.Request, stream)
	p := NewProxy(cfg, nil, nil, rtr, nil, nil)
	var failovers []*ledger.RequestLog
	p.onFailover = func(item *ledger.RequestLog) { failovers = append(failovers, item) }
	openAI := fx.Ingress == ingressOpenAI
	endpoint, handle := "/v1/messages", p.HandleAnthropicMessages
	if openAI {
		endpoint, handle = "/v1/chat/completions", p.HandleChatCompletions
	}
	handler := middleware.RequestID(http.HandlerFunc(handle))
	req := httptest.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Liltok-Route", replayRoute)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	exp := fx.Expect
	wantStatus := exp.Status
	if wantStatus == 0 {
		wantStatus = http.StatusOK
	}
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, wantStatus, rec.Body.String())
	}
	if exp.Attempts > 0 && len(scripted.requests) != exp.Attempts {
		t.Errorf("upstream attempts = %d, want %d", len(scripted.requests), exp.Attempts)
	}
	// Every failed attempt is reported to the live feed with its target and error.
	wantFailovers := len(scripted.requests)
	if rec.Code == http.StatusOK && !exp.DirectUpstream {
		wantFailovers--
	}
	if len(failovers) != wantFailovers {
		t.Errorf("failover events = %d, want %d", len(failovers), wantFailovers)
	}
	for i, f := range failovers {
		if f.Provider != replayRoute || f.Model != fmt.Sprintf("replay-model-%d", i+1) || f.ErrorMessage == "" || f.CacheStatus != "FAILOVER" {
			t.Errorf("failover event %d = %+v", i, f)
		}
	}

	directMu.Lock()
	reachedDirect := directHits > 0
	directMu.Unlock()
	if reachedDirect != exp.DirectUpstream {
		t.Errorf("direct upstream reached = %v, want %v", reachedDirect, exp.DirectUpstream)
	}
	for i, r := range scripted.requests {
		var sb strings.Builder
		sb.WriteString(r.SystemPrompt)
		for _, m := range r.Messages {
			sb.WriteString("\n")
			sb.WriteString(m.Content)
		}
		haystack := sb.String()
		for _, want := range exp.UpstreamContains {
			if !strings.Contains(haystack, want) {
				t.Errorf("upstream attempt %d request is missing %q", i+1, want)
			}
		}
	}
	if wantStatus != http.StatusOK {
		return
	}
	if mode == modeLive {
		if live := strings.Contains(rec.Body.String(), "msg_live_"); live != exp.LiveCommits {
			t.Errorf("reply streamed live = %v, want %v", live, exp.LiveCommits)
		}
	}

	var msg replayMessage
	var err error
	switch {
	case openAI && stream:
		msg, err = parseOpenAISSE(rec.Body.Bytes())
	case openAI:
		msg, err = parseOpenAIJSON(rec.Body.Bytes())
	case stream:
		msg, err = parseAnthropicSSE(rec.Body.Bytes())
	default:
		msg, err = parseAnthropicJSON(rec.Body.Bytes())
	}
	if err != nil {
		t.Fatalf("parse response: %v\n%s", err, rec.Body.String())
	}
	// The scripted provider reports 100 prompt and 20 completion tokens; Claude Code tracks its
	// context size from these, so they must survive translation and SSE replay. An OpenAI stream
	// carries no usage chunk (clients only get one with stream_options.include_usage), so the
	// openai sse run skips this check.
	if (!openAI || !stream) && (msg.InputTokens != 100 || msg.OutputTokens != 20) {
		t.Errorf("usage = %d in / %d out, want 100 / 20", msg.InputTokens, msg.OutputTokens)
	}
	checkReplayMessage(t, exp, msg, rec.Body.String())
}

// TestReplayCaptureRoundTrip records a fixture through routes.capture_dir and replays it: a capture
// must reproduce the raw upstream attempts and pass against the response it snapshotted.
func TestReplayCaptureRoundTrip(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "replay", "undeclared-tool-fails-over.json"))
	if err != nil {
		t.Fatal(err)
	}
	var source replayFixture
	if err := json.Unmarshal(raw, &source); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	runReplayFixture(t, source, modeSSE, dir)

	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != 1 {
		t.Fatalf("captured %d fixtures, want 1", len(files))
	}
	capturedRaw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var captured replayFixture
	dec := json.NewDecoder(bytes.NewReader(capturedRaw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&captured); err != nil {
		t.Fatalf("captured fixture does not decode: %v", err)
	}
	if len(captured.Attempts) != 2 || len(captured.Attempts[0].ToolCalls) != 1 || captured.Attempts[0].ToolCalls[0].Name != "Global" {
		t.Fatalf("captured attempts do not hold the raw upstream replies: %+v", captured.Attempts)
	}
	if captured.Expect.Attempts != 2 || len(captured.Expect.Blocks) != 1 || captured.Expect.Blocks[0].Name != "Glob" {
		t.Fatalf("captured expectations do not match the response sent: %+v", captured.Expect)
	}

	for _, stream := range []bool{false, true} {
		mode := modeJSON
		if stream {
			mode = modeSSE
		}
		runReplayFixture(t, captured, mode, "")
	}
}

func setStreamFlag(t *testing.T, request json.RawMessage, stream bool) []byte {
	t.Helper()
	var obj map[string]interface{}
	if err := json.Unmarshal(request, &obj); err != nil {
		t.Fatalf("fixture request is not a JSON object: %v", err)
	}
	obj["stream"] = stream
	out, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func checkReplayMessage(t *testing.T, exp replayExpectation, msg replayMessage, body string) {
	t.Helper()
	if exp.StopReason != "" && msg.StopReason != exp.StopReason {
		t.Errorf("stop_reason = %q, want %q", msg.StopReason, exp.StopReason)
	}
	if exp.Blocks == nil {
		return
	}
	if len(msg.Blocks) != len(exp.Blocks) {
		t.Fatalf("got %d content blocks %s, want %d %s\nbody: %s",
			len(msg.Blocks), blockTypes(msg.Blocks), len(exp.Blocks), wantTypes(exp.Blocks), body)
	}
	for i, want := range exp.Blocks {
		got := msg.Blocks[i]
		if got.Type != want.Type {
			t.Errorf("block %d type = %q, want %q", i, got.Type, want.Type)
			continue
		}
		if want.Name != "" && got.Name != want.Name {
			t.Errorf("block %d tool name = %q, want %q", i, got.Name, want.Name)
		}
		for _, s := range want.Contains {
			if !strings.Contains(got.Text, s) {
				t.Errorf("block %d (%s) is missing %q; text: %q", i, got.Type, s, got.Text)
			}
		}
		for _, s := range want.Excludes {
			if strings.Contains(got.Text, s) {
				t.Errorf("block %d (%s) must not contain %q; text: %q", i, got.Type, s, got.Text)
			}
		}
		for k, v := range want.Input {
			if !reflect.DeepEqual(got.Input[k], v) {
				t.Errorf("block %d input[%q] = %#v, want %#v", i, k, got.Input[k], v)
			}
		}
		for k, s := range want.InputContains {
			str, _ := got.Input[k].(string)
			if !strings.Contains(str, s) {
				t.Errorf("block %d input[%q] is missing %q; value: %q", i, k, s, str)
			}
		}
	}
}

func blockTypes(blocks []replayOutBlock) string {
	var types []string
	for _, b := range blocks {
		types = append(types, b.Type)
	}
	return "[" + strings.Join(types, " ") + "]"
}

func wantTypes(blocks []replayBlock) string {
	var types []string
	for _, b := range blocks {
		types = append(types, b.Type)
	}
	return "[" + strings.Join(types, " ") + "]"
}

// parseAnthropicSSE rebuilds the message from an Anthropic event stream the way a client does,
// and rejects streams that break the event protocol.
func parseAnthropicSSE(body []byte) (replayMessage, error) {
	var msg replayMessage
	var partialJSON []strings.Builder
	var texts []strings.Builder
	open := map[int]bool{}
	sawStart, sawStop := false, false

	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Message struct {
				Usage anthropicUsage `json:"usage"`
			} `json:"message"`
			Usage *anthropicUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev); err != nil {
			return msg, fmt.Errorf("invalid event data %q: %w", line, err)
		}
		switch ev.Type {
		case "message_start":
			sawStart = true
			msg.InputTokens = ev.Message.Usage.InputTokens
			msg.OutputTokens = ev.Message.Usage.OutputTokens
		case "content_block_start":
			if ev.Index != len(msg.Blocks) {
				return msg, fmt.Errorf("content_block_start index %d, want %d", ev.Index, len(msg.Blocks))
			}
			msg.Blocks = append(msg.Blocks, replayOutBlock{Type: ev.ContentBlock.Type, Name: ev.ContentBlock.Name})
			partialJSON = append(partialJSON, strings.Builder{})
			texts = append(texts, strings.Builder{})
			open[ev.Index] = true
		case "content_block_delta":
			if !open[ev.Index] {
				return msg, fmt.Errorf("delta for block %d that is not open", ev.Index)
			}
			switch ev.Delta.Type {
			case "text_delta":
				texts[ev.Index].WriteString(ev.Delta.Text)
			case "thinking_delta":
				texts[ev.Index].WriteString(ev.Delta.Thinking)
			case "input_json_delta":
				partialJSON[ev.Index].WriteString(ev.Delta.PartialJSON)
			}
		case "content_block_stop":
			if !open[ev.Index] {
				return msg, fmt.Errorf("stop for block %d that is not open", ev.Index)
			}
			open[ev.Index] = false
		case "message_delta":
			msg.StopReason = ev.Delta.StopReason
			// Like the Anthropic SDKs, message_delta usage updates the totals from message_start.
			if ev.Usage != nil {
				if ev.Usage.InputTokens > 0 {
					msg.InputTokens = ev.Usage.InputTokens
				}
				msg.OutputTokens = ev.Usage.OutputTokens
			}
		case "message_stop":
			sawStop = true
		}
	}
	if err := scanner.Err(); err != nil {
		return msg, err
	}
	if !sawStart || !sawStop {
		return msg, fmt.Errorf("stream missing message_start or message_stop")
	}
	for i := range msg.Blocks {
		if open[i] {
			return msg, fmt.Errorf("block %d never closed", i)
		}
		msg.Blocks[i].Text = texts[i].String()
		if msg.Blocks[i].Type == "tool_use" {
			input := map[string]interface{}{}
			if s := partialJSON[i].String(); s != "" {
				if err := json.Unmarshal([]byte(s), &input); err != nil {
					return msg, fmt.Errorf("block %d tool input is not valid JSON: %q", i, s)
				}
			}
			msg.Blocks[i].Input = input
		}
	}
	return msg, nil
}

// openAIToolCall is a tool call in an OpenAI chat completion message or stream delta.
type openAIToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// openAIMessage maps an OpenAI assistant message onto the Anthropic block layout the fixture
// expectations use: reasoning as a thinking block, content as a text block, then one tool_use
// block per tool call, with the finish_reason as the stop reason.
func openAIMessage(finish, reasoning, content string, calls []openAIToolCall, usage *openAIUsage) (replayMessage, error) {
	msg := replayMessage{StopReason: finish}
	if usage != nil {
		msg.InputTokens = usage.PromptTokens
		msg.OutputTokens = usage.CompletionTokens
	}
	if reasoning != "" {
		msg.Blocks = append(msg.Blocks, replayOutBlock{Type: "thinking", Text: reasoning})
	}
	if content != "" {
		msg.Blocks = append(msg.Blocks, replayOutBlock{Type: "text", Text: content})
	}
	for i, tc := range calls {
		if tc.ID == "" || tc.Function.Name == "" {
			return msg, fmt.Errorf("tool call %d has no id or name: %+v", i, tc)
		}
		input := map[string]interface{}{}
		if strings.TrimSpace(tc.Function.Arguments) != "" {
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
				return msg, fmt.Errorf("tool call %d arguments are not a JSON object: %q", i, tc.Function.Arguments)
			}
		}
		msg.Blocks = append(msg.Blocks, replayOutBlock{Type: "tool_use", Name: tc.Function.Name, Input: input})
	}
	return msg, nil
}

// parseOpenAIJSON reads a non-streaming OpenAI chat completion.
func parseOpenAIJSON(body []byte) (replayMessage, error) {
	var resp struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role             string           `json:"role"`
				Content          string           `json:"content"`
				ReasoningContent string           `json:"reasoning_content"`
				ToolCalls        []openAIToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *openAIUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return replayMessage{}, err
	}
	if resp.Object != "chat.completion" {
		return replayMessage{}, fmt.Errorf("object = %q, want chat.completion", resp.Object)
	}
	if len(resp.Choices) != 1 {
		return replayMessage{}, fmt.Errorf("got %d choices, want 1", len(resp.Choices))
	}
	c := resp.Choices[0]
	if c.Message.Role != "assistant" {
		return replayMessage{}, fmt.Errorf("message role = %q, want assistant", c.Message.Role)
	}
	return openAIMessage(c.FinishReason, c.Message.ReasoningContent, c.Message.Content, c.Message.ToolCalls, resp.Usage)
}

// parseOpenAISSE rebuilds the message from an OpenAI chat.completion.chunk stream the way the
// OpenAI SDKs accumulate it: content and reasoning deltas concatenate, and tool call deltas merge
// by index (the first delta carries id and name, later ones append argument text).
func parseOpenAISSE(body []byte) (replayMessage, error) {
	var content, reasoning strings.Builder
	var calls []openAIToolCall
	var finish string
	var usage *openAIUsage
	sawDone := false

	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			sawDone = true
			continue
		}
		if sawDone {
			return replayMessage{}, fmt.Errorf("chunk after [DONE]: %q", line)
		}
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Content          string           `json:"content"`
					ReasoningContent string           `json:"reasoning_content"`
					ToolCalls        []openAIToolCall `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *openAIUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return replayMessage{}, fmt.Errorf("invalid chunk %q: %w", line, err)
		}
		if chunk.Object != "chat.completion.chunk" {
			return replayMessage{}, fmt.Errorf("chunk object = %q, want chat.completion.chunk", chunk.Object)
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, c := range chunk.Choices {
			if finish != "" {
				return replayMessage{}, fmt.Errorf("delta after finish_reason %q: %q", finish, line)
			}
			content.WriteString(c.Delta.Content)
			reasoning.WriteString(c.Delta.ReasoningContent)
			for _, d := range c.Delta.ToolCalls {
				if d.Index == nil {
					return replayMessage{}, fmt.Errorf("tool call delta without index: %q", line)
				}
				switch i := *d.Index; {
				case i == len(calls):
					calls = append(calls, d)
				case i < len(calls):
					calls[i].Function.Arguments += d.Function.Arguments
				default:
					return replayMessage{}, fmt.Errorf("tool call delta index %d skips past %d", i, len(calls))
				}
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				finish = *c.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return replayMessage{}, err
	}
	if !sawDone || finish == "" {
		return replayMessage{}, fmt.Errorf("stream missing finish_reason or [DONE]")
	}
	return openAIMessage(finish, reasoning.String(), content.String(), calls, usage)
}
