package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/primaybr/liltok/internal/config"
)

// closedServerURL returns the URL of a server that has already shut down, so connecting fails fast.
func closedServerURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u
}

func TestRouteStatusOnline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/routes" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{
			"default_strategy": "free-first",
			"routes": [
				{"id": "auto-resilient", "description": "Balanced", "targets": ["a", "b"]},
				{"id": "free-first", "description": "Zero cost", "targets": ["groq", "gemini"]},
				{"id": "premium-only", "description": "Paid", "strategy": "fallback", "targets": ["openai/gpt"], "built_in": true, "customized": true},
				{"id": "cheap", "description": "Mine", "strategy": "least_cost", "targets": ["groq/llama"], "built_in": false, "customized": false}
			],
			"circuit_breakers": [{"provider": "groq", "state": "closed"}]
		}`))
	}))
	defer srv.Close()
	env := newTestEnv(t, "")

	out, err := env.run(t, "route", "status", "--gateway-url", srv.URL+"/")
	if err != nil {
		t.Fatalf("route status: %v", err)
	}
	assertContains(t, out,
		"Active Routing Strategy: FREE-FIRST",
		"  [auto-resilient] Balanced\n",
		"* [free-first] Zero cost",
		"Targets: groq -> gemini",
		"Strategy: fallback",
		"  [premium-only] Paid  (edited)\n    Strategy: fallback\n",
		"  [cheap] Mine  (custom)\n    Strategy: least_cost\n",
		"Circuit Breakers:",
		"groq",
		"closed",
	)
}

func TestRouteStatusErrors(t *testing.T) {
	env := newTestEnv(t, "routes:\n  default_strategy: 'premium-only'\n")
	offline := closedServerURL(t)

	// Offline gateway falls back to the strategy in the config file.
	out, err := env.run(t, "route", "status", "--gateway-url", offline)
	if err != nil {
		t.Fatalf("offline status: %v", err)
	}
	assertContains(t, out, "Liltok Gateway: OFFLINE", "Configured Strategy: premium-only")

	// Offline gateway and an unreadable config is an error.
	_, err = runCLI(t, "--config", filepath.Join(env.home, "missing.yaml"), "route", "status", "--gateway-url", offline)
	if err == nil || !strings.Contains(err.Error(), "failed to connect to gateway") {
		t.Errorf("offline without config: err = %v", err)
	}

	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer junk.Close()
	if _, err := env.run(t, "route", "status", "--gateway-url", junk.URL); err == nil || !strings.Contains(err.Error(), "failed to parse routes response") {
		t.Errorf("junk response: err = %v", err)
	}
}

func TestRouteSwitchOnline(t *testing.T) {
	var gotStrategy, gotMethod, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/routes/strategy" {
			http.NotFound(w, r)
			return
		}
		gotMethod = r.Method
		gotType = r.Header.Get("Content-Type")
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotStrategy = body["strategy"]
	}))
	defer srv.Close()
	env := newTestEnv(t, "")

	out, err := env.run(t, "route", "switch", "  Free-First ", "--gateway-url", srv.URL)
	if err != nil {
		t.Fatalf("route switch: %v", err)
	}
	if gotMethod != http.MethodPost || gotType != "application/json" || gotStrategy != "free-first" {
		t.Errorf("request = %s %s strategy=%q, want POST application/json free-first", gotMethod, gotType, gotStrategy)
	}
	assertContains(t, out, "Successfully switched active routing priority to: FREE-FIRST", "Zero-cost routing active")

	out, err = env.run(t, "route", "switch", "premium-only", "--gateway-url", srv.URL)
	if err != nil {
		t.Fatalf("route switch premium: %v", err)
	}
	if strings.Contains(out, "Zero-cost") {
		t.Errorf("premium-only switch printed the free-first note")
	}
}

func TestRouteSwitchErrors(t *testing.T) {
	env := newTestEnv(t, "routes:\n  default_strategy: 'auto-resilient'\n")

	if _, err := env.run(t, "route", "switch", "cheapest"); err == nil || !strings.Contains(err.Error(), "invalid strategy") {
		t.Errorf("invalid strategy: err = %v", err)
	}
	if _, err := env.run(t, "route", "switch"); err == nil {
		t.Error("switch without a strategy should fail argument validation")
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer failing.Close()
	if _, err := env.run(t, "route", "switch", "free-first", "--gateway-url", failing.URL); err == nil || !strings.Contains(err.Error(), "gateway returned HTTP 400") {
		t.Errorf("gateway 400: err = %v", err)
	}

	// Offline gateway persists the strategy into the config file instead.
	offline := closedServerURL(t)
	out, err := env.run(t, "route", "switch", "free-first", "--gateway-url", offline)
	if err != nil {
		t.Fatalf("offline switch: %v", err)
	}
	assertContains(t, out, "Gateway offline. Updated "+env.cfgPath+` strategy to "free-first"`)
	cfg, err := config.Load(env.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routes.DefaultStrategy != "free-first" {
		t.Errorf("persisted strategy = %q, want free-first", cfg.Routes.DefaultStrategy)
	}
	if filepath.ToSlash(cfg.Storage.DBPath) != env.dbPath {
		t.Errorf("persisting the strategy changed db_path to %q", cfg.Storage.DBPath)
	}

	// Offline gateway and a config that cannot be written is an error.
	_, err = runCLI(t, "--config", filepath.Join(env.home, "no-dir", "missing.yaml"), "route", "switch", "free-first", "--gateway-url", offline)
	if err == nil || !strings.Contains(err.Error(), "failed to update config") {
		t.Errorf("offline with unwritable config: err = %v", err)
	}
}

// TestAdminClientSendsConfiguredToken checks that CLI admin calls carry the admin token once one
// is configured, so they keep working against a gateway that requires it.
func TestAdminClientSendsConfiguredToken(t *testing.T) {
	env := newTestEnv(t, "")
	t.Setenv("LILTOK_ADMIN_TOKEN", "s3cret")
	var got string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Liltok-Admin-Token")
		_, _ = w.Write([]byte(`{"default_strategy":"auto-resilient","routes":[],"circuit_breakers":[]}`))
	}))
	defer gw.Close()
	if _, err := env.run(t, "route", "status", "--gateway-url", gw.URL); err != nil {
		t.Fatalf("route status: %v", err)
	}
	if got != "s3cret" {
		t.Fatalf("admin token header = %q, want s3cret", got)
	}

	// Requests outside the admin API never carry it.
	got = ""
	resp, err := adminHTTPClient(time.Second).Get(gw.URL + "/v1/models")
	if err == nil {
		resp.Body.Close()
	}
	if got != "" {
		t.Fatalf("token leaked to a non-admin path: %q", got)
	}
}
