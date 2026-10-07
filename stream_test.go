package hushscript

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// oneWay hides everything but Read, so the SDK sees a stream that cannot seek.
type oneWay struct{ io.Reader }

func (r *rig) transcribeStream(reader io.Reader, size int64, opts *TranscribeOptions) (*Transcript, error) {
	return r.client.TranscribeStream(context.Background(), oneWay{reader}, size, opts)
}

func TestAForwardOnlyStreamIsUploadedInOrderOnePartAtATimeWithTheFilesBytes(t *testing.T) {
	r := newRig(t)
	got, err := r.transcribeStream(bytes.NewReader(r.wav), int64(len(r.wav)), &TranscribeOptions{DurationSeconds: 20, Concurrency: 4})
	if err != nil || got.ID != "tr_1" {
		t.Fatalf("TranscribeStream = %v, %v", got, err)
	}
	var order []int
	for _, s := range r.api.requests("r2") {
		order = append(order, s.n)
	}
	if len(order) != 4 || order[0] != 1 || order[1] != 2 || order[2] != 3 || order[3] != 4 {
		t.Errorf("parts went %v, want 1 2 3 4", order)
	}
	if peak := r.api.r2Peak.Load(); peak != 1 {
		t.Errorf("%d parts in flight at once", peak)
	}
	want := partHashes(r.wav, testPartSize)
	for n, hash := range want {
		if r.api.parts[n].hash != hash {
			t.Errorf("part %d holds other bytes than the stream's", n)
		}
	}
	if r.api.count("complete") != 1 {
		t.Errorf("completes = %d", r.api.count("complete"))
	}
}

func TestAStreamThatReturnsShortReadsIsNotTakenForEnded(t *testing.T) {
	r := newRig(t)
	r.api.partSize = 1 << 20
	_, err := r.transcribeStream(iotest.OneByteReader(bytes.NewReader(r.wav)), int64(len(r.wav)), &TranscribeOptions{DurationSeconds: 20})
	if err != nil {
		t.Fatalf("TranscribeStream: %v", err)
	}
	if r.api.count("r2") != 1 || r.api.parts[1].hash != partHashes(r.wav, 1<<20)[1] {
		t.Errorf("parts %d, r2 requests %d", len(r.api.parts), r.api.count("r2"))
	}
}

func TestAStreamOfAnotherSizeThanDeclaredIsAStreamSizeMismatchBeforeComplete(t *testing.T) {
	for name, declared := range map[string]int64{"fewer": 640_044 + 100_000, "more": 640_044 - 100_000} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			if int64(len(r.wav)) != 640_044 {
				t.Fatalf("test file is %d bytes", len(r.wav))
			}
			_, err := r.transcribeStream(bytes.NewReader(r.wav), declared, &TranscribeOptions{DurationSeconds: 20})
			e := wantCode(t, err, KindUpload, "stream_size_mismatch")
			if r.api.count("complete") != 0 || e.IdempotencyKey == "" || e.ResumeToken == "" || !strings.Contains(e.Hint, "Pass the exact size in bytes") {
				t.Errorf("completes %d, error %+v", r.api.count("complete"), e)
			}
		})
	}
}

func TestAStreamThatHoldsOneByteMoreThanItsSizeNeverCompletes(t *testing.T) {
	r := newRig(t)
	_, err := r.transcribeStream(bytes.NewReader(append(bytes.Clone(r.wav), 0)), int64(len(r.wav)), &TranscribeOptions{DurationSeconds: 20})
	_ = wantCode(t, err, KindUpload, "stream_size_mismatch")
	if r.api.count("complete") != 0 {
		t.Error("completed an upload whose stream had bytes past its size")
	}
}

func TestAStreamNeedsASizeAndADurationBeforeAnyRequest(t *testing.T) {
	r := newRig(t)
	size := int64(len(r.wav))
	for _, declared := range []int64{0, -1} {
		_, err := r.transcribeStream(bytes.NewReader(r.wav), declared, &TranscribeOptions{DurationSeconds: 20})
		_ = wantCode(t, err, KindPreflight, "invalid_option")
	}
	_, err := r.transcribeStream(bytes.NewReader(r.wav), size, nil)
	_ = wantCode(t, err, KindPreflight, "invalid_option")
	_, err = r.transcribeStream(bytes.NewReader(r.wav), size, &TranscribeOptions{Concurrency: 2})
	_ = wantCode(t, err, KindPreflight, "invalid_option")
	_, err = r.client.TranscribeStream(context.Background(), nil, size, &TranscribeOptions{DurationSeconds: 20})
	_ = wantCode(t, err, KindPreflight, "invalid_option")
	if r.api.total() != 0 {
		t.Errorf("%d requests", r.api.total())
	}
}

func TestAStreamThatIsNotAudioIsRefusedByItsHeadWithNoCreate(t *testing.T) {
	r := newRig(t)
	junk := append([]byte{0x1a, 0x45, 0xdf, 0xa3}, make([]byte, 5000)...)
	_, err := r.transcribeStream(bytes.NewReader(junk), int64(len(junk)), &TranscribeOptions{DurationSeconds: 20})
	_ = wantCode(t, err, KindUnsupportedMedia, "unsupported_media")
	if r.api.count("create") != 0 {
		t.Error("a hold was placed for a file that is not audio")
	}
}

