package probe_test

import (
	"bytes"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hushscript/hushscript-sdk-go/internal/probe"
)

func TestOpusIsTheGranuleOverTheRateWithNoRoundingDrift(t *testing.T) {
	const preSkip = 312
	drifted := 0
	for granule := uint64(1000); granule < 3000; granule++ {
		old := float64(granule-preSkip)/48000 + float64(preSkip)/48000
		want := float64(granule) / 48000
		if old != want {
			drifted++
		}
		got, err := duration(opusFile(preSkip, granule), probe.FormatOgg)
		if err != nil {
			t.Fatalf("granule %d: %v", granule, err)
		}
		if got != want {
			t.Fatalf("granule %d: %v s, want exactly %v s", granule, got, want)
		}
	}
	if drifted == 0 {
		t.Fatal("no granule in the sweep drifts under (g-p)/48000 + p/48000, so this test proves nothing")
	}
}

func TestAnOggWhoseLastPageIsDamagedIsRefusedNotReadFromAnEarlierPage(t *testing.T) {
	header := oggPage(0x02, 0, 5, 0, vorbisHead(16000))
	middle := oggPage(0, 160000, 5, 1, make([]byte, 50))
	last := oggPage(0x04, 320000, 5, 2, make([]byte, 50))
	badCRC := bytes.Clone(last)
	badCRC[len(badCRC)-3] ^= 0xFF
	for name, data := range map[string][]byte{
		"bad checksum on the last page": join(header, middle, badCRC),
		"last page cut short":           join(header, middle, last[:len(last)-4]),
		"garbage after the last page":   join(header, middle, last, []byte("not a page at all")),
		"bad opus checksum":             join(oggPage(0x02, 0, 7, 0, opusHead(312)), oggPage(0, 480000, 7, 1, make([]byte, 40)), badOpus()),
	} {
		wantUnreadable(t, name, data, probe.FormatOgg)
	}
	wantSeconds(t, "an intact file still reads", join(header, middle, last), probe.FormatOgg, 20)
}

func badOpus() []byte {
	page := oggPage(0x04, 960000, 7, 2, make([]byte, 40))
	page[len(page)-1] ^= 0x55
	return page
}

// cutOffReader serves zeros and frames from memory up to limit, then fails every read at or past it.
type cutOffReader struct {
	head  []byte
	limit int64
	calls atomic.Int64
}

func (r *cutOffReader) ReadAt(p []byte, off int64) (int, error) {
	r.calls.Add(1)
	if off >= r.limit {
		return 0, errors.New("disk on fire")
	}
	n := 0
	for i := range p {
		pos := off + int64(i)
		if pos >= r.limit {
			return n, errors.New("disk on fire")
		}
		if pos < int64(len(r.head)) {
			p[i] = r.head[pos]
		} else {
			p[i] = 0
		}
		n++
	}
	return n, nil
}

var _ io.ReaderAt = (*cutOffReader)(nil)

func TestAnMP3ScanStopsAtAReadErrorInsteadOfSteppingToTheEndOfAHugeFile(t *testing.T) {
	// A file declared 1 GiB long whose reads fail past the first window: the old resync stepped
	// one byte at a time to the end of the declared size.
	frames := repeat(mpeg2Frame(1), 300)
	reader := &cutOffReader{head: frames, limit: 100 << 10}
	finished := make(chan error, 1)
	started := time.Now()
	go func() {
		_, err := probe.Duration(reader, 1<<30, probe.FormatMP3)
		finished <- err
	}()
	select {
	case err := <-finished:
		if err == nil || !errors.Is(err, probe.ErrUnreadable) {
			t.Fatalf("error %v, want one wrapping ErrUnreadable", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("still scanning after %v: a read error must end the resync", time.Since(started))
	}
	if time.Since(started) > 3*time.Second {
		t.Errorf("took %v to give up on a read error", time.Since(started))
	}
	if calls := reader.calls.Load(); calls > 20 {
		t.Errorf("%d reads before giving up, want a handful", calls)
	}
}

func TestAFLACBehindAnID3v2TagIsFLAC(t *testing.T) {
	flac := join(flacInfo(4608, 4608, 100, 400, 16000, 1, 320000), make([]byte, 100000))
	tagged := join(id3Tag(200, false), flac)
	if got := probe.Sniff(tagged[:512]); got != probe.FormatFLAC {
		t.Errorf("Sniff of a tagged FLAC head = %v, want flac", got)
	}
	if got := probe.Sniff(join(id3Tag(200, true), flac)[:512]); got != probe.FormatFLAC {
		t.Errorf("Sniff with a footer = %v, want flac", got)
	}
	wantSeconds(t, "tagged FLAC", tagged, probe.FormatFLAC, 20)
	wantSeconds(t, "tagged FLAC with footer", join(id3Tag(200, true), flac), probe.FormatFLAC, 20)
	// The truncation bound counts the audio after the tag, not the tag.
	wantUnreadable(t, "tagged and truncated", join(id3Tag(90000, false), flacInfo(4608, 4608, 100, 400, 16000, 1, 320000), make([]byte, 5000)), probe.FormatFLAC)
	if got := probe.Sniff(join(id3Tag(10, false), repeat(mpeg2Frame(1), 3))); got != probe.FormatMP3 {
		t.Errorf("a tag then frames = %v, want mp3", got)
	}
	if got := probe.Sniff(id3Tag(10, false)[:10]); got != probe.FormatMP3 {
		t.Errorf("a bare tag header = %v, want mp3", got)
	}
	badSize := join([]byte("ID3\x04\x00\x00\xFF\xFF\xFF\xFF"), flac)
	if got := probe.Sniff(badSize[:64]); got != probe.FormatMP3 {
		t.Errorf("a tag with a bad size = %v, want mp3 (the server's id3v2_end refuses it too)", got)
	}
	if got := probe.ID3v2End(join(id3Tag(200, true), flac)[:64]); got != 10+200+10 {
		t.Errorf("ID3v2End = %d, want 220", got)
	}
	if got := probe.ID3v2End([]byte("fLaC")); got != 0 {
		t.Errorf("ID3v2End without a tag = %d, want 0", got)
	}
}
