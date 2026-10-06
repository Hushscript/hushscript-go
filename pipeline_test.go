package hushscript

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// run is the common setup of a pipeline test: a fake API, a client on a clock that moves by itself,
// and a 20 s WAV file split into 4 parts of 160000 bytes.
type rig struct {
	api    *fakeAPI
	clock  *virtualClock
	client *Client
	wav    []byte
}

// testPartSize cuts the 640044-byte test file into 4 parts.
const testPartSize = 161_000

func newRig(t *testing.T) *rig {
	t.Helper()
	clock := newVirtualClock()
	api := newFakeAPI(t, clock)
	api.partSize = testPartSize
	clock.run(t, api)
	return &rig{api: api, clock: clock, client: pipelineClient(t, api, clock), wav: testWAV(20)}
}

func (r *rig) transcribe(opts *TranscribeOptions) (*Transcript, error) {
	return r.client.TranscribeReader(context.Background(), bytes.NewReader(r.wav), int64(len(r.wav)), opts)
}

func mustTranscribe(t *testing.T, r *rig, opts *TranscribeOptions) *Transcript {
	t.Helper()
	got, err := r.transcribe(opts)
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	return got
}

func wantCode(t *testing.T, err error, kind ErrorKind, code string) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %s %s", kind, code)
	}
	e := asError(t, err)
	if e.Kind != kind || e.Code != code {
		t.Fatalf("error %v is %s %q, want %s %q", err, e.Kind, e.Code, kind, code)
	}
	return e
}

