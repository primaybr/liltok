package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/provider"
	"github.com/primaybr/liltok/internal/router"
	"github.com/primaybr/liltok/internal/server/middleware"
)

func TestInProxyTool_Authorization(t *testing.T) {
	tests := []struct {
		toolName string
		expected bool
	}{
		{"liltok_stats", true},
		{"LILTOK_STATS", true},
		{"liltok_cache_search", true},
		{"liltok_ask", true},
		{"liltok_memory_list", true},
		{"bash", false},
		{"read_file", false},
		{"write_to_file", false},
		{"liltok_memory_save", false}, // save is mutating, not read-only
		{"liltok_memory_delete", false},
	}

	for _, tt := range tests {
		got := IsInProxyTool(tt.toolName)
		if got != tt.expected {
			t.Errorf("IsInProxyTool(%q) = %v; want %v", tt.toolName, got, tt.expected)
		}
	}
}

func TestAreAllInProxyTools(t *testing.T) {
	// Empty list
	if AreAllInProxyTools(nil) {
		t.Error("AreAllInProxyTools(nil) should be false")
	}

	// All authorized
	allAuth := []provider.UnifiedToolCall{
		{Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "liltok_stats"}},
		{Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "liltok_cache_search"}},
	}
	if !AreAllInProxyTools(allAuth) {
		t.Error("AreAllInProxyTools(allAuth) should be true")
	}

	// Mixed with unauthorized
	mixed := append(allAuth, provider.UnifiedToolCall{
		Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "bash"},
	})
	if AreAllInProxyTools(mixed) {
		t.Error("AreAllInProxyTools(mixed) should be false")
	}
}

func TestInProxyTool_Execution(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_inproxy.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	defer database.Close()

	ctx := context.Background()
	if err := database.Migrate(); err != nil {
		t.Fatalf("failed to run migrations: %v", err)
	}

	// Seed cache entry
	_, err = database.ExecContext(ctx, `
		INSERT INTO cache_entries (hash, model, normalized_prompt, response_payload, hit_count, created_at, last_accessed_at)
		VALUES ('hash123', 'test-model', 'what is the meaning of life', '{"choices":[{"message":{"content":"42"}}]}', 5, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`)
	if err != nil {
		t.Fatalf("failed to seed cache entry: %v", err)
	}

	// Seed memory
	_, err = database.SaveMemory(ctx, "test_project", "architecture", "Use microservices pattern")
	if err != nil {
		t.Fatalf("failed to seed memory: %v", err)
	}

	p := &Proxy{
		database: database,
	}

	// 1. Test liltok_stats
	tcStats := provider.UnifiedToolCall{
		ID:   "call_stats_1",
		Type: "function",
		Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{
			Name:      "liltok_stats",
			Arguments: "{}",
		},
	}
	statsOut, err := p.executeInProxyTool(ctx, tcStats)
	if err != nil {
		t.Fatalf("executeInProxyTool liltok_stats failed: %v", err)
	}
	var statsMap map[string]interface{}
	if err := json.Unmarshal([]byte(statsOut), &statsMap); err != nil {
		t.Fatalf("failed to parse stats JSON: %v", err)
	}
	if statsMap["status"] != "healthy" {
		t.Errorf("expected status 'healthy', got %v", statsMap["status"])
	}

	// 2. Test liltok_cache_search
	tcSearch := provider.UnifiedToolCall{
		ID:   "call_search_1",
		Type: "function",
		Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{
			Name:      "liltok_cache_search",
			Arguments: `{"query": "meaning"}`,
		},
	}
	searchOut, err := p.executeInProxyTool(ctx, tcSearch)
	if err != nil {
		t.Fatalf("executeInProxyTool liltok_cache_search failed: %v", err)
	}
	var searchMap map[string]interface{}
	if err := json.Unmarshal([]byte(searchOut), &searchMap); err != nil {
		t.Fatalf("failed to parse search JSON: %v", err)
	}
	if count, ok := searchMap["count"].(float64); !ok || int(count) != 1 {
		t.Errorf("expected count 1, got %v", searchMap["count"])
	}

	// 3. Test liltok_ask
	tcAsk := provider.UnifiedToolCall{
		ID:   "call_ask_1",
		Type: "function",
		Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{
			Name:      "liltok_ask",
			Arguments: `{"prompt": "meaning"}`,
		},
	}
	askOut, err := p.executeInProxyTool(ctx, tcAsk)
	if err != nil {
		t.Fatalf("executeInProxyTool liltok_ask failed: %v", err)
	}
	var askMap map[string]interface{}
	if err := json.Unmarshal([]byte(askOut), &askMap); err != nil {
		t.Fatalf("failed to parse ask JSON: %v", err)
	}
	if askMap["found"] != true {
		t.Errorf("expected found true, got %v", askMap["found"])
	}
	if respText, ok := askMap["response"].(string); !ok || respText != "42" {
		t.Errorf("expected response '42', got %v", askMap["response"])
	}

	// 4. Test liltok_memory_list
	tcMem := provider.UnifiedToolCall{
		ID:   "call_mem_1",
		Type: "function",
		Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{
			Name:      "liltok_memory_list",
			Arguments: `{"project_key": "test_project", "category": "architecture"}`,
		},
	}
	memOut, err := p.executeInProxyTool(ctx, tcMem)
	if err != nil {
		t.Fatalf("executeInProxyTool liltok_memory_list failed: %v", err)
	}
	var memMap map[string]interface{}
	if err := json.Unmarshal([]byte(memOut), &memMap); err != nil {
		t.Fatalf("failed to parse memory JSON: %v", err)
	}
	if count, ok := memMap["count"].(float64); !ok || int(count) != 1 {
		t.Errorf("expected memory count 1, got %v", memMap["count"])
	}
}

