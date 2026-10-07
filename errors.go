package hushscript

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Hushscript/hushscript-go/generated"
)

// ErrorKind is the class of an Error, following the hierarchy of the SDK specification (section 9.2).
type ErrorKind int

// The error kinds. KindAPI is any non-2xx answer that has no more specific kind.
const (
	KindAPI ErrorKind = iota
	KindConfiguration
	KindUnsupportedMedia
	KindFileLimit
	KindDailyLimit
	KindResumeMismatch
	KindAuthentication
	KindPermissionDenied
	KindInsufficientBalance
	KindNotFound
	KindConflict
	KindValidation
	KindRateLimit
	KindPaymentMethodRequired
	KindUpload
	KindJobFailed
	KindWaitTimeout
	KindAbort
	KindNetwork
	KindOutcomeUnknown
	// KindPreflight is last so that the values of the kinds released before it do not move.
	KindPreflight
)

var kindNames = map[ErrorKind]string{
	KindAPI:                   "ApiError",
	KindConfiguration:         "ConfigurationError",
	KindPreflight:             "PreflightError",
	KindUnsupportedMedia:      "UnsupportedMediaError",
	KindFileLimit:             "FileLimitError",
	KindDailyLimit:            "DailyLimitError",
	KindResumeMismatch:        "ResumeMismatchError",
	KindAuthentication:        "AuthenticationError",
	KindPermissionDenied:      "PermissionDeniedError",
	KindInsufficientBalance:   "InsufficientBalanceError",
	KindNotFound:              "NotFoundError",
	KindConflict:              "ConflictError",
	KindValidation:            "ValidationError",
	KindRateLimit:             "RateLimitError",
	KindPaymentMethodRequired: "PaymentMethodRequiredError",
	KindUpload:                "UploadError",
	KindJobFailed:             "JobFailedError",
	KindWaitTimeout:           "WaitTimeoutError",
	KindAbort:                 "AbortError",
	KindNetwork:               "NetworkError",
	KindOutcomeUnknown:        "OutcomeUnknownError",
}

// String returns the class name used by every Hushscript SDK, for example "RateLimitError".
func (k ErrorKind) String() string {
	if name, ok := kindNames[k]; ok {
		return name
	}
	return "ErrorKind(" + strconv.Itoa(int(k)) + ")"
}

// IsPreflight reports whether the kind belongs to the preflight family: errors raised before any
// request that holds credit, so no hold was placed. KindPreflight itself is the family's base,
// raised for an invalid option.
func (k ErrorKind) IsPreflight() bool {
	return k == KindPreflight || (k >= KindUnsupportedMedia && k <= KindResumeMismatch)
}

// IsAPI reports whether the kind is an answer from the API: ApiError and its subclasses.
func (k ErrorKind) IsAPI() bool {
	return k == KindAPI || (k >= KindAuthentication && k <= KindRateLimit)
}

// Error is the one error type of the SDK. Match it with errors.As and branch on Kind or Code.
// Status, NumericCode and RetryAfter are zero when they do not apply, which is always so for errors
// the SDK raises itself.
type Error struct {
	// Code is the API's error slug such as "insufficient_balance", or the SDK's own slug.
	Code string
	// NumericCode is the API's stable numeric code, 0 for errors raised by the SDK.
	NumericCode int
	// Status is the HTTP status, 0 when there was no response.
	Status int
	// Hint tells the caller what to do about it, "" when there is nothing to add.
	Hint string
	// Details holds the extra keys of the error body with the API's own snake_case names, nested
	// objects included.
	Details map[string]any
	// RetryAfter is how long to wait before trying again, when the API said so.
	RetryAfter time.Duration
	// Kind is the class of the error.
	Kind ErrorKind
	// Message is the catalog meaning for API errors, a fixed text for the SDK's own.
	Message string
	// IdempotencyKey is the key of the upload create, set on every error Transcribe raises once the
	// create was sent. Passing it back in TranscribeOptions replays the same job instead of placing
	// a second hold.
	IdempotencyKey string
	// ResumeToken lets Resume pick the job up again. It is set once the job id is known, on a
	// WaitTimeout error and on a cancellation, and on every other error of a Transcribe call.
	ResumeToken string
	// JobID is the id of the failed job, on a JobFailed error.
	JobID string
	// FailureKind is "media_unreadable" or "media_undecodable" when the file itself could not be
	// read, on a JobFailed error; "" otherwise.
	FailureKind string
	// JobError is the job's own failure text, opaque, on a JobFailed error.
	JobError string

	// receivedToken is the token string a claim or rotation answer carried when the rest of the answer
	// could not be used (read it with ReceivedToken).
	receivedToken secret

	cause error
	// causeText is the cause in one redacted line for Error(), set for network failures only.
	causeText string
}

