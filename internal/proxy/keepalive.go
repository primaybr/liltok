package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Streaming keep-alive for routed requests. A routed reply is produced by a non-streaming dispatch
// (so the router can validate the whole reply and fail over) and then replayed as SSE, which means
// a streaming client can wait many seconds for its first byte. When the dispatch takes longer than
// keepAliveDelay, the writer commits the SSE response early and sends SSE comment lines, which
// every SSE parser ignores, until the reply is ready. Nothing else about the reply changes.
//
// Once committed, the status line is gone, so later writes adapt: X-Liltok-Provider and
// X-Liltok-Cache-Tier travel as HTTP trailers, and a failure written with a 4xx/5xx status (an
// exhausted chain, an upstream error) is delivered as an SSE error event instead of a JSON body.

const (
	keepAliveDelay    = 2 * time.Second
	keepAliveInterval = 10 * time.Second
)

type keepAliveWriter struct {
	http.ResponseWriter
	anthropic bool

	mu        sync.Mutex
	committed bool
	stopped   bool
	done      chan struct{}
	finished  chan struct{}

	failStatus int
	failBody   bytes.Buffer
}

// startKeepAlive wraps w and begins the delayed commit. Call stop when the dispatch returns, and
// finish once the handler is done writing.
func startKeepAlive(w http.ResponseWriter, anthropic bool, delay, interval time.Duration) *keepAliveWriter {
	k := &keepAliveWriter{ResponseWriter: w, anthropic: anthropic, done: make(chan struct{}), finished: make(chan struct{})}
	go k.run(delay, interval)
	return k
}

func (k *keepAliveWriter) run(delay, interval time.Duration) {
	defer close(k.finished)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-k.done:
		return
	case <-timer.C:
	}
	k.mu.Lock()
	if k.stopped {
		k.mu.Unlock()
		return
	}
	h := k.ResponseWriter.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Liltok-Cache-Status", "MISS")
	h.Set("Trailer", "X-Liltok-Provider, X-Liltok-Cache-Tier")
	k.ResponseWriter.WriteHeader(http.StatusOK)
	k.committed = true
	k.comment("liltok routing")
	k.mu.Unlock()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-k.done:
			return
		case <-ticker.C:
			k.mu.Lock()
			if !k.stopped {
				k.comment("keep-alive")
			}
			k.mu.Unlock()
		}
	}
}

// comment writes an SSE comment line; the caller holds k.mu.
func (k *keepAliveWriter) comment(text string) {
	_, _ = k.ResponseWriter.Write([]byte(": " + text + "\n\n"))
	if f, ok := k.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// stop ends the keep-alive loop. After it returns the writer is used by one goroutine only.
func (k *keepAliveWriter) stop() {
	k.mu.Lock()
	if !k.stopped {
		k.stopped = true
		close(k.done)
	}
	k.mu.Unlock()
	<-k.finished
}

func (k *keepAliveWriter) WriteHeader(status int) {
	if !k.committed {
		k.ResponseWriter.WriteHeader(status)
		return
	}
	if status >= 400 {
		k.failStatus = status
	}
}

func (k *keepAliveWriter) Write(p []byte) (int, error) {
	if k.committed && k.failStatus != 0 {
		return k.failBody.Write(p)
	}
	return k.ResponseWriter.Write(p)
}

func (k *keepAliveWriter) Flush() {
	if f, ok := k.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// finish delivers a failure written after the commit as an SSE error event.
func (k *keepAliveWriter) finish() {
	k.stop()
	if !k.committed || k.failStatus == 0 {
		return
	}
	msg := strings.TrimSpace(k.failBody.String())
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(msg), &parsed) == nil && parsed.Error.Message != "" {
		msg = parsed.Error.Message
	}
	if msg == "" {
		msg = http.StatusText(k.failStatus)
	}
	errType := "api_error"
	switch {
	case k.failStatus == http.StatusTooManyRequests:
		errType = "rate_limit_error"
	case k.failStatus == 529 || k.failStatus == http.StatusServiceUnavailable:
		errType = "overloaded_error"
	case k.failStatus < 500:
		errType = "invalid_request_error"
	}
	detail, _ := json.Marshal(map[string]string{"type": errType, "message": fmt.Sprintf("%s (status %d)", msg, k.failStatus)})
	if k.anthropic {
		_, _ = fmt.Fprintf(k.ResponseWriter, "event: error\ndata: {\"type\":\"error\",\"error\":%s}\n\n", detail)
	} else {
		_, _ = fmt.Fprintf(k.ResponseWriter, "data: {\"error\":%s}\n\ndata: [DONE]\n\n", detail)
	}
	k.Flush()
}
