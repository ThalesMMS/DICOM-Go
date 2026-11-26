package roi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
)

// Seeded region growing family (#1095): connected threshold, confidence
// connected, neighbourhood connected and isolated connected growing over a
// sampled 2D or 3D volume.
//
// The four criteria follow ITK's Modules/Segmentation/RegionGrowing filters
// at commit 35b196e1 (ConnectedThresholdImageFilter,
// ConfidenceConnectedImageFilter, NeighborhoodConnectedImageFilter and
// IsolatedConnectedImageFilter; Apache-2.0, Copyright NumFOCUS; see ITK's
// NOTICE). Modifications: the flood fill is this package's span fill over
// run-length masks (see FloodFill3D) instead of ITK's per-pixel queue over a
// dense output image, so memory follows the region's runs; pixel values are
// float64 from a callback that may report unavailable voxels, which never
// join a region and are skipped by the statistics; 6/18/26 connectivity is
// selectable; every fill is bounded by a voxel budget and polls a context;
// the isolated search can stop a fill as soon as it reaches a second seed;
// and results report the interval, ignored seeds and, for the isolated
// search, whether the seeds were separated instead of only setting a flag.

var (
	// ErrRegionGrowInvalidVolume reports volume dimensions or a value source
	// that cannot describe a volume.
	ErrRegionGrowInvalidVolume = errors.New("roi: invalid region growing volume")
	// ErrRegionGrowNoSeeds reports an empty seed list.
	ErrRegionGrowNoSeeds = errors.New("roi: region growing requires at least one seed")
	// ErrRegionGrowInvalidParameters reports a NaN or inverted interval, a
	// negative radius or iteration count, a non-positive tolerance or an
	// isolated seed outside the volume.
	ErrRegionGrowInvalidParameters = errors.New("roi: invalid region growing parameters")
	// ErrRegionGrowResourceLimit reports a fill larger than the voxel budget,
	// a neighbourhood larger than its budget or too many fills.
	ErrRegionGrowResourceLimit = errors.New("roi: region growing resource limit exceeded")
	// ErrRegionGrowCancelled reports cancellation; the error also wraps
	// context.Canceled or context.DeadlineExceeded.
	ErrRegionGrowCancelled = errors.New("roi: region growing cancelled")
)

const (
	defaultRegionGrowMaxRegionVoxels       = 64 * 1024 * 1024
	defaultRegionGrowMaxNeighborhoodVoxels = 33 * 33 * 33
	defaultRegionGrowMaxFills              = 64
)

// RegionGrowSeed is a voxel index: column X, row Y and slice ordinal Slice
// (0 for a single image).
type RegionGrowSeed struct{ X, Y, Slice int }

// RegionGrowVolume is the image a region grows in: Columns×Rows×Slices
// voxels (Slices = 1 for a single image) whose values ValueAt returns.
// ValueAt reports false for unavailable voxels (padding, undecodable
// frames, slices of another size); those never join a region, and NaN
// values never satisfy an interval. ValueAt is only called with in-range
// indices and only on the region, its border and the neighbourhoods the
// criterion reads; nothing dense is allocated.
type RegionGrowVolume struct {
	Columns, Rows, Slices int
	ValueAt               func(x, y, slice int) (float64, bool)
}

// RegionGrowLimits bounds the work of one call. A zero field uses the
// default; negative fields are rejected.
type RegionGrowLimits struct {
	// MaxRegionVoxels caps the voxels of every fill (default 64 Mi).
	MaxRegionVoxels int
	// MaxNeighborhoodVoxels caps the box a neighbourhood criterion or the
	// initial confidence statistics read around a voxel (default 33³,
	// a radius of 16 in 3D).
	MaxNeighborhoodVoxels int
	// MaxFills caps the fills of one call: the confidence iterations plus
	// one, or the isolated bisection steps plus one (default 64).
	MaxFills int
}

// DefaultRegionGrowLimits returns the limits used for zero fields.
func DefaultRegionGrowLimits() RegionGrowLimits {
	return RegionGrowLimits{
		MaxRegionVoxels:       defaultRegionGrowMaxRegionVoxels,
		MaxNeighborhoodVoxels: defaultRegionGrowMaxNeighborhoodVoxels,
		MaxFills:              defaultRegionGrowMaxFills,
	}
}

