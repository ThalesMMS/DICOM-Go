package imagefilter

// Histogram matching by quantile landmarks.
//
// Adapted from ITK's HistogramMatchingImageFilter (Insight Toolkit, commit
// 35b196e13c366e0386389a71c936a0436c568cea, Apache-2.0, Copyright NumFOCUS;
// see THIRD_PARTY_NOTICES.md): the landmark table, the piecewise-linear
// mapping with its lower and upper segments and the histogram quantile
// interpolation follow
// Modules/Filtering/ImageIntensity/include/itkHistogramMatchingImageFilter.hxx
// (BeforeThreadedGenerateData, DynamicThreadedGenerateData,
// ConstructHistogramFromIntensityRange) and Histogram::Quantile in
// Modules/Numerics/Statistics/include/itkHistogram.hxx.
//
// Modifications from ITK:
//   - The histogram covers exactly [threshold, max]. ITK widens its first bin
//     down to the image minimum although samples below the threshold are not
//     counted, which can put a landmark below the threshold and make the
//     landmark table non-monotonic; here landmarks are always non-decreasing.
//   - A reference given as a histogram keeps its own threshold. ITK takes the
//     first bin's lower edge (the image minimum) as the reference threshold
//     in that mode, so image and histogram references disagree; here they
//     give the same mapping.
//   - The threshold is the exact mean (ITK truncates it to the pixel type),
//     samples are float32 and the output is float64 without a pixel-type
//     cast; NaN and ±Inf samples are ignored when histograms are built and
//     passed through unchanged by the mapping.
//   - Degenerate inputs (no finite samples, a constant image) are rejected
//     instead of producing a constant or undefined mapping.
//   - Defaults are 1024 histogram levels and 7 match points (ITK: 256 and 1),
//     the landmark count commonly used to standardize MR intensities.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

const (
	// DefaultHistogramMatchLevels is the default number of histogram bins
	// between the threshold and the maximum.
	DefaultHistogramMatchLevels = 1024
	// DefaultHistogramMatchPoints is the default number of quantile
	// landmarks between the threshold and the maximum (7: the octiles).
	DefaultHistogramMatchPoints = 7
	// MaxHistogramMatchLevels bounds HistogramOptions.Levels.
	MaxHistogramMatchLevels = 1 << 20
	// MaxHistogramMatchPoints bounds HistogramMatchOptions.MatchPoints.
	MaxHistogramMatchPoints = 1 << 12
)

// ErrInvalidHistogramMatch reports samples, histograms or options histogram
// matching cannot use.
var ErrInvalidHistogramMatch = errors.New("imagefilter: invalid histogram matching input")

// HistogramOptions configures the intensity histogram of one image.
type HistogramOptions struct {
	// Levels is the number of equal-width bins covering [Threshold, Max]
	// (0 selects DefaultHistogramMatchLevels).
	Levels int
	// IncludeBackground counts every sample. By default (false) only the
	// samples at or above the mean intensity are counted, as ITK's
	// ThresholdAtMeanIntensity does: in MR and other images with a large
	// dark background, the background would otherwise dominate the
	// quantiles. Samples below the threshold are still mapped, linearly
	// between the minimum and the threshold.
	IncludeBackground bool
}

func (o HistogramOptions) levels() (int, error) {
	levels := o.Levels
	if levels == 0 {
		levels = DefaultHistogramMatchLevels
	}
	if levels < 1 || levels > MaxHistogramMatchLevels {
		return 0, fmt.Errorf("%w: Levels=%d outside 1..%d", ErrInvalidHistogramMatch, o.Levels, MaxHistogramMatchLevels)
	}
	return levels, nil
}

// IntensityHistogram is the histogram of one image that histogram matching
// compares: Levels equal-width bins between Threshold and Max, counting the
// samples in that closed range. Min, Max and Mean describe every finite
// sample. A reference histogram can be computed once and matched against
// many sources.
type IntensityHistogram struct {
	Min, Max, Mean float64
	// Threshold is the lower bound of the counted range: Mean by default,
	// Min when the histogram includes the background.
	Threshold float64
	// Counts holds one count per bin; bin i covers
	// [Threshold+i·w, Threshold+(i+1)·w) with w = (Max−Threshold)/len(Counts),
	// the last bin also holding Max.
	Counts []uint64
	// Samples is the number of finite samples; Counted of them lie in
	// [Threshold, Max].
	Samples, Counted uint64
}

