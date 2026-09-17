package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