func (l RegionGrowLimits) normalize() (RegionGrowLimits, error) {
	if l.MaxRegionVoxels < 0 || l.MaxNeighborhoodVoxels < 0 || l.MaxFills < 0 {
		return RegionGrowLimits{}, fmt.Errorf("%w: negative limit", ErrRegionGrowInvalidParameters)
	}
	defaults := DefaultRegionGrowLimits()
	if l.MaxRegionVoxels == 0 {
		l.MaxRegionVoxels = defaults.MaxRegionVoxels
	}
	if l.MaxNeighborhoodVoxels == 0 {
		l.MaxNeighborhoodVoxels = defaults.MaxNeighborhoodVoxels
	}
	if l.MaxFills == 0 {
		l.MaxFills = defaults.MaxFills
	}
	return l, nil
}

// RegionGrowOptions are shared by the region growing functions.
type RegionGrowOptions struct {
	// Connectivity joins voxels by faces (6, the default and ITK's), edges
	// (18) or corners (26). On a single image 6 is 4-connected and 18 or 26
	// are 8-connected.
	Connectivity Connectivity3D
	Limits       RegionGrowLimits
}

// RegionGrowResult is a grown region.
type RegionGrowResult struct {
	// Masks holds the filled mask of every slice the region touches.
	Masks map[int]*RasterMask
	// VoxelCount is the number of voxels in the region.
	VoxelCount int
	// Lower and Upper are the closed value interval of the final fill.
	Lower, Upper float64
	// IgnoredSeeds lists the seeds that did not start a region: outside the
	// volume, unavailable, or not satisfying the final criterion.
	IgnoredSeeds []RegionGrowSeed
	// Fills is the number of flood fills that were run.
	Fills int
}

// NeighborhoodRadius is the half size of the box a neighbourhood criterion
// reads around a voxel: (2X+1)×(2Y+1)×(2Slice+1) voxels. Slice is ignored on
// a single image.
type NeighborhoodRadius struct{ X, Y, Slice int }

// UniformNeighborhoodRadius returns the radius r along every axis.
func UniformNeighborhoodRadius(r int) NeighborhoodRadius {
	return NeighborhoodRadius{X: r, Y: r, Slice: r}
}

// ConfidenceConnectedParams configure GrowConfidenceConnected. The zero
// value is not ITK's default: use DefaultConfidenceConnectedParams.
type ConfidenceConnectedParams struct {
	// Multiplier k of the interval mean ± k·σ.
	Multiplier float64
	// Iterations is the number of times the statistics are re-estimated on
	// the grown region and the region is grown again.
	Iterations int
	// InitialRadius is the radius of the box around each seed that the
	// initial statistics are taken from; 0 uses the seed values only.
	InitialRadius int

	// quantizeBounds, when set, maps each interval before it is used. The
	// ITK oracle tests use it to reproduce the cast of the bounds to an
	// 8-bit pixel type.
	quantizeBounds func(lower, upper float64) (float64, float64)
}

// DefaultConfidenceConnectedParams returns ITK's defaults: a multiplier of
// 2.5, 4 iterations and an initial radius of 1.
func DefaultConfidenceConnectedParams() ConfidenceConnectedParams {
	return ConfidenceConnectedParams{Multiplier: 2.5, Iterations: 4, InitialRadius: 1}
}

// ConfidenceStop tells why GrowConfidenceConnected stopped iterating.
type ConfidenceStop int

const (
	// ConfidenceStopIterations: every requested iteration ran.
	ConfidenceStopIterations ConfidenceStop = iota
	// ConfidenceStopZeroVariance: the region's variance was zero, so a
	// new interval could not differ from the seed intensities.
	ConfidenceStopZeroVariance
	// ConfidenceStopEmptyRegion: no seed started a region.
	ConfidenceStopEmptyRegion
)

func (s ConfidenceStop) String() string {
	switch s {
	case ConfidenceStopZeroVariance:
		return "zero variance"
	case ConfidenceStopEmptyRegion:
		return "empty region"
	default:
		return "iterations completed"
	}
}

