package roi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"

	"github.com/ThalesMMS/dicom-go/imagefilter"
	"github.com/ThalesMMS/dicom-go/internal/parallel"
	"github.com/ThalesMMS/dicom-go/render"
)

// Level set segmentation started by fast marching (#1178): a threshold on
// the fast marching arrival time over an edge speed, and two narrow-band
// level set evolutions, the geodesic active contour (edge driven) and the
// threshold level set (intensity range driven), for smart contouring from a
// click and for refining the border of a region such as a Growing Region.
//
// The pipeline follows the behaviour of ITK's
// GradientMagnitudeRecursiveGaussianImageFilter, SigmoidImageFilter,
// FastMarchingImageFilter, GeodesicActiveContourLevelSetImageFilter and
// ThresholdSegmentationLevelSetImageFilter (ITK 519a94d954, Apache-2.0,
// Copyright NumFOCUS). This is an independent implementation written from the
// published algorithms: Caselles, Kimmel and Sapiro, "Geodesic active
// contours" (IJCV 22, 1997); Sethian's fast marching and upwind schemes
// ("Level Set Methods and Fast Marching Methods", 1999); Adalsteinsson and
// Sethian's narrow band ("A fast level set method for propagating
// interfaces", JCP 118, 1995); and Lefohn, Cates and Whitaker's threshold
// speed ("Interactive, GPU-based level sets for 3D segmentation", MICCAI
// 2003). Differences from ITK: the evolution runs on a dense sub-volume
// around the initial region, bounded in millimetres, by a voxel and a memory
// budget and by a context; the level set is a signed distance in mm
// (negative inside) rebuilt by a band-limited fast marching whenever the
// front has moved half a voxel, instead of ITK's sparse field layers; the
// start around a seed is a ball cut by the fast marching arrival time rather
// than the arrival time itself; the curvature term is the mean curvature
// (1/r on a sphere and on a circle); the evolution also stops once the
// region stops changing over windows of iterations; the threshold speed is
// normalized to [-1, 1] (1 at the middle of the range, 0 at its bounds);
// the sigmoid parameters are estimated from the image when not given; and
// unavailable voxels never join a region and act as a zero-flux boundary.

var (
	// ErrLevelSetInvalidInput reports a volume, initial region or parameter
	// the level set functions cannot use.
	ErrLevelSetInvalidInput = errors.New("roi: invalid level set input")
	// ErrLevelSetEmpty reports an initialization without any available seed
	// or mask voxel.
	ErrLevelSetEmpty = errors.New("roi: level set has no initial region")
	// ErrLevelSetResourceLimit reports a sub-volume larger than the voxel or
	// memory budget.
	ErrLevelSetResourceLimit = errors.New("roi: level set resource limit exceeded")
	// ErrLevelSetCancelled reports cancellation; the error also wraps
	// context.Canceled or context.DeadlineExceeded.
	ErrLevelSetCancelled = errors.New("roi: level set cancelled")
)

const (
	defaultLevelSetMaxVoxels     = 16 << 20
	defaultLevelSetMaxBytes      = 1 << 30
	defaultLevelSetMarginMM      = 30
	defaultLevelSetMaxIterations = 1000
	defaultLevelSetRMSChange     = 0.005
	// levelSetBytesPerVoxel estimates the working memory of one sub-volume
	// voxel: image, edge potential and level set (float32), availability
	// and fast marching state, plus the fast marching initialization
	// (float64 speed and time, int32 heap position) and the Gaussian
	// derivative buffers.
	levelSetBytesPerVoxel = 64
	// levelSetBandVoxels is the half width of the narrow band in voxels of
	// the largest spacing.
	levelSetBandVoxels = 3
	// levelSetParallelBand is the band size from which the speeds are
	// computed by several goroutines, each on at least half of it: below,
	// waking the workers costs more than it saves.
	levelSetParallelBand = 32768
)

// LevelSetVolume is the image a level set evolves in: Columns×Rows×Slices
// voxels (Slices = 1 for a single image) spaced SpacingMM apart along x, y
// and z (the z spacing of a single image is ignored). ValueAt reports false
// for unavailable voxels (padding, undecodable frames, slices of another
// size); those never join a region. ValueAt is only called on the
// sub-volume around the initial region.
type LevelSetVolume struct {
	Columns, Rows, Slices int
	SpacingMM             [3]float64
	ValueAt               func(x, y, slice int) (float64, bool)
}

// LevelSetInitialization is the region a contour starts from: the ball of
// InitialRadiusMM around each seed, cut where the fast marching arrival
// time over the speed rises steeply (so the start does not cross a strong
// edge near the click), united with Masks.
type LevelSetInitialization struct {
	// Seeds are voxel indices (column, row, slice ordinal).
	Seeds []RegionGrowSeed
	// InitialRadiusMM is the radius of the start around each seed (three
	// times the smallest spacing, and at least twice the largest, when
	// zero: a smaller ball is too coarse on the grid and its curvature
	// would make it collapse).
	InitialRadiusMM float64
	// Masks holds an initial region per slice ordinal, for example a grown
	// region to refine. Masks must be Columns×Rows.
	Masks map[int]*RasterMask
}

// LevelSetLimits bounds the work of one call. A zero field uses the
// default; negative fields are rejected.
type LevelSetLimits struct {
	// MaxVoxels caps the sub-volume voxels (default 16 Mi).
	MaxVoxels int
	// MaxBytes caps the estimated working memory (default 1 GiB, about 64
	// bytes per sub-volume voxel).
	MaxBytes int64
}

// LevelSetOptions are shared by the level set functions.
type LevelSetOptions struct {
	// MarginMM extends the bounding box of the initial region on every side
	// to give the sub-volume the contour evolves in (default 30 mm). The
	// fast marching threshold ignores it and uses its stopping time.
	MarginMM float64
	Limits   LevelSetLimits
	// Progress, when set, is called on the calling goroutine after every
	// iteration of an evolution.
	Progress func(LevelSetProgress)
}

// LevelSetProgress reports an evolution iteration.
type LevelSetProgress struct {
	Iteration, MaxIterations int
	// RMSChange is the root mean square change of the level set (mm) at
	// the front in this iteration.
	RMSChange float64
}

// EdgePotentialParams configure the edge potential g, the speed that is 1
// in homogeneous regions and falls towards 0 on edges: the gradient
// magnitude G of the image at scale SigmaMM (recursive Gaussian) mapped by
// the sigmoid g = 1/(1+exp(-(G-Beta)/Alpha)) with a negative Alpha.
type EdgePotentialParams struct {
	// SigmaMM is the Gaussian scale of the gradient (the larger of 1 mm and
	// the smallest spacing when zero).
	SigmaMM float64
	// Alpha and Beta are the sigmoid width and centre. When Alpha is zero
	// both are estimated as ITK's guide suggests, Alpha = -(K1-K2)/6 and
	// Beta = (K1+K2)/2, with K2 the median gradient around the seeds (or
	// inside the masks) and K1 an edge level: the median gradient on the
	// mask border, or a quantile of the sub-volume gradients that stand out
	// from K2 (the upper quartile for the fast marching threshold, the
	// median for the geodesic active contour).
	Alpha, Beta float64
}

// FastMarchingSegmentParams configure FastMarchingSegment.
type FastMarchingSegmentParams struct {
	Edge EdgePotentialParams
	// StoppingTime is the arrival time that bounds the region (required).
	// The edge speed is at most 1, so it is the largest distance in mm the
	// region can reach from a seed; edges slow the front and stop it.
	StoppingTime float64
}

// GeodesicActiveContourParams configure GeodesicActiveContour. The
// evolution is
//
//	∂φ/∂t = -P·g·|∇φ| + C·g·κ|∇φ| + A·∇g·∇φ
//
// for a level set φ negative inside: P inflates (or, negative, deflates) the
// contour where the edge potential g is high, C smooths it by its mean
// curvature κ, and A draws it onto the edges, the valleys of g.
type GeodesicActiveContourParams struct {
	Edge EdgePotentialParams
	// PropagationScaling, CurvatureScaling and AdvectionScaling are P, C and
	// A, taken as given (see DefaultGeodesicActiveContourParams). C and A
	// must not be negative.
	PropagationScaling, CurvatureScaling, AdvectionScaling float64
	// MaxIterations bounds the evolution (default 1000).
	MaxIterations int
	// MaximumRMSChange ends the evolution once the root mean square change
	// of φ at the front in one iteration, in units of the smallest spacing,
	// falls below it (default 0.005). The evolution also ends once two
	// consecutive windows of 10 iterations each move at most 0.2% of the
	// front voxels to the other side, as a front on a noisy speed chatters
	// within a voxel without moving.
	MaximumRMSChange float64
}

