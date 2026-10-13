package probe_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hushscript/hushscript-sdk-go/internal/probe"
)

// mediaDir is a hard requirement, like the hint fixture: a run without the conformance checkout
// fails, so the probes cannot go untested by leaving a variable unset.
func mediaDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("CONFORMANCE_DIR")
	if dir == "" {
		t.Fatal("CONFORMANCE_DIR must point at a checkout of hushscript-sdk-conformance (the justfile sets it)")
	}
	return filepath.Join(dir, "fixtures", "media")
}

func readMedia(t *testing.T, name string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(mediaDir(t), name)) //nolint:gosec // a fixture path from the environment
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func trueDurations(t *testing.T) map[string]float64 {
	t.Helper()
	var durations map[string]float64
	if err := json.Unmarshal(readMedia(t, "durations.json"), &durations); err != nil {
		t.Fatal(err)
	}
	if len(durations) == 0 {
		t.Fatal("durations.json is empty")
	}
	return durations
}

var wantFormat = map[string]probe.Format{
	"aac-20s.m4a":        probe.FormatISOBMFF,
	"aac-61s.m4a":        probe.FormatISOBMFF,
	"video-20s.mp4":      probe.FormatISOBMFF,
	"video-61s.mp4":      probe.FormatISOBMFF,
	"cbr-20s.mp3":        probe.FormatMP3,
	"cbr-61s.mp3":        probe.FormatMP3,
	"short-10s.mp3":      probe.FormatMP3,
	"vbr-noxing-20s.mp3": probe.FormatMP3,
	"vbr-noxing-61s.mp3": probe.FormatMP3,
	"vbr-xing-20s.mp3":   probe.FormatMP3,
	"vbr-xing-61s.mp3":   probe.FormatMP3,
	"pcm-20s.wav":        probe.FormatWAV,
	"pcm-61s.wav":        probe.FormatWAV,
	"flac-20s.flac":      probe.FormatFLAC,
	"flac-61s.flac":      probe.FormatFLAC,
	"truncated.flac":     probe.FormatFLAC,
	"opus-20s.opus":      probe.FormatOgg,
	"opus-61s.opus":      probe.FormatOgg,
	"vorbis-20s.ogg":     probe.FormatOgg,
	"vorbis-61s.ogg":     probe.FormatOgg,
	"webm-renamed.mp3":   probe.FormatUnknown,
}

func TestSniffsEveryFixtureByItsMagicBytes(t *testing.T) {
	for name, want := range wantFormat {
		head := readMedia(t, name)
		head = head[:min(len(head), 64<<10)]
		if got := probe.Sniff(head); got != want {
			t.Errorf("%s: Sniff = %v, want %v", name, got, want)
		}
	}
}

func TestProbesEveryFixtureWithinOneSecondAndNeverUnder(t *testing.T) {
	durations := trueDurations(t)
	for name, truth := range durations {
		if name == "webm-renamed.mp3" {
			continue // listed for the unsupported-media scenario; it never reaches a probe
		}
		t.Run(name, func(t *testing.T) {
			data := readMedia(t, name)
			got, err := probe.Duration(bytes.NewReader(data), int64(len(data)), probe.Sniff(data))
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: probed %.6f, true %.3f, ceil %.0f", name, got, truth, math.Ceil(got))
			if math.Ceil(got) < truth {
				t.Errorf("ceil(%v) is under the true duration %v", got, truth)
			}
			if math.Ceil(got)-truth > 1 {
				t.Errorf("ceil(%v) is more than 1 s over the true duration %v", got, truth)
			}
		})
	}
}

func TestEveryListedFixtureIsKnownToTheTests(t *testing.T) {
	for name := range trueDurations(t) {
		if _, ok := wantFormat[name]; !ok {
			t.Errorf("%s is in durations.json but not in the sniff table", name)
		}
	}
}

func TestATruncatedFlacHasNoReadableDuration(t *testing.T) {
	data := readMedia(t, "truncated.flac")
	got, err := probe.Duration(bytes.NewReader(data), int64(len(data)), probe.FormatFLAC)
	if err == nil {
		t.Fatalf("a 4 KiB cut of a 20 s FLAC probed as %v s", got)
	}
	if !errors.Is(err, probe.ErrUnreadable) {
		t.Errorf("error %v does not wrap ErrUnreadable", err)
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error %q does not say the file is truncated", err)
	}
}

func TestTheCommittedTruncatedSeedMatchesTheFixture(t *testing.T) {
	seed, err := os.ReadFile("testdata/truncated.flac")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(seed, readMedia(t, "truncated.flac")) {
		t.Error("testdata/truncated.flac differs from the conformance fixture")
	}
}
