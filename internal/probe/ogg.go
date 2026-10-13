package probe

import (
	"bytes"
	"encoding/binary"
)

const (
	oggTailSize       = 64 << 10
	oggPageHeader     = 27
	oggMaxCRCFailures = 256
	oggOpusRate       = 48000
	oggFlagBOS        = 0x02
)

var oggCRCTable = func() [256]uint32 {
	var table [256]uint32
	for i := range table {
		r := uint32(i) << 24 //nolint:gosec // i is below 256
		for range 8 {
			if r&0x80000000 != 0 {
				r = r<<1 ^ 0x04C11DB7
			} else {
				r <<= 1
			}
		}
		table[i] = r
	}
	return table
}()

// oggCRC is the Ogg page checksum: CRC-32 with polynomial 0x04C11DB7, no reflection, initial value
// 0, over the page with its own checksum field zeroed. hash/crc32 cannot compute it.
func oggCRC(page []byte) uint32 {
	var crc uint32
	for i, b := range page {
		if i >= 22 && i < 26 {
			b = 0
		}
		crc = crc<<8 ^ oggCRCTable[byte(crc>>24)^b]
	}
	return crc
}

type oggPage struct {
	flags   byte
	granule uint64
	serial  uint32
	length  int // header, lacing table and body
	body    int // offset of the body inside the page
}

// oggPageAt parses the page header at the start of data. It checks the structure only.
func oggPageAt(data []byte) (oggPage, bool) {
	if len(data) < oggPageHeader || !bytes.HasPrefix(data, []byte("OggS")) || data[4] != 0 {
		return oggPage{}, false
	}
	segments := int(data[26])
	bodyStart := oggPageHeader + segments
	if len(data) < bodyStart {
		return oggPage{}, false
	}
	bodyLen := 0
	for _, lace := range data[oggPageHeader:bodyStart] {
		bodyLen += int(lace)
	}
	return oggPage{
		flags:   data[5],
		granule: binary.LittleEndian.Uint64(data[6:14]),
		serial:  binary.LittleEndian.Uint32(data[14:18]),
		length:  bodyStart + bodyLen,
		body:    bodyStart,
	}, true
}

// oggDuration reads the codec parameters from the first page and the granule position from the
// last page in the final 64 KiB. For Opus the position counts 48 kHz samples including the
// pre-skip given in OpusHead; for Vorbis it counts samples at the rate in the identification
// header. A file whose tail holds a second logical stream is refused: its last granule would not
// cover the whole file.
func oggDuration(s source) (float64, error) {
	first, err := oggFirstPage(s)
	if err != nil {
		return 0, err
	}
	granule, err := oggLastGranule(s, first.serial)
	if err != nil {
		return 0, err
	}
	if first.opus {
		if granule < uint64(first.preSkip) {
			return 0, unreadable("the Opus stream ends inside its pre-skip")
		}
		// The decoded audio is (granule - pre-skip) / 48000 long. The container reports the whole
		// granule position, and that is what ffprobe and the conformance fixtures count, so the
		// pre-skip is not subtracted: erring high keeps ceil(probe) at or above that figure. It
		// is read only to validate the granule.
		return float64(granule) / oggOpusRate, nil
	}
	return float64(granule) / float64(first.rate), nil
}

type oggStream struct {
	serial  uint32
	opus    bool
	preSkip uint16
	rate    uint32
}

func oggFirstPage(s source) (oggStream, error) {
	head, err := s.read(0, int(min(s.size, 512)))
	if err != nil {
		return oggStream{}, err
	}
	page, ok := oggPageAt(head)
	if !ok || page.flags&oggFlagBOS == 0 || len(head) < page.length {
		return oggStream{}, unreadable("the first Ogg page is not a stream start")
	}
	packet := head[page.body:page.length]
	stream := oggStream{serial: page.serial}
	switch {
	case bytes.HasPrefix(packet, []byte("OpusHead")) && len(packet) >= 19:
		stream.opus = true
		stream.preSkip = binary.LittleEndian.Uint16(packet[10:12])
	case bytes.HasPrefix(packet, []byte("\x01vorbis")) && len(packet) >= 16:
		stream.rate = binary.LittleEndian.Uint32(packet[12:16])
		if stream.rate == 0 {
			return oggStream{}, unreadable("the Vorbis sample rate is zero")
		}
	default:
		return oggStream{}, unreadable("the Ogg stream is neither Opus nor Vorbis")
	}
	return stream, nil
}

// oggLastGranule walks the pages of the tail window and returns the last granule position of the
// stream with the given serial. The first page of the window that parses with a good checksum
// starts the walk (an "OggS" inside a packet, or the cut start of the window, is skipped); from
// there every page must parse and fit, or the file is refused.
func oggLastGranule(s source, serial uint32) (uint64, error) {
	tailLen := min(s.size, oggTailSize)
	start := s.size - tailLen
	tail, err := s.read(start, int(tailLen))
	if err != nil {
		return 0, err
	}
	var granule uint64
	found, synced := false, false
	failures := 0
	for pos := 0; pos < len(tail); {
		if !synced {
			next := bytes.Index(tail[pos:], []byte("OggS"))
			if next < 0 {
				break
			}
			pos += next
		}
		page, ok := oggPageAt(tail[pos:])
		if !ok || pos+page.length > len(tail) || binary.LittleEndian.Uint32(tail[pos+22:pos+26]) != oggCRC(tail[pos:pos+page.length]) {
			if synced {
				// Past the first good page the pages follow one another to the end of the file. A page
				// that does not parse is a damaged or cut last page, and an earlier granule would
				// undercount the file.
				return 0, unreadable("the last Ogg page is damaged")
			}
			failures++
			if failures > oggMaxCRCFailures {
				return 0, unreadable("the end of the Ogg file is damaged")
			}
			pos++
			continue
		}
		synced = true
		if page.flags&oggFlagBOS != 0 && (start+int64(pos) > 0) {
			return 0, unreadable("the Ogg file holds more than one stream")
		}
		if page.serial == serial && page.granule != ^uint64(0) {
			granule, found = page.granule, true
		}
		pos += page.length
	}
	if !found {
		return 0, unreadable("no Ogg page with a position at the end of the file")
	}
	return granule, nil
}
