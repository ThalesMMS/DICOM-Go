package jpegls

import (
	"encoding/binary"
	"fmt"
	"io"
)

// bitWriter packs JPEG-LS entropy bits most significant first. After a
// 0xFF byte, T.87 A.1 stuffs a zero bit as the next byte's most significant
// bit; with the pending bits right-aligned in val, that is one more pending
// bit whose value is zero, so the stuffing costs no data movement. Bytes
// are emitted once 32 bits are pending, four at a time when none of them is
// 0xFF; padToByte emits everything, so the caller may append markers to
// buf after it.
type bitWriter struct {
	buf   []byte
	val   uint64 // pending bits, right-aligned
	count int    // number of pending bits, below 32 between calls
}

func (w *bitWriter) writeBit(bit uint32) {
	w.writeBits(bit&1, 1)
}

// writeBits appends the n low bits of value (n <= 32).
func (w *bitWriter) writeBits(value uint32, n int) {
	w.val = w.val<<uint(n) | uint64(value)&(1<<uint(n)-1)
	w.count += n
	for w.count >= 32 {
		word := uint32(w.val >> uint(w.count-32))
		if !hasFFByte(word) {
			w.buf = binary.BigEndian.AppendUint32(w.buf, word)
			w.count -= 32
			w.val &= 1<<uint(w.count) - 1
			continue
		}
		w.emitByte()
	}
}

// emitByte emits the eight most significant pending bits (count >= 8).
func (w *bitWriter) emitByte() {
	w.count -= 8
	b := byte(w.val >> uint(w.count))
	w.val &= 1<<uint(w.count) - 1
	w.buf = append(w.buf, b)
	if b == 0xff {
		w.count++ // the stuffed zero bit
	}
}

// hasFFByte reports whether a byte of v is 0xFF: a zero byte of ^v.
func hasFFByte(v uint32) bool {
	v = ^v
	return (v-0x01010101)&^v&0x80808080 != 0
}

func (w *bitWriter) writeOnes(n int) {
	for n > 32 {
		w.writeBits(0xffffffff, 32)
		n -= 32
	}
	w.writeBits(0xffffffff, n)
}

func (w *bitWriter) writeZeros(n int) {
	for n > 32 {
		w.writeBits(0, 32)
		n -= 32
	}
	w.writeBits(0, n)
}

// padToByte emits every pending bit, completing the last byte with zeros.
func (w *bitWriter) padToByte() {
	for w.count >= 8 {
		w.emitByte()
	}
	if w.count == 0 {
		return
	}
	w.writeZeros(8 - w.count)
	for w.count >= 8 {
		w.emitByte()
	}
}

// bitReader reads JPEG-LS entropy bits most significant first. It loads
// whole bytes ahead into val, applying the T.87 A.1 stuffing rule (a byte
// after 0xFF carries 7 bits and must have a zero most significant bit). A
// byte that cannot be loaded (the end of the data, or a marker after 0xFF)
// stops the look-ahead without being consumed; its error is returned only
// when a read needs more bits than were loaded, so errors surface at the
// same bit as with a reader that loads one byte at a time.
type bitReader struct {
	buf       []byte
	pos       int    // next byte to load
	val       uint64 // loaded bits not yet read, right-aligned
	count     int    // number of bits in val
	lastWasFF bool   // the last loaded byte was 0xFF
}

// fill loads bytes until val holds more than 56 bits or a byte cannot be
// loaded, whose error it returns.
func (r *bitReader) fill() error {
	for r.count <= 56 {
		if r.pos >= len(r.buf) {
			return io.ErrUnexpectedEOF
		}
		b := r.buf[r.pos]
		if r.lastWasFF {
			if b&0x80 != 0 {
				return fmt.Errorf("%w: unexpected marker in entropy data", ErrInvalidCodestream)
			}
			r.val = r.val<<7 | uint64(b)
			r.count += 7
			r.lastWasFF = false
		} else {
			r.val = r.val<<8 | uint64(b)
			r.count += 8
			r.lastWasFF = b == 0xff
		}
		r.pos++
	}
	return nil
}

// consume drops the n most significant loaded bits (n <= count).
func (r *bitReader) consume(n int) {
	r.count -= n
	r.val &= 1<<uint(r.count) - 1
}

// fail reads every loaded bit, as a bit-by-bit reader would before
// reaching the byte that failed, and returns err.
func (r *bitReader) fail(err error) error {
	r.val, r.count = 0, 0
	return err
}

func (r *bitReader) readBit() (uint32, error) {
	if r.count == 0 {
		if err := r.fill(); r.count == 0 {
			return 0, err
		}
	}
	r.count--
	bit := uint32(r.val>>uint(r.count)) & 1
	r.val &= 1<<uint(r.count) - 1
	return bit, nil
}

// readBits reads n bits (n <= 40; like a bit-by-bit reader, only the low 32
// of more than 32 bits are returned).
func (r *bitReader) readBits(n int) (uint32, error) {
	if r.count < n {
		if err := r.fill(); r.count < n {
			return 0, r.fail(err)
		}
	}
	value := uint32(r.val >> uint(r.count-n))
	if n < 32 {
		value &= 1<<uint(n) - 1
	}
	r.consume(n)
	return value, nil
}
