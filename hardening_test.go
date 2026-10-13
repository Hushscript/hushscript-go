package hushscript

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPurchaseFailureWhileBackingOffPreservesWhetherAnAttemptWasSent(t *testing.T) {
	for _, sent := range []bool{false, true} {
		t.Run(fmt.Sprint(sent), func(t *testing.T) {
			state := &purchaseState{}
			ctx := context.WithValue(context.Background(), purchaseStateKey{}, state)
			failure := error(&net.OpError{Op: "dial", Err: errors.New("connection refused")})
			if sent {
				failure = errors.New("connection reset")
			}
			var calls atomic.Int32
			c := newTestClient(t, "http://127.0.0.1:1", cancellingClock{newFakeClock()}, WithHTTPClient(failingAfterBody(&calls, failure)))
			_, err := doContext(ctx, t, c, http.MethodPost, "/v1/billing/purchase", `{}`)
			if asError(t, err).Kind != KindAbort || state.mayHaveSent != sent || calls.Load() != 1 {
				t.Errorf("error=%v sent=%v calls=%d", err, state.mayHaveSent, calls.Load())
			}
		})
	}
}

func TestPurchaseBodyReplayFailureDoesNotEraseAnEarlierSentAttempt(t *testing.T) {
	for _, sent := range []bool{false, true} {
		t.Run(fmt.Sprint(sent), func(t *testing.T) {
			state := &purchaseState{}
			ctx := context.WithValue(context.Background(), purchaseStateKey{}, state)
			failure := errors.New("body could not be reopened")
			var calls atomic.Int32
			c := newTestClient(t, "http://127.0.0.1:1", newFakeClock(), WithHTTPClient(failingAfterBody(&calls, errors.New("response lost"))))
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:1/v1/billing/purchase", strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			bodies := 0
			req.GetBody = func() (io.ReadCloser, error) {
				bodies++
				if sent && bodies == 1 {
					return io.NopCloser(strings.NewReader(`{}`)), nil
				}
				return nil, failure
			}
			response, err := c.core.doer.Do(req)
			if response != nil {
				_ = response.Body.Close()
			}
			if !errors.Is(err, failure) || state.mayHaveSent != sent {
				t.Errorf("error=%v sent=%v, want sent=%v", err, state.mayHaveSent, sent)
			}
		})
	}
}

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
	if got := foreign.request(t, 0).header.Get("Authorization"); got != "" {
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
		// The key decodes, then a second spelling of it does not: the body is not a clean create body.
		"key then a number": {`{"idempotency_key":"abc","idempotency_key":1}`, 1},
		"key":               {`{"size_bytes":1,"idempotency_key":"abc"}`, defaultMaxRetries + 1},
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
					if got := srv.request(t, i).body; !strings.Contains(got, `"idempotency_key":"abc"`) {
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

// dialingClient is a real http.Transport whose dial is dial, so the connection hooks fire.
func dialingClient(dial func(ctx context.Context) (net.Conn, error)) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dial(ctx)
	}}}
}

func TestAFailedConnectionThatNoErrorTypeProvesIsStillNotSent(t *testing.T) {
	failure := errors.New("custom dial failure") // not a *net.OpError: only the GetConn/GotConn hooks prove nothing left
	for path, want := range map[string]int32{"/v1/org/members": int32(defaultMaxRetries) + 1, "/v1/agent/credentials/rotate": 1} {
		var dials atomic.Int32
		client := dialingClient(func(context.Context) (net.Conn, error) {
			dials.Add(1)
			return nil, failure
		})
		c := newTestClient(t, "http://127.0.0.1:1", newFakeClock(), WithHTTPClient(client))
		_, err := do(t, c, http.MethodPost, path, `{}`)
		if e := asError(t, err); e.Kind != KindNetwork || dials.Load() != want || !errors.Is(err, failure) {
			t.Errorf("%s: kind %v after %d dials (want %s after %d), cause reachable %t", path, e.Kind, dials.Load(), KindNetwork, want, errors.Is(err, failure))
		}
	}
}

