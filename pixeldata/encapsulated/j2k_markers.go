package encapsulated

import "fmt"

// frameScanner consumes one frame's bytes and reports its final byte.
type frameScanner interface {
	feed(byte) (bool, error)
}

// validFramePad reports whether one byte after the end-of-codestream marker is
// acceptable Item padding. PS3.5 A.4 lets the last fragment of a frame be
// padded to an even Item length without fixing the pad value, and encoders
// use 0x00, 0xFF (a JPEG fill byte) or 0x20. One byte of any value is
// therefore accepted when it makes the frame length even; a zero byte is
// also accepted at any length, as before. More bytes, or bytes before the
// end marker, remain layout errors.
func validFramePad(format Format, b byte, length int) bool {
	return length%2 == 0 || format != JPEG2000 && b == 0
}

// TrailingPad reports the single padding byte that follows the end marker
// (FF D9: JPEG/JPEG-LS EOI, JPEG 2000 EOC) of a structurally valid frame.
// Callers that re-encapsulate a codestream use it to drop the source padding;
// decoders use it for diagnostics. It does not validate the frame.
func TrailingPad(data []byte) (pad byte, ok bool) {
	n := len(data)
	if n < 3 || data[n-3] != 0xff || data[n-2] != 0xd9 {
		return 0, false
	}
	if data[n-2] == 0xff && data[n-1] == 0xd9 {
		return 0, false
	}
	return data[n-1], true
}

func newFrameScanner(format Format) frameScanner {
	if format == JPEG2000 {
		return &j2kScanner{}
	}
	return &markerScanner{format: format}
}

const (
	j2kSOC = iota
	j2kSOCCode
	j2kMarker
	j2kMarkerCode
	j2kLengthHigh
	j2kLengthLow
	j2kSegment
	j2kSOTFields
	j2kTilePart
	j2kHeaderMarker
	j2kHeaderMarkerCode
	j2kHeaderLengthHigh
	j2kHeaderLengthLow
	j2kHeaderSegment
	j2kBitstream
	j2kBitstreamCode
	j2kEnd
)

// j2kScanner frames a JPEG 2000 Part 1 codestream (ISO/IEC 15444-1 Annex A)
// by its structure, not by searching for FF D9: SOC, main-header segments by
// their lengths, then tile-parts. A tile-part with a nonzero Psot is skipped by
// its length; Psot 0 (the last tile-part) runs to EOC, where only SOP/EPH and
// EOC markers may follow a 0xFF in packet data. Marker-segment payloads such as
// COM are therefore opaque.
type j2kScanner struct {
	state       int
	length      int
	remaining   int64
	sot         [8]byte
	sotRead     int
	marker      byte
	tileHeaders bool
}

func (s *j2kScanner) feed(b byte) (bool, error) {
	switch s.state {
	case j2kSOC:
		if b != 0xff {
			return false, fmt.Errorf("%w: missing JPEG 2000 SOC", ErrLayout)
		}
		s.state = j2kSOCCode
	case j2kSOCCode:
		if b != 0x4f {
			return false, fmt.Errorf("%w: missing JPEG 2000 SOC", ErrLayout)
		}
		s.state = j2kMarker
	case j2kMarker:
		if b != 0xff {
			return false, fmt.Errorf("%w: expected JPEG 2000 marker", ErrLayout)
		}
		s.state = j2kMarkerCode
	case j2kMarkerCode:
		switch {
		case b == 0xd9:
			s.state = j2kEnd
			return true, nil
		case b == 0x90:
			s.marker = b
			s.state = j2kLengthHigh
		case b < 0x30 || b == 0x4f || b == 0x93:
			return false, fmt.Errorf("%w: unexpected JPEG 2000 marker FF%02X", ErrLayout, b)
		default:
			s.marker = b
			s.state = j2kLengthHigh
		}
	case j2kLengthHigh:
		s.length = int(b) << 8
		s.state = j2kLengthLow
	case j2kLengthLow:
		s.length |= int(b)
		if s.length < 2 {
			return false, fmt.Errorf("%w: invalid JPEG 2000 marker length", ErrLayout)
		}
		if s.marker == 0x90 {
			if s.length != 10 {
				return false, fmt.Errorf("%w: invalid SOT length", ErrLayout)
			}
			s.sotRead = 0
			s.state = j2kSOTFields
			return false, nil
		}
		s.remaining = int64(s.length - 2)
		s.state = j2kSegment
		if s.remaining == 0 {
			s.state = j2kMarker
		}
	case j2kSegment:
		s.remaining--
		if s.remaining == 0 {
			s.state = j2kMarker
		}
	case j2kSOTFields:
		s.sot[s.sotRead] = b
		s.sotRead++
		if s.sotRead < len(s.sot) {
			return false, nil
		}
		psot := int64(s.sot[2])<<24 | int64(s.sot[3])<<16 | int64(s.sot[4])<<8 | int64(s.sot[5])
		if psot == 0 {
			// The last tile-part extends to EOC: parse its header by lengths,
			// then scan packet data for EOC.
			s.state = j2kHeaderMarker
			return false, nil
		}
		// Psot counts from the first byte of the SOT marker: 12 bytes are read.
		if psot < 14 {
			return false, fmt.Errorf("%w: invalid Psot", ErrLayout)
		}
		s.remaining = psot - 12
		s.state = j2kTilePart
	case j2kTilePart:
		s.remaining--
		if s.remaining == 0 {
			s.state = j2kMarker
		}
	case j2kHeaderMarker:
		if b != 0xff {
			return false, fmt.Errorf("%w: expected tile-part header marker", ErrLayout)
		}
		s.state = j2kHeaderMarkerCode
	case j2kHeaderMarkerCode:
		if b == 0x93 { // SOD
			s.state = j2kBitstream
			return false, nil
		}
		if b < 0x30 || b == 0x4f || b == 0x90 || b == 0xd9 {
			return false, fmt.Errorf("%w: unexpected tile-part header marker FF%02X", ErrLayout, b)
		}
		s.state = j2kHeaderLengthHigh
	case j2kHeaderLengthHigh:
		s.length = int(b) << 8
		s.state = j2kHeaderLengthLow
	case j2kHeaderLengthLow:
		s.length |= int(b)
		if s.length < 2 {
			return false, fmt.Errorf("%w: invalid JPEG 2000 marker length", ErrLayout)
		}
		s.remaining = int64(s.length - 2)
		s.state = j2kHeaderSegment
		if s.remaining == 0 {
			s.state = j2kHeaderMarker
		}
	case j2kHeaderSegment:
		s.remaining--
		if s.remaining == 0 {
			s.state = j2kHeaderMarker
		}
	case j2kBitstream:
		if b == 0xff {
			s.state = j2kBitstreamCode
		}
	case j2kBitstreamCode:
		switch {
		case b == 0xd9:
			s.state = j2kEnd
			return true, nil
		case b <= 0x8f:
			// Bit stuffing guarantees 0xFF is followed by at most 0x8F in
			// packet data.
			s.state = j2kBitstream
		case b == 0x91 || b == 0x92: // SOP, EPH
			s.state = j2kBitstream
		case b == 0xff:
		default:
			return false, fmt.Errorf("%w: unexpected marker FF%02X in JPEG 2000 packet data", ErrLayout, b)
		}
	case j2kEnd:
		return false, fmt.Errorf("%w: data after EOC", ErrLayout)
	}
	return false, nil
}
