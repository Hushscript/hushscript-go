// Package conformance runs the shared conformance scenarios (spec section 12) against the SDK, as
// docs/runner-contract.md in the conformance repository describes. It needs the mock, so it only runs
// through `just conformance`, which sets HUSHSCRIPT_CONFORMANCE=1.
package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	hushscript "github.com/Hushscript/hushscript-go"
	"github.com/Hushscript/hushscript-go/generated"
)

// testToken is the mock's TEST_TOKEN (runner contract, section 1).
const testToken = "hsr1:eu:pat." + "0123456789abcdef0123456789abcdef" + "." +
	"fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

type scenarioFile struct {
	Name   string `json:"name"`
	Call   call   `json:"call"`
	Expect expect `json:"expect"`
}

type call struct {
	Op    string         `json:"op"`
	Args  map[string]any `json:"args"`
	Prior *call          `json:"prior"`
}

type expect struct {
	Result map[string]any `json:"result"`
	Error  *errorExpect   `json:"error"`
}

type errorExpect struct {
	Class           string         `json:"class"`
	Code            string         `json:"code"`
	NumericCode     *int           `json:"numericCode"`
	Status          *int           `json:"status"`
	Details         map[string]any `json:"details"`
	RetryAfter      *float64       `json:"retryAfter"`
	HintContains    string         `json:"hintContains"`
	Fields          map[string]any `json:"fields"`
	MessageExcludes []string       `json:"messageExcludes"`
}

// operations maps call.op to what the SDK does. Later tasks add the rest; an op that is not here is
// reported as skipped, which is scaffold-only (the contract says to fail it once every op exists).
var operations = map[string]operation{
	"balance":         {run: balance},
	"claim":           {run: claim, errorsOnly: true},
	"rotate":          {run: rotate, errorsOnly: true},
	"listTranscripts": {run: firstPage, errorsOnly: true},
}

// operation is one mapped call.op. errorsOnly marks a call that goes through the generated client for
// now: it can prove how failures surface, but not what a success returns, so scenarios that expect a
// result stay skipped until the friendly layer exists.
type operation struct {
	run        func(context.Context, *hushscript.Client, map[string]any) (map[string]any, error)
	errorsOnly bool
}

var errNotMappedYet = errors.New("the friendly layer for this call does not exist yet")

func claim(ctx context.Context, client *hushscript.Client, args map[string]any) (map[string]any, error) {
	signupID, _ := args["signupId"].(string)
	secret, _ := args["claimSecret"].(string)
	_, err := client.API().ClaimAgentAccountWithResponse(ctx, signupID, generated.ClaimAgentAccountJSONRequestBody{ClaimSecret: secret})
	if err != nil {
		return nil, err
	}
	return nil, errNotMappedYet
}

func rotate(ctx context.Context, client *hushscript.Client, _ map[string]any) (map[string]any, error) {
	if _, err := client.API().RotateAgentCredentialsWithResponse(ctx); err != nil {
		return nil, err
	}
	return nil, errNotMappedYet
}

func firstPage(ctx context.Context, client *hushscript.Client, _ map[string]any) (map[string]any, error) {
	if _, err := client.API().ListTranscriptsWithResponse(ctx, &generated.ListTranscriptsParams{}); err != nil {
		return nil, err
	}
	return nil, errNotMappedYet
}

func balance(ctx context.Context, client *hushscript.Client, _ map[string]any) (map[string]any, error) {
	response, err := client.API().GetAccountWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, fmt.Errorf("GET /v1/account: no JSON body in the %d answer", response.StatusCode())
	}
	seconds := response.JSON200.BalanceSeconds
	return map[string]any{"seconds": float64(seconds), "minutes": float64(seconds / 60)}, nil
}

func TestConformance(t *testing.T) {
	if os.Getenv("HUSHSCRIPT_CONFORMANCE") != "1" {
		t.Skip("the conformance runner needs the mock; run it through `just conformance`")
	}
	ctx := context.Background()
	mock := newMockClient()
	names, err := mock.scenarios(ctx)
	if err != nil || len(names) == 0 {
		t.Fatalf("listing scenarios: %v (%d found)", err, len(names))
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) { runScenario(ctx, t, mock, name) })
	}
}

