package optimize

import (
	"context"
	"fmt"
	"math"
)

// Exhaustive visits the Cartesian grid x0[i]+k*StepSizes[i],
// k=-Steps[i]..Steps[i], with parameter zero varying fastest. Steps zero fixes
// that coordinate. StepSizes are in the parameter's own units, not scales.
// Equal values select the first point. Callbacks borrow trial vectors and must
// neither retain nor mutate them. The whole finite grid is admitted before any
// callback; points are generated lazily with bounded workspace.
type Exhaustive struct {
	Steps     []int
	StepSizes []float64
	Limits    SearchLimits
}

func (o Exhaustive) Minimize(f Function, x0 []float64) (Result, error) {
	return o.MinimizeContext(context.Background(), f, x0)
}

func (o Exhaustive) MinimizeContext(ctx context.Context, f Function, x0 []float64) (Result, error) {
	n := len(x0)
	l, err := searchAdmission(o.Limits, n, 1)
	if err != nil {
		return Result{}, err
	}
	if len(o.Steps) != n || len(o.StepSizes) != n || !searchFinite(x0) {
		return Result{}, fmt.Errorf("%w: exhaustive dimensions", ErrOptions)
	}
	count := 1
	for i, s := range o.Steps {
		step := o.StepSizes[i]
		if s < 0 || s > (math.MaxInt-1)/2 || !(step > 0) || math.IsInf(step, 0) || math.IsNaN(step) {
			return Result{}, fmt.Errorf("%w: exhaustive axis", ErrOptions)
		}
		width := 2*s + 1
		if count > l.MaxEvaluations/width {
			return Result{}, ErrSearchBudget
		}
		count *= width
		offset := float64(s) * step
		if math.IsInf(offset, 0) || !searchFinite([]float64{x0[i] - offset, x0[i] + offset}) {
			return Result{}, fmt.Errorf("%w: exhaustive endpoint overflow", ErrOptions)
		}
	}
	c, err := newSearchFunction(ctx, f, n, l.MaxEvaluations)
	if err != nil {
		return Result{}, err
	}
	start, steps, sizes := searchCopy(x0), append([]int(nil), o.Steps...), searchCopy(o.StepSizes)
	trial, best := make([]float64, n), make([]float64, n)
	value := math.Inf(1)
	for ordinal := 0; ordinal < count; ordinal++ {
		at := ordinal
		for i, s := range steps {
			width := 2*s + 1
			trial[i] = start[i] + float64(at%width-s)*sizes[i]
			at /= width
		}
		v, e := c.Value(trial)
		if e != nil {
			return Result{}, e
		}
		if v < value {
			value = v
			copy(best, trial)
		}
	}
	if math.IsInf(value, 1) {
		return Result{}, ErrNoFiniteCandidate
	}
	return Result{X: best, Value: value, Iterations: count, Evaluations: c.calls, Stop: StopExhaustiveComplete}, nil
}