// NewIntensityHistogram builds the histogram of samples. Non-finite samples
// are ignored. An image with no finite samples or without contrast (every
// finite sample equal) has nothing to match and is rejected.
func NewIntensityHistogram(samples []float32, opts HistogramOptions) (*IntensityHistogram, error) {
	levels, err := opts.levels()
	if err != nil {
		return nil, err
	}
	h := &IntensityHistogram{Min: math.Inf(1), Max: math.Inf(-1)}
	var sum float64
	for _, s := range samples {
		v := float64(s)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		h.Samples++
		sum += v
		h.Min = math.Min(h.Min, v)
		h.Max = math.Max(h.Max, v)
	}
	if h.Samples == 0 {
		return nil, fmt.Errorf("%w: no finite samples", ErrInvalidHistogramMatch)
	}
	if !(h.Max > h.Min) {
		return nil, fmt.Errorf("%w: every sample equals %v", ErrInvalidHistogramMatch, h.Min)
	}
	h.Mean = sum / float64(h.Samples)
	// Rounding can put the mean of nearly equal samples outside [Min, Max].
	h.Mean = math.Min(math.Max(h.Mean, h.Min), h.Max)
	h.Threshold = h.Mean
	if opts.IncludeBackground {
		h.Threshold = h.Min
	}
	if !(h.Max > h.Threshold) {
		return nil, fmt.Errorf("%w: no intensity range above the threshold %v", ErrInvalidHistogramMatch, h.Threshold)
	}
	h.Counts = make([]uint64, levels)
	scale := float64(levels) / (h.Max - h.Threshold)
	for _, s := range samples {
		v := float64(s)
		if !(v >= h.Threshold && v <= h.Max) {
			continue
		}
		bin := int((v - h.Threshold) * scale)
		if bin >= levels {
			bin = levels - 1
		}
		h.Counts[bin]++
		h.Counted++
	}
	return h, nil
}

// validate checks a histogram built elsewhere (for example decoded from a
// cache) before it is used as a reference.
func (h *IntensityHistogram) validate() error {
	if h == nil {
		return fmt.Errorf("%w: nil histogram", ErrInvalidHistogramMatch)
	}
	for _, v := range []float64{h.Min, h.Max, h.Threshold} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%w: non-finite histogram bounds", ErrInvalidHistogramMatch)
		}
	}
	if !(h.Min <= h.Threshold && h.Threshold < h.Max) {
		return fmt.Errorf("%w: histogram bounds min %v, threshold %v, max %v", ErrInvalidHistogramMatch, h.Min, h.Threshold, h.Max)
	}
	if len(h.Counts) == 0 || len(h.Counts) > MaxHistogramMatchLevels {
		return fmt.Errorf("%w: %d histogram bins", ErrInvalidHistogramMatch, len(h.Counts))
	}
	var total uint64
	for _, c := range h.Counts {
		total += c
	}
	if total == 0 {
		return fmt.Errorf("%w: empty histogram", ErrInvalidHistogramMatch)
	}
	return nil
}

func (h *IntensityHistogram) binMin(i int) float64 {
	return h.Threshold + float64(i)*(h.Max-h.Threshold)/float64(len(h.Counts))
}

func (h *IntensityHistogram) binMax(i int) float64 {
	if i == len(h.Counts)-1 {
		return h.Max
	}
	return h.binMin(i + 1)
}

// Quantile returns the intensity below which a fraction p of the counted
// samples lie, interpolating linearly inside the bin where the cumulative
// count reaches p (ITK's Histogram::Quantile: walking up from the first bin
// for p < 0.5 and down from the last bin otherwise). p is clamped to [0, 1].
func (h *IntensityHistogram) Quantile(p float64) float64 {
	if h == nil || len(h.Counts) == 0 {
		return math.NaN()
	}
	p = math.Min(math.Max(p, 0), 1)
	var total float64
	for _, c := range h.Counts {
		total += float64(c)
	}
	if total == 0 {
		return math.NaN()
	}
	size := len(h.Counts)
	var cumulated, fn, pnPrev float64
	if p < 0.5 {
		n, pn := 0, 0.0
		for {
			fn = float64(h.Counts[n])
			cumulated += fn
			pnPrev = pn
			pn = cumulated / total
			n++
			if !(n < size && pn < p) {
				break
			}
		}
		lo, hi := h.binMin(n-1), h.binMax(n-1)
		if fn == 0 {
			return lo
		}
		return lo + ((p-pnPrev)/(fn/total))*(hi-lo)
	}
	n, m, pn := size-1, 0, 1.0
	for {
		fn = float64(h.Counts[n])
		cumulated += fn
		pnPrev = pn
		pn = 1 - cumulated/total
		n--
		m++
		if !(m < size && pn > p) {
			break
		}
	}
	lo, hi := h.binMin(n+1), h.binMax(n+1)
	if fn == 0 {
		return hi
	}
	return hi - ((pnPrev-p)/(fn/total))*(hi-lo)
}

