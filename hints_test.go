package hushscript

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hushscript/hushscript-go/generated"
)

type hintCase struct {
	Slug     string         `json:"slug"`
	Scope    string         `json:"scope"`
	Details  map[string]any `json:"details"`
	Expected *string        `json:"expected"`
}

// loadHintCases reads the shared fixture. It is a hard requirement: a run without the conformance
// checkout fails, so the hint rules cannot go untested by leaving a variable unset.
func loadHintCases(t *testing.T) []hintCase {
	t.Helper()
	dir := os.Getenv("CONFORMANCE_DIR")
	if dir == "" {
		t.Fatal("CONFORMANCE_DIR must point at a checkout of hushscript-sdk-conformance (the justfile sets it)")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "fixtures", "hint-rendering.json")) //nolint:gosec // a fixture path from the environment
	if err != nil {
		t.Fatal(err)
	}
	var cases []hintCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("the hint fixture is empty")
	}
	return cases
}

func (c hintCase) want() string {
	if c.Expected == nil {
		return ""
	}
	return *c.Expected
}

func TestRendersEveryCaseOfTheSharedHintFixture(t *testing.T) {
	for i, tc := range loadHintCases(t) {
		inputs := hintInputsFromDetails(tc.Details)
		if tc.Scope != "" {
			inputs.scope = some(tc.Scope)
		}
		if got := renderHint(tc.Slug, inputs); got != tc.want() {
			t.Errorf("case %d (%s %v):\n got  %q\n want %q", i, tc.Slug, tc.Details, got, tc.want())
		}
	}
}

func TestRendersTheSharedHintFixtureFromAnAPIBody(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for i, tc := range loadHintCases(t) {
			body := map[string]any{"error": tc.Slug, "code": 1}
			if nested {
				body["details"] = tc.Details
			} else {
				for key, value := range tc.Details {
					body[key] = value
				}
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			// A scope comes from the operation: transcripts are read with transcripts:read, claim needs none.
			method, path := http.MethodPost, "/v1/agent/accounts/sg_1/claim"
			if tc.Scope != "" {
				method, path = http.MethodGet, "/v1/transcripts"
			}
			e := errorFromResponse(http.StatusForbidden, http.Header{}, raw, method, path, redactor{})
			if e.Hint != tc.want() {
				t.Errorf("nested=%v case %d (%s %v):\n got  %q\n want %q", nested, i, tc.Slug, tc.Details, e.Hint, tc.want())
			}
		}
	}
}

func TestARetryAfterOfZeroIsAWait(t *testing.T) {
	const want = "Too many requests. The SDK already waited and retried; slow down or try again in 0 s."
	header := http.Header{"Retry-After": []string{"0"}}
	e := fromResponse(429, header, jsonError("rate_limited", 5000), http.MethodGet, "/v1/account")
	if e.Hint != want || e.RetryAfter != 0 {
		t.Errorf("Retry-After: 0 gave hint %q, retryAfter %v", e.Hint, e.RetryAfter)
	}
	nested := `{"error":"rate_limited","code":5000,"details":{"retry_after_seconds":0}}`
	if e := fromResponse(429, nil, nested, http.MethodGet, "/v1/account"); e.Hint != want {
		t.Errorf("nested retry_after_seconds 0 gave hint %q", e.Hint)
	}
	if e := fromResponse(429, nil, jsonError("rate_limited", 5000), http.MethodGet, "/v1/account"); e.Hint != "Too many requests." {
		t.Errorf("no wait gave hint %q", e.Hint)
	}
}

func TestErrorsTheSDKRaisesRenderTheirHintFromInputs(t *testing.T) {
	e := newSDKError(KindDailyLimit, "daily_limit", "limit", nil, redactor{},
		hintInputs{usedSeconds: some(3599.0), maxSeconds: some(86400.0)})
	if want := "This upload would pass your daily transcription limit (59 of 1440 min used in the last 24 h)."; e.Hint != want {
		t.Errorf("hint = %q", e.Hint)
	}
	if e := newSDKError(KindDailyLimit, "daily_limit", "limit", nil, redactor{}, hintInputs{}); e.Hint != "" {
		t.Errorf("a daily limit with no numbers has hint %q", e.Hint)
	}
}

func TestOnlyTheMissingTokenConfigurationErrorCarriesAHint(t *testing.T) {
	t.Setenv("HUSHSCRIPT_TOKEN", "")
	for name, build := range map[string]func() error{
		"timeout": func() error { _, err := New(WithToken(testToken), WithTimeout(-1)); return err },
		"retries": func() error { _, err := New(WithToken(testToken), WithMaxRetries(-1)); return err },
		"token":   func() error { _, err := New(WithToken("sk-nope")); return err },
		"url":     func() error { _, err := New(WithToken(testToken), WithBaseURL("ftp://x")); return err },
	} {
		e := asError(t, build())
		if e.Hint != "" || strings.Contains(e.Error(), "(") {
			t.Errorf("%s: hint %q, text %q", name, e.Hint, e.Error())
		}
	}
	_, err := New()
	if e := asError(t, err); e.Hint == "" || !strings.HasPrefix(e.Hint, "Set HUSHSCRIPT_TOKEN or pass a token.") {
		t.Errorf("missing token hint = %q", e.Hint)
	}
	for _, slug := range generated.HintSlugs() {
		text, _ := generated.Hint(slug)
		if strings.Contains(text, "message carries") || slug == "config_invalid" {
			t.Errorf("the hints table holds a placeholder for %s: %q", slug, text)
		}
	}
}
