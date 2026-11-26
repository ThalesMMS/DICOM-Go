package jpegxl

import (
	"context"
	"math"
	"math/bits"
)

// bitReader reads the JPEG XL bit stream: least significant bit first within
// each byte (ISO/IEC 18181-1 Annex B). Reads past the end yield zero bits and
// set a sticky error so callers can check once per structure.
type bitReader struct {
	ctx   context.Context // set for the cancellable XYB coefficient path
	data  []byte
	pos   int    // next byte to load into buf
	buf   uint64 // pending bits, LSB first
	nbuf  uint   // valid bits in buf
	err   error
	total uint64 // bits consumed
}

func newBitReader(data []byte) *bitReader { return &bitReader{data: data} }

func (br *bitReader) checkpoint() error {
	if br.err == nil && br.ctx != nil {
		return br.ctx.Err()
	}
	return br.err
}

func (br *bitReader) refill() {
	for br.nbuf <= 56 {
		if br.pos < len(br.data) {
			br.buf |= uint64(br.data[br.pos]) << br.nbuf
			br.pos++
			br.nbuf += 8
			continue
		}
		return
	}
}

func (br *bitReader) readBits(n uint) uint64 {
	if n == 0 {
		return 0
	}
	if n > 32 {
		lo := br.readBits(32)
		return lo | br.readBits(n-32)<<32
	}
	if br.nbuf < n {
		br.refill()
		if br.nbuf < n {
			br.fail(errTruncated)
			br.total += uint64(n)
			br.buf, br.nbuf = 0, 0
			return 0
		}
	}
	v := br.buf & (1<<n - 1)
	br.buf >>= n
	br.nbuf -= n
	br.total += uint64(n)
	return v
}

func (br *bitReader) peekBits(n uint) uint64 {
	if br.nbuf < n {
		br.refill()
	}
	return br.buf & (1<<n - 1)
}

func (br *bitReader) skipBits(n uint) {
	if br.nbuf < n {
		br.refill()
		if br.nbuf < n {
			br.fail(errTruncated)
			br.total += uint64(n)
			br.buf, br.nbuf = 0, 0
			return
		}
	}
	br.buf >>= n
	br.nbuf -= n
	br.total += uint64(n)
}

func (br *bitReader) u(n uint) uint32 { return uint32(br.readBits(n)) }

func (br *bitReader) bool() bool { return br.readBits(1) == 1 }

func (br *bitReader) fail(err error) {
	if br.err == nil {
		br.err = err
	}
}

// bitsConsumed is the number of bits read so far.
func (br *bitReader) bitsConsumed() uint64 { return br.total }

// skipLarge skips n bits, which may exceed the buffer size.
func (br *bitReader) skipLarge(n uint64) {
	for n > 0 {
		k := uint(min(n, 32))
		br.readBits(k)
		n -= uint64(k)
		if br.err != nil {
			return
		}
	}
}

// zeroPadToByte skips to the next byte boundary; JPEG XL requires zero bits.
func (br *bitReader) zeroPadToByte() {
	if r := br.total % 8; r != 0 {
		if br.readBits(uint(8-r)) != 0 {
			br.fail(corrupt("non-zero padding bits"))
		}
	}
}

// bytePos returns the byte offset of the next unread bit; only meaningful at a
// byte boundary.
func (br *bitReader) bytePos() int { return int(br.total / 8) }

// u32Dist is one of the four distributions of a U32 field.
type u32Dist struct {
	bits   uint8
	offset uint32
}

func val(v uint32) u32Dist                { return u32Dist{0, v} }
func bitsOff(n uint8, off uint32) u32Dist { return u32Dist{n, off} }
func bitsN(n uint8) u32Dist               { return u32Dist{n, 0} }

func (br *bitReader) u32(d0, d1, d2, d3 u32Dist) uint32 {
	d := [4]u32Dist{d0, d1, d2, d3}[br.u(2)]
	return d.offset + br.u(uint(d.bits))
}

func (br *bitReader) u64() uint64 {
	switch br.u(2) {
	case 0:
		return 0
	case 1:
		return 1 + uint64(br.u(4))
	case 2:
		return 17 + uint64(br.u(8))
	}
	v := uint64(br.u(12))
	shift := uint(12)
	for br.bool() {
		if shift == 60 {
			v |= uint64(br.u(4)) << shift
			break
		}
		v |= uint64(br.u(8)) << shift
		shift += 8
	}
	return v
}

func (br *bitReader) f16() float32 {
	b := br.u(16)
	sign := b >> 15
	exp := (b >> 10) & 31
	mant := b & 1023
	if exp == 31 {
		br.fail(corrupt("non-finite F16 value"))
		return 0
	}
	var f float64
	if exp == 0 {
		f = float64(mant) / 1024 * math.Pow(2, -14)
	} else {
		f = (1 + float64(mant)/1024) * math.Pow(2, float64(int(exp)-15))
	}
	if sign == 1 {
		f = -f
	}
	return float32(f)
}

func (br *bitReader) enum() uint32 {
	v := br.u32(val(0), val(1), bitsOff(4, 2), bitsOff(6, 18))
	if v >= 64 {
		br.fail(corrupt("enum value out of range"))
	}
	return v
}

// extensions reads an Extensions field and skips the extension payloads.
func (br *bitReader) extensions() {
	ext := br.u64()
	var total uint64
	for ext != 0 {
		i := bits.TrailingZeros64(ext)
		ext &^= 1 << uint(i)
		n := br.u64()
		if total+n < total {
			br.fail(corrupt("extension length overflow"))
			return
		}
		total += n
	}
	br.skipLarge(total)
}

func unpackSigned(v uint32) int32 {
	if v&1 == 1 {
		return -int32(v>>1) - 1
	}
	return int32(v >> 1)
}

func unpackSigned64(v uint64) int64 {
	if v&1 == 1 {
		return -int64(v>>1) - 1
	}
	return int64(v >> 1)
}

func ceilLog2(x uint64) uint {
	if x <= 1 {
		return 0
	}
	return uint(bits.Len64(x - 1))
}

func floorLog2(x uint64) uint { return uint(bits.Len64(x)) - 1 }

func divCeil(a, b int) int { return (a + b - 1) / b }
