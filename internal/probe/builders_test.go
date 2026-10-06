package probe_test

import (
	"bytes"
	"encoding/binary"
)

func le16(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }
func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }
func le64(v uint64) []byte { return binary.LittleEndian.AppendUint64(nil, v) }
func be16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func be64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

// sizeOf is a length for a size field; the test inputs are tiny.
func sizeOf(b []byte) uint32 { return uint32(len(b)) } //nolint:gosec // test inputs are far below 4 GiB

func join(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// wavChunk is a RIFF chunk: id, declared size, body (padded when the declared size is odd).
func wavChunk(id string, size uint32, body []byte) []byte {
	out := join([]byte(id), le32(size), body)
	if size&1 == 1 && len(body)&1 == 1 {
		out = append(out, 0)
	}
	return out
}

func wavFmt(tag, channels uint16, rate uint32, align uint16) []byte {
	body := join(le16(tag), le16(channels), le32(rate), le32(rate*uint32(align)), le16(align), le16(16))
	return wavChunk("fmt ", sizeOf(body), body)
}

// pcmWAV is mono 16-bit audio at 8 kHz: 16000 bytes a second.
func pcmWAV(dataSize uint32, data []byte, extra ...[]byte) []byte {
	return join([]byte("RIFF"), le32(0), []byte("WAVE"), wavFmt(1, 1, 8000, 2), join(extra...), wavChunk("data", dataSize, data))
}

func flacInfo(minBlock, maxBlock uint16, minFrame, maxFrame, rate uint32, channels, bits uint8, total uint64) []byte {
	packed := uint64(rate)<<44 | uint64(channels-1)<<41 | uint64(bits-1)<<36 | total
	return join([]byte("fLaC"), []byte{0x80, 0, 0, 34}, be16(minBlock), be16(maxBlock),
		be32(minFrame)[1:], be32(maxFrame)[1:], be64(packed), make([]byte, 16))
}

func mp4Box(kind string, payload ...[]byte) []byte {
	body := join(payload...)
	return join(be32(sizeOf(body)+8), []byte(kind), body)
}

func mvhdV0(timescale, duration uint32) []byte {
	return mp4Box("mvhd", []byte{0, 0, 0, 0}, be32(0), be32(0), be32(timescale), be32(duration), make([]byte, 80))
}

func mvhdV1(timescale uint32, duration uint64) []byte {
	return mp4Box("mvhd", []byte{1, 0, 0, 0}, be64(0), be64(0), be32(timescale), be64(duration), make([]byte, 80))
}

func ftyp() []byte { return mp4Box("ftyp", []byte("M4A "), be32(0), []byte("M4A ")) }

// oggCRC32 is a bitwise reference implementation of the Ogg page checksum.
func oggCRC32(page []byte) uint32 {
	var crc uint32
	for _, b := range page {
		crc ^= uint32(b) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func oggPage(flags byte, granule uint64, serial, seq uint32, body []byte) []byte {
	var lacing []byte
	for n := len(body); ; n -= 255 {
		if n >= 255 {
			lacing = append(lacing, 255)
			continue
		}
		lacing = append(lacing, byte(n&0xFF))
		break
	}
	page := join([]byte("OggS"), []byte{0, flags}, le64(granule), le32(serial), le32(seq), le32(0), []byte{byte(len(lacing) & 0xFF)}, lacing, body)
	binary.LittleEndian.PutUint32(page[22:26], oggCRC32(page))
	return page
}

func opusHead(preSkip uint16) []byte {
	return join([]byte("OpusHead"), []byte{1, 1}, le16(preSkip), le32(16000), le16(0), []byte{0})
}

func vorbisHead(rate uint32) []byte {
	return join([]byte("\x01vorbis"), le32(0), []byte{1}, le32(rate), le32(0), le32(0), le32(0), []byte{0xB8, 1})
}

// opusFile is a BOS page, a tags page and a final page at the granule position.
func opusFile(preSkip uint16, granule uint64) []byte {
	return join(
		oggPage(0x02, 0, 7, 0, opusHead(preSkip)),
		oggPage(0, 0, 7, 1, []byte("OpusTags")),
		oggPage(0x04, granule, 7, 2, make([]byte, 100)),
	)
}

// mp3Header builds a Layer III header. version: 3 MPEG-1, 2 MPEG-2, 0 MPEG-2.5.
func mp3Header(version, bitrateIndex, rateIndex, padding byte, mono, noCRC bool) []byte {
	b1 := 0xE0 | version<<3 | 1<<1
	if noCRC {
		b1 |= 1
	}
	b3 := byte(0)
	if mono {
		b3 = 0xC0
	}
	return []byte{0xFF, b1, bitrateIndex<<4 | rateIndex<<2 | padding<<1, b3}
}

// mpeg2Frame is a mono 64 kbit/s 16 kHz frame of 288 bytes and 576 samples.
func mpeg2Frame(fill byte) []byte {
	return join(mp3Header(2, 8, 2, 0, true, true), bytes.Repeat([]byte{fill}, 284))
}

// mpeg1Frame is a stereo 128 kbit/s 44.1 kHz frame of 417 or 418 bytes and 1152 samples.
func mpeg1Frame(padding byte) []byte {
	return join(mp3Header(3, 9, 0, padding, false, true), make([]byte, 413+int(padding)))
}

func xingFrame(flags, frames uint32) []byte {
	frame := mpeg2Frame(0)
	copy(frame[4+9:], join([]byte("Info"), be32(flags), be32(frames)))
	return frame
}

func repeat(frame []byte, n int) []byte { return bytes.Repeat(frame, n) }

func id3Tag(bodySize int, footer bool) []byte {
	flags := byte(0)
	if footer {
		flags = 0x10
	}
	size := []byte{byte(bodySize >> 21 & 0x7F), byte(bodySize >> 14 & 0x7F), byte(bodySize >> 7 & 0x7F), byte(bodySize & 0x7F)}
	out := join([]byte("ID3"), []byte{4, 0, flags}, size, make([]byte, bodySize))
	if footer {
		out = append(out, make([]byte, 10)...)
	}
	return out
}
