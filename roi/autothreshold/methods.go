package autothreshold

import (
	"fmt"
	"math"
	"strings"
)

// Method names one automatic threshold calculator.
type Method string

const (
	Otsu               Method = "otsu"
	Huang              Method = "huang"
	Intermodes         Method = "intermodes"
	Minimum            Method = "minimum"
	IsoData            Method = "isodata"
	KittlerIllingworth Method = "kittler-illingworth"
	Li                 Method = "li"
	MaxEntropy         Method = "max-entropy"
	Mean               Method = "mean"
	Moments            Method = "moments"
	RenyiEntropy       Method = "renyi-entropy"
	Shanbhag           Method = "shanbhag"
	Triangle           Method = "triangle"
	Yen                Method = "yen"
)

var methodLabels = map[Method]string{
	Otsu:               "Otsu",
	Huang:              "Huang (fuzzy)",
	Intermodes:         "Intermodes",
	Minimum:            "Minimum",
	IsoData:            "IsoData (Ridler–Calvard)",
	KittlerIllingworth: "Kittler–Illingworth (minimum error)",
	Li:                 "Li (minimum cross entropy)",
	MaxEntropy:         "Maximum entropy (Kapur)",
	Mean:               "Mean",
	Moments:            "Moments (Tsai)",
	RenyiEntropy:       "Rényi entropy",
	Shanbhag:           "Shanbhag",
	Triangle:           "Triangle",
	Yen:                "Yen",
}

// Methods returns the two-class methods in a stable display order.
func Methods() []Method {
	return []Method{Otsu, Huang, Intermodes, Minimum, IsoData, KittlerIllingworth, Li, MaxEntropy, Mean, Moments, RenyiEntropy, Shanbhag, Triangle, Yen}
}

// Label returns a human-readable name.
func (m Method) Label() string {
	if label, ok := methodLabels[m]; ok {
		return label
	}
	return string(m)
}

// ParseMethod accepts a method name or label, ignoring case.
func ParseMethod(s string) (Method, error) {
	s = strings.TrimSpace(s)
	for _, m := range Methods() {
		if strings.EqualFold(s, string(m)) || strings.EqualFold(s, m.Label()) {
			return m, nil
		}
	}
	return "", fmt.Errorf("autothreshold: unknown method %q", s)
}

// Threshold is the split between a lower class (bins 0..Bin) and an upper
// class (bins Bin+1..). A sample v is in the upper class exactly when
// v >= Value; Center is the centre of bin Bin (the value reported by ITK and
// scikit-image, with the upper class v > Center).
type Threshold struct {
	Bin    int
	Value  float64
	Center float64
}

func (h *Histogram) threshold(bin int) Threshold {
	return Threshold{Bin: bin, Value: h.BinLower(bin + 1), Center: h.BinCenter(bin)}
}

// ClassOf returns the class of v given ascending thresholds: 0 below the
// first, len(thresholds) at or above the last.
func ClassOf(thresholds []Threshold, v float64) int {
	class := 0
	for _, t := range thresholds {
		if v >= t.Value {
			class++
		}
	}
	return class
}

// maxQuadraticBins bounds the occupied span of the methods whose cost grows
// with the square of the number of bins (Huang, Shanbhag, multi-Otsu, and
// the repeated smoothing of Intermodes and Minimum).
const maxQuadraticBins = 4096

// Compute builds the histogram of values and thresholds it with method.
func Compute(values Values, method Method, opts HistogramOptions) (Threshold, error) {
	h, err := NewHistogram(values, opts)
	if err != nil {
		return Threshold{}, err
	}
	return h.Threshold(method)
}

