package hushscript

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
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
	KindPermission
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
)

var kindNames = map[ErrorKind]string{
	KindAPI:                   "ApiError",
	KindConfiguration:         "ConfigurationError",
	KindUnsupportedMedia:      "UnsupportedMediaError",
	KindFileLimit:             "FileLimitError",
	KindDailyLimit:            "DailyLimitError",
	KindResumeMismatch:        "ResumeMismatchError",
	KindAuthentication:        "AuthenticationError",
	KindPermission:            "PermissionError",
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
// request that holds credit, so no hold was placed.
func (k ErrorKind) IsPreflight() bool {
	return k >= KindUnsupportedMedia && k <= KindResumeMismatch
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

	cause error
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
	return b.String()
}

// Unwrap returns the error that caused this one, for errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.cause }

// redactedCause keeps the original error reachable for errors.Is and errors.As while its text is
// redacted.
type redactedCause struct {
	text string
	err  error
}

func (c *redactedCause) Error() string { return c.text }

func (c *redactedCause) Unwrap() error { return c.err }

// newSDKError builds an error the SDK raises itself. The hint comes from the shared hints table by code.
func newSDKError(kind ErrorKind, code, message string, cause error, r redactor) *Error {
	e := &Error{Code: code, Kind: kind, Message: r.apply(message), Details: map[string]any{}}
	if cause != nil {
		e.cause = &redactedCause{text: r.apply(cause.Error()), err: cause}
	}
	if template, ok := generated.Hint(code); ok {
		e.Hint = r.apply(fillHint(template, nil))
	}
	return e
}

// errorFromResponse builds the error for a non-2xx answer. method and path locate the operation, so
// that the hint can name the scope it needs.
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
	e.RetryAfter = retryAfterOf(header, e.Details)
	if template, ok := generated.Hint(slug); ok {
		e.Hint = r.apply(fillHint(template, hintValues(e, method, path)))
	}
	return e
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

// camelCase turns an API key into the name hint placeholders use ("needed_seconds" is "neededSeconds").
func camelCase(snake string) string {
	parts := strings.Split(snake, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
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
		return KindPermission
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
func retryAfterOf(header http.Header, details map[string]any) time.Duration {
	if seconds, ok := parseRetryAfter(header); ok {
		return seconds
	}
	nested, _ := details["details"].(map[string]any)
	if seconds, ok := nested["retry_after_seconds"].(float64); ok && seconds > 0 {
		return time.Duration(seconds * float64(time.Second))
	}
	return 0
}

func parseRetryAfter(header http.Header) (time.Duration, bool) {
	text := strings.TrimSpace(header.Get("Retry-After"))
	if text == "" {
		return 0, false
	}
	seconds, err := strconv.Atoi(text)
	if err != nil || seconds < 0 {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

// hintValues collects what a hint template may mention: every detail by its camelCase name, the
// operation's scope, the balance in minutes and the wait.
func hintValues(e *Error, method, path string) map[string]string {
	values := map[string]string{}
	addValues := func(details map[string]any) {
		for key, value := range details {
			if _, nested := value.(map[string]any); !nested {
				values[camelCase(key)] = formatDetail(value)
			}
		}
	}
	addValues(e.Details)
	if nested, ok := e.Details["details"].(map[string]any); ok {
		addValues(nested)
	}
	if operation := operationOf(method, path); operation != "" {
		if scope, ok := generated.RequiredScope(operation); ok {
			values["scope"] = scope
		}
	}
	// Seconds shown as minutes: what is needed rounds up, what the user has rounds down.
	if seconds, ok := e.Details["needed_seconds"].(float64); ok {
		values["needed"] = strconv.Itoa(int(math.Ceil(seconds / secondsPerMinute)))
	}
	if seconds, ok := e.Details["balance_seconds"].(float64); ok {
		values["balance"] = strconv.Itoa(int(math.Floor(seconds / secondsPerMinute)))
	}
	if e.RetryAfter > 0 {
		values["retryAfter"] = strconv.Itoa(int(e.RetryAfter / time.Second))
	}
	return values
}

const secondsPerMinute = 60

func formatDetail(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case []any:
		parts := make([]string, len(typed))
		for i, item := range typed {
			parts[i] = formatDetail(item)
		}
		return strings.Join(parts, ", ")
	default:
		return fmt.Sprint(typed)
	}
}

var placeholder = regexp.MustCompile(`\{([A-Za-z]+)\}`)

// fillHint renders a hint template (spec section 9.3): split it on ". ", drop every sentence that
// mentions a placeholder with no value, fill the rest and join them with ". ". It returns "" when
// nothing is left.
func fillHint(template string, values map[string]string) string {
	var kept []string
	for _, sentence := range strings.Split(template, ". ") {
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
	return strings.Join(kept, ". ")
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
