package hushscript

import (
	"context"
	"sync"
	"time"
)

const (
	createsPerWindow = 4
	createWindow     = time.Minute
)

// pacer is the token bucket for POST /v1/uploads: 4 creates per 60 s per client (spec section 7.5).
// The API limits creates per user and answers 429 without Retry-After, so the SDK keeps below it.
type pacer struct {
	clock Clock

	mu     sync.Mutex
	starts []time.Time
}

func newPacer(clock Clock) *pacer {
	return &pacer{clock: clock}
}

// acquire waits until a create may start, then records it.
func (p *pacer) acquire(ctx context.Context) error {
	for {
		wait := p.reserve()
		if wait == 0 {
			return nil
		}
		if err := p.clock.Sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// reserve records a create and returns 0 when the window has room, else how long until it has.
func (p *pacer) reserve() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock.Now()
	kept := p.starts[:0]
	for _, start := range p.starts {
		if now.Sub(start) < createWindow {
			kept = append(kept, start)
		}
	}
	p.starts = kept
	if len(p.starts) < createsPerWindow {
		p.starts = append(p.starts, now)
		return 0
	}
	return p.starts[0].Add(createWindow).Sub(now)
}