// Error returns the code, the message and the hint, with the token redacted.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("hushscript: ")
	b.WriteString(e.Code)
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Hint != "" {
		b.WriteString(" (")
		b.WriteString(e.Hint)
		b.WriteString(")")
	}
	if e.causeText != "" {
		b.WriteString(" (cause: ")
		b.WriteString(e.causeText)
		b.WriteString(")")
	}
	return b.String()
}

// ReceivedToken is the token string a claim or rotation answer carried when the rest of the answer
// could not be used, so the credential the server showed once is not lost; "" when there is none. It
// is never part of Error(), the message, a formatted error or its JSON; treat it like any token.
func (e *Error) ReceivedToken() string {
	if e == nil {
		return ""
	}
	return e.receivedToken.reveal()
}

// LogValue is the error as slog logs it: the redacted text of Error, never the struct. It is on the
// pointer, so a nil *Error (an error variable that holds a typed nil) logs as "<nil>" and does not panic.
func (e *Error) LogValue() slog.Value {
	if e == nil {
		return slog.StringValue("<nil>")
	}
	return slog.StringValue(e.Error())
}

// Unwrap returns the error that caused this one, for errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.cause }

// redactedCause keeps the original error reachable for errors.Is and errors.As while everything a
// caller can read from it is redacted. It has no Unwrap, because one more step down the chain would
// return the raw error; Is and As look inside it instead, and As hands out only an error whose whole
// chain is free of the token (a transport that echoes request headers into its errors gets withheld).
//
// The raw error and the redactor sit in closures, not in fields: reflection-driven printing (a bad
// fmt verb, spew, testify's diff) shows a function value as an address, so it cannot reach them.
// Unsafe reflection or a memory dump still can.
type redactedCause struct {
	text string
	is   func(target error) bool
	as   func(target any) bool
}

func newRedactedCause(cause error, r redactor) *redactedCause {
	return &redactedCause{
		text: r.apply(cause.Error()),
		is:   func(target error) bool { return errors.Is(cause, target) },
		as:   func(target any) bool { return asRedacted(cause, r, target) },
	}
}

func (c *redactedCause) Error() string { return c.text }

func (c *redactedCause) Is(target error) bool { return c.is(target) }

func (c *redactedCause) As(target any) bool { return c.as(target) }

func asRedacted(cause error, r redactor, target any) bool {
	pointer := reflect.ValueOf(target)
	if pointer.Kind() != reflect.Pointer || pointer.IsNil() {
		return false
	}
	scratch := reflect.New(pointer.Type().Elem())
	if !errors.As(cause, scratch.Interface()) {
		return false
	}
	if found, ok := scratch.Elem().Interface().(error); ok && !chainIsClean(found, r) {
		return false
	}
	pointer.Elem().Set(scratch.Elem())
	return true
}

