package video

import (
	"bytes"
	"errors"
	"fmt"
)

// Annex B byte streams (ITU-T H.264 Annex B, H.265 Annex B) and RBSP
// bit reading for the parameter sets and slice headers the MP4 muxer needs.

var (
	// ErrMalformedStream reports an H.264 or HEVC elementary stream whose
	// NAL units or headers cannot be parsed.
	ErrMalformedStream = errors.New("dicom: malformed video elementary stream")
	// ErrUnsupportedStream reports a valid elementary stream feature the MP4
	// muxer does not handle (for example interlaced field pictures).
	ErrUnsupportedStream = errors.New("dicom: unsupported video elementary stream")
)

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformedStream, fmt.Sprintf(format, args...))
}

func unsupportedStream(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupportedStream, fmt.Sprintf(format, args...))
}

var startCode = []byte{0, 0, 1}

// splitAnnexB returns the NAL units of an Annex B byte stream, without start
// codes and trailing zero bytes. The slices alias data.
func splitAnnexB(data []byte, maxUnits int) ([][]byte, error) {
	first := bytes.Index(data, startCode)
	if first < 0 {
		return nil, malformed("no Annex B start code")
	}
	for _, b := range data[:first] {
		if b != 0 {
			return nil, malformed("data before the first start code")
		}
	}
	var units [][]byte
	pos := first + 3
	for pos <= len(data) {
		next := bytes.Index(data[pos:], startCode)
		end := len(data)
		if next >= 0 {
			end = pos + next
		}
		unit := data[pos:end]
		for len(unit) > 0 && unit[len(unit)-1] == 0 {
			unit = unit[:len(unit)-1] // trailing_zero_8bits and the 4-byte start code's zero
		}
		if len(unit) > 0 {
			if len(units) >= maxUnits {
				return nil, fmt.Errorf("%w: more than %d NAL units", ErrTooManyFragments, maxUnits)
			}
			units = append(units, unit)
		}
		if next < 0 {
			break
		}
		pos = end + 3
	}
	if len(units) == 0 {
		return nil, malformed("no NAL units")
	}
	return units, nil
}

// rbsp removes emulation prevention bytes (00 00 03) from a NAL unit.
func rbsp(nal []byte) []byte {
	if bytes.Index(nal, []byte{0, 0, 3}) < 0 {
		return nal
	}
	out := make([]byte, 0, len(nal))
	zeros := 0
	for _, b := range nal {
		if zeros >= 2 && b == 3 {
			zeros = 0
			continue
		}
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, b)
	}
	return out
}

// bitReader reads RBSP fields MSB first. Reading past the end sets err.
type bitReader struct {
	data []byte
	pos  int // bit position
	err  error
}

func newBitReader(data []byte) *bitReader { return &bitReader{data: data} }

func (r *bitReader) u(n int) uint32 {
	var v uint32
	for i := 0; i < n; i++ {
		if r.pos >= 8*len(r.data) {
			if r.err == nil {
				r.err = malformed("truncated header")
			}
			return 0
		}
		v = v<<1 | uint32(r.data[r.pos>>3]>>(7-r.pos&7))&1
		r.pos++
	}
	return v
}

func (r *bitReader) flag() bool { return r.u(1) == 1 }

func (r *bitReader) skip(n int) {
	if r.pos+n > 8*len(r.data) {
		r.pos = 8 * len(r.data)
		if r.err == nil {
			r.err = malformed("truncated header")
		}
		return
	}
	r.pos += n
}

// ue reads an unsigned Exp-Golomb code.
func (r *bitReader) ue() uint32 {
	zeros := 0
	for r.u(1) == 0 {
		if r.err != nil {
			return 0
		}
		zeros++
		if zeros > 31 {
			r.err = malformed("Exp-Golomb code too long")
			return 0
		}
	}
	return (1<<zeros - 1) + r.u(zeros)
}

// se reads a signed Exp-Golomb code.
func (r *bitReader) se() int32 {
	v := r.ue()
	if v&1 == 1 {
		return int32((v + 1) / 2)
	}
	return -int32(v / 2)
}

// ueMax reads ue() and checks it against an upper bound.
func (r *bitReader) ueMax(limit uint32, field string) uint32 {
	v := r.ue()
	if v > limit && r.err == nil {
		r.err = malformed("%s %d exceeds %d", field, v, limit)
	}
	return v
}
