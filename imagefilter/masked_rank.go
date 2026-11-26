package imagefilter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// ErrRankResourceLimit reports a finite masked-rank quota exceeded before work.
var ErrRankResourceLimit = errors.New("imagefilter: masked rank resource limit")

// RankNeighborhood selects a center-distance physical kernel.
type RankNeighborhood uint8

const (
	PhysicalBox RankNeighborhood = iota
	PhysicalEllipsoid
)

// RankBoundary defines how offsets outside the complete source are sampled.
type RankBoundary uint8

const (
	ClipBoundary RankBoundary = iota
	ReplicateBoundary
)

// RankCenters defines whether the output center itself must be eligible.
type RankCenters uint8

const (
	RequireValidCenter RankCenters = iota
	AnyValidNeighborhood
)

// RankNonFinite defines how mask-eligible NaN/Inf samples are handled.
type RankNonFinite uint8

const (
	RejectNonFinite RankNonFinite = iota
	ExcludeNonFinite
)

// RankRegion is a nonempty half-open output rectangle in source coordinates.
type RankRegion struct{ Origin, Size [3]int }

// RankLimits bounds a request, including the full source of a regional call.
// Zero selects the defaults: 16Mi voxels, 65536 bounding/kernel samples,
// 256MiB logical working storage, and 1Gi conservative sample visits.
// Ceilings are respectively 16Mi, 65536, 1GiB and 1Ti. Negative or larger
// options are invalid. Working storage is a capacity ledger, not process RSS.
type RankLimits struct {
	MaxVoxels        int64
	MaxKernelSamples int64
	MaxWorkingBytes  int64
	MaxSampleVisits  int64
}

// MaskedRankOptions configures an exact order statistic of eligible samples.
// Rank must be finite in [0,1]. Index floor(Rank*(n-1)) selects the minimum
// at 0 and lower median at .5 for an even count. Zero is an explicit minimum.
// RadiusMM uses exact binary64 center-distance box/ellipsoid membership, not
// Median's rounded radius. Active axes require positive finite spacing.
// EmptyValue is finite and accompanies Valid=false, never invented validity.
// Workers=0 captures the shared default, capped at 64 and available output rows.
// Source, mask and options must remain stable throughout a call.
type MaskedRankOptions struct {
	Rank         float64
	RadiusMM     [3]float64
	Neighborhood RankNeighborhood
	Boundary     RankBoundary
	Centers      RankCenters
	NonFinite    RankNonFinite
	EmptyValue   float32
	Workers      int
	Limits       RankLimits
}

// DefaultMaskedRankOptions selects rank .5, PhysicalBox, ClipBoundary,
// RequireValidCenter, RejectNonFinite and zero empty fill/default quotas.
func DefaultMaskedRankOptions() MaskedRankOptions { return MaskedRankOptions{Rank: .5} }

// MaskedRankResult owns values and validity for Region. Values has cropped
// dimensions; its physical origin and directions stay with the caller, who
// must apply Region.Origin. Invalid values contain EmptyValue. Signed zero is
// canonicalized to +0. WorkingBytes is the conservative admitted capacity;
// WorkerLimit is a captured upper bound, not the helpers actually reserved.
type MaskedRankResult struct {
	Values       *Volume
	Valid        []bool
	Region       RankRegion
	WorkingBytes int64
	WorkerLimit  int
}

// MaskedRank filters the entire materialized source. A nil validity mask makes
// every source sample mask-eligible. Masked-out nonfinite samples are ignored.
// A nil context means Background. Errors/cancellation return nil after joining
// workers; the borrowed source/mask are never modified or cloned wholesale.
func MaskedRank(ctx context.Context, src *Volume, valid []bool, opts MaskedRankOptions) (*MaskedRankResult, error) {
	region := RankRegion{}
	if src != nil {
		region.Size = src.Dims
	}
	return maskedRank(ctx, src, valid, region, opts, rankPathAuto)
}