// chainIsClean reports whether no error in the chain from err down prints anything redaction would change.
func chainIsClean(err error, r redactor) bool {
	if err == nil {
		return true
	}
	if r.apply(err.Error()) != err.Error() {
		return false
	}
	switch wrapped := err.(type) { //nolint:errorlint // walking the chain by hand is the point
	case interface{ Unwrap() error }:
		return chainIsClean(wrapped.Unwrap(), r)
	case interface{ Unwrap() []error }:
		for _, inner := range wrapped.Unwrap() {
			if !chainIsClean(inner, r) {
				return false
			}
		}
	}
	return true
}

// causeSummary is the cause as one line for Error(): a *url.Error without its URL, which for a
// direct upload carries a signature.
func causeSummary(cause error) string {
	var urlErr *url.Error
	if errors.As(cause, &urlErr) && urlErr.Err != nil {
		return urlErr.Op + ": " + urlErr.Err.Error()
	}
	return cause.Error()
}

// newSDKError builds an error the SDK raises itself. The hint comes from the shared hints table by
// code, rendered from inputs.
func newSDKError(kind ErrorKind, code, message string, cause error, r redactor, inputs hintInputs) *Error {
	e := &Error{Code: code, Kind: kind, Message: r.apply(message), Details: map[string]any{}}
	if cause != nil {
		e.cause = newRedactedCause(cause, r)
		if kind == KindNetwork || kind == KindOutcomeUnknown {
			e.causeText = r.apply(causeSummary(cause))
		}
	}
	e.Hint = r.apply(renderHint(code, inputs))
	return e
}

// errorFromResponse builds the error for a non-2xx answer. method and path (the escaped path) locate
// the operation, so that the hint can name the scope it needs.
func errorFromResponse(status int, header http.Header, body []byte, method, path string, r redactor) *Error {
	e := &Error{Status: status, Details: map[string]any{}}
	var raw map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	slug, slugOK := "", false
	if err := decoder.Decode(&raw); err == nil && raw != nil {
		slug, slugOK = raw["error"].(string)
		if number, isNumber := raw["code"].(json.Number); isNumber {
			if code, convErr := strconv.Atoi(number.String()); convErr == nil {
				e.NumericCode = code
			}
		}
	}
	if !slugOK || slug == "" {
		e.Code = "http_" + strconv.Itoa(status)
		e.NumericCode = 0
		e.Message = "The API answered HTTP " + strconv.Itoa(status) + " " + http.StatusText(status) + "."
	} else {
		e.Code = r.apply(slug)
		for key, value := range raw {
			if key == "error" || key == "code" {
				continue
			}
			e.Details[r.apply(key)] = r.value(plainJSON(value))
		}
		if entry, ok := generated.LookupError(slug); ok {
			e.Message = r.apply(entry.Meaning)
		} else {
			e.Message = "The API answered HTTP " + strconv.Itoa(status) + " " + http.StatusText(status) + " with the error " + r.apply(slug) + "."
		}
	}
	e.Kind = kindFor(slug, status)
	inputs := hintInputsFromDetails(e.Details)
	if operation := operationOf(method, path); operation != "" {
		if scope, ok := generated.RequiredScope(operation); ok {
			inputs.scope = some(scope)
		}
	}
	if wait, ok := retryAfterOf(header, inputs); ok {
		e.RetryAfter = wait
		inputs.retryAfterSeconds = some(float64(wait / time.Second))
	}
	if slug != "wrong_state" || jobOrUploadPath(path) {
		e.Hint = r.apply(renderHint(slug, inputs))
	}
	return e
}

// jobOrUploadPath reports whether an escaped request path belongs to a job or upload operation. The
// wrong_state hint names a job call, so it is attached to those operations only (spec section 9.3).
func jobOrUploadPath(path string) bool {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	return len(segments) >= 2 && segments[0] == "v1" && (segments[1] == "jobs" || segments[1] == "uploads")
}

// plainJSON turns json.Number into float64 so callers see ordinary numbers in Details.
func plainJSON(v any) any {
	switch typed := v.(type) {
	case json.Number:
		if f, err := typed.Float64(); err == nil {
			return f
		}
		return typed.String()
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = plainJSON(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = plainJSON(item)
		}
		return out
	default:
		return v
	}
}

