package hushscript

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hushscript/hushscript-sdk-go/generated"
)

func TestResumeTokenFieldBoundaries(t *testing.T) {
	for _, body := range []string{
		`{"v":0,"jobId":"j1","idempotencyKey":"k","size":1,"durationSeconds":15}`,
		`{"v":2,"jobId":"j1","idempotencyKey":"k","size":1,"durationSeconds":15}`,
		`{"v":1,"jobId":"..","idempotencyKey":"k","size":1,"durationSeconds":15}`,
		`{"v":1,"jobId":"j1","idempotencyKey":"","size":1,"durationSeconds":15}`,
		`{"v":1,"jobId":"j1","idempotencyKey":"k","size":0,"durationSeconds":15}`,
		`{"v":1,"jobId":"j1","idempotencyKey":"k","size":-1,"durationSeconds":15}`,
		`{"v":1,"jobId":"j1","idempotencyKey":"k","size":1,"durationSeconds":14.999}`,
		`{"v":1,"jobId":"j1","idempotencyKey":"k","size":1,"durationSeconds":36000.001}`,
		`{"v":1,"jobId":"j1","idempotencyKey":"k","size":1,"durationSeconds":15,"size":"bad"}`,
	} {
		state, err := decodeResumeToken(base64.RawURLEncoding.EncodeToString([]byte(body)))
		if !errors.Is(err, errBadToken) || state != (resumeState{}) {
			t.Errorf("invalid token %s: state=%+v err=%v", body, state, err)
		}
	}
	for _, duration := range []float64{15, 15.001, 35999.001, 36000} {
		want := resumeState{JobID: "j1", IdempotencyKey: "k", Size: 1, DurationSeconds: duration}
		got, err := decodeResumeToken(encodeResumeToken(want))
		if err != nil || got != want {
			t.Errorf("duration %v: state=%+v err=%v", duration, got, err)
		}
	}
	for _, duration := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := encodeResumeToken(resumeState{DurationSeconds: duration}); got != "" {
			t.Errorf("nonfinite duration encoded as %q", got)
		}
	}
	// With a JSON length that is a multiple of 3 every base64 group is whole, so the bytes decoded before
	// the bad character are the complete JSON: only the base64 error can refuse the token.
	for _, key := range []string{"k", "kk", "kkk"} {
		state := resumeState{JobID: "j1", IdempotencyKey: key, Size: 1, DurationSeconds: 15}
		valid := encodeResumeToken(state)
		if got, err := decodeResumeToken(valid); err != nil || got != state {
			t.Fatalf("valid token %q: state=%+v err=%v", valid, got, err)
		}
		for _, suffix := range []string{"!", "*"} {
			if got, err := decodeResumeToken(valid + suffix); !errors.Is(err, errBadToken) || got != (resumeState{}) {
				t.Fatalf("invalid base64 suffix %q after a %d-byte JSON: state=%+v err=%v", suffix, base64.RawURLEncoding.DecodedLen(len(valid)), got, err)
			}
		}
	}
	api := newRouteServer(t, func(recorded, int) step {
		return step{status: http.StatusNotFound, body: jsonError("not_found", 4040)}
	})
	slashToken := encodeResumeToken(resumeState{JobID: "../j1", IdempotencyKey: "k", Size: 1, DurationSeconds: 15})
	_, err := api.client(t).Resume(t.Context(), slashToken, nil)
	if failure := asError(t, err); failure.Code != "resume_job_missing" {
		t.Fatalf("slash-containing job error=%v", failure)
	}
	requests := api.all()
	if len(requests) != 1 || requests[0].path != "/v1/jobs/..%2Fj1" {
		t.Fatalf("resume job was not escaped as one path segment: %v", requests)
	}
}

