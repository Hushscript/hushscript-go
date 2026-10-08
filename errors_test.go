package hushscript

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Hushscript/hushscript-go/generated"
)

func asText(v any) string { return fmt.Sprintf("%v", v) }

func fromResponse(status int, header http.Header, body, method, path string) *Error {
	return errorFromResponse(status, header, []byte(body), method, path, newRedactor(testToken))
}

func TestMapsStatusesAndSlugsToKinds(t *testing.T) {
	cases := []struct {
		slug   string
		status int
		want   ErrorKind
	}{
		{"unauthorized", 401, KindAuthentication},
		{"agent_claim_invalid", 401, KindAuthentication},
		{"pat_scope_missing", 403, KindPermissionDenied},
		{"pat_purchase_requires_app", 403, KindPermissionDenied},
		{"country_restricted", 403, KindPermissionDenied},
		{"insufficient_balance", 402, KindInsufficientBalance},
		{"org_insufficient_balance", 402, KindInsufficientBalance},
		{"org_pool_expired", 402, KindInsufficientBalance},
		{"org_plan_frozen", 402, KindInsufficientBalance},
		{"not_found", 404, KindNotFound},
		{"wrong_state", 409, KindConflict},
		{"pat_limit", 409, KindConflict},
		{"bad_request", 400, KindValidation},
		{"policy_version_stale", 422, KindValidation},
		{"rate_limited", 429, KindRateLimit},
		{"too_many_active_jobs", 429, KindRateLimit},
		{"url_target_busy", 429, KindRateLimit},
		{"upload_incomplete", 422, KindUpload},
		{"size_exceeded", 422, KindUpload},
		{"internal", 500, KindAPI},
		{"something_new", 418, KindAPI},
	}
	for _, tc := range cases {
		e := fromResponse(tc.status, nil, jsonError(tc.slug, 1234), http.MethodGet, "/v1/account")
		if e.Kind != tc.want {
			t.Errorf("%s (%d): kind %v, want %v", tc.slug, tc.status, e.Kind, tc.want)
		}
		if e.Code != tc.slug || e.NumericCode != 1234 || e.Status != tc.status {
			t.Errorf("%s: code %q numeric %d status %d", tc.slug, e.Code, e.NumericCode, e.Status)
		}
	}
}

func TestEveryCatalogSlugMapsToTheClassSpecSection92Names(t *testing.T) {
	want := map[string]ErrorKind{
		"unauthorized": KindAuthentication, "agent_claim_invalid": KindAuthentication,
		"pat_scope_missing": KindPermissionDenied, "pat_forbidden": KindPermissionDenied, "account_suspended": KindPermissionDenied,
		"agent_pack_locked": KindPermissionDenied, "pat_purchase_requires_app": KindPermissionDenied,
		"pat_purchase_requires_authentication": KindPermissionDenied, "country_restricted": KindPermissionDenied,
		"signup_not_allowed":   KindPermissionDenied,
		"insufficient_balance": KindInsufficientBalance, "org_insufficient_balance": KindInsufficientBalance,
		"org_pool_expired": KindInsufficientBalance, "org_plan_frozen": KindInsufficientBalance,
		"wrong_state": KindConflict, "agent_signup_pending": KindConflict, "pat_limit": KindConflict,
		"rate_limited": KindRateLimit, "too_many_active_jobs": KindRateLimit,
		"agent_purchase_capped": KindRateLimit, "url_target_busy": KindRateLimit,
		"upload_incomplete": KindUpload, "size_exceeded": KindUpload,
	}
	for slug, kind := range want {
		entry, ok := generated.LookupError(slug)
		if !ok {
			t.Errorf("%s is not in the catalog", slug)
			continue
		}
		e := fromResponse(entry.Status, nil, jsonError(slug, entry.Code), http.MethodGet, "/v1/account")
		if e.Kind != kind {
			t.Errorf("%s: kind %v, want %v", slug, e.Kind, kind)
		}
	}
}

func TestUsesTheCatalogMeaningAsTheMessage(t *testing.T) {
	entry, _ := generated.LookupError("insufficient_balance")
	e := fromResponse(402, nil, jsonError("insufficient_balance", 3001), http.MethodPost, "/v1/uploads")
	if e.Message != entry.Meaning {
		t.Errorf("message = %q, want the catalog meaning", e.Message)
	}
	if !strings.Contains(e.Error(), "insufficient_balance") || !strings.Contains(e.Error(), entry.Meaning) {
		t.Errorf("Error() = %q", e.Error())
	}
}

