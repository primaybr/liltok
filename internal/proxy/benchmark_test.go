package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/primaybr/liltok/internal/cache/exact"
	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/server/middleware"
)

// Cache-hit benchmarks for the M10.3 target (5000 req/s, P99 < 5 ms on Tier-1 hits). Each benchmark
// warms one entry through a mock upstream, then drives the real handler in parallel. Besides ns/op
// they report p50-ms and p99-ms over every timed request, and fail if an iteration misses the cache
// or reaches the upstream. Per-request timings are only as fine as the OS monotonic clock, which is
// coarse on Windows; use scripts/k6/cache_hit.js against a running gateway for authoritative P99.
//
//	go test ./internal/proxy -run '^$' -bench CacheHit -benchtime 20000x

type benchCase struct {
	path     string
	body     string
	upstream string
	handler  func(p *Proxy) http.HandlerFunc
	config   func(cfg *config.Config, upstreamURL string)
}

var benchCases = map[string]benchCase{
	"openai": {
		path:     "/v1/chat/completions",
		body:     `{"model":"gpt-4o","messages":[{"role":"user","content":"benchmark query"}],"temperature":0.0}`,
		upstream: `{"id":"chatcmpl-bench","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"cached benchmark response"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}`,
		handler:  func(p *Proxy) http.HandlerFunc { return p.HandleChatCompletions },
		config: func(cfg *config.Config, u string) {
			cfg.Providers.OpenAI.BaseURL = u + "/v1"
			cfg.Providers.OpenAI.APIKey = "bench-key"
		},
	},
	"anthropic": {
		path:     "/v1/messages",
		body:     `{"model":"claude-sonnet-5","max_tokens":64,"messages":[{"role":"user","content":"benchmark query"}],"temperature":0.0}`,
		upstream: `{"id":"msg_bench","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"cached benchmark response"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":4}}`,
		handler:  func(p *Proxy) http.HandlerFunc { return p.HandleAnthropicMessages },
		config: func(cfg *config.Config, u string) {
			cfg.Providers.Anthropic.BaseURL = u
			cfg.Providers.Anthropic.APIKey = "bench-key"
		},
	},
}

func BenchmarkCacheHitOpenAI(b *testing.B)    { benchCacheHit(b, benchCases["openai"]) }
func BenchmarkCacheHitAnthropic(b *testing.B) { benchCacheHit(b, benchCases["anthropic"]) }

func benchCacheHit(b *testing.B, bc benchCase) {
	b.Setenv("LILTOK_SKIP_STARTER_SEED", "1")

	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bc.upstream))
	}))
	defer upstream.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer database.Close()
	store, err := exact.NewTieredStore(database, 1000)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()

	cfg := config.DefaultConfig()
	bc.config(cfg, upstream.URL)
	handler := middleware.RequestID(bc.handler(NewProxy(cfg, store, nil, nil, nil, nil)))

	serve := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, bc.path, bytes.NewReader([]byte(bc.body)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// Warm: the first call misses and stores the reply; the write can land asynchronously.
	if rec := serve(); rec.Code != http.StatusOK {
		b.Fatalf("warm-up call returned %d: %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for serve().Header().Get("X-Liltok-Cache-Status") != "HIT" {
		if time.Now().After(deadline) {
			b.Fatal("cache entry was not served as a HIT within 2s of warm-up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	warmCalls := upstreamCalls.Load()

	var (
		mu        sync.Mutex
		latencies = make([]time.Duration, 0, b.N)
		misses    atomic.Int64
	)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		local := make([]time.Duration, 0, 1024)
		for pb.Next() {
			start := time.Now()
			rec := serve()
			local = append(local, time.Since(start))
			if rec.Code != http.StatusOK || rec.Header().Get("X-Liltok-Cache-Tier") != "TIER1_EXACT" {
				misses.Add(1)
			}
		}
		mu.Lock()
		latencies = append(latencies, local...)
		mu.Unlock()
	})
	b.StopTimer()

	if n := misses.Load(); n > 0 {
		b.Fatalf("%d of %d requests were not Tier-1 hits", n, len(latencies))
	}
	if extra := upstreamCalls.Load() - warmCalls; extra > 0 {
		b.Fatalf("upstream was called %d times during the timed loop", extra)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	b.ReportMetric(percentileMs(latencies, 0.50), "p50-ms")
	b.ReportMetric(percentileMs(latencies, 0.99), "p99-ms")
}

// percentileMs returns the q-th quantile of sorted latencies in milliseconds.
func percentileMs(sorted []time.Duration, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * q)
	return float64(sorted[idx]) / float64(time.Millisecond)
}
