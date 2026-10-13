package hushscript

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testSecret = "0123456789abcdef0123456789abcdef.fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	testToken  = "hsr1:eu:pat." + testSecret
)

// sleepBudget bounds how often a test clock is slept on, so an SDK loop that never ends fails its
// test by name instead of running into the package timeout.
type sleepBudget struct {
	calls, idle int
	exceeded    bool
}

// The most idle (zero or negative) sleeps in a row any SDK loop may ask for before it counts as spinning.
const maxIdleSleeps = 100

var errSleepBudget = fmt.Errorf("test clock: sleep budget exceeded: %w", context.Canceled)

// spend counts one Sleep(d) under the clock's lock and reports whether the budget is now exceeded.
func (b *sleepBudget) spend(d time.Duration, limit int) bool {
	b.calls++
	b.idle++
	if d > 0 {
		b.idle = 0
	}
	if b.calls > limit || b.idle > maxIdleSleeps {
		b.exceeded = true
	}
	return b.exceeded
}

// fakeClock never waits: it records what the SDK asked to sleep and moves its own time forward.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
	budget sleepBudget
}

// The most sleeps one fakeClock serves; no test retries or polls anywhere near this often.
const maxFakeSleeps = 2000

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.budget.spend(d, maxFakeSleeps) {
		return errSleepBudget
	}
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return nil
}

func (c *fakeClock) overran() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.budget.exceeded
}

func (c *fakeClock) slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

// step is one scripted answer of the test server.
type step struct {
	status  int
	headers map[string]string
	body    string
	drop    bool          // close the connection after reading the request
	cut     bool          // send the headers and part of the body, then close the connection
	delay   time.Duration // hold the answer
}

func jsonError(slug string, code int) string {
	return fmt.Sprintf(`{"error":%q,"code":%d}`, slug, code)
}

type recorded struct {
	method string
	path   string
	query  string
	header http.Header
	body   string
}

// scriptedServer answers its steps in order and repeats the last one.
type scriptedServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recorded
}

func newScriptedServer(t *testing.T, steps ...step) *scriptedServer {
	t.Helper()
	s := &scriptedServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.requests = append(s.requests, recorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(body)})
		index := len(s.requests) - 1
		s.mu.Unlock()
		current := steps[min(index, len(steps)-1)]
		if current.drop {
			if hijacker, ok := w.(http.Hijacker); ok {
				if conn, _, err := hijacker.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		if current.cut {
			if hijacker, ok := w.(http.Hijacker); ok {
				if conn, _, err := hijacker.Hijack(); err == nil {
					_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"par")
					_ = conn.Close()
				}
			}
			return
		}
		if current.delay > 0 {
			select {
			case <-time.After(current.delay):
			case <-r.Context().Done():
				return
			}
		}
		for key, value := range current.headers {
			w.Header().Set(key, value)
		}
		w.WriteHeader(current.status)
		_, _ = io.WriteString(w, current.body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *scriptedServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *scriptedServer) request(t *testing.T, i int) recorded {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.requests) {
		t.Fatalf("request %d of %d recorded", i, len(s.requests))
	}
	return s.requests[i]
}

// nth returns recorded[i], failing the test when fewer were recorded.
func nth[T any](t *testing.T, recorded []T, i int) T {
	t.Helper()
	if i >= len(recorded) {
		t.Fatalf("item %d of %d recorded: %+v", i, len(recorded), recorded)
	}
	return recorded[i]
}

// deadAddress is a loopback address that refuses connections.
func deadAddress(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return "http://" + addr
}

// withJitter makes the backoff deterministic: the longest delay the policy allows.
func withJitter(f func(time.Duration) time.Duration) Option {
	return func(c *config) error {
		c.jitter = f
		return nil
	}
}

func maxJitter(limit time.Duration) time.Duration { return limit }

