package jpegxl

import (
	"math"
	"math/bits"
)

// bitWriter is the encoder counterpart of bitReader (LSB first).
type bitWriter struct {
	buf []byte
	acc uint64
	n   uint
}

func (w *bitWriter) write(n uint, v uint64) {
	for n > 32 {
		w.write(32, v&0xFFFFFFFF)
		v >>= 32
		n -= 32
	}
	if n == 0 {
		return
	}
	w.acc |= (v & (1<<n - 1)) << w.n
	w.n += n
	for w.n >= 8 {
		w.buf = append(w.buf, byte(w.acc))
		w.acc >>= 8
		w.n -= 8
	}
}

func (w *bitWriter) bool(b bool) {
	if b {
		w.write(1, 1)
	} else {
		w.write(1, 0)
	}
}

func (w *bitWriter) zeroPad() {
	if w.n > 0 {
		w.write(8-w.n, 0)
	}
}

func (w *bitWriter) bytes() []byte {
	w.zeroPad()
	return w.buf
}

// bitLen is the number of bits written so far.
func (w *bitWriter) bitLen() uint64 { return uint64(len(w.buf))*8 + uint64(w.n) }

// appendWriter appends the bits of other (which need not be byte aligned).
func (w *bitWriter) appendWriter(o *bitWriter) {
	for _, b := range o.buf {
		w.write(8, uint64(b))
	}
	w.write(o.n, o.acc)
}

// u32 writes v with the first distribution that can represent it.
func (w *bitWriter) u32(v uint32, d0, d1, d2, d3 u32Dist) {
	for sel, d := range [4]u32Dist{d0, d1, d2, d3} {
		if v < d.offset {
			continue
		}
		rest := uint64(v - d.offset)
		if d.bits == 0 && rest != 0 {
			continue
		}
		if d.bits < 32 && rest >= 1<<d.bits {
			continue
		}
		w.write(2, uint64(sel))
		w.write(uint(d.bits), rest)
		return
	}
	panic("jpegxl: value not representable in U32 field")
}

func (w *bitWriter) u64(v uint64) {
	switch {
	case v == 0:
		w.write(2, 0)
	case v <= 16:
		w.write(2, 1)
		w.write(4, v-1)
	case v <= 272:
		w.write(2, 2)
		w.write(8, v-17)
	default:
		w.write(2, 3)
		w.write(12, v&0xFFF)
		v >>= 12
		shift := 12
		for v > 0 {
			w.write(1, 1)
			if shift == 60 {
				w.write(4, v)
				return
			}
			w.write(8, v&0xFF)
			v >>= 8
			shift += 8
		}
		w.write(1, 0)
	}
}

// f16 writes a float as IEEE half precision, truncating the mantissa like
// the reference encoder.
func (w *bitWriter) f16(f float32) {
	w.write(16, uint64(float16Bits(f)))
}

func float16Bits(f float32) uint16 {
	b := math.Float32bits(f)
	sign := uint16(b>>31) << 15
	exp := int32(b>>23&0xFF) - 127
	mant := b & 0x7FFFFF
	if f == 0 || exp < -24 {
		return sign
	}
	if exp < -14 {
		// Subnormal half.
		m := (mant | 0x800000) >> uint(-exp-14+13)
		return sign | uint16(m)
	}
	if exp > 15 {
		return sign | 0x7BFF
	}
	return sign | uint16(exp+15)<<10 | uint16(mant>>13)
}

func (w *bitWriter) enum(v uint32) {
	w.u32(v, val(0), val(1), bitsOff(4, 2), bitsOff(6, 18))
}

func packSigned(v int32) uint32 {
	if v < 0 {
		return uint32(-(int64(v)+1))*2 + 1
	}
	return uint32(v) * 2
}

func floorLog2u32(v uint32) uint { return uint(bits.Len32(v)) - 1 }
