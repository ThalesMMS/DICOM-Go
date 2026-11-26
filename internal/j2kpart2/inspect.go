package j2kpart2

import (
	"bytes"
	"encoding/binary"
	"errors"
)

var ErrUnsupportedCollection = errors.New("jpeg2000: unsupported Annex J collection")

// Collection records the source image dimensions and the Annex J layout
// admitted by Inspect.
type Collection struct {
	Profile
	Rows, Columns int
	markerStart   int
	markerEnd     int
	codMCT        int
	end           int
}

// Inspect checks the bounded profile before the caller interprets the
// compressed components. Only the declared single-stage transform is
// accepted, so another Annex J transform is never decoded as differences.
func Inspect(data []byte) (Collection, error) {
	if len(data) < 2 || binary.BigEndian.Uint16(data) != 0xff4f {
		return Collection{}, ErrUnsupportedCollection
	}
	position := 2
	read := func(marker uint16) []byte {
		if len(data)-position < 4 || binary.BigEndian.Uint16(data[position:]) != marker {
			return nil
		}
		length := int(binary.BigEndian.Uint16(data[position+2:]))
		if length < 2 || length > len(data)-position-2 {
			return nil
		}
		start := position + 4
		position += 2 + length
		return data[start:position]
	}
	siz := read(0xff51)
	if len(siz) < 36 || binary.BigEndian.Uint16(siz) != Capabilities {
		return Collection{}, ErrUnsupportedCollection
	}
	width, height := binary.BigEndian.Uint32(siz[2:]), binary.BigEndian.Uint32(siz[6:])
	frames := int(binary.BigEndian.Uint16(siz[34:]))
	if width == 0 || height == 0 || width > 65535 || height > 65535 ||
		frames < 2 || frames > MaxFrames || len(siz) != 36+3*frames ||
		uint64(width)*uint64(height)*uint64(frames) > MaxSamples {
		return Collection{}, ErrUnsupportedCollection
	}
	// One tile, origin zero. The spatial DWT and packet layer follow Part 1.
	if binary.BigEndian.Uint64(siz[10:]) != 0 || binary.BigEndian.Uint64(siz[26:]) != 0 ||
		binary.BigEndian.Uint32(siz[18:]) != width || binary.BigEndian.Uint32(siz[22:]) != height {
		return Collection{}, ErrUnsupportedCollection
	}
	codMCT := position + 8
	cod := read(0xff52)
	if len(cod) != 10 || !bytes.Equal(cod[:5], []byte{0, 0, 0, 1, 2}) ||
		cod[5] > 16 || cod[6] != 4 || cod[7] != 4 || cod[8] != 0 || cod[9] > 1 {
		return Collection{}, ErrUnsupportedCollection
	}
	reversible := cod[9] == 1
	qcd := read(0xff5c)
	bandCount := 1 + 3*int(cod[5])
	if reversible {
		if len(qcd) != 1+bandCount || qcd[0]&31 != 0 {
			return Collection{}, ErrUnsupportedCollection
		}
	} else if len(qcd) != 1+2*bandCount || qcd[0]&31 != 2 {
		return Collection{}, ErrUnsupportedCollection
	}
	markerStart := position
	cbd := read(0xff78)
	if len(cbd) != 2+frames || binary.BigEndian.Uint16(cbd) != uint16(frames) {
		return Collection{}, ErrUnsupportedCollection
	}
	profile := Profile{
		Frames: frames, Bits: int(cbd[2]&0x7f) + 1, Signed: cbd[2]&0x80 != 0,
		Reversible: reversible,
	}
	if !profile.Valid() {
		return Collection{}, ErrUnsupportedCollection
	}
	for i := 0; i < frames; i++ {
		if siz[36+3*i] != byte(profile.CodedBits()-1)|0x80 || siz[37+3*i] != 1 || siz[38+3*i] != 1 {
			return Collection{}, ErrUnsupportedCollection
		}
	}
	markers := profile.Markers()
	if !bytes.HasPrefix(data[markerStart:], markers) {
		return Collection{}, ErrUnsupportedCollection
	}
	position = markerStart + len(markers)
	markerEnd := position
	sot := read(0xff90)
	if len(sot) != 8 || binary.BigEndian.Uint16(sot) != 0 || sot[6] != 0 || sot[7] != 1 ||
		len(data)-position < 2 || binary.BigEndian.Uint16(data[position:]) != 0xff93 {
		return Collection{}, ErrUnsupportedCollection
	}
	tileBytes := int64(binary.BigEndian.Uint32(sot[2:]))
	// Psot counts SOT through the last packet, then EOC follows exactly.
	if tileBytes < 14 || tileBytes > int64(len(data)-markerEnd-2) {
		return Collection{}, ErrUnsupportedCollection
	}
	end := markerEnd + int(tileBytes) + 2
	if binary.BigEndian.Uint16(data[end-2:]) != 0xffd9 ||
		(end != len(data) && (end%2 != 1 || len(data)-end != 1 || data[end] != 0)) {
		return Collection{}, ErrUnsupportedCollection
	}
	return Collection{
		Profile: profile, Rows: int(height), Columns: int(width),
		markerStart: markerStart, markerEnd: markerEnd, codMCT: codMCT, end: end,
	}, nil
}

// CodedComponents returns the spatial Part 1 payload with Annex J removed,
// for a Part 1 decoder. Call only with the same input passed to Inspect.
func (c Collection) CodedComponents(data []byte) []byte {
	out := make([]byte, 0, c.end-(c.markerEnd-c.markerStart))
	out = append(out, data[:c.markerStart]...)
	binary.BigEndian.PutUint16(out[6:], 0) // Rsiz in SIZ.
	out[c.codMCT] = 0
	return append(out, data[c.markerEnd:c.end]...)
}
