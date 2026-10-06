package hushscript

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"
)

// maxResponseBytes bounds what one response may hold in memory.
const maxResponseBytes = 256 << 20

const (
	claimOutcomeHint = "The claim may have succeeded. Retrying will fail with agent_claim_invalid. " +
		"If you have no token, contact support with the signup id."
	rotateOutcomeHint = "The old token may already be revoked. Try a request with it; if it fails, " +
		"the new token was lost and the account needs support."
)

var errResponseTooLarge = errors.New("the response is larger than the SDK reads into memory")

// doer is the SDK's retrying HttpRequestDoer: it wraps an http.Client and applies timeouts, the
// retry table of spec section 7.2, Retry-After, upload-create pacing, token handling and typed
// errors. Every non-2xx answer comes back as a *Error, never as a response.
type doer struct {
	cfg   config
	pacer *pacer
	red   redactor
}

func newDoer(cfg config) *doer {
	return &doer{cfg: cfg, pacer: newPacer(cfg.clock), red: cfg.redactor()}
}

// outcome is what one attempt produced.
type outcome struct {
	resp *http.Response
	body []byte
	err  error
	// sent says whether the request may have reached the server: a connection was obtained.
	sent bool
	took time.Duration
}

// retryState counts what the upload-create rules and the generic budget have used.
type retryState struct {
	retries int
	busy    int
	limited int
}

// Do sends the request and returns the 2xx response with its body already read, or a *Error.
func (d *doer) Do(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	class := classify(req.Method, req.URL.Path)
	budget := retryBudget(class, d.cfg.maxRetries)
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		budget = 0 // a body that cannot be replayed cannot be retried
	}
	label := d.logPath(req)
	var state retryState
	for number := 1; ; number++ {
		if err := ctx.Err(); err != nil {
			return nil, d.aborted(err)
		}
		if class == classUploadCreate {
			if err := d.pacer.acquire(ctx); err != nil {
				return nil, d.aborted(err)
			}
		}
		out := d.attempt(ctx, req)
		record := attempt{method: req.Method, path: label, number: number, duration: out.took}

		if out.err != nil {
			if err := ctx.Err(); err != nil {
				d.cfg.logAttempt(ctx, record)
				return nil, d.aborted(err)
			}
			if state.retries >= budget || !retryableNetworkError(class, out.sent) {
				d.cfg.logAttempt(ctx, record)
				return nil, d.networkFailure(class, req, out)
			}
			record.retryDelay = d.cfg.jitter(backoffCeiling(state.retries))
			state.retries++
			d.cfg.logAttempt(ctx, record)
			if err := d.cfg.clock.Sleep(ctx, record.retryDelay); err != nil {
				return nil, d.aborted(err)
			}
			continue
		}

		record.status = out.resp.StatusCode
		if out.resp.StatusCode >= http.StatusOK && out.resp.StatusCode < http.StatusMultipleChoices {
			d.cfg.logAttempt(ctx, record)
			return out.resp, nil
		}
		failure := errorFromResponse(out.resp.StatusCode, out.resp.Header, out.body, req.Method, req.URL.Path, d.red)
		retry, delay := d.statusRetry(class, &state, failure, out.resp.Header, budget)
		record.retryDelay = delay
		d.cfg.logAttempt(ctx, record)
		if !retry {
			return nil, failure
		}
		if err := d.cfg.clock.Sleep(ctx, delay); err != nil {
			return nil, d.aborted(err)
		}
	}
}

// attempt sends the request once, under its own timeout, and reads the whole answer inside it.
func (d *doer) attempt(parent context.Context, req *http.Request) outcome {
	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, d.cfg.timeout)
	defer cancel()

	var connected atomic.Bool
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }}
	r := req.Clone(httptrace.WithClientTrace(ctx, trace))
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return outcome{err: err, took: time.Since(started)}
		}
		r.Body = body
	}
	// The token goes to the API host only; direct-to-storage part uploads carry their own signature.
	if r.URL.Host == d.cfg.baseHost {
		r.Header.Set("Authorization", "Bearer "+d.cfg.token.reveal())
	}
	r.Header.Set("User-Agent", userAgent())

	resp, err := d.cfg.httpClient.Do(r)
	if err != nil {
		return outcome{err: err, sent: connected.Load(), took: time.Since(started)}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err == nil && len(body) > maxResponseBytes {
		err = errResponseTooLarge
	}
	if err != nil {
		return outcome{err: err, sent: true, took: time.Since(started)}
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	return outcome{resp: resp, body: body, sent: true, took: time.Since(started)}
}

// statusRetry decides whether an error answer is retried and how long to wait first.
func (d *doer) statusRetry(class retryClass, state *retryState, failure *Error, header http.Header, budget int) (bool, time.Duration) {
	if budget == 0 || !retryableStatus(class, failure.Status) {
		return false, 0
	}
	retryAfter, hasRetryAfter := parseRetryAfter(header)
	if hasRetryAfter && retryAfter > maxRetryAfterWait {
		return false, 0
	}
	if class == classUploadCreate {
		switch {
		case failure.Code == "too_many_active_jobs":
			if state.busy >= tooManyJobsTries-1 {
				return false, 0
			}
			state.busy++
			return true, tooManyJobsWait
		case failure.Code == "rate_limited" && !hasRetryAfter:
			if state.limited >= len(rateLimitedWaits) {
				return false, 0
			}
			wait := rateLimitedWaits[state.limited]
			state.limited++
			return true, wait
		}
	}
	if state.retries >= budget {
		return false, 0
	}
	delay := d.cfg.jitter(backoffCeiling(state.retries))
	if hasRetryAfter {
		delay = retryAfter
	}
	state.retries++
	return true, delay
}

func (d *doer) aborted(cause error) *Error {
	return newSDKError(KindAbort, "aborted", "The request was cancelled.", cause, d.red)
}

// networkFailure is the error for an attempt that got no answer and will not be retried. When the
// request was sent and the operation mints a credential, the outcome is unknown, not a plain failure.
func (d *doer) networkFailure(class retryClass, req *http.Request, out outcome) *Error {
	if class == classNever && out.sent {
		hint := ""
		switch {
		case strings.HasSuffix(req.URL.Path, "/claim"):
			hint = claimOutcomeHint
		case strings.HasSuffix(req.URL.Path, "/credentials/rotate"):
			hint = rotateOutcomeHint
		}
		if hint != "" {
			failure := newSDKError(KindOutcomeUnknown, "outcome_unknown",
				"The request was sent but no response arrived, so its outcome is unknown.", out.err, d.red)
			failure.Hint = hint
			return failure
		}
	}
	message := "The request to the Hushscript API failed."
	if errors.Is(out.err, context.DeadlineExceeded) {
		message = fmt.Sprintf("The request to the Hushscript API timed out after %s.", d.cfg.timeout)
	}
	return newSDKError(KindNetwork, "network_error", message, out.err, d.red)
}

// logPath is how a request is named in the log: its path on the API host, a caller's label for a
// direct upload, never a foreign URL (its query string can be a signature).
func (d *doer) logPath(req *http.Request) string {
	if label, ok := req.Context().Value(logLabelKey{}).(string); ok {
		return label
	}
	if req.URL.Host == d.cfg.baseHost {
		return req.URL.Path
	}
	return "external"
}
