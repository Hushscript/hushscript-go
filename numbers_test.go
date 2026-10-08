package hushscript

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Spec 9.2: every number the SDK parses is parsed totally. Out of range or non-finite counts as absent.

var hugeNumbers = map[string]string{
	"400 nines":       strings.Repeat("9", 400),
	"1e400":           "1e400",
	"1e300":           "1e300",
	"9.3e18":          "9.3e18",
	"max int64":       "9223372036854775807",
	"past a Duration": "9223372037",
	"min int64":       "-9223372036854775808",
}

// R71: a Retry-After integer past 60, however large, is "over 60": reported capped, never overflowed.
// Only a value that is not a non-negative integer is absent.
func TestARetryAfterNoDurationCanHoldIsCappedNotAbsent(t *testing.T) {
	const longest = maxWaitSeconds * time.Second
	for name, text := range hugeNumbers {
		header := http.Header{"Retry-After": []string{text}}
		wait, ok := parseRetryAfter(header)
		if strings.Trim(text, "0123456789") != "" {
			if ok {
				t.Errorf("%s: Retry-After %.20s read as %v", name, text, wait)
			}
			continue
		}
		t.Run(name, func(t *testing.T) {
			if !ok || wait != longest {
				t.Fatalf("Retry-After %.20s read as %v, %v", text, wait, ok)
			}
			e := fromResponse(429, header, jsonError("rate_limited", 5000), http.MethodGet, "/v1/account")
			if e.RetryAfter != longest || e.Kind != KindRateLimit {
				t.Errorf("Retry-After %.20s: retryAfter %v, kind %s", text, e.RetryAfter, e.Kind)
			}
			d := &doer{}
			if retry, _ := d.statusRetry(classSafe, &retryState{}, e, header, 3); retry {
				t.Errorf("Retry-After %.20s was retried", text)
			}
		})
	}
	for _, text := range []string{"NaN", "Infinity", "-1", "1.5", "1e3", " ", "0x10"} {
		if wait, ok := parseRetryAfter(http.Header{"Retry-After": []string{text}}); ok {
			t.Errorf("Retry-After %q read as %v", text, wait)
		}
	}
	const edge = "9223372036"
	if wait, ok := parseRetryAfter(http.Header{"Retry-After": []string{edge}}); !ok || wait != 9223372036*time.Second {
		t.Errorf("the last whole second a Duration holds: %v, %v", wait, ok)
	}
}

func TestADetailNumberOutOfRangeIsAbsent(t *testing.T) {
	for name, text := range hugeNumbers {
		if strings.HasPrefix(text, "-") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			body := `{"error":"rate_limited","code":5000,"retry_after_seconds":` + text + `,"details":{"retry_after_seconds":` + text + `}}`
			e := fromResponse(429, nil, body, http.MethodGet, "/v1/account")
			// A finite number past what a Duration holds is capped; one no float holds is absent.
			capped := text != "1e400"
			if capped != (e.RetryAfter == maxWaitSeconds*time.Second) || (!capped && e.RetryAfter != 0) || e.Kind != KindRateLimit {
				t.Errorf("retry_after_seconds %.20s: retryAfter %v, kind %s", text, e.RetryAfter, e.Kind)
			}
			// Past 2^53 a hint number is absent; the numeric code is any int the platform has.
			body = `{"error":"insufficient_balance","code":3001,"needed_seconds":` + text + `,"balance_seconds":` + text + `}`
			e = fromResponse(402, nil, body, http.MethodPost, "/v1/uploads")
			if e.Kind != KindInsufficientBalance || (name != "past a Duration" && (strings.Contains(e.Hint, "needed") || len(e.Hint) > 100)) {
				t.Errorf("needed_seconds %.20s: kind %s, hint %q", text, e.Kind, e.Hint)
			}
			body = `{"error":"insufficient_balance","code":` + text + `}`
			if e = fromResponse(402, nil, body, http.MethodPost, "/v1/uploads"); e.Code != "insufficient_balance" || (e.NumericCode != 0) != (name == "max int64" || name == "past a Duration") {
				t.Errorf("code %.20s: numeric code %d, code %q", text, e.NumericCode, e.Code)
			}
		})
	}
	// The last numbers that are still numbers.
	e := fromResponse(429, nil, `{"error":"rate_limited","code":5000,"retry_after_seconds":9223372036}`, http.MethodGet, "/v1/account")
	if e.RetryAfter != 9223372036*time.Second {
		t.Errorf("the largest wait a Duration holds: %v", e.RetryAfter)
	}
}

