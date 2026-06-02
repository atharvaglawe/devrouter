package telemetry

import (
	"log"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// loggerOnce guards Setup() so repeated calls (tests, embedder bench
// harness, etc.) are no-ops after the first. We don't expose a Reset
// equivalent for the logger because no test asserts on slog output —
// metrics are the structured surface.
var (
	loggerOnce sync.Once
	logger     *slog.Logger
)

// Setup configures the package-level logger and (when JSON mode is
// enabled) redirects the stdlib log package through slog so every
// existing `log.Printf("[router] …", …)` call also lands in the
// structured stream. Safe to call more than once; subsequent calls
// are no-ops.
//
// Environment:
//
//	DEVROUTER_LOG_FORMAT=json      — emit slog records as JSON on stderr
//	DEVROUTER_LOG_LEVEL=debug|info|warn|error — minimum level (default info)
func Setup() {
	loggerOnce.Do(setupLogger)
}

func setupLogger() {
	level := parseLevel(os.Getenv("DEVROUTER_LOG_LEVEL"))
	format := strings.ToLower(strings.TrimSpace(os.Getenv("DEVROUTER_LOG_FORMAT")))

	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: level}
	switch format {
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, opts)
		// Bridge stdlib log -> slog so legacy log.Printf calls
		// (still ubiquitous across the codebase) become structured
		// records. Drop the date+microsecond flags the stdlib logger
		// was setting in cmd/router/main.go — slog records carry a
		// real `time` field already, doubling up would just bloat
		// every line. Prefix is also redundant once we're JSON.
		std := slog.NewLogLogger(handler, level)
		log.SetOutput(std.Writer())
		log.SetFlags(0)
		log.SetPrefix("")
	default:
		// Default: keep the existing text behaviour (stderr, micro-
		// timestamps, [devrouter] prefix). slog still works for new
		// callers that want structured fields; it just emits the
		// stdlib-compatible TextHandler form.
		handler = slog.NewTextHandler(os.Stderr, opts)
	}

	logger = slog.New(handler)
	slog.SetDefault(logger)
}

// Logger returns the configured slog.Logger. Always non-nil — if Setup
// hasn't been called yet, returns the slog default (which is what new
// code wanted anyway).
func Logger() *slog.Logger {
	if logger == nil {
		return slog.Default()
	}
	return logger
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