// HistogramMatchOptions configures MatchHistograms and
// NewHistogramMatchFromSamples.
type HistogramMatchOptions struct {
	HistogramOptions
	// MatchPoints is the number of quantile landmarks between the threshold
	// and the maximum (0 selects DefaultHistogramMatchPoints): landmark j
	// is the j/(MatchPoints+1) quantile of each histogram.
	MatchPoints int
	// Workers bounds the goroutines MatchHistograms and Apply use (0
	// selects the shared concurrency budget). The result does not depend on it.
	Workers int
}

func (o HistogramMatchOptions) matchPoints() (int, error) {
	points := o.MatchPoints
	if points == 0 {
		points = DefaultHistogramMatchPoints
	}
	if points < 1 || points > MaxHistogramMatchPoints {
		return 0, fmt.Errorf("%w: MatchPoints=%d outside 1..%d", ErrInvalidHistogramMatch, o.MatchPoints, MaxHistogramMatchPoints)
	}
	return points, nil
}

// HistogramLandmark is one row of the landmark table: the intensity of the
// source and of the reference at the same quantile of their histograms.
// Fraction is 0 for the thresholds and 1 for the maxima.
type HistogramLandmark struct {
	Fraction          float64
	Source, Reference float64
}

// HistogramMatch is a monotonic, piecewise-linear intensity mapping that
// makes the histogram of a source image look like that of a reference image
// at MatchPoints quantile landmarks. Source intensities between two
// landmarks are interpolated linearly; intensities below the source
// threshold map linearly from the source minimum (to the reference minimum)
// up to the threshold (to the reference threshold); intensities at or above
// the source maximum map to the reference maximum.
//
// Matching is a display and analysis transform, like a window: it changes
// the values a viewer shows or a comparison uses so that one window serves
// a prior and a current MR examination, whose intensities are not
// calibrated. It never changes stored pixel data, and matched values are
// not measurements; callers must report the transform wherever matched
// values are shown.
//
// A HistogramMatch is immutable and safe for concurrent use.
type HistogramMatch struct {
	source, reference    []float64 // landmark intensities, MatchPoints+2 each
	fractions            []float64
	gradients            []float64
	sourceMin, sourceMax float64
	refMin, refMax       float64
	lowerGrad, upperGrad float64
	sourceHist, refHist  *IntensityHistogram
}

// NewHistogramMatch builds the mapping that matches source to reference at
// matchPoints quantile landmarks (0 selects DefaultHistogramMatchPoints).
// The histograms may have different bin counts.
func NewHistogramMatch(source, reference *IntensityHistogram, matchPoints int) (*HistogramMatch, error) {
	points, err := HistogramMatchOptions{MatchPoints: matchPoints}.matchPoints()
	if err != nil {
		return nil, err
	}
	if err := source.validate(); err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	if err := reference.validate(); err != nil {
		return nil, fmt.Errorf("reference: %w", err)
	}
	n := points + 2
	m := &HistogramMatch{
		source:     make([]float64, n),
		reference:  make([]float64, n),
		fractions:  make([]float64, n),
		gradients:  make([]float64, n-1),
		sourceMin:  source.Min,
		sourceMax:  source.Max,
		refMin:     reference.Min,
		refMax:     reference.Max,
		sourceHist: source,
		refHist:    reference,
	}
	m.source[0], m.reference[0] = source.Threshold, reference.Threshold
	m.source[n-1], m.reference[n-1] = source.Max, reference.Max
	m.fractions[n-1] = 1
	delta := 1 / float64(points+1)
	for j := 1; j <= points; j++ {
		p := float64(j) * delta
		m.fractions[j] = p
		m.source[j] = source.Quantile(p)
		m.reference[j] = reference.Quantile(p)
	}
	// Floating-point rounding in the quantile interpolation must not break
	// the ordering the lookup relies on.
	for j := 1; j < n; j++ {
		m.source[j] = math.Max(m.source[j], m.source[j-1])
		m.reference[j] = math.Max(m.reference[j], m.reference[j-1])
	}
	for j := 0; j < n-1; j++ {
		m.gradients[j] = slope(m.reference[j+1]-m.reference[j], m.source[j+1]-m.source[j])
	}
	m.lowerGrad = slope(m.reference[0]-m.refMin, m.source[0]-m.sourceMin)
	// As in ITK the upper segment is flat: the last landmark is the source
	// maximum, so larger values (from slices the histogram did not see) map
	// to the reference maximum.
	m.upperGrad = 0
	return m, nil
}

// slope returns rise/run, or 0 when the run is (almost) empty, as ITK's
// Math::NotAlmostEquals guard does.
func slope(rise, run float64) float64 {
	if math.Abs(run) <= 4*math.SmallestNonzeroFloat64 || math.Abs(run) <= 1e-12*math.Max(1, math.Abs(rise)) {
		return 0
	}
	return rise / run
}