func TestAStreamReadErrorIsAnUploadErrorWithTheKey(t *testing.T) {
	r := newRig(t)
	broken := io.ErrClosedPipe
	stream := io.MultiReader(bytes.NewReader(r.wav[:200_000]), iotest.ErrReader(broken))
	_, err := r.transcribeStream(stream, int64(len(r.wav)), &TranscribeOptions{DurationSeconds: 20})
	e := wantCode(t, err, KindUpload, "source_read_failed")
	if e.IdempotencyKey == "" || e.ResumeToken == "" || r.api.count("complete") != 0 {
		t.Errorf("error %+v", e)
	}
}

func TestAStreamIsOnlyReadFromWhereItStands(t *testing.T) {
	src := newStreamSource(bytes.NewReader(make([]byte, 100)), 100)
	buf := make([]byte, 10)
	if err := src.readPart(buf, 0, redactor{}); err != nil {
		t.Fatal(err)
	}
	_ = wantCode(t, src.readPart(buf, 50, redactor{}), KindUpload, "stream_not_seekable")
	_ = wantCode(t, src.readPart(buf, 0, redactor{}), KindUpload, "stream_not_seekable")
}

func TestAPartInsideTheBufferedHeadIsServedFromIt(t *testing.T) {
	data := make([]byte, 10_000)
	for i := range data {
		data[i] = byte(i)
	}
	src := newStreamSource(bytes.NewReader(data), int64(len(data)))
	if _, err := src.stream.takeHead(src.size); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for off := int64(0); off < src.size; off += 3000 {
		buf := make([]byte, min(3000, src.size-off))
		if err := src.readPart(buf, off, redactor{}); err != nil {
			t.Fatalf("part at %d: %v", off, err)
		}
		got = append(got, buf...)
	}
	if !bytes.Equal(got, data) {
		t.Error("the parts do not add up to the stream")
	}
	if err := src.checkNoMoreBytes(redactor{}); err != nil {
		t.Errorf("checkNoMoreBytes = %v", err)
	}
}

// stalledUpload leaves a job that is still uploading and returns its resume token.
func stalledUpload(t *testing.T, r *rig) string {
	t.Helper()
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
	_, err := r.client.TranscribeStream(cancelled, oneWay{bytes.NewReader(r.wav)}, int64(len(r.wav)), &TranscribeOptions{
		DurationSeconds: 20, OnResumeToken: func(s string) { token = s },
	})
	_ = wantCode(t, err, KindAbort, "aborted")
	r.api.setHook(nil)
	return token
}

func TestResumeTakesAForwardOnlyStreamWithTheSizeFromTheToken(t *testing.T) {
	r := newRig(t)
	token := stalledUpload(t, r)
	got, err := r.client.Resume(context.Background(), token, &ResumeOptions{Stream: oneWay{bytes.NewReader(r.wav)}})
	if err != nil || got.ID != "tr_1" {
		t.Fatalf("Resume with the stream = %v, %v", got, err)
	}
	creates := r.api.requests("create")
	if len(creates) != 2 || len(r.api.jobs) != 1 || r.api.count("complete") != 1 {
		t.Errorf("creates %d, jobs %d, completes %d", len(creates), len(r.api.jobs), r.api.count("complete"))
	}
	for n, hash := range partHashes(r.wav, testPartSize) {
		if r.api.parts[n].hash != hash {
			t.Errorf("part %d holds other bytes than the stream's", n)
		}
	}
}

func TestResumeRefusesAForwardOnlyStreamOfAnotherSizeOrNotAtTheStart(t *testing.T) {
	cases := map[string]func(wav []byte) io.Reader{
		"longer":     func(wav []byte) io.Reader { return bytes.NewReader(append(bytes.Clone(wav), 1)) },
		"shorter":    func(wav []byte) io.Reader { return bytes.NewReader(wav[:len(wav)-1]) },
		"mid-stream": func(wav []byte) io.Reader { return bytes.NewReader(wav[1000:]) },
	}
	for name, stream := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			token := stalledUpload(t, r)
			_, err := r.client.Resume(context.Background(), token, &ResumeOptions{Stream: oneWay{stream(r.wav)}})
			_ = wantCode(t, err, KindUpload, "stream_size_mismatch")
			if r.api.count("complete") != 0 {
				t.Error("completed an upload from a stream that did not match")
			}
		})
	}
}

func TestResumeOfAFinishedJobNeverReadsTheStream(t *testing.T) {
	r := newRig(t)
	token := timedOut(t, r)
	r.api.mu.Lock()
	r.api.jobStates, r.api.jobReads = []string{"done"}, 0
	r.api.mu.Unlock()
	reads := &countingReader{}
	if _, err := r.client.Resume(context.Background(), token, &ResumeOptions{Stream: reads}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if reads.calls != 0 {
		t.Errorf("the stream was read %d times", reads.calls)
	}
}

type countingReader struct{ calls int }

func (c *countingReader) Read([]byte) (int, error) { c.calls++; return 0, io.EOF }

func TestResumeTakesOnlyOneOfPathReaderAndStream(t *testing.T) {
	r := newRig(t)
	token := timedOut(t, r)
	before := r.api.total()
	_, err := r.client.Resume(context.Background(), token, &ResumeOptions{Reader: bytes.NewReader(r.wav), Size: int64(len(r.wav)), Stream: bytes.NewReader(r.wav)})
	_ = wantCode(t, err, KindPreflight, "invalid_option")
	_, err = r.client.Resume(context.Background(), token, &ResumeOptions{Path: "x.wav", Stream: bytes.NewReader(r.wav)})
	_ = wantCode(t, err, KindPreflight, "invalid_option")
	if r.api.total() != before {
		t.Errorf("%d requests were sent", r.api.total()-before)
	}
}
