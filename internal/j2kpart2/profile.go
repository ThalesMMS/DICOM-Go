// Package j2kpart2 implements the bounded Annex J profile used by the
// dicom-go JPEG 2000 Part 2 collection encoder.
package j2kpart2

import (
	"encoding/binary"
	"math"
)

const (
	Capabilities = 0x8100 // Part 2 and the Annex J multiple component extension.
	MaxFrames    = 16
	MaxSamples   = 1 << 21
)

// Profile describes one collection with identical reconstructed component
// precision and signedness. Every coded component is signed.
type Profile struct {
	Frames     int
	Bits       int
	Signed     bool
	Reversible bool
}

func (p Profile) Valid() bool {
	return p.Frames >= 2 && p.Frames <= MaxFrames && p.Bits >= 1 &&
		(p.Reversible && p.Bits <= 16 || !p.Reversible && p.Bits == 8)
}

func (p Profile) Shift() int32 {
	if p.Signed {
		return 0
	}
	return 1 << (p.Bits - 1)
}

func (p Profile) CodedBits() int { return p.Bits + 1 }

// Markers yields the complete Annex J marker sequence. See ITU-T T.801
// A.3.9–A.3.12 and J.3.1. The first coded plane is the level-shifted first
// frame; each subsequent plane is the difference to its predecessor.
func (p Profile) Markers() []byte {
	depth := byte(p.Bits - 1)
	if p.Signed {
		depth |= 0x80
	}
	cbd := binary.BigEndian.AppendUint16(nil, uint16(p.Frames))
	for range p.Frames {
		cbd = append(cbd, depth)
	}
	out := appendMarker(nil, 0xff78, cbd)

	// Imct combines an array index, its purpose (bits 8–9) and element
	// representation (bits 10–11). Zmct and Ymct zero mean one segment.
	const (
		dependency32    = 0x0401
		decorrelation32 = 0x0901
		offset32        = 0x0602
	)
	matrix := []byte{0, 0, byte(decorrelation32 >> 8), 1, 0, 0}
	if p.Reversible {
		matrix[2] = byte(dependency32 >> 8)
		// Each row predicts from the previous reconstructed plane using a
		// unit coefficient. The final entry in the row is the divisor.
		for row := 1; row < p.Frames; row++ {
			for column := 0; column <= row; column++ {
				coefficient := uint32(0)
				if column >= row-1 {
					coefficient = 1
				}
				matrix = binary.BigEndian.AppendUint32(matrix, coefficient)
			}
		}
	} else {
		// Inverse decorrelation matrix: sum the coded differences through
		// the requested output component.
		for row := 0; row < p.Frames; row++ {
			for column := 0; column < p.Frames; column++ {
				coefficient := uint32(0)
				if column <= row {
					coefficient = math.Float32bits(1)
				}
				matrix = binary.BigEndian.AppendUint32(matrix, coefficient)
			}
		}
	}
	out = appendMarker(out, 0xff74, matrix)
	offsets := []byte{0, 0, byte(offset32 >> 8), 2, 0, 0}
	for frame := 0; frame < p.Frames; frame++ {
		offset := p.Shift()
		// Reversible dependency predicts from already shifted outputs.
		if p.Reversible && frame != 0 {
			offset = 0
		}
		offsets = binary.BigEndian.AppendUint32(offsets, uint32(offset))
	}
	out = appendMarker(out, 0xff74, offsets)

	kind, reversible := byte(1), byte(0)
	if p.Reversible {
		kind, reversible = 0, 1
	}
	mcc := []byte{0, 0, 1, 0, 0, 0, 1, kind}
	// Both collections use identical, sequential one-byte indices, zero
	// high bit of Nmcc/Mmcc. Tmcc names offset array 2 and transform 1.
	for range 2 {
		mcc = binary.BigEndian.AppendUint16(mcc, uint16(p.Frames))
		for i := 0; i < p.Frames; i++ {
			mcc = append(mcc, byte(i))
		}
	}
	mcc = append(mcc, reversible, 2, 1)
	out = appendMarker(out, 0xff75, mcc)
	return appendMarker(out, 0xff77, []byte{1, 1})
}

func appendMarker(dst []byte, marker uint16, body []byte) []byte {
	dst = binary.BigEndian.AppendUint16(dst, marker)
	dst = binary.BigEndian.AppendUint16(dst, uint16(2+len(body)))
	return append(dst, body...)
}
