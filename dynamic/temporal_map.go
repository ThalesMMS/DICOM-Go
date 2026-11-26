package dynamic

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// TemporalGrid declares an already corresponding regular physical grid. Values
// are X-fastest, then Y, then Z. Direction is row-major and its columns are the
// orthonormal directions of the three grid axes. Distances are in millimetres.
// Every nonmissing frame must have exactly the same grid, including its explicit
// FrameOfReferenceUID. This API neither registers nor resamples frames.
type TemporalGrid struct {
	Size                [3]int
	Origin              [3]float64
	Spacing             [3]float64
	Direction           [9]float64
	FrameOfReferenceUID string
}

// TemporalFrameInfo describes one whole scalar grid at one temporal position.
// Missing frames need no grid, units, timing or pixels. Units are an explicit
// caller-defined identifier (for example "HU" or "UCUM:mm2/s"); identifiers must
// match exactly. Duration weights use declared positive Duration only, never an
// inferred gap between offsets or a cine display interval. SourceID and
// Registration describe the source and any registration applied by the caller.
// Text uses byte limits below; accepted retained text is copied and charged to
// MaxWorkingBytes so a substring cannot retain an unbounded source allocation.
type TemporalFrameInfo struct {
	Grid         TemporalGrid
	Units        string
	Missing      bool
	Offset       time.Duration
	HasOffset    bool
	Duration     time.Duration
	HasDuration  bool
	SourceID     string
	Registration string
}

// Metadata text limits bound each accepted frame before any pixel read. The
// SourceID limit permits explicit references to a bounded stack of many source
// planes; the total retained text must also fit TemporalLimits.MaxWorkingBytes.
const (
	MaxTemporalUnitBytes                = 128
	MaxTemporalFrameOfReferenceUIDBytes = 64
	MaxTemporalSourceIDBytes            = 4 << 20
	MaxTemporalRegistrationBytes        = 64 << 10
)

// TemporalSource supplies immutable scalar frames in temporal order. ReadFrame
// must fill the entire caller-owned buffer, observe ctx, and neither retain nor
// alias that buffer after returning. Metadata and count must stay unchanged for
// the operation. The implementation reads each accepted frame once, sequentially.
// Memory retained by the source is outside the algorithm's working-byte budget.
type TemporalSource interface {
	FrameCount() int
	FrameInfo(context.Context, int) (TemporalFrameInfo, error)
	ReadFrame(context.Context, int, []float64) error
}

type TemporalStatistic uint8

const (
	TemporalMean TemporalStatistic = iota
	TemporalMinimum
	TemporalMaximum
	TemporalStandardDeviation
)

func (value TemporalStatistic) String() string {
	return temporalEnumName(uint8(value), []string{"mean", "minimum", "maximum", "standard-deviation"})
}

type TemporalWeighting uint8

const (
	SampleWeighting TemporalWeighting = iota
	DurationWeighting
)

func (value TemporalWeighting) String() string {
	return temporalEnumName(uint8(value), []string{"sample", "duration"})
}

type TemporalVariance uint8

const (
	PopulationVariance TemporalVariance = iota
	// SampleVariance uses sum(w)-sum(w*w)/sum(w), the reliability-weighted
	// unbiased sample denominator. With equal weights this is n-1. Durations
	// are reliability weights here, not counts of independent observations.
	SampleVariance
)

func (value TemporalVariance) String() string {
	return temporalEnumName(uint8(value), []string{"population", "sample"})
}

type MissingFramePolicy uint8

const (
	RejectMissing MissingFramePolicy = iota
	SkipMissing
)

func (value MissingFramePolicy) String() string {
	return temporalEnumName(uint8(value), []string{"reject-missing", "skip-missing"})
}

type NonFinitePolicy uint8

const (
	RejectNonFinite NonFinitePolicy = iota
	// SkipNonFinite omits each nonfinite voxel observation independently.
	// Counts and weight sums describe the observations actually used per voxel.
	SkipNonFinite
)

func (value NonFinitePolicy) String() string {
	return temporalEnumName(uint8(value), []string{"reject-nonfinite", "skip-nonfinite"})
}

// TemporalLimits are mandatory finite admission limits. MaxWorkingBytes covers
// owned array payloads and copied retained metadata text: output, accumulators,
// one read buffer, provenance arrays, common units/FrameOfReferenceUID and each
// accepted source/registration description. It excludes Go object/slice headers,
// source decoding/caches and runtime/allocator overhead. No F*V array is owned.
type TemporalLimits struct {
	MaxFrames       int
	MaxVoxels       int
	MaxWorkingBytes uint64
}

