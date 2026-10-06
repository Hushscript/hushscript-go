package hushscript

import (
	"context"
	"errors"
	"sync"
	"time"
)

const heartbeatInterval = 120 * time.Second

// heartbeat keeps an uploading job alive: one POST every 120 s from the start until stop (spec 8.5).
type heartbeat struct {
	done chan struct{}

	mu     sync.Mutex
	failed *Error
}

// startHeartbeat runs the loop in a goroutine. A refused token cancels the upload through cancel and
// is kept for stop to return, because every error the cancellation causes downstream is a symptom.
func (c *core) startHeartbeat(ctx context.Context, cancel context.CancelFunc, job string) *heartbeat {
	h := &heartbeat{done: make(chan struct{})}
	go func() {
		defer close(h.done)
		h.run(ctx, cancel, c, job)
	}()
	return h
}

// run sends a heartbeat every 120 s, scheduled from the start so a slow request does not stretch the
// interval. Failures are ignored, except wrong_state (the job moved on: stop quietly) and
// unauthorized (abort the upload).
func (h *heartbeat) run(ctx context.Context, cancel context.CancelFunc, c *core, job string) {
	clock := c.cfg.clock
	next := clock.Now().Add(heartbeatInterval)
	for {
		if err := clock.Sleep(ctx, max(0, next.Sub(clock.Now()))); err != nil {
			return
		}
		// Late by more than an interval (a suspended process): start again from now instead of bursting.
		next = laterOf(next, clock.Now()).Add(heartbeatInterval)
		_, err := c.api.HeartbeatUploadWithResponse(ctx, job)
		if err == nil || ctx.Err() != nil {
			continue
		}
		var failure *Error
		if !errors.As(err, &failure) {
			continue
		}
		if failure.Kind == KindAuthentication {
			h.mu.Lock()
			h.failed = failure
			h.mu.Unlock()
			cancel()
			return
		}
		if failure.Code == "wrong_state" {
			return
		}
	}
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// stop waits for the loop to end (the caller has cancelled its context) and returns the failure that
// ended the upload, if the heartbeat caused one.
func (h *heartbeat) stop() *Error {
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.failed
}
