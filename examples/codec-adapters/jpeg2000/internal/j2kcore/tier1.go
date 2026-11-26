// Adapted from github.com/amrshadid/go-dicom compress/jpeg2000/tier1.go at
// commit daecea14be3fbc27300aff20912ed7c8c8a0d599 (v1.6.0, MIT License, see
// LICENSE.amrshadid-go-dicom and PROVENANCE.md in this directory).

package j2kcore

import "github.com/ThalesMMS/dicom-go/internal/htj2k"

// The tier-1 block decoder, PS 15444-1 Annex D.
//
// A code-block's bytes are one MQ-coded stream, and this is what reads it. The
// coefficients are decoded a bit plane at a time, most significant first, and
// each plane in three passes: significance propagation, magnitude refinement,
// and cleanup. Which pass a coefficient's bit belongs to depends on what its
// neighbors have done so far, so the passes are not independent — they walk
// the same block in the same order, and the order is what keeps encoder and
// decoder in step.
//
// The contexts are the point of the whole design: a bit is coded against a
// probability learned from the eight neighbors, so a smooth image spends
// almost nothing on the long runs of zeros that make up most of a subband.
//
// The code-block style options (D.6-D.7) are decoded too: the block's bytes
// then come as several codeword segments, each restarting the arithmetic
// decoder (or, with the bypass, read as raw bits); contexts may be reset
// between passes, the context of a stripe's last row may ignore the stripe
// below, and each cleanup pass may end with a segmentation symbol.

// The 19 contexts (Table D.1 and following): nine for significance, one for the
// run of a cleanup pass, five for sign, three for refinement, and one uniform.
const (
	cxZero       = 0 // significance, contexts 0..8
	cxSignFirst  = 9 // sign, contexts 9..13
	cxRefineNone = 14
	cxRefineNear = 15
	cxRefineMore = 16
	cxRunLen     = 17
	cxUniform    = 18
	numContexts  = 19
)

// Coefficient state, one word per sample.
//
// The low bits are the coefficient's own state. The high bits say which of its
// eight neighbors are significant, and they are written by the neighbor when it
// becomes significant rather than read back when a context is needed: the
// context of a sample is then a table lookup on those eight bits, instead of
// eight loads and a count. Tier-1 asks for a context far more often than a
// coefficient becomes significant, so paying at the write is much the cheaper
// side — it was 43% of a decode before the change.
const (
	flagSignificant = 1 << iota
	flagVisited     // coded in the significance pass of this bit plane
	flagRefined     // has been through at least one refinement pass
	flagNegative

	// The eight neighbors, in the order the lookup table counts them: the two
	// horizontal, the two vertical, then the four diagonal.
	flagLeft
	flagRight
	flagUp
	flagDown
	flagNW
	flagNE
	flagSW
	flagSE

	flagNeighbors = flagLeft | flagRight | flagUp | flagDown |
		flagNW | flagNE | flagSW | flagSE
	neighborShift = 4 // where the neighbor bits start
)

// blockDecoder decodes one code-block.
//
// The sample grid is padded by one on every side, so the eight neighbors of a
// real sample always exist and no neighbor lookup needs a bounds test. The
// padding stays zero, which is exactly what a sample outside the block should
// contribute: blocks are coded independently, and a neighbor in another block
// is treated as insignificant (D.3).
type blockDecoder struct {
	w, h   int
	stride int
	flags  []uint16
	data   []int32 // magnitudes, built up plane by plane

	mq  *mqDecoder
	raw rawDecoder
	// zc is the significance context table for this band's orientation.
	zc *[256]uint8
	// vsc is the vertically causal context: the last row of each stripe
	// ignores the stripe below (D.7).
	vsc bool
}

// rawDecoder reads the bits of a bypass segment (D.6): most significant bit
// first, and after a 0xFF byte only seven bits of the next, whose top bit the
// encoder stuffed with a 0. Past the end it reads 1 bits, as the arithmetic
// decoder does.
type rawDecoder struct {
	data []byte
	pos  int
	c    uint32
	ct   int
}

func (r *rawDecoder) start(data []byte) {
	*r = rawDecoder{data: data}
}

func (r *rawDecoder) bit() int {
	if r.ct == 0 {
		next := uint32(0xFF)
		if r.pos < len(r.data) {
			next = uint32(r.data[r.pos])
		}
		switch {
		case r.c != 0xFF:
			r.c, r.ct = next, 8
			r.pos++
		case next > 0x8F:
			// A marker or the end: stay on 0xFF and read ones.
			r.c, r.ct = 0xFF, 8
		default:
			r.c, r.ct = next, 7
			r.pos++
		}
	}
	r.ct--
	return int(r.c>>uint(r.ct)) & 1
}

