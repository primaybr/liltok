package admin_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/primaybr/liltok/internal/admin"
	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/crypto"
	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/ledger"
	"github.com/primaybr/liltok/internal/miner"
	"github.com/primaybr/liltok/internal/router"
)

type adminEnv struct {
	mux        *chi.Mux
	db         *db.DB
	handler    *admin.AdminHandler
	router     *router.Router
	keys       *ledger.KeyManager
	cfg        *config.Config
	b          *admin.Broadcaster
	configPath string
}

// offlineAdminConfig points every provider at a local server that answers 503, so routers
// built from it never reach a real provider.
func offlineAdminConfig(t *testing.T) *config.Config {
	t.Helper()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(dead.Close)
	cfg := config.DefaultConfig()
	creds := config.ProviderCreds{BaseURL: dead.URL}
	cfg.Providers = config.ProvidersConfig{
		OpenAI: creds, Anthropic: creds, NVIDIANIM: creds, Groq: creds, Gemini: creds,
		OpenRouter: creds, Ollama: creds, Kilo: creds, Cline: creds,
	}
	cfg.Routes.DefaultStrategy = ""
	return cfg
}

func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	database, err := db.Open(filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	cfg := offlineAdminConfig(t)
	km := ledger.NewKeyManager(database)
	led := ledger.NewLedger(database, km)
	t.Cleanup(func() { led.Close() })
	rtr := router.NewRouter(cfg)
	b := admin.NewBroadcaster()
	h := admin.NewAdminHandler(cfg, database, led, km, rtr, nil, b)
	configPath := filepath.Join(t.TempDir(), "liltok.yaml")
	if err := os.WriteFile(configPath, []byte("providers:\n  groq:\n    api_key: \"\"\nroutes:\n  default_strategy: \"auto-resilient\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.SetConfigPath(configPath)
	mux := chi.NewRouter()
	h.RegisterRoutes(mux)
	return &adminEnv{mux: mux, db: database, handler: h, router: rtr, keys: km, cfg: cfg, b: b, configPath: configPath}
}

// newBareMux registers a handler that has no database, key manager or router.
func newBareMux(t *testing.T, cfg *config.Config) *chi.Mux {
	t.Helper()
	h := admin.NewAdminHandler(cfg, nil, nil, nil, nil, nil, nil)
	h.SetConfigPath(filepath.Join(t.TempDir(), "liltok.yaml"))
	mux := chi.NewRouter()
	h.RegisterRoutes(mux)
	return mux
}

func do(t *testing.T, mux http.Handler, method, target string, body interface{}) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	var rdr *bytes.Reader
	switch b := body.(type) {
	case nil:
		rdr = bytes.NewReader(nil)
	case string:
		rdr = bytes.NewReader([]byte(b))
	default:
		raw, _ := json.Marshal(b)
		rdr = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, target, rdr)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// listen subscribes to the broadcaster and returns events after the initial "connected" one.
func listen(t *testing.T, b *admin.Broadcaster) <-chan admin.TelemetryEvent {
	t.Helper()
	srv := httptest.NewServer(b)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); srv.Close() })
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan admin.TelemetryEvent, 64)
	connected := make(chan struct{})
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		first := true
		for sc.Scan() {
			line, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			var ev admin.TelemetryEvent
			if json.Unmarshal([]byte(line), &ev) != nil {
				continue
			}
			if first {
				first = false
				close(connected)
				continue
			}
			events <- ev
		}
	}()
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("broadcaster subscription did not connect")
	}
	return events
}

// waitEvent returns the first event of the given type.
func waitEvent(t *testing.T, events <-chan admin.TelemetryEvent, typ string) map[string]interface{} {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Type == typ {
				data, _ := ev.Data.(map[string]interface{})
				return data
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %q event", typ)
			return nil
		}
	}
}

type logRow struct {
	id, model, requested, provider, status, tier, errMsg, age string
	cached, code                                              int
}

func insertLogs(t *testing.T, database *db.DB, rows []logRow) {
	t.Helper()
	for _, r := range rows {
		_, err := database.Exec(`INSERT INTO request_logs (request_id, timestamp, model, requested_model, provider, cache_status, cache_tier,
			prompt_tokens, completion_tokens, cached_tokens, latency_ms, cost_usd, saved_usd, status_code, error_message)
			VALUES (?, datetime('now', ?), ?, ?, ?, ?, ?, 10, 5, ?, 100, 0.01, 0.02, ?, ?)`,
			r.id, r.age, r.model, r.requested, r.provider, r.status, r.tier, r.cached, r.code, r.errMsg)
		if err != nil {
			t.Fatal(err)
		}
	}
}

var sampleLogs = []logRow{
	{id: "r1", model: "qwen", requested: "groq/qwen", provider: "groq", status: "HIT", tier: "TIER1_EXACT", age: "-1 minute", code: 200},
	{id: "r2", model: "gpt-4o", provider: "openai", status: "MISS", tier: "NONE", age: "-3 days", code: 200},
	{id: "r3", model: "claude", provider: "anthropic", status: "MISS", tier: "TIER2_PREFIX", cached: 50, age: "-2 hours", code: 429, errMsg: "rate limited upstream"},
	{id: "r4", model: "gpt-4o", provider: "cache-local", status: "HIT", tier: "TIER3_SEMANTIC", age: "-40 days", code: 502},
}