// ConfidenceConnectedResult is a confidence-connected region.
type ConfidenceConnectedResult struct {
	RegionGrowResult
	// Mean and Variance are the statistics that defined the final interval
	// (sample variance, n−1 denominator).
	Mean, Variance float64
	// InitialSamples is the number of voxel values behind the initial
	// statistics.
	InitialSamples int
	// Iterations is the number of re-estimations that changed the interval.
	Iterations int
	Stop       ConfidenceStop
}

// IsolatedConnectedParams configure GrowIsolatedConnected.
type IsolatedConnectedParams struct {
	// Lower and Upper bound the search. When FindUpperThreshold is set the
	// region is [Lower, t] and t is searched in [Lower, Upper] (seeds 1
	// darker than seeds 2, e.g. contrast vessel next to bone); otherwise the
	// region is [t, Upper] and t is searched downwards.
	Lower, Upper       float64
	FindUpperThreshold bool
	// Tolerance ends the bisection (ITK's IsolatedValueTolerance; 1 when
	// zero).
	Tolerance float64

	// integerMidpoint truncates every bisection midpoint toward zero, as
	// ITK's integer accumulate types do; the ITK oracle tests set it.
	integerMidpoint bool
}

// IsolatedConnectedResult is an isolated-connected region.
type IsolatedConnectedResult struct {
	RegionGrowResult
	// IsolatedValue is the threshold found: the upper bound of the region
	// when FindUpperThreshold was set, otherwise its lower bound.
	IsolatedValue float64
	// ThresholdingFailed reports that no threshold in the search range
	// separates the seeds: some first seed is outside the region or some
	// second seed is inside it. The region is still returned.
	ThresholdingFailed bool
}

// regionGrower carries the validated volume, connectivity and budget of one
// call.
type regionGrower struct {
	ctx        context.Context
	volume     RegionGrowVolume
	neighbours []fillNeighbour
	limits     RegionGrowLimits
	fills      int
}

func newRegionGrower(ctx context.Context, volume RegionGrowVolume, options RegionGrowOptions) (*regionGrower, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := regionGrowContextError(ctx); err != nil {
		return nil, err
	}
	if volume.ValueAt == nil {
		return nil, fmt.Errorf("%w: nil value source", ErrRegionGrowInvalidVolume)
	}
	if _, err := RasterMaskPixelCount(volume.Columns, volume.Rows); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRegionGrowInvalidVolume, err)
	}
	if volume.Slices <= 0 {
		return nil, fmt.Errorf("%w: %d slices", ErrRegionGrowInvalidVolume, volume.Slices)
	}
	connectivity := options.Connectivity
	if connectivity == 0 {
		connectivity = Connectivity6
	}
	if err := connectivity.validate(); err != nil {
		return nil, err
	}
	limits, err := options.Limits.normalize()
	if err != nil {
		return nil, err
	}
	return &regionGrower{ctx: ctx, volume: volume, neighbours: fillNeighbourRows(connectivity), limits: limits}, nil
}

func regionGrowContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrRegionGrowCancelled, err)
	}
	return nil
}

func (g *regionGrower) contains(seed RegionGrowSeed) bool {
	return seed.X >= 0 && seed.Y >= 0 && seed.Slice >= 0 &&
		seed.X < g.volume.Columns && seed.Y < g.volume.Rows && seed.Slice < g.volume.Slices
}

// value returns a voxel value that can satisfy an interval.
func (g *regionGrower) value(x, y, z int) (float64, bool) {
	v, ok := g.volume.ValueAt(x, y, z)
	return v, ok && !math.IsNaN(v)
}

func (g *regionGrower) inInterval(lower, upper float64) func(x, y, z int) bool {
	return func(x, y, z int) bool {
		v, ok := g.value(x, y, z)
		return ok && v >= lower && v <= upper
	}
}

