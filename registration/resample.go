package registration

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
	"github.com/ThalesMMS/dicom-go/render"
)

// Resampling a moving image onto the grid of a fixed (reference) image
// through a registration (#1179), after ITK's ResampleImageFilter: every
// sample of the output grid is a point of the fixed image's patient space,
// mapped by the registration into the moving image and interpolated there.
// The transform direction is the package's: fixed point → moving point, so
// a registration found by Method or Demons, or a Spatial Registration's
// Registered-to-Source mapping, is used as it is.

// ErrResample reports a grid, mapping or option Resample cannot use.
var ErrResample = errors.New("registration: invalid resampling")

// Grid is the geometry of a sampled volume: Size samples along x, y and z
// (x fastest), the patient (LPS) position of sample (0, 0, 0), the spacing
// in millimetres and the unit directions of the index axes as the columns
// of Direction, as for an Image.
type Grid struct {
	Size      [3]int
	Origin    render.Vec3
	Spacing   render.Vec3
	Direction Matrix3
}

// Grid is the image's geometry.
func (im *Image) Grid() Grid {
	return Grid{Size: im.size, Origin: im.origin, Spacing: im.spacing, Direction: im.direction}
}

// Point is the patient position of a continuous index of the grid.
func (g Grid) Point(index render.Vec3) render.Vec3 {
	scale := Matrix3{{g.Spacing.X, 0, 0}, {0, g.Spacing.Y, 0}, {0, 0, g.Spacing.Z}}
	return g.Origin.Add(g.Direction.Mul(scale).MulVec(index))
}

// SameAs reports whether two grids sample the same points: equal sizes,
// and origins, spacings and directions within tolerance (millimetres for
// the origin and spacing, unitless for the direction cosines).
func (g Grid) SameAs(other Grid, tolerance float64) bool {
	if g.Size != other.Size || g.Origin.Sub(other.Origin).Length() > tolerance || g.Spacing.Sub(other.Spacing).Length() > tolerance {
		return false
	}
	for row := 0; row < 3; row++ {
		for column := 0; column < 3; column++ {
			if math.Abs(g.Direction[row][column]-other.Direction[row][column]) > tolerance {
				return false
			}
		}
	}
	return true
}

// Interpolator selects how a moving sample is read at a mapped point.
type Interpolator int

const (
	// InterpolateLinear is trilinear interpolation of the eight
	// neighbouring samples, for images in modality values.
	InterpolateLinear Interpolator = iota
	// InterpolateNearest reads the nearest sample (halves round up, as
	// ITK's NearestNeighborInterpolateImageFunction), for label images
	// whose values must not be blended.
	InterpolateNearest
	// InterpolateLabelGaussian reads the label whose Gaussian-weighted
	// share of the neighbourhood is largest, after ITK's
	// LabelImageGaussianInterpolateImageFunction: every sample within
	// LabelGaussianAlpha σ of the point weighs the integral of the Gaussian
	// over its extent (a product of error-function differences per axis);
	// ties keep the smaller label. It smooths the staircase of rotated or
	// scaled labels without inventing values.
	InterpolateLabelGaussian
	// InterpolateCubicBSpline reads the cubic B-spline interpolant of the
	// moving image (#1190, render.CubicBSpline): exact at the samples and C²
	// between them, smoother than trilinear for exported modality images.
	// Its coefficients are prefiltered over the box of moving samples the
	// grid maps into, widened by bsplineRegionMargin, so it costs a first
	// mapping pass and float32 coefficients of that box: an export kernel,
	// not one for interactive views. A non-finite moving sample in the box
	// fails the resampling.
	InterpolateCubicBSpline
	// InterpolateLanczosWindowedSinc uses a radius-five Lanczos-windowed
	// sinc kernel for offline modality-image resampling. It extends border
	// samples, does not renormalize its weights, and can overshoot. Non-finite
	// samples in a visited stencil or a non-finite float32 result fail the job.
	InterpolateLanczosWindowedSinc
	// InterpolateWelchWindowedSinc uses the same offline contract with a
	// radius-five Welch window.
	InterpolateWelchWindowedSinc
)

// bsplineRegionMargin widens the prefiltered box beyond the four taps of
// the mapped points, so the mirror boundary of the box barely reaches them
// (the prefilter decays by |√3−2|^k ≈ 0.27^k).
const bsplineRegionMargin = 8

// LabelGaussianAlpha is the cut-off of InterpolateLabelGaussian in
// standard deviations, ITK's default alpha.
const LabelGaussianAlpha = 4.0

