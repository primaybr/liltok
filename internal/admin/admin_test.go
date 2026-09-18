package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/primaybr/liltok/internal/admin"
	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/ledger"
	"github.com/primaybr/liltok/internal/router"
)

func setupAdminTest(t *testing.T) (*chi.Mux, *db.DB, *admin.AdminHandler) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open memory db: %v", err)
	}

	cfg := config.DefaultConfig()
	km := ledger.NewKeyManager(database)
	led := ledger.NewLedger(database, km)
	rtr := router.NewRouter(cfg)
	broadcaster := admin.NewBroadcaster()

	handler := admin.NewAdminHandler(cfg, database, led, km, rtr, nil, broadcaster)
	tempConfigFile := filepath.Join(t.TempDir(), "liltok.yaml")
	_ = os.WriteFile(tempConfigFile, []byte("providers:\n  openai:\n    api_key: \"\"\n  anthropic:\n    api_key: \"\"\n  nvidianim:\n    api_key: \"\"\n  groq:\n    api_key: \"\"\n  gemini:\n    api_key: \"\"\n  openrouter:\n    api_key: \"\"\n  ollama:\n    base_url: \"http://localhost:11434\"\nroutes:\n  default_strategy: \"auto-resilient\"\n"), 0644)
	handler.SetConfigPath(tempConfigFile)

	r := chi.NewRouter()
	handler.RegisterRoutes(r)
	r.Get("/dashboard", admin.ServeDashboard)

	return r, database, handler
}

func TestAdminOverview(t *testing.T) {
	r, db, _ := setupAdminTest(t)
	defer db.Close()

	req := httptest.NewRequest("GET", "/api/v1/overview", nil)
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var data map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	if data["status"] != "healthy" {
		t.Errorf("expected status healthy, got %v", data["status"])
	}
	if _, hasUptime := data["uptime_seconds"]; !hasUptime {
		t.Errorf("expected uptime_seconds in overview")
	}
}