// Threshold computes the two-class threshold of the histogram with method.
// The split always leaves at least one occupied bin in each class.
func (h *Histogram) Threshold(method Method) (Threshold, error) {
	lo, hi, err := h.span()
	if err != nil {
		return Threshold{}, err
	}
	switch method {
	case Huang, Shanbhag, Intermodes, Minimum:
		if hi-lo+1 > maxQuadraticBins {
			return Threshold{}, fmt.Errorf("%w: %s on %d occupied bins, at most %d", ErrTooManyBins, method, hi-lo+1, maxQuadraticBins)
		}
	}
	p := h.normalized()
	var k int
	switch method {
	case Otsu:
		k = otsu(p, lo, hi)
	case Huang:
		k = huang(p, lo, hi)
	case Intermodes, Minimum:
		k, err = modes(h.Counts, lo, hi, method == Intermodes)
		if err != nil {
			return Threshold{}, err
		}
	case IsoData:
		k = isoData(p, lo, hi)
	case KittlerIllingworth:
		k = kittlerIllingworth(p, lo, hi)
	case Li:
		k = li(p, lo, hi)
	case MaxEntropy:
		k = renyi(p, lo, hi, 1)
	case Mean:
		k = int(math.Floor(mean(p, lo, hi)))
	case Moments:
		k = moments(p, lo, hi)
	case RenyiEntropy:
		k = renyiCombined(p, lo, hi)
	case Shanbhag:
		k = shanbhag(p, lo, hi)
	case Triangle:
		k = triangle(p, lo, hi)
	case Yen:
		k = yen(p, lo, hi)
	default:
		return Threshold{}, fmt.Errorf("autothreshold: unknown method %q", method)
	}
	return h.threshold(clampSplit(k, lo, hi)), nil
}

// MultiOtsu returns the classes-1 ascending thresholds that maximize Otsu's
// between-class variance over classes classes.
func (h *Histogram) MultiOtsu(classes int) ([]Threshold, error) {
	if classes < 2 {
		return nil, fmt.Errorf("autothreshold: multi-Otsu needs at least 2 classes, got %d", classes)
	}
	lo, hi, err := h.span()
	if err != nil {
		return nil, err
	}
	occupied := 0
	for _, c := range h.Counts[lo : hi+1] {
		if c > 0 {
			occupied++
		}
	}
	if occupied < classes {
		return nil, fmt.Errorf("autothreshold: %d classes need at least as many occupied bins, found %d", classes, occupied)
	}
	if hi-lo+1 > maxQuadraticBins {
		return nil, fmt.Errorf("%w: multi-Otsu on %d occupied bins", ErrTooManyBins, hi-lo+1)
	}
	bins := multiOtsu(h.normalized(), lo, hi, classes)
	out := make([]Threshold, len(bins))
	for i, k := range bins {
		out[i] = h.threshold(k)
	}
	return out, nil
}

func (h *Histogram) normalized() []float64 {
	total := h.Total()
	p := make([]float64, len(h.Counts))
	for i, c := range h.Counts {
		p[i] = c / total
	}
	return p
}

// clampSplit keeps a split inside the occupied span with both classes
// non-empty.
func clampSplit(k, lo, hi int) int {
	if k < lo {
		return lo
	}
	if k > hi-1 {
		return hi - 1
	}
	return k
}

// Bin indices serve as the gray levels below: every criterion is invariant
// under the affine map from bin index to bin centre (Li, which is not, is
// computed on indices shifted to start at zero, as values shifted to start
// at zero).

func mean(p []float64, lo, hi int) float64 {
	m := 0.0
	for i := lo; i <= hi; i++ {
		m += float64(i) * p[i]
	}
	return m
}

// otsu maximizes the between-class variance w0*w1*(m0-m1)^2, keeping the
// first maximum.
func otsu(p []float64, lo, hi int) int {
	total := mean(p, lo, hi)
	best, bestK := -1.0, lo
	w0, s0 := 0.0, 0.0
	for k := lo; k < hi; k++ {
		w0 += p[k]
		s0 += float64(k) * p[k]
		w1 := 1 - w0
		if w0 <= 0 || w1 <= 0 {
			continue
		}
		d := total*w0 - s0
		v := d * d / (w0 * w1)
		if v > best {
			best, bestK = v, k
		}
	}
	return bestK
}