// MaskedRankRegion reads neighborhoods from the full source and publishes only
// Region. Borders are source borders, not region borders. Disjoint regions,
// worker bounds and the exact histogram/selection paths give identical bits.
// This API is materialized, not an out-of-core or transactional writer.
func MaskedRankRegion(ctx context.Context, src *Volume, valid []bool, region RankRegion, opts MaskedRankOptions) (*MaskedRankResult, error) {
	return maskedRank(ctx, src, valid, region, opts, rankPathAuto)
}

type rankPath uint8

const (
	rankPathAuto rankPath = iota
	rankPathSelect
	rankPathHistogram
)

type rankRequest struct {
	src                             *Volume
	valid                           []bool
	opts                            MaskedRankOptions
	region                          RankRegion
	radius                          [3]int
	samples, rows, outputs, workers int
	working                         int64
	lo                              float32
	span                            int
	histogram                       bool
	kernel                          []rankSpan
}

type rankPoll struct {
	ctx context.Context
	n   uint32
}

func (p *rankPoll) step() error {
	p.n++
	if p.n&4095 == 0 {
		return p.ctx.Err()
	}
	return nil
}
func (p *rankPoll) check() error { p.n = 0; return p.ctx.Err() }
func rankFinite(v float32) bool  { return v >= -math.MaxFloat32 && v <= math.MaxFloat32 }
func rankZero(v float32) float32 {
	if v == 0 {
		return 0
	}
	return v
}
func rankIndex(q float64, n int) int {
	if q == 1 {
		return n - 1
	}
	return int(math.Floor(q * float64(n-1)))
}

func rankNormalizeLimits(l RankLimits) (RankLimits, error) {
	vals := []struct {
		p        *int64
		def, cap int64
	}{{&l.MaxVoxels, 1 << 24, 1 << 24}, {&l.MaxKernelSamples, 1 << 16, 1 << 16}, {&l.MaxWorkingBytes, 256 << 20, 1 << 30}, {&l.MaxSampleVisits, 1 << 30, 1 << 40}}
	for _, v := range vals {
		if *v.p < 0 || *v.p > v.cap {
			return l, fmt.Errorf("%w: invalid rank quota", ErrInvalidVolume)
		}
		if *v.p == 0 {
			*v.p = v.def
		}
	}
	return l, nil
}

