package hushscript

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// zeroWAV is a WAV of size bytes that costs no memory: a real header, then silence.
type zeroWAV struct {
	size   int64
	header []byte
}

func newZeroWAV(size int64) zeroWAV {
	return zeroWAV{size: size, header: wavHeader(int(size - 44))}
}

func (z zeroWAV) ReadAt(p []byte, off int64) (int, error) {
	n := min(int64(len(p)), z.size-off)
	if n <= 0 {
		return 0, io.EOF
	}
	clear(p[:n])
	if off < int64(len(z.header)) {
		copy(p[:n], z.header[off:])
	}
	if n < int64(len(p)) {
		return int(n), io.EOF
	}
	return int(n), nil
}

func TestMemoryStaysWithinConcurrencyTimesPartSizeWhateverTheFileSize(t *testing.T) {
	const (
		part        = 8 << 20
		concurrency = 4
		size        = 160 << 20 // 20 parts, 5 times the bound
	)
	clock := newVirtualClock()
	api := newFakeAPI(t, clock)
	api.partSize = part
	clock.run(t, api)
	client := pipelineClient(t, api, clock)

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	var mu sync.Mutex
	peak := before.HeapInuse
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			var now runtime.MemStats
			runtime.ReadMemStats(&now)
			mu.Lock()
			peak = max(peak, now.HeapInuse)
			mu.Unlock()
		}
	}()
	_, err := client.TranscribeReader(context.Background(), newZeroWAV(size), size, &TranscribeOptions{Concurrency: concurrency})
	close(stop)
	<-sampled
	if err != nil {
		t.Fatal(err)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if got := api.count("r2"); got != 20 {
		t.Fatalf("parts sent = %d, want 20", got)
	}
	mu.Lock()
	defer mu.Unlock()
	growth := int64(peak) - int64(before.HeapInuse)          //nolint:gosec // heap sizes are far below 2^63
	allocated := int64(after.TotalAlloc - before.TotalAlloc) //nolint:gosec // as above
	t.Logf("part size %d MiB, concurrency %d: bound %d MiB; file %d MiB; peak heap growth %.1f MiB; total allocated %.1f MiB",
		part>>20, concurrency, concurrency*part>>20, size>>20, float64(growth)/(1<<20), float64(allocated)/(1<<20))
	const slack = 4 << 20
	if growth > concurrency*part+slack {
		t.Errorf("peak heap grew by %d MiB, want at most %d MiB (concurrency x part size) plus slack", growth>>20, concurrency*part>>20)
	}
	if allocated > concurrency*part+slack {
		t.Errorf("%d MiB allocated for a %d MiB file: part buffers are not reused", allocated>>20, size>>20)
	}
}

func flacFile(seconds uint64, tag []byte) []byte {
	const rate = 16000
	packed := uint64(rate)<<44 | uint64(0)<<41 | uint64(15)<<36 | seconds*rate
	info := append([]byte("fLaC\x80\x00\x00\x22"), 0x12, 0, 0x12, 0, 0, 0, 100, 0, 1, 0x90)
	info = binary.BigEndian.AppendUint64(info, packed)
	info = append(info, make([]byte, 16)...)
	return append(append(append([]byte{}, tag...), info...), make([]byte, 100_000)...)
}

func id3v2(bodySize int) []byte {
	return append([]byte{'I', 'D', '3', 4, 0, 0, byte(bodySize >> 21 & 0x7F), byte(bodySize >> 14 & 0x7F), byte(bodySize >> 7 & 0x7F), byte(bodySize & 0x7F)}, make([]byte, bodySize)...)
}

func TestAFLACBehindAnID3TagIsRecognizedAndProbed(t *testing.T) {
	r := newRig(t)
	r.wav = flacFile(20, id3v2(300))
	mustTranscribe(t, r, nil)
	var body struct {
		Seconds int `json:"duration_seconds"`
	}
	if err := json.Unmarshal(r.api.requests("create")[0].body, &body); err != nil || body.Seconds != 20 {
		t.Errorf("declared %d s (%v), want 20", body.Seconds, err)
	}
	r2 := newRig(t)
	r2.wav = flacFile(20, nil)
	mustTranscribe(t, r2, nil)
}

