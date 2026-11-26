package optimize

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
)

// StopProjectedGradient reports that ||x-P(x-g)|| infinity is within tolerance.
const StopProjectedGradient StopReason = "projected-gradient"

// ErrLBFGSBResourceLimit reports rejected parameter or owned-workspace admission.
var ErrLBFGSBResourceLimit = errors.New("optimize: L-BFGS-B resource limit")

// LBFGSBLimits bounds buffers owned by one run, excluding caller callbacks and
// runtime overhead. Zero fields select finite defaults; this is not an RSS cap.
type LBFGSBLimits struct {
	MaxParameters     int
	MaxWorkspaceBytes int64
}

// LBFGSB minimizes a differentiable function inside a coordinate box using a
// generalized Cauchy point and compact limited-memory BFGS subspace model
// (Byrd, Lu, Nocedal and Zhu, 1995). It is independent of the unconstrained LBFGS.
// Bounds and the initial vector are copied. A finite starting vector outside
// the box is projected before its first evaluation. Equal bounds fix a variable.
// Callbacks must not mutate or retain the borrowed trial vector; their gradient
// buffer may be reused, and is copied before another evaluation.
type LBFGSB struct {
	LowerBounds, UpperBounds     []float64
	Memory                       int     // zero: 5; admitted range 1..64
	MaximumIterations            int     // zero: 100; negative is invalid
	MaximumEvaluations           int     // zero: 2000; includes rejected trials and initial call
	MaximumLineSearchEvaluations int     // zero: 20
	ProjectedGradientTolerance   float64 // zero: 1e-5
	ValueTolerance               float64 // zero: 1e7 float64 epsilons
	InitialStepLength            float64 // zero: first direction's Euclidean length one
	Limits                       LBFGSBLimits
}

type lbfgsbConfig struct {
	n, memory, iterations, evaluations, lineEvaluations int
	gradientTolerance, valueTolerance, initialStep      float64
	bytes                                               int64
}

// WorkspaceBytes admits N and reports a conservative peak of owned numeric,
// index, breakpoint and small-matrix buffers. It allocates no run workspace and
// makes no function calls. Its count includes the independently owned Result.X.
func (o LBFGSB) WorkspaceBytes(n int) (int64, error) {
	c, err := o.resolve(context.Background(), n)
	return c.bytes, err
}

func (o LBFGSB) resolve(ctx context.Context, n int) (lbfgsbConfig, error) {
	var c lbfgsbConfig
	if n <= 0 || o.Memory < 0 || o.Memory > 64 || o.MaximumIterations < 0 || o.MaximumEvaluations < 0 || o.MaximumLineSearchEvaluations < 0 || o.Limits.MaxParameters < 0 || o.Limits.MaxWorkspaceBytes < 0 {
		return c, fmt.Errorf("%w: L-BFGS-B dimensions or counts", ErrOptions)
	}
	for _, value := range []float64{o.ProjectedGradientTolerance, o.ValueTolerance, o.InitialStepLength} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return c, fmt.Errorf("%w: L-BFGS-B scalar", ErrOptions)
		}
	}
	maxN := defaultToInt(o.Limits.MaxParameters, 1<<20)
	if n > maxN {
		return c, ErrLBFGSBResourceLimit
	}
	if o.LowerBounds != nil && len(o.LowerBounds) != n || o.UpperBounds != nil && len(o.UpperBounds) != n {
		return c, fmt.Errorf("%w: L-BFGS-B bound length", ErrOptions)
	}
	for i := 0; (o.LowerBounds != nil || o.UpperBounds != nil) && i < n; i++ {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return c, err
			}
		}
		lo, hi := math.Inf(-1), math.Inf(1)
		if o.LowerBounds != nil {
			lo = o.LowerBounds[i]
		}
		if o.UpperBounds != nil {
			hi = o.UpperBounds[i]
		}
		if math.IsNaN(lo) || math.IsNaN(hi) || math.IsInf(lo, 1) || math.IsInf(hi, -1) || lo > hi {
			return c, fmt.Errorf("%w: L-BFGS-B bounds", ErrOptions)
		}
	}
	c = lbfgsbConfig{n: n, memory: defaultToInt(o.Memory, 5), iterations: defaultToInt(o.MaximumIterations, 100), evaluations: defaultToInt(o.MaximumEvaluations, 2000), lineEvaluations: defaultToInt(o.MaximumLineSearchEvaluations, 20), gradientTolerance: defaultTo(o.ProjectedGradientTolerance, 1e-5), valueTolerance: defaultTo(o.ValueTolerance, 1e7*2.220446049250313e-16), initialStep: o.InitialStepLength}
	factor := int64(16*c.memory + 192)
	tail := int64(256*c.memory*c.memory + 512*c.memory + 8192)
	if int64(n) > (math.MaxInt64-tail)/factor {
		return lbfgsbConfig{}, ErrLBFGSBResourceLimit
	}
	c.bytes = factor*int64(n) + tail
	maximum := o.Limits.MaxWorkspaceBytes
	if maximum == 0 {
		maximum = 512 << 20
	}
	if c.bytes > maximum || uint64(n) > uint64(^uint(0)>>1)/uint64(2*c.memory+16) {
		return lbfgsbConfig{}, ErrLBFGSBResourceLimit
	}
	return c, nil
}