// A purchase that was sent and answered with an unreadable number is still an SDK error that carries
// the purchase's key, and never a panic.
func TestABuyAnsweredWithAHugeNumberFailsAsAnSDKErrorCarryingTheKey(t *testing.T) {
	huge := strings.Repeat("9", 400)
	for name, answer := range map[string]step{
		"429 with a 400 digit Retry-After": {status: 429, headers: map[string]string{"Retry-After": huge}, body: jsonError("rate_limited", 5000)},
		"429 with 1e400 details":           {status: 429, body: `{"error":"agent_purchase_capped","code":4038,"details":{"retry_after_seconds":1e400}}`},
		"402 with 1e400 details":           {status: 402, body: `{"error":"insufficient_balance","code":3001,"needed_seconds":1e400,"balance_seconds":1e400}`},
		"402 with NaN details":             {status: 402, body: `{"error":"insufficient_balance","code":3001,"needed_seconds":NaN}`},
		"402 with a huge integer":          {status: 402, body: `{"error":"insufficient_balance","code":3001,"needed_seconds":` + huge + `}`},
		"2xx with a huge balance":          {status: 200, body: `{"status":"ok","balance_seconds":` + huge + `}`},
		"2xx with 1e400":                   {status: 200, body: `{"status":"ok","balance_seconds":1e400}`},
		"2xx with NaN":                     {status: 200, body: `{"status":"ok","balance_seconds":NaN}`},
		"2xx with a fraction":              {status: 200, body: `{"status":"ok","balance_seconds":1.5}`},
		"201 with a huge balance":          {status: 201, body: `{"status":"ok","balance_seconds":` + huge + `}`},
		"203 with 1e400":                   {status: 203, body: `{"status":"ok","balance_seconds":1e400}`},
	} {
		t.Run(name, func(t *testing.T) {
			api := buyServer(t, defaultCard, answer)
			_, err := api.client(t).Buy(context.Background(), "45min", &BuyOptions{IdempotencyKey: "numbers-key-1"})
			if err == nil {
				t.Fatal("no error")
			}
			e := asError(t, err)
			if answer.status >= 200 && answer.status < 300 {
				e = wantCode(t, err, KindAPI, "bad_response")
				if e.Status != answer.status {
					t.Errorf("status = %d, want %d", e.Status, answer.status)
				}
			}
			if e.IdempotencyKey != "numbers-key-1" {
				t.Errorf("%s %q carries key %q", e.Kind, e.Code, e.IdempotencyKey)
			}
		})
	}
	api := buyServer(t, defaultCard, step{status: 200, headers: map[string]string{"Retry-After": huge}, body: `{"status":"ok","balance_seconds":600}`})
	got, err := api.client(t).Buy(context.Background(), "45min", nil)
	if err != nil || got.Seconds != 600 {
		t.Errorf("a 200 with a 400 digit Retry-After: %+v, %v", got, err)
	}
}

// Spec 9.2: once a request that carries a key or a resume token was sent, a failure that is not an SDK
// error reaches the caller as unexpected_error, with the key, the token and a redacted cause.
func TestAFailureThatIsNotAnSDKErrorAfterTheSendIsUnexpectedError(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1", newFakeClock()).core
	cause := errors.New("decoder broke while holding " + testToken)
	got := (&annotator{core: c, key: "k-123456789012345", token: "resume-token"}).annotate(context.Background(), cause)
	e := asError(t, got)
	if e.Code != "unexpected_error" || e.Kind != KindUnexpected || e.Status != 0 || e.IdempotencyKey != "k-123456789012345" || e.ResumeToken != "resume-token" {
		t.Fatalf("error = %+v", e)
	}
	if want := "The SDK failed unexpectedly after the request was sent. *errors.errorString: decoder broke while holding hsr…[redacted]"; e.Message != want {
		t.Errorf("message = %q, want %q", e.Message, want)
	}
	if !errors.Is(e, cause) {
		t.Error("the original is not reachable through errors.Is")
	}
	// With only the key the SDK cannot tell whether the create reached the server, and says so.
	keyOnly := asError(t, (&annotator{core: c, key: "k-123456789012345"}).annotate(context.Background(), cause))
	if want := "The SDK failed unexpectedly, and the request may or may not have been sent. *errors.errorString: decoder broke while holding hsr…[redacted]"; keyOnly.Code != "unexpected_error" || keyOnly.Message != want {
		t.Errorf("key only: %q %q, want %q", keyOnly.Code, keyOnly.Message, want)
	}
	for _, text := range []string{e.Error(), errors.Unwrap(e).Error()} {
		if strings.Contains(text, testSecret) {
			t.Errorf("the token leaks through %q", text)
		}
	}
	// Before anything keyed was sent the same failure is the SDK's own request-building error, and a
	// cancellation stays an abort.
	unkeyed := asError(t, (&annotator{core: c}).annotate(context.Background(), cause))
	if want := "The request could not be built. *errors.errorString: decoder broke while holding hsr…[redacted]"; unkeyed.Code != "config_invalid" || unkeyed.Message != want {
		t.Errorf("unkeyed failure is %q %q, want %q", unkeyed.Code, unkeyed.Message, want)
	}
	if e := asError(t, (&annotator{core: c, key: "k-123456789012345"}).annotate(context.Background(), context.Canceled)); e.Kind != KindAbort || e.IdempotencyKey == "" {
		t.Errorf("a cancellation is %s %q", e.Kind, e.Code)
	}
}

