package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/primaybr/liltok/internal/cache/exact"
	"github.com/primaybr/liltok/internal/cache/semantic"
	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/ledger"
	"github.com/primaybr/liltok/internal/provider"
	"github.com/primaybr/liltok/internal/router"
	"github.com/primaybr/liltok/internal/server/middleware"
	"github.com/primaybr/liltok/internal/tokens"
)

func TestProxyOpenAIChatCompletions(t *testing.T) {
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-openai-key" {
			t.Errorf("unexpected upstream auth header: %s", r.Header.Get("Authorization"))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-123","choices":[{"message":{"role":"assistant","content":"pong"}}]}`))
	}))
	defer mockUpstream.Close()

	cfg := config.DefaultConfig()
	cfg.Providers.OpenAI.BaseURL = mockUpstream.URL + "/v1"
	cfg.Providers.OpenAI.APIKey = "test-openai-key"

	p := NewProxy(cfg, nil, nil, nil, nil, nil)

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"ping"}],"stream":false}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler := middleware.RequestID(http.HandlerFunc(p.HandleChatCompletions))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if rec.Header().Get("X-Liltok-Cache-Status") != "MISS" {
		t.Errorf("expected X-Liltok-Cache-Status MISS, got %s", rec.Header().Get("X-Liltok-Cache-Status"))
	}

	if !strings.Contains(rec.Body.String(), "chatcmpl-123") {
		t.Errorf("response body does not match expected upstream: %s", rec.Body.String())
	}
}

func TestProxyAnthropicMessages(t *testing.T) {
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-anthropic-key" {
			t.Errorf("unexpected upstream auth header: %s", r.Header.Get("x-api-key"))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg-456","content":[{"type":"text","text":"hello from claude"}]}`))
	}))
	defer mockUpstream.Close()

	cfg := config.DefaultConfig()
	cfg.Providers.Anthropic.BaseURL = mockUpstream.URL
	cfg.Providers.Anthropic.APIKey = "test-anthropic-key"

	p := NewProxy(cfg, nil, nil, nil, nil, nil)

	reqBody := `{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hello"}],"max_tokens":1024}`
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader([]byte(reqBody)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler := middleware.RequestID(http.HandlerFunc(p.HandleAnthropicMessages))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if rec.Header().Get("X-Liltok-Provider") != "anthropic" {
		t.Errorf("expected X-Liltok-Provider anthropic, got %s", rec.Header().Get("X-Liltok-Provider"))
	}

	if !strings.Contains(rec.Body.String(), "msg-456") {
		t.Errorf("response body does not match expected: %s", rec.Body.String())
	}
}

