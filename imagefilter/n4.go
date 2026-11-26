package imagefilter

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/ThalesMMS/dicom-go/internal/fft"
	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// N4 bias field correction (#1104).
//
// Ported from ITK (Insight Toolkit), commit
// 35b196e13c366e0386389a71c936a0436c568cea, Apache-2.0 (see
// THIRD_PARTY_NOTICES.md): N4BiasFieldCorrectionImageFilter (Tustison et
// al., IEEE TMI 2010) and the cubic B-spline approximation of its
// BSplineScatteredDataPointSetToImageFilter and
// BSplineControlPointImageFilter (Lee, Wolberg and Shin, IEEE TVCG 1997).
// Modified: the estimate runs on a block-averaged copy of the volume, and
// the field is then evaluated on the full grid at the positions of the
// block centres, instead of on an ITK shrunken image; the computation is in
// float64, the B-spline lattice is refined separably and not beyond half
// the samples along an axis, and the fit spreads fixed blocks of samples
// over at most Workers workers from the module's shared concurrency budget
// (#1188), summing the blocks in order so the result does not depend on the
// worker count.
//
// The model is v = u·b + n: the slowly varying multiplicative field b
// (surface coil sensitivity in MR) is estimated in log space by alternately
// sharpening the intensity histogram (Wiener deconvolution of a Gaussian
// blur) and fitting a cubic B-spline to the residual, refining the control
// lattice at each fitting level.

// N4Options configure N4BiasFieldCorrection. Zero values take the defaults
// of ITK's filter, except ShrinkFactor and Iterations, which default to the
// usual ANTs settings.
type N4Options struct {
	// Mask selects the voxels the field is estimated from (x-fastest, one
	// entry per voxel); nil selects every voxel. Voxels at or below zero are
	// always left out, since the model is multiplicative.
	Mask []bool
	// ShrinkFactor block-averages the volume by this factor along each axis
	// before estimating (default 4; 1 estimates at full resolution).
	ShrinkFactor int
	// Iterations are the maximum iterations of each fitting level; the
	// control lattice doubles its resolution between levels (default 50,
	// 50, 50, 50).
	Iterations []int
	// ConvergenceThreshold stops a level when the coefficient of variation
	// of the change in the field falls below it (default 0.001).
	ConvergenceThreshold float64
	// ControlPoints is the number of control points along each axis at the
	// first level (default 4, at least 4 for the cubic spline).
	ControlPoints int
	// HistogramBins, BiasFieldFWHM and WienerNoise shape the histogram
	// sharpening (defaults 200, 0.15 and 0.01).
	HistogramBins int
	BiasFieldFWHM float64
	WienerNoise   float64
	// Workers bounds the goroutines of the B-spline fit and evaluation (0
	// uses the module's shared concurrency budget, GOMAXPROCS workers by
	// default). Workers beyond the calling goroutine are taken from that
	// budget, so concurrent filters and renders together stay within it. The
	// result does not depend on it.
	Workers int
	// Progress, when set, is called after every iteration.
	Progress func(N4Progress)
}

// N4Progress reports one iteration.
type N4Progress struct {
	Level, Levels, Iteration int
	Convergence              float64
}

// N4Level summarizes one fitting level.
type N4Level struct {
	Iterations  int
	Convergence float64
}

// N4Result is the corrected volume and the estimated field.
type N4Result struct {
	// Corrected is the input divided by the bias field.
	Corrected *Volume
	// LogBiasField is the natural logarithm of the bias field on the input
	// grid.
	LogBiasField *Volume
	// Levels are the fitting levels run.
	Levels []N4Level
	// ShrunkenDims and Samples describe the estimation grid and the number
	// of its voxels in the mask.
	ShrunkenDims [3]int
	Samples      int
}

const n4SplineOrder = 3

