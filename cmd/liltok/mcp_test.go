package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rpcReply is the decoded form of one JSON-RPC response line written by the MCP server.
type rpcReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *jsonRPCError   `json:"error"`
}

// runMCPSession feeds input lines to the MCP loop through stdin and decodes every reply line.
func runMCPSession(t *testing.T, gatewayURL string, lines ...string) []rpcReply {
	t.Helper()
	var loopErr error
	out := ""
	withStdin(t, strings.Join(lines, "\n")+"\n", func() {
		out = captureStdout(t, func() { loopErr = runMCPLoop(gatewayURL) })
	})
	if loopErr != nil {
		t.Fatalf("runMCPLoop: %v", loopErr)
	}
	return decodeReplies(t, out)
}

func decodeReplies(t *testing.T, out string) []rpcReply {
	t.Helper()
	var replies []rpcReply
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var r rpcReply
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("reply is not JSON: %q: %v", line, err)
		}
		if r.JSONRPC != "2.0" {
			t.Errorf("reply jsonrpc = %q, want 2.0", r.JSONRPC)
		}
		replies = append(replies, r)
	}
	return replies
}

func toolResultOf(t *testing.T, r rpcReply) toolCallResult {
	t.Helper()
	var res toolCallResult
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatalf("result is not a tool result: %s: %v", r.Result, err)
	}
	if len(res.Content) != 1 || res.Content[0].Type != "text" {
		t.Fatalf("tool result content = %+v, want one text block", res.Content)
	}
	return res
}

