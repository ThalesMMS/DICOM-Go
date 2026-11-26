package optimize

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestExhaustiveIndependentGridAndOwnership(t *testing.T) {
	start, steps, sizes := []float64{2, -1}, []int{2, 1}, []float64{.5, 2}
	var visited [][]float64
	f := Func(func(x []float64) (float64, error) {
		visited = append(visited, searchCopy(x))
		return math.Pow(x[0]-1.5, 2) + math.Pow(x[1]-1, 2), nil
	})
	run, err := (Exhaustive{Steps: steps, StepSizes: sizes}).Minimize(f, start)
	if err != nil || !reflect.DeepEqual(run.X, []float64{1.5, 1}) || run.Value != 0 || run.Evaluations != 15 || run.Stop != StopExhaustiveComplete {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	var expected [][]float64
	for y := -1; y <= 1; y++ {
		for x := -2; x <= 2; x++ {
			expected = append(expected, []float64{2 + .5*float64(x), -1 + 2*float64(y)})
		}
	}
	if !reflect.DeepEqual(visited, expected) {
		t.Fatal("grid order/extent disagrees with independent nested loops")
	}
	run.X[0] = 99
	if !reflect.DeepEqual(start, []float64{2, -1}) || !reflect.DeepEqual(steps, []int{2, 1}) || !reflect.DeepEqual(sizes, []float64{.5, 2}) {
		t.Fatal("search changed options/start")
	}
	// Snapshot every axis before the first callback, even if a caller changes
	// its now-unborrowed option slices from that callback.
	changed := false
	f = Func(func(x []float64) (float64, error) {
		if !changed {
			changed = true
			start[0] = 900
			steps[0] = 0
			sizes[0] = 100
		}
		return x[0]*x[0] + x[1]*x[1], nil
	})
	run, err = (Exhaustive{Steps: steps, StepSizes: sizes}).Minimize(f, start)
	if err != nil || run.Evaluations != 15 || run.X[0] != 1 {
		t.Fatalf("options not snapshotted: %+v/%v", run, err)
	}
}

func TestExhaustiveAdmissionCancellationAndUnavailable(t *testing.T) {
	called := 0
	f := Func(func(x []float64) (float64, error) { called++; return 0, nil })
	for _, o := range []Exhaustive{
		{Steps: []int{2}, StepSizes: []float64{1}, Limits: SearchLimits{MaxEvaluations: 4}},
		{Steps: []int{1}, StepSizes: []float64{1}, Limits: SearchLimits{MaxWorkspaceBytes: 1}},
		{Steps: []int{math.MaxInt}, StepSizes: []float64{1}},
		{Steps: []int{-1}, StepSizes: []float64{1}},
		{Steps: []int{1}, StepSizes: []float64{math.Inf(1)}},
		{Steps: []int{1}, StepSizes: []float64{1}, Limits: SearchLimits{MaxStarts: -1}},
	} {
		if run, err := o.Minimize(f, []float64{0}); err == nil || len(run.X) != 0 {
			t.Fatalf("admitted invalid budget/options %+v", o)
		}
	}
	if called != 0 {
		t.Fatal("callback executed before admission")
	}
	ctx, cancel := context.WithCancel(context.Background())
	f = Func(func([]float64) (float64, error) { cancel(); return 1, nil })
	if run, err := (Exhaustive{Steps: []int{2}, StepSizes: []float64{1}}).MinimizeContext(ctx, f, []float64{0}); !errors.Is(err, context.Canceled) || len(run.X) != 0 {
		t.Fatalf("cancel=%+v/%v", run, err)
	}
	f = Func(func(x []float64) (float64, error) {
		if x[0] < 0 {
			return math.Inf(1), nil
		}
		return x[0], nil
	})
	if run, err := (Exhaustive{Steps: []int{2}, StepSizes: []float64{1}}).Minimize(f, []float64{0}); err != nil || run.X[0] != 0 {
		t.Fatalf("unavailable=%+v/%v", run, err)
	}
	for _, value := range []float64{math.NaN(), math.Inf(-1), math.Inf(1)} {
		f = Func(func([]float64) (float64, error) { return value, nil })
		if run, err := (Exhaustive{Steps: []int{0}, StepSizes: []float64{1}}).Minimize(f, []float64{0}); err == nil || len(run.X) != 0 {
			t.Fatal("nonfinite-only search returned a result")
		}
	}
}

type reusedSearchGradient struct{ gradient []float64 }

func (f *reusedSearchGradient) Value(x []float64) (float64, error) {
	return (x[0] - 2) * (x[0] - 2), nil
}
func (f *reusedSearchGradient) ValueAndGradient(x []float64) (float64, []float64, error) {
	f.gradient[0] = 2 * (x[0] - 2)
	v, _ := f.Value(x)
	return v, f.gradient, nil
}

func TestMultiStartLocalVerificationBudgetAndGradientOwnership(t *testing.T) {
	starts := [][]float64{{-4}, {7}, {1}}
	f := &reusedSearchGradient{gradient: make([]float64, 1)}
	local := func(ctx context.Context, fn Function, start []float64) (Result, error) {
		g := fn.(GradientFunction)
		_, first, err := g.ValueAndGradient(start)
		if err != nil {
			return Result{}, err
		}
		want := first[0]
		if _, _, err = g.ValueAndGradient([]float64{2}); err != nil {
			return Result{}, err
		}
		if first[0] != want {
			t.Fatal("borrowed reused gradient escaped search wrapper")
		}
		return Result{X: []float64{2}, Value: -999}, nil // reported value must not be trusted
	}
	run, err := (MultiStart{Starts: starts, Local: local}).Minimize(f)
	if err != nil || run.Value != 0 || run.X[0] != 2 || run.Evaluations != 12 || run.Stop != StopMultiStartComplete {
		t.Fatalf("run=%+v/%v", run, err)
	}
	run.X[0] = 123
	if starts[0][0] != -4 || starts[1][0] != 7 {
		t.Fatal("start/result ownership")
	}
	if run, err := (MultiStart{Starts: starts, Local: local, Limits: SearchLimits{MaxEvaluations: 5}}).Minimize(f); !errors.Is(err, ErrSearchBudget) || len(run.X) != 0 {
		t.Fatalf("global local budget=%+v/%v", run, err)
	}
	bad := func(context.Context, Function, []float64) (Result, error) {
		return Result{X: []float64{math.NaN()}}, nil
	}
	if run, err := (MultiStart{Starts: starts, Local: bad}).Minimize(f); err == nil || len(run.X) != 0 {
		t.Fatal("invalid local result accepted")
	}
}

func TestMultiStartSnapshotsAndSerialCancellation(t *testing.T) {
	starts := [][]float64{{4}, {3}, {2}}
	calls := 0
	f := Func(func(x []float64) (float64, error) {
		calls++
		if calls == 1 {
			starts[2][0] = 100
		}
		return x[0] * x[0], nil
	})
	run, err := (MultiStart{Starts: starts}).Minimize(f)
	if err != nil || run.X[0] != 2 || calls != 3 {
		t.Fatalf("detached starts=%+v/%v", run, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f = Func(func([]float64) (float64, error) { cancel(); return 0, nil })
	if run, err := (MultiStart{Starts: starts}).MinimizeContext(ctx, f); !errors.Is(err, context.Canceled) || len(run.X) != 0 {
		t.Fatalf("cancel=%+v/%v", run, err)
	}
	calls = 0
	if _, err = (MultiStart{Starts: [][]float64{{1}, {1, 2}}}).Minimize(Func(func([]float64) (float64, error) { calls++; return 0, nil })); err == nil || calls != 0 {
		t.Fatal("dimensions not admitted before callback")
	}
}