// multiOtsu maximizes sum(S_c^2/P_c) over contiguous partitions of lo..hi
// into classes classes (equivalent to the between-class variance) by
// dynamic programming over the prefix sums, exact like an exhaustive search.
func multiOtsu(p []float64, lo, hi, classes int) []int {
	n := hi - lo + 1
	P := make([]float64, n+1)
	S := make([]float64, n+1)
	for i := 0; i < n; i++ {
		P[i+1] = P[i] + p[lo+i]
		S[i+1] = S[i] + float64(i)*p[lo+i]
	}
	term := func(a, b int) float64 { // bins a..b-1 (relative)
		w := P[b] - P[a]
		if w <= 0 {
			return 0
		}
		s := S[b] - S[a]
		return s * s / w
	}
	// best[c][j]: best score of c+1 classes over the first j bins.
	best := make([][]float64, classes)
	from := make([][]int, classes)
	for c := range best {
		best[c] = make([]float64, n+1)
		from[c] = make([]int, n+1)
		for j := range best[c] {
			best[c][j] = math.Inf(-1)
		}
	}
	for j := 1; j <= n; j++ {
		best[0][j] = term(0, j)
	}
	for c := 1; c < classes; c++ {
		for j := c + 1; j <= n; j++ {
			for i := c; i < j; i++ {
				if v := best[c-1][i] + term(i, j); v > best[c][j] {
					best[c][j], from[c][j] = v, i
				}
			}
		}
	}
	splits := make([]int, classes-1)
	j := n
	for c := classes - 1; c > 0; c-- {
		i := from[c][j]
		splits[c-1] = lo + i - 1
		j = i
	}
	return splits
}

// huang minimizes the fuzziness (Shannon entropy of the memberships
// 1/(1+|g-mean|/C)) of the two-class partition.
func huang(p []float64, lo, hi int) int {
	c := float64(hi - lo)
	shannon := func(u float64) float64 {
		if u <= 0 || u >= 1 {
			return 0
		}
		return -u*math.Log(u) - (1-u)*math.Log(1-u)
	}
	best, bestK := math.Inf(1), lo
	w0, s0 := 0.0, 0.0
	totalW, totalS := 0.0, 0.0
	for i := lo; i <= hi; i++ {
		totalW += p[i]
		totalS += float64(i) * p[i]
	}
	for k := lo; k < hi; k++ {
		w0 += p[k]
		s0 += float64(k) * p[k]
		if w0 <= 0 || totalW-w0 <= 0 {
			continue
		}
		m0, m1 := s0/w0, (totalS-s0)/(totalW-w0)
		e := 0.0
		for i := lo; i <= hi; i++ {
			if p[i] == 0 {
				continue
			}
			m := m1
			if i <= k {
				m = m0
			}
			e += p[i] * shannon(1/(1+math.Abs(float64(i)-m)/c))
		}
		if e < best {
			best, bestK = e, k
		}
	}
	return bestK
}

// smoothToPeaks smooths the counts c[lo..hi] with a three-bin running mean (mirrored at
// the ends) until it has fewer than three peaks, and returns the smoothed
// histogram with its peaks.
func smoothToPeaks(c []float64, lo, hi int) ([]float64, []int, error) {
	const maxIterations = 10000
	y := append([]float64(nil), c[lo:hi+1]...)
	next := make([]float64, len(y))
	var peaks []int
	for iteration := 0; ; iteration++ {
		if iteration == maxIterations {
			return nil, nil, fmt.Errorf("%w: still %d peaks after %d smoothings", ErrNotBimodal, len(peaks), maxIterations)
		}
		n := len(y)
		for i := range y {
			left, right := y[max(i-1, 0)], y[min(i+1, n-1)]
			next[i] = (left + y[i] + right) / 3
		}
		y, next = next, y
		peaks = peaksOf(y)
		if len(peaks) < 3 {
			break
		}
	}
	if len(peaks) != 2 {
		return nil, nil, fmt.Errorf("%w: %d peak", ErrNotBimodal, len(peaks))
	}
	return y, peaks, nil
}

