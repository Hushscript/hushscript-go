package probe_test

import (
	"bytes"
	"errors"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/Hushscript/hushscript-go/internal/probe"
)

func duration(data []byte, f probe.Format) (float64, error) {
	return probe.Duration(bytes.NewReader(data), int64(len(data)), f)
}

func wantSeconds(t *testing.T, name string, data []byte, f probe.Format, want float64) {
	t.Helper()
	got, err := duration(data, f)
	if err != nil {
		t.Errorf("%s: %v", name, err)
		return
	}
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("%s: %v s, want %v", name, got, want)
	}
}

func wantUnreadable(t *testing.T, name string, data []byte, f probe.Format) {
	t.Helper()
	got, err := duration(data, f)
	if err == nil {
		t.Errorf("%s: probed %v s, want an error", name, got)
		return
	}
	if !errors.Is(err, probe.ErrUnreadable) {
		t.Errorf("%s: error %v does not wrap ErrUnreadable", name, err)
	}
}

func TestSniffsByMagicBytesAndNotByName(t *testing.T) {
	for name, tc := range map[string]struct {
		head []byte
		want probe.Format
	}{
		"wav":           {[]byte("RIFF\x00\x00\x00\x00WAVEfmt "), probe.FormatWAV},
		"rf64":          {[]byte("RF64\x00\x00\x00\x00WAVEds64"), probe.FormatWAV},
		"bw64":          {[]byte("BW64\x00\x00\x00\x00WAVEds64"), probe.FormatWAV},
		"avi":           {[]byte("RIFF\x00\x00\x00\x00AVI LIST"), probe.FormatUnknown},
		"flac":          {[]byte("fLaC\x00"), probe.FormatFLAC},
		"ogg":           {[]byte("OggS\x00\x02"), probe.FormatOgg},
		"mp4":           {[]byte("\x00\x00\x00\x18ftypmp42"), probe.FormatISOBMFF},
		"id3":           {[]byte("ID3\x04\x00"), probe.FormatMP3},
		"frame sync":    {[]byte{0xFF, 0xFB, 0x90, 0x00}, probe.FormatMP3},
		"adts is not":   {[]byte{0xFF, 0xF1, 0x50, 0x80}, probe.FormatUnknown},
		"webm":          {[]byte{0x1A, 0x45, 0xDF, 0xA3, 0x9F}, probe.FormatUnknown},
		"empty":         {nil, probe.FormatUnknown},
		"one byte":      {[]byte{0xFF}, probe.FormatUnknown},
		"short riff":    {[]byte("RIFF"), probe.FormatUnknown},
		"short ftyp":    {[]byte("\x00\x00\x00\x18ftyp"), probe.FormatISOBMFF},
		"text":          {[]byte("hello world, not audio"), probe.FormatUnknown},
		"truncated mp4": {[]byte("\x00\x00\x00\x18fty"), probe.FormatUnknown},
	} {
		if got := probe.Sniff(tc.head); got != tc.want {
			t.Errorf("%s: Sniff = %v, want %v", name, got, tc.want)
		}
	}
}

func TestNamesEveryFormat(t *testing.T) {
	for f, want := range map[probe.Format]string{
		probe.FormatUnknown: "unknown", probe.FormatMP3: "mp3", probe.FormatISOBMFF: "iso-bmff",
		probe.FormatWAV: "wav", probe.FormatFLAC: "flac", probe.FormatOgg: "ogg", probe.Format(99): "unknown",
	} {
		if got := f.String(); got != want {
			t.Errorf("Format(%d).String() = %q, want %q", int(f), got, want)
		}
	}
}

type failingReader struct{ err error }

func (r failingReader) ReadAt([]byte, int64) (int, error) { return 0, r.err }

type shortReader struct{ data []byte }

