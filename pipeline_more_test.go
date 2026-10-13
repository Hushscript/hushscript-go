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
	"sync/atomic"
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
	if err := json.Unmarshal(nth(t, r.api.requests("create"), 0).body, &body); err != nil || body.Seconds != 20 {
		t.Errorf("declared %d s (%v), want 20", body.Seconds, err)
	}
	r2 := newRig(t)
	r2.wav = flacFile(20, nil)
	mustTranscribe(t, r2, nil)
}

func TestARedactorHidesTheQueryOfAnyURLInText(t *testing.T) {
	r := newRedactor(testToken)
	in := `Put "https://r2.example.com/bucket/key?X-Amz-Signature=abcdef&X-Amz-Credential=zzz": dial tcp 1.2.3.4:443: connection refused; also http://127.0.0.1:4010/__r2/j/3?X-Amz-Signature=q`
	want := `Put "https://r2.example.com/bucket/key?[redacted]": dial tcp 1.2.3.4:443: connection refused; also http://127.0.0.1:4010/__r2/j/3?[redacted]`
	if got := r.apply(in); got != want {
		t.Errorf("apply = %q, want %q", got, want)
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

// lostSwap runs part 1 (always refused by storage) into the fallback decision only after part 2's direct
// PUT has succeeded, so part 1 loses the swap. partURLs answers the part-urls refetch it is then owed
// (nil: the fake's own page).
func lostSwap(t *testing.T, partURLs func() (int, string, bool)) (*fakeAPI, error) {
	t.Helper()
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
			select { // part 1 decides only once part 2's direct success is recorded
			case <-directDone:
			case <-time.After(10 * time.Second):
			}
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
		case s.route == "partUrls" && partURLs != nil:
			return partURLs()
		}
		return 0, "", false
	})
	_, err := client.TranscribeReader(context.Background(), bytes.NewReader(wav), int64(len(wav)), &TranscribeOptions{Concurrency: 2})
	return api, err
}

