package imagefilter

import (
	"context"
	"fmt"
	"github.com/ThalesMMS/dicom-go/internal/fft"
	"github.com/ThalesMMS/dicom-go/internal/parallel"
	"math"
)

// Frozen 08fc8b N4 algorithm, namespaced solely for exact regression comparisons.
func N4BiasFieldCorrectionReference1505(ctx context.Context, in *Volume, opts N4Options) (*N4Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := in.Validate(); err != nil {
		return nil, err
	}
	opts, err := opts.normalized(in)
	if err != nil {
		return nil, err
	}
	grid := shrinkForN4Reference1505(in, opts.Mask, opts.ShrinkFactor)
	if len(grid.samples) == 0 {
		return nil, fmt.Errorf("%w: no positive voxel in the N4 mask", ErrInvalidVolume)
	}
	result := &N4Result{ShrunkenDims: grid.dims, Samples: len(grid.samples)}
	logInput := make([]float64, len(grid.samples))
	for k, value := range grid.values {
		logInput[k] = math.Log(value)
	}
	logUncorrected := append([]float64(nil), logInput...)
	logBias := make([]float64, len(grid.samples))
	residual := make([]float64, len(grid.samples))
	sharpened := make([]float64, len(grid.samples))
	axes := grid.axes()
	var lattice *bsplineLatticeReference1505
	for level, maxIterations := range opts.Iterations {
		convergence := math.MaxFloat64
		iterations := 0
		for iterations < maxIterations && convergence > opts.ConvergenceThreshold {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			iterations++
			sharpenN4Reference1505(logUncorrected, sharpened, opts)
			for k := range residual {
				residual[k] = logUncorrected[k] - sharpened[k]
			}
			size := [3]int{opts.ControlPoints, opts.ControlPoints, opts.ControlPoints}
			if lattice != nil {
				size = lattice.n
			}
			phi := fitBSplineReference1505(size, axes, grid.samples, residual, opts.Workers)
			if lattice == nil {
				lattice = phi
			} else {
				for i := range lattice.phi {
					lattice.phi[i] += phi.phi[i]
				}
			}
			newBias := lattice.evaluateAt(axes, grid.samples, opts.Workers)
			convergence = n4ConvergenceReference1505(logBias, newBias)
			logBias = newBias
			for k := range logUncorrected {
				logUncorrected[k] = logInput[k] - logBias[k]
			}
			if opts.Progress != nil {
				opts.Progress(N4Progress{Level: level, Levels: len(opts.Iterations), Iteration: iterations, Convergence: convergence})
			}
		}
		result.Levels = append(result.Levels, N4Level{Iterations: iterations, Convergence: convergence})
		if level < len(opts.Iterations)-1 {
			lattice = lattice.refine(grid.dims)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	field := lattice.evaluateGrid(grid.fullAxes(in.Dims), opts.Workers)
	result.LogBiasField = &Volume{Dims: in.Dims, SpacingMM: in.SpacingMM, Data: make([]float32, len(field))}
	result.Corrected = &Volume{Dims: in.Dims, SpacingMM: in.SpacingMM, Data: make([]float32, len(field))}
	for i, logB := range field {
		result.LogBiasField.Data[i] = float32(logB)
		result.Corrected.Data[i] = float32(float64(in.Data[i]) / math.Exp(logB))
	}
	return result, nil
}

// n4GridReference1505 is the block-averaged estimation grid: the masked, positive
// voxels of each block averaged, their blocks' centres in input voxel
// coordinates.
type n4GridReference1505 struct {
	dims    [3]int
	factor  int
	centres [3][]float64
	// samples are the grid voxels with data, values their mean intensity.
	samples []int
	values  []float64
}

func shrinkForN4Reference1505(in *Volume, mask []bool, factor int) *n4GridReference1505 {
	g := &n4GridReference1505{factor: factor}
	for axis, n := range in.Dims {
		g.dims[axis] = (n + factor - 1) / factor
		g.centres[axis] = make([]float64, g.dims[axis])
		for i := range g.centres[axis] {
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
				if value := in.Data[i]; value > 0 && (mask == nil || mask[i]) {
					sums[block+x/factor] += float64(value)
					counts[block+x/factor]++
				}
			}
		}
	}
	for i, count := range counts {
		if count > 0 {
			g.samples = append(g.samples, i)
			g.values = append(g.values, sums[i]/float64(count))
		}
	}
	return g
}

// axisSplineReference1505 places the samples of one axis in the parametric domain of the
// lattice: position = (coordinate − lo)/(hi − lo), in [0, 1], hi > lo, or 0
// for an axis of one sample.
type axisSplineReference1505 struct {
	positions []float64
	// step is the parametric distance between neighbouring estimation
	// samples, for ITK's end-of-domain epsilon.
	step float64
}

func (g *n4GridReference1505) axes() [3]axisSplineReference1505 {
	var axes [3]axisSplineReference1505
	for axis := range axes {
		n := g.dims[axis]
		axes[axis].positions = make([]float64, n)
		if n > 1 {
			axes[axis].step = 1 / float64(n-1)
			for i := range axes[axis].positions {
				axes[axis].positions[i] = float64(i) / float64(n-1)
			}
		}
	}
	return axes
}

// fullAxes places the input voxels in the same domain: the block centres
// span [0, 1], and voxels beyond the first and last centre are clamped.
func (g *n4GridReference1505) fullAxes(dims [3]int) [3]axisSplineReference1505 {
	var axes [3]axisSplineReference1505
	for axis := range axes {
		centres := g.centres[axis]
		axes[axis].positions = make([]float64, dims[axis])
		if len(centres) < 2 {
			continue
		}
		lo, hi := centres[0], centres[len(centres)-1]
		axes[axis].step = 1 / float64(len(centres)-1)
		for i := range axes[axis].positions {
			axes[axis].positions[i] = math.Min(1, math.Max(0, (float64(i)-lo)/(hi-lo)))
		}
	}
	return axes
}

// bsplineLatticeReference1505 is a cubic B-spline control lattice, x-fastest; along an
// axis of n control points it spans n − 3 unit intervals of the parametric
// domain.
type bsplineLatticeReference1505 struct {
	n   [3]int
	phi []float64
}

// cubicSpanReference1505 returns the interval of parametric position s ∈ [0, 1] on a
// lattice of n control points and the four weights of control points
// span … span+3, applying ITK's epsilon at the end of the domain.
func cubicSpanReference1505(s, step float64, n int) (int, [4]float64) {
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

type splineWeightsReference1505 struct {
	span    []int
	weights [][4]float64
}

func weightsForReference1505(axis axisSplineReference1505, n int) splineWeightsReference1505 {
	w := splineWeightsReference1505{span: make([]int, len(axis.positions)), weights: make([][4]float64, len(axis.positions))}
	for i, s := range axis.positions {
		w.span[i], w.weights[i] = cubicSpanReference1505(s, axis.step, n)
	}
	return w
}

// n4FitBlockReference1505 is the number of samples one block of the B-spline fit
// accumulates. Blocks are fixed by the sample count alone and summed in
// order, so the fit is the same for any number of workers.
const n4FitBlockReference1505 = 4096

// n4MaxFitBlocksReference1505 bounds the blocks, each holding two lattice-sized
// accumulators.
const n4MaxFitBlocksReference1505 = 16

// fitBSplineReference1505 fits a lattice of size control points to the residual values
// of the samples (grid voxel indices), Lee, Wolberg and Shin's single-level
// approximation as ITK computes it: each sample spreads r·B³/ΣB² into δ and
// B² into ω over its 4 × 4 × 4 control points, and φ = δ/ω. The samples are
// split into blocks accumulated on up to workers workers.
func fitBSplineReference1505(size [3]int, axes [3]axisSplineReference1505, samples []int, values []float64, workers int) *bsplineLatticeReference1505 {
	var w [3]splineWeightsReference1505
	for axis := range w {
		w[axis] = weightsForReference1505(axes[axis], size[axis])
	}
	nx, ny := len(axes[0].positions), len(axes[1].positions)
	total := size[0] * size[1] * size[2]
	blocks := min(n4MaxFitBlocksReference1505, max(1, len(samples)/n4FitBlockReference1505))
	deltas, omegas := make([][]float64, blocks), make([][]float64, blocks)
	_ = parallel.Do(context.Background(), blocks, workers, func(block int) {
		delta, omega := make([]float64, total), make([]float64, total)
		for k := block * len(samples) / blocks; k < (block+1)*len(samples)/blocks; k++ {
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
	lattice := &bsplineLatticeReference1505{n: size, phi: make([]float64, total)}
	for i := range lattice.phi {
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
	return lattice
}

// evaluateGrid evaluates the spline at every voxel of the grid the axes
// describe (x-fastest), collapsing the lattice along z, then y, then x, one
// slice per item on up to workers workers.
func (l *bsplineLatticeReference1505) evaluateGrid(axes [3]axisSplineReference1505, workers int) []float64 {
	var w [3]splineWeightsReference1505
	for axis := range w {
		w[axis] = weightsForReference1505(axes[axis], l.n[axis])
	}
	nx, ny, nz := len(axes[0].positions), len(axes[1].positions), len(axes[2].positions)
	out := make([]float64, nx*ny*nz)
	_ = parallel.Run(context.Background(), nz, workers, 1, func() func(int) {
		plane := make([]float64, l.n[0]*l.n[1])
		line := make([]float64, l.n[0])
		return func(z int) {
			clear(plane)
			for c := 0; c < 4; c++ {
				zi := w[2].span[z] + c
				if zi >= l.n[2] {
					continue
				}
				weight := w[2].weights[z][c]
				base := zi * l.n[0] * l.n[1]
				for i := range plane {
					plane[i] += weight * l.phi[base+i]
				}
			}
			for y := 0; y < ny; y++ {
				clear(line)
				for b := 0; b < 4; b++ {
					yi := w[1].span[y] + b
					if yi >= l.n[1] {
						continue
					}
					weight := w[1].weights[y][b]
					for i := range line {
						line[i] += weight * plane[yi*l.n[0]+i]
					}
				}
				row := (z*ny + y) * nx
				for x := 0; x < nx; x++ {
					var v float64
					for a := 0; a < 4; a++ {
						if xi := w[0].span[x] + a; xi < l.n[0] {
							v += w[0].weights[x][a] * line[xi]
						}
					}
					out[row+x] = v
				}
			}
		}
	})
	return out
}

// evaluateAt evaluates the spline at the given voxels of the grid.
func (l *bsplineLatticeReference1505) evaluateAt(axes [3]axisSplineReference1505, samples []int, workers int) []float64 {
	grid := l.evaluateGrid(axes, workers)
	out := make([]float64, len(samples))
	for k, i := range samples {
		out[k] = grid[i]
	}
	return out
}

// refine doubles the lattice resolution without changing the spline: along
// each axis n control points become 2n − 3, by the cubic B-spline two-scale
// relation (ITK's RefineControlPointLattice). Unlike ITK, an axis is not
// refined beyond half its samples on the estimation grid (and at least four
// control points), so a small or thin volume cannot fit its anatomy as
// bias.
func (l *bsplineLatticeReference1505) refine(samples [3]int) *bsplineLatticeReference1505 {
	out := l
	for axis := 0; axis < 3; axis++ {
		if 2*out.n[axis]-n4SplineOrder <= max(n4SplineOrder+1, samples[axis]/2) {
			out = out.refineAxis(axis)
		}
	}
	return out
}

func (l *bsplineLatticeReference1505) refineAxis(axis int) *bsplineLatticeReference1505 {
	n := l.n[axis]
	size := l.n
	size[axis] = 2*n - n4SplineOrder
	out := &bsplineLatticeReference1505{n: size, phi: make([]float64, size[0]*size[1]*size[2])}
	stride := [3]int{1, l.n[0], l.n[0] * l.n[1]}
	outStride := [3]int{1, size[0], size[0] * size[1]}
	for z := 0; z < size[2]; z++ {
		for y := 0; y < size[1]; y++ {
			for x := 0; x < size[0]; x++ {
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
	return out
}

// sharpenN4Reference1505 sharpens the histogram of the log intensities (ITK's
// SharpenImage): a 200-bin histogram with linear (Parzen) binning, zero
// padded to a power of two, is Wiener-deconvolved from a Gaussian of the
// given FWHM, and each value is mapped to E[u|v] of the deconvolved
// histogram.
func sharpenN4Reference1505(values, out []float64, opts N4Options) {
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range values {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	if hi <= lo {
		copy(out, values)
		return
	}
	bins := opts.HistogramBins
	slope := (hi - lo) / float64(bins-1)
	histogram := make([]float64, bins)
	for _, v := range values {
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
		V[n+padding] = complex(h, 0)
	}
	fft.Transform(V, false)

	scaledFWHM := opts.BiasFieldFWHM / slope
	expFactor := 4 * math.Ln2 / (scaledFWHM * scaledFWHM)
	scaleFactor := 2 * math.Sqrt(math.Ln2/math.Pi) / scaledFWHM
	F := make([]complex128, padded)
	F[0] = complex(scaleFactor, 0)
	half := padded / 2
	for n := 1; n <= half; n++ {
		value := complex(scaleFactor*math.Exp(-float64(n*n)*expFactor), 0)
		F[n], F[padded-n] = value, value
	}
	if padded%2 == 0 {
		F[half] = complex(scaleFactor*math.Exp(0.25*-float64(padded*padded)*expFactor), 0)
	}
	fft.Transform(F, false)

	U := make([]complex128, padded)
	for n := range U {
		c := complex(real(F[n]), -imag(F[n]))
		g := c / (c*F[n] + complex(opts.WienerNoise, 0))
		U[n] = V[n] * complex(real(g), 0)
	}
	fft.Transform(U, true)
	scale := 1 / float64(padded)
	for n := range U {
		U[n] = complex(math.Max(real(U[n])*scale, 0), 0)
	}

	numerator := make([]complex128, padded)
	denominator := make([]complex128, padded)
	for n := range numerator {
		numerator[n] = complex((lo+(float64(n)-float64(padding))*slope)*real(U[n]), 0)
		denominator[n] = U[n]
	}
	for _, series := range [][]complex128{numerator, denominator} {
		fft.Transform(series, false)
		for n := range series {
			series[n] *= F[n]
		}
		fft.Transform(series, true)
	}
	E := make([]float64, bins)
	for n := range E {
		if d := real(denominator[n+padding]); d != 0 {
			E[n] = real(numerator[n+padding]) / d
		}
	}
	for k, v := range values {
		c := (v - lo) / slope
		i := int(math.Floor(c))
		if i < bins-1 {
			out[k] = E[i] + (E[i+1]-E[i])*(c-float64(i))
		} else {
			out[k] = E[bins-1]
		}
	}
}

// n4ConvergenceReference1505 is the coefficient of variation of exp(before − after), as
// ITK's CalculateConvergenceMeasurement accumulates it.
func n4ConvergenceReference1505(before, after []float64) float64 {
	var mu, sigma, n float64
	for k := range before {
		pixel := math.Exp(before[k] - after[k])
		n++
		if n > 1 {
			sigma += (pixel - mu) * (pixel - mu) * (n - 1) / n
		}
		mu = mu*(1-1/n) + pixel/n
	}
	return math.Sqrt(sigma/(n-1)) / mu
}