func TestARedactorHidesTheQueryOfAnyURLInText(t *testing.T) {
	r := redactor{token: testToken}
	in := `Put "https://r2.example.com/bucket/key?X-Amz-Signature=abcdef&X-Amz-Credential=zzz": dial tcp 1.2.3.4:443: connection refused; also http://127.0.0.1:4010/__r2/j/3?X-Amz-Signature=q`
	got := r.apply(in)
	if strings.Contains(got, "abcdef") || strings.Contains(got, "zzz") || strings.Contains(got, "=q") || !strings.Contains(got, "https://r2.example.com/bucket/key?…[redacted]") {
		t.Errorf("apply = %q", got)
	}
	if plain := "GET https://api.example.com/v1/jobs failed"; r.apply(plain) != plain {
		t.Errorf("a URL without a query changed: %q", r.apply(plain))
	}
}

func TestNewKeysAreUUIDv4(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		key := newIdempotencyKey()
		if !uuidV4.MatchString(key) || seen[key] {
			t.Fatalf("key %q is not a fresh UUID v4", key)
		}
		seen[key] = true
	}
}

func TestResumeTokensRoundTripAndRejectGarbage(t *testing.T) {
	state := resumeState{JobID: "job_1", IdempotencyKey: "0123456789abcdef", Size: 640044, DurationSeconds: 20.5}
	got, err := decodeResumeToken(encodeResumeToken(state))
	if err != nil || got != state {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	// padded base64url from another SDK is accepted too
	padded := encodeResumeToken(state)
	for len(padded)%4 != 0 {
		padded += "="
	}
	if got, err = decodeResumeToken(padded); err != nil || got != state {
		t.Errorf("padded token = %+v, %v", got, err)
	}
	for _, bad := range []string{"", "%%%", "e30", "bnVsbA"} {
		if _, err := decodeResumeToken(bad); err == nil {
			t.Errorf("token %q was accepted", bad)
		}
	}
}

func TestACanceledContextBeforeTheCreateIsAnAbortWithoutAToken(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.client.TranscribeReader(ctx, bytes.NewReader(r.wav), int64(len(r.wav)), nil)
	e := wantCode(t, err, KindAbort, "aborted")
	if e.ResumeToken != "" || r.api.count("create") != 0 {
		t.Errorf("error %+v, creates %d", e, r.api.count("create"))
	}
}

// A part that fails on R2 may fall back to the API only while no direct PUT has succeeded. The
// fallback decision and a concurrent direct success are interleaved by hand: the failing part waits
// at the decision until the other part has finished its direct PUT.
func TestAFallbackDecidedBeforeAConcurrentDirectSuccessNeverTurnsTheJobProxied(t *testing.T) {
	clock := newVirtualClock()
	api := newFakeAPI(t, clock)
	api.partSize = testPartSize
	clock.run(t, api)
	client := pipelineClient(t, api, clock, WithMaxRetries(0))
	wav := testWAV(20)

	inWindow := make(chan struct{})
	directDone := make(chan struct{})
	var windowOnce, doneOnce sync.Once
	client.core.uploadTrace = func(event string, part int) {
		switch {
		case event == "fallback" && part == 1:
			windowOnce.Do(func() { close(inWindow) })
			<-directDone
		case event == "direct-done" && part == 2:
			doneOnce.Do(func() { close(directDone) })
		}
	}
	api.setHook(func(s *seen) (int, string, bool) {
		switch {
		case s.route == "r2" && s.n == 1:
			return 500, "down", true
		case s.route == "r2" && s.n == 2:
			select { // part 2 answers only once part 1 is deciding
			case <-inWindow:
			case <-time.After(10 * time.Second):
			}
		}
		return 0, "", false
	})
	_, err := client.TranscribeReader(context.Background(), bytes.NewReader(wav), int64(len(wav)), &TranscribeOptions{Concurrency: 2})
	_ = wantCode(t, err, KindUpload, "parts_exhausted")
	if n := api.count("complete"); n != 0 {
		t.Errorf("%d complete requests after a failed part; a body-less complete would miss the direct parts", n)
	}
	// The swap was lost to a direct success, so the part is owed the direct path's one fresh URL page
	// (spec 8.4) and a second PUT before it is given up.
	if n := api.count("partUrls"); n != 1 {
		t.Errorf("%d part-urls fetches, want the one refetch", n)
	}
	attempts := 0
	for _, s := range api.requests("r2") {
		if s.n == 1 {
			attempts++
		}
	}
	if attempts != 2 {
		t.Errorf("part 1 was sent to storage %d times, want 2 (the PUT and the one after the refetch)", attempts)
	}
}

// The other order: the fallback wins the swap while a direct PUT is still on its way to being recorded.
// That part's R2 copy is not in the job's part list, so it is sent through the API again.
func TestADirectSuccessThatLosesTheSwapToAFallbackIsSentThroughTheAPIAgain(t *testing.T) {
	clock := newVirtualClock()
	api := newFakeAPI(t, clock)
	api.partSize = testPartSize
	clock.run(t, api)
	client := pipelineClient(t, api, clock, WithMaxRetries(0))
	wav := testWAV(20)

	client.core.uploadTrace = func(event string, part int) {
		if event == "direct-success" && part == 2 {
			for deadline := time.Now().Add(10 * time.Second); api.count("part") < 1 && time.Now().Before(deadline); { // part 1 is already going through the API
				time.Sleep(time.Millisecond)
			}
		}
	}
	api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "r2" && s.n == 1 {
			return 500, "down", true
		}
		return 0, "", false
	})
	if _, err := client.TranscribeReader(context.Background(), bytes.NewReader(wav), int64(len(wav)), &TranscribeOptions{Concurrency: 2}); err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	sent := map[int]bool{}
	for _, s := range api.requests("part") {
		sent[s.n] = true
	}
	if len(sent) != 4 || !sent[2] {
		t.Errorf("parts sent through the API = %v, want all four including part 2", sent)
	}
	if body := api.requests("complete")[0].body; len(body) != 0 {
		t.Errorf("a proxied job completed with a body: %s", body)
	}
}

