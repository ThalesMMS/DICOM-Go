package roi

import (
	"context"
	"fmt"
	"math"
	"math/bits"
	"sort"
	"sync"
)

// IntensityMoments are the count, extremes and moments of the values inside
// an object, accumulated in one streaming pass without copying or sorting
// the values (#1255): Welford's update of the deviations from the first
// value for the variance (the shift keeps a large common offset, such as
// 1e9 ± 1, from costing precision) and Neumaier's compensated sum for Sum
// and Mean. Use them when the median and
// percentiles are not needed; IntensityStatistics adds those. The fields
// have IntensityStatistics' definitions.
type IntensityMoments struct {
	// Label is the label value for LabelIntensityMoments, 0 otherwise.
	Label uint16
	// Count is the number of finite values; NaN and ±Inf values are skipped.
	Count int
	// NonFinite is the number of skipped NaN or ±Inf values.
	NonFinite         int
	Min, Max          float64
	Sum, Mean         float64
	Variance          float64 // sample variance (Count-1); 0 when Count < 2
	StandardDeviation float64
}

// IntensityAccumulator accumulates IntensityMoments one value at a time; the
// zero value is empty. It holds no values, so it costs the same for any
// object size.
type IntensityAccumulator struct {
	count, nonFinite int
	min, max         float64
	sum, compensate  float64 // Neumaier's running sum and its correction
	shift            float64 // the first finite value
	mean, m2         float64 // Welford's running mean of v-shift and squared deviations
}

// Add adds one value; NaN and ±Inf are counted as non-finite and skipped.
func (a *IntensityAccumulator) Add(v float64) {
	if !finite(v) {
		a.nonFinite++
		return
	}
	a.count++
	if a.count == 1 {
		a.min, a.max, a.shift = v, v, v
	} else if v < a.min {
		a.min = v
	} else if v > a.max {
		a.max = v
	}
	t := a.sum + v
	if math.Abs(a.sum) >= math.Abs(v) {
		a.compensate += (a.sum - t) + v
	} else {
		a.compensate += (v - t) + a.sum
	}
	a.sum = t
	d := v - a.shift
	delta := d - a.mean
	a.mean += delta / float64(a.count)
	a.m2 += delta * (d - a.mean)
}

// Moments returns the moments of the values added so far.
func (a *IntensityAccumulator) Moments() IntensityMoments {
	m := IntensityMoments{Count: a.count, NonFinite: a.nonFinite}
	if a.count == 0 {
		return m
	}
	m.Min, m.Max = a.min, a.max
	m.Sum = a.sum + a.compensate
	m.Mean = m.Sum / float64(a.count)
	if a.count > 1 {
		m.Variance = math.Max(0, a.m2/float64(a.count-1))
		m.StandardDeviation = math.Sqrt(m.Variance)
	}
	return m
}

// NewIntensityMoments returns the moments of values without copying them.
func NewIntensityMoments(values []float64) IntensityMoments {
	var a IntensityAccumulator
	for _, v := range values {
		a.Add(v)
	}
	return a.Moments()
}

// IntensityStatistics are exact order and moment statistics of the values
// inside an object (#1097): the base for median-based reporting and for PET
// measures such as MTV and TLG (Count × voxel volume, Mean × volume). The
// definitions follow ITK's StatisticsLabelObject: Variance is the unbiased
// sample variance (divisor Count-1), and the median of an even count is the
// mean of the two middle values. Unlike ITK's histogram-based median, the
// median and percentiles here are exact.
//
// The moments come from one streaming pass (see IntensityMoments) and the
// median from a linear-time selection; the values are sorted only when a
// percentile is first asked for (#1255).
type IntensityStatistics struct {
	// Label is the label value for LabelIntensityStatistics, 0 otherwise.
	Label uint16
	// Count is the number of finite values; NaN and ±Inf values are skipped.
	Count int
	// NonFinite is the number of skipped NaN or ±Inf values.
	NonFinite         int
	Min, Max          float64
	Sum, Mean         float64
	Variance          float64 // sample variance (Count-1); 0 when Count < 2
	StandardDeviation float64
	Median            float64

	order *intensityOrder
}

// intensityOrder holds the finite values of a statistics set, sorted on
// first use; copies of the statistics share it.
type intensityOrder struct {
	once   sync.Once
	values []float64
}

func (o *intensityOrder) sorted() []float64 {
	o.once.Do(func() { sort.Float64s(o.values) })
	return o.values
}

// Moments returns the statistics without the median.
func (s IntensityStatistics) Moments() IntensityMoments {
	return IntensityMoments{Label: s.Label, Count: s.Count, NonFinite: s.NonFinite, Min: s.Min, Max: s.Max,
		Sum: s.Sum, Mean: s.Mean, Variance: s.Variance, StandardDeviation: s.StandardDeviation}
}

// NewIntensityStatistics computes the statistics of values (copied; the
// slice is not modified).
func NewIntensityStatistics(values []float64) IntensityStatistics {
	var a IntensityAccumulator
	for _, v := range values {
		a.Add(v)
	}
	owned := make([]float64, 0, a.count)
	for _, v := range values {
		if finite(v) {
			owned = append(owned, v)
		}
	}
	return intensityFromOwned(owned, a.Moments())
}

