package optimize

import (
	"fmt"
	"math"
)

const (
	// StopLineSearch: the line search found no point with a sufficient
	// decrease, usually because the function is flat or noisy at the
	// precision reached.
	StopLineSearch StopReason = "line-search"
	// StopMaximumEvaluations: the evaluation budget is spent.
	StopMaximumEvaluations StopReason = "maximum-evaluations"
)

// LBFGS is the limited-memory BFGS quasi-Newton method (Nocedal, "Updating
// quasi-Newton matrices with limited storage", Math. Comp. 35, 1980; Liu
// and Nocedal, 1989), for smooth functions of many parameters such as the
// control points of a B-spline deformation. It plays the part of ITK's
// LBFGSBOptimizerv4 without the bounds: each iteration takes the two-loop
// product of the last Memory curvature pairs with the gradient (the
// initial Hessian scaled by sᵀy/yᵀy) and a line search satisfying the
// strong Wolfe conditions (Nocedal and Wright, Numerical Optimization,
// algorithms 3.5 and 3.6, with safeguarded cubic interpolation). Pairs with
// sᵀy ≤ 0 are skipped, which keeps the product positive definite.
//
// A function may return +Inf for points outside its domain (for example a
// metric where too few samples overlap): the line search then shortens the
// step. Any error ends the minimization with that error.
type LBFGS struct {
	// Memory is the number of curvature pairs kept (0: 5, ITK's default).
	Memory int
	// MaximumIterations bounds the iterations (0: 100; negative: none).
	MaximumIterations int
	// MaximumEvaluations bounds the function evaluations, line searches
	// included (0: none beyond the iterations).
	MaximumEvaluations int
	// GradientTolerance stops once the largest gradient component is at
	// most this (0: 1e-5, ITK's projected gradient tolerance).
	GradientTolerance float64
	// ValueTolerance stops once an iteration decreases the value by at
	// most ValueTolerance·max(|f|, |f_previous|, 1) (0: 1e7 machine
	// epsilons, ITK's cost function convergence factor).
	ValueTolerance float64
	// InitialStepLength is the largest parameter change of the first step,
	// along the negated gradient (0: a step of Euclidean length one).
	InitialStepLength float64
	// MaximumLineSearchEvaluations bounds the evaluations of one line
	// search (0: 20).
	MaximumLineSearchEvaluations int
}

// Strong Wolfe constants: sufficient decrease and curvature.
const (
	wolfeDecrease  = 1e-4
	wolfeCurvature = 0.9
)

