package encapsulated

import (
	"context"
	"encoding/binary"
	"fmt"
)

// J2KTilePart locates one tile-part of a JPEG 2000 codestream (ISO/IEC
// 15444-1 A.4.2). Offsets index the codestream passed to ParseJ2KLayout.
type J2KTilePart struct {
	Tile      int // Isot
	Part      int // TPsot
	Parts     int // TNsot; zero when the encoder left it unspecified
	Start     int // offset of the SOT marker
	HeaderEnd int // offset of the first byte after SOD
	End       int // offset just past the tile-part's last byte
}

// J2KLayout is the marker-level structure of one JPEG 2000 codestream: the
// main header, the tile-parts in codestream order and the EOC marker.
type J2KLayout struct {
	MainHeaderEnd int // offset of the first SOT marker
	TileParts     []J2KTilePart
	EOC           int // offset of the EOC marker
}

// ParseJ2KLayout validates a codestream with the framing rules of the
// JPEG 2000 frame scanner (ValidateFrame: lengths, not marker searches,
// delimit segments; a Psot of zero runs to EOC; at most one pad byte follows
// EOC) and then reports where the main header and each tile-part lie. It does
// not decode packets or coding parameters.
func ParseJ2KLayout(data []byte) (J2KLayout, error) {
	if err := ValidateFrame(context.Background(), data, JPEG2000); err != nil {
		return J2KLayout{}, err
	}
	// The scanner accepted the codestream, so every marker below is followed
	// by a length that stays inside data; the checks are defensive.
	segment := func(at int) (int, error) {
		if at+4 > len(data) {
			return 0, fmt.Errorf("%w: truncated JPEG 2000 marker segment", ErrLayout)
		}
		length := int(binary.BigEndian.Uint16(data[at+2:]))
		if length < 2 || at+2+length+2 > len(data) {
			return 0, fmt.Errorf("%w: invalid JPEG 2000 marker length", ErrLayout)
		}
		return at + 2 + length, nil
	}
	var layout J2KLayout
	at := 2
	for data[at+1] != 0x90 {
		if data[at+1] == 0xd9 {
			return J2KLayout{}, fmt.Errorf("%w: JPEG 2000 codestream has no tile-part", ErrLayout)
		}
		next, err := segment(at)
		if err != nil {
			return J2KLayout{}, err
		}
		at = next
	}
	layout.MainHeaderEnd = at
	eoc := len(data) - 2
	if data[eoc] != 0xff || data[eoc+1] != 0xd9 {
		eoc-- // one pad byte
	}
	for data[at+1] == 0x90 {
		if at+14 > len(data) {
			return J2KLayout{}, fmt.Errorf("%w: truncated SOT", ErrLayout)
		}
		part := J2KTilePart{
			Tile:  int(binary.BigEndian.Uint16(data[at+4:])),
			Part:  int(data[at+10]),
			Parts: int(data[at+11]),
			Start: at,
		}
		psot := int64(binary.BigEndian.Uint32(data[at+6:]))
		header := at + 12
		for data[header+1] != 0x93 {
			next, err := segment(header)
			if err != nil {
				return J2KLayout{}, err
			}
			header = next
		}
		part.HeaderEnd = header + 2
		if psot == 0 {
			part.End = eoc
		} else {
			if psot > int64(eoc-at) {
				return J2KLayout{}, fmt.Errorf("%w: invalid Psot", ErrLayout)
			}
			part.End = at + int(psot)
		}
		if part.End < part.HeaderEnd {
			return J2KLayout{}, fmt.Errorf("%w: invalid Psot", ErrLayout)
		}
		layout.TileParts = append(layout.TileParts, part)
		at = part.End
	}
	if at != eoc {
		return J2KLayout{}, fmt.Errorf("%w: expected EOC", ErrLayout)
	}
	layout.EOC = at
	return layout, nil
}
