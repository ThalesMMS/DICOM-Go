package registration

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/spatialreg"
)

// Deformable registration by the demons algorithm (#1091), after ITK's
// DemonsRegistrationFilter (Thirion's demons with the fixed image gradient)
// and DiffeomorphicDemonsRegistrationFilter (Vercauteren's diffeomorphic
// demons: the update is a velocity field whose exponential, by scaling and
// squaring, is composed with the field), run over a resolution pyramid as
// MultiResolutionPDEDeformableRegistration does.

// ErrDemons reports images or options Demons cannot use.
var ErrDemons = errors.New("registration: invalid demons registration")

// DisplacementField is a dense deformation on the fixed image grid: fixed
// point p corresponds to moving point p + Vectors[i], in millimetres, for
// the sample i at p, x fastest.
type DisplacementField struct {
	Size      [3]int
	Origin    render.Vec3
	Spacing   render.Vec3
	Direction Matrix3
	Vectors   []render.Vec3
}

// At is the displacement of sample (x, y, z).
func (f *DisplacementField) At(x, y, z int) render.Vec3 {
	return f.Vectors[(z*f.Size[1]+y)*f.Size[0]+x]
}

// grid is the field's geometry without an image sample buffer.
func (f *DisplacementField) grid() (Image, error) {
	return newImageGeometry(f.Size, f.Origin, f.Spacing, f.Direction)
}

// DisplacementAt interpolates the displacement linearly at a physical point
// (clamped at the grid edges).
func (f *DisplacementField) DisplacementAt(p render.Vec3) render.Vec3 {
	g, err := f.grid()
	if err != nil {
		return render.Vec3{}
	}
	return sampleVectors(f.Vectors, f.Size, g.ContinuousIndex(p))
}

// DemonsOptions configure Demons.
type DemonsOptions struct {
	// Shrinks are the pyramid levels, coarsest first, as shrink factors of
	// the fixed image (nil: a single level at full resolution).
	Shrinks []int
	// Iterations per level (0: 50).
	Iterations int
	// FieldSigma is the standard deviation, in samples, of the Gaussian
	// that regularizes the displacement field before each update (0: 1);
	// UpdateSigma smooths the update itself (0: none).
	FieldSigma, UpdateSigma float64
	// MaximumError and MaximumKernelWidth truncate the discrete Gaussian
	// kernels as ITK's GaussianOperator does (0: 0.1 and 30).
	MaximumError       float64
	MaximumKernelWidth int
	// IntensityDifferenceThreshold ignores smaller intensity differences
	// (0: 0.001).
	IntensityDifferenceThreshold float64
	// Diffeomorphic composes the exponential of each update (a velocity
	// field) with the field instead of adding it, so the field stays
	// invertible; MaximumUpdateStep bounds the update in samples (0: 0.5).
	Diffeomorphic     bool
	MaximumUpdateStep float64
	// Progress, when set, receives every iteration.
	Progress func(DemonsProgress)
}

// DemonsProgress reports one iteration.
type DemonsProgress struct {
	Level, Iteration int
	// MeanSquares is the mean squared intensity difference over the fixed
	// samples that map inside the moving image; RMSChange the RMS length of
	// the update in millimetres.
	MeanSquares, RMSChange float64
}

// DemonsResult is the field and the last iteration of each level.
type DemonsResult struct {
	Field  *DisplacementField
	Levels []DemonsProgress
}

