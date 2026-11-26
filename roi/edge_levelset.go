package roi

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/imagefilter"
	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// ErrCannyDomain reports holes/padding in a requested dense Canny domain.
var ErrCannyDomain = errors.New("roi: Canny requires a dense finite domain")

// ErrCannyNoEdges reports an image with no retained Canny edge voxels.
var ErrCannyNoEdges = errors.New("roi: Canny found no edges")

// EdgeLevelSetStopping applies only to the additive edge modes. The zero
// value preserves numerical-or-mask stopping; each reported reason is explicit.
type EdgeLevelSetStopping uint8

const (
	RMSOrStableMask EdgeLevelSetStopping = iota
	RMSOnly
	MaskStabilityOnly
	FixedIterations
)

// EdgeCurvatureConvention selects mean 1/r or the sum of principal curvatures.
type EdgeCurvatureConvention uint8

const (
	MeanCurvature EdgeCurvatureConvention = iota
	SumCurvature
)

// EdgeBoundaryPolicy either reports contacts or rejects the complete result.
type EdgeBoundaryPolicy uint8

const (
	ReportTouches EdgeBoundaryPolicy = iota
	RejectTouches
)

// LevelSetStopReason distinguishes numerical stopping from image convergence.
// Legacy APIs leave this new field unspecified to preserve their old results.
type LevelSetStopReason uint8

const (
	LevelSetStopUnspecified LevelSetStopReason = iota
	LevelSetStopIterationLimit
	LevelSetStopRMS
	LevelSetStopStableMask
	LevelSetStopEmptyBand
	LevelSetStopZeroTimeStep
	// LevelSetStopBoundaryActiveExhausted reports InterfaceLocal crossings
	// whose nearest anchors cannot support the centered derivative stencil.
	// It does not report RMS convergence or freeze the boundary embedding.
	LevelSetStopBoundaryActiveExhausted
)

// ErrEdgeLevelSetBoundary reports a completed contour refused by RejectTouches.
var ErrEdgeLevelSetBoundary = errors.New("roi: edge contour reached image or crop boundary")

func edgePolicies(stop EdgeLevelSetStopping, curvature EdgeCurvatureConvention, boundary EdgeBoundaryPolicy) error {
	if stop > FixedIterations || curvature > SumCurvature || boundary > RejectTouches {
		return fmt.Errorf("%w: edge stopping/curvature/boundary option", ErrLevelSetInvalidInput)
	}
	return nil
}
func edgeCurvature(c float64, convention EdgeCurvatureConvention, slices int) (float64, error) {
	if convention == SumCurvature && slices > 1 {
		c *= 2
	}
	if !finiteNonNegative(c) || c > math.MaxFloat32 {
		return 0, fmt.Errorf("%w: curvature convention scaling overflow", ErrLevelSetInvalidInput)
	}
	return c, nil
}
func edgeBoundaryResult(r *LevelSetResult, policy EdgeBoundaryPolicy) (*LevelSetResult, error) {
	if policy == RejectTouches && (r.TouchesRegionBoundary || r.TouchesVolumeBoundary || r.StopReason == LevelSetStopBoundaryActiveExhausted) {
		return nil, ErrEdgeLevelSetBoundary
	}
	return r, nil
}

// ShapeDetectionInput makes the input's interpretation explicit.
type ShapeDetectionInput uint8

const (
	// IntensityImage derives g through the Gaussian-gradient sigmoid.
	IntensityImage ShapeDetectionInput = iota
	// EdgePotentialImage uses supplied finite g in [0,1]. Edge must be zero.
	EdgePotentialImage
)

// ShapeDetectionParams selects propagation/curvature without advection:
// dphi/dt=-P*g*|grad(phi)|+C*g*kappa*|grad(phi)|, phi negative inside in mm.
// P is mm/pseudotime; C is mm²/pseudotime. The zero convention is mean
// curvature; SumCurvature uses the sum of principal curvatures.
// MaximumRMSChange is the existing tolerance multiplied by smallest spacing,
// not an absolute mm threshold. Zero stopping fields select existing defaults;
// the new APIs
// cap iterations at 10000 and limits at 16 Mi voxels/1 GiB.
type ShapeDetectionParams struct {
	// Evolution is opt-in; DenseBand preserves the existing solver.
	Evolution ShapeEvolutionProfile
	// SurfaceSampling applies only to InterfaceLocal.
	SurfaceSampling ShapeSurfaceSampling
	// Initialization applies only to InterfaceLocal; zero preserves CityBlockSeeds.
	Initialization                       ShapeInitializationProfile
	Stopping                             EdgeLevelSetStopping
	CurvatureConvention                  EdgeCurvatureConvention
	Boundary                             EdgeBoundaryPolicy
	Input                                ShapeDetectionInput
	Edge                                 EdgePotentialParams
	PropagationScaling, CurvatureScaling float64
	MaxIterations                        int
	MaximumRMSChange                     float64
}

