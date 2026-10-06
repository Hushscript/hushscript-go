package hushscript

import (
	"context"
	"time"
)

// Clock is the SDK's source of time. Everything that waits (backoff, pacing, polling) goes through
// Sleep, so a test or the conformance runner can inject a clock that never really waits.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// Sleep waits for d, or returns the context's error as soon as the context is done.
	Sleep(ctx context.Context, d time.Duration) error
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
