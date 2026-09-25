package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/provider"
)

// decodePayload runs buildPayload and decodes the result into a generic map.
func decodePayload(t *testing.T, a *Adapter, req *provider.UnifiedChatRequest, stream bool) map[string]interface{} {
	t.Helper()
	raw, err := a.buildPayload(req, stream)
	if err != nil {
		t.Fatalf("buildPayload returned error: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("buildPayload produced invalid JSON %q: %v", raw, err)
	}
	return m
}

func TestNewAdapterDefaultsAndTrim(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty uses default", "", "https://api.anthropic.com"},
		{"trailing slash trimmed", "http://example.test/", "http://example.test"},
		{"multiple trailing slashes trimmed", "http://example.test///", "http://example.test"},
		{"unchanged", "http://example.test/base", "http://example.test/base"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAdapter(tc.in, "test-key")
			if a.baseURL != tc.want {
				t.Errorf("baseURL = %q, want %q", a.baseURL, tc.want)
			}
			if a.apiKey != "test-key" {
				t.Errorf("apiKey = %q, want test-key", a.apiKey)
			}
			if a.httpClient == nil || a.httpClient.Timeout <= 0 {
				t.Errorf("expected http client with a positive timeout")
			}
		})
	}
}

func TestSetAPIKeyAndBaseURL(t *testing.T) {
	var gotKey, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	a := NewAdapter("http://unused.invalid", "old-key")
	a.SetAPIKey("new-key")
	a.SetBaseURL(srv.URL + "/")
	if a.baseURL != srv.URL {
		t.Fatalf("SetBaseURL did not trim trailing slash: %q", a.baseURL)
	}
	if ok, err := a.CheckHealth(context.Background()); !ok || err != nil {
		t.Fatalf("CheckHealth = %v, %v", ok, err)
	}
	if gotKey != "new-key" {
		t.Errorf("x-api-key = %q, want new-key", gotKey)
	}
	if gotPath != "/v1/models" {
		t.Errorf("path = %q, want /v1/models", gotPath)
	}

	a.SetBaseURL("")
	if a.baseURL != "https://api.anthropic.com" {
		t.Errorf("SetBaseURL(\"\") = %q, want default", a.baseURL)
	}
}

func TestNameAndTier(t *testing.T) {
	a := NewAdapter("", "test-key")
	if a.Name() != "anthropic" {
		t.Errorf("Name() = %q", a.Name())
	}
	if a.Tier() != provider.TierPremium {
		t.Errorf("Tier() = %q, want %q", a.Tier(), provider.TierPremium)
	}
}

func TestSetHeaders(t *testing.T) {
	a := NewAdapter("", "test-key")
	r := httptest.NewRequest("GET", "http://example.test", nil)
	a.setHeaders(r)
	want := map[string]string{
		"Content-Type":      "application/json",
		"anthropic-version": "2023-06-01",
		"anthropic-beta":    "prompt-caching-2024-07-25",
		"x-api-key":         "test-key",
	}
	for k, v := range want {
		if got := r.Header.Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}

	a.SetAPIKey("")
	r2 := httptest.NewRequest("GET", "http://example.test", nil)
	a.setHeaders(r2)
	if _, ok := r2.Header["X-Api-Key"]; ok {
		t.Errorf("x-api-key header should be absent when key is empty")
	}
}

func TestCheckHealth(t *testing.T) {
	t.Run("missing key", func(t *testing.T) {
		a := NewAdapter("http://unused.invalid", "")
		ok, err := a.CheckHealth(context.Background())
		if ok || err == nil || !strings.Contains(err.Error(), "not configured") {
			t.Fatalf("got %v, %v; want false and not-configured error", ok, err)
		}
	})

	t.Run("ok", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				t.Errorf("method = %s, want GET", r.Method)
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		ok, err := NewAdapter(srv.URL, "test-key").CheckHealth(context.Background())
		if !ok || err != nil {
			t.Fatalf("got %v, %v; want true, nil", ok, err)
		}
	})

	t.Run("error status includes body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("bad key"))
		}))
		defer srv.Close()
		ok, err := NewAdapter(srv.URL, "test-key").CheckHealth(context.Background())
		if ok || err == nil {
			t.Fatalf("got %v, %v; want false and error", ok, err)
		}
		if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "bad key") {
			t.Errorf("error %q should contain status and body", err)
		}
	})

	t.Run("connection failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		url := srv.URL
		srv.Close()
		ok, err := NewAdapter(url, "test-key").CheckHealth(context.Background())
		if ok || err == nil || !strings.Contains(err.Error(), "connection failed") {
			t.Fatalf("got %v, %v; want connection failed error", ok, err)
		}
	})

	t.Run("invalid url", func(t *testing.T) {
		ok, err := NewAdapter("://bad", "test-key").CheckHealth(context.Background())
		if ok || err == nil {
			t.Fatalf("got %v, %v; want request construction error", ok, err)
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer srv.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ok, err := NewAdapter(srv.URL, "test-key").CheckHealth(ctx)
		if ok || !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, %v; want context.Canceled", ok, err)
		}
	})
}

