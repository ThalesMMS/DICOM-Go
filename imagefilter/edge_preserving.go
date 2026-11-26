package imagefilter

// Edge-preserving denoising and sharpening of modality values (#1103).
//
// GradientAnisotropicDiffusion is Perona–Malik diffusion with the
// conductance of ITK's GradientNDAnisotropicDiffusionFunction (ITK
// 35b196e13c, Modules/Filtering/AnisotropicSmoothing, Apache-2.0, Copyright
// NumFOCUS): along each axis the flux through the half-sample faces is
// weighted by exp(-|∇I|²/K), |∇I|² using the one-sided difference across
// the face and the average of the centred transverse differences on both
// sides, and K = 2·C²·mean(|∇I|²) is rescaled every iteration from the
// average squared gradient, so the conductance C is relative to the image's
// own gradients. Modifications: the flux divides by the spacing twice, so the
// time step is in mm² and the explicit scheme is stable below
// 1/(2·Σ 1/hᵢ²) whatever the voxel size (ITK divides once and limits the
// step by the smallest spacing); borders are zero-flux; the default step is
// 90% of that bound; the work is parallel and cancellable.
//
// CurvatureAnisotropicDiffusion is the modified curvature diffusion equation
// (MCDE, Whitaker and Xue 2001) of ITK's CurvatureNDAnisotropicDiffusionFunction
// (same revision and licence): the update is |∇I| times the divergence of
// c(|∇I|)·∇I/|∇I|, with the same conductance c and K, and the gradient
// magnitude taken upwind in the direction of the speed. It keeps edges like
// Perona–Malik but does not sharpen noise into spurious steps. Modifications
// are those of the gradient variant: the divergence divides by the spacing,
// so the time step is in mm², and borders replicate the edge sample (zero
// normal derivative). The explicit MCDE step diverges at the Laplacian
// bound once the conductance is high, so its bound is half of it,
// 1/(4·Σ 1/hᵢ²), which is ITK's 1/2^(N+1) at unit spacing.
//
// Bilateral is the brute-force bilateral filter (Tomasi and Manduchi 1998)
// with a Gaussian domain kernel of σ in mm per axis truncated at 2.5σ and a
// Gaussian range kernel on modality values, written from the definition.
// UnsharpMask adds Amount times the difference between the image and its
// Gaussian (the recursive Gaussian of this package), optionally only where
// that difference exceeds Threshold.

