package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/liltok/liltok/internal/telemetry"
)

// TelemetryEvent represents a real-time event broadcast to dashboard clients.
type TelemetryEvent struct {
	Type      string      `json:"type"` // "request", "cache_hit", "failover", "breaker_trip"
	Timestamp time.Time   `json:"timestamp"`
	Data      interface{} `json:"data"`
}

// Broadcaster manages active Server-Sent Events (SSE) connections.
type Broadcaster struct {
	mu      sync.RWMutex
	clients map[chan []byte]struct{}
}

// NewBroadcaster creates a new SSE broadcaster.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		clients: make(map[chan []byte]struct{}),
	}
}

// Broadcast serializes an event and dispatches it to all connected SSE clients.
func (b *Broadcaster) Broadcast(event TelemetryEvent) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	payload, err := json.Marshal(event)
	if err != nil {
		return
	}

	msg := []byte(fmt.Sprintf("data: %s\n\n", string(payload)))

	b.mu.RLock()
	defer b.mu.RUnlock()

	for clientChan := range b.clients {
		select {
		case clientChan <- msg:
		default:
			// Non-blocking write: if client buffer full, skip to avoid blocking other clients
		}
	}
}

// ServeHTTP handles incoming SSE client subscriptions at /api/v1/events.
func (b *Broadcaster) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	clientChan := make(chan []byte, 64)

	b.mu.Lock()
	b.clients[clientChan] = struct{}{}
	b.mu.Unlock()

	telemetry.Log.Debug().Msg("SSE dashboard client connected")

	// Send initial ping event
	initEv, _ := json.Marshal(TelemetryEvent{
		Type:      "connected",
		Timestamp: time.Now(),
		Data:      map[string]string{"message": "connected to liltok live telemetry"},
	})
	_, _ = w.Write([]byte(fmt.Sprintf("data: %s\n\n", string(initEv))))
	flusher.Flush()

	defer func() {
		b.mu.Lock()
		delete(b.clients, clientChan)
		close(clientChan)
		b.mu.Unlock()
		telemetry.Log.Debug().Msg("SSE dashboard client disconnected")
	}()

	notify := r.Context().Done()
	for {
		select {
		case <-notify:
			return
		case msg, ok := <-clientChan:
			if !ok {
				return
			}
			if _, err := w.Write(msg); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ClientCount returns the number of active connected clients.
func (b *Broadcaster) ClientCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.clients)
}