func (o N4Options) normalized(in *Volume) (N4Options, error) {
	if o.ShrinkFactor == 0 {
		o.ShrinkFactor = 4
	}
	if len(o.Iterations) == 0 {
		o.Iterations = []int{50, 50, 50, 50}
	}
	if o.ConvergenceThreshold == 0 {
		o.ConvergenceThreshold = 0.001
	}
	if o.ControlPoints == 0 {
		o.ControlPoints = 4
	}
	if o.HistogramBins == 0 {
		o.HistogramBins = 200
	}
	if o.BiasFieldFWHM == 0 {
		o.BiasFieldFWHM = 0.15
	}
	if o.WienerNoise == 0 {
		o.WienerNoise = 0.01
	}
	switch {
	case o.ShrinkFactor < 1:
		return o, fmt.Errorf("%w: N4 shrink factor %d", ErrInvalidVolume, o.ShrinkFactor)
	case o.ControlPoints < n4SplineOrder+1:
		return o, fmt.Errorf("%w: N4 needs at least %d control points, not %d", ErrInvalidVolume, n4SplineOrder+1, o.ControlPoints)
	case o.HistogramBins < 2:
		return o, fmt.Errorf("%w: N4 histogram of %d bins", ErrInvalidVolume, o.HistogramBins)
	case !(o.ConvergenceThreshold > 0) || !(o.BiasFieldFWHM > 0) || !(o.WienerNoise > 0):
		return o, fmt.Errorf("%w: N4 convergence threshold, FWHM and Wiener noise must be positive", ErrInvalidVolume)
	case o.Workers < 0:
		return o, fmt.Errorf("%w: N4 workers %d", ErrInvalidVolume, o.Workers)
	case o.Mask != nil && len(o.Mask) != len(in.Data):
		return o, fmt.Errorf("%w: N4 mask of %d voxels for %d", ErrInvalidVolume, len(o.Mask), len(in.Data))
	}
	for _, n := range o.Iterations {
		if n < 1 {
			return o, fmt.Errorf("%w: N4 level of %d iterations", ErrInvalidVolume, n)
		}
	}
	return o, nil
}

