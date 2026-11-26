package roi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"strings"
)

// VectorConfidenceChannel describes one aligned, immutable scalar callback.
// Values are normalized as (raw-Offset)/Scale. Scale must be positive, including
// when no normalization is desired (Scale=1, Offset=0). Unit describes raw values.
// Grid and FrameOfReferenceID must exactly match the volume. Callbacks must return
// promptly and remain stable throughout the call; context cannot interrupt them.
type VectorConfidenceChannel struct {
	ID, Unit, FrameOfReferenceID string
	Grid                         VoxelGrid
	Offset, Scale                float64
	ValueAt                      func(x, y, slice int) (float64, bool)
}

// VectorConfidenceVolume is an explicitly aligned collection of channels in mm.
// IDs, units and frame IDs must be nonempty and at most 256 bytes. No registration
// or resampling is inferred. Only encountered coordinates are read and validated.
type VectorConfidenceVolume struct {
	Grid               VoxelGrid
	FrameOfReferenceID string
	Channels           []VectorConfidenceChannel
}

// VectorCovariancePolicy specifies how the normalized covariance is used.
type VectorCovariancePolicy uint8

const (
	// VectorCovarianceRequirePositiveDefinite refuses singular or poorly admitted
	// covariance. This is the zero/default policy, with no inferred regularization.
	VectorCovarianceRequirePositiveDefinite VectorCovariancePolicy = iota
	// VectorCovarianceRidge adds the explicitly specified Ridge to every diagonal.
	VectorCovarianceRidge
)

// VectorCovarianceNormalization selects the centered-moment denominator.
type VectorCovarianceNormalization uint8

const (
	// VectorCovarianceSample uses n-1, the unbiased sample covariance (default).
	VectorCovarianceSample VectorCovarianceNormalization = iota
	// VectorCovariancePopulation uses n, explicitly matching the selected ITK
	// vector-confidence profile. Both modes require at least two samples.
	VectorCovariancePopulation
)

// VectorConfidenceParams controls the normalized Mahalanobis ellipsoid. Iterations
// counts refits/regrows AFTER the initial fill. InitialRadius is an index radius;
// a single slice uses a 2D box. Border samples are clamped and counted repeatedly.
// MinPivotRatio=0 selects 1e-12; it is a Cholesky admission ratio, not a condition
// number guarantee. Every seed is included by explicitly expanding the radius.
type VectorConfidenceParams struct {
	Multiplier              float64
	Iterations              int
	InitialRadius           int
	CovariancePolicy        VectorCovariancePolicy
	CovarianceNormalization VectorCovarianceNormalization
	Ridge                   float64
	MinPivotRatio           float64
}

// DefaultVectorConfidenceParams returns explicit defaults; the zero struct is
// invalid because Multiplier must be positive. Radius=0 and Iterations=0 are valid.
func DefaultVectorConfidenceParams() VectorConfidenceParams {
	return VectorConfidenceParams{Multiplier: 2.5, Iterations: 4, InitialRadius: 1}
}

// VectorConfidenceLimits bounds the grid, region, samples, matrices and work.
// Zero fields select defaults; negative integer fields or over-hard values refuse.
// Estimated bytes count structural storage, not RSS, Go heap or callback memory.
// See VECTOR_CONFIDENCE_GROWING.md for formulas and the work unit definition.
type VectorConfidenceLimits struct {
	MaxChannels, MaxGridVoxels, MaxRegionVoxels, MaxNeighborhoodSamples int
	MaxSeeds, MaxFills                                                  int
	MaxWork, MaxEstimatedBytes                                          uint64
}

// DefaultVectorConfidenceLimits returns bounded library defaults.
func DefaultVectorConfidenceLimits() VectorConfidenceLimits {
	return VectorConfidenceLimits{16, 1 << 24, 1 << 22, 1 << 20, 4096, 16, 1 << 29, 128 << 20}
}

// VectorConfidenceOptions selects connectivity (zero means 6) and finite limits.
type VectorConfidenceOptions struct {
	Connectivity Connectivity3D
	Limits       VectorConfidenceLimits
}