func TestAdminLogsPaginationAndClear(t *testing.T) {
	env := newAdminEnv(t)
	insertLogs(t, env.db, sampleLogs)
	events := listen(t, env.b)

	rec, out := do(t, env.mux, "GET", "/api/v1/logs?limit=2&offset=1", nil)
	if rec.Code != http.StatusOK || out["limit"] != float64(2) || out["offset"] != float64(1) {
		t.Fatalf("status %d body %v", rec.Code, out)
	}
	logs, _ := out["logs"].([]interface{})
	if len(logs) != 2 {
		t.Fatalf("got %d logs, want 2", len(logs))
	}
	first := logs[0].(map[string]interface{})
	second := logs[1].(map[string]interface{})
	if first["request_id"] != "r3" || second["request_id"] != "r2" {
		t.Fatalf("page = %v, %v; want r3 then r2 (newest first, skipping r1)", first["request_id"], second["request_id"])
	}
	if second["requested_model"] != "gpt-4o" {
		t.Fatalf("empty requested_model should fall back to model, got %v", second["requested_model"])
	}

	_, keyObj, err := env.keys.CreateKey(context.Background(), "spender", 10, 60, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.keys.UpdateSpend(context.Background(), keyObj.ID, 1.5); err != nil {
		t.Fatal(err)
	}

	rec, out = do(t, env.mux, "POST", "/api/v1/logs/clear?reset_spends=true", nil)
	if rec.Code != http.StatusOK || out["cleared_count"] != float64(4) {
		t.Fatalf("clear status %d body %v", rec.Code, out)
	}
	if data := waitEvent(t, events, "logs_cleared"); data["cleared_count"] != float64(4) {
		t.Fatalf("logs_cleared event = %v", data)
	}
	var spend float64
	if err := env.db.QueryRow(`SELECT current_spend_usd FROM api_keys WHERE id = ?`, keyObj.ID).Scan(&spend); err != nil || spend != 0 {
		t.Fatalf("spend after reset = %v, %v; want 0", spend, err)
	}
	_, out = do(t, env.mux, "GET", "/api/v1/logs?limit=-1&offset=-3", nil)
	if out["limit"] != float64(50) || out["offset"] != float64(0) || out["logs"] != nil {
		t.Fatalf("after clear = %v; want defaults and no logs", out)
	}
}

func TestAdminQueryLogsFilters(t *testing.T) {
	env := newAdminEnv(t)
	insertLogs(t, env.db, sampleLogs)
	cases := []struct {
		query string
		want  float64
	}{
		{"", 4},
		{"provider=all&model=all&cache_status=all&time_range=all&status_code=all", 4},
		{"provider=groq", 1},
		{"model=gpt-4o", 2},
		{"model=groq/qwen", 1},
		{"cache_status=HIT", 2},
		{"cache_status=MISS", 1},
		{"cache_status=TIER1_EXACT", 1},
		{"cache_status=TIER2_PREFIX", 1},
		{"cache_status=TIER3_SEMANTIC", 1},
		{"time_range=1h", 1},
		{"time_range=24h", 2},
		{"time_range=7d", 3},
		{"time_range=30d", 3},
		{"status_code=200", 2},
		{"status_code=4xx", 1},
		{"status_code=5xx", 1},
		{"search=rate", 1},
		{"search=qwen", 1},
		{"provider=openai&time_range=7d", 1},
	}
	for _, tc := range cases {
		rec, out := do(t, env.mux, "GET", "/api/v1/logs/query?"+tc.query, nil)
		if rec.Code != http.StatusOK || out["total_count"] != tc.want {
			t.Errorf("query %q: status %d total %v, want %v", tc.query, rec.Code, out["total_count"], tc.want)
		}
	}

	req := httptest.NewRequest("GET", "/api/v1/logs/export?status_code=4xx", nil)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "r3") || strings.Contains(rec.Body.String(), "r1") {
		t.Fatalf("export status %d body %q; want only r3", rec.Code, rec.Body.String())
	}
}

func TestAdminSystemVacuumFileDatabase(t *testing.T) {
	env := newAdminEnv(t)
	rec, out := do(t, env.mux, "POST", "/api/v1/system/vacuum", nil)
	if rec.Code != http.StatusOK || out["db_size_bytes"].(float64) <= 0 {
		t.Fatalf("vacuum status %d body %v; want a positive file size", rec.Code, out)
	}
	_ = env.db.Close()
	if rec, _ := do(t, env.mux, "POST", "/api/v1/system/vacuum", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("vacuum on a closed database = %d, want 500", rec.Code)
	}
}

