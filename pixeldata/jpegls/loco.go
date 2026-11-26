package jpegls

import (
	"math/bits"
	"sync"
)

const (
	minC       = -128
	maxC       = 127
	basicT1    = 3
	basicT2    = 7
	basicT3    = 21
	basicReset = 64
	// maxGolombK bounds the Golomb parameter of a context. Every error value
	// a conforming encoder codes lies in the modulo-reduced range (T.87
	// A.4.5), so A never exceeds N times half of RANGE and k stays at most
	// qbpp, itself at most 16 for samples of at most 16 bits. The decoder
	// rejects other error values (inModularRange), so the bound is never
	// reached; it only guarantees that updating k terminates.
	maxGolombK = 16
)

var runJ = [32]int{
	0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3,
	4, 4, 5, 5, 6, 6, 7, 7, 8, 9, 10, 11, 12, 13, 14, 15,
}

type contextState struct {
	a, b, c, n int
	k          int
}

type locoState struct {
	maxval int
	range_ int
	qbpp   int
	bpp    int
	limit  int
	reset  int
	near   int
	t1     int
	t2     int
	t3     int
	// ctx holds the adaptive state of the 365 regular-mode contexts and
	// the two run-interruption contexts (T.87 A.2.1); B and C are unused
	// for the latter. The counters and cached regular-mode k are stored together.
	ctx    [367]contextState
	nn     [2]int
	runIdx int
	// qtab maps a gradient d to its quantized region at
	// d+quantizeTableOffset (quantizeSlow); see quantizeTable.
	qtab []int8
}

func newLocoState(maxval int) *locoState {
	t1, t2, t3 := defaultThresholds(maxval)
	return newLocoStateWithPreset(maxval, t1, t2, t3, basicReset)
}

func newLocoStateWithPreset(maxval, t1, t2, t3, reset int) *locoState {
	return newLocoStateForPreset(presetCodingParameters{maxval: maxval, t1: t1, t2: t2, t3: t3, reset: reset})
}

func newLocoStateForPreset(p presetCodingParameters) *locoState {
	s := &locoState{maxval: p.maxval, near: p.near, reset: p.reset, t1: p.t1, t2: p.t2, t3: p.t3}
	s.range_ = (p.maxval+2*p.near)/(2*p.near+1) + 1
	s.qbpp = ceilLog2(s.range_)
	s.bpp = maxInt(2, ceilLog2(p.maxval+1))
	s.limit = 2 * (s.bpp + maxInt(8, s.bpp))
	s.qtab = quantizeTable(p)
	initA := maxInt(2, (s.range_+32)/64)
	for i := 0; i < 367; i++ {
		s.ctx[i].a = initA
		s.ctx[i].n = 1
		s.ctx[i].k = s.golombK(initA, 1)
	}
	return s
}

func defaultThresholds(maxval int) (t1, t2, t3 int) {
	return defaultNearThresholds(maxval, 0)
}

func defaultNearThresholds(maxval, near int) (t1, t2, t3 int) {
	// T.87 C.2.4.1.1.1 CLAMP returns the lower bound for either violation,
	// including an expression above MAXVAL (unlike ordinary saturation).
	threshold := func(value, lower int) int {
		if value < lower || value > maxval {
			return lower
		}
		return value
	}
	if maxval >= 128 {
		factor := (minInt(maxval, 4095) + 128) / 256
		t1 = threshold(factor*(basicT1-2)+2+3*near, near+1)
		t2 = threshold(factor*(basicT2-3)+3+5*near, t1)
		t3 = threshold(factor*(basicT3-4)+4+7*near, t2)
		return t1, t2, t3
	}
	factor := 256 / (maxval + 1)
	t1 = threshold(maxInt(2, basicT1/factor+3*near), near+1)
	t2 = threshold(maxInt(3, basicT2/factor+5*near), t1)
	t3 = threshold(maxInt(4, basicT3/factor+7*near), t2)
	return t1, t2, t3
}

// quantize returns the region of gradient d (T.87 A.3.3). JPEG-LS samples
// have at most 16 bits, so every gradient lies in the table.
func (s *locoState) quantize(d int) int {
	return int(s.qtab[d+quantizeTableOffset])
}

const (
	maxQuantizeTables = 32
	// quantizeTableOffset centers the table on gradient 0: differences of
	// samples of at most 16 bits lie in [-65535, 65535].
	quantizeTableOffset = 1<<16 - 1
)

var quantizeTables struct {
	sync.Mutex
	tables map[presetCodingParameters][]int8
}

