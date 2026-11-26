package jpeg2000

// Tier-1 of ISO/IEC 15444-1 (Annex D): the embedded block coder. Every
// bit-plane of a code-block, from the most significant one down to zero, is
// coded by up to three passes (significance propagation, magnitude
// refinement and cleanup) through the MQ arithmetic coder of Annex C. The
// whole code-block forms one codeword segment, terminated once at its end:
// no coding-style option of Table A.19 is used.

// mqState is one row of Table C.2.
type mqState struct {
	qe         uint32
	nmps, nlps uint8
	switchMPS  bool
}

var mqStates = [47]mqState{
	{0x5601, 1, 1, true}, {0x3401, 2, 6, false}, {0x1801, 3, 9, false},
	{0x0AC1, 4, 12, false}, {0x0521, 5, 29, false}, {0x0221, 38, 33, false},
	{0x5601, 7, 6, true}, {0x5401, 8, 14, false}, {0x4801, 9, 14, false},
	{0x3801, 10, 14, false}, {0x3001, 11, 17, false}, {0x2401, 12, 18, false},
	{0x1C01, 13, 20, false}, {0x1601, 29, 21, false}, {0x5601, 15, 14, true},
	{0x5401, 16, 14, false}, {0x5101, 17, 15, false}, {0x4801, 18, 16, false},
	{0x3801, 19, 17, false}, {0x3401, 20, 18, false}, {0x3001, 21, 19, false},
	{0x2801, 22, 19, false}, {0x2401, 23, 20, false}, {0x2201, 24, 21, false},
	{0x1C01, 25, 22, false}, {0x1801, 26, 23, false}, {0x1601, 27, 24, false},
	{0x1401, 28, 25, false}, {0x1201, 29, 26, false}, {0x1101, 30, 27, false},
	{0x0AC1, 31, 28, false}, {0x09C1, 32, 29, false}, {0x08A1, 33, 30, false},
	{0x0521, 34, 31, false}, {0x0441, 35, 32, false}, {0x02A1, 36, 33, false},
	{0x0221, 37, 34, false}, {0x0141, 38, 35, false}, {0x0111, 39, 36, false},
	{0x0085, 40, 37, false}, {0x0049, 41, 38, false}, {0x0025, 42, 39, false},
	{0x0015, 43, 40, false}, {0x0009, 44, 41, false}, {0x0005, 45, 42, false},
	{0x0001, 45, 43, false}, {0x5601, 46, 46, false},
}

// Contexts of Annex D: nine for zero coding, five for signs, three for
// refinement, then run-length and uniform.
const (
	cxSign     = 9
	cxRefine   = 14
	cxRunLen   = 17
	cxUniform  = 18
	numContext = 19
)

// mqEncoder is the MQ coder of C.2, with the byte buffer's first position
// standing in for the byte before the codeword (BPST - 1).
type mqEncoder struct {
	a, c  uint32
	ct    uint
	out   []byte // out[0] is the byte before the codeword
	index [numContext]uint8
	mps   [numContext]uint8
}

func (e *mqEncoder) reset() {
	e.a, e.c, e.ct = 0x8000, 0, 12
	e.out = append(e.out[:0], 0)
	for i := range e.index {
		e.index[i], e.mps[i] = 0, 0
	}
	e.index[cxUniform] = 46
	e.index[cxRunLen] = 3
	e.index[0] = 4
}

func (e *mqEncoder) encode(cx int, d uint8) {
	s := &mqStates[e.index[cx]]
	e.a -= s.qe
	if d == e.mps[cx] {
		if e.a&0x8000 != 0 {
			e.c += s.qe
			return
		}
		if e.a < s.qe {
			e.a = s.qe
		} else {
			e.c += s.qe
		}
		e.index[cx] = s.nmps
	} else {
		if e.a < s.qe {
			e.c += s.qe
		} else {
			e.a = s.qe
		}
		if s.switchMPS {
			e.mps[cx] = 1 - e.mps[cx]
		}
		e.index[cx] = s.nlps
	}
	for e.a&0x8000 == 0 {
		e.a <<= 1
		e.c <<= 1
		e.ct--
		if e.ct == 0 {
			e.byteOut()
		}
	}
}

func (e *mqEncoder) byteOut() {
	last := len(e.out) - 1
	if e.out[last] == 0xFF {
		e.out = append(e.out, byte(e.c>>20))
		e.c &= 0xFFFFF
		e.ct = 7
		return
	}
	if e.c < 0x8000000 {
		e.out = append(e.out, byte(e.c>>19))
		e.c &= 0x7FFFF
		e.ct = 8
		return
	}
	e.out[last]++
	if e.out[last] == 0xFF {
		e.c &= 0x7FFFFFF
		e.out = append(e.out, byte(e.c>>20))
		e.c &= 0xFFFFF
		e.ct = 7
		return
	}
	e.out = append(e.out, byte(e.c>>19))
	e.c &= 0x7FFFF
	e.ct = 8
}