// DefaultShapeDetectionParams derives g from intensity, P=1, C=1.
func DefaultShapeDetectionParams() ShapeDetectionParams {
	return ShapeDetectionParams{PropagationScaling: 1, CurvatureScaling: 1}
}

// CannyLevelSetParams refines toward Canny edges. D is the unsigned Euclidean
// distance in mm to retained edge voxel centres, a=D²/2, and
// dphi/dt=-P*D*|grad(phi)|+C*kappa*|grad(phi)|+A*grad(a).grad(phi).
// P and A are inverse pseudotime; C is mm²/pseudotime. This convention uses
// negative-inside phi; the public ITK Canny oracle uses ReverseExpansionDirection.
type CannyLevelSetParams struct {
	Stopping                                               EdgeLevelSetStopping
	CurvatureConvention                                    EdgeCurvatureConvention
	Boundary                                               EdgeBoundaryPolicy
	Canny                                                  imagefilter.CannyParameters
	PropagationScaling, CurvatureScaling, AdvectionScaling float64
	MaxIterations                                          int
	MaximumRMSChange                                       float64
}

// DefaultCannyLevelSetParams returns sigma 1 mm, P=0, C=.2, A=1.
func DefaultCannyLevelSetParams() CannyLevelSetParams {
	return CannyLevelSetParams{Canny: imagefilter.DefaultCannyParameters(), CurvatureScaling: .2, AdvectionScaling: 1}
}

func edgeStopping(iterations int, rms float64) (int, float64, error) {
	if iterations > 10000 {
		return 0, 0, fmt.Errorf("%w: at most 10000 iterations", ErrLevelSetResourceLimit)
	}
	return levelSetStopping(iterations, rms)
}
func edgeScalings(p, c, a float64) error {
	if math.IsNaN(p) || math.IsInf(p, 0) || math.Abs(p) > math.MaxFloat32 || !finiteNonNegative(c) || c > math.MaxFloat32 || !finiteNonNegative(a) || a > math.MaxFloat32 {
		return fmt.Errorf("%w: edge scalings", ErrLevelSetInvalidInput)
	}
	return nil
}

// ShapeDetectionLevelSet shares the existing crop, initialization, unavailable
// voxel boundaries, signed-distance rebuilding and stopping engine. Results
// own their masks. Stable caller input/callback/options are required.
func ShapeDetectionLevelSet(ctx context.Context, volume LevelSetVolume, init LevelSetInitialization, p ShapeDetectionParams, o LevelSetOptions) (*LevelSetResult, error) {
	return shapeDetectionLevelSet(ctx, volume, init, p, o, nil)
}

