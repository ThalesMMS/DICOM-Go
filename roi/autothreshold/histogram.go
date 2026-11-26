package autothreshold

import (
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/roi"
)

// DefaultBins is the number of bins used when HistogramOptions leaves both
// Bins and BinWidth unset, the 256 gray levels the methods were published for.
const DefaultBins = 256

// MaxBins bounds the number of bins of one histogram.
const MaxBins = 1 << 20

var (
	// ErrNoSamples reports a histogram without any counted sample.
	ErrNoSamples = errors.New("autothreshold: no finite samples")
	// ErrSingleValue reports a histogram whose samples all fall in one bin,
	// which no threshold can split.
	ErrSingleValue = errors.New("autothreshold: all samples fall in one bin")
	// ErrNotBimodal reports that Intermodes or Minimum could not smooth the
	// histogram down to exactly two peaks.
	ErrNotBimodal = errors.New("autothreshold: histogram does not smooth to two peaks")
	// ErrTooManyBins reports a histogram whose occupied span is too wide for
	// a method whose cost grows with the square of the number of bins.
	ErrTooManyBins = errors.New("autothreshold: too many bins for this method")
)

// Values is a re-iterable sequence of samples: calling it feeds every sample
// to yield until yield returns false. NewHistogram may call it twice (once to
// find the range), so it must produce the same samples each time. It has the
// shape of iter.Seq[float64].
type Values func(yield func(float64) bool)

// SliceValues iterates over a slice.
func SliceValues(values []float64) Values {
	return func(yield func(float64) bool) {
		for _, v := range values {
			if !yield(v) {
				return
			}
		}
	}
}

// GridValues iterates over every available pixel of a columns x rows grid in
// row-major order; valueAt reports false for pixels without a value.
func GridValues(columns, rows int, valueAt func(x, y int) (float64, bool)) Values {
	return func(yield func(float64) bool) {
		if valueAt == nil {
			return
		}
		for y := 0; y < rows; y++ {
			for x := 0; x < columns; x++ {
				if v, ok := valueAt(x, y); ok && !yield(v) {
					return
				}
			}
		}
	}
}

// MaskValues iterates over the available pixels inside mask, row by row. A
// nil mask yields nothing; use GridValues for the whole image.
func MaskValues(mask *roi.RasterMask, valueAt func(x, y int) (float64, bool)) Values {
	return func(yield func(float64) bool) {
		if valueAt != nil {
			maskValues(mask, valueAt, yield)
		}
	}
}

// maskValues feeds the pixels of mask to yield and reports whether the
// iteration ran to the end.
func maskValues(mask *roi.RasterMask, valueAt func(x, y int) (float64, bool), yield func(float64) bool) bool {
	if mask == nil {
		return true
	}
	for y := 0; y < mask.Rows; y++ {
		for _, run := range mask.Runs(y) {
			for x := run.Start; x < run.End; x++ {
				if v, ok := valueAt(x, y); ok && !yield(v) {
					return false
				}
			}
		}
	}
	return true
}

// SegmentationValues iterates over the available voxels set in seg, slice by
// slice in ascending order.
func SegmentationValues(seg *roi.Segmentation3D, valueAt func(x, y, slice int) (float64, bool)) Values {
	return func(yield func(float64) bool) {
		if seg == nil || valueAt == nil {
			return
		}
		for _, slice := range seg.Slices() {
			mask, _ := seg.MaskAt(slice)
			at := func(x, y int) (float64, bool) { return valueAt(x, y, slice) }
			if !maskValues(mask, at, yield) {
				return
			}
		}
	}
}

// HistogramOptions selects the bins of NewHistogram. With the zero value the
// histogram has DefaultBins bins spanning the observed sample range.
type HistogramOptions struct {
	// Bins is the number of equal-width bins; 0 means DefaultBins.
	Bins int
	// BinWidth, when positive, replaces Bins with bins of this width centred
	// on its integer multiples (1 gives one bin per integer, the natural
	// histogram of stored values or Hounsfield units). It cannot be combined
	// with an explicit range.
	BinWidth float64
	// Lower and Upper, when Upper > Lower, fix the histogram range instead of
	// the observed one; samples outside it are excluded.
	Lower, Upper float64
}

// Histogram holds sample counts in equal-width bins. Bin i covers
// [Lower + i*w, Lower + (i+1)*w) with w = (Upper-Lower)/len(Counts); the last
// bin also includes Upper.
type Histogram struct {
	Counts       []float64
	Lower, Upper float64
	// Samples is the number of counted samples; Excluded counts the NaN,
	// infinite and out-of-range samples that were skipped.
	Samples  int
	Excluded int
	// MinValue and MaxValue are the smallest and largest counted samples
	// (NaN when the histogram was built from counts).
	MinValue, MaxValue float64
}

