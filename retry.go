package hushscript

import (
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

// retryClass is a row of the retry table in spec section 7.2.
type retryClass int

const (
	// classSafe covers GETs, heartbeat, complete and part uploads: network errors, 429 and 5xx are retried.
	classSafe retryClass = iota
	// classUploadCreate is POST /v1/uploads, which always carries an idempotency key and is paced.
	classUploadCreate
	// classPurchase is purchase and setup-session: network errors, 502 and 503, at most once.
	classPurchase
	// classNever is claim, rotate and signup: a lost response cannot be recovered, so nothing is retried.
	classNever
	// classMutate is every other mutation: retried only when the request provably never left.
	classMutate
)

const (
	backoffBase       = 500 * time.Millisecond
	backoffCap        = 8 * time.Second
	maxRetryAfterWait = 60 * time.Second
	purchaseRetries   = 1

	// Upload-create waits (spec section 7.5).
	tooManyJobsWait    = 30 * time.Second
	tooManyJobsTries   = 10
	rateLimitedBackoff = 3
)

var rateLimitedWaits = [rateLimitedBackoff]time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second}

// classify picks the retry class of a request by method and path.
func classify(method, path string) retryClass {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return classSafe
	case http.MethodPost:
		switch {
		case matches(segments, "v1", "uploads"):
			return classUploadCreate
		case matches(segments, "v1", "uploads", "*", "heartbeat"), matches(segments, "v1", "uploads", "*", "complete"):
			return classSafe
		case matches(segments, "v1", "billing", "purchase"), matches(segments, "v1", "billing", "setup-session"):
			return classPurchase
		case matches(segments, "v1", "agent", "accounts", "*", "claim"),
			matches(segments, "v1", "agent", "credentials", "rotate"),
			matches(segments, "v1", "agent", "accounts"):
			return classNever
		}
	case http.MethodPut:
		if matches(segments, "v1", "uploads", "*", "parts", "*") {
			return classSafe
		}
	}
	return classMutate
}

func matches(segments []string, template ...string) bool {
	if len(segments) != len(template) {
		return false
	}
	for i, want := range template {
		if want != "*" && segments[i] != want {
			return false
		}
	}
	return true
}

// backoffCeiling is min(8 s, 0.5 s x 2^attempt); attempt 0 is the first retry.
func backoffCeiling(attempt int) time.Duration {
	attempt = max(attempt, 0)
	if attempt >= 5 { // 0.5 s x 2^5 is already past the cap
		return backoffCap
	}
	return min(backoffCap, backoffBase<<attempt)
}

// fullJitter picks uniformly from [0, ceiling]. It is jitter, not a secret, so math/rand is right.
func fullJitter(ceiling time.Duration) time.Duration {
	if ceiling <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(ceiling) + 1)) //nolint:gosec // backoff jitter, not a security decision
}

// retryBudget is how many retries a class gets in total under maxRetries.
func retryBudget(class retryClass, maxRetries int) int {
	switch class {
	case classNever:
		return 0
	case classPurchase:
		return min(maxRetries, purchaseRetries)
	default:
		return maxRetries
	}
}

// retryableStatus reports whether an answer with this status is worth another try for the class.
func retryableStatus(class retryClass, status int) bool {
	switch class {
	case classSafe:
		return status == http.StatusTooManyRequests || status == http.StatusInternalServerError ||
			status == http.StatusBadGateway || status == http.StatusServiceUnavailable ||
			status == http.StatusGatewayTimeout
	case classUploadCreate:
		return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
	case classPurchase:
		return status == http.StatusBadGateway || status == http.StatusServiceUnavailable
	default:
		return false
	}
}

// retryableNetworkError reports whether a failed attempt may be repeated. sent says whether the
// request may have reached the server.
func retryableNetworkError(class retryClass, sent bool) bool {
	switch class {
	case classSafe, classUploadCreate, classPurchase:
		return true
	case classMutate:
		return !sent
	default:
		return false
	}
}