// peaksOf returns the local maxima of y: the last index of every rise (or
// of the start) that is followed by a strict fall, and the last bin when the
// histogram rises into it.
func peaksOf(y []float64) []int {
	var peaks []int
	rising, rose := true, false
	for i := 0; i+1 < len(y); i++ {
		switch {
		case rising && y[i+1] < y[i]:
			peaks = append(peaks, i)
			rising = false
		case y[i+1] > y[i]:
			rising, rose = true, true
		}
	}
	if rising && rose && len(y) > 1 {
		peaks = append(peaks, len(y)-1)
	}
	return peaks
}

// modes implements Intermodes (midpoint of the two peaks) and Minimum (the
// lowest smoothed bin between them).
func modes(counts []float64, lo, hi int, intermodes bool) (int, error) {
	y, peaks, err := smoothToPeaks(counts, lo, hi)
	if err != nil {
		return 0, err
	}
	if intermodes {
		return lo + (peaks[0]+peaks[1])/2, nil
	}
	k := peaks[0]
	for i := peaks[0] + 1; i <= peaks[1]; i++ {
		if y[i] < y[k] {
			k = i
		}
	}
	return lo + k, nil
}

// isoData returns the lowest split k whose mean of the two class means lies
// in [k, k+1): the smallest fixed point of Ridler and Calvard's iteration.
// One always exists since the midpoint grows by at most one bin per step.
func isoData(p []float64, lo, hi int) int {
	totalW, totalS := 0.0, 0.0
	for i := lo; i <= hi; i++ {
		totalW += p[i]
		totalS += float64(i) * p[i]
	}
	w0, s0 := 0.0, 0.0
	for k := lo; k < hi; k++ {
		w0 += p[k]
		s0 += float64(k) * p[k]
		if w0 <= 0 {
			continue
		}
		mid := (s0/w0 + (totalS-s0)/(totalW-w0)) / 2
		if mid >= float64(k) && mid < float64(k+1) {
			return k
		}
	}
	return hi - 1
}

// kittlerIllingworth minimizes the classification error criterion
// J = 1 + 2(P0 ln s0 + P1 ln s1) - 2(P0 ln P0 + P1 ln P1) over every split.
// Class variances include Sheppard's correction (1/12 of a bin squared), the
// variance a single occupied bin carries, so J stays finite.
func kittlerIllingworth(p []float64, lo, hi int) int {
	totalW, totalS, totalQ := 0.0, 0.0, 0.0
	for i := lo; i <= hi; i++ {
		x := float64(i - lo)
		totalW += p[i]
		totalS += x * p[i]
		totalQ += x * x * p[i]
	}
	best, bestK := math.Inf(1), lo
	w0, s0, q0 := 0.0, 0.0, 0.0
	for k := lo; k < hi; k++ {
		x := float64(k - lo)
		w0 += p[k]
		s0 += x * p[k]
		q0 += x * x * p[k]
		w1 := totalW - w0
		if w0 <= 0 || w1 <= 0 {
			continue
		}
		m0, m1 := s0/w0, (totalS-s0)/w1
		v0 := math.Max(q0/w0-m0*m0, 0) + 1.0/12
		v1 := math.Max((totalQ-q0)/w1-m1*m1, 0) + 1.0/12
		j := w0*math.Log(v0) + w1*math.Log(v1) - 2*(w0*math.Log(w0)+w1*math.Log(w1))
		if j < best {
			best, bestK = j, k
		}
	}
	return bestK
}

