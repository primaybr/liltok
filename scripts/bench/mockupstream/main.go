// Command mockupstream is a deterministic OpenAI-compatible mock server used by the
// head-to-head gateway benchmark. Every request sleeps a fixed latency and returns a
// fixed-size reply derived only from the request body, so measured differences between
// gateways are gateway overhead and not upstream variance.
//
// Usage: go run ./scripts/bench/mockupstream -addr 127.0.0.1:9999 -latency 50ms
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9999", "listen address")
	latency := flag.Duration("latency", 50*time.Millisecond, "fixed per-request latency")
	flag.Parse()

	var calls atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls.Add(1)
		time.Sleep(*latency)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Model == "" {
			req.Model = "mock"
		}
		sum := sha256.Sum256(body)
		content := "mock reply " + hex.EncodeToString(sum[:8])
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":      "chatcmpl-mock-" + hex.EncodeToString(sum[:4]),
			"object":  "chat.completion",
			"created": 1700000000,
			"model":   req.Model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]string{"role": "assistant", "content": content},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 20, "completion_tokens": 8, "total_tokens": 28},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-4o-mini","object":"model"}]}`))
	})
	// /stats reports how many requests reached the upstream (cache hits never do).
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"upstream_calls":%d}`, calls.Load())
	})
	mux.HandleFunc("/reset", func(w http.ResponseWriter, _ *http.Request) {
		calls.Store(0)
		w.WriteHeader(http.StatusNoContent)
	})

	log.Printf("mockupstream listening on %s latency=%s", *addr, *latency)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
