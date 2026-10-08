package hushscript

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// failingAfterBody is a transport that is not an http.Transport, so no connection hook ever fires for
// it. It reads the whole request body, as a transport does when it writes it, then fails.
func failingAfterBody(calls *atomic.Int32, failure error) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Body != nil {
			_, _ = io.ReadAll(r.Body)
		}
		return nil, failure
	})}
}

func TestACustomTransportThatFailsAfterTheBodyCountsAsSent(t *testing.T) {
	reset := errors.New("connection reset by peer")
	for _, tc := range []struct {
		path    string
		kind    ErrorKind
		hint    string
		wantRun int32
	}{
		{"/v1/agent/accounts/sg_1/claim", KindOutcomeUnknown, claimOutcomeHint, 1},
		{"/v1/agent/credentials/rotate", KindOutcomeUnknown, rotateOutcomeHint, 1},
		{"/v1/org/members", KindNetwork, "", 1},
		{"/v1/billing/cards/default", KindNetwork, "", 1},
	} {
		var calls atomic.Int32
		c := newTestClient(t, "http://127.0.0.1:1", newFakeClock(), WithHTTPClient(failingAfterBody(&calls, reset)))
		_, err := do(t, c, http.MethodPost, tc.path, `{"x":1}`)
		e := asError(t, err)
		if e.Kind != tc.kind || e.Hint != tc.hint || calls.Load() != tc.wantRun {
			t.Errorf("%s: kind %v hint %q after %d calls", tc.path, e.Kind, e.Hint, calls.Load())
		}
		if !errors.Is(err, reset) {
			t.Errorf("%s: the cause is not reachable", tc.path)
		}
	}
}

func TestOnlyProvableConnectionSetupFailuresCountAsNotSent(t *testing.T) {
	setup := map[string]error{
		"dial":     &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
		"dns":      &net.DNSError{Err: "no such host", Name: "api.example"},
		"x509":     x509.UnknownAuthorityError{},
		"wrapped":  fmt.Errorf("transport: %w", &net.OpError{Op: "dial", Err: errors.New("timeout")}),
		"hostname": x509.HostnameError{Certificate: &x509.Certificate{}, Host: "x"},
	}
	for name, failure := range setup {
		var calls atomic.Int32
		c := newTestClient(t, "http://127.0.0.1:1", newFakeClock(), WithHTTPClient(failingAfterBody(&calls, failure)))
		_, err := do(t, c, http.MethodPost, "/v1/org/members", `{}`)
		if e := asError(t, err); e.Kind != KindNetwork || calls.Load() != int32(defaultMaxRetries)+1 {
			t.Errorf("%s: kind %v after %d calls, want every retry used", name, e.Kind, calls.Load())
		}
		calls.Store(0)
		_, err = do(t, c, http.MethodPost, "/v1/agent/credentials/rotate", `{}`)
		if e := asError(t, err); e.Kind != KindNetwork || calls.Load() != 1 {
			t.Errorf("%s: a rotate that never left is a plain network error, got %v", name, e.Kind)
		}
	}
	var calls atomic.Int32
	read := &net.OpError{Op: "read", Err: errors.New("connection reset")}
	c := newTestClient(t, "http://127.0.0.1:1", newFakeClock(), WithHTTPClient(failingAfterBody(&calls, read)))
	if _, err := do(t, c, http.MethodPost, "/v1/org/members", `{}`); calls.Load() != 1 || asError(t, err).Kind != KindNetwork {
		t.Errorf("a read error after the body is sent: %d calls", calls.Load())
	}
}

func TestNeverFollowsRedirectsUnderACallersClient(t *testing.T) {
	var other atomic.Int32
	var leaked atomic.Value
	target := newScriptedServer(t, okBody)
	target.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other.Add(1)
		leaked.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	})
	api := newScriptedServer(t, step{status: http.StatusTemporaryRedirect, headers: map[string]string{"Location": target.URL + "/stolen"}})
	callers := &http.Client{} // follows redirects by default, and carries Authorization to a same-host port
	c := newTestClient(t, api.URL, newFakeClock(), WithHTTPClient(callers))
	_, err := do(t, c, http.MethodPost, "/v1/agent/credentials/rotate", `{}`)
	e := asError(t, err)
	if e.Status != http.StatusTemporaryRedirect {
		t.Errorf("a 3xx should be an error, got %+v", e)
	}
	if other.Load() != 0 || leaked.Load() != nil {
		t.Errorf("%d requests reached the redirect target, Authorization %v", other.Load(), leaked.Load())
	}
	if api.count() != 1 {
		t.Errorf("rotate was made %d times", api.count())
	}
	if callers.CheckRedirect != nil {
		t.Error("the caller's client was modified")
	}
}