func TestAFailedTLSHandshakeIsNotSent(t *testing.T) {
	var hellos atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the handshake should have failed")
	}))
	//nolint:gosec // the server stops at TLS 1.2 so a client that asks for 1.3 fails its handshake
	srv.TLS = &tls.Config{MaxVersion: tls.VersionTLS12, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		hellos.Add(1)
		return nil, nil //nolint:nilnil // nil keeps the server's own config
	}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	transport, ok := srv.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("httptest client transport is not an *http.Transport")
	}
	transport = transport.Clone()
	transport.TLSClientConfig.MinVersion = tls.VersionTLS13 // the server stops at 1.2: a protocol_version alert, not a certificate error
	for path, want := range map[string]int32{"/v1/org/members": int32(defaultMaxRetries) + 1, "/v1/agent/credentials/rotate": 1} {
		hellos.Store(0)
		c := newTestClient(t, srv.URL, newFakeClock(), WithHTTPClient(&http.Client{Transport: transport}))
		_, err := do(t, c, http.MethodPost, path, `{}`)
		e := asError(t, err)
		if connectionSetupFailure(err) {
			t.Fatalf("%s: %v proves itself a setup failure; the test needs one only the hooks can see", path, err)
		}
		if e.Kind != KindNetwork || hellos.Load() != want {
			t.Errorf("%s: kind %v after %d handshakes, want %s after %d", path, e.Kind, hellos.Load(), KindNetwork, want)
		}
	}
}

func TestACancelledDialOfAClaimOrRotateIsAnAbortNotAnUnknownOutcome(t *testing.T) {
	for _, path := range []string{"/v1/agent/accounts/sg_1/claim", "/v1/agent/credentials/rotate", "/v1/agent/accounts"} {
		var dials atomic.Int32
		client := dialingClient(func(ctx context.Context) (net.Conn, error) {
			dials.Add(1)
			<-ctx.Done()
			return nil, ctx.Err()
		})
		c := newTestClient(t, "http://127.0.0.1:1", newFakeClock(), WithHTTPClient(client))
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, err := doContext(ctx, t, c, http.MethodPost, path, `{}`)
		cancel()
		if e := asError(t, err); e.Kind != KindAbort || e.Code != "aborted" || !isContextError(err) || dials.Load() != 1 {
			t.Errorf("%s: kind %v code %q, context cause %t, %d dials", path, e.Kind, e.Code, isContextError(err), dials.Load())
		}
	}
}

func TestAMutationCancelledInFlightIsAnAbort(t *testing.T) {
	srv := newScriptedServer(t, step{status: 200, delay: 5 * time.Second})
	c := newTestClient(t, srv.URL, newFakeClock())
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := doContext(ctx, t, c, http.MethodPatch, "/v1/org/members/u1", `{}`)
	if e := asError(t, err); e.Kind != KindAbort || e.Code != "aborted" || !isContextError(err) || srv.count() != 1 {
		t.Errorf("kind %v code %q, context cause %t, %d requests", e.Kind, e.Code, isContextError(err), srv.count())
	}
}