func TestKeepsTheExtraBodyKeysWithTheAPIsOwnNamesAsDetails(t *testing.T) {
	body := `{"error":"insufficient_balance","code":3001,"needed_seconds":21,"balance_seconds":10,"held_seconds":0,"org_balance_seconds":5}`
	e := fromResponse(402, nil, body, http.MethodPost, "/v1/uploads")
	want := map[string]float64{"needed_seconds": 21, "balance_seconds": 10, "held_seconds": 0, "org_balance_seconds": 5}
	for key, value := range want {
		if got, ok := e.Details[key].(float64); !ok || got != value {
			t.Errorf("details[%s] = %v", key, e.Details[key])
		}
	}
	for _, key := range []string{"error", "code"} {
		if _, present := e.Details[key]; present {
			t.Errorf("%s is not a detail", key)
		}
	}
}

func TestKeepsTheNestedDetailsObjectAndFillsHintsFromIt(t *testing.T) {
	body := `{"error":"agent_pack_locked","code":4037,"details":{"allowed_packs":["45min","300min"],"retry_after_seconds":120}}`
	e := fromResponse(403, nil, body, http.MethodPost, "/v1/billing/purchase")
	nested, ok := e.Details["details"].(map[string]any)
	if !ok {
		t.Fatalf("details = %v", e.Details)
	}
	if packs, _ := nested["allowed_packs"].([]any); len(packs) != 2 {
		t.Errorf("allowed_packs = %v", nested["allowed_packs"])
	}
	if !strings.Contains(e.Hint, "45min, 300min") {
		t.Errorf("hint = %q", e.Hint)
	}
	if e.RetryAfter != 120*time.Second {
		t.Errorf("retryAfter from details = %v", e.RetryAfter)
	}
}

func TestDropsHintSentencesWithAnUnfilledPlaceholder(t *testing.T) {
	// rate_limited without a wait: the second sentence mentions {retryAfter} and goes.
	e := fromResponse(429, nil, jsonError("rate_limited", 5000), http.MethodGet, "/v1/account")
	if e.Hint != "Too many requests." {
		t.Errorf("hint = %q", e.Hint)
	}
	// agent_pack_locked with no allowed packs: the only sentence goes and the hint is empty.
	e = fromResponse(403, nil, jsonError("agent_pack_locked", 4037), http.MethodPost, "/v1/billing/purchase")
	if e.Hint != "" {
		t.Errorf("hint = %q, want none", e.Hint)
	}
	if got := fillHint("A. B {x}. C {y}", map[string]string{"y": "1"}); got != "A. C 1" {
		t.Errorf("fillHint = %q", got)
	}
}

func TestReadsRetryAfterFromTheHeader(t *testing.T) {
	h := http.Header{"Retry-After": []string{"61"}}
	e := fromResponse(429, h, jsonError("rate_limited", 5000), http.MethodGet, "/v1/account")
	if e.RetryAfter != 61*time.Second {
		t.Errorf("retryAfter = %v", e.RetryAfter)
	}
	if !strings.Contains(e.Hint, "61 s") {
		t.Errorf("hint should say how long to wait: %q", e.Hint)
	}
	for _, bad := range []string{"", "soon", "-3", "1.5", "Wed, 21 Oct 2026 07:28:00 GMT"} {
		e := fromResponse(429, http.Header{"Retry-After": []string{bad}}, jsonError("rate_limited", 5000), http.MethodGet, "/v1/account")
		if e.RetryAfter != 0 {
			t.Errorf("Retry-After %q gave %v", bad, e.RetryAfter)
		}
	}
}

func TestNamesTheMissingScopeInTheHint(t *testing.T) {
	scope, ok := generated.RequiredScope("POST /v1/billing/purchase")
	if !ok {
		t.Fatal("the scope map has no entry for the purchase operation")
	}
	e := fromResponse(403, nil, jsonError("pat_scope_missing", 2023), http.MethodPost, "/v1/billing/purchase")
	if !strings.Contains(e.Hint, `"`+scope+`" permission`) {
		t.Errorf("hint = %q, want it to name %q", e.Hint, scope)
	}
	e = fromResponse(403, nil, jsonError("pat_scope_missing", 2023), http.MethodGet, "/v1/transcripts")
	if !strings.Contains(e.Hint, `"transcripts:read" permission`) {
		t.Errorf("hint = %q", e.Hint)
	}
	e = fromResponse(403, nil, jsonError("pat_scope_missing", 2023), http.MethodGet, "/v1/transcripts/tr_123")
	if strings.Contains(e.Hint, "{scope}") || !strings.Contains(e.Hint, "permission") {
		t.Errorf("a templated path should still find its scope: %q", e.Hint)
	}
}

