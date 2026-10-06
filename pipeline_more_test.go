package hushscript

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
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
	const slack = 24 << 20
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
