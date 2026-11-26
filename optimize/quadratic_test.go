package optimize

import (
	"errors"
	"fmt"
	"math"
	"testing"
)

// quadratic is the cost of ITK's Optimizersv4 tests:
// ½xᵀAx − bᵀx with A = [[3, 2], [2, 6]] and b = (2, −8), minimal at (2, −2).
type quadratic struct{}

func (quadratic) Value(x []float64) (float64, error) {
	return 0.5*(3*x[0]*x[0]+4*x[0]*x[1]+6*x[1]*x[1]) - 2*x[0] + 8*x[1], nil
}

func (q quadratic) ValueAndGradient(x []float64) (float64, []float64, error) {
	v, _ := q.Value(x)
	return v, []float64{3*x[0] + 2*x[1] - 2, 2*x[0] + 6*x[1] + 8}, nil
}

func near(t *testing.T, name string, r Result, want []float64, tolerance float64) {
	t.Helper()
	for i := range want {
		if math.Abs(r.X[i]-want[i]) > tolerance {
			t.Fatalf("%s: stopped at %v (%s after %d iterations), want %v", name, r.X, r.Stop, r.Iterations, want)
		}
	}
	t.Logf("%s: %v value %.9g, %s after %d iterations, %d evaluations", name, r.X, r.Value, r.Stop, r.Iterations, r.Evaluations)
}

func TestNelderMeadAndPowellSolveTheITKQuadratic(t *testing.T) {
	r, err := NelderMead{ParametersTolerance: 1e-8, FunctionTolerance: 1e-8, MaximumIterations: 1000}.Minimize(quadratic{}, []float64{100, -100})
	if err != nil {
		t.Fatal(err)
	}
	near(t, "nelder-mead", r, []float64{2, -2}, 1e-3)
	// ITK's itkPowellOptimizerv4Test settings.
	r, err = Powell{StepLength: 10, StepTolerance: 0.01, ValueTolerance: 0.1, MaximumIterations: 100, MaximumLineIterations: 100}.Minimize(quadratic{}, []float64{100, -100})
	if err != nil {
		t.Fatal(err)
	}
	near(t, "powell", r, []float64{2, -2}, 1e-2)
}

// TestOnePlusOneRunsTheITKCase uses the settings of ITK's
// itkOnePlusOneEvolutionaryOptimizerv4Test: radius 1.01, growth 1.05,
// shrink 1.05^(−1/4), epsilon 0.1 and 8000 iterations from (100, −100).
// With epsilon 0.1 the search stops about 0.01 from (2, −2), so whether it
// lands within ITK's 0.01 depends on the random sequence (ITK's passes with
// its generator; here seed 0 does, and about 70% of seeds). With epsilon
// 1e-3 every seed does.
func TestOnePlusOneRunsTheITKCase(t *testing.T) {
	itk := OnePlusOne{InitialRadius: 1.01, GrowthFactor: 1.05, ShrinkFactor: math.Pow(1.05, -0.25), Epsilon: 0.1, MaximumIterations: 8000}
	r, err := itk.Minimize(quadratic{}, []float64{100, -100})
	if err != nil {
		t.Fatal(err)
	}
	near(t, "one-plus-one ITK settings", r, []float64{2, -2}, 1e-2)
	for seed := int64(1); seed <= 20; seed++ {
		o := itk
		o.Epsilon, o.Seed = 1e-3, seed
		r, err := o.Minimize(quadratic{}, []float64{100, -100})
		if err != nil {
			t.Fatal(err)
		}
		near(t, fmt.Sprintf("one-plus-one seed %d", seed), r, []float64{2, -2}, 1e-2)
	}
	if _, err := (OnePlusOne{GrowthFactor: 0.9}).Minimize(quadratic{}, []float64{0, 0}); !errors.Is(err, ErrOptions) {
		t.Fatalf("growth below one: %v", err)
	}
}