func (r shortReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(p, r.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestRefusesInputsThatCannotBeProbed(t *testing.T) {
	data := pcmWAV(16000, make([]byte, 16000))
	for _, f := range []probe.Format{probe.FormatMP3, probe.FormatISOBMFF, probe.FormatWAV, probe.FormatFLAC, probe.FormatOgg, probe.FormatUnknown, probe.Format(99)} {
		if _, err := probe.Duration(nil, 100, f); !errors.Is(err, probe.ErrUnreadable) {
			t.Errorf("%v: nil reader gave %v", f, err)
		}
		for _, size := range []int64{0, -1, math.MinInt64} {
			if _, err := probe.Duration(bytes.NewReader(data), size, f); !errors.Is(err, probe.ErrUnreadable) {
				t.Errorf("%v: size %d gave %v", f, size, err)
			}
		}
		if _, err := probe.Duration(failingReader{errors.New("disk on fire")}, 1<<20, f); !errors.Is(err, probe.ErrUnreadable) {
			t.Errorf("%v: a failing reader gave %v", f, err)
		}
		// A size larger than the reader holds must be an error, not a hang or a panic.
		if _, err := probe.Duration(shortReader{data}, int64(len(data))+1<<30, f); err == nil && f == probe.FormatUnknown {
			t.Errorf("%v: a short reader was accepted", f)
		}
	}
	if _, err := probe.Duration(failingReader{errors.New("disk on fire")}, 1<<20, probe.FormatWAV); err == nil || !strings.Contains(err.Error(), "disk on fire") {
		t.Errorf("a read failure lost its cause: %v", err)
	}
}

func TestReadsWAVByDataSizeOverByteRate(t *testing.T) {
	wantSeconds(t, "plain", pcmWAV(32000, make([]byte, 32000)), probe.FormatWAV, 2)
	wantSeconds(t, "odd chunk before data is padded", pcmWAV(16000, make([]byte, 16000), wavChunk("LIST", 3, []byte{1, 2, 3})), probe.FormatWAV, 1)
	wantSeconds(t, "trailing chunks stay out of the audio", join(pcmWAV(16000, make([]byte, 16000)), wavChunk("LIST", 100, make([]byte, 100))), probe.FormatWAV, 1)
	wantSeconds(t, "streaming sentinel", pcmWAV(0xFFFFFFFF, make([]byte, 8000)), probe.FormatWAV, 0.5)
	wantSeconds(t, "zero size", pcmWAV(0, make([]byte, 8000)), probe.FormatWAV, 0.5)
	wantSeconds(t, "size past the file", pcmWAV(1<<30, make([]byte, 8000)), probe.FormatWAV, 0.5)
	extensible := join(le16(0xFFFE), le16(1), le32(8000), le32(16000), le16(2), le16(16), le16(22), le16(16), le32(4), le16(1), make([]byte, 14))
	wantSeconds(t, "extensible PCM", join([]byte("RIFF"), le32(0), []byte("WAVE"), wavChunk("fmt ", sizeOf(extensible), extensible), wavChunk("data", 16000, make([]byte, 16000))), probe.FormatWAV, 1)
	rf64 := join([]byte("RF64"), le32(0xFFFFFFFF), []byte("WAVE"), wavChunk("ds64", 28, join(le64(0), le64(16000), le64(8000), le32(0))), wavFmt(1, 1, 8000, 2), wavChunk("data", 0xFFFFFFFF, make([]byte, 24000)))
	wantSeconds(t, "rf64 reads ds64", rf64, probe.FormatWAV, 1)
	rf64NoDS64 := join([]byte("RF64"), le32(0xFFFFFFFF), []byte("WAVE"), wavFmt(1, 1, 8000, 2), wavChunk("data", 0xFFFFFFFF, make([]byte, 24000)))
	wantSeconds(t, "rf64 without ds64 uses the bytes present", rf64NoDS64, probe.FormatWAV, 1.5)
	shortDS64 := join([]byte("RF64"), le32(0xFFFFFFFF), []byte("WAVE"), wavChunk("ds64", 4, make([]byte, 4)), wavFmt(1, 1, 8000, 2), wavChunk("data", 0xFFFFFFFF, make([]byte, 16000)))
	wantSeconds(t, "short ds64", shortDS64, probe.FormatWAV, 1)
}

func TestRefusesWAVThatCannotBeTrusted(t *testing.T) {
	good := pcmWAV(16000, make([]byte, 16000))
	notWave := bytes.Clone(good)
	copy(notWave[8:], "AVI ")
	manyChunks := join([]byte("RIFF"), le32(0), []byte("WAVE"), bytes.Repeat(wavChunk("junk", 0, nil), 5000), wavFmt(1, 1, 8000, 2), wavChunk("data", 8, make([]byte, 8)))
	for name, data := range map[string][]byte{
		"not a wave":           notWave,
		"truncated header":     good[:10],
		"no data chunk":        join([]byte("RIFF"), le32(0), []byte("WAVE"), wavFmt(1, 1, 8000, 2)),
		"data before fmt":      join([]byte("RIFF"), le32(0), []byte("WAVE"), wavChunk("data", 8, make([]byte, 8)), wavFmt(1, 1, 8000, 2)),
		"empty data":           pcmWAV(0, nil),
		"compressed codec":     join([]byte("RIFF"), le32(0), []byte("WAVE"), wavFmt(2, 1, 8000, 2), wavChunk("data", 8, make([]byte, 8))),
		"zero sample rate":     join([]byte("RIFF"), le32(0), []byte("WAVE"), wavFmt(1, 1, 0, 2), wavChunk("data", 8, make([]byte, 8))),
		"huge sample rate":     join([]byte("RIFF"), le32(0), []byte("WAVE"), wavFmt(1, 1, 5000000, 2), wavChunk("data", 8, make([]byte, 8))),
		"zero channels":        join([]byte("RIFF"), le32(0), []byte("WAVE"), wavFmt(1, 0, 8000, 0), wavChunk("data", 8, make([]byte, 8))),
		"block size lies":      join([]byte("RIFF"), le32(0), []byte("WAVE"), wavFmt(1, 1, 8000, 1), wavChunk("data", 8, make([]byte, 8))),
		"short fmt chunk":      join([]byte("RIFF"), le32(0), []byte("WAVE"), wavChunk("fmt ", 8, make([]byte, 8)), wavChunk("data", 8, make([]byte, 8))),
		"chunk size past file": join([]byte("RIFF"), le32(0), []byte("WAVE"), wavChunk("junk", 0xFFFFFFF0, nil)),
		"endless chunks":       manyChunks,
	} {
		wantUnreadable(t, name, data, probe.FormatWAV)
	}
}

func TestReadsFLACStreamInfo(t *testing.T) {
	audio := make([]byte, 100000)
	wantSeconds(t, "20 s", join(flacInfo(4608, 4608, 100, 400, 16000, 1, 16, 320000), audio), probe.FormatFLAC, 20)
	wantSeconds(t, "unknown frame sizes skip the truncation bound", flacInfo(4608, 4608, 0, 0, 16000, 1, 16, 320000), probe.FormatFLAC, 20)
	wantSeconds(t, "36 bit sample count", flacInfo(0, 0, 0, 0, 44100, 2, 16, 1<<35), probe.FormatFLAC, float64(uint64(1)<<35)/44100)
}

func TestRefusesFLACThatCannotBeTrusted(t *testing.T) {
	good := join(flacInfo(4608, 4608, 100, 400, 16000, 1, 16, 320000), make([]byte, 100000))
	notFirst := bytes.Clone(good)
	notFirst[4] = 0x84
	short := bytes.Clone(good)
	short[7] = 10
	for name, data := range map[string][]byte{
		"empty after the magic":   []byte("fLaC"),
		"cut inside STREAMINFO":   good[:30],
		"STREAMINFO not first":    notFirst,
		"STREAMINFO too short":    short,
		"zero sample rate":        flacInfo(4608, 4608, 100, 400, 0, 1, 16, 320000),
		"zero samples":            flacInfo(4608, 4608, 100, 400, 16000, 1, 16, 0),
		"fewer bytes than frames": join(flacInfo(4608, 4608, 100, 400, 16000, 1, 16, 320000), make([]byte, 5000)),
	} {
		wantUnreadable(t, name, data, probe.FormatFLAC)
	}
}

func TestReadsTheMovieHeaderOfISOBMFF(t *testing.T) {
	mdat := mp4Box("mdat", make([]byte, 50))
	wantSeconds(t, "moov after mdat", join(ftyp(), mdat, mp4Box("moov", mp4Box("trak"), mvhdV0(1000, 20500))), probe.FormatISOBMFF, 20.5)
	wantSeconds(t, "moov before mdat", join(ftyp(), mp4Box("moov", mvhdV0(44100, 441000)), mdat), probe.FormatISOBMFF, 10)
	wantSeconds(t, "version 1", join(ftyp(), mp4Box("moov", mvhdV1(1000, 1<<33))), probe.FormatISOBMFF, float64(uint64(1)<<33)/1000)
	large := join(be32(1), []byte("free"), be64(16+5), make([]byte, 5))
	wantSeconds(t, "64-bit box size", join(ftyp(), large, mp4Box("moov", mvhdV0(1, 7))), probe.FormatISOBMFF, 7)
	toEnd := join(be32(0), []byte("moov"), mvhdV0(1, 9))
	wantSeconds(t, "box running to the end", join(ftyp(), toEnd), probe.FormatISOBMFF, 9)
	wantSeconds(t, "mvhd running to the end", join(ftyp(), mp4Box("moov", join(be32(0), []byte("mvhd"), []byte{0, 0, 0, 0}, be32(0), be32(0), be32(10), be32(50), make([]byte, 80)))), probe.FormatISOBMFF, 5)
	wantSeconds(t, "mdat running past the end is never entered", join(ftyp(), mp4Box("moov", mvhdV0(1, 3))), probe.FormatISOBMFF, 3)
}

func TestRefusesISOBMFFThatCannotBeTrusted(t *testing.T) {
	tiny := bytes.Repeat(mp4Box("free"), 20000)
	for name, data := range map[string][]byte{
		"no moov":                 join(ftyp(), mp4Box("mdat", make([]byte, 20))),
		"no mvhd":                 join(ftyp(), mp4Box("moov", mp4Box("trak"))),
		"timescale zero":          join(ftyp(), mp4Box("moov", mvhdV0(0, 100))),
		"timescale zero v1":       join(ftyp(), mp4Box("moov", mvhdV1(0, 100))),
		"unknown duration":        join(ftyp(), mp4Box("moov", mvhdV0(1000, 0xFFFFFFFF))),
		"unknown duration v1":     join(ftyp(), mp4Box("moov", mvhdV1(1000, math.MaxUint64))),
		"zero duration":           join(ftyp(), mp4Box("moov", mvhdV0(1000, 0))),
		"fragmented, no duration": join(ftyp(), mp4Box("moov", mvhdV0(1000, 0), mp4Box("mvex"))),
		"unknown mvhd version":    join(ftyp(), mp4Box("moov", mp4Box("mvhd", []byte{7, 0, 0, 0}, make([]byte, 80)))),
		"mvhd too short":          join(ftyp(), mp4Box("moov", mp4Box("mvhd", []byte{0, 0, 0, 0}, be32(0)))),
		"mvhd v1 too short":       join(ftyp(), mp4Box("moov", mp4Box("mvhd", []byte{1, 0, 0, 0}, be32(0), be32(0)))),
		"mvhd without a version":  join(ftyp(), mp4Box("moov", mp4Box("mvhd"))),
		"box size under header":   join(ftyp(), be32(3), []byte("moov")),
		"box size past the file":  join(ftyp(), be32(1<<30), []byte("moov")),
		"huge 64-bit box":         join(ftyp(), be32(1), []byte("moov"), be64(math.MaxUint64)),
		"64-bit box cut short":    join(ftyp(), be32(1), []byte("moov")),
		"child past its parent":   join(ftyp(), mp4Box("moov", join(be32(900), []byte("mvhd")))),
		"endless tiny boxes":      join(ftyp(), tiny),
		"cut header":              ftyp()[:6],
	} {
		wantUnreadable(t, name, data, probe.FormatISOBMFF)
	}
}

func TestReadsOggByTheLastGranule(t *testing.T) {
	wantSeconds(t, "opus adds the pre-skip back to the decoded length", opusFile(312, 960312), probe.FormatOgg, 960312.0/48000)
	// The pre-skip comes from OpusHead: the same granule is valid with a small one and refused with a large one.
	wantSeconds(t, "small pre-skip", opusFile(100, 3000), probe.FormatOgg, 3000.0/48000)
	wantUnreadable(t, "granule inside the pre-skip", opusFile(3840, 3000), probe.FormatOgg)
	wantUnreadable(t, "granule zero", opusFile(0, 0), probe.FormatOgg)
	vorbis := join(oggPage(0x02, 0, 5, 0, vorbisHead(16000)), oggPage(0, 80000, 5, 1, make([]byte, 50)), oggPage(0x04, 320000, 5, 2, make([]byte, 50)))
	wantSeconds(t, "vorbis", vorbis, probe.FormatOgg, 20)
	noPosition := join(vorbis, oggPage(0, math.MaxUint64, 5, 3, make([]byte, 50)))
	wantSeconds(t, "a last page without a position is skipped", noPosition, probe.FormatOgg, 20)
	otherStream := join(oggPage(0x02, 0, 5, 0, vorbisHead(16000)), oggPage(0, 160000, 5, 1, make([]byte, 50)), oggPage(0, 999999, 6, 1, make([]byte, 50)))
	wantSeconds(t, "pages of another serial are ignored", otherStream, probe.FormatOgg, 10)
	falsePage := join([]byte("OggS\x00\x00"), make([]byte, 30))
	wantSeconds(t, "an OggS inside a packet is skipped by its checksum", join(oggPage(0x02, 0, 5, 0, vorbisHead(16000)), oggPage(0x04, 160000, 5, 1, falsePage)), probe.FormatOgg, 10)
	big := join(oggPage(0x02, 0, 5, 0, vorbisHead(16000)), bytes.Repeat(oggPage(0, 160000, 5, 1, make([]byte, 2000)), 100), oggPage(0x04, 320000, 5, 2, make([]byte, 50)))
	wantSeconds(t, "only the last 64 KiB is read", big, probe.FormatOgg, 20)
}

func TestRefusesOggThatCannotBeTrusted(t *testing.T) {
	vorbis := join(oggPage(0x02, 0, 5, 0, vorbisHead(16000)), oggPage(0x04, 320000, 5, 1, make([]byte, 50)))
	corrupt := bytes.Clone(vorbis)
	corrupt[len(corrupt)-3] ^= 0xFF
	versionOne := bytes.Clone(vorbis)
	versionOne[4] = 1
	chained := join(vorbis, oggPage(0x02, 0, 9, 0, vorbisHead(16000)), oggPage(0x04, 1000, 9, 1, make([]byte, 50)))
	manyFalse := join(oggPage(0x02, 0, 5, 0, vorbisHead(16000)), bytes.Repeat([]byte("OggS\x00\x00\x01\x02\x03"), 4000))
	for name, data := range map[string][]byte{
		"cut first page":       vorbis[:20],
		"first page not BOS":   join(oggPage(0, 0, 5, 0, vorbisHead(16000)), oggPage(0x04, 320000, 5, 1, nil)),
		"other codec":          join(oggPage(0x02, 0, 5, 0, []byte("\x7fFLAC and some more bytes")), oggPage(0x04, 320000, 5, 1, nil)),
		"zero vorbis rate":     join(oggPage(0x02, 0, 5, 0, vorbisHead(0)), oggPage(0x04, 320000, 5, 1, nil)),
		"short OpusHead":       join(oggPage(0x02, 0, 5, 0, []byte("OpusHead\x01\x01")), oggPage(0x04, 320000, 5, 1, nil)),
		"short vorbis header":  join(oggPage(0x02, 0, 5, 0, []byte("\x01vorbis\x00")), oggPage(0x04, 320000, 5, 1, nil)),
		"only one page":        oggPage(0x02, 0, 5, 0, vorbisHead(16000)),
		"corrupt last page":    corrupt,
		"unknown page version": versionOne,
		"a second stream":      chained,
		"damaged tail":         manyFalse,
		"page past the file":   vorbis[:len(vorbis)-4],
	} {
		wantUnreadable(t, name, data, probe.FormatOgg)
	}
}

func TestReadsMP3FrameCountsFromTheFirstFrame(t *testing.T) {
	const frameSeconds = 576.0 / 16000
	wantSeconds(t, "Info header", join(xingFrame(1, 100), repeat(mpeg2Frame(1), 3)), probe.FormatMP3, 100*frameSeconds)
	wantSeconds(t, "ID3 tag first", join(id3Tag(20, false), xingFrame(3, 50), repeat(mpeg2Frame(1), 3)), probe.FormatMP3, 50*frameSeconds)
	wantSeconds(t, "ID3 tag with a footer", join(id3Tag(20, true), xingFrame(1, 50), repeat(mpeg2Frame(1), 3)), probe.FormatMP3, 50*frameSeconds)
	wantSeconds(t, "two ID3 tags", join(id3Tag(5, false), id3Tag(7, false), xingFrame(1, 50), repeat(mpeg2Frame(1), 3)), probe.FormatMP3, 50*frameSeconds)
	wantSeconds(t, "padding between the tag and the audio", join(id3Tag(5, false), make([]byte, 300), xingFrame(1, 50), repeat(mpeg2Frame(1), 3)), probe.FormatMP3, 50*frameSeconds)
	noFrames := join(xingFrame(0, 100), repeat(mpeg2Frame(1), 9))
	wantSeconds(t, "no frames flag falls back to the scan", noFrames, probe.FormatMP3, 10*frameSeconds)
	zero := join(xingFrame(1, 0), repeat(mpeg2Frame(1), 9))
	wantSeconds(t, "a zero count falls back to the scan", zero, probe.FormatMP3, 10*frameSeconds)

	vbri := mpeg2Frame(0)
	copy(vbri[4+32:], join([]byte("VBRI"), be16(1), be16(0), be16(0), be32(0), be32(200)))
	wantSeconds(t, "VBRI", join(vbri, repeat(mpeg2Frame(1), 3)), probe.FormatMP3, 200*frameSeconds)
	vbriZero := mpeg2Frame(0)
	copy(vbriZero[4+32:], join([]byte("VBRI"), be16(1), be16(0), be16(0), be32(0), be32(0)))
	wantSeconds(t, "VBRI without a count falls back to the scan", join(vbriZero, repeat(mpeg2Frame(1), 3)), probe.FormatMP3, 4*frameSeconds)

	crcFrame := join(mp3Header(2, 8, 2, 0, true, false), make([]byte, 284))
	copy(crcFrame[4+2+9:], join([]byte("Xing"), be32(1), be32(30)))
	wantSeconds(t, "a protected frame has a two-byte CRC before the side info", join(crcFrame, repeat(mpeg2Frame(1), 3)), probe.FormatMP3, 30*frameSeconds)

	mpeg1 := mpeg1Frame(0)
	copy(mpeg1[4+32:], join([]byte("Xing"), be32(1), be32(100)))
	wantSeconds(t, "MPEG-1 stereo", join(mpeg1, repeat(mpeg1Frame(0), 3)), probe.FormatMP3, 100*1152.0/44100)
}

func TestScansEveryMP3FrameWithoutAHeader(t *testing.T) {
	const frameSeconds = 576.0 / 16000
	wantSeconds(t, "plain", repeat(mpeg2Frame(1), 20), probe.FormatMP3, 20*frameSeconds)
	wantSeconds(t, "ID3v2 first", join(id3Tag(50, false), repeat(mpeg2Frame(1), 20)), probe.FormatMP3, 20*frameSeconds)
	wantSeconds(t, "an ID3v1 tag at the end is not audio", join(repeat(mpeg2Frame(1), 20), []byte("TAG"), make([]byte, 125)), probe.FormatMP3, 20*frameSeconds)
	wantSeconds(t, "junk between frames is skipped", join(repeat(mpeg2Frame(1), 10), make([]byte, 50), repeat(mpeg2Frame(1), 10)), probe.FormatMP3, 20*frameSeconds)
	wantSeconds(t, "a cut last frame still counts", join(repeat(mpeg2Frame(1), 10), mpeg2Frame(1)[:100]), probe.FormatMP3, 11*frameSeconds)
	wantSeconds(t, "a single short frame", mpeg2Frame(1)[:200], probe.FormatMP3, frameSeconds)
	wantSeconds(t, "padded frames", repeat(mpeg1Frame(1), 10), probe.FormatMP3, 10*1152.0/44100)
	wantSeconds(t, "bitrates may change", join(repeat(mpeg2Frame(1), 5), repeat(join(mp3Header(2, 9, 2, 0, true, true), make([]byte, 360-4)), 5)), probe.FormatMP3, 10*frameSeconds)
	other := join(repeat(mpeg2Frame(1), 5), repeat(mpeg1Frame(0), 5))
	wantSeconds(t, "frames of another version are not counted", other, probe.FormatMP3, 5*frameSeconds)
	wantSeconds(t, "MPEG-2.5", repeat(join(mp3Header(0, 8, 2, 0, true, true), make([]byte, 72*64000/8000-4)), 10), probe.FormatMP3, 10*576.0/8000)
}

func TestAnID3TagWithABadSizeIsNotSkipped(t *testing.T) {
	data := join([]byte("ID3\x04\x00\x00\xFF\xFF\xFF\xFF"), repeat(mpeg2Frame(1), 3))
	wantSeconds(t, "bad syncsafe size", data, probe.FormatMP3, 3*576.0/16000)
}

func TestRefusesMP3ThatCannotBeTrusted(t *testing.T) {
	layer2 := bytes.Repeat([]byte{0xFF, 0xFD, 0x90, 0x00, 1, 1, 1, 1}, 100)
	free := bytes.Repeat(join([]byte{0xFF, 0xF3, 0x08, 0xC4}, make([]byte, 100)), 10)
	badRate := bytes.Repeat(join([]byte{0xFF, 0xF3, 0x8C, 0xC4}, make([]byte, 100)), 10)
	reserved := bytes.Repeat(join([]byte{0xFF, 0xEB, 0x88, 0xC4}, make([]byte, 100)), 10)
	for name, data := range map[string][]byte{
		"no frames":          bytes.Repeat([]byte("not an mp3 at all "), 50),
		"layer II":           layer2,
		"free format":        free,
		"reserved rate":      badRate,
		"reserved version":   reserved,
		"a tag fills it":     id3Tag(100, false),
		"tag then nothing":   join(id3Tag(10, false), []byte("xyz")),
		"only a tag header":  []byte("ID3\x04\x00\x00\x00\x00\x00\x7f"),
		"too short":          {0xFF, 0xF3},
		"lone sync":          {0xFF, 0xF3, 0x88, 0xC4, 0, 0},
		"endless zero bytes": make([]byte, 1<<20),
	} {
		got, err := duration(data, probe.FormatMP3)
		if name == "lone sync" {
			// a frame that ends the file is accepted: this documents it, and it cannot loop
			if err != nil || got <= 0 {
				t.Errorf("%s: %v, %v", name, got, err)
			}
			continue
		}
		if err == nil || !errors.Is(err, probe.ErrUnreadable) {
			t.Errorf("%s: probed %v with error %v", name, got, err)
		}
	}
}

func TestEveryPrefixOfEveryFixtureIsAnErrorOrAFiniteDuration(t *testing.T) {
	for name := range wantFormat {
		data := readMedia(t, name)
		for n := 0; n <= min(len(data), 600); n++ {
			checkContract(t, name, data[:n])
		}
		for _, n := range []int{len(data) / 4, len(data) / 2, len(data) - 1, len(data)} {
			checkContract(t, name, data[:n])
		}
	}
}

func TestEveryFixtureWithItsHeaderBytesFlippedIsAnErrorOrAFiniteDuration(t *testing.T) {
	for name := range wantFormat {
		data := readMedia(t, name)
		for i := 0; i < min(len(data), 200); i++ {
			for _, flip := range []byte{0x01, 0x80, 0xFF} {
				mutated := bytes.Clone(data[:min(len(data), 70000)])
				mutated[i] ^= flip
				checkContract(t, name, mutated)
			}
		}
	}
}

func checkContract(t *testing.T, name string, data []byte) {
	t.Helper()
	for _, f := range []probe.Format{probe.FormatMP3, probe.FormatISOBMFF, probe.FormatWAV, probe.FormatFLAC, probe.FormatOgg} {
		got, err := duration(data, f)
		if err == nil && (math.IsNaN(got) || math.IsInf(got, 0) || got <= 0) {
			t.Fatalf("%s (%d bytes) as %v: probed %v without an error", name, len(data), f, got)
		}
		if err != nil && !errors.Is(err, probe.ErrUnreadable) {
			t.Fatalf("%s (%d bytes) as %v: error %v does not wrap ErrUnreadable", name, len(data), f, err)
		}
	}
}