func TestAnUploadCreateBodyThatCannotBeReadIsKeyless(t *testing.T) {
	srv := newScriptedServer(t, step{status: 503, body: "{}"})
	c := newTestClient(t, srv.URL, newFakeClock())
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/v1/uploads", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = nil // a body that cannot be reopened, so there is nothing to peek
	if _, err = send(c, req); asError(t, err).Status != 503 || srv.count() != 1 {
		t.Errorf("a create without a body: %v after %d requests, want one 503", err, srv.count())
	}

	reopen := errors.New("the body cannot be reopened")
	req, err = http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/v1/uploads", strings.NewReader(`{"idempotency_key":"abc"}`))
	if err != nil {
		t.Fatal(err)
	}
	opened := 0
	req.GetBody = func() (io.ReadCloser, error) {
		opened++
		return nil, reopen
	}
	func() {
		defer func() {
			if raised := recover(); raised != nil {
				t.Fatalf("a create whose body cannot be reopened panicked: %v", raised)
			}
		}()
		_, err = send(c, req)
	}()
	// Keyless, so a mutation: nothing was sent, so every attempt is tried, and each fails to reopen the body.
	if e := asError(t, err); e.Kind != KindNetwork || !errors.Is(err, reopen) || srv.count() != 1 || opened != defaultMaxRetries+2 {
		t.Errorf("a create whose body cannot be reopened: %v, %d requests, %d opens", err, srv.count(), opened)
	}
}

// trackedBody counts how often it is closed and how many bytes were read from it.
type trackedBody struct {
	r      io.Reader
	closes atomic.Int32
	read   atomic.Int64
}

func (b *trackedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read.Add(int64(n))
	return n, err
}

func (b *trackedBody) Close() error {
	b.closes.Add(1)
	return nil
}

// endless reads x forever.
type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// failsWithData returns the whole buffer and an error in one call.
type failsWithData struct{ err error }

func (f failsWithData) Read(p []byte) (int, error) {
	n, _ := endless{}.Read(p)
	return n, f.err
}

func TestEveryResponseBodyIsClosedAndReadNoFurtherThanTheLimit(t *testing.T) {
	broken := errors.New("connection reset mid-body")
	for _, tc := range []struct {
		name     string
		body     io.Reader
		read     int64
		tooLarge bool
		cause    error
	}{
		{"fits", strings.NewReader("012345678"), 9, false, nil},
		{"endless", endless{}, 10, true, nil},
		{"error with data past the limit", failsWithData{broken}, 10, false, broken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &trackedBody{r: tc.body}
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Header: http.Header{}, Body: body}, nil
			})}
			c := newTestClient(t, "http://127.0.0.1:1", newFakeClock(), WithHTTPClient(client), WithMaxRetries(0))
			c.core.doer.limit = 9
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:1/v1/account", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := c.core.doer.Do(req)
			if resp != nil {
				defer func() { _ = resp.Body.Close() }()
			}
			if body.closes.Load() != 1 || body.read.Load() != tc.read {
				t.Errorf("closed %d times after reading %d bytes, want once after %d", body.closes.Load(), body.read.Load(), tc.read)
			}
			switch {
			case tc.cause == nil && !tc.tooLarge:
				if err != nil || resp.ContentLength != tc.read {
					t.Errorf("ContentLength = %d, err %v; want %d", resp.ContentLength, err, tc.read)
				}
			case tc.tooLarge:
				if !errors.Is(err, errResponseTooLarge) {
					t.Errorf("err = %v, want the too-large error", err)
				}
			default:
				if !errors.Is(err, tc.cause) || errors.Is(err, errResponseTooLarge) || asError(t, err).Kind != KindNetwork {
					t.Errorf("err = %v, want the read error itself", err)
				}
			}
		})
	}
}

