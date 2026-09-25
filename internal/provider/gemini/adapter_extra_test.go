package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/primaybr/liltok/internal/provider"
)

func TestAdapterAccessors(t *testing.T) {
	a := NewAdapter("http://example.test/", "k1; k2\nk3,,")
	if a.Name() != "gemini" || a.Tier() != provider.TierFree {
		t.Errorf("name/tier = %q/%q", a.Name(), a.Tier())
	}
	if a.KeyCount() != 3 {
		t.Errorf("expected 3 keys parsed, got %d", a.KeyCount())
	}
	if a.baseURL != "http://example.test" {
		t.Errorf("trailing slash must be trimmed, got %q", a.baseURL)
	}

	a.SetAPIKey("only-one")
	if a.KeyCount() != 1 || a.apiKey != "only-one" {
		t.Errorf("SetAPIKey did not replace keys: %d %q", a.KeyCount(), a.apiKey)
	}
	a.SetAPIKey("")
	if a.KeyCount() != 0 {
		t.Errorf("empty key must clear keys, got %d", a.KeyCount())
	}

	a.SetBaseURL("http://other.test//")
	if a.baseURL != "http://other.test" {
		t.Errorf("SetBaseURL = %q", a.baseURL)
	}
	a.SetBaseURL("")
	if a.baseURL != "https://generativelanguage.googleapis.com" {
		t.Errorf("empty base URL must reset to default, got %q", a.baseURL)
	}
	if NewAdapter("", "").baseURL != "https://generativelanguage.googleapis.com" {
		t.Errorf("NewAdapter must default the base URL")
	}
}

func TestListModels(t *testing.T) {
	var mu sync.Mutex
	var keysSeen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		mu.Lock()
		keysSeen = append(keysSeen, key)
		mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != "/v1beta/models" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		switch key {
		case "bad-status":
			http.Error(w, "denied", http.StatusForbidden)
		case "bad-json":
			_, _ = w.Write([]byte("{not json"))
		default:
			_, _ = w.Write([]byte(`{"models":[
				{"name":"models/gemini-x","displayName":"X","inputTokenLimit":1000},
				{"name":"plain-id","inputTokenLimit":5}
			]}`))
		}
	}))
	defer srv.Close()

	models, err := NewAdapter(srv.URL, "bad-status,bad-json,good").ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []provider.ModelInfo{
		{ID: "gemini-x", Provider: "gemini", Active: true, ContextWindow: 1000, OwnedBy: "google"},
		{ID: "plain-id", Provider: "gemini", Active: true, ContextWindow: 5, OwnedBy: "google"},
	}
	if !reflect.DeepEqual(models, want) {
		t.Errorf("models = %+v, want %+v", models, want)
	}
	mu.Lock()
	if strings.Join(keysSeen, ",") != "bad-status,bad-json,good" {
		t.Errorf("keys must be tried in order until one works, got %v", keysSeen)
	}
	mu.Unlock()

	if _, err := NewAdapter(srv.URL, "bad-status").ListModels(context.Background()); err == nil || !strings.Contains(err.Error(), "status 403") {
		t.Errorf("expected status error, got %v", err)
	}
	if _, err := NewAdapter(srv.URL, "bad-json").ListModels(context.Background()); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("expected decode error, got %v", err)
	}
	if _, err := NewAdapter(srv.URL, "").ListModels(context.Background()); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("expected missing key error, got %v", err)
	}
}