func DefaultTemporalLimits() TemporalLimits {
	return TemporalLimits{MaxFrames: 10_000, MaxVoxels: 16_777_216, MaxWorkingBytes: 1 << 30}
}

type TemporalMapOptions struct {
	Statistic TemporalStatistic
	Weighting TemporalWeighting
	Variance  TemporalVariance
	Missing   MissingFramePolicy
	NonFinite NonFinitePolicy
	Limits    TemporalLimits
}

func DefaultTemporalMapOptions() TemporalMapOptions {
	return TemporalMapOptions{Limits: DefaultTemporalLimits()}
}

type TemporalWindowAlignment uint8

const (
	CausalWindow TemporalWindowAlignment = iota
	CenteredWindow
)

func (value TemporalWindowAlignment) String() string {
	return temporalEnumName(uint8(value), []string{"causal", "centered"})
}

type TemporalWindowBorder uint8

const (
	// ClipWindow truncates the requested window at acquisition boundaries and
	// renormalizes the remaining accepted observations, without zero padding.
	ClipWindow TemporalWindowBorder = iota
	// RequireFullWindow refuses targets whose declared window crosses a boundary.
	RequireFullWindow
)

func (value TemporalWindowBorder) String() string {
	return temporalEnumName(uint8(value), []string{"clip", "require-full"})
}

func temporalEnumName(value uint8, names []string) string {
	if int(value) >= len(names) {
		return fmt.Sprintf("unknown(%d)", value)
	}
	return names[value]
}

// MovingMeanOptions declares a window in temporal positions, including missing
// positions. Causal windows end at the target; centered windows require an odd
// count and equal past/future extents. Duration weighting changes observation
// weights, not membership. Skipped missing positions are never replaced.
type MovingMeanOptions struct {
	Window    int
	Alignment TemporalWindowAlignment
	Border    TemporalWindowBorder
	Weighting TemporalWeighting
	Missing   MissingFramePolicy
	NonFinite NonFinitePolicy
	Limits    TemporalLimits
}

func DefaultMovingMeanOptions() MovingMeanOptions {
	return MovingMeanOptions{Window: 3, Limits: DefaultTemporalLimits()}
}

// TemporalMapProvenance preserves the effective frame selection and all
// requested policies. Weights are 1 for sample weighting and seconds for duration
// weighting. Timing remains explicit with Has flags. MissingIndices contains
// omitted positions, while FrameIndices lists only accepted frames.
type TemporalMapProvenance struct {
	Options        TemporalMapOptions
	Moving         bool
	Window         int
	Alignment      TemporalWindowAlignment
	Border         TemporalWindowBorder
	Target         int
	Start          int
	End            int // exclusive, after clipping
	FrameIndices   []int
	MissingIndices []int
	Weights        []float64
	Offsets        []time.Duration
	HasOffsets     []bool
	Durations      []time.Duration
	HasDurations   []bool
	Sources        []string
	Registrations  []string
}

// TemporalMap owns derived values without modifying the source. Invalid voxels
// have Valid=false and a NaN value (no accepted observations, or fewer than two
// for sample standard deviation). No partially calculated map is ever returned.
type TemporalMap struct {
	Values           []float64
	Valid            []bool
	Counts           []uint32
	WeightSums       []float64
	Grid             TemporalGrid
	Units            string
	Provenance       TemporalMapProvenance
	PeakWorkingBytes uint64
}

var (
	ErrTemporalAdmission = errors.New("dicom/dynamic: temporal map admission")
	ErrTemporalNoSamples = errors.New("dicom/dynamic: no temporal samples")
	ErrTemporalWindow    = errors.New("dicom/dynamic: incomplete temporal window")
	ErrTemporalData      = errors.New("dicom/dynamic: invalid temporal data")
)

// ComputeTemporalMap incrementally computes one statistic over the acquisition.
// Welford's weighted recurrence avoids the cancellation of E[x*x]-E[x]^2.
// Finite input whose recurrence exceeds float64 range is explicitly rejected.
func ComputeTemporalMap(ctx context.Context, source TemporalSource, options TemporalMapOptions) (*TemporalMap, error) {
	count, err := temporalAdmission(ctx, source, options)
	if err != nil {
		return nil, err
	}
	return computeTemporalMap(ctx, source, options, TemporalMapProvenance{Options: options, Target: -1, End: count})
}

