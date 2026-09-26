package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// putRouteBody is the PUT /api/v1/routes body the CLI sends.
type putRouteBody struct {
	ID          string            `json:"id"`
	Description string            `json:"description"`
	Strategy    string            `json:"strategy"`
	Targets     []routeTargetSpec `json:"targets"`
}

// fakeRouteGateway is an in-memory stand-in for the admin routes API. It validates requests the
// way the gateway does for the cases the tests need and records every request it receives.
type fakeRouteGateway struct {
	t         *testing.T
	mu        sync.Mutex
	routes    []routeInfo
	providers map[string]bool
	requests  []string
	lastPut   putRouteBody
}

func newFakeRouteGateway(t *testing.T) (*fakeRouteGateway, *httptest.Server) {
	g := &fakeRouteGateway{
		t: t,
		routes: []routeInfo{
			{ID: "auto-resilient", Description: "Balanced", Strategy: "fallback", Targets: []string{"groq/llama"}, BuiltIn: true},
			{ID: "free-first", Description: "Zero cost", Strategy: "free_first", Targets: []string{"groq/qwen"}, BuiltIn: true, Customized: true},
			{ID: "my-route", Description: "Mine", Strategy: "least_cost", Targets: []string{"groq/llama"}},
		},
		providers: map[string]bool{"groq": true, "openrouter": true},
	}
	srv := httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(srv.Close)
	return g, srv
}

func (g *fakeRouteGateway) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, r.Method+" "+r.URL.EscapedPath())
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/routes":
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"default_strategy": "auto-resilient", "routes": g.routes})

	case r.Method == http.MethodPut && r.URL.Path == "/api/v1/routes":
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			g.t.Errorf("PUT Content-Type = %q, want application/json", ct)
		}
		var body putRouteBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeTestError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		g.lastPut = body
		for _, tg := range body.Targets {
			if !g.providers[tg.Provider] {
				writeTestError(w, http.StatusBadRequest, "route "+body.ID+": unknown provider \""+tg.Provider+"\"")
				return
			}
		}
		saved := routeInfo{ID: body.ID, Description: body.Description, Strategy: body.Strategy}
		if saved.Strategy == "" {
			saved.Strategy = "fallback"
		}
		for _, tg := range body.Targets {
			saved.Targets = append(saved.Targets, tg.Provider+"/"+tg.Model)
		}
		for _, existing := range g.routes {
			if existing.ID == body.ID && existing.BuiltIn {
				saved.BuiltIn, saved.Customized = true, true
			}
		}
		_ = json.NewEncoder(w).Encode(saved)

	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/routes/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/routes/")
		for i, existing := range g.routes {
			if existing.ID == id {
				if !existing.BuiltIn {
					g.routes = append(g.routes[:i], g.routes[i+1:]...)
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "success", "message": "route " + id + " reset"})
				return
			}
		}
		writeTestError(w, http.StatusNotFound, "route \""+id+"\" does not exist")

	default:
		http.NotFound(w, r)
	}
}

// writeTestError writes the admin API error envelope.
func writeTestError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func TestParseRouteTarget(t *testing.T) {
	cases := []struct {
		in, provider, model string
	}{
		{"groq/llama-3.3-70b-versatile", "groq", "llama-3.3-70b-versatile"},
		{"openrouter/deepseek/deepseek-v4-flash-0731:free", "openrouter", "deepseek/deepseek-v4-flash-0731:free"},
		{"  nvidia/meta/llama-3.1-8b-instruct  ", "nvidia", "meta/llama-3.1-8b-instruct"},
	}
	for _, c := range cases {
		got, err := parseRouteTarget(c.in)
		if err != nil {
			t.Errorf("parseRouteTarget(%q): %v", c.in, err)
			continue
		}
		if got.Provider != c.provider || got.Model != c.model {
			t.Errorf("parseRouteTarget(%q) = %+v, want %s + %s", c.in, got, c.provider, c.model)
		}
	}
	for _, bad := range []string{"groq", "", "/model", "groq/", " / "} {
		if _, err := parseRouteTarget(bad); err == nil || !strings.Contains(err.Error(), "expected provider/model") {
			t.Errorf("parseRouteTarget(%q) err = %v, want expected provider/model", bad, err)
		}
	}
}