// newTestClient builds a client against a test server with a fake clock and maximal jitter.
func newTestClient(t *testing.T, baseURL string, clock Clock, opts ...Option) *Client {
	t.Helper()
	all := []Option{WithToken(testToken), WithBaseURL(baseURL), WithClock(clock), withJitter(maxJitter)}
	c, err := New(append(all, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if bounded, ok := clock.(interface{ overran() bool }); ok {
		t.Cleanup(func() {
			if bounded.overran() {
				t.Errorf("the SDK slept on the test clock past its budget: a retry or polling loop did not end")
			}
		})
	}
	return c
}

func do(t *testing.T, c *Client, method, path, body string) (reply, error) {
	t.Helper()
	return doContext(context.Background(), t, c, method, path, body)
}

func doContext(ctx context.Context, t *testing.T, c *Client, method, path, body string) (reply, error) {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.core.cfg.baseURL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	return send(c, req)
}

// reply is a 2xx answer with its body already read, so tests never hold an open response.
type reply struct {
	StatusCode int
	Text       string
}

func send(c *Client, req *http.Request) (reply, error) {
	resp, err := c.core.doer.Do(req)
	if err != nil {
		return reply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return reply{StatusCode: resp.StatusCode, Text: string(body)}, err
}

func asError(t *testing.T, err error) *Error {
	t.Helper()
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v (%T) is not a *hushscript.Error", err, err)
	}
	return apiErr
}

// cancellingClock cancels nothing itself; it fails every sleep the way a cancelled context would.
type cancellingClock struct{ *fakeClock }

func (cancellingClock) Sleep(_ context.Context, _ time.Duration) error { return context.Canceled }

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func wantCoreError(t *testing.T, err error, kind ErrorKind, code string, status int) *Error {
	t.Helper()
	e := wantCode(t, err, kind, code)
	if e.Status != status {
		t.Fatalf("status %d, want %d: %v", e.Status, status, err)
	}
	return e
}

func assertCoreError(t *testing.T, err error, kind ErrorKind, code string, status int) {
	t.Helper()
	_ = wantCoreError(t, err, kind, code, status)
}

// A finite clock contract is used only synchronously, so its sentinel is recovered by the test.
type sleepContractViolation struct{ message string }

type clockStep struct {
	duration time.Duration
	advance  time.Duration
	err      error
}

type contractClock struct {
	now   time.Time
	steps []clockStep
	slept []time.Duration
}

func (c *contractClock) Now() time.Time { return c.now }
func (c *contractClock) Sleep(_ context.Context, d time.Duration) error {
	i := len(c.slept)
	c.slept = append(c.slept, d)
	if i >= len(c.steps) {
		panic(sleepContractViolation{fmt.Sprintf("unexpected Sleep(%v) after %d scripted sleeps", d, i)})
	}
	step := c.steps[i]
	if d != step.duration {
		panic(sleepContractViolation{fmt.Sprintf("Sleep[%d] = %v, want %v", i, d, step.duration)})
	}
	c.now = c.now.Add(step.advance)
	return step.err
}

func assertClockContract(t *testing.T, clock *contractClock, run func()) {
	t.Helper()
	defer func() {
		if raised := recover(); raised != nil {
			if failure, ok := raised.(sleepContractViolation); ok {
				t.Fatalf("clock contract: %s", failure.message)
			}
			t.Fatalf("panic during the contract run: %v", raised)
		}
	}()
	run()
	if len(clock.slept) != len(clock.steps) {
		t.Fatalf("Sleep sequence = %v, want %d scripted sleeps", clock.slept, len(clock.steps))
	}
}

func assertSDKDoesNotPanic(t *testing.T, run func()) {
	t.Helper()
	defer func() {
		if raised := recover(); raised != nil {
			if _, ok := raised.(sleepContractViolation); ok {
				panic(raised)
			}
			t.Fatalf("SDK heartbeat contract must not panic: %v", raised)
		}
	}()
	run()
}

func awaitUploadResult(t *testing.T, done <-chan error, api *fakeAPI, clock *virtualClock) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("upload result missing: heartbeat=%d complete=%d active parts=%d pending timers=%d sleeps=%v", api.count("heartbeat"), api.count("complete"), api.r2Active.Load(), clock.sleepers(), clock.sleeps())
		return nil
	}
}

func advanceHeartbeatDue(t *testing.T, clock *virtualClock, due time.Time) {
	t.Helper()
	waitFor(t, func() bool { return clock.sleepers() == 1 })
	at, ok := clock.earliest()
	if !ok || !at.Equal(due) {
		t.Fatalf("heartbeat timer due at %v, want %v", at, due)
	}
	clock.advanceTo(due.Add(-time.Nanosecond))
	if clock.sleepers() != 1 {
		t.Fatal("heartbeat timer fired before its due boundary")
	}
	clock.advanceTo(due)
}
