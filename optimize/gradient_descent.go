package optimize

import (
	"fmt"
	"math"
)

// StepScaleEstimator estimates how far a parameter step moves the image in
// physical units, for learning-rate estimation
// (registration.PhysicalShiftScales implements it).
type StepScaleEstimator interface {
	StepScale(step []float64) (float64, error)
}

// RegularStepGradientDescent is ITK's RegularStepGradientDescentOptimizerv4:
// each iteration steps a fixed length against the scaled gradient, and the
// length shrinks by RelaxationFactor whenever the gradient reverses
// direction (its scalar product with the previous one is negative), which
// happens once the step overshoots the minimum.
type RegularStepGradientDescent struct {
	// LearningRate is the initial step length (0: 1).
	LearningRate float64
	// RelaxationFactor, in [0, 1), multiplies the step length when the
	// gradient reverses (0: 0.5).
	RelaxationFactor float64
	// MinimumStepLength stops once the step is shorter (0: 1e-4).
	MinimumStepLength float64
	// GradientMagnitudeTolerance stops once the scaled gradient is smaller
	// (0: 1e-4; negative is an error).
	GradientMagnitudeTolerance float64
	// MaximumIterations bounds the iterations (0: 100; negative: none).
	MaximumIterations int
	// Scales divide the gradient, so that parameters of different units
	// (radians, millimetres) move comparably (nil: all ones).
	Scales []float64
	// StepScale and MaximumStepSize, when both set, estimate the learning
	// rate so that the first step (or every step, with
	// EstimateLearningRateEachIteration) moves the image by at most
	// MaximumStepSize, in physical units.
	StepScale                         StepScaleEstimator
	MaximumStepSize                   float64
	EstimateLearningRateEachIteration bool
	// ConvergenceWindow, when positive, also stops once the
	// WindowConvergenceMonitor of that window size over the values falls
	// to MinimumConvergenceValue or below (0: 1e-8). ITK leaves it off for
	// this optimizer.
	ConvergenceWindow       int
	MinimumConvergenceValue float64
}

// Minimize runs from x0.
func (o RegularStepGradientDescent) Minimize(f GradientFunction, x0 []float64) (Result, error) {
	n := len(x0)
	if n == 0 {
		return Result{}, fmt.Errorf("%w: no parameters", ErrOptions)
	}
	scales, err := scalesFor(o.Scales, n)
	if err != nil {
		return Result{}, err
	}
	relaxationFactor := o.RelaxationFactor
	if relaxationFactor == 0 {
		relaxationFactor = 0.5
	}
	if relaxationFactor < 0 || relaxationFactor >= 1 {
		return Result{}, fmt.Errorf("%w: relaxation factor %v outside [0, 1)", ErrOptions, relaxationFactor)
	}
	if o.GradientMagnitudeTolerance < 0 {
		return Result{}, fmt.Errorf("%w: negative gradient magnitude tolerance", ErrOptions)
	}
	learningRate := defaultTo(o.LearningRate, 1)
	minimumStep := defaultTo(o.MinimumStepLength, 1e-4)
	gradientTolerance := defaultTo(o.GradientMagnitudeTolerance, 1e-4)
	maxIterations := o.MaximumIterations
	if maxIterations == 0 {
		maxIterations = 100
	}
	var monitor *WindowConvergenceMonitor
	if o.ConvergenceWindow > 0 {
		monitor = NewWindowConvergenceMonitor(o.ConvergenceWindow)
	}
	minimumConvergence := defaultTo(o.MinimumConvergenceValue, 1e-8)

	x := clone(x0)
	previous := make([]float64, n)
	relaxation := 1.0
	result := Result{Stop: StopMaximumIterations}
	evaluations := 0
	for iteration := 0; maxIterations < 0 || iteration < maxIterations; iteration++ {
		value, gradient, err := f.ValueAndGradient(x)
		evaluations++
		if err != nil {
			return Result{}, err
		}
		if len(gradient) != n {
			return Result{}, fmt.Errorf("%w: %d gradient components for %d parameters", ErrOptions, len(gradient), n)
		}
		result.Value, result.Iterations = value, iteration
		if monitor != nil {
			monitor.Add(value)
			if monitor.Value() <= minimumConvergence {
				result.Stop = StopConvergenceWindow
				break
			}
		}
		scaled := make([]float64, n)
		var magnitude, direction float64
		for i := range scaled {
			scaled[i] = gradient[i] / scales[i]
			magnitude += scaled[i] * scaled[i]
			direction += scaled[i] * previous[i]
		}
		magnitude = math.Sqrt(magnitude)
		if magnitude < gradientTolerance {
			result.Stop = StopGradientMagnitude
			break
		}
		if direction < 0 {
			relaxation *= relaxationFactor
		}
		step := relaxation * learningRate
		if step < minimumStep {
			result.Stop = StopMinimumStepLength
			break
		}
		if o.StepScale != nil && o.MaximumStepSize > 0 && (iteration == 0 || o.EstimateLearningRateEachIteration) {
			scale, err := o.StepScale.StepScale(scaled)
			if err != nil {
				return Result{}, err
			}
			learningRate = 1
			if scale > 0 {
				learningRate = o.MaximumStepSize / scale
			}
			learningRate *= magnitude
			step = relaxation * learningRate
		}
		for i := range x {
			x[i] -= step * scaled[i] / magnitude
		}
		previous = scaled
		result.Iterations = iteration + 1
	}
	result.X, result.Evaluations = x, evaluations
	return result, nil
}

// WindowConvergenceMonitor is ITK's WindowConvergenceMonitoringFunction: it
// fits a line to the last Window values, normalized by the total of all
// absolute values added, and reports the negated slope, which falls toward
// zero as an optimization levels off.
type WindowConvergenceMonitor struct {
	window int
	values []float64
	total  float64
}

// NewWindowConvergenceMonitor watches the last window values (at least 2;
// ITK's gradient descent uses 50).
func NewWindowConvergenceMonitor(window int) *WindowConvergenceMonitor {
	return &WindowConvergenceMonitor{window: max(window, 2)}
}

// Add records a value.
func (m *WindowConvergenceMonitor) Add(value float64) {
	m.values = append(m.values, value)
	if len(m.values) > m.window {
		m.values = m.values[1:]
	}
	m.total += math.Abs(value)
}

// Value is the convergence value: +Inf until the window is full, then the
// decrease over the window of the linear B-spline fitted to the normalized
// values (ITK's single-level Lee–Wolberg–Shin fit with two control points).
func (m *WindowConvergenceMonitor) Value() float64 {
	if len(m.values) < m.window || m.total == 0 {
		return math.Inf(1)
	}
	var numerator, denominator [2]float64
	for n, v := range m.values {
		t := float64(n) / float64(m.window-1)
		t = math.Min(t, 1-1e-4)
		basis := [2]float64{1 - t, t}
		w2 := basis[0]*basis[0] + basis[1]*basis[1]
		z := v / m.total
		for k := 0; k < 2; k++ {
			numerator[k] += basis[k] * basis[k] * basis[k] * z / w2
			denominator[k] += basis[k] * basis[k]
		}
	}
	var control [2]float64
	for k := 0; k < 2; k++ {
		if denominator[k] > 1e-300 {
			control[k] = numerator[k] / denominator[k]
		}
	}
	return control[0] - control[1]
}