// belowStripe are the neighbor bits of the stripe below a sample.
const belowStripe = flagDown | flagSW | flagSE

// contextFlags is a sample's flags as its contexts see them: with the
// vertically causal context, the last row of a stripe does not see the
// stripe below.
func (b *blockDecoder) contextFlags(i, y int) uint16 {
	if b.vsc && y%4 == 3 {
		return b.flags[i] &^ belowStripe
	}
	return b.flags[i]
}

// neighborWrite is one of the eight bits a newly significant coefficient sets:
// where the neighbor sits, and which of its neighbor bits this one is. Read
// each entry as "the neighbor at (dx, dy) learns that the coefficient on its
// opposite side became significant".
type neighborWrite struct {
	dx, dy int
	bit    uint16
}

var neighborWrites = [8]neighborWrite{
	{-1, 0, flagRight}, {1, 0, flagLeft},
	{0, -1, flagDown}, {0, 1, flagUp},
	{-1, -1, flagSE}, {1, -1, flagSW},
	{-1, 1, flagNE}, {1, 1, flagNW},
}

// newBlockDecoder prepares a decoder for a block of w by h samples.
func newBlockDecoder(w, h, bandType int, data []byte) *blockDecoder {
	stride := w + 2
	d := &blockDecoder{
		w:      w,
		h:      h,
		stride: stride,
		flags:  make([]uint16, stride*(h+2)),
		data:   make([]int32, w*h),
		mq:     newMQDecoder(data, numContexts),
		zc:     &zeroCodingTables[orientationOf(bandType)],
	}
	d.resetContexts()
	return d
}

// resetContexts puts every context in its starting state: state 0, except
// the three Table D.7 starts elsewhere.
func (b *blockDecoder) resetContexts() {
	for cx := range b.mq.contexts {
		b.mq.contexts[cx] = mqContext{}
	}
	b.mq.setContext(cxUniform, 46, 0)
	b.mq.setContext(cxRunLen, 3, 0)
	b.mq.setContext(cxZero, 4, 0)
}

// Band orientations, which is what the significance context depends on: the LL
// and LH bands share a table, HL has it transposed, and HH has its own.
const (
	orientLLandLH = 0
	orientHL      = 1
	orientHH      = 2
)

func orientationOf(bandType int) int {
	switch bandType {
	case BandHL:
		return orientHL
	case BandHH:
		return orientHH
	default:
		return orientLLandLH
	}
}

// zeroCodingTables is Table D.1 as one entry per arrangement of the eight
// neighbors, built once. The table is the decoder's only copy of the rules:
// there is no second path that computes a context from counts at decode time,
// so what the tests check is what runs.
var zeroCodingTables = func() [3][256]uint8 {
	var tables [3][256]uint8
	for orientation := range tables {
		for mask := 0; mask < 256; mask++ {
			h := bitsIn(mask, 0, 1)
			v := bitsIn(mask, 2, 3)
			d := bitsIn(mask, 4, 5, 6, 7)
			tables[orientation][mask] = uint8(zeroCodingContext(orientation, h, v, d))
		}
	}
	return tables
}()

func bitsIn(mask int, positions ...int) int {
	n := 0
	for _, p := range positions {
		if mask&(1<<p) != 0 {
			n++
		}
	}
	return n
}

// index is the offset of sample (x, y) in the padded flag grid.
func (b *blockDecoder) index(x, y int) int { return (y+1)*b.stride + x + 1 }

// SubbandIndex is where a subband's quantization values sit in a QCD or QCC
// list (A.6.4): the LL band first, then HL, LH and HH of each resolution.
func SubbandIndex(resolution, bandType int) int {
	if resolution == 0 {
		return 0
	}
	return 3*(resolution-1) + bandType
}

