package optimize

import (
	"context"
	"fmt"
	"math"
)

// LocalMinimizer runs serially against a budgeted Function. Gradient functions
// remain available through a GradientFunction assertion. It must use the passed
// function for every evaluation, honor errors, and not retain borrowed starts.
// Arbitrary callback code cannot be forcibly stopped or memory-accounted here.
type LocalMinimizer func(context.Context, Function, []float64) (Result, error)

// MultiStart evaluates every owned start, optionally refining it. It verifies
// returned positions by evaluating the objective, rather than trusting a local
// optimizer's reported value. All starts share one finite evaluation budget.
// Local work is serial because registration objectives mutate a transform.
type MultiStart struct {
	Starts [][]float64
	Local  LocalMinimizer
	Limits SearchLimits
}

func (o MultiStart) Minimize(f Function) (Result, error) {
	return o.MinimizeContext(context.Background(), f)
}

func (o MultiStart) MinimizeContext(ctx context.Context, f Function) (Result, error) {
	if len(o.Starts) == 0 {
		return Result{}, fmt.Errorf("%w: no starts", ErrOptions)
	}
	n := len(o.Starts[0])
	l, err := searchAdmission(o.Limits, n, len(o.Starts))
	if err != nil {
		return Result{}, err
	}
	if len(o.Starts) > l.MaxEvaluations {
		return Result{}, ErrSearchBudget
	}
	for _, x := range o.Starts {
		if len(x) != n || !searchFinite(x) {
			return Result{}, fmt.Errorf("%w: inconsistent starts", ErrOptions)
		}
	}
	c, err := newSearchFunction(ctx, f, n, l.MaxEvaluations)
	if err != nil {
		return Result{}, err
	}
	starts := make([][]float64, len(o.Starts))
	for i, x := range o.Starts {
		if err := c.ctx.Err(); err != nil {
			return Result{}, err
		}
		starts[i] = searchCopy(x)
	}
	best := make([]float64, n)
	value := math.Inf(1)
	for _, start := range starts {
		v, e := c.Value(start)
		if e != nil {
			return Result{}, e
		}
		if v < value {
			value = v
			copy(best, start)
		}
		if o.Local == nil || math.IsInf(v, 1) {
			continue
		}
		run, e := o.Local(c.ctx, c, start)
		if ctxErr := c.ctx.Err(); ctxErr != nil {
			return Result{}, ctxErr
		}
		if e != nil {
			return Result{}, e
		}
		// Return metadata cannot hide an out-of-domain/wrong-length result.
		v, e = c.Value(run.X)
		if e != nil {
			return Result{}, e
		}
		if v < value {
			value = v
			copy(best, run.X)
		}
	}
	if math.IsInf(value, 1) {
		return Result{}, ErrNoFiniteCandidate
	}
	return Result{X: best, Value: value, Iterations: len(starts), Evaluations: c.calls, Stop: StopMultiStartComplete}, nil
}
