package autothreshold

import (
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/roi"
)

// mixture returns a 256-bin histogram over [0, 256) whose counts are the
// exact density of a Gaussian mixture at the bin centres.
func mixture(t *testing.T, components ...[3]float64) *Histogram {
	t.Helper()
	counts := make([]float64, 256)
	for i := range counts {
		x := float64(i) + 0.5
		for _, c := range components {
			weight, mean, sigma := c[0], c[1], c[2]
			d := (x - mean) / sigma
			counts[i] += weight / (sigma * math.Sqrt(2*math.Pi)) * math.Exp(-d*d/2)
		}
	}
	h, err := NewHistogramFromCounts(counts, 0, 256)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// TestSymmetricBimodalSplitsAtMidpoint: two equal, well-separated Gaussians
// at 64 and 192 put every symmetric criterion's split at the midpoint 128.
func TestSymmetricBimodalSplitsAtMidpoint(t *testing.T) {
	h := mixture(t, [3]float64{1e6, 64, 16}, [3]float64{1e6, 192, 16})
	for _, method := range Methods() {
		if method == Li || method == Triangle {
			continue // not symmetric; checked below
		}
		got, err := h.Threshold(method)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		if math.Abs(got.Value-128) > 2 {
			t.Errorf("%s: boundary %v, want 128 +/- 2", method, got.Value)
		}
	}
	// Li's fixed point is the logarithmic mean of the class means,
	// (192-64)/ln 3 on levels starting at 0.
	got, err := h.Threshold(Li)
	if err != nil {
		t.Fatal(err)
	}
	if want := 128 / math.Log(3); math.Abs(got.Value-want) > 1.5 {
		t.Errorf("Li: boundary %v, want %v +/- 1.5", got.Value, want)
	}
}

// TestKittlerIllingworthFindsBayesBoundary: on a mixture with unequal
// weights and variances the minimum error threshold is the Bayes decision
// boundary w1 N(x; m1, s1) = w2 N(x; m2, s2).
func TestKittlerIllingworthFindsBayesBoundary(t *testing.T) {
	w1, m1, s1 := 3e6, 70.0, 10.0
	w2, m2, s2 := 1e6, 170.0, 20.0
	h := mixture(t, [3]float64{w1, m1, s1}, [3]float64{w2, m2, s2})
	// Solve ln(w1/s1) - (x-m1)^2/(2 s1^2) = ln(w2/s2) - (x-m2)^2/(2 s2^2).
	a := 1/(2*s2*s2) - 1/(2*s1*s1)
	b := m1/(s1*s1) - m2/(s2*s2)
	c := m2*m2/(2*s2*s2) - m1*m1/(2*s1*s1) + math.Log(w1/s1) - math.Log(w2/s2)
	root := (-b - math.Sqrt(b*b-4*a*c)) / (2 * a)
	if root < m1 || root > m2 {
		root = (-b + math.Sqrt(b*b-4*a*c)) / (2 * a)
	}
	got, err := h.Threshold(KittlerIllingworth)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got.Value-root) > 1.5 {
		t.Errorf("Kittler-Illingworth boundary %v, Bayes boundary %v", got.Value, root)
	}
	// Otsu's criterion is biased toward the wider class, away from the
	// boundary, which the minimum error criterion corrects.
	otsu, _ := h.Threshold(Otsu)
	if math.Abs(otsu.Value-root) <= math.Abs(got.Value-root) {
		t.Errorf("Otsu %v is not farther than Kittler-Illingworth %v from %v", otsu.Value, got.Value, root)
	}
}