func TestADirectUploadSendsEveryPartWithoutTheTokenAndReturnsTheTranscript(t *testing.T) {
	r := newRig(t)
	var tokens []string
	var reports []Progress
	got := mustTranscribe(t, r, &TranscribeOptions{
		OnResumeToken: func(token string) { tokens = append(tokens, token) },
		OnProgress:    func(p Progress) { reports = append(reports, p) },
	})
	if got.ID != "tr_1" || got.Title != "T" || got.Language != "en" || got.DurationSeconds != 20 {
		t.Errorf("transcript = %+v", got)
	}
	if !strings.Contains(string(got.Raw), `"x_unknown_field":{"nested":true}`) {
		t.Errorf("Raw lost an unknown field: %s", got.Raw)
	}
	for route, want := range map[string]int{"account": 1, "create": 1, "r2": 4, "part": 0, "complete": 1} {
		if n := r.api.count(route); n != want {
			t.Errorf("%s requests = %d, want %d", route, n, want)
		}
	}
	for _, s := range r.api.requests("r2") {
		if s.authorization {
			t.Errorf("part %d carried Authorization", s.n)
		}
	}
	if len(r.api.r2Failures) > 0 {
		t.Errorf("R2 saw: %v", r.api.r2Failures)
	}
	if want := partHashes(r.wav, testPartSize); len(r.api.parts) != len(want) {
		t.Fatalf("stored %d parts, want %d", len(r.api.parts), len(want))
	} else {
		for n, hash := range want {
			if r.api.parts[n].hash != hash {
				t.Errorf("part %d holds other bytes than the file's", n)
			}
		}
	}
	complete := r.api.requests("complete")[0]
	var body struct {
		Parts []struct {
			N    int    `json:"n"`
			Etag string `json:"etag"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(complete.body, &body); err != nil || len(body.Parts) != 4 || body.Parts[0].N != 1 || body.Parts[3].Etag != "etag-4" {
		t.Errorf("complete body = %s (%v)", complete.body, err)
	}
	if len(tokens) != 1 || tokens[0] == "" {
		t.Errorf("OnResumeToken calls = %v", tokens)
	}
	if len(reports) < 6 || reports[0] != (Progress{Stage: StageUploading, BytesTotal: int64(len(r.wav))}) {
		t.Fatalf("reports = %+v", reports)
	}
	last := -1
	sawProcessing := false
	for _, p := range reports {
		switch p.Stage {
		case StageUploading:
			if p.Percent < last || p.BytesSent > p.BytesTotal {
				t.Errorf("progress went backwards or past the end: %+v", p)
			}
			last = p.Percent
		case StageProcessing:
			sawProcessing = p.State == "processing" || p.State == "queued"
		}
	}
	if last != 100 || !sawProcessing {
		t.Errorf("final upload percent %d, processing reported %v", last, sawProcessing)
	}
}

func TestAFileOnDiskIsUploadedByPath(t *testing.T) {
	r := newRig(t)
	path := filepath.Join(t.TempDir(), "meeting.dat") // the extension is not what decides the format
	if err := os.WriteFile(path, r.wav, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := r.client.Transcribe(context.Background(), path, nil)
	if err != nil || got.ID != "tr_1" {
		t.Fatalf("Transcribe = %v, %v", got, err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	_, err = r.client.Transcribe(context.Background(), missing, nil)
	e := wantCode(t, err, KindPreflight, "file_unreadable")
	if !errors.Is(err, fs.ErrNotExist) || !e.Kind.IsPreflight() {
		t.Errorf("a missing file should be a PreflightError with the OS error as its cause: %v", err)
	}
	before := r.api.total()
	_, err = r.client.Transcribe(context.Background(), t.TempDir(), nil)
	e = wantCode(t, err, KindPreflight, "file_unreadable")
	if e.IdempotencyKey != "" || r.api.total() != before {
		t.Errorf("a directory reached the API: %+v", e)
	}
}

func TestAProxiedJobSendsPartsThroughTheAPIAndCompletesWithoutABody(t *testing.T) {
	r := newRig(t)
	r.api.proxiedOnly = true
	mustTranscribe(t, r, nil)
	if r.api.count("r2") != 0 || r.api.count("part") != 1 {
		t.Fatalf("r2 %d, proxied %d", r.api.count("r2"), r.api.count("part"))
	}
	part := r.api.requests("part")[0]
	if !part.authorization || part.contentType != "application/octet-stream" || part.contentLength != int64(len(r.wav)) {
		t.Errorf("proxied part = %+v", part)
	}
	complete := r.api.requests("complete")[0]
	if len(complete.body) != 0 || complete.contentType != "" {
		t.Errorf("complete sent a body %q (%s)", complete.body, complete.contentType)
	}
}

func TestAPlanThatDoesNotMatchTheFileIsRefused(t *testing.T) {
	r := newRig(t)
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "create" {
			return 200, `{"job_id":"job_1","state":"uploading","balance_seconds":1,"held_seconds":1,"funding":"personal","limits":{"active_jobs":1,"max_active_jobs":50,"max_daily_seconds":1,"used_today_seconds":0},"upload":{"part_size_bytes":160000,"total_parts":9,"part_urls":[],"next_part_number":1}}`, true
		}
		return 0, "", false
	})
	_, err := r.transcribe(nil)
	e := wantCode(t, err, KindUpload, "bad_part_plan")
	if e.IdempotencyKey == "" || e.ResumeToken == "" {
		t.Errorf("error after create lacks key or token: %+v", e)
	}
}

func TestWhenDirectNeverWorksEveryPartGoesThroughTheAPIOnce(t *testing.T) {
	r := newRig(t)
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "r2" {
			return 500, `oops`, true
		}
		return 0, "", false
	})
	mustTranscribe(t, r, nil)
	if n := r.api.count("part"); n != 4 {
		t.Errorf("proxied parts = %d, want 4", n)
	}
	if n := r.api.count("r2"); n < 1 {
		t.Errorf("direct attempts = %d, want at least 1", n)
	}
	if n := r.api.count("complete"); n != 1 {
		t.Errorf("complete = %d", n)
	}
	if got := r.api.requests("complete")[0].body; len(got) != 0 {
		t.Errorf("a proxied job completed with a body: %s", got)
	}
}

func TestOnceADirectPartSucceededAFailingPartKeepsRetryingDirect(t *testing.T) {
	r := newRig(t)
	var failures atomic.Int32
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "r2" && s.n == 4 && r.api.count("r2") > 3 && failures.Add(1) <= 5 {
			return 503, `busy`, true
		}
		return 0, "", false
	})
	mustTranscribe(t, r, &TranscribeOptions{Concurrency: 1})
	if r.api.count("part") != 0 {
		t.Errorf("fell back to proxied parts after a direct success")
	}
	// 3 good parts + 5 failures + 1 success on part 4: the doer's 4 attempts, a fresh page, 2 more.
	if n := r.api.count("r2"); n != 9 {
		t.Errorf("direct PUTs = %d, want 9", n)
	}
	if n := r.api.count("partUrls"); n != 1 {
		t.Errorf("part-urls fetches = %d, want 1 (the refresh)", n)
	}
}

func TestAPartThatKeepsFailingAfterAFreshPageIsPartsExhaustedWithItsCauseRedacted(t *testing.T) {
	r := newRig(t)
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "r2" && s.n == 4 {
			return 503, `busy`, true
		}
		return 0, "", false
	})
	_, err := r.transcribe(&TranscribeOptions{Concurrency: 1})
	e := wantCode(t, err, KindUpload, "parts_exhausted")
	if e.IdempotencyKey == "" || e.ResumeToken == "" {
		t.Errorf("no key or token on %+v", e)
	}
	if n := r.api.count("partUrls"); n != 1 {
		t.Errorf("part-urls fetches = %d, want 1", n)
	}
	if n := len(r.api.requests("r2")); n != 3+4+4 {
		t.Errorf("direct PUTs = %d, want 11 (3 good, then 4 attempts before and 4 after the fresh page)", n)
	}
	var cause *Error
	if !errors.As(errors.Unwrap(e), &cause) || cause.Status != 503 {
		t.Errorf("the last failure is not the cause: %v", errors.Unwrap(e))
	}
	assertNoSignature(t, err)
}

// assertNoSignature fails when a presigned query string can be read from any surface of the error.
func assertNoSignature(t *testing.T, err error) {
	t.Helper()
	surfaces := []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err), fmt.Sprintf("%v", err)}
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		surfaces = append(surfaces, cause.Error())
	}
	for _, text := range surfaces {
		if strings.Contains(text, "X-Amz-Signature") || strings.Contains(text, "sig4") || signedURL.MatchString(text) {
			t.Errorf("a signed URL leaks through %q", text)
		}
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && strings.ContainsAny(urlErr.URL, "?#") {
		t.Errorf("errors.As hands out a *url.Error with the query: %s", urlErr.URL)
	}
}

var signedURL = regexp.MustCompile(`https?://[^\s"]+\?[^…\s"]`)

func TestAnExpiredPartURLIsRefreshedAfterAForbidden(t *testing.T) {
	r := newRig(t)
	var expired atomic.Bool
	expired.Store(true)
	r.api.setHook(func(s *seen) (int, string, bool) {
		switch s.route {
		case "r2":
			if expired.Load() {
				return 403, `<Error><Code>AccessDenied</Code></Error>`, true
			}
		case "partUrls":
			expired.Store(false)
		}
		return 0, "", false
	})
	mustTranscribe(t, r, nil)
	if r.api.count("partUrls") < 1 || r.api.count("part") != 0 {
		t.Errorf("part-urls %d, proxied %d", r.api.count("partUrls"), r.api.count("part"))
	}
}

func TestPartURLsAreFetchedPageByPageFromTheNextPartNumber(t *testing.T) {
	clock := newVirtualClock()
	api := newFakeAPI(t, clock)
	api.partSize = 10_000
	clock.run(t, api)
	client := pipelineClient(t, api, clock)
	wav := append(wavHeader(390_000), make([]byte, 390_000)...) // 40 parts of 10000 bytes, the last 44 bytes short
	// 12 s of audio: the override keeps the preflight at 15 s or more
	_, err := client.TranscribeReader(context.Background(), bytes.NewReader(wav), int64(len(wav)), &TranscribeOptions{DurationSeconds: 20})
	if err != nil {
		t.Fatal(err)
	}
	pages := api.requests("partUrls")
	var starts []string
	for _, p := range pages {
		starts = append(starts, p.query)
	}
	if got := fmt.Sprint(starts); got != "[start=17 start=33]" {
		t.Errorf("pages fetched = %s, want [start=17 start=33]", got)
	}
	if n := api.count("r2"); n != api.totalParts(int64(len(wav))) {
		t.Errorf("direct PUTs = %d", n)
	}
}

func TestACreateRateLimitedWithoutRetryAfterWaitsAndKeepsTheKey(t *testing.T) {
	r := newRig(t)
	var first atomic.Bool
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "create" && first.CompareAndSwap(false, true) {
			return 429, `{"error":"rate_limited","code":5000}`, true
		}
		return 0, "", false
	})
	mustTranscribe(t, r, nil)
	creates := r.api.requests("create")
	if len(creates) != 2 || !bytes.Equal(keyOf(creates[0].body), keyOf(creates[1].body)) || len(keyOf(creates[0].body)) != 36 {
		t.Fatalf("creates = %d, keys %q %q", len(creates), keyOf(creates[0].body), keyOf(creates[1].body))
	}
	if gap := creates[1].at.Sub(creates[0].at); gap < 15*time.Second {
		t.Errorf("second create after %v, want at least 15 s", gap)
	}
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func keyOf(body []byte) []byte {
	var payload struct {
		Key string `json:"idempotency_key"`
	}
	_ = json.Unmarshal(body, &payload)
	return []byte(payload.Key)
}

func TestTheKeyIsAUUIDv4MintedOncePerCallUnlessGiven(t *testing.T) {
	r := newRig(t)
	mustTranscribe(t, r, nil)
	mustTranscribe(t, r, nil)
	creates := r.api.requests("create")
	if !uuidV4.Match(keyOf(creates[0].body)) || !uuidV4.Match(keyOf(creates[1].body)) || bytes.Equal(keyOf(creates[0].body), keyOf(creates[1].body)) {
		t.Errorf("keys %q and %q: want two different UUID v4", keyOf(creates[0].body), keyOf(creates[1].body))
	}
	mustTranscribe(t, r, &TranscribeOptions{IdempotencyKey: "my-own-key-0123456789"})
	if got := string(keyOf(r.api.requests("create")[2].body)); got != "my-own-key-0123456789" {
		t.Errorf("key = %q", got)
	}
}

func TestACreateLostAfterTheServerCommittedIsReplayedWithTheSameKey(t *testing.T) {
	r := newRig(t)
	var dropped atomic.Bool
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "create" && dropped.CompareAndSwap(false, true) {
			// commit the job the way the server would, then lose the answer
			r.api.mu.Lock()
			r.api.jobs[string(keyOf(s.body))] = "job_1"
			r.api.size = int64(len(r.wav))
			r.api.mu.Unlock()
			return 0, "", true
		}
		return 0, "", false
	})
	mustTranscribe(t, r, nil)
	creates := r.api.requests("create")
	if len(creates) != 2 || !bytes.Equal(keyOf(creates[0].body), keyOf(creates[1].body)) {
		t.Errorf("creates = %d", len(creates))
	}
	if len(r.api.jobs) != 1 {
		t.Errorf("jobs = %d, want 1", len(r.api.jobs))
	}
}

