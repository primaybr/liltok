package proxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sync"
)

var bufPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

// StreamTapFunc is invoked with each streamed chunk as it is sent to the client.
type StreamTapFunc func(chunk []byte)

// StreamSSE streams Server-Sent Events from upstream to downstream client with minimal buffering.
// It simultaneously feeds chunks into tapFn to accumulate the completion for caching and accounting.
func StreamSSE(w http.ResponseWriter, upstreamBody io.ReadCloser, tapFn StreamTapFunc) error {
	defer upstreamBody.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("response writer does not support flushing")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Bypass Nginx proxy buffering if in front

	scanner := bufio.NewScanner(upstreamBody)
	// Allow large SSE chunk lines (up to 512KB for large tool calls/thinking traces)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 512*1024)

	lineBuf := bufPool.Get().(*bytes.Buffer)
	lineBuf.Reset()
	defer bufPool.Put(lineBuf)

	for scanner.Scan() {
		line := scanner.Bytes()
		lineBuf.Write(line)
		lineBuf.WriteString("\n")

		// An empty line signifies the end of an SSE event
		if len(line) == 0 {
			eventBytes := lineBuf.Bytes()
			if _, err := w.Write(eventBytes); err != nil {
				return err
			}
			flusher.Flush()

			if tapFn != nil {
				tapFn(eventBytes)
			}
			lineBuf.Reset()
		}
	}

	// Flush any trailing line in buffer
	if lineBuf.Len() > 0 {
		eventBytes := lineBuf.Bytes()
		if _, err := w.Write(eventBytes); err != nil {
			return err
		}
		flusher.Flush()
		if tapFn != nil {
			tapFn(eventBytes)
		}
	}

	return scanner.Err()
}
