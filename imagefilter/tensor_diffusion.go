package imagefilter

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

var (
	ErrTensorDiffusionInput   = errors.New("imagefilter: invalid tensor diffusion input")
	ErrTensorDiffusionLimit   = errors.New("imagefilter: tensor diffusion resource limit")
	ErrTensorDiffusionNumeric = errors.New("imagefilter: unsupported tensor diffusion arithmetic")
)

// TensorDiffusionMethod selects the structure-tensor eigenvalue transform.
type TensorDiffusionMethod string

const (
	CoherenceEnhancing TensorDiffusionMethod = "CED"
	EdgeEnhancing      TensorDiffusionMethod = "EED"
)

// TensorDiffusionLimits bound owned logical payload and stencil sample reads,
// not RSS or goroutine stacks. Zero selects finite defaults; see TENSOR_DIFFUSION.md.
type TensorDiffusionLimits struct {
	MaxVoxels, MaxScratchBytes, MaxWorkingBytes, MaxStencilVisits int64
	MaxIterations                                                 int
}

// TensorDiffusionOptions is an explicit experimental 2D/3D scalar profile.
// Intensity units are those of src; no noise model or modality preset is inferred.
type TensorDiffusionOptions struct {
	Method TensorDiffusionMethod
	// SigmaMM smooths the intensity before physical centered differentiation;
	// RhoMM integrates its gradient outer product. Both are per-axis standard
	// deviations in mm; zero disables that axis's smoothing. The normalized
	// sampled Gaussian has radius ceil(3*sigma/spacing), capped at 64
	// (129 kernel samples).
	SigmaMM, RhoMM [3]float64
	// Alpha is the minimum diffusivity, mandatory in [1e-6,1]. Alpha=1 is
	// isotropic heat diffusion. Threshold is mandatory in [1e-24,1e24], in
	// (input intensity/mm)^2. It thresholds structure-tensor eigenvalue gaps.
	Alpha, Threshold float64
	// Exponent defaults to 2; admitted integers are 1 through 8.
	Exponent int
	// Iterations defaults to 5. TimeStep is in mm²; zero selects 90% of
	// 1/(2*sum_active_axes(1/spacing²)). Larger steps are rejected strictly.
	Iterations int
	TimeStep   float64
	// Workers is 0 for the shared budget, otherwise 1..64; private scratch
	// never grows with workers. Results are independent of worker count.
	Workers int
	// Optional caller identifier, at most 4096 bytes, copied into provenance.
	SourceReference string
	Limits          TensorDiffusionLimits
}

// TensorDiffusionMetadata describes a derived estimate, never acquired pixels.
// Statistics refer to float64 iterates before the final float32 rounding.
type TensorDiffusionMetadata struct {
	Derived                                                               bool
	Method                                                                TensorDiffusionMethod
	Domain, Boundary, Discretization, SourceSHA256, SourceReference       string
	Dims                                                                  [3]int
	SpacingMM, SigmaMM, RhoMM                                             [3]float64
	Alpha, Threshold, TimeStep, StabilityLimit, TotalTime                 float64
	Exponent, IterationsCompleted, WorkersRequested                       int
	ScratchBytes, WorkingBytes, StencilVisitBound                         int64
	InitialMean, FinalMean, InitialL2Squared, FinalL2Squared, ResidualRMS float64
}

type TensorDiffusionResult struct {
	Volume   *Volume
	Metadata TensorDiffusionMetadata
}

type tensorDiffusionPlan struct {
	n, iterations, exponent, workers int
	active                           []int
	strides                          [3]int
	inverse                          [3]float64
	dt, stability                    float64
	radii                            [2][3]int
	kernels                          [2][3][]float64
	scratch, working, visits         int64
}