// fill grows the union of the regions started by the seeds that are inside
// the volume and satisfy inside. stop, when non-nil, ends the fill after the
// first span it accepts.
func (g *regionGrower) fill(seeds []RegionGrowSeed, inside func(x, y, z int) bool, stop func(z, y, x0, x1 int) bool) (map[int]*RasterMask, int, error) {
	if g.fills >= g.limits.MaxFills {
		return nil, 0, fmt.Errorf("%w: more than %d fills", ErrRegionGrowResourceLimit, g.limits.MaxFills)
	}
	g.fills++
	f := spanFiller{
		columns: g.volume.Columns, rows: g.volume.Rows, slices: g.volume.Slices,
		inside:     inside,
		neighbours: g.neighbours,
		masks:      map[int]*RasterMask{},
		ctx:        g.ctx,
		maxVoxels:  g.limits.MaxRegionVoxels,
		stop:       stop,
	}
	for _, seed := range seeds {
		if !g.contains(seed) {
			continue
		}
		f.fill(seed.X, seed.Y, seed.Slice)
		if f.err != nil {
			return nil, 0, f.err
		}
		if f.stopped {
			break
		}
	}
	if err := regionGrowContextError(g.ctx); err != nil {
		return nil, 0, err
	}
	return f.masks, f.filled, nil
}

func (g *regionGrower) result(seeds []RegionGrowSeed, masks map[int]*RasterMask, count int, lower, upper float64) RegionGrowResult {
	out := RegionGrowResult{Masks: masks, VoxelCount: count, Lower: lower, Upper: upper, Fills: g.fills}
	for _, seed := range seeds {
		if !g.contains(seed) || !masks[seed.Slice].Get(seed.X, seed.Y) {
			out.IgnoredSeeds = append(out.IgnoredSeeds, seed)
		}
	}
	return out
}

func validateInterval(lower, upper float64) error {
	if math.IsNaN(lower) || math.IsNaN(upper) || lower > upper {
		return fmt.Errorf("%w: interval [%g, %g]", ErrRegionGrowInvalidParameters, lower, upper)
	}
	return nil
}

// GrowConnectedThreshold grows the voxels connected to any seed whose values
// lie in [lower, upper] (ITK's ConnectedThresholdImageFilter). Seeds outside
// the volume or the interval start nothing and are reported in IgnoredSeeds;
// with no usable seed the region is empty.
func GrowConnectedThreshold(ctx context.Context, volume RegionGrowVolume, seeds []RegionGrowSeed, lower, upper float64, options RegionGrowOptions) (*RegionGrowResult, error) {
	g, err := newRegionGrower(ctx, volume, options)
	if err != nil {
		return nil, err
	}
	if len(seeds) == 0 {
		return nil, ErrRegionGrowNoSeeds
	}
	if err := validateInterval(lower, upper); err != nil {
		return nil, err
	}
	masks, count, err := g.fill(seeds, g.inInterval(lower, upper), nil)
	if err != nil {
		return nil, err
	}
	result := g.result(seeds, masks, count, lower, upper)
	return &result, nil
}

// boxRange returns the clamped index range [lo, hi] of a radius around c.
func boxRange(c, r, n int) (int, int) {
	return max(c-r, 0), min(c+r, n-1)
}

func (g *regionGrower) boxVoxels(radius NeighborhoodRadius) (int, error) {
	if radius.X < 0 || radius.Y < 0 || radius.Slice < 0 {
		return 0, fmt.Errorf("%w: negative radius %+v", ErrRegionGrowInvalidParameters, radius)
	}
	size := 1
	for _, r := range []int{radius.X, radius.Y, radius.Slice} {
		side := 2*r + 1
		if side <= 0 || size > g.limits.MaxNeighborhoodVoxels/side {
			return 0, fmt.Errorf("%w: neighbourhood %+v exceeds %d voxels", ErrRegionGrowResourceLimit, radius, g.limits.MaxNeighborhoodVoxels)
		}
		size *= side
	}
	return size, nil
}