func shapeDetectionLevelSet(ctx context.Context, volume LevelSetVolume, init LevelSetInitialization, p ShapeDetectionParams, o LevelSetOptions, prior *pcaShapeEvolution) (*LevelSetResult, error) {
	if err := shapeProfileOptions(p); err != nil {
		return nil, err
	}
	if e := edgePolicies(p.Stopping, p.CurvatureConvention, p.Boundary); e != nil {
		return nil, e
	}
	var scalingErr error
	p.CurvatureScaling, scalingErr = edgeCurvature(p.CurvatureScaling, p.CurvatureConvention, volume.Slices)
	if scalingErr != nil {
		return nil, scalingErr
	}
	if !finiteNonNegative(p.Edge.SigmaMM) || math.IsNaN(p.Edge.Alpha) || math.IsInf(p.Edge.Alpha, 0) || math.IsNaN(p.Edge.Beta) || math.IsInf(p.Edge.Beta, 0) {
		return nil, fmt.Errorf("%w: shape Edge parameters", ErrLevelSetInvalidInput)
	}
	if p.Input != IntensityImage && p.Input != EdgePotentialImage {
		return nil, fmt.Errorf("%w: shape input mode", ErrLevelSetInvalidInput)
	}
	if p.Input == EdgePotentialImage && p.Edge != (EdgePotentialParams{}) {
		return nil, fmt.Errorf("%w: Edge parameters with provided potential", ErrLevelSetInvalidInput)
	}
	if err := edgeScalings(p.PropagationScaling, p.CurvatureScaling, 0); err != nil {
		return nil, err
	}
	it, tol, err := edgeStopping(p.MaxIterations, p.MaximumRMSChange)
	if err != nil {
		return nil, err
	}
	g, err := newEdgeLevelSetGridProfile(ctx, volume, init, o, p.Evolution == InterfaceLocal, p.Evolution == InterfaceLocal)
	if err != nil {
		return nil, err
	}
	if prior != nil && g.unavailable != nil {
		return nil, fmt.Errorf("%w: shape prior requires a finite dense crop", ErrPCAShapeInvalid)
	}
	var edge []float32
	var alpha, beta float64
	if p.Input == EdgePotentialImage {
		edge = make([]float32, g.n)
		for i, v := range g.image {
			if i%4096 == 0 {
				if e := levelSetContextError(g.ctx); e != nil {
					return nil, e
				}
			}
			if g.available(i) && !(v >= 0 && v <= 1) {
				return nil, fmt.Errorf("%w: potential outside [0,1]", ErrLevelSetInvalidInput)
			}
			if g.available(i) {
				edge[i] = v
			}
		}
	} else {
		edge, alpha, beta, err = g.edgePotential(p.Edge, init, levelSetEdgeQuantile)
		if err != nil {
			return nil, err
		}
	}
	phi, err := g.initialLevelSet(init, edge)
	if err != nil {
		return nil, err
	}
	terms := levelSetTerms{stopping: p.Stopping, propagation: float32(p.PropagationScaling), curvature: float32(p.CurvatureScaling), speed: edge, curvatureWeight: edge}
	if prior != nil {
		if err = prior.prepare(g); err != nil {
			return nil, err
		}
		terms.shapePrior = prior
	}
	var r *LevelSetResult
	if p.Evolution == InterfaceLocal {
		r, err = g.evolveInterfaceWithInitialization(phi, terms, p.SurfaceSampling, p.Initialization, it, tol, o.Progress)
	} else {
		r, err = g.evolve(phi, terms, it, tol, o.Progress)
	}
	if err != nil {
		return nil, err
	}
	if err = levelSetContextError(g.ctx); err != nil {
		return nil, err
	}
	r.Alpha, r.Beta = alpha, beta
	return edgeBoundaryResult(r, p.Boundary)
}

// CannyLevelSet requires a dense finite regular cropped domain. Holes/padding
// and no-edge images produce explicit errors, without a fallback or result.
// Seeds initialize a geometric ball with unit speed; D is not seed speed.
func CannyLevelSet(ctx context.Context, volume LevelSetVolume, init LevelSetInitialization, p CannyLevelSetParams, o LevelSetOptions) (*LevelSetResult, error) {
	if e := edgePolicies(p.Stopping, p.CurvatureConvention, p.Boundary); e != nil {
		return nil, e
	}
	var scalingErr error
	p.CurvatureScaling, scalingErr = edgeCurvature(p.CurvatureScaling, p.CurvatureConvention, volume.Slices)
	if scalingErr != nil {
		return nil, scalingErr
	}
	if err := edgeScalings(p.PropagationScaling, p.CurvatureScaling, p.AdvectionScaling); err != nil {
		return nil, err
	}
	it, tol, err := edgeStopping(p.MaxIterations, p.MaximumRMSChange)
	if err != nil {
		return nil, err
	}
	dimensions := 3
	if volume.Slices == 1 {
		dimensions = 2
	}
	p.Canny, err = p.Canny.Normalize(dimensions)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLevelSetInvalidInput, err)
	}
	g, err := newEdgeLevelSetGrid(ctx, volume, init, o, true)
	if err != nil {
		return nil, err
	}
	edges, err := imagefilter.Canny(g.ctx, &imagefilter.Volume{Dims: g.size, SpacingMM: g.h, Data: g.image}, imagefilter.CannyOptions{CannyParameters: p.Canny, Workers: g.workers})
	if err != nil {
		return nil, g.filterError(err)
	}
	if edges.EdgeCount == 0 {
		return nil, ErrCannyNoEdges
	}
	distance, field, err := g.cannyDistanceFields(edges.Edges.Data)
	if err != nil {
		return nil, err
	}
	unit := make([]float32, g.n)
	for i := range unit {
		if i%4096 == 0 {
			if e := levelSetContextError(g.ctx); e != nil {
				return nil, e
			}
		}
		unit[i] = 1
	}
	phi, err := g.initialLevelSet(init, unit)
	if err != nil {
		return nil, err
	}
	r, err := g.evolve(phi, levelSetTerms{stopping: p.Stopping, propagation: float32(p.PropagationScaling), curvature: float32(p.CurvatureScaling), advection: float32(p.AdvectionScaling), speed: distance, advectionField: field}, it, tol, o.Progress)
	if err != nil {
		return nil, err
	}
	if err = levelSetContextError(g.ctx); err != nil {
		return nil, err
	}
	return edgeBoundaryResult(r, p.Boundary)
}

