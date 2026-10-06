package hushscript

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"
)

type logLabelKey struct{}

// withLogLabel names a request in the log by something other than its URL. Direct-to-storage part
// uploads use it ("r2:part/3"), because their query string is a signature.
func withLogLabel(ctx context.Context, label string) context.Context {
	return context.WithValue(ctx, logLabelKey{}, label)
}

// resolveLogger picks the logger: an explicit one, else standard error at debug level when
// HUSHSCRIPT_LOG=debug, else none.
func resolveLogger(c *config) error {
	level := strings.ToLower(strings.TrimSpace(os.Getenv("HUSHSCRIPT_LOG")))
	switch level {
	case "", "off":
	case "debug":
		if c.logger == nil {
			c.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
		}
	default:
		return newConfigError("HUSHSCRIPT_LOG must be off or debug.")
	}
	return nil
}

// attempt is what one try of a request tells the log.
type attempt struct {
	method     string
	path       string
	status     int
	number     int
	duration   time.Duration
	retryDelay time.Duration
}

// logAttempt writes one metadata line: method, path, status (0 when there was no response),
// attempt number, duration and the delay before the next try. Nothing else leaves the request.
func (c config) logAttempt(ctx context.Context, a attempt) {
	if c.logger == nil {
		return
	}
	c.logger.LogAttrs(ctx, slog.LevelDebug, "hushscript request",
		slog.String("method", a.method),
		slog.String("path", a.path),
		slog.Int("status", a.status),
		slog.Int("attempt", a.number),
		slog.Int64("duration_ms", a.duration.Milliseconds()),
		slog.Int64("retry_delay_ms", a.retryDelay.Milliseconds()),
	)
}
