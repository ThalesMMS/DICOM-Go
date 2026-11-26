package registration

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// Rigid motion correction of dynamic (4D) series (#1204): every phase of a
// time series is registered rigidly to one reference phase with Method and
// resampled onto the reference phase's grid with Resample, so that a voxel
// of the corrected series shows the same anatomy at every time point before
// time-intensity curves are read. The scheme (pairwise registration of each
// phase to a fixed reference, each run initialized from the neighbouring
// phase nearer the reference) follows the behaviour of elastix and ITK
// motion-correction pipelines; nothing is ported from them.

// ErrMotion reports phases or options CorrectMotion cannot use.
var ErrMotion = errors.New("registration: invalid motion correction")

// MotionMetric chooses the similarity metric of each phase registration.
type MotionMetric int

const (
	// MotionMattes is Mattes mutual information with 32 bins: robust to the
	// intensity changes of contrast uptake, the default.
	MotionMattes MotionMetric = iota
	// MotionMeanSquares compares intensities directly, for phases whose
	// intensities do not change (gated series without contrast).
	MotionMeanSquares
	// MotionCorrelation is normalized correlation, robust to a global
	// linear intensity change.
	MotionCorrelation
)

// Default limits of CorrectMotion.
const (
	// DefaultMotionSamples bounds the registration images per axis: larger
	// phases are block-averaged down to it (the resampling always reads the
	// full-resolution phase).
	DefaultMotionSamples = 96
	// DefaultMotionPoints is about the number of points each level compares
	// when its SamplingPercentage is zero.
	DefaultMotionPoints = 60000
	// DefaultMotionIterations bounds the gradient descent of each level:
	// successive phases start close to their motion, so fewer iterations
	// than a single registration's 500 suffice.
	DefaultMotionIterations = 150
)

// MotionOptions configure CorrectMotion.
type MotionOptions struct {
	// Reference is the index of the phase the others are aligned to; its
	// grid is the grid of the corrected series.
	Reference int
	Metric    MotionMetric
	// Levels are coarse to fine (nil: shrink 2, 1, 1 with σ 4, 2, 0 mm). A
	// level's zero SamplingPercentage keeps about MaxPoints points.
	Levels []Level
	// MaxSamples bounds the registration images per axis (0:
	// DefaultMotionSamples); MaxPoints the compared points per level (0:
	// DefaultMotionPoints).
	MaxSamples, MaxPoints int
	// MaxIterations bounds each level's gradient descent (0:
	// DefaultMotionIterations).
	MaxIterations int
	// Seed makes the regular sampling's jitter repeatable.
	Seed int64
	// Padding is written where a corrected phase is outside its own field;
	// PadWithMinimum uses each phase's lowest value instead (air for CT).
	Padding        float32
	PadWithMinimum bool
	// Workers bounds the metric's goroutines (0: GOMAXPROCS).
	Workers int
	// Progress, when set, receives the stages of every phase.
	Progress func(MotionProgress)
}

// MotionStage is a step of one phase's correction.
type MotionStage int

const (
	// MotionLoad reads a phase.
	MotionLoad MotionStage = iota
	// MotionRegister registers it to the reference (Level and Evaluation).
	MotionRegister
	// MotionResample resamples it onto the reference grid (Done of Total
	// slices).
	MotionResample
)

// MotionProgress reports CorrectMotion: Completed of Phases phases are
// done, and Phase (an index) is at Stage.
type MotionProgress struct {
	Phase, Completed, Phases int
	Stage                    MotionStage
	Level, Evaluation        int
	Done, Total              int
}

// PhaseMotion is the rigid motion found for one phase.
type PhaseMotion struct {
	Index     int
	Reference bool
	// Transform maps reference points onto the phase (the package's fixed
	// to moving direction); the identity for the reference phase.
	Transform *Euler3D
	// RotationDegrees are the Euler angles about x, y and z, Translation
	// the translation in millimetres, and MaxDisplacementMM the largest
	// displacement of the reference grid's corner samples.
	RotationDegrees   [3]float64
	Translation       render.Vec3
	MaxDisplacementMM float64
	// Levels are the registration's levels (none for the reference).
	Levels []LevelResult
	// Value is the metric at the end of the registration.
	Value float64
}

// PhaseCorrection is one corrected phase.
type PhaseCorrection struct {
	PhaseMotion
	// Corrected is the phase on the reference grid.
	Corrected *Resampled
}