func TestACompleteThatAlreadyReportsDoneReadsTheJobOnceWithoutPolling(t *testing.T) {
	r := newRig(t)
	r.api.completeAs = "done"
	r.api.jobStates = []string{"done"}
	mustTranscribe(t, r, nil)
	if n := r.api.count("job"); n != 1 {
		t.Errorf("job reads = %d, want 1", n)
	}
	if n := r.api.count("transcript"); n != 1 {
		t.Errorf("transcript fetches = %d", n)
	}
}

func TestUploadIncompleteIsAnUploadError(t *testing.T) {
	r := newRig(t)
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "complete" {
			return 422, `{"error":"upload_incomplete","code":1041}`, true
		}
		return 0, "", false
	})
	_, err := r.transcribe(nil)
	e := wantCode(t, err, KindUpload, "upload_incomplete")
	if e.NumericCode != 1041 || e.Status != 422 || e.IdempotencyKey == "" || e.ResumeToken == "" {
		t.Errorf("error = %+v", e)
	}
}

func TestAFailedJobIsAJobFailedErrorWithItsKindAndText(t *testing.T) {
	r := newRig(t)
	r.api.jobStates = []string{"processing", "failed"}
	r.api.jobFields = map[string]any{"error": "Could not decode the audio", "failure_kind": "media_undecodable"}
	_, err := r.transcribe(nil)
	e := wantCode(t, err, KindJobFailed, "job_failed")
	if e.JobID != "job_1" || e.JobError != "Could not decode the audio" || e.FailureKind != "media_undecodable" || e.ResumeToken == "" {
		t.Errorf("error = %+v", e)
	}
	if r.api.count("transcript") != 0 {
		t.Error("fetched a transcript for a failed job")
	}
	r2 := newRig(t)
	r2.api.jobStates = []string{"failed"}
	r2.api.jobFields = map[string]any{"error": nil, "failure_kind": "something_new"}
	_, err = r2.transcribe(nil)
	if e := wantCode(t, err, KindJobFailed, "job_failed"); e.FailureKind != "" || e.JobError != "" {
		t.Errorf("an unknown failure kind and no text leaked through: %+v", e)
	}
}

func TestADoneJobWithoutATranscriptIdIsAnUploadError(t *testing.T) {
	r := newRig(t)
	r.api.jobStates = []string{"done"}
	r.api.jobFields = map[string]any{"transcript_id": nil}
	_, err := r.transcribe(nil)
	_ = wantCode(t, err, KindUpload, "transcript_missing")
}

func TestAnUnknownJobStateKeepsPolling(t *testing.T) {
	r := newRig(t)
	r.api.jobStates = []string{"reviewing", "reviewing", "done"}
	mustTranscribe(t, r, nil)
	if n := r.api.count("job"); n != 3 {
		t.Errorf("job reads = %d, want 3", n)
	}
}

func TestPollingBacksOffFromFiveSecondsByOneAndAHalfToFifteenAndTimesOutOnTheDeadline(t *testing.T) {
	r := newRig(t)
	r.api.jobStates = []string{"processing"}
	_, err := r.transcribe(&TranscribeOptions{WaitTimeout: 60 * time.Second})
	e := wantCode(t, err, KindWaitTimeout, "wait_timeout")
	if e.ResumeToken == "" || e.IdempotencyKey == "" {
		t.Errorf("a timeout carries the token and key: %+v", e)
	}
	var polls []time.Duration
	for _, d := range r.clock.sleeps() {
		if d < 30*time.Second {
			polls = append(polls, d)
		}
	}
	want := []time.Duration{5 * time.Second, 7500 * time.Millisecond, 11250 * time.Millisecond, 15 * time.Second, 15 * time.Second, 6250 * time.Millisecond}
	if fmt.Sprint(polls) != fmt.Sprint(want) {
		t.Errorf("poll sleeps = %v, want %v", polls, want)
	}
	reads := r.api.requests("job")
	if gap := reads[len(reads)-1].at.Sub(reads[0].at); gap != 60*time.Second {
		t.Errorf("last read %v after the first, want exactly the 60 s deadline", gap)
	}
}