func closedServerURL() string {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

func TestConnectionFailures(t *testing.T) {
	a := NewAdapter(closedServerURL(), "test-key")
	ctx := context.Background()
	if _, err := a.ListModels(ctx); err == nil || !strings.Contains(err.Error(), "list models failed") {
		t.Errorf("ListModels: expected connection error, got %v", err)
	}
	if ok, err := a.CheckHealth(ctx); ok || err == nil || !strings.Contains(err.Error(), "connection failed") {
		t.Errorf("CheckHealth: expected connection error, got %v %v", ok, err)
	}
	_, err := a.SendChat(ctx, &provider.UnifiedChatRequest{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "all gemini api keys exhausted") || !strings.Contains(err.Error(), "request failed") {
		t.Errorf("SendChat: expected exhausted error wrapping the request failure, got %v", err)
	}

	// An invalid base URL makes request construction fail.
	bad := NewAdapter("http://bad host", "test-key")
	if _, err := bad.ListModels(ctx); err == nil {
		t.Errorf("ListModels: expected an error for an invalid URL")
	}
	if _, err := bad.CheckHealth(ctx); err == nil {
		t.Errorf("CheckHealth: expected an error for an invalid URL")
	}
	if _, err := bad.SendChat(ctx, &provider.UnifiedChatRequest{Model: "m"}); err == nil {
		t.Errorf("SendChat: expected an error for an invalid URL")
	}
}

func TestSendChat_RequestShape(t *testing.T) {
	var gotPath, gotKey, gotCT, gotMethod string
	var body map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotKey, gotCT = r.Method, r.URL.Path, r.URL.Query().Get("key"), r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"a"},{"text":"b"}]},"finishReason":"MAX_TOKENS"}],
			"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2,"totalTokenCount":3}}`))
	}))
	defer srv.Close()

	req := &provider.UnifiedChatRequest{
		Model:          "gemini-flash",
		SystemPrompt:   "be brief",
		Temperature:    0,
		HasTemperature: true,
		TopP:           0.5,
		MaxTokens:      64,
		Messages: []provider.UnifiedChatMessage{
			{Role: "system", Content: "dropped, sent as systemInstruction"},
			{Role: "user", Content: "hi"},
		},
		Tools: []interface{}{
			map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "lookup",
					"description": "look things up",
					"parameters":  map[string]interface{}{"type": "object", "properties": map[string]interface{}{"q": map[string]interface{}{"type": "string"}}},
				},
			},
		},
	}
	resp, err := NewAdapter(srv.URL, "test-key").SendChat(context.Background(), req)
	if err != nil {
		t.Fatalf("SendChat: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/v1beta/models/gemini-3.8-flash:generateContent" || gotKey != "test-key" || gotCT != "application/json" {
		t.Errorf("request = %s %s key=%q ct=%q", gotMethod, gotPath, gotKey, gotCT)
	}
	sys, _ := json.Marshal(body["systemInstruction"])
	if string(sys) != `{"parts":[{"text":"be brief"}]}` {
		t.Errorf("systemInstruction = %s", sys)
	}
	gc, _ := json.Marshal(body["generationConfig"])
	if string(gc) != `{"maxOutputTokens":64,"temperature":0,"topP":0.5}` {
		t.Errorf("generationConfig = %s (explicit temperature 0 must be sent)", gc)
	}
	contents, _ := json.Marshal(body["contents"])
	if string(contents) != `[{"parts":[{"text":"hi"}],"role":"user"}]` {
		t.Errorf("contents = %s", contents)
	}
	tools, _ := json.Marshal(body["tools"])
	if !strings.Contains(string(tools), `"name":"lookup"`) || !strings.Contains(string(tools), `"description":"look things up"`) {
		t.Errorf("tools = %s", tools)
	}

	if resp.Content != "ab" || resp.FinishReason != "max_tokens" || resp.Model != "gemini-flash" || resp.Role != "assistant" {
		t.Errorf("response = %+v", resp)
	}
	if resp.Usage != (provider.UnifiedUsage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}) {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if !strings.HasPrefix(resp.ID, "gemini-") || len(resp.RawResponse) == 0 {
		t.Errorf("id/raw not set: %q %d", resp.ID, len(resp.RawResponse))
	}
}

func TestSendChat_ModelPrefixKept(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"candidates":[]}`))
	}))
	defer srv.Close()

	resp, err := NewAdapter(srv.URL, "test-key").SendChat(context.Background(), &provider.UnifiedChatRequest{Model: "models/gemini-custom"})
	if err != nil {
		t.Fatalf("SendChat: %v", err)
	}
	if gotPath != "/v1beta/models/gemini-custom:generateContent" {
		t.Errorf("path = %q", gotPath)
	}
	if resp.Content != "" || resp.FinishReason != "stop" {
		t.Errorf("no candidates must give empty content and stop, got %+v", resp)
	}
}

func TestSendChat_ErrorResponses(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"non-retryable 400", http.StatusBadRequest, `{"error":"bad"}`, "gemini error 400"},
		{"invalid json", http.StatusOK, `{not json`, "failed to parse gemini json"},
		{"quota body on 200 retries then exhausts", http.StatusOK, `{"error":{"status":"RESOURCE_EXHAUSTED"}}`, "all gemini api keys exhausted"},
		{"503 exhausts", http.StatusServiceUnavailable, `busy`, "status 503"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			_, err := NewAdapter(srv.URL, "test-key").SendChat(context.Background(), &provider.UnifiedChatRequest{Model: "m"})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got %v, want error containing %q", err, tt.wantErr)
			}
			if calls != 1 {
				t.Errorf("one key means exactly one attempt, got %d", calls)
			}
		})
	}

	if _, err := NewAdapter("", "").SendChat(context.Background(), &provider.UnifiedChatRequest{Model: "m"}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("expected missing key error, got %v", err)
	}
}

func TestSendChat_RoundRobinStartKey(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.URL.Query().Get("key"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"candidates":[]}`))
	}))
	defer srv.Close()

	a := NewAdapter(srv.URL, "k1,k2,k3")
	for i := 0; i < 4; i++ {
		if _, err := a.SendChat(context.Background(), &provider.UnifiedChatRequest{Model: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(keys, ",") != "k1,k2,k3,k1" {
		t.Errorf("successive requests must rotate the starting key, got %v", keys)
	}
}

func TestSendChat_FunctionCallWithoutID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[
			{"text":"calling"},
			{"functionCall":{"name":"run","args":{"cmd":"ls"}}}
		]},"finishReason":"STOP"}]}`))
	}))
	defer srv.Close()

	resp, err := NewAdapter(srv.URL, "test-key").SendChat(context.Background(), &provider.UnifiedChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("SendChat: %v", err)
	}
	if resp.FinishReason != "tool_calls" || resp.Content != "calling" || len(resp.ToolCalls) != 1 {
		t.Fatalf("response = %+v", resp)
	}
	tc := resp.ToolCalls[0]
	if !strings.HasPrefix(tc.ID, "call_") || tc.Function.Name != "run" || tc.Function.Arguments != `{"cmd":"ls"}` || tc.Type != "function" {
		t.Errorf("tool call = %+v", tc)
	}
}

func TestStreamChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			t.Errorf("StreamChat must fall back to generateContent, got %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"streamed"}]},"finishReason":"STOP"}],
			"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":5,"totalTokenCount":9}}`))
	}))
	defer srv.Close()

	events, errs, err := NewAdapter(srv.URL, "test-key").StreamChat(context.Background(), &provider.UnifiedChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	var got []provider.UnifiedSSEEvent
	for ev := range events {
		got = append(got, ev)
	}
	for e := range errs {
		t.Errorf("unexpected stream error: %v", e)
	}
	if len(got) != 2 {
		t.Fatalf("expected text_delta and finish events, got %+v", got)
	}
	if got[0].Type != "text_delta" || got[0].DeltaText != "streamed" || got[0].Role != "assistant" {
		t.Errorf("first event = %+v", got[0])
	}
	if got[1].Type != "finish" || got[1].FinishReason != "stop" || got[1].Usage == nil || got[1].Usage.TotalTokens != 9 {
		t.Errorf("finish event = %+v", got[1])
	}

	if _, _, err := NewAdapter(srv.URL, "").StreamChat(context.Background(), &provider.UnifiedChatRequest{Model: "m"}); err == nil {
		t.Errorf("expected an error to be returned synchronously when SendChat fails")
	}
}

