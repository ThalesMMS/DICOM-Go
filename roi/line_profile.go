package roi

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// Intensity profiles along a line with uniform physical spacing (#1117), the
// line-profile tool of Horos/RadiAnt: the values of a 2D image, of an MPR
// plane or of a 3D volume sampled every stepMM millimetres from a to b.
//
// End points. Both end points are always samples: the first at distance 0
// and exactly a, the last at the line length and exactly b. Interior samples
// sit at i·stepMM; when the length is not a multiple of stepMM the last
// interval is shorter than stepMM rather than overshooting b or dropping it.
// A length within 1e-6 step of a multiple of stepMM counts as that multiple,
// so floating-point noise never produces a sliver of a final interval. A
// zero-length line is a single sample at a. ITK fixed the same class of
// end-point bugs in its line rasterizer (ITK 59e3c0690d, b7248b1ca4 and
// 92b3aaf86d); BresenhamLine carries those cases for voxel walks.
//
// Values are what the sampler returns: rescaled modality values (HU for CT,
// the stored PET unit, ...). Samples outside the image are kept with Valid
// false so distances stay uniform and the caller decides how to draw gaps.

// MaxProfileSamples bounds the samples of one profile; LineProfile fails
// instead of allocating more (a 1 µm step along a 2 m line).
const MaxProfileSamples = 1 << 20

// profileEndpointTolerance is the fraction of a step under which the length
// counts as a whole multiple of the step.
const profileEndpointTolerance = 1e-6

// imagePlaneToleranceMM is how far off the image plane (Z = 0) a point may lie
// and still sample an ImageProfileSampler.
const imagePlaneToleranceMM = 1e-6

// ProfileSampler maps a point of a physical frame in millimetres to the
// modality value there. ok is false outside the image.
type ProfileSampler interface {
	SampleMM(p render.Vec3, mode render.Interpolation) (value float64, ok bool)
}

// ProfileSample is one sample of a line profile.
type ProfileSample struct {
	// DistanceMM is the distance from the start point along the line.
	DistanceMM float64
	// Point is the sample position in the sampler's frame (mm).
	Point render.Vec3
	// Value is the modality value; meaningful only when Valid.
	Value float64
	// Valid is false for samples outside the image or without pixel data.
	Valid bool
}

// LineProfile samples the line from a to b every stepMM millimetres, both
// end points included (see the package notes above). mode selects nearest,
// linear (also InterpolationUnknown) or cubic interpolation.
func LineProfile(sampler ProfileSampler, a, b render.Vec3, stepMM float64, mode render.Interpolation) ([]ProfileSample, error) {
	return LineProfileContext(context.Background(), sampler, a, b, stepMM, mode)
}

// LineProfileContext is LineProfile with cancellation, for long 3D profiles
// computed off the UI thread.
func LineProfileContext(ctx context.Context, sampler ProfileSampler, a, b render.Vec3, stepMM float64, mode render.Interpolation) ([]ProfileSample, error) {
	if sampler == nil {
		return nil, errors.New("roi: line profile needs a sampler")
	}
	if v, ok := sampler.(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			return nil, err
		}
	}
	switch mode {
	case render.InterpolationUnknown, render.InterpolationNearest, render.InterpolationLinear, render.InterpolationCubic:
	default:
		return nil, fmt.Errorf("roi: unsupported line profile interpolation %d", mode)
	}
	if !finiteVec3(a) || !finiteVec3(b) {
		return nil, errors.New("roi: line profile end points must be finite")
	}
	intervals, length, err := profileIntervals(a, b, stepMM)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	delta := b.Sub(a)
	samples := make([]ProfileSample, intervals+1)
	for i := range samples {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		var distance float64
		var point render.Vec3
		switch {
		case i == 0:
			point = a
		case i == intervals:
			distance, point = length, b
		default:
			distance = float64(i) * stepMM
			point = a.Add(delta.Scale(distance / length))
		}
		value, ok := sampler.SampleMM(point, mode)
		if ok && (math.IsNaN(value) || math.IsInf(value, 0)) {
			ok = false
		}
		if !ok {
			value = 0
		}
		samples[i] = ProfileSample{DistanceMM: distance, Point: point, Value: value, Valid: ok}
	}
	return samples, nil
}