func TestResumeTokenIsBase64URLJSONWithTheJobTheKeyTheSizeAndTheDuration(t *testing.T) {
	r := newRig(t)
	r.api.jobStates = []string{"processing"}
	var token string
	_, err := r.transcribe(&TranscribeOptions{WaitTimeout: time.Second, OnResumeToken: func(s string) { token = s }})
	e := wantCode(t, err, KindWaitTimeout, "wait_timeout")
	if token == "" || token != e.ResumeToken {
		t.Fatalf("OnResumeToken %q, error token %q", token, e.ResumeToken)
	}
	if strings.ContainsAny(token, "=+/") {
		t.Errorf("token %q is not unpadded base64url", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		V               int     `json:"v"`
		JobID           string  `json:"jobId"`
		IdempotencyKey  string  `json:"idempotencyKey"`
		Size            int64   `json:"size"`
		DurationSeconds float64 `json:"durationSeconds"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.V != 1 || body.JobID != "job_1" || body.IdempotencyKey != e.IdempotencyKey || body.Size != int64(len(r.wav)) || body.DurationSeconds != 20 {
		t.Errorf("token body = %s", raw)
	}
}

func timedOut(t *testing.T, r *rig) string {
	t.Helper()
	r.api.jobStates = []string{"processing"}
	_, err := r.transcribe(&TranscribeOptions{WaitTimeout: time.Second})
	return wantCode(t, err, KindWaitTimeout, "wait_timeout").ResumeToken
}

func TestResumeAfterTheUploadFinishedWaitsAndFetchesWithoutTheFileOrAReplay(t *testing.T) {
	r := newRig(t)
	token := timedOut(t, r)
	r.api.mu.Lock()
	r.api.jobStates, r.api.jobReads = []string{"processing", "done"}, 0
	r.api.mu.Unlock()
	before := r.api.count("create")
	got, err := r.client.Resume(context.Background(), token, nil)
	if err != nil || got.ID != "tr_1" {
		t.Fatalf("Resume = %v, %v", got, err)
	}
	if r.api.count("create") != before || r.api.count("complete") != 1 {
		t.Errorf("creates %d (was %d), completes %d", r.api.count("create"), before, r.api.count("complete"))
	}
}

func TestResumeWithAFileOfAnotherSizeIsRefusedBeforeAnyRequest(t *testing.T) {
	r := newRig(t)
	token := timedOut(t, r)
	before := r.api.total()
	other := testWAV(21)
	_, err := r.client.Resume(context.Background(), token, &ResumeOptions{Reader: bytes.NewReader(other), Size: int64(len(other))})
	_ = wantCode(t, err, KindResumeMismatch, "resume_file_mismatch")
	if r.api.total() != before {
		t.Errorf("%d requests were sent", r.api.total()-before)
	}
	if e := asError(t, err); e.Status != 0 || !strings.Contains(e.Hint, "not the file the upload started with") {
		t.Errorf("error = %+v", e)
	}
}

func TestResumeOfAJobThatIsStillUploadingReplaysTheCreateAndNeedsTheFile(t *testing.T) {
	r := newRig(t)
	var token string
	r.api.jobStates = []string{"uploading", "uploading", "processing", "done"}
	cancelled, cancel := context.WithCancel(context.Background())
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "r2" || s.route == "part" {
			cancel()
			return 500, "down", true
		}
		return 0, "", false
	})
	_, err := r.client.TranscribeReader(cancelled, bytes.NewReader(r.wav), int64(len(r.wav)), &TranscribeOptions{OnResumeToken: func(s string) { token = s }})
	e := wantCode(t, err, KindAbort, "aborted")
	if e.ResumeToken != token || token == "" {
		t.Fatalf("a cancelled call carries the token: %+v (%q)", e, token)
	}
	r.api.setHook(nil)
	_, err = r.client.Resume(context.Background(), token, nil)
	_ = wantCode(t, err, KindResumeMismatch, "resume_file_required")
	if r.api.count("create") != 1 {
		t.Errorf("a resume without the file replayed the create")
	}
	got, err := r.client.Resume(context.Background(), token, &ResumeOptions{Reader: bytes.NewReader(r.wav), Size: int64(len(r.wav))})
	if err != nil || got.ID != "tr_1" {
		t.Fatalf("Resume with the file = %v, %v", got, err)
	}
	creates := r.api.requests("create")
	if len(creates) != 2 || !bytes.Equal(keyOf(creates[0].body), keyOf(creates[1].body)) || len(r.api.jobs) != 1 {
		t.Errorf("creates %d, jobs %d", len(creates), len(r.api.jobs))
	}
}

func TestResumeOfAJobThatNoLongerExistsDoesNotReplayTheCreate(t *testing.T) {
	r := newRig(t)
	token := timedOut(t, r)
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "job" {
			return 404, `{"error":"not_found","code":4040}`, true
		}
		return 0, "", false
	})
	creates := r.api.count("create")
	_, err := r.client.Resume(context.Background(), token, &ResumeOptions{Reader: bytes.NewReader(r.wav), Size: int64(len(r.wav))})
	e := wantCode(t, err, KindResumeMismatch, "resume_job_missing")
	if r.api.count("create") != creates || !strings.Contains(e.Hint, "no longer exists") {
		t.Errorf("creates %d (was %d), hint %q", r.api.count("create"), creates, e.Hint)
	}
}

func TestAReplayThatReturnsAnotherJobStopsWithoutFurtherRequests(t *testing.T) {
	r := newRig(t)
	r.api.jobStates = []string{"uploading"}
	var token string
	cancelled, cancel := context.WithCancel(context.Background())
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "r2" {
			cancel()
			return 500, "down", true
		}
		return 0, "", false
	})
	_, _ = r.client.TranscribeReader(cancelled, bytes.NewReader(r.wav), int64(len(r.wav)), &TranscribeOptions{OnResumeToken: func(s string) { token = s }})
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "create" {
			return 200, `{"job_id":"job_other","state":"uploading","balance_seconds":1,"held_seconds":1,"funding":"personal","limits":{"active_jobs":1,"max_active_jobs":50,"max_daily_seconds":1,"used_today_seconds":0},"upload":null}`, true
		}
		return 0, "", false
	})
	waitFor(t, func() bool { return r.api.inFlight.Load() == 0 }) // a part of the cancelled run may still be arriving
	before := r.api.total()
	_, err := r.client.Resume(context.Background(), token, &ResumeOptions{Reader: bytes.NewReader(r.wav), Size: int64(len(r.wav))})
	_ = wantCode(t, err, KindResumeMismatch, "resume_job_missing")
	if sent := r.api.total() - before; sent != 2 { // the job read and the replay, nothing else
		t.Errorf("%d requests after the replay returned another job, want 2 in all", sent)
	}
}

func TestABadResumeTokenIsAConfigurationError(t *testing.T) {
	r := newRig(t)
	for _, token := range []string{
		"", "!!!", base64.RawURLEncoding.EncodeToString([]byte(`{"v":2}`)),
		base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"jobId":"","idempotencyKey":"k","size":1,"durationSeconds":20}`)),
		base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"jobId":"j","idempotencyKey":"k","size":0,"durationSeconds":20}`)),
		base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"jobId":"j","idempotencyKey":"k","size":1,"durationSeconds":-1}`)),
		base64.RawURLEncoding.EncodeToString([]byte(`[]`)),
	} {
		_, err := r.client.Resume(context.Background(), token, nil)
		_ = wantCode(t, err, KindConfiguration, "config_invalid")
	}
	if r.api.total() != 0 {
		t.Errorf("%d requests for bad tokens", r.api.total())
	}
}