// kindFor maps a slug, then a status, to an error kind (spec section 9.2).
func kindFor(slug string, status int) ErrorKind {
	switch slug {
	case "upload_incomplete", "size_exceeded":
		return KindUpload
	case "insufficient_balance", "org_insufficient_balance", "org_pool_expired", "org_plan_frozen":
		return KindInsufficientBalance
	case "agent_claim_invalid", "unauthorized":
		return KindAuthentication
	}
	switch status {
	case http.StatusUnauthorized:
		return KindAuthentication
	case http.StatusForbidden:
		return KindPermissionDenied
	case http.StatusPaymentRequired:
		return KindInsufficientBalance
	case http.StatusNotFound:
		return KindNotFound
	case http.StatusConflict:
		return KindConflict
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return KindValidation
	case http.StatusTooManyRequests:
		return KindRateLimit
	default:
		return KindAPI
	}
}

// retryAfterOf reads Retry-After (integer seconds) and falls back to the retry_after_seconds detail.
// The second result says whether a wait was given at all: a wait of zero is still a wait.
func retryAfterOf(header http.Header, inputs hintInputs) (time.Duration, bool) {
	if wait, ok := parseRetryAfter(header); ok {
		return wait, true
	}
	if seconds, ok := inputs.retryAfterSeconds.get(); ok && seconds <= maxWaitSeconds {
		return time.Duration(seconds * float64(time.Second)), true
	}
	return 0, false
}

// maxWaitSeconds is the longest wait a time.Duration can hold, in whole seconds (about 292 years). A
// Retry-After beyond it, or beyond what a number can say, counts as absent.
const maxWaitSeconds = math.MaxInt64 / 1_000_000_000

// maxDetailNumber is the largest number a hint input may be, the last whole number a float64 holds
// exactly (every SDK's safe-integer bound); a bigger one is treated as absent.
const maxDetailNumber = 1 << 53