// Mapping maps a point of the fixed image's patient space to the moving
// image's patient space. ok is false where the mapping is not defined (for
// example outside a displacement field or where an inverse does not
// converge): that output sample is then outside the field like a point
// that maps outside the moving image. A Mapping is called concurrently.
type Mapping func(fixed render.Vec3) (moving render.Vec3, ok bool)

// TransformMapping maps points through a fixed-to-moving transform, such as
// the result of Method.Run. The transform is cloned, so later changes to t
// do not affect the mapping.
func TransformMapping(t Transform) (Mapping, error) {
	if t == nil {
		return nil, fmt.Errorf("%w: nil transform", ErrResample)
	}
	clone := t.Clone()
	return func(p render.Vec3) (render.Vec3, bool) {
		q := clone.TransformPoint(p)
		return q, finiteVec(q)
	}, nil
}

// FieldMapping maps a point p to p + f(p) through a dense displacement
// field on the fixed grid (as Demons returns), interpolated linearly. The
// mapping is undefined more than half a sample outside the field's grid.
func FieldMapping(f *DisplacementField) (Mapping, error) {
	if f == nil {
		return nil, fmt.Errorf("%w: nil displacement field", ErrResample)
	}
	grid, err := f.grid()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrResample, err)
	}
	if len(f.Vectors) != f.Size[0]*f.Size[1]*f.Size[2] {
		return nil, fmt.Errorf("%w: %d vectors for size %v", ErrResample, len(f.Vectors), f.Size)
	}
	vectors, size := f.Vectors, f.Size
	return func(p render.Vec3) (render.Vec3, bool) {
		index := grid.ContinuousIndex(p)
		if !grid.inside(index) {
			return render.Vec3{}, false
		}
		q := p.Add(sampleVectors(vectors, size, index))
		return q, finiteVec(q)
	}, nil
}

// ComposeMappings applies first, then second: a point maps to
// second(first(p)), undefined where either is.
func ComposeMappings(first, second Mapping) Mapping {
	return func(p render.Vec3) (render.Vec3, bool) {
		q, ok := first(p)
		if !ok {
			return render.Vec3{}, false
		}
		return second(q)
	}
}

// ResampleOptions configure Resample.
type ResampleOptions struct {
	Interpolator Interpolator
	// MaxOutputBytes caps the output payload (float32 samples, Inside and
	// SourceSlices), before prefiltering or allocation. Negative is invalid.
	// Zero leaves the four original interpolators uncapped for compatibility;
	// for Lanczos/Welch windowed-sinc it selects a 512 MiB default. A positive
	// value overrides either behavior. This is not a cap on total process RSS,
	// the existing moving image, worker scratch, or B-spline coefficients.
	MaxOutputBytes int64
	// Padding is written where the moving image is not defined: the point
	// maps more than half a sample outside the moving grid (ITK's buffer;
	// within it the outer samples extend), or the mapping is undefined.
	// There is no implicit default: zero pads with zero.
	Padding float32
	// LabelSigma is the Gaussian's standard deviation in millimetres per
	// axis of the moving image for InterpolateLabelGaussian; zero uses the
	// moving image's spacing.
	LabelSigma render.Vec3
	// Progress, when set, receives the number of output slices done and
	// the total; calls come from the resampling goroutines one at a time.
	Progress func(done, total int)
}

// SliceRange is a range of moving slice indices (z), First ≤ Last, or
// empty when First > Last.
type SliceRange struct{ First, Last int }

// Empty reports whether the range holds no slice.
func (r SliceRange) Empty() bool { return r.First > r.Last }

// Resampled is a moving image on the fixed grid.
type Resampled struct {
	// Image has the fixed grid and the moving image's values (Padding
	// outside the field).
	Image *Image
	// Inside marks the output samples read from the moving image; Outside
	// counts the others.
	Inside  []bool
	Outside int
	// SourceSlices[z] is the range of moving slices (z indices) whose
	// samples contributed to output slice z, for references to the source
	// images.
	SourceSlices []SliceRange
}