func TestJobsWaitReturnsTheFinishedJobWhateverItsOutcome(t *testing.T) {
	r := newRig(t)
	r.api.jobStates = []string{"queued", "failed"}
	r.api.jobFields = map[string]any{"error": "boom", "failure_kind": "media_unreadable"}
	var reports []Progress
	job, err := r.client.Jobs.Wait(context.Background(), "job_9", &WaitOptions{OnProgress: func(p Progress) { reports = append(reports, p) }})
	if err != nil || job.State != "failed" || job.Error != "boom" || job.FailureKind != "media_unreadable" || job.ID != "job_9" {
		t.Fatalf("Wait = %+v, %v", job, err)
	}
	if len(reports) != 1 || reports[0] != (Progress{Stage: StageProcessing, State: "queued"}) {
		t.Errorf("reports = %+v", reports)
	}
	r.api.mu.Lock()
	r.api.jobStates, r.api.jobReads = []string{"done"}, 0
	r.api.jobFields = nil
	r.api.mu.Unlock()
	if job, err = r.client.Jobs.Wait(context.Background(), "job_9", nil); err != nil || job.TranscriptID != "tr_1" {
		t.Errorf("Wait = %+v, %v", job, err)
	}
}

func TestJobsWaitTimesOutWithoutAResumeToken(t *testing.T) {
	r := newRig(t)
	r.api.jobStates = []string{"processing"}
	_, err := r.client.Jobs.Wait(context.Background(), "job_9", &WaitOptions{WaitTimeout: 20 * time.Second})
	e := wantCode(t, err, KindWaitTimeout, "wait_timeout")
	if e.ResumeToken != "" {
		t.Errorf("token %q", e.ResumeToken)
	}
	_, err = r.client.Jobs.Wait(context.Background(), "job_9", &WaitOptions{WaitTimeout: -time.Second})
	_ = wantCode(t, err, KindPreflight, "invalid_option")
	var zero Client
	_, err = zero.Jobs.Wait(context.Background(), "x", nil)
	_ = wantCode(t, err, KindConfiguration, "config_invalid")
}

func TestInvalidOptionsAreRefusedBeforeAnyRequestAndNeverClamped(t *testing.T) {
	r := newRig(t)
	for name, opts := range map[string]*TranscribeOptions{
		"concurrency -1":         {Concurrency: -1},
		"concurrency 9":          {Concurrency: 9},
		"wait timeout negative":  {WaitTimeout: -time.Second},
		"duration NaN":           {DurationSeconds: math.NaN()},
		"duration +Inf":          {DurationSeconds: math.Inf(1)},
		"duration -Inf":          {DurationSeconds: math.Inf(-1)},
		"duration negative":      {DurationSeconds: -5},
		"short key":              {IdempotencyKey: "short"},
		"long key":               {IdempotencyKey: strings.Repeat("k", 129)},
		"key with a newline":     {IdempotencyKey: "0123456789abcdef\n"},
		"key with a space":       {IdempotencyKey: "0123456789abcde f"},
		"key with a DEL":         {IdempotencyKey: "0123456789abcdef\x7f"},
		"key with non-ASCII":     {IdempotencyKey: "0123456789abcdefé"},
		"medical without lang":   {Medical: true},
		"medical in Japanese":    {Medical: true, Language: "ja"},
		"too many keyterms":      {Keyterms: make([]string, 1001)},
		"a keyterm of 7 words":   {Keyterms: []string{"one two three four five six seven"}},
		"matter code too long":   {MatterCode: strings.Repeat("m", 65)},
		"a keyterm of 7, spaced": {Keyterms: []string{"  a  b c d e f g "}},
	} {
		_, err := r.transcribe(opts)
		e := wantCode(t, err, KindPreflight, "invalid_option")
		if e.Status != 0 || e.IdempotencyKey != "" || e.ResumeToken != "" {
			t.Errorf("%s: %+v", name, e)
		}
	}
	if n := r.api.total(); n != 0 {
		t.Errorf("%d requests were sent for invalid options", n)
	}
	for _, concurrency := range []int{1, 2, 7, 8} {
		if _, err := r.transcribe(&TranscribeOptions{Concurrency: concurrency}); err != nil {
			t.Errorf("concurrency %d: %v", concurrency, err)
		}
	}
	if _, err := r.transcribe(&TranscribeOptions{Medical: true, Language: "de", Keyterms: []string{"one two three four five six"}, MatterCode: strings.Repeat("m", 64)}); err != nil {
		t.Errorf("limits are inclusive: %v", err)
	}
}

func TestADurationOutsideFifteenToThirtySixThousandSecondsIsAFileLimitErrorWithNoRequest(t *testing.T) {
	r := newRig(t)
	for name, seconds := range map[string]float64{"14.9": 14.9, "10": 10, "36000.5": 36000.5, "50000": 50000} {
		_, err := r.transcribe(&TranscribeOptions{DurationSeconds: seconds})
		e := wantCode(t, err, KindFileLimit, "duration_out_of_range")
		if !strings.Contains(e.Hint, "between 15 seconds and 10 hours") {
			t.Errorf("%s: hint %q", name, e.Hint)
		}
	}
	if r.api.total() != 0 {
		t.Errorf("%d requests", r.api.total())
	}
	for _, seconds := range []float64{15, 36000} {
		if _, err := r.transcribe(&TranscribeOptions{DurationSeconds: seconds}); err != nil {
			t.Errorf("%v s is inside the limits: %v", seconds, err)
		}
	}
	creates := r.api.requests("create")
	var body struct {
		Seconds int `json:"duration_seconds"`
	}
	_ = json.Unmarshal(creates[0].body, &body)
	if body.Seconds != 15 {
		t.Errorf("declared %d s for an override of 15", body.Seconds)
	}
}