// lateCloseTransport forwards requests to the default transport but closes each direct part's body
// only after release (the RoundTripper contract allows a transport to keep reading after it answered).
type lateCloseTransport struct {
	delay  time.Duration
	after  func() // called when a direct part has been answered
	mu     sync.Mutex
	starts []time.Time
	closes []time.Time
}

type lateBody struct {
	io.ReadCloser
	owner *lateCloseTransport
}

func (b lateBody) Close() error {
	b.owner.mu.Lock()
	b.owner.closes = append(b.owner.closes, time.Now())
	b.owner.mu.Unlock()
	return b.ReadCloser.Close()
}

func (l *lateCloseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.HasPrefix(req.URL.Path, "/r2/") {
		return http.DefaultTransport.RoundTrip(req)
	}
	l.mu.Lock()
	l.starts = append(l.starts, time.Now())
	l.mu.Unlock()
	original := req.Body
	forward := req.Clone(req.Context())
	forward.Body = io.NopCloser(original)
	resp, err := http.DefaultTransport.RoundTrip(forward)
	time.AfterFunc(l.delay, func() { _ = lateBody{ReadCloser: original, owner: l}.Close() })
	if err == nil && l.after != nil {
		l.after()
	}
	return resp, err
}

func TestAPartBufferWaitsForALateClosingTransportBeyondOneSecond(t *testing.T) {
	clock := newVirtualClock()
	api := newFakeAPI(t, clock)
	api.partSize = 330_000 // two parts
	clock.run(t, api)
	late := &lateCloseTransport{delay: 1300 * time.Millisecond}
	client := pipelineClient(t, api, clock, WithHTTPClient(&http.Client{Transport: late}))
	wav := testWAV(20)
	if _, err := client.TranscribeReader(context.Background(), bytes.NewReader(wav), int64(len(wav)), &TranscribeOptions{Concurrency: 1}); err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	late.mu.Lock()
	defer late.mu.Unlock()
	if len(late.starts) != 2 || len(late.closes) != 2 {
		t.Fatalf("part requests = %d, bodies closed = %d, want 2 and 2", len(late.starts), len(late.closes))
	}
	for i := 1; i < len(late.starts); i++ {
		if late.starts[i].Before(late.closes[i-1]) {
			t.Errorf("part %d started %v before the body of part %d was closed: its buffer was rewritten under the transport", i+1, late.closes[i-1].Sub(late.starts[i]), i)
		}
	}
}