// MovingTemporalMean returns a detached map for one target. Processing another
// target is an explicit request; the API never materializes/caches the full 4D
// acquisition or publishes earlier targets before a cancelled operation ends.
func MovingTemporalMean(ctx context.Context, source TemporalSource, target int, options MovingMeanOptions) (*TemporalMap, error) {
	mapOptions := TemporalMapOptions{Weighting: options.Weighting, Missing: options.Missing, NonFinite: options.NonFinite, Limits: options.Limits}
	count, err := temporalAdmission(ctx, source, mapOptions)
	if err != nil {
		return nil, err
	}
	if target < 0 || target >= count || options.Window <= 0 || options.Window > options.Limits.MaxFrames || options.Alignment > CenteredWindow || options.Border > RequireFullWindow {
		return nil, fmt.Errorf("%w: invalid target or window options", ErrTemporalAdmission)
	}
	left, right := options.Window-1, 0
	if options.Alignment == CenteredWindow {
		if options.Window%2 == 0 {
			return nil, fmt.Errorf("%w: centered window must be odd", ErrTemporalAdmission)
		}
		left, right = options.Window/2, options.Window/2
	}
	crossesStart, crossesEnd := left > target, right >= count-target
	if options.Border == RequireFullWindow && (crossesStart || crossesEnd) {
		return nil, ErrTemporalWindow
	}
	start, end := 0, count
	if !crossesStart {
		start = target - left
	}
	if !crossesEnd {
		end = target + right + 1
	}
	provenance := TemporalMapProvenance{Options: mapOptions, Moving: true, Window: options.Window, Alignment: options.Alignment, Border: options.Border, Target: target, Start: start, End: end}
	return computeTemporalMap(ctx, source, mapOptions, provenance)
}