func TestListModels(t *testing.T) {
	t.Run("missing key", func(t *testing.T) {
		_, err := NewAdapter("http://unused.invalid", "").ListModels(context.Background())
		if err == nil || !strings.Contains(err.Error(), "not configured") {
			t.Fatalf("want not-configured error, got %v", err)
		}
	})

	t.Run("ok", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				t.Errorf("path = %s", r.URL.Path)
			}
			if r.Header.Get("x-api-key") != "test-key" {
				t.Errorf("missing api key header")
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-a"},{"id":"claude-b"}]}`))
		}))
		defer srv.Close()
		models, err := NewAdapter(srv.URL, "test-key").ListModels(context.Background())
		if err != nil {
			t.Fatalf("ListModels error: %v", err)
		}
		want := []provider.ModelInfo{
			{ID: "claude-a", Provider: "anthropic", Active: true, OwnedBy: "anthropic"},
			{ID: "claude-b", Provider: "anthropic", Active: true, OwnedBy: "anthropic"},
		}
		if !reflect.DeepEqual(models, want) {
			t.Errorf("models = %+v, want %+v", models, want)
		}
	})

	t.Run("empty list", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		defer srv.Close()
		models, err := NewAdapter(srv.URL, "test-key").ListModels(context.Background())
		if err != nil || len(models) != 0 {
			t.Fatalf("got %v, %v; want empty, nil", models, err)
		}
	})

	t.Run("error status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
		}))
		defer srv.Close()
		_, err := NewAdapter(srv.URL, "test-key").ListModels(context.Background())
		if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("want status 500 error with body, got %v", err)
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"data":`))
		}))
		defer srv.Close()
		_, err := NewAdapter(srv.URL, "test-key").ListModels(context.Background())
		if err == nil || !strings.Contains(err.Error(), "decode") {
			t.Fatalf("want decode error, got %v", err)
		}
	})

	t.Run("connection failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		url := srv.URL
		srv.Close()
		_, err := NewAdapter(url, "test-key").ListModels(context.Background())
		if err == nil || !strings.Contains(err.Error(), "list models failed") {
			t.Fatalf("want list models failed error, got %v", err)
		}
	})

	t.Run("invalid url", func(t *testing.T) {
		_, err := NewAdapter("://bad", "test-key").ListModels(context.Background())
		if err == nil {
			t.Fatalf("want request construction error")
		}
	})
}

func TestSendChatRequestAndResponseMapping(t *testing.T) {
	var gotBody map[string]interface{}
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = w.Write([]byte(`{
			"id": "msg-1",
			"model": "claude-test",
			"role": "assistant",
			"content": [
				{"type": "thinking", "thinking": "step one. "},
				{"type": "thinking", "thinking": "step two."},
				{"type": "text", "text": "Hello "},
				{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "Paris"}},
				{"type": "text", "text": "world"},
				{"type": "unknown_block", "text": "ignored"}
			],
			"stop_reason": "tool_use",
			"usage": {"input_tokens": 10, "output_tokens": 7, "cache_creation_input_tokens": 3, "cache_read_input_tokens": 4}
		}`))
	}))
	defer srv.Close()

	resp, err := NewAdapter(srv.URL, "test-key").SendChat(context.Background(), &provider.UnifiedChatRequest{
		Model:    "claude-test",
		Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("SendChat error: %v", err)
	}

	if gotMethod != "POST" || gotPath != "/v1/messages" {
		t.Errorf("request = %s %s, want POST /v1/messages", gotMethod, gotPath)
	}
	if gotBody["stream"] != false {
		t.Errorf("non-streaming request should send stream=false, got %v", gotBody["stream"])
	}

	if resp.ID != "msg-1" || resp.Model != "claude-test" || resp.Role != "assistant" {
		t.Errorf("unexpected id/model/role: %q %q %q", resp.ID, resp.Model, resp.Role)
	}
	if resp.Content != "Hello world" {
		t.Errorf("Content = %q, want concatenated text blocks", resp.Content)
	}
	if resp.ReasoningContent != "step one. step two." {
		t.Errorf("ReasoningContent = %q", resp.ReasoningContent)
	}
	if resp.FinishReason != "tool_use" {
		t.Errorf("FinishReason = %q", resp.FinishReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v, want 1", resp.ToolCalls)
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Type != "function" || tc.Function.Name != "get_weather" {
		t.Errorf("tool call = %+v", tc)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil || args["city"] != "Paris" {
		t.Errorf("Arguments = %q, want JSON with city=Paris", tc.Function.Arguments)
	}
	wantUsage := provider.UnifiedUsage{PromptTokens: 10, CompletionTokens: 7, TotalTokens: 17, CachedTokens: 4}
	if resp.Usage != wantUsage {
		t.Errorf("Usage = %+v, want %+v", resp.Usage, wantUsage)
	}
	if len(resp.RawResponse) == 0 || !json.Valid(resp.RawResponse) {
		t.Errorf("RawResponse should hold the upstream body")
	}
}

func TestSendChatErrors(t *testing.T) {
	req := &provider.UnifiedChatRequest{Model: "claude-test", Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "hi"}}}

	t.Run("error status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
		}))
		defer srv.Close()
		resp, err := NewAdapter(srv.URL, "test-key").SendChat(context.Background(), req)
		if resp != nil || err == nil || !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "rate_limit_error") {
			t.Fatalf("got %v, %v; want 429 error with body", resp, err)
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`not json`))
		}))
		defer srv.Close()
		_, err := NewAdapter(srv.URL, "test-key").SendChat(context.Background(), req)
		if err == nil || !strings.Contains(err.Error(), "failed to parse") {
			t.Fatalf("want parse error, got %v", err)
		}
	})

	t.Run("connection failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		url := srv.URL
		srv.Close()
		_, err := NewAdapter(url, "test-key").SendChat(context.Background(), req)
		if err == nil || !strings.Contains(err.Error(), "request failed") {
			t.Fatalf("want request failed error, got %v", err)
		}
	})

	t.Run("invalid url", func(t *testing.T) {
		_, err := NewAdapter("://bad", "test-key").SendChat(context.Background(), req)
		if err == nil {
			t.Fatalf("want request construction error")
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer srv.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := NewAdapter(srv.URL, "test-key").SendChat(ctx, req)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	})

	t.Run("truncated body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Promise more bytes than are sent so the client read fails.
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":`))
		}))
		defer srv.Close()
		_, err := NewAdapter(srv.URL, "test-key").SendChat(context.Background(), req)
		if err == nil || !strings.Contains(err.Error(), "failed to read response body") {
			t.Fatalf("want read body error, got %v", err)
		}
	})
}

