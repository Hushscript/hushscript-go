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

const (
	logOff   = "off"
	logDebug = "debug"
)

// resolveLogger picks the logger. An explicit WithLogLevel beats HUSHSCRIPT_LOG; "off" removes any
// logger, "debug" uses the given one or standard error. Without either, a WithLogger logger is used
// as given, and HUSHSCRIPT_LOG=debug turns on standard error.
func resolveLogger(c *config) error {
	level := c.logLevel
	if level == "" {
		switch env := strings.ToLower(strings.TrimSpace(os.Getenv("HUSHSCRIPT_LOG"))); env {
		case "", logOff:
		case logDebug:
			level = logDebug
		default:
			return newConfigError("HUSHSCRIPT_LOG must be off or debug.")
		}
	}
	switch {
	case level == logOff:
		c.logger = nil
	case level == logDebug && c.logger == nil:
		c.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
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