// GrowNeighborhoodConnected grows the voxels connected to any seed whose
// whole box of the given radius lies in [lower, upper] (ITK's
// NeighborhoodConnectedImageFilter). Requiring the neighbourhood keeps the
// region from leaking through bridges thinner than the box; the region is
// the set of box centres, so it stays about one radius inside the object's
// edge. Boxes are clamped at the volume border, as ITK's zero-flux boundary
// condition does, and an unavailable voxel in a box rejects its centre.
// Several seeds grow one region.
func GrowNeighborhoodConnected(ctx context.Context, volume RegionGrowVolume, seeds []RegionGrowSeed, lower, upper float64, radius NeighborhoodRadius, options RegionGrowOptions) (*RegionGrowResult, error) {
	g, err := newRegionGrower(ctx, volume, options)
	if err != nil {
		return nil, err
	}
	if len(seeds) == 0 {
		return nil, ErrRegionGrowNoSeeds
	}
	if err := validateInterval(lower, upper); err != nil {
		return nil, err
	}
	if volume.Slices == 1 {
		radius.Slice = 0
	}
	if _, err := g.boxVoxels(radius); err != nil {
		return nil, err
	}
	inside := func(x, y, z int) bool {
		// The centre first: most rejected candidates fail there.
		if v, ok := g.value(x, y, z); !ok || v < lower || v > upper {
			return false
		}
		z0, z1 := boxRange(z, radius.Slice, g.volume.Slices)
		y0, y1 := boxRange(y, radius.Y, g.volume.Rows)
		x0, x1 := boxRange(x, radius.X, g.volume.Columns)
		for zz := z0; zz <= z1; zz++ {
			for yy := y0; yy <= y1; yy++ {
				for xx := x0; xx <= x1; xx++ {
					if v, ok := g.value(xx, yy, zz); !ok || v < lower || v > upper {
						return false
					}
				}
			}
		}
		return true
	}
	masks, count, err := g.fill(seeds, inside, nil)
	if err != nil {
		return nil, err
	}
	result := g.result(seeds, masks, count, lower, upper)
	return &result, nil
}

