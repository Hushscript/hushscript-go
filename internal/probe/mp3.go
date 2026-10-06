package probe

import (
	"bytes"
	"encoding/binary"
)

const (
	mp3MaxTags      = 4
	mp3FirstScan    = 64 << 10
	mp3WindowSize   = 64 << 10
	mp3HeaderLength = 4
)

var (
	mp3Bitrates = [2][15]int{
		{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}, // MPEG-1 Layer III
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},     // MPEG-2 and 2.5 Layer III
	}
	mp3SampleRates = [3][3]int{
		{44100, 48000, 32000}, // MPEG-1
		{22050, 24000, 16000}, // MPEG-2
		{11025, 12000, 8000},  // MPEG-2.5
	}
)

// mp3Frame is one parsed Layer III frame header.
type mp3Frame struct {
	version    int // index into mp3SampleRates: 0 MPEG-1, 1 MPEG-2, 2 MPEG-2.5
	sampleRate int
	samples    uint64 // per frame: 1152 for MPEG-1, 576 otherwise
	length     int64
	sideInfo   int
	protected  bool
}

func parseMP3Frame(h []byte) (mp3Frame, bool) {
	if len(h) < mp3HeaderLength || !isLayer3Sync(h[0], h[1]) {
		return mp3Frame{}, false
	}
	var f mp3Frame
	switch (h[1] >> 3) & 3 {
	case 3:
		f.version = 0
	case 2:
		f.version = 1
	case 0:
		f.version = 2
	default:
		return mp3Frame{}, false
	}
	bitrateIndex := int(h[2] >> 4)
	rateIndex := int((h[2] >> 2) & 3)
	if bitrateIndex == 0 || bitrateIndex == 15 || rateIndex == 3 {
		return mp3Frame{}, false
	}
	f.sampleRate = mp3SampleRates[f.version][rateIndex]
	mono := h[3]>>6 == 3
	padding := int64(h[2]>>1) & 1
	f.protected = h[1]&1 == 0
	if f.version == 0 {
		f.samples = 1152
		f.length = int64(144*mp3Bitrates[0][bitrateIndex]*1000/f.sampleRate) + padding
		f.sideInfo = 32
		if mono {
			f.sideInfo = 17
		}
	} else {
		f.samples = 576
		f.length = int64(72*mp3Bitrates[1][bitrateIndex]*1000/f.sampleRate) + padding
		f.sideInfo = 17
		if mono {
			f.sideInfo = 9
		}
	}
	return f, f.length >= mp3HeaderLength
}

// mp3Window serves small reads from a cached block, so a frame-by-frame walk costs one read per
// block and not one per frame. The first read error is kept and ends the walk.
type mp3Window struct {
	s    source
	buf  []byte
	base int64
	err  error
}

func (w *mp3Window) at(pos int64, n int) []byte {
	if pos >= 0 && pos >= w.base && pos+int64(n) <= w.base+int64(len(w.buf)) {
		return w.buf[pos-w.base : pos-w.base+int64(n)]
	}
	if pos < 0 || pos+int64(n) > w.s.size || w.err != nil {
		return nil
	}
	size := int(min(w.s.size-pos, mp3WindowSize))
	buf, err := w.s.read(pos, size)
	if err != nil {
		w.err = err
		return nil
	}
	w.buf, w.base = buf, pos
	return buf[:n]
}

func (w *mp3Window) frame(pos int64) (mp3Frame, bool) {
	return parseMP3Frame(w.at(pos, mp3HeaderLength))
}

// confirmed tells a real frame from header-shaped bytes inside other data: the next frame must
// parse and share its version and sample rate. A frame that reaches the end of the file, where a
// next header cannot fit, is accepted, so a short file or a cut last frame still counts.
func (w *mp3Window) confirmed(pos int64, f mp3Frame) bool {
	end := pos + f.length
	if end+mp3HeaderLength > w.s.size {
		return true
	}
	next, ok := w.frame(end)
	return ok && next.version == f.version && next.sampleRate == f.sampleRate
}

