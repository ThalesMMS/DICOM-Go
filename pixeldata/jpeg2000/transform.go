package jpeg2000

import "math"

// The wavelet transforms of ISO/IEC 15444-1 Annex F for a tile whose origin
// is (0, 0), so every row and column starts on an even (low-pass) sample.
// A plane is transformed in place; after each level its low-pass quarter
// occupies the top left corner, followed by HL to the right, LH below and HH
// diagonally (the low-pass half of each axis is the first ceil(n/2) samples).

// The 9/7 lifting coefficients and scaling of Table F.4. The analysis ends by
// multiplying low-pass samples by 1/K and high-pass ones by K/2, the inverse
// of the synthesis scalings K and 2/K.
const (
	liftAlpha = -1.586134342059924
	liftBeta  = -0.052980118572961
	liftGamma = 0.882911075530934
	liftDelta = 0.443506852043971
	scaleK    = 1.230174104914001
)

// mirror is whole-sample symmetric extension (F.3.7) of index i into [0, n).
func mirror(i, n int) int {
	if n == 1 {
		return 0
	}
	period := 2 * (n - 1)
	i %= period
	if i < 0 {
		i += period
	}
	if i >= n {
		i = period - i
	}
	return i
}

// forward53 is the reversible 5/3 analysis of one interleaved signal
// (F.4.8.2), followed by deinterleaving into low then high samples.
func forward53(x, scratch []int32) {
	n := len(x)
	if n < 2 {
		return
	}
	for i := 1; i < n; i += 2 {
		x[i] -= (x[i-1] + x[mirror(i+1, n)]) >> 1
	}
	for i := 0; i < n; i += 2 {
		x[i] += (x[mirror(i-1, n)] + x[mirror(i+1, n)] + 2) >> 2
	}
	deinterleave32(x, scratch)
}

// inverse53 undoes forward53; the encoder's tests use it.
func inverse53(x, scratch []int32) {
	n := len(x)
	if n < 2 {
		return
	}
	interleave32(x, scratch)
	for i := 0; i < n; i += 2 {
		x[i] -= (x[mirror(i-1, n)] + x[mirror(i+1, n)] + 2) >> 2
	}
	for i := 1; i < n; i += 2 {
		x[i] += (x[i-1] + x[mirror(i+1, n)]) >> 1
	}
}

func deinterleave32(x, scratch []int32) {
	n := len(x)
	low := (n + 1) / 2
	copy(scratch, x)
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			x[i/2] = scratch[i]
		} else {
			x[low+i/2] = scratch[i]
		}
	}
}

func interleave32(x, scratch []int32) {
	n := len(x)
	low := (n + 1) / 2
	copy(scratch, x)
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			x[i] = scratch[i/2]
		} else {
			x[i] = scratch[low+i/2]
		}
	}
}

// forward97 is the irreversible 9/7 analysis of one signal (F.4.8.2).
func forward97(x, scratch []float64) {
	n := len(x)
	if n < 2 {
		return
	}
	lift := func(start int, c float64) {
		for i := start; i < n; i += 2 {
			x[i] += c * (x[mirror(i-1, n)] + x[mirror(i+1, n)])
		}
	}
	lift(1, liftAlpha)
	lift(0, liftBeta)
	lift(1, liftGamma)
	lift(0, liftDelta)
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			x[i] /= scaleK
		} else {
			x[i] *= scaleK / 2
		}
	}
	low := (n + 1) / 2
	copy(scratch, x)
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			x[i/2] = scratch[i]
		} else {
			x[low+i/2] = scratch[i]
		}
	}
}

// inverse97 undoes forward97; it measures subband norms and serves tests.
func inverse97(x, scratch []float64) {
	n := len(x)
	if n < 2 {
		return
	}
	low := (n + 1) / 2
	copy(scratch, x)
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			x[i] = scratch[i/2] * scaleK
		} else {
			x[i] = scratch[low+i/2] * 2 / scaleK
		}
	}
	lift := func(start int, c float64) {
		for i := start; i < n; i += 2 {
			x[i] -= c * (x[mirror(i-1, n)] + x[mirror(i+1, n)])
		}
	}
	lift(0, liftDelta)
	lift(1, liftGamma)
	lift(0, liftBeta)
	lift(1, liftAlpha)
}