func TestMCPProtocolMessages(t *testing.T) {
	replies := runMCPSession(t, "http://127.0.0.1:1",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		``,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`this is not json`,
		`{"jsonrpc":"2.0","id":"p","method":"ping"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"no/such/method"}`,
		`{"jsonrpc":"2.0","method":"notifications/unknown"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":"not an object"}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
	)

	// Notifications get no reply; everything else gets exactly one, in order.
	if len(replies) != 7 {
		t.Fatalf("got %d replies, want 7: %+v", len(replies), replies)
	}

	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
		Capabilities map[string]interface{} `json:"capabilities"`
	}
	if err := json.Unmarshal(replies[0].Result, &init); err != nil {
		t.Fatal(err)
	}
	if string(replies[0].ID) != "1" || init.ProtocolVersion != "2024-11-05" || init.ServerInfo.Name != "liltok" {
		t.Errorf("initialize reply = %+v", init)
	}
	if _, ok := init.Capabilities["tools"]; !ok {
		t.Errorf("initialize does not advertise tools capability")
	}

	if replies[1].Error == nil || replies[1].Error.Code != -32700 || string(replies[1].ID) != "" {
		t.Errorf("parse error reply = %+v", replies[1])
	}

	if string(replies[2].ID) != `"p"` || string(replies[2].Result) != "{}" {
		t.Errorf("ping reply = id %s result %s", replies[2].ID, replies[2].Result)
	}

	var list struct {
		Tools []mcpTool `json:"tools"`
	}
	if err := json.Unmarshal(replies[3].Result, &list); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
		if tool.InputSchema["type"] != "object" {
			t.Errorf("tool %s schema type = %v", tool.Name, tool.InputSchema["type"])
		}
	}
	if strings.Join(names, ",") != "liltok_ask,liltok_cache_search,liltok_stats" {
		t.Errorf("tools = %v", names)
	}

	if e := replies[4].Error; e == nil || e.Code != -32601 || !strings.Contains(e.Message, "no/such/method") {
		t.Errorf("unknown method reply = %+v", replies[4])
	}
	if e := replies[5].Error; e == nil || e.Code != -32602 || string(replies[5].ID) != "4" {
		t.Errorf("invalid params reply = %+v", replies[5])
	}
	res := toolResultOf(t, replies[6])
	if !res.IsError || !strings.Contains(res.Content[0].Text, "unknown tool: nope") {
		t.Errorf("unknown tool result = %+v", res)
	}
}

func TestMCPCommandUsesGatewayFlag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/overview" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"status":"healthy","total_requests":1}`))
	}))
	defer srv.Close()

	var out string
	var err error
	withStdin(t, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"liltok_stats","arguments":{}}}`+"\n", func() {
		out, err = runCLI(t, "mcp", "--gateway-url", srv.URL+"/")
	})
	if err != nil {
		t.Fatalf("mcp command: %v", err)
	}
	replies := decodeReplies(t, out)
	if len(replies) != 1 {
		t.Fatalf("replies = %d, want 1", len(replies))
	}
	res := toolResultOf(t, replies[0])
	if res.IsError || !strings.Contains(res.Content[0].Text, "Operational Status: healthy") {
		t.Errorf("stats via mcp command = %+v", res)
	}
}

func TestExecuteToolValidation(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args map[string]interface{}
		want string
	}{
		{"ask without prompt", "liltok_ask", map[string]interface{}{}, "prompt parameter is required"},
		{"ask with blank prompt", "liltok_ask", map[string]interface{}{"prompt": "   "}, "prompt parameter is required"},
		{"search without query", "liltok_cache_search", map[string]interface{}{"query": ""}, "query parameter is required"},
		{"unknown tool", "liltok_nope", nil, "unknown tool: liltok_nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := executeTool(tc.tool, tc.args, "http://127.0.0.1:1")
			if !res.IsError || !strings.HasPrefix(res.Content[0].Text, "Error: ") || !strings.Contains(res.Content[0].Text, tc.want) {
				t.Errorf("result = %+v, want error containing %q", res, tc.want)
			}
		})
	}
}

// chatRecorder is a fake gateway /v1/chat/completions endpoint that records the last request.
type chatRecorder struct {
	body   map[string]interface{}
	auth   string
	xKey   string
	status int
	reply  string
	header map[string]string
}

func (c *chatRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		c.body = nil
		_ = json.NewDecoder(r.Body).Decode(&c.body)
		c.auth = r.Header.Get("Authorization")
		c.xKey = r.Header.Get("x-api-key")
		for k, v := range c.header {
			w.Header().Set(k, v)
		}
		if c.status != 0 {
			w.WriteHeader(c.status)
		}
		_, _ = w.Write([]byte(c.reply))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLiltokAskSendsRequest(t *testing.T) {
	newTestEnv(t, "")
	rec := &chatRecorder{
		reply:  `{"choices":[{"message":{"role":"assistant","content":"forty-two"}}],"usage":{"prompt_tokens":11,"completion_tokens":5}}`,
		header: map[string]string{"X-Cache": "HIT", "X-Cache-Tier": "exact"},
	}
	srv := rec.server(t)

	res := executeTool("liltok_ask", map[string]interface{}{
		"prompt":  "what is the answer?",
		"model":   "gpt-4o",
		"system":  "be brief",
		"api_key": "explicit-key",
	}, srv.URL)
	if res.IsError {
		t.Fatalf("ask failed: %+v", res)
	}
	text := res.Content[0].Text
	assertContains(t, text, "Cache: HIT", "Tier: exact", "Model: gpt-4o", "Tokens: 11/5", "\n\nforty-two")

	if rec.body["model"] != "gpt-4o" || rec.body["stream"] != false {
		t.Errorf("request body = %v", rec.body)
	}
	msgs, _ := rec.body["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("messages = %v, want system and user", msgs)
	}
	first, _ := msgs[0].(map[string]interface{})
	second, _ := msgs[1].(map[string]interface{})
	if first["role"] != "system" || first["content"] != "be brief" || second["role"] != "user" || second["content"] != "what is the answer?" {
		t.Errorf("messages = %v", msgs)
	}
	if rec.auth != "Bearer explicit-key" || rec.xKey != "explicit-key" {
		t.Errorf("auth headers = %q / %q", rec.auth, rec.xKey)
	}
}

func TestLiltokAskKeyFromEnvironment(t *testing.T) {
	cases := []struct {
		model  string
		envVar string
	}{
		{"", "ANTHROPIC_API_KEY"}, // default model is a claude model
		{"llama-3.3-70b", "GROQ_API_KEY"},
		{"groq/compound", "GROQ_API_KEY"},
		{"gemini-2.5-flash", "GEMINI_API_KEY"},
		{"openrouter/free", "OPENROUTER_API_KEY"},
		{"gpt-4o", "OPENAI_API_KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.envVar+"_"+tc.model, func(t *testing.T) {
			newTestEnv(t, "")
			t.Setenv(tc.envVar, "env-"+tc.envVar)
			rec := &chatRecorder{reply: `{"choices":[{"message":{"content":"ok"}}]}`}
			srv := rec.server(t)

			args := map[string]interface{}{"prompt": "hi"}
			if tc.model != "" {
				args["model"] = tc.model
			}
			res := executeTool("liltok_ask", args, srv.URL)
			if res.IsError {
				t.Fatalf("ask failed: %+v", res)
			}
			if rec.auth != "Bearer env-"+tc.envVar {
				t.Errorf("Authorization = %q, want key from %s", rec.auth, tc.envVar)
			}
			if tc.model == "" && rec.body["model"] != "claude-opus-5-5" {
				t.Errorf("default model = %v", rec.body["model"])
			}
			msgs, _ := rec.body["messages"].([]interface{})
			if len(msgs) != 1 {
				t.Errorf("messages without system = %v, want only the user turn", msgs)
			}
		})
	}
}

func TestLiltokAskFallbackKeyAndNoKey(t *testing.T) {
	newTestEnv(t, "")
	rec := &chatRecorder{reply: `{"choices":[{"message":{"content":"ok"}}]}`}
	srv := rec.server(t)

	res := executeTool("liltok_ask", map[string]interface{}{"prompt": "hi", "model": "gpt-4o"}, srv.URL)
	if res.IsError || rec.auth != "" || rec.xKey != "" {
		t.Errorf("no key configured: res=%+v auth=%q x-api-key=%q, want no auth headers", res, rec.auth, rec.xKey)
	}

	t.Setenv("LILTOK_API_KEY", "virtual-key")
	res = executeTool("liltok_ask", map[string]interface{}{"prompt": "hi", "model": "gpt-4o"}, srv.URL)
	if res.IsError || rec.auth != "Bearer virtual-key" {
		t.Errorf("LILTOK_API_KEY fallback: res=%+v auth=%q", res, rec.auth)
	}
}

func TestLiltokAskResponses(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		reply   string
		wantErr bool
		want    []string
	}{
		{"unauthorized", http.StatusUnauthorized, `{}`, true, []string{"Cache MISS and no upstream API key provided"}},
		{"server error", http.StatusBadGateway, `upstream down`, true, []string{"gateway returned HTTP 502: upstream down"}},
		{"non json body", 0, `plain text answer`, false, []string{"plain text answer"}},
		{"data envelope", 0, `{"data":{"choices":[{"message":{"content":"wrapped"}}],"usage":{"prompt_tokens":2,"completion_tokens":3}}}`, false, []string{"Tokens: 2/3", "wrapped"}},
		{"no choices", 0, `{"choices":[]}`, false, []string{"[Liltok Gateway | Cache: "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newTestEnv(t, "")
			rec := &chatRecorder{status: tc.status, reply: tc.reply}
			srv := rec.server(t)
			res := callLiltokChat(srv.URL, "gpt-4o", "", "hi", "k")
			if res.IsError != tc.wantErr {
				t.Fatalf("IsError = %v, want %v: %+v", res.IsError, tc.wantErr, res)
			}
			assertContains(t, res.Content[0].Text, tc.want...)
		})
	}
}

func TestLiltokAskGatewayOffline(t *testing.T) {
	newTestEnv(t, "")
	offline := closedServerURL(t)
	res := callLiltokChat(offline, "gpt-4o", "", "hi", "k")
	if !res.IsError || !strings.Contains(res.Content[0].Text, "gateway connection failed (is liltok running at "+offline) {
		t.Errorf("offline result = %+v", res)
	}
}

func TestLiltokStats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/overview" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"status":"healthy","uptime_seconds":90,"total_requests":10,"total_hits":4,
			"local_hits":3,"model_cache_hits":1,"hit_rate_percent":40,"total_cost_usd":1.5,
			"total_saved_usd":2.25,"avg_latency_ms":12.34,"cache_entries":77}`))
	}))
	defer srv.Close()

	res := executeTool("liltok_stats", nil, srv.URL)
	if res.IsError {
		t.Fatalf("stats failed: %+v", res)
	}
	assertContains(t, res.Content[0].Text,
		"Operational Status: healthy",
		"Uptime: 90s",
		"Total Requests Processed: 10",
		"Cache Optimization Rate: 40.0%",
		"Local Exact/Semantic Hits (0ms latency, $0.00 cost): 3",
		"Upstream Model KV-Cache Hits (50%-90% discount): 1",
		"Uncached Full Misses: 6",
		"Total Dollars Saved: $2.2500",
		"Gross Upstream Spend: $1.5000",
		"Average Latency: 12.3ms",
		"Active Local Cache Entries: 77",
	)
}

