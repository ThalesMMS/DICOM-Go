package optimize

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
)

// ErrSearchBudget reports finite search admission or evaluation exhaustion.
var ErrSearchBudget = errors.New("optimize: search budget exceeded")

// ErrNoFiniteCandidate reports a search with no available finite value.
var ErrNoFiniteCandidate = errors.New("optimize: no finite search candidate")

const (
	StopExhaustiveComplete StopReason = "exhaustive-complete"
	StopMultiStartComplete StopReason = "multi-start-complete"
)

// SearchLimits bound owned logical workspace and calls, not caller allocations
// or process RSS. Zero fields select finite defaults; negative fields fail.
type SearchLimits struct {
	MaxParameters     int   // default 4096
	MaxEvaluations    int   // default 100000, including rejected/unavailable trials
	MaxStarts         int   // default 4096
	MaxWorkspaceBytes int64 // default 64 MiB
}

func (l SearchLimits) normalized() (SearchLimits, error) {
	if l.MaxParameters < 0 || l.MaxEvaluations < 0 || l.MaxStarts < 0 || l.MaxWorkspaceBytes < 0 {
		return l, fmt.Errorf("%w: negative search limit", ErrOptions)
	}
	if l.MaxParameters == 0 {
		l.MaxParameters = 4096
	}
	if l.MaxEvaluations == 0 {
		l.MaxEvaluations = 100000
	}
	if l.MaxStarts == 0 {
		l.MaxStarts = 4096
	}
	if l.MaxWorkspaceBytes == 0 {
		l.MaxWorkspaceBytes = 64 << 20
	}
	return l, nil
}

func searchAdmission(l SearchLimits, n, starts int) (SearchLimits, error) {
	l, err := l.normalized()
	if err != nil {
		return l, err
	}
	if n < 1 || starts < 1 {
		return l, fmt.Errorf("%w: empty search", ErrOptions)
	}
	if n > l.MaxParameters || starts > l.MaxStarts {
		return l, ErrSearchBudget
	}
	// Six reusable vectors, owned start snapshots, copied gradients/results,
	// and conservative header/loop allowance. No Cartesian grid is allocated.
	const allowance int64 = 8192
	if int64(n) > (math.MaxInt64-allowance)/96 {
		return l, ErrSearchBudget
	}
	bytes := allowance + 96*int64(n)
	if int64(n) > (math.MaxInt64-24)/8 {
		return l, ErrSearchBudget
	}
	perStart := 8*int64(n) + 24
	if int64(starts) > (math.MaxInt64-bytes)/perStart {
		return l, ErrSearchBudget
	}
	bytes += perStart * int64(starts)
	if bytes > l.MaxWorkspaceBytes {
		return l, ErrSearchBudget
	}
	return l, nil
}

func searchFinite(x []float64) bool {
	for _, v := range x {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

func searchCopy(x []float64) []float64 { y := make([]float64, len(x)); copy(y, x); return y }

type searchFunction struct {
	ctx               context.Context
	f                 Function
	n, calls, maximum int
}

func newSearchFunction(ctx context.Context, f Function, n, maximum int) (*searchFunction, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f == nil {
		return nil, fmt.Errorf("%w: nil function", ErrOptions)
	}
	v := reflect.ValueOf(f)
	switch v.Kind() {
	case reflect.Func, reflect.Ptr, reflect.Interface:
		if v.IsNil() {
			return nil, fmt.Errorf("%w: nil function", ErrOptions)
		}
	}
	return &searchFunction{ctx: ctx, f: f, n: n, maximum: maximum}, nil
}

func (f *searchFunction) before(x []float64) error {
	if err := f.ctx.Err(); err != nil {
		return err
	}
	if len(x) != f.n || !searchFinite(x) {
		return fmt.Errorf("%w: invalid search parameters", ErrOptions)
	}
	if f.calls >= f.maximum {
		return ErrSearchBudget
	}
	f.calls++
	return nil
}

func (f *searchFunction) Value(x []float64) (float64, error) {
	if err := f.before(x); err != nil {
		return 0, err
	}
	v, err := f.f.Value(x)
	if ctxErr := f.ctx.Err(); ctxErr != nil {
		return 0, ctxErr
	}
	if err != nil {
		return 0, err
	}
	if math.IsNaN(v) || math.IsInf(v, -1) {
		return 0, fmt.Errorf("%w: invalid search value", ErrOptions)
	}
	return v, nil // +Inf declares an unavailable trial.
}

func (f *searchFunction) ValueAndGradient(x []float64) (float64, []float64, error) {
	g, ok := f.f.(GradientFunction)
	if !ok {
		return 0, nil, fmt.Errorf("%w: function has no gradient", ErrOptions)
	}
	if err := f.before(x); err != nil {
		return 0, nil, err
	}
	v, gradient, err := g.ValueAndGradient(x)
	if ctxErr := f.ctx.Err(); ctxErr != nil {
		return 0, nil, ctxErr
	}
	if err != nil {
		return 0, nil, err
	}
	if math.IsNaN(v) || math.IsInf(v, -1) {
		return 0, nil, fmt.Errorf("%w: invalid search value", ErrOptions)
	}
	if math.IsInf(v, 1) {
		return v, nil, nil
	}
	if len(gradient) != f.n || !searchFinite(gradient) {
		return 0, nil, fmt.Errorf("%w: invalid search gradient", ErrOptions)
	}
	return v, searchCopy(gradient), nil
}