// DefaultGeodesicActiveContourParams returns P = C = A = 1 with the
// estimated edge potential.
func DefaultGeodesicActiveContourParams() GeodesicActiveContourParams {
	return GeodesicActiveContourParams{PropagationScaling: 1, CurvatureScaling: 1, AdvectionScaling: 1}
}

// ThresholdLevelSetParams configure ThresholdLevelSet. The evolution is
//
//	∂φ/∂t = -P·s(I)·|∇φ| + C·κ|∇φ|
//
// with s(I) = 1 - |I - (Lower+Upper)/2| / ((Upper-Lower)/2) clamped to
// [-1, 1]: the contour grows over values in [Lower, Upper] and retreats
// from the others, smoothed by its mean curvature κ.
type ThresholdLevelSetParams struct {
	Lower, Upper float64
	// PropagationScaling and CurvatureScaling are P and C, taken as given
	// (see DefaultThresholdLevelSetParams). C must not be negative.
	PropagationScaling, CurvatureScaling float64
	// MaxIterations and MaximumRMSChange stop the evolution as in
	// GeodesicActiveContourParams.
	MaxIterations    int
	MaximumRMSChange float64
}

// DefaultThresholdLevelSetParams returns P = 1 and C = 0.2 over [lower,
// upper]. The speed is normalized to [-1, 1] (ITK's is in intensity units,
// which makes its curvature term relatively weak), and a heavier curvature
// makes a small start collapse on a noisy speed when the range holds the
// structure's values only within a few SD.
func DefaultThresholdLevelSetParams(lower, upper float64) ThresholdLevelSetParams {
	return ThresholdLevelSetParams{Lower: lower, Upper: upper, PropagationScaling: 1, CurvatureScaling: 0.2}
}

// LevelSetResult is a segmented region.
type LevelSetResult struct {
	// Masks holds the region's mask per slice ordinal it touches.
	Masks      map[int]*RasterMask
	VoxelCount int
	// VolumeMM3 is VoxelCount times the voxel volume (area in mm² for a
	// single image).
	VolumeMM3 float64
	// Iterations is the number of evolution iterations (0 for the fast
	// marching threshold); Converged reports that the evolution stopped
	// before MaxIterations (see MaximumRMSChange); RMSChange is the last
	// RMS change at the front, in mm.
	Iterations int
	Converged  bool
	RMSChange  float64
	// RegionOrigin and RegionSize are the sub-volume (voxel indices) the
	// contour evolved in. TouchesRegionBoundary reports a region reaching a
	// side of the sub-volume that is not a side of the volume: it may have
	// been cut there, and a larger margin may be needed.
	RegionOrigin, RegionSize [3]int
	TouchesRegionBoundary    bool
	// StopReason and source-boundary contact are populated only by new edge APIs.
	StopReason            LevelSetStopReason
	TouchesVolumeBoundary bool
	// Alpha and Beta are the sigmoid of the edge potential that was used
	// (zero for the threshold level set).
	Alpha, Beta float64
}

func (l LevelSetLimits) normalize() (LevelSetLimits, error) {
	if l.MaxVoxels < 0 || l.MaxBytes < 0 {
		return LevelSetLimits{}, fmt.Errorf("%w: negative limit", ErrLevelSetInvalidInput)
	}
	if l.MaxVoxels == 0 {
		l.MaxVoxels = defaultLevelSetMaxVoxels
	}
	if l.MaxBytes == 0 {
		l.MaxBytes = defaultLevelSetMaxBytes
	}
	return l, nil
}

func levelSetContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrLevelSetCancelled, err)
	}
	return nil
}

func finiteNonNegative(v float64) bool { return v >= 0 && !math.IsInf(v, 0) && !math.IsNaN(v) }

// FastMarchingSegment segments the voxels whose fast marching arrival time
// from the seeds, over the edge potential, is at most StoppingTime: the
// first, single-pass smart contour. Several seeds grow one region.
func FastMarchingSegment(ctx context.Context, volume LevelSetVolume, seeds []RegionGrowSeed, params FastMarchingSegmentParams, options LevelSetOptions) (*LevelSetResult, error) {
	if !(params.StoppingTime > 0) || math.IsInf(params.StoppingTime, 0) {
		return nil, fmt.Errorf("%w: stopping time %v", ErrLevelSetInvalidInput, params.StoppingTime)
	}
	init := LevelSetInitialization{Seeds: seeds}
	g, err := newLevelSetGrid(ctx, volume, init, func(h float64) float64 { return params.StoppingTime + 2*h }, options)
	if err != nil {
		return nil, err
	}
	if len(g.seeds) == 0 {
		return nil, ErrLevelSetEmpty
	}
	edge, alpha, beta, err := g.edgePotential(params.Edge, init, levelSetCrestQuantile)
	if err != nil {
		return nil, err
	}
	times, err := g.fastMarchingTimes(edge, params.StoppingTime)
	if err != nil {
		return nil, err
	}
	result := g.result(func(i int) bool { return times[i] <= params.StoppingTime })
	if result == nil {
		return nil, levelSetContextError(g.ctx)
	}
	result.Alpha, result.Beta = alpha, beta
	return result, nil
}

// GeodesicActiveContour evolves a geodesic active contour from init (see
// GeodesicActiveContourParams) and returns the region inside it.
func GeodesicActiveContour(ctx context.Context, volume LevelSetVolume, init LevelSetInitialization, params GeodesicActiveContourParams, options LevelSetOptions) (*LevelSetResult, error) {
	if !finiteNonNegative(params.CurvatureScaling) || !finiteNonNegative(params.AdvectionScaling) ||
		math.IsNaN(params.PropagationScaling) || math.IsInf(params.PropagationScaling, 0) {
		return nil, fmt.Errorf("%w: scaling", ErrLevelSetInvalidInput)
	}
	iterations, tolerance, err := levelSetStopping(params.MaxIterations, params.MaximumRMSChange)
	if err != nil {
		return nil, err
	}
	g, err := newLevelSetGrid(ctx, volume, init, marginFunc(options.MarginMM), options)
	if err != nil {
		return nil, err
	}
	edge, alpha, beta, err := g.edgePotential(params.Edge, init, levelSetEdgeQuantile)
	if err != nil {
		return nil, err
	}
	phi, err := g.initialLevelSet(init, edge)
	if err != nil {
		return nil, err
	}
	terms := levelSetTerms{
		propagation: float32(params.PropagationScaling), curvature: float32(params.CurvatureScaling), advection: float32(params.AdvectionScaling),
		speed: edge, curvatureWeight: edge, advectionField: edge,
	}
	result, err := g.evolve(phi, terms, iterations, tolerance, options.Progress)
	if err != nil {
		return nil, err
	}
	result.Alpha, result.Beta = alpha, beta
	return result, nil
}

// ThresholdLevelSet evolves a threshold level set from init (see
// ThresholdLevelSetParams) and returns the region inside it. Seeds start
// over the voxels in [Lower, Upper] only.
func ThresholdLevelSet(ctx context.Context, volume LevelSetVolume, init LevelSetInitialization, params ThresholdLevelSetParams, options LevelSetOptions) (*LevelSetResult, error) {
	if math.IsNaN(params.Lower) || math.IsNaN(params.Upper) || params.Lower > params.Upper ||
		math.IsInf(params.Lower, 0) || math.IsInf(params.Upper, 0) {
		return nil, fmt.Errorf("%w: interval [%v, %v]", ErrLevelSetInvalidInput, params.Lower, params.Upper)
	}
	if !finiteNonNegative(params.CurvatureScaling) || math.IsNaN(params.PropagationScaling) || math.IsInf(params.PropagationScaling, 0) {
		return nil, fmt.Errorf("%w: scaling", ErrLevelSetInvalidInput)
	}
	iterations, tolerance, err := levelSetStopping(params.MaxIterations, params.MaximumRMSChange)
	if err != nil {
		return nil, err
	}
	g, err := newLevelSetGrid(ctx, volume, init, marginFunc(options.MarginMM), options)
	if err != nil {
		return nil, err
	}
	speed := make([]float32, g.n)
	mid, half := (params.Lower+params.Upper)/2, (params.Upper-params.Lower)/2
	if half <= 0 {
		half = math.SmallestNonzeroFloat32
	}
	for i, v := range g.image {
		s := float32(-1)
		if g.available(i) {
			s = float32(math.Max(-1, math.Min(1, 1-math.Abs(float64(v)-mid)/half)))
		}
		speed[i] = s
	}
	phi, err := g.initialLevelSet(init, speed)
	if err != nil {
		return nil, err
	}
	terms := levelSetTerms{propagation: float32(params.PropagationScaling), curvature: float32(params.CurvatureScaling), speed: speed}
	return g.evolve(phi, terms, iterations, tolerance, options.Progress)
}