func TestFillsTheInsufficientBalanceHintInMinutes(t *testing.T) {
	body := `{"error":"insufficient_balance","code":3001,"needed_seconds":1201,"balance_seconds":659,"held_seconds":0}`
	e := fromResponse(402, nil, body, http.MethodPost, "/v1/uploads")
	if !strings.Contains(e.Hint, "21 needed, 10 available") {
		t.Errorf("needed rounds up and balance rounds down: %q", e.Hint)
	}
}

func TestHasNoHintForASlugWithoutOne(t *testing.T) {
	e := fromResponse(404, nil, jsonError("not_found", 6000), http.MethodGet, "/v1/jobs/x")
	assertCoreError(t, e, KindNotFound, "not_found", 404)
	if _, listed := generated.Hint("not_found"); listed {
		t.Fatal("not_found now has a hint; choose a slug absent from the hint catalog")
	}
	if e.Hint != "" {
		t.Errorf("hint = %q for a slug without one", e.Hint)
	}
}

func TestHandlesBodiesThatAreNotTheErrorShape(t *testing.T) {
	for name, body := range map[string]string{
		"text":      "Bad Gateway",
		"empty":     "",
		"html":      "<html>502</html>",
		"array":     "[1,2]",
		"no slug":   `{"code": 5}`,
		"bad types": `{"error": 7, "code": "x"}`,
	} {
		e := fromResponse(502, nil, body, http.MethodGet, "/v1/account")
		if e.Kind != KindAPI || e.Status != 502 || e.Code != "http_502" || e.NumericCode != 0 {
			t.Errorf("%s: %+v", name, e)
		}
		if e.Message == "" {
			t.Errorf("%s: empty message", name)
		}
	}
}

func TestUsesTheStatusForAnUnknownSlug(t *testing.T) {
	e := fromResponse(409, nil, jsonError("brand_new_slug", 9999), http.MethodGet, "/v1/account")
	if e.Code != "brand_new_slug" || e.Kind != KindConflict || e.Message == "" {
		t.Errorf("%+v", e)
	}
}

func TestNeverLetsTheTokenIntoAnythingAnErrorExposes(t *testing.T) {
	body := `{"error":"unauthorized","code":2000,"details":{"echo":"` + testToken + `","nested":["` + testToken + `"]},"note":"` + testToken + `"}`
	e := fromResponse(401, nil, body, http.MethodGet, "/v1/account")
	wrapped := newSDKError(KindNetwork, "network_error", "GET failed", errors.New("dial "+testToken), newRedactor(testToken), hintInputs{})
	for name, text := range map[string]string{
		"message": e.Message, "hint": e.Hint, "details": asText(e.Details), "error": e.Error(),
		"wrapped": wrapped.Error(), "cause": wrapped.Unwrap().Error(), "verbose": fmt.Sprintf("%+v %#v", e, wrapped),
	} {
		if strings.Contains(text, testSecret) {
			t.Errorf("%s leaked the token: %s", name, text)
		}
	}
}

// Pinning only (the code already redacts these): a server that echoes the token as its error slug, as a
// details key at the top level or nested, or as a value, leaks it through no field of the error.
func TestRedactsTheTokenInTheServersSlugAndInDetailsKeys(t *testing.T) {
	body := `{"error":"` + testToken + `","code":2000,"` + testToken + `":"top","details":{"` + testToken + `":{"` + testToken + `":1},"k":"` + testToken + `"}}`
	e := fromResponse(401, nil, body, http.MethodGet, "/v1/account")
	for name, text := range map[string]string{
		"code": e.Code, "message": e.Message, "hint": e.Hint, "details": asText(e.Details), "error": e.Error(),
		"verbose": fmt.Sprintf("%+v %#v %v", e, e, e.Details),
	} {
		if strings.Contains(text, testSecret) || strings.Contains(text, testToken) {
			t.Errorf("%s leaked the token: %s", name, text)
		}
	}
}

func TestSDKErrorsCarryHintsAndTheirCause(t *testing.T) {
	e := newSDKError(KindAbort, "aborted", "The request was cancelled.", context.Canceled, redactor{}, hintInputs{})
	if !errors.Is(e, context.Canceled) {
		t.Error("the cause is not reachable through errors.Is")
	}
	var target *Error
	if !errors.As(fmt.Errorf("outer: %w", e), &target) || target.Kind != KindAbort {
		t.Error("errors.As does not find the SDK error")
	}
	hinted := newSDKError(KindUpload, "wrong_state", "x", nil, redactor{}, hintInputs{})
	if !strings.Contains(hinted.Hint, "Fetch the job") {
		t.Errorf("hint for an SDK slug = %q", hinted.Hint)
	}
	if e.Unwrap() == nil || hinted.Unwrap() != nil {
		t.Error("Unwrap should return the cause, or nil")
	}
}

