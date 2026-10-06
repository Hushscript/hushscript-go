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
	// classSafe covers GETs and heartbeat: network errors, 429, 500, 502, 503 and 504 are retried.
	classSafe retryClass = iota
	// classUpload covers complete and part PUTs, which are safe to repeat: network errors, 429 and any 5xx are retried.
	classUpload
	// classUploadCreate is POST /v1/uploads with an idempotency key. It is paced, and a replay returns the same job.
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

// classify picks the retry class of a request by method and path. The path must be the escaped one
// (URL.EscapedPath): a signup id with a slash in it is one segment there, not two.
func classify(method, path string) retryClass {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return classSafe
	case http.MethodPost:
		switch {
		case matches(segments, "v1", "uploads"):
			return classUploadCreate
		case matches(segments, "v1", "uploads", "*", "heartbeat"):
			return classSafe
		case matches(segments, "v1", "uploads", "*", "complete"):
			return classUpload
		case matches(segments, "v1", "billing", "purchase"), matches(segments, "v1", "billing", "setup-session"):
			return classPurchase
		case matches(segments, "v1", "agent", "accounts", "*", "claim"),
			matches(segments, "v1", "agent", "credentials", "rotate"),
			matches(segments, "v1", "agent", "accounts"):
			return classNever
		}
	case http.MethodPut:
		if matches(segments, "v1", "uploads", "*", "parts", "*") {
			return classUpload
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
	if class == classNever {
		return 0
	}
	if class == classPurchase {
		return min(maxRetries, purchaseRetries)
	}
	return maxRetries
}

// retryableStatus reports whether an answer with this status is worth another try for the class.
func retryableStatus(class retryClass, status int) bool {
	if class == classSafe {
		return status == http.StatusTooManyRequests || status == http.StatusInternalServerError ||
			status == http.StatusBadGateway || status == http.StatusServiceUnavailable ||
			status == http.StatusGatewayTimeout
	}
	if class == classUpload || class == classUploadCreate {
		return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
	}
	if class == classPurchase {
		return status == http.StatusBadGateway || status == http.StatusServiceUnavailable
	}
	return false
}

// retryableNetworkError reports whether a failed attempt may be repeated. sent says whether the
// request may have reached the server.
func retryableNetworkError(class retryClass, sent bool) bool {
	if class == classNever {
		return false
	}
	if class == classMutate {
		return !sent
	}
	return true
}