// Demons registers moving onto fixed, both in one patient space (resample
// the moving image first when a rigid registration relates them), and
// returns the displacement field on the fixed grid.
func Demons(ctx context.Context, fixed, moving *Image, opts DemonsOptions) (DemonsResult, error) {
	if fixed == nil || moving == nil {
		return DemonsResult{}, fmt.Errorf("%w: missing image", ErrDemons)
	}
	shrinks := opts.Shrinks
	if len(shrinks) == 0 {
		shrinks = []int{1}
	}
	iterations := opts.Iterations
	if iterations <= 0 {
		iterations = 50
	}
	fieldSigma := opts.FieldSigma
	if fieldSigma == 0 {
		fieldSigma = 1
	}
	threshold := opts.IntensityDifferenceThreshold
	if threshold == 0 {
		threshold = 0.001
	}
	maxStep := opts.MaximumUpdateStep
	if maxStep == 0 {
		maxStep = 0.5
	}
	if fieldSigma < 0 || opts.UpdateSigma < 0 || threshold < 0 || maxStep < 0 {
		return DemonsResult{}, fmt.Errorf("%w: negative option", ErrDemons)
	}
	if opts.MaximumError == 0 {
		opts.MaximumError = 0.1
	}
	if opts.MaximumKernelWidth == 0 {
		opts.MaximumKernelWidth = 30
	}
	if !(opts.MaximumError > 0 && opts.MaximumError < 1) || opts.MaximumKernelWidth < 1 {
		return DemonsResult{}, fmt.Errorf("%w: Gaussian maximum error %v, kernel width %d", ErrDemons, opts.MaximumError, opts.MaximumKernelWidth)
	}
	var field *DisplacementField
	var result DemonsResult
	for level, shrink := range shrinks {
		if shrink < 1 {
			return DemonsResult{}, fmt.Errorf("%w: shrink %d", ErrDemons, shrink)
		}
		var factors [3]int
		for axis := range factors {
			factors[axis] = min(shrink, fixed.Size()[axis])
		}
		levelFixed, err := fixed.Downsample(factors)
		if err != nil {
			return DemonsResult{}, err
		}
		next := newField(levelFixed)
		if field != nil {
			previousGrid, err := field.grid()
			if err != nil {
				return DemonsResult{}, err
			}
			for z := 0; z < next.Size[2]; z++ {
				for y := 0; y < next.Size[1]; y++ {
					for x := 0; x < next.Size[0]; x++ {
						p := levelFixed.Point(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)})
						next.Vectors[(z*next.Size[1]+y)*next.Size[0]+x] = sampleVectors(field.Vectors, field.Size, previousGrid.ContinuousIndex(p))
					}
				}
			}
		}
		field = next
		last, err := demonsLevel(ctx, levelFixed, moving, field, level, iterations, fieldSigma, threshold, maxStep, opts)
		if err != nil {
			return DemonsResult{}, err
		}
		result.Levels = append(result.Levels, last)
	}
	result.Field = field
	return result, nil
}

func newField(im *Image) *DisplacementField {
	size := im.Size()
	return &DisplacementField{
		Size: size, Origin: im.Origin(), Spacing: im.Spacing(), Direction: im.Direction(),
		Vectors: make([]render.Vec3, size[0]*size[1]*size[2]),
	}
}

