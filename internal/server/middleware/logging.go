package middleware

import (
	"net/http"
	"time"

	"github.com/primaybr/liltok/internal/telemetry"
)

type responseWriterWrapper struct {
	http.ResponseWriter
	statusCode int
	bytesWritten int64
}

func (rw *responseWriterWrapper) WriteHeader(statusCode int) {
	rw.statusCode = statusCode
	rw.ResponseWriter.WriteHeader(statusCode)
}

func (rw *responseWriterWrapper) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)
	rw.bytesWritten += int64(n)
	return n, err
}

func (rw *responseWriterWrapper) Flush() {
	if flusher, ok := rw.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Logger middleware logs incoming HTTP requests with timing and status codes.
func Logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapper := &responseWriterWrapper{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
		}

		next.ServeHTTP(wrapper, r)

		duration := time.Since(start)
		reqID := GetRequestID(r.Context())

		telemetry.Log.Info().
			Str("request_id", reqID).
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", wrapper.statusCode).
			Int64("bytes", wrapper.bytesWritten).
			Dur("duration", duration).
			Str("remote_addr", r.RemoteAddr).
			Msg("HTTP Request completed")
	})
}
