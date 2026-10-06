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

// fakeClock never waits: it records what the SDK asked to sleep and moves its own time forward.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

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
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return nil
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

func (s *scriptedServer) request(i int) recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[i]
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