func levelSetStopping(iterations int, tolerance float64) (int, float64, error) {
	if iterations < 0 || !finiteNonNegative(tolerance) {
		return 0, 0, fmt.Errorf("%w: stopping criteria", ErrLevelSetInvalidInput)
	}
	if iterations == 0 {
		iterations = defaultLevelSetMaxIterations
	}
	if tolerance == 0 {
		tolerance = defaultLevelSetRMSChange
	}
	return iterations, tolerance, nil
}

func marginFunc(margin float64) func(float64) float64 {
	return func(float64) float64 {
		if margin == 0 {
			return defaultLevelSetMarginMM
		}
		return margin
	}
}

// levelSetGrid is the dense sub-volume a level set works on.
type levelSetGrid struct {
	ctx        context.Context
	volume     LevelSetVolume
	nx, ny, nz int
	n          int
	// size is (nx, ny, nz) and stride the index step along each axis.
	size, stride   [3]int
	bounded, dense bool
	interfaceLocal bool
	workers        int
	origin         [3]int
	h              [3]float64
	hmin, hmax     float64
	dims           int
	image          []float32
	// unavailable is nil when every voxel is available.
	unavailable []bool
	// seeds are the initial seeds inside the volume, in sub-volume indices.
	seeds [][3]int
}

func (g *levelSetGrid) available(i int) bool { return g.unavailable == nil || !g.unavailable[i] }

func (g *levelSetGrid) index(x, y, z int) int { return (z*g.ny+y)*g.nx + x }

func (g *levelSetGrid) coords(i int) (int, int, int) {
	return i % g.nx, (i / g.nx) % g.ny, i / (g.nx * g.ny)
}

func newLevelSetGrid(ctx context.Context, volume LevelSetVolume, init LevelSetInitialization, margin func(hmax float64) float64, options LevelSetOptions) (*levelSetGrid, error) {
	return newLevelSetGridReserved(ctx, volume, init, margin, options, nil)
}

func newLevelSetGridReserved(ctx context.Context, volume LevelSetVolume, init LevelSetInitialization, margin func(float64) float64, options LevelSetOptions, reserve func(*levelSetGrid) error) (*levelSetGrid, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := levelSetContextError(ctx); err != nil {
		return nil, err
	}
	if volume.ValueAt == nil || volume.Columns <= 0 || volume.Rows <= 0 || volume.Slices <= 0 {
		return nil, fmt.Errorf("%w: volume %dx%dx%d", ErrLevelSetInvalidInput, volume.Columns, volume.Rows, volume.Slices)
	}
	limits, err := options.Limits.normalize()
	if err != nil {
		return nil, err
	}
	if !finiteNonNegative(options.MarginMM) || !finiteNonNegative(init.InitialRadiusMM) {
		return nil, fmt.Errorf("%w: negative or non-finite margin or radius", ErrLevelSetInvalidInput)
	}
	g := &levelSetGrid{ctx: ctx, volume: volume, h: volume.SpacingMM}
	sizes := [3]int{volume.Columns, volume.Rows, volume.Slices}
	g.hmin, g.hmax = math.Inf(1), 0
	for axis := 0; axis < 3; axis++ {
		if sizes[axis] == 1 && axis == 2 {
			g.h[axis] = 1
			continue
		}
		s := g.h[axis]
		if !(s > 0) || math.IsInf(s, 0) {
			return nil, fmt.Errorf("%w: spacing %v mm along axis %d", ErrLevelSetInvalidInput, s, axis)
		}
		g.dims++
		g.hmin, g.hmax = math.Min(g.hmin, s), math.Max(g.hmax, s)
	}
	// Bounding box of the initial region.
	lo := [3]int{sizes[0], sizes[1], sizes[2]}
	hi := [3]int{-1, -1, -1}
	extend := func(x, y, z int) {
		for axis, v := range [3]int{x, y, z} {
			lo[axis] = min(lo[axis], v)
			hi[axis] = max(hi[axis], v)
		}
	}
	for k, seed := range init.Seeds {
		if k%4096 == 0 {
			if e := levelSetContextError(ctx); e != nil {
				return nil, e
			}
		}
		if seed.X < 0 || seed.Y < 0 || seed.Slice < 0 || seed.X >= sizes[0] || seed.Y >= sizes[1] || seed.Slice >= sizes[2] {
			return nil, fmt.Errorf("%w: seed (%d, %d, %d) outside the volume", ErrLevelSetInvalidInput, seed.X, seed.Y, seed.Slice)
		}
		extend(seed.X, seed.Y, seed.Slice)
	}
	for slice, mask := range init.Masks {
		if mask == nil {
			continue
		}
		if slice < 0 || slice >= sizes[2] || mask.Columns != sizes[0] || mask.Rows != sizes[1] {
			return nil, fmt.Errorf("%w: initial mask of slice %d", ErrLevelSetInvalidInput, slice)
		}
		if e := levelSetMaskRuns(ctx, mask, func(y int, run MaskRun) error { extend(run.Start, y, slice); extend(run.End-1, y, slice); return nil }); e != nil {
			return nil, e
		}
	}
	if hi[0] < 0 {
		return nil, ErrLevelSetEmpty
	}
	m := margin(g.hmax)
	if !finiteNonNegative(m) {
		return nil, fmt.Errorf("%w: margin %v", ErrLevelSetInvalidInput, m)
	}
	size := [3]int{}
	for axis := 0; axis < 3; axis++ {
		pad := 0
		if sizes[axis] > 1 {
			pad = int(math.Min(math.Ceil(m/g.h[axis]), float64(sizes[axis])))
		}
		lo[axis] = max(0, lo[axis]-pad)
		hi[axis] = min(sizes[axis]-1, hi[axis]+pad)
		size[axis] = hi[axis] - lo[axis] + 1
	}
	g.origin = lo
	g.nx, g.ny, g.nz = size[0], size[1], size[2]
	if size[0] > limits.MaxVoxels/size[1] || size[0]*size[1] > limits.MaxVoxels/size[2] {
		return nil, fmt.Errorf("%w: a %dx%dx%d sub-volume exceeds %d voxels", ErrLevelSetResourceLimit, size[0], size[1], size[2], limits.MaxVoxels)
	}
	g.n = size[0] * size[1] * size[2]
	g.size, g.stride = size, [3]int{1, size[0], size[0] * size[1]}
	if int64(g.n) > limits.MaxBytes/levelSetBytesPerVoxel {
		return nil, fmt.Errorf("%w: a %dx%dx%d sub-volume needs about %d MiB", ErrLevelSetResourceLimit, size[0], size[1], size[2], int64(g.n)*levelSetBytesPerVoxel>>20)
	}
	if reserve != nil {
		if err := reserve(g); err != nil {
			return nil, err
		}
	}
	if g.bounded {
		g.seeds = make([][3]int, 0, len(init.Seeds))
	}
	for _, seed := range init.Seeds {
		g.seeds = append(g.seeds, [3]int{seed.X - lo[0], seed.Y - lo[1], seed.Slice - lo[2]})
	}
	g.image = make([]float32, g.n)
	for z := 0; z < g.nz; z++ {
		if err := levelSetContextError(ctx); err != nil {
			return nil, err
		}
		for y := 0; y < g.ny; y++ {
			row := g.image[g.index(0, y, z):]
			for x := 0; x < g.nx; x++ {
				if (y*g.nx+x)%4096 == 0 {
					if e := levelSetContextError(ctx); e != nil {
						return nil, e
					}
				}
				v, ok := volume.ValueAt(x+lo[0], y+lo[1], z+lo[2])
				if g.interfaceLocal {
					if e := levelSetContextError(ctx); e != nil {
						return nil, e
					}
				}
				if g.bounded && ok && (math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > math.MaxFloat32) {
					if g.interfaceLocal {
						return nil, fmt.Errorf("%w: nonfinite/overflow sample", ErrShapeInterfaceDomain)
					}
					return nil, fmt.Errorf("%w: non-finite/overflow sample", ErrLevelSetInvalidInput)
				}
				if g.dense && !ok {
					if e := levelSetContextError(ctx); e != nil {
						return nil, e
					}
					if g.interfaceLocal {
						return nil, ErrShapeInterfaceDomain
					}
					return nil, ErrCannyDomain
				}
				if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
					if g.unavailable == nil {
						g.unavailable = make([]bool, g.n)
					}
					g.unavailable[g.index(x, y, z)] = true
					v = 0
				}
				row[x] = float32(v)
			}
		}
	}
	return g, nil
}

