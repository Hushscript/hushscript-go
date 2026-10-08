package hushscript

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func debugLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func logLines(buf *bytes.Buffer) []string {
	text := strings.TrimSpace(buf.String())
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func TestRetriesExactlyTheStatusesOfTheSafeClass(t *testing.T) {
	for status, retried := range map[int]bool{
		429: true, 500: true, 502: true, 503: true, 504: true,
		400: false, 401: false, 404: false, 409: false, 501: false, 505: false,
	} {
		srv := newScriptedServer(t, step{status: status}, okBody)
		c := newTestClient(t, srv.URL, newFakeClock())
		_, err := do(t, c, http.MethodGet, "/v1/account", "")
		if retried != (err == nil) || srv.count() != map[bool]int{true: 2, false: 1}[retried] {
			t.Errorf("status %d: retried=%v, %d requests, err %v", status, retried, srv.count(), err)
		}
	}
}

func TestTreatsAnyNonSuccessStatusAsAnError(t *testing.T) {
	srv := newScriptedServer(t, step{status: 300})
	c := newTestClient(t, srv.URL, newFakeClock(), WithHTTPClient(&http.Client{}))
	if _, err := do(t, c, http.MethodGet, "/v1/account", ""); err == nil {
		t.Error("a 300 answer was returned as a success")
	}
}

func TestRetriesARequestWithoutABody(t *testing.T) {
	srv := newScriptedServer(t, step{status: 503}, okBody)
	c := newTestClient(t, srv.URL, newFakeClock())
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/v1/account", nil) //nolint:gocritic // a nil body is the case under test
	if err != nil {
		t.Fatal(err)
	}
	if _, err := send(c, req); err != nil || srv.count() != 2 {
		t.Errorf("%d requests, err %v", srv.count(), err)
	}
}

func TestBoundsWhatItReadsIntoMemory(t *testing.T) {
	srv := newScriptedServer(t, step{status: 200, body: "0123456789"})
	c := newTestClient(t, srv.URL, newFakeClock(), WithMaxRetries(0))
	c.core.doer.limit = 10
	if reply, err := do(t, c, http.MethodGet, "/v1/account", ""); err != nil || reply.Text != "0123456789" {
		t.Errorf("a body exactly at the limit: %q, %v", reply.Text, err)
	}
	c.core.doer.limit = 9
	_, err := do(t, c, http.MethodGet, "/v1/account", "")
	if e := asError(t, err); e.Kind != KindNetwork || !errors.Is(err, errResponseTooLarge) {
		t.Errorf("a body over the limit: %v", err)
	}
}

func TestReportsTheContentLengthOfTheBufferedBody(t *testing.T) {
	srv := newScriptedServer(t, okBody)
	c := newTestClient(t, srv.URL, newFakeClock())
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/v1/account", http.NoBody)
	resp, err := c.core.doer.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ContentLength != int64(len(`{"ok":true}`)) {
		t.Errorf("ContentLength = %d", resp.ContentLength)
	}
}

func TestSaysHowLongAnAttemptWaitedBeforeTimingOut(t *testing.T) {
	srv := newScriptedServer(t, step{status: 200, delay: 2 * time.Second})
	c := newTestClient(t, srv.URL, newFakeClock(), WithTimeout(80*time.Millisecond), WithMaxRetries(0))
	_, err := do(t, c, http.MethodGet, "/v1/account", "")
	if e := asError(t, err); e.Kind != KindNetwork || !strings.Contains(e.Message, "timed out after 80ms") {
		t.Errorf("error = %v", err)
	}
}

func TestSendsTheTokenOnlyOverTheSchemeOfTheBaseURL(t *testing.T) {
	var seen []http.Header
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r.Header.Clone())
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
	})
	c := newTestClient(t, "https://api.example.test", newFakeClock(), WithHTTPClient(&http.Client{Transport: transport}))
	for _, url := range []string{"https://api.example.test/v1/account", "http://api.example.test/v1/account"} {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
		if _, err := send(c, req); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 2 || seen[0].Get("Authorization") == "" || seen[1].Get("Authorization") != "" {
		t.Errorf("authorization by scheme: %v", seen)
	}
}

