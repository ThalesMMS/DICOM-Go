package registration

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"

	"github.com/ThalesMMS/dicom-go/optimize"
)

// FFDLevel is one resolution of a free-form deformation registration.
type FFDLevel struct {
	Level
	// RefineMesh doubles the control mesh, along every axis on which the
	// fixed image has more than one sample, before this level (ignored on
	// the first): the previous level's deformation carries over exactly
	// and the finer grid can then follow smaller structures.
	RefineMesh bool
}

// DefaultFFDLevels are three levels (shrink 4, 2, 1 with smoothing σ 2, 1
// and 0 mm) refining the mesh before the second and the third, so that the
// default 4-interval mesh ends with 16 intervals per axis.
func DefaultFFDLevels() []FFDLevel {
	return []FFDLevel{
		{Level: Level{Shrink: 4, SmoothingSigmaMM: 2}},
		{Level: Level{Shrink: 2, SmoothingSigmaMM: 1}, RefineMesh: true},
		{Level: Level{Shrink: 1}, RefineMesh: true},
	}
}

// ErrFFD reports images or options FreeFormDeformation cannot use.
var ErrFFD = errors.New("registration: invalid free-form deformation")

// FreeFormDeformation registers a moving image to a fixed one with a cubic
// B-spline deformation (#1184), after ITK's B-spline registration with
// MattesMutualInformationImageToImageMetricv4 and LBFGSBOptimizerv4: Mattes
// mutual information makes it usable across modalities (MR to CT, the soft
// tissue of PET/CT), the control grid is refined between resolution levels
// and each level is optimized with L-BFGS, or opt-in bounded L-BFGS-B. The
// metric's derivative uses the B-spline's sparse analytic
// Jacobian, so its cost does not grow with the number of control points.
//
// Each level smooths both images, shrinks the fixed grid into the points
// the metric compares and optionally samples them, as Method does; a fixed
// Seed makes the sampled points, and so the run, repeatable, and the
// metric's reduction does not depend on the number of workers. Run stops
// with the context's error when it is cancelled. The result is a proposal
// to review: nothing here stores a Spatial Registration.
type FreeFormDeformation struct {
	Fixed, Moving *Image
	// FixedMask and MovingMask restrict the compared points, as in
	// MetricDomain.
	FixedMask, MovingMask *Image
	// Initial, when set, is a fixed-to-moving transform found before (for
	// example the rigid or affine result of Method.Run). It is applied
	// after the deformation and kept fixed: a fixed point x maps to
	// Initial(x + D(x)). It is cloned, not changed.
	Initial Transform
	// MeshSize is the number of B-spline intervals per axis over the fixed
	// image at the first level (zero: 4 on axes with more than one fixed
	// sample, 1 otherwise). The grid covers the fixed image as
	// NewBSplineForImage does.
	MeshSize [3]int
	// Levels are coarse to fine (nil: DefaultFFDLevels).
	Levels []FFDLevel
	// Bins is the number of Mattes histogram bins (0: 32).
	Bins int
	// Sampling and Seed choose the compared points, as in Method.
	Sampling Sampling
	Seed     int64
	// Regularization weighs a membrane energy added to the metric: the
	// mean over control points of Σ ((c(k+e_a) − c(k))/h_a)², the squared
	// differences of neighbouring control-point displacements per grid
	// spacing h_a along each axis a, summed over the three displacement
	// components. It penalizes folding and keeps poorly constrained regions
	// smooth (0: none; values around 0.01–1 relative to the metric's nats).
	Regularization float64
	// Optimizer is the L-BFGS of each level (zero value: 100 iterations,
	// ITK's value tolerance, a gradient tolerance of 1e-9 and a first step
	// moving control points by at most one sample of the level's grid).
	Optimizer optimize.LBFGS
	// BoundedOptimizer selects L-BFGS-B and absolute displacement boxes. Nil
	// preserves Optimizer and all legacy defaults. See FFDBoundedOptions for
	// the optional sufficient Jacobian condition and its restricted domain.
	BoundedOptimizer *FFDBoundedOptions
	// Progress, when set, receives every metric evaluation.
	Progress func(Progress)
	// Workers bounds the metric's goroutines (0: GOMAXPROCS).
	Workers int
}

// FFDResult is a free-form deformation proposal.
type FFDResult struct {
	// BSpline is the deformation alone, in the fixed image's space.
	BSpline *BSpline
	// Transform maps fixed points to moving points: the B-spline, or a
	// Composite of the B-spline and the Initial transform. Use it with
	// TransformMapping and Resample, or sample it with
	// TransformDisplacementField for a Deformable Spatial Registration.
	Transform Transform
	// Value is the metric (with the regularization) at the end of the last
	// level.
	Value  float64
	Levels []LevelResult
	// JacobianBound is populated only for admitted guaranteed bounded mode.
	JacobianBound *FFDJacobianBound
}