// VectorConfidenceChannelInfo is owned metadata for a normalized result channel.
type VectorConfidenceChannelInfo struct {
	ID, Unit      string
	Offset, Scale float64
}

// VectorConfidenceResult owns the returned mask and model arrays. Covariance and
// AppliedCovariance are row-major normalized matrices; Mean/ModelSamples describe
// the model used by the LAST fill, not post-fill statistics of the returned mask.
// EffectiveMultiplier reports seed-driven expansion of the requested multiplier.
type VectorConfidenceResult struct {
	CovarianceNormalization                        VectorCovarianceNormalization
	Masks                                          map[int]*RasterMask
	Grid                                           VoxelGrid
	FrameOfReferenceID                             string
	Channels                                       []VectorConfidenceChannelInfo
	Mean, Covariance, AppliedCovariance            []float64
	EffectiveMultiplier                            float64
	VoxelCount, InitialSamples, ModelSamples       int
	UniqueSeeds, DuplicateSeeds, Iterations, Fills int
	CallbackReads, Work, EstimatedBytes            uint64
}

var (
	// ErrVectorConfidenceGeometry reports mismatched or unusable physical grids.
	ErrVectorConfidenceGeometry = errors.New("roi: vector confidence geometry mismatch")
	// ErrVectorConfidenceUnavailable reports an encountered unavailable channel.
	ErrVectorConfidenceUnavailable = errors.New("roi: vector confidence channel unavailable")
	// ErrVectorConfidenceNumeric reports nonfinite, out-of-range or failed arithmetic.
	ErrVectorConfidenceNumeric = errors.New("roi: vector confidence numeric failure")
	// ErrVectorConfidenceCovariance reports insufficient samples or rejected pivots.
	ErrVectorConfidenceCovariance = errors.New("roi: vector confidence covariance not admitted")
)

type vectorConfidenceGrower struct {
	ctx                   context.Context
	volume                VectorConfidenceVolume
	params                VectorConfidenceParams
	limits                VectorConfidenceLimits
	connectivity          Connectivity3D
	n, regionCap, samples int
	estimate, work, reads uint64
	state                 []byte
	queue, seeds          []uint32
	seedValues            []float64
	model                 vectorConfidenceModel
	count                 int
	effective             float64
}