func TestTheProbedDurationIsDeclaredRoundedUp(t *testing.T) {
	r := newRig(t)
	r.wav = append(wavHeader(32000*20+16000), make([]byte, 32000*20+16000)...) // 20.5 s
	mustTranscribe(t, r, nil)
	var body struct {
		Seconds int   `json:"duration_seconds"`
		Size    int64 `json:"size_bytes"`
	}
	_ = json.Unmarshal(r.api.requests("create")[0].body, &body)
	if body.Seconds != 21 || body.Size != int64(len(r.wav)) {
		t.Errorf("create declared %+v", body)
	}
}

func TestAFileThatIsNotAudioIsRefusedByItsBytesWithTheConvertHint(t *testing.T) {
	r := newRig(t)
	r.wav = append([]byte{0x1A, 0x45, 0xDF, 0xA3}, make([]byte, 5000)...)
	_, err := r.transcribe(nil)
	e := wantCode(t, err, KindUnsupportedMedia, "unsupported_media")
	if !strings.Contains(e.Hint, "ffmpeg -i input -vn -c:a libopus output.opus") || r.api.total() != 0 {
		t.Errorf("hint %q, requests %d", e.Hint, r.api.total())
	}
}

func TestADurationThatCannotBeReadAsksForTheOverride(t *testing.T) {
	r := newRig(t)
	r.wav = []byte("RIFF\x00\x00\x00\x00WAVEjunk")
	_, err := r.transcribe(nil)
	e := wantCode(t, err, KindUnsupportedMedia, "duration_unreadable")
	if !strings.Contains(e.Hint, "Pass durationSeconds") || r.api.total() != 0 {
		t.Errorf("hint %q, requests %d", e.Hint, r.api.total())
	}
}

func TestTheReaderSizeMustBePositiveAndAtMostFiveGigabytes(t *testing.T) {
	r := newRig(t)
	for _, size := range []int64{0, -1} {
		_, err := r.client.TranscribeReader(context.Background(), bytes.NewReader(r.wav), size, nil)
		_ = wantCode(t, err, KindPreflight, "invalid_option")
	}
	_, err := r.client.TranscribeReader(context.Background(), bytes.NewReader(r.wav), 5_000_000_001, &TranscribeOptions{DurationSeconds: 20})
	_ = wantCode(t, err, KindFileLimit, "size_out_of_range")
	_, err = r.client.TranscribeReader(context.Background(), nil, 100, nil)
	_ = wantCode(t, err, KindPreflight, "invalid_option")
	if r.api.total() != 0 {
		t.Errorf("%d requests", r.api.total())
	}
}

func TestNotEnoughBalanceIsRefusedBeforeTheCreateWithTheNumbersInTheHint(t *testing.T) {
	r := newRig(t)
	r.api.balance = 10
	_, err := r.transcribe(nil)
	e := wantCode(t, err, KindInsufficientBalance, "insufficient_balance")
	if e.Status != 0 || r.api.count("create") != 0 || !strings.Contains(e.Hint, "1 needed, 0 available") {
		t.Errorf("status %d, creates %d, hint %q", e.Status, r.api.count("create"), e.Hint)
	}
	if e.Details["needed_seconds"] != float64(20) || e.Details["balance_seconds"] != float64(10) {
		t.Errorf("details %v", e.Details)
	}
	r2 := newRig(t)
	r2.api.balance = 29 // medical doubles the estimate, each translation adds a quarter: 20 x 2.5 = 50
	_, err = r2.transcribe(&TranscribeOptions{Medical: true, Language: "en", TranslateTo: []string{"de", "fr"}})
	_ = wantCode(t, err, KindInsufficientBalance, "insufficient_balance")
	r2.api.set(func(f *fakeAPI) { f.balance = 50 })
	if _, err = r2.transcribe(&TranscribeOptions{Medical: true, Language: "en", TranslateTo: []string{"de", "fr"}}); err != nil {
		t.Errorf("50 s covers 20 x 2.5: %v", err)
	}
}

func TestPassingTheDailyLimitIsRefusedBeforeTheCreate(t *testing.T) {
	r := newRig(t)
	r.api.maxDaily, r.api.usedToday = 600, 590
	_, err := r.transcribe(nil)
	e := wantCode(t, err, KindDailyLimit, "daily_limit")
	if r.api.count("create") != 0 || !strings.Contains(e.Hint, "9 of 10 min") {
		t.Errorf("creates %d, hint %q", r.api.count("create"), e.Hint)
	}
}

func TestEveryErrorAfterTheCreateWasSentCarriesTheKeyAndOnceTheJobIsKnownTheToken(t *testing.T) {
	r := newRig(t)
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "create" {
			return 402, `{"error":"insufficient_balance","code":3001,"needed_seconds":21,"balance_seconds":10,"held_seconds":0}`, true
		}
		return 0, "", false
	})
	_, err := r.transcribe(nil)
	e := wantCode(t, err, KindInsufficientBalance, "insufficient_balance")
	if e.Status != 402 || e.NumericCode != 3001 || !uuidV4.MatchString(e.IdempotencyKey) || e.ResumeToken != "" {
		t.Errorf("a failed create carries the key and no token: %+v", e)
	}
	if got := string(keyOf(r.api.requests("create")[0].body)); got != e.IdempotencyKey {
		t.Errorf("error key %q, sent key %q", e.IdempotencyKey, got)
	}
	// Passing the key back replays the same job.
	r.api.setHook(nil)
	got, err := r.transcribe(&TranscribeOptions{IdempotencyKey: e.IdempotencyKey})
	if err != nil || got.ID != "tr_1" {
		t.Fatalf("replay = %v, %v", got, err)
	}
}

