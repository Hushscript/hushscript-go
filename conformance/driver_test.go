package conformance

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	settle        = 20 * time.Millisecond
	stallAfter    = 500 * time.Millisecond
	pollInterval  = 5 * time.Millisecond
	mockSecondsFn = time.Second
)

// countingTransport counts the requests the SDK has in flight, which the driver needs to tell an SDK
// that waits for a response from one that waits for time.
type countingTransport struct {
	inner    http.RoundTripper
	inFlight atomic.Int64
	activity atomic.Int64
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.inFlight.Add(1)
	t.activity.Add(1)
	resp, err := t.inner.RoundTrip(req)
	if err != nil {
		t.leave()
		return nil, err
	}
	resp.Body = &countedBody{ReadCloser: resp.Body, leave: t.leave}
	return resp, nil
}

func (t *countingTransport) leave() {
	t.inFlight.Add(-1)
	t.activity.Add(1)
}

// countedBody keeps a request in flight until the SDK has read the body to its end or closed it: the
// body is read after the headers arrive, and a stalled read must not look like a quiet SDK.
type countedBody struct {
	io.ReadCloser
	leave func()
	once  sync.Once
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(b.leave)
	}
	return n, err
}

func (b *countedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.leave)
	return err
}

// driver moves the mock clock and the SDK's virtual clock while a call is pending.
type driver struct {
	mock      *mockClient
	clock     *virtualClock
	transport *countingTransport
}

// activityCount is everything that shows the SDK is doing something.
func (d *driver) activityCount() int64 {
	return d.clock.activity.Load() + d.transport.activity.Load()
}

// waitQuiet returns true when the SDK has been quiet for the settle period: nothing in flight beyond
// the requests the mock holds for the clock, and no new timer. It returns false when done closes.
func (d *driver) waitQuiet(ctx context.Context, done <-chan struct{}) (bool, error) {
	quietSince := time.Now()
	last := d.activityCount()
	for {
		select {
		case <-done:
			return false, nil
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(pollInterval):
		}
		state, err := d.mock.state(ctx)
		if err != nil {
			return false, err
		}
		now := d.activityCount()
		if now != last || d.transport.inFlight.Load()-state.PendingHolds > 0 {
			last, quietSince = now, time.Now()
			continue
		}
		if time.Since(quietSince) >= settle {
			return true, nil
		}
	}
}

// mockSeconds converts mock seconds to a Duration, rounding to the nearest nanosecond.
func mockSeconds(seconds float64) time.Duration {
	return time.Duration(math.Round(seconds * float64(mockSecondsFn)))
}

var errStalled = errors.New("SDK stalled: nothing is in flight, held or scheduled, and the call is still pending")

// run drives the clocks until done closes.
func (d *driver) run(ctx context.Context, done <-chan struct{}) error {
	var idleSince time.Time
	var idleActivity int64
	for {
		quiet, err := d.waitQuiet(ctx, done)
		if err != nil || !quiet {
			return err
		}
		state, err := d.mock.state(ctx)
		if err != nil {
			return err
		}
		target, found := d.clock.earliest()
		if state.NextHoldAt != nil {
			hold := mockSeconds(*state.NextHoldAt)
			if !found || hold < target {
				target, found = hold, true
			}
		}
		if !found {
			if count := d.activityCount(); idleSince.IsZero() || count != idleActivity {
				idleSince, idleActivity = time.Now(), count
			}
			if time.Since(idleSince) >= stallAfter {
				return errStalled
			}
			continue
		}
		idleSince = time.Time{}
		if advance := target - d.clock.current(); advance > 0 {
			now, err := d.mock.advance(ctx, advance)
			if err != nil {
				return err
			}
			// The mock's own clock is the truth: the SDK's clock follows it to the nanosecond, so a
			// float step can never leave a timer or a hold one tick short.
			d.clock.setNow(max(target, mockSeconds(now)))
			// Deliver the held responses due now before any SDK timer due at the same instant.
			if quiet, err := d.waitQuiet(ctx, done); err != nil || !quiet {
				return err
			}
		}
		d.clock.fireDue()
	}
}