// collectStream drains both channels and returns the events and the first error.
func collectStream(t *testing.T, events <-chan provider.UnifiedSSEEvent, errs <-chan error) ([]provider.UnifiedSSEEvent, error) {
	t.Helper()
	var out []provider.UnifiedSSEEvent
	for ev := range events {
		out = append(out, ev)
	}
	return out, <-errs
}

func TestStreamChatEventMapping(t *testing.T) {
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Join([]string{
			"event: message_start",
			`data: {"type":"message_start"}`,
			"",
			": comment line ignored",
			"event: content_block_delta",
			`data: {"delta":{"type":"text_delta","text":"Hel"}}`,
			"",
			"event: content_block_delta",
			`data: not-json`,
			"",
			"event: content_block_delta",
			`data: {"delta":{"type":"text_delta","text":"lo"}}`,
			"",
			"event: message_delta",
			`data: {"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
			"",
			"event: message_delta",
			`data: {broken`,
			"",
			"event: message_stop",
			`data: {"type":"message_stop"}`,
			"",
			"event: content_block_delta",
			`data: {"delta":{"type":"text_delta","text":"after stop"}}`,
			"",
		}, "\n"))
	}))
	defer srv.Close()

	events, errs, err := NewAdapter(srv.URL, "test-key").StreamChat(context.Background(), &provider.UnifiedChatRequest{
		Model:    "claude-test",
		Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamChat error: %v", err)
	}
	got, streamErr := collectStream(t, events, errs)
	if streamErr != nil {
		t.Fatalf("unexpected stream error: %v", streamErr)
	}
	if gotBody["stream"] != true {
		t.Errorf("streaming request should send stream=true, got %v", gotBody["stream"])
	}

	type simple struct{ Type, Text, Finish string }
	var simplified []simple
	for _, ev := range got {
		simplified = append(simplified, simple{ev.Type, ev.DeltaText, ev.FinishReason})
	}
	want := []simple{
		{"text_delta", "Hel", ""},
		{"text_delta", "lo", ""},
		{"finish", "", "end_turn"},
		{"done", "", ""},
	}
	if !reflect.DeepEqual(simplified, want) {
		t.Fatalf("events = %+v, want %+v", simplified, want)
	}

	if string(got[0].RawChunk) != "event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"Hel\"}}\n\n" {
		t.Errorf("text_delta RawChunk = %q", got[0].RawChunk)
	}
	if !strings.HasPrefix(string(got[2].RawChunk), "event: message_delta\ndata: ") {
		t.Errorf("finish RawChunk = %q", got[2].RawChunk)
	}
	if string(got[3].RawChunk) != "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n" {
		t.Errorf("done RawChunk = %q", got[3].RawChunk)
	}
}

func TestStreamChatErrors(t *testing.T) {
	req := &provider.UnifiedChatRequest{Model: "claude-test", Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "hi"}}}

	t.Run("error status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("invalid request"))
		}))
		defer srv.Close()
		ev, errs, err := NewAdapter(srv.URL, "test-key").StreamChat(context.Background(), req)
		if ev != nil || errs != nil {
			t.Errorf("channels should be nil on error")
		}
		if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid request") {
			t.Fatalf("want 400 error with body, got %v", err)
		}
	})

	t.Run("connection failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		url := srv.URL
		srv.Close()
		_, _, err := NewAdapter(url, "test-key").StreamChat(context.Background(), req)
		if err == nil || !strings.Contains(err.Error(), "stream failed") {
			t.Fatalf("want stream failed error, got %v", err)
		}
	})

	t.Run("invalid url", func(t *testing.T) {
		_, _, err := NewAdapter("://bad", "test-key").StreamChat(context.Background(), req)
		if err == nil {
			t.Fatalf("want request construction error")
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer srv.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, err := NewAdapter(srv.URL, "test-key").StreamChat(ctx, req)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	})

	t.Run("stream ends without message_stop", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"delta\":{\"text\":\"partial\"}}\n\n")
		}))
		defer srv.Close()
		events, errs, err := NewAdapter(srv.URL, "test-key").StreamChat(context.Background(), req)
		if err != nil {
			t.Fatalf("StreamChat error: %v", err)
		}
		got, streamErr := collectStream(t, events, errs)
		if streamErr != nil {
			t.Fatalf("clean EOF should not report an error, got %v", streamErr)
		}
		if len(got) != 1 || got[0].DeltaText != "partial" {
			t.Fatalf("events = %+v, want one partial text_delta", got)
		}
	})

	t.Run("oversized line reports scanner error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: "+strings.Repeat("x", 600*1024)+"\n\n")
		}))
		defer srv.Close()
		events, errs, err := NewAdapter(srv.URL, "test-key").StreamChat(context.Background(), req)
		if err != nil {
			t.Fatalf("StreamChat error: %v", err)
		}
		got, streamErr := collectStream(t, events, errs)
		if len(got) != 0 {
			t.Errorf("expected no events, got %+v", got)
		}
		if streamErr == nil || !strings.Contains(streamErr.Error(), "too long") {
			t.Fatalf("want token too long error, got %v", streamErr)
		}
	})
}

func TestBuildPayloadTranslatedRequest(t *testing.T) {
	a := NewAdapter("", "test-key")
	got := decodePayload(t, a, &provider.UnifiedChatRequest{
		Model:        "claude-test",
		SystemPrompt: "be terse",
		Temperature:  0.3,
		TopP:         0.9,
		MaxTokens:    256,
		Messages: []provider.UnifiedChatMessage{
			{Role: "system", Content: "dropped system message"},
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "second"},
			{Role: "user", Content: "third"},
		},
	}, false)

	want := map[string]interface{}{
		"model":       "claude-test",
		"system":      "be terse",
		"temperature": 0.3,
		"top_p":       0.9,
		"max_tokens":  float64(256),
		"stream":      false,
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "first"},
			map[string]interface{}{"role": "assistant", "content": "second"},
			map[string]interface{}{"role": "user", "content": "third"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("payload mismatch:\n%s", gotJSON)
	}
}

func TestBuildPayloadDefaultsAndOmissions(t *testing.T) {
	a := NewAdapter("", "test-key")
	tests := []struct {
		name      string
		maxTokens int
		wantMax   float64
	}{
		{"zero max_tokens defaults to 4096", 0, 4096},
		{"negative max_tokens defaults to 4096", -5, 4096},
		{"large max_tokens passed through unclamped", 200000, 200000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decodePayload(t, a, &provider.UnifiedChatRequest{Model: "claude-test", MaxTokens: tc.maxTokens}, true)
			if got["max_tokens"] != tc.wantMax {
				t.Errorf("max_tokens = %v, want %v", got["max_tokens"], tc.wantMax)
			}
			if got["stream"] != true {
				t.Errorf("stream = %v, want true", got["stream"])
			}
			for _, k := range []string{"system", "temperature", "top_p"} {
				if _, ok := got[k]; ok {
					t.Errorf("%s should be omitted when unset, got %v", k, got[k])
				}
			}
			msgs, ok := got["messages"].([]interface{})
			if !ok || len(msgs) != 0 {
				t.Errorf("messages = %#v, want empty array (not null)", got["messages"])
			}
		})
	}
}

func TestBuildPayloadAnthropicPassthrough(t *testing.T) {
	a := NewAdapter("", "test-key")

	t.Run("raw payload reused with stream overridden", func(t *testing.T) {
		raw := []byte(`{"model":"claude-raw","max_tokens":123,"stream":false,"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}],"tools":[{"name":"t1","input_schema":{"type":"object"}}]}`)
		got := decodePayload(t, a, &provider.UnifiedChatRequest{
			Model:             "ignored-model",
			MaxTokens:         999,
			IsAnthropicSource: true,
			RawPayload:        raw,
		}, true)

		var want map[string]interface{}
		_ = json.Unmarshal(raw, &want)
		want["stream"] = true
		if !reflect.DeepEqual(got, want) {
			gotJSON, _ := json.Marshal(got)
			t.Fatalf("passthrough payload = %s", gotJSON)
		}
	})

	t.Run("missing max_tokens gets default", func(t *testing.T) {
		got := decodePayload(t, a, &provider.UnifiedChatRequest{
			IsAnthropicSource: true,
			RawPayload:        []byte(`{"model":"claude-raw","messages":[]}`),
		}, false)
		if got["max_tokens"] != float64(4096) {
			t.Errorf("max_tokens = %v, want 4096", got["max_tokens"])
		}
		if got["stream"] != false {
			t.Errorf("stream = %v, want false", got["stream"])
		}
	})

	t.Run("invalid raw payload falls back to translation", func(t *testing.T) {
		got := decodePayload(t, a, &provider.UnifiedChatRequest{
			Model:             "claude-test",
			IsAnthropicSource: true,
			RawPayload:        []byte(`{not json`),
			Messages:          []provider.UnifiedChatMessage{{Role: "user", Content: "hi"}},
		}, false)
		if got["model"] != "claude-test" {
			t.Errorf("model = %v, want translated claude-test", got["model"])
		}
		msgs := got["messages"].([]interface{})
		if len(msgs) != 1 || msgs[0].(map[string]interface{})["content"] != "hi" {
			t.Errorf("messages = %#v", msgs)
		}
	})

	t.Run("raw payload ignored when not anthropic source", func(t *testing.T) {
		got := decodePayload(t, a, &provider.UnifiedChatRequest{
			Model:      "claude-test",
			RawPayload: []byte(`{"model":"claude-raw"}`),
		}, false)
		if got["model"] != "claude-test" {
			t.Errorf("model = %v, want claude-test", got["model"])
		}
	})
}