func TestLogsZeroDelayWhenNothingIsRetried(t *testing.T) {
	for name, s := range map[string]step{
		"client error":      {status: 404, body: jsonError("not_found", 6000)},
		"long retry-after":  {status: 503, headers: map[string]string{"Retry-After": "120"}},
		"exhausted retries": {status: 503},
	} {
		var buf bytes.Buffer
		srv := newScriptedServer(t, s)
		c := newTestClient(t, srv.URL, newFakeClock(), WithLogger(debugLogger(&buf)), WithMaxRetries(0))
		_, _ = do(t, c, http.MethodGet, "/v1/account", "")
		lines := logLines(&buf)
		if len(lines) != 1 || !strings.Contains(lines[0], "retry_delay_ms=0") {
			t.Errorf("%s: %v", name, lines)
		}
	}
	for name, path := range map[string]string{"purchase": "/v1/billing/purchase", "create": "/v1/uploads", "mutation": "/v1/org/members/u1"} {
		var buf bytes.Buffer
		srv := newScriptedServer(t, step{status: 403, body: jsonError("pat_forbidden", 2022)})
		c := newTestClient(t, srv.URL, newFakeClock(), WithLogger(debugLogger(&buf)))
		_, _ = do(t, c, http.MethodPost, path, `{}`)
		if lines := logLines(&buf); len(lines) != 1 || !strings.Contains(lines[0], "retry_delay_ms=0") {
			t.Errorf("%s: %v", name, lines)
		}
	}
	var buf bytes.Buffer
	srv := newScriptedServer(t, step{status: 503})
	c := newTestClient(t, srv.URL, newFakeClock(), WithLogger(debugLogger(&buf)), WithMaxRetries(1))
	_, _ = do(t, c, http.MethodPost, "/v1/billing/purchase", `{}`)
	_, _ = do(t, c, http.MethodPost, "/v1/uploads/j1/complete", `{}`)
	_, _ = do(t, c, http.MethodGet, "/v1/account", "")
	lines := logLines(&buf)
	if len(lines) != 6 {
		t.Fatalf("got %d log lines for six attempts: %v", len(lines), lines)
	}
	for _, line := range lines {
		if !strings.Contains(line, "retry_delay_ms=500") && !strings.Contains(line, "retry_delay_ms=0") {
			t.Errorf("unexpected delay: %s", line)
		}
	}
}

func TestLogsEveryNetworkAttemptWithItsDelay(t *testing.T) {
	var buf bytes.Buffer
	srv := newScriptedServer(t, step{drop: true}, okBody)
	c := newTestClient(t, srv.URL, newFakeClock(), WithLogger(debugLogger(&buf)))
	if _, err := do(t, c, http.MethodGet, "/v1/account", ""); err != nil {
		t.Fatal(err)
	}
	lines := logLines(&buf)
	if len(lines) != 2 || !strings.Contains(lines[0], "status=0") || !strings.Contains(lines[0], "retry_delay_ms=500") || !strings.Contains(lines[1], "status=200") {
		t.Errorf("lines = %v", lines)
	}
}

func TestLogsACancelledAttemptOnce(t *testing.T) {
	var buf bytes.Buffer
	srv := newScriptedServer(t, step{status: 200, delay: 5 * time.Second})
	c := newTestClient(t, srv.URL, newFakeClock(), WithLogger(debugLogger(&buf)))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _ = doContext(ctx, t, c, http.MethodGet, "/v1/account", "")
	if lines := logLines(&buf); len(lines) != 1 || !strings.Contains(lines[0], "status=0") {
		t.Errorf("lines = %v", lines)
	}
	buf.Reset()
	srv = newScriptedServer(t, step{status: 200, delay: 5 * time.Second})
	c = newTestClient(t, srv.URL, newFakeClock(), WithLogger(debugLogger(&buf)))
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _ = doContext(ctx, t, c, http.MethodPost, "/v1/agent/credentials/rotate", `{}`)
	if lines := logLines(&buf); len(lines) != 1 {
		t.Errorf("an unknown outcome is logged once: %v", lines)
	}
}

func TestCancellationDuringANetworkRetryBackoffAborts(t *testing.T) {
	srv := newScriptedServer(t, step{drop: true})
	c := newTestClient(t, srv.URL, cancellingClock{newFakeClock()})
	_, err := do(t, c, http.MethodGet, "/v1/account", "")
	if e := asError(t, err); e.Kind != KindAbort || !isContextError(err) || srv.count() != 1 {
		t.Errorf("kind %v, %d requests", e.Kind, srv.count())
	}
}

func TestCancellationWhilePacingAborts(t *testing.T) {
	srv := newScriptedServer(t, okBody)
	clock := newFakeClock()
	c := newTestClient(t, srv.URL, clock)
	for range createsPerWindow {
		if _, err := do(t, c, http.MethodPost, "/v1/uploads", `{"idempotency_key":"k"}`); err != nil {
			t.Fatal(err)
		}
	}
	c.core.doer.pacer.clock = cancellingClock{clock}
	_, err := do(t, c, http.MethodPost, "/v1/uploads", `{"idempotency_key":"k"}`)
	if e := asError(t, err); e.Kind != KindAbort || srv.count() != createsPerWindow {
		t.Errorf("kind %v, %d requests", e.Kind, srv.count())
	}
}

func TestBackoffAndJitterBoundaries(t *testing.T) {
	if got := backoffCeiling(-1); got != 500*time.Millisecond {
		t.Errorf("a negative attempt gave %v", got)
	}
	seen := map[time.Duration]bool{}
	for range 2000 {
		got := fullJitter(time.Nanosecond)
		if got < 0 || got > time.Nanosecond {
			t.Fatalf("jitter(1ns) = %v", got)
		}
		seen[got] = true
	}
	if !seen[0] || !seen[time.Nanosecond] {
		t.Errorf("jitter should reach both ends of its range: %v", seen)
	}
	if fullJitter(-time.Second) != 0 {
		t.Error("a negative ceiling gives no delay")
	}
}