// GrowConfidenceConnected grows a region from the seeds whose interval is
// mean ± Multiplier·σ (ITK's ConfidenceConnectedImageFilter). The initial
// mean is the average of the means of the (2r+1)^d boxes around the seeds
// (clamped at the border and counting clamped voxels again, as ITK does) and
// the initial variance pools all box samples; with InitialRadius 0 the seed
// values are used. The interval always contains every seed value. The
// region is then grown and, Iterations times, the mean and variance are
// re-estimated over the whole region and the region is grown again from
// the seeds; a zero variance stops early. Seeds outside the volume or with
// an unavailable value are ignored; with no usable seed the region is
// empty.
func GrowConfidenceConnected(ctx context.Context, volume RegionGrowVolume, seeds []RegionGrowSeed, params ConfidenceConnectedParams, options RegionGrowOptions) (*ConfidenceConnectedResult, error) {
	g, err := newRegionGrower(ctx, volume, options)
	if err != nil {
		return nil, err
	}
	if len(seeds) == 0 {
		return nil, ErrRegionGrowNoSeeds
	}
	if math.IsNaN(params.Multiplier) || math.IsInf(params.Multiplier, 0) || params.Multiplier < 0 || params.Iterations < 0 || params.InitialRadius < 0 {
		return nil, fmt.Errorf("%w: confidence parameters %+v", ErrRegionGrowInvalidParameters, params)
	}
	if params.Iterations >= g.limits.MaxFills {
		return nil, fmt.Errorf("%w: %d iterations need more than %d fills", ErrRegionGrowResourceLimit, params.Iterations, g.limits.MaxFills)
	}
	radius := UniformNeighborhoodRadius(params.InitialRadius)
	if volume.Slices == 1 {
		radius.Slice = 0
	}
	if _, err := g.boxVoxels(radius); err != nil {
		return nil, err
	}

	// Initial statistics and the seed intensity range.
	var meanSum, sumSquares float64
	samples, usedSeeds := 0, 0
	lowestSeed, highestSeed := math.Inf(1), math.Inf(-1)
	for _, seed := range seeds {
		if !g.contains(seed) {
			continue
		}
		seedValue, ok := g.value(seed.X, seed.Y, seed.Slice)
		if !ok || math.IsInf(seedValue, 0) {
			continue
		}
		var sum, squares float64
		n := 0
		z0, z1 := boxRange(seed.Slice, radius.Slice, volume.Slices)
		y0, y1 := boxRange(seed.Y, radius.Y, volume.Rows)
		x0, x1 := boxRange(seed.X, radius.X, volume.Columns)
		// Box positions outside the volume read the nearest border voxel,
		// which is counted again (ITK's zero-flux Neumann boundary).
		for dz := -radius.Slice; dz <= radius.Slice; dz++ {
			zz := min(max(seed.Slice+dz, z0), z1)
			for dy := -radius.Y; dy <= radius.Y; dy++ {
				yy := min(max(seed.Y+dy, y0), y1)
				for dx := -radius.X; dx <= radius.X; dx++ {
					xx := min(max(seed.X+dx, x0), x1)
					v, ok := g.value(xx, yy, zz)
					if !ok || math.IsInf(v, 0) {
						continue
					}
					sum += v
					squares += v * v
					n++
				}
			}
		}
		if n == 0 {
			continue
		}
		meanSum += sum / float64(n)
		sumSquares += squares
		samples += n
		usedSeeds++
		lowestSeed = math.Min(lowestSeed, seedValue)
		highestSeed = math.Max(highestSeed, seedValue)
	}
	result := &ConfidenceConnectedResult{InitialSamples: samples}
	if usedSeeds == 0 {
		result.RegionGrowResult = g.result(seeds, map[int]*RasterMask{}, 0, math.NaN(), math.NaN())
		result.Mean, result.Variance = math.NaN(), math.NaN()
		result.Stop = ConfidenceStopEmptyRegion
		return result, nil
	}
	mean := meanSum / float64(usedSeeds)
	variance := sampleVariance(sumSquares-mean*mean*float64(samples), samples)

	bounds := func(mean, variance float64) (float64, float64) {
		spread := params.Multiplier * math.Sqrt(variance)
		lower, upper := mean-spread, mean+spread
		// Keep every seed inside, otherwise nothing would be grown.
		lower, upper = math.Min(lower, lowestSeed), math.Max(upper, highestSeed)
		if params.quantizeBounds != nil {
			lower, upper = params.quantizeBounds(lower, upper)
		}
		return lower, upper
	}
	lower, upper := bounds(mean, variance)
	masks, count, err := g.fill(seeds, g.inInterval(lower, upper), nil)
	if err != nil {
		return nil, err
	}
	result.Mean, result.Variance = mean, variance
	for iteration := 0; iteration < params.Iterations; iteration++ {
		var sum, squares float64
		n := 0
		// Sorted slices keep the floating-point sums reproducible.
		for _, slice := range sortedMaskSlices(masks) {
			masks[slice].ForEachRun(func(y int, run MaskRun) {
				for x := run.Start; x < run.End; x++ {
					if v, ok := g.value(x, y, slice); ok {
						sum += v
						squares += v * v
						n++
					}
				}
			})
		}
		if err := regionGrowContextError(g.ctx); err != nil {
			return nil, err
		}
		if n == 0 {
			result.Stop = ConfidenceStopEmptyRegion
			break
		}
		regionMean := sum / float64(n)
		regionVariance := sampleVariance(squares-sum*sum/float64(n), n)
		if regionVariance <= 0.1*epsilon64 {
			result.Stop = ConfidenceStopZeroVariance
			break
		}
		lower, upper = bounds(regionMean, regionVariance)
		masks, count, err = g.fill(seeds, g.inInterval(lower, upper), nil)
		if err != nil {
			return nil, err
		}
		result.Mean, result.Variance = regionMean, regionVariance
		result.Iterations++
	}
	result.RegionGrowResult = g.result(seeds, masks, count, lower, upper)
	return result, nil
}

func sortedMaskSlices(masks map[int]*RasterMask) []int {
	slices := make([]int, 0, len(masks))
	for slice := range masks {
		slices = append(slices, slice)
	}
	sort.Ints(slices)
	return slices
}

// epsilon64 is the float64 machine epsilon; ITK treats a variance within a
// tenth of it as zero.
const epsilon64 = 2.220446049250313e-16

// sampleVariance divides a centred sum of squares by n−1, returning 0 for
// fewer than two samples or a negative rounding residue.
func sampleVariance(centredSquares float64, n int) float64 {
	if n < 2 || !(centredSquares > 0) {
		return 0
	}
	return centredSquares / float64(n-1)
}