// ResampleAffine samples moving on grid using the fixed-to-moving LPS
// affine. It validates the homogeneous transform once and uses the same
// buffer, interpolation, provenance and cancellation contracts as Resample.
func ResampleAffine(ctx context.Context, moving *Image, grid Grid, fixedToMoving render.GeometryAffine, opts ResampleOptions) (*Resampled, error) {
	if !fixedToMoving.Finite() {
		return nil, fmt.Errorf("%w: invalid fixed-to-moving affine", ErrResample)
	}
	identity := render.GeometryAffine{0: 1, 5: 1, 10: 1, 15: 1}
	if fixedToMoving == identity {
		return resample(ctx, moving, grid, func(p render.Vec3) (render.Vec3, bool) { return p, true }, opts, true)
	}
	return Resample(ctx, moving, grid, func(p render.Vec3) (render.Vec3, bool) {
		return fixedToMoving.TransformPoint(p), true
	}, opts)
}

// Resample samples moving at every point of grid mapped through mapping
// (fixed → moving), slice by slice on the available CPUs. ctx cancels it
// between slices, and within slices for the offline windowed-sinc kernels.
// A nil ctx is treated as context.Background. Cancellation or a kernel error
// returns nil rather than a partially resampled image.
func Resample(ctx context.Context, moving *Image, grid Grid, mapping Mapping, opts ResampleOptions) (*Resampled, error) {
	return resample(ctx, moving, grid, mapping, opts, false)
}