func TestRouteSetCreatesCustomRoute(t *testing.T) {
	g, srv := newFakeRouteGateway(t)
	env := newTestEnv(t, "")

	out, err := env.run(t, "route", "set", "cheap",
		"--target", "openrouter/deepseek/deepseek-v4-flash-0731:free",
		"--target", "groq/llama-3.3-70b-versatile",
		"--strategy", "least_cost",
		"--description", "Cheapest first",
		"--gateway-url", srv.URL+"/")
	if err != nil {
		t.Fatalf("route set: %v", err)
	}
	want := putRouteBody{
		ID: "cheap", Description: "Cheapest first", Strategy: "least_cost",
		Targets: []routeTargetSpec{
			{Provider: "openrouter", Model: "deepseek/deepseek-v4-flash-0731:free"},
			{Provider: "groq", Model: "llama-3.3-70b-versatile"},
		},
	}
	if g.lastPut.ID != want.ID || g.lastPut.Description != want.Description || g.lastPut.Strategy != want.Strategy ||
		len(g.lastPut.Targets) != 2 || g.lastPut.Targets[0] != want.Targets[0] || g.lastPut.Targets[1] != want.Targets[1] {
		t.Errorf("PUT body = %+v, want %+v", g.lastPut, want)
	}
	if len(g.requests) != 1 || g.requests[0] != "PUT /api/v1/routes" {
		t.Errorf("requests = %v, want one PUT /api/v1/routes", g.requests)
	}
	assertContains(t, out,
		"Saved route cheap:",
		"[cheap] Cheapest first  (custom)",
		"Strategy: least_cost",
		"Targets: openrouter/deepseek/deepseek-v4-flash-0731:free -> groq/llama-3.3-70b-versatile",
	)
}

func TestRouteSetStrategyPassthrough(t *testing.T) {
	g, srv := newFakeRouteGateway(t)
	env := newTestEnv(t, "")

	for _, strategy := range []string{"fallback", "free_first", "round_robin", "least_cost"} {
		out, err := env.run(t, "route", "set", "r1", "--target", "groq/llama", "--strategy", strategy, "--gateway-url", srv.URL)
		if err != nil {
			t.Fatalf("route set --strategy %s: %v", strategy, err)
		}
		if g.lastPut.Strategy != strategy {
			t.Errorf("sent strategy %q, want %q", g.lastPut.Strategy, strategy)
		}
		assertContains(t, out, "Strategy: "+strategy)
	}

	// Without --strategy the field is sent empty and the gateway applies fallback.
	out, err := env.run(t, "route", "set", "r1", "--target", "groq/llama", "--gateway-url", srv.URL)
	if err != nil {
		t.Fatalf("route set without strategy: %v", err)
	}
	if g.lastPut.Strategy != "" || g.lastPut.Description != "" {
		t.Errorf("default request = %+v, want empty strategy and description", g.lastPut)
	}
	assertContains(t, out, "Strategy: fallback")

	// Overriding a built-in route reports it as edited.
	out, err = env.run(t, "route", "set", "auto-resilient", "--target", "groq/llama", "--gateway-url", srv.URL)
	if err != nil {
		t.Fatalf("route set built-in: %v", err)
	}
	assertContains(t, out, "[auto-resilient]", "(edited)")
}