// TensorAnisotropicDiffusion recomputes J=G_rho*(grad(G_sigma*u) outer grad)
// every explicit step, then uses J's eigenvectors with CED/EED diffusivities.
// The operator is the negative gradient of the averaged one-sided physical
// difference energy over all orthants, with absent boundary differences zero.
// It is symmetric positive semidefinite after sign reversal, conserves the
// mean and contracts L2 at the admitted step. It is not a monotone LBR stencil:
// oblique tensors can create over/undershoot and no maximum principle is claimed.
// Input/identifier must remain immutable during a call. Cancellation and any
// failure return nil, after workers join; no incomplete result is published.
func TensorAnisotropicDiffusion(ctx context.Context, src *Volume, opts TensorDiffusionOptions) (*TensorDiffusionResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	p, err := admitTensorDiffusion(ctx, src, opts)
	if err != nil {
		return nil, err
	}
	for kind := 0; kind < 2; kind++ {
		for _, axis := range p.active {
			r := p.radii[kind][axis]
			if r == 0 {
				continue
			}
			sigma := opts.SigmaMM[axis]
			if kind == 1 {
				sigma = opts.RhoMM[axis]
			}
			kernel := make([]float64, 2*r+1)
			sum := 0.0
			for j := -r; j <= r; j++ {
				x := float64(j) * src.SpacingMM[axis] / sigma
				kernel[j+r] = math.Exp(-0.5 * x * x)
				sum += kernel[j+r]
			}
			for j := range kernel {
				kernel[j] /= sum
			}
			p.kernels[kind][axis] = kernel
		}
	}
	current, next := make([]float64, p.n), make([]float64, p.n)
	smooth, scratch := make([]float64, p.n), make([]float64, p.n)
	var tensor [6][]float64
	for k := range tensor {
		tensor[k] = make([]float64, p.n)
	}
	h := sha256.New()
	var word [4]byte
	var initialMean, initialL2 float64
	for i, value := range src.Data {
		if i&255 == 0 {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
		}
		current[i] = float64(value)
		initialMean += current[i]
		initialL2 += current[i] * current[i]
		binary.LittleEndian.PutUint32(word[:], math.Float32bits(value))
		_, _ = h.Write(word[:])
	}
	for step := 0; step < p.iterations; step++ {
		copy(smooth, current)
		if err = p.smooth(ctx, src.Dims, smooth, scratch, 0); err != nil {
			return nil, err
		}
		if err = p.forEachVoxel(ctx, func(i int) {
			g := p.gradient(src.Dims, smooth, i)
			for a := 0; a < 3; a++ {
				for b := a; b < 3; b++ {
					tensor[tensorComponent(a, b)][i] = g[a] * g[b]
				}
			}
		}); err != nil {
			return nil, err
		}
		for k := range tensor {
			if err = p.smooth(ctx, src.Dims, tensor[k], scratch, 1); err != nil {
				return nil, err
			}
		}
		if err = p.forEachVoxel(ctx, func(i int) {
			var j [6]float64
			for k := range j {
				j[k] = tensor[k][i]
			}
			d := tensorDiffusivity(j, p.active, opts.Method, opts.Alpha, opts.Threshold, p.exponent)
			for k := range d {
				tensor[k][i] = d[k]
			}
		}); err != nil {
			return nil, err
		}
		if err = p.forEachVoxel(ctx, func(i int) {
			next[i] = current[i] + p.dt*p.update(src.Dims, current, tensor, i)
		}); err != nil {
			return nil, err
		}
		current, next = next, current
	}
	out := &Volume{Dims: src.Dims, SpacingMM: src.SpacingMM, Data: make([]float32, p.n)}
	var finalMean, finalL2, residual float64
	for i, value := range current {
		if i&255 == 0 {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > math.MaxFloat32 {
			return nil, fmt.Errorf("%w: output index %d", ErrTensorDiffusionNumeric, i)
		}
		out.Data[i] = float32(value)
		finalMean += value
		finalL2 += value * value
		d := value - float64(src.Data[i])
		residual += d * d
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return &TensorDiffusionResult{Volume: out, Metadata: TensorDiffusionMetadata{
		Derived: true, Method: opts.Method, Domain: "derived-scalar-intensity-estimate",
		Boundary: "natural-absent-differences; replicated-Gaussian", Discretization: "orthant-average-variational",
		SourceSHA256: hex.EncodeToString(h.Sum(nil)), SourceReference: strings.Clone(opts.SourceReference),
		Dims: src.Dims, SpacingMM: src.SpacingMM, SigmaMM: opts.SigmaMM, RhoMM: opts.RhoMM,
		Alpha: opts.Alpha, Threshold: opts.Threshold, Exponent: p.exponent,
		TimeStep: p.dt, StabilityLimit: p.stability, TotalTime: p.dt * float64(p.iterations),
		IterationsCompleted: p.iterations, WorkersRequested: opts.Workers,
		ScratchBytes: p.scratch, WorkingBytes: p.working, StencilVisitBound: p.visits,
		InitialMean: initialMean / float64(p.n), FinalMean: finalMean / float64(p.n),
		InitialL2Squared: initialL2, FinalL2Squared: finalL2, ResidualRMS: math.Sqrt(residual / float64(p.n)),
	}}, nil
}

func admitTensorDiffusion(ctx context.Context, src *Volume, o TensorDiffusionOptions) (p tensorDiffusionPlan, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	bad := func(s string) error { return fmt.Errorf("%w: %s", ErrTensorDiffusionInput, s) }
	if src == nil {
		return p, bad("nil volume")
	}
	n, ok := voxelCount(src.Dims)
	if !ok || n != len(src.Data) {
		return p, bad("dimensions/data")
	}
	p.n = n
	if o.Method != CoherenceEnhancing && o.Method != EdgeEnhancing {
		return p, bad("method must be CED or EED")
	}
	if !(o.Alpha >= 1e-6 && o.Alpha <= 1) || !(o.Threshold >= 1e-24 && o.Threshold <= 1e24) {
		return p, bad("alpha/threshold")
	}
	p.iterations, p.exponent = o.Iterations, o.Exponent
	if p.iterations == 0 {
		p.iterations = 5
	}
	if p.exponent == 0 {
		p.exponent = 2
	}
	if p.iterations < 1 || p.iterations > 1000 || p.exponent < 1 || p.exponent > 8 || o.Workers < 0 || o.Workers > 64 {
		return p, bad("iterations/exponent/workers")
	}
	if len(o.SourceReference) > 4096 {
		return p, bad("source reference exceeds 4096 bytes")
	}
	check := func(v, def, hard, need int64, label string) error {
		if v < 0 || v > hard {
			return bad(label)
		}
		if v == 0 {
			v = def
		}
		if need > v {
			return fmt.Errorf("%w: %s requires %d > %d", ErrTensorDiffusionLimit, label, need, v)
		}
		return nil
	}
	if err = check(o.Limits.MaxVoxels, 1<<20, 1<<24, int64(n), "voxels"); err != nil {
		return
	}
	if err = check(int64(o.Limits.MaxIterations), 128, 1000, int64(p.iterations), "iterations"); err != nil {
		return
	}
	p.strides = [3]int{1, src.Dims[0], src.Dims[0] * src.Dims[1]}
	var invSum float64
	var kernelBytes, widths [2]int64
	for axis, h := range src.SpacingMM {
		if src.Dims[axis] > 1 {
			if !(h >= 1e-4 && h <= 1e4) {
				return p, bad("active spacing outside [1e-4,1e4] mm")
			}
			p.active = append(p.active, axis)
			p.inverse[axis] = 1 / h
			invSum += 1 / (h * h)
		} else if !(h >= 0 && h <= math.MaxFloat64) {
			return p, bad("inactive spacing")
		}
		for kind, scale := range [2]float64{o.SigmaMM[axis], o.RhoMM[axis]} {
			if !(scale >= 0 && scale <= 1e4) {
				return p, bad("Gaussian scale outside [0,1e4] mm")
			}
			if src.Dims[axis] == 1 || scale == 0 {
				continue
			}
			radius := math.Ceil(3 * scale / h)
			if radius > 64 {
				return p, fmt.Errorf("%w: Gaussian axis %d needs radius %g > 64", ErrTensorDiffusionLimit, axis, radius)
			}
			p.radii[kind][axis] = int(radius)
			widths[kind] += 2*int64(radius) + 1
			kernelBytes[kind] += 8 * (2*int64(radius) + 1)
		}
	}
	if len(p.active) < 2 {
		return p, bad("requires two or three active axes")
	}
	p.stability = 1 / (2 * invSum)
	p.dt = o.TimeStep
	if p.dt == 0 {
		p.dt = 0.9 * p.stability
	}
	if !(p.dt > 0 && p.dt <= p.stability) {
		return p, bad("time step exceeds physical stability limit")
	}
	p.workers = min(64, parallel.Workers(o.Workers))
	p.scratch = 80*int64(n) + kernelBytes[0] + kernelBytes[1]
	p.working = p.scratch + 4*int64(n) + int64(len(o.SourceReference))
	// At most 128 source/tensor reads for gradients, construction, eigen
	// conversion and the gathered update; separable smoothing reads are exact.
	p.visits = int64(n) * (int64(p.iterations)*(widths[0]+6*widths[1]+128) + 8)
	if err = check(o.Limits.MaxScratchBytes, 128<<20, 2<<30, p.scratch, "scratch bytes"); err != nil {
		return
	}
	if err = check(o.Limits.MaxWorkingBytes, 160<<20, 2<<30, p.working, "working bytes"); err != nil {
		return
	}
	if err = check(o.Limits.MaxStencilVisits, 2000000000, 50000000000, p.visits, "stencil visits"); err != nil {
		return
	}
	for i, value := range src.Data {
		if i&255 == 0 {
			if err = ctx.Err(); err != nil {
				return
			}
		}
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return p, bad("nonfinite input sample")
		}
		if math.Abs(float64(value)) > 1e30 {
			return p, fmt.Errorf("%w: input magnitude exceeds 1e30", ErrTensorDiffusionNumeric)
		}
	}
	return
}

func tensorComponent(a, b int) int {
	if a > b {
		a, b = b, a
	}
	return [3][3]int{{0, 1, 2}, {1, 3, 4}, {2, 4, 5}}[a][b]
}

// Each pass writes independent voxels, then copies back; scratch is shared
// sequentially by all components, never by concurrent workers.
func (p *tensorDiffusionPlan) smooth(ctx context.Context, dims [3]int, data, scratch []float64, kind int) error {
	for _, axis := range p.active {
		kernel := p.kernels[kind][axis]
		if len(kernel) == 0 {
			continue
		}
		stride, r := p.strides[axis], len(kernel)/2
		if err := p.forEachVoxel(ctx, func(i int) {
			c := (i / stride) % dims[axis]
			sum := 0.0
			for j, w := range kernel {
				neighbor := max(0, min(dims[axis]-1, c+j-r))
				sum += w * data[i+(neighbor-c)*stride]
			}
			scratch[i] = sum
		}); err != nil {
			return err
		}
		copy(data, scratch)
	}
	return ctx.Err()
}

// forEachVoxel runs fn for every voxel on up to p.workers workers, which
// claim contiguous runs of voxels rather than one at a time (#2025).
func (p *tensorDiffusionPlan) forEachVoxel(ctx context.Context, fn func(int)) error {
	return parallel.Run(ctx, p.n, p.workers, 0, func() func(int) { return fn })
}

func (p *tensorDiffusionPlan) gradient(dims [3]int, data []float64, i int) (g [3]float64) {
	for _, a := range p.active {
		c := (i / p.strides[a]) % dims[a]
		lo, hi := i, i
		if c > 0 {
			lo -= p.strides[a]
		}
		if c+1 < dims[a] {
			hi += p.strides[a]
		}
		g[a] = (data[hi] - data[lo]) * 0.5 * p.inverse[a]
	}
	return
}

// Gather the diagonal edge energy and adjoint centered cross terms. This is
// algebraically the all-orthant scatter, without worker write conflicts.
func (p *tensorDiffusionPlan) update(dims [3]int, u []float64, d [6][]float64, i int) (delta float64) {
	for _, a := range p.active {
		stride, ih := p.strides[a], p.inverse[a]
		c := (i / stride) % dims[a]
		component := tensorComponent(a, a)
		cross := func(j int) float64 {
			g := p.gradient(dims, u, j)
			value := 0.0
			for _, b := range p.active {
				if b != a {
					value += d[tensorComponent(a, b)][j] * g[b]
				}
			}
			return value
		}
		if c > 0 {
			j := i - stride
			delta += 0.5 * (d[component][i] + d[component][j]) * (u[j] - u[i]) * ih * ih
			delta -= 0.5 * ih * cross(j)
		} else {
			delta += 0.5 * ih * cross(i)
		}
		if c+1 < dims[a] {
			j := i + stride
			delta += 0.5 * (d[component][i] + d[component][j]) * (u[j] - u[i]) * ih * ih
			delta += 0.5 * ih * cross(j)
		} else {
			delta -= 0.5 * ih * cross(i)
		}
	}
	return
}

// Eigenvalue transfer from the public Weickert CED/EED family: CED depends
// on mu_max-mu_i; EED on mu_i-mu_min. Direct isotropic handling makes repeated
// eigenvalues independent of arbitrary eigenvectors.
func tensorDiffusivity(j [6]float64, active []int, method TensorDiffusionMethod, alpha, threshold float64, exponent int) (d [6]float64) {
	transfer := func(gap float64) float64 {
		e := 0.0
		if gap > 0 {
			r := threshold / gap
			x := 1.0
			for k := 0; k < exponent; k++ {
				x *= r
			}
			e = math.Exp(-x)
		}
		if method == CoherenceEnhancing {
			return alpha + (1-alpha)*e
		}
		return 1 - (1-alpha)*e
	}
	if alpha == 1 {
		for _, a := range active {
			d[tensorComponent(a, a)] = 1
		}
		return
	}
	if len(active) == 2 {
		a, b := active[0], active[1]
		x, y, z := j[tensorComponent(a, a)], j[tensorComponent(a, b)], j[tensorComponent(b, b)]
		gap := math.Hypot(x-z, 2*y)
		if gap == 0 {
			v := transfer(0)
			d[tensorComponent(a, a)], d[tensorComponent(b, b)] = v, v
			return
		}
		lo, hi := transfer(gap), transfer(0)
		if method == EdgeEnhancing {
			lo, hi = hi, lo
		}
		// Projectors using normalized traceless tensor avoid subtracting
		// nearly equal large eigenvalues or constructing tiny eigenvectors.
		average, difference := 0.5*(hi+lo), 0.5*(hi-lo)
		d[tensorComponent(a, a)] = average + difference*(x-z)/gap
		d[tensorComponent(b, b)] = average - difference*(x-z)/gap
		d[tensorComponent(a, b)] = difference * 2 * y / gap
		return
	}
	values, vectors := tensorEigen3(j)
	for k, mu := range values {
		gap := values[2] - mu
		if method == EdgeEnhancing {
			gap = mu - values[0]
		}
		weight := transfer(gap)
		for a := 0; a < 3; a++ {
			for b := a; b < 3; b++ {
				d[tensorComponent(a, b)] += weight * vectors[a][k] * vectors[b][k]
			}
		}
	}
	return
}

// Jacobi rotations derive eigenvectors of a real symmetric 3x3 matrix.
// Fixed sweeps bound work; relative stopping and Hypot avoid unstable angles.
func tensorEigen3(j [6]float64) (values [3]float64, vectors [3][3]float64) {
	a := [3][3]float64{{j[0], j[1], j[2]}, {j[1], j[3], j[4]}, {j[2], j[4], j[5]}}
	vectors = [3][3]float64{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}
	scale := math.Max(math.Abs(j[0])+math.Abs(j[3])+math.Abs(j[5]), 1e-300)
	for sweep := 0; sweep < 16; sweep++ {
		changed := false
		for _, pair := range [3][2]int{{0, 1}, {0, 2}, {1, 2}} {
			p, q := pair[0], pair[1]
			if math.Abs(a[p][q]) <= 1e-15*scale {
				continue
			}
			changed = true
			tau := (a[q][q] - a[p][p]) / (2 * a[p][q])
			t := math.Copysign(1, tau) / (math.Abs(tau) + math.Hypot(1, tau))
			c := 1 / math.Hypot(1, t)
			s := t * c
			app, aqq, apq := a[p][p], a[q][q], a[p][q]
			a[p][p] = app - t*apq
			a[q][q] = aqq + t*apq
			a[p][q] = 0
			a[q][p] = 0
			for r := 0; r < 3; r++ {
				if r != p && r != q {
					x, y := a[r][p], a[r][q]
					a[r][p] = c*x - s*y
					a[p][r] = a[r][p]
					a[r][q] = s*x + c*y
					a[q][r] = a[r][q]
				}
				x, y := vectors[r][p], vectors[r][q]
				vectors[r][p] = c*x - s*y
				vectors[r][q] = s*x + c*y
			}
		}
		if !changed {
			break
		}
	}
	for k := 0; k < 3; k++ {
		values[k] = math.Max(0, a[k][k])
	}
	for k := 0; k < 2; k++ {
		for l := k + 1; l < 3; l++ {
			if values[l] < values[k] {
				values[l], values[k] = values[k], values[l]
				for r := 0; r < 3; r++ {
					vectors[r][l], vectors[r][k] = vectors[r][k], vectors[r][l]
				}
			}
		}
	}
	return
}