// quantizeTable returns the quantization table of the coding parameters
// over every gradient of 16-bit samples, shared between scans with the same
// parameters (at most maxQuantizeTables of them are kept, so LSE segments
// with arbitrary thresholds cannot grow the cache).
func quantizeTable(p presetCodingParameters) []int8 {
	key := presetCodingParameters{near: p.near, t1: p.t1, t2: p.t2, t3: p.t3}
	quantizeTables.Lock()
	table, ok := quantizeTables.tables[key]
	quantizeTables.Unlock()
	if ok {
		return table
	}
	s := locoState{near: p.near, t1: p.t1, t2: p.t2, t3: p.t3}
	table = make([]int8, 2*quantizeTableOffset+1)
	for i := range table {
		table[i] = int8(s.quantizeSlow(i - quantizeTableOffset))
	}
	quantizeTables.Lock()
	if quantizeTables.tables == nil {
		quantizeTables.tables = map[presetCodingParameters][]int8{}
	}
	if len(quantizeTables.tables) < maxQuantizeTables {
		quantizeTables.tables[key] = table
	}
	quantizeTables.Unlock()
	return table
}

// quantizeSlow computes the region of gradient d from the thresholds; it
// fills the tables quantize reads.
func (s *locoState) quantizeSlow(d int) int {
	if d <= -s.t3 {
		return -4
	}
	if d <= -s.t2 {
		return -3
	}
	if d <= -s.t1 {
		return -2
	}
	if d < -s.near {
		return -1
	}
	if d <= s.near {
		return 0
	}
	if d < s.t1 {
		return 1
	}
	if d < s.t2 {
		return 2
	}
	if d < s.t3 {
		return 3
	}
	return 4
}

func (s *locoState) modRange(err int) int {
	// Branch-free form of: add RANGE to a negative error, then subtract it
	// from an error of at least (RANGE+1)/2 (T.87 A.4.5).
	err += s.range_ & (err >> 63)
	err -= s.range_ & (((s.range_+1)/2 - 1 - err) >> 63)
	return err
}

// inModularRange reports whether err is a value modRange can produce,
// -floor(RANGE/2) through ceil(RANGE/2)-1: the only error values a
// conforming encoder codes. One unsigned comparison covers both bounds.
func (s *locoState) inModularRange(err int) bool {
	return uint(err+s.range_>>1) < uint(s.range_)
}

// contextIndex merges the quantized gradients into the context index Q and
// the sign of the context (T.87 A.3.4), without branching on the sign,
// which is unpredictable on noisy data.
func contextIndex(q1, q2, q3 int) (q, sign int) {
	q = 81*q1 + 9*q2 + q3
	negative := q >> 63 // -1 or 0
	return (q ^ negative) - negative, negative | 1
}

func (s *locoState) mapRegular(err, q, k int) int {
	// Branch-free T.87 A.5.2: 2*err for err >= 0 and -2*err-1 otherwise; the
	// special mapping (NEAR=0, k=0, 2*B[Q] <= -N[Q]) adds 1 for err >= 0
	// and subtracts 1 otherwise.
	mapped := err<<1 ^ err>>63
	if s.near == 0 && k == 0 && 2*s.ctx[q].b <= -s.ctx[q].n {
		mapped += 1 + 2*(err>>63)
	}
	return mapped
}

func (s *locoState) unmapRegular(mapped, q, k int) int {
	// Inverse of mapRegular: an even value is err/2 and an odd one
	// -(err+1)/2, with the parities swapped under the special mapping.
	flip := mapped & 1
	if s.near == 0 && k == 0 && 2*s.ctx[q].b <= -s.ctx[q].n {
		flip ^= 1
	}
	return mapped>>1 ^ -flip
}

func (s *locoState) updateRegular(q, err int) {
	cx := &s.ctx[q]
	cx.b += err * (2*s.near + 1)
	cx.a += absInt(err)
	if cx.n == s.reset {
		cx.a >>= 1
		if cx.b >= 0 {
			cx.b >>= 1
		} else {
			cx.b = -((1 - cx.b) >> 1)
		}
		cx.n >>= 1
	}
	cx.n++
	// Bias correction (T.87 A.6.2) without branches, which mispredict on
	// noisy data: when B <= -N, B becomes max(B+N, 1-N) and C decreases
	// down to MIN_C; when B > 0, B becomes min(B-N, 0) and C increases up to
	// MAX_C. down and up are all ones in their case and zero otherwise; the
	// cases exclude each other since N > 0.
	b, n, c := cx.b, cx.n, cx.c
	down := (b + n - 1) >> 63
	up := -b >> 63
	keep := ^(down | up)
	cx.b = b&keep | max(b+n, 1-n)&down | min(b-n, 0)&up
	cx.c = c&keep | max(c-1, minC)&down | min(c+1, maxC)&up
	// The regular context's Golomb parameter usually stays unchanged. Update
	// it from its last value instead of recomputing two bit lengths per sample.
	// The cap keeps the loop finite even if A were out of bounds: past 63, the
	// shift would yield 0 and never reach A.
	for cx.k < maxGolombK && cx.n<<uint(cx.k) < cx.a {
		cx.k++
	}
	for cx.k > 0 && cx.n<<uint(cx.k-1) >= cx.a {
		cx.k--
	}
}

func (s *locoState) reconstruct(prediction, err int) int {
	step := 2*s.near + 1
	rx := prediction + err*step
	if rx < -s.near {
		rx += s.range_ * step
	}
	if rx > s.maxval+s.near {
		rx -= s.range_ * step
	}
	return clampInt(rx, 0, s.maxval)
}

