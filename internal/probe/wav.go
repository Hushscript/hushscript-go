package probe

import (
	"bytes"
	"encoding/binary"
)

const (
	wavMaxChunks     = 4096
	wavMaxSampleRate = 768000
	wavMaxChannels   = 256
	wavFmtRead       = 40
)

// wavDuration divides the data chunk by the byte rate of a PCM, float, A-law or mu-law fmt chunk.
// A data size that is zero, the streaming sentinel or larger than the file falls back to the bytes
// actually present, so a lying header cannot stretch the duration past the file.
func wavDuration(s source) (float64, error) {
	head, err := s.read(0, 12)
	if err != nil {
		return 0, err
	}
	isRIFF := bytes.HasPrefix(head, []byte("RIFF"))
	if !bytes.Equal(head[8:12], []byte("WAVE")) {
		return 0, unreadable("not a WAVE file")
	}
	var sampleRate, blockAlign uint32
	var ds64Data uint64
	var haveFmt, haveDS64 bool
	pos := int64(12)
	for range wavMaxChunks {
		chunk, err := s.read(pos, 8)
		if err != nil {
			return 0, unreadable("no data chunk")
		}
		id := chunk[:4]
		size := binary.LittleEndian.Uint32(chunk[4:8])
		body := pos + 8
		switch {
		case !isRIFF && bytes.Equal(id, []byte("ds64")):
			if size >= 16 {
				ds64Body, readErr := s.read(body, 16)
				if readErr != nil {
					return 0, readErr
				}
				ds64Data = binary.LittleEndian.Uint64(ds64Body[8:16])
				haveDS64 = true
			}
		case bytes.Equal(id, []byte("fmt ")):
			sampleRate, blockAlign, err = wavFormat(s, body, size)
			if err != nil {
				return 0, err
			}
			haveFmt = true
		case bytes.Equal(id, []byte("data")):
			if !haveFmt {
				return 0, unreadable("the data chunk comes before the fmt chunk")
			}
			available := s.size - body
			declared, known := uint64(size), true
			if size == 0xFFFFFFFF {
				declared, known = ds64Data, haveDS64
			}
			audio := available
			if known && declared != 0 && declared <= uint64(available) { //nolint:gosec // available is a file size and never negative here
				audio = int64(declared) //nolint:gosec // bounded by available, a file size
			}
			if audio <= 0 {
				return 0, unreadable("the data chunk is empty")
			}
			return float64(audio) / (float64(blockAlign) * float64(sampleRate)), nil
		}
		pos = body + int64(size) + int64(size&1)
	}
	return 0, unreadable("too many chunks before the data chunk")
}

// wavFormat checks a fmt chunk and returns its sample rate and block size. Only fixed-size-frame
// codecs qualify, with a block size that matches channels times sample bytes: a header that lies
// about the frame size would otherwise stretch the duration.
func wavFormat(s source, body int64, size uint32) (rate, align uint32, err error) {
	if size < 16 {
		return 0, 0, unreadable("the fmt chunk is too short")
	}
	fmtBytes, err := s.read(body, int(min(size, wavFmtRead)))
	if err != nil {
		return 0, 0, err
	}
	tag := binary.LittleEndian.Uint16(fmtBytes[0:2])
	channels := binary.LittleEndian.Uint16(fmtBytes[2:4])
	rate = binary.LittleEndian.Uint32(fmtBytes[4:8])
	align = uint32(binary.LittleEndian.Uint16(fmtBytes[12:14]))
	bits := binary.LittleEndian.Uint16(fmtBytes[14:16])
	if tag == 0xFFFE && len(fmtBytes) >= 26 {
		tag = binary.LittleEndian.Uint16(fmtBytes[24:26])
	}
	frameBytes := uint32(channels) * ((uint32(bits) + 7) / 8)
	switch {
	case tag != 1 && tag != 3 && tag != 6 && tag != 7:
		return 0, 0, unreadable("the WAV codec has no fixed frame size")
	case channels < 1 || channels > wavMaxChannels, bits < 1 || bits > 64:
		return 0, 0, unreadable("the WAV format is implausible")
	case rate < 1 || rate > wavMaxSampleRate:
		return 0, 0, unreadable("the WAV sample rate is implausible")
	case align != frameBytes:
		return 0, 0, unreadable("the WAV block size does not match its format")
	}
	return rate, align, nil
}
