package registration

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"

	"github.com/ThalesMMS/dicom-go/imagefilter"
	"github.com/ThalesMMS/dicom-go/optimize"
	"github.com/ThalesMMS/dicom-go/render"
)

// Level is one resolution of a multi-resolution registration.
type Level struct {
	// Shrink coarsens the grid of fixed points the metric compares by this
	// factor on every axis (1: the fixed image's grid). The images keep
	// their full resolution, as in ITK.
	Shrink int
	// SmoothingSigmaMM smooths both images with a recursive Gaussian of
	// this standard deviation, in millimetres (0: none).
	SmoothingSigmaMM float64
	// SamplingPercentage, in (0, 1], is the share of the level's points
	// the metric samples with a Sampling other than SampleAll (0: 1).
	SamplingPercentage float64
}

// DefaultLevels are ITK's ImageRegistrationMethodv4 defaults: shrink 2,
// 1, 1 with smoothing σ 2, 1, 0 mm.
func DefaultLevels() []Level {
	return []Level{{Shrink: 2, SmoothingSigmaMM: 2}, {Shrink: 1, SmoothingSigmaMM: 1}, {Shrink: 1}}
}

// Sampling chooses the points of a level the metric compares.
type Sampling int

const (
	// SampleAll compares every point of the level's grid.
	SampleAll Sampling = iota
	// SampleRegular takes every ⌈1/percentage⌉-th point in raster order,
	// starting with the first, each moved by a Gaussian jitter of a third
	// of the spacing.
	SampleRegular
	// SampleRandom draws ⌊points·percentage⌋ points uniformly, with
	// replacement, with the same jitter.
	SampleRandom
)

// ErrSamplingPercentage reports a sampling percentage outside (0, 1].
var ErrSamplingPercentage = errors.New("registration: sampling percentage outside (0, 1]")

// ValidateSamplingPercentage accepts a sampling percentage in (0, 1], as
// ITK's SetMetricSamplingPercentage does (a Level's zero means 1).
func ValidateSamplingPercentage(p float64) error {
	if !(p > 0 && p <= 1) {
		return fmt.Errorf("%w: %v", ErrSamplingPercentage, p)
	}
	return nil
}

// Method registers a moving image to a fixed image over resolution levels,
// after ITK's ImageRegistrationMethodv4: at each level both images are
// smoothed, the fixed grid is shrunk into the points the metric compares
// (optionally sampled), the parameter scales and learning rate are
// estimated from physical shifts, and the transform is optimized from the
// previous level's result. A registration is only a proposal: nothing here
// infers or stores a Spatial Registration.
type Method struct {
	Fixed, Moving *Image
	// FixedMask and MovingMask restrict the compared points, as in
	// MetricDomain.
	FixedMask, MovingMask *Image
	// Transform is the initial fixed-to-moving transform (see
	// CenterTransform); it is cloned, not changed.
	Transform Transform
	// NewMetric builds the metric of a level from the smoothed images;
	// nil uses Mattes mutual information with 20 bins, ITK's default.
	NewMetric func(fixed, moving *Image, domain MetricDomain) (Metric, error)
	// Levels are coarse to fine (nil: DefaultLevels).
	Levels []Level
	// Sampling and Seed choose the compared points; a fixed seed makes a
	// run repeatable.
	Sampling Sampling
	Seed     int64
	// Optimizer is the regular-step gradient descent of each level; its
	// Scales, StepScale and MaximumStepSize are set per level from
	// PhysicalShiftScales unless Scales is set (zero value: 500
	// iterations, relaxation 0.5, minimum step 1e-6, and a gradient
	// tolerance of 1e-12, since scaled metric gradients are small).
	Optimizer optimize.RegularStepGradientDescent
	// Minimize, when set, replaces the gradient descent: it receives the
	// level, the objective, the starting parameters and the estimated
	// scales (for Powell, Nelder–Mead or the (1+1) strategy).
	Minimize func(level int, objective optimize.GradientFunction, start, scales []float64) (optimize.Result, error)
	// Progress, when set, receives every metric evaluation.
	Progress func(Progress)
	// Workers bounds the metric's goroutines (0: the shared concurrency
	// budget, GOMAXPROCS by default).
	Workers int
}

// Progress reports one metric evaluation.
type Progress struct {
	Level, Evaluation int
	Value             float64
}

// LevelResult summarizes one level.
type LevelResult struct {
	Level       Level
	Points      int // compared points before masks and overlap
	Iterations  int
	Evaluations int
	Value       float64
	Stop        optimize.StopReason
	Parameters  []float64
}

// MethodResult is a registration proposal.
type MethodResult struct {
	// Transform is the optimized fixed-to-moving transform.
	Transform Transform
	// Value is the metric at the end of the last level.
	Value  float64
	Levels []LevelResult
}

