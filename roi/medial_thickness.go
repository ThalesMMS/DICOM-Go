package roi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
)

var ErrMedialThicknessLimit = errors.New("roi: medial thickness resource limit")
var ErrMedialThicknessDomain = errors.New("roi: unsupported medial thickness domain")

const (
	MedialThicknessMaxVoxels     = 1 << 20
	MedialThicknessMaxBytes      = int64(256 << 20)
	MedialThicknessMaxComponents = 1024
	MedialThicknessMethod        = "Lee 26-connected skeleton; 2 x foreground-boundary-centre distance"
)

// MedialThicknessOptions bounds input, thinning, distance, components and
// owned output together. Zero selects the finite cap; overrides only lower it.
// MaxWorkingBytes reserves managed arrays, including the input mask, not RSS.
type MedialThicknessOptions struct {
	MaxVoxels, MaxComponents int
	MaxWorkingBytes          int64
}

func (o MedialThicknessOptions) normalized() (MedialThicknessOptions, error) {
	if o.MaxVoxels == 0 {
		o.MaxVoxels = MedialThicknessMaxVoxels
	}
	if o.MaxComponents == 0 {
		o.MaxComponents = MedialThicknessMaxComponents
	}
	if o.MaxWorkingBytes == 0 {
		o.MaxWorkingBytes = MedialThicknessMaxBytes
	}
	if o.MaxVoxels < 1 || o.MaxVoxels > MedialThicknessMaxVoxels || o.MaxComponents < 1 || o.MaxComponents > MedialThicknessMaxComponents || o.MaxWorkingBytes < 1 || o.MaxWorkingBytes > MedialThicknessMaxBytes {
		return o, fmt.Errorf("%w: invalid limits", ErrMedialThicknessLimit)
	}
	return o, nil
}

// MedialThicknessReservation admits dimensions before a caller allocates a
// cropped mask. The same reservation is enforced by MedialThickness itself.
func MedialThicknessReservation(columns, rows, slices int, o MedialThicknessOptions) (int64, error) {
	o, err := o.normalized()
	if err != nil {
		return 0, err
	}
	_, bytes, err := medialThicknessAdmission(columns, rows, slices, o)
	return bytes, err
}

func medialThicknessAdmission(columns, rows, slices int, o MedialThicknessOptions) (int, int64, error) {
	n, padded, longest := int64(1), int64(1), 0
	for _, d := range []int{columns, rows, slices} {
		if d < 3 {
			return 0, 0, fmt.Errorf("%w: 3D axes require at least three centres", ErrMedialThicknessDomain)
		}
		if d > o.MaxVoxels || n > int64(o.MaxVoxels)/int64(d) {
			return 0, 0, ErrMedialThicknessLimit
		}
		n *= int64(d)
		padded *= int64(d + 2)
		longest = max(longest, d)
	}
	// Conservative capacities for input/clone, Thin foreground and deletion
	// arrays (including geometric growth), padded image, labels/queue, EDT and
	// boundary masks, samples and sorting, 16 EDT workers and fixed summaries.
	reserved := 160*padded + 1024*int64(longest) + 256*int64(o.MaxComponents) + (64 << 10)
	if reserved > o.MaxWorkingBytes {
		return 0, 0, ErrMedialThicknessLimit
	}
	return int(n), reserved, nil
}

type MedialThicknessSample struct {
	Index       int // x-fastest source-grid index; samples are sorted by index.
	Component   int // Original foreground's deterministic 26-connected component.
	ThicknessMM float64
}

// Empty distributions have Count=0 and NaN statistics, never quantitative zero.
type MedialThicknessDistribution struct {
	Count                                               int
	MinimumMM, MaximumMM, MeanMM, MedianMM, P5MM, P95MM float64
	Histogram                                           [12]int // Equal-width bins from MinimumMM to MaximumMM.
}

type MedialThicknessComponent struct {
	ID, ForegroundVoxels int
	Distribution         MedialThicknessDistribution
}

// MedialThicknessResult owns its geometry, skeleton and sparse samples. Only
// skeleton points have a defined value. A zero thickness at a boundary centre
// IS a valid sample (for example, a single foreground voxel).
type MedialThicknessResult struct {
	Skeleton      *VolumeMask
	SpacingMM     [3]float64
	Samples       []MedialThicknessSample
	Components    []MedialThicknessComponent
	Distribution  MedialThicknessDistribution
	ReservedBytes int64
}

func (r *MedialThicknessResult) At(x, y, z int) (float64, bool) {
	if r == nil || r.Skeleton == nil || x < 0 || y < 0 || z < 0 || x >= r.Skeleton.Columns || y >= r.Skeleton.Rows || z >= r.Skeleton.Slices {
		return math.NaN(), false
	}
	i := r.Skeleton.index(x, y, z)
	j := sort.Search(len(r.Samples), func(j int) bool { return r.Samples[j].Index >= i })
	if j == len(r.Samples) || r.Samples[j].Index != i {
		return math.NaN(), false
	}
	return r.Samples[j].ThicknessMM, true
}