// li iterates t = (m0-m1)/(ln m0 - ln m1), the minimum cross entropy
// condition, on gray levels shifted so the first occupied bin is zero,
// starting from the mean and stopping when t moves by less than half a bin.
func li(p []float64, lo, hi int) int {
	const maxIterations = 1000
	next := mean(p, lo, hi) - float64(lo)
	current := -1.0
	for iteration := 0; math.Abs(next-current) > 0.5 && iteration < maxIterations; iteration++ {
		current = next
		w0, s0, w1, s1 := 0.0, 0.0, 0.0, 0.0
		for i := lo; i <= hi; i++ {
			x := float64(i - lo)
			if x > current {
				w1 += p[i]
				s1 += x * p[i]
			} else {
				w0 += p[i]
				s0 += x * p[i]
			}
		}
		if w0 <= 0 || w1 <= 0 {
			break
		}
		m0, m1 := s0/w0, s1/w1
		if m0 == 0 {
			break
		}
		next = (m0 - m1) / (math.Log(m0) - math.Log(m1))
	}
	return lo + int(math.Floor(next))
}

// renyi maximizes the sum of the Rényi entropies of order alpha of the two
// classes (alpha 1 is Shannon's: Kapur's maximum entropy method). With
// prefix sums of p^alpha (or p ln p) every split costs O(1):
// H = ln(sum p^alpha / P^alpha)/(1-alpha), or ln P - sum(p ln p)/P.
func renyi(p []float64, lo, hi int, alpha float64) int {
	f := func(v float64) float64 {
		if v <= 0 {
			return 0
		}
		if alpha == 1 {
			return v * math.Log(v)
		}
		return math.Pow(v, alpha)
	}
	entropy := func(w, s float64) float64 {
		if alpha == 1 {
			return math.Log(w) - s/w
		}
		return (math.Log(s) - alpha*math.Log(w)) / (1 - alpha)
	}
	totalW, totalF := 0.0, 0.0
	for i := lo; i <= hi; i++ {
		totalW += p[i]
		totalF += f(p[i])
	}
	best, bestK := math.Inf(-1), lo
	w0, f0 := 0.0, 0.0
	for k := lo; k < hi; k++ {
		w0 += p[k]
		f0 += f(p[k])
		w1, f1 := totalW-w0, totalF-f0
		if w0 <= 0 || w1 <= 0 || (alpha != 1 && (f0 <= 0 || f1 <= 0)) {
			continue
		}
		if h := entropy(w0, f0) + entropy(w1, f1); h > best {
			best, bestK = h, k
		}
	}
	return bestK
}

// renyiCombined combines the Rényi thresholds of orders 0.5, 1 and 2 with
// the weights of Sahoo, Wilkins and Yeager (1997).
func renyiCombined(p []float64, lo, hi int) int {
	t := []int{renyi(p, lo, hi, 0.5), renyi(p, lo, hi, 1), renyi(p, lo, hi, 2)}
	if t[0] > t[1] {
		t[0], t[1] = t[1], t[0]
	}
	if t[1] > t[2] {
		t[1], t[2] = t[2], t[1]
	}
	if t[0] > t[1] {
		t[0], t[1] = t[1], t[0]
	}
	cumulative := func(k int) float64 {
		s := 0.0
		for i := lo; i <= k; i++ {
			s += p[i]
		}
		return s
	}
	near := func(a, b int) bool { return a-b <= 5 && b-a <= 5 }
	var b1, b2, b3 float64
	switch {
	case near(t[0], t[1]) && !near(t[1], t[2]):
		b1, b2, b3 = 0, 1, 3
	case !near(t[0], t[1]) && near(t[1], t[2]):
		b1, b2, b3 = 3, 1, 0
	default:
		b1, b2, b3 = 1, 2, 1
	}
	p1, p3 := cumulative(t[0]), cumulative(t[2])
	omega := p3 - p1
	x1, x2, x3 := float64(t[0]-lo), float64(t[1]-lo), float64(t[2]-lo)
	combined := x1*(p1+omega*b1/4) + x2*omega*b2/4 + x3*(1-p3+omega*b3/4)
	// The weights sum to one; the tolerance keeps rounding from dropping an
	// exact integer to the bin below.
	return lo + int(math.Floor(combined+1e-7))
}