// demonsLevel iterates the demons on one level.
func demonsLevel(ctx context.Context, fixed, moving *Image, field *DisplacementField, level, iterations int, fieldSigma, threshold, maxStep float64, opts DemonsOptions) (DemonsProgress, error) {
	size := fixed.Size()
	n := size[0] * size[1] * size[2]
	spacing := fixed.Spacing()
	// ITK's normalizer: the mean squared spacing.
	var normalizer float64
	dims := 0
	for axis, s := range [3]float64{spacing.X, spacing.Y, spacing.Z} {
		if size[axis] > 1 {
			normalizer += s * s
			dims++
		}
	}
	normalizer /= float64(max(1, dims))
	fixedGradient := imageGradient(fixed)
	minSpacing := math.Inf(1)
	for axis, s := range [3]float64{spacing.X, spacing.Y, spacing.Z} {
		if size[axis] > 1 {
			minSpacing = math.Min(minSpacing, s)
		}
	}
	fieldKernel := gaussianKernel(fieldSigma, opts.MaximumError, opts.MaximumKernelWidth)
	update := make([]render.Vec3, n)
	warped := make([]float32, n)
	inside := make([]bool, n)
	// composed receives each diffeomorphic update's field, which then trades
	// places with the current one instead of being copied back (#2025).
	var composed []render.Vec3
	if opts.Diffeomorphic {
		composed = make([]render.Vec3, n)
	}
	// Per-slice sums, added in slice order, so the reported progress does
	// not depend on the order the workers finish the slices in.
	sliceSquares, sliceChange, sliceCount := make([]float64, size[2]), make([]float64, size[2]), make([]int, size[2])
	var last DemonsProgress
	for iteration := 1; iteration <= iterations; iteration++ {
		if err := ctx.Err(); err != nil {
			return DemonsProgress{}, err
		}
		// As ITK's InitializeIteration, the field is regularized before
		// each update, so the last update is not smoothed away.
		if fieldSigma > 0 {
			smoothVectors(field.Vectors, size, fieldKernel)
		}
		// The moving image warped by the field.
		parallelSlices(size[2], func(z int) {
			for y := 0; y < size[1]; y++ {
				for x := 0; x < size[0]; x++ {
					i := (z*size[1]+y)*size[0] + x
					p := fixed.Point(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)}).Add(field.Vectors[i])
					value, ok := moving.Interpolate(p)
					warped[i], inside[i] = float32(value), ok
				}
			}
		})
		var movingGradient []render.Vec3
		if opts.Diffeomorphic {
			// ESM (symmetric) gradient: the mean of the fixed gradient and
			// the gradient of the warped moving image.
			warpedImage, err := NewImage(size, warped, fixed.Origin(), spacing, fixed.Direction())
			if err != nil {
				return DemonsProgress{}, err
			}
			movingGradient = imageGradient(warpedImage)
		}
		parallelSlices(size[2], func(z int) {
			var localSquares, localChange float64
			var localCount int
			for y := 0; y < size[1]; y++ {
				for x := 0; x < size[0]; x++ {
					i := (z*size[1]+y)*size[0] + x
					update[i] = render.Vec3{}
					if !inside[i] {
						continue
					}
					speed := float64(fixed.At(x, y, z)) - float64(warped[i])
					localSquares += speed * speed
					localCount++
					gradient := fixedGradient[i]
					if movingGradient != nil {
						gradient = gradient.Add(movingGradient[i]).Scale(0.5)
					}
					denominator := speed*speed/normalizer + gradient.Dot(gradient)
					if math.Abs(speed) < threshold || denominator < 1e-9 {
						continue
					}
					u := gradient.Scale(speed / denominator)
					if opts.Diffeomorphic {
						// ESM steps are twice Thirion's; bound them.
						u = u.Scale(2)
						if length := u.Length(); length > maxStep*minSpacing {
							u = u.Scale(maxStep * minSpacing / length)
						}
					}
					update[i] = u
					localChange += u.Dot(u)
				}
			}
			sliceSquares[z], sliceChange[z], sliceCount[z] = localSquares, localChange, localCount
		})
		var sumSquares, sumChange float64
		var count int
		for z := range sliceSquares {
			sumSquares += sliceSquares[z]
			sumChange += sliceChange[z]
			count += sliceCount[z]
		}
		if opts.UpdateSigma > 0 {
			smoothVectors(update, size, gaussianKernel(opts.UpdateSigma, opts.MaximumError, opts.MaximumKernelWidth))
		}
		if opts.Diffeomorphic {
			exponential := exponentiate(update, fixed)
			composeFieldInto(composed, field, exponential, fixed)
			field.Vectors, composed = composed, field.Vectors
		} else {
			for i := range field.Vectors {
				field.Vectors[i] = field.Vectors[i].Add(update[i])
			}
		}
		last = DemonsProgress{Level: level, Iteration: iteration}
		if count > 0 {
			last.MeanSquares = sumSquares / float64(count)
			last.RMSChange = math.Sqrt(sumChange / float64(count))
		}
		if opts.Progress != nil {
			opts.Progress(last)
		}
	}
	return last, nil
}