func parseRetryAfter(header http.Header) (time.Duration, bool) {
	text := strings.TrimSpace(header.Get("Retry-After"))
	if text == "" {
		return 0, false
	}
	seconds, err := strconv.ParseInt(text, 10, 64)
	if err != nil || seconds < 0 || seconds > maxWaitSeconds {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

// maybe is a value that may be absent. Zero is a legitimate value ("retry after 0 s"), so presence
// is tracked apart from it.
type maybe[T any] struct {
	value T
	ok    bool
}

func some[T any](value T) maybe[T] { return maybe[T]{value: value, ok: true} }

func (m maybe[T]) get() (T, bool) { return m.value, m.ok }

// hintInputs is everything a hint template may mention, with the units the API uses.
type hintInputs struct {
	scope             maybe[string]
	neededSeconds     maybe[float64]
	balanceSeconds    maybe[float64]
	usedSeconds       maybe[float64]
	maxSeconds        maybe[float64]
	retryAfterSeconds maybe[float64]
	allowedPacks      maybe[[]string]
	currentVersion    maybe[string]
}

// hintInputsFromDetails reads the hint inputs from a body's extra keys, at the top level and inside a
// nested "details" object (the nested one wins).
func hintInputsFromDetails(details map[string]any) hintInputs {
	var in hintInputs
	read := func(level map[string]any) {
		number := func(key string, limit float64, into *maybe[float64]) {
			if value, ok := asFloat(level[key]); ok && value >= 0 && value <= limit {
				*into = some(value)
			}
		}
		number("needed_seconds", maxDetailNumber, &in.neededSeconds)
		number("balance_seconds", maxDetailNumber, &in.balanceSeconds)
		number("used_seconds", maxDetailNumber, &in.usedSeconds)
		number("max_seconds", maxDetailNumber, &in.maxSeconds)
		number("retry_after_seconds", maxWaitSeconds, &in.retryAfterSeconds)
		if packs, ok := level["allowed_packs"].([]any); ok {
			var names []string
			for _, pack := range packs {
				if name, isString := pack.(string); isString {
					names = append(names, name)
				}
			}
			if len(names) > 0 {
				in.allowedPacks = some(names)
			}
		}
		if version, ok := level["current_version"].(string); ok && version != "" {
			in.currentVersion = some(version)
		}
	}
	read(details)
	if nested, ok := details["details"].(map[string]any); ok {
		read(nested)
	}
	return in
}

func asFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case json.Number:
		f, err := typed.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

const secondsPerMinute = 60

// hintValues turns the inputs into the placeholder values of spec section 9.3. Minutes: what is
// needed rounds up, what the user has or has used rounds down, so the hint never overstates.
func hintValues(in hintInputs) map[string]string {
	values := map[string]string{}
	whole := func(number float64) string { return strconv.FormatFloat(number, 'f', 0, 64) }
	minutes := func(name string, seconds maybe[float64], round func(float64) float64) {
		if value, ok := seconds.get(); ok {
			values[name] = whole(round(value / secondsPerMinute))
		}
	}
	minutes("needed", in.neededSeconds, math.Ceil)
	minutes("balance", in.balanceSeconds, math.Floor)
	minutes("used", in.usedSeconds, math.Floor)
	minutes("max", in.maxSeconds, math.Floor)
	if value, ok := in.retryAfterSeconds.get(); ok {
		values["retryAfter"] = whole(math.Floor(value))
	}
	if value, ok := in.scope.get(); ok {
		values["scope"] = value
	}
	if value, ok := in.allowedPacks.get(); ok {
		values["allowedPacks"] = strings.Join(value, ", ")
	}
	if value, ok := in.currentVersion.get(); ok {
		values["currentVersion"] = value
	}
	return values
}

// renderHint is the hint of a slug rendered from its inputs, or "" when the slug has none or no
// sentence of it can be filled.
func renderHint(slug string, in hintInputs) string {
	template, ok := generated.Hint(slug)
	if !ok {
		return ""
	}
	return fillHint(template, hintValues(in))
}

var placeholder = regexp.MustCompile(`\{([A-Za-z]+)\}`)

// fillHint renders a hint template (spec section 9.3): split it after each ". " so every sentence
// keeps its own punctuation, drop the sentences that mention a placeholder with no value, fill the
// rest and join them with one space. It returns "" when nothing is left.
func fillHint(template string, values map[string]string) string {
	var kept []string
	for _, sentence := range splitSentences(template) {
		complete := true
		for _, match := range placeholder.FindAllStringSubmatch(sentence, -1) {
			if _, ok := values[match[1]]; !ok {
				complete = false
				break
			}
		}
		if !complete {
			continue
		}
		kept = append(kept, placeholder.ReplaceAllStringFunc(sentence, func(token string) string {
			return values[token[1:len(token)-1]]
		}))
	}
	return strings.Join(kept, " ")
}

// splitSentences cuts after every ". ", keeping the period with its sentence.
func splitSentences(text string) []string {
	var sentences []string
	for {
		index := strings.Index(text, ". ")
		if index < 0 {
			return append(sentences, text)
		}
		sentences = append(sentences, text[:index+1])
		text = text[index+2:]
	}
}

// operationOf finds the "METHOD /template" key of the operation a concrete request path belongs to.
func operationOf(method, path string) string {
	requestSegments := strings.Split(strings.Trim(path, "/"), "/")
	for _, key := range generated.ScopedOperations() {
		keyMethod, template, _ := strings.Cut(key, " ")
		if keyMethod != method {
			continue
		}
		templateSegments := strings.Split(strings.Trim(template, "/"), "/")
		if len(templateSegments) != len(requestSegments) {
			continue
		}
		matches := true
		for i, segment := range templateSegments {
			isVariable := strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}")
			if !isVariable && segment != requestSegments[i] {
				matches = false
				break
			}
		}
		if matches {
			return key
		}
	}
	return ""
}