func TestSendsTheTokenOnlyToTheConfiguredOrigin(t *testing.T) {
	foreign := newScriptedServer(t, okBody)
	c := newTestClient(t, deadAddress(t), newFakeClock())
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, foreign.URL+"/part", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	if _, err := send(c, req); err != nil {
		t.Fatal(err)
	}
	if got := foreign.request(0).header.Get("Authorization"); got != "" {
		t.Errorf("a foreign origin received Authorization %q", got)
	}
}

func TestAnUploadCreateIsRetriedOnlyWithAnIdempotencyKey(t *testing.T) {
	for name, tc := range map[string]struct {
		body  string
		count int
	}{
		"no key":    {`{"size_bytes":1,"duration_seconds":20}`, 1},
		"empty key": {`{"size_bytes":1,"idempotency_key":""}`, 1},
		"not json":  {`size_bytes=1`, 1},
		"key":       {`{"size_bytes":1,"idempotency_key":"abc"}`, defaultMaxRetries + 1},
	} {
		for kind, failing := range map[string]step{"503": {status: 503, body: "{}"}, "drop": {drop: true}} {
			srv := newScriptedServer(t, failing)
			c := newTestClient(t, srv.URL, newFakeClock())
			_, _ = do(t, c, http.MethodPost, "/v1/uploads", tc.body)
			if srv.count() != tc.count {
				t.Errorf("%s, %s: %d requests, want %d", name, kind, srv.count(), tc.count)
			}
			if strings.Contains(tc.body, `"idempotency_key":"abc"`) {
				for i := range srv.count() {
					if got := srv.request(i).body; !strings.Contains(got, `"idempotency_key":"abc"`) {
						t.Errorf("%s, %s: request %d reached the server as %q, without the idempotency key", name, kind, i, got)
					}
				}
			}
		}
	}
}

