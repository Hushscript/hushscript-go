package probe

import (
	"encoding/binary"
	"math"
)

const isobmffMaxBoxes = 16384

type box struct {
	kind string
	body int64 // first byte after the header
	end  int64 // first byte after the box
}

// isobmffDuration walks the top-level boxes to moov, then moov's children to mvhd, and divides the
// movie duration by its timescale. mvhd covers the longest track, video included, so it can only
// be at or above the audio length.
func isobmffDuration(s source) (float64, error) {
	boxes := 0
	moov, found, err := findBox(s, 0, s.size, "moov", &boxes)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, unreadable("no moov box")
	}
	mvhd, found, err := findBox(s, moov.body, moov.end, "mvhd", &boxes)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, unreadable("no mvhd box")
	}
	return mvhdSeconds(s, mvhd)
}

// findBox walks the sibling boxes in [from, limit) for one of the given kind. Every size is checked
// against the enclosing limit, and the shared counter caps the work.
func findBox(s source, from, limit int64, kind string, boxes *int) (box, bool, error) {
	for pos := from; pos+8 <= limit; {
		*boxes++
		if *boxes > isobmffMaxBoxes {
			return box{}, false, unreadable("too many boxes")
		}
		b, err := readBox(s, pos, limit)
		if err != nil {
			return box{}, false, err
		}
		if b.kind == kind {
			return b, true, nil
		}
		pos = b.end
	}
	return box{}, false, nil
}

// readBox reads the header of the box at pos: a 32-bit size, a type, and a 64-bit size after the
// type when the 32-bit one is 1. A size of 0 means the box runs to the end of its parent.
func readBox(s source, pos, limit int64) (box, error) {
	head, err := s.read(pos, 8)
	if err != nil {
		return box{}, err
	}
	size := int64(binary.BigEndian.Uint32(head[:4]))
	header := int64(8)
	switch size {
	case 0:
		size = limit - pos
	case 1:
		large, err := s.read(pos+8, 8)
		if err != nil {
			return box{}, err
		}
		wide := binary.BigEndian.Uint64(large)
		if wide > math.MaxInt64 {
			return box{}, unreadable("a box is too large")
		}
		size, header = int64(wide), 16
	}
	if size < header || size > limit-pos {
		return box{}, unreadable("a box size lies outside its parent")
	}
	return box{kind: string(head[4:8]), body: pos + header, end: pos + size}, nil
}

func mvhdSeconds(s source, b box) (float64, error) {
	payload := b.end - b.body
	if payload < 4 {
		return 0, unreadable("mvhd is too short")
	}
	version, err := s.read(b.body, 1)
	if err != nil {
		return 0, err
	}
	var timescale uint32
	var duration uint64
	switch version[0] {
	case 0:
		if payload < 20 {
			return 0, unreadable("mvhd is too short")
		}
		f, err := s.read(b.body+12, 8)
		if err != nil {
			return 0, err
		}
		timescale = binary.BigEndian.Uint32(f[:4])
		duration = uint64(binary.BigEndian.Uint32(f[4:8]))
		if duration == math.MaxUint32 {
			return 0, unreadable("mvhd duration is unknown")
		}
	case 1:
		if payload < 32 {
			return 0, unreadable("mvhd is too short")
		}
		f, err := s.read(b.body+20, 12)
		if err != nil {
			return 0, err
		}
		timescale = binary.BigEndian.Uint32(f[:4])
		duration = binary.BigEndian.Uint64(f[4:12])
		if duration == math.MaxUint64 {
			return 0, unreadable("mvhd duration is unknown")
		}
	default:
		return 0, unreadable("unknown mvhd version")
	}
	if timescale == 0 {
		return 0, unreadable("mvhd timescale is zero")
	}
	return float64(duration) / float64(timescale), nil
}
