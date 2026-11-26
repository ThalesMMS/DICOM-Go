package htj2k

// The cleanup segment holds three bit streams. MagSgn grows forward from the
// start of the segment, LSB first; MEL grows forward from the end of MagSgn,
// MSB first; VLC grows backward from the end of the segment, LSB first. Each
// stream stuffs bits so that the segment never contains a marker code: the
// forward streams leave the top bit of the byte after an 0xFF clear, and the
// backward stream leaves the top bit clear when a byte would otherwise be
// 0xFF and follow one above 0x8F.

// magSgnWriter is the forward, LSB-first MagSgn stream.
type magSgnWriter struct {
	buf  []byte
	tmp  uint32
	used uint
	max  uint // 8, or 7 after an 0xFF byte
}

func (w *magSgnWriter) put(bits uint32, n uint) {
	for n > 0 {
		take := w.max - w.used
		if take > n {
			take = n
		}
		w.tmp |= (bits & (1<<take - 1)) << w.used
		w.used += take
		bits >>= take
		n -= take
		if w.used == w.max {
			b := byte(w.tmp)
			w.buf = append(w.buf, b)
			w.max = 8
			if b == 0xFF {
				w.max = 7
			}
			w.tmp, w.used = 0, 0
		}
	}
}

// finish pads the last byte with ones and drops trailing 0xFF bytes: a
// decoder reads past the end of MagSgn as if it were 0xFF, so they carry
// nothing, and a segment must not hold an 0xFF before the MEL bytes.
func (w *magSgnWriter) finish() []byte {
	if w.used > 0 {
		w.tmp |= (0xFF << w.used) & (1<<w.max - 1)
		w.buf = append(w.buf, byte(w.tmp))
		w.tmp, w.used = 0, 0
	}
	for len(w.buf) > 0 && w.buf[len(w.buf)-1] == 0xFF {
		w.buf = w.buf[:len(w.buf)-1]
	}
	return w.buf
}

// melWriter is the forward, MSB-first MEL stream with its run-length coder.
type melWriter struct {
	buf       []byte
	tmp       uint8
	remaining uint // free bits in tmp
	state     int
	run       int
}

func newMELWriter() melWriter { return melWriter{remaining: 8} }

func (w *melWriter) bit(b uint8) {
	w.tmp = w.tmp<<1 | b
	w.remaining--
	if w.remaining == 0 {
		w.buf = append(w.buf, w.tmp)
		w.remaining = 8
		if w.tmp == 0xFF {
			w.remaining = 7
		}
		w.tmp = 0
	}
}

// event codes one MEL symbol. A run of 2^E zeros is one 1 bit; a one ends
// a shorter run with a 0 bit and the run length in E bits.
func (w *melWriter) event(one bool) {
	e := int(melExponent[w.state])
	if !one {
		w.run++
		if w.run == 1<<e {
			w.bit(1)
			w.run = 0
			if w.state < 12 {
				w.state++
			}
		}
		return
	}
	w.bit(0)
	for i := e - 1; i >= 0; i-- {
		w.bit(uint8(w.run>>i) & 1)
	}
	w.run = 0
	if w.state > 0 {
		w.state--
	}
}

// finish closes an open run (a decoder never asks for the zeros it implies
// beyond the last event) and pads the last byte with zeros. A final 0xFF is
// followed by a zero byte so that the next byte, the first of the VLC
// stream, cannot complete a marker code.
func (w *melWriter) finish() []byte {
	if w.run > 0 {
		w.bit(1)
	}
	full := uint(8)
	if len(w.buf) > 0 && w.buf[len(w.buf)-1] == 0xFF {
		full = 7
	}
	if w.remaining != full {
		w.buf = append(w.buf, w.tmp<<w.remaining)
	} else if len(w.buf) > 0 && w.buf[len(w.buf)-1] == 0xFF {
		w.buf = append(w.buf, 0)
	}
	return w.buf
}

// vlcWriter is the backward, LSB-first VLC stream. bytes holds the stream in
// the order it is written, the last byte of the segment first.
type vlcWriter struct {
	bytes   []byte
	tmp     uint32
	used    uint
	stuffed bool // the previous byte was above 0x8F
}

// newVLCWriter starts the stream the way the segment ends: a last byte and
// the low half of the one before it are reserved for Scup, and both count as
// above 0x8F for stuffing.
func newVLCWriter() vlcWriter {
	return vlcWriter{bytes: []byte{0xFF}, tmp: 0x0F, used: 4, stuffed: true}
}

func (w *vlcWriter) put(bits uint32, n uint) {
	for ; n > 0; n-- {
		w.tmp |= (bits & 1) << w.used
		bits >>= 1
		w.used++
		if w.used == 7 && w.stuffed && w.tmp == 0x7F {
			// Seven ones after a byte above 0x8F: the byte keeps a zero top
			// bit and the stream continues in the next one.
			w.emit()
			continue
		}
		if w.used == 8 {
			w.emit()
		}
	}
}

func (w *vlcWriter) emit() {
	b := byte(w.tmp)
	w.bytes = append(w.bytes, b)
	w.stuffed = b > 0x8F
	w.tmp, w.used = 0, 0
}

// finish returns the stream in segment order.
func (w *vlcWriter) finish() []byte {
	if w.used > 0 {
		w.emit()
	}
	out := make([]byte, len(w.bytes))
	for i, b := range w.bytes {
		out[len(out)-1-i] = b
	}
	return out
}

// putUVLC writes the prefix of a u value (or u-2 in the initial row when
// both quads carry one): 1 -> "1", 2 -> "01", 3..4 -> "001", 5.. -> "000".
func (w *vlcWriter) putUPrefix(u uint32) {
	switch {
	case u == 1:
		w.put(1, 1)
	case u == 2:
		w.put(2, 2)
	case u <= 4:
		w.put(4, 3)
	default:
		w.put(0, 3)
	}
}

// putUSuffix writes the suffix and, for u above 32, the extension.
func (w *vlcWriter) putUSuffix(u uint32) {
	switch {
	case u <= 2:
	case u <= 4:
		w.put(u-3, 1)
	case u <= 32:
		w.put(u-5, 5)
	default:
		w.put(28+(u-33)&3, 5)
		w.put((u-33)>>2, 4)
	}
}
