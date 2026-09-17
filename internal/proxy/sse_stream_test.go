package proxy

import (
	"bytes"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

type flusherRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (f *flusherRecorder) Flush() {
	f.flushed = true
}

func TestStreamSSE(t *testing.T) {
	sseData := "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" World\"}}]}\n\n" +
		"data: [DONE]\n\n"

	body := io.NopCloser(strings.NewReader(sseData))
	rec := &flusherRecorder{
		ResponseRecorder: httptest.NewRecorder(),
	}

	var tappedChunks [][]byte
	tapFn := func(chunk []byte) {
		tappedChunks = append(tappedChunks, bytes.Clone(chunk))
	}

	err := StreamSSE(rec, body, tapFn)
	if err != nil {
		t.Fatalf("StreamSSE returned unexpected error: %v", err)
	}

	if !rec.flushed {
		t.Errorf("expected flusher to be called")
	}

	output := rec.Body.String()
	if !strings.Contains(output, "Hello") || !strings.Contains(output, "World") || !strings.Contains(output, "[DONE]") {
		t.Errorf("output does not contain expected chunks: %s", output)
	}

	if len(tappedChunks) < 3 {
		t.Errorf("expected at least 3 tapped chunks, got %d", len(tappedChunks))
	}
}