func TestProxy_InProxyToolExecutionAndContinuation(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_inproxy_e2e.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatalf("failed to run migrations: %v", err)
	}

	chatCount := 0
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
			return
		}
		if r.URL.Path == "/v1/chat/completions" {
			chatCount++
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if chatCount == 1 {
			// First turn: upstream emits tool call for liltok_stats
			_, _ = w.Write([]byte(`{
				"id": "chatcmpl-t1",
				"choices": [{
					"message": {
						"role": "assistant",
						"content": "",
						"tool_calls": [{
							"id": "call_stat_123",
							"type": "function",
							"function": {
								"name": "liltok_stats",
								"arguments": "{}"
							}
						}]
					}
				}],
				"usage": {"prompt_tokens": 50, "completion_tokens": 15, "total_tokens": 65}
			}`))
			return
		}

		// Second turn (continuation): upstream receives tool response and answers
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-t2",
			"choices": [{
				"message": {
					"role": "assistant",
					"content": "Gateway telemetry: total requests 0, system healthy."
				}
			}],
			"usage": {"prompt_tokens": 120, "completion_tokens": 25, "total_tokens": 145}
		}`))
	}))
	defer mockUpstream.Close()

	cfg := config.DefaultConfig()
	cfg.Providers.OpenAI.BaseURL = mockUpstream.URL + "/v1"
	cfg.Providers.OpenAI.APIKey = "test-key"

	rtr := router.NewRouter(cfg)
	p := NewProxy(cfg, nil, nil, rtr, nil, nil)
	p.SetDatabase(database)

	handler := middleware.RequestID(http.HandlerFunc(p.HandleChatCompletions))

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"check gateway status"}],"stream":false}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if chatCount != 2 {
		t.Errorf("expected 2 upstream calls (turn 1 + continuation), got %d", chatCount)
	}

	if rec.Header().Get("X-Liltok-Inproxy-Executed") != "1" {
		t.Errorf("expected X-Liltok-Inproxy-Executed header 1, got %q", rec.Header().Get("X-Liltok-Inproxy-Executed"))
	}

	if !strings.Contains(rec.Body.String(), "Gateway telemetry: total requests 0") {
		t.Errorf("expected final answer in response body: %s", rec.Body.String())
	}
}

func TestProxy_InProxyToolExternalToolBypass(t *testing.T) {
	chatCount := 0
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
			return
		}
		if r.URL.Path == "/v1/chat/completions" {
			chatCount++
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		// Model emits external tool call 'bash'
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-t1",
			"choices": [{
				"message": {
					"role": "assistant",
					"content": "",
					"tool_calls": [{
						"id": "call_bash_1",
						"type": "function",
						"function": {
							"name": "bash",
							"arguments": "{\"command\":\"ls\"}"
						}
					}]
				}
			}],
			"usage": {"prompt_tokens": 50, "completion_tokens": 15, "total_tokens": 65}
		}`))
	}))
	defer mockUpstream.Close()

	cfg := config.DefaultConfig()
	cfg.Providers.OpenAI.BaseURL = mockUpstream.URL + "/v1"
	cfg.Providers.OpenAI.APIKey = "test-key"

	rtr := router.NewRouter(cfg)
	p := NewProxy(cfg, nil, nil, rtr, nil, nil)

	handler := middleware.RequestID(http.HandlerFunc(p.HandleChatCompletions))

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"run ls"}],"stream":false}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Should not have executed continuation, should pass bash tool call through
	if chatCount != 1 {
		t.Errorf("expected 1 upstream call, got %d", chatCount)
	}

	if rec.Header().Get("X-Liltok-Inproxy-Executed") != "" {
		t.Errorf("expected empty X-Liltok-Inproxy-Executed for external tool, got %q", rec.Header().Get("X-Liltok-Inproxy-Executed"))
	}

	if !strings.Contains(rec.Body.String(), "call_bash_1") {
		t.Errorf("expected bash tool call in response body: %s", rec.Body.String())
	}
}

func TestProxy_InProxyToolHeaderBypass(t *testing.T) {
	chatCount := 0
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
			return
		}
		if r.URL.Path == "/v1/chat/completions" {
			chatCount++
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		// Model emits liltok_stats tool call
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-t1",
			"choices": [{
				"message": {
					"role": "assistant",
					"content": "",
					"tool_calls": [{
						"id": "call_stat_1",
						"type": "function",
						"function": {
							"name": "liltok_stats",
							"arguments": "{}"
						}
					}]
				}
			}],
			"usage": {"prompt_tokens": 50, "completion_tokens": 15, "total_tokens": 65}
		}`))
	}))
	defer mockUpstream.Close()

	cfg := config.DefaultConfig()
	cfg.Providers.OpenAI.BaseURL = mockUpstream.URL + "/v1"
	cfg.Providers.OpenAI.APIKey = "test-key"

	rtr := router.NewRouter(cfg)
	p := NewProxy(cfg, nil, nil, rtr, nil, nil)

	handler := middleware.RequestID(http.HandlerFunc(p.HandleChatCompletions))

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"check stats"}],"stream":false}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Liltok-Inproxy-Tools", "false") // explicit bypass header
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Should NOT have run continuation
	if chatCount != 1 {
		t.Errorf("expected 1 upstream call on bypass, got %d", chatCount)
	}

	if rec.Header().Get("X-Liltok-Inproxy-Executed") != "" {
		t.Errorf("expected no X-Liltok-Inproxy-Executed header on bypass, got %q", rec.Header().Get("X-Liltok-Inproxy-Executed"))
	}
}
