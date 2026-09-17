package telemetry

import (
	"io"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

var Log zerolog.Logger

func init() {
	// Initialize with safe default console logger
	Log = NewLogger("info", "console", os.Stdout)
}

// NewLogger instantiates a configured zerolog.Logger instance.
func NewLogger(levelStr, format string, output io.Writer) zerolog.Logger {
	var lvl zerolog.Level
	switch strings.ToLower(levelStr) {
	case "trace":
		lvl = zerolog.TraceLevel
	case "debug":
		lvl = zerolog.DebugLevel
	case "info":
		lvl = zerolog.InfoLevel
	case "warn", "warning":
		lvl = zerolog.WarnLevel
	case "error":
		lvl = zerolog.ErrorLevel
	default:
		lvl = zerolog.InfoLevel
	}

	var w io.Writer = output
	if strings.ToLower(format) == "console" {
		w = zerolog.ConsoleWriter{
			Out:        output,
			TimeFormat: time.RFC3339,
		}
	}

	logger := zerolog.New(w).Level(lvl).With().Timestamp().Logger()
	return logger
}

// InitGlobalLogger configures the package-level global logger.
func InitGlobalLogger(levelStr, format string) {
	Log = NewLogger(levelStr, format, os.Stdout)
}