// NewHistogramMatchFromSamples builds the histograms of source and reference
// samples with opts and the mapping between them.
func NewHistogramMatchFromSamples(source, reference []float32, opts HistogramMatchOptions) (*HistogramMatch, error) {
	points, err := opts.matchPoints()
	if err != nil {
		return nil, err
	}
	src, err := NewIntensityHistogram(source, opts.HistogramOptions)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	ref, err := NewIntensityHistogram(reference, opts.HistogramOptions)
	if err != nil {
		return nil, fmt.Errorf("reference: %w", err)
	}
	return NewHistogramMatch(src, ref, points)
}

// Map returns the matched intensity of a source intensity. NaN and ±Inf are
// returned unchanged.
func (m *HistogramMatch) Map(v float64) float64 {
	if m == nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	n := len(m.source)
	// j is the first landmark strictly above v (ITK's linear scan).
	j := sort.Search(n, func(i int) bool { return v < m.source[i] })
	switch j {
	case 0:
		return m.refMin + (v-m.sourceMin)*m.lowerGrad
	case n:
		return m.refMax + (v-m.sourceMax)*m.upperGrad
	default:
		return m.reference[j-1] + (v-m.source[j-1])*m.gradients[j-1]
	}
}

// Landmarks returns the landmark table: the thresholds, the MatchPoints
// quantiles and the maxima of the source and the reference.
func (m *HistogramMatch) Landmarks() []HistogramLandmark {
	if m == nil {
		return nil
	}
	out := make([]HistogramLandmark, len(m.source))
	for i := range out {
		out[i] = HistogramLandmark{Fraction: m.fractions[i], Source: m.source[i], Reference: m.reference[i]}
	}
	return out
}

// MatchPoints returns the number of quantile landmarks between the
// thresholds and the maxima.
func (m *HistogramMatch) MatchPoints() int {
	if m == nil {
		return 0
	}
	return len(m.source) - 2
}

// SourceHistogram and ReferenceHistogram return the histograms the mapping
// was built from. They are shared, not copied, and must not be modified.
func (m *HistogramMatch) SourceHistogram() *IntensityHistogram { return m.sourceHist }

// ReferenceHistogram returns the reference histogram; see SourceHistogram.
func (m *HistogramMatch) ReferenceHistogram() *IntensityHistogram { return m.refHist }

// String summarizes the mapping for status lines and logs.
func (m *HistogramMatch) String() string {
	if m == nil {
		return "histogram match: none"
	}
	return fmt.Sprintf("histogram match: %d landmarks, source [%.6g, %.6g] threshold %.6g -> reference [%.6g, %.6g] threshold %.6g",
		m.MatchPoints(), m.sourceMin, m.sourceMax, m.source[0], m.refMin, m.refMax, m.reference[0])
}

// Apply maps src into dst, which must have the same length; dst may be src.
// The work is split over workers goroutines (0 selects the shared concurrency budget); ctx is
// checked between chunks and on cancellation dst may be partially written.
func (m *HistogramMatch) Apply(ctx context.Context, dst, src []float32, workers int) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if m == nil {
		return fmt.Errorf("%w: nil mapping", ErrInvalidHistogramMatch)
	}
	if len(dst) != len(src) {
		return fmt.Errorf("%w: %d destination and %d source samples", ErrInvalidHistogramMatch, len(dst), len(src))
	}
	if workers <= 0 {
		workers = parallel.DefaultWorkers()
	}
	const chunk = 1 << 16
	chunks := (len(src) + chunk - 1) / chunk
	return runParallel(ctx, chunks, workers, func() func(int) {
		return func(c int) {
			lo, hi := c*chunk, min((c+1)*chunk, len(src))
			for i, v := range src[lo:hi] {
				dst[lo+i] = float32(m.Map(float64(v)))
			}
		}
	})
}

// MatchHistograms returns a new volume holding source with its intensities
// matched to reference, and the mapping used. Neither input is modified;
// only the grid (dimensions and spacing) of source is copied, so the two
// volumes need not share a grid or be registered. The histograms ignore
// the geometry: matching compares intensity distributions only.
func MatchHistograms(ctx context.Context, source, reference *Volume, opts HistogramMatchOptions) (*Volume, *HistogramMatch, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := source.Validate(); err != nil {
		return nil, nil, fmt.Errorf("source: %w", err)
	}
	if err := reference.Validate(); err != nil {
		return nil, nil, fmt.Errorf("reference: %w", err)
	}
	match, err := NewHistogramMatchFromSamples(source.Data, reference.Data, opts)
	if err != nil {
		return nil, nil, err
	}
	out := &Volume{Dims: source.Dims, SpacingMM: source.SpacingMM, Data: make([]float32, len(source.Data))}
	if err := match.Apply(ctx, out.Data, source.Data, opts.Workers); err != nil {
		return nil, nil, err
	}
	return out, match, nil
}
