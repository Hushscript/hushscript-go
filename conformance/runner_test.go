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
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	hushscript "github.com/Hushscript/hushscript-go"
	"github.com/Hushscript/hushscript-go/generated"
	"github.com/Hushscript/hushscript-go/internal/probe"
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
	Op          string         `json:"op"`
	Args        map[string]any `json:"args"`
	Prior       *call          `json:"prior"`
	ExpectError *errorExpect   `json:"expectError"`
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

// env is what a mapped op works with: the client, the media downloaded from the mock, and the resume
// token of the prior call's error.
type env struct {
	client *hushscript.Client
	media  func(ctx context.Context, name string) (string, error)
	token  string
}

// operations maps call.op to what the SDK does. An op that is not here is reported as skipped,
// which is scaffold-only (the contract says to fail it once every op exists).
var operations = map[string]operation{
	"balance":         {run: balance},
	"claim":           {run: claim, errorsOnly: true},
	"rotate":          {run: rotate, errorsOnly: true},
	"listTranscripts": {run: firstPage, errorsOnly: true},
	"transcribe":      {run: transcribe},
	"resume":          {run: resume},
	"probe":           {run: probeDurations},
}

// operation is one mapped call.op. errorsOnly marks a call that goes through the generated client for
// now: it can prove how failures surface, but not what a success returns, so scenarios that expect a
// result stay skipped until the friendly layer exists.
type operation struct {
	run        func(context.Context, *env, map[string]any) (map[string]any, error)
	errorsOnly bool
}

var errNotMappedYet = errors.New("the friendly layer for this call does not exist yet")

func claim(ctx context.Context, e *env, args map[string]any) (map[string]any, error) {
	signupID, _ := args["signupId"].(string)
	secret, _ := args["claimSecret"].(string)
	_, err := e.client.API().ClaimAgentAccountWithResponse(ctx, signupID, generated.ClaimAgentAccountJSONRequestBody{ClaimSecret: secret})
	if err != nil {
		return nil, err
	}
	return nil, errNotMappedYet
}

func rotate(ctx context.Context, e *env, _ map[string]any) (map[string]any, error) {
	if _, err := e.client.API().RotateAgentCredentialsWithResponse(ctx); err != nil {
		return nil, err
	}
	return nil, errNotMappedYet
}

func firstPage(ctx context.Context, e *env, _ map[string]any) (map[string]any, error) {
	if _, err := e.client.API().ListTranscriptsWithResponse(ctx, &generated.ListTranscriptsParams{}); err != nil {
		return nil, err
	}
	return nil, errNotMappedYet
}

func balance(ctx context.Context, e *env, _ map[string]any) (map[string]any, error) {
	response, err := e.client.API().GetAccountWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, fmt.Errorf("GET /v1/account: no JSON body in the %d answer", response.StatusCode())
	}
	seconds := response.JSON200.BalanceSeconds
	return map[string]any{"seconds": float64(seconds), "minutes": float64(seconds / 60)}, nil
}

// transcribeOptions reads a scenario's camelCase options into the SDK's.
func transcribeOptions(args map[string]any) *hushscript.TranscribeOptions {
	raw, _ := args["options"].(map[string]any)
	opts := &hushscript.TranscribeOptions{}
	if seconds, ok := raw["waitTimeout"].(float64); ok {
		opts.WaitTimeout = time.Duration(seconds * float64(time.Second))
	}
	if n, ok := raw["concurrency"].(float64); ok {
		opts.Concurrency = int(n)
	}
	return opts
}

// transcriptResult is the contract's result of a transcribe: {id, title, language, durationSeconds,
// utteranceCount, text, raw}. The model with toText arrives with the formatters, so text is absent.
func transcriptResult(t *hushscript.Transcript) (map[string]any, error) {
	var raw map[string]any
	if err := json.Unmarshal(t.Raw, &raw); err != nil {
		return nil, err
	}
	var body struct {
		Body struct {
			Original struct {
				Utterances []json.RawMessage `json:"utterances"`
			} `json:"original"`
		} `json:"body"`
	}
	if err := json.Unmarshal(t.Raw, &body); err != nil {
		return nil, err
	}
	return map[string]any{
		"id": t.ID, "title": t.Title, "language": t.Language, "durationSeconds": t.DurationSeconds,
		"utteranceCount": float64(len(body.Body.Original.Utterances)), "raw": raw,
	}, nil
}

func transcribe(ctx context.Context, e *env, args map[string]any) (map[string]any, error) {
	name, _ := args["file"].(string)
	path, err := e.media(ctx, name)
	if err != nil {
		return nil, err
	}
	got, err := e.client.Transcribe(ctx, path, transcribeOptions(args))
	if err != nil {
		return nil, err
	}
	return transcriptResult(got)
}

func resume(ctx context.Context, e *env, args map[string]any) (map[string]any, error) {
	name, _ := args["file"].(string)
	path, err := e.media(ctx, name)
	if err != nil {
		return nil, err
	}
	got, err := e.client.Resume(ctx, e.token, &hushscript.ResumeOptions{Path: path})
	if err != nil {
		return nil, err
	}
	return transcriptResult(got)
}

// refusedByItsBytes is the one file of durations.json the SDK must not sniff: a WebM renamed to .mp3,
// which the unsupported-media scenario requires it to refuse. Any other file that sniffs as unknown is
// a failure.
const refusedByItsBytes = "webm-renamed.mp3"