// A float64 duration is checked before it is turned into an integer and before any request: 1e300 and
// 9.3e18 do not fit an int, and a cast would wrap.
func TestADurationOverrideThatCannotBeAnIntegerIsRefusedBeforeAnyRequest(t *testing.T) {
	for name, tc := range map[string]struct {
		seconds float64
		kind    ErrorKind
		code    string
	}{
		"1e300":     {1e300, KindFileLimit, "duration_out_of_range"},
		"9.3e18":    {9.3e18, KindFileLimit, "duration_out_of_range"},
		"max int64": {math.MaxInt64, KindFileLimit, "duration_out_of_range"},
		"max float": {math.MaxFloat64, KindFileLimit, "duration_out_of_range"},
		"+Inf":      {math.Inf(1), KindPreflight, "invalid_option"},
		"-Inf":      {math.Inf(-1), KindPreflight, "invalid_option"},
		"NaN":       {math.NaN(), KindPreflight, "invalid_option"},
		"negative":  {-20, KindPreflight, "invalid_option"},
		"36000.5":   {36000.5, KindFileLimit, "duration_out_of_range"},
		"14":        {14, KindFileLimit, "duration_out_of_range"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			_, err := r.transcribe(&TranscribeOptions{DurationSeconds: tc.seconds})
			e := wantCode(t, err, tc.kind, tc.code)
			if r.api.total() != 0 || e.IdempotencyKey != "" {
				t.Errorf("%d requests, key %q", r.api.total(), e.IdempotencyKey)
			}
		})
	}
}

// A resume token is caller input too: a duration that is no integer in range would wrap when the
// create is replayed.
func TestAResumeTokenWithADurationOutOfRangeIsRefusedBeforeAnyRequest(t *testing.T) {
	for name, seconds := range map[string]float64{"1e300": 1e300, "9.3e18": 9.3e18, "36001": 36001, "14": 14, "zero": 0, "negative": -20} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			token := encodeResumeToken(resumeState{JobID: "job_1", IdempotencyKey: "numbers-key-1", Size: int64(len(r.wav)), DurationSeconds: seconds})
			_, err := r.client.Resume(context.Background(), token, &ResumeOptions{})
			_ = wantCode(t, err, KindConfiguration, "config_invalid")
			if r.api.total() != 0 {
				t.Errorf("%d requests", r.api.total())
			}
		})
	}
	state := resumeState{JobID: "job_1", IdempotencyKey: "numbers-key-1", Size: 640044}
	for _, seconds := range []float64{15, 20, 36000} {
		state.DurationSeconds = seconds
		if _, err := decodeResumeToken(encodeResumeToken(state)); err != nil {
			t.Errorf("a token with %v s is refused: %v", seconds, err)
		}
	}
}

func FuzzErrorFromResponse(f *testing.F) {
	huge := strings.Repeat("9", 400)
	for _, seed := range []struct {
		status           int
		retryAfter, body string
	}{
		{429, huge, `{"error":"rate_limited","code":5000}`},
		{429, "9223372037", `{"error":"rate_limited","code":5000,"retry_after_seconds":1e400}`},
		{429, "", `{"error":"agent_purchase_capped","code":4038,"details":{"retry_after_seconds":` + huge + `}}`},
		{402, "-1", `{"error":"insufficient_balance","code":3001,"needed_seconds":1e300,"balance_seconds":NaN}`},
		{402, "NaN", `{"error":"insufficient_balance","code":` + huge + `,"needed_seconds":9.3e18}`},
		{409, "0", `{"error":"wrong_state","code":4001,"details":{"details":{"details":[1e999,[],{}]}}}`},
		{500, "1e3", `null`},
		{200, "", ``},
	} {
		f.Add(seed.status, seed.retryAfter, []byte(seed.body))
	}
	f.Fuzz(func(t *testing.T, status int, retryAfter string, body []byte) {
		header := http.Header{}
		header.Set("Retry-After", retryAfter)
		e := errorFromResponse(status, header, body, http.MethodPost, "/v1/uploads", newRedactor(testToken))
		if e == nil || e.Code == "" || e.Details == nil || e.RetryAfter < 0 {
			t.Fatalf("status %d, Retry-After %q, body %q gave %+v", status, retryAfter, body, e)
		}
		_ = e.Error()
	})
}