// TestTwoValuesAreSeparatedByEveryMethod: with only two occupied bins every
// method must put them in different classes.
func TestTwoValuesAreSeparatedByEveryMethod(t *testing.T) {
	counts := make([]float64, 64)
	counts[10], counts[50] = 300, 700
	h, err := NewHistogramFromCounts(counts, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range Methods() {
		got, err := h.Threshold(method)
		if err != nil {
			t.Errorf("%s: %v", method, err)
			continue
		}
		thresholds := []Threshold{got}
		if ClassOf(thresholds, 10.5) != 0 || ClassOf(thresholds, 50.5) != 1 {
			t.Errorf("%s: split at bin %d does not separate the two values", method, got.Bin)
		}
	}
	thresholds, err := h.MultiOtsu(2)
	if err != nil || len(thresholds) != 1 || thresholds[0].Bin < 10 || thresholds[0].Bin >= 50 {
		t.Errorf("MultiOtsu(2) = %+v, %v", thresholds, err)
	}
	if _, err := h.MultiOtsu(3); err == nil {
		t.Error("MultiOtsu(3) with two occupied bins succeeded")
	}
}

// TestMultiOtsuSplitsEqualModesAtMidpoints and agrees with Otsu for two
// classes.
func TestMultiOtsuSplitsEqualModesAtMidpoints(t *testing.T) {
	h := mixture(t, [3]float64{1e6, 40, 8}, [3]float64{1e6, 128, 8}, [3]float64{1e6, 216, 8})
	got, err := h.MultiOtsu(3)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []float64{84, 172} {
		if math.Abs(got[i].Value-want) > 1 {
			t.Errorf("threshold %d: %v, want %v", i, got[i].Value, want)
		}
	}
	for _, v := range []float64{40, 128, 216} {
		if class := ClassOf(got, v); class != int(v)/88 {
			t.Errorf("ClassOf(%v) = %d", v, class)
		}
	}
	bimodal := mixture(t, [3]float64{2e6, 80, 15}, [3]float64{1e6, 180, 25})
	otsu, _ := bimodal.Threshold(Otsu)
	multi, err := bimodal.MultiOtsu(2)
	if err != nil || multi[0] != otsu {
		t.Errorf("MultiOtsu(2) = %+v, %v; Otsu %+v", multi, err, otsu)
	}
}

// TestTriangleFindsFarthestTailPoint: a peak with a convex quadratic tail
// h = H(1-u)^2 lies farthest below the peak-to-tail line at u = 1/2.
func TestTriangleFindsFarthestTailPoint(t *testing.T) {
	counts := make([]float64, 200)
	peak, length := 20, 121
	counts[peak-1] = 50
	for i := peak; i < peak+length; i++ {
		u := float64(i-peak) / float64(length)
		counts[i] = 1000 * (1 - u) * (1 - u)
	}
	h, err := NewHistogramFromCounts(counts, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.Threshold(Triangle)
	if err != nil {
		t.Fatal(err)
	}
	if want := peak + length/2; got.Bin < want-1 || got.Bin > want+1 {
		t.Errorf("Triangle bin %d, want %d +/- 1", got.Bin, want)
	}
	// Mirrored, the tail is on the left.
	mirrored := make([]float64, len(counts))
	for i, c := range counts {
		mirrored[len(counts)-1-i] = c
	}
	h, _ = NewHistogramFromCounts(mirrored, 0, 200)
	got, _ = h.Threshold(Triangle)
	if want := len(counts) - 1 - (peak + length/2); got.Bin < want-1 || got.Bin > want+1 {
		t.Errorf("mirrored Triangle bin %d, want %d +/- 1", got.Bin, want)
	}
}

// TestMaskedThresholdIgnoresPixelsOutsideTheROI: the image holds a bimodal
// region (0/100) and a bright block (1000); inside a mask over the region
// the threshold splits 0 from 100, globally it isolates the block.
func TestMaskedThresholdIgnoresPixelsOutsideTheROI(t *testing.T) {
	const columns, rows = 40, 20
	valueAt := func(x, y int) (float64, bool) {
		switch {
		case x >= 30:
			return 1000, true
		case (x+y)%3 == 0:
			return 100, true
		default:
			return 0, true
		}
	}
	mask := roi.NewRasterMask(columns, rows)
	for y := 0; y < rows; y++ {
		mask.SetRun(y, 0, 30)
	}
	inside, err := Compute(MaskValues(mask, valueAt), Otsu, HistogramOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if inside.Value <= 0 || inside.Value > 100 {
		t.Errorf("masked Otsu boundary %v, want in (0, 100]", inside.Value)
	}
	global, err := Compute(GridValues(columns, rows, valueAt), Otsu, HistogramOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if global.Value <= 100 || global.Value > 1000 {
		t.Errorf("global Otsu boundary %v, want in (100, 1000]", global.Value)
	}
	seg := roi.NewSegmentation3D(render.VolumeGeometry{}, columns, rows)
	seg.SetMask(3, mask)
	volume, err := Compute(SegmentationValues(seg, func(x, y, slice int) (float64, bool) {
		if slice != 3 {
			t.Fatalf("voxel of slice %d requested", slice)
		}
		return valueAt(x, y)
	}), Otsu, HistogramOptions{})
	if err != nil || volume != inside {
		t.Errorf("segmentation Otsu = %+v, %v; masked %+v", volume, err, inside)
	}
	if _, err := Compute(MaskValues(nil, valueAt), Otsu, HistogramOptions{}); !errors.Is(err, ErrNoSamples) {
		t.Errorf("nil mask: %v, want ErrNoSamples", err)
	}
}

func TestHistogramBinning(t *testing.T) {
	values := SliceValues([]float64{0, 1, 2, 3, math.NaN(), math.Inf(1), 10})
	h, err := NewHistogram(values, HistogramOptions{Bins: 5})
	if err != nil {
		t.Fatal(err)
	}
	if h.Lower != 0 || h.Upper != 10 || h.Samples != 5 || h.Excluded != 2 {
		t.Fatalf("histogram %+v", h)
	}
	if want := []float64{2, 2, 0, 0, 1}; !equalCounts(h.Counts, want) {
		t.Errorf("counts %v, want %v (the maximum falls in the last bin)", h.Counts, want)
	}
	if h.MinValue != 0 || h.MaxValue != 10 || h.BinCenter(0) != 1 || h.BinLower(5) != 10 || h.BinOf(9.99) != 4 {
		t.Errorf("range helpers: %+v", h)
	}

	h, err = NewHistogram(values, HistogramOptions{Bins: 4, Lower: 0.5, Upper: 4.5})
	if err != nil {
		t.Fatal(err)
	}
	if want := []float64{1, 1, 1, 0}; !equalCounts(h.Counts, want) || h.Excluded != 4 {
		t.Errorf("explicit range counts %v excluded %d", h.Counts, h.Excluded)
	}

	h, err = NewHistogram(SliceValues([]float64{-3, -1, 2}), HistogramOptions{BinWidth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if h.Lower != -3.5 || h.Upper != 2.5 || h.Bins() != 6 || h.Counts[0] != 1 || h.Counts[2] != 1 || h.Counts[5] != 1 {
		t.Errorf("unit bins %+v", h)
	}
	h, err = NewHistogram(SliceValues([]float64{0, 10, 25}), HistogramOptions{BinWidth: 10})
	if err != nil || h.Lower != -5 || h.Bins() != 4 {
		t.Errorf("width-10 bins %+v, %v", h, err)
	}

	if _, err := NewHistogram(SliceValues([]float64{math.NaN()}), HistogramOptions{}); !errors.Is(err, ErrNoSamples) {
		t.Errorf("NaN only: %v", err)
	}
	if _, err := NewHistogram(values, HistogramOptions{BinWidth: 1, Lower: 0, Upper: 1}); err == nil {
		t.Error("BinWidth with an explicit range accepted")
	}
	if _, err := NewHistogram(SliceValues([]float64{0, 1e12}), HistogramOptions{BinWidth: 1}); !errors.Is(err, ErrTooManyBins) {
		t.Errorf("huge unit-bin range: %v", err)
	}
	single, err := NewHistogram(SliceValues([]float64{7, 7, 7}), HistogramOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := single.Threshold(Otsu); !errors.Is(err, ErrSingleValue) {
		t.Errorf("single value threshold: %v", err)
	}
	if _, err := NewHistogramFromCounts([]float64{1, -1}, 0, 2); err == nil {
		t.Error("negative count accepted")
	}
}

func TestMethodNames(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range Methods() {
		parsed, err := ParseMethod(m.Label())
		if err != nil || parsed != m {
			t.Errorf("ParseMethod(%q) = %q, %v", m.Label(), parsed, err)
		}
		if parsed, err := ParseMethod(" " + string(m) + " "); err != nil || parsed != m {
			t.Errorf("ParseMethod(%q) = %q, %v", m, parsed, err)
		}
		if seen[m.Label()] {
			t.Errorf("duplicate label %q", m.Label())
		}
		seen[m.Label()] = true
	}
	if _, err := ParseMethod("median"); err == nil {
		t.Error("unknown method parsed")
	}
	h := mixture(t, [3]float64{1, 64, 16}, [3]float64{1, 192, 16})
	if _, err := h.Threshold("median"); err == nil {
		t.Error("unknown method thresholded")
	}
}

// TestIntermodesNeedsTwoPeaks: a unimodal histogram never smooths to two
// peaks.
func TestIntermodesNeedsTwoPeaks(t *testing.T) {
	h := mixture(t, [3]float64{1e6, 128, 20})
	for _, method := range []Method{Intermodes, Minimum} {
		if _, err := h.Threshold(method); !errors.Is(err, ErrNotBimodal) {
			t.Errorf("%s on one mode: %v, want ErrNotBimodal", method, err)
		}
	}
}

// TestQuadraticMethodsRefuseHugeSpans keeps the O(bins^2) methods bounded.
func TestQuadraticMethodsRefuseHugeSpans(t *testing.T) {
	counts := make([]float64, maxQuadraticBins+1)
	counts[0], counts[len(counts)-1] = 1, 1
	h, err := NewHistogramFromCounts(counts, 0, float64(len(counts)))
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []Method{Huang, Shanbhag, Intermodes, Minimum} {
		if _, err := h.Threshold(method); !errors.Is(err, ErrTooManyBins) {
			t.Errorf("%s: %v, want ErrTooManyBins", method, err)
		}
	}
	if _, err := h.MultiOtsu(2); !errors.Is(err, ErrTooManyBins) {
		t.Errorf("MultiOtsu: %v, want ErrTooManyBins", err)
	}
	if got, err := h.Threshold(Otsu); err != nil || got.Bin != 0 {
		t.Errorf("Otsu on a wide span = %+v, %v", got, err)
	}
}

func equalCounts(got, want []float64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