// MotionSource reads phase index (0 ≤ index < phases) in modality values.
// CorrectMotion reads each phase once, except the reference, and keeps
// only the reference and the phase being corrected.
type MotionSource func(ctx context.Context, index int) (*Image, error)

// MotionSink receives each corrected phase, the reference first and then
// outward from it (Reference+1, …, last, then Reference−1, …, 0). An error
// stops the correction.
type MotionSink func(PhaseCorrection) error

// CorrectMotion registers every phase rigidly (Euler3D) to the reference
// phase and resamples it onto the reference grid trilinearly. Each phase is
// registered from the motion of its neighbour nearer the reference, since
// motion between successive time points is small; the reference phase is
// resampled through the identity. It returns the motion of every phase in
// index order. ctx cancels it between and within registrations.
func CorrectMotion(ctx context.Context, phases int, source MotionSource, opts MotionOptions, sink MotionSink) ([]PhaseMotion, error) {
	if phases < 2 || source == nil || sink == nil {
		return nil, fmt.Errorf("%w: %d phases, a source and a sink are required", ErrMotion, phases)
	}
	if opts.Reference < 0 || opts.Reference >= phases {
		return nil, fmt.Errorf("%w: reference %d outside 0…%d", ErrMotion, opts.Reference, phases-1)
	}
	if opts.MaxSamples < 0 || opts.MaxPoints < 0 || opts.MaxIterations < 0 || !finite(float64(opts.Padding)) {
		return nil, fmt.Errorf("%w: negative limits or non-finite padding", ErrMotion)
	}
	switch opts.Metric {
	case MotionMattes, MotionMeanSquares, MotionCorrelation:
	default:
		return nil, fmt.Errorf("%w: unknown metric %d", ErrMotion, opts.Metric)
	}
	levels := opts.Levels
	if levels == nil {
		levels = []Level{{Shrink: 2, SmoothingSigmaMM: 4}, {Shrink: 1, SmoothingSigmaMM: 2}, {Shrink: 1}}
	}
	maxSamples, maxPoints := opts.MaxSamples, opts.MaxPoints
	if maxSamples == 0 {
		maxSamples = DefaultMotionSamples
	}
	if maxPoints == 0 {
		maxPoints = DefaultMotionPoints
	}
	iterations := opts.MaxIterations
	if iterations == 0 {
		iterations = DefaultMotionIterations
	}
	completed := 0
	report := func(p MotionProgress) {
		if opts.Progress != nil {
			p.Completed, p.Phases = completed, phases
			opts.Progress(p)
		}
	}
	load := func(index int) (*Image, error) {
		report(MotionProgress{Phase: index, Stage: MotionLoad})
		im, err := source(ctx, index)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, fmt.Errorf("registration: phase %d: %w", index, err)
		}
		if im == nil {
			return nil, fmt.Errorf("%w: phase %d is missing", ErrMotion, index)
		}
		return im, nil
	}
	reference, err := load(opts.Reference)
	if err != nil {
		return nil, err
	}
	grid := reference.Grid()
	fixed, err := reference.Downsample(motionFactors(reference.Size(), maxSamples))
	if err != nil {
		return nil, err
	}
	centre := fixed.Point(render.Vec3{X: float64(fixed.size[0]-1) / 2, Y: float64(fixed.size[1]-1) / 2, Z: float64(fixed.size[2]-1) / 2})
	corners := CornerPoints(reference)
	motions := make([]PhaseMotion, phases)

	correct := func(index int, moving *Image, motion PhaseMotion) error {
		padding := opts.Padding
		if opts.PadWithMinimum {
			padding = lowest(moving)
		}
		mapping, err := TransformMapping(motion.Transform)
		if err != nil {
			return err
		}
		report(MotionProgress{Phase: index, Stage: MotionResample, Total: grid.Size[2]})
		resampled, err := Resample(ctx, moving, grid, mapping, ResampleOptions{
			Padding: padding,
			Progress: func(done, total int) {
				report(MotionProgress{Phase: index, Stage: MotionResample, Done: done, Total: total})
			},
		})
		if err != nil {
			return err
		}
		motions[index] = motion
		if err := sink(PhaseCorrection{PhaseMotion: motion, Corrected: resampled}); err != nil {
			return err
		}
		completed++
		return nil
	}

	identity := NewEuler3D()
	if err := identity.SetCenter(centre); err != nil {
		return nil, err
	}
	if err := correct(opts.Reference, reference, describeMotion(opts.Reference, true, identity, corners, MethodResult{})); err != nil {
		return nil, err
	}
	for _, direction := range []int{1, -1} {
		previous := identity
		for index := opts.Reference + direction; index >= 0 && index < phases; index += direction {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			phase, err := load(index)
			if err != nil {
				return nil, err
			}
			moving, err := phase.Downsample(motionFactors(phase.Size(), maxSamples))
			if err != nil {
				return nil, err
			}
			phaseLevels := append([]Level(nil), levels...)
			for i := range phaseLevels {
				if phaseLevels[i].SamplingPercentage == 0 {
					phaseLevels[i].SamplingPercentage = motionSampling(fixed, max(phaseLevels[i].Shrink, 1), maxPoints)
				}
			}
			method := Method{
				Fixed: fixed, Moving: moving, Transform: previous, Levels: phaseLevels,
				Sampling: SampleRegular, Seed: opts.Seed, Workers: opts.Workers,
				NewMetric: motionMetric(opts.Metric),
				Progress: func(p Progress) {
					report(MotionProgress{Phase: index, Stage: MotionRegister, Level: p.Level, Evaluation: p.Evaluation})
				},
			}
			method.Optimizer.MaximumIterations = iterations
			report(MotionProgress{Phase: index, Stage: MotionRegister})
			result, err := method.Run(ctx)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, ctxErr
				}
				return nil, fmt.Errorf("registration: phase %d: %w", index, err)
			}
			euler, ok := result.Transform.(*Euler3D)
			if !ok {
				return nil, fmt.Errorf("%w: unexpected transform %s", ErrMotion, result.Transform.Kind())
			}
			if err := correct(index, phase, describeMotion(index, false, euler, corners, result)); err != nil {
				return nil, err
			}
			previous = euler
		}
	}
	return motions, nil
}