// Run registers, stopping early with ctx's error when it is cancelled.
func (m Method) Run(ctx context.Context) (MethodResult, error) {
	if m.Fixed == nil || m.Moving == nil || m.Transform == nil {
		return MethodResult{}, fmt.Errorf("%w: fixed image, moving image and transform are required", ErrImage)
	}
	levels := m.Levels
	if levels == nil {
		levels = DefaultLevels()
	}
	for _, level := range levels {
		if level.Shrink < 1 || level.SmoothingSigmaMM < 0 {
			return MethodResult{}, fmt.Errorf("registration: level %+v: shrink must be at least 1 and σ not negative", level)
		}
		if p := level.SamplingPercentage; p != 0 {
			if err := ValidateSamplingPercentage(p); err != nil {
				return MethodResult{}, err
			}
		}
	}
	newMetric := m.NewMetric
	if newMetric == nil {
		newMetric = func(fixed, moving *Image, domain MetricDomain) (Metric, error) {
			return NewMattesMutualInformation(fixed, moving, domain, 20)
		}
	}
	transform := m.Transform.Clone()
	random := rand.New(rand.NewSource(m.Seed))
	result := MethodResult{}
	maximumStep := 0.0
	for index, level := range levels {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		fixed, moving, err := m.smooth(ctx, level.SmoothingSigmaMM)
		if err != nil {
			return result, err
		}
		virtual := shrinkGrid(m.Fixed, level.Shrink)
		points, err := m.samplePoints(virtual, level, random)
		if err != nil {
			return result, err
		}
		domain := MetricDomain{FixedMask: m.FixedMask, MovingMask: m.MovingMask, Points: points, Workers: m.Workers}
		metric, err := newMetric(fixed, moving, domain)
		if err != nil {
			return result, fmt.Errorf("registration: level %d: %w", index, err)
		}
		estimator := PhysicalShiftScales{Transform: transform.Clone(), Points: CornerPoints(virtual)}
		scales := m.Optimizer.Scales
		if scales == nil {
			if scales, err = estimator.Scales(); err != nil {
				return result, err
			}
		}
		if maximumStep == 0 {
			// Estimated once, from the first level, as ITK does.
			maximumStep = MaximumStepSize(virtual)
		}
		objective := &progressObjective{ctx: ctx, objective: Objective{Metric: metric, Transform: transform}, level: index, report: m.Progress}
		start := transform.Parameters()
		var run optimize.Result
		if m.Minimize != nil {
			run, err = m.Minimize(index, objective, start, scales)
		} else {
			o := m.Optimizer
			o.Scales = scales
			if o.StepScale == nil && o.MaximumStepSize == 0 {
				o.StepScale, o.MaximumStepSize = estimator, maximumStep
			}
			if o.MaximumIterations == 0 {
				o.MaximumIterations = 500
			}
			if o.MinimumStepLength == 0 {
				// The step mixes radians and millimetres; 1e-4 ends a level
				// before small rotation components converge.
				o.MinimumStepLength = 1e-6
			}
			if o.GradientMagnitudeTolerance == 0 {
				// Metric gradients divided by scales of thousands are tiny;
				// the step length and iterations end a level instead.
				o.GradientMagnitudeTolerance = 1e-12
			}
			run, err = o.Minimize(objective, start)
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, ctxErr
			}
			return result, fmt.Errorf("registration: level %d: %w", index, err)
		}
		if err := transform.SetParameters(run.X); err != nil {
			return result, err
		}
		count := len(points)
		if points == nil {
			count = len(fixed.data)
		}
		result.Levels = append(result.Levels, LevelResult{
			Level: level, Points: count, Iterations: run.Iterations, Evaluations: objective.evaluations,
			Value: run.Value, Stop: run.Stop, Parameters: clone(run.X),
		})
		result.Value = run.Value
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.Transform = transform
	return result, nil
}

func clone(x []float64) []float64 { return append([]float64(nil), x...) }

// progressObjective counts evaluations, reports them and stops on
// cancellation.
type progressObjective struct {
	ctx         context.Context
	objective   Objective
	level       int
	evaluations int
	report      func(Progress)
}

func (p *progressObjective) Value(x []float64) (float64, error) {
	if err := p.ctx.Err(); err != nil {
		return 0, err
	}
	var value float64
	var err error
	if metric, ok := p.objective.Metric.(interface {
		ValueContext(context.Context, Transform) (float64, error)
	}); ok {
		if err = p.objective.Transform.SetParameters(x); err == nil {
			value, err = metric.ValueContext(p.ctx, p.objective.Transform)
		}
	} else {
		value, err = p.objective.Value(x)
	}
	p.note(value, err)
	return value, err
}