// MedialThickness composes Thin with the existing exact Euclidean transform
// to foreground boundary centres (26-neighbour background test). It accepts
// regular orthogonal axes in mm, including anisotropic spacing. Objects must
// have a background halo: touching any image edge is refused, rather than
// inventing missing anatomy. Cavities contribute boundary sites. This is a
// discrete curve-skeleton measurement, not cortical thickness or a maximum
// sphere diameter propagated over the whole mask. Input must remain stable.
// Every error/cancellation returns nil; no input is changed.
func (m *VolumeMask) MedialThickness(ctx context.Context, o MedialThicknessOptions) (*MedialThicknessResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var err error
	o, err = o.normalized()
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, ErrInvalidVolumeMask
	}
	n, reserved, err := medialThicknessAdmission(m.Columns, m.Rows, m.Slices, o)
	if err != nil {
		return nil, err
	}
	if len(m.Voxels) != n {
		return nil, ErrInvalidVolumeMask
	}
	if err := m.validateContext(ctx); err != nil {
		return nil, err
	}
	var spacing [3]float64
	for a, axis := range [][]float64{m.X, m.Y, m.Z} {
		spacing[a] = axis[1] - axis[0]
		if _, err := signedDistanceAxis(ctx, axis); err != nil {
			return nil, err
		}
		for i := 2; i < len(axis); i++ {
			if math.Abs((axis[i]-axis[i-1])-spacing[a]) > 1e-6*spacing[a] {
				return nil, fmt.Errorf("%w: irregular axis %d", ErrMedialThicknessDomain, a)
			}
		}
	}
	labels := make([]int32, n)
	queue := make([]int, 0, n)
	components := make([]MedialThicknessComponent, 0, min(o.MaxComponents, n))
	for i, on := range m.Voxels {
		if i&4095 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !on {
			continue
		}
		x, y, z := i%m.Columns, i/m.Columns%m.Rows, i/(m.Columns*m.Rows)
		if x == 0 || y == 0 || z == 0 || x == m.Columns-1 || y == m.Rows-1 || z == m.Slices-1 {
			return nil, fmt.Errorf("%w: foreground touches image edge", ErrMedialThicknessDomain)
		}
		if labels[i] != 0 {
			continue
		}
		if len(components) == o.MaxComponents {
			return nil, ErrMedialThicknessLimit
		}
		id := int32(len(components) + 1)
		labels[i] = id
		queue = append(queue[:0], i)
		for head := 0; head < len(queue); head++ {
			if head&4095 == 0 && ctx.Err() != nil {
				return nil, ctx.Err()
			}
			p := queue[head]
			x, y, z := p%m.Columns, p/m.Columns%m.Rows, p/(m.Columns*m.Rows)
			for zz := max(0, z-1); zz <= min(m.Slices-1, z+1); zz++ {
				for yy := max(0, y-1); yy <= min(m.Rows-1, y+1); yy++ {
					for xx := max(0, x-1); xx <= min(m.Columns-1, x+1); xx++ {
						q := m.index(xx, yy, zz)
						if m.Voxels[q] && labels[q] == 0 {
							labels[q] = id
							queue = append(queue, q)
						}
					}
				}
			}
		}
		components = append(components, MedialThicknessComponent{ID: int(id), ForegroundVoxels: len(queue)})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	skeleton, err := m.Thin(ctx)
	if err != nil {
		return nil, err
	}
	// Thin preserves its historical borrowed axes; this composite API owns
	// its complete output, including patient-coordinate arrays.
	skeleton.X = append([]float64(nil), m.X...)
	skeleton.Y = append([]float64(nil), m.Y...)
	skeleton.Z = append([]float64(nil), m.Z...)
	distance, err := m.SignedDistanceTransform(ctx, SignedDistanceOptions{InsideIsPositive: true, MaxVoxels: o.MaxVoxels, MaxBytes: o.MaxWorkingBytes})
	if err != nil {
		return nil, err
	}
	result := &MedialThicknessResult{Skeleton: skeleton, SpacingMM: spacing, Components: components, ReservedBytes: reserved}
	values := make([]float64, 0, int(n))
	perComponent := make([][]float64, len(components))
	for i, on := range skeleton.Voxels {
		if i&4095 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !on {
			continue
		}
		v := 2 * distance[i]
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || labels[i] == 0 {
			return nil, fmt.Errorf("%w: invalid medial sample", ErrMedialThicknessDomain)
		}
		id := int(labels[i])
		result.Samples = append(result.Samples, MedialThicknessSample{Index: i, Component: id, ThicknessMM: v})
		values = append(values, v)
		perComponent[id-1] = append(perComponent[id-1], v)
	}
	result.Distribution = medialDistribution(values)
	for i := range result.Components {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		result.Components[i].Distribution = medialDistribution(perComponent[i])
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func medialDistribution(values []float64) MedialThicknessDistribution {
	nan := math.NaN()
	d := MedialThicknessDistribution{Count: len(values), MinimumMM: nan, MaximumMM: nan, MeanMM: nan, MedianMM: nan, P5MM: nan, P95MM: nan}
	if len(values) == 0 {
		return d
	}
	sort.Float64s(values)
	d.MinimumMM, d.MaximumMM = values[0], values[len(values)-1]
	d.MeanMM = 0
	for _, v := range values {
		d.MeanMM += v / float64(len(values))
	}
	quantile := func(q float64) float64 {
		p := q * float64(len(values)-1)
		lo := int(p)
		hi := min(lo+1, len(values)-1)
		return values[lo] + (p-float64(lo))*(values[hi]-values[lo])
	}
	d.MedianMM, d.P5MM, d.P95MM = quantile(.5), quantile(.05), quantile(.95)
	for _, v := range values {
		bin := 0
		if d.MaximumMM > d.MinimumMM {
			bin = min(11, int(12*(v-d.MinimumMM)/(d.MaximumMM-d.MinimumMM)))
		}
		d.Histogram[bin]++
	}
	return d
}