// moments picks the p0-tile of the histogram, where p0 is the fraction of
// the two-level image preserving the first three moments (Tsai).
func moments(p []float64, lo, hi int) int {
	m := mean(p, lo, hi)
	m2, m3 := 0.0, 0.0
	for i := lo; i <= hi; i++ {
		d := float64(i) - m
		m2 += d * d * p[i]
		m3 += d * d * d * p[i]
	}
	sd := math.Sqrt(m2)
	skew := m3 / (sd * sd * sd)
	// On standardized levels (mean 0, variance 1) the two preserved levels
	// z0 < z1 solve z^2 - skew*z - 1 = 0.
	root := math.Sqrt(skew*skew + 4)
	z0, z1 := (skew-root)/2, (skew+root)/2
	p0 := z1 / (z1 - z0)
	cumulative := 0.0
	for k := lo; k < hi; k++ {
		cumulative += p[k]
		if cumulative >= p0 {
			return k
		}
	}
	return hi - 1
}

// shanbhag minimizes the difference between the information measures of the
// two classes, with memberships 0.5 + (mass between the level and the
// threshold)/(2 * class mass).
func shanbhag(p []float64, lo, hi int) int {
	best, bestK := math.Inf(1), lo
	for k := lo; k < hi; k++ {
		w0 := 0.0
		for i := lo; i <= k; i++ {
			w0 += p[i]
		}
		w1 := 1 - w0
		if w0 <= 0 || w1 <= 0 {
			continue
		}
		e0, tail := 0.0, 0.0
		for i := k; i >= lo; i-- {
			tail += p[i]
			if p[i] > 0 {
				e0 -= p[i] * math.Log(0.5+tail/(2*w0))
			}
		}
		e1, head := 0.0, 0.0
		for i := k + 1; i <= hi; i++ {
			head += p[i]
			if p[i] > 0 {
				e1 -= p[i] * math.Log(0.5+head/(2*w1))
			}
		}
		if d := math.Abs(e0/w0 - e1/w1); d < best {
			best, bestK = d, k
		}
	}
	return bestK
}

// triangle draws a line from the histogram peak to the far end of its
// longer tail (the last occupied bin, at height zero) and splits at the bin
// farthest below that line, scanning from the tail end.
func triangle(p []float64, lo, hi int) int {
	peak := lo
	for i := lo; i <= hi; i++ {
		if p[i] > p[peak] {
			peak = i
		}
	}
	height := p[peak]
	best, bestK := math.Inf(-1), lo
	if hi-peak > peak-lo {
		width := float64(hi - peak)
		for i := hi; i > peak; i-- {
			d := height*float64(hi-i) - width*p[i]
			if d > best {
				best, bestK = d, i
			}
		}
		return bestK
	}
	width := float64(peak - lo)
	for i := lo; i < peak; i++ {
		d := height*float64(i-lo) - width*p[i]
		if d > best {
			best, bestK = d, i
		}
	}
	return bestK
}

// yen maximizes the correlation criterion
// -ln(sum p^2/P0^2) - ln(sum p^2/P1^2).
func yen(p []float64, lo, hi int) int {
	totalQ := 0.0
	for i := lo; i <= hi; i++ {
		totalQ += p[i] * p[i]
	}
	best, bestK := math.Inf(-1), lo
	w0, q0 := 0.0, 0.0
	for k := lo; k < hi; k++ {
		w0 += p[k]
		q0 += p[k] * p[k]
		w1, q1 := 1-w0, totalQ-q0
		if w0 <= 0 || w1 <= 0 || q0 <= 0 || q1 <= 0 {
			continue
		}
		c := -math.Log(q0) - math.Log(q1) + 2*math.Log(w0) + 2*math.Log(w1)
		if c > best {
			best, bestK = c, k
		}
	}
	return bestK
}
