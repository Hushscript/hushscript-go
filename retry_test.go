package hushscript

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

var okBody = step{status: 200, body: `{"ok":true}`}

func rateLimited(headers map[string]string) step {
	return step{status: 429, headers: headers, body: jsonError("rate_limited", 5000)}
}

func TestClassifiesOperationsByTheRetryTable(t *testing.T) {
	cases := []struct {
		method, path string
		want         retryClass
	}{
		{"GET", "/v1/account", classSafe},
		{"GET", "/v1/transcripts/tr_1", classSafe},
		{"GET", "/v1/uploads/j1/part-urls", classSafe},
		{"HEAD", "/v1/account", classSafe},
		{"POST", "/v1/uploads/j1/heartbeat", classSafe},
		{"POST", "/v1/uploads/j1/complete", classUpload},
		{"PUT", "/v1/uploads/j1/parts/4", classUpload},
		{"POST", "/v1/uploads", classUploadCreate},
		{"POST", "/v1/billing/purchase", classPurchase},
		{"POST", "/v1/billing/setup-session", classPurchase},
		{"POST", "/v1/agent/accounts/sg_1/claim", classNever},
		{"POST", "/v1/agent/credentials/rotate", classNever},
		{"POST", "/v1/agent/accounts", classNever},
		{"PATCH", "/v1/org/members/u1", classMutate},
		{"DELETE", "/v1/org/transcripts/t1/hold", classMutate},
		{"POST", "/v1/org/transcripts/t1/hold", classMutate},
		{"POST", "/v1/billing/cards/default", classMutate},
		{"POST", "/v1/anything/new", classMutate},
	}
	for _, tc := range cases {
		if got := classify(tc.method, tc.path); got != tc.want {
			t.Errorf("%s %s = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestSendsTheTokenAndTheUserAgentToTheAPIHostOnly(t *testing.T) {
	api := newScriptedServer(t, okBody)
	other := newScriptedServer(t, okBody)
	c := newTestClient(t, api.URL, newFakeClock())
	_, err := do(t, c, http.MethodGet, "/v1/account", "")
	if err != nil {
		t.Fatal(err)
	}
	h := api.request(t, 0).header
	if h.Get("Authorization") != "Bearer "+testToken || h.Get("User-Agent") != userAgent() {
		t.Errorf("headers = %v", h)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, other.URL+"/r2/part/1", strings.NewReader("bytes"))
	_, err = send(c, req)
	if err != nil {
		t.Fatal(err)
	}
	if got := other.request(t, 0).header.Get("Authorization"); got != "" {
		t.Errorf("the token went to another host: %q", got)
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("the caller's request was modified")
	}
}

func TestRetriesASafeRequestWithFullJitterBackoff(t *testing.T) {
	srv := newScriptedServer(t, step{status: 503}, step{status: 502}, okBody)
	clock := newFakeClock()
	c := newTestClient(t, srv.URL, clock)
	resp, err := do(t, c, http.MethodGet, "/v1/account", "")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != `{"ok":true}` || resp.StatusCode != http.StatusOK {
		t.Errorf("response = %d %s", resp.StatusCode, resp.Text)
	}
	if srv.count() != 3 {
		t.Errorf("requests = %d", srv.count())
	}
	want := []time.Duration{500 * time.Millisecond, time.Second}
	got := clock.slept()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("sleeps = %v, want the jitter ceilings %v", got, want)
	}
}

func TestCapsTheBackoffAtEightSeconds(t *testing.T) {
	for attempt, want := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second} {
		if got := backoffCeiling(attempt); got != want {
			t.Errorf("attempt %d ceiling = %v, want %v", attempt, got, want)
		}
	}
	if got := backoffCeiling(500); got != 8*time.Second {
		t.Errorf("a huge attempt number overflowed: %v", got)
	}
}

func TestDefaultJitterStaysInsideTheCeiling(t *testing.T) {
	for range 200 {
		if got := fullJitter(time.Second); got < 0 || got > time.Second {
			t.Fatalf("jitter = %v", got)
		}
	}
	if fullJitter(0) != 0 {
		t.Error("jitter of nothing is nothing")
	}
}

func TestGivesUpAfterMaxRetriesWithTheLastError(t *testing.T) {
	srv := newScriptedServer(t, step{status: 503, body: "unavailable"})
	c := newTestClient(t, srv.URL, newFakeClock(), WithMaxRetries(2))
	_, err := do(t, c, http.MethodGet, "/v1/account", "")
	e := asError(t, err)
	if srv.count() != 3 || e.Status != 503 || e.Kind != KindAPI {
		t.Errorf("requests %d, error %+v", srv.count(), e)
	}
	srv = newScriptedServer(t, step{status: 503})
	c = newTestClient(t, srv.URL, newFakeClock(), WithMaxRetries(0))
	if _, err := do(t, c, http.MethodGet, "/v1/account", ""); err == nil || srv.count() != 1 {
		t.Errorf("maxRetries 0 made %d requests", srv.count())
	}
}

func TestDoesNotRetryAClientError(t *testing.T) {
	srv := newScriptedServer(t, step{status: 404, body: jsonError("not_found", 6000)})
	c := newTestClient(t, srv.URL, newFakeClock())
	_, err := do(t, c, http.MethodGet, "/v1/transcripts/x", "")
	if e := asError(t, err); e.Kind != KindNotFound || srv.count() != 1 {
		t.Errorf("kind %v, %d requests", e.Kind, srv.count())
	}
}

func TestHonorsRetryAfterAndOverridesTheBackoff(t *testing.T) {
	srv := newScriptedServer(t, rateLimited(map[string]string{"Retry-After": "7"}), okBody)
	clock := newFakeClock()
	c := newTestClient(t, srv.URL, clock)
	if _, err := do(t, c, http.MethodGet, "/v1/account", ""); err != nil {
		t.Fatal(err)
	}
	if got := clock.slept(); len(got) != 1 || got[0] != 7*time.Second {
		t.Errorf("sleeps = %v", got)
	}
}

func TestDoesNotWaitForRetryAfterAbove60Seconds(t *testing.T) {
	srv := newScriptedServer(t, rateLimited(map[string]string{"Retry-After": "61"}))
	clock := newFakeClock()
	c := newTestClient(t, srv.URL, clock)
	_, err := do(t, c, http.MethodGet, "/v1/account", "")
	e := asError(t, err)
	if e.Kind != KindRateLimit || e.Code != "rate_limited" || e.RetryAfter != 61*time.Second {
		t.Errorf("error = %+v", e)
	}
	if srv.count() != 1 || len(clock.slept()) != 0 {
		t.Errorf("requests %d, sleeps %v", srv.count(), clock.slept())
	}
	srv = newScriptedServer(t, rateLimited(map[string]string{"Retry-After": "60"}), okBody)
	clock = newFakeClock()
	c = newTestClient(t, srv.URL, clock)
	if _, err := do(t, c, http.MethodGet, "/v1/account", ""); err != nil {
		t.Fatalf("60 s is still waited for: %v", err)
	}
	if got := clock.slept(); len(got) != 1 || got[0] != 60*time.Second {
		t.Errorf("sleeps = %v", got)
	}
}

func TestRetryAfterAbove60OnAServerErrorIsReturnedAsIsWithoutWaiting(t *testing.T) {
	srv := newScriptedServer(t, step{status: 503, headers: map[string]string{"Retry-After": "120"}})
	clock := newFakeClock()
	c := newTestClient(t, srv.URL, clock)
	_, err := do(t, c, http.MethodGet, "/v1/account", "")
	e := asError(t, err)
	if e.Status != 503 || e.RetryAfter != 120*time.Second || srv.count() != 1 || len(clock.slept()) != 0 {
		t.Errorf("error %+v, requests %d, sleeps %v", e, srv.count(), clock.slept())
	}
}

func TestRetriesAPurchaseAtMostOnceAndNeverOnA4xx(t *testing.T) {
	path := "/v1/billing/purchase"
	srv := newScriptedServer(t, step{status: 502}, step{status: 502}, okBody)
	c := newTestClient(t, srv.URL, newFakeClock())
	if _, err := do(t, c, http.MethodPost, path, `{"pack":"300min"}`); err == nil || srv.count() != 2 {
		t.Errorf("502,502: %d requests, err %v", srv.count(), err)
	}
	for _, status := range []int{403, 429, 500, 504} {
		srv = newScriptedServer(t, step{status: status, body: jsonError("x", 1)}, okBody)
		c = newTestClient(t, srv.URL, newFakeClock())
		if _, err := do(t, c, http.MethodPost, path, `{}`); err == nil || srv.count() != 1 {
			t.Errorf("%d: %d requests, err %v", status, srv.count(), err)
		}
	}
	srv = newScriptedServer(t, step{status: 503}, okBody)
	c = newTestClient(t, srv.URL, newFakeClock())
	if _, err := do(t, c, http.MethodPost, "/v1/billing/setup-session", `{}`); err != nil || srv.count() != 2 {
		t.Errorf("503 then ok: %d requests, err %v", srv.count(), err)
	}
}

func TestRetriesAPurchaseAfterANetworkErrorOnceWithTheSameBody(t *testing.T) {
	srv := newScriptedServer(t, step{drop: true}, okBody)
	c := newTestClient(t, srv.URL, newFakeClock())
	if _, err := do(t, c, http.MethodPost, "/v1/billing/purchase", `{"idempotency_key":"k-1"}`); err != nil {
		t.Fatal(err)
	}
	if srv.count() != 2 || srv.request(t, 0).body != srv.request(t, 1).body || srv.request(t, 1).body != `{"idempotency_key":"k-1"}` {
		t.Errorf("requests %d, bodies %q / %q", srv.count(), srv.request(t, 0).body, srv.request(t, 1).body)
	}
	srv = newScriptedServer(t, step{drop: true})
	c = newTestClient(t, srv.URL, newFakeClock())
	if _, err := do(t, c, http.MethodPost, "/v1/billing/purchase", `{}`); err == nil || srv.count() != 2 {
		t.Errorf("purchase retries are capped at one: %d requests", srv.count())
	}
}

func TestNeverRetriesClaimOrRotateAndReportsAnUnknownOutcome(t *testing.T) {
	for _, path := range []string{"/v1/agent/accounts/sg_1/claim", "/v1/agent/credentials/rotate"} {
		srv := newScriptedServer(t, step{drop: true}, okBody)
		c := newTestClient(t, srv.URL, newFakeClock())
		_, err := do(t, c, http.MethodPost, path, `{}`)
		e := asError(t, err)
		var cause net.Error
		if e.Kind != KindOutcomeUnknown || e.Code != "outcome_unknown" || e.Status != 0 || !errors.As(err, &cause) || srv.count() != 1 {
			t.Errorf("%s: kind %v, %d requests", path, e.Kind, srv.count())
		}
		if strings.Contains(path, "claim") && !strings.Contains(e.Hint, "agent_claim_invalid") {
			t.Errorf("claim hint = %q", e.Hint)
		}
		if strings.Contains(path, "rotate") && !strings.Contains(e.Hint, "old token may already be revoked") {
			t.Errorf("rotate hint = %q", e.Hint)
		}
		srv = newScriptedServer(t, step{status: 503}, okBody)
		c = newTestClient(t, srv.URL, newFakeClock())
		_, err = do(t, c, http.MethodPost, path, `{}`)
		if e := asError(t, err); e.Kind != KindAPI || e.Code != "http_503" || e.Status != 503 || srv.count() != 1 {
			t.Errorf("%s on 503: %+v, %d requests", path, e, srv.count())
		}
	}
}

func TestNeverRetriesSignupAndReportsAnUnknownOutcome(t *testing.T) {
	srv := newScriptedServer(t, step{drop: true}, okBody)
	c := newTestClient(t, srv.URL, newFakeClock())
	_, err := do(t, c, http.MethodPost, "/v1/agent/accounts", `{}`)
	if e := asError(t, err); e.Kind != KindOutcomeUnknown || e.Hint != "" || srv.count() != 1 {
		t.Errorf("a lost signup is an unknown outcome with no hint: kind %v hint %q, %d requests", e.Kind, e.Hint, srv.count())
	}
}

func TestRefusedConnectionOfAnUnsentClaimIsANetworkErrorNotAnUnknownOutcome(t *testing.T) {
	for _, path := range []string{"/v1/agent/accounts/sg_1/claim", "/v1/agent/credentials/rotate"} {
		c := newTestClient(t, deadAddress(t), newFakeClock())
		_, err := do(t, c, http.MethodPost, path, `{}`)
		e := wantCode(t, err, KindNetwork, "network_error")
		if e.Status != 0 {
			t.Errorf("%s: status %d on an unsent request", path, e.Status)
		}
	}
}

func TestRetriesOtherMutationsOnlyWhenNothingWasSent(t *testing.T) {
	clock := newFakeClock()
	c := newTestClient(t, deadAddress(t), clock, WithMaxRetries(2))
	_, err := do(t, c, http.MethodPatch, "/v1/org/members/u1", `{}`)
	if e := asError(t, err); e.Kind != KindNetwork || len(clock.slept()) != 2 {
		t.Errorf("a refused connection should be retried: kind %v, sleeps %v", e.Kind, clock.slept())
	}
	srv := newScriptedServer(t, step{drop: true}, okBody)
	c = newTestClient(t, srv.URL, newFakeClock())
	_, err = do(t, c, http.MethodPatch, "/v1/org/members/u1", `{}`)
	if e := asError(t, err); e.Kind != KindNetwork || srv.count() != 1 {
		t.Errorf("a lost response must not be replayed: kind %v, %d requests", e.Kind, srv.count())
	}
	srv = newScriptedServer(t, step{status: 503}, okBody)
	c = newTestClient(t, srv.URL, newFakeClock())
	if _, err := do(t, c, http.MethodPost, "/v1/org/transcripts/t1/hold", `{}`); err == nil || srv.count() != 1 {
		t.Errorf("a 503 on a mutation is not retried: %d requests", srv.count())
	}
}

func TestRetriesSafeRequestsAfterALostResponse(t *testing.T) {
	srv := newScriptedServer(t, step{drop: true}, okBody)
	c := newTestClient(t, srv.URL, newFakeClock())
	if _, err := do(t, c, http.MethodGet, "/v1/account", ""); err != nil || srv.count() != 2 {
		t.Errorf("%d requests, err %v", srv.count(), err)
	}
	srv = newScriptedServer(t, step{drop: true})
	c = newTestClient(t, srv.URL, newFakeClock())
	_, err := do(t, c, http.MethodGet, "/v1/account", "")
	if e := asError(t, err); e.Kind != KindNetwork || srv.count() != 4 {
		t.Errorf("kind %v, %d requests", e.Kind, srv.count())
	}
}

func TestTimesOutAnAttemptAndRetriesIt(t *testing.T) {
	srv := newScriptedServer(t, step{status: 200, delay: 2 * time.Second}, okBody)
	c := newTestClient(t, srv.URL, newFakeClock(), WithTimeout(100*time.Millisecond))
	if _, err := do(t, c, http.MethodGet, "/v1/account", ""); err != nil || srv.count() != 2 {
		t.Errorf("%d requests, err %v", srv.count(), err)
	}
}

func TestReadsTheWholeBodyInsideTheAttemptTimeout(t *testing.T) {
	srv := newScriptedServer(t, okBody)
	c := newTestClient(t, srv.URL, newFakeClock(), WithTimeout(5*time.Second))
	resp, err := do(t, c, http.MethodGet, "/v1/account", "")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if resp.Text != `{"ok":true}` {
		t.Errorf("body %q", resp.Text)
	}
}

func TestCancellationDuringBackoffReturnsAnAbortError(t *testing.T) {
	srv := newScriptedServer(t, step{status: 503})
	c := newTestClient(t, srv.URL, cancellingClock{newFakeClock()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := doContext(ctx, t, c, http.MethodGet, "/v1/account", "")
	e := asError(t, err)
	if e.Kind != KindAbort {
		t.Errorf("kind = %v", e.Kind)
	}
	if !isContextError(err) {
		t.Error("errors.Is(err, context.Canceled) should hold")
	}
}

func TestCancellationBeforeTheRequestReturnsAnAbortErrorWithoutSending(t *testing.T) {
	srv := newScriptedServer(t, okBody)
	c := newTestClient(t, srv.URL, newFakeClock())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := doContext(ctx, t, c, http.MethodGet, "/v1/account", "")
	if e := asError(t, err); e.Kind != KindAbort || srv.count() != 0 {
		t.Errorf("kind %v, %d requests", e.Kind, srv.count())
	}
}

func TestCancellationDuringARequestReturnsAnAbortError(t *testing.T) {
	srv := newScriptedServer(t, step{status: 200, delay: 5 * time.Second})
	c := newTestClient(t, srv.URL, newFakeClock())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := doContext(ctx, t, c, http.MethodGet, "/v1/account", "")
	if e := asError(t, err); e.Kind != KindAbort || !isContextError(err) {
		t.Errorf("kind %v, err %v", e.Kind, err)
	}
}

func TestReplaysARequestBodyOnRetry(t *testing.T) {
	srv := newScriptedServer(t, step{status: 503}, okBody)
	c := newTestClient(t, srv.URL, newFakeClock())
	if _, err := do(t, c, http.MethodPost, "/v1/uploads/j1/complete", `{"parts":[{"n":1,"etag":"e"}]}`); err != nil {
		t.Fatal(err)
	}
	if srv.request(t, 1).body != `{"parts":[{"n":1,"etag":"e"}]}` {
		t.Errorf("retried body = %q", srv.request(t, 1).body)
	}
}

func TestDoesNotRetryABodyItCannotReplay(t *testing.T) {
	srv := newScriptedServer(t, step{status: 503}, okBody)
	c := newTestClient(t, srv.URL, newFakeClock())
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, c.core.cfg.baseURL+"/v1/uploads/j1/parts/1", io.NopCloser(strings.NewReader("part")))
	if _, err := send(c, req); err == nil || srv.count() != 1 {
		t.Errorf("%d requests, err %v", srv.count(), err)
	}
}

func TestNeverSetsAnIdempotencyKeyHeaderItself(t *testing.T) {
	srv := newScriptedServer(t, okBody)
	c := newTestClient(t, srv.URL, newFakeClock())
	_, err := do(t, c, http.MethodPost, "/v1/uploads", `{"idempotency_key":"k"}`)
	if err != nil {
		t.Fatal(err)
	}
	for key := range srv.request(t, 0).header {
		if strings.Contains(strings.ToLower(key), "idempotency") {
			t.Errorf("the SDK set %s, which would make net/http replay the POST on its own", key)
		}
	}
}

func TestDoesNotFollowRedirectsWithTheToken(t *testing.T) {
	target := newScriptedServer(t, okBody)
	srv := newScriptedServer(t, step{status: 302, headers: map[string]string{"Location": target.URL + "/stolen"}})
	c := newTestClient(t, srv.URL, newFakeClock())
	_, err := do(t, c, http.MethodGet, "/v1/account", "")
	if e := asError(t, err); e.Status != 302 || target.count() != 0 {
		t.Errorf("status %d, %d requests reached the redirect target", e.Status, target.count())
	}
}

func TestPacesUploadCreatesToFourPerMinute(t *testing.T) {
	srv := newScriptedServer(t, okBody)
	clock := newFakeClock()
	c := newTestClient(t, srv.URL, clock)
	for i := range 5 {
		_, err := do(t, c, http.MethodPost, "/v1/uploads", `{"idempotency_key":"k"}`)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if srv.count() != 5 {
		t.Fatalf("requests = %d", srv.count())
	}
	if got := clock.slept(); len(got) != 1 || got[0] != time.Minute {
		t.Errorf("the fifth create should wait one window: sleeps %v", got)
	}
	_, err := do(t, c, http.MethodGet, "/v1/account", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(clock.slept()) != 1 {
		t.Error("only creates are paced")
	}
}

func TestPacingSharesOneBucketPerClientAndAdvancesTheWindow(t *testing.T) {
	clock := newFakeClock()
	p := newPacer(clock)
	ctx := context.Background()
	for range 4 {
		if err := p.acquire(ctx); err != nil {
			t.Fatal(err)
		}
		_ = clock.Sleep(ctx, 10*time.Second)
	}
	if err := p.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if got := clock.slept(); got[len(got)-1] != 20*time.Second {
		t.Errorf("the oldest create left the window 20 s later, slept %v", got)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for range 4 {
		_ = p.acquire(ctx)
	}
	if err := p.acquire(cancelled); err == nil {
		t.Error("a cancelled wait returned nil")
	}
}

func TestBacksOffFifteenThirtySixtyWhenACreateIsRateLimitedWithoutRetryAfter(t *testing.T) {
	srv := newScriptedServer(t, rateLimited(nil), rateLimited(nil), rateLimited(nil), okBody)
	clock := newFakeClock()
	c := newTestClient(t, srv.URL, clock)
	_, err := do(t, c, http.MethodPost, "/v1/uploads", `{"idempotency_key":"k"}`)
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second}
	got := clock.slept()
	if srv.count() != 4 || len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("requests %d, sleeps %v", srv.count(), got)
	}
}

func TestRaisesAfterThreeRateLimitBackoffsOnCreate(t *testing.T) {
	srv := newScriptedServer(t, rateLimited(nil))
	c := newTestClient(t, srv.URL, newFakeClock())
	_, err := do(t, c, http.MethodPost, "/v1/uploads", `{"idempotency_key":"k"}`)
	if e := asError(t, err); e.Kind != KindRateLimit || srv.count() != 4 {
		t.Errorf("kind %v, %d requests", e.Kind, srv.count())
	}
}

func TestUsesRetryAfterOnCreateWhenTheServerSendsIt(t *testing.T) {
	srv := newScriptedServer(t, rateLimited(map[string]string{"Retry-After": "9"}), okBody)
	clock := newFakeClock()
	c := newTestClient(t, srv.URL, clock)
	if _, err := do(t, c, http.MethodPost, "/v1/uploads", `{"idempotency_key":"k"}`); err != nil {
		t.Fatal(err)
	}
	if got := clock.slept(); len(got) != 1 || got[0] != 9*time.Second {
		t.Errorf("sleeps = %v", got)
	}
}

func TestWaitsThirtySecondsBetweenTriesWhenTooManyJobsAreActive(t *testing.T) {
	busy := step{status: 429, body: jsonError("too_many_active_jobs", 5001)}
	steps := make([]step, 0, 6)
	for range 5 {
		steps = append(steps, busy)
	}
	srv := newScriptedServer(t, append(steps, okBody)...)
	clock := newFakeClock()
	c := newTestClient(t, srv.URL, clock)
	if _, err := do(t, c, http.MethodPost, "/v1/uploads", `{"idempotency_key":"k"}`); err != nil {
		t.Fatal(err)
	}
	waits := 0
	for _, d := range clock.slept() {
		if d == 30*time.Second {
			waits++
		}
	}
	if waits != 5 || srv.count() != 6 {
		t.Errorf("%d waits of 30 s, %d requests", waits, srv.count())
	}
}

func TestRaisesAfterTenTriesWhenTooManyJobsStayActive(t *testing.T) {
	srv := newScriptedServer(t, step{status: 429, body: jsonError("too_many_active_jobs", 5001)})
	c := newTestClient(t, srv.URL, newFakeClock())
	_, err := do(t, c, http.MethodPost, "/v1/uploads", `{"idempotency_key":"k"}`)
	if e := asError(t, err); e.Code != "too_many_active_jobs" || e.Kind != KindRateLimit || srv.count() != 10 {
		t.Errorf("code %q kind %v, %d requests", e.Code, e.Kind, srv.count())
	}
}

func TestMaxRetriesZeroDisablesEveryCreateBackoff(t *testing.T) {
	srv := newScriptedServer(t, rateLimited(nil))
	c := newTestClient(t, srv.URL, newFakeClock(), WithMaxRetries(0))
	if _, err := do(t, c, http.MethodPost, "/v1/uploads", `{"idempotency_key":"k"}`); err == nil || srv.count() != 1 {
		t.Errorf("%d requests", srv.count())
	}
}

func TestRetriesAServerErrorOnCreateWithTheSameBody(t *testing.T) {
	srv := newScriptedServer(t, step{status: 500}, okBody)
	c := newTestClient(t, srv.URL, newFakeClock())
	if _, err := do(t, c, http.MethodPost, "/v1/uploads", `{"idempotency_key":"k"}`); err != nil {
		t.Fatal(err)
	}
	if srv.request(t, 0).body != srv.request(t, 1).body {
		t.Error("a create must be replayed byte for byte")
	}
}

func TestLogsOneMetadataLinePerAttemptAndNothingElse(t *testing.T) {
	srv := newScriptedServer(t, step{status: 503, body: "secret transcript text"}, okBody)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := newTestClient(t, srv.URL, newFakeClock(), WithLogger(logger))
	_, err := doContext(context.Background(), t, c, http.MethodPost, "/v1/uploads/j1/complete?sig=SECRETSIG", `{"title":"private title"}`)
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if lines := strings.Count(strings.TrimSpace(out), "\n") + 1; lines != 2 {
		t.Fatalf("expected 2 lines, got %d:\n%s", lines, out)
	}
	for _, want := range []string{"method=POST", "path=/v1/uploads/j1/complete", "status=503", "status=200", "attempt=1", "attempt=2", "duration_ms=", "retry_delay_ms=500"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	for _, leaked := range []string{testSecret, "SECRETSIG", "private title", "secret transcript text", "Authorization", "Bearer"} {
		if strings.Contains(out, leaked) {
			t.Errorf("log leaked %q:\n%s", leaked, out)
		}
	}
}

func TestLogsNetworkFailuresAndHidesForeignURLs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	other := newScriptedServer(t, okBody)
	c := newTestClient(t, deadAddress(t), newFakeClock(), WithLogger(logger), WithMaxRetries(0))
	_, _ = do(t, c, http.MethodGet, "/v1/account", "")
	if !strings.Contains(buf.String(), "status=0") {
		t.Errorf("a failed attempt should log status 0:\n%s", buf.String())
	}
	buf.Reset()
	c = newTestClient(t, other.URL, newFakeClock(), WithLogger(logger))
	req, _ := http.NewRequestWithContext(withLogLabel(context.Background(), "r2:part/3"), http.MethodPut, "http://127.0.0.1:1/bucket/key?X-Amz-Signature=SIG", strings.NewReader("x"))
	_, _ = send(c, req)
	if !strings.Contains(buf.String(), "path=r2:part/3") || strings.Contains(buf.String(), "SIG") || strings.Contains(buf.String(), "bucket") {
		t.Errorf("a direct URL must be logged by its label only:\n%s", buf.String())
	}
	buf.Reset()
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodPut, "http://127.0.0.1:1/bucket/key?X-Amz-Signature=SIG", strings.NewReader("x"))
	_, _ = send(c, req)
	if strings.Contains(buf.String(), "SIG") || strings.Contains(buf.String(), "bucket") {
		t.Errorf("an unlabeled foreign URL must not be logged:\n%s", buf.String())
	}
}

func TestIsSilentWithoutALogger(t *testing.T) {
	srv := newScriptedServer(t, okBody)
	c := newTestClient(t, srv.URL, newFakeClock())
	_, err := do(t, c, http.MethodGet, "/v1/account", "")
	if err != nil {
		t.Fatal(err)
	}
}

func TestACancelledClaimRotateOrSignupAfterTheBodyWasSentIsAnUnknownOutcome(t *testing.T) {
	for path, hint := range map[string]string{
		"/v1/agent/accounts/sg_1/claim": claimOutcomeHint,
		"/v1/agent/credentials/rotate":  rotateOutcomeHint,
		"/v1/agent/accounts":            "",
	} {
		srv := newScriptedServer(t, step{status: 200, delay: 5 * time.Second})
		c := newTestClient(t, srv.URL, newFakeClock())
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		_, err := doContext(ctx, t, c, http.MethodPost, path, `{}`)
		cancel()
		e := asError(t, err)
		if e.Kind != KindOutcomeUnknown || e.Hint != hint || !isContextError(err) || srv.count() != 1 {
			t.Errorf("%s: kind %v hint %q cause ok %v, %d requests", path, e.Kind, e.Hint, isContextError(err), srv.count())
		}
		// The caller's own cancellation is not a foreign error, so no transport type is named.
		if e.Message != "The request was sent but no response arrived, so its outcome is unknown." {
			t.Errorf("%s: message %q", path, e.Message)
		}
	}
	// Cancelled before anything was sent: a plain abort.
	srv := newScriptedServer(t, okBody)
	c := newTestClient(t, srv.URL, newFakeClock())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := doContext(ctx, t, c, http.MethodPost, "/v1/agent/credentials/rotate", `{}`)
	if e := asError(t, err); e.Kind != KindAbort || srv.count() != 0 {
		t.Errorf("kind %v, %d requests", e.Kind, srv.count())
	}
}

func TestABodyThatFailsMidStreamCountsAsSent(t *testing.T) {
	for _, path := range []string{"/v1/agent/accounts/sg_1/claim", "/v1/agent/credentials/rotate"} {
		srv := newScriptedServer(t, step{cut: true}, okBody)
		c := newTestClient(t, srv.URL, newFakeClock())
		_, err := do(t, c, http.MethodPost, path, `{}`)
		if e := asError(t, err); e.Kind != KindOutcomeUnknown || srv.count() != 1 {
			t.Errorf("%s: kind %v, %d requests", path, e.Kind, srv.count())
		}
	}
	srv := newScriptedServer(t, step{cut: true}, okBody)
	c := newTestClient(t, srv.URL, newFakeClock())
	reply, err := do(t, c, http.MethodGet, "/v1/account", "")
	if err != nil || srv.count() != 2 || reply.Text != `{"ok":true}` {
		t.Errorf("a GET whose body broke is retried: %d requests, err %v", srv.count(), err)
	}
	srv = newScriptedServer(t, step{cut: true}, okBody)
	c = newTestClient(t, srv.URL, newFakeClock())
	_, err = do(t, c, http.MethodPatch, "/v1/org/members/u1", `{}`)
	if e := asError(t, err); e.Kind != KindNetwork || srv.count() != 1 {
		t.Errorf("a mutation whose answer broke must not be replayed: kind %v, %d requests", e.Kind, srv.count())
	}
}

func TestOnlyTheirOwnRateLimitsCountTowardTheCreateSchedules(t *testing.T) {
	// Two 500s use the generic budget; the 15/30/60 schedule and its three waits are still all available.
	steps := []step{{status: 500}, {status: 500}, rateLimited(nil), rateLimited(nil), rateLimited(nil), okBody}
	srv := newScriptedServer(t, steps...)
	clock := newFakeClock()
	c := newTestClient(t, srv.URL, clock)
	if _, err := do(t, c, http.MethodPost, "/v1/uploads", `{"idempotency_key":"k"}`); err != nil {
		t.Fatalf("after %d requests: %v", srv.count(), err)
	}
	if srv.count() != 6 {
		t.Errorf("requests = %d", srv.count())
	}
	// Active-jobs waits do not eat the rate-limit schedule either.
	busy := step{status: 429, body: jsonError("too_many_active_jobs", 5001)}
	srv = newScriptedServer(t, busy, busy, rateLimited(nil), rateLimited(nil), rateLimited(nil), okBody)
	c = newTestClient(t, srv.URL, newFakeClock())
	if _, err := do(t, c, http.MethodPost, "/v1/uploads", `{"idempotency_key":"k"}`); err != nil {
		t.Fatalf("after %d requests: %v", srv.count(), err)
	}
}

func TestOnlyANonRateLimitStatusWithAHugeRetryAfterKeepsItsOwnClass(t *testing.T) {
	srv := newScriptedServer(t, step{status: 503, headers: map[string]string{"Retry-After": "61"}})
	c := newTestClient(t, srv.URL, newFakeClock())
	_, err := do(t, c, http.MethodGet, "/v1/account", "")
	if e := asError(t, err); e.Kind == KindRateLimit || e.RetryAfter != 61*time.Second {
		t.Errorf("kind %v retryAfter %v: a 503 is not a rate limit", e.Kind, e.RetryAfter)
	}
}