func TestProxyTier1ExactCacheHit(t *testing.T) {
	var upstreamCalls int64
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&upstreamCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-cached-test","choices":[{"message":{"role":"assistant","content":"exact hit response"}}]}`))
	}))
	defer mockUpstream.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open memory db: %v", err)
	}
	defer database.Close()

	store, err := exact.NewTieredStore(database, 100)
	if err != nil {
		t.Fatalf("failed to open tiered store: %v", err)
	}
	defer store.Close()

	cfg := config.DefaultConfig()
	cfg.Providers.OpenAI.BaseURL = mockUpstream.URL + "/v1"
	cfg.Providers.OpenAI.APIKey = "key"

	p := NewProxy(cfg, store, nil, nil, nil, nil)
	handler := middleware.RequestID(http.HandlerFunc(p.HandleChatCompletions))

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"repeatable query"}],"temperature":0.0}`

	// 1. First Call: Should be a cache MISS and hit upstream
	req1 := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()

	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first call failed: %d", rec1.Code)
	}
	if rec1.Header().Get("X-Liltok-Cache-Status") != "MISS" {
		t.Errorf("expected first call to be MISS, got %s", rec1.Header().Get("X-Liltok-Cache-Status"))
	}
	if atomic.LoadInt64(&upstreamCalls) != 1 {
		t.Errorf("expected 1 upstream call, got %d", atomic.LoadInt64(&upstreamCalls))
	}

	time.Sleep(100 * time.Millisecond)

	// 2. Second Call: Identical query should be a cache HIT and NOT hit upstream
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()

	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second call failed: %d", rec2.Code)
	}
	if rec2.Header().Get("X-Liltok-Cache-Status") != "HIT" {
		t.Errorf("expected second call to be HIT, got %s", rec2.Header().Get("X-Liltok-Cache-Status"))
	}
	if rec2.Header().Get("X-Liltok-Cache-Tier") != "TIER1_EXACT" {
		t.Errorf("expected TIER1_EXACT, got %s", rec2.Header().Get("X-Liltok-Cache-Tier"))
	}
	if !strings.Contains(rec2.Body.String(), "exact hit response") {
		t.Errorf("expected cached response in body: %s", rec2.Body.String())
	}
	if atomic.LoadInt64(&upstreamCalls) != 1 {
		t.Errorf("expected still 1 upstream call on cache hit, got %d", atomic.LoadInt64(&upstreamCalls))
	}
}

func TestProxyRouterCrossProtocolFallback(t *testing.T) {
	mockNIM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-nim-123",
			"choices": [{"message": {"role": "assistant", "content": "response from free llama"}}],
			"usage": {"prompt_tokens": 50, "completion_tokens": 20, "total_tokens": 70}
		}`))
	}))
	defer mockNIM.Close()

	cfg := config.DefaultConfig()
	cfg.Providers.NVIDIANIM.BaseURL = mockNIM.URL
	cfg.Providers.NVIDIANIM.APIKey = "nv-key"

	rtr := router.NewRouter(cfg)
	p := NewProxy(cfg, nil, nil, rtr, nil, nil)

	handler := middleware.RequestID(http.HandlerFunc(p.HandleAnthropicMessages))

	reqBody := `{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"write a binary search"}],"max_tokens":1024}`
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader([]byte(reqBody)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Liltok-Route", "free-first")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if rec.Header().Get("X-Liltok-Provider") != "nvidianim" {
		t.Errorf("expected winning provider nvidianim, got %s", rec.Header().Get("X-Liltok-Provider"))
	}

	var anthResp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &anthResp); err != nil {
		t.Fatalf("failed to parse returned Anthropic JSON: %v", err)
	}

	if anthResp["type"] != "message" {
		t.Errorf("expected type message in response, got %v", anthResp["type"])
	}

	if !strings.Contains(rec.Body.String(), "response from free llama") {
		t.Errorf("expected translated response content in body: %s", rec.Body.String())
	}
}