// GrowVectorConfidenceConnected estimates the selected covariance in normalized
// channels and grows seed-connected Mahalanobis ellipsoids. It uses dense BINARY
// visitation, a bounded index queue and channel callbacks, never a dense vector
// volume. Unavailable/nonfinite encountered samples, covariance rejection, resource
// exhaustion and cancellation return nil result. No partial mask is published.
func GrowVectorConfidenceConnected(ctx context.Context, volume VectorConfidenceVolume, seeds []RegionGrowSeed, params VectorConfidenceParams, options VectorConfidenceOptions) (*VectorConfidenceResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	g := vectorConfidenceGrower{ctx: ctx, volume: volume, params: params, connectivity: options.Connectivity}
	if err := g.prepare(seeds, options.Limits); err != nil {
		return nil, err
	}
	for i, index := range g.seeds {
		if err := g.read(index, g.seedValues[i*len(volume.Channels):(i+1)*len(volume.Channels)]); err != nil {
			return nil, err
		}
	}
	if err := g.initialStatistics(); err != nil {
		return nil, err
	}
	initialSamples := g.model.samples
	for fill := 0; fill <= params.Iterations; fill++ {
		if fill > 0 {
			if err := g.regionStatistics(); err != nil {
				return nil, err
			}
		}
		if err := g.model.factor(&g); err != nil {
			return nil, err
		}
		g.effective = params.Multiplier
		for i := range g.seeds {
			d, err := g.model.distance(&g, g.seedValues[i*len(volume.Channels):(i+1)*len(volume.Channels)])
			if err != nil {
				return nil, err
			}
			g.effective = math.Max(g.effective, d)
		}
		if err := g.fill(); err != nil {
			return nil, err
		}
	}
	masks, err := g.masks()
	if err != nil {
		return nil, err
	}
	c := len(volume.Channels)
	if err := g.charge(uint64(2*c*c + 2*c)); err != nil {
		return nil, err
	}
	result := &VectorConfidenceResult{
		CovarianceNormalization: g.params.CovarianceNormalization,
		Masks:                   masks, Grid: volume.Grid, FrameOfReferenceID: volume.FrameOfReferenceID,
		Channels: make([]VectorConfidenceChannelInfo, c),
		Mean:     append([]float64(nil), g.model.mean...), Covariance: append([]float64(nil), g.model.cov...),
		AppliedCovariance: append([]float64(nil), g.model.applied...), EffectiveMultiplier: g.effective,
		VoxelCount: g.count, InitialSamples: initialSamples, ModelSamples: g.model.samples,
		UniqueSeeds: len(g.seeds), DuplicateSeeds: len(seeds) - len(g.seeds),
		Iterations: params.Iterations, Fills: params.Iterations + 1,
		CallbackReads: g.reads, Work: g.work, EstimatedBytes: g.estimate,
	}
	for i, ch := range volume.Channels {
		result.Channels[i] = VectorConfidenceChannelInfo{ch.ID, ch.Unit, ch.Offset, ch.Scale}
	}
	if err := regionGrowContextError(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

func (g *vectorConfidenceGrower) prepare(seeds []RegionGrowSeed, limits VectorConfidenceLimits) error {
	if err := regionGrowContextError(g.ctx); err != nil {
		return err
	}
	var err error
	g.limits, err = vectorConfidenceNormalizeLimits(limits)
	if err != nil {
		return err
	}
	if g.connectivity == 0 {
		g.connectivity = Connectivity6
	}
	if err := g.connectivity.validate(); err != nil {
		return err
	}
	p := &g.params
	if p.CovarianceNormalization != VectorCovarianceSample && p.CovarianceNormalization != VectorCovariancePopulation {
		return fmt.Errorf("%w: unknown covariance normalization", ErrRegionGrowInvalidParameters)
	}
	if !vectorConfidenceFinite(p.Multiplier) || p.Multiplier <= 0 || p.Multiplier > 1e6 || p.Iterations < 0 || p.Iterations > 64 || p.InitialRadius < 0 || p.InitialRadius > 16 {
		return fmt.Errorf("%w: vector confidence parameters", ErrRegionGrowInvalidParameters)
	}
	if p.MinPivotRatio == 0 {
		p.MinPivotRatio = 1e-12
	}
	if !vectorConfidenceFinite(p.MinPivotRatio) || p.MinPivotRatio < 1e-15 || p.MinPivotRatio > 1e-2 || !vectorConfidenceFinite(p.Ridge) {
		return fmt.Errorf("%w: covariance admission parameters", ErrRegionGrowInvalidParameters)
	}
	switch p.CovariancePolicy {
	case VectorCovarianceRequirePositiveDefinite:
		if p.Ridge != 0 {
			return fmt.Errorf("%w: strict covariance requires zero ridge", ErrRegionGrowInvalidParameters)
		}
	case VectorCovarianceRidge:
		if p.Ridge < 1e-200 || p.Ridge > 1e200 {
			return fmt.Errorf("%w: ridge outside finite profile", ErrRegionGrowInvalidParameters)
		}
	default:
		return fmt.Errorf("%w: unknown covariance policy", ErrRegionGrowInvalidParameters)
	}
	if p.Iterations+1 > g.limits.MaxFills {
		return fmt.Errorf("%w: fill limit", ErrRegionGrowResourceLimit)
	}
	grid := g.volume.Grid
	if grid.Columns <= 0 || grid.Rows <= 0 || grid.Slices <= 0 {
		return fmt.Errorf("%w: non-positive vector grid dimensions", ErrRegionGrowInvalidVolume)
	}
	n, ok := vectorConfidenceProduct(uint64(grid.Columns), uint64(grid.Rows), uint64(grid.Slices))
	if !ok || n > uint64(g.limits.MaxGridVoxels) {
		return fmt.Errorf("%w: grid voxel limit", ErrRegionGrowResourceLimit)
	}
	g.n, g.regionCap = int(n), min(int(n), g.limits.MaxRegionVoxels)
	if err := vectorConfidenceValidateGrid(grid); err != nil {
		return err
	}
	c := len(g.volume.Channels)
	if c < 1 || c > g.limits.MaxChannels {
		return fmt.Errorf("%w: channel count", ErrRegionGrowResourceLimit)
	}
	if !vectorConfidenceText(g.volume.FrameOfReferenceID) {
		return fmt.Errorf("%w: frame identity", ErrRegionGrowInvalidVolume)
	}
	for i, ch := range g.volume.Channels {
		if !vectorConfidenceText(ch.ID) || !vectorConfidenceText(ch.Unit) || !vectorConfidenceText(ch.FrameOfReferenceID) || ch.ValueAt == nil {
			return fmt.Errorf("%w: channel %d metadata or callback", ErrRegionGrowInvalidVolume, i)
		}
		for j := 0; j < i; j++ {
			if g.volume.Channels[j].ID == ch.ID {
				return fmt.Errorf("%w: duplicate channel identity", ErrRegionGrowInvalidVolume)
			}
		}
		if ch.Grid != grid || ch.FrameOfReferenceID != g.volume.FrameOfReferenceID {
			return fmt.Errorf("%w: channel %q", ErrVectorConfidenceGeometry, ch.ID)
		}
		if !vectorConfidenceFinite(ch.Offset) || !vectorConfidenceFinite(ch.Scale) || ch.Scale <= 0 {
			return fmt.Errorf("%w: channel %q normalization", ErrRegionGrowInvalidParameters, ch.ID)
		}
	}
	if len(seeds) == 0 {
		return ErrRegionGrowNoSeeds
	}
	if len(seeds) > g.limits.MaxSeeds {
		return fmt.Errorf("%w: seed limit", ErrRegionGrowResourceLimit)
	}
	for i, s := range seeds {
		if err := vectorConfidenceBulkPoll(g.ctx, i); err != nil {
			return err
		}
		if s.X < 0 || s.Y < 0 || s.Slice < 0 || s.X >= grid.Columns || s.Y >= grid.Rows || s.Slice >= grid.Slices {
			return fmt.Errorf("%w: seed outside vector grid", ErrRegionGrowInvalidParameters)
		}
	}
	// Conservative 64-bit structural sizes also bound 32-bit representations.
	// Output map allowance is a model; runtime allocation/RSS are not capped.
	rows := uint64(grid.Rows) * uint64(grid.Slices)
	g.estimate = n + 20*uint64(g.regionCap) + 4*uint64(len(seeds)) + 8*uint64(len(seeds)*c) +
		24*rows + 512*uint64(grid.Slices) + uint64(16*c*c+16*c)*8 + uint64(c)*1024 + 4096
	if g.estimate > g.limits.MaxEstimatedBytes {
		return fmt.Errorf("%w: structural byte limit", ErrRegionGrowResourceLimit)
	}
	// Admit known initialization/sort work before the first workspace allocation.
	if err := g.charge(n + uint64(g.regionCap) + uint64(len(seeds))*(1+uint64(4*bits.Len(uint(len(seeds))))) + uint64(4*c*c+4*c)); err != nil {
		return err
	}
	g.seeds = make([]uint32, len(seeds))
	for i, s := range seeds {
		if err := vectorConfidenceBulkPoll(g.ctx, i); err != nil {
			return err
		}
		g.seeds[i] = uint32((s.Slice*grid.Rows+s.Y)*grid.Columns + s.X)
	}
	if err := vectorConfidenceSortSeeds(g.ctx, g.seeds); err != nil {
		return err
	}
	unique := 0
	for i, index := range g.seeds {
		if err := vectorConfidenceBulkPoll(g.ctx, i); err != nil {
			return err
		}
		if unique == 0 || g.seeds[unique-1] != index {
			g.seeds[unique] = index
			unique++
		}
	}
	g.seeds = g.seeds[:unique]
	side := uint64(2*p.InitialRadius + 1)
	box := side * side
	if grid.Slices > 1 {
		box *= side
	}
	if box*uint64(unique) > uint64(g.limits.MaxNeighborhoodSamples) {
		return fmt.Errorf("%w: initial neighborhood sample limit", ErrRegionGrowResourceLimit)
	}
	g.samples = int(box) * unique
	// Matrix/sample work is charged before each update; known sample read minimum
	// is refused before allocating the remaining workspace.
	if uint64(g.samples)*uint64(3*c) > g.limits.MaxWork-g.work {
		return fmt.Errorf("%w: initial sample work minimum", ErrRegionGrowResourceLimit)
	}
	if err := regionGrowContextError(g.ctx); err != nil {
		return err
	}
	g.state = make([]byte, g.n)
	if err := regionGrowContextError(g.ctx); err != nil {
		return err
	}
	g.queue = make([]uint32, g.regionCap)
	if err := regionGrowContextError(g.ctx); err != nil {
		return err
	}
	g.seedValues = make([]float64, unique*c)
	if err := regionGrowContextError(g.ctx); err != nil {
		return err
	}
	g.model = newVectorConfidenceModel(c)
	return regionGrowContextError(g.ctx)
}

func vectorConfidenceNormalizeLimits(l VectorConfidenceLimits) (VectorConfidenceLimits, error) {
	d := DefaultVectorConfidenceLimits()
	values := []*int{&l.MaxChannels, &l.MaxGridVoxels, &l.MaxRegionVoxels, &l.MaxNeighborhoodSamples, &l.MaxSeeds, &l.MaxFills}
	defaults := []int{d.MaxChannels, d.MaxGridVoxels, d.MaxRegionVoxels, d.MaxNeighborhoodSamples, d.MaxSeeds, d.MaxFills}
	hard := []int{32, 1 << 26, 1 << 26, 1 << 24, 65536, 65}
	for i, v := range values {
		if *v == 0 {
			*v = defaults[i]
		}
		if *v < 0 || *v > hard[i] {
			return l, fmt.Errorf("%w: vector confidence limit outside hard profile", ErrRegionGrowInvalidParameters)
		}
	}
	if l.MaxWork == 0 {
		l.MaxWork = d.MaxWork
	}
	if l.MaxEstimatedBytes == 0 {
		l.MaxEstimatedBytes = d.MaxEstimatedBytes
	}
	if l.MaxWork > 1<<34 || l.MaxEstimatedBytes > 1<<30 {
		return l, fmt.Errorf("%w: work/byte limit outside hard profile", ErrRegionGrowInvalidParameters)
	}
	return l, nil
}

func vectorConfidenceValidateGrid(grid VoxelGrid) error {
	if err := grid.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrVectorConfidenceGeometry, err)
	}
	for _, x := range []float64{0, float64(grid.Columns - 1)} {
		for _, y := range []float64{0, float64(grid.Rows - 1)} {
			for _, z := range []float64{0, float64(grid.Slices - 1)} {
				p := grid.IndexToPoint(x, y, z)
				index, ok := grid.PointToIndex(p)
				if !ok || !finiteVec3(p) || !finiteVec3(index) {
					return fmt.Errorf("%w: nonfinite grid transform/corner", ErrVectorConfidenceGeometry)
				}
			}
		}
	}
	return nil
}