// A part that fails on R2 may fall back to the API only while no direct PUT has succeeded. The
// fallback decision and a concurrent direct success are interleaved by hand: the failing part waits
// at the decision until the other part has finished its direct PUT.
func TestAFallbackDecidedBeforeAConcurrentDirectSuccessNeverTurnsTheJobProxied(t *testing.T) {
	api, err := lostSwap(t, nil)
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

	var doneParts atomic.Int32
	client.core.uploadTrace = func(event string, part int) {
		if event == "direct-done" {
			doneParts.Add(1)
		}
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
	if body := nth(t, api.requests("complete"), 0).body; len(body) != 0 {
		t.Errorf("a proxied job completed with a body: %s", body)
	}
	if n := doneParts.Load(); n != 0 {
		t.Errorf("%d direct parts were recorded for a job that went proxied", n)
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

// Success is any 2xx with a usable body (spec section 7.4, R58): a 201, 202 or 203 is read like a 200,
// on every call of the pipeline.
func TestAWellFormedBodyOnAnyTwoXXSucceeds(t *testing.T) {
	for _, status := range []int{201, 202, 203} {
		for _, route := range []string{"account", "create", "partUrls", "complete", "job", "transcript"} {
			t.Run(fmt.Sprintf("%s %d", route, status), func(t *testing.T) {
				r := newRig(t)
				r.api.pageSize = 2
				r.api.set(func(f *fakeAPI) { f.successStatus = map[string]int{route: status} })
				tr, err := r.transcribe(nil)
				if err != nil || tr == nil || tr.ID != "tr_1" {
					t.Fatalf("a %d on %s: %v", status, route, err)
				}
				if r.api.count(route) == 0 {
					t.Fatalf("the rig never called %s", route)
				}
			})
		}
	}
}

// A 2xx with a body the SDK cannot use is bad_response with the status the API really answered with,
// whatever that status is.
func TestAnUnusableBodyOnANonOKTwoXXIsBadResponseWithThatStatus(t *testing.T) {
	cases := []struct {
		name, route, body string
		created           bool
	}{
		{"account", "account", `{"balance_seconds":"x"}`, false},
		{"create", "create", `{"state":"uploading"}`, true},
		{"complete", "complete", `{"ok":true}`, true},
		{"job", "job", `{"id":"job_1"}`, true},
		{"transcript", "transcript", `{"id":"tr_1"}`, true},
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
		{`{"c":1,"b":{}}`, []string{"b.c"}, false},
		{`{"c":1,"a":{"c":2,"b":{}}}`, []string{"a.b.c"}, false},
		{`{"a":{"b":{"c":0}}}`, []string{"a.b.c"}, true},
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

func TestRowsHaveNeedsAnArrayOfCompleteRowsAtTheKey(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"rows":[{"id":1},{"id":"x"}]}`, true},
		{`{"rows":[]}`, true},
		{`{}`, false},
		{`{"rows":5}`, false},
		{`{"rows":[{"id":null}]}`, false},
		{`{"rows":[{}]}`, false},
		{`not json`, false},
	} {
		if got := rowsHave([]byte(tc.body), "rows", "id"); got != tc.want {
			t.Errorf("rowsHave(%s) = %t, want %t", tc.body, got, tc.want)
		}
	}
}

func TestListRowsDecodesEachRowAndTheCursorOrRejectsTheAnswer(t *testing.T) {
	type row struct {
		ID string `json:"id"`
	}
	rows, raws, cursor, ok := listRows[row]([]byte(`{"rows":[{"id":"a"}, {"id":"b","x":1}],"next_cursor":"c2"}`), "rows")
	if !ok || fmt.Sprint(rows) != "[{a} {b}]" || len(raws) != 2 || string(raws[0]) != `{"id":"a"}` || string(raws[1]) != `{"id":"b","x":1}` || cursor != "c2" {
		t.Errorf("listRows = %v %q %q %t", rows, raws, cursor, ok)
	}
	for _, body := range []string{`{"rows":[],"next_cursor":null}`, `{"rows":[]}`} {
		if rows, _, cursor, ok := listRows[row]([]byte(body), "rows"); !ok || len(rows) != 0 || cursor != "" {
			t.Errorf("listRows(%s) = %v %q %t, want no rows and no cursor", body, rows, cursor, ok)
		}
	}
	for _, body := range []string{`{}`, `{"rows":{}}`, `{"rows":[],"next_cursor":5}`, `{"rows":[],"next_cursor":{}}`, `{"rows":[5]}`, `{"rows":[{"id":5}]}`} {
		if rows, raws, cursor, ok := listRows[row]([]byte(body), "rows"); ok || rows != nil || raws != nil || cursor != "" {
			t.Errorf("listRows(%s) = %v %q %q %t, want a rejection", body, rows, raws, cursor, ok)
		}
	}
}

// A 2xx part-urls answer is read at the call site: an object without part_urls, or with a row that has
// no n or no url, is a bad_response. Nothing is stored from it, so no part is ever sent to a made-up URL.
func TestAPartURLsAnswerWithoutUsableRowsIsBadResponseAndStoresNothing(t *testing.T) {
	const route = "GET /v1/uploads/j1/part-urls"
	for name, body := range map[string]string{
		"empty object":           `{}`,
		"array":                  `[]`,
		"empty row":              `{"part_urls":[{}]}`,
		"null row":               `{"part_urls":[null]}`,
		"row without url":        `{"part_urls":[{"n":1}]}`,
		"row without n":          `{"part_urls":[{"url":"https://r2.example/1"}]}`,
		"later row incomplete":   `{"part_urls":[{"n":1,"url":"https://r2.example/1"},{"n":2}]}`,
		"url null":               `{"part_urls":[{"n":1,"url":null}]}`,
		"n zero":                 `{"part_urls":[{"n":0,"url":"https://r2.example/0"}]}`,
		"n negative":             `{"part_urls":[{"n":-1,"url":"https://r2.example/1"}]}`,
		"url empty":              `{"part_urls":[{"n":1,"url":""}]}`,
		"later row unusable":     `{"part_urls":[{"n":1,"url":"https://r2.example/1"},{"n":0,"url":"https://r2.example/0"}]}`,
		"next part not a number": `{"part_urls":[{"n":1,"url":"https://r2.example/1"}],"next_part_number":"x"}`,
		"keys in another case":   `{"part_urls":[{"N":1,"URL":"https://r2.example/1"}]}`,
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
	// Usable rows under a content type that is not JSON: the answer is not decoded, so it is no page.
	plain := newRouteServer(t, func(recorded, int) step {
		return step{status: 200, headers: map[string]string{"Content-Type": "text/plain"}, body: `{"part_urls":[{"n":1,"url":"https://r2.example/1"}],"next_part_number":null}`}
	})
	plainPages := newPartPages(plain.client(t).core, "j1", &uploadPlan{next: 1})
	if e := wantCode(t, plainPages.load(t.Context(), 1), KindAPI, "bad_response"); e.Status != http.StatusOK || len(plainPages.pages) != 0 {
		t.Errorf("status %d, stored %d pages", e.Status, len(plainPages.pages))
	}
	srv := newRouteServer(t, func(recorded, int) step {
		return ok(`{"part_urls":[{"n":1,"url":"https://r2.example/1"},{"n":2,"url":"https://r2.example/2"}],"next_part_number":null}`)
	})
	pages := newPartPages(srv.client(t).core, "j1", &uploadPlan{next: 1})
	if err := pages.load(context.Background(), 1); err != nil || len(pages.pages) != 1 {
		t.Errorf("a complete answer: %v, %d pages", err, len(pages.pages))
	}
}

// A JSON null page is the API saying direct upload is not available for the job: the parts go through
// the API, as they do for an empty list.
func TestANullPartURLsPageMeansTheJobTakesProxiedPartsOnly(t *testing.T) {
	for name, body := range map[string]string{"null": `null`, "null with whitespace": " \n null \n", "empty list": `{"part_urls":[]}`, "part_urls null": `{"part_urls":null}`} {
		t.Run(name, func(t *testing.T) {
			srv := newRouteServer(t, func(recorded, int) step { return ok(body) })
			pages := newPartPages(srv.client(t).core, "j1", &uploadPlan{next: 1})
			if err := pages.load(context.Background(), 1); err != nil {
				t.Fatal(err)
			}
			url, direct, err := pages.urlFor(context.Background(), 1)
			if err != nil || direct || url != "" || len(pages.pages) != 0 {
				t.Errorf("url %q, direct %v, %d pages, %v", url, direct, len(pages.pages), err)
			}
		})
	}
	r := newRig(t)
	r.api.size = int64(len(r.wav))
	r.api.setHook(func(s *seen) (int, string, bool) {
		switch s.route {
		case "create":
			return 200, `{"job_id":"job_1","state":"uploading","balance_seconds":1,"held_seconds":1,"funding":"personal","limits":{"active_jobs":1,"max_active_jobs":50,"max_daily_seconds":1,"used_today_seconds":0},"upload":{"part_size_bytes":161000,"total_parts":4,"part_urls":[],"next_part_number":1}}`, true
		case "partUrls":
			return 200, `null`, true
		}
		return 0, "", false
	})
	mustTranscribe(t, r, nil)
	if r.api.count("partUrls") != 1 || r.api.count("r2") != 0 || r.api.count("part") != 4 || r.api.count("complete") != 1 {
		t.Errorf("part-urls %d, direct %d, proxied %d, complete %d", r.api.count("partUrls"), r.api.count("r2"), r.api.count("part"), r.api.count("complete"))
	}
}

// An HTTP 404 on a part-urls page is the server's answer when direct-to-R2 is not available for the job:
// it is handled exactly like a null page, and it is not retried.
func TestA404PartURLsPageMeansTheJobTakesProxiedPartsOnly(t *testing.T) {
	srv := newRouteServer(t, func(recorded, int) step {
		return step{status: 404, headers: map[string]string{"Content-Type": "application/json"}, body: jsonError("not_found", 4000)}
	})
	pages := newPartPages(srv.client(t).core, "j1", &uploadPlan{next: 1})
	if err := pages.load(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	url, direct, err := pages.urlFor(context.Background(), 1)
	if err != nil || direct || url != "" || len(pages.pages) != 0 {
		t.Errorf("url %q, direct %v, %d pages, %v", url, direct, len(pages.pages), err)
	}
	if got := len(srv.to("GET /v1/uploads/j1/part-urls")); got != 1 {
		t.Errorf("%d part-urls requests, want 1 (a 404 is not retried)", got)
	}
	r := newRig(t)
	r.api.size = int64(len(r.wav))
	r.api.setHook(func(s *seen) (int, string, bool) {
		switch s.route {
		case "create":
			return 200, `{"job_id":"job_1","state":"uploading","balance_seconds":1,"held_seconds":1,"funding":"personal","limits":{"active_jobs":1,"max_active_jobs":50,"max_daily_seconds":1,"used_today_seconds":0},"upload":{"part_size_bytes":161000,"total_parts":4,"part_urls":[],"next_part_number":1}}`, true
		case "partUrls":
			return 404, jsonError("not_found", 4000), true
		}
		return 0, "", false
	})
	mustTranscribe(t, r, nil)
	if got := r.counts(); got != "part-urls 1, direct 0, proxied 4, complete 1" {
		t.Error(got)
	}
}

// Once a direct PUT has succeeded, a 404 next page ends the upload as a null page does.
func TestA404NextPageAfterADirectSuccessIsPartURLMissing(t *testing.T) {
	r := newRig(t)
	r.api.pageSize = 2
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "partUrls" {
			return 404, jsonError("not_found", 4000), true
		}
		return 0, "", false
	})
	_, err := r.transcribe(&TranscribeOptions{Concurrency: 1})
	if e := wantCode(t, err, KindUpload, "part_url_missing"); e.Message != "No upload URL for part 3." {
		t.Errorf("message %q", e.Message)
	}
	if got := r.counts(); got != "part-urls 1, direct 2, proxied 0, complete 0" {
		t.Error(got)
	}
}

// The refresh after an expired URL that answers 404 reads as a null page: proxied parts while no direct
// PUT has succeeded (TestARefusedRefreshAfterAForbiddenFallsBack), part_url_missing after one has.
func TestA404RefreshAfterADirectSuccessIsPartURLMissing(t *testing.T) {
	r := newRig(t)
	r.client = pipelineClient(t, r.api, r.clock, WithMaxRetries(0))
	r.api.setHook(func(s *seen) (int, string, bool) {
		switch {
		case s.route == "r2" && s.n == 3:
			return 403, forbiddenPart, true
		case s.route == "partUrls":
			return 404, jsonError("not_found", 4000), true
		}
		return 0, "", false
	})
	_, err := r.transcribe(&TranscribeOptions{Concurrency: 1})
	if e := wantCode(t, err, KindUpload, "part_url_missing"); e.Message != "No upload URL for part 3." {
		t.Errorf("message %q", e.Message)
	}
	if got := r.counts(); got != "part-urls 1, direct 3, proxied 0, complete 0" {
		t.Error(got)
	}
}

// A 404 anywhere else keeps its meaning: a proxied part that answers it ends the upload as NotFoundError.
func TestA404ProxiedPartKeepsItsMeaning(t *testing.T) {
	r := newRig(t)
	r.api.proxiedOnly = true
	r.client = pipelineClient(t, r.api, r.clock, WithMaxRetries(0))
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "part" {
			return 404, jsonError("not_found", 4000), true
		}
		return 0, "", false
	})
	_, err := r.transcribe(&TranscribeOptions{Concurrency: 1})
	if e := wantCode(t, err, KindNotFound, "not_found"); e.Status != 404 {
		t.Errorf("status %d", e.Status)
	}
	if got := r.counts(); got != "part-urls 0, direct 0, proxied 1, complete 0" {
		t.Error(got)
	}
}

// The create answer's own part_urls rows are held to the same rule: a part number of zero or less, or
// an empty URL, is a bad_response and no part is sent.
func TestACreateAnswerWithUnusablePartURLRowsIsBadResponse(t *testing.T) {
	for name, rows := range map[string]string{
		"n zero":     `[{"n":0,"url":"https://r2.example/0"}]`,
		"n negative": `[{"n":-3,"url":"https://r2.example/3"}]`,
		"url empty":  `[{"n":1,"url":""}]`,
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.api.setHook(func(s *seen) (int, string, bool) {
				if s.route == "create" {
					return 200, `{"job_id":"job_1","state":"uploading","balance_seconds":1,"held_seconds":1,"funding":"personal","limits":{"active_jobs":1,"max_active_jobs":50,"max_daily_seconds":1,"used_today_seconds":0},"upload":{"part_size_bytes":160000,"total_parts":1,"part_urls":` + rows + `,"next_part_number":null}}`, true
				}
				return 0, "", false
			})
			_, err := r.transcribe(nil)
			_ = wantCode(t, err, KindAPI, "bad_response")
			if r.api.count("r2") != 0 || r.api.count("part") != 0 {
				t.Errorf("parts were sent: r2 %d, proxied %d", r.api.count("r2"), r.api.count("part"))
			}
		})
	}
}

func TestJobsWaitTimeoutRejectsNegativeAndAcceptsZeroDefault(t *testing.T) {
	for _, timeout := range []time.Duration{-time.Second, 0} {
		t.Run(timeout.String(), func(t *testing.T) {
			srv := newScriptedServer(t, step{status: 200, headers: map[string]string{"Content-Type": "application/json"}, body: jobJSON("job_1", "done", "")})
			c := newTestClient(t, srv.URL, newFakeClock())
			job, err := c.Jobs.Wait(context.Background(), "job_1", &WaitOptions{WaitTimeout: timeout})
			if timeout < 0 {
				e := wantCode(t, err, KindPreflight, "invalid_option")
				if e.Status != 0 || job != nil || srv.count() != 0 {
					t.Errorf("invalid timeout: status = %d, job = %+v, requests = %d; want 0, nil, 0", e.Status, job, srv.count())
				}
				return
			}
			if err != nil || job == nil || job.ID != "job_1" || job.State != "done" {
				t.Fatalf("default timeout: job = %+v, error = %v", job, err)
			}
			if srv.count() != 1 {
				t.Fatalf("default timeout sent %d requests, want 1", srv.count())
			}
			if request := srv.request(t, 0); request.method != "GET" || request.path != "/v1/jobs/job_1" {
				t.Errorf("request = %s %s, want GET /v1/jobs/job_1", request.method, request.path)
			}
		})
	}
}

func TestResumeInvalidOptionsSendNoRequests(t *testing.T) {
	for name, opts := range map[string]*ResumeOptions{
		"concurrency -1":        {Concurrency: -1},
		"concurrency 9":         {Concurrency: 9},
		"negative wait timeout": {WaitTimeout: -time.Second},
		"reader size zero":      {Reader: bytes.NewReader(testWAV(20)), Size: 0},
		"reader size negative":  {Reader: bytes.NewReader(testWAV(20)), Size: -1},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			token := encodeResumeToken(resumeState{JobID: "job_1", IdempotencyKey: strings.Repeat("k", 16), Size: int64(len(r.wav)), DurationSeconds: 20})
			_, err := r.client.Resume(context.Background(), token, opts)
			e := wantCode(t, err, KindPreflight, "invalid_option")
			if e.Status != 0 || r.api.total() != 0 {
				t.Errorf("status = %d, requests = %d; want 0, 0", e.Status, r.api.total())
			}
		})
	}
}

func TestHeartbeatFailuresAreIgnoredAndTheCadenceContinues(t *testing.T) {
	for name, status := range map[string]int{"500 after retries": 500, "403": 403, "404": 404, "network": 0} {
		t.Run(name, func(t *testing.T) {
			clock := newVirtualClock()
			api := newFakeAPI(t, clock)
			api.partSize = testPartSize
			ctx, release := api.holdUpload(t)
			client := pipelineClient(t, api, clock)
			api.setHook(func(s *seen) (int, string, bool) {
				if s.route == "heartbeat" {
					return status, `{"error":"heartbeat_failure","code":9999}`, true
				}
				return 0, "", false
			})
			done := make(chan error, 1)
			waitForAlive := func(condition func() bool) {
				waitFor(t, func() bool {
					select {
					case err := <-done:
						t.Fatalf("heartbeat failure ended the upload while parts were held: %v", err)
					default:
					}
					return condition()
				})
			}
			wav := testWAV(20)
			go func() {
				_, err := client.TranscribeReader(ctx, bytes.NewReader(wav), int64(len(wav)), nil)
				done <- err
			}()
			waitForAlive(func() bool { return api.count("r2") >= 1 && clock.sleepers() == 1 })
			start := clock.Now()
			advanceHeartbeatDue(t, clock, start.Add(120*time.Second))
			attempts := 1
			if status == 500 || status == 0 {
				attempts = 4
			}
			for k := 1; k <= attempts; k++ {
				waitForAlive(func() bool { return api.count("heartbeat") == k && clock.sleepers() == 1 })
				if k < attempts {
					at, ok := clock.earliest()
					if !ok {
						t.Fatal("no retry timer")
					}
					clock.advanceTo(at)
				}
			}
			at, ok := clock.earliest()
			if !ok || at.After(start.Add(240*time.Second)) {
				t.Fatalf("next heartbeat at %v, want no later than %v", at, start.Add(240*time.Second))
			}
			clock.advanceTo(at)
			waitForAlive(func() bool { return api.count("heartbeat") == attempts+1 && clock.sleepers() == 1 })
			beats := api.requests("heartbeat")
			if !beats[0].at.Equal(start.Add(120*time.Second)) || beats[attempts].at.After(start.Add(240*time.Second)) {
				t.Errorf("heartbeat times = %v, %v", beats[0].at, beats[attempts].at)
			}
			release()
			clock.run(t, api)
			if err := awaitUploadResult(t, done, api, clock); err != nil {
				t.Fatalf("heartbeat failure aborted upload: %v", err)
			}
			if api.count("complete") != 1 {
				t.Errorf("complete = %d, want 1", api.count("complete"))
			}
		})
	}
}

func TestResumeDefaultConcurrencyFinishes(t *testing.T) {
	r := newRig(t)
	token := stalledUpload(t, r)
	got, err := r.client.Resume(context.Background(), token, &ResumeOptions{
		Reader: bytes.NewReader(r.wav), Size: int64(len(r.wav)), Concurrency: 0,
	})
	if err != nil || got == nil || got.ID != "tr_1" {
		t.Fatalf("Resume = %v, %v", got, err)
	}
}

func TestResumeWithADirectoryAsItsPath(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t)
	token := timedOut(t, r)
	r.api.mu.Lock()
	r.api.jobStates, r.api.jobReads = []string{"processing", "done"}, 0
	r.api.mu.Unlock()
	if got, err := r.client.Resume(context.Background(), token, &ResumeOptions{Path: dir}); err != nil || got.ID != "tr_1" {
		t.Fatalf("Resume of a finished upload with a directory as its Path = %v, %v", got, err)
	}

	r = newRig(t)
	token = uploadingToken(t, r)
	creates := r.api.count("create")
	_, err := r.client.Resume(context.Background(), token, &ResumeOptions{Path: dir})
	if e := wantCode(t, err, KindPreflight, "file_unreadable"); e.ResumeToken != token || r.api.count("create") != creates {
		t.Errorf("a directory where the file is needed: %+v, %d creates (was %d)", e, r.api.count("create"), creates)
	}
}

func TestResumeRefusesAFileThatChangedSizeWhileTheJobWasRead(t *testing.T) {
	r := newRig(t)
	token := uploadingToken(t, r)
	path := filepath.Join(t.TempDir(), "same.wav")
	if err := os.WriteFile(path, r.wav, 0o600); err != nil {
		t.Fatal(err)
	}
	creates := r.api.count("create")
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "job" {
			if err := os.WriteFile(path, r.wav[:len(r.wav)-1], 0o600); err != nil {
				t.Error(err)
			}
		}
		return 0, "", false
	})
	_, err := r.client.Resume(context.Background(), token, &ResumeOptions{Path: path})
	if e := wantCode(t, err, KindResumeMismatch, "resume_file_mismatch"); e.ResumeToken != token || r.api.count("create") != creates {
		t.Errorf("a file one byte shorter by the time it was opened: %+v, %d creates (was %d)", e, r.api.count("create"), creates)
	}
}

func TestResumeReturnsAFailedReplayedCreateWithItsToken(t *testing.T) {
	r := newRig(t)
	token := uploadingToken(t, r)
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "create" {
			return http.StatusBadRequest, jsonError("invalid_request", 1000), true
		}
		return 0, "", false
	})
	_, err := r.client.Resume(context.Background(), token, &ResumeOptions{Reader: bytes.NewReader(r.wav), Size: int64(len(r.wav))})
	e := asError(t, err)
	if e.Kind != KindValidation || e.Code != "invalid_request" || e.Status != http.StatusBadRequest || e.ResumeToken != token {
		t.Errorf("a refused replayed create: %+v", e)
	}
}

func TestADecodedBodyIsDecodedOnlyWhenTheGeneratedParserDidNot(t *testing.T) {
	type answer struct {
		ID string `json:"id"`
	}
	jsonResponse := &http.Response{Header: http.Header{"Content-Type": {"application/json"}}}
	already := &answer{ID: "parsed"}
	if got := decodedBody(already, jsonResponse, []byte(`{"id":"body"}`)); got != already {
		t.Errorf("a parsed 200 came back as %+v, a second decode of its body", got)
	}
	if got := decodedBody[answer](nil, jsonResponse, []byte(`{"id":"body"}`)); got == nil || got.ID != "body" {
		t.Errorf("an unparsed json 2xx = %+v", got)
	}
	textResponse := &http.Response{Header: http.Header{"Content-Type": {"text/plain"}}}
	if got := decodedBody[answer](nil, textResponse, []byte(`{"id":"body"}`)); got != nil {
		t.Errorf("a text 2xx = %+v, want nothing to use", got)
	}
}

// traceEvents records the uploader's trace events ("fallback 1", "direct-done 2") of c.
func traceEvents(c *Client) func() []string {
	var mu sync.Mutex
	var events []string
	c.core.uploadTrace = func(event string, part int) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, fmt.Sprintf("%s %d", event, part))
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), events...)
	}
}

func hasEvent(events []string, prefix string) bool {
	for _, event := range events {
		if strings.HasPrefix(event, prefix) {
			return true
		}
	}
	return false
}

// counts reads the rig's request counts for the upload routes, in one line for a failure message.
func (r *rig) counts() string {
	return fmt.Sprintf("part-urls %d, direct %d, proxied %d, complete %d", r.api.count("partUrls"), r.api.count("r2"), r.api.count("part"), r.api.count("complete"))
}

const forbiddenPart = `<Error><Code>AccessDenied</Code></Error>`

// R8: size_exceeded from part-urls cannot be helped by sending the part another way, so it ends the
// upload as itself, before any direct success as well.
func TestASizeExceededPartURLsAnswerEndsTheUploadInsteadOfFallingBack(t *testing.T) {
	r := newRig(t)
	r.api.size = int64(len(r.wav))
	create := createPlan(`"part_size_bytes":161000,"total_parts":4`, `"part_urls":[],"next_part_number":1`)
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "partUrls" {
			return 422, jsonError("size_exceeded", 1024), true
		}
		return create(s)
	})
	_, err := r.transcribe(&TranscribeOptions{Concurrency: 1})
	e := wantCode(t, err, KindUpload, "size_exceeded")
	if e.Status != 422 || e.NumericCode != 1024 || e.ResumeToken == "" {
		t.Errorf("status %d, code %d, token %q", e.Status, e.NumericCode, e.ResumeToken)
	}
	if got := r.counts(); got != "part-urls 1, direct 0, proxied 0, complete 0" {
		t.Errorf("%s; want one part-urls fetch and nothing sent", got)
	}
}

// R8 on the refetch a 403 is owed: a size_exceeded answer ends the upload, it does not fall back.
func TestASizeExceededRefreshAfterAForbiddenEndsTheUploadAsItself(t *testing.T) {
	r := newRig(t)
	r.api.setHook(func(s *seen) (int, string, bool) {
		switch s.route {
		case "r2":
			return 403, forbiddenPart, true
		case "partUrls":
			return 422, jsonError("size_exceeded", 1024), true
		}
		return 0, "", false
	})
	_, err := r.transcribe(&TranscribeOptions{Concurrency: 1})
	if e := wantCode(t, err, KindUpload, "size_exceeded"); e.Status != 422 {
		t.Errorf("status %d", e.Status)
	}
	if got := r.counts(); got != "part-urls 1, direct 1, proxied 0, complete 0" {
		t.Errorf("%s; want the one refetch after the first PUT and nothing else", got)
	}
}

// Once a direct PUT has succeeded, a page that says direct upload is not available leaves the next part
// with no way to go: part_url_missing, and nothing goes through the API.
func TestANullPageAfterADirectSuccessIsPartURLMissing(t *testing.T) {
	r := newRig(t)
	r.api.pageSize = 2
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "partUrls" {
			return 200, `null`, true
		}
		return 0, "", false
	})
	_, err := r.transcribe(&TranscribeOptions{Concurrency: 1})
	if e := wantCode(t, err, KindUpload, "part_url_missing"); e.Message != "No upload URL for part 3." {
		t.Errorf("message %q", e.Message)
	}
	if got := r.counts(); got != "part-urls 1, direct 2, proxied 0, complete 0" {
		t.Error(got)
	}
}

// A storage answer without an ETag is a failed direct PUT like any other: with no direct success yet,
// the job falls back to proxied parts.
func TestADirectAnswerWithoutAnETagFallsBackWhileNoDirectPUTHasSucceeded(t *testing.T) {
	r := newRig(t)
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "r2" {
			return 200, `{}`, true
		}
		return 0, "", false
	})
	mustTranscribe(t, r, &TranscribeOptions{Concurrency: 1})
	if got := r.counts(); got != "part-urls 0, direct 1, proxied 4, complete 1" {
		t.Error(got)
	}
}

// Any failed PUT once a direct PUT has succeeded is owed one fresh page, a 403 included.
func TestAForbiddenAfterADirectSuccessGetsOneFreshPage(t *testing.T) {
	r := newRig(t)
	var refreshed atomic.Bool
	r.api.setHook(func(s *seen) (int, string, bool) {
		switch {
		case s.route == "partUrls":
			refreshed.Store(true)
		case s.route == "r2" && s.n == 2 && !refreshed.Load():
			return 403, forbiddenPart, true
		}
		return 0, "", false
	})
	mustTranscribe(t, r, &TranscribeOptions{Concurrency: 1})
	if got := r.counts(); got != "part-urls 1, direct 5, proxied 0, complete 1" {
		t.Error(got)
	}
}

// A refused first part-urls fetch, before any direct success, sends the parts through the API, with no
// second fetch of the same page.
func TestARefusedFirstPartURLsFetchFallsBackWithoutARefetch(t *testing.T) {
	r := newRig(t)
	r.api.size = int64(len(r.wav))
	create := createPlan(`"part_size_bytes":161000,"total_parts":4`, `"part_urls":[],"next_part_number":1`)
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "partUrls" {
			return 403, jsonError("pat_forbidden", 1234), true
		}
		return create(s)
	})
	mustTranscribe(t, r, &TranscribeOptions{Concurrency: 1})
	if got := r.counts(); got != "part-urls 1, direct 0, proxied 4, complete 1" {
		t.Error(got)
	}
}

// After a direct success, a failed fetch of the next page is the API's own answer when it is a 4xx other
// than 404 (which reads as a null page), and parts_exhausted with that answer as its cause otherwise; the page is asked for once either way.
func TestAFailedNextPageAfterADirectSuccess(t *testing.T) {
	for _, tc := range []struct {
		status int
		slug   string
		kind   ErrorKind
		code   string
	}{
		{400, "bad_request", KindValidation, "bad_request"},
		{503, "internal", KindUpload, "parts_exhausted"},
	} {
		t.Run(tc.slug, func(t *testing.T) {
			r := newRig(t)
			r.api.pageSize = 2
			r.client = pipelineClient(t, r.api, r.clock, WithMaxRetries(0))
			r.api.setHook(func(s *seen) (int, string, bool) {
				if s.route == "partUrls" {
					return tc.status, jsonError(tc.slug, 1234), true
				}
				return 0, "", false
			})
			_, err := r.transcribe(&TranscribeOptions{Concurrency: 1})
			e := wantCode(t, err, tc.kind, tc.code)
			if cause := errorStatus(e); cause != tc.status {
				t.Errorf("status of the error or its cause = %d, want %d", cause, tc.status)
			}
			if got := r.counts(); got != "part-urls 1, direct 2, proxied 0, complete 0" {
				t.Error(got)
			}
		})
	}
}

// errorStatus is the status of e, or of its cause when e is the SDK's own parts_exhausted.
func errorStatus(e *Error) int {
	if e.Code != "parts_exhausted" {
		return e.Status
	}
	var cause *Error
	if !errors.As(errors.Unwrap(e), &cause) {
		return -1
	}
	return cause.Status
}

// The fresh page a failed PUT is owed may itself fail. Once a direct PUT has succeeded, a 4xx other than
// 404 (which reads as a null page) ends the upload as itself, and anything else is parts_exhausted
// carrying that failure.
func TestAFailedRefreshAfterADirectSuccess(t *testing.T) {
	for _, tc := range []struct {
		status int
		slug   string
		kind   ErrorKind
		code   string
	}{
		{400, "bad_request", KindValidation, "bad_request"},
		{502, "internal", KindUpload, "parts_exhausted"},
	} {
		t.Run(tc.slug, func(t *testing.T) {
			r := newRig(t)
			r.client = pipelineClient(t, r.api, r.clock, WithMaxRetries(0))
			r.api.setHook(func(s *seen) (int, string, bool) {
				switch {
				case s.route == "r2" && s.n == 4:
					return 503, `busy`, true
				case s.route == "partUrls":
					return tc.status, jsonError(tc.slug, 1234), true
				}
				return 0, "", false
			})
			_, err := r.transcribe(&TranscribeOptions{Concurrency: 1})
			if e := wantCode(t, err, tc.kind, tc.code); errorStatus(e) != tc.status {
				t.Errorf("status of the error or its cause = %d, want %d", errorStatus(e), tc.status)
			}
			if got := r.counts(); got != "part-urls 1, direct 4, proxied 0, complete 0" {
				t.Error(got)
			}
		})
	}
}

// Before any direct success, a refresh after a 403 that answers 404 (read as a null page) leaves the API way open.
func TestARefusedRefreshAfterAForbiddenFallsBack(t *testing.T) {
	r := newRig(t)
	r.api.setHook(func(s *seen) (int, string, bool) {
		switch s.route {
		case "r2":
			return 403, forbiddenPart, true
		case "partUrls":
			return 404, jsonError("not_found", 1234), true
		}
		return 0, "", false
	})
	mustTranscribe(t, r, &TranscribeOptions{Concurrency: 1})
	if got := r.counts(); got != "part-urls 1, direct 1, proxied 4, complete 1" {
		t.Error(got)
	}
}

// A part that lost the swap gets its fresh page; when that fetch fails, a 4xx other than 404 (which reads
// as a null page) ends the upload as itself and anything else is parts_exhausted carrying the fetch's
// failure.
func TestAFailedRefreshAfterALostSwap(t *testing.T) {
	for _, tc := range []struct {
		status int
		slug   string
		kind   ErrorKind
		code   string
	}{
		{400, "bad_request", KindValidation, "bad_request"},
		{502, "internal", KindUpload, "parts_exhausted"},
	} {
		t.Run(tc.slug, func(t *testing.T) {
			api, err := lostSwap(t, func() (int, string, bool) { return tc.status, jsonError(tc.slug, 1234), true })
			if e := wantCode(t, err, tc.kind, tc.code); errorStatus(e) != tc.status {
				t.Errorf("status of the error or its cause = %d, want %d", errorStatus(e), tc.status)
			}
			if api.count("partUrls") != 1 || api.count("complete") != 0 || api.count("part") != 0 {
				t.Errorf("part-urls %d, complete %d, proxied %d", api.count("partUrls"), api.count("complete"), api.count("part"))
			}
		})
	}
}

// A refused proxied part ends the upload with the API's answer, and the job is not completed.
func TestAFailedProxiedPartEndsTheUploadWithItsAnswer(t *testing.T) {
	r := newRig(t)
	r.api.proxiedOnly = true
	r.client = pipelineClient(t, r.api, r.clock, WithMaxRetries(0))
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "part" {
			return 400, jsonError("bad_request", 1234), true
		}
		return 0, "", false
	})
	_, err := r.transcribe(&TranscribeOptions{Concurrency: 1})
	if e := wantCode(t, err, KindValidation, "bad_request"); e.Status != 400 || e.ResumeToken == "" {
		t.Errorf("status %d, token %q", e.Status, e.ResumeToken)
	}
	if got := r.counts(); got != "part-urls 0, direct 0, proxied 1, complete 0" {
		t.Error(got)
	}
}

// The first part to fail ends the others: a direct PUT still held by storage is cancelled, not waited for.
func TestTheFirstFailedPartCancelsThePartsStillInFlight(t *testing.T) {
	r := newRig(t)
	held := make(chan struct{})
	r.api.heldR2 = held
	t.Cleanup(func() { close(held) })
	r.api.setHook(func(s *seen) (int, string, bool) {
		switch {
		case s.route == "r2" && s.n == 1:
			return 400, `bad`, true
		case s.route == "part":
			return 400, jsonError("bad_request", 1234), true
		}
		return 0, "", false
	})
	done := make(chan error, 1)
	go func() {
		_, err := r.transcribe(&TranscribeOptions{Concurrency: 4})
		done <- err
	}()
	select {
	case err := <-done:
		_ = wantCode(t, err, KindValidation, "bad_request")
	case <-time.After(5 * time.Second):
		t.Fatal("the parts held by storage were not cancelled when part 1 failed")
	}
	r.api.mu.Lock()
	defer r.api.mu.Unlock()
	if len(r.api.parts) != 0 {
		t.Errorf("stored parts %v after the upload failed", r.api.parts)
	}
}

// Parts waiting for the same page share its one fetch.
func TestPartsWaitingForOnePageShareOneFetch(t *testing.T) {
	r := newRig(t)
	r.api.pageSize = 2
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "partUrls" {
			time.Sleep(100 * time.Millisecond) // parts 3 and 4 both wait for this page
		}
		return 0, "", false
	})
	mustTranscribe(t, r, &TranscribeOptions{Concurrency: 4})
	if got := r.counts(); got != "part-urls 1, direct 4, proxied 0, complete 1" {
		t.Error(got)
	}
}

// A page that ends the list sets no next page: a part no page holds is part_url_missing at once.
func TestALastPageLeavesNoNextPageToFetch(t *testing.T) {
	r := newRig(t)
	r.api.size = int64(len(r.wav))
	create := createPlan(`"part_size_bytes":161000,"total_parts":4`, `"part_urls":[],"next_part_number":1`)
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "partUrls" {
			return 200, fmt.Sprintf(`{"part_urls":[{"n":1,"url":"%[1]s/r2/job_1/1?X-Amz-Signature=sig1"},{"n":2,"url":"%[1]s/r2/job_1/2?X-Amz-Signature=sig2"}],"next_part_number":null}`, r.api.URL), true
		}
		return create(s)
	})
	_, err := r.transcribe(&TranscribeOptions{Concurrency: 1})
	if e := wantCode(t, err, KindUpload, "part_url_missing"); e.Message != "No upload URL for part 3." {
		t.Errorf("message %q", e.Message)
	}
	if got := r.counts(); got != "part-urls 1, direct 2, proxied 0, complete 0" {
		t.Error(got)
	}
}

// failingSleepClock is a virtual clock whose Sleep fails once fail is set, as a caller's own Clock may:
// the doer's retry wait then ends as an abort while the call's context is still alive.
type failingSleepClock struct {
	*virtualClock
	fail atomic.Bool
}

func (c *failingSleepClock) Sleep(ctx context.Context, d time.Duration) error {
	if c.fail.Load() {
		return errors.New("clock stopped")
	}
	return c.virtualClock.Sleep(ctx, d)
}

func newFailingSleepRig(t *testing.T) (*rig, *failingSleepClock) {
	t.Helper()
	clock := &failingSleepClock{virtualClock: newVirtualClock()}
	api := newFakeAPI(t, clock)
	api.partSize = testPartSize
	clock.run(t, api)
	return &rig{api: api, clock: clock.virtualClock, client: pipelineClient(t, api, clock), wav: testWAV(20)}, clock
}

// An abort is final even while the call's context is alive: a failed PUT whose retry wait was refused
// neither falls back to the API nor completes.
func TestAnAbortedDirectPUTDoesNotFallBack(t *testing.T) {
	r, clock := newFailingSleepRig(t)
	r.api.setHook(func(s *seen) (int, string, bool) {
		if s.route == "r2" {
			clock.fail.Store(true)
			return 503, `busy`, true
		}
		return 0, "", false
	})
	_, err := r.transcribe(&TranscribeOptions{Concurrency: 1})
	_ = wantCode(t, err, KindAbort, "aborted")
	if got := r.counts(); got != "part-urls 0, direct 1, proxied 0, complete 0" {
		t.Error(got)
	}
}

// The same for the fresh page after a direct success: an aborted refresh ends the upload as an abort.
func TestAnAbortedRefreshAfterADirectSuccessIsAnAbort(t *testing.T) {
	r, clock := newFailingSleepRig(t)
	r.api.setHook(func(s *seen) (int, string, bool) {
		switch {
		case s.route == "r2" && s.n == 4:
			return 403, forbiddenPart, true
		case s.route == "partUrls":
			clock.fail.Store(true)
			return 503, jsonError("internal", 1234), true
		}
		return 0, "", false
	})
	_, err := r.transcribe(&TranscribeOptions{Concurrency: 1})
	_ = wantCode(t, err, KindAbort, "aborted")
	if got := r.counts(); got != "part-urls 1, direct 4, proxied 0, complete 0" {
		t.Error(got)
	}
}

// cancelOnClose cancels the call when the body of an answer on a matching path is closed, which the
// doer does once it has read it: the failure that answer carries reaches the uploader with the caller's
// context already ended.
type cancelOnClose struct {
	path   string
	cancel context.CancelFunc
}

type cancellingBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancellingBody) Close() error {
	b.cancel()
	return b.ReadCloser.Close()
}

func (c cancelOnClose) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err == nil && strings.Contains(req.URL.Path, c.path) {
		resp.Body = cancellingBody{ReadCloser: resp.Body, cancel: c.cancel}
	}
	return resp, err
}

// A failure that arrives after the caller cancelled is the end of the part: no fallback is weighed and
// no refetch either.
func TestAFailureAfterTheCallerCancelledDecidesNothing(t *testing.T) {
	for _, tc := range []struct{ name, path string }{{"direct PUT", "/r2/"}, {"refresh", "/part-urls"}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r.client = pipelineClient(t, r.api, r.clock, WithMaxRetries(0), WithHTTPClient(&http.Client{Transport: cancelOnClose{path: tc.path, cancel: cancel}}))
			events := traceEvents(r.client)
			r.api.setHook(func(s *seen) (int, string, bool) {
				switch {
				case tc.path == "/r2/" && s.route == "r2":
					return 503, `busy`, true
				case tc.path == "/part-urls" && s.route == "r2" && s.n == 4:
					return 403, forbiddenPart, true
				case tc.path == "/part-urls" && s.route == "partUrls":
					return 500, jsonError("internal", 1234), true
				}
				return 0, "", false
			})
			if _, err := r.client.TranscribeReader(ctx, bytes.NewReader(r.wav), int64(len(r.wav)), &TranscribeOptions{Concurrency: 1}); err == nil {
				t.Fatal("a cancelled call succeeded")
			}
			if got := events(); hasEvent(got, "fallback") {
				t.Errorf("trace %v: a fallback was weighed after the cancel", got)
			}
			if n := r.api.count("part"); n != 0 {
				t.Errorf("%d proxied parts after the cancel", n)
			}
		})
	}
}

// readAfter counts the source reads made once the call was cancelled.
type readAfter struct {
	*bytes.Reader
	cancelled *atomic.Bool
	late      atomic.Int32
}

func (r *readAfter) ReadAt(p []byte, off int64) (int, error) {
	if r.cancelled.Load() {
		r.late.Add(1)
	}
	return r.Reader.ReadAt(p, off)
}

// A cancelled upload reads no further part from the source.
func TestNoPartIsReadAfterTheUploadWasCancelled(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var cancelled atomic.Bool
	src := &readAfter{Reader: bytes.NewReader(r.wav), cancelled: &cancelled}
	_, err := r.client.TranscribeReader(ctx, src, int64(len(r.wav)), &TranscribeOptions{Concurrency: 1, OnProgress: func(p Progress) {
		if p.Stage == StageUploading && p.BytesSent > 0 {
			cancelled.Store(true)
			cancel()
		}
	}})
	_ = wantCode(t, err, KindAbort, "aborted")
	if n := src.late.Load(); n != 0 {
		t.Errorf("%d source reads after the cancel", n)
	}
	if n := r.api.count("r2"); n != 1 {
		t.Errorf("%d direct PUTs, want the one before the cancel", n)
	}
}