func TestAdminLogs(t *testing.T) {
	r, db, _ := setupAdminTest(t)
	defer db.Close()

	// Seed a request log
	_, _ = db.Exec(`
		INSERT INTO request_logs (request_id, model, provider, cache_status, cache_tier, prompt_tokens, completion_tokens, latency_ms, cost_usd, saved_usd)
		VALUES ('req_test_1', 'gpt-4o', 'openai', 'MISS', 'NONE', 100, 20, 250, 0.001, 0.0)
	`)

	req := httptest.NewRequest("GET", "/api/v1/logs?limit=10", nil)
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var data struct {
		Logs []map[string]interface{} `json:"logs"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &data)
	if len(data.Logs) != 1 {
		t.Errorf("expected 1 log, got %d", len(data.Logs))
	}
	if data.Logs[0]["request_id"] != "req_test_1" {
		t.Errorf("expected req_test_1, got %v", data.Logs[0]["request_id"])
	}
}

func TestAdminCacheOperations(t *testing.T) {
	r, db, _ := setupAdminTest(t)
	defer db.Close()

	// Seed cache entry
	_, _ = db.Exec(`
		INSERT INTO cache_entries (hash, model, normalized_prompt, response_payload)
		VALUES ('hash_abc123', 'gpt-4o', 'prompt test', 'payload')
	`)

	// 1. List cache
	reqList := httptest.NewRequest("GET", "/api/v1/cache", nil)
	recList := httptest.NewRecorder()
	r.ServeHTTP(recList, reqList)

	if recList.Code != http.StatusOK {
		t.Fatalf("expected 200 listing cache, got %d", recList.Code)
	}

	// 2. Delete cache entry
	reqDel := httptest.NewRequest("DELETE", "/api/v1/cache/hash_abc123", nil)
	recDel := httptest.NewRecorder()
	r.ServeHTTP(recDel, reqDel)

	if recDel.Code != http.StatusOK {
		t.Fatalf("expected 200 deleting cache entry, got %d", recDel.Code)
	}

	// 3. Purge all
	reqPurge := httptest.NewRequest("POST", "/api/v1/cache/purge?all=true", nil)
	recPurge := httptest.NewRecorder()
	r.ServeHTTP(recPurge, reqPurge)

	if recPurge.Code != http.StatusOK {
		t.Fatalf("expected 200 purging cache, got %d", recPurge.Code)
	}

	// 4. Pack starter cache
	targetGz := filepath.Join(t.TempDir(), "packed_starter.json.gz")
	packBody := fmt.Sprintf(`{"target_path": %q, "sanitize": true, "min_hits": 0}`, targetGz)
	reqPack := httptest.NewRequest("POST", "/api/v1/cache/pack", strings.NewReader(packBody))
	reqPack.Header.Set("Content-Type", "application/json")
	recPack := httptest.NewRecorder()
	r.ServeHTTP(recPack, reqPack)

	if recPack.Code != http.StatusOK {
		t.Fatalf("expected 200 packing starter cache, got %d: %s", recPack.Code, recPack.Body.String())
	}
}

func TestAdminKeysLifecycle(t *testing.T) {
	r, db, _ := setupAdminTest(t)
	defer db.Close()

	// 1. Create Key
	createBody := `{"name":"dashboard-tester","budget":35.0,"rpm":100,"tpm":150000}`
	reqCreate := httptest.NewRequest("POST", "/api/v1/keys", bytes.NewReader([]byte(createBody)))
	reqCreate.Header.Set("Content-Type", "application/json")
	recCreate := httptest.NewRecorder()

	r.ServeHTTP(recCreate, reqCreate)
	if recCreate.Code != http.StatusCreated {
		t.Fatalf("expected 201 created key, got %d: %s", recCreate.Code, recCreate.Body.String())
	}

	var createData struct {
		SecretKey string `json:"secret_key"`
		Key       struct {
			ID string `json:"id"`
		} `json:"key"`
	}
	_ = json.Unmarshal(recCreate.Body.Bytes(), &createData)
	if !strings.HasPrefix(createData.SecretKey, "lt-live-") {
		t.Errorf("expected lt-live- prefix in secret key, got %s", createData.SecretKey)
	}

	// 2. List Keys
	reqList := httptest.NewRequest("GET", "/api/v1/keys", nil)
	recList := httptest.NewRecorder()
	r.ServeHTTP(recList, reqList)

	if recList.Code != http.StatusOK {
		t.Fatalf("expected 200 listing keys, got %d", recList.Code)
	}

	// 3. Revoke Key
	reqRevoke := httptest.NewRequest("DELETE", "/api/v1/keys/"+createData.Key.ID, nil)
	recRevoke := httptest.NewRecorder()
	r.ServeHTTP(recRevoke, reqRevoke)

	if recRevoke.Code != http.StatusOK {
		t.Fatalf("expected 200 revoking key, got %d", recRevoke.Code)
	}
}

func TestAdminRoutes(t *testing.T) {
	r, db, _ := setupAdminTest(t)
	defer db.Close()

	req := httptest.NewRequest("GET", "/api/v1/routes", nil)
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var data struct {
		DefaultStrategy string `json:"default_strategy"`
		Routes          []map[string]interface{}
		CircuitBreakers []map[string]interface{}
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &data)
	if data.DefaultStrategy != "auto-resilient" {
		t.Errorf("expected default strategy auto-resilient, got %s", data.DefaultStrategy)
	}
	if len(data.Routes) == 0 {
		t.Errorf("expected defined routes in response")
	}

	// 2. Switch strategy to free-first
	switchBody := `{"strategy":"free-first"}`
	reqSwitch := httptest.NewRequest("POST", "/api/v1/routes/strategy", strings.NewReader(switchBody))
	reqSwitch.Header.Set("Content-Type", "application/json")
	recSwitch := httptest.NewRecorder()
	r.ServeHTTP(recSwitch, reqSwitch)

	if recSwitch.Code != http.StatusOK {
		t.Fatalf("expected 200 switching strategy, got %d: %s", recSwitch.Code, recSwitch.Body.String())
	}

	// 3. Verify updated strategy
	req2 := httptest.NewRequest("GET", "/api/v1/routes", nil)
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)

	var data2 struct {
		DefaultStrategy string `json:"default_strategy"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &data2)
	if data2.DefaultStrategy != "free-first" {
		t.Errorf("expected updated strategy free-first, got %s", data2.DefaultStrategy)
	}
}