func TestResumeSourcePreservesReaderAndTokenSizes(t *testing.T) {
	reader := bytes.NewReader([]byte("original"))
	state := resumeState{Size: 8}
	for _, tc := range []struct {
		opts *ResumeOptions
		size int64
	}{
		{&ResumeOptions{Reader: reader, Size: 7}, 7},
		{&ResumeOptions{Reader: reader, Size: 1}, 1},
		{&ResumeOptions{Stream: reader}, 8},
	} {
		src, err := resumeSource(tc.opts, state, redactor{})
		if err != nil || src == nil || src.size != tc.size {
			t.Fatalf("source=%+v err=%v want size=%d", src, err, tc.size)
		}
		if size, known := knownSize(src, "ignored"); !known || size != tc.size {
			t.Fatalf("known size=%d known=%v", size, known)
		}
		if tc.opts.Reader != nil && src.r != reader {
			t.Fatal("Reader changed")
		}
		if tc.opts.Stream != nil && (src.stream == nil || src.stream.r != reader) {
			t.Fatal("Stream changed")
		}
	}
	for _, opts := range []*ResumeOptions{{}, {Path: "not-opened-yet"}} {
		if src, err := resumeSource(opts, state, redactor{}); src != nil || err != nil {
			t.Fatalf("deferred source=%+v err=%v", src, err)
		}
	}
	for _, path := range []string{"", t.TempDir(), filepath.Join(t.TempDir(), "missing.wav")} {
		if size, known := knownSize(nil, path); known || size != 0 {
			t.Fatalf("path %q: size=%d known=%v, want 0 and unknown", path, size, known)
		}
	}
}