func insertCacheEntry(t *testing.T, database *db.DB, hash, model string, hits int, tags ...string) {
	t.Helper()
	if _, err := database.Exec(`INSERT INTO cache_entries (hash, model, normalized_prompt, response_payload, prompt_tokens, completion_tokens, hit_count)
		VALUES (?, ?, ?, ?, 1000, 500, ?)`, hash, model, "prompt for "+hash, []byte(`{"answer":"`+hash+`"}`), hits); err != nil {
		t.Fatal(err)
	}
	for _, tag := range tags {
		if _, err := database.Exec(`INSERT INTO cache_tags (cache_hash, tag) VALUES (?, ?)`, hash, tag); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAdminCacheEntryLifecycle(t *testing.T) {
	env := newAdminEnv(t)
	insertCacheEntry(t, env.db, "h-one", "gpt-4o", 3, "go", "docs")
	insertCacheEntry(t, env.db, "h-two", "claude-x", 0)
	insertCacheEntry(t, env.db, "h-three", "claude-x", 0)

	rec, out := do(t, env.mux, "GET", "/api/v1/cache/h-one", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status %d: %s", rec.Code, rec.Body.String())
	}
	if out["total_tokens"] != float64(1500) || out["response_payload"] != `{"answer":"h-one"}` || out["is_pinned"] != false || out["hit_count"] != float64(3) {
		t.Fatalf("entry = %v", out)
	}
	if tags, _ := out["tags"].([]interface{}); len(tags) != 2 {
		t.Fatalf("tags = %v, want two", out["tags"])
	}
	if saved, _ := out["saved_usd_est"].(float64); saved <= 0 {
		t.Fatalf("saved_usd_est = %v, want the per-hit saving times 3 hits", out["saved_usd_est"])
	}
	env.handler.SetPricing(nil)
	if _, out := do(t, env.mux, "GET", "/api/v1/cache/h-one", nil); out["saved_usd_est"] != float64(0) {
		t.Fatalf("without pricing saved_usd_est = %v, want 0", out["saved_usd_est"])
	}
	if rec, _ := do(t, env.mux, "GET", "/api/v1/cache/missing", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("get missing = %d, want 404", rec.Code)
	}

	for i, want := range []bool{true, false} {
		rec, out := do(t, env.mux, "POST", "/api/v1/cache/h-one/pin", nil)
		if rec.Code != http.StatusOK || out["is_pinned"] != want {
			t.Fatalf("toggle %d = %d %v, want is_pinned %v", i+1, rec.Code, out, want)
		}
		var pinned bool
		_ = env.db.QueryRow(`SELECT is_pinned FROM cache_entries WHERE hash = 'h-one'`).Scan(&pinned)
		if pinned != want {
			t.Fatalf("stored is_pinned = %v after toggle %d", pinned, i+1)
		}
	}
	if rec, _ := do(t, env.mux, "POST", "/api/v1/cache/missing/pin", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("pin missing = %d, want 404", rec.Code)
	}

	rec, out = do(t, env.mux, "DELETE", "/api/v1/cache/h-one", nil)
	if rec.Code != http.StatusOK || out["hash"] != "h-one" {
		t.Fatalf("delete = %d %v", rec.Code, out)
	}
	var n int
	_ = env.db.QueryRow(`SELECT COUNT(*) FROM cache_entries WHERE hash = 'h-one'`).Scan(&n)
	if n != 0 {
		t.Fatal("entry still present after delete")
	}

	if rec, _ := do(t, env.mux, "POST", "/api/v1/cache/purge", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("purge without a filter = %d, want 400", rec.Code)
	}
	if _, out := do(t, env.mux, "POST", "/api/v1/cache/purge?model=claude-x", nil); out["deleted_count"] != float64(2) {
		t.Fatalf("purge by model = %v, want 2 deleted", out)
	}
	insertCacheEntry(t, env.db, "h-four", "other", 0)
	if _, out := do(t, env.mux, "POST", "/api/v1/cache/purge?all=true", nil); out["deleted_count"] != float64(1) {
		t.Fatalf("purge all = %v, want 1 deleted", out)
	}

	_ = env.db.Close()
	if rec, _ := do(t, env.mux, "GET", "/api/v1/cache/h-two", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("get on a closed database = %d, want 500", rec.Code)
	}
	if rec, _ := do(t, env.mux, "POST", "/api/v1/cache/h-two/pin", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("pin on a closed database = %d, want 500", rec.Code)
	}
}

func TestAdminHandlersWithoutBackends(t *testing.T) {
	mux := newBareMux(t, nil)
	cases := []struct {
		method, target string
		body           interface{}
		want           int
	}{
		{"GET", "/api/v1/logs", nil, http.StatusOK},
		{"POST", "/api/v1/logs/clear", nil, http.StatusInternalServerError},
		{"POST", "/api/v1/system/vacuum", nil, http.StatusInternalServerError},
		{"GET", "/api/v1/cache/abc", nil, http.StatusInternalServerError},
		{"POST", "/api/v1/cache/abc/pin", nil, http.StatusInternalServerError},
		{"DELETE", "/api/v1/cache/abc", nil, http.StatusOK},
		{"POST", "/api/v1/cache/purge?all=true", nil, http.StatusInternalServerError},
		{"POST", "/api/v1/cache/pack", "{}", http.StatusInternalServerError},
		{"PUT", "/api/v1/routes", `{"id":"x"}`, http.StatusServiceUnavailable},
		{"DELETE", "/api/v1/routes/x", nil, http.StatusServiceUnavailable},
		{"POST", "/api/v1/routes/reset-breakers", nil, http.StatusOK},
		{"GET", "/api/v1/models/catalog", nil, http.StatusOK},
		{"POST", "/api/v1/models/catalog/reactivate", `{"provider":"groq","model":"m"}`, http.StatusNotFound},
		{"GET", "/api/v1/keys", nil, http.StatusOK},
		{"POST", "/api/v1/keys", `{"name":"k"}`, http.StatusInternalServerError},
		{"DELETE", "/api/v1/keys/abc", nil, http.StatusInternalServerError},
		{"POST", "/api/v1/providers", `{"providers":{}}`, http.StatusInternalServerError},
		{"POST", "/api/v1/providers/test", `{"provider":"groq"}`, http.StatusInternalServerError},
		{"POST", "/api/v1/providers/groq/sync-models", nil, http.StatusInternalServerError},
		{"GET", "/api/v1/providers/stats", nil, http.StatusOK},
		{"GET", "/api/v1/miner/status", nil, http.StatusOK},
		{"GET", "/api/v1/miner/prompts", nil, http.StatusInternalServerError},
		{"POST", "/api/v1/miner/start", `{}`, http.StatusInternalServerError},
		{"POST", "/api/v1/miner/stop", nil, http.StatusInternalServerError},
		{"POST", "/api/v1/moderate/upload", nil, http.StatusForbidden},
		{"GET", "/api/v1/moderate/entries", nil, http.StatusForbidden},
		{"POST", "/api/v1/moderate/approve", `{"ids":[1]}`, http.StatusForbidden},
		{"POST", "/api/v1/moderate/reject", `{"ids":[1]}`, http.StatusForbidden},
		{"DELETE", "/api/v1/moderate/clear", nil, http.StatusForbidden},
	}
	for _, tc := range cases {
		if rec, _ := do(t, mux, tc.method, tc.target, tc.body); rec.Code != tc.want {
			t.Errorf("%s %s = %d, want %d (%s)", tc.method, tc.target, rec.Code, tc.want, rec.Body.String())
		}
	}
	if _, out := do(t, mux, "GET", "/api/v1/miner/status", nil); out["status"] != string(miner.MiningStatusIdle) {
		t.Errorf("miner status without a manager = %v, want idle", out["status"])
	}
}

func TestAdminPackStarterCache(t *testing.T) {
	env := newAdminEnv(t)
	insertCacheEntry(t, env.db, "pack-1", "gpt-4o", 5)
	target := filepath.Join(t.TempDir(), "starter.json.gz")

	rec, out := do(t, env.mux, "POST", "/api/v1/cache/pack", map[string]interface{}{"target_path": target, "sanitize": false, "min_hits": 1})
	if rec.Code != http.StatusOK || out["target_path"] != target || out["merged_from_db"].(float64) < 1 {
		t.Fatalf("pack status %d body %v", rec.Code, out)
	}
	if fi, err := os.Stat(target); err != nil || fi.Size() == 0 {
		t.Fatalf("packed file missing or empty: %v", err)
	}

	// A directory cannot be written as the pack file.
	rec, _ = do(t, env.mux, "POST", "/api/v1/cache/pack", map[string]interface{}{"target_path": t.TempDir()})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("pack onto a directory = %d, want 500", rec.Code)
	}
}

type failingDeleteStore struct{}

func (failingDeleteStore) LoadRoutes(context.Context) ([]router.Route, error) { return nil, nil }
func (failingDeleteStore) SaveRoute(context.Context, router.Route) error      { return nil }
func (failingDeleteStore) DeleteRoute(context.Context, string) error {
	return errors.New("route table locked")
}

func TestAdminRouteStrategyAndBreakers(t *testing.T) {
	env := newAdminEnv(t)
	events := listen(t, env.b)

	if rec, _ := do(t, env.mux, "POST", "/api/v1/routes/strategy", "{"); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON = %d, want 400", rec.Code)
	}
	if rec, _ := do(t, env.mux, "POST", "/api/v1/routes/strategy", map[string]string{"strategy": "cheapest"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown strategy = %d, want 400", rec.Code)
	}

	groq, _ := env.router.GetBreaker("groq")
	groq.TripImmediate()
	rec, out := do(t, env.mux, "POST", "/api/v1/routes/strategy", map[string]string{"strategy": " Free-First "})
	if rec.Code != http.StatusOK || out["strategy"] != "free-first" {
		t.Fatalf("set strategy = %d %v", rec.Code, out)
	}
	if env.router.DefaultStrategy() != "free-first" || env.cfg.Routes.DefaultStrategy != "free-first" {
		t.Fatal("strategy not applied to the router and config")
	}
	if state, _ := groq.State(); state != router.StateClosed {
		t.Fatalf("breakers should be reset on a strategy change, groq = %s", state)
	}
	if data, _ := os.ReadFile(env.configPath); !strings.Contains(string(data), "free-first") {
		t.Fatalf("strategy not persisted: %s", data)
	}
	if data := waitEvent(t, events, "strategy_change"); data["strategy"] != "free-first" {
		t.Fatalf("strategy_change event = %v", data)
	}

	groq.TripImmediate()
	rec, out = do(t, env.mux, "POST", "/api/v1/routes/reset-breakers?name=groq", nil)
	if rec.Code != http.StatusOK || !strings.Contains(out["message"].(string), "groq") {
		t.Fatalf("reset named breaker = %d %v", rec.Code, out)
	}
	if state, _ := groq.State(); state != router.StateClosed {
		t.Fatalf("groq breaker = %s after a named reset", state)
	}
	if data := waitEvent(t, events, "breakers_reset"); data["name"] != "groq" {
		t.Fatalf("breakers_reset event = %v", data)
	}
	other, _ := env.router.GetBreaker("gemini")
	other.TripImmediate()
	if rec, _ := do(t, env.mux, "POST", "/api/v1/routes/reset-breakers", nil); rec.Code != http.StatusOK {
		t.Fatalf("reset all = %d", rec.Code)
	}
	if state, _ := other.State(); state != router.StateClosed {
		t.Fatalf("gemini breaker = %s after resetting all", state)
	}

	if rec, _ := do(t, env.mux, "DELETE", "/api/v1/routes/no-such-route", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("reset unknown route = %d, want 404", rec.Code)
	}
	if rec, _ := do(t, env.mux, "DELETE", "/api/v1/routes/free-first", nil); rec.Code != http.StatusOK {
		t.Fatalf("reset built-in route = %d, want 200", rec.Code)
	}
	if err := env.router.SetRouteStore(context.Background(), failingDeleteStore{}); err != nil {
		t.Fatal(err)
	}
	if rec, out := do(t, env.mux, "DELETE", "/api/v1/routes/free-first", nil); rec.Code != http.StatusInternalServerError || !strings.Contains(fmt.Sprint(out["error"]), "locked") {
		t.Fatalf("reset with a failing store = %d %v, want 500", rec.Code, out)
	}

	if rec, _ := do(t, env.mux, "PUT", "/api/v1/routes", "not json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("upsert invalid JSON = %d, want 400", rec.Code)
	}
	if rec, _ := do(t, env.mux, "PUT", "/api/v1/routes", map[string]interface{}{"id": "Bad ID!"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("upsert invalid route = %d, want 400", rec.Code)
	}
}

func TestAdminModelCatalogAndReactivate(t *testing.T) {
	env := newAdminEnv(t)
	cat := env.router.Catalog()
	cat.MarkInactive("nvidianim", "zeta", "status 404")
	cat.MarkInactive("groq", "beta", "status 404")
	cat.MarkInactive("groq", "alpha", "decommissioned")

	req := httptest.NewRequest("GET", "/api/v1/models/catalog", nil)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	var entries []router.CatalogEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, e := range entries {
		order = append(order, e.Provider+"/"+e.Model)
	}
	if strings.Join(order, ",") != "groq/alpha,groq/beta,nvidianim/zeta" {
		t.Fatalf("catalog order = %v, want sorted by provider then model", order)
	}

	if rec, _ := do(t, env.mux, "POST", "/api/v1/models/catalog/reactivate", `{"provider":"groq"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("reactivate without a model = %d, want 400", rec.Code)
	}
	if rec, _ := do(t, env.mux, "POST", "/api/v1/models/catalog/reactivate", `{"provider":"groq","model":"never-marked"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("reactivate an active model = %d, want 404", rec.Code)
	}
	if rec, _ := do(t, env.mux, "POST", "/api/v1/models/catalog/reactivate", `{"provider":"groq","model":"beta"}`); rec.Code != http.StatusOK {
		t.Fatalf("reactivate = %d, want 200", rec.Code)
	}
	if !cat.Usable("groq", "beta") {
		t.Fatal("groq/beta should be usable after reactivation")
	}
}

func TestAdminKeysErrorPaths(t *testing.T) {
	env := newAdminEnv(t)
	if rec, _ := do(t, env.mux, "POST", "/api/v1/keys", "{bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("create with a bad body = %d, want 400", rec.Code)
	}
	if rec, _ := do(t, env.mux, "DELETE", "/api/v1/keys/does-not-exist", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("revoke unknown key = %d, want 404", rec.Code)
	}
	_ = env.db.Close()
	if rec, _ := do(t, env.mux, "GET", "/api/v1/keys", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("list on a closed database = %d, want 500", rec.Code)
	}
	if rec, _ := do(t, env.mux, "POST", "/api/v1/keys", `{"name":"k"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("create on a closed database = %d, want 500", rec.Code)
	}
}

// modelServer answers health checks and model listings, recording the auth headers it saw.
func modelServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu    sync.Mutex
		auths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"listed-model","context_window":32000}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), auths...)
	}
}

func TestAdminUpdateTestAndSyncProviders(t *testing.T) {
	env := newAdminEnv(t)
	srv, auths := modelServer(t)
	events := listen(t, env.b)

	if rec, _ := do(t, env.mux, "POST", "/api/v1/providers", "nope"); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid payload = %d, want 400", rec.Code)
	}

	body := map[string]interface{}{"providers": map[string]interface{}{
		"GROQ":       map[string]string{"api_key": "  groq-key  ", "base_url": srv.URL},
		"openai":     map[string]string{"api_key": "oai"},
		"anthropic":  map[string]string{"api_key": "ant"},
		"nvidianim":  map[string]string{"api_key": "nim"},
		"gemini":     map[string]string{"api_key": "gem"},
		"openrouter": map[string]string{"api_key": "or"},
		"kilo":       map[string]string{"api_key": "kilo"},
		"cline":      map[string]string{"api_key": "cline"},
		"ollama":     map[string]string{"base_url": "  "},
		"unknown":    map[string]string{"api_key": "ignored"},
	}}
	rec, _ := do(t, env.mux, "POST", "/api/v1/providers", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", rec.Code, rec.Body.String())
	}
	p := env.cfg.Providers
	if p.Groq.APIKey != "groq-key" || p.Groq.BaseURL != srv.URL || p.OpenAI.APIKey != "oai" || p.Anthropic.APIKey != "ant" ||
		p.NVIDIANIM.APIKey != "nim" || p.Gemini.APIKey != "gem" || p.OpenRouter.APIKey != "or" || p.Kilo.APIKey != "kilo" || p.Cline.APIKey != "cline" {
		t.Fatalf("config after update = %+v", p)
	}
	if p.Ollama.BaseURL == "" || strings.TrimSpace(p.Ollama.BaseURL) == "" {
		t.Fatal("a blank base_url must not clear the existing one")
	}
	if data, _ := os.ReadFile(env.configPath); !strings.Contains(string(data), "groq-key") {
		t.Fatalf("providers not persisted: %s", data)
	}
	waitEvent(t, events, "providers_updated")

	if rec, _ := do(t, env.mux, "POST", "/api/v1/providers/test", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("test without a provider = %d, want 400", rec.Code)
	}
	rec, out := do(t, env.mux, "POST", "/api/v1/providers/test", `{"provider":"groq"}`)
	if rec.Code != http.StatusOK || out["ok"] != true {
		t.Fatalf("test groq = %d %v", rec.Code, out)
	}
	found := false
	for _, a := range auths() {
		found = found || a == "Bearer groq-key"
	}
	if !found {
		t.Fatalf("groq health check did not use the new key: %v", auths())
	}
	if _, out := do(t, env.mux, "POST", "/api/v1/providers/test", `{"provider":"nope"}`); out["ok"] != false || out["error"] == nil {
		t.Fatalf("test unknown provider = %v, want ok false with an error", out)
	}

	rec, out = do(t, env.mux, "POST", "/api/v1/providers/groq/sync-models", nil)
	if rec.Code != http.StatusOK || out["total"] != float64(1) || out["status"] != "synced" {
		t.Fatalf("sync groq = %d %v", rec.Code, out)
	}
	if rec, _ := do(t, env.mux, "POST", "/api/v1/providers/nope/sync-models", nil); rec.Code != http.StatusBadGateway {
		t.Fatalf("sync unknown provider = %d, want 502", rec.Code)
	}

	// The runtime keeps the update when the config file cannot be written.
	env.handler.SetConfigPath(t.TempDir())
	rec, out = do(t, env.mux, "POST", "/api/v1/providers", map[string]interface{}{"providers": map[string]interface{}{"groq": map[string]string{"api_key": "second"}}})
	if rec.Code != http.StatusInternalServerError || !strings.Contains(fmt.Sprint(out["error"]), "lost on restart") {
		t.Fatalf("update with an unwritable config = %d %v", rec.Code, out)
	}
	if env.cfg.Providers.Groq.APIKey != "second" {
		t.Fatal("runtime config should hold the new key even when persisting fails")
	}
}

func TestAdminProviderStats(t *testing.T) {
	env := newAdminEnv(t)
	insertLogs(t, env.db, sampleLogs)
	rec, out := do(t, env.mux, "GET", "/api/v1/providers/stats", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	stats, _ := out["stats"].([]interface{})
	byProvider := map[string]map[string]interface{}{}
	for _, s := range stats {
		m := s.(map[string]interface{})
		byProvider[m["provider"].(string)] = m
	}
	if len(byProvider) != 4 {
		t.Fatalf("stats = %v, want one row per provider", stats)
	}
	if g := byProvider["groq"]; g["total_requests"] != float64(1) || g["total_tokens"] != float64(15) || g["avg_latency_ms"] != float64(100) {
		t.Fatalf("groq stats = %v", g)
	}
}

func TestAdminMinerLifecycle(t *testing.T) {
	env := newAdminEnv(t)
	for _, k := range []string{"GROQ_API_KEY", "NVIDIA_NIM_API_KEY", "OPENROUTER_API_KEY", "CLINE_API_KEY", "KILO_API_KEY"} {
		t.Setenv(k, "")
	}

	rec, out := do(t, env.mux, "GET", "/api/v1/miner/prompts?category=all", nil)
	if rec.Code != http.StatusOK || out["summary"] == nil {
		t.Fatalf("prompts = %d %v", rec.Code, out)
	}
	if rec, _ := do(t, env.mux, "POST", "/api/v1/miner/start", "{"); rec.Code != http.StatusBadRequest {
		t.Fatalf("start with invalid JSON = %d, want 400", rec.Code)
	}
	for _, prov := range []string{"", "nvidianim", "openrouter", "cline"} {
		rec, out := do(t, env.mux, "POST", "/api/v1/miner/start", map[string]string{"provider": prov})
		if rec.Code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out["error"]), "Missing API key") {
			t.Fatalf("start %q without a key = %d %v, want 400 missing key", prov, rec.Code, out)
		}
	}
	rec, out = do(t, env.mux, "POST", "/api/v1/miner/start", map[string]string{"provider": "kilo", "category": "no-such-category"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out["error"]), "No matching prompts") {
		t.Fatalf("start with an empty category = %d %v, want 400", rec.Code, out)
	}

	// A local upstream that holds each request until the session is stopped.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	prompts := miner.GetCuratedPrompts("all")
	if len(prompts) == 0 {
		t.Fatal("corpus is empty")
	}
	start := map[string]interface{}{"provider": "ollama", "base_url": upstream.URL, "model": "local", "prompt_ids": []string{prompts[0].ID}, "workers": 1}
	rec, out = do(t, env.mux, "POST", "/api/v1/miner/start", start)
	if rec.Code != http.StatusOK || out["status"] != "started" || out["total_prompts"] != float64(1) || out["provider"] != "ollama" {
		t.Fatalf("start = %d %v", rec.Code, out)
	}
	if rec, _ := do(t, env.mux, "POST", "/api/v1/miner/start", start); rec.Code != http.StatusConflict {
		t.Fatalf("second start = %d, want 409", rec.Code)
	}
	if _, out := do(t, env.mux, "GET", "/api/v1/miner/status", nil); out["status"] != string(miner.MiningStatusRunning) {
		t.Fatalf("status while mining = %v", out["status"])
	}
	if rec, out := do(t, env.mux, "POST", "/api/v1/miner/stop", nil); rec.Code != http.StatusOK || out["status"] != "stopping" {
		t.Fatalf("stop = %d %v", rec.Code, out)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		st := env.handler.MiningManager().Status().Status
		if st != miner.MiningStatusRunning && st != miner.MiningStatusStopping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mining session still %s after stop", st)
		}
		time.Sleep(20 * time.Millisecond)
	}

	mm := miner.NewMiningManager(env.db, nil)
	env.handler.SetMiningManager(mm)
	if env.handler.MiningManager() != mm {
		t.Fatal("MiningManager() should return the manager set by SetMiningManager")
	}
	if env.handler.Broadcaster() != env.b {
		t.Fatal("Broadcaster() should return the handler's broadcaster")
	}
}

func multipartUpload(t *testing.T, mux http.Handler, field, filename string, data []byte) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, _ := w.CreateFormFile(field, filename)
	_, _ = part.Write(data)
	_ = w.Close()
	req := httptest.NewRequest("POST", "/api/v1/moderate/upload", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestAdminModerationPaths(t *testing.T) {
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	database, err := db.Open(filepath.Join(t.TempDir(), "mod.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	pair, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	other, _ := crypto.GenerateKeyPair()

	cfg := config.DefaultConfig()
	cfg.Maintainer.Enabled = true
	cfg.Maintainer.PrivateKeyFile = "" // never read a real key from the home directory
	h := admin.NewAdminHandler(cfg, database, nil, nil, nil, nil, nil)
	h.SetConfigPath(filepath.Join(t.TempDir(), "liltok.yaml"))
	mux := chi.NewRouter()
	h.RegisterRoutes(mux)

	items := func(prefix string, n int) []byte {
		var list []db.StarterCacheItem
		for i := 0; i < n; i++ {
			list = append(list, db.StarterCacheItem{Hash: fmt.Sprintf("%s-%d", prefix, i), Model: "gpt-4o", NormalizedPrompt: prefix + " prompt", ResponsePayload: "answer", TTLSeconds: 60})
		}
		raw, _ := json.Marshal(list)
		return raw
	}
	gz := func(b []byte) []byte {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(b)
		_ = zw.Close()
		return buf.Bytes()
	}

	if rec, _ := do(t, mux, "POST", "/api/v1/moderate/upload", "not multipart"); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-multipart upload = %d, want 400", rec.Code)
	}
	if rec, out := multipartUpload(t, mux, "other", "x.json", items("x", 1)); rec.Code != http.StatusBadRequest || out["error"] != "missing file field" {
		t.Fatalf("upload without a file field = %d %v", rec.Code, out)
	}
	if rec, out := multipartUpload(t, mux, "file", "plain.json", items("plain", 2)); rec.Code != http.StatusOK || out["staged_count"] != float64(2) {
		t.Fatalf("plain JSON upload = %d %v", rec.Code, out)
	}
	if rec, out := multipartUpload(t, mux, "file", "packed.json.gz", gz(items("gz", 1))); rec.Code != http.StatusOK || out["staged_count"] != float64(1) {
		t.Fatalf("gzip upload = %d %v", rec.Code, out)
	}
	if rec, _ := multipartUpload(t, mux, "file", "bad.json", []byte("[{")); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON upload = %d, want 400", rec.Code)
	}

	encrypted, err := crypto.EncryptPayload(pair.PublicKey, items("enc", 1))
	if err != nil {
		t.Fatal(err)
	}
	if rec, out := multipartUpload(t, mux, "file", "s.enc", encrypted); rec.Code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out["error"]), "not configured") {
		t.Fatalf("encrypted upload without a key file = %d %v", rec.Code, out)
	}
	cfg.Maintainer.PrivateKeyFile = filepath.Join(t.TempDir(), "missing.key")
	if rec, out := multipartUpload(t, mux, "file", "s.enc", encrypted); rec.Code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out["error"]), "failed to read private key") {
		t.Fatalf("encrypted upload with a missing key file = %d %v", rec.Code, out)
	}
	badKey := filepath.Join(t.TempDir(), "bad.key")
	_ = os.WriteFile(badKey, []byte("not a key"), 0o600)
	cfg.Maintainer.PrivateKeyFile = badKey
	if rec, out := multipartUpload(t, mux, "file", "s.enc", encrypted); rec.Code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out["error"]), "invalid private key") {
		t.Fatalf("encrypted upload with an invalid key = %d %v", rec.Code, out)
	}
	wrongKey := filepath.Join(t.TempDir(), "wrong.key")
	_ = os.WriteFile(wrongKey, []byte(other.PrivateKeyStr), 0o600)
	cfg.Maintainer.PrivateKeyFile = wrongKey
	if rec, _ := multipartUpload(t, mux, "file", "s.enc", encrypted); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("encrypted upload for another key = %d, want 422", rec.Code)
	}
	goodKey := filepath.Join(t.TempDir(), "good.key")
	_ = os.WriteFile(goodKey, []byte(pair.PrivateKeyStr), 0o600)
	cfg.Maintainer.PrivateKeyFile = goodKey
	if rec, out := multipartUpload(t, mux, "file", "s.enc", encrypted); rec.Code != http.StatusOK || out["staged_count"] != float64(1) {
		t.Fatalf("encrypted raw JSON upload = %d %v", rec.Code, out)
	}

	var entries struct {
		Entries []db.StagedEntry `json:"entries"`
		Count   int              `json:"count"`
		Status  string           `json:"status"`
	}
	req := httptest.NewRequest("GET", "/api/v1/moderate/entries?limit=2&offset=1", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	_ = json.Unmarshal(rec.Body.Bytes(), &entries)
	if rec.Code != http.StatusOK || entries.Count != 2 || entries.Status != "pending" {
		t.Fatalf("entries page = %d %+v, want 2 pending", rec.Code, entries)
	}

	for _, action := range []string{"approve", "reject"} {
		if rec, _ := do(t, mux, "POST", "/api/v1/moderate/"+action, "{"); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s invalid JSON = %d, want 400", action, rec.Code)
		}
		if rec, _ := do(t, mux, "POST", "/api/v1/moderate/"+action, `{"ids":[]}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s without ids = %d, want 400", action, rec.Code)
		}
	}
	ids := []int64{entries.Entries[0].ID, entries.Entries[1].ID}
	rec, out := do(t, mux, "POST", "/api/v1/moderate/reject", map[string]interface{}{"ids": ids})
	if rec.Code != http.StatusOK || out["rejected_count"] != float64(2) {
		t.Fatalf("reject = %d %v", rec.Code, out)
	}
	_, out = do(t, mux, "GET", "/api/v1/moderate/entries?status=rejected", nil)
	if out["count"] != float64(2) {
		t.Fatalf("rejected entries = %v, want 2", out["count"])
	}
	rec, out = do(t, mux, "DELETE", "/api/v1/moderate/clear?status=rejected", nil)
	if rec.Code != http.StatusOK || out["cleared_count"] != float64(2) {
		t.Fatalf("clear rejected = %d %v", rec.Code, out)
	}
	rec, out = do(t, mux, "DELETE", "/api/v1/moderate/clear", nil)
	if rec.Code != http.StatusOK || out["cleared_count"] != float64(2) {
		t.Fatalf("clear all = %d %v, want the 2 remaining pending entries", rec.Code, out)
	}
}