// predict is the median edge detector (T.87 A.4.1): min(Ra, Rb) when Rc >=
// max(Ra, Rb), max(Ra, Rb) when Rc <= min(Ra, Rb), else Ra+Rb-Rc. That is
// Ra+Rb-Rc clamped to [min(Ra, Rb), max(Ra, Rb)], written without branches,
// which mispredict on noisy images.
func predict(ra, rb, rc int) int {
	return min(max(ra+rb-rc, min(ra, rb)), max(ra, rb))
}

func (s *locoState) golombK(a, n int) int {
	// The smallest k with N<<k >= A (T.87 A.5.1), without the data-dependent
	// loop: with k0 = max(0, len(A) - len(N)), N<<(k0-1) is shorter than A,
	// so k is k0, or k0+1 when N<<k0 < A. N <= RESET <= 65535 and A grows
	// by less than 2^17 per sample until N reaches RESET and both halve, so
	// A < 2^33 and N<<k0 < 2A: the shift and the difference fit int64.
	k := bits.Len64(uint64(a)) - bits.Len64(uint64(n))
	k &^= k >> 63
	return k - int((int64(n)<<uint(k)-int64(a))>>63)
}

func writeLimitedGolomb(w *bitWriter, mapped, k, limit, qbpp int) {
	high := mapped >> k
	maxHigh := limit - qbpp - 1
	if high < maxHigh {
		// The unary zeros, the terminating one and the k low bits as one
		// code word when it fits.
		if n := high + 1 + k; n <= 32 {
			w.writeBits(uint32(1)<<uint(k)|uint32(mapped)&(1<<uint(k)-1), n)
			return
		}
		w.writeZeros(high)
		w.writeBit(1)
		if k > 0 {
			w.writeBits(uint32(mapped)&uint32((1<<k)-1), k)
		}
		return
	}
	w.writeZeros(maxHigh)
	w.writeBit(1)
	w.writeBits(uint32(mapped-1), qbpp)
}

func readLimitedGolomb(r *bitReader, k, limit, qbpp int) (int, error) {
	maxHigh := limit - qbpp - 1
	// Fast path: the unary part, its terminating one and the k low bits all
	// among the loaded bits. A refill error is left for the general path,
	// which meets it again only if the bits are really needed.
	if r.count < 32 {
		_ = r.fill()
	}
	if run := r.count - bits.Len64(r.val); run < maxHigh && run+1+k <= r.count {
		r.count -= run + 1
		rem := r.val >> uint(r.count-k) & (1<<uint(k) - 1)
		r.consume(k)
		return run<<uint(k) | int(rem), nil
	}
	zeros := 0
	for {
		if r.count == 0 {
			if err := r.fill(); r.count == 0 {
				return 0, err
			}
		}
		// Leading zeros among the loaded bits (all of them when val is 0).
		run := r.count - bits.Len64(r.val)
		if zeros+run >= maxHigh {
			r.consume(maxHigh - zeros)
			break
		}
		if run < r.count {
			r.consume(run + 1) // the zeros and the terminating one
			high := zeros + run
			if k == 0 {
				return high, nil
			}
			rem, err := r.readBits(k)
			if err != nil {
				return 0, err
			}
			return high<<k | int(rem), nil
		}
		zeros += run
		r.consume(run)
	}
	bit, err := r.readBit()
	if err != nil {
		return 0, err
	}
	if bit != 1 {
		return 0, fmtInvalid("limited Golomb terminator")
	}
	val, err := r.readBits(qbpp)
	if err != nil {
		return 0, err
	}
	return int(val) + 1, nil
}

// neighbors returns the causal neighbors of any sample; the line loops read
// interior samples directly and call it only at the image borders, so it is
// kept out of line.
//
//go:noinline
func neighbors(recon []int, width, x, y int) (ra, rb, rc, rd int) {
	if y > 0 {
		rb = recon[(y-1)*width+x]
		if x+1 < width {
			rd = recon[(y-1)*width+x+1]
		} else {
			rd = rb
		}
	}
	if x > 0 {
		ra = recon[y*width+x-1]
		if y > 0 {
			rc = recon[(y-1)*width+x-1]
		}
		return ra, rb, rc, rd
	}
	ra = rb
	// ISO/IEC 14495-1 initializes the virtual pixel immediately left of the
	// current row from the first sample of the previous row. Consequently, at
	// x=0 Rc is the virtual-left value retained by the previous row: the first
	// sample two rows above (or zero for the first two rows), not Rb.
	if y > 1 {
		rc = recon[(y-2)*width]
	}
	return ra, rb, rc, rd
}

func absInt(v int) int {
	sign := v >> 63
	return (v ^ sign) - sign
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clampInt(v, lo, hi int) int {
	return min(max(v, lo), hi)
}

func ceilLog2(v int) int {
	if v <= 1 {
		return 0
	}
	n := 0
	x := v - 1
	for x > 0 {
		x >>= 1
		n++
	}
	return n
}