func TestRetryRoutesRejectNearMatches(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         retryClass
	}{
		{http.MethodOptions, "/v1/account", classSafe},
		{http.MethodPost, "/v2/uploads", classMutate},
		{http.MethodPost, "/v1/uploads/extra", classMutate},
		{http.MethodPost, "/v1/other/j1/heartbeat", classMutate},
		{http.MethodPost, "/v1/uploads/j1/other", classMutate},
		{http.MethodPost, "/v1/billing/other", classMutate},
		{http.MethodPost, "/v1/agent/other/sg1/claim", classMutate},
		{http.MethodPost, "/v1/agent/accounts/sg1/other", classMutate},
		{http.MethodPost, "/v1/agent/credentials/other", classMutate},
		{http.MethodPut, "/v1/uploads/j1/other/1", classMutate},
		{http.MethodPut, "/v1/other/j1/parts/1", classMutate},
		{http.MethodPut, "/v1/uploads/j1/parts/1/extra", classMutate},
		{http.MethodPost, "/v1/agent/accounts/sg%2F1/claim", classNever},
	} {
		if got := classify(tc.method, tc.path); got != tc.want {
			t.Errorf("%s %s: got %v want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestRetryPolicyStatusAndBudgetBoundaries(t *testing.T) {
	for _, class := range []retryClass{classSafe, classUpload, classUploadCreate, classPurchase, classNever, classMutate} {
		for _, status := range []int{200, 399, 400, 428, 429, 430, 499, 500, 501, 502, 503, 504, 505, 599} {
			want := false
			switch class {
			case classSafe:
				want = status == 429 || status == 500 || status == 502 || status == 503 || status == 504
			case classUpload, classUploadCreate:
				want = status == 429 || status >= 500
			case classPurchase:
				want = status == 502 || status == 503
			case classNever, classMutate:
			}
			if got := retryableStatus(class, status); got != want {
				t.Errorf("class %v status %d: %v want %v", class, status, got, want)
			}
		}
		for _, budget := range []int{0, 1, 2, 5} {
			want := budget
			if class == classNever {
				want = 0
			} else if class == classPurchase && budget > 1 {
				want = 1
			}
			if got := retryBudget(class, budget); got != want {
				t.Errorf("class %v budget %d: %d want %d", class, budget, got, want)
			}
		}
		for _, sent := range []bool{false, true} {
			want := class != classNever && (class != classMutate || !sent)
			if got := retryableNetworkError(class, sent); got != want {
				t.Errorf("class %v sent %v: %v want %v", class, sent, got, want)
			}
		}
	}
}

func TestPacingExpiresAtTheWindowEdge(t *testing.T) {
	clock := newFakeClock()
	p := newPacer(clock)
	for range 4 {
		if got := p.reserve(); got != 0 {
			t.Fatalf("initial reservation waited %v", got)
		}
	}
	if got := p.reserve(); got != time.Minute {
		t.Fatalf("full window waited %v", got)
	}
	if err := clock.Sleep(t.Context(), time.Minute-time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	if got := p.reserve(); got != time.Nanosecond {
		t.Fatalf("reservation before expiry waited %v", got)
	}
	if err := clock.Sleep(t.Context(), time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if got := p.reserve(); got != 0 {
			t.Fatalf("expired reservation waited %v", got)
		}
	}
	if got := p.reserve(); got != time.Minute {
		t.Fatalf("new full window waited %v", got)
	}
}

func TestHeartbeatSelectsTheLaterTime(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	for _, offset := range []time.Duration{-time.Second, 0, time.Second, 5 * time.Minute} {
		other := start.Add(offset)
		want := start
		if offset > 0 {
			want = other
		}
		if got := laterOf(start, other); !got.Equal(want) {
			t.Errorf("offset %v: got %v want %v", offset, got, want)
		}
	}
}

func TestUploadPartRequestsReplayExactBytes(t *testing.T) {
	data := []byte{0, 1, 2, 255}
	hold := &bodyHold{}
	req, err := partRequest(t.Context(), "https://storage.example.test/part?signature=test", data, hold)
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPut || req.ContentLength != 4 || req.Body != http.NoBody {
		t.Fatalf("request method=%s length=%d body=%v", req.Method, req.ContentLength, req.Body)
	}
	for range 2 {
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			t.Fatal(bodyErr)
		}
		got, readErr := io.ReadAll(body)
		if readErr != nil || !bytes.Equal(got, data) || hold.open != 1 {
			t.Fatalf("replay=%v err=%v open=%d", got, readErr, hold.open)
		}
		if err := body.Close(); err != nil {
			t.Fatal(err)
		}
		if err := body.Close(); err != nil {
			t.Fatal(err)
		}
		if hold.open != 0 || !hold.wait(t.Context()) {
			t.Fatal("closing a replay must release its hold exactly once")
		}
	}
	if _, err := partRequest(t.Context(), ":invalid", data, hold); err == nil {
		t.Fatal("invalid target accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	hold.acquire()
	cancel()
	if hold.wait(ctx) {
		t.Fatal("outstanding body ignored cancellation")
	}
	hold.release()
}

// A waiter on a hold with two bodies open wakes on the first close, finds one still open and waits again.
func TestABodyHoldWaitsForEveryOutstandingBody(t *testing.T) {
	hold := &bodyHold{}
	hold.acquire()
	hold.acquire()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- hold.wait(ctx) }()
	for waiting := false; !waiting; {
		hold.mu.Lock()
		waiting = hold.changed != nil
		hold.mu.Unlock()
		if ctx.Err() != nil {
			t.Fatal("the waiter never started waiting")
		}
		time.Sleep(time.Millisecond)
	}
	hold.release()
	select {
	case got := <-done:
		t.Fatalf("wait returned %v with one body still open", got)
	case <-time.After(50 * time.Millisecond):
	}
	hold.release()
	select {
	case got := <-done:
		if !got {
			t.Fatal("wait gave up although every body was closed")
		}
	case <-ctx.Done():
		t.Fatal("wait did not return after the last body was closed")
	}
	hold.mu.Lock()
	defer hold.mu.Unlock()
	if hold.open != 0 || hold.changed != nil {
		t.Fatalf("open %d, changed %v after every body was closed", hold.open, hold.changed)
	}
}

// The part count is the file size over the part size, rounded up: a plan must say exactly that.
func TestThePartCountIsTheSizeOverThePartSizeRoundedUp(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:4010", newFakeClock())
	for _, tc := range []struct {
		size  int64
		parts int
	}{{1, 1}, {3, 1}, {4, 2}, {8, 3}, {9, 3}, {10, 4}, {11, 4}} {
		r := &run{state: resumeState{JobID: "j1"}, src: &fileSource{size: tc.size}, concurrency: 1}
		if u, err := c.core.newUploader(r, &uploadPlan{partSize: 3, totalParts: tc.parts}); err != nil || u.total != tc.parts {
			t.Errorf("size %d: %v, want a plan of %d parts accepted", tc.size, err, tc.parts)
		}
		_, err := c.core.newUploader(r, &uploadPlan{partSize: 3, totalParts: tc.parts + 1})
		want := fmt.Sprintf("The API planned %d parts; the file needs %d.", tc.parts+1, tc.parts)
		if e := wantCode(t, err, KindUpload, "bad_part_plan"); e.Message != want {
			t.Errorf("size %d: message %q, want %q", tc.size, e.Message, want)
		}
	}
}

// TestMutationContractUploadPipeline runs the upload scenarios in a child, each under a deadline, so a
// panic on an SDK worker goroutine, a lock misuse and a hang all end as a named failure here.
func TestMutationContractUploadPipeline(t *testing.T) {
	if contractSubprocess(t, uploadLockFrames) {
		return
	}
	for _, scenario := range []func(*testing.T){
		TestUploadPartRequestsReplayExactBytes,
		TestABodyHoldWaitsForEveryOutstandingBody,
		TestUploadPlanAndPageBoundaries,
		TestADirectUploadSendsEveryPartWithoutTheTokenAndReturnsTheTranscript,
		TestAProxiedJobSendsPartsThroughTheAPIAndCompletesWithoutABody,
		TestWhenDirectNeverWorksEveryPartGoesThroughTheAPIOnce,
		TestOnceADirectPartSucceededAFailingPartKeepsRetryingDirect,
		TestAPartThatKeepsFailingAfterAFreshPageIsPartsExhaustedWithItsCauseRedacted,
		TestAnExpiredPartURLIsRefreshedAfterAForbidden,
		TestPartURLsAreFetchedPageByPageFromTheNextPartNumber,
		TestPartsWaitingForOnePageShareOneFetch,
		TestANullPartURLsPageMeansTheJobTakesProxiedPartsOnly,
		TestA404PartURLsPageMeansTheJobTakesProxiedPartsOnly,
		TestA404NextPageAfterADirectSuccessIsPartURLMissing,
		TestA404RefreshAfterADirectSuccessIsPartURLMissing,
		TestA404ProxiedPartKeepsItsMeaning,
		TestAFallbackDecidedBeforeAConcurrentDirectSuccessNeverTurnsTheJobProxied,
		TestADirectSuccessThatLosesTheSwapToAFallbackIsSentThroughTheAPIAgain,
		TestAFailedRefreshAfterADirectSuccess,
		TestAFailedRefreshAfterALostSwap,
		TestAFailedProxiedPartEndsTheUploadWithItsAnswer,
		TestTheFirstFailedPartCancelsThePartsStillInFlight,
		TestAPanicInOnProgressOnAnUploadWorkerReachesTheCallerWithItsValue,
	} {
		boundedScenario(t, 3*time.Second, scenario)
	}
}

func TestUploadPlanAndPageBoundaries(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:4010", newFakeClock())
	for _, tc := range []struct {
		size, total int
		bad         bool
	}{
		{0, 1, true},
		{-1, 1, true},
		{1, 0, true},
		{1, -1, true},
		{1, 1, false},
		{32 << 20, 1, false},
		{(32 << 20) + 1, 1, true},
	} {
		plan, failure := c.core.planOf(&createdUpload{UploadCreateResponse: &generated.UploadCreateResponse{
			Upload: &struct {
				NextPartNumber *int                         `json:"next_part_number"`
				PartSizeBytes  int                          `json:"part_size_bytes"`
				PartUrls       []generated.PresignedPartUrl `json:"part_urls"`
				TotalParts     int                          `json:"total_parts"`
			}{PartSizeBytes: tc.size, TotalParts: tc.total},
		}}, http.StatusCreated)
		if (failure != nil) != tc.bad {
			t.Fatalf("size=%d total=%d plan=%+v error=%v", tc.size, tc.total, plan, failure)
		}
		if tc.bad && (failure.Code != "bad_response" || failure.Status != http.StatusCreated) {
			t.Fatalf("invalid plan error=%v", failure)
		}
		if !tc.bad && (plan.partSize != int64(tc.size) || plan.totalParts != tc.total) {
			t.Fatalf("plan=%+v", plan)
		}
	}
	p := newPartPages(c.core, "j1", &uploadPlan{next: 17})
	if start := p.store([]partURL{{n: 2, url: newSecret("two")}, {n: 1, url: newSecret("one")}}); start != 1 {
		t.Fatalf("page starts at %d", start)
	}
	if p.pageOf(3) != nil || p.pageOf(1) == nil || p.pageOf(2) == nil {
		t.Fatal("page lookup did not match exact part numbers")
	}
	p.store([]partURL{{n: 1, url: newSecret("replacement")}})
	if len(p.pages) != 1 || p.pageOf(2) != nil || p.pageOf(1).urls[1].reveal() != "replacement" {
		t.Fatal("replacement page retained stale URLs")
	}
	if err := p.refresh(t.Context(), 1, "one"); err != nil {
		t.Fatalf("refreshing an already replaced URL: %v", err)
	}
	if err := p.refresh(t.Context(), 3, "absent"); err != nil {
		t.Fatalf("refreshing an absent URL: %v", err)
	}
	if !p.pages[0].fetchedAt.Equal(c.core.cfg.clock.Now()) {
		t.Fatal("page timestamp differs from the clock")
	}
}

func TestUploadFallbackCannotChangeASettledMode(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:4010", newFakeClock())
	for _, initial := range []uint32{modeUndecided, modeDirect, modeProxied} {
		u := &uploader{core: c.core}
		u.mode.Store(initial)
		got := u.fallBack(1)
		want := initial != modeDirect
		if got != want || u.isProxied() != want || u.isDirect() == want {
			t.Fatalf("fallback from %d: got=%v mode=%d", initial, got, u.mode.Load())
		}
		if u.settleDirect() != !want || u.isProxied() != want {
			t.Fatalf("direct success changed fallback decision from %d", initial)
		}
		u.mode.Store(initial)
		got = u.settleDirect()
		want = initial != modeProxied
		if got != want || u.isDirect() != want || u.isProxied() == want {
			t.Fatalf("direct success from %d: got=%v mode=%d", initial, got, u.mode.Load())
		}
		if u.fallBack(1) != !want || u.isDirect() != want {
			t.Fatalf("fallback changed direct decision from %d", initial)
		}
	}
}

func TestUploadTerminalStatusBoundaries(t *testing.T) {
	for _, status := range []int{200, 399, 400, 401, 403, 428, 429, 430, 499, 500, 503} {
		want := status >= 400 && status < 500 && status != 429
		if got := isTerminal(&Error{Status: status}); got != want {
			t.Errorf("status %d: terminal=%v want=%v", status, got, want)
		}
	}
}

func TestUploadNullURLsAndRowValidation(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"part_urls":null}`, true},
		{`{"part_urls": null }`, true},
		{`{"part_urls":[]}`, false},
		{`{}`, false},
		{`null`, false},
		{`{"part_urls":"null"}`, false},
		{`{"part_urls":`, false},
	} {
		if got := nullPartURLs([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: null=%v want=%v", tc.body, got, tc.want)
		}
		if got := createdWithoutPartURLs([]byte(`{"upload":` + tc.body + `}`)); got != tc.want {
			t.Errorf("created %s: null=%v want=%v", tc.body, got, tc.want)
		}
	}
	for _, tc := range []struct {
		rows []generated.PresignedPartUrl
		want bool
	}{
		{nil, true},
		{[]generated.PresignedPartUrl{{N: 1, Url: "one"}, {N: 2, Url: "two"}}, true},
		{[]generated.PresignedPartUrl{{N: 0, Url: "one"}}, false},
		{[]generated.PresignedPartUrl{{N: -1, Url: "one"}}, false},
		{[]generated.PresignedPartUrl{{N: 1, Url: ""}}, false},
		{[]generated.PresignedPartUrl{{N: 1, Url: "one"}, {N: 2, Url: ""}}, false},
	} {
		if got := partRowsUsable(tc.rows); got != tc.want {
			t.Errorf("rows=%v: usable=%v want=%v", tc.rows, got, tc.want)
		}
		urls := partURLsOf(tc.rows)
		if len(urls) != len(tc.rows) {
			t.Fatalf("converted %d rows to %d URLs", len(tc.rows), len(urls))
		}
		for i, row := range tc.rows {
			if urls[i].n != row.N || urls[i].url.reveal() != row.Url {
				t.Errorf("URL %d changed its number or value", i)
			}
		}
	}
}

func TestMutationContractPacerAcquire(t *testing.T) {
	if contractSubprocess(t, pacerLockFrames) {
		return
	}
	for _, name := range []string{"available", "full", "sleep error"} {
		t.Run(name, func(t *testing.T) {
			sentinel := errors.New("scripted pacing cancellation")
			clock := &contractClock{now: time.Unix(1_000_000, 0)}
			p := newPacer(clock)
			var want error
			if name != "available" {
				for range 4 {
					p.starts = append(p.starts, clock.now)
				}
				clock.now = clock.now.Add(17 * time.Second)
				clock.steps = []clockStep{{duration: 43 * time.Second, advance: 43 * time.Second}}
				if name == "sleep error" {
					clock.steps[0].advance = 0
					clock.steps[0].err = sentinel
					want = sentinel
				}
			}
			assertClockContract(t, clock, func() {
				if got := p.acquire(t.Context()); !errors.Is(got, want) {
					t.Fatalf("acquire error = %v, want matching %v", got, want)
				}
			})
			wantStarts := 1
			if name == "sleep error" {
				wantStarts = 4
			}
			if len(p.starts) != wantStarts {
				t.Fatalf("reservations = %d, want %d", len(p.starts), wantStarts)
			}
		})
	}
}

type heartbeatContractDoer struct {
	requests       int
	errors         []error
	clock          *contractClock
	requestAdvance time.Duration
}

func (d *heartbeatContractDoer) Do(req *http.Request) (*http.Response, error) {
	i := d.requests
	d.requests++
	if req.Method != http.MethodPost || req.URL.Path != "/v1/uploads/job_1/heartbeat" {
		panic(sleepContractViolation{fmt.Sprintf("unexpected heartbeat request %s %s", req.Method, req.URL.Path)})
	}
	if i >= len(d.errors) {
		panic(sleepContractViolation{"heartbeat request after terminal outcome"})
	}
	d.clock.now = d.clock.now.Add(d.requestAdvance)
	if err := d.errors[i]; err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
}

func TestMutationContractHeartbeatRun(t *testing.T) {
	if contractSubprocess(t, heartbeatLockFrames) {
		return
	}
	wrong := &Error{Kind: KindConflict, Code: "wrong_state"}
	auth := &Error{Kind: KindAuthentication, Code: "unauthorized"}
	transient := &Error{Kind: KindNetwork, Code: "heartbeat_failure"}
	for _, tc := range []struct {
		name            string
		steps           []clockStep
		errors          []error
		requestAdvance  time.Duration
		canceledContext bool
		failed          *Error
		cancels         int
	}{
		{name: "sleep cancellation", steps: []clockStep{{duration: 120 * time.Second, err: context.Canceled}}},
		{name: "success cadence", steps: []clockStep{{duration: 120 * time.Second, advance: 120 * time.Second}, {duration: 120 * time.Second, err: context.Canceled}}, errors: []error{nil}},
		{name: "transient cadence", steps: []clockStep{{duration: 120 * time.Second, advance: 120 * time.Second}, {duration: 120 * time.Second, err: context.Canceled}}, errors: []error{transient}},
		{name: "network cadence", steps: []clockStep{{duration: 120 * time.Second, advance: 120 * time.Second}, {duration: 120 * time.Second, err: context.Canceled}}, errors: []error{errors.New("network sentinel")}},
		{name: "wrong state", steps: []clockStep{{duration: 120 * time.Second, advance: 120 * time.Second}}, errors: []error{wrong}},
		{name: "authentication", steps: []clockStep{{duration: 120 * time.Second, advance: 120 * time.Second}}, errors: []error{auth}, failed: auth, cancels: 1},
		{name: "late wakeup", steps: []clockStep{{duration: 120 * time.Second, advance: 301 * time.Second}, {duration: 120 * time.Second, err: context.Canceled}}, errors: []error{nil}},
		{name: "slow request", steps: []clockStep{{duration: 120 * time.Second, advance: 120 * time.Second}, {duration: 113 * time.Second, err: context.Canceled}}, errors: []error{nil}, requestAdvance: 7 * time.Second},
		{name: "request as long as the interval", steps: []clockStep{{duration: 120 * time.Second, advance: 120 * time.Second}, {duration: 0, err: context.Canceled}}, errors: []error{nil}, requestAdvance: 120 * time.Second},
		{name: "request longer than the interval", steps: []clockStep{{duration: 120 * time.Second, advance: 120 * time.Second}, {duration: 0, err: context.Canceled}}, errors: []error{nil}, requestAdvance: 125 * time.Second},
		{name: "canceled request", steps: []clockStep{{duration: 120 * time.Second, advance: 120 * time.Second}, {duration: 120 * time.Second, err: context.Canceled}}, errors: []error{auth}, canceledContext: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &contractClock{now: time.Unix(1_000_000, 0), steps: tc.steps}
			doer := &heartbeatContractDoer{errors: tc.errors, clock: clock, requestAdvance: tc.requestAdvance}
			api, err := generated.NewClientWithResponses("https://example.test", generated.WithHTTPClient(doer))
			if err != nil {
				t.Fatal(err)
			}
			c := &core{cfg: config{clock: clock}, api: api}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.canceledContext {
				cancel()
			}
			h := &heartbeat{}
			cancels := 0
			assertClockContract(t, clock, func() {
				assertSDKDoesNotPanic(t, func() { h.run(ctx, func() { cancels++; cancel() }, c, "job_1") })
			})
			if doer.requests != len(tc.errors) || h.failed != tc.failed || cancels != tc.cancels {
				t.Fatalf("requests=%d failed=%v cancels=%d; want requests=%d identical failure=%v cancels=%d", doer.requests, h.failed, cancels, len(tc.errors), tc.failed, tc.cancels)
			}
		})
	}
}

func TestMutationContractHeartbeatLifecycle(t *testing.T) {
	if contractSubprocess(t, heartbeatLockFrames) {
		return
	}
	clock := newVirtualClock()
	api := newFakeAPI(t, clock)
	client := newTestClient(t, api.URL, clock)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	h := client.core.startHeartbeat(ctx, cancel, "job_1")
	waitFor(t, func() bool {
		select {
		case <-h.done:
			t.Fatal("heartbeat finished before cancellation or its first sleep")
		default:
		}
		return clock.sleepers() == 1
	})
	at, ok := clock.earliest()
	if !ok || !at.Equal(clock.Now().Add(120*time.Second)) {
		t.Fatalf("started heartbeat timer=%v, want exactly 120 seconds", at)
	}
	cancel()
	select {
	case <-h.done:
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not close done after cancellation")
	}
	if got := h.stop(); got != nil {
		t.Fatalf("canceled heartbeat stop=%v, want nil", got)
	}
	if api.count("heartbeat") != 0 || clock.sleepers() != 0 {
		t.Fatalf("canceled first sleep: requests=%d timers=%d", api.count("heartbeat"), clock.sleepers())
	}
}

// blockingHeartbeatDoer holds every heartbeat until release is closed, whatever its context says.
type blockingHeartbeatDoer struct {
	entered chan struct{}
	release chan struct{}
}

func (d *blockingHeartbeatDoer) Do(*http.Request) (*http.Response, error) {
	d.entered <- struct{}{}
	<-d.release
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
}

func TestHeartbeatStopWaitsForTheRequestInFlight(t *testing.T) {
	clock := newVirtualClock()
	doer := &blockingHeartbeatDoer{entered: make(chan struct{}, 1), release: make(chan struct{})}
	api, err := generated.NewClientWithResponses("https://example.test", generated.WithHTTPClient(doer))
	if err != nil {
		t.Fatal(err)
	}
	c := &core{cfg: config{clock: clock}, api: api}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h := c.startHeartbeat(ctx, cancel, "job_1")
	waitFor(t, func() bool { return clock.sleepers() == 1 })
	clock.advance(heartbeatInterval)
	select {
	case <-doer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first heartbeat was not sent")
	}
	cancel()
	stopped := make(chan *Error, 1)
	go func() { stopped <- h.stop() }()
	select {
	case got := <-stopped:
		t.Fatalf("stop returned %v while a heartbeat was still in flight", got)
	case <-time.After(100 * time.Millisecond):
	}
	close(doer.release)
	select {
	case got := <-stopped:
		if got != nil {
			t.Errorf("stop = %v, want nil", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return after the heartbeat ended")
	}
	select {
	case <-h.done:
	default:
		t.Error("stop returned before the loop ended")
	}
}