func (g *levelSetGrid) filterError(err error) error {
	if e := levelSetContextError(g.ctx); e != nil {
		return e
	}
	if errors.Is(err, imagefilter.ErrCannyResourceLimit) {
		return fmt.Errorf("%w: %w", ErrLevelSetResourceLimit, err)
	}
	return fmt.Errorf("%w: %w", ErrLevelSetInvalidInput, err)
}

func newEdgeLevelSetGrid(ctx context.Context, v LevelSetVolume, init LevelSetInitialization, o LevelSetOptions, dense bool) (*levelSetGrid, error) {
	return newEdgeLevelSetGridProfile(ctx, v, init, o, dense, false)
}

func newEdgeLevelSetGridProfile(ctx context.Context, v LevelSetVolume, init LevelSetInitialization, o LevelSetOptions, dense, interfaceLocal bool) (*levelSetGrid, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if e := levelSetContextError(ctx); e != nil {
		return nil, e
	}
	// New edge fields are float32 in physical units. Reject extents that
	// cannot retain finite phi/D² rather than accepting an infinite field.
	for a, n := range [3]int{v.Columns, v.Rows, v.Slices} {
		if a == 2 && n == 1 {
			continue
		}
		h := v.SpacingMM[a]
		if !(h >= 2*float64(math.SmallestNonzeroFloat32) && h <= math.MaxFloat64) || !(float64(n)*h <= math.Sqrt(math.MaxFloat32)/4) {
			return nil, fmt.Errorf("%w: physical edge spacing/extent axis %d", ErrLevelSetInvalidInput, a)
		}
	}
	if !finiteNonNegative(init.InitialRadiusMM) || init.InitialRadiusMM > math.MaxFloat32/4 {
		return nil, fmt.Errorf("%w: edge initialization radius range", ErrLevelSetInvalidInput)
	}
	l, e := o.Limits.normalize()
	if e != nil {
		return nil, e
	}
	if l.MaxVoxels > defaultLevelSetMaxVoxels || l.MaxBytes > defaultLevelSetMaxBytes {
		return nil, fmt.Errorf("%w: new edge limits exceed finite ceilings", ErrLevelSetInvalidInput)
	}
	// Bound metadata traversal before a mask helper or repeated seed work.
	if len(init.Seeds) > 64 || v.Columns <= 0 || v.Rows <= 0 || v.Slices <= 0 || v.Columns > defaultLevelSetMaxVoxels || v.Slices > defaultLevelSetMaxVoxels || v.Rows > l.MaxVoxels || len(init.Masks) > l.MaxVoxels/v.Rows {
		return nil, fmt.Errorf("%w: initialization dimensions/visits", ErrLevelSetResourceLimit)
	}
	visits := int64(len(init.Seeds))
	for z, m := range init.Masks {
		if m == nil {
			continue
		}
		if z < 0 || z >= v.Slices || m.Columns != v.Columns || m.Rows != v.Rows {
			return nil, fmt.Errorf("%w: initial mask", ErrLevelSetInvalidInput)
		}
		for y, runs := range m.runs {
			if y%4096 == 0 {
				if e := levelSetContextError(ctx); e != nil {
					return nil, e
				}
			}
			visits++
			end := 0
			for k, run := range runs {
				if interfaceLocal && k%4096 == 0 {
					if e := levelSetContextError(ctx); e != nil {
						return nil, e
					}
				}
				if run.Start < end || run.Start < 0 || run.End <= run.Start || run.End > m.Columns {
					return nil, fmt.Errorf("%w: initial mask run", ErrLevelSetInvalidInput)
				}
				end = run.End
				visits += int64(run.End-run.Start) + 1
				if visits > int64(l.MaxVoxels)*4 {
					return nil, fmt.Errorf("%w: initial mask visits", ErrLevelSetResourceLimit)
				}
			}
		}
	}
	workers := parallel.DefaultWorkers()
	reserve := func(g *levelSetGrid) error {
		g.bounded = true
		g.dense = dense
		g.workers = workers
		g.interfaceLocal = interfaceLocal
		// Every dense array, retained lazy heap capacity, band/rates, output RLE
		// capacity is included in 192N. Full source-height output headers are
		// additional even for a tiny cropped sub-volume.
		bytes := int64(g.n)*192 + int64(v.Rows)*int64(g.nz)*32 + int64(g.nz)*128 + int64(len(init.Seeds))*256 + 4096
		if interfaceLocal {
			if g.nx < 3 || g.ny < 3 || g.nz != 1 && g.nz < 3 {
				return fmt.Errorf("%w: InterfaceLocal requires at least 3 samples on active axes", ErrLevelSetInvalidInput)
			}
			bytes += int64(g.n)*32 + int64(workers)*128
		}
		scratch, e := imagefilter.CannyGaussianScratchBytes(g.size, workers)
		if e != nil {
			return g.filterError(e)
		}
		bytes += scratch
		edtScratch := int64(0)
		for a, n := range g.size {
			lines := g.n / n
			w := min(max(1, lines/16), 16)
			edtScratch = max(edtScratch, int64(w)*(int64(n)*32+8))
			bytes += int64(g.size[a]) * 8
		}
		bytes += edtScratch
		if dense && !interfaceLocal {
			if g.nx < 2 || g.ny < 2 {
				return fmt.Errorf("%w: Canny crop must have at least 2x2 samples", ErrLevelSetInvalidInput)
			}
			cbytes, e := imagefilter.CannyWorkingBytes(g.size, workers)
			if e != nil {
				return g.filterError(e)
			}
			if cbytes > 512<<20 {
				return fmt.Errorf("%w: Canny working bytes", ErrLevelSetResourceLimit)
			}
		}
		if bytes > l.MaxBytes {
			return fmt.Errorf("%w: reserved %d bytes exceeds %d", ErrLevelSetResourceLimit, bytes, l.MaxBytes)
		}
		// Estimated sigmoid samples may repeat between seeds. Bound their total
		// retained capacity to 4N; seed-distance work is bounded by 64N.
		sampleVisits := int64(0)
		radius := init.InitialRadiusMM
		if radius == 0 {
			radius = g.defaultInitialRadius()
		}
		for _, seed := range init.Seeds {
			box := int64(1)
			pos := [3]int{seed.X - g.origin[0], seed.Y - g.origin[1], seed.Slice - g.origin[2]}
			for a, n := range g.size {
				r := int(math.Min(float64(n), math.Floor(radius/g.h[a])))
				box *= int64(min(n-1, pos[a]+r) - max(0, pos[a]-r) + 1)
			}
			sampleVisits += box
			if sampleVisits > int64(g.n)*4 {
				return fmt.Errorf("%w: seed sampling visits", ErrLevelSetResourceLimit)
			}
		}
		return nil
	}
	return newLevelSetGridReserved(ctx, v, init, marginFunc(o.MarginMM), o, reserve)
}