func TestProxyTier0Pruning(t *testing.T) {
	var forwardedBody []byte
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwardedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"diff reviewed"}}]}`))
	}))
	defer mockUpstream.Close()

	cfg := config.DefaultConfig()
	cfg.Providers.OpenAI.BaseURL = mockUpstream.URL + "/v1"
	cfg.Providers.OpenAI.APIKey = "test-key"

	p := NewProxy(cfg, nil, nil, nil, nil, nil)
	handler := middleware.RequestID(http.HandlerFunc(p.HandleChatCompletions))

	rawDiff := "diff --git a/foo.go b/foo.go\nindex 123456..789012 100644\n--- a/foo.go\n+++ b/foo.go\n@@ -1,10 +1,10 @@\n 1\n 2\n 3\n 4\n-old\n+new\n 5\n 6\n 7\n 8\n"
	reqPayload := map[string]interface{}{
		"model": "gpt-4o",
		"messages": []map[string]string{
			{"role": "user", "content": "Review this:\n" + rawDiff},
		},
	}
	body, _ := json.Marshal(reqPayload)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	if strings.Contains(string(forwardedBody), "index 123456..789012") {
		t.Errorf("expected diff to be pruned before upstream forwarding, got: %s", string(forwardedBody))
	}
	if !strings.Contains(string(forwardedBody), "+new") {
		t.Errorf("expected code additions preserved, got: %s", string(forwardedBody))
	}
}

func TestProxyTier3SemanticCacheHit(t *testing.T) {
	var upstreamCalls int64
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&upstreamCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-semantic-1","choices":[{"message":{"role":"assistant","content":"semantic response"}}]}`))
	}))
	defer mockUpstream.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open memory db: %v", err)
	}
	defer database.Close()

	store, err := exact.NewTieredStore(database, 100)
	if err != nil {
		t.Fatalf("failed to open tiered store: %v", err)
	}
	defer store.Close()

	semCache := semantic.NewSemanticCache(database, semantic.NewFastLocalEmbedder(256), 0.80)

	cfg := config.DefaultConfig()
	cfg.Providers.OpenAI.BaseURL = mockUpstream.URL + "/v1"
	cfg.Providers.OpenAI.APIKey = "key"

	p := NewProxy(cfg, store, semCache, nil, nil, nil)
	handler := middleware.RequestID(http.HandlerFunc(p.HandleChatCompletions))

	// Call 1: "Explain quicksort in Go"
	req1Body := `{"model":"gpt-4o","messages":[{"role":"user","content":"Explain quicksort in Go"}],"temperature":0.0}`
	req1 := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(req1Body)))
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()

	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("call 1 failed: %d", rec1.Code)
	}

	time.Sleep(100 * time.Millisecond)

	// Call 2: "Please explain quicksort in Go" (different wording -> Tier-1 exact miss, but Tier-3 semantic HIT!)
	req2Body := `{"model":"gpt-4o","messages":[{"role":"user","content":"Please explain quicksort in Go"}],"temperature":0.0}`
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(req2Body)))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()

	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("call 2 failed: %d", rec2.Code)
	}

	if rec2.Header().Get("X-Liltok-Cache-Status") != "HIT" {
		t.Errorf("expected call 2 to be HIT, got %s", rec2.Header().Get("X-Liltok-Cache-Status"))
	}
	if rec2.Header().Get("X-Liltok-Cache-Tier") != "TIER3_SEMANTIC" {
		t.Errorf("expected cache tier TIER3_SEMANTIC, got %s", rec2.Header().Get("X-Liltok-Cache-Tier"))
	}
	if !strings.Contains(rec2.Body.String(), "semantic response") {
		t.Errorf("expected cached response body: %s", rec2.Body.String())
	}
	if atomic.LoadInt64(&upstreamCalls) != 1 {
		t.Errorf("expected upstream call count to stay 1 on semantic hit, got %d", atomic.LoadInt64(&upstreamCalls))
	}
}