func TestLiltokStatsErrors(t *testing.T) {
	// More hits than requests must not report negative misses.
	skewed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"total_requests":1,"total_hits":5}`))
	}))
	defer skewed.Close()
	if res := getLiltokStats(skewed.URL); res.IsError || !strings.Contains(res.Content[0].Text, "Uncached Full Misses: 0") {
		t.Errorf("skewed stats = %+v", res)
	}

	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>"))
	}))
	defer junk.Close()
	if res := getLiltokStats(junk.URL); !res.IsError || !strings.Contains(res.Content[0].Text, "failed to parse stats response") {
		t.Errorf("junk stats = %+v", res)
	}

	offline := closedServerURL(t)
	if res := getLiltokStats(offline); !res.IsError || !strings.Contains(res.Content[0].Text, "failed to connect to Liltok at "+offline) {
		t.Errorf("offline stats = %+v", res)
	}
}

func TestLiltokCacheSearchViaGateway(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/cache" {
			http.NotFound(w, r)
			return
		}
		gotQuery = r.URL.Query().Get("q")
		var items []map[string]interface{}
		for i := 0; i < 7; i++ {
			items = append(items, map[string]interface{}{
				"hash":           fmt.Sprintf("%02dabcdefabcdefabcdef", i),
				"model":          "gpt-4o",
				"prompt_preview": fmt.Sprintf("preview %d", i),
				"hit_count":      i,
			})
		}
		items = append(items, map[string]interface{}{"hash": "tiny", "model": "m", "prompt_preview": "p"})
		_ = json.NewEncoder(w).Encode(items)
	}))
	defer srv.Close()

	res := executeTool("liltok_cache_search", map[string]interface{}{"query": "go & rust"}, srv.URL)
	if res.IsError {
		t.Fatalf("search failed: %+v", res)
	}
	if gotQuery != "go & rust" {
		t.Errorf("query sent = %q, want it URL-escaped and round-tripped", gotQuery)
	}
	text := res.Content[0].Text
	assertContains(t, text,
		"Search results for 'go & rust' in Liltok cache:",
		"### Match 1: Hash 00abcdefabcd (Hits: 0 | Model: gpt-4o)",
		"**Prompt Preview:** preview 4",
	)
	// Results are capped at five.
	if strings.Contains(text, "Match 6") || strings.Contains(text, "preview 5") {
		t.Errorf("more than five matches returned:\n%s", text)
	}
}

func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(s))
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestLiltokCacheSearchLocalFallback covers the direct SQLite search used when the gateway has
// no results or is offline, including gzip, OpenAI and Anthropic shaped payloads.
func TestLiltokCacheSearchLocalFallback(t *testing.T) {
	env := newTestEnv(t, "")
	configPath = env.cfgPath
	t.Cleanup(func() { configPath = "" })

	database := env.openDB(t)
	longAnswer := strings.Repeat("a", 400)
	rows := []struct {
		hash, prompt string
		payload      []byte
		hits         int
	}{
		{"openai00000000hash", "needle openai", []byte(`{"choices":[{"message":{"content":"openai answer"}}]}`), 9},
		{"anthropic000000hash", "needle anthropic", []byte(`{"content":[{"text":"anthropic answer"}]}`), 8},
		{"gzipped00000000hash", "needle gzip " + strings.Repeat("z", 250), gzipBytes(t, `{"choices":[{"message":{"content":"`+longAnswer+`"}}]}`), 7},
		{"rawtext00000000hash", "needle raw", []byte("not json at all"), 6},
		{"unrelated000000hash", "haystack only", []byte("{}"), 100},
	}
	for _, r := range rows {
		if _, err := database.Exec(`INSERT INTO cache_entries (hash, model, normalized_prompt, response_payload, hit_count) VALUES (?, 'gpt-4o', ?, ?, ?)`,
			r.hash, r.prompt, r.payload, r.hits); err != nil {
			t.Fatal(err)
		}
	}
	database.Close()

	// An empty answer from a running gateway is final: no fallback scan of the database it holds.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[]"))
	}))
	defer empty.Close()
	if res := searchLiltokCache(empty.URL, "needle"); res.IsError || !strings.Contains(res.Content[0].Text, "No cached entries match 'needle'.") {
		t.Fatalf("empty gateway answer = %+v, want a no-match result", res)
	}

	// With the gateway offline, the search reads SQLite. This database has no search index yet (the
	// gateway builds it), so the prompts themselves are scanned.
	offline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	offlineURL := offline.URL
	offline.Close()

	res := searchLiltokCache(offlineURL, "needle")
	if res.IsError {
		t.Fatalf("local search failed: %+v", res)
	}
	text := res.Content[0].Text
	assertContains(t, text,
		"Search results for 'needle' in local cache:",
		"### Match 1: Hash openai000000 (Hits: 9 | Model: gpt-4o)",
		"**Response Preview:** openai answer",
		"**Response Preview:** anthropic answer",
		"**Response Preview:** "+strings.Repeat("a", 300)+"...",
		"**Response Preview:** not json at all",
		"needle gzip "+strings.Repeat("z", 188)+"...",
	)
	if strings.Contains(text, "haystack") {
		t.Errorf("non-matching entry returned:\n%s", text)
	}

	// An offline gateway also falls back, and a query with no matches says so.
	res = searchLiltokCache(closedServerURL(t), "no-such-needle")
	if res.IsError || !strings.Contains(res.Content[0].Text, "No cache entries found matching 'no-such-needle' in "+env.dbPath) {
		t.Errorf("no-match result = %+v", res)
	}
}

// TestLiltokCacheSearchLocalShortHash guards the local fallback against cache rows whose hash is
// shorter than the 12-character preview (for example rows imported from a hand-built pack).
func TestLiltokCacheSearchLocalShortHash(t *testing.T) {
	env := newTestEnv(t, "")
	configPath = env.cfgPath
	t.Cleanup(func() { configPath = "" })
	env.insertEntry(t, "short", "gpt-4o", "needle short", `{}`, 1, false)

	res := searchLiltokCache("", "needle")
	if res.IsError || !strings.Contains(res.Content[0].Text, "### Match 1: Hash short (Hits: 1 | Model: gpt-4o)") {
		t.Errorf("short hash result = %+v", res)
	}
}

func TestLiltokCacheSearchDatabaseError(t *testing.T) {
	env := newTestEnv(t, "")
	// db_path below a regular file makes db.Open fail to create the directory.
	blocker := filepath.Join(env.home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(env.home, "bad.yaml")
	yaml := "storage:\n  db_path: '" + filepath.ToSlash(filepath.Join(blocker, "sub", "liltok.db")) + "'\n"
	if err := os.WriteFile(cfg, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	configPath = cfg
	t.Cleanup(func() { configPath = "" })

	res := searchLiltokCache("", "needle")
	if !res.IsError || !strings.Contains(res.Content[0].Text, "failed to open cache db") {
		t.Errorf("db error result = %+v", res)
	}
}

// TestLiltokCacheSearchLocalIndexed checks the offline search through the cache_search index: every
// query word must match the indexed user text, and an indexed database with no match says so
// instead of scanning prompts.
func TestLiltokCacheSearchLocalIndexed(t *testing.T) {
	env := newTestEnv(t, "")
	configPath = env.cfgPath
	t.Cleanup(func() { configPath = "" })

	database := env.openDB(t)
	for _, r := range []struct{ hash, question string }{
		{"indexedhandler0001", "How do I add a timeout to a Go HTTP handler?"},
		{"indexedsql00000002", "Explain SQL window functions"},
	} {
		prompt := `{"messages":[{"role":"user","content":"` + r.question + `"}]}`
		if _, err := database.Exec(`INSERT INTO cache_entries (hash, model, normalized_prompt, response_payload, hit_count) VALUES (?, 'gpt-4o', ?, ?, 1)`,
			r.hash, prompt, []byte(`{"choices":[{"message":{"content":"answer for `+r.hash+`"}}]}`)); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := database.IndexCacheSearch(context.Background(), 10); err != nil || n != 2 {
		t.Fatalf("indexed %d (%v), want 2", n, err)
	}
	database.Close()

	offline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	offlineURL := offline.URL
	offline.Close()

	res := searchLiltokCache(offlineURL, "go timeout handler")
	if res.IsError {
		t.Fatalf("search failed: %+v", res)
	}
	assertContains(t, res.Content[0].Text, "indexedhand", "answer for indexedhandler0001")
	if strings.Contains(res.Content[0].Text, "indexedsql") {
		t.Errorf("an entry missing a query word was returned:\n%s", res.Content[0].Text)
	}
	if res := searchLiltokCache(offlineURL, "kubernetes operator"); !strings.Contains(res.Content[0].Text, "No cached entries match 'kubernetes operator' in the local cache.") {
		t.Errorf("no-match result = %+v", res)
	}
}