// Admission reads only headers/lengths before all dimension/kernel/visit/byte
// quotas. It does not call Volume.Validate, which rejects masked-out NaN.
func rankAdmit(ctx context.Context, src *Volume, valid []bool, region RankRegion, opts MaskedRankOptions) (*rankRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if src == nil {
		return nil, fmt.Errorf("%w: nil rank source", ErrInvalidVolume)
	}
	if opts.Neighborhood > PhysicalEllipsoid || opts.Boundary > ReplicateBoundary || opts.Centers > AnyValidNeighborhood || opts.NonFinite > ExcludeNonFinite || !(opts.Rank >= 0 && opts.Rank <= 1) || !rankFinite(opts.EmptyValue) || opts.Workers < 0 || opts.Workers > 64 {
		return nil, fmt.Errorf("%w: rank options", ErrInvalidVolume)
	}
	limits, err := rankNormalizeLimits(opts.Limits)
	if err != nil {
		return nil, err
	}
	opts.Limits = limits
	n, ok := voxelCount(src.Dims)
	if !ok || len(src.Data) != n || (valid != nil && len(valid) != n) {
		return nil, fmt.Errorf("%w: rank dimensions/data/mask length", ErrInvalidVolume)
	}
	if int64(n) > limits.MaxVoxels {
		return nil, fmt.Errorf("%w: rank source voxels", ErrRankResourceLimit)
	}
	for a, d := range src.Dims {
		if d > 1 && !(src.SpacingMM[a] > 0 && src.SpacingMM[a] <= math.MaxFloat64) {
			return nil, fmt.Errorf("%w: rank spacing axis %d", ErrInvalidVolume, a)
		}
		if !(opts.RadiusMM[a] >= 0 && opts.RadiusMM[a] <= math.MaxFloat64) {
			return nil, fmt.Errorf("%w: rank radius axis %d", ErrInvalidVolume, a)
		}
		if region.Origin[a] < 0 || region.Size[a] <= 0 || region.Origin[a] > d || region.Size[a] > d-region.Origin[a] {
			return nil, fmt.Errorf("%w: rank region", ErrInvalidVolume)
		}
	}
	m, ok := voxelCount(region.Size)
	if !ok {
		return nil, fmt.Errorf("%w: rank output dimensions", ErrInvalidVolume)
	}
	r := &rankRequest{src: src, valid: valid, opts: opts, region: region, outputs: m}
	bounding := int64(1)
	for a, d := range src.Dims {
		if d > 1 {
			r.radius[a], err = r.axisRadius(a)
			if err != nil {
				return nil, err
			}
		}
		width := 2*int64(r.radius[a]) + 1
		if bounding > limits.MaxKernelSamples/width {
			return nil, fmt.Errorf("%w: rank bounding kernel", ErrRankResourceLimit)
		}
		bounding *= width
	}
	r.rows, r.samples, err = rankCountKernel(ctx, r)
	if err != nil {
		return nil, err
	}
	// Both scanning selection and admitted sliding histogram access at most K
	// samples per output on average. Histogram is selected only when K>=2R.
	if int64(m) > (limits.MaxSampleVisits-int64(n)-bounding)/int64(r.samples) {
		return nil, fmt.Errorf("%w: rank sample visits", ErrRankResourceLimit)
	}
	r.workers = opts.Workers
	if r.workers == 0 {
		r.workers = min(63, parallel.Budget()) + 1
	}
	r.workers = max(1, min(r.workers, region.Size[1]*region.Size[2]))
	// All capacities retained at once: 5M output; 12R spans; W*(16R
	// row cache + max(4K selection, 4*(65536+256) histogram) +256 headers),
	// plus 4096 for request/output/closures/cancellation/scheduler headers.
	r.working = 5*int64(m) + 12*int64(r.rows) + int64(r.workers)*(16*int64(r.rows)+max(4*int64(r.samples), 4*int64(medianHistogramBins+256))+256) + 4096
	if r.working > limits.MaxWorkingBytes {
		return nil, fmt.Errorf("%w: rank working bytes (%d)", ErrRankResourceLimit, r.working)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r, nil
}

// The exact positive axial predicate is monotone. Division-derived integer
// bounds can omit a rounded multiplication endpoint (43*.1 == 4.3 while
// 4.3/.1 < 43), so search the predicate itself within the finite quota.
// Limits are normalized to 1..65536 before this conversion; each axis uses at
// most 17 header-only probes and no payload read or scratch allocation.
func (r *rankRequest) axisRadius(axis int) (int, error) {
	maximum := int((r.opts.Limits.MaxKernelSamples - 1) / 2)
	offsets := [3]int{}
	offsets[axis] = maximum + 1
	if r.inside(offsets[0], offsets[1], offsets[2]) {
		return 0, fmt.Errorf("%w: rank axial kernel axis %d", ErrRankResourceLimit, axis)
	}
	lo, hi := 0, maximum
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		offsets[axis] = mid
		if r.inside(offsets[0], offsets[1], offsets[2]) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo, nil
}

func (r *rankRequest) eligible(i int) bool {
	return (r.valid == nil || r.valid[i]) && rankFinite(r.src.Data[i])
}
func (r *rankRequest) scan(ctx context.Context) error {
	p := rankPoll{ctx: ctx}
	found, integral := false, true
	var hi float32
	for i, v := range r.src.Data {
		if err := p.step(); err != nil {
			return err
		}
		if r.valid != nil && !r.valid[i] {
			continue
		}
		if !rankFinite(v) {
			if r.opts.NonFinite == RejectNonFinite {
				return fmt.Errorf("%w: nonfinite rank sample %d", ErrInvalidVolume, i)
			}
			continue
		}
		if !found {
			r.lo, hi, found = v, v, true
		} else {
			r.lo = min(r.lo, v)
			hi = max(hi, v)
		}
		if v != float32(math.Trunc(float64(v))) || v >= 1<<24 || v <= -(1<<24) {
			integral = false
		}
	}
	if found && integral {
		r.span = int(hi-r.lo) + 1
		r.histogram = r.span <= medianHistogramBins && r.samples >= 2*r.rows
	}
	return p.check()
}

func maskedRank(ctx context.Context, src *Volume, valid []bool, region RankRegion, opts MaskedRankOptions, path rankPath) (*MaskedRankResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	r, err := rankAdmit(ctx, src, valid, region, opts)
	if err != nil {
		return nil, err
	}
	if err = r.scan(ctx); err != nil {
		return nil, err
	}
	if path == rankPathSelect {
		r.histogram = false
	}
	if path == rankPathHistogram && !r.histogram {
		return nil, fmt.Errorf("%w: test histogram path ineligible", ErrInvalidVolume)
	}
	if err = r.makeKernel(ctx); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	out := &MaskedRankResult{Values: &Volume{Dims: region.Size, SpacingMM: src.SpacingMM, Data: make([]float32, r.outputs)}, Valid: make([]bool, r.outputs), Region: region, WorkingBytes: r.working, WorkerLimit: r.workers}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var once sync.Once
	var failure error
	fail := func(err error) {
		if err != nil {
			once.Do(func() { failure = err; cancel() })
		}
	}
	err = parallel.Run(workCtx, region.Size[1]*region.Size[2], r.workers, 1, func() func(int) {
		s := rankScratch{rows: make([]rankRow, r.rows), poll: rankPoll{ctx: workCtx}}
		if r.histogram {
			s.hist = newMedianHistogram(r.span)
		} else {
			s.values = make([]float32, r.samples)
		}
		return func(line int) {
			if e := s.poll.check(); e != nil {
				fail(e)
				return
			}
			fail(r.line(&s, out, line))
		}
	})
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if failure != nil {
		return nil, failure
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

type rankRow struct{ base, half int }
type rankScratch struct {
	rows   []rankRow
	values []float32
	hist   *medianHistogram
	poll   rankPoll
}

func (r *rankRequest) line(s *rankScratch, out *MaskedRankResult, line int) error {
	y := r.region.Origin[1] + line%r.region.Size[1]
	z := r.region.Origin[2] + line/r.region.Size[1]
	for i, k := range r.kernel {
		if err := s.poll.step(); err != nil {
			return err
		}
		cy, cz := y+int(k.dy), z+int(k.dz)
		if r.opts.Boundary == ClipBoundary && (cy < 0 || cy >= r.src.Dims[1] || cz < 0 || cz >= r.src.Dims[2]) {
			s.rows[i] = rankRow{base: -1}
			continue
		}
		cy, cz = clampAxis(cy, r.src.Dims[1]), clampAxis(cz, r.src.Dims[2])
		s.rows[i] = rankRow{base: (cz*r.src.Dims[1] + cy) * r.src.Dims[0], half: int(k.half)}
	}
	if r.histogram {
		return r.histogramLine(s, out, line, y, z)
	}
	for x := 0; x < r.region.Size[0]; x++ {
		if err := s.poll.step(); err != nil {
			return err
		}
		gx := r.region.Origin[0] + x
		i := line*r.region.Size[0] + x
		if r.opts.Centers == RequireValidCenter && !r.eligible((z*r.src.Dims[1]+y)*r.src.Dims[0]+gx) {
			out.Values.Data[i] = rankZero(r.opts.EmptyValue)
			continue
		}
		n := 0
		for _, row := range s.rows {
			if row.base < 0 {
				continue
			}
			for dx := -row.half; dx <= row.half; dx++ {
				if err := s.poll.step(); err != nil {
					return err
				}
				cx := gx + dx
				if r.opts.Boundary == ClipBoundary && (cx < 0 || cx >= r.src.Dims[0]) {
					continue
				}
				j := row.base + clampAxis(cx, r.src.Dims[0])
				if r.eligible(j) {
					s.values[n] = rankZero(r.src.Data[j])
					n++
				}
			}
		}
		if n == 0 {
			out.Values.Data[i] = rankZero(r.opts.EmptyValue)
			continue
		}
		v, err := rankSelect(s.values[:n], rankIndex(r.opts.Rank, n), &s.poll)
		if err != nil {
			return err
		}
		out.Values.Data[i], out.Valid[i] = rankZero(v), true
	}
	return s.poll.check()
}