func TestProxyTier2AnthropicPrefixCaching(t *testing.T) {
	var forwardedBody []byte
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwardedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "msg_01",
			"type": "message",
			"role": "assistant",
			"content": [{"type":"text","text":"cached response"}],
			"usage": {"input_tokens": 100, "output_tokens": 10, "cache_read_input_tokens": 1000}
		}`))
	}))
	defer mockUpstream.Close()

	cfg := config.DefaultConfig()
	cfg.Providers.Anthropic.BaseURL = mockUpstream.URL
	cfg.Providers.Anthropic.APIKey = "key"

	p := NewProxy(cfg, nil, nil, nil, nil, nil)
	handler := middleware.RequestID(http.HandlerFunc(p.HandleAnthropicMessages))

	longSys := strings.Repeat("System instructions for code generator. ", 150)
	reqPayload := map[string]interface{}{
		"model":      "claude-3-5-sonnet-20241022",
		"system":     longSys,
		"messages":   []map[string]string{{"role": "user", "content": "hello"}},
		"max_tokens": 1024,
	}
	body, _ := json.Marshal(reqPayload)

	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if !strings.Contains(string(forwardedBody), `"cache_control":{"type":"ephemeral"}`) {
		t.Errorf("expected ephemeral cache_control injected, got: %s", string(forwardedBody))
	}
}

func TestProxyLedgerAndBudgetEnforcement(t *testing.T) {
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-budget-test","choices":[{"message":{"role":"assistant","content":"upstream reply"}}]}`))
	}))
	defer mockUpstream.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open memory db: %v", err)
	}
	defer database.Close()

	store, _ := exact.NewTieredStore(database, 100)
	defer store.Close()

	km := ledger.NewKeyManager(database)
	qe := ledger.NewQuotaEnforcer()
	led := ledger.NewLedger(database, km)
	defer led.Close()
	pricing := tokens.NewPricingRegistry(database)

	cfg := config.DefaultConfig()
	cfg.Providers.OpenAI.BaseURL = mockUpstream.URL + "/v1"
	cfg.Providers.OpenAI.APIKey = "key"

	p := NewProxy(cfg, store, nil, nil, led, pricing)

	// Create a virtual key with a tiny budget of $0.001
	rawKey, keyObj, err := km.CreateKey(context.Background(), "budget-tester", 0.001, 60, 10000)
	if err != nil {
		t.Fatalf("failed to create key: %v", err)
	}

	handler := middleware.RequestID(middleware.NewAuth(km, qe)(http.HandlerFunc(p.HandleChatCompletions)))

	// 1. First Request: Cache miss, within budget ($0 spent so far). Upstream call allowed!
	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"first query"}],"temperature":0.0}`
	req1 := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", "Bearer "+rawKey)
	rec1 := httptest.NewRecorder()

	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("expected request 1 to succeed, got %d: %s", rec1.Code, rec1.Body.String())
	}

	// Update spend to exhaust budget ($0.05 spend > $0.001 budget)
	_ = km.UpdateSpend(context.Background(), keyObj.ID, 0.05)
	time.Sleep(100 * time.Millisecond)

	// 2. Second Request: Repeat identical query. Because it is a 100% free Cache Hit, it MUST be allowed!
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+rawKey)
	rec2 := httptest.NewRecorder()

	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected cache hit to succeed even with budget exhausted, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if rec2.Header().Get("X-Liltok-Cache-Status") != "HIT" {
		t.Errorf("expected cache status HIT, got %s", rec2.Header().Get("X-Liltok-Cache-Status"))
	}

	// 3. Third Request: New query (Cache Miss) with exhausted budget. Upstream MUST be blocked with HTTP 429!
	newReqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"brand new query"}],"temperature":0.0}`
	req3 := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(newReqBody)))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("Authorization", "Bearer "+rawKey)
	rec3 := httptest.NewRecorder()

	handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusTooManyRequests {
		t.Fatalf("expected HTTP 429 for budget exceeded on cache miss, got %d: %s", rec3.Code, rec3.Body.String())
	}
	if !strings.Contains(rec3.Body.String(), "insufficient_quota") {
		t.Errorf("expected insufficient_quota in error response: %s", rec3.Body.String())
	}
}