func TestACancelDuringALateBodyCloseReturnsAtOnce(t *testing.T) {
	clock := newVirtualClock()
	api := newFakeAPI(t, clock)
	api.partSize = testPartSize
	clock.run(t, api)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cancelledAt time.Time
	var once sync.Once
	late := &lateCloseTransport{delay: 3 * time.Second, after: func() { once.Do(func() { cancelledAt = time.Now(); cancel() }) }}
	client := pipelineClient(t, api, clock, WithHTTPClient(&http.Client{Transport: late}))
	wav := testWAV(20)
	_, err := client.TranscribeReader(ctx, bytes.NewReader(wav), int64(len(wav)), &TranscribeOptions{Concurrency: 1})
	returned := time.Now()
	_ = wantCode(t, err, KindAbort, "aborted")
	if took := returned.Sub(cancelledAt); took > 400*time.Millisecond {
		t.Errorf("Transcribe returned %v after the cancel, waiting for a transport that closes late", took)
	}
}

// uploadingToken starts a transcription, cancels it during the first part and returns the resume
// token of the job, which the fake still reports as uploading.
func uploadingToken(t *testing.T, r *rig) string {
	t.Helper()
	var token string
	r.api.mu.Lock()
	r.api.jobStates, r.api.jobReads = []string{"uploading"}, 0
	r.api.mu.Unlock()
	cancelled, cancel := context.WithCancel(context.Background())
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "r2" || s.route == "part" {
			cancel()
			return 500, "down", true
		}
		return 0, "", false
	})
	_, err := r.client.TranscribeReader(cancelled, bytes.NewReader(r.wav), int64(len(r.wav)), &TranscribeOptions{OnResumeToken: func(s string) { token = s }})
	_ = wantCode(t, err, KindAbort, "aborted")
	r.api.setHook(nil)
	return token
}

func TestResumeOpensItsPathOnlyWhileTheJobIsUploading(t *testing.T) {
	r := newRig(t)
	missing := filepath.Join(t.TempDir(), "gone.wav")

	token := timedOut(t, r) // the upload is complete; the job is processing
	r.api.mu.Lock()
	r.api.jobStates, r.api.jobReads = []string{"processing", "done"}, 0
	r.api.mu.Unlock()
	got, err := r.client.Resume(context.Background(), token, &ResumeOptions{Path: missing})
	if err != nil || got.ID != "tr_1" {
		t.Fatalf("Resume of a finished upload with a Path that no longer exists = %v, %v", got, err)
	}

	r = newRig(t)
	token = uploadingToken(t, r)
	creates := r.api.count("create")
	_, err = r.client.Resume(context.Background(), token, &ResumeOptions{Path: missing})
	e := wantCode(t, err, KindPreflight, "file_unreadable")
	if !errors.Is(err, fs.ErrNotExist) || e.ResumeToken != token {
		t.Errorf("a needed file that is missing: %+v (is ErrNotExist %t)", e, errors.Is(err, fs.ErrNotExist))
	}
	if r.api.count("create") != creates {
		t.Error("the create was replayed without a file to upload")
	}

	other := filepath.Join(t.TempDir(), "other.wav")
	if err = os.WriteFile(other, testWAV(21), 0o600); err != nil {
		t.Fatal(err)
	}
	before := r.api.total()
	_, err = r.client.Resume(context.Background(), token, &ResumeOptions{Path: other})
	_ = wantCode(t, err, KindResumeMismatch, "resume_file_mismatch")
	if r.api.total() != before {
		t.Errorf("%d requests were sent for a file of another size", r.api.total()-before)
	}

	path := filepath.Join(t.TempDir(), "same.wav")
	if err = os.WriteFile(path, r.wav, 0o600); err != nil {
		t.Fatal(err)
	}
	r.api.mu.Lock()
	r.api.jobStates, r.api.jobReads = []string{"uploading", "processing", "done"}, 0
	r.api.mu.Unlock()
	if got, resumeErr := r.client.Resume(context.Background(), token, &ResumeOptions{Path: path}); resumeErr != nil || got.ID != "tr_1" {
		t.Fatalf("Resume with the original file as a Path = %v, %v", got, resumeErr)
	}
}

