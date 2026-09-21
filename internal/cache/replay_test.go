package cache

import (
	"net/http/httptest"
	"strings"
	"testing"
)

type mockFlusherRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (m *mockFlusherRecorder) Flush() {
	m.flushed = true
}

func TestReplayNonStreaming(t *testing.T) {
	rec := httptest.NewRecorder()
	payload := []byte(`{"id":"chatcmpl-1","choices":[{"message":{"content":"hello world"}}]}`)
	entry := &CacheEntry{
		Hash:            "test-hash",
		Model:           "gpt-4o",
		ResponsePayload: payload,
	}

	err := ReplayCacheHit(rec, entry, false, false, 500)
	if err != nil {
		t.Fatalf("replay failed: %v", err)
	}

	if rec.Header().Get("X-Liltok-Cache-Status") != "HIT" {
		t.Errorf("expected HIT cache status, got %s", rec.Header().Get("X-Liltok-Cache-Status"))
	}
	if rec.Header().Get("X-Liltok-Cache-Tier") != "TIER1_EXACT" {
		t.Errorf("expected TIER1_EXACT, got %s", rec.Header().Get("X-Liltok-Cache-Tier"))
	}
	if rec.Header().Get("X-Liltok-Latency-Saved-Ms") != "500" {
		t.Errorf("expected 500ms saved, got %s", rec.Header().Get("X-Liltok-Latency-Saved-Ms"))
	}
	if rec.Body.String() != string(payload) {
		t.Errorf("body mismatch: %s", rec.Body.String())
	}
}

func TestReplayOpenAISSE(t *testing.T) {
	rec := &mockFlusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	payload := []byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hello streaming"}}]}`)
	entry := &CacheEntry{
		Hash:            "test-hash-sse",
		Model:           "gpt-4o",
		ResponsePayload: payload,
	}

	err := ReplayCacheHit(rec, entry, true, false, 850)
	if err != nil {
		t.Fatalf("replay failed: %v", err)
	}

	if !rec.flushed {
		t.Errorf("expected flusher to be called")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "chat.completion.chunk") {
		t.Errorf("expected chunk format in body: %s", body)
	}
	if !strings.Contains(body, "hello streaming") {
		t.Errorf("expected content in body: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("expected [DONE] in body: %s", body)
	}
}

func TestReplayAnthropicSSE(t *testing.T) {
	rec := &mockFlusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	payload := []byte(`{"id":"msg-1","content":[{"type":"text","text":"claude answer"}]}`)
	entry := &CacheEntry{
		Hash:             "test-hash-anth",
		Model:            "claude-3-5-sonnet-20241022",
		ResponsePayload:  payload,
		CompletionTokens: 12,
	}

	err := ReplayCacheHit(rec, entry, true, true, 1200)
	if err != nil {
		t.Fatalf("replay failed: %v", err)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "event: message_start") {
		t.Errorf("expected message_start event: %s", body)
	}
	if !strings.Contains(body, "claude answer") {
		t.Errorf("expected text delta: %s", body)
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Errorf("expected message_stop event: %s", body)
	}
}

func TestReplayAnthropicSSE_ToolUse(t *testing.T) {
	rec := &mockFlusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	payload := []byte(`{"id":"msg-tool-1","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_999","name":"execute_command","input":{"cmd":"dir"}}]}`)
	entry := &CacheEntry{
		Hash:             "test-hash-tool",
		Model:            "claude-opus-5",
		ResponsePayload:  payload,
		CompletionTokens: 20,
	}

	err := ReplayCacheHit(rec, entry, true, true, 1500)
	if err != nil {
		t.Fatalf("replay failed: %v", err)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "event: content_block_start") || !strings.Contains(body, "tool_use") {
		t.Errorf("expected tool_use block start in body: %s", body)
	}
	if !strings.Contains(body, "execute_command") || !strings.Contains(body, "toolu_999") {
		t.Errorf("expected tool metadata in body: %s", body)
	}
	if !strings.Contains(body, "stop_reason\":\"tool_use\"") {
		t.Errorf("expected stop_reason tool_use in message_delta: %s", body)
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Errorf("expected message_stop event: %s", body)
	}
}

func TestReplayAnthropicSSE_Thinking(t *testing.T) {
	rec := &mockFlusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	payload := []byte(`{"id":"msg-think-1","stop_reason":"end_turn","content":[{"type":"thinking","thinking":"Analyzing script requirements..."},{"type":"text","text":"Ready to proceed."}]}`)
	entry := &CacheEntry{
		Hash:             "test-hash-think",
		Model:            "claude-sonnet-5",
		ResponsePayload:  payload,
		CompletionTokens: 35,
	}

	err := ReplayCacheHit(rec, entry, true, true, 1400)
	if err != nil {
		t.Fatalf("replay failed: %v", err)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "event: content_block_start") || !strings.Contains(body, "thinking") {
		t.Errorf("expected thinking content_block_start in body: %s", body)
	}
	if !strings.Contains(body, "thinking_delta") || !strings.Contains(body, "Analyzing script requirements...") {
		t.Errorf("expected thinking_delta in body: %s", body)
	}
	if !strings.Contains(body, "text_delta") || !strings.Contains(body, "Ready to proceed.") {
		t.Errorf("expected text_delta in body: %s", body)
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Errorf("expected message_stop event: %s", body)
	}
}

