package imagefilter

import (
	"context"
	"fmt"
	"math"
	"sync/atomic"

	"github.com/ThalesMMS/dicom-go/internal/linalg"
	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// Multiscale Hessian-based vesselness (#1099): Frangi's objectness and Sato's
// line measure, evaluated from the scale-normalized Hessian at several scales
// and kept at the scale of the strongest response.
//
// The per-voxel measures and the multiscale scheme follow ITK (Apache-2.0,
// Copyright NumFOCUS; see ITK's NOTICE), commit 35b196e1:
// HessianToObjectnessMeasureImageFilter (Frangi et al. 1998 as generalized by
// L. Antiga to M-dimensional objects in N-dimensional images),
// Hessian3DToVesselnessMeasureImageFilter (Sato et al. 1998) and
// MultiScaleHessianBasedMeasureImageFilter (logarithmic or equispaced σ steps,
// σ²-normalized Hessian, maximum over the scales with the first scale kept on
// ties, optional scales output). Modifications: the Hessian comes from this
// package's recursive Gaussian with σ in millimetres; it is computed slab by
// slab along z and never stored for the whole volume (see below); the
// eigenvalues are the closed-form ones of internal/linalg instead of an
// iterative solver; Frangi's structureness weight γ may be automatic (half
// the largest Hessian norm at each scale, Frangi's suggestion and
// scikit-image's default) or disabled; the object brightness is a flag on
// the Hessian sign rather than a separate constraint; and work is parallel
// and cancellable. No ITK code is copied.
//
// Memory. The six Hessian components of a 512×512×500 volume take 3 GB in
// float32; they are never held at once. The volume is processed in slabs of
// whole slices: for each scale and slab, the z derivatives (orders 0, 1 and 2)
// of the slab are computed from the input lines extended by a margin of
// vesselnessMarginSigmas·σ on each side (the recursion starts from the
// replicated margin edge, whose influence has decayed below 3·10⁻⁴ of the
// kernel peak at the slab), then the in-plane passes run on the slab alone,
// and the measure of each voxel is folded into the maximum over scales. The
// working set is seven slab-sized float32 buffers plus a per-worker line
// scratch, allocated once and reused for every slab and scale; the slab
// thickness is the largest that keeps the buffers within MaxWorkingBytes.
// The caller holds the input and the outputs (the response, and the scale
// map if requested), 4 bytes per voxel each. With the automatic γ, a volume
// split in several slabs needs the largest Hessian norm before any measure,
// so each scale costs a second Hessian pass; a single slab computes it once.
// To analyse a region only (a VOI, or the box around a CPR path), filter a
// cropped Volume with a margin of a few σ around the region.

// VesselnessMeasure selects the per-voxel measure of the Hessian eigenvalues.
type VesselnessMeasure uint8

const (
	// VesselnessFrangi is Frangi's objectness (ITK's
	// HessianToObjectnessMeasureImageFilter): with the eigenvalues sorted by
	// magnitude |λ1| ≤ |λ2| ≤ |λ3|, a tube is
	// (1 − exp(−Ra²/2α²))·exp(−Rb²/2β²)·(1 − exp(−S²/2γ²)), Ra = |λ2|/|λ3|,
	// Rb = |λ1|/√|λ2λ3| and S the Frobenius norm of the Hessian, and is zero
	// unless λ2 and λ3 have the sign of a bright (or dark) object. Without
	// ScaleByLargestEigenvalue it lies in [0, 1]. Blobs and plates use
	// Antiga's generalization (Structure).
	VesselnessFrangi VesselnessMeasure = iota
	// VesselnessSato is Sato's line measure (ITK's
	// Hessian3DToVesselnessMeasureImageFilter): with the eigenvalues sorted
	// by value λ1 ≤ λ2 ≤ λ3, a bright line gives −λ2·exp(−λ3²/2(α·λ2)²),
	// α = Alpha1 when λ3 ≤ 0 and Alpha2 when λ3 > 0, and zero when λ2 ≥ 0. It
	// is in intensity units (per the σ² normalization) and defined for 3-D
	// volumes only.
	VesselnessSato
)

// String returns "frangi" or "sato".
func (m VesselnessMeasure) String() string {
	switch m {
	case VesselnessFrangi:
		return "frangi"
	case VesselnessSato:
		return "sato"
	default:
		return "unknown"
	}
}

// HessianStructure is the shape the Frangi objectness enhances: the object
// dimension M of Antiga's generalization.
type HessianStructure uint8

const (
	// StructureTube enhances lines (M = 1): vessels, airways, bronchi.
	StructureTube HessianStructure = iota
	// StructureBlob enhances blobs (M = 0): nodules, aneurysm sacs.
	StructureBlob
	// StructurePlate enhances sheets (M = 2, 3-D only): cortical bone,
	// fissures.
	StructurePlate
)

func (s HessianStructure) objectDimension() int {
	switch s {
	case StructureBlob:
		return 0
	case StructurePlate:
		return 2
	default:
		return 1
	}
}

const (
	// DefaultVesselnessSigmaSteps is the number of scales when
	// VesselnessOptions.SigmaSteps is zero and SigmaMaxMM > SigmaMinMM.
	DefaultVesselnessSigmaSteps = 4
	// DefaultVesselnessWorkingBytes bounds the slab buffers and line scratch
	// when VesselnessOptions.MaxWorkingBytes is zero.
	DefaultVesselnessWorkingBytes = 256 << 20
	// vesselnessMarginSigmas is the z margin of a slab, in σ.
	vesselnessMarginSigmas = 6
	// vesselnessSlabBuffers is the number of slab-sized buffers.
	vesselnessSlabBuffers = 7
)

// VesselnessOptions configures Vesselness. The zero value is Frangi's tube
// measure of bright structures with α = β = 0.5 and automatic γ, which only
// lacks the scales.
type VesselnessOptions struct {
	// Measure selects Frangi's objectness (default) or Sato's line measure.
	Measure VesselnessMeasure
	// Structure is the shape Frangi's objectness enhances (tubes by default).
	// Plates need a 3-D volume. Sato's measure always enhances lines.
	Structure HessianStructure
	// DarkObjects enhances structures darker than their surroundings (for
	// example black-blood MR or airways in CT) instead of brighter ones
	// (contrast-enhanced angiography).
	DarkObjects bool

	// SigmasMM lists the scales (Gaussian σ in millimetres, the same along
	// every axis). When empty the scales run from SigmaMinMM to SigmaMaxMM
	// (SigmaMinMM when zero) in SigmaSteps steps (DefaultVesselnessSigmaSteps
	// when zero and the range is not empty, 1 otherwise), logarithmically
	// spaced unless EquispacedSigmas, as ITK's
	// MultiScaleHessianBasedMeasureImageFilter computes them. A tube of
	// radius r with a Gaussian profile answers best at σ = r; a solid
	// cylinder at about σ = r/√2.
	SigmasMM         []float64
	SigmaMinMM       float64
	SigmaMaxMM       float64
	SigmaSteps       int
	EquispacedSigmas bool

	// Alpha and Beta weigh Frangi's Ra and Rb ratios (0 selects 0.5, ITK's
	// and Frangi's default). Smaller values make the measure more selective
	// for the structure's dimensionality.
	Alpha, Beta float64
	// Gamma weighs Frangi's structureness S (the Frobenius norm of the
	// normalized Hessian), in the input's intensity units: 0 selects
	// automatically half the largest norm at each scale (Frangi 1998,
	// scikit-image), a negative value leaves the term out (its factor is 1),
	// as ITK does for γ = 0.
	Gamma float64
	// ScaleByLargestEigenvalue multiplies Frangi's objectness by |λ3|, as
	// ITK's ScaleObjectnessMeasure does (on by default in ITK), which puts it
	// in intensity units and damps weak structures.
	ScaleByLargestEigenvalue bool
	// Alpha1 and Alpha2 are Sato's weights for λ3 ≤ 0 and λ3 > 0 (0 selects
	// ITK's defaults 0.5 and 2).
	Alpha1, Alpha2 float64

	// ScalesOutput also returns, per voxel, the σ of the strongest response.
	ScalesOutput bool
	// MaxWorkingBytes bounds the slab buffers Vesselness allocates beyond
	// its outputs (0 selects DefaultVesselnessWorkingBytes): seven float32
	// buffers of a slab's slices. The slabs are thinned to fit, down to one
	// slice, which is the minimum whatever the bound. A line scratch of
	// 8·32·(longest axis) bytes per worker (128 KiB for 512 samples) comes on
	// top.
	MaxWorkingBytes int64
	// Workers bounds the goroutines used (0 selects the shared concurrency budget). The result
	// does not depend on the worker count.
	Workers int
}

// VesselnessResult is the output of Vesselness.
type VesselnessResult struct {
	// Response is the largest measure over the scales, zero where no scale
	// responds; it has the input's dimensions and spacing.
	Response *Volume
	// ScaleMM holds the σ (mm) of the scale that gave Response, 0 where
	// Response is 0. It is nil unless ScalesOutput was set.
	ScaleMM *Volume
	// SigmasMM are the scales evaluated, in order.
	SigmasMM []float64
	// Gammas are Frangi's effective γ per scale (0 where the structureness
	// term is left out); nil for Sato's measure.
	Gammas []float64
	// SlabSlices is the number of slices per slab (the whole volume when it
	// fits the working bound).
	SlabSlices int
	// WorkingBytes is the memory allocated beyond the outputs: the slab
	// buffers and the per-worker line scratch. Only small per-pass
	// bookkeeping (goroutine closures) is allocated besides.
	WorkingBytes int64
}

// VesselnessSigmas returns steps scales from minMM to maxMM, logarithmically
// spaced or, with equispaced, evenly spaced, as ITK's
// MultiScaleHessianBasedMeasureImageFilter::ComputeSigmaValue: one step, or
// an empty range, returns minMM alone.
func VesselnessSigmas(minMM, maxMM float64, steps int, equispaced bool) ([]float64, error) {
	if !(minMM > 0) || math.IsInf(minMM, 0) || !(maxMM >= minMM) || math.IsInf(maxMM, 0) {
		return nil, fmt.Errorf("%w: sigma range %v–%v mm", ErrInvalidVolume, minMM, maxMM)
	}
	if steps < 1 {
		return nil, fmt.Errorf("%w: %d sigma steps", ErrInvalidVolume, steps)
	}
	if steps == 1 || maxMM == minMM {
		return []float64{minMM}, nil
	}
	sigmas := make([]float64, steps)
	for i := range sigmas {
		if equispaced {
			sigmas[i] = minMM + (maxMM-minMM)*float64(i)/float64(steps-1)
		} else {
			step := (math.Log(maxMM) - math.Log(minMM)) / float64(steps-1)
			sigmas[i] = math.Exp(math.Log(minMM) + step*float64(i))
		}
	}
	sigmas[steps-1] = maxMM
	return sigmas, nil
}

// Vesselness computes the multiscale Hessian vesselness of src: at each
// scale the σ²-normalized Hessian of the Gaussian-smoothed volume, its
// eigenvalues per voxel and the selected measure, keeping per voxel the
// largest response. A 2-D image (Dims[2] == 1) uses the 2×2 Hessian and
// Frangi's two-dimensional objectness (tubes and blobs); Sato's measure
// needs a volume. ctx is polled between groups of lines and voxels; on
// cancellation ctx.Err() is returned and no partial result.
func Vesselness(ctx context.Context, src *Volume, opts VesselnessOptions) (*VesselnessResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := src.Validate(); err != nil {
		return nil, err
	}
	p, err := newVesselPlan(src, opts)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.ctx = ctx
	p.allocate()

	res := &VesselnessResult{
		Response: &Volume{Dims: src.Dims, SpacingMM: src.SpacingMM, Data: make([]float32, len(src.Data))},
		SigmasMM: append([]float64(nil), p.sigmas...), SlabSlices: p.slab, WorkingBytes: p.workingBytes,
	}
	if opts.ScalesOutput {
		res.ScaleMM = &Volume{Dims: src.Dims, SpacingMM: src.SpacingMM, Data: make([]float32, len(src.Data))}
	}
	if p.measure == VesselnessFrangi {
		res.Gammas = make([]float64, len(p.sigmas))
	}
	for i, sigma := range p.sigmas {
		if err := p.setScale(sigma); err != nil {
			return nil, err
		}
		gamma := p.gamma
		slabs := (p.nz + p.slab - 1) / p.slab
		if p.measure == VesselnessFrangi && p.autoGamma {
			if slabs > 1 {
				// The largest norm must be known before any measure.
				maxNorm := 0.0
				for z0 := 0; z0 < p.nz; z0 += p.slab {
					z1 := min(p.nz, z0+p.slab)
					h, err := p.hessianSlab(z0, z1)
					if err != nil {
						return nil, err
					}
					m, err := p.maxNorm(h, (z1-z0)*p.nxy)
					if err != nil {
						return nil, err
					}
					maxNorm = math.Max(maxNorm, m)
				}
				gamma = maxNorm / 2
			} else {
				h, err := p.hessianSlab(0, p.nz)
				if err != nil {
					return nil, err
				}
				m, err := p.maxNorm(h, p.nz*p.nxy)
				if err != nil {
					return nil, err
				}
				gamma = m / 2
				if err := p.measureSlab(h, 0, p.nz, gamma, sigma, res); err != nil {
					return nil, err
				}
				res.Gammas[i] = gamma
				continue
			}
		}
		if res.Gammas != nil {
			res.Gammas[i] = math.Max(gamma, 0)
		}
		for z0 := 0; z0 < p.nz; z0 += p.slab {
			z1 := min(p.nz, z0+p.slab)
			h, err := p.hessianSlab(z0, z1)
			if err != nil {
				return nil, err
			}
			if err := p.measureSlab(h, z0, z1, gamma, sigma, res); err != nil {
				return nil, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return res, nil
}

// vesselPlan is one Vesselness request: the validated options, the slab
// geometry and the reusable buffers.
type vesselPlan struct {
	ctx        context.Context
	src        *Volume
	nx, ny, nz int
	nxy        int
	sigmas     []float64
	measure    VesselnessMeasure
	objectDim  int
	imageDim   int
	dark       bool
	alpha      float64
	beta       float64
	gamma      float64 // Frangi γ when fixed; ≤ 0 disables the term
	autoGamma  bool
	scaleByL3  bool
	alpha1     float64
	alpha2     float64

	workers      int
	slab         int
	maxLine      int
	workingBytes int64
	bufs         [vesselnessSlabBuffers][]float32
	scratch      []laneScratch
	nextScratch  atomic.Int32

	// Per-scale state.
	kernels [3][3]*recursiveKernel // [axis][order]; nil when the axis has one sample
	margin  int                    // z margin in slices
}

type laneScratch struct {
	tmp, state []float64
}

// hessianSlab is the Hessian of a slab's core slices; nil components are
// identically zero.
type hessianSlab struct {
	xx, xy, xz, yy, yz, zz []float32
}

func newVesselPlan(src *Volume, opts VesselnessOptions) (*vesselPlan, error) {
	p := &vesselPlan{
		src: src, nx: src.Dims[0], ny: src.Dims[1], nz: src.Dims[2],
		measure: opts.Measure, dark: opts.DarkObjects, workers: opts.Workers,
		scaleByL3: opts.ScaleByLargestEigenvalue,
	}
	p.nxy = p.nx * p.ny
	p.imageDim = 3
	if p.nz == 1 {
		p.imageDim = 2
	}
	switch opts.Measure {
	case VesselnessFrangi:
		if opts.Structure > StructurePlate {
			return nil, fmt.Errorf("%w: Hessian structure %d", ErrInvalidVolume, opts.Structure)
		}
		p.objectDim = opts.Structure.objectDimension()
		if p.objectDim >= p.imageDim {
			return nil, fmt.Errorf("%w: plate objectness needs a 3-D volume", ErrInvalidVolume)
		}
		var err error
		if p.alpha, err = weight("alpha", opts.Alpha, 0.5); err != nil {
			return nil, err
		}
		if p.beta, err = weight("beta", opts.Beta, 0.5); err != nil {
			return nil, err
		}
		if math.IsNaN(opts.Gamma) || math.IsInf(opts.Gamma, 0) {
			return nil, fmt.Errorf("%w: gamma %v", ErrInvalidVolume, opts.Gamma)
		}
		p.gamma = opts.Gamma
		p.autoGamma = opts.Gamma == 0
	case VesselnessSato:
		if p.imageDim != 3 {
			return nil, fmt.Errorf("%w: Sato's line measure needs a 3-D volume", ErrInvalidVolume)
		}
		var err error
		if p.alpha1, err = weight("alpha1", opts.Alpha1, 0.5); err != nil {
			return nil, err
		}
		if p.alpha2, err = weight("alpha2", opts.Alpha2, 2); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: vesselness measure %d", ErrInvalidVolume, opts.Measure)
	}

	if len(opts.SigmasMM) > 0 {
		for _, s := range opts.SigmasMM {
			if !(s > 0) || math.IsInf(s, 0) {
				return nil, fmt.Errorf("%w: sigma %v mm", ErrInvalidVolume, s)
			}
		}
		p.sigmas = append([]float64(nil), opts.SigmasMM...)
	} else {
		maxMM, steps := opts.SigmaMaxMM, opts.SigmaSteps
		if maxMM == 0 {
			maxMM = opts.SigmaMinMM
		}
		if steps == 0 {
			steps = 1
			if maxMM > opts.SigmaMinMM {
				steps = DefaultVesselnessSigmaSteps
			}
		}
		sigmas, err := VesselnessSigmas(opts.SigmaMinMM, maxMM, steps, opts.EquispacedSigmas)
		if err != nil {
			return nil, err
		}
		p.sigmas = sigmas
	}

	if p.workers <= 0 {
		p.workers = parallel.DefaultWorkers()
	}
	p.maxLine = max(p.nx, p.ny, p.nz)
	scratchBytes := int64(p.workers) * int64(p.maxLine*laneWidth+4*laneWidth) * 8
	budget := opts.MaxWorkingBytes
	if budget < 0 {
		return nil, fmt.Errorf("%w: working bound %d bytes", ErrInvalidVolume, budget)
	}
	if budget == 0 {
		budget = DefaultVesselnessWorkingBytes
	}
	// The slab depends on the bound and the volume only, not on the worker
	// count, so neither does the result.
	slab, planErr := PlanVesselnessSlabSlices(src.Dims, budget)
	if planErr != nil {
		return nil, planErr
	}
	p.slab = slab
	sliceBytes := int64(vesselnessSlabBuffers) * int64(p.nxy) * 4
	p.workingBytes = int64(p.slab)*sliceBytes + scratchBytes
	return p, nil
}

func weight(name string, v, def float64) (float64, error) {
	if v == 0 {
		return def, nil
	}
	if !(v > 0) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("%w: %s %v", ErrInvalidVolume, name, v)
	}
	return v, nil
}

func (p *vesselPlan) allocate() {
	n := p.slab * p.nxy
	for i := range p.bufs {
		p.bufs[i] = make([]float32, n)
	}
	p.scratch = make([]laneScratch, p.workers)
	for i := range p.scratch {
		p.scratch[i] = laneScratch{tmp: make([]float64, p.maxLine*laneWidth), state: make([]float64, 4*laneWidth)}
	}
}

// setScale builds the scale-normalized kernels for σ (mm): a derivative of
// order k along an axis is multiplied by σ^k and divided by the spacing^k,
// so the kernel's scale is (σ/spacing)^k.
func (p *vesselPlan) setScale(sigma float64) error {
	p.kernels = [3][3]*recursiveKernel{}
	for axis := 0; axis < 3; axis++ {
		if p.src.Dims[axis] == 1 {
			continue
		}
		samples := sigma / p.src.SpacingMM[axis]
		for order := 0; order <= 2; order++ {
			k, err := newRecursiveKernel(samples, order, math.Pow(samples, float64(order)))
			if err != nil {
				return fmt.Errorf("%w: axis %d: %v", ErrInvalidVolume, axis, err)
			}
			p.kernels[axis][order] = k
		}
	}
	p.margin = 0
	if p.nz > 1 {
		p.margin = int(math.Ceil(vesselnessMarginSigmas * sigma / p.src.SpacingMM[2]))
	}
	return nil
}

// run executes count items on the plan's workers, each worker using its own
// preallocated line scratch.
func (p *vesselPlan) run(count int, fn func(item int, s *laneScratch)) error {
	p.nextScratch.Store(0)
	return runParallel(p.ctx, count, p.workers, func() func(int) {
		s := &p.scratch[p.nextScratch.Add(1)-1]
		return func(item int) { fn(item, s) }
	})
}

// zPass stores in dst the order-th z derivative of the core slices [z0, z1),
// filtering each line over the core extended by the margin.
func (p *vesselPlan) zPass(dst []float32, order, z0, z1 int) error {
	k := p.kernels[2][order]
	zlo, zhi := max(0, z0-p.margin), min(p.nz, z1+p.margin)
	length := zhi - zlo
	window := laneWindow{step: p.nxy, laneStride: 1, lo: z0 - zlo, hi: z1 - zlo}
	chunks := (p.nxy + laneWidth - 1) / laneWidth
	return p.run(chunks, func(c int, s *laneScratch) {
		first := c * laneWidth
		count := min(laneWidth, p.nxy-first)
		w := window
		w.base = first
		k.filterLanesWindow(dst, w, p.src.Data, zlo*p.nxy+first, p.nxy, 1, length, count, s.tmp, s.state)
	})
}

// planePass filters the slices slices of src along axis 0 (x) or 1 (y) into
// dst.
func (p *vesselPlan) planePass(dst, src []float32, k *recursiveKernel, axis, slices int) error {
	var length, step, laneStride, lanes, groups, groupStride int
	if axis == 0 {
		length, step, laneStride, lanes, groups = p.nx, 1, p.nx, p.ny*slices, 1
	} else {
		length, step, laneStride, lanes, groups, groupStride = p.ny, p.nx, 1, p.nx, slices, p.nxy
	}
	chunks := (lanes + laneWidth - 1) / laneWidth
	return p.run(groups*chunks, func(item int, s *laneScratch) {
		g, c := item/chunks, item%chunks
		first := c * laneWidth
		count := min(laneWidth, lanes-first)
		base := g*groupStride + first*laneStride
		k.filterLanesWindow(dst, laneWindow{base: base, step: step, laneStride: laneStride, lo: 0, hi: length},
			src, base, step, laneStride, length, count, s.tmp, s.state)
	})
}

// component returns ∂^(oy+ox) of the z-filtered slab zsrc along y (order oy)
// then x (order ox), in dst, using tmp for the y pass; nil means zero.
func (p *vesselPlan) component(dst, tmp, zsrc []float32, oy, ox, slices int) ([]float32, error) {
	if zsrc == nil {
		return nil, nil
	}
	n := slices * p.nxy
	y := zsrc
	if k := p.kernels[1][oy]; k != nil {
		if err := p.planePass(tmp[:n], zsrc, k, 1, slices); err != nil {
			return nil, err
		}
		y = tmp[:n]
	} else if oy > 0 {
		return nil, nil
	}
	if k := p.kernels[0][ox]; k != nil {
		if err := p.planePass(dst[:n], y, k, 0, slices); err != nil {
			return nil, err
		}
	} else if ox > 0 {
		return nil, nil
	} else {
		copy(dst[:n], y)
	}
	return dst[:n], nil
}

// hessianSlab computes the six Hessian components of the core slices
// [z0, z1) in the plan's buffers (15 line passes, as in
// HessianRecursiveGaussian). The buffers are reused: a z derivative's buffer
// receives a component once its last y pass has read it.
func (p *vesselPlan) hessianSlab(z0, z1 int) (hessianSlab, error) {
	slices := z1 - z0
	n := slices * p.nxy
	b := &p.bufs
	var zd [3][]float32
	if p.nz == 1 {
		copy(b[0][:n], p.src.Data[:n])
		zd[0] = b[0][:n]
	} else {
		for order := 0; order < 3; order++ {
			if err := p.zPass(b[order][:n], order, z0, z1); err != nil {
				return hessianSlab{}, err
			}
			zd[order] = b[order][:n]
		}
	}
	tmp := b[3]
	var h hessianSlab
	var err error
	if h.zz, err = p.component(b[4], tmp, zd[2], 0, 0, slices); err != nil {
		return h, err
	}
	if h.xz, err = p.component(b[2], tmp, zd[1], 0, 1, slices); err != nil {
		return h, err
	}
	if h.yz, err = p.component(b[5], tmp, zd[1], 1, 0, slices); err != nil {
		return h, err
	}
	if h.xx, err = p.component(b[1], tmp, zd[0], 0, 2, slices); err != nil {
		return h, err
	}
	if h.xy, err = p.component(b[6], tmp, zd[0], 1, 1, slices); err != nil {
		return h, err
	}
	// The last reader of zd[0]: its y pass fills tmp, then dst may be b[0].
	if h.yy, err = p.component(b[0], tmp, zd[0], 2, 0, slices); err != nil {
		return h, err
	}
	return h, p.ctx.Err()
}

// maxNorm returns the largest Frobenius norm of the slab's Hessian.
func (p *vesselPlan) maxNorm(h hessianSlab, n int) (float64, error) {
	const chunk = 1 << 14
	count := (n + chunk - 1) / chunk
	partial := make([]float64, count)
	err := runParallel(p.ctx, count, p.workers, func() func(int) {
		return func(c int) {
			lo, hi := c*chunk, min(n, (c+1)*chunk)
			best := 0.0
			for i := lo; i < hi; i++ {
				xx, xy, xz, yy, yz, zz := h.at(i)
				best = math.Max(best, xx*xx+yy*yy+zz*zz+2*(xy*xy+xz*xz+yz*yz))
			}
			partial[c] = best
		}
	})
	best := 0.0
	for _, v := range partial {
		best = math.Max(best, v)
	}
	return math.Sqrt(best), err
}

func (h hessianSlab) at(i int) (xx, xy, xz, yy, yz, zz float64) {
	if h.xx != nil {
		xx = float64(h.xx[i])
	}
	if h.xy != nil {
		xy = float64(h.xy[i])
	}
	if h.xz != nil {
		xz = float64(h.xz[i])
	}
	if h.yy != nil {
		yy = float64(h.yy[i])
	}
	if h.yz != nil {
		yz = float64(h.yz[i])
	}
	if h.zz != nil {
		zz = float64(h.zz[i])
	}
	return
}

// measureSlab folds the measure of the core slices [z0, z1) at scale σ into
// the result, keeping the first scale on ties.
func (p *vesselPlan) measureSlab(h hessianSlab, z0, z1 int, gamma, sigma float64, res *VesselnessResult) error {
	n := (z1 - z0) * p.nxy
	offset := z0 * p.nxy
	out := res.Response.Data[offset : offset+n]
	var scales []float32
	if res.ScaleMM != nil {
		scales = res.ScaleMM.Data[offset : offset+n]
	}
	const chunk = 1 << 14
	count := (n + chunk - 1) / chunk
	return runParallel(p.ctx, count, p.workers, func() func(int) {
		return func(c int) {
			lo, hi := c*chunk, min(n, (c+1)*chunk)
			for i := lo; i < hi; i++ {
				xx, xy, xz, yy, yz, zz := h.at(i)
				if p.dark {
					xx, xy, xz, yy, yz, zz = -xx, -xy, -xz, -yy, -yz, -zz
				}
				v := p.voxelMeasure(xx, xy, xz, yy, yz, zz, gamma)
				if f := float32(v); f > out[i] {
					out[i] = f
					if scales != nil {
						scales[i] = float32(sigma)
					}
				}
			}
		}
	})
}

// voxelMeasure evaluates the measure of one Hessian of a bright object.
func (p *vesselPlan) voxelMeasure(xx, xy, xz, yy, yz, zz, gamma float64) float64 {
	if p.measure == VesselnessSato {
		e := linalg.SymmetricEigenvalues3(xx, xy, xz, yy, yz, zz)
		return satoLineMeasure(e, p.alpha1, p.alpha2)
	}
	// A bright tube or blob needs its largest-magnitude eigenvalues
	// non-positive, which bounds the trace by the largest one: a positive
	// trace cannot respond, so its eigenvalues are not needed.
	if p.objectDim <= 1 && xx+yy+zz > 0 {
		return 0
	}
	var abs [3]float64
	var sorted [3]float64
	if p.imageDim == 2 {
		e := linalg.SymmetricEigenvalues2(xx, xy, yy)
		sorted[0], sorted[1] = e[0], e[1]
		if math.Abs(sorted[0]) > math.Abs(sorted[1]) {
			sorted[0], sorted[1] = sorted[1], sorted[0]
		}
	} else {
		e := linalg.SymmetricEigenvalues3(xx, xy, xz, yy, yz, zz)
		sorted = sortByMagnitude(e)
	}
	for i := range abs[:p.imageDim] {
		abs[i] = math.Abs(sorted[i])
	}
	return objectness(sorted[:p.imageDim], abs[:p.imageDim], p.objectDim, p.alpha, p.beta, gamma, p.scaleByL3)
}

// objectness is ITK's HessianToObjectnessMeasureImageFilter per-pixel
// measure for a bright object of dimension m in an image of dimension
// len(sorted), the eigenvalues sorted by magnitude (abs holds their
// magnitudes). gamma ≤ 0 leaves the structureness term out.
func objectness(sorted, abs []float64, m int, alpha, beta, gamma float64, scaleByLargest bool) float64 {
	n := len(sorted)
	for i := m; i < n; i++ {
		if sorted[i] > 0 {
			return 0
		}
	}
	v := 1.0
	if m < n-1 {
		den := 1.0
		for j := m + 1; j < n; j++ {
			den *= abs[j]
		}
		if !(den > 0) {
			return 0
		}
		rA := abs[m] / root(den, n-m-1)
		v *= 1 - math.Exp(-0.5*rA*rA/(alpha*alpha))
	}
	if m > 0 {
		den := 1.0
		for j := m; j < n; j++ {
			den *= abs[j]
		}
		if !(den > 0) {
			return 0
		}
		rB := abs[m-1] / root(den, n-m)
		v *= math.Exp(-0.5 * rB * rB / (beta * beta))
	}
	if gamma > 0 {
		s2 := 0.0
		for _, a := range abs {
			s2 += a * a
		}
		v *= 1 - math.Exp(-0.5*s2/(gamma*gamma))
	}
	if scaleByLargest {
		v *= abs[n-1]
	}
	return v
}

// root returns the k-th root of x ≥ 0 for k = 1, 2 or 3.
func root(x float64, k int) float64 {
	switch k {
	case 1:
		return x
	case 2:
		return math.Sqrt(x)
	default:
		return math.Cbrt(x)
	}
}

// satoLineMeasure is ITK's Hessian3DToVesselnessMeasureImageFilter measure
// of a bright line for eigenvalues sorted by value.
func satoLineMeasure(e [3]float64, alpha1, alpha2 float64) float64 {
	normalize := math.Min(-e[1], -e[0])
	if !(normalize > 0) {
		return 0
	}
	a := alpha2
	if e[2] <= 0 {
		a = alpha1
	}
	r := e[2] / (a * normalize)
	return math.Exp(-0.5*r*r) * normalize
}

func sortByMagnitude(e [3]float64) [3]float64 {
	if math.Abs(e[0]) > math.Abs(e[1]) {
		e[0], e[1] = e[1], e[0]
	}
	if math.Abs(e[1]) > math.Abs(e[2]) {
		e[1], e[2] = e[2], e[1]
	}
	if math.Abs(e[0]) > math.Abs(e[1]) {
		e[0], e[1] = e[1], e[0]
	}
	return e
}