// Minimize runs from x0.
func (o LBFGS) Minimize(f GradientFunction, x0 []float64) (Result, error) {
	n := len(x0)
	if n == 0 {
		return Result{}, fmt.Errorf("%w: no parameters", ErrOptions)
	}
	if o.Memory < 0 || o.GradientTolerance < 0 || o.ValueTolerance < 0 || o.InitialStepLength < 0 || o.MaximumLineSearchEvaluations < 0 || o.MaximumEvaluations < 0 {
		return Result{}, fmt.Errorf("%w: negative L-BFGS option", ErrOptions)
	}
	memory := defaultToInt(o.Memory, 5)
	maxIterations := o.MaximumIterations
	if maxIterations == 0 {
		maxIterations = 100
	}
	gradientTolerance := defaultTo(o.GradientTolerance, 1e-5)
	valueTolerance := defaultTo(o.ValueTolerance, 1e7*2.220446049250313e-16)
	lineEvaluations := defaultToInt(o.MaximumLineSearchEvaluations, 20)

	search := &lineSearch{f: f, n: n, maxEvaluations: lineEvaluations, budget: o.MaximumEvaluations}
	x := clone(x0)
	value, gradient, err := search.evaluate(x)
	if err != nil {
		return Result{}, err
	}
	if math.IsInf(value, 1) || math.IsNaN(value) {
		return Result{}, fmt.Errorf("%w: the starting point has no finite value", ErrOptions)
	}
	result := Result{X: x, Value: value, Stop: StopMaximumIterations}
	var (
		sHistory, yHistory [][]float64
		rho                []float64
	)
	direction := make([]float64, n)
	alpha := make([]float64, memory)
	for iteration := 0; maxIterations < 0 || iteration < maxIterations; iteration++ {
		if maxAbs(gradient) <= gradientTolerance {
			result.Stop = StopGradientMagnitude
			break
		}
		// Two-loop recursion: direction = −H·g.
		copy(direction, gradient)
		for i := len(sHistory) - 1; i >= 0; i-- {
			alpha[i] = rho[i] * dot(sHistory[i], direction)
			axpy(-alpha[i], yHistory[i], direction)
		}
		if k := len(sHistory) - 1; k >= 0 {
			gamma := dot(sHistory[k], yHistory[k]) / dot(yHistory[k], yHistory[k])
			for i := range direction {
				direction[i] *= gamma
			}
		}
		for i := range sHistory {
			beta := rho[i] * dot(yHistory[i], direction)
			axpy(alpha[i]-beta, sHistory[i], direction)
		}
		for i := range direction {
			direction[i] = -direction[i]
		}
		slope := dot(gradient, direction)
		if !(slope < 0) {
			// Not a descent direction (rounding): restart from the gradient.
			sHistory, yHistory, rho = nil, nil, nil
			for i := range direction {
				direction[i] = -gradient[i]
			}
			slope = -dot(gradient, gradient)
		}
		step := 1.0
		if len(sHistory) == 0 {
			if o.InitialStepLength > 0 {
				step = o.InitialStepLength / maxAbs(direction)
			} else {
				step = 1 / math.Sqrt(-slope)
			}
		}
		next, nextValue, nextGradient, ok, err := search.run(x, value, slope, direction, step)
		if err != nil {
			return Result{}, err
		}
		if !ok {
			if search.exhausted() {
				result.Stop = StopMaximumEvaluations
			} else {
				result.Stop = StopLineSearch
			}
			break
		}
		s := make([]float64, n)
		y := make([]float64, n)
		for i := range s {
			s[i] = next[i] - x[i]
			y[i] = nextGradient[i] - gradient[i]
		}
		previous := value
		x, value, gradient = next, nextValue, nextGradient
		result.X, result.Value, result.Iterations = x, value, iteration+1
		if sy := dot(s, y); sy > 1e-10*math.Sqrt(dot(s, s)*dot(y, y)) {
			if len(sHistory) == memory {
				sHistory, yHistory, rho = sHistory[1:], yHistory[1:], rho[1:]
			}
			sHistory, yHistory, rho = append(sHistory, s), append(yHistory, y), append(rho, 1/sy)
		}
		if previous-value <= valueTolerance*math.Max(math.Max(math.Abs(previous), math.Abs(value)), 1) {
			result.Stop = StopValueTolerance
			break
		}
		if search.exhausted() {
			result.Stop = StopMaximumEvaluations
			break
		}
	}
	result.Evaluations = search.evaluations
	return result, nil
}

// lineSearch evaluates φ(α) = f(x + α·d) for the strong Wolfe search.
type lineSearch struct {
	f              GradientFunction
	n              int
	maxEvaluations int
	budget         int
	evaluations    int
}

func (s *lineSearch) exhausted() bool { return s.budget > 0 && s.evaluations >= s.budget }

func (s *lineSearch) evaluate(x []float64) (float64, []float64, error) {
	s.evaluations++
	value, gradient, err := s.f.ValueAndGradient(x)
	if err != nil {
		return 0, nil, err
	}
	if math.IsNaN(value) || math.IsInf(value, 1) {
		return math.Inf(1), nil, nil
	}
	if len(gradient) != s.n {
		return 0, nil, fmt.Errorf("%w: %d gradient components for %d parameters", ErrOptions, len(gradient), s.n)
	}
	return value, gradient, nil
}

// trial is one evaluated step.
type trial struct {
	step, value, slope float64
	x, gradient        []float64
}

