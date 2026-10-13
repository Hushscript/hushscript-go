// Package probe reads the duration of an audio or video file without decoding it. The bytes are
// untrusted, so every read is bounds-checked and every loop advances or has a fixed cap.
package probe

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
)

// Format is a container the probe understands.
type Format int

// The formats Sniff recognizes.
const (
	FormatUnknown Format = iota
	FormatMP3
	FormatISOBMFF
	FormatWAV
	FormatFLAC
	FormatOgg
)

// String names the format for messages.
func (f Format) String() string {
	switch f {
	case FormatUnknown:
		return "unknown"
	case FormatMP3:
		return "mp3"
	case FormatISOBMFF:
		return "iso-bmff"
	case FormatWAV:
		return "wav"
	case FormatFLAC:
		return "flac"
	case FormatOgg:
		return "ogg"
	}
	return "unknown"
}

// ErrUnreadable means the duration could not be read from the file.
var ErrUnreadable = errors.New("probe: could not read the duration")

func unreadable(why string) error {
	return fmt.Errorf("%w: %s", ErrUnreadable, why)
}

// Sniff names the container from its first bytes, not from a file name. It accepts MP3 (an ID3
// tag or a Layer III frame sync), ISO-BMFF (ftyp at offset 4), WAV (RIFF, RF64 or BW64 with WAVE),
// FLAC and Ogg, and returns FormatUnknown for anything else.
func Sniff(head []byte) Format {
	switch {
	case len(head) >= 12 && bytes.Equal(head[8:12], []byte("WAVE")) &&
		(bytes.HasPrefix(head, []byte("RIFF")) || bytes.HasPrefix(head, []byte("RF64")) || bytes.HasPrefix(head, []byte("BW64"))):
		return FormatWAV
	case bytes.HasPrefix(head, []byte("fLaC")):
		return FormatFLAC
	case bytes.HasPrefix(head, []byte("OggS")):
		return FormatOgg
	case len(head) >= 8 && bytes.Equal(head[4:8], []byte("ftyp")):
		return FormatISOBMFF
	case bytes.HasPrefix(head, []byte("ID3")):
		// A FLAC stream may sit behind an ID3v2 tag, as the server's FLAC path accepts.
		if end := id3v2End(head); end > 0 && end <= len(head) && bytes.HasPrefix(head[end:], []byte("fLaC")) {
			return FormatFLAC
		}
		return FormatMP3
	case len(head) >= 2 && isLayer3Sync(head[0], head[1]):
		return FormatMP3
	}
	return FormatUnknown
}

// Duration returns the length in seconds of the size bytes behind r, read as format f. The result
// errs high: the caller rounds it up, and the rounded value is never below the true duration. Any
// malformed input returns an error wrapping ErrUnreadable.
func Duration(r io.ReaderAt, size int64, f Format) (float64, error) {
	if r == nil || size <= 0 {
		return 0, unreadable("the file is empty")
	}
	s := source{r: r, size: size}
	var seconds float64
	var err error
	switch f {
	case FormatMP3:
		seconds, err = mp3Duration(s)
	case FormatISOBMFF:
		seconds, err = isobmffDuration(s)
	case FormatWAV:
		seconds, err = wavDuration(s)
	case FormatFLAC:
		seconds, err = flacDuration(s)
	case FormatOgg:
		seconds, err = oggDuration(s)
	case FormatUnknown:
		err = unreadable("the format is not supported")
	default:
		err = unreadable("the format is not supported")
	}
	if err != nil {
		return 0, err
	}
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return 0, unreadable("the file declares no length")
	}
	return seconds, nil
}

// source is a ReaderAt with a declared size. Every read is checked against that size first.
type source struct {
	r    io.ReaderAt
	size int64
}

// read returns exactly n bytes at off, or an error when they are not all inside the file.
func (s source) read(off int64, n int) ([]byte, error) {
	if n < 0 || off < 0 || off > s.size || int64(n) > s.size-off {
		return nil, unreadable("a field lies outside the file")
	}
	buf := make([]byte, n)
	got, err := s.r.ReadAt(buf, off)
	if got == n {
		return buf, nil
	}
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, unreadable("the file is shorter than its declared size")
	}
	return nil, fmt.Errorf("%w: reading the file: %w", ErrUnreadable, err)
}

func isLayer3Sync(a, b byte) bool {
	return a == 0xFF && b&0xE0 == 0xE0 && b&0x06 == 0x02
}

// sniffBytes is how much of the start of a file SniffAt looks at.
const sniffBytes = 64

// SniffAt is Sniff on the start of the size bytes behind r. It reads past an ID3v2 tag to tell a
// tagged FLAC from an MP3, which a short head cannot. A short or failing reader gives FormatUnknown.
func SniffAt(r io.ReaderAt, size int64) Format {
	if r == nil || size <= 0 {
		return FormatUnknown
	}
	head := make([]byte, min(size, sniffBytes))
	got, _ := r.ReadAt(head, 0)
	head = head[:got]
	format := Sniff(head)
	if format != FormatMP3 {
		return format
	}
	if end := id3v2End(head); end > 0 && int64(end)+4 <= size {
		if tail, err := (source{r: r, size: size}).read(int64(end), 4); err == nil && bytes.Equal(tail, []byte("fLaC")) {
			return FormatFLAC
		}
	}
	return format
}

// id3v2End is the offset just past an ID3v2 tag at the start of head, or 0 without one, or with a
// size that is not syncsafe. It follows id3v2_end in backend/src/audio_probe.rs.
func id3v2End(head []byte) int {
	if len(head) < 10 || !bytes.HasPrefix(head, []byte("ID3")) || (head[6]|head[7]|head[8]|head[9])&0x80 != 0 {
		return 0
	}
	end := 10 + int(head[6])<<21 + int(head[7])<<14 + int(head[8])<<7 + int(head[9])
	if head[5]&0x10 != 0 {
		end += 10
	}
	return end
}