func TestKindNamesFollowSpecSection92(t *testing.T) {
	want := map[ErrorKind]string{
		KindAPI: "ApiError", KindConfiguration: "ConfigurationError", KindUnsupportedMedia: "UnsupportedMediaError",
		KindFileLimit: "FileLimitError", KindDailyLimit: "DailyLimitError", KindResumeMismatch: "ResumeMismatchError",
		KindAuthentication: "AuthenticationError", KindPermissionDenied: "PermissionDeniedError",
		KindInsufficientBalance: "InsufficientBalanceError", KindNotFound: "NotFoundError",
		KindConflict: "ConflictError", KindValidation: "ValidationError", KindRateLimit: "RateLimitError",
		KindPaymentMethodRequired: "PaymentMethodRequiredError", KindUpload: "UploadError",
		KindJobFailed: "JobFailedError", KindWaitTimeout: "WaitTimeoutError", KindAbort: "AbortError",
		KindNetwork: "NetworkError", KindOutcomeUnknown: "OutcomeUnknownError", KindPreflight: "PreflightError", KindUnexpected: "HushscriptError",
	}
	for kind, name := range want {
		if kind.String() != name {
			t.Errorf("%d is %q, want %q", int(kind), kind.String(), name)
		}
	}
	if ErrorKind(-1).String() != "ErrorKind(-1)" || ErrorKind(999).String() == "" {
		t.Error("an unknown kind should still print something")
	}
}

// The numeric values of the kinds that existed before KindPreflight stay where they were: KindPreflight
// and KindUnexpected are appended, not inserted.
func TestKindValuesDoNotMoveWhenAKindIsAdded(t *testing.T) {
	released := []ErrorKind{
		KindAPI, KindConfiguration, KindUnsupportedMedia, KindFileLimit, KindDailyLimit, KindResumeMismatch,
		KindAuthentication, KindPermissionDenied, KindInsufficientBalance, KindNotFound, KindConflict, KindValidation,
		KindRateLimit, KindPaymentMethodRequired, KindUpload, KindJobFailed, KindWaitTimeout, KindAbort, KindNetwork,
		KindOutcomeUnknown,
	}
	for i, kind := range released {
		if int(kind) != i {
			t.Errorf("%v is %d, want %d", kind, int(kind), i)
		}
	}
	if int(KindPreflight) != len(released) || int(KindUnexpected) != len(released)+1 {
		t.Errorf("KindPreflight is %d, KindUnexpected %d, want %d and %d", int(KindPreflight), int(KindUnexpected), len(released), len(released)+1)
	}
}

func TestKindFamilies(t *testing.T) {
	for _, kind := range []ErrorKind{KindPreflight, KindUnsupportedMedia, KindFileLimit, KindDailyLimit, KindResumeMismatch} {
		if !kind.IsPreflight() || kind.IsAPI() {
			t.Errorf("%v should be preflight only", kind)
		}
	}
	for _, kind := range []ErrorKind{KindAPI, KindAuthentication, KindPermissionDenied, KindInsufficientBalance, KindNotFound, KindConflict, KindValidation, KindRateLimit} {
		if !kind.IsAPI() || kind.IsPreflight() {
			t.Errorf("%v should be API only", kind)
		}
	}
	for _, kind := range []ErrorKind{KindConfiguration, KindNetwork, KindAbort, KindOutcomeUnknown, KindUpload, KindJobFailed, KindWaitTimeout, KindPaymentMethodRequired, KindUnexpected} {
		if kind.IsAPI() || kind.IsPreflight() {
			t.Errorf("%v belongs to neither family", kind)
		}
	}
}

func TestMatchesOperationsByTemplate(t *testing.T) {
	for _, tc := range []struct{ method, path, want string }{
		{"GET", "/v1/transcripts/tr_1", "GET /v1/transcripts/{id}"},
		{"GET", "/v1/transcripts", "GET /v1/transcripts"},
		{"PUT", "/v1/uploads/job_1/parts/3", "PUT /v1/uploads/{job}/parts/{n}"},
		{"POST", "/v1/agent/accounts/sg_1/claim", ""}, // claim declares no scope, so it is not in the scope map
	} {
		if got := operationOf(tc.method, tc.path); got != tc.want {
			t.Errorf("%s %s = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
	if got := operationOf("GET", "/v1/nope/at/all"); got != "" {
		t.Errorf("unknown path matched %q", got)
	}
	if got := operationOf("TRACE", "/v1/account"); got != "" {
		t.Errorf("unknown method matched %q", got)
	}
}
