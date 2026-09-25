package openai

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

// recordedRequest captures what the adapter sent upstream.
type recordedRequest struct {
	Method, Path string
	Header       http.Header
	Body         map[string]interface{}
}

// recordingServer serves the given status, content type and body and records each request.
func recordingServer(t *testing.T, status int, contentType, body string) (*httptest.Server, func() []recordedRequest) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec := recordedRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone()}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &rec.Body); err != nil {
				t.Errorf("request body is not JSON: %v", err)
			}
		}
		mu.Lock()
		reqs = append(reqs, rec)
		mu.Unlock()
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), reqs...)
	}
}

func closedServerURL() string {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

func TestConstructors(t *testing.T) {
	tests := []struct {
		name     string
		a        *Adapter
		wantName string
		wantURL  string
		wantKey  string
	}{
		{"nvidia", NewNVIDIANIMAdapter("k"), "nvidianim", "https://integrate.api.nvidia.com/v1", "k"},
		{"groq", NewGroqAdapter("k"), "groq", "https://api.groq.com/openai/v1", "k"},
		{"openrouter default", NewOpenRouterAdapter("k", ""), "openrouter", "https://openrouter.ai/api/v1", "k"},
		{"openrouter custom", NewOpenRouterAdapter("k", "http://or.test/"), "openrouter", "http://or.test", "k"},
		{"ollama default", NewOllamaAdapter(""), "ollama", "http://localhost:11434/v1", ""},
		{"ollama custom", NewOllamaAdapter("http://ollama.test"), "ollama", "http://ollama.test", ""},
		{"kilo default", NewKiloAdapter("k", ""), "kilo", "https://api.kilo.ai/api/gateway", "k"},
		{"kilo custom", NewKiloAdapter("k", "http://kilo.test"), "kilo", "http://kilo.test", "k"},
		{"cline default", NewClineAdapter("k", ""), "cline", "https://api.cline.bot/api/v1", "k"},
		{"cline custom", NewClineAdapter("k", "http://cline.test"), "cline", "http://cline.test", "k"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.a.Name() != tt.wantName || tt.a.Tier() != provider.TierFree || tt.a.baseURL != tt.wantURL || tt.a.apiKey != tt.wantKey {
				t.Errorf("got name=%q tier=%q url=%q key=%q", tt.a.Name(), tt.a.Tier(), tt.a.baseURL, tt.a.apiKey)
			}
		})
	}
	if a := NewAdapter("openai", provider.TierPremium, "", ""); a.Tier() != provider.TierPremium {
		t.Errorf("tier = %q", a.Tier())
	}
}