// initialLevelSet is the initial level set (negative inside). Around the
// seeds it is started by fast marching as in ITK's level set examples, from
// the arrival time T over speed (non-positive speeds block) and the initial
// radius r, but as max(d - r, T - c·r) with d the Euclidean distance to the
// nearest seed and c = levelSetArrivalSlack: in a homogeneous region the
// start is the ball of radius r (a first-order arrival time overestimates
// diagonal distances, and its flattened level sets would read as a strong
// curvature), while an edge, where T rises steeply, still cuts it. Mask
// voxels are -h/2 and the others +h/2; the two are united by their minimum.
func (g *levelSetGrid) initialLevelSet(init LevelSetInitialization, speed []float32) ([]float32, error) {
	far := float32(4 * levelSetBandVoxels * g.hmax)
	phi := make([]float32, g.n)
	for i := range phi {
		if i%4096 == 0 {
			if e := levelSetContextError(g.ctx); e != nil {
				return nil, e
			}
		}
		phi[i] = far
	}
	empty := true
	if len(init.Masks) > 0 {
		half := float32(g.hmin / 2)
		for i := range phi {
			if i%4096 == 0 {
				if e := levelSetContextError(g.ctx); e != nil {
					return nil, e
				}
			}
			phi[i] = half
		}
		for slice, mask := range init.Masks {
			z := slice - g.origin[2]
			if mask == nil || z < 0 || z >= g.nz {
				continue
			}
			if e := levelSetMaskRuns(g.ctx, mask, func(y int, run MaskRun) error {
				for x := run.Start; x < run.End; x++ {
					if (x-run.Start)%4096 == 0 {
						if e := levelSetContextError(g.ctx); e != nil {
							return e
						}
					}
					if i := g.index(x-g.origin[0], y-g.origin[1], z); g.available(i) {
						phi[i] = -half
						empty = false
					}
				}
				return nil
			}); e != nil {
				return nil, e
			}
		}
	}
	if len(g.seeds) > 0 {
		radius := init.InitialRadiusMM
		if radius == 0 {
			radius = g.defaultInitialRadius()
		}
		times, err := g.fastMarchingTimes(speed, levelSetArrivalSlack*radius+float64(far))
		if err != nil {
			return nil, err
		}
		for i, t := range times {
			if i%4096 == 0 {
				if e := levelSetContextError(g.ctx); e != nil {
					return nil, e
				}
			}
			if math.IsInf(t, 1) {
				continue
			}
			x, y, z := g.coords(i)
			d := math.Inf(1)
			for _, seed := range g.seeds {
				dx, dy, dz := float64(x-seed[0])*g.h[0], float64(y-seed[1])*g.h[1], float64(z-seed[2])*g.h[2]
				d = math.Min(d, dx*dx+dy*dy+dz*dz)
			}
			v := float32(math.Min(math.Max(math.Sqrt(d)-radius, t-levelSetArrivalSlack*radius), float64(far)))
			phi[i] = min(phi[i], v)
			if v < 0 {
				empty = false
			}
		}
		for _, seed := range g.seeds {
			if i := g.index(seed[0], seed[1], seed[2]); g.available(i) && phi[i] >= 0 {
				// A radius below the spacing still keeps the seed voxel.
				phi[i] = -float32(g.hmin / 2)
				empty = false
			}
		}
	}
	if empty {
		return nil, ErrLevelSetEmpty
	}
	return phi, nil
}

func (g *levelSetGrid) defaultInitialRadius() float64 { return math.Max(3*g.hmin, 2*g.hmax) }

// levelSetArrivalSlack bounds the overestimation of a first-order arrival
// time over a homogeneous region (about 1.2 on a diagonal in 3D).
const levelSetArrivalSlack = 1.3

// fastMarchingTimes returns the arrival time from the seeds over speed up
// to stop (+Inf beyond and on unavailable voxels). Seeds on unavailable
// voxels are skipped; a seed on a voxel of non-positive speed still starts
// the front.
func (g *levelSetGrid) fastMarchingTimes(speed []float32, stop float64) ([]float64, error) {
	grid := render.FastMarchingGrid{Nx: g.nx, Ny: g.ny, Nz: g.nz, Spacing: render.Vec3{X: g.h[0], Y: g.h[1], Z: g.h[2]}, Speed: make([]float64, g.n)}
	for i, s := range speed {
		if i%4096 == 0 {
			if e := levelSetContextError(g.ctx); e != nil {
				return nil, e
			}
		}
		if g.available(i) && s > 0 {
			grid.Speed[i] = float64(s)
		}
	}
	var seeds []render.FastMarchingSeed
	if g.interfaceLocal {
		seeds = make([]render.FastMarchingSeed, 0, len(g.seeds))
	}
	for _, seed := range g.seeds {
		i := g.index(seed[0], seed[1], seed[2])
		if !g.available(i) {
			continue
		}
		if grid.Speed[i] <= 0 {
			grid.Speed[i] = math.SmallestNonzeroFloat32
		}
		seeds = append(seeds, render.FastMarchingSeed{Index: seed})
	}
	if len(seeds) == 0 {
		return nil, ErrLevelSetEmpty
	}
	var result *render.FastMarchingResult
	var err error
	if g.interfaceLocal {
		result, err = render.FastMarchingFixedBuffers(g.ctx, grid, seeds, render.FastMarchingOptions{StoppingValue: stop})
	} else {
		result, err = render.FastMarching(g.ctx, grid, seeds, render.FastMarchingOptions{StoppingValue: stop})
	}
	if err != nil {
		if ctxErr := levelSetContextError(g.ctx); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: fast marching: %w", ErrLevelSetInvalidInput, err)
	}
	for i := range result.Times {
		if i%4096 == 0 {
			if e := levelSetContextError(g.ctx); e != nil {
				return nil, e
			}
		}
		if !g.available(i) {
			result.Times[i] = math.Inf(1)
		}
	}
	return result.Times, nil
}

// edgePotential returns g = sigmoid(|∇(G_σ * I)|) and the sigmoid used.
func (g *levelSetGrid) edgePotential(params EdgePotentialParams, init LevelSetInitialization, edgeQuantile float64) ([]float32, float64, float64, error) {
	if !finiteNonNegative(params.SigmaMM) || math.IsNaN(params.Alpha) || math.IsInf(params.Alpha, 0) ||
		math.IsNaN(params.Beta) || math.IsInf(params.Beta, 0) {
		return nil, 0, 0, fmt.Errorf("%w: edge potential parameters", ErrLevelSetInvalidInput)
	}
	sigma := params.SigmaMM
	if sigma == 0 {
		sigma = math.Max(1, g.hmin)
	}
	src := &imagefilter.Volume{Dims: [3]int{g.nx, g.ny, g.nz}, SpacingMM: g.h, Data: g.image}
	gradient, err := imagefilter.GradientMagnitudeRecursiveGaussian(g.ctx, src, imagefilter.RecursiveGaussianOptions{SigmaMM: imagefilter.IsotropicSigma(sigma), Workers: g.workers})
	if err != nil {
		if ctxErr := levelSetContextError(g.ctx); ctxErr != nil {
			return nil, 0, 0, ctxErr
		}
		return nil, 0, 0, fmt.Errorf("%w: gradient magnitude: %w", ErrLevelSetInvalidInput, err)
	}
	grad := gradient.Data
	alpha, beta := params.Alpha, params.Beta
	if alpha == 0 {
		alpha, beta, err = g.estimateSigmoidContext(grad, init, edgeQuantile)
		if err != nil {
			return nil, 0, 0, err
		}
	}
	for i, v := range grad {
		if i%4096 == 0 {
			if e := levelSetContextError(g.ctx); e != nil {
				return nil, 0, 0, e
			}
		}
		e := 1 / (1 + math.Exp(-(float64(v)-beta)/alpha))
		if !g.available(i) {
			e = 0
		}
		if g.bounded && (math.IsNaN(e) || math.IsInf(e, 0)) {
			return nil, 0, 0, fmt.Errorf("%w: non-finite potential", ErrLevelSetInvalidInput)
		}
		grad[i] = float32(e)
	}
	return grad, alpha, beta, nil
}