func TestProxySingleFlightCoalescing(t *testing.T) {
	var upstreamCalls int32

	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		time.Sleep(50 * time.Millisecond) // simulate upstream latency

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-sf","choices":[{"message":{"role":"assistant","content":"coalesced answer"}}]}`))
	}))
	defer mockUpstream.Close()

	cfg := config.DefaultConfig()
	cfg.Providers.OpenAI.BaseURL = mockUpstream.URL + "/v1"
	cfg.Providers.OpenAI.APIKey = "test-openai-key"

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	defer database.Close()

	store, err := exact.NewTieredStore(database, 100)
	if err != nil {
		t.Fatalf("failed to create tiered store: %v", err)
	}
	defer store.Close()

	led := ledger.NewLedger(database, nil)
	defer led.Close()

	pricing := tokens.NewPricingRegistry(database)
	p := NewProxy(cfg, store, nil, nil, led, pricing)

	handler := middleware.RequestID(http.HandlerFunc(p.HandleChatCompletions))

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"coalescing test query"}],"temperature":0.0}`

	var wg sync.WaitGroup
	rec1 := httptest.NewRecorder()
	rec2 := httptest.NewRecorder()

	wg.Add(2)
	go func() {
		defer wg.Done()
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
		req.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(rec1, req)
	}()

	go func() {
		defer wg.Done()
		time.Sleep(10 * time.Millisecond) // fire slightly after req1 while req1 is in-flight
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
		req.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(rec2, req)
	}()

	wg.Wait()

	if rec1.Code != http.StatusOK {
		t.Fatalf("rec1 failed: %d - %s", rec1.Code, rec1.Body.String())
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("rec2 failed: %d - %s", rec2.Code, rec2.Body.String())
	}

	// Upstream MUST have been called exactly ONCE despite 2 concurrent requests
	calls := atomic.LoadInt32(&upstreamCalls)
	if calls != 1 {
		t.Errorf("expected exactly 1 upstream call, got %d", calls)
	}

	// One should be MISS, the second should be HIT
	status1 := rec1.Header().Get("X-Liltok-Cache-Status")
	status2 := rec2.Header().Get("X-Liltok-Cache-Status")

	if (status1 == "MISS" && status2 == "HIT") || (status1 == "HIT" && status2 == "MISS") {
		// As expected
	} else {
		t.Errorf("expected one MISS and one HIT, got status1=%s, status2=%s", status1, status2)
	}
}

func TestProxyHandleModelsReturnsActiveModels(t *testing.T) {
	cfg := config.DefaultConfig()
	r := router.NewRouter(cfg)
	p := NewProxy(cfg, nil, nil, r, nil, nil)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()

	p.HandleModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}

	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode models response: %v", err)
	}

	if resp.Object != "list" {
		t.Errorf("expected object 'list', got %s", resp.Object)
	}

	foundGroqActive := false
	for _, m := range resp.Data {
		if m.OwnedBy == "groq" && m.ID == "openai/gpt-oss-120b" {
			foundGroqActive = true
			break
		}
	}

	if !foundGroqActive {
		t.Errorf("expected active groq model openai/gpt-oss-120b in /v1/models response, got: %+v", resp.Data)
	}
}

type mockFailingProvider struct {
	name string
}

func (m *mockFailingProvider) Name() string                { return m.name }
func (m *mockFailingProvider) Tier() provider.ProviderTier { return provider.TierFree }
func (m *mockFailingProvider) SendChat(ctx context.Context, req *provider.UnifiedChatRequest) (*provider.UnifiedChatResponse, error) {
	return nil, errors.New("simulated upstream failure")
}
func (m *mockFailingProvider) StreamChat(ctx context.Context, req *provider.UnifiedChatRequest) (<-chan provider.UnifiedSSEEvent, <-chan error, error) {
	return nil, nil, errors.New("simulated upstream failure")
}
func (m *mockFailingProvider) CheckHealth(ctx context.Context) (bool, error) {
	return false, errors.New("unhealthy")
}

func TestProxyFreeFirstBypassesPaidUpstreamFallback(t *testing.T) {
	upstreamCalled := false
	mockAnthropicUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			upstreamCalled = true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg-paid","type":"message","role":"assistant","content":[{"type":"text","text":"paid response"}]}`))
	}))
	defer mockAnthropicUpstream.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open memory db: %v", err)
	}
	defer database.Close()

	store, _ := exact.NewTieredStore(database, 100)
	defer store.Close()

	cfg := config.DefaultConfig()
	cfg.Providers.Anthropic.BaseURL = mockAnthropicUpstream.URL
	cfg.Providers.Anthropic.APIKey = "sk-ant-test"

	r := router.NewRouter(cfg)
	// Explicitly register failing mock on all free and paid providers so router dispatch exhausts
	for _, name := range []string{"groq", "gemini", "nvidianim", "openrouter", "kilo", "cline"} {
		r.SetProvider(name, &mockFailingProvider{name: name})
	}

	p := NewProxy(cfg, store, nil, nil, nil, nil)
	p.SetRouter(r)

	handler := middleware.RequestID(http.HandlerFunc(p.HandleAnthropicMessages))

	reqBody := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Liltok-Route", "free-first") // Explicit free-first routing requested
	req.Header.Set("x-api-key", "sk-ant-test")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Under free-first strategy, router exhaustion must return HTTP 502, NOT call paid upstream
	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected status 502 Bad Gateway under free-first exhaustion, got %d: %s", rec.Code, rec.Body.String())
	}
	if upstreamCalled {
		t.Errorf("paid direct upstream was contacted despite X-Liltok-Route: free-first")
	}
}
