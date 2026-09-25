package miner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/primaybr/liltok/internal/db"
)

func openInternalTestDB(t *testing.T) *db.DB {
	t.Helper()
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	database, err := db.Open(filepath.Join(t.TempDir(), "miner.db"))
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func TestNewCacheMiner_ProviderDefaults(t *testing.T) {
	tests := []struct {
		name         string
		cfg          MinerConfig
		wantProvider string
		wantBaseURL  string
		wantModel    string
	}{
		{"empty provider falls back to groq", MinerConfig{}, "groq", "https://api.groq.com/openai/v1", "qwen/qwen3.8-27b"},
		{"unknown provider falls back to groq", MinerConfig{Provider: "mystery"}, "groq", "https://api.groq.com/openai/v1", "qwen/qwen3.8-27b"},
		{"groq keeps explicit values", MinerConfig{Provider: "groq", BaseURL: "http://local", Model: "m1"}, "groq", "http://local", "m1"},
		{"openrouter defaults", MinerConfig{Provider: "openrouter"}, "openrouter", "https://openrouter.ai/api/v1", "openrouter/free"},
		{"openrouter resolves alias", MinerConfig{Provider: "OpenRouter", Model: "auto"}, "OpenRouter", "https://openrouter.ai/api/v1", "openrouter/free"},
		{"openrouter keeps concrete model", MinerConfig{Provider: "openrouter", Model: "vendor/model-x"}, "openrouter", "https://openrouter.ai/api/v1", "vendor/model-x"},
		{"nvidianim defaults", MinerConfig{Provider: "nvidianim"}, "nvidianim", "https://integrate.api.nvidia.com/v1", "deepseek-ai/deepseek-v4-flash-0731"},
		{"kilo defaults", MinerConfig{Provider: "kilo"}, "kilo", "https://api.kilo.ai/api/gateway", "kilo-auto/free"},
		{"kilo resolves alias", MinerConfig{Provider: "kilo", Model: "free"}, "kilo", "https://api.kilo.ai/api/gateway", "kilo-auto/free"},
		{"kilo keeps concrete model", MinerConfig{Provider: "kilo", Model: "some/model"}, "kilo", "https://api.kilo.ai/api/gateway", "some/model"},
		{"cline defaults", MinerConfig{Provider: "cline"}, "cline", "https://api.cline.bot/api/v1", "nvidia/nemotron-3.5-lightning:free"},
		{"ollama defaults", MinerConfig{Provider: "ollama"}, "ollama", "http://localhost:11434/v1", "qwen2.5-coder:7b"},
		{"ollama keeps base url", MinerConfig{Provider: "ollama", BaseURL: "http://gpu-box:11434/v1"}, "ollama", "http://gpu-box:11434/v1", "qwen2.5-coder:7b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewCacheMiner(tt.cfg, nil, nil)
			if m.cfg.Provider != tt.wantProvider {
				t.Errorf("provider = %q, want %q", m.cfg.Provider, tt.wantProvider)
			}
			if m.cfg.BaseURL != tt.wantBaseURL {
				t.Errorf("base url = %q, want %q", m.cfg.BaseURL, tt.wantBaseURL)
			}
			if m.cfg.Model != tt.wantModel {
				t.Errorf("model = %q, want %q", m.cfg.Model, tt.wantModel)
			}
		})
	}
}

func TestNewCacheMiner_ConcurrencyDefaults(t *testing.T) {
	m := NewCacheMiner(MinerConfig{Workers: -1, RateLimitRPM: 0}, nil, nil)
	if m.cfg.Workers != 2 {
		t.Errorf("workers = %d, want 2", m.cfg.Workers)
	}
	if m.cfg.RateLimitRPM != 30 {
		t.Errorf("rate limit = %d, want 30", m.cfg.RateLimitRPM)
	}
	if len(m.cfg.TargetModels) != len(DefaultTargetModels()) {
		t.Errorf("target models = %v, want defaults", m.cfg.TargetModels)
	}
	if m.httpClient == nil || m.httpClient.Timeout != 60*time.Second {
		t.Errorf("expected http client with 60s timeout, got %+v", m.httpClient)
	}

	custom := NewCacheMiner(MinerConfig{Workers: 5, RateLimitRPM: 90, TargetModels: []string{"only"}}, nil, nil)
	if custom.cfg.Workers != 5 || custom.cfg.RateLimitRPM != 90 {
		t.Errorf("explicit workers/rpm overwritten: %+v", custom.cfg)
	}
	if len(custom.cfg.TargetModels) != 1 || custom.cfg.TargetModels[0] != "only" {
		t.Errorf("explicit target models overwritten: %v", custom.cfg.TargetModels)
	}
}