// Quantiles of the gradients that stand out taken as the edge level K1: the
// fast marching threshold stops where the speed falls, so it needs the
// crest of the edges (the voxels beside an edge also stand out, below its
// crest); a level set is drawn onto the crest by advection and needs the
// weaker edges to hold it.
const (
	levelSetCrestQuantile = 0.75
	levelSetEdgeQuantile  = 0.5
)

// estimateSigmoid derives the sigmoid from the gradient magnitude G. K2 is
// the median of G inside the initial region (the seed balls and the mask
// interiors), the level of a homogeneous region, and its spread the scaled
// median absolute deviation. K1, the level of an edge, is the median of G on
// the mask borders when it stands out (above K2 + 3 spreads), else the
// edgeQuantile of the sub-volume gradients that stand out, else K2 + 6
// spreads.
func (g *levelSetGrid) estimateSigmoid(grad []float32, init LevelSetInitialization, edgeQuantile float64) (float64, float64) {
	a, b, _ := g.estimateSigmoidContext(grad, init, edgeQuantile)
	return a, b
}

func (g *levelSetGrid) estimateSigmoidContext(grad []float32, init LevelSetInitialization, edgeQuantile float64) (float64, float64, error) {
	radius := init.InitialRadiusMM
	if radius == 0 {
		radius = g.defaultInitialRadius()
	}
	var inside, border []float32
	for _, seed := range g.seeds {
		var lo, hi [3]int
		for axis, n := range [3]int{g.nx, g.ny, g.nz} {
			r := int(radius / g.h[axis])
			lo[axis], hi[axis] = max(0, seed[axis]-r), min(n-1, seed[axis]+r)
		}
		for z := lo[2]; z <= hi[2]; z++ {
			for y := lo[1]; y <= hi[1]; y++ {
				if e := levelSetContextError(g.ctx); e != nil {
					return 0, 0, e
				}
				for x := lo[0]; x <= hi[0]; x++ {
					if x%4096 == 0 {
						if e := levelSetContextError(g.ctx); e != nil {
							return 0, 0, e
						}
					}
					dx, dy, dz := float64(x-seed[0])*g.h[0], float64(y-seed[1])*g.h[1], float64(z-seed[2])*g.h[2]
					if dx*dx+dy*dy+dz*dz <= radius*radius {
						if i := g.index(x, y, z); g.available(i) {
							inside = append(inside, grad[i])
						}
					}
				}
			}
		}
	}
	if len(init.Masks) > 0 {
		mask := make([]bool, g.n)
		for slice, m := range init.Masks {
			z := slice - g.origin[2]
			if m == nil || z < 0 || z >= g.nz {
				continue
			}
			if e := levelSetMaskRuns(g.ctx, m, func(y int, run MaskRun) error {
				for x := run.Start; x < run.End; x++ {
					if (x-run.Start)%4096 == 0 {
						if e := levelSetContextError(g.ctx); e != nil {
							return e
						}
					}
					mask[g.index(x-g.origin[0], y-g.origin[1], z)] = true
				}
				return nil
			}); e != nil {
				return 0, 0, e
			}
		}
		for i, in := range mask {
			if i%4096 == 0 {
				if e := levelSetContextError(g.ctx); e != nil {
					return 0, 0, e
				}
			}
			if !in || !g.available(i) {
				continue
			}
			if g.onBorder(mask, i) {
				border = append(border, grad[i])
			} else {
				inside = append(inside, grad[i])
			}
		}
	}
	if len(inside) == 0 {
		inside = border
	}
	k2, err := quantile32Context(g.ctx, inside, 0.5)
	if err != nil {
		return 0, 0, err
	}
	deviations := make([]float32, len(inside))
	for i, v := range inside {
		if i%4096 == 0 {
			if e := levelSetContextError(g.ctx); e != nil {
				return 0, 0, e
			}
		}
		deviations[i] = float32(math.Abs(float64(v) - k2))
	}
	deviation, err := quantile32Context(g.ctx, deviations, 0.5)
	if err != nil {
		return 0, 0, err
	}
	spread := math.Max(1.4826*deviation, 1e-6*math.Max(1, k2))
	significant := k2 + 3*spread
	k1 := 0.0
	if len(border) > 0 {
		k1, err = quantile32Context(g.ctx, border, .5)
		if err != nil {
			return 0, 0, err
		}
	}
	if !(k1 > significant) {
		stride := max(1, g.n/(1<<18))
		var edges []float32
		for i := 0; i < g.n; i += stride {
			if i%4096 == 0 {
				if e := levelSetContextError(g.ctx); e != nil {
					return 0, 0, e
				}
			}
			if g.available(i) && float64(grad[i]) > significant {
				edges = append(edges, grad[i])
			}
		}
		k1, err = quantile32Context(g.ctx, edges, edgeQuantile)
		if err != nil {
			return 0, 0, err
		}
	}
	if !(k1 > significant) {
		k1 = k2 + 6*spread
	}
	return -(k1 - k2) / 6, (k1 + k2) / 2, levelSetContextError(g.ctx)
}

// onBorder reports a mask voxel with a 6-neighbour outside the mask (the
// sides of the sub-volume do not count).
func (g *levelSetGrid) onBorder(mask []bool, i int) bool {
	x, y, z := g.coords(i)
	for _, n := range [6][3]int{{x - 1, y, z}, {x + 1, y, z}, {x, y - 1, z}, {x, y + 1, z}, {x, y, z - 1}, {x, y, z + 1}} {
		if n[0] < 0 || n[1] < 0 || n[2] < 0 || n[0] >= g.nx || n[1] >= g.ny || n[2] >= g.nz {
			continue
		}
		if !mask[g.index(n[0], n[1], n[2])] {
			return true
		}
	}
	return false
}

func quantile32(values []float32, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float32(nil), values...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a] < sorted[b] })
	return float64(sorted[int(q*float64(len(sorted)-1)+0.5)])
}

// result converts the inside predicate on the sub-volume into slice masks.
func (g *levelSetGrid) result(inside func(i int) bool) *LevelSetResult {
	out := &LevelSetResult{Masks: map[int]*RasterMask{}, RegionOrigin: g.origin, RegionSize: [3]int{g.nx, g.ny, g.nz}}
	sizes := [3]int{g.volume.Columns, g.volume.Rows, g.volume.Slices}
	cut := func(axis, v, n int) bool {
		return (v == 0 && g.origin[axis] > 0) || (v == n-1 && g.origin[axis]+n < sizes[axis])
	}
	for z := 0; z < g.nz; z++ {
		var mask *RasterMask
		for y := 0; y < g.ny; y++ {
			if e := levelSetContextError(g.ctx); e != nil {
				return nil
			}
			start := -1
			for x := 0; x <= g.nx; x++ {
				if x%4096 == 0 && levelSetContextError(g.ctx) != nil {
					return nil
				}
				in := x < g.nx && inside(g.index(x, y, z)) && g.available(g.index(x, y, z))
				if in {
					out.VoxelCount++
					if g.bounded && (x+g.origin[0] == 0 || x+g.origin[0] == sizes[0]-1 || y+g.origin[1] == 0 || y+g.origin[1] == sizes[1]-1 || g.volume.Slices > 1 && (z+g.origin[2] == 0 || z+g.origin[2] == sizes[2]-1)) {
						out.TouchesVolumeBoundary = true
					}
					if cut(0, x, g.nx) || cut(1, y, g.ny) || cut(2, z, g.nz) {
						out.TouchesRegionBoundary = true
					}
					if start < 0 {
						start = x
					}
					continue
				}
				if start >= 0 {
					if mask == nil {
						mask = NewRasterMask(g.volume.Columns, g.volume.Rows)
					}
					if g.bounded {
						// Runs are already sorted and disjoint. Appending
						// avoids repeated merging/traversal of an output row.
						row := y + g.origin[1]
						mask.runs[row] = append(mask.runs[row], MaskRun{Start: start + g.origin[0], End: x + g.origin[0]})
					} else {
						mask.SetRun(y+g.origin[1], start+g.origin[0], x+g.origin[0])
					}
					start = -1
				}
			}
		}
		if mask != nil {
			out.Masks[z+g.origin[2]] = mask
		}
	}
	voxel := g.h[0] * g.h[1]
	if g.volume.Slices > 1 {
		voxel *= g.h[2]
	}
	out.VolumeMM3 = float64(out.VoxelCount) * voxel
	return out
}