// describeMotion summarizes a phase's transform.
func describeMotion(index int, reference bool, transform *Euler3D, corners []render.Vec3, result MethodResult) PhaseMotion {
	clone := transform.Clone().(*Euler3D)
	parameters := clone.Parameters()
	motion := PhaseMotion{
		Index: index, Reference: reference, Transform: clone,
		RotationDegrees: [3]float64{parameters[0] * 180 / math.Pi, parameters[1] * 180 / math.Pi, parameters[2] * 180 / math.Pi},
		Translation:     render.Vec3{X: parameters[3], Y: parameters[4], Z: parameters[5]},
		Levels:          result.Levels, Value: result.Value,
	}
	for _, p := range corners {
		motion.MaxDisplacementMM = math.Max(motion.MaxDisplacementMM, clone.TransformPoint(p).Sub(p).Length())
	}
	return motion
}

// motionFactors block-averages size to at most limit samples per axis.
func motionFactors(size [3]int, limit int) [3]int {
	var factors [3]int
	for axis := range factors {
		factors[axis] = max(1, (size[axis]+limit-1)/limit)
	}
	return factors
}

// motionSampling keeps about points of a level's grid.
func motionSampling(im *Image, shrink, points int) float64 {
	size := im.Size()
	total := float64(max(1, size[0]/shrink) * max(1, size[1]/shrink) * max(1, size[2]/shrink))
	return math.Min(1, float64(points)/total)
}

func motionMetric(metric MotionMetric) func(fixed, moving *Image, domain MetricDomain) (Metric, error) {
	switch metric {
	case MotionMeanSquares:
		return func(f, m *Image, d MetricDomain) (Metric, error) { return NewMeanSquares(f, m, d) }
	case MotionCorrelation:
		return func(f, m *Image, d MetricDomain) (Metric, error) { return NewCorrelation(f, m, d) }
	}
	return func(f, m *Image, d MetricDomain) (Metric, error) { return NewMattesMutualInformation(f, m, d, 32) }
}

// lowest is the smallest finite sample of im (0 when there is none).
func lowest(im *Image) float32 {
	out := float32(math.Inf(1))
	for _, v := range im.data {
		if finite(float64(v)) && v < out {
			out = v
		}
	}
	if math.IsInf(float64(out), 0) {
		return 0
	}
	return out
}