func temporalAdmission(ctx context.Context, source TemporalSource, options TemporalMapOptions) (int, error) {
	if ctx == nil || source == nil {
		return 0, fmt.Errorf("%w: context and source are required", ErrTemporalAdmission)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	limits := options.Limits
	if limits.MaxFrames <= 0 || uint64(limits.MaxFrames) > math.MaxUint32 || limits.MaxVoxels <= 0 || limits.MaxWorkingBytes == 0 || options.Statistic > TemporalStandardDeviation || options.Weighting > DurationWeighting || options.Variance > SampleVariance || options.Missing > SkipMissing || options.NonFinite > SkipNonFinite {
		return 0, fmt.Errorf("%w: invalid policies or finite limits", ErrTemporalAdmission)
	}
	count := source.FrameCount()
	if count <= 0 || count > limits.MaxFrames {
		return 0, fmt.Errorf("%w: frame count %d exceeds limit %d", ErrTemporalAdmission, count, limits.MaxFrames)
	}
	return count, nil
}

func computeTemporalMap(ctx context.Context, source TemporalSource, options TemporalMapOptions, provenance TemporalMapProvenance) (*TemporalMap, error) {
	positions := provenance.End - provenance.Start
	// Reserve all provenance arrays, including missing positions, before reading
	// metadata. This upper bound is retained in PeakWorkingBytes even when frames
	// are skipped, so it cannot understate the allocated capacities.
	metadataBytes := uint64(positions) * (2*uint64(strconv.IntSize/8) + 3*8 + 2 + 2*uint64(2*strconv.IntSize/8))
	if metadataBytes > options.Limits.MaxWorkingBytes {
		return nil, fmt.Errorf("%w: provenance needs %d bytes", ErrTemporalAdmission, metadataBytes)
	}
	provenance.FrameIndices = make([]int, 0, positions)
	provenance.MissingIndices = make([]int, 0, positions)
	provenance.Weights = make([]float64, 0, positions)
	provenance.Offsets = make([]time.Duration, 0, positions)
	provenance.HasOffsets = make([]bool, 0, positions)
	provenance.Durations = make([]time.Duration, 0, positions)
	provenance.HasDurations = make([]bool, 0, positions)
	provenance.Sources = make([]string, 0, positions)
	provenance.Registrations = make([]string, 0, positions)
	var grid TemporalGrid
	var units string
	voxels := 0
	for frame := provenance.Start; frame < provenance.End; frame++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := source.FrameInfo(ctx, frame)
		if err != nil {
			return nil, fmt.Errorf("frame %d metadata: %w", frame, err)
		}
		if info.Missing {
			if options.Missing == RejectMissing {
				return nil, fmt.Errorf("%w: frame %d is missing", ErrTemporalData, frame)
			}
			provenance.MissingIndices = append(provenance.MissingIndices, frame)
			continue
		}
		if len(info.Units) > MaxTemporalUnitBytes || len(info.Grid.FrameOfReferenceUID) > MaxTemporalFrameOfReferenceUIDBytes || len(info.SourceID) > MaxTemporalSourceIDBytes || len(info.Registration) > MaxTemporalRegistrationBytes {
			return nil, fmt.Errorf("%w: frame %d metadata text exceeds its field limit", ErrTemporalAdmission, frame)
		}
		firstAccepted := voxels == 0
		if voxels == 0 {
			voxels, err = temporalGridVoxels(info.Grid, options.Limits.MaxVoxels)
			if err != nil {
				return nil, err
			}
			grid, units = info.Grid, info.Units
			if strings.TrimSpace(units) == "" {
				return nil, fmt.Errorf("%w: frame %d has no explicit units", ErrTemporalData, frame)
			}
		} else if info.Grid != grid || info.Units != units {
			return nil, fmt.Errorf("%w: frame %d grid or units diverge", ErrTemporalData, frame)
		}
		if info.HasOffset && info.Offset < 0 {
			return nil, fmt.Errorf("%w: frame %d has a negative offset", ErrTemporalData, frame)
		}
		if info.HasDuration && info.Duration <= 0 {
			return nil, fmt.Errorf("%w: frame %d has a nonpositive duration", ErrTemporalData, frame)
		}
		weight := 1.0
		if options.Weighting == DurationWeighting {
			if !info.HasDuration || info.Duration <= 0 {
				return nil, fmt.Errorf("%w: frame %d needs a declared positive duration", ErrTemporalData, frame)
			}
			weight = info.Duration.Seconds()
		}
		textBytes := uint64(len(info.SourceID)) + uint64(len(info.Registration))
		if firstAccepted {
			textBytes += uint64(len(info.Units)) + uint64(len(info.Grid.FrameOfReferenceUID))
		}
		if textBytes > options.Limits.MaxWorkingBytes-metadataBytes {
			return nil, fmt.Errorf("%w: frame %d retained metadata exceeds the working-byte limit", ErrTemporalAdmission, frame)
		}
		metadataBytes += textBytes
		if firstAccepted {
			grid.FrameOfReferenceUID = strings.Clone(grid.FrameOfReferenceUID)
			units = strings.Clone(units)
		}
		provenance.FrameIndices = append(provenance.FrameIndices, frame)
		provenance.Weights = append(provenance.Weights, weight)
		provenance.Offsets = append(provenance.Offsets, info.Offset)
		provenance.HasOffsets = append(provenance.HasOffsets, info.HasOffset)
		provenance.Durations = append(provenance.Durations, info.Duration)
		provenance.HasDurations = append(provenance.HasDurations, info.HasDuration)
		provenance.Sources = append(provenance.Sources, strings.Clone(info.SourceID))
		provenance.Registrations = append(provenance.Registrations, strings.Clone(info.Registration))
	}
	if voxels == 0 {
		return nil, ErrTemporalNoSamples
	}
	bytesPerVoxel := uint64(8 + 1 + 4 + 8 + 8) // result arrays and one read buffer
	if options.Statistic == TemporalStandardDeviation {
		bytesPerVoxel += 8 // M2
		if options.Variance == SampleVariance {
			bytesPerVoxel += 8 // stable sample denominator
		}
	}
	if uint64(voxels) > (options.Limits.MaxWorkingBytes-metadataBytes)/bytesPerVoxel {
		return nil, fmt.Errorf("%w: working arrays exceed %d bytes", ErrTemporalAdmission, options.Limits.MaxWorkingBytes)
	}
	workingBytes := metadataBytes + uint64(voxels)*bytesPerVoxel
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := &TemporalMap{Values: make([]float64, voxels), Valid: make([]bool, voxels), Counts: make([]uint32, voxels), WeightSums: make([]float64, voxels), Grid: grid, Units: units, Provenance: provenance, PeakWorkingBytes: workingBytes}
	buffer := make([]float64, voxels)
	var m2, sampleDenominators []float64
	if options.Statistic == TemporalStandardDeviation {
		m2 = make([]float64, voxels)
		if options.Variance == SampleVariance {
			sampleDenominators = make([]float64, voxels)
		}
	}
	for position, frame := range provenance.FrameIndices {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := source.ReadFrame(ctx, frame, buffer); err != nil {
			return nil, fmt.Errorf("frame %d pixels: %w", frame, err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		weight := provenance.Weights[position]
		for voxel, value := range buffer {
			if voxel%4096 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if !finite(value) {
				if options.NonFinite == RejectNonFinite {
					return nil, fmt.Errorf("%w: frame %d voxel %d is nonfinite", ErrTemporalData, frame, voxel)
				}
				continue
			}
			oldWeight := result.WeightSums[voxel]
			newWeight := oldWeight + weight
			if result.Counts[voxel] == 0 {
				result.Values[voxel] = value
			} else {
				switch options.Statistic {
				case TemporalMinimum:
					result.Values[voxel] = min(result.Values[voxel], value)
				case TemporalMaximum:
					result.Values[voxel] = max(result.Values[voxel], value)
				default:
					delta := value - result.Values[voxel]
					mean := result.Values[voxel] + delta*(weight/newWeight)
					if !finite(delta) || !finite(mean) {
						return nil, fmt.Errorf("%w: frame %d voxel %d recurrence overflow", ErrTemporalData, frame, voxel)
					}
					if m2 != nil {
						m2[voxel] += delta * (delta * (oldWeight * (weight / newWeight)))
						if !finite(m2[voxel]) {
							return nil, fmt.Errorf("%w: frame %d voxel %d variance overflow", ErrTemporalData, frame, voxel)
						}
					}
					result.Values[voxel] = mean
				}
			}
			result.Counts[voxel]++
			result.WeightSums[voxel] = newWeight
			if sampleDenominators != nil {
				// Algebraically W-sum(w*w)/W, without cancellation when the
				// weights span very different durations.
				sampleDenominators[voxel] = (oldWeight / newWeight) * (sampleDenominators[voxel] + 2*weight)
			}
		}
	}
	for voxel := range result.Values {
		if voxel%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		result.Valid[voxel] = result.Counts[voxel] > 0
		if m2 != nil && result.Valid[voxel] {
			denominator := result.WeightSums[voxel]
			if sampleDenominators != nil {
				denominator = sampleDenominators[voxel]
			}
			if denominator <= 0 || (sampleDenominators != nil && result.Counts[voxel] < 2) {
				result.Valid[voxel] = false
			} else {
				result.Values[voxel] = math.Sqrt(max(m2[voxel], 0) / denominator)
				if !finite(result.Values[voxel]) {
					return nil, fmt.Errorf("%w: voxel %d standard deviation overflow", ErrTemporalData, voxel)
				}
			}
		}
		if !result.Valid[voxel] {
			result.Values[voxel] = math.NaN()
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func temporalGridVoxels(grid TemporalGrid, limit int) (int, error) {
	if strings.TrimSpace(grid.FrameOfReferenceUID) == "" {
		return 0, fmt.Errorf("%w: explicit frame of reference is required", ErrTemporalData)
	}
	voxels := 1
	for axis := range grid.Size {
		if grid.Size[axis] <= 0 || voxels > limit/grid.Size[axis] {
			return 0, fmt.Errorf("%w: grid exceeds %d voxels", ErrTemporalAdmission, limit)
		}
		voxels *= grid.Size[axis]
		if !finite(grid.Origin[axis]) || !finite(grid.Spacing[axis]) || grid.Spacing[axis] <= 0 {
			return 0, fmt.Errorf("%w: invalid grid origin or spacing", ErrTemporalData)
		}
	}
	for axis := 0; axis < 3; axis++ {
		for other := axis; other < 3; other++ {
			dot := 0.0
			for row := 0; row < 3; row++ {
				a, b := grid.Direction[row*3+axis], grid.Direction[row*3+other]
				if !finite(a) || !finite(b) {
					return 0, fmt.Errorf("%w: nonfinite grid direction", ErrTemporalData)
				}
				dot += a * b
			}
			want := 0.0
			if axis == other {
				want = 1
			}
			if math.Abs(dot-want) > 1e-9 {
				return 0, fmt.Errorf("%w: grid direction is not orthonormal", ErrTemporalData)
			}
		}
	}
	return voxels, nil
}