func TestResumeReportsZeroPercentBeforeItsReplayedCreate(t *testing.T) {
	r := newRig(t)
	token := uploadingToken(t, r)
	creates := r.api.count("create")
	r.api.mu.Lock()
	r.api.jobStates, r.api.jobReads = []string{"uploading", "processing", "done"}, 0
	r.api.mu.Unlock()
	var first *Progress
	createsAtFirst := -1
	_, err := r.client.Resume(context.Background(), token, &ResumeOptions{
		Reader: bytes.NewReader(r.wav), Size: int64(len(r.wav)),
		OnProgress: func(p Progress) {
			if first == nil {
				first, createsAtFirst = &p, r.api.count("create")
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || *first != (Progress{Stage: StageUploading, BytesTotal: int64(len(r.wav))}) {
		t.Fatalf("first report = %+v, want 0%% uploading", first)
	}
	if createsAtFirst != creates {
		t.Errorf("the 0%% report came after the replayed create (%d creates, was %d)", createsAtFirst, creates)
	}
}

func TestAnUnusableSuccessBodyIsBadResponseWithTheStatusItCameWith(t *testing.T) {
	cases := []struct {
		name, route, body string
		created           bool // the create was sent, so the error carries the key
	}{
		{"account {}", "account", `{}`, false},
		{"account null", "account", `null`, false},
		{"account mistyped", "account", `{"balance_seconds":"x","limits":{"used_today_seconds":0,"max_daily_seconds":9}}`, false},
		{"account truncated", "account", `{"balance_seconds":`, false},
		{"account without limits", "account", `{"balance_seconds":100000}`, false},
		{"create {}", "create", `{}`, true},
		{"create null", "create", `null`, true},
		{"create mistyped", "create", `{"job_id":7,"state":"uploading"}`, true},
		{"complete {}", "complete", `{}`, true},
		{"complete null", "complete", `null`, true},
		{"job {}", "job", `{}`, true},
		{"job mistyped", "job", `{"id":"job_1","state":5}`, true},
		{"transcript not an object", "transcript", `[]`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.api.setHook(func(s *seen) (int, string, bool) {
				if s.route == tc.route {
					return 200, tc.body, true
				}
				return 0, "", false
			})
			_, err := r.transcribe(nil)
			e := wantCode(t, err, KindAPI, "bad_response")
			if e.Status != 200 || e.Kind.IsPreflight() {
				t.Errorf("status %d, preflight %t", e.Status, e.Kind.IsPreflight())
			}
			if tc.created != (e.IdempotencyKey != "") {
				t.Errorf("idempotency key %q on an error where created = %t", e.IdempotencyKey, tc.created)
			}
			if !tc.created && r.api.count("create") != 0 {
				t.Error("a create was sent after a preflight answer the SDK could not use")
			}
		})
	}
}

// A 2xx other than 200 has no decoded body, so the SDK cannot use it however well formed it is; the error
// carries the status the API really answered with.
func TestAWellFormedBodyOnANon200SuccessIsBadResponseWithThatStatus(t *testing.T) {
	cases := []struct {
		name, route, body string
		created           bool
	}{
		{"account", "account", `{"balance_seconds":36000,"held_seconds":0,"limits":{"active_jobs":0,"max_active_jobs":50,"max_daily_seconds":100000,"used_today_seconds":0}}`, false},
		{"create", "create", `{"job_id":"job_1","state":"uploading"}`, true},
		{"complete", "complete", `{"ok":true,"state":"queued"}`, true},
		{"job", "job", `{"id":"job_1","state":"done","transcript_id":"tr_1"}`, true},
		{"transcript", "transcript", `{"id":"tr_1","title":"T","duration_seconds":20,"created_at":1,"updated_at":2,"body":{"version":1,"original":{"utterances":[]},"translations":{}}}`, true},
	}
	for _, status := range []int{201, 202, 204} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s %d", tc.name, status), func(t *testing.T) {
				r := newRig(t)
				r.api.setHook(func(s *seen) (int, string, bool) {
					if s.route == tc.route {
						return status, tc.body, true
					}
					return 0, "", false
				})
				_, err := r.transcribe(nil)
				e := wantCode(t, err, KindAPI, "bad_response")
				if e.Status != status {
					t.Errorf("status %d, want %d", e.Status, status)
				}
				if tc.created != (e.IdempotencyKey != "") {
					t.Errorf("idempotency key %q where created = %t", e.IdempotencyKey, tc.created)
				}
			})
		}
	}
}