// NewHistogram builds a histogram of the finite samples of values.
func NewHistogram(values Values, opts HistogramOptions) (*Histogram, error) {
	if values == nil {
		return nil, ErrNoSamples
	}
	explicit := opts.Upper > opts.Lower
	if opts.Bins < 0 || math.IsNaN(opts.BinWidth) || math.IsInf(opts.BinWidth, 0) || opts.BinWidth < 0 {
		return nil, fmt.Errorf("autothreshold: invalid bins %d or bin width %v", opts.Bins, opts.BinWidth)
	}
	if explicit && opts.BinWidth > 0 {
		return nil, fmt.Errorf("autothreshold: BinWidth cannot be combined with an explicit range")
	}
	if explicit && (math.IsInf(opts.Lower, 0) || math.IsInf(opts.Upper, 0) || math.IsInf(opts.Upper-opts.Lower, 0)) {
		return nil, fmt.Errorf("autothreshold: histogram range must be finite")
	}
	bins := opts.Bins
	if bins == 0 {
		bins = DefaultBins
	}
	if bins > MaxBins {
		return nil, fmt.Errorf("%w: %d bins requested, at most %d", ErrTooManyBins, bins, MaxBins)
	}
	h := &Histogram{MinValue: math.Inf(1), MaxValue: math.Inf(-1)}
	lower, upper := opts.Lower, opts.Upper
	if !explicit {
		lower, upper = observedRange(values)
		if lower > upper {
			return nil, ErrNoSamples
		}
		if opts.BinWidth > 0 {
			first, last := math.Round(lower/opts.BinWidth), math.Round(upper/opts.BinWidth)
			if n := last - first + 1; n > MaxBins || math.IsNaN(n) {
				return nil, fmt.Errorf("%w: bin width %v needs %v bins", ErrTooManyBins, opts.BinWidth, n)
			}
			bins = int(last-first) + 1
			lower = (first - 0.5) * opts.BinWidth
			upper = lower + float64(bins)*opts.BinWidth
		} else if upper == lower {
			// One distinct value: a single bin of unit width around it.
			bins = 1
			lower, upper = lower-0.5, upper+0.5
		} else if math.IsInf(upper-lower, 0) {
			return nil, fmt.Errorf("autothreshold: sample range overflows")
		}
	}
	h.Lower, h.Upper = lower, upper
	h.Counts = make([]float64, bins)
	scale := float64(bins) / (upper - lower)
	values(func(v float64) bool {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < lower || v > upper {
			h.Excluded++
			return true
		}
		i := int((v - lower) * scale)
		if i >= bins {
			i = bins - 1
		} else if i < 0 {
			i = 0
		}
		h.Counts[i]++
		h.Samples++
		h.MinValue = math.Min(h.MinValue, v)
		h.MaxValue = math.Max(h.MaxValue, v)
		return true
	})
	if h.Samples == 0 {
		return nil, ErrNoSamples
	}
	return h, nil
}

func observedRange(values Values) (lower, upper float64) {
	lower, upper = math.Inf(1), math.Inf(-1)
	values(func(v float64) bool {
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			lower, upper = math.Min(lower, v), math.Max(upper, v)
		}
		return true
	})
	return lower, upper
}

// NewHistogramFromCounts wraps precomputed bin counts spanning [lower, upper].
// Counts must be finite and non-negative; they may be weights.
func NewHistogramFromCounts(counts []float64, lower, upper float64) (*Histogram, error) {
	if len(counts) == 0 || len(counts) > MaxBins {
		return nil, fmt.Errorf("autothreshold: %d bins, want 1..%d", len(counts), MaxBins)
	}
	if !(upper > lower) || math.IsInf(lower, 0) || math.IsInf(upper, 0) || math.IsInf(upper-lower, 0) {
		return nil, fmt.Errorf("autothreshold: invalid histogram range [%v, %v]", lower, upper)
	}
	h := &Histogram{Counts: append([]float64(nil), counts...), Lower: lower, Upper: upper, MinValue: math.NaN(), MaxValue: math.NaN()}
	total := 0.0
	for i, c := range counts {
		if math.IsNaN(c) || math.IsInf(c, 0) || c < 0 {
			return nil, fmt.Errorf("autothreshold: bin %d count %v is not a finite non-negative number", i, c)
		}
		total += c
	}
	if total == 0 {
		return nil, ErrNoSamples
	}
	h.Samples = int(math.Min(math.Round(total), math.MaxInt32))
	return h, nil
}

// Bins returns the number of bins.
func (h *Histogram) Bins() int { return len(h.Counts) }

// BinWidth returns the width of one bin.
func (h *Histogram) BinWidth() float64 { return (h.Upper - h.Lower) / float64(len(h.Counts)) }

// BinLower returns the lower edge of bin i (BinLower(Bins()) is Upper).
func (h *Histogram) BinLower(i int) float64 {
	if i == len(h.Counts) {
		return h.Upper
	}
	return h.Lower + float64(i)*h.BinWidth()
}

// BinCenter returns the centre of bin i.
func (h *Histogram) BinCenter(i int) float64 { return h.Lower + (float64(i)+0.5)*h.BinWidth() }

// BinOf returns the bin holding v, clamped to the histogram.
func (h *Histogram) BinOf(v float64) int {
	i := int(math.Floor((v - h.Lower) / h.BinWidth()))
	if i < 0 {
		return 0
	}
	if i >= len(h.Counts) {
		return len(h.Counts) - 1
	}
	return i
}

// Total returns the sum of the counts.
func (h *Histogram) Total() float64 {
	total := 0.0
	for _, c := range h.Counts {
		total += c
	}
	return total
}

// span returns the first and last occupied bins.
func (h *Histogram) span() (lo, hi int, err error) {
	if h == nil || len(h.Counts) == 0 {
		return 0, 0, ErrNoSamples
	}
	lo, hi = -1, -1
	for i, c := range h.Counts {
		if c > 0 {
			if lo < 0 {
				lo = i
			}
			hi = i
		}
	}
	if lo < 0 {
		return 0, 0, ErrNoSamples
	}
	if lo == hi {
		return 0, 0, ErrSingleValue
	}
	return lo, hi, nil
}
