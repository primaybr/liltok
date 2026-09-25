package sync_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	cachesync "github.com/primaybr/liltok/internal/cache/sync"
	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/miner"
)

func openSyncDB(t *testing.T) *db.DB {
	t.Helper()
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	database, err := db.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func gzItems(t *testing.T, items []miner.CacheExportItem) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(items); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func metadata(t *testing.T, database *db.DB, key string) string {
	t.Helper()
	var v string
	_ = database.QueryRow("SELECT value FROM sync_metadata WHERE key = ?", key).Scan(&v)
	return v
}

// recordedRequest captures the headers the syncer sends.
type recordedRequest struct {
	ifNoneMatch, ifModifiedSince, userAgent string
}

type recorder struct {
	mu   sync.Mutex
	reqs []recordedRequest
}

func (r *recorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, recordedRequest{
		ifNoneMatch:     req.Header.Get("If-None-Match"),
		ifModifiedSince: req.Header.Get("If-Modified-Since"),
		userAgent:       req.Header.Get("User-Agent"),
	})
}

func (r *recorder) all() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedRequest(nil), r.reqs...)
}

func TestSync_PersistsMetadataAndSendsConditionalHeaders(t *testing.T) {
	pack := gzItems(t, []miner.CacheExportItem{{Hash: "h1", Model: "gpt-4o", NormalizedPrompt: "p", ResponsePayload: "r"}})
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		if r.Header.Get("If-Modified-Since") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		// No ETag: only Last-Modified is available for the next conditional request.
		w.Header().Set("Last-Modified", " Wed, 17 Sep 2026 12:00:00 GMT ")
		_, _ = w.Write(pack)
	}))
	defer srv.Close()

	database := openSyncDB(t)
	s := cachesync.NewCacheSyncer(database, srv.URL, srv.Client())

	res, err := s.Sync(context.Background(), false)
	if err != nil {
		t.Fatalf("first sync failed: %v", err)
	}
	if res.NewEntries != 1 || res.UpToDate || res.ETag != "" {
		t.Errorf("unexpected first result: %+v", res)
	}
	if got := metadata(t, database, "last_modified"); got != "Wed, 17 Sep 2026 12:00:00 GMT" {
		t.Errorf("last_modified = %q, want trimmed header value", got)
	}
	if got := metadata(t, database, "last_etag"); got != "" {
		t.Errorf("last_etag should stay unset without an ETag header, got %q", got)
	}
	ts := metadata(t, database, "last_sync_timestamp")
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Errorf("last_sync_timestamp %q is not RFC3339: %v", ts, err)
	}

	res, err = s.Sync(context.Background(), false)
	if err != nil {
		t.Fatalf("second sync failed: %v", err)
	}
	if !res.UpToDate || res.NewEntries != 0 {
		t.Errorf("expected 304 up-to-date result, got %+v", res)
	}

	if _, err := s.Sync(context.Background(), true); err != nil {
		t.Fatalf("forced sync failed: %v", err)
	}

	reqs := rec.all()
	if len(reqs) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(reqs))
	}
	if reqs[0].ifModifiedSince != "" || reqs[0].ifNoneMatch != "" {
		t.Errorf("cold sync sent conditional headers: %+v", reqs[0])
	}
	if reqs[1].ifModifiedSince != "Wed, 17 Sep 2026 12:00:00 GMT" || reqs[1].ifNoneMatch != "" {
		t.Errorf("warm sync headers = %+v, want only If-Modified-Since", reqs[1])
	}
	if reqs[2].ifModifiedSince != "" || reqs[2].ifNoneMatch != "" {
		t.Errorf("forced sync sent conditional headers: %+v", reqs[2])
	}
	for i, r := range reqs {
		if !strings.HasPrefix(r.userAgent, "liltok-cache-syncer/") {
			t.Errorf("request %d user agent = %q", i, r.userAgent)
		}
	}
}