// mp3Duration uses the Xing, Info or VBRI frame count of the first frame when it has one and
// otherwise adds up every frame of the file. The count includes the encoder delay and padding,
// which a decoder drops, so the result is at or above the decoded length.
func mp3Duration(s source) (float64, error) {
	w := &mp3Window{s: s}
	pos, err := mp3SkipTags(s)
	if err != nil {
		return 0, err
	}
	first, at, ok := mp3FirstFrame(w, pos)
	if !ok {
		if w.err != nil {
			return 0, w.err
		}
		return 0, unreadable("no MP3 frame found")
	}
	if frames, ok := mp3HeaderFrameCount(w, at, first); ok {
		return float64(frames) * float64(first.samples) / float64(first.sampleRate), nil
	}
	return mp3ScanFrames(w, at, first)
}

// mp3SkipTags returns the offset after the ID3v2 tags at the start of the file.
func mp3SkipTags(s source) (int64, error) {
	pos := int64(0)
	for range mp3MaxTags {
		h, err := s.read(pos, 10)
		if err != nil || !bytes.HasPrefix(h, []byte("ID3")) || (h[6]|h[7]|h[8]|h[9])&0x80 != 0 {
			return pos, nil //nolint:nilerr // no tag here: the frames start at pos
		}
		size := int64(h[6])<<21 | int64(h[7])<<14 | int64(h[8])<<7 | int64(h[9])
		if h[5]&0x10 != 0 {
			size += 10
		}
		pos += 10 + size
		if pos >= s.size {
			return 0, unreadable("an ID3 tag fills the file")
		}
	}
	return pos, nil
}

// mp3FirstFrame finds the first confirmed frame within a bounded window after pos.
func mp3FirstFrame(w *mp3Window, pos int64) (mp3Frame, int64, bool) {
	limit := min(pos+mp3FirstScan, w.s.size-mp3HeaderLength)
	for at := pos; at <= limit; at++ {
		if f, ok := w.frame(at); ok && w.confirmed(at, f) {
			return f, at, true
		}
	}
	return mp3Frame{}, 0, false
}

// mp3HeaderFrameCount reads the frame count of a Xing or Info header (flag bit 1) or a VBRI header
// out of the first frame.
func mp3HeaderFrameCount(w *mp3Window, at int64, f mp3Frame) (uint32, bool) {
	crc := 0
	if f.protected {
		crc = 2
	}
	xing := int64(mp3HeaderLength + crc + f.sideInfo)
	if xing+12 <= f.length {
		if b := w.at(at+xing, 12); b != nil && (bytes.HasPrefix(b, []byte("Xing")) || bytes.HasPrefix(b, []byte("Info"))) {
			if binary.BigEndian.Uint32(b[4:8])&1 != 0 {
				if frames := binary.BigEndian.Uint32(b[8:12]); frames > 0 {
					return frames, true
				}
			}
			return 0, false
		}
	}
	const vbri = mp3HeaderLength + 32
	if vbri+18 <= f.length {
		if b := w.at(at+vbri, 18); b != nil && bytes.HasPrefix(b, []byte("VBRI")) {
			if frames := binary.BigEndian.Uint32(b[14:18]); frames > 0 {
				return frames, true
			}
		}
	}
	return 0, false
}

// mp3ScanFrames hops from frame to frame. Where the next header does not parse, it looks for the
// next confirmed frame byte by byte, and stops when there is none; a truncated last frame counts.
func mp3ScanFrames(w *mp3Window, at int64, first mp3Frame) (float64, error) {
	var samples uint64
	for pos := at; pos+mp3HeaderLength <= w.s.size; {
		f, ok := w.frame(pos)
		if !ok || f.version != first.version || f.sampleRate != first.sampleRate {
			next, found := mp3Resync(w, pos+1, first)
			if !found {
				break
			}
			pos = next
			continue
		}
		samples += f.samples
		pos += f.length
	}
	if w.err != nil {
		return 0, w.err
	}
	return float64(samples) / float64(first.sampleRate), nil
}

func mp3Resync(w *mp3Window, pos int64, first mp3Frame) (int64, bool) {
	for at := pos; at+mp3HeaderLength <= w.s.size; at++ {
		if f, ok := w.frame(at); ok && f.version == first.version && f.sampleRate == first.sampleRate && w.confirmed(at, f) {
			return at, true
		}
	}
	return 0, false
}