// Minimize runs with a background context.
func (o LBFGSB) Minimize(f GradientFunction, x0 []float64) (Result, error) {
	return o.MinimizeContext(context.Background(), f, x0)
}

// MinimizeContext checks cancellation in numerical work and around callbacks.
// It cannot interrupt a blocking callback. Callback/cancellation errors return
// zero Result. A +Inf trial is outside the function domain; NaN/-Inf are errors.
func (o LBFGSB) MinimizeContext(ctx context.Context, f GradientFunction, x0 []float64) (Result, error) {
	if ctx == nil || f == nil {
		return Result{}, fmt.Errorf("%w: L-BFGS-B context/function", ErrOptions)
	}
	vf := reflect.ValueOf(f)
	if (vf.Kind() == reflect.Ptr || vf.Kind() == reflect.Func || vf.Kind() == reflect.Interface) && vf.IsNil() {
		return Result{}, fmt.Errorf("%w: nil L-BFGS-B function", ErrOptions)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	c, err := o.resolve(ctx, len(x0))
	if err != nil {
		return Result{}, err
	}
	// Validate before the first workspace allocation.
	for i, v := range x0 {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return Result{}, fmt.Errorf("%w: non-finite L-BFGS-B start", ErrOptions)
		}
	}
	arena := make([]float64, (2*c.memory+16)*c.n)
	take := func() []float64 { v := arena[:c.n:c.n]; arena = arena[c.n:]; return v }
	model := newLBFGSBModel(ctx, c.n, c.memory, arena[:2*c.memory*c.n])
	arena = arena[2*c.memory*c.n:]
	x, g, lo, hi := take(), take(), take(), take()
	xc, d, z, bz, step, change := take(), take(), take(), take(), take(), take()
	tx, tg, bx, bg := take(), take(), take(), take()
	_ = take()
	_ = take()
	for i, v := range x0 {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
		}
		lo[i], hi[i] = math.Inf(-1), math.Inf(1)
		if o.LowerBounds != nil {
			lo[i] = o.LowerBounds[i]
		}
		if o.UpperBounds != nil {
			hi[i] = o.UpperBounds[i]
		}
		x[i] = lbfgsbClamp(v, lo[i], hi[i])
	}
	search := lbfgsbSearch{ctx: ctx, f: f, config: c, lo: lo, hi: hi, trialX: tx, trialG: tg, bestX: bx, bestG: bg}
	value, err := search.evaluate(x, g)
	if err != nil {
		return Result{}, err
	}
	if math.IsInf(value, 1) {
		return Result{}, fmt.Errorf("%w: L-BFGS-B start has no finite value", ErrOptions)
	}
	active := make([]bool, c.n)
	breaks := make([]lbfgsbBreakpoint, 0, c.n)
	result := Result{Value: value, Stop: StopMaximumIterations}
	for iteration := 0; iteration < c.iterations; iteration++ {
		residual, err := lbfgsbProjectedNorm(ctx, x, g, lo, hi)
		if err != nil {
			return Result{}, err
		}
		if residual <= c.gradientTolerance {
			result.Stop = StopProjectedGradient
			break
		}
		if search.exhausted() {
			result.Stop = StopMaximumEvaluations
			break
		}
		err = model.build()
		if err == nil {
			err = model.cauchy(x, g, lo, hi, xc, d, active, breaks)
		}
		if err == nil {
			err = model.subspace(x, g, lo, hi, xc, z, bz, active)
		}
		if errors.Is(err, errLBFGSBModel) {
			model.reset()
			if err = model.build(); err == nil {
				err = model.cauchy(x, g, lo, hi, xc, d, active, breaks)
			}
			if err == nil {
				err = model.subspace(x, g, lo, hi, xc, z, bz, active)
			}
		}
		if err != nil {
			return Result{}, err
		}
		for i := range d {
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return Result{}, err
				}
			}
			d[i] = xc[i] - x[i]
		}
		slope, err := lbfgsbDot(ctx, g, d)
		if err != nil {
			return Result{}, err
		}
		if !(slope < 0) {
			result.Stop = StopLineSearch
			break
		}
		initial := 1.0
		if model.count == 0 {
			if c.initialStep > 0 {
				initial = math.Min(1, c.initialStep/maxAbs(d))
			} else {
				norm, err := lbfgsbDot(ctx, d, d)
				if err != nil {
					return Result{}, err
				}
				initial = math.Min(1, 1/math.Sqrt(norm))
			}
		}
		next, ok, err := search.run(x, value, slope, d, initial)
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
		for i := range step {
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return Result{}, err
				}
			}
			step[i] = bx[i] - x[i]
			change[i] = bg[i] - g[i]
		}
		if err := model.add(step, change); err != nil {
			return Result{}, err
		}
		previous := value
		value = next
		copy(x, bx)
		copy(g, bg)
		result.Iterations = iteration + 1
		result.Value = value
		residual, err = lbfgsbProjectedNorm(ctx, x, g, lo, hi)
		if err != nil {
			return Result{}, err
		}
		if residual <= c.gradientTolerance {
			result.Stop = StopProjectedGradient
			break
		}
		if previous-value <= c.valueTolerance*math.Max(math.Max(math.Abs(previous), math.Abs(value)), 1) {
			result.Stop = StopValueTolerance
			break
		}
		if search.exhausted() {
			result.Stop = StopMaximumEvaluations
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	result.X = clone(x)
	result.Evaluations = search.evaluations
	return result, nil
}

func lbfgsbClamp(x, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, x)) }
func lbfgsbProjectedNorm(ctx context.Context, x, g, lo, hi []float64) (float64, error) {
	var out float64
	for i := range x {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		// Equivalent to x-P(x-g), without cancellation when a small gradient
		// is subtracted from a large physical parameter coordinate.
		v := 0.0
		if g[i] > 0 {
			v = math.Min(g[i], x[i]-lo[i])
		} else if g[i] < 0 {
			v = math.Min(-g[i], hi[i]-x[i])
		}
		out = math.Max(out, v)
	}
	return out, nil
}
func lbfgsbDot(ctx context.Context, a, b []float64) (float64, error) {
	var v float64
	for i := range a {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		v += a[i] * b[i]
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, errLBFGSBModel
	}
	return v, nil
}