// flush terminates the codeword (C.2.9) and returns it.
func (e *mqEncoder) flush() []byte {
	temp := e.c + e.a
	e.c |= 0xFFFF
	if e.c >= temp {
		e.c -= 0x8000
	}
	e.c <<= e.ct
	e.byteOut()
	e.c <<= e.ct
	e.byteOut()
	out := e.out[1:]
	if n := len(out); n > 0 && out[n-1] == 0xFF {
		out = out[:n-1]
	}
	return out
}

// Sample state flags of the block coder.
const (
	flagSig     = 1 << iota // significant
	flagNeg                 // negative, once significant
	flagVisited             // coded in this bit-plane's significance pass
	flagRefined             // refined at least once
)

// Band orientations for the zero-coding contexts of Table D.1.
const (
	orientLL = iota
	orientHL
	orientLH
	orientHH
)

// zeroContext is Table D.1 from the counts of significant horizontal,
// vertical and diagonal neighbors. HL is the LL/LH table with h and v
// exchanged.
func zeroContext(orientation, h, v, d int) int {
	if orientation == orientHL {
		h, v = v, h
	}
	if orientation == orientHH {
		hv := h + v
		switch {
		case d >= 3:
			return 8
		case d == 2:
			if hv >= 1 {
				return 7
			}
			return 6
		case d == 1:
			if hv >= 2 {
				return 5
			}
			return 3 + hv
		default:
			if hv >= 2 {
				return 2
			}
			return hv
		}
	}
	switch {
	case h == 2:
		return 8
	case h == 1:
		switch {
		case v >= 1:
			return 7
		case d >= 1:
			return 6
		default:
			return 5
		}
	case v == 2:
		return 4
	case v == 1:
		return 3
	case d >= 2:
		return 2
	default:
		return d
	}
}

// blockCoder codes one code-block. Its flags carry a one-sample border so
// that neighbors outside the block read as insignificant.
type blockCoder struct {
	w, h, stride int
	orientation  int
	mag          []uint32
	flags        []uint8
	mq           mqEncoder
}

// encodeBlock codes the quantization indices of a w x h code-block and
// returns its codeword, the number of coding passes and the number of
// magnitude bit-planes K. A block with no nonzero index has no passes.
func (b *blockCoder) encodeBlock(coeffs []int32, w, h, orientation int) ([]byte, int, int) {
	b.w, b.h, b.stride, b.orientation = w, h, w+2, orientation
	n := (w + 2) * (h + 2)
	if cap(b.flags) < n {
		b.flags = make([]uint8, n)
		b.mag = make([]uint32, n)
	}
	b.flags, b.mag = b.flags[:n], b.mag[:n]
	for i := range b.flags {
		b.flags[i], b.mag[i] = 0, 0
	}
	var maxMag uint32
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := coeffs[y*w+x]
			i := (y+1)*b.stride + x + 1
			if v < 0 {
				b.mag[i] = uint32(-v)
				b.flags[i] = flagNeg
			} else {
				b.mag[i] = uint32(v)
			}
			if b.mag[i] > maxMag {
				maxMag = b.mag[i]
			}
		}
	}
	planes := 0
	for maxMag > 0 {
		planes++
		maxMag >>= 1
	}
	if planes == 0 {
		return nil, 0, 0
	}
	b.mq.reset()
	for p := planes - 1; p >= 0; p-- {
		if p != planes-1 {
			b.significancePass(uint(p))
			b.refinementPass(uint(p))
		}
		b.cleanupPass(uint(p))
		for i := range b.flags {
			b.flags[i] &^= flagVisited
		}
	}
	data := b.mq.flush()
	out := make([]byte, len(data))
	copy(out, data)
	return out, 3*planes - 2, planes
}

func (b *blockCoder) neighbors(i int) (h, v, d int) {
	f := b.flags
	s := b.stride
	h = int(f[i-1]&flagSig + f[i+1]&flagSig)
	v = int(f[i-s]&flagSig + f[i+s]&flagSig)
	d = int(f[i-s-1]&flagSig + f[i-s+1]&flagSig + f[i+s-1]&flagSig + f[i+s+1]&flagSig)
	return
}

func (b *blockCoder) contribution(i int) int {
	switch {
	case b.flags[i]&flagSig == 0:
		return 0
	case b.flags[i]&flagNeg != 0:
		return -1
	default:
		return 1
	}
}