func TestAnUploadCreateWithoutAKeyIsStillPaced(t *testing.T) {
	srv := newScriptedServer(t, okBody)
	clock := newFakeClock()
	c := newTestClient(t, srv.URL, clock)
	for i := range createsPerWindow + 1 {
		if _, err := do(t, c, http.MethodPost, "/v1/uploads", `{"size_bytes":1}`); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if waits := clock.slept(); len(waits) != 1 || waits[0] != time.Minute {
		t.Errorf("create waits = %v, want exactly [1m0s]", waits)
	}
}

func TestClassifiesOnTheEscapedPath(t *testing.T) {
	if got := classify(http.MethodPost, "/v1/agent/accounts/a%2Fb/claim"); got != classNever {
		t.Errorf("a signup id with a slash gave class %v", got)
	}
	srv := newScriptedServer(t, step{drop: true})
	c := newTestClient(t, srv.URL, newFakeClock())
	resp, err := c.API().ClaimAgentAccountWithBody(context.Background(), "a/b", "application/json", strings.NewReader(`{"claim_secret":"s"}`))
	if resp != nil {
		_ = resp.Body.Close()
	}
	e := asError(t, err)
	if e.Kind != KindOutcomeUnknown || e.Hint != claimOutcomeHint || srv.count() != 1 {
		t.Errorf("kind %v hint %q after %d requests", e.Kind, e.Hint, srv.count())
	}
}

func TestCompleteAndPartUploadsRetryAnyServerError(t *testing.T) {
	for name, tc := range map[string]struct {
		method, path string
		count        int
	}{
		"complete":  {http.MethodPost, "/v1/uploads/j1/complete", defaultMaxRetries + 1},
		"part":      {http.MethodPut, "/v1/uploads/j1/parts/2", defaultMaxRetries + 1},
		"heartbeat": {http.MethodPost, "/v1/uploads/j1/heartbeat", 1},
		"part urls": {http.MethodGet, "/v1/uploads/j1/part-urls", 1},
	} {
		for _, status := range []int{501, 507} {
			srv := newScriptedServer(t, step{status: status, body: "{}"})
			c := newTestClient(t, srv.URL, newFakeClock())
			_, _ = do(t, c, tc.method, tc.path, "x")
			if srv.count() != tc.count {
				t.Errorf("%s %d: %d requests, want %d", name, status, srv.count(), tc.count)
			}
		}
	}
}

func TestAPerRequestClassAndTimeoutOverrideTheDefaults(t *testing.T) {
	srv := newScriptedServer(t, step{status: 503, body: "{}"})
	c := newTestClient(t, srv.URL, newFakeClock())
	ctx := withRetryClass(context.Background(), classUpload)
	if _, err := doContext(ctx, t, c, http.MethodPut, "/elsewhere/part", "x"); err == nil || srv.count() != defaultMaxRetries+1 {
		t.Errorf("class override: %d requests", srv.count())
	}

	slow := newScriptedServer(t, step{status: 200, body: "{}", delay: 300 * time.Millisecond})
	c = newTestClient(t, slow.URL, newFakeClock(), WithTimeout(40*time.Millisecond), WithMaxRetries(0))
	_, err := do(t, c, http.MethodGet, "/v1/account", "")
	if e := asError(t, err); e.Kind != KindNetwork || !strings.Contains(e.Message, "40ms") {
		t.Errorf("the configured timeout should apply: %v", err)
	}
	long := withAttemptTimeout(context.Background(), 5*time.Second)
	if _, err := doContext(long, t, c, http.MethodGet, "/v1/account", ""); err != nil {
		t.Errorf("the timeout override should apply: %v", err)
	}
}

// echoingError is what a transport that copies request headers into its errors would return.
type echoingError struct{ text string }

func (e *echoingError) Error() string { return e.text }

func TestNoErrorReachableThroughTheCauseChainPrintsTheToken(t *testing.T) {
	sentinel := errors.New("sentinel")
	echo := &echoingError{text: "transport said: Authorization: Bearer " + testToken}
	var calls atomic.Int32
	c := newTestClient(t, "http://127.0.0.1:1", newFakeClock(),
		WithHTTPClient(failingAfterBody(&calls, fmt.Errorf("%w: %w", sentinel, echo))), WithMaxRetries(0))
	_, err := do(t, c, http.MethodGet, "/v1/account", "")
	if !errors.Is(err, sentinel) {
		t.Error("errors.Is no longer reaches the cause")
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		if strings.Contains(current.Error(), testSecret) {
			t.Errorf("%T prints the token: %s", current, current.Error())
		}
	}
	var echoed *echoingError
	if errors.As(err, &echoed) {
		t.Errorf("errors.As handed out an error that prints the token: %s", echoed.Error())
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && strings.Contains(urlErr.Error(), testSecret) {
		t.Errorf("errors.As handed out a url.Error that prints the token: %s", urlErr.Error())
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), testSecret) {
		t.Error("the error prints the token")
	}
}

func TestACleanTransportErrorStaysReachableThroughErrorsAs(t *testing.T) {
	c := newTestClient(t, deadAddress(t), newFakeClock(), WithMaxRetries(0))
	_, err := do(t, c, http.MethodGet, "/v1/account", "")
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "dial" {
		t.Errorf("errors.As did not find the dial error in %v", err)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Error("errors.As did not find a net.Error")
	}
}

func TestANetworkErrorTellsWhyAndNeverShowsAForeignURL(t *testing.T) {
	c := newTestClient(t, deadAddress(t), newFakeClock(), WithMaxRetries(0))
	_, err := do(t, c, http.MethodGet, "/v1/account", "")
	if text := err.Error(); !strings.Contains(text, "connection refused") {
		t.Errorf("Error() = %q, want the cause in it", text)
	}

	dead := strings.TrimPrefix(deadAddress(t), "http://")
	req, reqErr := http.NewRequestWithContext(context.Background(), http.MethodPut, "http://"+dead+"/part?X-Amz-Signature=topsecret", strings.NewReader("x"))
	if reqErr != nil {
		t.Fatal(reqErr)
	}
	_, err = send(c, req)
	if text := err.Error(); strings.Contains(text, "topsecret") || !strings.Contains(text, "connection refused") {
		t.Errorf("Error() = %q", text)
	}
}

func TestAZeroClientFormatsAndFailsEveryCallWithoutPanicking(t *testing.T) {
	var zero Client
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%d"} {
		text := fmt.Sprintf(verb, zero)
		if !strings.Contains(text, "unconfigured") {
			t.Errorf("formatted as %q", text)
		}
	}
	resp, err := zero.API().GetAccount(context.Background())
	if resp != nil {
		_ = resp.Body.Close()
	}
	if e := asError(t, err); e.Kind != KindConfiguration {
		t.Errorf("kind %v", e.Kind)
	}
}

func TestUserAgentToolchainVersions(t *testing.T) {
	for _, tc := range []struct{ runtimeVersion, version string }{
		{"go1.27.1", "1.27.1"},
		{"go1.27.1-X:nodwarf5", "1.27.1"},
		{"devel go1.28-abc +def", "1.28"},
		{"devel +abc", "devel"},
		{"", "devel"},
		{" \t\u2003", "devel"},
		{"go1.27", "1.27"},
		{"go1", "devel"},
	} {
		t.Run(tc.runtimeVersion, func(t *testing.T) {
			t.Parallel()
			want := "hushscript-go/" + sdkVersion() + " go/" + tc.version
			if got := userAgentForVersion(tc.runtimeVersion); got != want {
				t.Errorf("user agent = %q, want %q", got, want)
			}
		})
	}
}