// imageGradient is the physical gradient by central differences (one-sided
// at the edges, zero along single-sample axes).
func imageGradient(im *Image) []render.Vec3 {
	size := im.Size()
	spacing := [3]float64{im.Spacing().X, im.Spacing().Y, im.Spacing().Z}
	direction := im.Direction()
	out := make([]render.Vec3, size[0]*size[1]*size[2])
	parallelSlices(size[2], func(z int) {
		for y := 0; y < size[1]; y++ {
			for x := 0; x < size[0]; x++ {
				var g [3]float64
				at := [3]int{x, y, z}
				for axis := 0; axis < 3; axis++ {
					if size[axis] < 2 {
						continue
					}
					lo, hi := at, at
					lo[axis], hi[axis] = max(0, at[axis]-1), min(size[axis]-1, at[axis]+1)
					g[axis] = (float64(im.At(hi[0], hi[1], hi[2])) - float64(im.At(lo[0], lo[1], lo[2]))) / (float64(hi[axis]-lo[axis]) * spacing[axis])
				}
				// Index-axis derivatives to patient axes: the direction
				// columns are orthonormal, so ∇ = D·g.
				out[(z*size[1]+y)*size[0]+x] = direction.MulVec(render.Vec3{X: g[0], Y: g[1], Z: g[2]})
			}
		}
	})
	return out
}

// exponentiate returns exp(v) by scaling and squaring: v is divided by 2^N
// so that its largest step is under half a sample, then composed with
// itself N times.
func exponentiate(v []render.Vec3, grid *Image) []render.Vec3 {
	spacing := grid.Spacing()
	minSpacing := math.Min(spacing.X, math.Min(spacing.Y, spacing.Z))
	var largest float64
	for _, u := range v {
		largest = math.Max(largest, u.Length())
	}
	squarings := 0
	for largest/math.Pow(2, float64(squarings)) > 0.5*minSpacing && squarings < 16 {
		squarings++
	}
	scale := math.Pow(2, -float64(squarings))
	out := make([]render.Vec3, len(v))
	for i, u := range v {
		out[i] = u.Scale(scale)
	}
	size := grid.Size()
	next := make([]render.Vec3, len(v))
	for s := 0; s < squarings; s++ {
		// φ ← φ ∘ φ: u(x) + u(x + u(x)).
		parallelSlices(size[2], func(z int) {
			for y := 0; y < size[1]; y++ {
				for x := 0; x < size[0]; x++ {
					i := (z*size[1]+y)*size[0] + x
					p := grid.Point(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)}).Add(out[i])
					next[i] = out[i].Add(sampleVectors(out, size, grid.ContinuousIndex(p)))
				}
			}
		})
		out, next = next, out
	}
	return out
}

// composeFieldInto writes u ∘ exp(v), x ↦ x + e(x) + u(x + e(x)), of the
// field u into composed, ITK's field update in the diffeomorphic demons.
// composed must not share storage with the field.
func composeFieldInto(composed []render.Vec3, field *DisplacementField, exponential []render.Vec3, grid *Image) {
	size := field.Size
	parallelSlices(size[2], func(z int) {
		for y := 0; y < size[1]; y++ {
			for x := 0; x < size[0]; x++ {
				i := (z*size[1]+y)*size[0] + x
				p := grid.Point(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)}).Add(exponential[i])
				composed[i] = exponential[i].Add(sampleVectors(field.Vectors, size, grid.ContinuousIndex(p)))
			}
		}
	})
}

// sampleVectors interpolates vectors linearly at a continuous index,
// clamped to the grid.
func sampleVectors(vectors []render.Vec3, size [3]int, index render.Vec3) render.Vec3 {
	var base [3]int
	var weight [3]float64
	for axis, value := range [3]float64{index.X, index.Y, index.Z} {
		value = math.Min(math.Max(value, 0), float64(size[axis]-1))
		base[axis] = int(math.Floor(value))
		weight[axis] = value - float64(base[axis])
		if base[axis] >= size[axis]-1 {
			base[axis], weight[axis] = size[axis]-1, 0
		}
	}
	var out render.Vec3
	for corner := 0; corner < 8; corner++ {
		w := 1.0
		at := base
		for axis := 0; axis < 3; axis++ {
			if corner>>axis&1 == 1 {
				if weight[axis] == 0 {
					w = 0
					break
				}
				w *= weight[axis]
				at[axis]++
			} else {
				w *= 1 - weight[axis]
			}
		}
		if w != 0 {
			out = out.Add(vectors[(at[2]*size[1]+at[1])*size[0]+at[0]].Scale(w))
		}
	}
	return out
}

