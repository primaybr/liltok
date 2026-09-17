package telemetry

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestNewLoggerJSON(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := NewLogger("debug", "json", buf)

	logger.Info().Str("module", "test").Msg("hello json")

	out := buf.String()
	if !strings.Contains(out, `"level":"info"`) {
		t.Errorf("expected level info in output: %s", out)
	}
	if !strings.Contains(out, `"module":"test"`) {
		t.Errorf("expected module test in output: %s", out)
	}
	if !strings.Contains(out, `"message":"hello json"`) {
		t.Errorf("expected message in output: %s", out)
	}
}

func TestNewLoggerConsole(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := NewLogger("warn", "console", buf)

	logger.Debug().Msg("should not appear")
	if buf.Len() > 0 {
		t.Errorf("expected debug to be filtered out at warn level, got: %s", buf.String())
	}

	logger.Warn().Msg("warning message")
	if !strings.Contains(buf.String(), "warning message") {
		t.Errorf("expected warning message to be logged: %s", buf.String())
	}
}

func TestInitGlobalLogger(t *testing.T) {
	InitGlobalLogger("trace", "console")
	if Log.GetLevel() != zerolog.TraceLevel {
		t.Errorf("expected global log level trace, got %v", Log.GetLevel())
	}
}