func runScenario(parent context.Context, t *testing.T, mock *mockClient, name string) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	scenario, err := mock.scenario(ctx, name)
	if err != nil {
		t.Fatalf("loading the scenario: %v", err)
	}
	mapped, known := operations[scenario.Call.Op]
	if !known || scenario.Call.Prior != nil || (mapped.errorsOnly && scenario.Expect.Error == nil) {
		// The contract says a runner must not skip. Until every op is mapped a skip passes, and
		// CONFORMANCE_STRICT=1 turns it into the failure the contract asks for.
		if os.Getenv("CONFORMANCE_STRICT") == "1" {
			t.Fatalf("op %q is not mapped yet", scenario.Call.Op)
		}
		t.Skipf("op %q is not mapped yet", scenario.Call.Op)
	}
	if err = mock.load(ctx, name); err != nil {
		t.Fatal(err)
	}

	clock := &virtualClock{}
	transport := &countingTransport{inner: http.DefaultTransport}
	client, err := hushscript.New(
		hushscript.WithToken(testToken),
		hushscript.WithBaseURL(mockBaseURL),
		hushscript.WithClock(clock),
		hushscript.WithHTTPClient(&http.Client{Transport: transport}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err = mock.mark(ctx); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		result map[string]any
		err    error
	}
	finished := make(chan outcome, 1)
	done := make(chan struct{})
	go func() {
		result, callErr := mapped.run(ctx, client, scenario.Call.Args)
		finished <- outcome{result, callErr}
		close(done)
	}()
	drive := &driver{mock: mock, clock: clock, transport: transport}
	if err = drive.run(ctx, done); err != nil {
		t.Fatal(err)
	}
	got := <-finished

	switch {
	case scenario.Expect.Error != nil:
		if got.err == nil {
			t.Fatalf("expected an error, the call returned %v", got.result)
		}
		for _, problem := range checkError(got.err, scenario.Expect.Error) {
			t.Error(problem)
		}
	case got.err != nil:
		t.Fatalf("unexpected error: %v", got.err)
	default:
		if err = subset(scenario.Expect.Result, got.result, "result"); err != nil {
			t.Error(err)
		}
	}
	verdict, err := mock.verify(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range verdict.Failures {
		t.Errorf("mock: %s", failure)
	}
}

func checkError(err error, want *errorExpect) []string {
	var sdkErr *hushscript.Error
	if !errors.As(err, &sdkErr) {
		return []string{fmt.Sprintf("error %v (%T) is not a *hushscript.Error", err, err)}
	}
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if want.Class != "" && !classMatches(sdkErr.Kind, want.Class) {
		add("class %s, want %s", sdkErr.Kind, want.Class)
	}
	if want.Code != "" && sdkErr.Code != want.Code {
		add("code %q, want %q", sdkErr.Code, want.Code)
	}
	if want.NumericCode != nil && sdkErr.NumericCode != *want.NumericCode {
		add("numericCode %d, want %d", sdkErr.NumericCode, *want.NumericCode)
	}
	if want.Status != nil && sdkErr.Status != *want.Status {
		add("status %d, want %d", sdkErr.Status, *want.Status)
	}
	if want.RetryAfter != nil && sdkErr.RetryAfter.Seconds() != *want.RetryAfter {
		add("retryAfter %v, want %v s", sdkErr.RetryAfter, *want.RetryAfter)
	}
	if want.HintContains != "" && !strings.Contains(sdkErr.Hint, want.HintContains) {
		add("hint %q lacks %q", sdkErr.Hint, want.HintContains)
	}
	if want.Details != nil {
		if problem := subset(want.Details, camelKeys(sdkErr.Details), "details"); problem != nil {
			add("%v", problem)
		}
	}
	surfaces := []string{sdkErr.Message, sdkErr.Hint, fmt.Sprint(sdkErr.Details), err.Error(), fmt.Sprintf("%+v", sdkErr)}
	for cause := errors.Unwrap(sdkErr); cause != nil; cause = errors.Unwrap(cause) {
		surfaces = append(surfaces, cause.Error())
	}
	for _, excluded := range want.MessageExcludes {
		excluded = strings.ReplaceAll(excluded, "$TOKEN", testToken)
		for _, surface := range surfaces {
			if strings.Contains(surface, excluded) {
				add("%q appears in %q", excluded, surface)
			}
		}
	}
	return problems
}

// classMatches maps the contract's class names onto Kind (HushscriptError accepts any SDK error).
func classMatches(kind hushscript.ErrorKind, class string) bool {
	switch class {
	case "HushscriptError":
		return true
	case "PreflightError":
		return kind.IsPreflight()
	case "ApiError":
		return kind.IsAPI()
	default:
		return kind.String() == class
	}
}

// camelKeys renames the API's snake_case detail keys, which the scenarios spell in camelCase.
func camelKeys(v any) any {
	switch typed := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, value := range typed {
			parts := strings.Split(key, "_")
			for i := 1; i < len(parts); i++ {
				if parts[i] != "" {
					parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
				}
			}
			out[strings.Join(parts, "")] = camelKeys(value)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = camelKeys(item)
		}
		return out
	default:
		return v
	}
}

// subset is the contract's result match: objects match key by key, arrays element by element at
// equal length, scalars by equality (numbers compared as numbers).
func subset(want, got any, path string) error {
	switch expected := want.(type) {
	case map[string]any:
		actual, ok := got.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: want an object, got %v", path, got)
		}
		for key, value := range expected {
			if err := subset(value, actual[key], path+"."+key); err != nil {
				return err
			}
		}
		return nil
	case []any:
		actual, ok := got.([]any)
		if !ok || len(actual) != len(expected) {
			return fmt.Errorf("%s: want %v, got %v", path, want, got)
		}
		for i := range expected {
			if err := subset(expected[i], actual[i], fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	default:
		wantJSON, _ := json.Marshal(want)
		gotJSON, _ := json.Marshal(got)
		if !bytes.Equal(wantJSON, gotJSON) {
			return fmt.Errorf("%s: want %s, got %s", path, wantJSON, gotJSON)
		}
		return nil
	}
}