// gaussianKernel is ITK's GaussianOperator for variance sigma² (in
// samples): the discrete Gaussian e^(−t)·I_n(t) of the modified Bessel
// functions, extended until it holds 1 − maxError of the mass or reaches
// maxWidth coefficients per side, then normalized.
func gaussianKernel(sigma, maxError float64, maxWidth int) []float64 {
	t := sigma * sigma
	et := math.Exp(-t)
	half := []float64{et * besselI0(t), et * besselI1(t)}
	sum := half[0] + 2*half[1]
	for i := 2; sum < 1-maxError && i <= maxWidth; i++ {
		next := half[i-2] - 2*float64(i-1)/t*half[i-1]
		if next <= 1e-12 {
			break
		}
		half = append(half, next)
		sum += 2 * next
	}
	kernel := make([]float64, 2*len(half)-1)
	for i, c := range half {
		kernel[len(half)-1+i], kernel[len(half)-1-i] = c/sum, c/sum
	}
	return kernel
}

// besselI0 and besselI1 are the modified Bessel functions of the first kind
// by their power series, accurate for the small arguments used here.
func besselI0(x float64) float64 {
	term, sum := 1.0, 1.0
	for k := 1; k < 60 && term > 1e-17*sum; k++ {
		term *= (x / 2) * (x / 2) / float64(k*k)
		sum += term
	}
	return sum
}

func besselI1(x float64) float64 {
	term := x / 2
	sum := term
	for k := 1; k < 60 && term > 1e-17*sum; k++ {
		term *= (x / 2) * (x / 2) / float64(k*(k+1))
		sum += term
	}
	return sum
}

// smoothVectors convolves each component with a symmetric kernel along every
// axis with more than one sample (edges clamped).
func smoothVectors(vectors []render.Vec3, size [3]int, kernel []float64) {
	radius := len(kernel) / 2
	strides := [3]int{1, size[0], size[0] * size[1]}
	scratch := make([]render.Vec3, len(vectors))
	for axis := 0; axis < 3; axis++ {
		n := size[axis]
		if n < 2 {
			continue
		}
		stride := strides[axis]
		parallelSlices(len(vectors)/n, func(line int) {
			// The line's first sample: all index combinations of the other
			// axes.
			var start int
			switch axis {
			case 0:
				start = line * size[0]
			case 1:
				start = line%size[0] + line/size[0]*size[0]*size[1]
			case 2:
				start = line
			}
			for i := 0; i < n; i++ {
				var sum render.Vec3
				for k, w := range kernel {
					j := min(max(i+k-radius, 0), n-1)
					sum = sum.Add(vectors[start+j*stride].Scale(w))
				}
				scratch[start+i*stride] = sum
			}
		})
		copy(vectors, scratch)
	}
}

// parallelSlices runs work for every index in [0, n) on the calling
// goroutine plus helpers from the module's shared concurrency budget.
func parallelSlices(n int, work func(int)) {
	_ = parallel.Do(context.Background(), n, 0, work)
}

// Grid returns the field as a Deformable Spatial Registration grid in the
// fixed (registered) Frame of Reference, the displacement of each sample
// pointing into the moving (source) one. The fixed image's axes must be
// right-handed, as DICOM's grid orientation implies.
func (f *DisplacementField) Grid() (*spatialreg.Grid, error) {
	d := f.Direction
	row := render.Vec3{X: d[0][0], Y: d[1][0], Z: d[2][0]}
	column := render.Vec3{X: d[0][1], Y: d[1][1], Z: d[2][1]}
	normal := render.Vec3{X: d[0][2], Y: d[1][2], Z: d[2][2]}
	if row.Cross(column).Dot(normal) <= 0 {
		return nil, fmt.Errorf("%w: the field's axes are left-handed", ErrDemons)
	}
	return spatialreg.NewGrid(f.Origin, row, column, f.Spacing, [3]uint32{uint32(f.Size[0]), uint32(f.Size[1]), uint32(f.Size[2])}, f.Vectors)
}