import (
	"context"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// AnisotropicDiffusionOptions configures GradientAnisotropicDiffusion.
type AnisotropicDiffusionOptions struct {
	// Iterations is the number of explicit steps; 5 when zero.
	Iterations int
	// TimeStep is the step in mm² per iteration; zero selects 90% of the
	// stability bound, 1/(2·Σ 1/hᵢ²) for GradientAnisotropicDiffusion and
	// half of that for CurvatureAnisotropicDiffusion. Larger values are
	// rejected.
	TimeStep float64
	// Conductance scales the edge threshold relative to the root mean
	// square gradient: smaller values preserve weaker edges. 1 when zero.
	// The estimate excludes outer samples on axes longer than two. When
	// that estimate vanishes, it includes the replicated boundary gradients.
	Conductance float64
	// Workers bounds the goroutines (0 selects the shared concurrency budget).
	Workers int
	// MaxWorkingBytes bounds the scratch beyond the output (0 selects
	// DefaultSlabWorkingBytes). A volume that fits is updated through one
	// volume-sized buffer; a larger one through two slab buffers, each
	// slab computed from the previous iterate and written back once the
	// next slab, which reads its last slice, is done. The result is
	// identical either way; slabs are at least one slice.
	MaxWorkingBytes int64
}

// GradientAnisotropicDiffusion smooths src within regions while keeping the
// edges between them (Perona–Malik). Axes of a single sample are not
// diffused, so a 2-D image is filtered in plane.
func GradientAnisotropicDiffusion(ctx context.Context, src *Volume, opts AnisotropicDiffusionOptions) (*Volume, error) {
	return anisotropicDiffusion(ctx, src, opts, diffusionLine, 1)
}

// CurvatureAnisotropicDiffusion smooths src with the modified curvature
// diffusion equation (MCDE), which keeps edges without the staircasing of
// Perona–Malik on noisy slopes. Options and stability are those of
// GradientAnisotropicDiffusion; the scheme does not conserve the mean.
func CurvatureAnisotropicDiffusion(ctx context.Context, src *Volume, opts AnisotropicDiffusionOptions) (*Volume, error) {
	return anisotropicDiffusion(ctx, src, opts, curvatureDiffusionLine, 0.5)
}

// diffusionLineFunc updates line (z*ny + y) of v into next, whose first
// sample is that of index offset in v.
type diffusionLineFunc func(v *Volume, next []float32, offset, line int, axes []int, invH2 [3]float64, k, dt float64)

// anisotropicDiffusion runs the explicit scheme update with a time step
// bounded by stability times 1/(2·Σ 1/hᵢ²).
func anisotropicDiffusion(ctx context.Context, src *Volume, opts AnisotropicDiffusionOptions, update diffusionLineFunc, stability float64) (*Volume, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := src.Validate(); err != nil {
		return nil, err
	}
	if opts.Iterations == 0 {
		opts.Iterations = 5
	}
	if opts.Conductance == 0 {
		opts.Conductance = 1
	}
	if opts.Iterations < 0 || opts.Iterations > 1000 || !(opts.Conductance > 0) || math.IsInf(opts.Conductance, 0) {
		return nil, fmt.Errorf("%w: iterations %d, conductance %v", ErrInvalidVolume, opts.Iterations, opts.Conductance)
	}
	var axes []int
	invH2 := [3]float64{}
	limit := 0.0
	for axis := 0; axis < 3; axis++ {
		if src.Dims[axis] > 1 {
			axes = append(axes, axis)
			invH2[axis] = 1 / (src.SpacingMM[axis] * src.SpacingMM[axis])
			limit += invH2[axis]
		}
	}
	out := src.Clone()
	if len(axes) == 0 {
		return out, nil
	}
	stable := stability / (2 * limit)
	if opts.TimeStep == 0 {
		opts.TimeStep = 0.9 * stable
	}
	if !(opts.TimeStep > 0) || opts.TimeStep > stable*(1+1e-9) {
		return nil, fmt.Errorf("%w: time step %v mm² exceeds the stable %v mm²", ErrInvalidVolume, opts.TimeStep, stable)
	}
	if opts.MaxWorkingBytes < 0 {
		return nil, fmt.Errorf("%w: working bound %d bytes", ErrInvalidVolume, opts.MaxWorkingBytes)
	}
	if opts.MaxWorkingBytes == 0 {
		opts.MaxWorkingBytes = DefaultSlabWorkingBytes
	}
	ny, nz := src.Dims[1], src.Dims[2]
	nxy := src.Dims[0] * ny
	workers := workerCount(opts.Workers, nz*ny)
	// One volume-sized buffer when it fits, else two slabs of whole slices.
	slab := nz
	if int64(len(out.Data))*4 > opts.MaxWorkingBytes {
		slab = int(max(1, min(int64(nz), opts.MaxWorkingBytes/(2*int64(nxy)*4))))
	}
	var bufs [2][]float32
	bufs[0] = make([]float32, slab*nxy)
	if slab < nz {
		bufs[1] = make([]float32, slab*nxy)
	}
	for iteration := 0; iteration < opts.Iterations; iteration++ {
		meanSquare, err := averageGradientSquared(ctx, out, axes, false)
		if err != nil {
			return nil, err
		}
		if meanSquare == 0 {
			// Centered interior differences can vanish on a nonconstant
			// image, such as an alternating line. Include the boundary
			// gradients before deciding that there is no diffusion.
			meanSquare, err = averageGradientSquared(ctx, out, axes, true)
			if err != nil {
				return nil, err
			}
			if meanSquare == 0 {
				break
			}
		}
		k := 2 * opts.Conductance * opts.Conductance * meanSquare
		if slab == nz {
			next := bufs[0]
			if err := parallelLines(ctx, ny*nz, workers, func(line int) {
				update(out, next, 0, line, axes, invH2, k, opts.TimeStep)
			}); err != nil {
				return nil, err
			}
			out.Data, bufs[0] = next, out.Data
			continue
		}
		// Slab s reads the last slice of slab s−1, so slab s−1 is written
		// back only after slab s is computed.
		pending := -1
		for s, z0 := 0, 0; z0 < nz; s, z0 = s+1, z0+slab {
			z1 := min(nz, z0+slab)
			next := bufs[s%2]
			if err := parallelLines(ctx, (z1-z0)*ny, workers, func(line int) {
				update(out, next, z0*nxy, z0*ny+line, axes, invH2, k, opts.TimeStep)
			}); err != nil {
				return nil, err
			}
			if pending >= 0 {
				copy(out.Data[pending*nxy:z0*nxy], bufs[(s+1)%2])
			}
			pending = z0
		}
		copy(out.Data[pending*nxy:], bufs[((nz-1)/slab)%2][:(nz-pending)*nxy])
	}
	return out, nil
}

// averageGradientSquared uses squared centered derivatives in intensity per
// mm. The normal estimate excludes the outer samples on axes longer than
// two. When that estimate is zero, the boundary pass includes the interior
// as zero-valued terms in the whole-volume mean. All outer derivatives use
// the same replicated boundary as the update.
func averageGradientSquared(ctx context.Context, v *Volume, axes []int, boundaryOnly bool) (float64, error) {
	nx, ny, nz := v.Dims[0], v.Dims[1], v.Dims[2]
	strides := [3]int{1, nx, nx * ny}
	sum, count := 0.0, 0
	for z := 0; z < nz; z++ {
		for y := 0; y < ny; y++ {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			for x := 0; x < nx; x++ {
				c := [3]int{x, y, z}
				interior := true
				for _, axis := range axes {
					if v.Dims[axis] > 2 && (c[axis] == 0 || c[axis] == v.Dims[axis]-1) {
						interior = false
					}
				}
				if interior == boundaryOnly {
					continue
				}
				i := (z*ny+y)*nx + x
				g := 0.0
				for _, axis := range axes {
					low := i - min(c[axis], 1)*strides[axis]
					high := i + min(v.Dims[axis]-1-c[axis], 1)*strides[axis]
					d := (float64(v.Data[high]) - float64(v.Data[low])) / (2 * v.SpacingMM[axis])
					g += d * d
				}
				sum += g
				count++
			}
		}
	}
	if boundaryOnly {
		count = len(v.Data)
	}
	if count == 0 {
		return 0, ctx.Err()
	}
	return sum / float64(count), ctx.Err()
}

// diffusionLine updates one x line (line = z*ny + y) into next.
func diffusionLine(v *Volume, next []float32, offset, line int, axes []int, invH2 [3]float64, k, dt float64) {
	nx, ny := v.Dims[0], v.Dims[1]
	y, z := line%ny, line/ny
	strides := [3]int{1, nx, nx * ny}
	data := v.Data
	at := func(c [3]int) float64 {
		for axis := 0; axis < 3; axis++ {
			if c[axis] < 0 {
				c[axis] = 0
			} else if c[axis] >= v.Dims[axis] {
				c[axis] = v.Dims[axis] - 1
			}
		}
		return float64(data[(c[2]*ny+c[1])*nx+c[0]])
	}
	for x := 0; x < nx; x++ {
		c := [3]int{x, y, z}
		i := (z*ny+y)*nx + x
		centre := float64(data[i])
		delta := 0.0
		for _, axis := range axes {
			h := v.SpacingMM[axis]
			for _, side := range [2]int{1, -1} {
				n := c
				n[axis] += side
				if n[axis] < 0 || n[axis] >= v.Dims[axis] {
					continue // zero flux through the border
				}
				neighbour := float64(data[i+side*strides[axis]])
				normal := (neighbour - centre) / h
				g2 := normal * normal
				for _, other := range axes {
					if other == axis {
						continue
					}
					ho := v.SpacingMM[other]
					p, m := c, c
					p[other]++
					m[other]--
					here := (at(p) - at(m)) / (2 * ho)
					p[axis] += side
					m[axis] += side
					there := (at(p) - at(m)) / (2 * ho)
					t := (here + there) / 2
					g2 += t * t
				}
				delta += math.Exp(-g2/k) * (neighbour - centre) * invH2[axis]
			}
		}
		next[i-offset] = float32(centre + dt*delta)
	}
}

// curvatureDiffusionLine updates one x line with the MCDE step.
func curvatureDiffusionLine(v *Volume, next []float32, offset, line int, axes []int, invH2 [3]float64, k, dt float64) {
	const minNorm = 1e-10 // keeps |∇I| away from zero in flat regions, as ITK
	nx, ny := v.Dims[0], v.Dims[1]
	y, z := line%ny, line/ny
	data := v.Data
	at := func(c [3]int) float64 {
		for axis := 0; axis < 3; axis++ {
			if c[axis] < 0 {
				c[axis] = 0
			} else if c[axis] >= v.Dims[axis] {
				c[axis] = v.Dims[axis] - 1
			}
		}
		return float64(data[(c[2]*ny+c[1])*nx+c[0]])
	}
	// centred is the centred derivative along axis at c, per mm.
	centred := func(c [3]int, axis int) float64 {
		p, m := c, c
		p[axis]++
		m[axis]--
		return (at(p) - at(m)) / (2 * v.SpacingMM[axis])
	}
	for x := 0; x < nx; x++ {
		c := [3]int{x, y, z}
		i := (z*ny+y)*nx + x
		centre := float64(data[i])
		var forward, backward, mid [3]float64
		for _, axis := range axes {
			p, m := c, c
			p[axis]++
			m[axis]--
			forward[axis] = (at(p) - centre) / v.SpacingMM[axis]
			backward[axis] = (centre - at(m)) / v.SpacingMM[axis]
			mid[axis] = centred(c, axis)
		}
		speed := 0.0
		for _, axis := range axes {
			g2f := forward[axis] * forward[axis]
			g2b := backward[axis] * backward[axis]
			for _, other := range axes {
				if other == axis {
					continue
				}
				p, m := c, c
				p[axis]++
				m[axis]--
				aug := mid[other] + centred(p, other)
				dim := mid[other] + centred(m, other)
				g2f += 0.25 * aug * aug
				g2b += 0.25 * dim * dim
			}
			nf := forward[axis] / math.Sqrt(minNorm+g2f) * math.Exp(-g2f/k)
			nb := backward[axis] / math.Sqrt(minNorm+g2b) * math.Exp(-g2b/k)
			speed += (nf - nb) / v.SpacingMM[axis]
		}
		// Upwind |∇I| so the level sets move in the direction of the speed.
		propagation := 0.0
		for _, axis := range axes {
			if speed > 0 {
				propagation += sqr(math.Min(backward[axis], 0)) + sqr(math.Max(forward[axis], 0))
			} else {
				propagation += sqr(math.Max(backward[axis], 0)) + sqr(math.Min(forward[axis], 0))
			}
		}
		next[i-offset] = float32(centre + dt*math.Sqrt(propagation)*speed)
	}
}

func sqr(v float64) float64 { return v * v }

// BilateralOptions configures Bilateral.
type BilateralOptions struct {
	// SigmaMM is the domain (spatial) Gaussian σ in mm per axis; axes with
	// zero σ or a single sample are not averaged across.
	SigmaMM [3]float64
	// RangeSigma is the range Gaussian σ in modality units (HU for CT):
	// neighbours differing by several RangeSigma barely contribute.
	RangeSigma float64
	// MaxKernelSamples bounds the truncated kernel; 1<<16 when zero.
	MaxKernelSamples int
	// Workers bounds the goroutines (0 selects the shared concurrency budget).
	Workers int
}

// Bilateral averages each sample with its neighbours weighted by distance
// and by similarity of value, smoothing noise while keeping edges.
func Bilateral(ctx context.Context, src *Volume, opts BilateralOptions) (*Volume, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := src.Validate(); err != nil {
		return nil, err
	}
	k, err := newBilateralKernel(src.Dims, src.SpacingMM, opts)
	if err != nil {
		return nil, err
	}
	out := &Volume{Dims: src.Dims, SpacingMM: src.SpacingMM, Data: make([]float32, len(src.Data))}
	if err := k.run(ctx, wholeView(src), 0, src.Dims[2], out.Data); err != nil {
		return nil, err
	}
	return out, nil
}

// bilateralTap is one sample offset of the truncated domain kernel.
type bilateralTap struct {
	dx, dy, dz int
	w          float64
}

// bilateralKernel is a validated Bilateral request for one grid.
type bilateralKernel struct {
	dims       [3]int
	radius     [3]int
	taps       []bilateralTap
	rangeScale float64
	workers    int
}

func (opts BilateralOptions) slabKernel(dims [3]int, spacingMM [3]float64) (slabKernel, error) {
	return newBilateralKernel(dims, spacingMM, opts)
}

func newBilateralKernel(dims [3]int, spacingMM [3]float64, opts BilateralOptions) (*bilateralKernel, error) {
	if !(opts.RangeSigma > 0) || math.IsInf(opts.RangeSigma, 0) {
		return nil, fmt.Errorf("%w: range sigma %v", ErrInvalidVolume, opts.RangeSigma)
	}
	if opts.MaxKernelSamples == 0 {
		opts.MaxKernelSamples = 1 << 16
	}
	k := &bilateralKernel{dims: dims, workers: opts.Workers, rangeScale: -1 / (2 * opts.RangeSigma * opts.RangeSigma)}
	samples := 1
	for axis := 0; axis < 3; axis++ {
		s := opts.SigmaMM[axis]
		if s < 0 || math.IsNaN(s) || math.IsInf(s, 0) {
			return nil, fmt.Errorf("%w: sigma %v mm", ErrInvalidVolume, s)
		}
		if s > 0 && dims[axis] > 1 {
			k.radius[axis] = int(math.Ceil(2.5 * s / spacingMM[axis]))
		}
		samples *= 2*k.radius[axis] + 1
		if samples > opts.MaxKernelSamples {
			return nil, fmt.Errorf("%w: the bilateral kernel exceeds %d samples", ErrInvalidVolume, opts.MaxKernelSamples)
		}
	}
	r := k.radius
	for dz := -r[2]; dz <= r[2]; dz++ {
		for dy := -r[1]; dy <= r[1]; dy++ {
			for dx := -r[0]; dx <= r[0]; dx++ {
				d2 := 0.0
				for axis, d := range [3]int{dx, dy, dz} {
					if d != 0 {
						r := float64(d) * spacingMM[axis] / opts.SigmaMM[axis]
						d2 += r * r
					}
				}
				if d2 <= 2.5*2.5 {
					k.taps = append(k.taps, bilateralTap{dx, dy, dz, math.Exp(-d2 / 2)})
				}
			}
		}
	}
	return k, nil
}

func (k *bilateralKernel) halo() int { return k.radius[2] }

func (k *bilateralKernel) scratchBytes(int) int64 { return 0 }

// run averages the output slices [z0, z1); taps outside the volume are
// left out.
func (k *bilateralKernel) run(ctx context.Context, in slabView, z0, z1 int, dst []float32) error {
	nx, ny, nz := k.dims[0], k.dims[1], k.dims[2]
	lines := (z1 - z0) * ny
	return parallelLines(ctx, lines, workerCount(k.workers, lines), func(line int) {
		y, z := line%ny, z0+line/ny
		centreRow := in.row(y, z)
		for x := 0; x < nx; x++ {
			centre := float64(in.data[centreRow+x])
			sum, weights := 0.0, 0.0
			for _, t := range k.taps {
				sx, sy, sz := x+t.dx, y+t.dy, z+t.dz
				if sx < 0 || sy < 0 || sz < 0 || sx >= nx || sy >= ny || sz >= nz {
					continue
				}
				v := float64(in.data[in.row(sy, sz)+sx])
				d := v - centre
				w := t.w * math.Exp(d*d*k.rangeScale)
				sum += w * v
				weights += w
			}
			dst[line*nx+x] = float32(sum / weights)
		}
	})
}

// UnsharpMaskOptions configures UnsharpMask.
type UnsharpMaskOptions struct {
	// SigmaMM is the Gaussian σ in mm per axis of the blurred copy.
	SigmaMM [3]float64
	// Amount scales the added detail; 0.5 when zero.
	Amount float64
	// Threshold, when positive, adds detail only where |I - G*I| exceeds it
	// (in modality units), leaving flat noise alone.
	Threshold float64
	// Workers bounds the goroutines (0 selects the shared concurrency budget).
	Workers int
}

// UnsharpMask sharpens src: I + Amount·(I − G_σ∗I). The result is written
// over the blurred copy, so the filter allocates the output and one
// intermediate volume of the recursive Gaussian.
func UnsharpMask(ctx context.Context, src *Volume, opts UnsharpMaskOptions) (*Volume, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	blurred, err := SmoothRecursiveGaussian(ctx, src, RecursiveGaussianOptions{SigmaMM: opts.SigmaMM, Workers: opts.Workers})
	if err != nil {
		return nil, err
	}
	opts.sharpen(src.Data, blurred.Data, blurred.Data)
	return blurred, nil
}

func (opts *UnsharpMaskOptions) validate() error {
	if opts.Amount == 0 {
		opts.Amount = 0.5
	}
	if math.IsNaN(opts.Amount) || math.IsInf(opts.Amount, 0) || opts.Threshold < 0 || math.IsNaN(opts.Threshold) {
		return fmt.Errorf("%w: amount %v, threshold %v", ErrInvalidVolume, opts.Amount, opts.Threshold)
	}
	return nil
}

// sharpen stores in dst (which may alias blurred) src + Amount·(src −
// blurred), or src where the detail is within the threshold.
func (opts UnsharpMaskOptions) sharpen(src, blurred, dst []float32) {
	for i, v := range src {
		detail := float64(v) - float64(blurred[i])
		if opts.Threshold > 0 && math.Abs(detail) <= opts.Threshold {
			dst[i] = v
			continue
		}
		dst[i] = float32(float64(v) + opts.Amount*detail)
	}
}

// unsharpKernel runs UnsharpMask slab by slab; it needs a zero z σ (or a
// 2-D image), as the recursive Gaussian has unbounded support along z. The
// y and x passes are those of SmoothRecursiveGaussian (the same kernels and
// lane grouping per slice), into buffers kept across slabs.
type unsharpKernel struct {
	opts    UnsharpMaskOptions
	dims    [3]int
	kernels [2]*recursiveKernel // x and y; nil when the axis is not smoothed
	workers int
	yPass   []float32
	blurred []float32
	scratch scratchPool[laneScratch]
}

func (opts UnsharpMaskOptions) slabKernel(dims [3]int, spacingMM [3]float64) (slabKernel, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if dims[2] > 1 && opts.SigmaMM[2] != 0 {
		return nil, fmt.Errorf("%w: UnsharpMask streams only in plane (a zero z sigma): its recursive Gaussian along z reads the whole column", ErrInvalidVolume)
	}
	k := &unsharpKernel{opts: opts, dims: dims, workers: workerBound(opts.Workers)}
	for axis, sigma := range opts.SigmaMM {
		if !(sigma >= 0 && sigma <= math.MaxFloat64) {
			return nil, fmt.Errorf("%w: sigma %v mm along axis %d", ErrInvalidVolume, sigma, axis)
		}
		if axis == 2 || sigma == 0 || dims[axis] == 1 {
			continue
		}
		rk, err := newRecursiveKernel(sigma/spacingMM[axis], 0, 1)
		if err != nil {
			return nil, fmt.Errorf("%w: axis %d: %v", ErrInvalidVolume, axis, err)
		}
		k.kernels[axis] = rk
	}
	return k, nil
}

func (k *unsharpKernel) halo() int { return 0 }

// scratchBytes is the two pass buffers plus the per-worker line scratch.
func (k *unsharpKernel) scratchBytes(slices int) int64 {
	n := int64(slices) * int64(k.dims[0]*k.dims[1])
	line := int64(max(k.dims[0], k.dims[1])*laneWidth+4*laneWidth) * 8
	return 2*n*4 + int64(k.workers)*line
}

func (k *unsharpKernel) run(ctx context.Context, in slabView, z0, z1 int, dst []float32) error {
	nx, ny := k.dims[0], k.dims[1]
	slices := z1 - z0
	n := slices * nx * ny
	src := in.data[(z0-in.lo)*nx*ny : (z1-in.lo)*nx*ny]
	if len(k.yPass) < n {
		k.yPass = make([]float32, n)
		k.blurred = make([]float32, n)
	}
	y := src
	if rk := k.kernels[1]; rk != nil {
		// Lanes are consecutive columns of one slice.
		if err := k.pass(ctx, rk, k.yPass[:n], src, ny, nx, 1, nx, slices, nx*ny); err != nil {
			return err
		}
		y = k.yPass[:n]
	}
	blurred := y
	if rk := k.kernels[0]; rk != nil {
		// Lanes are consecutive rows.
		if err := k.pass(ctx, rk, k.blurred[:n], y, nx, 1, nx, ny*slices, 1, 0); err != nil {
			return err
		}
		blurred = k.blurred[:n]
	}
	k.opts.sharpen(src, blurred, dst)
	return ctx.Err()
}

// pass filters groups×lanes lines of length samples laneWidth at a time,
// as gaussianPlan.pass does.
func (k *unsharpKernel) pass(ctx context.Context, rk *recursiveKernel, dst, src []float32, length, step, laneStride, lanes, groups, groupStride int) error {
	chunks := (lanes + laneWidth - 1) / laneWidth
	return runWithScratch(ctx, groups*chunks, k.workers, &k.scratch, func(s *laneScratch, item int) {
		if len(s.tmp) < length*laneWidth {
			s.tmp = make([]float64, length*laneWidth)
			s.state = make([]float64, 4*laneWidth)
		}
		g, c := item/chunks, item%chunks
		first := c * laneWidth
		count := min(laneWidth, lanes-first)
		base := g*groupStride + first*laneStride
		rk.filterLanes(dst, src, base, step, laneStride, length, count, s.tmp, s.state)
	})
}

func workerCount(requested, lines int) int {
	workers := requested
	if workers <= 0 {
		workers = parallel.DefaultWorkers()
	}
	return max(1, min(workers, lines))
}

// parallelLines runs fn for every line index on up to workers workers from
// the shared concurrency budget, checking ctx between lines.
func parallelLines(ctx context.Context, lines, workers int, fn func(line int)) error {
	return parallel.Run(ctx, lines, workers, max(1, lines/(max(1, workers)*8)), func() func(int) { return fn })
}