func TestJobsWaitMapsAnUnusableJobBodyToBadResponse(t *testing.T) {
	for name, body := range map[string]string{"empty object": `{}`, "null": `null`, "mistyped": `{"id":"job_1","state":{}}`, "truncated": `{"id":`} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.api.setHook(func(s *seen) (int, string, bool) {
				if s.route == "job" {
					return 200, body, true
				}
				return 0, "", false
			})
			_, err := r.client.Jobs.Wait(context.Background(), "job_1", nil)
			e := wantCode(t, err, KindAPI, "bad_response")
			if e.Status != 200 {
				t.Errorf("status %d, want 200", e.Status)
			}
		})
	}
}

func TestHasFieldsNeedsEveryDottedPathPresentAndNotNull(t *testing.T) {
	for _, tc := range []struct {
		body  string
		paths []string
		want  bool
	}{
		{`{"a":1}`, []string{"a"}, true},
		{`{"a":0,"b":{"c":false}}`, []string{"a", "b.c"}, true},
		{`{"a":[]}`, []string{"a"}, true},
		{`{}`, []string{"a"}, false},
		{`{"a":null}`, []string{"a"}, false},
		{`{"b":null}`, []string{"b.c"}, false},
		{`{"b":5}`, []string{"b.c"}, false},
		{`{"b":{}}`, []string{"b.c"}, false},
		{`null`, nil, false},
		{`[]`, nil, false},
		{``, nil, false},
		{`{"a":1}`, nil, true},
	} {
		if got := hasFields([]byte(tc.body), tc.paths); got != tc.want {
			t.Errorf("hasFields(%s, %v) = %t, want %t", tc.body, tc.paths, got, tc.want)
		}
	}
}

// A 2xx part-urls answer is read at the call site: an object without part_urls, or with a row that has
// no n or no url, is a bad_response. Nothing is stored from it, so no part is ever sent to a made-up URL.
func TestAPartURLsAnswerWithoutUsableRowsIsBadResponseAndStoresNothing(t *testing.T) {
	const route = "GET /v1/uploads/j1/part-urls"
	for name, body := range map[string]string{
		"empty object":         `{}`,
		"null":                 `null`,
		"array":                `[]`,
		"part_urls null":       `{"part_urls":null}`,
		"empty row":            `{"part_urls":[{}]}`,
		"null row":             `{"part_urls":[null]}`,
		"row without url":      `{"part_urls":[{"n":1}]}`,
		"row without n":        `{"part_urls":[{"url":"https://r2.example/1"}]}`,
		"later row incomplete": `{"part_urls":[{"n":1,"url":"https://r2.example/1"},{"n":2}]}`,
		"url null":             `{"part_urls":[{"n":1,"url":null}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := newRouteServer(t, func(recorded, int) step { return ok(body) })
			c := srv.client(t)
			pages := newPartPages(c.core, "j1", &uploadPlan{next: 1})
			e := wantCode(t, pages.load(context.Background(), 1), KindAPI, "bad_response")
			if e.Status != http.StatusOK || len(srv.to(route)) != 1 {
				t.Errorf("status %d after %d requests", e.Status, len(srv.to(route)))
			}
			if len(pages.pages) != 0 || !pages.direct {
				t.Errorf("stored %d pages, direct %v", len(pages.pages), pages.direct)
			}
		})
	}
	srv := newRouteServer(t, func(recorded, int) step {
		return ok(`{"part_urls":[{"n":1,"url":"https://r2.example/1"},{"n":2,"url":"https://r2.example/2"}],"next_part_number":null}`)
	})
	pages := newPartPages(srv.client(t).core, "j1", &uploadPlan{next: 1})
	if err := pages.load(context.Background(), 1); err != nil || len(pages.pages) != 1 {
		t.Errorf("a complete answer: %v, %d pages", err, len(pages.pages))
	}
}