// probeDurations runs the SDK's duration probe over every file of durations.json: each must probe to
// its listed duration, except refusedByItsBytes, which must be refused by its bytes.
func probeDurations(ctx context.Context, e *env, args map[string]any) (map[string]any, error) {
	tolerance, _ := args["toleranceSeconds"].(float64)
	path, err := e.media(ctx, "media/durations.json")
	if err != nil {
		return nil, err
	}
	listing, err := os.ReadFile(path) //nolint:gosec // a file this run just downloaded
	if err != nil {
		return nil, err
	}
	var expected map[string]float64
	if err := json.Unmarshal(listing, &expected); err != nil {
		return nil, err
	}
	failures := []any{}
	for name, want := range expected {
		file, openErr := e.media(ctx, "media/"+name)
		if openErr != nil {
			return nil, openErr
		}
		got, probeErr := probeFile(file)
		switch {
		case errors.Is(probeErr, errUnsupported) && name == refusedByItsBytes:
		case probeErr != nil, got-want > tolerance, want-got > tolerance, math.Ceil(got) < want-1e-6:
			failures = append(failures, map[string]any{"file": name, "expected": want, "got": got})
		}
	}
	return map[string]any{"checked": float64(len(expected)), "failures": failures}, nil
}

var errUnsupported = errors.New("the format is not supported")

func probeFile(path string) (float64, error) {
	file, err := os.Open(path) //nolint:gosec // a file this run just downloaded
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	format := probe.SniffAt(file, info.Size())
	if format == probe.FormatUnknown {
		return 0, errUnsupported
	}
	return probe.Duration(file, info.Size(), format)
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
	files := &mediaCache{dir: t.TempDir(), mock: mock, paths: map[string]string{}}
	for _, name := range names {
		t.Run(name, func(t *testing.T) { runScenario(ctx, t, mock, files, name) })
	}
}

// mediaCache downloads fixtures from the mock once per run: the SDK reads its file from disk, and the
// SDK container shares the mock's network but not its filesystem.
type mediaCache struct {
	dir  string
	mock *mockClient

	mu    sync.Mutex
	paths map[string]string
}

func (m *mediaCache) fetch(ctx context.Context, name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if path, ok := m.paths[name]; ok {
		return path, nil
	}
	data, err := m.mock.file(ctx, "/__files/fixtures/"+name)
	if err != nil {
		return "", err
	}
	path := filepath.Join(m.dir, fmt.Sprintf("%d-%s", len(m.paths), filepath.Base(name)))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	m.paths[name] = path
	return path, nil
}

// executed is what one call produced, and whether the driver stalled doing it.
type executed struct {
	result map[string]any
	err    error
}

// execute runs one mapped call while the driver moves the clocks, and returns what it produced.
func execute(ctx context.Context, drive *driver, op operation, e *env, args map[string]any) (executed, error) {
	finished := make(chan executed, 1)
	done := make(chan struct{})
	go func() {
		result, err := op.run(ctx, e, args)
		finished <- executed{result, err}
		close(done)
	}()
	if err := drive.run(ctx, done); err != nil {
		return executed{}, err
	}
	return <-finished, nil
}

func runScenario(parent context.Context, t *testing.T, mock *mockClient, files *mediaCache, name string) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	scenario, err := mock.scenario(ctx, name)
	if err != nil {
		t.Fatalf("loading the scenario: %v", err)
	}
	mapped, known := operations[scenario.Call.Op]
	priorOp, priorKnown := operation{}, true
	if scenario.Call.Prior != nil {
		priorOp, priorKnown = operations[scenario.Call.Prior.Op]
	}
	if !known || !priorKnown || (mapped.errorsOnly && scenario.Expect.Error == nil) {
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
	e := &env{client: client, media: files.fetch}
	drive := &driver{mock: mock, clock: clock, transport: transport}

	if prior := scenario.Call.Prior; prior != nil {
		first, driveErr := execute(ctx, drive, priorOp, e, prior.Args)
		if driveErr != nil {
			t.Fatalf("prior call: %v", driveErr)
		}
		if first.err == nil || prior.ExpectError == nil {
			t.Fatalf("the prior call must fail with its expectError, got %v (expect %v)", first.err, prior.ExpectError)
		}
		for _, problem := range checkError(first.err, prior.ExpectError) {
			t.Errorf("prior call: %s", problem)
		}
		var failure *hushscript.Error
		if errors.As(first.err, &failure) {
			e.token = failure.ResumeToken
		}
	}
	if err = mock.mark(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := execute(ctx, drive, mapped, e, scenario.Call.Args)
	if err != nil {
		t.Fatal(err)
	}

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

// checkFields compares the class attributes a scenario names (spec 12: resumeToken, failureKind, ...).
func checkFields(sdkErr *hushscript.Error, want map[string]any) []string {
	var problems []string
	have := map[string]string{
		"resumeToken": sdkErr.ResumeToken, "idempotencyKey": sdkErr.IdempotencyKey,
		"failureKind": sdkErr.FailureKind, "error": sdkErr.JobError, "jobId": sdkErr.JobID,
	}
	for field, expected := range want {
		got, known := have[field]
		switch {
		case !known:
			problems = append(problems, fmt.Sprintf("field %q is not one the Go error has", field))
		case expected == "<present>" && got == "":
			problems = append(problems, fmt.Sprintf("field %q is empty, want it present", field))
		case expected != "<present>" && got != expected:
			problems = append(problems, fmt.Sprintf("field %q = %q, want %v", field, got, expected))
		}
	}
	return problems
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
	problems = append(problems, checkFields(sdkErr, want.Fields)...)
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