// levelSetTerms are the speeds of an evolution:
// ∂φ/∂t = -propagation·speed·|∇φ| + curvature·w·κ|∇φ| + advection·∇a·∇φ,
// with w = curvatureWeight (1 when nil) and a = advectionField (none when
// nil).
type levelSetTerms struct {
	stopping                          EdgeLevelSetStopping
	propagation, curvature, advection float32
	speed, curvatureWeight            []float32
	advectionField                    []float32
	shapePrior                        *pcaShapeEvolution
}

// levelSetState is the narrow band level set.
type levelSetState struct {
	*levelSetGrid
	phi   []float32
	band  []int32
	width float32
	// reinitialization buffers
	status  []byte
	touched []int32
	heap    levelSetHeap
}

const (
	lsFar byte = iota
	lsTrial
	lsSeed
	lsFrozen
)

const (
	// levelSetWindow is the number of iterations of a convergence window.
	levelSetWindow = 10
	// levelSetWindowFlips is the fraction of the front voxels that may
	// change side over two consecutive windows of a converged evolution.
	levelSetWindowFlips = 0.002
)

// evolve runs the narrow band evolution from the initial level set phi,
// which it takes over. It stops after maxIterations, when the RMS change at
// the front falls below tolerance (in units of the smallest spacing), or
// when two consecutive windows of levelSetWindow iterations each change the
// side of at most levelSetWindowFlips of the front voxels.
func (g *levelSetGrid) evolve(phi []float32, terms levelSetTerms, maxIterations int, tolerance float64, progress func(LevelSetProgress)) (*LevelSetResult, error) {
	s := &levelSetState{levelSetGrid: g, phi: phi, width: float32(levelSetBandVoxels * g.hmax), status: make([]byte, g.n)}
	if g.bounded {
		s.band = make([]int32, 0, g.n)
		s.touched = make([]int32, 0, g.n)
		s.heap.maxItems = g.n * 7
	}
	if err := s.reinitialize(nil); err != nil {
		return nil, err
	}
	if terms.shapePrior != nil {
		if err := terms.shapePrior.measure(g.ctx, s.phi, true); err != nil {
			return nil, err
		}
	}
	rates := make([]float32, 0)
	if g.bounded {
		rates = make([]float32, g.n)
	}
	tol := tolerance * g.hmin
	moved := 0.0
	iterations, rms, converged := 0, 0.0, false
	reason := LevelSetStopIterationLimit
	// Window convergence: on a noisy or discontinuous speed the front
	// chatters within a voxel and the per-iteration change never vanishes,
	// so the evolution also stops once a window of iterations moves the
	// region by almost nothing.
	window := make([]bool, g.n)
	snapshot := func() {
		for i, v := range s.phi {
			if i%4096 == 0 && levelSetContextError(g.ctx) != nil {
				return
			}
			window[i] = v < 0
		}
	}
	snapshot()
	quiet := 0
	for iterations < maxIterations {
		if err := levelSetContextError(g.ctx); err != nil {
			return nil, err
		}
		if len(s.band) == 0 {
			reason = LevelSetStopEmptyBand
			converged = true
			break
		}
		if cap(rates) < len(s.band) {
			rates = make([]float32, len(s.band), len(s.band)*5/4)
		}
		rates = rates[:len(s.band)]
		firstOrder, curvatureMax := s.rates(terms, rates)
		if e := levelSetContextError(g.ctx); e != nil {
			return nil, e
		}
		if g.bounded && (math.IsNaN(firstOrder) || math.IsInf(firstOrder, 0) || math.IsNaN(curvatureMax) || math.IsInf(curvatureMax, 0)) {
			return nil, fmt.Errorf("%w: non-finite rate extremes", ErrLevelSetInvalidInput)
		}
		dt := levelSetTimeStep(firstOrder, curvatureMax, g.hmin, g.dims)
		if terms.shapePrior != nil {
			// Add the reaction's explicit stability bound to the same harmonic
			// denominator. A nil prior keeps the base arithmetic unchanged.
			dt = 1 / (firstOrder/(0.5*g.hmin) + curvatureMax*2*float64(g.dims)/(g.hmin*g.hmin) + 2*terms.shapePrior.options.Weight)
		}
		if dt == 0 {
			reason = LevelSetStopZeroTimeStep
			converged = true
			break
		}
		front := float32(g.hmax)
		sum, count, largest := 0.0, 0, 0.0
		for k, i := range s.band {
			if k%4096 == 0 {
				if e := levelSetContextError(g.ctx); e != nil {
					return nil, e
				}
			}
			change := dt * float64(rates[k])
			if p := s.phi[i]; p < front && p > -front {
				sum += change * change
				count++
				largest = math.Max(largest, math.Abs(change))
			}
			if g.bounded && (math.IsNaN(change) || math.IsInf(change, 0) || math.Abs(change+float64(s.phi[i])) > math.MaxFloat32) {
				return nil, fmt.Errorf("%w: non-finite level-set update", ErrLevelSetInvalidInput)
			}
			s.phi[i] += float32(change)
		}
		iterations++
		rms = 0
		if count > 0 {
			rms = math.Sqrt(sum / float64(count))
		}
		if progress != nil {
			progress(LevelSetProgress{Iteration: iterations, MaxIterations: maxIterations, RMSChange: rms})
		}
		if rms < tol && terms.stopping != MaskStabilityOnly && terms.stopping != FixedIterations {
			reason = LevelSetStopRMS
			converged = true
			break
		}
		if iterations%levelSetWindow == 0 {
			flips := 0
			for i, v := range s.phi {
				if i%4096 == 0 {
					if e := levelSetContextError(g.ctx); e != nil {
						return nil, e
					}
				}
				if (v < 0) != window[i] {
					flips++
				}
			}
			if float64(flips) <= levelSetWindowFlips*float64(count) {
				quiet++
			} else {
				quiet = 0
			}
			if quiet >= 2 && terms.stopping != RMSOnly && terms.stopping != FixedIterations {
				reason = LevelSetStopStableMask
				converged = true
				break
			}
			snapshot()
		}
		moved += largest
		if moved >= 0.5*g.hmin {
			if err := s.reinitialize(s.band); err != nil {
				return nil, err
			}
			moved = 0
		}
	}
	if terms.shapePrior != nil {
		if err := terms.shapePrior.measure(g.ctx, s.phi, false); err != nil {
			return nil, err
		}
	}
	result := g.result(func(i int) bool { return s.phi[i] < 0 })
	if result == nil {
		return nil, levelSetContextError(g.ctx)
	}
	result.Iterations, result.Converged, result.RMSChange = iterations, converged, rms
	if g.bounded {
		result.StopReason = reason
	}
	return result, nil
}

// levelSetTimeStep is the largest stable explicit step: half a voxel per
// step for the first-order terms and the diffusion limit h²/(2d) for the
// curvature, combined harmonically.
func levelSetTimeStep(firstOrder, curvature, hmin float64, dims int) float64 {
	rate := firstOrder/(0.5*hmin) + curvature*2*float64(dims)/(hmin*hmin)
	if !(rate > 0) {
		return 0
	}
	return 1 / rate
}

// rates computes ∂φ/∂t on every band voxel and returns the largest
// first-order speed (propagation plus advection) and curvature weight.
func (s *levelSetState) rates(terms levelSetTerms, out []float32) (float64, float64) {
	workers := 1
	if len(s.band) >= levelSetParallelBand {
		workers = min(parallel.DefaultWorkers(), len(s.band)/(levelSetParallelBand/2))
	}
	type extremes struct{ first, curvature float64 }
	results := make([]extremes, workers)
	run := func(w, lo, hi int) {
		var e extremes
		for k := lo; k < hi; k++ {
			if k%4096 == 0 && s.ctx.Err() != nil {
				return
			}
			rate, first, curvature := s.rate(int(s.band[k]), terms)
			out[k] = rate
			e.first = math.Max(e.first, first)
			e.curvature = math.Max(e.curvature, curvature)
		}
		results[w] = e
	}
	chunk := (len(s.band) + workers - 1) / workers
	_ = parallel.Do(s.ctx, workers, workers, func(w int) {
		if lo, hi := w*chunk, min(len(s.band), (w+1)*chunk); lo < hi {
			run(w, lo, hi)
		}
	})
	var first, curvature float64
	for _, e := range results {
		first, curvature = math.Max(first, e.first), math.Max(curvature, e.curvature)
	}
	return first, curvature
}