func TestRouteSetValidation(t *testing.T) {
	g, srv := newFakeRouteGateway(t)
	env := newTestEnv(t, "")

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no targets", []string{"route", "set", "r1"}, "at least one --target"},
		{"target without slash", []string{"route", "set", "r1", "--target", "groq/llama", "--target", "llama"}, `invalid target "llama"`},
		{"unknown strategy", []string{"route", "set", "r1", "--target", "groq/llama", "--strategy", "cheapest"}, `invalid strategy "cheapest"`},
		{"blank id", []string{"route", "set", "  ", "--target", "groq/llama"}, "route id must not be empty"},
	}
	for _, c := range cases {
		_, err := env.run(t, append(c.args, "--gateway-url", srv.URL)...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	if _, err := env.run(t, "route", "set", "--target", "groq/llama", "--gateway-url", srv.URL); err == nil {
		t.Error("set without an id should fail argument validation")
	}
	if len(g.requests) != 0 {
		t.Errorf("client-side validation failures reached the gateway: %v", g.requests)
	}
}

func TestRouteSetSurfacesAPIErrors(t *testing.T) {
	g, srv := newFakeRouteGateway(t)
	env := newTestEnv(t, "")

	_, err := env.run(t, "route", "set", "r1", "--target", "nosuch/model", "--gateway-url", srv.URL)
	if err == nil || !strings.Contains(err.Error(), `HTTP 400): route r1: unknown provider "nosuch"`) {
		t.Errorf("unknown provider: err = %v", err)
	}
	if len(g.requests) != 1 {
		t.Errorf("requests = %v, want one PUT", g.requests)
	}

	// The nested {"error": {"message": ...}} form and bodies without a message are handled too.
	nested := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error": {"message": "route id \"bad id\" is invalid"}}`))
	}))
	defer nested.Close()
	if _, err := env.run(t, "route", "set", "r1", "--target", "groq/llama", "--gateway-url", nested.URL); err == nil || !strings.Contains(err.Error(), `route id "bad id" is invalid`) {
		t.Errorf("nested error: err = %v", err)
	}

	bare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("down"))
	}))
	defer bare.Close()
	if _, err := env.run(t, "route", "set", "r1", "--target", "groq/llama", "--gateway-url", bare.URL); err == nil || !strings.Contains(err.Error(), "gateway returned HTTP 503") {
		t.Errorf("bare error: err = %v", err)
	}

	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer junk.Close()
	if _, err := env.run(t, "route", "set", "r1", "--target", "groq/llama", "--gateway-url", junk.URL); err == nil || !strings.Contains(err.Error(), "failed to parse saved route") {
		t.Errorf("junk response: err = %v", err)
	}
}

func TestRouteReset(t *testing.T) {
	g, srv := newFakeRouteGateway(t)
	env := newTestEnv(t, "")

	cases := []struct {
		id, want string
	}{
		{"free-first", "Restored built-in route free-first to its shipped definition"},
		{"auto-resilient", "Built-in route auto-resilient was not edited"},
		{"my-route", "Deleted custom route my-route"},
	}
	for _, c := range cases {
		g.requests = nil
		out, err := env.run(t, "route", "reset", c.id, "--gateway-url", srv.URL)
		if err != nil {
			t.Fatalf("reset %s: %v", c.id, err)
		}
		assertContains(t, out, c.want)
		if len(g.requests) != 2 || g.requests[0] != "GET /api/v1/routes" || g.requests[1] != "DELETE /api/v1/routes/"+c.id {
			t.Errorf("reset %s requests = %v", c.id, g.requests)
		}
	}

	// The custom route is gone now, so a second reset is a 404 with the gateway's message.
	_, err := env.run(t, "route", "reset", "my-route", "--gateway-url", srv.URL)
	if err == nil || !strings.Contains(err.Error(), `HTTP 404): route "my-route" does not exist`) {
		t.Errorf("reset missing: err = %v", err)
	}

	// IDs are path-escaped.
	g.requests = nil
	_, _ = env.run(t, "route", "reset", "a b", "--gateway-url", srv.URL)
	if len(g.requests) != 2 || g.requests[1] != "DELETE /api/v1/routes/a%20b" {
		t.Errorf("escaped reset requests = %v", g.requests)
	}

	if _, err := env.run(t, "route", "reset", " ", "--gateway-url", srv.URL); err == nil || !strings.Contains(err.Error(), "route id must not be empty") {
		t.Errorf("blank id: err = %v", err)
	}
}

func TestRouteResetUnlistedRoute(t *testing.T) {
	// A route missing from the list but accepted by DELETE gets a neutral message.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"routes": []}`))
			return
		}
		_, _ = w.Write([]byte(`{"status": "success"}`))
	}))
	defer srv.Close()
	env := newTestEnv(t, "")
	out, err := env.run(t, "route", "reset", "ghost", "--gateway-url", srv.URL)
	if err != nil {
		t.Fatalf("reset ghost: %v", err)
	}
	assertContains(t, out, "Reset route ghost")
}

func TestRouteResetListErrors(t *testing.T) {
	env := newTestEnv(t, "")

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestError(w, http.StatusServiceUnavailable, "router is not available")
	}))
	defer failing.Close()
	if _, err := env.run(t, "route", "reset", "x", "--gateway-url", failing.URL); err == nil || !strings.Contains(err.Error(), "router is not available") {
		t.Errorf("list 503: err = %v", err)
	}

	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer junk.Close()
	if _, err := env.run(t, "route", "reset", "x", "--gateway-url", junk.URL); err == nil || !strings.Contains(err.Error(), "failed to parse routes response") {
		t.Errorf("list junk: err = %v", err)
	}

	// The gateway going away between the lookup and the DELETE is reported as offline.
	listOnly := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("response writer is not a hijacker")
			}
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{"routes": []}`))
	}))
	defer listOnly.Close()
	if _, err := env.run(t, "route", "reset", "x", "--gateway-url", listOnly.URL); err == nil || !strings.Contains(err.Error(), "route editing requires a running gateway") {
		t.Errorf("delete connection drop: err = %v", err)
	}
}

func TestRouteEditGatewayOffline(t *testing.T) {
	env := newTestEnv(t, "")
	offline := closedServerURL(t)

	for _, args := range [][]string{
		{"route", "set", "r1", "--target", "groq/llama"},
		{"route", "reset", "r1"},
	} {
		_, err := env.run(t, append(args, "--gateway-url", offline)...)
		if err == nil || !strings.Contains(err.Error(), "route editing requires a running gateway") || !strings.Contains(err.Error(), "liltok start") {
			t.Errorf("%v offline: err = %v", args, err)
		}
	}
}
