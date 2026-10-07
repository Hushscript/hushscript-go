package hushscript

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
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
	// limit is the most a response may hold in memory.
	limit int64
}

func newDoer(cfg config) *doer {
	return &doer{cfg: cfg, pacer: newPacer(cfg.clock), limit: maxResponseBytes}
}

// maxKeyPeekBytes bounds how much of an upload-create body is read to look for its idempotency key.
const maxKeyPeekBytes = 1 << 20

type (
	retryClassKey     struct{}
	attemptTimeoutKey struct{}
	directStorageKey  struct{}
)

// partTimeout is the per-attempt timeout of an upload part, direct or proxied (spec section 3).
const partTimeout = 300 * time.Second

// withDirectStorage marks a request as a direct-to-storage part upload: its URL is a presigned one
// that may share the API's origin, so the token must stay off it whatever its host says. It is
// retried like any upload part, gets the 300 s part timeout, is logged as "r2:part/N" and its errors
// never show the signature in the URL's query.
func withDirectStorage(ctx context.Context, part int) context.Context {
	ctx = withRetryClass(ctx, classUpload)
	ctx = withAttemptTimeout(ctx, partTimeout)
	ctx = withLogLabel(ctx, "r2:part/"+strconv.Itoa(part))
	return context.WithValue(ctx, directStorageKey{}, true)
}

func isDirectStorage(ctx context.Context) bool {
	direct, _ := ctx.Value(directStorageKey{}).(bool)
	return direct
}

// withoutQuery is rawURL up to its query or fragment: a presigned URL's query is the signature.
func withoutQuery(rawURL string) string {
	if i := strings.IndexAny(rawURL, "?#"); i >= 0 {
		return rawURL[:i]
	}
	return rawURL
}

// scrubSignedURLs cuts the query off every *url.Error in the chain, in place: the error came out of
// our own http.Client call, so nobody else holds it, and errors.As then hands out a clean one.
func scrubSignedURLs(err error) {
	for ; err != nil; err = errors.Unwrap(err) {
		if urlErr, ok := err.(*url.Error); ok { //nolint:errorlint // walking the chain by hand is the point
			urlErr.URL = withoutQuery(urlErr.URL)
		}
	}
}

// withRetryClass makes the doer treat the request as the given class, whatever its method and path say.
// Direct-to-storage part uploads use it: their URL is not an API path.
func withRetryClass(ctx context.Context, class retryClass) context.Context {
	return context.WithValue(ctx, retryClassKey{}, class)
}

// withAttemptTimeout replaces the configured timeout for each attempt of the request (the 300 s of
// an upload part, spec section 3). The caller's own context deadline still applies.
func withAttemptTimeout(ctx context.Context, timeout time.Duration) context.Context {
	return context.WithValue(ctx, attemptTimeoutKey{}, timeout)
}