// TestRegularStepGradientDescentRunsITKCases runs the settings of ITK's
// itkRegularStepGradientDescentOptimizerv4Test: from (100, −100) with
// learning rate 100 and 900 iterations the quadratic's minimum (2, −2) is
// reached within 0.01 for relaxation 0.5 and 0.8, and invalid settings are
// refused.
func TestRegularStepGradientDescentRunsITKCases(t *testing.T) {
	for _, relaxation := range []float64{0.5, 0.8} {
		o := RegularStepGradientDescent{
			LearningRate: 100, RelaxationFactor: relaxation, MinimumStepLength: 1e-6,
			GradientMagnitudeTolerance: 1e-6, MaximumIterations: 900, Scales: []float64{1, 1},
		}
		r, err := o.Minimize(quadratic{}, []float64{100, -100})
		if err != nil {
			t.Fatal(err)
		}
		near(t, fmt.Sprintf("rsgd relaxation %v", relaxation), r, []float64{2, -2}, 1e-2)
	}
	for name, o := range map[string]RegularStepGradientDescent{
		"scales of the wrong size":    {Scales: []float64{1}},
		"negative gradient tolerance": {GradientMagnitudeTolerance: -1},
		"negative relaxation":         {RelaxationFactor: -1},
		"relaxation above one":        {RelaxationFactor: 1.1},
	} {
		if _, err := o.Minimize(quadratic{}, []float64{100, -100}); !errors.Is(err, ErrOptions) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// TestWindowConvergenceMonitor: +Inf until the window fills, then positive
// for decreasing values and shrinking as they level off (the values of
// ITK's GTest, 2^−x).
func TestWindowConvergenceMonitor(t *testing.T) {
	m := NewWindowConvergenceMonitor(10)
	previous := math.Inf(1)
	for x := 0; x < 20; x++ {
		m.Add(math.Pow(2, -float64(x)))
		v := m.Value()
		if x < 9 {
			if !math.IsInf(v, 1) {
				t.Fatalf("value %v before the window fills", v)
			}
			continue
		}
		if !(v > 0) || v >= previous {
			t.Fatalf("x=%d: convergence value %v after %v", x, v, previous)
		}
		previous = v
	}
	flat := NewWindowConvergenceMonitor(5)
	for i := 0; i < 5; i++ {
		flat.Add(3)
	}
	// ITK's single-pass fit is not exact: clamping the last abscissa to
	// 1 − 1e-4 leaves a residue of about 1e-5.
	if v := flat.Value(); math.Abs(v) > 1e-4 {
		t.Fatalf("constant values converge to about 0, got %v", v)
	}
}

// piecewise is the second function of ITK's itkAmoebaOptimizerv4Test:
// x² + 4x for x < 0 (a local minimum −4 at −2) and 2x² − 8x otherwise (the
// global minimum −8 at 2).
func piecewise(x []float64) (float64, error) {
	if x[0] < 0 {
		return x[0]*x[0] + 4*x[0], nil
	}
	return 2*x[0]*x[0] - 8*x[0], nil
}

// TestNelderMeadRunsITKCases: the quadratic from (100, −100) with ITK's
// tolerances (0.01, 0.001), 10 then 100 evaluations; the piecewise function
// from −100 with a simplex delta of 10 stops at the local minimum −2, and
// with restarts reaches the global one, 2.
func TestNelderMeadRunsITKCases(t *testing.T) {
	o := NelderMead{ParametersTolerance: 0.01, FunctionTolerance: 0.001, MaximumIterations: 10}
	r, err := o.Minimize(quadratic{}, []float64{100, -100})
	if err != nil {
		t.Fatal(err)
	}
	o.MaximumIterations = 100
	if r, err = o.Minimize(quadratic{}, r.X); err != nil {
		t.Fatal(err)
	}
	near(t, "amoeba quadratic", r, []float64{2, -2}, 0.01)
	if math.Abs(r.Value+9.99998) > 0.01 {
		t.Fatalf("value %v", r.Value)
	}
	if _, err := (NelderMead{ParametersTolerance: -0.01}).Minimize(quadratic{}, []float64{0, 0}); !errors.Is(err, ErrOptions) {
		t.Fatalf("negative tolerance: %v", err)
	}
	one := NelderMead{ParametersTolerance: 0.01, FunctionTolerance: 0.001, MaximumIterations: 100, InitialSimplexDelta: []float64{10}}
	r, err = one.Minimize(Func(piecewise), []float64{-100})
	if err != nil {
		t.Fatal(err)
	}
	near(t, "amoeba local", r, []float64{-2}, 0.01)
	one.Restarts = true
	r, err = one.Minimize(Func(piecewise), []float64{-100})
	if err != nil {
		t.Fatal(err)
	}
	near(t, "amoeba restarts", r, []float64{2}, 0.01)
}