// N4BiasFieldCorrection estimates the multiplicative bias field of in and
// divides it out. The field is smooth by construction (a cubic B-spline on
// a lattice of at most ControlPoints·2^(levels−1) control points per axis),
// so it corrects intensity non-uniformity, not contrast.
func N4BiasFieldCorrection(ctx context.Context, in *Volume, opts N4Options) (*N4Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateN4Context(ctx, in); err != nil {
		return nil, err
	}
	opts, err := opts.normalized(in)
	if err != nil {
		return nil, err
	}
	grid, err := shrinkForN4Context(ctx, in, opts.Mask, opts.ShrinkFactor)
	if err != nil {
		return nil, err
	}
	if len(grid.samples) == 0 {
		return nil, fmt.Errorf("%w: no positive voxel in the N4 mask", ErrInvalidVolume)
	}
	result := &N4Result{ShrunkenDims: grid.dims, Samples: len(grid.samples)}
	logInput := make([]float64, len(grid.samples))
	for k, value := range grid.values {
		if err := n4Check(ctx, k); err != nil {
			return nil, err
		}
		logInput[k] = math.Log(value)
	}
	logUncorrected := make([]float64, len(logInput))
	for k, v := range logInput {
		if err := n4Check(ctx, k); err != nil {
			return nil, err
		}
		logUncorrected[k] = v
	}
	logBias := make([]float64, len(grid.samples))
	// newBias receives each iteration's bias and then trades places with
	// logBias, so iterations allocate no sample-sized buffer (#2025).
	newBias := make([]float64, len(grid.samples))
	residual := make([]float64, len(grid.samples))
	sharpened := make([]float64, len(grid.samples))
	axes, err := grid.axesContext(ctx)
	if err != nil {
		return nil, err
	}
	var lattice *bsplineLattice
	for level, maxIterations := range opts.Iterations {
		convergence := math.MaxFloat64
		iterations := 0
		for iterations < maxIterations && convergence > opts.ConvergenceThreshold {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			iterations++
			if err := sharpenN4Context(ctx, logUncorrected, sharpened, opts); err != nil {
				return nil, err
			}
			for k := range residual {
				if err := n4Check(ctx, k); err != nil {
					return nil, err
				}
				residual[k] = logUncorrected[k] - sharpened[k]
			}
			size := [3]int{opts.ControlPoints, opts.ControlPoints, opts.ControlPoints}
			if lattice != nil {
				size = lattice.n
			}
			phi, err := fitBSplineContext(ctx, size, axes, grid.samples, residual, opts.Workers)
			if err != nil {
				return nil, err
			}
			if lattice == nil {
				lattice = phi
			} else {
				for i := range lattice.phi {
					if err := n4Check(ctx, i); err != nil {
						return nil, err
					}
					lattice.phi[i] += phi.phi[i]
				}
			}
			if _, err := lattice.evaluateAtContext(ctx, axes, grid.samples, opts.Workers, newBias); err != nil {
				return nil, err
			}
			convergence, err = n4ConvergenceContext(ctx, logBias, newBias)
			if err != nil {
				return nil, err
			}
			logBias, newBias = newBias, logBias
			for k := range logUncorrected {
				if err := n4Check(ctx, k); err != nil {
					return nil, err
				}
				logUncorrected[k] = logInput[k] - logBias[k]
			}
			if opts.Progress != nil {
				opts.Progress(N4Progress{Level: level, Levels: len(opts.Iterations), Iteration: iterations, Convergence: convergence})
			}
		}
		result.Levels = append(result.Levels, N4Level{Iterations: iterations, Convergence: convergence})
		if level < len(opts.Iterations)-1 {
			lattice, err = lattice.refineContext(ctx, grid.dims)
			if err != nil {
				return nil, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result.LogBiasField = &Volume{Dims: in.Dims, SpacingMM: in.SpacingMM, Data: make([]float32, len(in.Data))}
	result.Corrected = &Volume{Dims: in.Dims, SpacingMM: in.SpacingMM, Data: make([]float32, len(in.Data))}
	axes, err = grid.fullAxesContext(ctx, in.Dims)
	if err != nil {
		return nil, err
	}
	err = lattice.evaluateToContext(ctx, axes, opts.Workers, func(_ int) func(int, float64) {
		return func(i int, logB float64) {
			result.LogBiasField.Data[i] = float32(logB)
			result.Corrected.Data[i] = float32(float64(in.Data[i]) / math.Exp(logB))
		}
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// n4Grid is the block-averaged estimation grid: the masked, positive
// voxels of each block averaged, their blocks' centres in input voxel
// coordinates.
type n4Grid struct {
	dims    [3]int
	factor  int
	centres [3][]float64
	// samples are the grid voxels with data, values their mean intensity.
	samples []int
	values  []float64
}

func shrinkForN4Context(ctx context.Context, in *Volume, mask []bool, factor int) (*n4Grid, error) {
	g := &n4Grid{factor: factor}
	for axis, n := range in.Dims {
		g.dims[axis] = (n + factor - 1) / factor
		g.centres[axis] = make([]float64, g.dims[axis])
		for i := range g.centres[axis] {
			if err := n4Check(ctx, i); err != nil {
				return nil, err
			}
			length := min(factor, n-i*factor)
			g.centres[axis][i] = float64(i*factor) + float64(length-1)/2
		}
	}
	sums := make([]float64, g.dims[0]*g.dims[1]*g.dims[2])
	counts := make([]int, len(sums))
	for z := 0; z < in.Dims[2]; z++ {
		for y := 0; y < in.Dims[1]; y++ {
			row := (z*in.Dims[1] + y) * in.Dims[0]
			block := ((z/factor)*g.dims[1] + y/factor) * g.dims[0]
			for x := 0; x < in.Dims[0]; x++ {
				i := row + x
				if err := n4Check(ctx, i); err != nil {
					return nil, err
				}
				if value := in.Data[i]; value > 0 && (mask == nil || mask[i]) {
					sums[block+x/factor] += float64(value)
					counts[block+x/factor]++
				}
			}
		}
	}
	for i, count := range counts {
		if err := n4Check(ctx, i); err != nil {
			return nil, err
		}
		if count > 0 {
			g.samples = append(g.samples, i)
			g.values = append(g.values, sums[i]/float64(count))
		}
	}
	return g, ctx.Err()
}

// axisSpline places the samples of one axis in the parametric domain of the
// lattice: position = (coordinate − lo)/(hi − lo), in [0, 1], hi > lo, or 0
// for an axis of one sample.
type axisSpline struct {
	positions []float64
	// step is the parametric distance between neighbouring estimation
	// samples, for ITK's end-of-domain epsilon.
	step float64
}

func (g *n4Grid) axes() [3]axisSpline { axes, _ := g.axesContext(context.Background()); return axes }

func (g *n4Grid) axesContext(ctx context.Context) ([3]axisSpline, error) {
	var axes [3]axisSpline
	for axis := range axes {
		n := g.dims[axis]
		axes[axis].positions = make([]float64, n)
		if n > 1 {
			axes[axis].step = 1 / float64(n-1)
			for i := range axes[axis].positions {
				if err := n4Check(ctx, i); err != nil {
					return axes, err
				}
				axes[axis].positions[i] = float64(i) / float64(n-1)
			}
		}
	}
	return axes, ctx.Err()
}

// fullAxes places the input voxels in the same domain: the block centres
// span [0, 1], and voxels beyond the first and last centre are clamped.
func (g *n4Grid) fullAxesContext(ctx context.Context, dims [3]int) ([3]axisSpline, error) {
	var axes [3]axisSpline
	for axis := range axes {
		centres := g.centres[axis]
		axes[axis].positions = make([]float64, dims[axis])
		if len(centres) < 2 {
			continue
		}
		lo, hi := centres[0], centres[len(centres)-1]
		axes[axis].step = 1 / float64(len(centres)-1)
		for i := range axes[axis].positions {
			if err := n4Check(ctx, i); err != nil {
				return axes, err
			}
			axes[axis].positions[i] = math.Min(1, math.Max(0, (float64(i)-lo)/(hi-lo)))
		}
	}
	return axes, ctx.Err()
}

// bsplineLattice is a cubic B-spline control lattice, x-fastest; along an
// axis of n control points it spans n − 3 unit intervals of the parametric
// domain.
type bsplineLattice struct {
	n   [3]int
	phi []float64
}

// cubicSpan returns the interval of parametric position s ∈ [0, 1] on a
// lattice of n control points and the four weights of control points
// span … span+3, applying ITK's epsilon at the end of the domain.
func cubicSpan(s, step float64, n int) (int, [4]float64) {
	spans := float64(n - n4SplineOrder)
	p := s * spans
	epsilon := spans * step * 1e-3
	if math.Abs(p-spans) <= epsilon {
		p = spans - epsilon
	}
	p = math.Max(0, math.Min(p, math.Nextafter(spans, 0)))
	span := int(p)
	t := p - float64(span)
	u := 1 - t
	return span, [4]float64{
		u * u * u / 6,
		(3*t*t*t - 6*t*t + 4) / 6,
		(-3*t*t*t + 3*t*t + 3*t + 1) / 6,
		t * t * t / 6,
	}
}

type splineWeights struct {
	span    []int
	weights [][4]float64
}

func weightsForContext(ctx context.Context, axis axisSpline, n int) (splineWeights, error) {
	if err := ctx.Err(); err != nil {
		return splineWeights{}, err
	}
	w := splineWeights{span: make([]int, len(axis.positions)), weights: make([][4]float64, len(axis.positions))}
	for i, s := range axis.positions {
		if err := n4Check(ctx, i); err != nil {
			return w, err
		}
		w.span[i], w.weights[i] = cubicSpan(s, axis.step, n)
	}
	return w, ctx.Err()
}

// n4FitBlock is the number of samples one block of the B-spline fit
// accumulates. Blocks are fixed by the sample count alone and summed in
// order, so the fit is the same for any number of workers.
const n4FitBlock = 4096

// n4MaxFitBlocks bounds the blocks, each holding two lattice-sized
// accumulators.
const n4MaxFitBlocks = 16

// fitBSpline fits a lattice of size control points to the residual values
// of the samples (grid voxel indices), Lee, Wolberg and Shin's single-level
// approximation as ITK computes it: each sample spreads r·B³/ΣB² into δ and
// B² into ω over its 4 × 4 × 4 control points, and φ = δ/ω. The samples are
// split into blocks accumulated on up to workers workers.
func fitBSplineContext(ctx context.Context, size [3]int, axes [3]axisSpline, samples []int, values []float64, workers int) (*bsplineLattice, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var w [3]splineWeights
	for axis := range w {
		var err error
		w[axis], err = weightsForContext(ctx, axes[axis], size[axis])
		if err != nil {
			return nil, err
		}
	}
	nx, ny := len(axes[0].positions), len(axes[1].positions)
	total := size[0] * size[1] * size[2]
	blocks := min(n4MaxFitBlocks, max(1, len(samples)/n4FitBlock))
	deltas, omegas := make([][]float64, blocks), make([][]float64, blocks)
	err := parallel.Do(ctx, blocks, workers, func(block int) {
		delta, omega := make([]float64, total), make([]float64, total)
		for k := block * len(samples) / blocks; k < (block+1)*len(samples)/blocks; k++ {
			if n4Check(ctx, k) != nil {
				return
			}
			i := samples[k]
			x, y, z := i%nx, (i/nx)%ny, i/(nx*ny)
			bx, by, bz := w[0].weights[x], w[1].weights[y], w[2].weights[z]
			var sx, sy, sz float64
			for j := 0; j < 4; j++ {
				sx += bx[j] * bx[j]
				sy += by[j] * by[j]
				sz += bz[j] * bz[j]
			}
			scale := values[k] / (sx * sy * sz)
			for c := 0; c < 4; c++ {
				zi := w[2].span[z] + c
				if zi >= size[2] {
					continue
				}
				for b := 0; b < 4; b++ {
					yi := w[1].span[y] + b
					if yi >= size[1] {
						continue
					}
					bzy := bz[c] * by[b]
					row := (zi*size[1] + yi) * size[0]
					for a := 0; a < 4; a++ {
						xi := w[0].span[x] + a
						if xi >= size[0] {
							continue
						}
						B := bzy * bx[a]
						B2 := B * B
						omega[row+xi] += B2
						delta[row+xi] += scale * B2 * B
					}
				}
			}
		}
		deltas[block], omegas[block] = delta, omega
	})
	if err != nil {
		return nil, err
	}
	lattice := &bsplineLattice{n: size, phi: make([]float64, total)}
	for i := range lattice.phi {
		if err := n4Check(ctx, i); err != nil {
			return nil, err
		}
		var delta, omega float64
		for block := range deltas {
			delta += deltas[block][i]
			omega += omegas[block][i]
		}
		if math.Abs(omega) > 0.1*2.220446049250313e-16 {
			if phi := delta / omega; !math.IsNaN(phi) && !math.IsInf(phi, 0) {
				lattice.phi[i] = phi
			}
		}
	}
	return lattice, ctx.Err()
}

// evaluateGrid evaluates the spline at every voxel of the grid the axes
// describe (x-fastest), collapsing the lattice along z, then y, then x, one
// slice per item on up to workers workers.
func (l *bsplineLattice) evaluateToContext(ctx context.Context, axes [3]axisSpline, workers int, emit func(int) func(int, float64)) error {
	var w [3]splineWeights
	for axis := range w {
		var err error
		w[axis], err = weightsForContext(ctx, axes[axis], l.n[axis])
		if err != nil {
			return err
		}
	}
	nx, ny, nz := len(axes[0].positions), len(axes[1].positions), len(axes[2].positions)
	return parallel.Run(ctx, nz, workers, 1, func() func(int) {
		plane := make([]float64, l.n[0]*l.n[1])
		line := make([]float64, l.n[0])
		return func(z int) {
			sink := emit(z)
			clear(plane)
			for c := 0; c < 4; c++ {
				zi := w[2].span[z] + c
				if zi >= l.n[2] {
					continue
				}
				weight := w[2].weights[z][c]
				base := zi * l.n[0] * l.n[1]
				for i := range plane {
					if n4Check(ctx, i) != nil {
						return
					}
					plane[i] += weight * l.phi[base+i]
				}
			}
			for y := 0; y < ny; y++ {
				if ctx.Err() != nil {
					return
				}
				clear(line)
				for b := 0; b < 4; b++ {
					yi := w[1].span[y] + b
					if yi >= l.n[1] {
						continue
					}
					weight := w[1].weights[y][b]
					for i := range line {
						if n4Check(ctx, i) != nil {
							return
						}
						line[i] += weight * plane[yi*l.n[0]+i]
					}
				}
				row := (z*ny + y) * nx
				for x := 0; x < nx; x++ {
					if n4Check(ctx, x) != nil {
						return
					}
					var v float64
					for a := 0; a < 4; a++ {
						if xi := w[0].span[x] + a; xi < l.n[0] {
							v += w[0].weights[x][a] * line[xi]
						}
					}
					sink(row+x, v)
				}
			}
		}
	})
}

// evaluateGrid remains the package-test helper. Production emits directly to
// the two required public outputs, with no full float64 reconstruction.
func (l *bsplineLattice) evaluateGrid(axes [3]axisSpline, workers int) []float64 {
	out := make([]float64, len(axes[0].positions)*len(axes[1].positions)*len(axes[2].positions))
	_ = l.evaluateToContext(context.Background(), axes, workers, func(int) func(int, float64) { return func(i int, v float64) { out[i] = v } })
	return out
}

func (l *bsplineLattice) evaluateAt(axes [3]axisSpline, samples []int, workers int) []float64 {
	out, _ := l.evaluateAtContext(context.Background(), axes, samples, workers, nil)
	return out
}

// Sparse evaluation costs 64 lattice contributions per selected sample;
// dense evaluation reuses complete collapsed rows. A fixed 1/16 occupancy
// crossover is a cost heuristic, not a measured speedup guarantee. Both paths
// preserve the original z-then-y-then-x floating-point accumulation order.
// The values are written into out, whatever it held, when it has room for
// one per sample, so iterations can reuse it; otherwise into a new slice.
func (l *bsplineLattice) evaluateAtContext(ctx context.Context, axes [3]axisSpline, samples []int, workers int, out []float64) ([]float64, error) {
	if cap(out) < len(samples) {
		out = make([]float64, len(samples))
	}
	out = out[:len(samples)]
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	nx, ny, nz := len(axes[0].positions), len(axes[1].positions), len(axes[2].positions)
	sorted := true
	for k := 1; k < len(samples); k++ {
		if err := n4Check(ctx, k); err != nil {
			return nil, err
		}
		if samples[k] <= samples[k-1] {
			sorted = false
			break
		}
	}
	if sorted && len(samples) > nx*ny*nz/16 {
		err := l.evaluateToContext(ctx, axes, workers, func(z int) func(int, float64) {
			k := sort.SearchInts(samples, z*nx*ny)
			return func(i int, v float64) {
				if k < len(samples) && samples[k] == i {
					out[k] = v
					k++
				}
			}
		})
		if err != nil {
			return nil, err
		}
		return out, nil
	}
	var w [3]splineWeights
	for axis := range w {
		var err error
		w[axis], err = weightsForContext(ctx, axes[axis], l.n[axis])
		if err != nil {
			return nil, err
		}
	}
	// Workers claim runs of samples rather than one at a time (#2025).
	err := parallel.Run(ctx, len(samples), workers, 0, func() func(int) {
		return func(k int) {
			if ctx.Err() != nil {
				return
			}
			i := samples[k]
			x, y, z := i%nx, (i/nx)%ny, i/(nx*ny)
			var plane [4][4]float64
			for c := 0; c < 4; c++ {
				zi := w[2].span[z] + c
				if zi >= l.n[2] {
					continue
				}
				for b := 0; b < 4; b++ {
					yi := w[1].span[y] + b
					if yi >= l.n[1] {
						continue
					}
					for a := 0; a < 4; a++ {
						xi := w[0].span[x] + a
						if xi < l.n[0] {
							plane[b][a] += w[2].weights[z][c] * l.phi[(zi*l.n[1]+yi)*l.n[0]+xi]
						}
					}
				}
			}
			var line [4]float64
			for b := 0; b < 4; b++ {
				for a := 0; a < 4; a++ {
					line[a] += w[1].weights[y][b] * plane[b][a]
				}
			}
			var sum float64
			for a := 0; a < 4; a++ {
				sum += w[0].weights[x][a] * line[a]
			}
			out[k] = sum
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// refine doubles the lattice resolution without changing the spline: along
// each axis n control points become 2n − 3, by the cubic B-spline two-scale
// relation (ITK's RefineControlPointLattice). Unlike ITK, an axis is not
// refined beyond half its samples on the estimation grid (and at least four
// control points), so a small or thin volume cannot fit its anatomy as
// bias.
func (l *bsplineLattice) refine(samples [3]int) *bsplineLattice {
	out, _ := l.refineContext(context.Background(), samples)
	return out
}

func (l *bsplineLattice) refineContext(ctx context.Context, samples [3]int) (*bsplineLattice, error) {
	out := l
	for axis := 0; axis < 3; axis++ {
		if 2*out.n[axis]-n4SplineOrder <= max(n4SplineOrder+1, samples[axis]/2) {
			var err error
			out, err = out.refineAxisContext(ctx, axis)
			if err != nil {
				return nil, err
			}
		}
	}
	return out, ctx.Err()
}

func (l *bsplineLattice) refineAxisContext(ctx context.Context, axis int) (*bsplineLattice, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n := l.n[axis]
	size := l.n
	size[axis] = 2*n - n4SplineOrder
	out := &bsplineLattice{n: size, phi: make([]float64, size[0]*size[1]*size[2])}
	stride := [3]int{1, l.n[0], l.n[0] * l.n[1]}
	outStride := [3]int{1, size[0], size[0] * size[1]}
	for z := 0; z < size[2]; z++ {
		for y := 0; y < size[1]; y++ {
			for x := 0; x < size[0]; x++ {
				if err := n4Check(ctx, (z*size[1]+y)*size[0]+x); err != nil {
					return nil, err
				}
				index := [3]int{x, y, z}
				r := index[axis]
				index[axis] = r / 2
				source := index[0]*stride[0] + index[1]*stride[1] + index[2]*stride[2]
				at := func(offset int) float64 { return l.phi[source+offset*stride[axis]] }
				var v float64
				if r%2 == 0 {
					v = (at(0) + at(1)) / 2
				} else {
					v = (at(0) + 6*at(1) + at(2)) / 8
				}
				out.phi[x*outStride[0]+y*outStride[1]+z*outStride[2]] = v
			}
		}
	}
	return out, ctx.Err()
}

// sharpenN4 sharpens the histogram of the log intensities (ITK's
// SharpenImage): a 200-bin histogram with linear (Parzen) binning, zero
// padded to a power of two, is Wiener-deconvolved from a Gaussian of the
// given FWHM, and each value is mapped to E[u|v] of the deconvolved
// histogram.
func sharpenN4(values, out []float64, opts N4Options) {
	_ = sharpenN4Context(context.Background(), values, out, opts)
}

func sharpenN4Context(ctx context.Context, values, out []float64, opts N4Options) error {
	lo, hi := math.Inf(1), math.Inf(-1)
	for k, v := range values {
		if err := n4Check(ctx, k); err != nil {
			return err
		}
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	if hi <= lo {
		for k, v := range values {
			if err := n4Check(ctx, k); err != nil {
				return err
			}
			out[k] = v
		}
		return ctx.Err()
	}
	bins := opts.HistogramBins
	slope := (hi - lo) / float64(bins-1)
	histogram := make([]float64, bins)
	for k, v := range values {
		if err := n4Check(ctx, k); err != nil {
			return err
		}
		c := (v - lo) / slope
		i := int(math.Floor(c))
		offset := c - float64(i)
		if offset == 0 {
			histogram[i]++
		} else if i < bins-1 {
			histogram[i] += 1 - offset
			histogram[i+1] += offset
		}
	}
	padded := int(math.Pow(2, math.Ceil(math.Log(float64(bins))/math.Log(2))+1) + 0.5)
	padding := int(0.5 * float64(padded-bins))
	V := make([]complex128, padded)
	for n, h := range histogram {
		if err := n4Check(ctx, n); err != nil {
			return err
		}
		V[n+padding] = complex(h, 0)
	}
	if err := fft.TransformContext(ctx, V, false); err != nil {
		return err
	}

	scaledFWHM := opts.BiasFieldFWHM / slope
	expFactor := 4 * math.Ln2 / (scaledFWHM * scaledFWHM)
	scaleFactor := 2 * math.Sqrt(math.Ln2/math.Pi) / scaledFWHM
	F := make([]complex128, padded)
	F[0] = complex(scaleFactor, 0)
	half := padded / 2
	for n := 1; n <= half; n++ {
		if err := n4Check(ctx, n); err != nil {
			return err
		}
		value := complex(scaleFactor*math.Exp(-float64(n*n)*expFactor), 0)
		F[n], F[padded-n] = value, value
	}
	if padded%2 == 0 {
		F[half] = complex(scaleFactor*math.Exp(0.25*-float64(padded*padded)*expFactor), 0)
	}
	if err := fft.TransformContext(ctx, F, false); err != nil {
		return err
	}

	U := make([]complex128, padded)
	for n := range U {
		if err := n4Check(ctx, n); err != nil {
			return err
		}
		c := complex(real(F[n]), -imag(F[n]))
		g := c / (c*F[n] + complex(opts.WienerNoise, 0))
		U[n] = V[n] * complex(real(g), 0)
	}
	if err := fft.TransformContext(ctx, U, true); err != nil {
		return err
	}
	scale := 1 / float64(padded)
	for n := range U {
		if err := n4Check(ctx, n); err != nil {
			return err
		}
		U[n] = complex(math.Max(real(U[n])*scale, 0), 0)
	}

	numerator := make([]complex128, padded)
	denominator := make([]complex128, padded)
	for n := range numerator {
		if err := n4Check(ctx, n); err != nil {
			return err
		}
		numerator[n] = complex((lo+(float64(n)-float64(padding))*slope)*real(U[n]), 0)
		denominator[n] = U[n]
	}
	for _, series := range [][]complex128{numerator, denominator} {
		if err := fft.TransformContext(ctx, series, false); err != nil {
			return err
		}
		for n := range series {
			if err := n4Check(ctx, n); err != nil {
				return err
			}
			series[n] *= F[n]
		}
		if err := fft.TransformContext(ctx, series, true); err != nil {
			return err
		}
	}
	E := make([]float64, bins)
	for n := range E {
		if err := n4Check(ctx, n); err != nil {
			return err
		}
		if d := real(denominator[n+padding]); d != 0 {
			E[n] = real(numerator[n+padding]) / d
		}
	}
	for k, v := range values {
		if err := n4Check(ctx, k); err != nil {
			return err
		}
		c := (v - lo) / slope
		i := int(math.Floor(c))
		if i < bins-1 {
			out[k] = E[i] + (E[i+1]-E[i])*(c-float64(i))
		} else {
			out[k] = E[bins-1]
		}
	}
	return ctx.Err()
}

// n4Convergence is the coefficient of variation of exp(before − after), as
// ITK's CalculateConvergenceMeasurement accumulates it.
func n4ConvergenceContext(ctx context.Context, before, after []float64) (float64, error) {
	var mu, sigma, n float64
	for k := range before {
		if err := n4Check(ctx, k); err != nil {
			return 0, err
		}
		pixel := math.Exp(before[k] - after[k])
		n++
		if n > 1 {
			sigma += (pixel - mu) * (pixel - mu) * (n - 1) / n
		}
		mu = mu*(1-1/n) + pixel/n
	}
	return math.Sqrt(sigma/(n-1)) / mu, ctx.Err()
}

// Poll at bounded work intervals; no output is published until all kernels join.
func n4Check(ctx context.Context, index int) error {
	if index&1023 == 0 {
		return ctx.Err()
	}
	return nil
}

func validateN4Context(ctx context.Context, in *Volume) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if in == nil {
		return fmt.Errorf("%w: nil volume", ErrInvalidVolume)
	}
	n, ok := voxelCount(in.Dims)
	if !ok || len(in.Data) != n {
		return fmt.Errorf("%w: %d samples for dimensions %v", ErrInvalidVolume, len(in.Data), in.Dims)
	}
	for axis, s := range in.SpacingMM {
		if in.Dims[axis] > 1 && !(s > 0 && s <= math.MaxFloat64) {
			return fmt.Errorf("%w: spacing %v mm along axis %d", ErrInvalidVolume, s, axis)
		}
	}
	for i, x := range in.Data {
		if err := n4Check(ctx, i); err != nil {
			return err
		}
		if x != x || math.IsInf(float64(x), 0) {
			return fmt.Errorf("%w: non-finite sample at index %d", ErrInvalidVolume, i)
		}
	}
	return ctx.Err()
}