func vectorConfidenceProduct(values ...uint64) (uint64, bool) {
	n := uint64(1)
	for _, v := range values {
		if v != 0 && n > math.MaxUint64/v {
			return 0, false
		}
		n *= v
	}
	return n, true
}

func vectorConfidenceFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func vectorConfidenceText(v string) bool    { return len(v) <= 256 && strings.TrimSpace(v) != "" }

func (g *vectorConfidenceGrower) charge(n uint64) error {
	if err := regionGrowContextError(g.ctx); err != nil {
		return err
	}
	if n > g.limits.MaxWork-g.work {
		return fmt.Errorf("%w: vector confidence work limit", ErrRegionGrowResourceLimit)
	}
	g.work += n
	return nil
}

func (g *vectorConfidenceGrower) coordinates(index uint32) (x, y, z int) {
	n := int(index)
	x = n % g.volume.Grid.Columns
	n /= g.volume.Grid.Columns
	y, z = n%g.volume.Grid.Rows, n/g.volume.Grid.Rows
	return
}

func (g *vectorConfidenceGrower) read(index uint32, dst []float64) error {
	if err := g.charge(uint64(3 * len(g.volume.Channels))); err != nil {
		return err
	}
	x, y, z := g.coordinates(index)
	for i, ch := range g.volume.Channels {
		if err := regionGrowContextError(g.ctx); err != nil {
			return err
		}
		g.reads++
		value, ok := ch.ValueAt(x, y, z)
		if err := regionGrowContextError(g.ctx); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: channel %q at (%d,%d,%d)", ErrVectorConfidenceUnavailable, ch.ID, x, y, z)
		}
		value = (value - ch.Offset) / ch.Scale
		if !vectorConfidenceFinite(value) || math.Abs(value) > 1e100 {
			return fmt.Errorf("%w: channel %q at (%d,%d,%d)", ErrVectorConfidenceNumeric, ch.ID, x, y, z)
		}
		dst[i] = value
	}
	return nil
}

