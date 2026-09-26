package db

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestExtractSearchText(t *testing.T) {
	cases := []struct {
		name, prompt, want string
	}{
		{"string content, reminder removed",
			`{"messages":[{"role":"user","content":"<system-reminder>hook output</system-reminder>How do I write a Go HTTP handler?"}]}`,
			"How do I write a Go HTTP handler?"},
		{"text blocks kept, tool results skipped",
			`{"messages":[{"role":"user","content":[{"type":"tool_result","content":"FAIL"},{"type":"text","text":"fix the failing test"}]}]}`,
			"fix the failing test"},
		{"latest user message then the first",
			`{"messages":[{"role":"user","content":"build a rate limiter"},{"role":"assistant","content":"ok"},{"role":"user","content":[{"type":"tool_result","content":"done"}]},{"role":"user","content":"now add tests"}]}`,
			"now add tests\nbuild a rate limiter"},
		{"reminder-only turns ignored",
			`{"messages":[{"role":"user","content":"explain WAL"},{"role":"user","content":[{"type":"text","text":"<system-reminder>todo list empty</system-reminder>"}]}]}`,
			"explain WAL"},
		{"not a request: plain text", "just a plain prompt", "just a plain prompt"},
	}
	for _, tc := range cases {
		if got := ExtractSearchText(tc.prompt); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	long := `{"messages":[{"role":"user","content":"` + strings.Repeat("é", 2000) + `"}]}`
	if got := ExtractSearchText(long); len(got) > searchTextMax || !strings.HasSuffix(got, "é") {
		t.Errorf("long text must be clipped to %d bytes on a character boundary, got %d bytes", searchTextMax, len(got))
	}
}

func searchDB(t *testing.T) *DB {
	t.Helper()
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	d, err := Open(filepath.Join(t.TempDir(), "search.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func addEntry(t *testing.T, d *DB, hash, userText string, hits int) {
	t.Helper()
	prompt := `{"model":"m","messages":[{"role":"user","content":"` + userText + `"}]}`
	if _, err := d.Exec(`INSERT INTO cache_entries (hash, model, normalized_prompt, response_payload, hit_count) VALUES (?, 'm', ?, x'7b7d', ?)`, hash, prompt, hits); err != nil {
		t.Fatal(err)
	}
}

func TestSearchIndexAndQuery(t *testing.T) {
	d := searchDB(t)
	ctx := context.Background()
	addEntry(t, d, "h-handler", "How do I write a Go HTTP handler with a timeout?", 3)
	addEntry(t, d, "h-handler-popular", "Go HTTP handler middleware example", 9)
	addEntry(t, d, "h-sql", "Explain SQL window functions", 5)
	addEntry(t, d, "h-percent", "What does 100% CPU mean?", 1)

	if hits, err := d.SearchCacheHashes(ctx, "handler", 5); err != nil || len(hits) != 0 {
		t.Fatalf("before indexing = %v, %v; want no matches", hits, err)
	}
	n, err := d.IndexCacheSearch(ctx, 2)
	if err != nil || n != 2 {
		t.Fatalf("first batch indexed %d (%v), want 2", n, err)
	}
	if n, _ = d.IndexCacheSearch(ctx, 10); n != 2 {
		t.Fatalf("second batch indexed %d, want the remaining 2", n)
	}
	if n, _ = d.IndexCacheSearch(ctx, 10); n != 0 {
		t.Fatalf("a fully indexed cache indexed %d more", n)
	}

	cases := []struct {
		query string
		want  []string
	}{
		{"go handler", []string{"h-handler-popular", "h-handler"}}, // every word, most hits first
		{"GO HTTP TIMEOUT", []string{"h-handler"}},                 // case-insensitive
		{"handler sql", nil},            // all words must match
		{"100%", []string{"h-percent"}}, // % is literal, not a wildcard
		{"a", nil},                      // one-letter words are ignored
	}
	for _, tc := range cases {
		got, err := d.SearchCacheHashes(ctx, tc.query, 5)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("search %q = %v (%v), want %v", tc.query, got, err, tc.want)
		}
	}
	texts, err := d.SearchTexts(ctx, []string{"h-sql", "missing"})
	if err != nil || texts["h-sql"] != "Explain SQL window functions" || len(texts) != 1 {
		t.Errorf("SearchTexts = %v, %v", texts, err)
	}

	if _, err := d.Exec(`DELETE FROM cache_entries WHERE hash = 'h-sql'`); err != nil {
		t.Fatal(err)
	}
	if err := d.PruneCacheSearch(ctx); err != nil {
		t.Fatal(err)
	}
	var left int
	_ = d.QueryRow(`SELECT COUNT(*) FROM cache_search WHERE hash = 'h-sql'`).Scan(&left)
	if left != 0 {
		t.Error("prune must remove search rows of deleted entries")
	}
}

func TestRunSearchIndexerIndexesAndStops(t *testing.T) {
	d := searchDB(t)
	for i := 0; i < searchIndexBatch+5; i++ {
		addEntry(t, d, "bulk-"+strings.Repeat("x", i+1), "bulk entry about caching", 0)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.RunSearchIndexer(ctx, time.Hour); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		_ = d.QueryRow(`SELECT COUNT(*) FROM cache_search`).Scan(&n)
		if n == searchIndexBatch+5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("indexer indexed %d of %d entries", n, searchIndexBatch+5)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("indexer did not stop when its context ended")
	}
}

func TestPurgeCacheLargerThan(t *testing.T) {
	d := searchDB(t)
	ctx := context.Background()
	big := strings.Repeat("x", 3000)
	addEntry(t, d, "small", "short question", 1)
	addEntry(t, d, "large", big, 1)
	addEntry(t, d, "large-pinned", big, 1)
	if _, err := d.Exec(`UPDATE cache_entries SET is_pinned = 1 WHERE hash = 'large-pinned'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.IndexCacheSearch(ctx, 10); err != nil {
		t.Fatal(err)
	}

	n, err := d.PurgeCacheLargerThan(ctx, 2048)
	if err != nil || n != 1 {
		t.Fatalf("purged %d (%v), want only the unpinned large entry", n, err)
	}
	var left []string
	rows, _ := d.Query(`SELECT hash FROM cache_entries ORDER BY hash`)
	for rows.Next() {
		var h string
		_ = rows.Scan(&h)
		left = append(left, h)
	}
	rows.Close()
	if !reflect.DeepEqual(left, []string{"large-pinned", "small"}) {
		t.Fatalf("entries left = %v", left)
	}
	var searchRows int
	_ = d.QueryRow(`SELECT COUNT(*) FROM cache_search WHERE hash = 'large'`).Scan(&searchRows)
	if searchRows != 0 {
		t.Error("the purged entry's search row must be removed")
	}
}
