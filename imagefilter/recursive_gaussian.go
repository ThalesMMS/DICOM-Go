package imagefilter

import (
	"context"
	"fmt"
	"math"
	"math/cmplx"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// Recursive (IIR) Gaussian filtering and Gaussian derivatives of dense
// volumes and 2-D images.
//
// Each axis is filtered by a fourth-order recursive filter in the style of
// R. Deriche ("Recursively implementing the Gaussian and its derivatives",
// INRIA RR-1893, 1993): the sampled kernel is approximated on each side of
// the origin by a sum of two exponentially damped cosines,
//
//	h(t) = Σ_{i=1,2} (a_i cos(ω_i t/σ) + b_i sin(ω_i t/σ)) exp(λ_i t/σ),  t ≥ 0,
//
// mirrored (orders 0 and 2) or mirrored and negated (order 1) for t < 0.
// Smoothing (order 0) uses Deriche's Gaussian parameters. The derivatives use
// the parameters fitted by G. Farnebäck and C.-F. Westin ("Improving
// Deriche-style recursive Gaussian filters", J. Math. Imaging Vis. 26, 2006,
// Table 3), the set ITK's RecursiveGaussianImageFilter uses for every order;
// Deriche's published second-derivative parameters are several times less
// accurate. This implementation was written from those papers; it is not
// derived from ITK's code. Each damped cosine is run as a complex first-order
// recursion y[k] = c·x[k] + z·y[k−1] with the pole z = exp((λ+iω)/σ), a causal
// pass for the samples at and before k and an anticausal pass for the
// samples after it, and the output is the real part of the sum. The cost is
// therefore a fixed number of operations per sample and pass, independent
// of σ.
//
// Normalization is computed in closed form from the geometric series of the
// poles, on the infinite sampled kernel: order 0 sums to 1, so a constant is
// preserved; order 1 answers a unit-slope ramp with exactly 1; order 2 has
// zero sum (a small multiple of the Farnebäck–Westin Gaussian, which shares
// its poles, cancels the fitted kernel's residual sum) and answers the
// parabola t² with exactly 2. Derivatives are then divided by the sample
// spacing, so they are in intensity units per millimetre (per mm² for second
// derivatives). With NormalizeAcrossScale, a derivative of order k along an
// axis is multiplied by σ^k (σ in millimetres along that axis), which makes
// the responses of structures of different sizes comparable across scales
// (T. Lindeberg's scale-space normalization), as ITK's NormalizeAcrossScale
// option does.
//
// Borders replicate the edge sample to infinity (zero-flux, Neumann-type
// boundary): the causal recursion starts in the steady state of a constant
// input equal to the first sample and the anticausal one in that of the last
// sample, which is exact for the infinite filter. A constant is therefore
// preserved up to the edge, but a ramp bends within about 3σ of the edge,
// where its first derivative falls to about half the slope.
//
// Accuracy, against the sampled Gaussian and its derivatives, for σ from 1 to
// 32 samples: the smoothing impulse response is within 0.05% of the kernel
// peak and its variance is 0.996σ²; first derivatives are within 0.4% and
// second derivatives within 1.5% of their peaks. The Farnebäck–Westin
// kernels are narrower in their moments (they act on smooth data like a
// Gaussian of variance 0.938σ²), which is also why they are not used for
// smoothing; derivative results therefore match ITK's (for example the
// scale-normalized ridge Hxx of −0.3547 where the continuous value is
// −0.3536), while smoothing is closer to a true Gaussian than ITK's. For σ
// below one sample the Gaussian is undersampled and the approximation, like
// any sampled Gaussian, degrades; such axes are still filtered.

// RecursiveGaussianOptions configures the recursive Gaussian filters.
type RecursiveGaussianOptions struct {
	// SigmaMM is the Gaussian standard deviation in millimetres along x, y
	// and z (see IsotropicSigma). It is converted to samples with the volume
	// spacing, so anisotropic voxels are smoothed equally in patient space.
	// A zero σ leaves that axis unsmoothed and is rejected when a derivative
	// along that axis is requested. Axes of a single sample (the z axis of a
	// 2-D image) are never filtered: smoothing leaves them as they are and
	// their derivatives are zero.
	SigmaMM [3]float64
	// NormalizeAcrossScale multiplies each derivative of order k along an
	// axis by σ^k (σ of that axis in millimetres), giving the
	// scale-normalized derivatives of scale-space theory. It does not affect
	// smoothing.
	NormalizeAcrossScale bool
	// Workers bounds the goroutines used (0 selects the shared concurrency budget). The result
	// does not depend on the worker count.
	Workers int
}

// IsotropicSigma returns the same σ (mm) for x, y and z.
func IsotropicSigma(mm float64) [3]float64 { return [3]float64{mm, mm, mm} }

// Hessian holds the six distinct second derivatives of a volume, in the
// order of ITK's symmetric second-rank tensor (xx, xy, xz, yy, yz, zz). For a
// 2-D image XZ, YZ and ZZ are zero.
type Hessian struct {
	XX, XY, XZ, YY, YZ, ZZ *Volume
}

// SmoothRecursiveGaussian returns src smoothed by a Gaussian of
// opts.SigmaMM along each axis (ITK's SmoothingRecursiveGaussianImageFilter).
func SmoothRecursiveGaussian(ctx context.Context, src *Volume, opts RecursiveGaussianOptions) (*Volume, error) {
	return RecursiveGaussian(ctx, src, [3]int{}, opts)
}

// RecursiveGaussian returns src convolved along x, y and z with the Gaussian
// derivative of the given order (0, 1 or 2) along each axis; orders {1, 0, 0}
// is ∂/∂x, {1, 1, 0} is ∂²/∂x∂y and {0, 0, 0} is plain smoothing. ctx is
// checked between groups of lines; on cancellation ctx.Err() is returned.
func RecursiveGaussian(ctx context.Context, src *Volume, orders [3]int, opts RecursiveGaussianOptions) (*Volume, error) {
	var out *Volume
	err := runRecursiveGaussian(ctx, src, [][3]int{orders}, opts, func(p *gaussianPlan, _ int, data []float32) error {
		out = p.volume(data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GradientRecursiveGaussian returns the first derivatives ∂/∂x, ∂/∂y and ∂/∂z
// of src at scale opts.SigmaMM, in intensity units per millimetre (times σ
// with NormalizeAcrossScale).
func GradientRecursiveGaussian(ctx context.Context, src *Volume, opts RecursiveGaussianOptions) ([3]*Volume, error) {
	var out [3]*Volume
	err := runRecursiveGaussian(ctx, src, [][3]int{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}, opts, func(p *gaussianPlan, i int, data []float32) error {
		out[i] = p.volume(data)
		return nil
	})
	if err != nil {
		return [3]*Volume{}, err
	}
	return out, nil
}

// GradientMagnitudeRecursiveGaussian returns the magnitude of the gradient
// of src at scale opts.SigmaMM (ITK's GradientMagnitudeRecursiveGaussian
// ImageFilter).
func GradientMagnitudeRecursiveGaussian(ctx context.Context, src *Volume, opts RecursiveGaussianOptions) (*Volume, error) {
	var acc []float32
	err := runRecursiveGaussian(ctx, src, [][3]int{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}, opts, func(p *gaussianPlan, _ int, data []float32) error {
		if acc == nil {
			return p.parallelSpan(len(data), func(lo, hi int) {
				for i, v := range data[lo:hi] {
					data[lo+i] = v * v
				}
			}, func() { acc = data })
		}
		defer p.pool.put(data)
		return p.parallelSpan(len(data), func(lo, hi int) {
			for i, v := range data[lo:hi] {
				acc[lo+i] += v * v
			}
		}, nil)
	}, func(p *gaussianPlan) error {
		return p.parallelSpan(len(acc), func(lo, hi int) {
			for i, v := range acc[lo:hi] {
				acc[lo+i] = float32(math.Sqrt(float64(v)))
			}
		}, nil)
	})
	if err != nil {
		return nil, err
	}
	return &Volume{Dims: src.Dims, SpacingMM: src.SpacingMM, Data: acc}, nil
}

// LaplacianRecursiveGaussian returns the Laplacian of Gaussian of src,
// ∂²/∂x² + ∂²/∂y² + ∂²/∂z² at scale opts.SigmaMM (ITK's
// LaplacianRecursiveGaussianImageFilter), in intensity units per mm² (times
// σ² per axis with NormalizeAcrossScale).
func LaplacianRecursiveGaussian(ctx context.Context, src *Volume, opts RecursiveGaussianOptions) (*Volume, error) {
	var acc []float32
	err := runRecursiveGaussian(ctx, src, [][3]int{{2, 0, 0}, {0, 2, 0}, {0, 0, 2}}, opts, func(p *gaussianPlan, _ int, data []float32) error {
		if acc == nil {
			acc = data
			return nil
		}
		defer p.pool.put(data)
		return p.parallelSpan(len(data), func(lo, hi int) {
			for i, v := range data[lo:hi] {
				acc[lo+i] += v
			}
		}, nil)
	})
	if err != nil {
		return nil, err
	}
	return &Volume{Dims: src.Dims, SpacingMM: src.SpacingMM, Data: acc}, nil
}

// HessianRecursiveGaussian returns the six second derivatives of src at
// scale opts.SigmaMM (ITK's HessianRecursiveGaussianImageFilter). The
// smoothing passes shared by several components are computed once (15 line
// passes instead of 18).
func HessianRecursiveGaussian(ctx context.Context, src *Volume, opts RecursiveGaussianOptions) (*Hessian, error) {
	var out [6]*Volume
	specs := [][3]int{{2, 0, 0}, {1, 1, 0}, {1, 0, 1}, {0, 2, 0}, {0, 1, 1}, {0, 0, 2}}
	err := runRecursiveGaussian(ctx, src, specs, opts, func(p *gaussianPlan, i int, data []float32) error {
		out[i] = p.volume(data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Hessian{XX: out[0], XY: out[1], XZ: out[2], YY: out[3], YZ: out[4], ZZ: out[5]}, nil
}

// gaussianPlan is one filtering request: the validated volume, the kernels
// per axis and order, and the buffers shared by its passes.
type gaussianPlan struct {
	src     *Volume
	n       int
	kernels [3][3]*recursiveKernel // [axis][order]; nil when the axis is not filtered
	workers int
	pool    bufferPool
	scratch scratchPool[gaussianLaneScratch]
	ctx     context.Context // the request context, for the helpers run from emit
}

func (p *gaussianPlan) volume(data []float32) *Volume {
	return &Volume{Dims: p.src.Dims, SpacingMM: p.src.SpacingMM, Data: data}
}

// runRecursiveGaussian computes one output per entry of specs (derivative
// orders along x, y, z) and hands each to emit, which takes ownership of the
// buffer. The passes run along z, then y, then x, and a z or y pass shared by
// several outputs is computed once. The optional finish runs after the last
// output.
func runRecursiveGaussian(ctx context.Context, src *Volume, specs [][3]int, opts RecursiveGaussianOptions,
	emit func(p *gaussianPlan, i int, data []float32) error, finish ...func(p *gaussianPlan) error,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p, err := newGaussianPlanContext(ctx, src, specs, opts)
	if err != nil {
		return err
	}
	p.ctx = ctx
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make([]bool, len(specs))
	for i, spec := range specs {
		if done[i] {
			continue
		}
		if p.isZero(spec) {
			done[i] = true
			if err := emit(p, i, make([]float32, p.n)); err != nil {
				return err
			}
			continue
		}
		z, err := p.pass(ctx, node{data: src.Data}, 2, spec[2], false)
		if err != nil {
			return err
		}
		for j := i; j < len(specs); j++ {
			if done[j] || specs[j][2] != spec[2] || p.isZero(specs[j]) {
				continue
			}
			lastY := !p.needsAgain(specs, done, j)
			consumeZ := z.owned && lastY
			y, err := p.pass(ctx, z, 1, specs[j][1], consumeZ)
			if consumeZ {
				// The final y family takes the private z buffer, including an
				// identity y pass. No other consumer may release or read it.
				z = node{}
			}
			if err != nil {
				return err
			}
			if y.owned && lastY {
				// The last y pass from z: its buffer can serve an x pass.
				p.release(z)
				z = node{}
			}
			for k := j; k < len(specs); k++ {
				if done[k] || specs[k][2] != spec[2] || specs[k][1] != specs[j][1] || p.isZero(specs[k]) {
					continue
				}
				consumeY := y.owned && !p.needsXAgain(specs, done, k)
				x, err := p.pass(ctx, y, 0, specs[k][0], consumeY)
				if consumeY {
					y = node{}
				}
				if err != nil {
					return err
				}
				if !x.owned {
					buf := p.pool.get()
					copy(buf.data, x.data)
					x = buf
				}
				done[k] = true
				if err := emit(p, k, x.data); err != nil {
					return err
				}
			}
			p.release(y)
		}
		p.release(z)
	}
	for _, f := range finish {
		if err := f(p); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func newGaussianPlan(src *Volume, specs [][3]int, opts RecursiveGaussianOptions) (*gaussianPlan, error) {
	return newGaussianPlanContext(context.Background(), src, specs, opts)
}

func newGaussianPlanContext(ctx context.Context, src *Volume, specs [][3]int, opts RecursiveGaussianOptions) (*gaussianPlan, error) {
	if err := validateGaussianSource(ctx, src); err != nil {
		return nil, err
	}
	p := &gaussianPlan{src: src, n: len(src.Data), workers: opts.Workers}
	if p.workers <= 0 {
		p.workers = parallel.DefaultWorkers()
	}
	p.pool.n = p.n
	for axis, sigma := range opts.SigmaMM {
		if !(sigma >= 0 && sigma <= math.MaxFloat64) {
			return nil, fmt.Errorf("%w: sigma %v mm along axis %d", ErrInvalidVolume, sigma, axis)
		}
	}
	for _, spec := range specs {
		for axis, order := range spec {
			if order < 0 || order > 2 {
				return nil, fmt.Errorf("%w: derivative order %d along axis %d (want 0, 1 or 2)", ErrInvalidVolume, order, axis)
			}
			sigma := opts.SigmaMM[axis]
			if src.Dims[axis] == 1 || (order == 0 && sigma == 0) {
				continue
			}
			if sigma == 0 {
				return nil, fmt.Errorf("%w: order-%d derivative along axis %d needs a positive sigma", ErrInvalidVolume, order, axis)
			}
			if p.kernels[axis][order] != nil {
				continue
			}
			scale := math.Pow(src.SpacingMM[axis], -float64(order))
			if opts.NormalizeAcrossScale {
				scale *= math.Pow(sigma, float64(order))
			}
			k, err := newRecursiveKernel(sigma/src.SpacingMM[axis], order, scale)
			if err != nil {
				return nil, fmt.Errorf("%w: axis %d: %v", ErrInvalidVolume, axis, err)
			}
			p.kernels[axis][order] = k
		}
	}
	return p, nil
}

// needsAgain reports whether a spec after j, not yet computed and not zero,
// shares the z order of specs[j] but needs another y order, so the z pass
// result must be kept.
func (p *gaussianPlan) needsAgain(specs [][3]int, done []bool, j int) bool {
	for k := j + 1; k < len(specs); k++ {
		if !done[k] && !p.isZero(specs[k]) && specs[k][2] == specs[j][2] && specs[k][1] != specs[j][1] {
			return true
		}
	}
	return false
}

// needsXAgain reports another x output that still needs this y family.
func (p *gaussianPlan) needsXAgain(specs [][3]int, done []bool, k int) bool {
	for i := k + 1; i < len(specs); i++ {
		if !done[i] && !p.isZero(specs[i]) && specs[i][2] == specs[k][2] && specs[i][1] == specs[k][1] {
			return true
		}
	}
	return false
}

// isZero reports whether a derivative is identically zero: any derivative
// along an axis of a single sample.
func (p *gaussianPlan) isZero(spec [3]int) bool {
	for axis, order := range spec {
		if order > 0 && p.src.Dims[axis] == 1 {
			return true
		}
	}
	return false
}

// node is an intermediate volume; owned buffers came from the pool, the
// others alias the source or a parent node.
type node struct {
	data  []float32
	owned bool
}

// release returns an owned node's buffer to the pool. A consuming pass
// transfers ownership; its caller invalidates the previous owner's node.
func (p *gaussianPlan) release(n node) {
	if n.owned {
		p.pool.put(n.data)
	}
}

// pass filters every line of in along axis with the kernel of the given
// order. consume transfers a private node's ownership to the returned node;
// the caller must stop using in. Otherwise in stays borrowed, including an
// identity pass, and a filtering pass obtains a distinct private destination.
func (p *gaussianPlan) pass(ctx context.Context, in node, axis, order int, consume bool) (node, error) {
	k := p.kernels[axis][order]
	if k == nil {
		if consume && in.owned {
			return in, nil
		}
		return node{data: in.data}, nil
	}
	out := in
	if !consume || !in.owned {
		out = p.pool.get()
	}
	dims := p.src.Dims
	nx, ny, nz := dims[0], dims[1], dims[2]
	var (
		length, step, laneStride, lanes int
		groups, groupStride             int
	)
	// Lines along the axis are filtered laneWidth at a time, so the
	// independent recursions interleave and the memory accesses of each step
	// stay within a few cache lines.
	switch axis {
	case 0: // lanes are consecutive rows
		length, step, laneStride, lanes = nx, 1, nx, ny*nz
		groups, groupStride = 1, 0
	case 1: // lanes are consecutive columns of one slice
		length, step, laneStride, lanes = ny, nx, 1, nx
		groups, groupStride = nz, nx*ny
	default: // lanes are consecutive voxels of a slice
		length, step, laneStride, lanes = nz, nx*ny, 1, nx*ny
		groups, groupStride = 1, 0
	}
	chunks := (lanes + laneWidth - 1) / laneWidth
	count := groups * chunks
	scratchSamples := length * min(laneWidth, lanes)
	// The shared helper budget is a ceiling, not a reservation. Run may grant
	// fewer helpers; only workers that obtain work allocate their line scratch.
	workers := max(1, min(p.workers, count, parallel.DefaultWorkers()))
	for len(p.scratch.items) < workers {
		p.scratch.items = append(p.scratch.items, new(gaussianLaneScratch))
	}
	p.scratch.next.Store(0)
	err := runParallel(ctx, count, workers, func() func(int) {
		s := p.scratch.items[p.scratch.next.Add(1)-1]
		if len(s.tmp) < scratchSamples {
			// A long axis may have only one lane group. Its large scratch
			// belongs only to workers that claim that pass, not every worker
			// used by the shorter axes of an elongated volume.
			s.tmp = make([]float64, scratchSamples)
		}
		if s.state == nil {
			s.state = make([]float64, 4*laneWidth)
		}
		return func(item int) {
			g, c := item/chunks, item%chunks
			first := c * laneWidth
			count := min(laneWidth, lanes-first)
			base := g*groupStride + first*laneStride
			k.filterLanesWindowContext(ctx, out.data, laneWindow{lo: 0, hi: length, base: base, step: step, laneStride: laneStride}, in.data, base, step, laneStride, length, count, s.tmp, s.state)
		}
	})
	if err != nil {
		p.pool.put(out.data)
		return node{}, err
	}
	return out, nil
}

type gaussianLaneScratch struct {
	tmp, state []float64
}

// laneWidth is the number of lines filtered together.
const laneWidth = 32

// parallelSpan runs fn over [0, n) in chunks on the plan's workers, then
// after (if any) once every chunk is done.
func (p *gaussianPlan) parallelSpan(n int, fn func(lo, hi int), after func()) error {
	const chunk = 1 << 16
	count := (n + chunk - 1) / chunk
	err := runParallel(p.ctx, count, p.workers, func() func(int) {
		return func(i int) { fn(i*chunk, min(n, (i+1)*chunk)) }
	})
	if err != nil {
		return err
	}
	if after != nil {
		after()
	}
	return nil
}

// bufferPool recycles the volume-sized buffers of one request.
type bufferPool struct {
	n    int
	free [][]float32
}

func (b *bufferPool) get() node {
	if last := len(b.free) - 1; last >= 0 {
		buf := b.free[last]
		b.free = b.free[:last]
		return node{data: buf, owned: true}
	}
	return node{data: make([]float32, b.n), owned: true}
}

func (b *bufferPool) put(buf []float32) {
	if len(buf) == b.n {
		b.free = append(b.free, buf)
	}
}

// iirFit is one fitted kernel: two damped cosines of decay λ and frequency ω
// (per σ) with cosine and sine weights a and b.
type iirFit struct {
	lambda, omega, a, b [2]float64
}

var (
	// dericheGaussian is Deriche's (1993) fourth-order Gaussian.
	dericheGaussian = iirFit{
		lambda: [2]float64{-1.783, -1.723}, omega: [2]float64{0.6318, 1.997},
		a: [2]float64{1.680, -0.6803}, b: [2]float64{3.735, -0.2598},
	}
	// Farnebäck–Westin (2006) Table 3: the Gaussian and its first and second
	// derivatives with shared poles.
	fwLambda   = [2]float64{-1.3932, -1.3732}
	fwOmega    = [2]float64{0.6681, 2.0787}
	fwGaussian = iirFit{lambda: fwLambda, omega: fwOmega, a: [2]float64{1.3530, -0.3531}, b: [2]float64{1.8151, 0.0902}}
	fwFirst    = iirFit{lambda: fwLambda, omega: fwOmega, a: [2]float64{-0.6724, 0.6724}, b: [2]float64{-3.4327, 0.6100}}
	fwSecond   = iirFit{lambda: fwLambda, omega: fwOmega, a: [2]float64{-1.3563, 0.3446}, b: [2]float64{5.2318, -2.2355}}
)

// gains returns the complex weights c with a cos + b sin = Re(c e^{iωt}).
func (f iirFit) gains() [2]complex128 {
	return [2]complex128{complex(f.a[0], -f.b[0]), complex(f.a[1], -f.b[1])}
}

// recursiveKernel is a normalized recursive Gaussian derivative along one
// axis, as two complex first-order recursions. The kernel weight of the
// sample n positions before the output is Re Σ c_i z_i^n (n ≥ 0) and that of
// the sample n positions after it is Re Σ d_i z_i^n (n ≥ 1), d = ±c.
type recursiveKernel struct {
	z, c, d [2]complex128
	// cInit and dInit are the causal and anticausal states for a constant
	// unit input extending to infinity: c/(1−z) and d·z/(1−z).
	cInit, dInit [2]complex128
}

// newRecursiveKernel builds the kernel of a Gaussian derivative of the given
// order with σ in samples, its output multiplied by scale.
func newRecursiveKernel(sigma float64, order int, scale float64) (*recursiveKernel, error) {
	if !(sigma > 0) || math.IsInf(sigma, 0) {
		return nil, fmt.Errorf("sigma %v samples", sigma)
	}
	fit := [3]iirFit{dericheGaussian, fwFirst, fwSecond}
	if order < 0 || order > 2 {
		return nil, fmt.Errorf("order %d", order)
	}
	k := &recursiveKernel{}
	for i := range k.z {
		k.z[i] = cmplx.Exp(complex(fit[order].lambda[i], fit[order].omega[i]) / complex(sigma, 0))
	}
	// Moments of the one-sided kernel Re Σ g_i z_i^n over n ≥ 0.
	moment := func(g [2]complex128, m int) float64 {
		var s complex128
		for i, z := range k.z {
			u := 1 - z
			switch m {
			case 0:
				s += g[i] / u
			case 1:
				s += g[i] * z / (u * u)
			default:
				s += g[i] * z * (1 + z) / (u * u * u)
			}
		}
		return real(s)
	}
	origin := func(g [2]complex128) float64 { return real(g[0] + g[1]) }
	// sum is the sum of the two-sided symmetric kernel.
	sum := func(g [2]complex128) float64 { return 2*moment(g, 0) - origin(g) }

	var g [2]complex128
	sign := complex(1, 0)
	var norm float64
	switch order {
	case 0:
		g = dericheGaussian.gains()
		norm = sum(g)
	case 1:
		// Antisymmetric: the ramp response Σ h(n)(−n) + Σ −h(n)(+n) is
		// −2·Σ n h(n); the origin weight a_1 + a_2 is zero.
		g = fwFirst.gains()
		sign = -1
		norm = -2 * moment(g, 1)
	default:
		// Cancel the residual sum with the Gaussian of the same poles, then
		// make the response to n² (2·Σ n² h(n)) equal to 2.
		g2, g0 := fwSecond.gains(), fwGaussian.gains()
		beta := -sum(g2) / sum(g0)
		for i := range g {
			g[i] = g2[i] + complex(beta, 0)*g0[i]
		}
		norm = moment(g, 2)
	}
	if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
		return nil, fmt.Errorf("degenerate kernel for sigma %v samples", sigma)
	}
	factor := complex(scale/norm, 0)
	for i, z := range k.z {
		k.c[i] = g[i] * factor
		k.d[i] = sign * k.c[i]
		k.cInit[i] = k.c[i] / (1 - z)
		k.dInit[i] = k.d[i] * z / (1 - z)
	}
	return k, nil
}

// filterLanes filters count lines of length samples. Line l's sample j is
// src[base + l*laneStride + j*step]; the result goes to the same index of
// dst. dst may be exactly src: the anticausal pass retains the next original
// sample before writing, and each lane has disjoint indices. Other overlaps
// are unsupported. tmp holds length*count values and state 4*count.
func (k *recursiveKernel) filterLanes(dst, src []float32, base, step, laneStride, length, count int, tmp, state []float64) {
	k.filterLanesWindow(dst, laneWindow{base: base, step: step, laneStride: laneStride, lo: 0, hi: length},
		src, base, step, laneStride, length, count, tmp, state)
}

// laneWindow selects the outputs filterLanesWindow stores: samples j in
// [lo, hi) of each line, sample j of line l going to
// dst[base + l*laneStride + (j−lo)*step].
type laneWindow struct {
	base, step, laneStride int
	lo, hi                 int
}

// filterLanesWindow filters count lines of length samples like filterLanes
// but stores only the samples of the window, at the window's own indices;
// the recursions still run over the whole line, so the stored values are
// those of the whole-line filter. The causal pass stops at the end of the
// window. tmp holds (hi−lo)*count values and state 4*count. Direct partial or
// compact-window callers must not alias src; filterLanes uses the full-line,
// same-index exception described above.
func (k *recursiveKernel) filterLanesWindow(dst []float32, w laneWindow, src []float32, base, step, laneStride, length, count int, tmp, state []float64) {
	k.filterLanesWindowContext(nil, dst, w, src, base, step, laneStride, length, count, tmp, state)
}

func (k *recursiveKernel) filterLanesWindowContext(ctx context.Context, dst []float32, w laneWindow, src []float32, base, step, laneStride, length, count int, tmp, state []float64) {
	z1r, z1i, z2r, z2i := real(k.z[0]), imag(k.z[0]), real(k.z[1]), imag(k.z[1])
	c1r, c1i, c2r, c2i := real(k.c[0]), imag(k.c[0]), real(k.c[1]), imag(k.c[1])
	st := state[:4*count]
	lo, hi := w.lo, w.hi
	tmp = tmp[:(hi-lo)*count]

	// Causal pass: y_i[j] = c_i x[j] + z_i y_i[j−1], from the steady state
	// of the first sample replicated before the line.
	for l := 0; l < count; l++ {
		x := complex(float64(src[base+l*laneStride]), 0)
		y1, y2 := k.cInit[0]*x, k.cInit[1]*x
		st[4*l], st[4*l+1], st[4*l+2], st[4*l+3] = real(y1), imag(y1), real(y2), imag(y2)
	}
	for j := 0; j < hi; j++ {
		if ctx != nil && j%1024 == 0 && ctx.Err() != nil {
			return
		}
		row := base + j*step
		if j < lo {
			for l := 0; l < count; l++ {
				x := float64(src[row+l*laneStride])
				s := st[4*l : 4*l+4 : 4*l+4]
				r1 := c1r*x + z1r*s[0] - z1i*s[1]
				i1 := c1i*x + z1r*s[1] + z1i*s[0]
				r2 := c2r*x + z2r*s[2] - z2i*s[3]
				i2 := c2i*x + z2r*s[3] + z2i*s[2]
				s[0], s[1], s[2], s[3] = r1, i1, r2, i2
			}
			continue
		}
		out := tmp[(j-lo)*count : (j-lo+1)*count]
		for l := range out {
			x := float64(src[row+l*laneStride])
			s := st[4*l : 4*l+4 : 4*l+4]
			r1 := c1r*x + z1r*s[0] - z1i*s[1]
			i1 := c1i*x + z1r*s[1] + z1i*s[0]
			r2 := c2r*x + z2r*s[2] - z2i*s[3]
			i2 := c2i*x + z2r*s[3] + z2i*s[2]
			s[0], s[1], s[2], s[3] = r1, i1, r2, i2
			out[l] = r1 + r2
		}
	}

	// Anticausal pass: w_i[j] = z_i (d_i x[j+1] + w_i[j+1]), from the steady
	// state of the last sample replicated after the line.
	d1r, d1i, d2r, d2i := real(k.d[0]), imag(k.d[0]), real(k.d[1]), imag(k.d[1])
	last := base + (length-1)*step
	var nextOriginal [laneWidth]float64
	for l := 0; l < count; l++ {
		nextOriginal[l] = float64(src[last+l*laneStride])
		x := complex(nextOriginal[l], 0)
		w1, w2 := k.dInit[0]*x, k.dInit[1]*x
		st[4*l], st[4*l+1], st[4*l+2], st[4*l+3] = real(w1), imag(w1), real(w2), imag(w2)
		if length-1 < hi {
			dst[w.base+l*w.laneStride+(length-1-lo)*w.step] = float32(tmp[(length-1-lo)*count+l] + real(w1) + real(w2))
		}
	}
	for j := length - 2; j >= lo; j-- {
		if ctx != nil && j%1024 == 0 && ctx.Err() != nil {
			return
		}
		current := base + j*step
		if j >= hi {
			for l := 0; l < count; l++ {
				x := nextOriginal[l]
				nextOriginal[l] = float64(src[current+l*laneStride])
				s := st[4*l : 4*l+4 : 4*l+4]
				u1r, u1i := d1r*x+s[0], d1i*x+s[1]
				u2r, u2i := d2r*x+s[2], d2i*x+s[3]
				s[0], s[1] = z1r*u1r-z1i*u1i, z1r*u1i+z1i*u1r
				s[2], s[3] = z2r*u2r-z2i*u2i, z2r*u2i+z2i*u2r
			}
			continue
		}
		row := w.base + (j-lo)*w.step
		in := tmp[(j-lo)*count : (j-lo+1)*count]
		for l := range in {
			x := nextOriginal[l]
			// Read the original sample before storing this output. On a full
			// same-index pass this is the next iteration's x[j+1], even when
			// dst and src are the same private backing.
			nextOriginal[l] = float64(src[current+l*laneStride])
			s := st[4*l : 4*l+4 : 4*l+4]
			u1r, u1i := d1r*x+s[0], d1i*x+s[1]
			u2r, u2i := d2r*x+s[2], d2i*x+s[3]
			r1, i1 := z1r*u1r-z1i*u1i, z1r*u1i+z1i*u1r
			r2, i2 := z2r*u2r-z2i*u2i, z2r*u2i+z2i*u2r
			s[0], s[1], s[2], s[3] = r1, i1, r2, i2
			dst[row+l*w.laneStride] = float32(in[l] + r1 + r2)
		}
	}
}

// Context-aware equivalent of Volume.Validate; numerical/domain checks remain
// identical for existing Gaussian entry points.
func validateGaussianSource(ctx context.Context, src *Volume) error {
	if src == nil {
		return fmt.Errorf("%w: nil volume", ErrInvalidVolume)
	}
	n, ok := voxelCount(src.Dims)
	if !ok || len(src.Data) != n {
		return fmt.Errorf("%w: %d samples for dimensions %v", ErrInvalidVolume, len(src.Data), src.Dims)
	}
	for a, h := range src.SpacingMM {
		if src.Dims[a] > 1 && !(h > 0 && h <= math.MaxFloat64) {
			return fmt.Errorf("%w: spacing %v mm along axis %d", ErrInvalidVolume, h, a)
		}
	}
	for i, v := range src.Data {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if v != v || math.IsInf(float64(v), 0) {
			return fmt.Errorf("%w: non-finite sample at index %d", ErrInvalidVolume, i)
		}
	}
	return ctx.Err()
}