// intensityFromOwned takes ownership of values (all finite, the values the
// moments describe) and selects their median.
func intensityFromOwned(values []float64, m IntensityMoments) IntensityStatistics {
	s := IntensityStatistics{Label: m.Label, Count: m.Count, NonFinite: m.NonFinite, Min: m.Min, Max: m.Max,
		Sum: m.Sum, Mean: m.Mean, Variance: m.Variance, StandardDeviation: m.StandardDeviation,
		order: &intensityOrder{values: values}}
	if len(values) > 0 {
		s.Median = selectedMedian(values)
	}
	return s
}

// selectedMedian returns Percentile(50) of values by selection, reordering
// but not sorting them.
func selectedMedian(values []float64) float64 {
	n := len(values)
	rank := 0.5 * float64(n-1)
	i := int(math.Floor(rank))
	selectKth(values, i)
	if i >= n-1 {
		return values[i]
	}
	lo := values[i]
	hi := values[i+1]
	for _, v := range values[i+2:] {
		hi = math.Min(hi, v)
	}
	return interpolateRank(lo, hi, rank-float64(i))
}

// interpolateRank is the linear interpolation between the closest ranks of
// Percentile.
func interpolateRank(lo, hi, f float64) float64 {
	if f == 0 || lo == hi {
		return lo
	}
	return lo + f*(hi-lo)
}

// selectKth reorders values (all finite) so that values[k] is the k-th
// smallest, with no larger value before it and no smaller one after it:
// quickselect with a median-of-three pivot, falling back to sorting the
// remaining range after 2·log2(n) unproductive rounds so the worst case
// stays O(n log n).
func selectKth(values []float64, k int) {
	lo, hi := 0, len(values)-1
	budget := 2 * bits.Len(uint(len(values)))
	for hi > lo {
		if budget == 0 {
			sort.Float64s(values[lo : hi+1])
			return
		}
		budget--
		mid := lo + (hi-lo)/2
		if values[mid] < values[lo] {
			values[mid], values[lo] = values[lo], values[mid]
		}
		if values[hi] < values[lo] {
			values[hi], values[lo] = values[lo], values[hi]
		}
		if values[hi] < values[mid] {
			values[hi], values[mid] = values[mid], values[hi]
		}
		pivot := values[mid]
		i, j := lo, hi
		for i <= j {
			for values[i] < pivot {
				i++
			}
			for values[j] > pivot {
				j--
			}
			if i <= j {
				values[i], values[j] = values[j], values[i]
				i++
				j--
			}
		}
		// [lo, j] <= pivot <= [i, hi]; the values between equal the pivot.
		switch {
		case k <= j:
			hi = j
		case k >= i:
			lo = i
		default:
			return
		}
	}
}

// Percentile returns the p-th percentile (0 <= p <= 100) by linear
// interpolation between the closest ranks, rank = p/100·(Count-1) (NumPy's
// default "linear" method, Hyndman and Fan type 7). ok is false for an empty
// set or p outside [0, 100]. The first call sorts the values.
func (s IntensityStatistics) Percentile(p float64) (value float64, ok bool) {
	if s.order == nil || len(s.order.values) == 0 || !(p >= 0 && p <= 100) {
		return 0, false
	}
	sorted := s.order.sorted()
	rank := p / 100 * float64(len(sorted)-1)
	i := int(math.Floor(rank))
	if i >= len(sorted)-1 {
		return sorted[len(sorted)-1], true
	}
	return interpolateRank(sorted[i], sorted[i+1], rank-float64(i)), true
}

// Percentiles returns Percentile for each p (NaN where it is undefined).
func (s IntensityStatistics) Percentiles(ps ...float64) []float64 {
	out := make([]float64, len(ps))
	for i, p := range ps {
		v, ok := s.Percentile(p)
		if !ok {
			v = math.NaN()
		}
		out[i] = v
	}
	return out
}

// checkMaskValues validates the mask and its value count.
func (m *VolumeMask) checkMaskValues(values []float64) error {
	if err := m.validate(); err != nil {
		return err
	}
	if len(values) != len(m.Voxels) {
		return fmt.Errorf("%w: %d values for %d voxels", ErrInvalidShapeInput, len(values), len(m.Voxels))
	}
	return nil
}

// IntensityMoments computes the moments of values (x-fastest, one per
// voxel) inside the mask in one pass, without copying them.
func (m *VolumeMask) IntensityMoments(values []float64) (IntensityMoments, error) {
	if err := m.checkMaskValues(values); err != nil {
		return IntensityMoments{}, err
	}
	var a IntensityAccumulator
	for i, on := range m.Voxels {
		if on {
			a.Add(values[i])
		}
	}
	return a.Moments(), nil
}