// neighbour returns the voxel one step from i (at pos) along axis in
// direction dir (±1), or i itself beyond the sub-volume or on an
// unavailable voxel: a zero-flux boundary.
func (s *levelSetState) neighbour(i int, pos [3]int, axis, dir int) int {
	c := pos[axis] + dir
	if c < 0 || c >= s.size[axis] {
		return i
	}
	j := i + dir*s.stride[axis]
	if !s.available(j) {
		return i
	}
	return j
}

// mixed is ∂²φ/∂a∂b at voxel i with the same boundary repetition as
// neighbour; interior voxels index their diagonal neighbours directly.
func (s *levelSetState) mixed(i int, pos [3]int, a, b int, interior bool) float64 {
	return s.mixedBoundary(i, pos, a, b, interior, nil)
}

func (s *levelSetState) mixedBoundary(i int, pos [3]int, a, b int, interior bool, allowed []byte) float64 {
	at := func(da, db int) float64 {
		if interior {
			if allowed != nil {
				return float64(s.interfaceStencilValue(i+da*s.stride[a]+db*s.stride[b], allowed))
			}
			return float64(s.phi[i+da*s.stride[a]+db*s.stride[b]])
		}
		j, at := s.neighbour(i, pos, a, da), pos
		if j != i {
			at[a] += da
		}
		index := s.neighbour(j, at, b, db)
		if allowed != nil {
			return float64(s.interfaceStencilValue(index, allowed))
		}
		return float64(s.phi[index])
	}
	return (at(1, 1) - at(1, -1) - at(-1, 1) + at(-1, -1)) / (4 * s.h[a] * s.h[b])
}

// rate is ∂φ/∂t at voxel i, with its first-order speed and curvature
// weight. Neighbours beyond the sub-volume or unavailable repeat voxel i.
func (s *levelSetState) rate(i int, terms levelSetTerms) (float32, float64, float64) {
	return s.ratePotential(i, terms, false, 0)
}

// ratePotential retains the historical curvature cutoff and arithmetic.
func (s *levelSetState) ratePotential(i int, terms levelSetTerms, sampled bool, potential float32) (float32, float64, float64) {
	return s.ratePotentialRegularized(i, terms, sampled, potential, 0)
}

// ratePotentialRegularized only regularizes the curvature denominator. The
// interface-local embedding uses dimensionless regularization 1e-6; zero keeps
// the historical cutoff and division, without changing its arithmetic order.
func (s *levelSetState) ratePotentialRegularized(i int, terms levelSetTerms, sampled bool, potential float32, regularization float64) (float32, float64, float64) {
	return s.ratePotentialBoundary(i, terms, sampled, potential, regularization, nil)
}

func (s *levelSetState) ratePotentialBoundary(i int, terms levelSetTerms, sampled bool, potential float32, regularization float64, allowed []byte) (float32, float64, float64) {
	x, y, z := s.coords(i)
	phi := s.phi
	c := float64(phi[i])
	var minus, plus [3]int
	pos := [3]int{x, y, z}
	// An interior voxel has every neighbour of its 3×3×3 box available and
	// inside the sub-volume (along the axes of more than one voxel).
	interior := s.unavailable == nil
	for axis := 0; axis < 3 && interior; axis++ {
		interior = s.size[axis] == 1 || (pos[axis] > 0 && pos[axis] < s.size[axis]-1)
	}
	for axis := 0; axis < 3; axis++ {
		switch {
		case s.size[axis] == 1:
			minus[axis], plus[axis] = i, i
		case interior:
			minus[axis], plus[axis] = i-s.stride[axis], i+s.stride[axis]
		default:
			minus[axis] = s.neighbour(i, pos, axis, -1)
			plus[axis] = s.neighbour(i, pos, axis, 1)
		}
	}
	var dm, dp, d1, d2 [3]float64
	for axis := 0; axis < 3; axis++ {
		h := s.h[axis]
		a, b := float64(phi[minus[axis]]), float64(phi[plus[axis]])
		if allowed != nil {
			a, b = float64(s.interfaceStencilValue(minus[axis], allowed)), float64(s.interfaceStencilValue(plus[axis], allowed))
		}
		dm[axis] = (c - a) / h
		dp[axis] = (b - c) / h
		d1[axis] = (b - a) / (2 * h)
		d2[axis] = (b - 2*c + a) / (h * h)
	}
	rate := 0.0
	first := 0.0
	// Propagation: upwind (Osher–Sethian) gradient magnitude.
	if terms.propagation != 0 && terms.speed != nil {
		speed := terms.speed[i]
		if sampled {
			speed = potential
		}
		f := float64(terms.propagation) * float64(speed)
		var grad2 float64
		for axis := 0; axis < 3; axis++ {
			if f > 0 {
				grad2 += sq(math.Max(dm[axis], 0)) + sq(math.Min(dp[axis], 0))
			} else {
				grad2 += sq(math.Min(dm[axis], 0)) + sq(math.Max(dp[axis], 0))
			}
		}
		rate -= f * math.Sqrt(grad2)
		first += math.Abs(f)
	}
	// Advection along V = -A·∇a, upwind.
	if terms.advection != 0 && terms.advectionField != nil {
		field := terms.advectionField
		for axis := 0; axis < 3; axis++ {
			v := -float64(terms.advection) * float64(field[plus[axis]]-field[minus[axis]]) / (2 * s.h[axis])
			if v > 0 {
				rate -= v * dm[axis]
			} else {
				rate -= v * dp[axis]
			}
			first += math.Abs(v) * s.hmin / s.h[axis]
		}
	}
	// Mean curvature times |∇φ| from central differences (Osher and Fedkiw,
	// "Level Set Methods and Dynamic Implicit Surfaces", 2003).
	curvature := 0.0
	if terms.curvature != 0 {
		weight := float64(terms.curvature)
		if terms.curvatureWeight != nil {
			w := terms.curvatureWeight[i]
			if sampled {
				w = potential
			}
			weight *= float64(w)
		}
		if weight != 0 {
			gx, gy, gz := d1[0], d1[1], d1[2]
			num := d2[0]*(gy*gy+gz*gz) + d2[1]*(gx*gx+gz*gz)
			if s.nx > 1 && s.ny > 1 {
				num -= 2 * gx * gy * s.mixedBoundary(i, pos, 0, 1, interior, allowed)
			}
			if s.nz > 1 {
				num += d2[2] * (gx*gx + gy*gy)
				if s.nx > 1 {
					num -= 2 * gx * gz * s.mixedBoundary(i, pos, 0, 2, interior, allowed)
				}
				if s.ny > 1 {
					num -= 2 * gy * gz * s.mixedBoundary(i, pos, 1, 2, interior, allowed)
				}
			}
			// num/|∇φ|² is the sum of the principal curvatures times |∇φ|;
			// dividing by d-1 gives the mean curvature, 1/r on a sphere
			// as on a circle.
			weight /= float64(max(1, s.dims-1))
			norm2 := gx*gx + gy*gy + gz*gz
			if regularization > 0 {
				rate += weight * num / (norm2 + regularization)
			} else if norm2 > 1e-12 {
				rate += weight * num / norm2
			}
			curvature = math.Abs(weight)
		}
	}
	if terms.shapePrior != nil {
		reaction := terms.shapePrior.options.Weight * (c - float64(terms.shapePrior.target[i]))
		rate -= reaction
		first += math.Abs(reaction)
	}
	return float32(rate), first, curvature
}

