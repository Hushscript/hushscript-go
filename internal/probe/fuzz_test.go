package probe_test

import (
	"os"
	"testing"

	"github.com/Hushscript/hushscript-sdk-go/internal/probe"
)

// FuzzDuration feeds arbitrary bytes to every probe. The contract is an error or a finite positive
// duration, never a panic and never a loop. The seeds are committed, so the fuzz job needs no
// conformance checkout.
func FuzzDuration(f *testing.F) {
	truncated, err := os.ReadFile("testdata/truncated.flac")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(truncated)
	f.Add(pcmWAV(16000, make([]byte, 64)))
	f.Add(flacInfo(4608, 4608, 100, 400, 16000, 1, 320000))
	f.Add(join(ftyp(), mp4Box("moov", mvhdV0(1000, 20000))))
	f.Add(join(ftyp(), mp4Box("moov", mvhdV1(1000, 20000))))
	f.Add(opusFile(312, 960312))
	f.Add(join(oggPage(0x02, 0, 5, 0, vorbisHead(16000)), oggPage(0x04, 320000, 5, 1, make([]byte, 50))))
	f.Add(join(id3Tag(10, true), xingFrame(1, 100), repeat(mpeg2Frame(1), 3)))
	f.Add(repeat(mpeg1Frame(1), 4))
	f.Fuzz(func(t *testing.T, data []byte) {
		formats := []probe.Format{probe.Sniff(data), probe.FormatMP3, probe.FormatISOBMFF, probe.FormatWAV, probe.FormatFLAC, probe.FormatOgg}
		for _, format := range formats {
			checkContract(t, format.String(), data)
		}
	})
}