func TestBuildPayload_AssistantHistoryAndMerging(t *testing.T) {
	raw, err := NewAdapter("", "test-key").buildPayload(&provider.UnifiedChatRequest{
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "first"},
			{Role: "user", Content: "second"},
			{Role: "assistant", Content: "", ToolCalls: []provider.UnifiedToolCall{
				func() provider.UnifiedToolCall {
					var tc provider.UnifiedToolCall
					tc.Function.Name = "noargs"
					tc.Function.Arguments = "not json"
					return tc
				}(),
			}},
			{Role: "assistant", Content: "after"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Contents []struct {
			Role  string                   `json:"role"`
			Parts []map[string]interface{} `json:"parts"`
		} `json:"contents"`
		GenerationConfig map[string]interface{} `json:"generationConfig"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Contents) != 2 || payload.Contents[0].Role != "user" || len(payload.Contents[0].Parts) != 2 {
		t.Fatalf("consecutive user turns must merge: %s", raw)
	}
	model := payload.Contents[1]
	if model.Role != "model" || len(model.Parts) != 2 {
		t.Fatalf("assistant turns must merge into one model content: %s", raw)
	}
	fc, _ := json.Marshal(model.Parts[0])
	if string(fc) != `{"functionCall":{"args":{},"name":"noargs"},"thoughtSignature":"skip_thought_signature_validator"}` {
		t.Errorf("invalid arguments must become empty args, got %s", fc)
	}
	if payload.GenerationConfig != nil {
		t.Errorf("no sampling settings means no generationConfig, got %v", payload.GenerationConfig)
	}
}

func TestConvertToolsToGemini(t *testing.T) {
	if got := convertToolsToGemini([]interface{}{"not a map", map[string]interface{}{"description": "no name"}}); got != nil {
		t.Errorf("tools without usable entries must give nil, got %v", got)
	}

	got := convertToolsToGemini([]interface{}{
		map[string]interface{}{"name": "anthropic_tool", "description": "d", "input_schema": map[string]interface{}{"type": "OBJECT"}},
		map[string]interface{}{"name": "bare"},
		map[string]interface{}{"function": map[string]interface{}{"name": "fn_tool"}},
	})
	want := []map[string]interface{}{{"functionDeclarations": []map[string]interface{}{
		{"name": "anthropic_tool", "description": "d", "parameters": map[string]interface{}{"type": "object"}},
		{"name": "bare"},
		{"name": "fn_tool"},
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v\nwant %#v", got, want)
	}

	// A request whose tools all get dropped must not carry a tools field.
	raw, err := NewAdapter("", "test-key").buildPayload(&provider.UnifiedChatRequest{Tools: []interface{}{"junk"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"tools"`) {
		t.Errorf("payload must omit tools, got %s", raw)
	}
}

func mustJSON(t *testing.T, s string) interface{} {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("bad test JSON %q: %v", s, err)
	}
	return v
}