// MagnitudeBits is Mb, how many bit planes a subband's coefficients can hold
// (Equation E-2): the guard bits plus the band's exponent, less one. It is what
// a code-block's zero bit planes are counted down from, so getting it wrong
// shifts every coefficient of the band by a power of two.
func MagnitudeBits(q Quantization, band Band, resolution, levels int) (int, error) {
	exponent := 0
	switch q.Style {
	case QuantDerived:
		// E-5: one exponent is signaled, and each band's is derived from how
		// many decompositions separate it from the image.
		if len(q.Exponents) == 0 {
			return 0, errorf("a derived quantization with no exponent")
		}
		exponent = q.Exponents[0] + band.Level - levels
	default:
		index := SubbandIndex(resolution, band.Type)
		if index < 0 || index >= len(q.Exponents) {
			return 0, errorf("subband %d has no quantization exponent, of %d given",
				index, len(q.Exponents))
		}
		exponent = q.Exponents[index]
	}

	bits := q.GuardBits + exponent - 1
	if bits < 0 || bits > 31 {
		return 0, errorf("a subband claiming %d magnitude bits", bits)
	}
	return bits, nil
}

// DecodeBlock decodes one code-block into signed coefficients, row by row.
//
// The values returned are in units of half a coefficient: a returned 3 means
// 1.5. The half is the reconstruction estimate a truncated coefficient carries
// (see setSignificant), and it is kept rather than rounded away because a lossy
// decode multiplies these by a quantization step, where half a unit still
// counts. Dividing by two, rounding towards zero, gives the coefficient.
//
// magnitudeBits is the band's Mb, from MagnitudeBits.
func DecodeBlock(block CodeBlockData, magnitudeBits int) ([]int32, error) {
	d, err := decodeBlock(block, magnitudeBits)
	if err != nil {
		return nil, err
	}
	return d.data, nil
}

// DecodeBlockBytesReadForTest decodes a block and reports how far into its data
// the coding passes read.
//
// It exists for the test that requires every code-block of every fixture to
// land on its last byte, which is the check that says tier-1 made the encoder's
// decisions in the encoder's contexts. That test reads the corpus, so it has to
// live in the external test package, which cannot see decodeBlock.
func DecodeBlockBytesReadForTest(block CodeBlockData, magnitudeBits int) (int, error) {
	d, err := decodeBlock(block, magnitudeBits)
	if err != nil {
		return 0, err
	}
	return d.mq.bp, nil
}

// BlockDataLengthForTest is the length of a block's single codeword segment,
// what DecodeBlockBytesReadForTest must reach; -1 for several segments.
func BlockDataLengthForTest(block CodeBlockData) int {
	if len(block.Segments) != 1 {
		return -1
	}
	return len(block.Segments[0].Data)
}

// decodeBlock is DecodeBlock, returning the decoder itself so that tests can
// ask how much of the block's data the passes consumed.
func decodeBlock(block CodeBlockData, magnitudeBits int) (*blockDecoder, error) {
	w, h := block.Block.Width(), block.Block.Height()
	if w <= 0 || h <= 0 {
		return nil, errorf("a code-block of %dx%d samples", w, h)
	}
	if w > 1<<12 || h > 1<<12 || w*h > 1<<22 {
		return nil, errorf("a code-block of %dx%d samples is larger than any legal one", w, h)
	}

	d := newBlockDecoder(w, h, block.BandType, nil)
	d.vsc = block.Style&BlockVSC != 0

	// The first plane a block can hold is its most significant, less the planes
	// the header said are entirely zero. The first pass of that plane is always
	// a cleanup pass: with nothing significant yet, the other two have nothing
	// to say (D.2).
	plane := magnitudeBits - 1 - block.ZeroBitPlanes
	if plane < 0 {
		// Every plane the band could hold is zero, so the block is all zeros.
		return d, nil
	}
	// One less than an int32 holds: the estimates below are kept in units of
	// half a coefficient, which costs a bit.
	if plane > 29 {
		return nil, errorf("a code-block claims bit plane %d, past what an int32 holds", plane)
	}
	if block.Style&BlockHT != 0 {
		return d, d.decodeHT(block, plane, magnitudeBits)
	}

	// A block's planes allow exactly three passes each, less the two the first
	// plane does not have: it opens with a cleanup pass, since nothing is
	// significant yet for the other two to work on. More passes than that is a
	// contradiction within the header, and the usual cause is a field read at
	// the wrong width — which is worth saying rather than quietly truncating.
	if limit := 3*(plane+1) - 2; block.Passes > limit {
		return nil, errorf("a code-block claims %d coding passes, of the %d its %d bit planes hold",
			block.Passes, limit, plane+1)
	}

	// Each codeword segment restarts the arithmetic decoder, or with the
	// bypass the raw one (D.4.1, D.6); the passes a segment holds were
	// counted by the packet headers.
	segment, left, segmentRaw := -1, 0, false
	passType := passCleanup
	for pass := 0; pass < block.Passes && plane >= 0; pass++ {
		raw := rawPass(pass, block.Style)
		if left == 0 {
			segment++
			if segment >= len(block.Segments) {
				break
			}
			left, segmentRaw = block.Segments[segment].Passes, raw
			if raw {
				d.raw.start(block.Segments[segment].Data)
			} else {
				d.mq.start(block.Segments[segment].Data)
			}
		}
		if raw != segmentRaw {
			return nil, errorf("a codeword segment mixes raw and arithmetic coding passes")
		}
		if block.Style&BlockReset != 0 && pass > 0 {
			d.resetContexts()
		}
		switch {
		case passType == passSignificance && raw:
			d.rawSignificancePass(plane)
		case passType == passSignificance:
			d.significancePass(plane)
		case passType == passRefinement && raw:
			d.rawRefinementPass(plane)
		case passType == passRefinement:
			d.refinementPass(plane)
		default:
			d.cleanupPass(plane)
			if block.Style&BlockSegSym != 0 {
				// The segmentation symbol, 1010 in the uniform context
				// (D.5). It only lets an encoder's errors be detected, so
				// a wrong one is not fatal here.
				for k := 0; k < 4; k++ {
					d.mq.decode(cxUniform)
				}
			}
		}
		left--

		if passType == passCleanup {
			plane--
			passType = passSignificance
		} else {
			passType++
		}
	}

	d.applySigns()
	return d, nil
}

