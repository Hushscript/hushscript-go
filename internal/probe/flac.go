package probe

import (
	"bytes"
	"encoding/binary"
)

const (
	flacStreamInfoLen = 34
	flacMinBlockSize  = 16
)

// flacDuration reads total samples and the sample rate from STREAMINFO, the first metadata block.
// A leading ID3v2 tag is skipped, as the server's FLAC path does. STREAMINFO survives a cut of the file, so it is checked against the file's own size: the audio
// frames cannot take fewer bytes than the smallest frame STREAMINFO declares, times the least
// number of frames the samples need.
func flacDuration(s source) (float64, error) {
	tag, err := s.read(0, int(min(s.size, 10)))
	if err != nil {
		return 0, err
	}
	base := int64(id3v2End(tag))
	head, err := s.read(base, 8)
	if err != nil {
		return 0, err
	}
	blockLen := int64(head[5])<<16 | int64(head[6])<<8 | int64(head[7])
	if !bytes.HasPrefix(head, []byte("fLaC")) || head[4]&0x7F != 0 || blockLen < flacStreamInfoLen {
		return 0, unreadable("STREAMINFO is not the first FLAC block")
	}
	info, err := s.read(base+8, flacStreamInfoLen)
	if err != nil {
		return 0, err
	}
	maxBlock := uint64(binary.BigEndian.Uint16(info[2:4]))
	minFrame := uint64(info[4])<<16 | uint64(info[5])<<8 | uint64(info[6])
	rate := uint64(info[10])<<12 | uint64(info[11])<<4 | uint64(info[12])>>4
	total := uint64(info[13]&0x0F)<<32 | uint64(binary.BigEndian.Uint32(info[14:18]))
	if rate == 0 || total == 0 {
		return 0, unreadable("STREAMINFO declares no length")
	}
	if maxBlock >= flacMinBlockSize && minFrame > 0 {
		frames := (total + maxBlock - 1) / maxBlock
		audioBytes := s.size - base - 8 - blockLen
		if audioBytes < 0 || uint64(audioBytes) < (frames-1)*minFrame {
			return 0, unreadable("the file is truncated: it holds less audio than STREAMINFO declares")
		}
	}
	return float64(total) / float64(rate), nil
}