func TestMineSinglePrompt_Errors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"provider error status", http.StatusInternalServerError, `{"error":"boom"}`, "returned status 500"},
		{"unparseable body", http.StatusOK, `not json`, "failed to parse valid response"},
		{"no choices", http.StatusOK, `{"choices":[]}`, "no choices returned"},
		{"empty data envelope", http.StatusOK, `{"data":{"choices":[]}}`, "no choices returned"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()

			m := NewCacheMiner(MinerConfig{Provider: "groq", BaseURL: srv.URL, TargetModels: []string{"gpt-4o"}}, nil, nil)
			var stats MiningStats
			entries, tokens, err := m.mineSinglePrompt(context.Background(), PromptItem{ID: "p", UserPrompt: "q"}, &stats)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
			if entries != 0 || tokens != 0 || stats.CacheEntries != 0 || stats.TokensGenerated != 0 {
				t.Errorf("expected no entries or tokens on error, got entries=%d tokens=%d stats=%+v", entries, tokens, stats)
			}
		})
	}
}

func TestMineSinglePrompt_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	m := NewCacheMiner(MinerConfig{Provider: "ollama", BaseURL: url}, nil, nil)
	var stats MiningStats
	_, _, err := m.mineSinglePrompt(context.Background(), PromptItem{ID: "p", UserPrompt: "q"}, &stats)
	if err == nil || !strings.Contains(err.Error(), "http call to ollama failed") {
		t.Fatalf("err = %v, want http call failure", err)
	}
}

func TestMineSinglePrompt_RateLimitedHonoursContext(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	m := NewCacheMiner(MinerConfig{BaseURL: srv.URL}, nil, nil)
	var stats MiningStats
	start := time.Now()
	_, _, err := m.mineSinglePrompt(ctx, PromptItem{ID: "p", UserPrompt: "q"}, &stats)
	if err != context.DeadlineExceeded {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("backoff ignored cancellation, took %v", elapsed)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("expected 1 upstream call before cancellation, got %d", calls)
	}
}

func TestMineSinglePrompt_RetriesAfterRateLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the 3s retry backoff")
	}
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	}))
	defer srv.Close()

	m := NewCacheMiner(MinerConfig{BaseURL: srv.URL, TargetModels: []string{"gpt-4o"}}, nil, nil)
	var stats MiningStats
	entries, tokens, err := m.mineSinglePrompt(context.Background(), PromptItem{ID: "p", UserPrompt: "q"}, &stats)
	if err != nil {
		t.Fatalf("unexpected error after retry: %v", err)
	}
	if tokens != 3 || entries != 2 {
		t.Errorf("entries=%d tokens=%d, want 2 and 3", entries, tokens)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Errorf("expected 2 upstream calls, got %d", calls)
	}
}

func TestMineSinglePrompt_DataEnvelopeStoresHashesMatchingAudit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"choices":[{"message":{"role":"assistant","content":"wrapped answer"}}],"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10}}}`)
	}))
	defer srv.Close()

	database := openInternalTestDB(t)
	models := []string{"gpt-4o", "claude-sonnet-5"}
	m := NewCacheMiner(MinerConfig{BaseURL: srv.URL, TargetModels: models}, database, nil)
	item := PromptItem{ID: "wrapped", SystemPrompt: "sys", UserPrompt: "how do I wrap?"}

	var stats MiningStats
	entries, tokens, err := m.mineSinglePrompt(context.Background(), item, &stats)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entries != 4 || tokens != 10 || stats.CacheEntries != 4 || stats.TokensGenerated != 10 {
		t.Fatalf("entries=%d tokens=%d stats=%+v, want 4 entries and 10 tokens", entries, tokens, stats)
	}

	// Every hash the audit computes must be present, stored under the right model.
	hashes := computePromptHashes(item, models)
	if len(hashes) != 4 {
		t.Fatalf("expected 4 distinct hashes, got %d", len(hashes))
	}
	for h, model := range hashes {
		var gotModel string
		var payload []byte
		var pinned, pTok, cTok int
		err := database.QueryRow("SELECT model, response_payload, is_pinned, prompt_tokens, completion_tokens FROM cache_entries WHERE hash = ?", h).
			Scan(&gotModel, &payload, &pinned, &pTok, &cTok)
		if err != nil {
			t.Fatalf("hash %s for model %s not stored: %v", h, model, err)
		}
		if gotModel != model {
			t.Errorf("hash %s stored under %q, want %q", h, gotModel, model)
		}
		if pinned != 1 || pTok != 4 || cTok != 6 {
			t.Errorf("unexpected row metadata pinned=%d prompt=%d completion=%d", pinned, pTok, cTok)
		}
		if !strings.Contains(string(payload), "wrapped answer") {
			t.Errorf("payload missing generated text: %s", payload)
		}
	}
}