func TestSync_ErrorStatusesAndBadPayload(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    []byte
		wantErr string
	}{
		{"not found", http.StatusNotFound, nil, "HTTP 404"},
		{"server error", http.StatusInternalServerError, nil, "returned HTTP 500"},
		{"no content", http.StatusNoContent, nil, "returned HTTP 204"},
		{"corrupt pack", http.StatusOK, []byte("not gzip"), "failed to ingest downloaded cache pack"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"should-not-persist"`)
				w.WriteHeader(tt.status)
				_, _ = w.Write(tt.body)
			}))
			defer srv.Close()

			database := openSyncDB(t)
			s := cachesync.NewCacheSyncer(database, srv.URL, srv.Client())
			res, err := s.Sync(context.Background(), false)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
			if res.NewEntries != 0 || res.UpToDate {
				t.Errorf("unexpected result on error: %+v", res)
			}
			if got := metadata(t, database, "last_etag"); got != "" {
				t.Errorf("failed sync persisted etag %q", got)
			}
		})
	}
}

func TestSync_NilDatabaseSkipsConditionalHeaders(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	s := cachesync.NewCacheSyncer(nil, srv.URL, srv.Client())
	res, err := s.Sync(context.Background(), false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.UpToDate || res.ETag != "" {
		t.Errorf("unexpected result: %+v", res)
	}
	if reqs := rec.all(); len(reqs) != 1 || reqs[0].ifNoneMatch != "" || reqs[0].ifModifiedSince != "" {
		t.Errorf("unexpected requests: %+v", reqs)
	}
}

func TestSync_CancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request should not reach the server")
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := cachesync.NewCacheSyncer(nil, srv.URL, srv.Client())
	if _, err := s.Sync(ctx, true); err == nil || !strings.Contains(err.Error(), "network error") {
		t.Errorf("expected network error for cancelled context, got %v", err)
	}
}

func TestStartBackgroundLoop_SyncsPeriodicallyUntilCancelled(t *testing.T) {
	if testing.Short() {
		t.Skip("the loop waits 5s before its first sync")
	}

	pack := gzItems(t, []miner.CacheExportItem{{Hash: "loop-1", Model: "gpt-4o", NormalizedPrompt: "p", ResponsePayload: "r"}})
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		switch {
		case n == 1:
			w.Header().Set("ETag", `"loop-tag"`)
			_, _ = w.Write(pack)
		case r.Header.Get("If-None-Match") == `"loop-tag"` && n == 2:
			w.WriteHeader(http.StatusNotModified)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	database := openSyncDB(t)
	s := cachesync.NewCacheSyncer(database, srv.URL, srv.Client())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartBackgroundLoop(ctx, 50*time.Millisecond)

	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}

	deadline := time.Now().Add(15 * time.Second)
	for count() < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if count() < 3 {
		t.Fatalf("expected at least 3 background syncs, got %d", count())
	}

	var rows int
	_ = database.QueryRow("SELECT COUNT(*) FROM cache_entries WHERE hash = 'loop-1'").Scan(&rows)
	if rows != 1 {
		t.Errorf("background sync did not import the pack, rows=%d", rows)
	}
	if got := metadata(t, database, "last_etag"); got != `"loop-tag"` {
		t.Errorf("last_etag = %q, want loop tag", got)
	}

	cancel()
	// Allow an in-flight tick to finish, then confirm the loop has stopped.
	time.Sleep(100 * time.Millisecond)
	after := count()
	time.Sleep(250 * time.Millisecond)
	if count() != after {
		t.Errorf("loop kept syncing after cancel: %d -> %d", after, count())
	}
}

func TestStartBackgroundLoop_CancelBeforeFirstSync(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no sync expected when cancelled during the startup delay")
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	s := cachesync.NewCacheSyncer(nil, srv.URL, srv.Client())
	s.StartBackgroundLoop(ctx, 0)
	cancel()
	time.Sleep(50 * time.Millisecond)
}

func TestSyncWithoutDatabaseRejectsDownloadedPack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pack bytes"))
	}))
	defer srv.Close()

	s := cachesync.NewCacheSyncer(nil, srv.URL, srv.Client())
	if _, err := s.Sync(context.Background(), true); err == nil || !strings.Contains(err.Error(), "no database") {
		t.Fatalf("Sync with a nil database on HTTP 200 = %v, want a no-database error instead of a panic", err)
	}
}
