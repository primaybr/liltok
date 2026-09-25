package miner_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/miner"
)

const okCompletion = `{"choices":[{"message":{"role":"assistant","content":"mined answer"}}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	database, err := db.Open(filepath.Join(t.TempDir(), "miner.db"))
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func insertEntry(t *testing.T, database *db.DB, hash, model, prompt, payload string, hits int, semantic bool) {
	t.Helper()
	sem := 0
	if semantic {
		sem = 1
	}
	_, err := database.Exec(`
		INSERT INTO cache_entries (
			hash, model, normalized_prompt, response_payload,
			prompt_tokens, completion_tokens, hit_count,
			created_at, last_accessed_at, ttl_seconds, is_pinned, is_semantic
		) VALUES (?, ?, ?, ?, 5, 6, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 3600, 0, ?)`,
		hash, model, prompt, []byte(payload), hits, sem)
	if err != nil {
		t.Fatalf("failed to insert entry %s: %v", hash, err)
	}
}

func decodeGz(t *testing.T, data []byte) []miner.CacheExportItem {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("output is not gzip: %v", err)
	}
	defer zr.Close()
	var items []miner.CacheExportItem
	if err := json.NewDecoder(zr).Decode(&items); err != nil {
		t.Fatalf("output is not a JSON item list: %v", err)
	}
	return items
}

func encodeGz(t *testing.T, v interface{}) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(v); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func TestSanitizeContent(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"openai style key", "key sk-abcdefghijklmnopqrstuvwxyz here", "key [REDACTED_API_KEY] here"},
		{"groq key", "gsk_abcdefghijklmnopqrstuvwxyz", "[REDACTED_API_KEY]"},
		{"nvidia key", "nvapi-abcdefghijklmnopqrstuvwxyz", "[REDACTED_API_KEY]"},
		{"bearer token", "Authorization: Bearer abcdefghijklmnopqrstuvwxyz.123", "Authorization: [REDACTED_API_KEY]"},
		{"short key untouched", "sk-short", "sk-short"},
		{"windows home path", `open X:\Users\example\notes.txt`, `open /home/dev\notes.txt`},
		{"mac home path", "/Users/example/src/app", "/home/dev/src/app"},
		{"linux home path", "/home/example/.bashrc", "/home/dev/.bashrc"},
		{"private ip 192.168", "host 192.168.1.20 up", "host 127.0.0.1 up"},
		{"private ip 10", "10.0.0.7", "127.0.0.1"},
		{"private ip 172.16-31", "172.20.3.4 and 172.40.3.4", "127.0.0.1 and 172.40.3.4"},
		{"public ip untouched", "8.8.8.8", "8.8.8.8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := miner.SanitizeContent(tt.in); got != tt.want {
				t.Errorf("SanitizeContent(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestExportCacheWithOptions_Filters(t *testing.T) {
	database := openTestDB(t)
	insertEntry(t, database, "h-cold", "gpt-4o", "cold prompt", "cold", 0, false)
	insertEntry(t, database, "h-hot-gpt", "gpt-4o", "hot prompt at 192.168.0.9", "hot sk-abcdefghijklmnopqrstuvwxyz", 5, true)
	insertEntry(t, database, "h-hot-claude", "claude-sonnet-5", "claude prompt", "claude", 9, false)

	tests := []struct {
		name       string
		opts       miner.ExportOptions
		wantHashes []string
	}{
		{"no filter", miner.ExportOptions{}, []string{"h-cold", "h-hot-claude", "h-hot-gpt"}},
		{"min hits", miner.ExportOptions{MinHits: 5}, []string{"h-hot-claude", "h-hot-gpt"}},
		{"model and min hits", miner.ExportOptions{MinHits: 1, Model: "gpt-4o"}, []string{"h-hot-gpt"}},
		{"no match", miner.ExportOptions{Model: "absent"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			n, err := miner.ExportCacheWithOptions(database, &buf, tt.opts)
			if err != nil {
				t.Fatalf("export failed: %v", err)
			}
			items := decodeGz(t, buf.Bytes())
			if n != len(items) || n != len(tt.wantHashes) {
				t.Fatalf("count=%d decoded=%d, want %d", n, len(items), len(tt.wantHashes))
			}
			var got []string
			for _, it := range items {
				got = append(got, it.Hash)
			}
			sort.Strings(got)
			for i := range got {
				if got[i] != tt.wantHashes[i] {
					t.Errorf("hashes = %v, want %v", got, tt.wantHashes)
					break
				}
			}
		})
	}

	t.Run("sanitize and field mapping", func(t *testing.T) {
		var buf bytes.Buffer
		if _, err := miner.ExportCacheWithOptions(database, &buf, miner.ExportOptions{Model: "gpt-4o", MinHits: 1, Sanitize: true}); err != nil {
			t.Fatalf("export failed: %v", err)
		}
		items := decodeGz(t, buf.Bytes())
		if len(items) != 1 {
			t.Fatalf("expected 1 item, got %d", len(items))
		}
		it := items[0]
		if it.NormalizedPrompt != "hot prompt at 127.0.0.1" {
			t.Errorf("prompt not sanitized: %q", it.NormalizedPrompt)
		}
		if it.ResponsePayload != "hot [REDACTED_API_KEY]" {
			t.Errorf("payload not sanitized: %q", it.ResponsePayload)
		}
		if !it.IsSemantic || it.PromptTokens != 5 || it.CompletionTokens != 6 || it.TTLSeconds != 3600 {
			t.Errorf("fields not mapped: %+v", it)
		}
	})
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestExportCacheWithOptions_Errors(t *testing.T) {
	database := openTestDB(t)
	insertEntry(t, database, "h1", "gpt-4o", "p", "r", 0, false)

	if _, err := miner.ExportCacheWithOptions(database, failingWriter{}, miner.ExportOptions{}); err == nil {
		t.Error("expected error when writer fails")
	}

	_ = database.Close()
	if _, err := miner.ExportCacheToGz(database, io.Discard); err == nil || !strings.Contains(err.Error(), "failed to query") {
		t.Errorf("expected query error on closed db, got %v", err)
	}
}

func TestImportCacheFromGz_Errors(t *testing.T) {
	database := openTestDB(t)

	if _, err := miner.ImportCacheFromGz(database, strings.NewReader("not gzip")); err == nil || !strings.Contains(err.Error(), "gzip reader") {
		t.Errorf("expected gzip error, got %v", err)
	}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte("{not a list"))
	_ = zw.Close()
	if _, err := miner.ImportCacheFromGz(database, &buf); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("expected decode error, got %v", err)
	}

	n, err := miner.ImportCacheFromGz(database, bytes.NewReader(encodeGz(t, []miner.CacheExportItem{})))
	if err != nil || n != 0 {
		t.Errorf("empty list: n=%d err=%v, want 0 and nil", n, err)
	}

	items := []miner.CacheExportItem{{Hash: "h", Model: "m", NormalizedPrompt: "p", ResponsePayload: "r"}}
	_ = database.Close()
	if _, err := miner.ImportCacheFromGz(database, bytes.NewReader(encodeGz(t, items))); err == nil {
		t.Error("expected error importing into closed db")
	}
}

func TestImportCacheFromGz_PreservesFields(t *testing.T) {
	database := openTestDB(t)
	items := []miner.CacheExportItem{
		{Hash: "sem", Model: "gpt-4o", NormalizedPrompt: "p1", ResponsePayload: "r1", PromptTokens: 11, CompletionTokens: 12, TTLSeconds: 99, IsSemantic: true},
		{Hash: "exact", Model: "claude-sonnet-5", NormalizedPrompt: "p2", ResponsePayload: "r2"},
	}
	n, err := miner.ImportCacheFromGz(database, bytes.NewReader(encodeGz(t, items)))
	if err != nil || n != 2 {
		t.Fatalf("import n=%d err=%v, want 2 and nil", n, err)
	}

	var model, payload string
	var pTok, cTok, ttl, sem, pinned int
	err = database.QueryRow("SELECT model, response_payload, prompt_tokens, completion_tokens, ttl_seconds, is_semantic, is_pinned FROM cache_entries WHERE hash='sem'").
		Scan(&model, &payload, &pTok, &cTok, &ttl, &sem, &pinned)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if model != "gpt-4o" || payload != "r1" || pTok != 11 || cTok != 12 || ttl != 99 || sem != 1 || pinned != 1 {
		t.Errorf("unexpected row: model=%s payload=%s p=%d c=%d ttl=%d sem=%d pinned=%d", model, payload, pTok, cTok, ttl, sem, pinned)
	}
}

func TestPackStarterCache_MergesExistingArchive(t *testing.T) {
	database := openTestDB(t)
	insertEntry(t, database, "shared", "gpt-4o", "from db", "db payload", 3, false)
	insertEntry(t, database, "db-only", "gpt-4o", "db only", "db", 3, false)
	insertEntry(t, database, "cold", "gpt-4o", "cold", "cold", 0, false)
	insertEntry(t, database, "huge", "gpt-4o", strings.Repeat("x", 200), "big", 3, false)

	target := filepath.Join(t.TempDir(), "nested", "dir", "starter.json.gz")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	existing := []miner.CacheExportItem{
		{Hash: "shared", Model: "gpt-4o", NormalizedPrompt: "old", ResponsePayload: "old"},
		{Hash: "archive-only", Model: "gpt-4o", NormalizedPrompt: "a", ResponsePayload: "a"},
	}
	if err := os.WriteFile(target, encodeGz(t, existing), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := miner.PackStarterCache(database, target, miner.PackOptions{MinHits: 1, MaxPromptBytes: 100})
	if err != nil {
		t.Fatalf("pack failed: %v", err)
	}
	if res.ExistingEntries != 2 || res.MergedFromDB != 1 || res.TotalEntries != 3 || res.TargetPath != target {
		t.Errorf("unexpected result: %+v", res)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if res.SizeBytes != len(data) {
		t.Errorf("size = %d, file has %d bytes", res.SizeBytes, len(data))
	}
	byHash := map[string]miner.CacheExportItem{}
	for _, it := range decodeGz(t, data) {
		byHash[it.Hash] = it
	}
	if _, ok := byHash["huge"]; ok {
		t.Error("prompt larger than MaxPromptBytes was packed")
	}
	if _, ok := byHash["cold"]; ok {
		t.Error("entry below MinHits was packed")
	}
	if byHash["shared"].ResponsePayload != "db payload" {
		t.Errorf("db entry did not replace archive entry: %+v", byHash["shared"])
	}
	if _, ok := byHash["archive-only"]; !ok {
		t.Error("existing archive entry dropped")
	}
}

func TestPackStarterCache_NilDatabaseAndCorruptArchive(t *testing.T) {
	target := filepath.Join(t.TempDir(), "starter.json.gz")
	if err := os.WriteFile(target, []byte("corrupt"), 0644); err != nil {
		t.Fatal(err)
	}
	res, err := miner.PackStarterCache(nil, target, miner.PackOptions{})
	if err != nil {
		t.Fatalf("pack failed: %v", err)
	}
	if res.TotalEntries != 0 || res.ExistingEntries != 0 || res.MergedFromDB != 0 {
		t.Errorf("unexpected result: %+v", res)
	}
	data, _ := os.ReadFile(target)
	if items := decodeGz(t, data); len(items) != 0 {
		t.Errorf("expected empty archive, got %d items", len(items))
	}
}

func TestPackStarterCache_Errors(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := miner.PackStarterCache(nil, filepath.Join(blocker, "sub", "out.gz"), miner.PackOptions{}); err == nil {
		t.Error("expected error when parent path is a file")
	}

	database := openTestDB(t)
	_ = database.Close()
	if _, err := miner.PackStarterCache(database, filepath.Join(dir, "out.gz"), miner.PackOptions{}); err == nil || !strings.Contains(err.Error(), "failed to query") {
		t.Errorf("expected query error on closed db, got %v", err)
	}
}

func newCompletionServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)
	return srv
}

func TestMinePromptsWithProgress_HooksAndErrors(t *testing.T) {
	srv := newCompletionServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "fail me") {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, okCompletion)
	})

	database := openTestDB(t)
	m := miner.NewCacheMiner(miner.MinerConfig{BaseURL: srv.URL, Workers: 3, RateLimitRPM: 60000, TargetModels: []string{"gpt-4o"}}, database, nil)

	prompts := []miner.PromptItem{
		{ID: "ok-1", UserPrompt: "first"},
		{ID: "ok-2", UserPrompt: "second"},
		{ID: "bad", UserPrompt: "fail me"},
	}

	var mu sync.Mutex
	started := map[string]bool{}
	doneErr := map[string]error{}
	doneEntries := map[string]int{}
	stats, err := m.MinePromptsWithProgress(context.Background(), prompts,
		func(item miner.PromptItem) {
			mu.Lock()
			started[item.ID] = true
			mu.Unlock()
		},
		func(item miner.PromptItem, err error, newEntries int, tokens int) {
			mu.Lock()
			doneErr[item.ID] = err
			doneEntries[item.ID] = newEntries
			mu.Unlock()
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.TotalPrompts != 3 || stats.Completed != 2 || stats.Errors != 1 || stats.CacheEntries != 4 || stats.TokensGenerated != 14 {
		t.Errorf("unexpected stats: %+v", stats)
	}
	if stats.Duration <= 0 {
		t.Error("expected a positive duration")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(started) != 3 {
		t.Errorf("onStart called for %v, want all 3", started)
	}
	if doneErr["bad"] == nil || !strings.Contains(doneErr["bad"].Error(), "status 502") {
		t.Errorf("expected 502 error for bad prompt, got %v", doneErr["bad"])
	}
	if doneErr["ok-1"] != nil || doneEntries["ok-1"] != 2 {
		t.Errorf("ok-1: err=%v entries=%d, want nil and 2", doneErr["ok-1"], doneEntries["ok-1"])
	}

	var rows int
	_ = database.QueryRow("SELECT COUNT(*) FROM cache_entries").Scan(&rows)
	if rows != 4 {
		t.Errorf("expected 4 stored rows (2 prompts x 2 schemas), got %d", rows)
	}
}

func TestMinePromptsWithProgress_EmptyAndCancelled(t *testing.T) {
	var calls int32
	srv := newCompletionServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = io.WriteString(w, okCompletion)
	})
	m := miner.NewCacheMiner(miner.MinerConfig{BaseURL: srv.URL, RateLimitRPM: 60000}, nil, nil)

	stats, err := m.MinePrompts(context.Background(), nil)
	if err != nil || stats.TotalPrompts != 0 || stats.Completed != 0 {
		t.Errorf("empty input: stats=%+v err=%v", stats, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stats, err = m.MinePrompts(ctx, []miner.PromptItem{{ID: "a", UserPrompt: "a"}, {ID: "b", UserPrompt: "b"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.TotalPrompts != 2 || stats.Completed != 0 {
		t.Errorf("cancelled context: stats=%+v, want 2 total and 0 completed", stats)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Errorf("expected no upstream calls after cancellation, got %d", n)
	}
}

func TestAuditCorpusStatus(t *testing.T) {
	const category = "errors"
	prompts := miner.GetCuratedPrompts(category)
	if len(prompts) < 2 {
		t.Skipf("need at least 2 %q prompts, have %d", category, len(prompts))
	}

	t.Run("nil database reports everything missing", func(t *testing.T) {
		items, summary, err := miner.AuditCorpusStatus(nil, category)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(items) != len(prompts) || summary.TotalPrompts != len(prompts) || summary.TotalMissing != len(prompts) || summary.TotalCached != 0 {
			t.Errorf("unexpected summary %+v for %d prompts", summary, len(prompts))
		}
		sum := 0
		for _, n := range summary.Categories {
			sum += n
		}
		if sum != len(prompts) {
			t.Errorf("category counts sum to %d, want %d", sum, len(prompts))
		}
	})

	t.Run("mined and substring matches are cached", func(t *testing.T) {
		srv := newCompletionServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, okCompletion)
		})
		database := openTestDB(t)

		mined := prompts[0]
		m := miner.NewCacheMiner(miner.MinerConfig{BaseURL: srv.URL, RateLimitRPM: 60000}, database, nil)
		if stats, err := m.MinePrompts(context.Background(), []miner.PromptItem{mined}); err != nil || stats.Completed != 1 {
			t.Fatalf("mining failed: stats=%+v err=%v", stats, err)
		}

		var fallback miner.PromptItem
		for _, p := range prompts[1:] {
			if len(p.UserPrompt) < 400 && !strings.Contains(strings.ToLower(mined.UserPrompt), strings.ToLower(p.UserPrompt)) {
				fallback = p
				break
			}
		}
		if fallback.ID == "" {
			t.Skip("no suitable prompt for substring fallback")
		}
		insertEntry(t, database, "legacy-hash", "legacy-model", "legacy: "+strings.ToLower(fallback.UserPrompt), "r", 7, false)

		items, summary, err := miner.AuditCorpusStatus(database, category)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if summary.TotalCached < 2 || summary.TotalCached+summary.TotalMissing != summary.TotalPrompts {
			t.Errorf("unexpected summary: %+v", summary)
		}

		byID := map[string]miner.PromptAuditItem{}
		for _, it := range items {
			byID[it.ID] = it
		}
		got := byID[mined.ID]
		if !got.IsCached || got.CachedAt == "" {
			t.Errorf("mined prompt not reported cached: %+v", got)
		}
		if len(got.ModelsCached) != len(miner.DefaultTargetModels()) {
			t.Errorf("mined prompt cached for %v, want all default target models", got.ModelsCached)
		}

		fb := byID[fallback.ID]
		if !fb.IsCached || fb.HitCount != 7 || len(fb.ModelsCached) != 1 || fb.ModelsCached[0] != "legacy-model" {
			t.Errorf("substring fallback not applied: %+v", fb)
		}
	})
}

func waitForStatus(t *testing.T, mm *miner.MiningManager, want miner.MiningStatus) miner.MiningStatusResponse {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st := mm.Status()
		if st.Status == want {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("status did not reach %q, last %+v", want, mm.Status())
	return miner.MiningStatusResponse{}
}

type eventLog struct {
	mu     sync.Mutex
	events []miner.MiningProgressEvent
}

func (l *eventLog) add(ev miner.MiningProgressEvent) {
	l.mu.Lock()
	l.events = append(l.events, ev)
	l.mu.Unlock()
}

func (l *eventLog) types() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]int{}
	for _, ev := range l.events {
		out[ev.Type]++
	}
	return out
}

func containsLog(logs []string, substr string) bool {
	for _, l := range logs {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

func TestMiningManager_CompletesSession(t *testing.T) {
	srv := newCompletionServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, okCompletion)
	})
	database := openTestDB(t)
	events := &eventLog{}
	mm := miner.NewMiningManager(database, events.add)

	idle := mm.Status()
	if idle.Status != miner.MiningStatusIdle || idle.StartedAt != nil || idle.ElapsedSec != 0 || len(idle.RecentLogs) != 0 {
		t.Errorf("unexpected initial status: %+v", idle)
	}
	if err := mm.Stop(); err != nil {
		t.Errorf("Stop on idle manager returned %v", err)
	}

	cfg := miner.MinerConfig{Provider: "ollama", BaseURL: srv.URL, Model: "gen", Workers: 2, RateLimitRPM: 60000, TargetModels: []string{"gpt-4o"}}
	prompts := []miner.PromptItem{{ID: "a", UserPrompt: "alpha"}, {ID: "b", UserPrompt: "beta"}}
	if err := mm.Start(cfg, "custom", prompts); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	st := waitForStatus(t, mm, miner.MiningStatusCompleted)
	if st.Provider != "ollama" || st.Category != "custom" || st.StartedAt == nil || st.CurrentPrompt != nil {
		t.Errorf("unexpected final status: %+v", st)
	}
	if st.Stats.Completed != 2 || st.Stats.CacheEntries != 4 || st.Stats.TokensGenerated != 14 || st.Stats.Errors != 0 {
		t.Errorf("unexpected final stats: %+v", st.Stats)
	}
	if !containsLog(st.RecentLogs, "CACHED [a]") || !containsLog(st.RecentLogs, "Mining session finished") {
		t.Errorf("expected cached and finished logs, got %v", st.RecentLogs)
	}

	// The completion event is emitted after the status flips; wait for it.
	deadline := time.Now().Add(5 * time.Second)
	for events.types()["miner_complete"] == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	types := events.types()
	if types["miner_complete"] != 1 || types["miner_log"] != 2 || types["miner_progress"] < 5 {
		t.Errorf("unexpected event counts: %v", types)
	}
}

func TestMiningManager_AllFailuresMarkFailed(t *testing.T) {
	srv := newCompletionServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	mm := miner.NewMiningManager(nil, nil)
	cfg := miner.MinerConfig{BaseURL: srv.URL, RateLimitRPM: 60000, TargetModels: []string{"gpt-4o"}}
	if err := mm.Start(cfg, "all", []miner.PromptItem{{ID: "x", UserPrompt: "x"}}); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	st := waitForStatus(t, mm, miner.MiningStatusFailed)
	if st.Stats.Errors != 1 || st.Stats.Completed != 0 {
		t.Errorf("unexpected stats: %+v", st.Stats)
	}
	if !containsLog(st.RecentLogs, "ERROR [x]") || !containsLog(st.RecentLogs, "Mining session failed") {
		t.Errorf("expected error logs, got %v", st.RecentLogs)
	}
}

func TestMiningManager_RejectsConcurrentStartAndStops(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := newCompletionServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		// The server does not notice the client hanging up while the body is
		// unread, so the handler also waits on an explicit release.
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	// Registered after the server so it runs first and unblocks srv.Close.
	t.Cleanup(func() { close(release) })
	mm := miner.NewMiningManager(nil, nil)
	cfg := miner.MinerConfig{BaseURL: srv.URL, Workers: 1, RateLimitRPM: 60000, TargetModels: []string{"gpt-4o"}}
	if err := mm.Start(cfg, "all", []miner.PromptItem{{ID: "slow", UserPrompt: "slow"}}); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("upstream request never arrived")
	}

	running := mm.Status()
	if running.Status != miner.MiningStatusRunning || running.CurrentPrompt == nil || running.CurrentPrompt.ID != "slow" {
		t.Errorf("unexpected running status: %+v", running)
	}
	if err := mm.Start(cfg, "all", nil); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Errorf("expected already-in-progress error, got %v", err)
	}

	if err := mm.Stop(); err != nil {
		t.Fatalf("stop failed: %v", err)
	}
	st := waitForStatus(t, mm, miner.MiningStatusIdle)
	if !containsLog(st.RecentLogs, "Abort requested") || !containsLog(st.RecentLogs, "halted by user") {
		t.Errorf("expected abort logs, got %v", st.RecentLogs)
	}
}