func TestMineSinglePrompt_RequestShape(t *testing.T) {
	tests := []struct {
		name          string
		provider      string
		apiKey        string
		system        string
		wantMaxTokens float64
		wantMessages  int
	}{
		{"groq caps max tokens and sends key", "groq", "test-key", "", 512, 1},
		{"other providers allow 2048 and include system", "ollama", "", "be brief", 2048, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var gotPath, gotAuth string
			var body map[string]interface{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				_ = json.NewDecoder(r.Body).Decode(&body)
				mu.Unlock()
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"a"}}],"usage":{"total_tokens":1}}`)
			}))
			defer srv.Close()

			m := NewCacheMiner(MinerConfig{Provider: tt.provider, APIKey: tt.apiKey, BaseURL: srv.URL + "/", Model: "gen-model", TargetModels: []string{"x"}}, nil, nil)
			var stats MiningStats
			if _, _, err := m.mineSinglePrompt(context.Background(), PromptItem{ID: "p", SystemPrompt: tt.system, UserPrompt: "question"}, &stats); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()

			if gotPath != "/chat/completions" {
				t.Errorf("path = %q, want /chat/completions (trailing slash trimmed)", gotPath)
			}
			wantAuth := ""
			if tt.apiKey != "" {
				wantAuth = "Bearer " + tt.apiKey
			}
			if gotAuth != wantAuth {
				t.Errorf("authorization = %q, want %q", gotAuth, wantAuth)
			}
			if body["model"] != "gen-model" {
				t.Errorf("model = %v, want gen-model", body["model"])
			}
			if body["max_tokens"] != tt.wantMaxTokens {
				t.Errorf("max_tokens = %v, want %v", body["max_tokens"], tt.wantMaxTokens)
			}
			msgs, _ := body["messages"].([]interface{})
			if len(msgs) != tt.wantMessages {
				t.Fatalf("messages = %v, want %d entries", msgs, tt.wantMessages)
			}
			last, _ := msgs[len(msgs)-1].(map[string]interface{})
			if last["role"] != "user" || last["content"] != "question" {
				t.Errorf("last message = %v, want user question", last)
			}
			if tt.wantMessages == 2 {
				first, _ := msgs[0].(map[string]interface{})
				if first["role"] != "system" || first["content"] != tt.system {
					t.Errorf("first message = %v, want system prompt", first)
				}
			}
		})
	}
}

func TestComputePromptHashes_DistinctPerModelAndSystem(t *testing.T) {
	item := PromptItem{UserPrompt: "same question"}
	withSystem := PromptItem{UserPrompt: "same question", SystemPrompt: "sys"}

	a := computePromptHashes(item, []string{"m1", "m2"})
	if len(a) != 4 {
		t.Fatalf("expected 4 hashes (2 models x 2 schemas), got %d", len(a))
	}
	counts := map[string]int{}
	for _, m := range a {
		counts[m]++
	}
	if counts["m1"] != 2 || counts["m2"] != 2 {
		t.Errorf("expected 2 hashes per model, got %v", counts)
	}

	b := computePromptHashes(withSystem, []string{"m1", "m2"})
	for h := range b {
		if _, dup := a[h]; dup {
			t.Errorf("system prompt did not change hash %s", h)
		}
	}

	if got := computePromptHashes(item, nil); len(got) != 0 {
		t.Errorf("expected no hashes without models, got %d", len(got))
	}
}

func TestMiningManager_LogRingKeepsLatest100(t *testing.T) {
	mm := NewMiningManager(nil, nil)
	for i := 0; i < 150; i++ {
		mm.addLog(fmt.Sprintf("entry-%d", i))
	}
	logs := mm.Status().RecentLogs
	if len(logs) != 100 {
		t.Fatalf("expected 100 retained logs, got %d", len(logs))
	}
	if !strings.HasSuffix(logs[0], "entry-50") {
		t.Errorf("oldest retained log = %q, want entry-50", logs[0])
	}
	if !strings.HasSuffix(logs[99], "entry-149") {
		t.Errorf("newest log = %q, want entry-149", logs[99])
	}
}