func TestAdminProvidersEndpoints(t *testing.T) {
	r, db, _ := setupAdminTest(t)
	defer db.Close()

	// 1. List Providers
	reqList := httptest.NewRequest("GET", "/api/v1/providers", nil)
	recList := httptest.NewRecorder()
	r.ServeHTTP(recList, reqList)

	if recList.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recList.Code, recList.Body.String())
	}

	var listData struct {
		Providers []admin.ProviderSummary `json:"providers"`
	}
	_ = json.Unmarshal(recList.Body.Bytes(), &listData)
	if len(listData.Providers) == 0 {
		t.Fatalf("expected providers list to not be empty")
	}

	// 2. Update Providers
	body := `{"providers":{"groq":{"api_key":"gsk_test_key_1234567890"},"gemini":{"api_key":"gem1,gem2"},"openrouter":{"api_key":"sk-or-v1-testkey123456789"}}}`
	reqUp := httptest.NewRequest("POST", "/api/v1/providers", strings.NewReader(body))
	reqUp.Header.Set("Content-Type", "application/json")
	recUp := httptest.NewRecorder()
	r.ServeHTTP(recUp, reqUp)

	if recUp.Code != http.StatusOK {
		t.Fatalf("expected 200 updating providers, got %d: %s", recUp.Code, recUp.Body.String())
	}

	// 3. Verify Masked Keys in list
	reqList2 := httptest.NewRequest("GET", "/api/v1/providers", nil)
	recList2 := httptest.NewRecorder()
	r.ServeHTTP(recList2, reqList2)

	var listData2 struct {
		Providers []admin.ProviderSummary `json:"providers"`
	}
	_ = json.Unmarshal(recList2.Body.Bytes(), &listData2)
	for _, p := range listData2.Providers {
		if p.Name == "groq" {
			if !p.HasKey || !strings.Contains(p.APIKeyMasked, "••••••••") {
				t.Errorf("expected groq key to be masked, got %q", p.APIKeyMasked)
			}
		}
		if p.Name == "gemini" {
			if p.KeyCount != 2 {
				t.Errorf("expected 2 gemini keys, got %d", p.KeyCount)
			}
		}
		if p.Name == "openrouter" {
			if !p.HasKey || !strings.Contains(p.APIKeyMasked, "••••••••") {
				t.Errorf("expected openrouter key to be masked, got %q", p.APIKeyMasked)
			}
		}
	}

	// 4. Provider Stats
	reqStats := httptest.NewRequest("GET", "/api/v1/providers/stats", nil)
	recStats := httptest.NewRecorder()
	r.ServeHTTP(recStats, reqStats)
	if recStats.Code != http.StatusOK {
		t.Fatalf("expected 200 for provider stats, got %d", recStats.Code)
	}
}

func TestServeDashboard(t *testing.T) {
	r, db, _ := setupAdminTest(t)
	defer db.Close()

	req := httptest.NewRequest("GET", "/dashboard", nil)
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for dashboard, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
		t.Errorf("expected HTML doctype in dashboard response")
	}
	if !strings.Contains(rec.Body.String(), "liltok") {
		t.Errorf("expected liltok in dashboard body")
	}
}

func TestBroadcaster(t *testing.T) {
	b := admin.NewBroadcaster()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest("GET", "/api/v1/events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	go func() {
		b.ServeHTTP(rec, req)
	}()

	time.Sleep(50 * time.Millisecond)
	if count := b.ClientCount(); count != 1 {
		t.Fatalf("expected 1 connected client, got %d", count)
	}

	b.Broadcast(admin.TelemetryEvent{
		Type: "request",
		Data: map[string]string{"model": "gpt-4o"},
	})

	cancel()
	time.Sleep(50 * time.Millisecond)
	if count := b.ClientCount(); count != 0 {
		t.Errorf("expected 0 clients after disconnect, got %d", count)
	}
}