// IntensityStatistics computes the statistics of values (x-fastest, one per
// voxel) inside the mask.
func (m *VolumeMask) IntensityStatistics(values []float64) (IntensityStatistics, error) {
	if err := m.checkMaskValues(values); err != nil {
		return IntensityStatistics{}, err
	}
	var a IntensityAccumulator
	for i, on := range m.Voxels {
		if on {
			a.Add(values[i])
		}
	}
	inside := make([]float64, 0, a.count)
	for i, on := range m.Voxels {
		if on && finite(values[i]) {
			inside = append(inside, values[i])
		}
	}
	return intensityFromOwned(inside, a.Moments()), nil
}

// forEachValue calls fn with every value valueAt reports (ok true) for the
// segmentation's voxels.
func (s *Segmentation3D) forEachValue(valueAt func(x, y, slice int) (float64, bool), fn func(float64)) {
	for _, z := range s.Slices() {
		s.masks[z].ForEachPixel(func(x, y int) {
			if v, ok := valueAt(x, y, z); ok {
				fn(v)
			}
		})
	}
}

// IntensityMoments computes the moments of the values valueAt reports (ok
// true) for the segmentation's voxels in one pass, holding no values.
func (s *Segmentation3D) IntensityMoments(valueAt func(x, y, slice int) (float64, bool)) IntensityMoments {
	if s == nil || valueAt == nil {
		return IntensityMoments{}
	}
	var a IntensityAccumulator
	s.forEachValue(valueAt, a.Add)
	return a.Moments()
}

// IntensityStatistics computes the statistics of the values valueAt reports
// (ok true) for the segmentation's voxels, e.g. MPRVolume.ValueAt.
func (s *Segmentation3D) IntensityStatistics(valueAt func(x, y, slice int) (float64, bool)) IntensityStatistics {
	if s == nil || valueAt == nil {
		return IntensityStatistics{}
	}
	var a IntensityAccumulator
	var inside []float64
	s.forEachValue(valueAt, func(v float64) {
		a.Add(v)
		if finite(v) {
			inside = append(inside, v)
		}
	})
	return intensityFromOwned(inside, a.Moments())
}

// checkLabelValues validates the label volume and its value count.
func checkLabelValues(v *LabelVolume, values []float64) error {
	if v == nil || int64(len(v.Labels)) != v.Grid.VoxelCount() || v.Grid.Columns <= 0 || v.Grid.Rows <= 0 {
		return fmt.Errorf("%w: label volume size", ErrInvalidShapeInput)
	}
	if len(values) != len(v.Labels) {
		return fmt.Errorf("%w: %d values for %d voxels", ErrInvalidShapeInput, len(values), len(v.Labels))
	}
	return nil
}

// labelAccumulators runs one IntensityAccumulator per non-zero label over
// the volume, checking ctx once per slice.
func labelAccumulators(ctx context.Context, v *LabelVolume, values []float64) (map[uint16]*IntensityAccumulator, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := checkLabelValues(v, values); err != nil {
		return nil, err
	}
	byLabel := map[uint16]*IntensityAccumulator{}
	var lastLabel uint16
	var last *IntensityAccumulator
	slice := v.Grid.Columns * v.Grid.Rows
	for i, label := range v.Labels {
		if i%slice == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if label == 0 {
			continue
		}
		if last == nil || label != lastLabel {
			last = byLabel[label]
			if last == nil {
				last = &IntensityAccumulator{}
				byLabel[label] = last
			}
			lastLabel = label
		}
		last.Add(values[i])
	}
	return byLabel, nil
}

// LabelIntensityMoments returns the moments of values (x-fastest, one per
// voxel) for every non-zero label, sorted by label, in one pass that holds
// no values.
func LabelIntensityMoments(ctx context.Context, v *LabelVolume, values []float64) ([]IntensityMoments, error) {
	byLabel, err := labelAccumulators(ctx, v, values)
	if err != nil {
		return nil, err
	}
	labels := sortedLabels(byLabel)
	out := make([]IntensityMoments, 0, len(labels))
	for _, label := range labels {
		m := byLabel[label].Moments()
		m.Label = label
		out = append(out, m)
	}
	return out, nil
}

// LabelIntensityStatistics returns the statistics of values (x-fastest, one
// per voxel) for every non-zero label, sorted by label.
func LabelIntensityStatistics(ctx context.Context, v *LabelVolume, values []float64) ([]IntensityStatistics, error) {
	// Count first so each label's values fill one exact allocation.
	counts, err := labelAccumulators(ctx, v, values)
	if err != nil {
		return nil, err
	}
	byLabel := make(map[uint16][]float64, len(counts))
	for label, a := range counts {
		byLabel[label] = make([]float64, 0, a.count)
	}
	for i, label := range v.Labels {
		if label != 0 && finite(values[i]) {
			byLabel[label] = append(byLabel[label], values[i])
		}
	}
	labels := sortedLabels(counts)
	out := make([]IntensityStatistics, 0, len(labels))
	for _, label := range labels {
		m := counts[label].Moments()
		m.Label = label
		out = append(out, intensityFromOwned(byLabel[label], m))
	}
	return out, nil
}