func TestCleanGeminiSchema_Keywords(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"const becomes enum", `{"const": 5}`, `{"enum":["5"],"type":"string"}`},
		{"const ignored when enum present", `{"const": "a", "enum": ["b"]}`, `{"enum":["b"],"type":"string"}`},
		{"exclusive bounds", `{"type":"integer","exclusiveMinimum":1,"exclusiveMaximum":9}`, `{"maximum":9,"minimum":1,"type":"integer"}`},
		{"explicit bounds win", `{"type":"number","exclusiveMinimum":1,"minimum":0,"exclusiveMaximum":9,"maximum":10}`, `{"maximum":10,"minimum":0,"type":"number"}`},
		{"prefixItems", `{"type":"array","prefixItems":[{"type":"STRING","title":"x"}]}`, `{"items":{"type":"string"},"type":"array"}`},
		{"type union with null", `{"type":["STRING","null"]}`, `{"nullable":true,"type":"string"}`},
		{"format, description, nullable", `{"type":"string","format":"date","description":"d","nullable":false,"pattern":"x","title":"t"}`, `{"description":"d","format":"date","nullable":false,"type":"string"}`},
		{"empty format and description dropped", `{"type":"string","format":"","description":""}`, `{"type":"string"}`},
		{"mixed enum stringified", `{"enum":["a",1,true]}`, `{"enum":["a","1","true"],"type":"string"}`},
		{"required keeps strings only", `{"type":"object","properties":{"a":{"type":"string"}},"required":["a",3]}`, `{"properties":{"a":{"type":"string"}},"required":["a"],"type":"object"}`},
		{"properties imply object", `{"properties":{"a":{}}}`, `{"properties":{"a":{}},"type":"object"}`},
		{"length and item limits", `{"type":"array","items":{"type":"string"},"min_items":1,"maxItems":3,"minLength":2,"max_length":4,"minimum":0,"maximum":1}`, `{"items":{"type":"string"},"maxItems":3,"maxLength":4,"maximum":1,"minItems":1,"minLength":2,"minimum":0,"type":"array"}`},
		{"array without items gets default", `{"type":"array"}`, `{"items":{"type":"string"},"type":"array"}`},
		{"items tuple", `{"type":"array","items":[{"type":"integer"}]}`, `{"items":{"type":"integer"},"type":"array"}`},
		{"items empty tuple", `{"type":"array","items":[]}`, `{"items":{"type":"string"},"type":"array"}`},
		{"items bool", `{"type":"array","items":true}`, `{"items":{"type":"string"},"type":"array"}`},
		{"items scalar", `{"type":"array","items":"weird"}`, `{"items":{"type":"string"},"type":"array"}`},
		{"items empty object", `{"type":"array","items":{}}`, `{"items":{"type":"string"},"type":"array"}`},
		{"anyOf nullable single", `{"anyOf":[{"type":"string"},{"type":"null"}]}`, `{"nullable":true,"type":"string"}`},
		{"anyOf nullable non-map", `{"anyOf":["x",{"type":"null"}]}`, `{"anyOf":["x"],"nullable":true}`},
		{"anyOf multiple", `{"any_of":[{"type":"string"},{"type":"integer"}]}`, `{"anyOf":[{"type":"string"},{"type":"integer"}]}`},
		{"anyOf only null", `{"anyOf":[{"type":"null"}]}`, `{}`},
		{"anyOf not a list", `{"anyOf":{"type":"string"}}`, `{}`},
		{"dropped keywords", `{"$schema":"x","additionalProperties":false,"default":1,"$ref":"#/a"}`, `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := json.Marshal(cleanGeminiSchema(mustJSON(t, tt.in)))
			if string(got) != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestCleanGeminiSchema_NonMapInputs(t *testing.T) {
	if cleanGeminiSchema(nil) != nil {
		t.Errorf("nil must stay nil")
	}
	if cleanGeminiSchema("s") != "s" || cleanGeminiSchema(3.0) != 3.0 {
		t.Errorf("scalars must pass through unchanged")
	}
	got, _ := json.Marshal(cleanGeminiSchema([]interface{}{map[string]interface{}{"type": "STRING", "title": "x"}, 1.0}))
	if string(got) != `[{"type":"string"},1]` {
		t.Errorf("list items must be cleaned individually, got %s", got)
	}

	// Go-typed string slices (not produced by json.Unmarshal) are accepted for enum and required.
	got, _ = json.Marshal(cleanGeminiSchema(map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"a": map[string]interface{}{"enum": []string{"x", "y"}}},
		"required":   []string{"a"},
	}))
	if string(got) != `{"properties":{"a":{"enum":["x","y"],"type":"string"}},"required":["a"],"type":"object"}` {
		t.Errorf("got %s", got)
	}
}