func TestEveryPeekedUploadCreateBodyIsClosed(t *testing.T) {
	var opened, closed atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		_, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	c := newTestClient(t, "http://127.0.0.1:1", newFakeClock(), WithHTTPClient(client))
	const create = `{"idempotency_key":"abc"}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://127.0.0.1:1/v1/uploads", strings.NewReader(create))
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = func() (io.ReadCloser, error) {
		opened.Add(1)
		return closeCounter{io.NopCloser(strings.NewReader(create)), &closed}, nil
	}
	if _, err := send(c, req); err != nil {
		t.Fatal(err)
	}
	if opened.Load() != 2 || closed.Load() != 2 {
		t.Errorf("%d bodies opened (the key peek and the attempt), %d closed; want 2 and 2", opened.Load(), closed.Load())
	}
}

type closeCounter struct {
	io.ReadCloser
	closed *atomic.Int32
}

func (c closeCounter) Close() error {
	c.closed.Add(1)
	return c.ReadCloser.Close()
}

func TestAStatusBelow200IsAnError(t *testing.T) {
	for _, status := range []int{100, 199} {
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		})}
		c := newTestClient(t, "http://127.0.0.1:1", newFakeClock(), WithHTTPClient(client), WithMaxRetries(0))
		reply, err := do(t, c, http.MethodGet, "/v1/account", "")
		if err == nil || asError(t, err).Status != status {
			t.Errorf("status %d: reply %+v, err %v; want an error with that status", status, reply, err)
		}
	}
}

func TestAForcedClassIsPacedWhenTheRequestOrTheClassIsAnUploadCreate(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1", newFakeClock())
	for _, tc := range []struct {
		method, path string
		class        retryClass
		paced        bool
	}{
		{http.MethodPost, "/v1/uploads", classSafe, true},
		{http.MethodPut, "/elsewhere/part", classUploadCreate, true},
		{http.MethodPut, "/elsewhere/part", classUpload, false},
	} {
		ctx := withRetryClass(context.Background(), tc.class)
		req, err := http.NewRequestWithContext(ctx, tc.method, "http://127.0.0.1:1"+tc.path, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		if class, paced := c.core.doer.plan(req); class != tc.class || paced != tc.paced {
			t.Errorf("%s %s forced %v: class %v paced %t, want paced %t", tc.method, tc.path, tc.class, class, paced, tc.paced)
		}
	}
}

func TestAnAttemptTimeoutOverrideAppliesOnlyAboveZero(t *testing.T) {
	srv := newScriptedServer(t, okBody)
	c := newTestClient(t, srv.URL, newFakeClock(), WithTimeout(5*time.Second), WithMaxRetries(0))
	for _, override := range []time.Duration{0, -1} {
		if _, err := doContext(withAttemptTimeout(context.Background(), override), t, c, http.MethodGet, "/v1/account", ""); err != nil {
			t.Errorf("override %v should fall back to the configured timeout: %v", override, err)
		}
	}
	_, err := doContext(withAttemptTimeout(context.Background(), time.Nanosecond), t, c, http.MethodGet, "/v1/account", "")
	if e := asError(t, err); e.Kind != KindNetwork || e.Message != "The request to the Hushscript API timed out after 1ns." {
		t.Errorf("a 1ns override: %v", err)
	}
}

func TestWithoutQueryCutsAtTheFirstQueryOrFragment(t *testing.T) {
	for raw, want := range map[string]string{
		"https://h/p":       "https://h/p",
		"https://h/p?x=1":   "https://h/p",
		"https://h/p#f":     "https://h/p",
		"https://h/p#f?x=1": "https://h/p",
		"?x=1":              "",
		"#f":                "",
		"":                  "",
	} {
		if got := withoutQuery(raw); got != want {
			t.Errorf("withoutQuery(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestAnUnreplayableBodyIsNeverRetriedEvenUnderAForcedCreateClass(t *testing.T) {
	srv := newScriptedServer(t, step{status: 429, body: jsonError("too_many_active_jobs", 5001)}, okBody)
	c := newTestClient(t, srv.URL, newFakeClock())
	ctx := withRetryClass(context.Background(), classUploadCreate)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/uploads", io.MultiReader(strings.NewReader(`{"idempotency_key":"k"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = send(c, req); asError(t, err).Code != "too_many_active_jobs" || srv.count() != 1 {
		t.Errorf("%v after %d requests, want the first answer and no retry", err, srv.count())
	}
}

func TestAURLErrorWithoutACauseIsNamedAsItself(t *testing.T) {
	got := underlyingError(&url.Error{Op: "Get", URL: "https://h/p"})
	if !strings.HasPrefix(got, "*url.Error: Get ") {
		t.Errorf("underlyingError = %q", got)
	}
}
