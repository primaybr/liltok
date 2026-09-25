package anthropic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/primaybr/liltok/internal/provider"
)

// realStream is an Anthropic Messages stream with a thinking block, a text block and a tool_use
// block whose input arrives as several input_json_delta fragments.
var realStream = strings.Join([]string{
	`event: message_start`,
	`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-test","usage":{"input_tokens":1200,"output_tokens":1,"cache_read_input_tokens":800}}}`,
	``,
	`event: content_block_start`,
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
	``,
	`event: content_block_delta`,
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Need the file."}}`,
	``,
	`event: content_block_stop`,
	`data: {"type":"content_block_stop","index":0}`,
	``,
	`event: content_block_start`,
	`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
	``,
	`event: content_block_delta`,
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Reading it."}}`,
	``,
	`event: content_block_stop`,
	`data: {"type":"content_block_stop","index":1}`,
	``,
	`event: content_block_start`,
	`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_7","name":"Read","input":{}}}`,
	``,
	`event: content_block_delta`,
	`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"file_"}}`,
	``,
	`event: content_block_delta`,
	`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"path\": \"main"}}`,
	``,
	`event: content_block_delta`,
	`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":".go\"}"}}`,
	``,
	`event: content_block_stop`,
	`data: {"type":"content_block_stop","index":2}`,
	``,
	`event: message_delta`,
	`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`,
	``,
	`event: message_stop`,
	`data: {"type":"message_stop"}`,
	``,
}, "\n")

func streamFrom(t *testing.T, body string) (<-chan provider.UnifiedSSEEvent, <-chan error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	events, errs, err := NewAdapter(srv.URL, "test-key").StreamChat(context.Background(), &provider.UnifiedChatRequest{
		Model:    "claude-test",
		Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamChat error: %v", err)
	}
	return events, errs
}

func TestStreamChatThinkingTextToolCallAndUsage(t *testing.T) {
	events, errs := streamFrom(t, realStream)
	var got []provider.UnifiedSSEEvent
	for ev := range events {
		got = append(got, ev)
	}
	if err := <-errs; err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}

	var types []string
	for _, ev := range got {
		types = append(types, ev.Type)
	}
	wantTypes := []string{"thinking_delta", "text_delta", "tool_call", "finish", "done"}
	if !reflect.DeepEqual(types, wantTypes) {
		t.Fatalf("event types = %v, want %v", types, wantTypes)
	}
	if got[0].DeltaText != "Need the file." || got[1].DeltaText != "Reading it." {
		t.Errorf("thinking/text deltas = %q / %q", got[0].DeltaText, got[1].DeltaText)
	}

	calls := got[2].ToolCalls
	if len(calls) != 1 || calls[0].ID != "toolu_7" || calls[0].Function.Name != "Read" ||
		calls[0].Function.Arguments != `{"file_path": "main.go"}` {
		t.Errorf("tool call = %+v, want Read toolu_7 with assembled input", calls)
	}

	finish := got[3]
	wantUsage := &provider.UnifiedUsage{PromptTokens: 1200, CompletionTokens: 42, TotalTokens: 1242, CachedTokens: 800}
	if finish.FinishReason != "tool_use" || !reflect.DeepEqual(finish.Usage, wantUsage) {
		t.Errorf("finish = %q usage %+v, want tool_use %+v", finish.FinishReason, finish.Usage, wantUsage)
	}
}

func TestStreamChatToolWithoutInputAndErrorEvent(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"TodoWrite"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"never sent"}}`,
		``,
	}, "\n")
	events, errs := streamFrom(t, body)
	var got []provider.UnifiedSSEEvent
	for ev := range events {
		got = append(got, ev)
	}
	if len(got) != 1 || got[0].Type != "tool_call" || got[0].ToolCalls[0].Function.Arguments != "{}" {
		t.Fatalf("want one tool_call with {} arguments before the error, got %+v", got)
	}
	err := <-errs
	if err == nil || !strings.Contains(err.Error(), "overloaded_error") || !strings.Contains(err.Error(), "Overloaded") {
		t.Fatalf("want overloaded stream error, got %v", err)
	}
}

// A consumer that stops reading and cancels must not leave the stream goroutine blocked.
func TestStreamChatStopsWhenConsumerCancels(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 500; i++ { // more events than the channel buffer holds
		sb.WriteString(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}` + "\n")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, sb.String())
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	events, _, err := NewAdapter(srv.URL, "test-key").StreamChat(ctx, &provider.UnifiedChatRequest{Model: "claude-test"})
	if err != nil {
		t.Fatal(err)
	}
	<-events // read one event, then walk away
	cancel()

	done := make(chan struct{})
	go func() {
		for range events {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("event channel was not closed after the consumer cancelled")
	}
}

// StreamChat reads the base URL under the adapter lock, like SendChat.
func TestStreamChatUsesCurrentBaseURL(t *testing.T) {
	hit := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit <- r.URL.Path
		_, _ = io.WriteString(w, `data: {"type":"message_stop"}`+"\n")
	}))
	defer srv.Close()

	a := NewAdapter("http://127.0.0.1:1", "test-key")
	a.SetBaseURL(srv.URL)
	events, _, err := a.StreamChat(context.Background(), &provider.UnifiedChatRequest{Model: "claude-test"})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if path := <-hit; path != "/v1/messages" {
		t.Errorf("request path = %q, want /v1/messages", path)
	}
}