func (g *levelSetGrid) cannyDistanceFields(edges []float32) ([]float32, []float32, error) {
	mask := &VolumeMask{Columns: g.nx, Rows: g.ny, Slices: g.nz, Voxels: make([]bool, g.n)}
	axes := [3][]float64{}
	for a, n := range g.size {
		axes[a] = make([]float64, n)
		for i := range axes[a] {
			if i%4096 == 0 {
				if e := levelSetContextError(g.ctx); e != nil {
					return nil, nil, e
				}
			}
			axes[a][i] = float64(i) * g.h[a]
		}
	}
	mask.X, mask.Y, mask.Z = axes[0], axes[1], axes[2]
	for i, v := range edges {
		if i%4096 == 0 {
			if e := levelSetContextError(g.ctx); e != nil {
				return nil, nil, e
			}
		}
		mask.Voxels[i] = v != 0
	}
	d, e := mask.DistanceTransform(g.ctx, true)
	if e != nil {
		return nil, nil, g.filterError(e)
	}
	speed, field := make([]float32, g.n), make([]float32, g.n)
	for i, v := range d {
		if i%4096 == 0 {
			if e := levelSetContextError(g.ctx); e != nil {
				return nil, nil, e
			}
		}
		a := v * v / 2
		if !(v >= 0 && v <= math.MaxFloat32 && a <= math.MaxFloat32) {
			return nil, nil, fmt.Errorf("%w: non-finite Canny distance/D squared", ErrLevelSetInvalidInput)
		}
		speed[i], field[i] = float32(v), float32(a)
	}
	return speed, field, nil
}

