package conformance

import (
	"context"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// virtualClock is the SDK's clock under the runner contract (section 5): sleeps register timers, and
// only the driver moves time.
type virtualClock struct {
	mu       sync.Mutex
	now      time.Duration
	timers   []*timer
	activity atomic.Int64
}

type timer struct {
	at   time.Duration
	done chan struct{}
}

func (c *virtualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Unix(0, 0).Add(c.now)
}

func (c *virtualClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	c.mu.Lock()
	t := &timer{at: c.now + d, done: make(chan struct{})}
	c.timers = append(c.timers, t)
	c.mu.Unlock()
	c.activity.Add(1)
	select {
	case <-t.done:
		c.activity.Add(1)
		return nil
	case <-ctx.Done():
		// A cancelled sleep no longer waits for time: its timer must not hold the driver back.
		c.mu.Lock()
		c.timers = slices.DeleteFunc(c.timers, func(other *timer) bool { return other == t })
		c.mu.Unlock()
		c.activity.Add(1)
		return ctx.Err()
	}
}

// earliest is the time of the first pending timer.
func (c *virtualClock) earliest() (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.timers) == 0 {
		return 0, false
	}
	first := c.timers[0].at
	for _, t := range c.timers {
		first = min(first, t.at)
	}
	return first, true
}

func (c *virtualClock) setNow(now time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = max(c.now, now)
}

func (c *virtualClock) current() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// fireDue fires every timer due at the current time, earliest first.
func (c *virtualClock) fireDue() {
	c.mu.Lock()
	var due []*timer
	rest := c.timers[:0]
	for _, t := range c.timers {
		if t.at <= c.now {
			due = append(due, t)
		} else {
			rest = append(rest, t)
		}
	}
	c.timers = rest
	c.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return due[i].at < due[j].at })
	for _, t := range due {
		close(t.done)
	}
}