func resample(ctx context.Context, moving *Image, grid Grid, mapping Mapping, opts ResampleOptions, identityAffine bool) (*Resampled, error) {
	if moving == nil || mapping == nil {
		return nil, fmt.Errorf("%w: nil moving image or mapping", ErrResample)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target, err := newImageGeometry(grid.Size, grid.Origin, grid.Spacing, grid.Direction)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrResample, err)
	}
	if !finite(float64(opts.Padding)) {
		return nil, fmt.Errorf("%w: non-finite padding", ErrResample)
	}
	var gaussian *labelGaussian
	switch opts.Interpolator {
	case InterpolateLinear, InterpolateNearest, InterpolateCubicBSpline, InterpolateLanczosWindowedSinc, InterpolateWelchWindowedSinc:
	case InterpolateLabelGaussian:
		if gaussian, err = newLabelGaussian(moving, opts.LabelSigma); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: unknown interpolator %d", ErrResample, opts.Interpolator)
	}
	count, err := resampleOutputCount(grid.Size, opts)
	if err != nil {
		return nil, err
	}
	copyGrid := false
	if identityAffine && (opts.Interpolator == InterpolateLinear || opts.Interpolator == InterpolateNearest) && grid == moving.Grid() {
		copyGrid = moving.exactIdentityGrid(ctx)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	var spline *render.CubicBSpline
	if opts.Interpolator == InterpolateCubicBSpline {
		if spline, err = mappedBSpline(ctx, moving, &target, mapping); err != nil {
			return nil, err
		}
	}
	var sinc *windowedSinc
	if isWindowedSinc(opts.Interpolator) {
		sinc = &windowedSinc{image: moving, window: opts.Interpolator}
	}
	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	var failureOnce sync.Once
	var failure error
	size := grid.Size
	plane := size[0] * size[1]
	out := &Resampled{Inside: make([]bool, count), SourceSlices: make([]SliceRange, size[2])}
	data := make([]float32, count)
	var outside atomic.Int64
	var progressMu sync.Mutex
	done := 0
	finishSlice := func(z int, sources SliceRange) {
		out.SourceSlices[z] = sources
		if opts.Progress != nil {
			progressMu.Lock()
			done++
			opts.Progress(done, size[2])
			progressMu.Unlock()
		}
	}
	// Split the target index-to-LPS matrix into column vectors. The y and z
	// terms are invariant within a scanline. Keep Point's addition order so
	// half-voxel and slice boundaries retain their existing rounding.
	matrix := target.indexToPhysical
	xStep := render.Vec3{X: matrix[0][0], Y: matrix[1][0], Z: matrix[2][0]}
	yStep := render.Vec3{X: matrix[0][1], Y: matrix[1][1], Z: matrix[2][1]}
	zStep := render.Vec3{X: matrix[0][2], Y: matrix[1][2], Z: matrix[2][2]}
	_ = parallel.Do(workCtx, size[2], 0, func(z int) {
		if workCtx.Err() != nil {
			return
		}
		if copyGrid {
			start, end := z*plane, (z+1)*plane
			copyIdentitySlice(data[start:end], out.Inside[start:end], moving.data[start:end], opts.Interpolator)
			finishSlice(z, SliceRange{First: z, Last: z})
			return
		}
		sources := SliceRange{First: math.MaxInt, Last: math.MinInt}
		var missed int64
		zTerm := zStep.Scale(float64(z))
		for y := 0; y < size[1]; y++ {
			yTerm := yStep.Scale(float64(y))
			row := z*plane + y*size[0]
			for x := 0; x < size[0]; x++ {
				if sinc != nil && workCtx.Err() != nil {
					return
				}
				i := row + x
				fixed := xStep.Scale(float64(x)).Add(yTerm).Add(zTerm).Add(target.origin)
				p, ok := mapping(fixed)
				if !ok {
					data[i] = opts.Padding
					missed++
					continue
				}
				index := moving.ContinuousIndex(p)
				if !moving.inside(index) {
					data[i] = opts.Padding
					missed++
					continue
				}
				var value float64
				var first, last int
				switch opts.Interpolator {
				case InterpolateNearest:
					at := moving.nearest(index)
					value, first, last = float64(moving.At(at[0], at[1], at[2])), at[2], at[2]
				case InterpolateLabelGaussian:
					value, first, last = gaussian.at(index)
				case InterpolateCubicBSpline:
					index = moving.clampIndex(index)
					value = spline.At(index)
					first, last = bsplineSlices(moving, index.Z)
				case InterpolateLanczosWindowedSinc, InterpolateWelchWindowedSinc:
					var sampleErr error
					value, first, last, sampleErr = sinc.at(workCtx, index)
					if sampleErr != nil {
						failureOnce.Do(func() { failure = sampleErr; cancelWork() })
						return
					}
				default:
					value, first, last = moving.linearAtWithSlices(index)
				}
				data[i] = float32(value)
				out.Inside[i] = true
				sources.First, sources.Last = min(sources.First, first), max(sources.Last, last)
			}
		}
		if sinc != nil && workCtx.Err() != nil {
			return
		}
		outside.Add(missed)
		finishSlice(z, sources)
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if failure != nil {
		return nil, failure
	}
	out.Outside = int(outside.Load())
	target.data = data
	out.Image = &target
	return out, nil
}

// exactIdentityGrid reports whether an identical fixed grid maps to integral
// moving indices under the staged physical round trip. With one nonzero in
// each matrix row the axes are independent. Power-of-two steps make the
// coordinate products exact, including on targets that fuse a multiply and
// add. The round trip still checks for precision lost at the physical origin.
func (im *Image) exactIdentityGrid(ctx context.Context) bool {
	for _, row := range im.indexToPhysical {
		var nonzero int
		for _, step := range row {
			if step == 0 {
				continue
			}
			mantissa, _ := math.Frexp(math.Abs(step))
			if mantissa != 0.5 {
				return false
			}
			nonzero++
		}
		if nonzero != 1 {
			return false
		}
	}
	for axis, n := range im.size {
		// Keep every integer index and its half-index rounding exactly
		// representable. The grid is already validated as nonsingular.
		if uint64(n) > 1<<52 {
			return false
		}
		for i := range n {
			if i%1024 == 0 && ctx.Err() != nil {
				return false
			}
			coordinates := [3]float64{}
			coordinates[axis] = float64(i)
			index := render.Vec3{X: coordinates[0], Y: coordinates[1], Z: coordinates[2]}
			if im.ContinuousIndex(im.Point(index)) != index {
				return false
			}
		}
	}
	return true
}

func copyIdentitySlice(dst []float32, inside []bool, src []float32, interpolator Interpolator) {
	if interpolator == InterpolateLinear {
		for i, value := range src {
			// Match the single-corner path, including the sum with +0.
			dst[i] = float32(0 + float64(value))
			inside[i] = true
		}
		return
	}
	for i, value := range src {
		dst[i] = float32(float64(value))
		inside[i] = true
	}
}

// resampleOutputCount checks all payload arithmetic before make or prefilter.
// The limit covers only returned arrays; it does not estimate the process heap.
func resampleOutputCount(size [3]int, opts ResampleOptions) (int, error) {
	if opts.MaxOutputBytes < 0 {
		return 0, fmt.Errorf("%w: negative output byte limit", ErrResample)
	}
	maxInt := uint64(^uint(0) >> 1)
	count := uint64(1)
	for _, n := range size {
		if n < 1 || count > maxInt/uint64(n) {
			return 0, fmt.Errorf("%w: output sample count overflows int", ErrResample)
		}
		count *= uint64(n)
	}
	// float32 and bool payloads plus two ints for each source-slice range.
	const bytesPerSample = 5
	const bytesPerRange = 2 * (strconv.IntSize / 8)
	if uint64(size[2]) > maxInt/bytesPerRange {
		return 0, fmt.Errorf("%w: output slice ranges overflow int", ErrResample)
	}
	rangeBytes := uint64(size[2]) * bytesPerRange
	if count > (maxInt-rangeBytes)/bytesPerSample {
		return 0, fmt.Errorf("%w: output payload bytes overflow int", ErrResample)
	}
	bytes := count*bytesPerSample + rangeBytes
	limit := opts.MaxOutputBytes
	if limit == 0 && isWindowedSinc(opts.Interpolator) {
		limit = defaultWindowedSincOutputBytes
	}
	if limit > 0 && bytes > uint64(limit) {
		return 0, fmt.Errorf("%w: output payload %d bytes exceeds limit %d", ErrResample, bytes, limit)
	}
	return int(count), nil
}

// mappedBSpline prefilters the moving samples of the box the grid maps
// into, widened by bsplineRegionMargin and clamped to the moving grid; nil
// when no point maps inside the moving image.
func mappedBSpline(ctx context.Context, moving *Image, target *Image, mapping Mapping) (*render.CubicBSpline, error) {
	size := target.size
	var mu sync.Mutex
	lo := [3]int{math.MaxInt, math.MaxInt, math.MaxInt}
	hi := [3]int{math.MinInt, math.MinInt, math.MinInt}
	parallelSlices(size[2], func(z int) {
		if ctx.Err() != nil {
			return
		}
		sliceLo := [3]int{math.MaxInt, math.MaxInt, math.MaxInt}
		sliceHi := [3]int{math.MinInt, math.MinInt, math.MinInt}
		for y := 0; y < size[1]; y++ {
			for x := 0; x < size[0]; x++ {
				p, ok := mapping(target.Point(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)}))
				if !ok {
					continue
				}
				index := moving.ContinuousIndex(p)
				if !finiteVec(index) || !moving.inside(index) {
					continue
				}
				index = moving.clampIndex(index)
				for axis, value := range [3]float64{index.X, index.Y, index.Z} {
					floor := int(math.Floor(value))
					sliceLo[axis], sliceHi[axis] = min(sliceLo[axis], floor), max(sliceHi[axis], floor)
				}
			}
		}
		mu.Lock()
		for axis := range lo {
			lo[axis], hi[axis] = min(lo[axis], sliceLo[axis]), max(hi[axis], sliceHi[axis])
		}
		mu.Unlock()
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if lo[0] > hi[0] {
		return nil, nil
	}
	var first, extent [3]int
	for axis := range lo {
		first[axis] = max(lo[axis]-1-bsplineRegionMargin, 0)
		last := min(hi[axis]+2+bsplineRegionMargin, moving.size[axis]-1)
		extent[axis] = last - first[axis] + 1
	}
	spline, err := render.NewCubicBSplineContext(ctx, first, extent, func(x, y, z int) float64 {
		return float64(moving.At(x, y, z))
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", ErrResample, err)
	}
	return spline, nil
}

// clampIndex clamps a continuous index to the grid's samples.
func (im *Image) clampIndex(index render.Vec3) render.Vec3 {
	return render.Vec3{
		X: math.Min(math.Max(index.X, 0), float64(im.size[0]-1)),
		Y: math.Min(math.Max(index.Y, 0), float64(im.size[1]-1)),
		Z: math.Min(math.Max(index.Z, 0), float64(im.size[2]-1)),
	}
}

// bsplineSlices are the slices of the coefficient taps that weigh at
// continuous z: floor(z)−1 … floor(z)+2, without an outer tap whose weight
// (1−t)³/6 or t³/6 is negligible (on or within rounding of a slice),
// clamped to the grid.
func bsplineSlices(im *Image, z float64) (int, int) {
	const negligible = 1e-12
	base := int(math.Floor(z))
	t := z - float64(base)
	first, last := base-1, base+2
	if (1-t)*(1-t)*(1-t)/6 <= negligible {
		first = base
	}
	if t*t*t/6 <= negligible {
		last = base + 1
	}
	return max(first, 0), min(last, im.size[2]-1)
}

// nearest is the sample nearest a continuous index inside the grid, halves
// rounding up.
func (im *Image) nearest(index render.Vec3) [3]int {
	var at [3]int
	for axis, value := range [3]float64{index.X, index.Y, index.Z} {
		at[axis] = min(max(int(math.Floor(value+0.5)), 0), im.size[axis]-1)
	}
	return at
}

// labelGaussian evaluates InterpolateLabelGaussian on one image.
type labelGaussian struct {
	image *Image
	// scale is 1/(√2 σ) per axis in index units; radius the neighbourhood
	// half-width in samples.
	scale  [3]float64
	radius [3]int
}

func newLabelGaussian(im *Image, sigma render.Vec3) (*labelGaussian, error) {
	if sigma == (render.Vec3{}) {
		sigma = im.spacing
	}
	g := &labelGaussian{image: im}
	spacing := [3]float64{im.spacing.X, im.spacing.Y, im.spacing.Z}
	for axis, s := range [3]float64{sigma.X, sigma.Y, sigma.Z} {
		if !(s > 0) || math.IsInf(s, 0) {
			return nil, fmt.Errorf("%w: label sigma %v", ErrResample, sigma)
		}
		inIndex := s / spacing[axis]
		g.scale[axis] = 1 / (math.Sqrt2 * inIndex)
		g.radius[axis] = int(math.Ceil(LabelGaussianAlpha * inIndex))
	}
	return g, nil
}

// at returns the winning label at a continuous index inside the grid and
// the range of slices it weighed.
func (g *labelGaussian) at(index render.Vec3) (float64, int, int) {
	im := g.image
	centre := [3]float64{index.X, index.Y, index.Z}
	var lo, hi [3]int
	var weights [3][]float64
	for axis := 0; axis < 3; axis++ {
		c := int(math.Floor(centre[axis] + 0.5))
		lo[axis] = max(c-g.radius[axis], 0)
		hi[axis] = min(c+g.radius[axis], im.size[axis]-1)
		weights[axis] = make([]float64, hi[axis]-lo[axis]+1)
		for i := lo[axis]; i <= hi[axis]; i++ {
			// The integral of the Gaussian over the sample's extent.
			a := (float64(i) - 0.5 - centre[axis]) * g.scale[axis]
			b := (float64(i) + 0.5 - centre[axis]) * g.scale[axis]
			weights[axis][i-lo[axis]] = (math.Erf(b) - math.Erf(a)) / 2
		}
	}
	type share struct {
		label  float32
		weight float64
	}
	var shares []share
	for k := lo[2]; k <= hi[2]; k++ {
		wz := weights[2][k-lo[2]]
		for j := lo[1]; j <= hi[1]; j++ {
			wyz := wz * weights[1][j-lo[1]]
			for i := lo[0]; i <= hi[0]; i++ {
				w := wyz * weights[0][i-lo[0]]
				if w <= 0 {
					continue
				}
				label := im.At(i, j, k)
				found := false
				for s := range shares {
					if shares[s].label == label {
						shares[s].weight += w
						found = true
						break
					}
				}
				if !found {
					shares = append(shares, share{label: label, weight: w})
				}
			}
		}
	}
	if len(shares) == 0 {
		at := im.nearest(index)
		return float64(im.At(at[0], at[1], at[2])), at[2], at[2]
	}
	best := shares[0]
	for _, s := range shares[1:] {
		if s.weight > best.weight || (s.weight == best.weight && s.label < best.label) {
			best = s
		}
	}
	return float64(best.label), lo[2], hi[2]
}

// Subtract returns minuend − subtrahend sample by sample, in the images'
// own units (Hounsfield units for CT volumes read with ImageFromSnapshot),
// on their common grid: the result has that grid, the difference where
// valid is true (nil: everywhere) and padding elsewhere. The grids must
// match within 1e-4 mm (see Grid.SameAs); resample one image onto the
// other's grid first.
func Subtract(minuend, subtrahend *Image, valid []bool, padding float32) (*Image, error) {
	if minuend == nil || subtrahend == nil {
		return nil, fmt.Errorf("%w: nil image", ErrResample)
	}
	if !minuend.Grid().SameAs(subtrahend.Grid(), 1e-4) {
		return nil, fmt.Errorf("%w: the images do not share a grid", ErrResample)
	}
	if valid != nil && len(valid) != len(minuend.data) {
		return nil, fmt.Errorf("%w: %d validity flags for %d samples", ErrResample, len(valid), len(minuend.data))
	}
	if !finite(float64(padding)) {
		return nil, fmt.Errorf("%w: non-finite padding", ErrResample)
	}
	data := make([]float32, len(minuend.data))
	for i := range data {
		if valid != nil && !valid[i] {
			data[i] = padding
			continue
		}
		data[i] = minuend.data[i] - subtrahend.data[i]
	}
	return NewImage(minuend.size, data, minuend.origin, minuend.spacing, minuend.direction)
}