// run searches along direction from x (value f0, slope φ′(0) < 0) starting
// with step; ok is false when no acceptable point was found within the
// evaluation limits. A point with a sufficient decrease that misses the
// curvature condition is still returned when the search runs out.
func (s *lineSearch) run(x []float64, f0, slope0 float64, direction []float64, step float64) ([]float64, float64, []float64, bool, error) {
	at := func(alpha float64) (trial, error) {
		point := make([]float64, s.n)
		for i := range point {
			point[i] = x[i] + alpha*direction[i]
		}
		value, gradient, err := s.evaluate(point)
		if err != nil {
			return trial{}, err
		}
		t := trial{step: alpha, value: value, x: point, gradient: gradient}
		if gradient != nil {
			t.slope = dot(gradient, direction)
		}
		return t, nil
	}
	sufficient := func(t trial) bool {
		return !math.IsInf(t.value, 1) && t.value <= f0+wolfeDecrease*t.step*slope0
	}
	curvature := func(t trial) bool { return math.Abs(t.slope) <= -wolfeCurvature*slope0 }
	var best *trial
	keep := func(t trial) {
		if sufficient(t) && t.value < f0 && (best == nil || t.value < best.value) {
			c := t
			best = &c
		}
	}
	finish := func(t *trial) ([]float64, float64, []float64, bool, error) {
		if t == nil {
			return nil, 0, nil, false, nil
		}
		return t.x, t.value, t.gradient, true, nil
	}
	previous := trial{step: 0, value: f0, slope: slope0}
	used := 0
	alpha := step
	for {
		if used >= s.maxEvaluations || s.exhausted() {
			return finish(best)
		}
		current, err := at(alpha)
		used++
		if err != nil {
			return nil, 0, nil, false, err
		}
		keep(current)
		if !sufficient(current) || (used > 1 && current.value >= previous.value) {
			return s.zoom(at, previous, current, f0, slope0, used, sufficient, curvature, keep, finish, &best)
		}
		if curvature(current) {
			return finish(&current)
		}
		if current.slope >= 0 {
			return s.zoom(at, current, previous, f0, slope0, used, sufficient, curvature, keep, finish, &best)
		}
		previous = current
		alpha *= 2
	}
}

// zoom narrows [lo, hi], lo the best step that satisfies the sufficient
// decrease, until a step satisfies both Wolfe conditions.
func (s *lineSearch) zoom(at func(float64) (trial, error), lo, hi trial, f0, slope0 float64, used int,
	sufficient, curvature func(trial) bool, keep func(trial),
	finish func(*trial) ([]float64, float64, []float64, bool, error), best **trial,
) ([]float64, float64, []float64, bool, error) {
	for used < s.maxEvaluations && !s.exhausted() {
		alpha := interpolateStep(lo, hi)
		current, err := at(alpha)
		used++
		if err != nil {
			return nil, 0, nil, false, err
		}
		keep(current)
		if !sufficient(current) || current.value >= lo.value {
			hi = current
			continue
		}
		if curvature(current) {
			return finish(&current)
		}
		if current.slope*(hi.step-lo.step) >= 0 {
			hi = lo
		}
		lo = current
		if math.Abs(hi.step-lo.step) <= 1e-12*math.Max(math.Abs(lo.step), 1e-300) {
			break
		}
	}
	return finish(*best)
}

// interpolateStep is the minimizer of the cubic through lo and hi (values
// and slopes), kept at least a tenth of the interval away from either end;
// bisection when hi has no finite value or the cubic has no minimizer.
func interpolateStep(lo, hi trial) float64 {
	a, b := math.Min(lo.step, hi.step), math.Max(lo.step, hi.step)
	margin := 0.1 * (b - a)
	mid := (lo.step + hi.step) / 2
	if math.IsInf(hi.value, 1) || math.IsInf(lo.value, 1) {
		return mid
	}
	d1 := lo.slope + hi.slope - 3*(lo.value-hi.value)/(lo.step-hi.step)
	radicand := d1*d1 - lo.slope*hi.slope
	if radicand < 0 {
		return mid
	}
	d2 := math.Copysign(math.Sqrt(radicand), hi.step-lo.step)
	alpha := hi.step - (hi.step-lo.step)*(hi.slope+d2-d1)/(hi.slope-lo.slope+2*d2)
	if math.IsNaN(alpha) || math.IsInf(alpha, 0) {
		return mid
	}
	return math.Min(math.Max(alpha, a+margin), b-margin)
}

func dot(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func axpy(alpha float64, x, y []float64) {
	for i := range y {
		y[i] += alpha * x[i]
	}
}

func maxAbs(v []float64) float64 {
	var m float64
	for _, x := range v {
		m = math.Max(m, math.Abs(x))
	}
	return m
}