// outcome is what one attempt produced.
type outcome struct {
	resp *http.Response
	body []byte
	err  error
	// sent says whether the request may have reached the server. It is true unless the failure is
	// provably in connection setup, which a custom transport cannot be trusted to report any other way.
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
	class, paced := d.plan(req)
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
		if paced {
			if err := d.pacer.acquire(ctx); err != nil {
				return nil, d.aborted(err)
			}
		}
		out := d.attempt(ctx, req)
		record := attempt{method: req.Method, path: label, number: number, duration: out.took}

		if out.err != nil {
			if err := ctx.Err(); err != nil {
				d.cfg.logAttempt(ctx, record)
				if class == classNever && out.sent {
					return nil, d.outcomeUnknown(req, err)
				}
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
		failure := errorFromResponse(out.resp.StatusCode, out.resp.Header, out.body, req.Method, req.URL.EscapedPath(), d.cfg.redactor())
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

// plan picks the retry class of a request, and says whether it is an upload create, which is paced
// whether or not it may be retried. An upload create is retried only when its body carries an
// idempotency key: without one a replay would place a second hold.
func (d *doer) plan(req *http.Request) (class retryClass, paced bool) {
	natural := classify(req.Method, req.URL.EscapedPath())
	paced = natural == classUploadCreate
	if forced, ok := req.Context().Value(retryClassKey{}).(retryClass); ok {
		return forced, paced || forced == classUploadCreate
	}
	if natural == classUploadCreate && !carriesIdempotencyKey(req) {
		natural = classMutate
	}
	return natural, paced
}

// carriesIdempotencyKey reports whether the JSON body has a non-empty idempotency_key. It reads a
// fresh copy of the body, so the request itself is untouched.
func carriesIdempotencyKey(req *http.Request) bool {
	if req.GetBody == nil {
		return false
	}
	body, err := req.GetBody()
	if err != nil {
		return false
	}
	defer func() { _ = body.Close() }()
	var payload struct {
		Key string `json:"idempotency_key"`
	}
	if err := json.NewDecoder(io.LimitReader(body, maxKeyPeekBytes)).Decode(&payload); err != nil {
		return false
	}
	return payload.Key != ""
}

// timeoutFor is the per-attempt timeout of a request.
func (d *doer) timeoutFor(ctx context.Context) time.Duration {
	if override, ok := ctx.Value(attemptTimeoutKey{}).(time.Duration); ok && override > 0 {
		return override
	}
	return d.cfg.timeout
}

// attempt sends the request once, under its own timeout, and reads the whole answer inside it.
func (d *doer) attempt(parent context.Context, req *http.Request) outcome {
	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, d.timeoutFor(parent))
	defer cancel()

	// The hooks fire only when the transport is (or wraps) an http.Transport; for any other transport
	// the error itself has to prove that nothing was sent.
	var asked, connected, handshakeFailed atomic.Bool
	trace := &httptrace.ClientTrace{
		GetConn: func(string) { asked.Store(true) },
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err != nil {
				handshakeFailed.Store(true)
			}
		},
	}
	r := req.Clone(httptrace.WithClientTrace(ctx, trace))
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return outcome{err: err, took: time.Since(started)}
		}
		r.Body = body
	}
	// The token goes to the API host only; direct-to-storage part uploads carry their own signature,
	// and never the token, even when the storage URL happens to share the API's origin.
	direct := isDirectStorage(parent)
	token := ""
	if d.cfg.tokens != nil {
		token = d.cfg.tokens.get() // read per attempt: a rotation applies to retries already queued
	}
	if !direct && token != "" && strings.EqualFold(r.URL.Host, d.cfg.baseHost) && r.URL.Scheme == d.cfg.baseScheme {
		r.Header.Set("Authorization", "Bearer "+token)
	} else {
		r.Header.Del("Authorization")
	}
	r.Header.Set("User-Agent", userAgent())

	resp, err := d.cfg.httpClient.Do(r)
	if err != nil {
		if direct {
			scrubSignedURLs(err)
		}
		unsent := handshakeFailed.Load() || (asked.Load() && !connected.Load()) || connectionSetupFailure(err)
		return outcome{err: err, sent: !unsent, took: time.Since(started)}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, d.limit+1))
	if err == nil && int64(len(body)) > d.limit {
		err = errResponseTooLarge
	}
	if err != nil {
		return outcome{err: err, sent: true, took: time.Since(started)}
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	return outcome{resp: resp, body: body, sent: true, took: time.Since(started)}
}

// connectionSetupFailure reports whether err proves the request never left: DNS, dial and TLS
// certificate or handshake-framing failures. Anything else may have happened after the body was written.
func connectionSetupFailure(err error) bool {
	var dnsErr *net.DNSError
	var opErr *net.OpError
	var verifyErr *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var record tls.RecordHeaderError
	switch {
	case errors.As(err, &dnsErr), errors.As(err, &verifyErr), errors.As(err, &unknownAuthority),
		errors.As(err, &hostname), errors.As(err, &invalid), errors.As(err, &record):
		return true
	case errors.As(err, &opErr):
		return opErr.Op == "dial"
	default:
		return false
	}
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
	return newSDKError(KindAbort, "aborted", "The request was cancelled.", cause, d.cfg.redactor(), hintInputs{})
}

// networkFailure is the error for an attempt that got no answer and will not be retried. When the
// request was sent and the operation mints a credential or an account, the outcome is unknown, not a
// plain failure.
func (d *doer) networkFailure(class retryClass, req *http.Request, out outcome) *Error {
	if class == classNever && out.sent {
		return d.outcomeUnknown(req, out.err)
	}
	message := "The request to the Hushscript API failed."
	if errors.Is(out.err, context.DeadlineExceeded) {
		message = fmt.Sprintf("The request to the Hushscript API timed out after %s.", d.timeoutFor(req.Context()))
	}
	return newSDKError(KindNetwork, "network_error", message, out.err, d.cfg.redactor(), hintInputs{})
}

// outcomeUnknown is the error for a claim, rotate or signup whose request was sent and whose answer
// never arrived (a network error, a timeout or a cancellation): the server may have acted. Claim and
// rotate carry the hints of spec section 7.4; signup has none.
func (d *doer) outcomeUnknown(req *http.Request, cause error) *Error {
	failure := newSDKError(KindOutcomeUnknown, "outcome_unknown",
		"The request was sent but no response arrived, so its outcome is unknown.", cause, d.cfg.redactor(), hintInputs{})
	switch path := req.URL.EscapedPath(); {
	case strings.HasSuffix(path, "/claim"):
		failure.Hint = claimOutcomeHint
	case strings.HasSuffix(path, "/credentials/rotate"):
		failure.Hint = rotateOutcomeHint
	default:
		failure.Hint = ""
	}
	return failure
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