// forwardDWT53 applies levels of 2D_SD (columns, then rows) to a w x h plane.
func forwardDWT53(plane []int32, w, h, levels int) {
	line := make([]int32, max(w, h))
	scratch := make([]int32, max(w, h))
	cw, ch := w, h
	for l := 0; l < levels; l++ {
		for x := 0; x < cw; x++ {
			col := line[:ch]
			for y := 0; y < ch; y++ {
				col[y] = plane[y*w+x]
			}
			forward53(col, scratch)
			for y := 0; y < ch; y++ {
				plane[y*w+x] = col[y]
			}
		}
		for y := 0; y < ch; y++ {
			forward53(plane[y*w:y*w+cw], scratch)
		}
		cw, ch = (cw+1)/2, (ch+1)/2
	}
}

// inverseDWT53 undoes forwardDWT53.
func inverseDWT53(plane []int32, w, h, levels int) {
	line := make([]int32, max(w, h))
	scratch := make([]int32, max(w, h))
	for l := levels - 1; l >= 0; l-- {
		cw, ch := levelSize(w, l), levelSize(h, l)
		for y := 0; y < ch; y++ {
			inverse53(plane[y*w:y*w+cw], scratch)
		}
		for x := 0; x < cw; x++ {
			col := line[:ch]
			for y := 0; y < ch; y++ {
				col[y] = plane[y*w+x]
			}
			inverse53(col, scratch)
			for y := 0; y < ch; y++ {
				plane[y*w+x] = col[y]
			}
		}
	}
}

// forwardDWT97 is forwardDWT53 with the irreversible filter.
func forwardDWT97(plane []float64, w, h, levels int) {
	line := make([]float64, max(w, h))
	scratch := make([]float64, max(w, h))
	cw, ch := w, h
	for l := 0; l < levels; l++ {
		for x := 0; x < cw; x++ {
			col := line[:ch]
			for y := 0; y < ch; y++ {
				col[y] = plane[y*w+x]
			}
			forward97(col, scratch)
			for y := 0; y < ch; y++ {
				plane[y*w+x] = col[y]
			}
		}
		for y := 0; y < ch; y++ {
			forward97(plane[y*w:y*w+cw], scratch)
		}
		cw, ch = (cw+1)/2, (ch+1)/2
	}
}

// inverseDWT97 undoes forwardDWT97.
func inverseDWT97(plane []float64, w, h, levels int) {
	line := make([]float64, max(w, h))
	scratch := make([]float64, max(w, h))
	for l := levels - 1; l >= 0; l-- {
		cw, ch := levelSize(w, l), levelSize(h, l)
		for y := 0; y < ch; y++ {
			inverse97(plane[y*w:y*w+cw], scratch)
		}
		for x := 0; x < cw; x++ {
			col := line[:ch]
			for y := 0; y < ch; y++ {
				col[y] = plane[y*w+x]
			}
			inverse97(col, scratch)
			for y := 0; y < ch; y++ {
				plane[y*w+x] = col[y]
			}
		}
	}
}

// levelSize is the length of a side after l decompositions: ceil(n/2^l).
func levelSize(n, l int) int {
	return (n + 1<<l - 1) >> l
}

// synthesisNorm is the L2 norm of the 1D synthesis basis function of a
// coefficient in the low (high=false) or high band of decomposition level
// level (1 = finest), measured by synthesizing a unit impulse.
func synthesisNorm(level int, high bool) float64 {
	n := 64 << level
	x := make([]float64, n)
	scratch := make([]float64, n)
	// A coefficient far from both edges of its band.
	position := (n >> level) / 4
	if high {
		position += n >> level
	}
	x[position] = 1
	for l := level - 1; l >= 0; l-- {
		inverse97(x[:levelSize(n, l)], scratch)
	}
	sum := 0.0
	for _, v := range x {
		sum += v * v
	}
	return math.Sqrt(sum)
}