func TestAdminAnalytics(t *testing.T) {
	r, database, _ := setupAdminTest(t)
	defer database.Close()

	ctx := context.Background()
	_, err := database.ExecContext(ctx, `
		INSERT INTO request_logs (
			request_id, timestamp, model, requested_model, provider,
			cache_status, cache_tier, prompt_tokens, completion_tokens,
			cached_tokens, latency_ms, cost_usd, prompt_cost_usd, completion_cost_usd, saved_usd, status_code
		) VALUES 
		('req_1', datetime('now', '-2 hours'), 'claude-sonnet-5', 'claude-sonnet-5', 'anthropic', 'HIT', 'TIER1_EXACT', 100, 20, 0, 150, 0.0, 0.0, 0.0, 0.05, 200),
		('req_2', datetime('now', '-1 hour'), 'gemini-3.8-flash', 'claude-sonnet-5', 'gemini', 'MISS', 'NONE', 500, 50, 0, 800, 0.001, 0.0008, 0.0002, 0.25, 200)
	`)
	if err != nil {
		t.Fatalf("failed to insert mock logs: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/v1/analytics?range=24h", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode json: %v", err)
	}

	if resp["range"] != "24h" {
		t.Errorf("expected range 24h, got %v", resp["range"])
	}
	if _, ok := resp["time_series"]; !ok {
		t.Errorf("expected time_series in analytics response")
	}
	if _, ok := resp["providers"]; !ok {
		t.Errorf("expected providers in analytics response")
	}
	if _, ok := resp["percentiles"]; !ok {
		t.Errorf("expected percentiles in analytics response")
	}
}

func TestAdminQueryAndExportLogs(t *testing.T) {
	r, database, _ := setupAdminTest(t)
	defer database.Close()

	ctx := context.Background()
	_, _ = database.ExecContext(ctx, `
		INSERT INTO request_logs (
			request_id, timestamp, model, requested_model, provider,
			cache_status, cache_tier, prompt_tokens, completion_tokens,
			cached_tokens, latency_ms, cost_usd, prompt_cost_usd, completion_cost_usd, saved_usd, status_code
		) VALUES 
		('req_a1', datetime('now', '-5 minutes'), 'claude-sonnet-5', 'claude-sonnet-5', 'cache-local', 'HIT', 'TIER1_EXACT', 200, 40, 0, 10, 0.0, 0.0, 0.0, 0.10, 200),
		('req_a2', datetime('now', '-2 minutes'), 'gemini-3.8-flash', 'claude-sonnet-5', 'gemini', 'MISS', 'NONE', 1000, 100, 0, 500, 0.005, 0.004, 0.001, 0.50, 200)
	`)

	// Test Query
	req := httptest.NewRequest("GET", "/api/v1/logs/query?page=1&page_size=10&provider=gemini", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var queryData struct {
		Page       int                      `json:"page"`
		TotalCount int64                    `json:"total_count"`
		Logs       []*ledger.RequestLog     `json:"logs"`
		Summary    map[string]interface{}   `json:"summary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &queryData); err != nil {
		t.Fatalf("failed to decode json: %v", err)
	}

	if queryData.TotalCount != 1 {
		t.Errorf("expected 1 filtered log, got %d", queryData.TotalCount)
	}
	if len(queryData.Logs) != 1 || queryData.Logs[0].Provider != "gemini" {
		t.Errorf("expected log for provider gemini")
	}

	// Test Export CSV
	reqCSV := httptest.NewRequest("GET", "/api/v1/logs/export?format=csv", nil)
	recCSV := httptest.NewRecorder()
	r.ServeHTTP(recCSV, reqCSV)

	if recCSV.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recCSV.Code)
	}
	if !strings.Contains(recCSV.Body.String(), "req_a1") {
		t.Errorf("expected CSV to contain req_a1")
	}

	// Test Export JSON
	reqJSON := httptest.NewRequest("GET", "/api/v1/logs/export?format=json", nil)
	recJSON := httptest.NewRecorder()
	r.ServeHTTP(recJSON, reqJSON)

	if recJSON.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recJSON.Code)
	}
	if !strings.Contains(recJSON.Body.String(), "req_a1") {
		t.Errorf("expected JSON to contain req_a1")
	}
}

func TestAdminSystemDiagnosticsAndVacuum(t *testing.T) {
	r, database, _ := setupAdminTest(t)
	defer database.Close()

	// Diagnostics
	req := httptest.NewRequest("GET", "/api/v1/system", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var sysData map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &sysData); err != nil {
		t.Fatalf("failed to decode system json: %v", err)
	}

	if _, ok := sysData["runtime"]; !ok {
		t.Errorf("expected runtime stats in system diagnostics")
	}
	if _, ok := sysData["database"]; !ok {
		t.Errorf("expected database stats in system diagnostics")
	}
	if _, ok := sysData["circuit_breakers"]; !ok {
		t.Errorf("expected circuit_breakers in system diagnostics")
	}

	// Vacuum
	reqVac := httptest.NewRequest("POST", "/api/v1/system/vacuum", nil)
	recVac := httptest.NewRecorder()
	r.ServeHTTP(recVac, reqVac)

	if recVac.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recVac.Code)
	}
}