// DisplacementField samples the result's fixed-to-moving mapping on grid,
// usually the fixed image's (Image.Grid), as a dense field: its Grid method
// gives the Deformable Spatial Registration grid, and FieldMapping the
// mapping Resample takes.
func (r FFDResult) DisplacementField(ctx context.Context, grid Grid) (*DisplacementField, error) {
	return TransformDisplacementField(ctx, r.Transform, grid)
}

// Run registers, stopping early with ctx's error when it is cancelled.
func (f FreeFormDeformation) Run(ctx context.Context) (FFDResult, error) {
	if f.Fixed == nil || f.Moving == nil {
		return FFDResult{}, fmt.Errorf("%w: fixed and moving images are required", ErrFFD)
	}
	if f.Regularization < 0 {
		return FFDResult{}, fmt.Errorf("%w: negative regularization", ErrFFD)
	}
	levels := f.Levels
	if levels == nil {
		levels = DefaultFFDLevels()
	}
	if len(levels) == 0 {
		return FFDResult{}, fmt.Errorf("%w: no level", ErrFFD)
	}
	if f.BoundedOptimizer != nil {
		maximum := f.BoundedOptimizer.MaxLevels
		if maximum < 0 {
			return FFDResult{}, fmt.Errorf("%w: bounded FFD level limit", ErrFFD)
		}
		if maximum == 0 {
			maximum = 16
		}
		if len(levels) > maximum {
			return FFDResult{}, ErrFFDResourceLimit
		}
		if ctx == nil {
			return FFDResult{}, fmt.Errorf("%w: bounded FFD nil context", ErrFFD)
		}
		if err := ctx.Err(); err != nil {
			return FFDResult{}, err
		}
	}
	for _, level := range levels {
		if level.Shrink < 1 || level.SmoothingSigmaMM < 0 {
			return FFDResult{}, fmt.Errorf("%w: level %+v: shrink must be at least 1 and σ not negative", ErrFFD, level.Level)
		}
		if p := level.SamplingPercentage; p != 0 {
			if err := ValidateSamplingPercentage(p); err != nil {
				return FFDResult{}, err
			}
		}
	}
	size := f.Fixed.Size()
	mesh := f.MeshSize
	var refineAxes [3]bool
	for axis := range mesh {
		refineAxes[axis] = size[axis] > 1
		if mesh[axis] == 0 {
			mesh[axis] = 1
			if refineAxes[axis] {
				mesh[axis] = 4
			}
		}
		if mesh[axis] < 1 {
			return FFDResult{}, fmt.Errorf("%w: mesh size %v", ErrFFD, f.MeshSize)
		}
	}
	bins := f.Bins
	if bins == 0 {
		bins = 32
	}
	var bounded *ffdBoundedRun
	if f.BoundedOptimizer != nil {
		var err error
		bounded, err = prepareFFDBounded(ctx, f, levels, mesh, refineAxes)
		if err != nil {
			return FFDResult{}, err
		}
		levels = append([]FFDLevel(nil), levels...)
	}
	bspline, err := NewBSplineForImage(f.Fixed, mesh)
	if err != nil {
		return FFDResult{}, err
	}
	assemble := func(b *BSpline) (Transform, error) {
		if f.Initial == nil {
			return b, nil
		}
		c, err := NewComposite(b, f.Initial.Clone())
		if err != nil {
			return nil, err
		}
		return c, c.SetOptimized(1, false)
	}
	transform, err := assemble(bspline)
	if err != nil {
		return FFDResult{}, err
	}
	helper := Method{Fixed: f.Fixed, Moving: f.Moving, FixedMask: f.FixedMask, MovingMask: f.MovingMask, Sampling: f.Sampling, Workers: f.Workers}
	random := rand.New(rand.NewSource(f.Seed))
	result := FFDResult{}
	if bounded != nil {
		result.Levels = make([]LevelResult, 0, len(levels))
	}
	for index, level := range levels {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if index > 0 && level.RefineMesh {
			if bspline, err = bspline.Refine(refineAxes); err != nil {
				return result, err
			}
			if transform, err = assemble(bspline); err != nil {
				return result, err
			}
		}
		if bounded != nil {
			if _, err := bounded.verify(ctx, bspline, transform); err != nil {
				return result, err
			}
		}
		fixed, moving, err := helper.smooth(ctx, level.SmoothingSigmaMM)
		if err != nil {
			return result, err
		}
		virtual := shrinkGrid(f.Fixed, level.Shrink)
		points, err := helper.samplePoints(virtual, level.Level, random)
		if err != nil {
			return result, err
		}
		domain := MetricDomain{FixedMask: f.FixedMask, MovingMask: f.MovingMask, Points: points, Workers: f.Workers}
		mattes, err := NewMattesMutualInformation(fixed, moving, domain, bins)
		if err != nil {
			return result, fmt.Errorf("registration: level %d: %w", index, err)
		}
		metric := regularizedMetric{metric: mattes, bspline: bspline, weight: f.Regularization}
		objective := &progressObjective{ctx: ctx, objective: Objective{Metric: metric, Transform: transform}, level: index, report: f.Progress}
		o := f.Optimizer
		if o.MaximumIterations == 0 {
			o.MaximumIterations = 100
		}
		if o.GradientTolerance == 0 {
			o.GradientTolerance = 1e-9
		}
		if o.InitialStepLength == 0 {
			o.InitialStepLength = MaximumStepSize(virtual)
		}
		var run optimize.Result
		if bounded == nil {
			run, err = o.Minimize(objective, transform.Parameters())
		} else {
			var boxOptimizer optimize.LBFGSB
			boxOptimizer, err = bounded.optimizer(ctx, bspline, virtual)
			if err == nil {
				run, err = boxOptimizer.MinimizeContext(ctx, objective, transform.Parameters())
			}
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
		if bounded != nil {
			result.JacobianBound, err = bounded.verify(ctx, bspline, transform)
			if err != nil {
				return result, err
			}
		}
		count := len(points)
		if points == nil {
			count = len(fixed.data)
		}
		var parameters []float64
		if bounded == nil {
			parameters = clone(run.X)
		} else {
			parameters = make([]float64, len(run.X))
			copy(parameters, run.X)
		}
		result.Levels = append(result.Levels, LevelResult{
			Level: level.Level, Points: count, Iterations: run.Iterations, Evaluations: objective.evaluations,
			Value: run.Value, Stop: run.Stop, Parameters: parameters,
		})
		result.Value = run.Value
	}
	result.BSpline, result.Transform = bspline, transform
	return result, nil
}

// regularizedMetric adds weight × the membrane energy of the B-spline's
// control points to a metric, and reports a metric without enough
// overlapping samples as +Inf so that the line search shortens its step.
type regularizedMetric struct {
	metric  Metric
	bspline *BSpline
	weight  float64
}

func (r regularizedMetric) Value(t Transform) (float64, error) {
	v, err := r.metric.Value(t)
	if errors.Is(err, ErrTooFewSamples) {
		return math.Inf(1), nil
	}
	if err != nil || r.weight == 0 {
		return v, err
	}
	energy, _ := r.bspline.MembraneEnergy(false)
	return v + r.weight*energy, nil
}

func (r regularizedMetric) ValueAndDerivative(t Transform) (float64, []float64, error) {
	v, gradient, err := r.metric.ValueAndDerivative(t)
	if errors.Is(err, ErrTooFewSamples) {
		return math.Inf(1), nil, nil
	}
	if err != nil || r.weight == 0 {
		return v, gradient, err
	}
	energy, energyGradient := r.bspline.MembraneEnergy(true)
	// The B-spline's parameters come first in the transform's vector (the
	// initial transform is frozen).
	for i, g := range energyGradient {
		gradient[i] += r.weight * g
	}
	return v + r.weight*energy, gradient, nil
}

// MembraneEnergy is the mean over control points of the squared
// differences of neighbouring displacements per grid spacing,
// (1/N)·Σ_k Σ_a Σ_d ((c_d(k+e_a) − c_d(k))/h_a)², and its gradient with
// respect to the parameters when asked: the regularization of
// FreeFormDeformation.
func (b *BSpline) MembraneEnergy(gradient bool) (float64, []float64) {
	count := b.count()
	var g []float64
	if gradient {
		g = make([]float64, len(b.coefficients))
	}
	strides := [3]int{1, b.size[0], b.size[0] * b.size[1]}
	spacing := [3]float64{b.spacing.X, b.spacing.Y, b.spacing.Z}
	scale := 1 / float64(count)
	var energy float64
	for d := 0; d < 3; d++ {
		values := b.coefficients[d*count : (d+1)*count]
		for z := 0; z < b.size[2]; z++ {
			for y := 0; y < b.size[1]; y++ {
				for x := 0; x < b.size[0]; x++ {
					at := [3]int{x, y, z}
					i := x + y*strides[1] + z*strides[2]
					for a := 0; a < 3; a++ {
						if at[a]+1 >= b.size[a] {
							continue
						}
						j := i + strides[a]
						inverse := 1 / (spacing[a] * spacing[a])
						diff := values[j] - values[i]
						energy += scale * inverse * diff * diff
						if gradient {
							g[d*count+j] += 2 * scale * inverse * diff
							g[d*count+i] -= 2 * scale * inverse * diff
						}
					}
				}
			}
		}
	}
	return energy, g
}