// Exact nearest-rank selection with bounded cancellation spans; only the
// private copy is partitioned, and the returned rank matches quantile32.
func quantile32Context(ctx context.Context, values []float32, q float64) (float64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if e := levelSetContextError(ctx); e != nil {
		return 0, e
	}
	if len(values) == 0 {
		return 0, nil
	}
	data := make([]float32, len(values))
	for i, v := range values {
		if i%4096 == 0 {
			if e := levelSetContextError(ctx); e != nil {
				return 0, e
			}
		}
		data[i] = v
	}
	rank := int(q*float64(len(data)-1) + .5)
	lo, hi := 0, len(data)-1
	steps := 0
	for lo < hi {
		pivot := data[lo+(hi-lo)/2]
		i, j := lo, hi
		for i <= j {
			for data[i] < pivot {
				i++
				steps++
				if steps%4096 == 0 {
					if e := levelSetContextError(ctx); e != nil {
						return 0, e
					}
				}
			}
			for data[j] > pivot {
				j--
				steps++
				if steps%4096 == 0 {
					if e := levelSetContextError(ctx); e != nil {
						return 0, e
					}
				}
			}
			if i <= j {
				data[i], data[j] = data[j], data[i]
				i++
				j--
				steps++
				if steps%4096 == 0 {
					if e := levelSetContextError(ctx); e != nil {
						return 0, e
					}
				}
			}
		}
		if rank <= j {
			hi = j
		} else if rank >= i {
			lo = i
		} else {
			break
		}
	}
	return float64(data[rank]), levelSetContextError(ctx)
}

// In-place heapsort uses no extra array; only bounded new requests use this
// cancellable sort. Numeric ordering is identical to slices.Sort.
func sortLevelSetBand(ctx context.Context, data []int32) error {
	steps := 0
	var down func(int, int) error
	down = func(root, end int) error {
		for root*2+1 < end {
			steps++
			if steps%4096 == 0 {
				if e := levelSetContextError(ctx); e != nil {
					return e
				}
			}
			child := root*2 + 1
			if child+1 < end && data[child] < data[child+1] {
				child++
			}
			if data[root] >= data[child] {
				return nil
			}
			data[root], data[child] = data[child], data[root]
			root = child
		}
		return nil
	}
	for i := len(data)/2 - 1; i >= 0; i-- {
		if e := down(i, len(data)); e != nil {
			return e
		}
	}
	for end := len(data) - 1; end > 0; end-- {
		data[0], data[end] = data[end], data[0]
		if e := down(0, end); e != nil {
			return e
		}
	}
	return levelSetContextError(ctx)
}

// levelSetMaskRuns preserves row/run order while admitting cancellation in
// empty rows and between runs, before entering potentially long pixel loops.
func levelSetMaskRuns(ctx context.Context, m *RasterMask, fn func(int, MaskRun) error) error {
	if m == nil {
		return nil
	}
	visited := 0
	for y, runs := range m.runs {
		if y%4096 == 0 {
			if e := levelSetContextError(ctx); e != nil {
				return e
			}
		}
		for _, r := range runs {
			if visited%4096 == 0 {
				if e := levelSetContextError(ctx); e != nil {
					return e
				}
			}
			visited++
			if e := fn(y, r); e != nil {
				return e
			}
		}
	}
	return levelSetContextError(ctx)
}