// reinitialize rebuilds φ as the signed distance (mm) to its zero level set
// within the band half width and ±width beyond, by fast marching from the
// voxels next to the interface, whose distance is interpolated linearly
// along each axis. candidates lists the voxels that may hold values other
// than ±width (every voxel when nil); the new band replaces s.band.
func (s *levelSetState) reinitialize(candidates []int32) error {
	if err := levelSetContextError(s.ctx); err != nil {
		return err
	}
	type seed struct {
		i int32
		d float32
	}
	var seeds []seed
	if s.bounded {
		seeds = make([]seed, 0, s.n)
	}
	visit := func(i int) {
		if !s.available(i) {
			return
		}
		if d, ok := s.interfaceDistance(i); ok {
			seeds = append(seeds, seed{int32(i), d})
		}
	}
	if candidates == nil {
		for i := 0; i < s.n; i++ {
			if i%4096 == 0 {
				if e := levelSetContextError(s.ctx); e != nil {
					return e
				}
			}
			visit(i)
		}
	} else {
		for k, i := range candidates {
			if k%4096 == 0 {
				if e := levelSetContextError(s.ctx); e != nil {
					return e
				}
			}
			visit(int(i))
		}
	}
	reset := func(i int) {
		if s.phi[i] < 0 {
			s.phi[i] = -s.width
		} else {
			s.phi[i] = s.width
		}
	}
	if candidates == nil {
		for i := 0; i < s.n; i++ {
			if i%4096 == 0 {
				if e := levelSetContextError(s.ctx); e != nil {
					return e
				}
			}
			reset(i)
		}
	} else {
		for k, i := range candidates {
			if k%4096 == 0 {
				if e := levelSetContextError(s.ctx); e != nil {
					return e
				}
			}
			reset(int(i))
		}
	}
	s.heap.items = s.heap.items[:0]
	s.touched = s.touched[:0]
	for k, sd := range seeds {
		if k%4096 == 0 {
			if e := levelSetContextError(s.ctx); e != nil {
				return e
			}
		}
		i := sd.i
		if s.phi[i] < 0 {
			s.phi[i] = -sd.d
		} else {
			s.phi[i] = sd.d
		}
		if s.status[i] == lsFar {
			s.touched = append(s.touched, i)
		}
		s.status[i] = lsSeed
		s.heap.push(levelSetHeapItem{d: sd.d, i: i})
	}
	band := s.band[:0]
	for pops := 0; len(s.heap.items) > 0; pops++ {
		if pops&4095 == 0 {
			if err := levelSetContextError(s.ctx); err != nil {
				return err
			}
		}
		item := s.heap.pop()
		i := int(item.i)
		if s.status[i] == lsFrozen || item.d != abs32(s.phi[i]) {
			continue
		}
		if item.d >= s.width {
			break
		}
		s.status[i] = lsFrozen
		band = append(band, item.i)
		x, y, z := s.coords(i)
		for _, nb := range [6][3]int{{x - 1, y, z}, {x + 1, y, z}, {x, y - 1, z}, {x, y + 1, z}, {x, y, z - 1}, {x, y, z + 1}} {
			if nb[0] < 0 || nb[1] < 0 || nb[2] < 0 || nb[0] >= s.nx || nb[1] >= s.ny || nb[2] >= s.nz {
				continue
			}
			j := s.index(nb[0], nb[1], nb[2])
			if st := s.status[j]; st == lsFrozen || st == lsSeed || !s.available(j) {
				continue
			}
			d := float32(s.eikonal(nb[0], nb[1], nb[2]))
			if d >= abs32(s.phi[j]) {
				continue
			}
			if s.status[j] == lsFar {
				s.touched = append(s.touched, int32(j))
			}
			s.status[j] = lsTrial
			if s.phi[j] < 0 {
				s.phi[j] = -d
			} else {
				s.phi[j] = d
			}
			s.heap.push(levelSetHeapItem{d: d, i: int32(j)})
		}
	}
	if s.heap.overflow {
		return fmt.Errorf("%w: reinitialization heap", ErrLevelSetResourceLimit)
	}
	for k, i := range s.touched {
		if k%4096 == 0 {
			if e := levelSetContextError(s.ctx); e != nil {
				return e
			}
		}
		if s.status[i] != lsFrozen {
			reset(int(i))
		}
		s.status[i] = lsFar
	}
	if s.bounded {
		if e := sortLevelSetBand(s.ctx, band); e != nil {
			return e
		}
	} else {
		slices.Sort(band)
	}
	s.band = band
	return nil
}

// interfaceDistance returns the distance from voxel i to the zero crossings
// between it and its 6-neighbours of the other sign (φ < 0 is inside),
// combining the axes like a plane: 1/d² = Σ 1/dₐ².
func (s *levelSetState) interfaceDistance(i int) (float32, bool) {
	x, y, z := s.coords(i)
	p := float64(s.phi[i])
	inside := p < 0
	pos := [3]int{x, y, z}
	n := [3]int{s.nx, s.ny, s.nz}
	stride := [3]int{1, s.nx, s.nx * s.ny}
	sum := 0.0
	found := false
	for axis := 0; axis < 3; axis++ {
		best := math.Inf(1)
		for _, delta := range [2]int{-1, 1} {
			c := pos[axis] + delta
			if c < 0 || c >= n[axis] {
				continue
			}
			j := i + delta*stride[axis]
			q := float64(s.phi[j])
			if (q < 0) == inside || !s.available(j) {
				continue
			}
			d := 0.0
			if total := math.Abs(p) + math.Abs(q); total > 0 {
				d = s.h[axis] * math.Abs(p) / total
			}
			best = math.Min(best, d)
		}
		if math.IsInf(best, 1) {
			continue
		}
		found = true
		if best == 0 {
			return 0, true
		}
		sum += 1 / (best * best)
	}
	if !found {
		return 0, false
	}
	return float32(1 / math.Sqrt(sum)), true
}

// eikonal solves |∇d| = 1 at (x, y, z) from its frozen (or seed)
// 6-neighbours with the first-order upwind scheme.
func (s *levelSetState) eikonal(x, y, z int) float64 {
	var a, h [3]float64
	n := 0
	pos := [3]int{x, y, z}
	sizes := [3]int{s.nx, s.ny, s.nz}
	for axis := 0; axis < 3; axis++ {
		best := math.Inf(1)
		for _, delta := range [2]int{-1, 1} {
			c := pos
			c[axis] += delta
			if c[axis] < 0 || c[axis] >= sizes[axis] {
				continue
			}
			j := s.index(c[0], c[1], c[2])
			if st := s.status[j]; st == lsFrozen {
				best = math.Min(best, float64(abs32(s.phi[j])))
			}
		}
		if !math.IsInf(best, 1) {
			a[n], h[n] = best, s.h[axis]
			n++
		}
	}
	for i := 1; i < n; i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
			h[j], h[j-1] = h[j-1], h[j]
		}
	}
	solution := math.Inf(1)
	var sumW, sumWA, sumWA2 float64
	for k := 0; k < n; k++ {
		if k > 0 && solution <= a[k] {
			break
		}
		w := 1 / (h[k] * h[k])
		sumW += w
		sumWA += w * a[k]
		sumWA2 += w * a[k] * a[k]
		disc := sumWA*sumWA - sumW*(sumWA2-1)
		if disc < 0 {
			break
		}
		solution = (sumWA + math.Sqrt(disc)) / sumW
	}
	return solution
}

func abs32(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}

type levelSetHeapItem struct {
	d float32
	i int32
}

// levelSetHeap is a binary min-heap with lazy deletion: stale entries are
// skipped when popped.
type levelSetHeap struct {
	items    []levelSetHeapItem
	maxItems int
	overflow bool
}

func (h *levelSetHeap) push(item levelSetHeapItem) {
	if h.maxItems > 0 {
		if len(h.items) >= h.maxItems {
			h.overflow = true
			return
		}
		if len(h.items) == cap(h.items) {
			capacity := min(h.maxItems, max(16, cap(h.items)*2))
			next := make([]levelSetHeapItem, len(h.items), capacity)
			copy(next, h.items)
			h.items = next
		}
	}
	h.items = append(h.items, item)
	k := len(h.items) - 1
	for k > 0 {
		parent := (k - 1) / 2
		if h.items[parent].d <= h.items[k].d {
			break
		}
		h.items[parent], h.items[k] = h.items[k], h.items[parent]
		k = parent
	}
}

func (h *levelSetHeap) pop() levelSetHeapItem {
	top := h.items[0]
	last := len(h.items) - 1
	h.items[0] = h.items[last]
	h.items = h.items[:last]
	k := 0
	for {
		l, r, m := 2*k+1, 2*k+2, k
		if l < last && h.items[l].d < h.items[m].d {
			m = l
		}
		if r < last && h.items[r].d < h.items[m].d {
			m = r
		}
		if m == k {
			break
		}
		h.items[k], h.items[m] = h.items[m], h.items[k]
		k = m
	}
	return top
}