// The three pass types, in the order they run within a bit plane.
const (
	passSignificance = iota
	passRefinement
	passCleanup
)

// zeroCodingContext is Table D.1: the context a significance bit is coded in,
// from how many of the horizontal, vertical and diagonal neighbors are already
// significant. The HL band's rules are the LL and LH ones with the horizontal
// and vertical roles exchanged, because its structure is transposed.
func zeroCodingContext(orientation, h, v, d int) int {
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

// significanceContext looks up the context of a sample from its context flags.
func (b *blockDecoder) significanceContext(flags uint16) int {
	return int(b.zc[(flags>>neighborShift)&0xFF])
}

// signContext is the context and the XOR bit for a sign (Table D.3 and D.4).
//
// The horizontal and vertical neighbors vote: each contributes +1 if it is
// significant and positive, -1 if significant and negative, and nothing if it is
// not significant yet. The two sums pick one of five contexts and say whether
// the decoded bit means the sign it appears to or its opposite.
func (b *blockDecoder) signContext(i, y int) (int, int) {
	h := b.contribution(i-1) + b.contribution(i+1)
	v := b.contribution(i - b.stride)
	if !b.vsc || y%4 != 3 {
		v += b.contribution(i + b.stride)
	}
	h, v = clampUnit(h), clampUnit(v)

	if h == 0 {
		switch v {
		case 0:
			return cxSignFirst, 0
		case 1:
			return cxSignFirst + 1, 0
		default:
			return cxSignFirst + 1, 1
		}
	}
	if h == 1 {
		switch v {
		case 0:
			return cxSignFirst + 3, 0
		case 1:
			return cxSignFirst + 4, 0
		default:
			return cxSignFirst + 2, 0
		}
	}
	switch v {
	case 0:
		return cxSignFirst + 3, 1
	case 1:
		return cxSignFirst + 2, 1
	default:
		return cxSignFirst + 4, 1
	}
}

// contribution is one neighbor's vote for the sign context: +1 positive, -1
// negative, 0 not yet significant.
func (b *blockDecoder) contribution(i int) int {
	f := b.flags[i]
	if f&flagSignificant == 0 {
		return 0
	}
	if f&flagNegative != 0 {
		return -1
	}
	return 1
}

func clampUnit(v int) int {
	switch {
	case v > 1:
		return 1
	case v < -1:
		return -1
	}
	return v
}

// decodeSign reads a coefficient's sign and records it.
func (b *blockDecoder) decodeSign(i, y int) {
	cx, xorBit := b.signContext(i, y)
	if b.mq.decode(cx)^xorBit == 1 {
		b.flags[i] |= flagNegative
	}
}

// setSignificant marks a coefficient significant and gives it the value its
// new bit plane implies.
//
// That value is the middle of the interval the bit plane leaves open, not its
// floor. A coefficient that first becomes significant at plane p is known only
// to lie in [2^p, 2^(p+1)), and the best estimate of it is 1.5 * 2^p. Taking
// the floor instead biases every truncated coefficient downwards, which is
// visible in the decoded image: against pydicom it was 1005 of MR_small's 4096
// samples wrong, three quarters of them low by one.
//
// The half is carried by working in units of half a coefficient throughout and
// halving at the end (applySigns), so the estimate stays exact in integers. A
// coefficient decoded all the way to plane 0 comes out at its true value, since
// the half is then below the last bit and the halving drops it.
func (b *blockDecoder) setSignificant(x, y, i, plane int) {
	b.flags[i] |= flagSignificant
	b.data[y*b.w+x] = 3 << uint(plane)
	for _, n := range neighborWrites {
		b.flags[i+n.dy*b.stride+n.dx] |= n.bit
	}
}

// significancePass is D.3.1: every coefficient that is not yet significant but
// has a significant neighbor gets a bit. Those are the ones most likely to
// become significant in this plane, which is why they are coded first and why a
// truncated stream loses the least.
func (b *blockDecoder) significancePass(plane int) {
	for y0 := 0; y0 < b.h; y0 += 4 {
		for x := 0; x < b.w; x++ {
			i := b.index(x, y0)
			for y := y0; y < y0+4 && y < b.h; y, i = y+1, i+b.stride {
				f := b.contextFlags(i, y)
				// Significant already, or with no significant neighbor: the
				// first has nothing left to say in this pass, the second
				// belongs to the cleanup pass and is not marked visited.
				if f&flagSignificant != 0 || f&flagNeighbors == 0 {
					continue
				}
				if b.mq.decode(b.significanceContext(f)) == 1 {
					b.setSignificant(x, y, i, plane)
					b.decodeSign(i, y)
				}
				b.flags[i] |= flagVisited
			}
		}
	}
}

// rawSignificancePass is the significance pass of a bypass segment (D.6):
// the same samples, each bit and sign read raw (a sign bit of 1 is negative).
func (b *blockDecoder) rawSignificancePass(plane int) {
	for y0 := 0; y0 < b.h; y0 += 4 {
		for x := 0; x < b.w; x++ {
			i := b.index(x, y0)
			for y := y0; y < y0+4 && y < b.h; y, i = y+1, i+b.stride {
				f := b.contextFlags(i, y)
				if f&flagSignificant != 0 || f&flagNeighbors == 0 {
					continue
				}
				if b.raw.bit() == 1 {
					b.setSignificant(x, y, i, plane)
					if b.raw.bit() == 1 {
						b.flags[i] |= flagNegative
					}
				}
				b.flags[i] |= flagVisited
			}
		}
	}
}

// rawRefinementPass is the refinement pass of a bypass segment (D.6).
func (b *blockDecoder) rawRefinementPass(plane int) {
	for y0 := 0; y0 < b.h; y0 += 4 {
		for x := 0; x < b.w; x++ {
			i := b.index(x, y0)
			for y := y0; y < y0+4 && y < b.h; y, i = y+1, i+b.stride {
				f := b.flags[i]
				if f&flagSignificant == 0 || f&flagVisited != 0 {
					continue
				}
				if b.raw.bit() == 1 {
					b.data[y*b.w+x] += 1 << uint(plane)
				} else {
					b.data[y*b.w+x] -= 1 << uint(plane)
				}
				b.flags[i] |= flagRefined
			}
		}
	}
}

// refinementPass is D.3.2: one more bit of every coefficient that was already
// significant before this plane. The first refinement of a coefficient uses a
// context chosen by its neighbors, later ones a single context, because after
// the first the bits are close to random.
func (b *blockDecoder) refinementPass(plane int) {
	for y0 := 0; y0 < b.h; y0 += 4 {
		for x := 0; x < b.w; x++ {
			i := b.index(x, y0)
			for y := y0; y < y0+4 && y < b.h; y, i = y+1, i+b.stride {
				f := b.contextFlags(i, y)
				if f&flagSignificant == 0 || f&flagVisited != 0 {
					continue
				}

				cx := cxRefineMore
				if f&flagRefined == 0 {
					if f&flagNeighbors == 0 {
						cx = cxRefineNone
					} else {
						cx = cxRefineNear
					}
				}
				// A refinement halves the interval the coefficient is known
				// to lie in, and moves the estimate to the middle of whichever
				// half it names.
				if b.mq.decode(cx) == 1 {
					b.data[y*b.w+x] += 1 << uint(plane)
				} else {
					b.data[y*b.w+x] -= 1 << uint(plane)
				}
				b.flags[i] |= flagRefined
			}
		}
	}
}

// cleanupPass is D.3.4: everything the other two passes left. Its own trick is
// the run-length mode — when a whole column of four is insignificant with no
// significant neighbor, which is the usual case in a high-frequency band, one
// decision says so and four coefficients cost almost nothing.
func (b *blockDecoder) cleanupPass(plane int) {
	for y0 := 0; y0 < b.h; y0 += 4 {
		for x := 0; x < b.w; x++ {
			y := y0
			for y < y0+4 && y < b.h {
				i := b.index(x, y)

				if y == y0 && y0+4 <= b.h && b.columnIsClean(i, y0) {
					if b.mq.decode(cxRunLen) == 0 {
						// All four stay insignificant.
						b.clearVisited(i, 4)
						y = y0 + 4
						continue
					}
					// Two bits, most significant first, say which of the four
					// becomes significant; the ones above it are known zero.
					first := b.mq.decode(cxUniform)<<1 | b.mq.decode(cxUniform)
					b.clearVisited(i, first)
					y = y0 + first
					i += first * b.stride
					b.setSignificant(x, y, i, plane)
					b.decodeSign(i, y)
					b.flags[i] &^= flagVisited
					y++
					continue
				}

				if f := b.contextFlags(i, y); f&(flagSignificant|flagVisited) == 0 {
					if b.mq.decode(b.significanceContext(f)) == 1 {
						b.setSignificant(x, y, i, plane)
						b.decodeSign(i, y)
					}
				}
				b.flags[i] &^= flagVisited
				y++
			}
		}
	}
}

// columnIsClean says whether the column of four starting at flag index i can go
// into run-length mode: nothing in it significant or already coded this plane,
// and no neighbor of any of the four significant either (D.3.4).
func (b *blockDecoder) columnIsClean(i, y0 int) bool {
	const busy = flagSignificant | flagVisited | flagNeighbors
	return b.flags[i]&busy == 0 &&
		b.flags[i+b.stride]&busy == 0 &&
		b.flags[i+2*b.stride]&busy == 0 &&
		b.contextFlags(i+3*b.stride, y0+3)&busy == 0
}

// clearVisited clears the visited mark on n samples down a column.
func (b *blockDecoder) clearVisited(i, n int) {
	for k := 0; k < n; k++ {
		b.flags[i+k*b.stride] &^= flagVisited
	}
}

// applySigns gives the estimates their signs. The half a unit they are carried
// in is kept: a lossy decode multiplies them by a quantization step, and
// rounding them to whole coefficients first would throw the half away before it
// could count.
func (b *blockDecoder) applySigns() {
	for y := 0; y < b.h; y++ {
		for x := 0; x < b.w; x++ {
			if b.flags[b.index(x, y)]&flagNegative != 0 {
				b.data[y*b.w+x] = -b.data[y*b.w+x]
			}
		}
	}
}

// decodeHT decodes an HT code-block (ISO/IEC 15444-15): one HT cleanup pass
// at bit-plane plane, which codes every magnitude bit from there up. Each
// value is kept in halves like the Part 1 estimates: the middle of the
// interval the cleanup plane leaves open, (2mu+1) << plane.
func (b *blockDecoder) decodeHT(block CodeBlockData, plane, magnitudeBits int) error {
	if block.Passes != 1 || len(block.Segments) != 1 {
		return &ErrUnsupported{Feature: "HT refinement or placeholder passes"}
	}
	// The exponent bound of a quad cannot pass the band's bit-planes above
	// the cleanup plane by more than one (Part 15's U <= Mb - p + 1), and
	// the halves must fit in an int32.
	bound := magnitudeBits - plane + 1
	if bound > 31 {
		bound = 31
	}
	if err := htj2k.DecodeCleanup(block.Segments[0].Data, b.w, b.h, b.data, bound); err != nil {
		return errorf("HT code-block: %v", err)
	}
	limit := int64(1) << 30
	for i, v := range b.data {
		if v == 0 {
			continue
		}
		mu := int64(v)
		if mu < 0 {
			mu = -mu
		}
		halves := (2*mu + 1) << uint(plane)
		if halves >= limit {
			return errorf("HT code-block value past what an int32 holds")
		}
		if v < 0 {
			halves = -halves
		}
		b.data[i] = int32(halves)
	}
	return nil
}