// profileIntervals returns the number of intervals of a profile (samples
// minus one) and the line length.
func profileIntervals(a, b render.Vec3, stepMM float64) (int, float64, error) {
	if !(stepMM > 0) || math.IsInf(stepMM, 0) {
		return 0, 0, fmt.Errorf("roi: line profile step %g mm must be finite and positive", stepMM)
	}
	length := b.Sub(a).Length()
	if math.IsInf(length, 0) || math.IsNaN(length) {
		return 0, 0, errors.New("roi: line profile length is not finite")
	}
	if length == 0 {
		return 0, 0, nil
	}
	steps := length / stepMM
	if steps >= MaxProfileSamples {
		return 0, 0, fmt.Errorf("roi: line profile of %g mm at %g mm needs more than %d samples", length, stepMM, MaxProfileSamples)
	}
	intervals := int(math.Ceil(steps - profileEndpointTolerance))
	if intervals < 1 {
		intervals = 1
	}
	return intervals, length, nil
}

func finiteVec3(p render.Vec3) bool {
	for _, v := range [3]float64{p.X, p.Y, p.Z} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// VolumeProfileSampler samples a volume at DICOM patient LPS points (mm),
// the frame of 3D and MPR line profiles: an MPR plane maps its end points to
// patient space and samples the volume itself, not the rendered plane.
type VolumeProfileSampler struct {
	Reader *render.VolumeReader
}

// Validate reports a sampler without a reader.
func (s VolumeProfileSampler) Validate() error {
	if s.Reader == nil {
		return errors.New("roi: volume line profile needs a volume reader")
	}
	return nil
}

// SampleMM samples the patient point with the volume's half-voxel border
// policy.
func (s VolumeProfileSampler) SampleMM(p render.Vec3, mode render.Interpolation) (float64, bool) {
	return s.Reader.SamplePatientAt(p, mode)
}

// VolumeLineProfile is LineProfileContext over a volume between two patient
// LPS points, holding one read lease for the whole profile.
func VolumeLineProfile(ctx context.Context, volume *render.Volume, a, b render.Vec3, stepMM float64, mode render.Interpolation) ([]ProfileSample, error) {
	if volume == nil {
		return nil, errors.New("roi: volume line profile needs a volume")
	}
	reader, err := volume.AcquireReader()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return LineProfileContext(ctx, VolumeProfileSampler{Reader: reader}, a, b, stepMM, mode)
}

// ImageProfileSampler samples one 2D image in its own plane: X runs along
// columns and Y along rows, in millimetres from the centre of pixel (0,0);
// Z must be 0. Pixel centres sit at (col·ColumnSpacingMM, row·RowSpacingMM),
// which PointMM computes. The border policy is the volume sampler's: points
// within half a pixel of the grid clamp to it, farther ones are outside.
type ImageProfileSampler struct {
	Columns, Rows int
	// ColumnSpacingMM is the distance between adjacent columns (the second
	// value of DICOM Pixel Spacing); RowSpacingMM between adjacent rows (the
	// first value).
	ColumnSpacingMM, RowSpacingMM float64
	// ValueAt returns the modality value of a pixel.
	ValueAt func(x, y int) (float64, bool)
}

// Validate reports an unusable image description.
func (s ImageProfileSampler) Validate() error {
	switch {
	case s.Columns <= 0 || s.Rows <= 0:
		return fmt.Errorf("roi: image line profile needs positive dimensions, got %dx%d", s.Columns, s.Rows)
	case !(s.ColumnSpacingMM > 0) || math.IsInf(s.ColumnSpacingMM, 0) || !(s.RowSpacingMM > 0) || math.IsInf(s.RowSpacingMM, 0):
		return fmt.Errorf("roi: image line profile needs finite positive spacing, got %g x %g mm", s.ColumnSpacingMM, s.RowSpacingMM)
	case s.ValueAt == nil:
		return errors.New("roi: image line profile needs a pixel accessor")
	}
	return nil
}

// PointMM returns the physical position of fractional pixel coordinates.
func (s ImageProfileSampler) PointMM(x, y float64) render.Vec3 {
	return render.Vec3{X: x * s.ColumnSpacingMM, Y: y * s.RowSpacingMM}
}

// SampleMM samples the image at a point of its plane.
func (s ImageProfileSampler) SampleMM(p render.Vec3, mode render.Interpolation) (float64, bool) {
	if s.Validate() != nil || math.Abs(p.Z) > imagePlaneToleranceMM {
		return 0, false
	}
	x, okX := clampToPixelGrid(p.X/s.ColumnSpacingMM, s.Columns)
	y, okY := clampToPixelGrid(p.Y/s.RowSpacingMM, s.Rows)
	if !okX || !okY {
		return 0, false
	}
	switch mode {
	case render.InterpolationNearest:
		nearest := func(v float64, n int) int { return min(render.NearestIndex(v), n-1) }
		return s.ValueAt(nearest(x, s.Columns), nearest(y, s.Rows))
	case render.InterpolationCubic:
		xi, xw, xn := catmullRomTaps(x, s.Columns)
		yi, yw, yn := catmullRomTaps(y, s.Rows)
		return s.weightedSum(xi[:xn], xw[:xn], yi[:yn], yw[:yn])
	}
	xi, xw, xn := linearTaps(x, s.Columns)
	yi, yw, yn := linearTaps(y, s.Rows)
	return s.weightedSum(xi[:xn], xw[:xn], yi[:yn], yw[:yn])
}

// weightedSum is the separable interpolation over the given taps; a missing
// tap makes the sample invalid.
func (s ImageProfileSampler) weightedSum(xi []int, xw []float64, yi []int, yw []float64) (float64, bool) {
	sum := 0.0
	for j := range yi {
		row := 0.0
		for i := range xi {
			value, ok := s.ValueAt(xi[i], yi[j])
			if !ok {
				return 0, false
			}
			row += xw[i] * value
		}
		sum += yw[j] * row
	}
	return sum, true
}

// clampToPixelGrid applies the half-pixel border policy of the render
// volume sampler (render.voxelBorderTolerance included).
func clampToPixelGrid(v float64, n int) (float64, bool) {
	return render.ClampContinuousIndex(v, n, 1e-3)
}

// linearTaps returns one tap on a pixel centre, otherwise the two
// neighbouring pixels with linear weights.
func linearTaps(v float64, n int) (index [2]int, weight [2]float64, taps int) {
	base := int(math.Floor(v))
	f := v - float64(base)
	if f == 0 || base >= n-1 {
		index[0], weight[0] = min(base, n-1), 1
		return index, weight, 1
	}
	index[0], index[1] = base, base+1
	weight[0], weight[1] = 1-f, f
	return index, weight, 2
}

// catmullRomTaps mirrors the render package's cubic kernel (Keys, a = -0.5):
// one tap on a pixel centre, otherwise four taps clamped to the n pixels.
func catmullRomTaps(v float64, n int) (index [4]int, weight [4]float64, taps int) {
	base := int(math.Floor(v))
	f := v - float64(base)
	if f == 0 {
		index[0], weight[0] = min(base, n-1), 1
		return index, weight, 1
	}
	f2, f3 := f*f, f*f*f
	weight = [4]float64{
		(-f3 + 2*f2 - f) / 2,
		(3*f3 - 5*f2 + 2) / 2,
		(-3*f3 + 4*f2 + f) / 2,
		(f3 - f2) / 2,
	}
	for i := range index {
		index[i] = max(0, min(base-1+i, n-1))
	}
	return index, weight, 4
}

// BresenhamLine returns the grid indices of the N-dimensional integer
// Bresenham line from p0 to p1: exactly max_i |p1[i]-p0[i]| + 1 points,
// starting at p0 and ending at p1 by construction, each step moving one index
// along the dominant axis and at most one along every other axis (26-
// connected in 3D). It is the voxel walk of a profile without interpolation.
//
// The integer error-accumulator scheme follows ITK's
// BresenhamLine::BuildLine(Index, Index) after ITK commit 59e3c0690d
// (Apache-2.0, Copyright NumFOCUS), which replaced a floating-point direction
// that overshot the end index; modifications: Go slices of any dimension and
// explicit errors instead of templates.
func BresenhamLine(p0, p1 []int) ([][]int, error) {
	if len(p0) == 0 || len(p0) != len(p1) {
		return nil, fmt.Errorf("roi: Bresenham line end points need the same non-zero dimension, got %d and %d", len(p0), len(p1))
	}
	dims := len(p0)
	absDelta := make([]int, dims)
	step := make([]int, dims)
	mainAxis, maxAbsDelta := 0, 0
	for i := range p0 {
		delta := p1[i] - p0[i]
		step[i] = 1
		if delta < 0 {
			step[i], delta = -1, -delta
		}
		absDelta[i] = delta
		if delta > maxAbsDelta {
			mainAxis, maxAbsDelta = i, delta
		}
	}
	if maxAbsDelta >= MaxProfileSamples {
		return nil, fmt.Errorf("roi: Bresenham line of %d steps exceeds %d points", maxAbsDelta, MaxProfileSamples)
	}
	points := make([][]int, maxAbsDelta+1)
	backing := make([]int, (maxAbsDelta+1)*dims)
	current := append([]int(nil), p0...)
	accumulated := make([]int, dims)
	for s := range points {
		point := backing[s*dims : (s+1)*dims : (s+1)*dims]
		copy(point, current)
		points[s] = point
		for i := range current {
			if i == mainAxis {
				current[i] += step[i]
				continue
			}
			accumulated[i] += 2 * absDelta[i]
			if accumulated[i] >= maxAbsDelta {
				current[i] += step[i]
				accumulated[i] -= 2 * maxAbsDelta
			}
		}
	}
	return points, nil
}
