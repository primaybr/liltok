package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/primaybr/liltok/internal/cache/exact"
	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/server/middleware"
)

// TestMaxPromptBytesSkipsCaching checks cache.max_prompt_bytes: a request above the limit is sent
// upstream every time and never stored; the same request under a higher limit is cached.
func TestMaxPromptBytesSkipsCaching(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	run := func(limit int) (statuses []string, stored int) {
		t.Helper()
		t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
		database, err := db.Open(t.TempDir() + "/limit.db")
		if err != nil {
			t.Fatal(err)
		}
		defer database.Close()
		store, err := exact.NewTieredStore(database, 100)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		cfg := config.DefaultConfig()
		cfg.Providers.OpenAI.BaseURL = upstream.URL + "/v1"
		cfg.Providers.OpenAI.APIKey = "key"
		cfg.Cache.MaxPromptBytes = limit
		handler := middleware.RequestID(http.HandlerFunc(NewProxy(cfg, store, nil, nil, nil, nil).HandleChatCompletions))
		body := `{"model":"gpt-4o","temperature":0,"messages":[{"role":"user","content":"` + strings.Repeat("transcript ", 60) + `"}]}`
		for i := 0; i < 2; i++ {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
			req.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(rec, req)
			statuses = append(statuses, rec.Header().Get("X-Liltok-Cache-Status"))
			time.Sleep(50 * time.Millisecond) // let the async cache write land
		}
		_ = database.QueryRow(`SELECT COUNT(*) FROM cache_entries`).Scan(&stored)
		return statuses, stored
	}

	calls.Store(0)
	statuses, stored := run(200)
	if statuses[0] != "MISS" || statuses[1] != "MISS" || calls.Load() != 2 || stored != 0 {
		t.Fatalf("over the limit: statuses %v, upstream calls %d, stored %d; want two misses and nothing stored", statuses, calls.Load(), stored)
	}

	calls.Store(0)
	statuses, stored = run(0)
	if statuses[0] != "MISS" || statuses[1] != "HIT" || calls.Load() != 1 || stored != 1 {
		t.Fatalf("no limit: statuses %v, upstream calls %d, stored %d; want miss then hit", statuses, calls.Load(), stored)
	}
}