func TestACancelledCallIsAnAbortErrorThatCarriesTheResumeToken(t *testing.T) {
	r := newRig(t)
	r.api.jobStates = []string{"processing"}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for r.api.count("job") < 2 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	_, err := r.client.TranscribeReader(ctx, bytes.NewReader(r.wav), int64(len(r.wav)), &TranscribeOptions{})
	e := wantCode(t, err, KindAbort, "aborted")
	if e.ResumeToken == "" || e.IdempotencyKey == "" || !errors.Is(err, context.Canceled) {
		t.Errorf("error = %+v", e)
	}
}

func TestAReaderThatHoldsFewerBytesThanItsSizeIsAStreamSizeMismatchBeforeComplete(t *testing.T) {
	r := newRig(t)
	size := int64(len(r.wav)) + 100_000
	_, err := r.client.TranscribeReader(context.Background(), bytes.NewReader(r.wav), size, &TranscribeOptions{DurationSeconds: 20})
	e := wantCode(t, err, KindUpload, "stream_size_mismatch")
	if r.api.count("complete") != 0 || e.IdempotencyKey == "" || e.ResumeToken == "" || !strings.Contains(e.Hint, "Pass the exact size in bytes") {
		t.Errorf("completes %d, error %+v", r.api.count("complete"), e)
	}
}

func TestAReaderThatHoldsMoreBytesThanItsSizeIsAStreamSizeMismatchBeforeComplete(t *testing.T) {
	r := newRig(t)
	long := append(bytes.Clone(r.wav), 1, 2, 3)
	_, err := r.client.TranscribeReader(context.Background(), bytes.NewReader(long), int64(len(r.wav)), nil)
	_ = wantCode(t, err, KindUpload, "stream_size_mismatch")
	if r.api.count("complete") != 0 {
		t.Error("completed an upload whose source had bytes past its size")
	}
	// a reader that returns io.EOF together with the last bytes is legal
	exact := eofWithData{r.wav}
	if _, err := r.client.TranscribeReader(context.Background(), exact, int64(len(r.wav)), nil); err != nil {
		t.Errorf("EOF with the last bytes: %v", err)
	}
}

type eofWithData struct{ data []byte }

func (e eofWithData) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(e.data)) {
		return 0, io.EOF
	}
	n := copy(p, e.data[off:])
	if off+int64(n) == int64(len(e.data)) {
		return n, io.EOF
	}
	return n, nil
}

func TestAReadErrorFromTheSourceIsAnUploadErrorWithTheKey(t *testing.T) {
	r := newRig(t)
	broken := errors.New("disk on fire")
	src := brokenAfter{data: r.wav, at: 100_000, err: broken}
	_, err := r.client.TranscribeReader(context.Background(), src, int64(len(r.wav)), &TranscribeOptions{DurationSeconds: 20, Concurrency: 1})
	e := wantCode(t, err, KindUpload, "source_read_failed")
	if !errors.Is(err, broken) || e.IdempotencyKey == "" || e.ResumeToken == "" {
		t.Errorf("error %+v", e)
	}
}

type brokenAfter struct {
	data []byte
	at   int64
	err  error
}

func (b brokenAfter) ReadAt(p []byte, off int64) (int, error) {
	if off >= b.at {
		return 0, b.err
	}
	n := copy(p, b.data[off:min(off+int64(len(p)), b.at)])
	if n < len(p) {
		return n, b.err
	}
	return n, nil
}

func TestHeartbeatsComeEvery120SecondsFromTheCreateUntilCompleteAndThenStop(t *testing.T) {
	clock := newVirtualClock()
	api := newFakeAPI(t, clock)
	api.partSize = testPartSize
	api.heldR2 = make(chan struct{})
	client := pipelineClient(t, api, clock)
	wav := testWAV(20)
	done := make(chan error, 1)
	go func() {
		_, err := client.TranscribeReader(context.Background(), bytes.NewReader(wav), int64(len(wav)), nil)
		done <- err
	}()
	waitFor(t, func() bool { return api.count("r2") >= 1 && clock.sleepers() == 1 })
	start := clock.Now()
	for k := 1; k <= 3; k++ {
		clock.advanceTo(start.Add(time.Duration(k) * 120 * time.Second))
		waitFor(t, func() bool { return api.count("heartbeat") == k })
	}
	beats := api.requests("heartbeat")
	for k, beat := range beats {
		if want := start.Add(time.Duration(k+1) * 120 * time.Second); !beat.at.Equal(want) {
			t.Errorf("heartbeat %d at %v, want %v", k+1, beat.at.Sub(start), want.Sub(start))
		}
	}
	close(api.heldR2)
	clock.run(t, api)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if clock.sleepers() != 0 {
		t.Errorf("%d timers still pending after the call returned", clock.sleepers())
	}
	sent := api.count("heartbeat")
	clock.advance(time.Hour)
	time.Sleep(20 * time.Millisecond)
	if api.count("heartbeat") != sent {
		t.Error("heartbeats continued after the call returned")
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in 10 s")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAWrongStateAnswerStopsTheHeartbeatsAndIsIgnored(t *testing.T) {
	clock := newVirtualClock()
	api := newFakeAPI(t, clock)
	api.partSize = testPartSize
	api.heldR2 = make(chan struct{})
	api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "heartbeat" {
			return 409, `{"error":"wrong_state","code":4001}`, true
		}
		return 0, "", false
	})
	client := pipelineClient(t, api, clock)
	wav := testWAV(20)
	done := make(chan error, 1)
	go func() {
		_, err := client.TranscribeReader(context.Background(), bytes.NewReader(wav), int64(len(wav)), nil)
		done <- err
	}()
	waitFor(t, func() bool { return api.count("r2") >= 1 && clock.sleepers() == 1 })
	clock.advance(120 * time.Second)
	waitFor(t, func() bool { return api.count("heartbeat") == 1 })
	waitFor(t, func() bool { return clock.sleepers() == 0 })
	clock.advance(time.Hour)
	time.Sleep(20 * time.Millisecond)
	if api.count("heartbeat") != 1 {
		t.Errorf("heartbeats = %d after wrong_state, want 1", api.count("heartbeat"))
	}
	close(api.heldR2)
	clock.run(t, api)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAnUnauthorizedHeartbeatAbortsTheUploadAsAnAuthenticationError(t *testing.T) {
	clock := newVirtualClock()
	api := newFakeAPI(t, clock)
	api.partSize = testPartSize
	api.heldR2 = make(chan struct{})
	api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "heartbeat" {
			return 401, `{"error":"unauthorized","code":2000}`, true
		}
		return 0, "", false
	})
	client := pipelineClient(t, api, clock)
	wav := testWAV(20)
	done := make(chan error, 1)
	go func() {
		_, err := client.TranscribeReader(context.Background(), bytes.NewReader(wav), int64(len(wav)), nil)
		done <- err
	}()
	waitFor(t, func() bool { return api.count("r2") >= 1 && clock.sleepers() == 1 })
	clock.advance(120 * time.Second)
	err := <-done
	e := wantCode(t, err, KindAuthentication, "unauthorized")
	if e.Status != 401 || e.IdempotencyKey == "" || e.ResumeToken == "" {
		t.Errorf("error %+v", e)
	}
	if api.count("complete") != 0 {
		t.Error("completed after the token was refused")
	}
}

