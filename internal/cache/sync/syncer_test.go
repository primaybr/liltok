package sync_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	cachesync "github.com/primaybr/liltok/internal/cache/sync"
	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/miner"
)

func TestCacheSyncer_HTTP200_And_HTTP304(t *testing.T) {
	// 1. Prepare sample compressed cache items
	testItems := []miner.CacheExportItem{
		{
			Hash:             "hash-ota-001",
			Model:            "claude-3-5-sonnet-20241022",
			NormalizedPrompt: `{"messages":[{"content":"test ota 1","role":"user"}],"model":"claude-3-5-sonnet-20241022"}`,
			ResponsePayload:  `{"id":"msg-ota-1","content":[{"type":"text","text":"ota answer 1"}]}`,
			PromptTokens:     10,
			CompletionTokens: 20,
			TTLSeconds:       86400,
		},
		{
			Hash:             "hash-ota-002",
			Model:            "gpt-4o",
			NormalizedPrompt: `{"messages":[{"content":"test ota 2","role":"user"}],"model":"gpt-4o"}`,
			ResponsePayload:  `{"id":"chatcmpl-ota-2","choices":[{"message":{"content":"ota answer 2"}}]}`,
			PromptTokens:     15,
			CompletionTokens: 25,
			TTLSeconds:       86400,
		},
	}

	var gzBuf bytes.Buffer
	gzWriter := gzip.NewWriter(&gzBuf)
	_ = json.NewEncoder(gzWriter).Encode(testItems)
	_ = gzWriter.Close()
	gzBytes := gzBuf.Bytes()

	testETag := `"release-v1.1-cache-tag"`
	requestCount := 0

	// Mock remote server (GitHub Releases simulation)
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++

		if r.Header.Get("If-None-Match") == testETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("ETag", testETag)
		w.Header().Set("Last-Modified", "Wed, 17 Sep 2026 12:00:00 GMT")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gzBytes)
	}))
	defer mockServer.Close()

	// 2. Open clean SQLite database
	tempDBPath := filepath.Join(t.TempDir(), "synctest.db")
	database, err := db.Open(tempDBPath)
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer database.Close()

	syncer := cachesync.NewCacheSyncer(database, mockServer.URL, mockServer.Client())

	// 3. First Sync (Cold: Should receive HTTP 200 and insert entries)
	ctx := context.Background()
	res1, err := syncer.Sync(ctx, false)
	if err != nil {
		t.Fatalf("first sync failed: %v", err)
	}
	if res1.UpToDate {
		t.Errorf("expected res1 to NOT be up-to-date on initial sync")
	}
	if res1.NewEntries != 2 {
		t.Errorf("expected 2 new entries, got %d", res1.NewEntries)
	}
	if res1.ETag != testETag {
		t.Errorf("expected ETag %s, got %s", testETag, res1.ETag)
	}

	// Verify entries exist in SQLite
	var rowCount int
	_ = database.QueryRowContext(ctx, "SELECT COUNT(*) FROM cache_entries WHERE hash IN ('hash-ota-001', 'hash-ota-002')").Scan(&rowCount)
	if rowCount != 2 {
		t.Errorf("expected 2 ota rows in database, got %d", rowCount)
	}

	// 4. Second Sync (Warm: Should send If-None-Match and receive HTTP 304)
	res2, err := syncer.Sync(ctx, false)
	if err != nil {
		t.Fatalf("second sync failed: %v", err)
	}
	if !res2.UpToDate {
		t.Errorf("expected res2 to be up-to-date (HTTP 304)")
	}
	if res2.NewEntries != 0 {
		t.Errorf("expected 0 new entries on 304, got %d", res2.NewEntries)
	}

	// 5. Force Sync (Should bypass ETag and redownload)
	res3, err := syncer.Sync(ctx, true)
	if err != nil {
		t.Fatalf("force sync failed: %v", err)
	}
	if res3.UpToDate {
		t.Errorf("expected force sync to redownload")
	}
	// Due to INSERT OR IGNORE, new entries count is 0 because they already exist
	if res3.NewEntries != 0 {
		t.Errorf("expected 0 new entries on re-import due to INSERT OR IGNORE, got %d", res3.NewEntries)
	}
}

func TestCacheSyncer_ErrorHandling(t *testing.T) {
	// 1. 404 Not Found simulation
	notFoundServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer notFoundServer.Close()

	tempDBPath := filepath.Join(t.TempDir(), "errtest.db")
	database, err := db.Open(tempDBPath)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer database.Close()

	syncer := cachesync.NewCacheSyncer(database, notFoundServer.URL, notFoundServer.Client())
	_, err = syncer.Sync(context.Background(), false)
	if err == nil {
		t.Errorf("expected error on 404 response")
	}

	// 2. Offline / bad host simulation (Fail-open test)
	badSyncer := cachesync.NewCacheSyncer(database, "http://127.0.0.1:59999/nonexistent", &http.Client{Timeout: 500 * time.Millisecond})
	_, err = badSyncer.Sync(context.Background(), false)
	if err == nil {
		t.Errorf("expected network error on unreachable port")
	}
}
