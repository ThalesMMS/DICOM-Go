package optimize

import (
	"context"
	"errors"
	"math"
	"testing"
)

// rosenbrock is the n-dimensional Rosenbrock function, minimal (0) at all
// ones.
type rosenbrock struct{ evaluations *int }

func (r rosenbrock) Value(x []float64) (float64, error) {
	v, _, err := r.ValueAndGradient(x)
	return v, err
}

func (r rosenbrock) ValueAndGradient(x []float64) (float64, []float64, error) {
	if r.evaluations != nil {
		*r.evaluations++
	}
	var v float64
	g := make([]float64, len(x))
	for i := 0; i+1 < len(x); i++ {
		a, b := 1-x[i], x[i+1]-x[i]*x[i]
		v += a*a + 100*b*b
		g[i] += -2*a - 400*x[i]*b
		g[i+1] += 200 * b
	}
	return v, g, nil
}

func TestLBFGSSolvesTheITKQuadratic(t *testing.T) {
	r, err := LBFGS{GradientTolerance: 1e-10}.Minimize(quadratic{}, []float64{100, -100})
	if err != nil {
		t.Fatal(err)
	}
	near(t, "lbfgs", r, []float64{2, -2}, 1e-8)
	if r.Iterations > 10 {
		t.Fatalf("%d iterations on a 2-D quadratic", r.Iterations)
	}
}

func TestLBFGSSolvesRosenbrock(t *testing.T) {
	x0 := make([]float64, 20)
	for i := range x0 {
		x0[i] = -1.2
		if i%2 == 1 {
			x0[i] = 1
		}
	}
	var evaluations int
	r, err := LBFGS{GradientTolerance: 1e-9, MaximumIterations: 1000, ValueTolerance: 1e-300}.Minimize(rosenbrock{&evaluations}, x0)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]float64, 20)
	for i := range want {
		want[i] = 1
	}
	near(t, "rosenbrock", r, want, 1e-6)
	if r.Evaluations != evaluations {
		t.Fatalf("reported %d evaluations, made %d", r.Evaluations, evaluations)
	}
}

func TestLBFGSStopsOnBudgetsAndTolerances(t *testing.T) {
	x0 := []float64{-1.2, 1}
	r, err := LBFGS{MaximumIterations: 3}.Minimize(rosenbrock{}, x0)
	if err != nil || r.Stop != StopMaximumIterations || r.Iterations != 3 {
		t.Fatalf("iterations: %+v %v", r, err)
	}
	r, err = LBFGS{MaximumEvaluations: 5, MaximumIterations: -1}.Minimize(rosenbrock{}, x0)
	if err != nil || r.Stop != StopMaximumEvaluations || r.Evaluations > 5 {
		t.Fatalf("evaluations: %+v %v", r, err)
	}
	r, err = LBFGS{GradientTolerance: 1e6}.Minimize(rosenbrock{}, x0)
	if err != nil || r.Stop != StopGradientMagnitude || r.Iterations != 0 || r.X[0] != -1.2 {
		t.Fatalf("gradient: %+v %v", r, err)
	}
	if _, err := (LBFGS{Memory: -1}).Minimize(rosenbrock{}, x0); !errors.Is(err, ErrOptions) {
		t.Fatalf("negative memory: %v", err)
	}
	if _, err := (LBFGS{}).Minimize(rosenbrock{}, nil); !errors.Is(err, ErrOptions) {
		t.Fatalf("no parameters: %v", err)
	}
}

// domainLimited is a quadratic undefined (+Inf) beyond |x| > 3: the line
// search must shorten steps that leave the domain.
type domainLimited struct{}

func (domainLimited) Value(x []float64) (float64, error) { return 0, nil }

func (domainLimited) ValueAndGradient(x []float64) (float64, []float64, error) {
	if math.Abs(x[0]) > 3 {
		return math.Inf(1), nil, nil
	}
	return (x[0] - 2) * (x[0] - 2), []float64{2 * (x[0] - 2)}, nil
}

func TestLBFGSShortensStepsOutsideTheDomain(t *testing.T) {
	r, err := LBFGS{InitialStepLength: 50, GradientTolerance: 1e-9}.Minimize(domainLimited{}, []float64{-2})
	if err != nil {
		t.Fatal(err)
	}
	near(t, "domain", r, []float64{2}, 1e-6)
}

type failing struct{ after, calls int }

func (f *failing) Value([]float64) (float64, error) { return 0, nil }

func (f *failing) ValueAndGradient(x []float64) (float64, []float64, error) {
	f.calls++
	if f.calls > f.after {
		return 0, nil, context.Canceled
	}
	return x[0] * x[0], []float64{2 * x[0]}, nil
}

func TestLBFGSReturnsFunctionErrors(t *testing.T) {
	if _, err := (LBFGS{}).Minimize(&failing{after: 2}, []float64{5}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error: %v", err)
	}
}

func BenchmarkLBFGSRosenbrock100(b *testing.B) {
	x0 := make([]float64, 100)
	for i := range x0 {
		x0[i] = -1.2
	}
	for i := 0; i < b.N; i++ {
		if _, err := (LBFGS{MaximumIterations: 2000, GradientTolerance: 1e-8}).Minimize(rosenbrock{}, x0); err != nil {
			b.Fatal(err)
		}
	}
}