func TestDirectPartsNeverCarryTheTokenWhateverHostTheirURLIsOn(t *testing.T) {
	clock := newVirtualClock()
	storage := newFakeAPI(t, clock)
	api := newFakeAPI(t, clock)
	api.partSize = testPartSize
	api.urlBase = storage.URL
	api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "complete" {
			return 200, `{"ok":true,"state":"queued","balance_seconds":1,"held_seconds":0,"verified_seconds":20}`, true
		}
		return 0, "", false
	})
	clock.run(t, api)
	client := pipelineClient(t, api, clock)
	wav := testWAV(20)
	if _, err := client.TranscribeReader(context.Background(), bytes.NewReader(wav), int64(len(wav)), nil); err != nil {
		t.Fatal(err)
	}
	if api.count("r2") != 0 || storage.count("r2") != 4 || len(storage.r2Failures) != 0 {
		t.Errorf("api r2 %d, storage r2 %d, violations %v", api.count("r2"), storage.count("r2"), storage.r2Failures)
	}
}

func TestAPartURLThatCannotBeReachedLeaksNeitherItsSignatureNorItsQuery(t *testing.T) {
	r := newRig(t)
	r.api.deadPart = 4
	_, err := r.transcribe(&TranscribeOptions{Concurrency: 1})
	e := wantCode(t, err, KindUpload, "parts_exhausted")
	var cause *Error
	if !errors.As(errors.Unwrap(e), &cause) || cause.Kind != KindNetwork {
		t.Fatalf("cause = %v", errors.Unwrap(e))
	}
	assertNoSignature(t, err)
	assertNoSignature(t, cause)
}

func TestThePartTimeoutIs300SecondsAndTheDefaultIsSixtyForTheRest(t *testing.T) {
	var mu sync.Mutex
	deadlines := map[string]time.Duration{}
	clock := newVirtualClock()
	api := newFakeAPI(t, clock)
	api.partSize = testPartSize
	clock.run(t, api)
	inner := http.DefaultTransport
	client := pipelineClient(t, api, clock, WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		route, _ := routeOf(r.Method, r.URL.Path)
		if deadline, ok := r.Context().Deadline(); ok {
			mu.Lock()
			if got := time.Until(deadline); got > deadlines[route] {
				deadlines[route] = got
			}
			mu.Unlock()
		}
		return inner.RoundTrip(r)
	})}))
	wav := testWAV(20)
	if _, err := client.TranscribeReader(context.Background(), bytes.NewReader(wav), int64(len(wav)), &TranscribeOptions{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if d := deadlines["r2"]; d < 295*time.Second || d > 300*time.Second {
		t.Errorf("direct part deadline %v, want about 300 s", d)
	}
	if d := deadlines["create"]; d < 55*time.Second || d > 60*time.Second {
		t.Errorf("create deadline %v, want about 60 s", d)
	}
}

func TestNoMoreThanConcurrencyPartsAreInFlight(t *testing.T) {
	for _, concurrency := range []int{1, 2, 3} {
		clock := newVirtualClock()
		api := newFakeAPI(t, clock)
		api.partSize = 16_000
		clock.run(t, api)
		release := make(chan struct{})
		api.heldR2 = release
		client := pipelineClient(t, api, clock)
		wav := testWAV(20)
		done := make(chan error, 1)
		go func() {
			_, err := client.TranscribeReader(context.Background(), bytes.NewReader(wav), int64(len(wav)), &TranscribeOptions{Concurrency: concurrency})
			done <- err
		}()
		waitFor(t, func() bool { return api.r2Active.Load() == int64(concurrency) })
		time.Sleep(30 * time.Millisecond)
		if peak := api.r2Peak.Load(); peak != int64(concurrency) {
			t.Errorf("concurrency %d: %d parts in flight", concurrency, peak)
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestTheTranscriptionOptionsReachTheCreateBody(t *testing.T) {
	r := newRig(t)
	yes, no := true, false
	mustTranscribe(t, r, &TranscribeOptions{
		Title: "Board", Language: "en", Speakers: &no, TranslateTo: []string{"de"}, Keyterms: []string{"alpha"}, Prompt: "ctx",
		Medical: true, Multichannel: true, AutoInsights: &yes, MatterCode: "M-1",
	})
	var body map[string]any
	if err := json.Unmarshal(r.api.requests("create")[0].body, &body); err != nil {
		t.Fatal(err)
	}
	want := `{"duration_seconds":20,"idempotency_key":"` + fmt.Sprint(body["idempotency_key"]) + `","matter_code":"M-1","size_bytes":` + fmt.Sprint(len(r.wav)) +
		`,"title":"Board","transcription_options":{"auto_generate_insights":true,"keyterms_prompt":["alpha"],"language_code":"en","medical_mode":true,"multichannel":true,"prompt":"ctx","speaker_detection":false,"translation_targets":["de"]}}`
	got, _ := json.Marshal(body)
	if string(got) != want {
		t.Errorf("create body\n got %s\nwant %s", got, want)
	}
	r2 := newRig(t)
	mustTranscribe(t, r2, nil)
	if got := string(r2.api.requests("create")[0].body); strings.Contains(got, "transcription_options") || strings.Contains(got, `"title"`) {
		t.Errorf("an empty option set added fields: %s", got)
	}
}

func TestAZeroClientRefusesToTranscribe(t *testing.T) {
	var zero Client
	for _, err := range []error{
		func() error { _, err := zero.Transcribe(context.Background(), "x", nil); return err }(),
		func() error {
			_, err := zero.TranscribeReader(context.Background(), bytes.NewReader(nil), 1, nil)
			return err
		}(),
		func() error { _, err := zero.Resume(context.Background(), "t", nil); return err }(),
	} {
		_ = wantCode(t, err, KindConfiguration, "config_invalid")
	}
}