func (g *vectorConfidenceGrower) initialStatistics() error {
	r, rz := g.params.InitialRadius, g.params.InitialRadius
	grid := g.volume.Grid
	if grid.Slices == 1 {
		rz = 0
	}
	for _, index := range g.seeds {
		x, y, z := g.coordinates(index)
		for dz := -rz; dz <= rz; dz++ {
			zz := min(max(z+dz, 0), grid.Slices-1)
			for dy := -r; dy <= r; dy++ {
				yy := min(max(y+dy, 0), grid.Rows-1)
				for dx := -r; dx <= r; dx++ {
					xx := min(max(x+dx, 0), grid.Columns-1)
					if err := g.read(uint32((zz*grid.Rows+yy)*grid.Columns+xx), g.model.values); err != nil {
						return err
					}
					if err := g.model.add(g, g.model.values); err != nil {
						return err
					}
				}
			}
		}
	}
	return g.model.covariance(g)
}

func (g *vectorConfidenceGrower) regionStatistics() error {
	if err := g.model.reset(g); err != nil {
		return err
	}
	if err := g.charge(uint64(g.n)); err != nil {
		return err
	}
	for i, state := range g.state {
		if i%1024 == 0 {
			if err := regionGrowContextError(g.ctx); err != nil {
				return err
			}
		}
		if state == 2 {
			if err := g.read(uint32(i), g.model.values); err != nil {
				return err
			}
			if err := g.model.add(g, g.model.values); err != nil {
				return err
			}
		}
	}
	return g.model.covariance(g)
}