func (p *progressObjective) ValueAndGradient(x []float64) (float64, []float64, error) {
	if err := p.ctx.Err(); err != nil {
		return 0, nil, err
	}
	var value float64
	var gradient []float64
	var err error
	if metric, ok := p.objective.Metric.(interface {
		ValueAndDerivativeContext(context.Context, Transform) (float64, []float64, error)
	}); ok {
		if err = p.objective.Transform.SetParameters(x); err == nil {
			value, gradient, err = metric.ValueAndDerivativeContext(p.ctx, p.objective.Transform)
		}
	} else {
		value, gradient, err = p.objective.ValueAndGradient(x)
	}
	p.note(value, err)
	return value, gradient, err
}

func (p *progressObjective) note(value float64, err error) {
	p.evaluations++
	if err == nil && p.report != nil {
		p.report(Progress{Level: p.level, Evaluation: p.evaluations, Value: value})
	}
}

// smooth returns both images smoothed with σ mm (the originals for 0).
func (m Method) smooth(ctx context.Context, sigma float64) (*Image, *Image, error) {
	if sigma == 0 {
		return m.Fixed, m.Moving, nil
	}
	out := [2]*Image{}
	for i, im := range []*Image{m.Fixed, m.Moving} {
		volume := &imagefilter.Volume{Dims: im.size, SpacingMM: [3]float64{im.spacing.X, im.spacing.Y, im.spacing.Z}, Data: im.data}
		smoothed, err := imagefilter.SmoothRecursiveGaussian(ctx, volume, imagefilter.RecursiveGaussianOptions{SigmaMM: imagefilter.IsotropicSigma(sigma), Workers: m.Workers})
		if err != nil {
			return nil, nil, err
		}
		if out[i], err = NewImage(im.size, smoothed.Data, im.origin, im.spacing, im.direction); err != nil {
			return nil, nil, err
		}
	}
	return out[0], out[1], nil
}

// shrinkGrid is the geometry of im shrunk by factor, as ITK's
// ShrinkImageFilter: spacing × factor, size ⌊size/factor⌋ (at least 1),
// and the same physical centre. Only the geometry is used.
func shrinkGrid(im *Image, factor int) *Image {
	if factor == 1 {
		return im
	}
	var size [3]int
	for axis := range size {
		size[axis] = max(1, im.size[axis]/factor)
	}
	spacing := im.spacing.Scale(float64(factor))
	scale := Matrix3{{spacing.X, 0, 0}, {0, spacing.Y, 0}, {0, 0, spacing.Z}}
	centreIn := im.Point(render.Vec3{X: float64(im.size[0]-1) / 2, Y: float64(im.size[1]-1) / 2, Z: float64(im.size[2]-1) / 2})
	centreOut := im.direction.Mul(scale).MulVec(render.Vec3{X: float64(size[0]-1) / 2, Y: float64(size[1]-1) / 2, Z: float64(size[2]-1) / 2})
	shrunk, err := NewImage(size, make([]float32, size[0]*size[1]*size[2]), centreIn.Sub(centreOut), spacing, im.direction)
	if err != nil {
		return im
	}
	return shrunk
}

// samplePoints returns the level's compared points, nil for every sample
// of the fixed image itself.
func (m Method) samplePoints(virtual *Image, level Level, random *rand.Rand) ([]render.Vec3, error) {
	percentage := level.SamplingPercentage
	if percentage == 0 {
		percentage = 1
	}
	total := len(virtual.data)
	point := func(i int) render.Vec3 {
		return virtual.Point(render.Vec3{X: float64(i % virtual.size[0]), Y: float64(i / virtual.size[0] % virtual.size[1]), Z: float64(i / (virtual.size[0] * virtual.size[1]))})
	}
	jitter := func(p render.Vec3) render.Vec3 {
		// A Gaussian of a third of the spacing along each index axis.
		offset := render.Vec3{
			X: random.NormFloat64() * virtual.spacing.X / 3, Y: random.NormFloat64() * virtual.spacing.Y / 3, Z: random.NormFloat64() * virtual.spacing.Z / 3,
		}
		if virtual.size[2] == 1 {
			offset.Z = 0
		}
		return p.Add(virtual.direction.MulVec(offset))
	}
	switch m.Sampling {
	case SampleAll:
		if virtual == m.Fixed {
			return nil, nil
		}
		points := make([]render.Vec3, total)
		for i := range points {
			points[i] = point(i)
		}
		return points, nil
	case SampleRegular:
		step := int(math.Ceil(1 / percentage))
		points := make([]render.Vec3, 0, total/step+1)
		for i := 0; i < total; i += step {
			points = append(points, jitter(point(i)))
		}
		return points, nil
	case SampleRandom:
		count := int(float64(total) * percentage)
		points := make([]render.Vec3, count)
		for i := range points {
			points[i] = jitter(point(random.Intn(total)))
		}
		return points, nil
	}
	return nil, fmt.Errorf("registration: unknown sampling %d", m.Sampling)
}