// GrowIsolatedConnected grows the region connected to seeds1 that excludes
// seeds2, bisecting the threshold that separates them (ITK's
// IsolatedConnectedImageFilter). With FindUpperThreshold the region is
// [Lower, t] and t starts at Upper and is lowered whenever the fill from
// seeds1 reaches a seed of seeds2; otherwise the region is [t, Upper] and t
// starts at Lower and is raised. The bisection stops, as ITK's does, once
// the midpoint is within Tolerance of the separating side of the bracket, so
// IsolatedValue is that side and lies within 2·Tolerance of the first value
// that joins the seeds; the region is grown once more with it. When no threshold separates the
// seeds the region is still returned with ThresholdingFailed set. All seeds
// must lie inside the volume. Fills during the bisection stop as soon as
// they reach a second seed.
func GrowIsolatedConnected(ctx context.Context, volume RegionGrowVolume, seeds1, seeds2 []RegionGrowSeed, params IsolatedConnectedParams, options RegionGrowOptions) (*IsolatedConnectedResult, error) {
	g, err := newRegionGrower(ctx, volume, options)
	if err != nil {
		return nil, err
	}
	if len(seeds1) == 0 || len(seeds2) == 0 {
		return nil, fmt.Errorf("%w: isolated growing needs seeds on both sides", ErrRegionGrowNoSeeds)
	}
	for _, seed := range append(append([]RegionGrowSeed(nil), seeds1...), seeds2...) {
		if !g.contains(seed) {
			return nil, fmt.Errorf("%w: seed %+v outside the volume", ErrRegionGrowInvalidParameters, seed)
		}
	}
	if err := validateInterval(params.Lower, params.Upper); err != nil {
		return nil, err
	}
	if math.IsInf(params.Lower, 0) || math.IsInf(params.Upper, 0) {
		return nil, fmt.Errorf("%w: the isolated search range must be finite", ErrRegionGrowInvalidParameters)
	}
	tolerance := params.Tolerance
	if tolerance == 0 {
		tolerance = 1
	}
	if !(tolerance > 0) || math.IsInf(tolerance, 0) {
		return nil, fmt.Errorf("%w: tolerance %g", ErrRegionGrowInvalidParameters, params.Tolerance)
	}
	midpoint := func(a, b float64) float64 {
		m := (a + b) / 2
		if params.integerMidpoint {
			m = math.Trunc(m)
		}
		return m
	}
	reachesSeeds2 := func(z, y, x0, x1 int) bool {
		for _, seed := range seeds2 {
			if seed.Slice == z && seed.Y == y && seed.X >= x0 && seed.X < x1 {
				return true
			}
		}
		return false
	}
	// reached grows [lo, hi] from seeds1 and reports whether it touched a
	// second seed.
	reached := func(lo, hi float64) (bool, error) {
		touched := false
		_, _, err := g.fill(seeds1, g.inInterval(lo, hi), func(z, y, x0, x1 int) bool {
			if reachesSeeds2(z, y, x0, x1) {
				touched = true
			}
			return touched
		})
		return touched, err
	}

	lower, upper := params.Lower, params.Upper
	var isolated float64
	if params.FindUpperThreshold {
		for guess := upper; lower+tolerance < guess; guess = midpoint(lower, upper) {
			hit, err := reached(params.Lower, guess)
			if err != nil {
				return nil, err
			}
			if hit {
				upper = guess
			} else {
				lower = guess
			}
		}
		isolated = lower
	} else {
		for guess := lower; guess < upper-tolerance; guess = midpoint(lower, upper) {
			hit, err := reached(guess, params.Upper)
			if err != nil {
				return nil, err
			}
			if hit {
				lower = guess
			} else {
				upper = guess
			}
		}
		isolated = upper
	}
	regionLower, regionUpper := params.Lower, isolated
	if !params.FindUpperThreshold {
		regionLower, regionUpper = isolated, params.Upper
	}
	masks, count, err := g.fill(seeds1, g.inInterval(regionLower, regionUpper), nil)
	if err != nil {
		return nil, err
	}
	result := &IsolatedConnectedResult{
		RegionGrowResult: g.result(seeds1, masks, count, regionLower, regionUpper),
		IsolatedValue:    isolated,
	}
	failed := len(result.IgnoredSeeds) > 0
	for _, seed := range seeds2 {
		if masks[seed.Slice].Get(seed.X, seed.Y) {
			failed = true
		}
	}
	result.ThresholdingFailed = failed
	return result, nil
}