func clampUnit(v int) int {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}

// codeSign codes a newly significant sample's sign (Tables D.3, D.4).
func (b *blockCoder) codeSign(i int) {
	s := b.stride
	h := clampUnit(b.contribution(i-1) + b.contribution(i+1))
	v := clampUnit(b.contribution(i-s) + b.contribution(i+s))
	var cx int
	var xor uint8
	switch h {
	case 0:
		cx = cxSign
		if v != 0 {
			cx = cxSign + 1
		}
		if v < 0 {
			xor = 1
		}
	case 1:
		cx = cxSign + 3 + v
		if v < 0 {
			cx = cxSign + 2
		}
	default:
		xor = 1
		switch v {
		case 0:
			cx = cxSign + 3
		case 1:
			cx = cxSign + 2
		default:
			cx = cxSign + 4
		}
	}
	sign := uint8(0)
	if b.flags[i]&flagNeg != 0 {
		sign = 1
	}
	b.mq.encode(cx, sign^xor)
}

func (b *blockCoder) bit(i int, p uint) uint8 {
	return uint8(b.mag[i] >> p & 1)
}

// significancePass is D.3.1: insignificant samples with a significant
// neighbor.
func (b *blockCoder) significancePass(p uint) {
	for y0 := 0; y0 < b.h; y0 += 4 {
		for x := 0; x < b.w; x++ {
			for y := y0; y < y0+4 && y < b.h; y++ {
				i := (y+1)*b.stride + x + 1
				if b.flags[i]&flagSig != 0 {
					continue
				}
				h, v, d := b.neighbors(i)
				if h+v+d == 0 {
					continue
				}
				bit := b.bit(i, p)
				b.mq.encode(zeroContext(b.orientation, h, v, d), bit)
				b.flags[i] |= flagVisited
				if bit != 0 {
					b.codeSign(i)
					b.flags[i] |= flagSig
				}
			}
		}
	}
}

// refinementPass is D.3.3: samples significant before this bit-plane.
func (b *blockCoder) refinementPass(p uint) {
	for y0 := 0; y0 < b.h; y0 += 4 {
		for x := 0; x < b.w; x++ {
			for y := y0; y < y0+4 && y < b.h; y++ {
				i := (y+1)*b.stride + x + 1
				if b.flags[i]&(flagSig|flagVisited) != flagSig {
					continue
				}
				cx := cxRefine + 2
				if b.flags[i]&flagRefined == 0 {
					cx = cxRefine
					if h, v, d := b.neighbors(i); h+v+d > 0 {
						cx = cxRefine + 1
					}
				}
				b.mq.encode(cx, b.bit(i, p))
				b.flags[i] |= flagRefined
			}
		}
	}
}

// cleanupPass is D.3.4: every sample the significance pass did not code and
// that is not yet significant, with run-length coding of quiet columns.
func (b *blockCoder) cleanupPass(p uint) {
	for y0 := 0; y0 < b.h; y0 += 4 {
		for x := 0; x < b.w; x++ {
			y := y0
			if y0+4 <= b.h && b.quietColumn(x, y0) {
				first := -1
				for k := 0; k < 4; k++ {
					if b.bit((y0+k+1)*b.stride+x+1, p) != 0 {
						first = k
						break
					}
				}
				if first < 0 {
					b.mq.encode(cxRunLen, 0)
					continue
				}
				b.mq.encode(cxRunLen, 1)
				b.mq.encode(cxUniform, uint8(first>>1))
				b.mq.encode(cxUniform, uint8(first&1))
				i := (y0+first+1)*b.stride + x + 1
				b.codeSign(i)
				b.flags[i] |= flagSig
				y = y0 + first + 1
			}
			for ; y < y0+4 && y < b.h; y++ {
				i := (y+1)*b.stride + x + 1
				if b.flags[i]&(flagSig|flagVisited) != 0 {
					continue
				}
				h, v, d := b.neighbors(i)
				bit := b.bit(i, p)
				b.mq.encode(zeroContext(b.orientation, h, v, d), bit)
				if bit != 0 {
					b.codeSign(i)
					b.flags[i] |= flagSig
				}
			}
		}
	}
}

// quietColumn reports whether a full stripe column qualifies for run-length
// coding: no sample significant or already coded, and no significant
// neighbor.
func (b *blockCoder) quietColumn(x, y0 int) bool {
	for k := 0; k < 4; k++ {
		i := (y0+k+1)*b.stride + x + 1
		if b.flags[i]&(flagSig|flagVisited) != 0 {
			return false
		}
		if h, v, d := b.neighbors(i); h+v+d != 0 {
			return false
		}
	}
	return true
}