func TestSetAPIKeyAndBaseURLUsedByRequests(t *testing.T) {
	srv, reqs := recordingServer(t, http.StatusOK, "application/json",
		`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)

	a := NewAdapter("openai", provider.TierPremium, "http://unused.invalid", "old-key")
	a.SetBaseURL(srv.URL + "/")
	a.SetAPIKey("new-key")
	if _, err := a.SendChat(context.Background(), &provider.UnifiedChatRequest{Model: "m"}); err != nil {
		t.Fatalf("SendChat: %v", err)
	}
	got := reqs()
	if len(got) != 1 || got[0].Path != "/chat/completions" || got[0].Header.Get("Authorization") != "Bearer new-key" {
		t.Errorf("requests = %+v", got)
	}
}

func TestCheckHealth(t *testing.T) {
	ctx := context.Background()

	if ok, err := NewAdapter("openai", provider.TierPremium, "http://x", "").CheckHealth(ctx); ok || err == nil || !strings.Contains(err.Error(), "openai api key is not configured") {
		t.Errorf("missing key: got %v %v", ok, err)
	}
	for _, url := range []string{"", "disabled"} {
		a := NewOllamaAdapter("")
		a.SetBaseURL(url)
		if ok, err := a.CheckHealth(ctx); ok || err == nil || !strings.Contains(err.Error(), "disabled") {
			t.Errorf("ollama %q: got %v %v", url, ok, err)
		}
	}

	srv, reqs := recordingServer(t, http.StatusOK, "application/json", `{}`)
	cases := []struct {
		name       string
		a          *Adapter
		wantPath   string
		wantAuth   string
		wantOR     bool
		wantHealth bool
	}{
		{"openai", NewAdapter("openai", provider.TierPremium, srv.URL, "test-key"), "/models", "Bearer test-key", false, true},
		{"openrouter headers", NewOpenRouterAdapter("test-key", srv.URL), "/models", "Bearer test-key", true, true},
		{"kilo without key", NewKiloAdapter("", srv.URL), "/models", "", false, true},
		{"ollama native", NewOllamaAdapter(srv.URL), "/api/tags", "", false, true},
		{"ollama v1", NewOllamaAdapter(srv.URL + "/v1"), "/v1/models", "", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := len(reqs())
			ok, err := c.a.CheckHealth(ctx)
			if ok != c.wantHealth || err != nil {
				t.Fatalf("got %v %v", ok, err)
			}
			r := reqs()[before]
			if r.Method != http.MethodGet || r.Path != c.wantPath || r.Header.Get("Authorization") != c.wantAuth {
				t.Errorf("request = %s %s auth=%q", r.Method, r.Path, r.Header.Get("Authorization"))
			}
			if hasOR := r.Header.Get("X-Title") == "liltok" && r.Header.Get("HTTP-Referer") != ""; hasOR != c.wantOR {
				t.Errorf("openrouter headers present = %v, want %v", hasOR, c.wantOR)
			}
		})
	}

	bad, _ := recordingServer(t, http.StatusUnauthorized, "application/json", `{"error":"nope"}`)
	if ok, err := NewGroqAdapter("test-key").withURL(bad.URL).CheckHealth(ctx); ok || err == nil || !strings.Contains(err.Error(), "groq returned status 401") || !strings.Contains(err.Error(), "nope") {
		t.Errorf("status error: got %v %v", ok, err)
	}
	if ok, err := NewGroqAdapter("test-key").withURL(closedServerURL()).CheckHealth(ctx); ok || err == nil || !strings.Contains(err.Error(), "groq connection failed") {
		t.Errorf("connection error: got %v %v", ok, err)
	}
	if _, err := NewGroqAdapter("test-key").withURL("http://bad host").CheckHealth(ctx); err == nil {
		t.Errorf("expected an error for an invalid URL")
	}
}

// withURL points the adapter at a test server.
func (a *Adapter) withURL(url string) *Adapter {
	a.SetBaseURL(url)
	return a
}

func TestListModels_ErrorsAndOllamaTags(t *testing.T) {
	ctx := context.Background()
	if _, err := NewGroqAdapter("").ListModels(ctx); err == nil || !strings.Contains(err.Error(), "groq api key is not configured") {
		t.Errorf("missing key: %v", err)
	}
	if _, err := NewOllamaAdapter("").withURL("disabled").ListModels(ctx); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("ollama disabled: %v", err)
	}

	tags, reqs := recordingServer(t, http.StatusOK, "application/json",
		`{"models":[{"name":"llama3:8b"},{"model":"qwen:7b"},{}]}`)
	models, err := NewOllamaAdapter(tags.URL).ListModels(ctx)
	if err != nil {
		t.Fatalf("ollama tags: %v", err)
	}
	want := []provider.ModelInfo{
		{ID: "llama3:8b", Provider: "ollama", Active: true},
		{ID: "qwen:7b", Provider: "ollama", Active: true},
	}
	if !reflect.DeepEqual(models, want) {
		t.Errorf("models = %+v", models)
	}
	if r := reqs()[0]; r.Path != "/api/tags" || r.Header.Get("Authorization") != "" {
		t.Errorf("request = %s auth=%q", r.Path, r.Header.Get("Authorization"))
	}

	badTags, _ := recordingServer(t, http.StatusOK, "application/json", `not json`)
	if _, err := NewOllamaAdapter(badTags.URL).ListModels(ctx); err == nil || !strings.Contains(err.Error(), "failed to decode ollama tags") {
		t.Errorf("bad tags: %v", err)
	}
	if _, err := NewGroqAdapter("test-key").withURL(badTags.URL).ListModels(ctx); err == nil || !strings.Contains(err.Error(), "failed to decode models list") {
		t.Errorf("bad models list: %v", err)
	}

	status, _ := recordingServer(t, http.StatusInternalServerError, "", `boom`)
	if _, err := NewGroqAdapter("test-key").withURL(status.URL).ListModels(ctx); err == nil || !strings.Contains(err.Error(), "groq returned status 500: boom") {
		t.Errorf("status error: %v", err)
	}
	if _, err := NewGroqAdapter("test-key").withURL(closedServerURL()).ListModels(ctx); err == nil || !strings.Contains(err.Error(), "groq list models failed") {
		t.Errorf("connection error: %v", err)
	}
	if _, err := NewGroqAdapter("test-key").withURL("http://bad host").ListModels(ctx); err == nil {
		t.Errorf("expected an error for an invalid URL")
	}
}

func TestListModels_ContextWindowAndOwner(t *testing.T) {
	srv, reqs := recordingServer(t, http.StatusOK, "application/json", `{"data":[
		{"id":"vendor/ctx-length","context_length":1000},
		{"id":"max-ctx","max_context_length":2000,"owned_by":"me"},
		{"id":"explicit","context_window":3000},
		{"id":"inactive","active":false},
		{"id":"meta/llama-3.2-3b-instruct"},
		{"id":"x/laguna-xs"},
		{"id":"x/diffusiongemma"},
		{"id":"nvidia/nv-embed-v1"}
	]}`)

	models, err := NewNVIDIANIMAdapter("test-key").withURL(srv.URL).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	got := map[string]provider.ModelInfo{}
	for _, m := range models {
		got[m.ID] = m
	}
	want := map[string]struct {
		ctx   int
		owner string
	}{
		"vendor/ctx-length":          {1000, "vendor"},
		"max-ctx":                    {2000, "me"},
		"explicit":                   {3000, ""},
		"meta/llama-3.2-3b-instruct": {131072, "meta"},
		"x/laguna-xs":                {65536, "x"},
		"x/diffusiongemma":           {32768, "x"},
	}
	if len(got) != len(want) {
		t.Errorf("expected inactive and embedding models filtered, got %+v", models)
	}
	for id, w := range want {
		m, ok := got[id]
		if !ok || m.ContextWindow != w.ctx || m.OwnedBy != w.owner || m.Provider != "nvidianim" || !m.Active {
			t.Errorf("%s = %+v, want ctx %d owner %q", id, m, w.ctx, w.owner)
		}
	}
	if r := reqs()[0]; r.Path != "/models" || r.Header.Get("Authorization") != "Bearer test-key" {
		t.Errorf("request = %s auth=%q", r.Path, r.Header.Get("Authorization"))
	}

	// Other providers without a context size get no default.
	plain, _ := recordingServer(t, http.StatusOK, "application/json", `{"data":[{"id":"gpt-x"}]}`)
	models, err = NewAdapter("openai", provider.TierPremium, plain.URL, "test-key").ListModels(context.Background())
	if err != nil || len(models) != 1 || models[0].ContextWindow != 0 || models[0].OwnedBy != "" {
		t.Errorf("openai models = %+v, err %v", models, err)
	}

	// OpenRouter sends its identification headers and gets the large default window.
	or, orReqs := recordingServer(t, http.StatusOK, "application/json", `{"data":[{"id":"a/b:free"}]}`)
	models, err = NewOpenRouterAdapter("test-key", or.URL).ListModels(context.Background())
	if err != nil || len(models) != 1 || models[0].ContextWindow != 262144 {
		t.Errorf("openrouter models = %+v, err %v", models, err)
	}
	if h := orReqs()[0].Header; h.Get("X-Title") != "liltok" || h.Get("HTTP-Referer") == "" {
		t.Errorf("openrouter headers missing: %v", h)
	}
}

func TestGetNVIDIANIMContextWindow(t *testing.T) {
	tests := map[string]int{
		"deepseek-ai/deepseek-v4-flash": 131072,
		"nvidia/Nemotron-3.5-super":     131072,
		"google/gemma-4-31b-it":         131072,
		"z-ai/glm-5.3":                  131072,
		"openai/gpt-oss-20b":            131072,
		"mistralai/mistral-nemotron":    131072,
		"poolside/laguna-xs":            65536,
		"google/diffusiongemma-2b":      32768,
		"unknown/model":                 32768,
	}
	for id, want := range tests {
		if got := getNVIDIANIMContextWindow(id); got != want {
			t.Errorf("%s = %d, want %d", id, got, want)
		}
	}
}

func TestFreeModelFilters(t *testing.T) {
	audio := []string{"text", "AUDIO"}
	tests := []struct {
		name  string
		fn    func(id, p, c string, mods []string) bool
		id    string
		p, c  string
		mods  []string
		wantF bool
	}{
		{"or audio modality", isOpenRouterFreeChatModel, "a/b:free", "0", "0", audio, false},
		{"or lyria", isOpenRouterFreeChatModel, "google/lyria:free", "0", "0", nil, false},
		{"or guard", isOpenRouterFreeChatModel, "meta/llama-guard:free", "0", "0", nil, false},
		{"or zero price", isOpenRouterFreeChatModel, "a/b", "0", "0", nil, true},
		{"or paid", isOpenRouterFreeChatModel, "a/b", "0.1", "0.2", nil, false},
		{"or router", isOpenRouterFreeChatModel, "openrouter/free", "", "", nil, true},
		{"kilo audio", isKiloFreeChatModel, "a/b:free", "0", "0", audio, false},
		{"kilo music", isKiloFreeChatModel, "x/music-gen:free", "0", "0", nil, false},
		{"kilo safety", isKiloFreeChatModel, "x/safety:free", "0", "0", nil, false},
		{"kilo tiny price", isKiloFreeChatModel, "a/b", "0.0000000000001", "0", nil, true},
		{"kilo free suffix", isKiloFreeChatModel, "kilo-auto/free", "1", "1", nil, true},
		{"kilo paid", isKiloFreeChatModel, "a/b", "0.5", "0", nil, false},
	}
	for _, tt := range tests {
		if got := tt.fn(tt.id, tt.p, tt.c, tt.mods); got != tt.wantF {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.wantF)
		}
	}

	cline := map[string]bool{
		"a/b:free":              true,
		"x/free-model":          true,
		"a/b":                   false,
		"x/embed:free":          false,
		"x/moderation:free":     false,
		"meta/llama-guard/free": false,
	}
	for id, want := range cline {
		if got := isClineFreeChatModel(id); got != want {
			t.Errorf("cline %s: got %v, want %v", id, got, want)
		}
	}
}

func TestSendChat_RequestAndHeaders(t *testing.T) {
	srv, reqs := recordingServer(t, http.StatusOK, "application/json", `{
		"id":"c1","model":"served-model",
		"choices":[{"message":{"role":"assistant","content":"","tool_calls":[
			{"id":"t1","type":"function","function":{"name":"run","arguments":"{\"a\":1}"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`)

	a := NewOpenRouterAdapter("test-key", srv.URL)
	resp, err := a.SendChat(context.Background(), &provider.UnifiedChatRequest{
		Model:    "a/b:free",
		Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("SendChat: %v", err)
	}
	r := reqs()[0]
	if r.Method != http.MethodPost || r.Path != "/chat/completions" || r.Header.Get("Content-Type") != "application/json" ||
		r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("X-Title") != "liltok" {
		t.Errorf("request = %s %s headers %v", r.Method, r.Path, r.Header)
	}
	if r.Body["model"] != "a/b:free" || r.Body["stream"] != false {
		t.Errorf("body = %v", r.Body)
	}
	if resp.ID != "c1" || resp.Model != "served-model" || resp.FinishReason != "tool_calls" || len(resp.ToolCalls) != 1 {
		t.Fatalf("response = %+v", resp)
	}
	if tc := resp.ToolCalls[0]; tc.ID != "t1" || tc.Type != "function" || tc.Function.Name != "run" || tc.Function.Arguments != `{"a":1}` {
		t.Errorf("tool call = %+v", tc)
	}
	if resp.Usage != (provider.UnifiedUsage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7}) {
		t.Errorf("usage = %+v", resp.Usage)
	}

	// A keyless local provider sends no Authorization header.
	local, localReqs := recordingServer(t, http.StatusOK, "application/json", `{"choices":[{"message":{"content":"x"}}]}`)
	if _, err := NewOllamaAdapter(local.URL).SendChat(context.Background(), &provider.UnifiedChatRequest{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if h := localReqs()[0].Header; h.Get("Authorization") != "" || h.Get("X-Title") != "" {
		t.Errorf("unexpected headers for ollama: %v", h)
	}
}

func TestSendChat_ContentFallbacks(t *testing.T) {
	tests := []struct {
		name, msg, wantContent, wantReasoning string
	}{
		{"reasoning field", `{"content":"","reasoning":"r1"}`, "r1", "r1"},
		{"thought field", `{"content":"  ","thought":"t1"}`, "t1", "t1"},
		{"refusal", `{"content":"","refusal":"no"}`, "no", ""},
		{"content kept with reasoning", `{"content":"answer","reasoning_content":"why"}`, "answer", "why"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := recordingServer(t, http.StatusOK, "application/json",
				`{"choices":[{"message":`+tt.msg+`,"finish_reason":"stop"}]}`)
			resp, err := NewGroqAdapter("test-key").withURL(srv.URL).SendChat(context.Background(), &provider.UnifiedChatRequest{Model: "m"})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Content != tt.wantContent || resp.ReasoningContent != tt.wantReasoning {
				t.Errorf("content %q reasoning %q, want %q %q", resp.Content, resp.ReasoningContent, tt.wantContent, tt.wantReasoning)
			}
		})
	}
}

func TestSendChat_Errors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"http status", http.StatusBadGateway, `bad gateway`, "upstream error status 502: bad gateway"},
		{"invalid json", http.StatusOK, `{nope`, "failed to parse upstream json"},
		{"error object with message", http.StatusOK, `{"error":{"message":"quota gone","code":1}}`, "upstream error: quota gone"},
		{"error object without message", http.StatusOK, `{"error":{"code":42}}`, "upstream error: map[code:42]"},
		{"error string", http.StatusOK, `{"error":"flat error"}`, "upstream error: flat error"},
		{"empty error string then no choices", http.StatusOK, `{"error":""}`, "upstream returned no choices"},
		{"no choices", http.StatusOK, `{"choices":[]}`, "upstream returned no choices"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := recordingServer(t, tt.status, "application/json", tt.body)
			_, err := NewGroqAdapter("test-key").withURL(srv.URL).SendChat(context.Background(), &provider.UnifiedChatRequest{Model: "m"})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got %v, want error containing %q", err, tt.wantErr)
			}
		})
	}

	if _, err := NewGroqAdapter("test-key").withURL(closedServerURL()).SendChat(context.Background(), &provider.UnifiedChatRequest{Model: "m"}); err == nil || !strings.Contains(err.Error(), "upstream request failed") {
		t.Errorf("connection error: %v", err)
	}
	if _, err := NewGroqAdapter("test-key").withURL("http://bad host").SendChat(context.Background(), &provider.UnifiedChatRequest{Model: "m"}); err == nil {
		t.Errorf("expected an error for an invalid URL")
	}
}

func collectStream(t *testing.T, events <-chan provider.UnifiedSSEEvent, errs <-chan error) ([]provider.UnifiedSSEEvent, []error) {
	t.Helper()
	var evs []provider.UnifiedSSEEvent
	for ev := range events {
		evs = append(evs, ev)
	}
	var es []error
	for e := range errs {
		es = append(es, e)
	}
	return evs, es
}

func TestStreamChat_EventsAndRequest(t *testing.T) {
	body := strings.Join([]string{
		": keep-alive comment",
		"event: message",
		`data: {"choices":[{"delta":{"role":"assistant","content":"He"}}]}`,
		`data: {not json}`,
		`data: {"choices":[]}`,
		`data: {"choices":[{"delta":{"content":"llo"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"data: [DONE]",
		`data: {"choices":[{"delta":{"content":"after done is ignored"}}]}`,
	}, "\n\n") + "\n\n"
	srv, reqs := recordingServer(t, http.StatusOK, "text/event-stream", body)

	events, errs, err := NewOpenRouterAdapter("test-key", srv.URL).StreamChat(context.Background(), &provider.UnifiedChatRequest{
		Model:    "a/b:free",
		Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	evs, es := collectStream(t, events, errs)
	if len(es) != 0 {
		t.Errorf("unexpected errors: %v", es)
	}
	var types []string
	text := ""
	for _, ev := range evs {
		types = append(types, ev.Type)
		text += ev.DeltaText
	}
	if strings.Join(types, ",") != "text_delta,text_delta,finish,done" || text != "Hello" {
		t.Errorf("events = %v text %q", types, text)
	}
	if evs[0].Role != "assistant" || !strings.HasSuffix(string(evs[0].RawChunk), "\n\n") || evs[2].FinishReason != "stop" {
		t.Errorf("event details wrong: %+v", evs)
	}

	r := reqs()[0]
	if r.Method != http.MethodPost || r.Path != "/chat/completions" || r.Body["stream"] != true ||
		r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("X-Title") != "liltok" {
		t.Errorf("request = %s %s body %v headers %v", r.Method, r.Path, r.Body, r.Header)
	}
}

func TestStreamChat_Errors(t *testing.T) {
	tests := []struct {
		name, ct, body, wantErr string
		status                  int
	}{
		{"http status", "text/plain", "overloaded", "upstream error status 503: overloaded", http.StatusServiceUnavailable},
		{"json error field", "application/json", `{"error":{"message":"bad model"}}`, "upstream stream error: map[message:bad model]", http.StatusOK},
		{"json without error", "application/json; charset=utf-8", `{"ok":true}`, "upstream returned json error status 200", http.StatusOK},
		{"json invalid", "application/json", `nope`, "upstream returned json error status 200: nope", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := recordingServer(t, tt.status, tt.ct, tt.body)
			_, _, err := NewGroqAdapter("test-key").withURL(srv.URL).StreamChat(context.Background(), &provider.UnifiedChatRequest{Model: "m"})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got %v, want error containing %q", err, tt.wantErr)
			}
		})
	}

	if _, _, err := NewGroqAdapter("test-key").withURL(closedServerURL()).StreamChat(context.Background(), &provider.UnifiedChatRequest{Model: "m"}); err == nil || !strings.Contains(err.Error(), "upstream stream request failed") {
		t.Errorf("connection error: %v", err)
	}
	if _, _, err := NewGroqAdapter("test-key").withURL("http://bad host").StreamChat(context.Background(), &provider.UnifiedChatRequest{Model: "m"}); err == nil {
		t.Errorf("expected an error for an invalid URL")
	}
}

func TestStreamChat_OversizedLineReportsError(t *testing.T) {
	// A single SSE line above the 512 KiB scanner limit ends the stream with an error.
	huge := "data: " + strings.Repeat("x", 600*1024) + "\n\n"
	srv, _ := recordingServer(t, http.StatusOK, "text/event-stream", huge)
	events, errs, err := NewGroqAdapter("test-key").withURL(srv.URL).StreamChat(context.Background(), &provider.UnifiedChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	evs, es := collectStream(t, events, errs)
	if len(evs) != 0 || len(es) != 1 {
		t.Errorf("expected no events and one error, got %d events and %v", len(evs), es)
	}
}

// StreamChat must read the key and base URL under the adapter lock, since the dashboard can
// update them while requests are in flight. Run with -race.
func TestStreamChat_ConcurrentConfigUpdate(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, "text/event-stream", "data: [DONE]\n\n")
	a := NewGroqAdapter("test-key").withURL(srv.URL)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			a.SetAPIKey("test-key")
			a.SetBaseURL(srv.URL)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			events, errs, err := a.StreamChat(context.Background(), &provider.UnifiedChatRequest{Model: "m"})
			if err != nil {
				t.Errorf("StreamChat: %v", err)
				return
			}
			collectStream(t, events, errs)
		}
	}()
	wg.Wait()
}

func TestBuildPayload_OpenAIPassthrough(t *testing.T) {
	a := NewAdapter("openai", provider.TierPremium, "http://x", "test-key")
	raw, err := a.buildPayload(&provider.UnifiedChatRequest{
		Model:      "routed-model",
		RawPayload: []byte(`{"model":"client-model","stream":false,"logprobs":true,"messages":[{"role":"user","content":"hi"}]}`),
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "routed-model" || got["stream"] != true || got["logprobs"] != true {
		t.Errorf("passthrough must keep extra fields and override model and stream, got %v", got)
	}

	// An unparsable raw payload falls back to building from the unified fields.
	raw, err = a.buildPayload(&provider.UnifiedChatRequest{
		Model:      "m",
		RawPayload: []byte(`not json`),
		Messages:   []provider.UnifiedChatMessage{{Role: "user", Content: "hi"}},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"messages":[{"content":"hi","role":"user"}]`) {
		t.Errorf("fallback payload = %s", raw)
	}
}

func TestBuildPayload_SystemMessagesAndLimits(t *testing.T) {
	tool := map[string]interface{}{"name": "t", "input_schema": map[string]interface{}{"type": "object"}}
	tests := []struct {
		name      string
		adapter   string
		req       provider.UnifiedChatRequest
		wantMax   interface{}
		wantTopP  interface{}
		wantFirst string
	}{
		{"groq cap", "groq", provider.UnifiedChatRequest{MaxTokens: 20000}, float64(8192), nil, ""},
		{"nvidia cap", "nvidianim", provider.UnifiedChatRequest{MaxTokens: 20000, TopP: 0.9}, float64(16384), 0.9, ""},
		{"openai no cap", "openai", provider.UnifiedChatRequest{MaxTokens: 20000}, float64(20000), nil, ""},
		{"tools raise small max", "openai", provider.UnifiedChatRequest{MaxTokens: 100, Tools: []interface{}{tool}}, float64(4096), nil, ""},
		{"tools set default max", "openai", provider.UnifiedChatRequest{Tools: []interface{}{tool}}, float64(4096), nil, ""},
		{"no max", "openai", provider.UnifiedChatRequest{}, nil, nil, ""},
		{"duplicate leading system skipped", "openai", provider.UnifiedChatRequest{
			SystemPrompt: "sys",
			Messages:     []provider.UnifiedChatMessage{{Role: "system", Content: "sys"}, {Role: "user", Content: "u"}},
		}, nil, nil, "system:sys"},
		{"leading system kept without prompt", "openai", provider.UnifiedChatRequest{
			Messages: []provider.UnifiedChatMessage{{Role: "system", Content: "only"}, {Role: "user", Content: "u"}},
		}, nil, nil, "system:only"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.req
			req.IsAnthropicSource = true
			raw, err := NewAdapter(tt.adapter, provider.TierFree, "http://x", "k").buildPayload(&req, false)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				MaxTokens interface{}              `json:"max_tokens"`
				TopP      interface{}              `json:"top_p"`
				Messages  []map[string]interface{} `json:"messages"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if got.MaxTokens != tt.wantMax || got.TopP != tt.wantTopP {
				t.Errorf("max_tokens %v top_p %v, want %v %v", got.MaxTokens, got.TopP, tt.wantMax, tt.wantTopP)
			}
			if tt.wantFirst != "" {
				if len(got.Messages) != 2 {
					t.Fatalf("messages = %v", got.Messages)
				}
				if first := got.Messages[0]["role"].(string) + ":" + got.Messages[0]["content"].(string); first != tt.wantFirst {
					t.Errorf("first message = %q, want %q", first, tt.wantFirst)
				}
			}
		})
	}
}

func TestBuildPayload_AssistantToolCallWithTextAndToolWithoutID(t *testing.T) {
	var tc provider.UnifiedToolCall
	tc.ID, tc.Function.Name, tc.Function.Arguments = "c1", "run", "{}"
	raw, err := NewAdapter("openai", provider.TierPremium, "http://x", "k").buildPayload(&provider.UnifiedChatRequest{
		IsAnthropicSource: true,
		Messages: []provider.UnifiedChatMessage{
			{Role: "assistant", Content: "let me run it", ToolCalls: []provider.UnifiedToolCall{tc}},
			{Role: "tool", Content: "result"},
		},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages []map[string]interface{} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Messages[0]["content"] != "let me run it" || got.Messages[0]["tool_calls"] == nil {
		t.Errorf("assistant message = %v", got.Messages[0])
	}
	if _, has := got.Messages[1]["tool_call_id"]; has || got.Messages[1]["content"] != "result" {
		t.Errorf("tool message without ID must omit tool_call_id: %v", got.Messages[1])
	}
}

func TestConvertToolsToOpenAI(t *testing.T) {
	openAITool := map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "already"}}
	got := convertToolsToOpenAI([]interface{}{
		"opaque",
		openAITool,
		map[string]interface{}{"name": "bare"},
		map[string]interface{}{"name": "full", "description": "d", "input_schema": map[string]interface{}{"type": "object", "required": []interface{}{"a"}}},
	})
	want := []interface{}{
		"opaque",
		openAITool,
		map[string]interface{}{"type": "function", "function": map[string]interface{}{
			"name": "bare", "parameters": map[string]interface{}{"type": "object"},
		}},
		map[string]interface{}{"type": "function", "function": map[string]interface{}{
			"name": "full", "description": "d", "parameters": map[string]interface{}{"type": "object", "required": []interface{}{"a"}},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
}

func TestConvertToolChoiceToOpenAI(t *testing.T) {
	tests := []struct {
		name string
		in   interface{}
		want interface{}
	}{
		{"string passthrough", "none", "none"},
		{"auto", map[string]interface{}{"type": "auto"}, "auto"},
		{"any", map[string]interface{}{"type": "any"}, "required"},
		{"tool", map[string]interface{}{"type": "tool", "name": "run"}, map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "run"}}},
		{"unknown map passthrough", map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "x"}}, map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "x"}}},
	}
	for _, tt := range tests {
		if got := convertToolChoiceToOpenAI(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %#v, want %#v", tt.name, got, tt.want)
		}
	}

	// The converted choice is what reaches the upstream payload.
	raw, err := NewAdapter("openai", provider.TierPremium, "http://x", "k").buildPayload(&provider.UnifiedChatRequest{
		IsAnthropicSource: true,
		ToolChoice:        map[string]interface{}{"type": "any"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"tool_choice":"required"`) {
		t.Errorf("payload = %s", raw)
	}
}