func (g *vectorConfidenceGrower) fill() error {
	if err := g.charge(uint64(g.n)); err != nil {
		return err
	}
	for i := range g.state {
		if i%1024 == 0 {
			if err := regionGrowContextError(g.ctx); err != nil {
				return err
			}
		}
		g.state[i] = 0
	}
	g.count = 0
	for _, index := range g.seeds {
		if err := g.visit(index); err != nil {
			return err
		}
	}
	grid := g.volume.Grid
	for head := 0; head < g.count; head++ {
		if err := g.charge(1); err != nil {
			return err
		}
		x, y, z := g.coordinates(g.queue[head])
		for dz := -1; dz <= 1; dz++ {
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					d := vectorConfidenceAbs(dx) + vectorConfidenceAbs(dy) + vectorConfidenceAbs(dz)
					if d == 0 || g.connectivity == Connectivity6 && d > 1 || g.connectivity == Connectivity18 && d > 2 {
						continue
					}
					if err := g.charge(1); err != nil {
						return err
					}
					xx, yy, zz := x+dx, y+dy, z+dz
					if xx < 0 || yy < 0 || zz < 0 || xx >= grid.Columns || yy >= grid.Rows || zz >= grid.Slices {
						continue
					}
					if err := g.visit(uint32((zz*grid.Rows+yy)*grid.Columns + xx)); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func vectorConfidenceAbs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func vectorConfidenceBulkPoll(ctx context.Context, visit int) error {
	if visit%1024 == 0 {
		return regionGrowContextError(ctx)
	}
	return nil
}

// In-place heapsort bounds comparisons, stack and cancellation without a
// second seed buffer or a comparator panic. Input is at most 65536 indices.
func vectorConfidenceSortSeeds(ctx context.Context, indices []uint32) error {
	if err := regionGrowContextError(ctx); err != nil {
		return err
	}
	visits := 0
	poll := func() error {
		err := vectorConfidenceBulkPoll(ctx, visits)
		visits++
		return err
	}
	down := func(root, end int) error {
		for child := 2*root + 1; child < end; child = 2*root + 1 {
			if err := poll(); err != nil {
				return err
			}
			if child+1 < end && indices[child] < indices[child+1] {
				child++
			}
			if indices[root] >= indices[child] {
				break
			}
			indices[root], indices[child] = indices[child], indices[root]
			root = child
		}
		return nil
	}
	for root := len(indices)/2 - 1; root >= 0; root-- {
		if err := poll(); err != nil {
			return err
		}
		if err := down(root, len(indices)); err != nil {
			return err
		}
	}
	for end := len(indices) - 1; end > 0; end-- {
		if err := poll(); err != nil {
			return err
		}
		indices[0], indices[end] = indices[end], indices[0]
		if err := down(0, end); err != nil {
			return err
		}
	}
	return regionGrowContextError(ctx)
}

func (g *vectorConfidenceGrower) visit(index uint32) error {
	if err := g.charge(1); err != nil {
		return err
	}
	if g.state[index] != 0 {
		return nil
	}
	g.state[index] = 1
	if err := g.read(index, g.model.values); err != nil {
		return err
	}
	d, err := g.model.distance(g, g.model.values)
	if err != nil || d > g.effective {
		return err
	}
	if g.count == g.regionCap {
		return fmt.Errorf("%w: vector region voxel limit", ErrRegionGrowResourceLimit)
	}
	g.state[index] = 2
	g.queue[g.count] = index
	g.count++
	return nil
}

func (g *vectorConfidenceGrower) masks() (map[int]*RasterMask, error) {
	if err := g.charge(uint64(2*g.n + g.volume.Grid.Rows*g.volume.Grid.Slices)); err != nil {
		return nil, err
	}
	grid := g.volume.Grid
	runs, occupied := 0, 0
	for z := 0; z < grid.Slices; z++ {
		hasSlice := false
		for y := 0; y < grid.Rows; y++ {
			inside := false
			for x := 0; x < grid.Columns; x++ {
				index := (z*grid.Rows+y)*grid.Columns + x
				if index%1024 == 0 {
					if err := regionGrowContextError(g.ctx); err != nil {
						return nil, err
					}
				}
				on := g.state[index] == 2
				if on && !inside {
					runs++
					hasSlice = true
				}
				inside = on
			}
		}
		if hasSlice {
			occupied++
		}
	}
	if err := g.charge(uint64(runs + occupied*grid.Rows)); err != nil {
		return nil, err
	}
	buffer := make([]MaskRun, runs)
	if err := regionGrowContextError(g.ctx); err != nil {
		return nil, err
	}
	out := make(map[int]*RasterMask, occupied)
	position := 0
	visits := 0
	for z := 0; z < grid.Slices; z++ {
		var mask *RasterMask
		for y := 0; y < grid.Rows; y++ {
			rowStart, runStart := position, -1
			for x := 0; x <= grid.Columns; x++ {
				index := (z*grid.Rows+y)*grid.Columns + x
				if err := vectorConfidenceBulkPoll(g.ctx, visits); err != nil {
					return nil, err
				}
				visits++
				on := x < grid.Columns && g.state[index] == 2
				if on && runStart < 0 {
					runStart = x
				}
				if !on && runStart >= 0 {
					buffer[position] = MaskRun{Start: runStart, End: x}
					position++
					runStart = -1
				}
			}
			if position > rowStart {
				if mask == nil {
					if err := regionGrowContextError(g.ctx); err != nil {
						return nil, err
					}
					mask = NewRasterMask(grid.Columns, grid.Rows)
					out[z] = mask
				}
				mask.runs[y] = buffer[rowStart:position:position]
			}
		}
	}
	return out, regionGrowContextError(g.ctx)
}
