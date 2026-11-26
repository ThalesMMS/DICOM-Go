package roi

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
)

func probabilityFixture(values []float64) ScalarClassificationInput {
	return ScalarClassificationInput{Dimensions: [3]int{len(values), 1, 1}, SpacingMM: [3]float64{1, 1, 1}, Values: values, Units: "HU", Normalization: "none"}
}
func probabilityOptions() ProbabilisticOptions {
	return ProbabilisticOptions{Classes: 2, Units: "HU", Normalization: "none"}
}
func probabilityNear(t *testing.T, a, b, tolerance float64) {
	t.Helper()
	if math.Abs(a-b) > tolerance || math.IsNaN(a) {
		t.Fatalf("got %.17g want %.17g tolerance%g", a, b, tolerance)
	}
}

// Independent direct density oracle uses neither solver's log-domain code nor EM.
func probabilityDensity(x, mean, variance float64) float64 {
	return math.Exp(-(x-mean)*(x-mean)/(2*variance)) / math.Sqrt(2*math.Pi*variance)
}

func TestGaussianBayesDirectLikelihoodPosterior(t *testing.T) {
	in := probabilityFixture([]float64{-1, 0, 1})
	o := probabilityOptions()
	model := []GaussianClass{{-1, 1, .25}, {1, 4, .75}}
	r, err := ClassifyGaussianBayes(context.Background(), in, model, o)
	if err != nil {
		t.Fatal(err)
	}
	ll := 0.0
	for i, x := range in.Values {
		a := .25 * probabilityDensity(x, -1, 1)
		b := .75 * probabilityDensity(x, 1, 4)
		ll += math.Log(a + b)
		probabilityNear(t, r.Posteriors[i*2], a/(a+b), 1e-14)
		probabilityNear(t, r.Posteriors[i*2]+r.Posteriors[i*2+1], 1, 1e-14)
	}
	probabilityNear(t, r.LogLikelihood, ll, 1e-14)
}
func TestGaussianMixtureKnownParametersAndStopping(t *testing.T) {
	in := probabilityFixture([]float64{-5, -3, -5, -3, 3, 5, 3, 5})
	o := probabilityOptions()
	o.Initial = []GaussianClass{{-4, 1, .5}, {4, 1, .5}}
	r, err := FitGaussianMixture(context.Background(), in, o)
	if err != nil {
		t.Fatal(err)
	}
	for j, mean := range []float64{-4, 4} {
		probabilityNear(t, r.Classes[j].Mean, mean, 1e-8)
		probabilityNear(t, r.Classes[j].Variance, 1, 1e-8)
		probabilityNear(t, r.Classes[j].Weight, .5, 1e-10)
	}
	if !r.EMConverged || r.EMIterations < 1 || r.EMIterations >= 100 {
		t.Fatalf("stopping %+v", r)
	}
	o.EMIterations = 1
	r, err = FitGaussianMixture(context.Background(), in, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.EMConverged || r.EMIterations != 1 {
		t.Fatal("unconverged budget must be explicit")
	}
}
func TestGaussianMixtureSeedAndClassPermutation(t *testing.T) {
	in := probabilityFixture([]float64{-5, -3, -5, -3, 3, 5, 3, 5})
	o := probabilityOptions()
	o.Seed = 17
	a, err := FitGaussianMixture(context.Background(), in, o)
	if err != nil {
		t.Fatal(err)
	}
	b, err := FitGaussianMixture(context.Background(), in, o)
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("non deterministic seed", err)
	}
	model := []GaussianClass{{1, 2, .8}, {-1, 1, .2}}
	a, err = ClassifyGaussianBayes(context.Background(), in, model, o)
	if err != nil {
		t.Fatal(err)
	}
	b, err = ClassifyGaussianBayes(context.Background(), in, []GaussianClass{model[1], model[0]}, o)
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("class permutation", err)
	}
}
func TestGaussianBayesPriorsZeroAndNormalization(t *testing.T) {
	in := probabilityFixture([]float64{0, 0})
	o := probabilityOptions()
	model := []GaussianClass{{-1, 1, .5}, {1, 1, .5}}
	o.Priors = []float64{2, 8}
	r, err := ClassifyGaussianBayes(context.Background(), in, model, o)
	if err != nil {
		t.Fatal(err)
	}
	probabilityNear(t, r.Posteriors[0], .2, 1e-14)
	o.Priors = []float64{0, 1}
	r, err = ClassifyGaussianBayes(context.Background(), in, model, o)
	if err != nil || r.Posteriors[0] != 0 || r.Labels[0] != 2 {
		t.Fatal("zero prior", err)
	}
	o.Priors = []float64{0, 0}
	if _, err = ClassifyGaussianBayes(context.Background(), in, model, o); !errors.Is(err, ErrProbabilisticInput) {
		t.Fatal(err)
	}
}
func TestGaussianMixtureConstantSingularAndEmptyPolicies(t *testing.T) {
	in := probabilityFixture([]float64{7, 7, 7})
	o := probabilityOptions()
	o.Classes = 1
	o.VarianceFloor = .01
	r, err := FitGaussianMixture(context.Background(), in, o)
	if err != nil {
		t.Fatal(err)
	}
	probabilityNear(t, r.Classes[0].Mean, 7, 0)
	probabilityNear(t, r.Classes[0].Variance, .01, 0)
	o.Classes = 2
	if r, err = FitGaussianMixture(context.Background(), in, o); r != nil || !errors.Is(err, ErrProbabilisticEmptyClass) {
		t.Fatal("constant must not invent two identifiable classes", err)
	}
	o.Classes = 1
	r, err = ClassifyGaussianBayes(context.Background(), in, []GaussianClass{{7, 0, 1}}, o)
	if err != nil || r.Classes[0].Variance != .01 {
		t.Fatal("singular covariance floor", err)
	}
	o.Classes = 2
	o.Initial = []GaussianClass{{7, 1, 1}, {1e10, 1, 1}}
	if r, err = FitGaussianMixture(context.Background(), in, o); r != nil || !errors.Is(err, ErrProbabilisticEmptyClass) {
		t.Fatal("empty responsibility mass", err)
	}
}
func TestGaussianBayesNonFiniteMaskAndCalibration(t *testing.T) {
	in := probabilityFixture([]float64{-1, math.NaN(), math.Inf(1), math.Inf(-1), 1})
	o := probabilityOptions()
	model := []GaussianClass{{-1, 1, .5}, {1, 1, .5}}
	if r, e := ClassifyGaussianBayes(context.Background(), in, model, o); r != nil || !errors.Is(e, ErrProbabilisticInput) {
		t.Fatal(e)
	}
	o.SkipNonFinite = true
	r, e := ClassifyGaussianBayes(context.Background(), in, model, o)
	if e != nil || r.SkippedNonFinite != 3 || r.SelectedVoxels != 2 {
		t.Fatal(e)
	}
	for _, i := range []int{1, 2, 3} {
		if r.Labels[i] != 0 || r.Posteriors[i*2] != 0 || r.Posteriors[i*2+1] != 0 {
			t.Fatal("excluded voxel")
		}
	}
	for _, change := range []func(*ProbabilisticOptions){func(x *ProbabilisticOptions) { x.Units = "SUV" }, func(x *ProbabilisticOptions) { x.Normalization = "zscore" }} {
		bad := o
		change(&bad)
		if _, e = ClassifyGaussianBayes(context.Background(), in, model, bad); !errors.Is(e, ErrProbabilisticInput) {
			t.Fatal("calibration mismatch", e)
		}
	}
	in.Mask = []bool{true, false, false, false, true}
	o.SkipNonFinite = false
	if _, e = ClassifyGaussianBayes(context.Background(), in, model, o); e != nil {
		t.Fatal("masked nonfinite should not enter model", e)
	}
}
func TestGaussianMRFIndependentPhysicalStep(t *testing.T) {
	in := probabilityFixture([]float64{-1, 0, -1})
	model := []GaussianClass{{-1, 1, .5}, {1, 1, .5}}
	o := probabilityOptions()
	o.MRF = MRFOptions{Beta: 1, Iterations: 1, Damping: 1, Tolerance: 1e-12}
	neighbor := probabilityDensity(-1, -1, 1) / (probabilityDensity(-1, -1, 1) + probabilityDensity(-1, 1, 1))
	for _, spacing := range []float64{1, 4} {
		in.SpacingMM[0] = spacing
		r, e := ClassifyGaussianBayes(context.Background(), in, model, o)
		if e != nil {
			t.Fatal(e)
		}
		expected := 1 / (1 + math.Exp(-2*(2*neighbor-1)/spacing))
		probabilityNear(t, r.Posteriors[2], expected, 1e-14)
		if r.MRFIterations != 1 || r.MRFConverged {
			t.Fatal("finite budget/stopping")
		}
	}
	in.SpacingMM[0] = 1
	o.MRF.ContrastSigma = 1
	r, e := ClassifyGaussianBayes(context.Background(), in, model, o)
	if e != nil {
		t.Fatal(e)
	}
	probabilityNear(t, r.Posteriors[2], 1/(1+math.Exp(-2*(2*neighbor-1)*math.Exp(-.5))), 1e-14)
}
func TestGaussianMRFMaskBoundaryAndZeroPrior(t *testing.T) {
	in := probabilityFixture([]float64{-1, 0, 1})
	in.Mask = []bool{false, true, false}
	o := probabilityOptions()
	o.MRF = MRFOptions{Beta: 100, Iterations: 3}
	r, e := ClassifyGaussianBayes(context.Background(), in, []GaussianClass{{-1, 1, .5}, {1, 1, .5}}, o)
	if e != nil {
		t.Fatal(e)
	}
	probabilityNear(t, r.Posteriors[2], .5, 0)
	if r.Labels[0] != 0 || r.Labels[2] != 0 {
		t.Fatal("mask edges")
	}
	o.Priors = []float64{0, 1}
	r, e = ClassifyGaussianBayes(context.Background(), in, []GaussianClass{{-1, 1, .5}, {1, 1, .5}}, o)
	if e != nil || r.Posteriors[2] != 0 || r.Posteriors[3] != 1 {
		t.Fatal("MRF must preserve zero prior", e)
	}
}
func TestGaussianClassificationAdmissionBeforeAllocation(t *testing.T) {
	in := probabilityFixture([]float64{0, 1})
	o := probabilityOptions()
	model := []GaussianClass{{0, 1, .5}, {1, 1, .5}}
	for _, limits := range []ProbabilisticLimits{{MaxWorkingBytes: 1}, {MaxVoxels: 1}, {MaxClasses: 1}, {MaxIterations: 1}} {
		bad := o
		bad.Limits = limits
		if r, e := ClassifyGaussianBayes(context.Background(), in, model, bad); r != nil || !errors.Is(e, ErrProbabilisticLimit) {
			t.Fatalf("%+v %v", limits, e)
		}
	}
	in.Dimensions = [3]int{math.MaxInt, math.MaxInt, 2}
	if _, e := ClassifyGaussianBayes(context.Background(), in, model, o); !errors.Is(e, ErrProbabilisticLimit) {
		t.Fatal("dimension overflow", e)
	}
}
func TestGaussianClassificationDetachedOwnership(t *testing.T) {
	in := probabilityFixture([]float64{-1, 0, 1})
	in.Mask = []bool{true, true, true}
	before := append([]float64(nil), in.Values...)
	mask := append([]bool(nil), in.Mask...)
	o := probabilityOptions()
	o.Initial = []GaussianClass{{-1, 1, .5}, {1, 1, .5}}
	o.Priors = []float64{.25, .75}
	model := append([]GaussianClass(nil), o.Initial...)
	r, e := ClassifyGaussianBayes(context.Background(), in, model, o)
	if e != nil {
		t.Fatal(e)
	}
	r.Classes[0].Mean = 99
	r.Priors[0] = 99
	r.Posteriors[0] = 99
	r.Labels[0] = 99
	if !reflect.DeepEqual(in.Values, before) || !reflect.DeepEqual(in.Mask, mask) || !reflect.DeepEqual(o.Initial, model) || o.Priors[0] != .25 {
		t.Fatal("input alias")
	}
	if r.SourceSHA256 == "" || r.ParametersSHA256 == "" {
		t.Fatal("missing provenance")
	}
}

// This context observes a bounded number of checkpoints, cancelling real work
// after admission/validation rather than relying on wall-clock scheduling.
type probabilityCancellingContext struct {
	context.Context
	calls  int
	cancel context.CancelFunc
}

func (c *probabilityCancellingContext) Err() error {
	c.calls++
	if c.calls == 12 {
		c.cancel()
	}
	return c.Context.Err()
}
func TestGaussianClassificationCancellationNoPartialResult(t *testing.T) {
	values := make([]float64, 1024)
	for i := range values {
		values[i] = float64(i % 17)
	}
	in := probabilityFixture(values)
	o := probabilityOptions()
	o.Initial = []GaussianClass{{0, 2, .5}, {16, 2, .5}}
	o.MRF = MRFOptions{Beta: 1, Iterations: 20}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &probabilityCancellingContext{Context: ctx, cancel: cancel}
	if r, e := FitGaussianMixture(c, in, o); r != nil || !errors.Is(e, context.Canceled) {
		t.Fatal("partial result on actual cancelled work", e)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if r, e := ClassifyGaussianBayes(ctx, in, o.Initial, o); r != nil || !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
func TestGaussianClassificationConcurrentIndependentCalls(t *testing.T) {
	in := probabilityFixture([]float64{-5, -3, -5, -3, 3, 5, 3, 5})
	o := probabilityOptions()
	o.Seed = 4
	var wg sync.WaitGroup
	results := make([]*ProbabilisticResult, 4)
	errs := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i], errs[i] = FitGaussianMixture(context.Background(), in, o) }(i)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || !reflect.DeepEqual(results[i], results[0]) {
			t.Fatal("concurrent call mismatch", errs[i])
		}
	}
}

func TestGaussianMRFAxisPermutationAndCancellation(t *testing.T) {
	model := []GaussianClass{{-1, 1, .5}, {1, 1, .5}}
	o := probabilityOptions()
	o.MRF = MRFOptions{Beta: 1, Iterations: 1, Damping: 1}
	var expected float64
	for axis := 0; axis < 3; axis++ {
		in := probabilityFixture([]float64{-1, 0, -1})
		in.Dimensions = [3]int{1, 1, 1}
		in.Dimensions[axis] = 3
		in.SpacingMM[axis] = 4
		r, e := ClassifyGaussianBayes(context.Background(), in, model, o)
		if e != nil {
			t.Fatal(e)
		}
		if axis == 0 {
			expected = r.Posteriors[2]
		} else {
			probabilityNear(t, r.Posteriors[2], expected, 0)
		}
	}
	values := make([]float64, 1024)
	in := probabilityFixture(values)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &probabilityCancellingContext{Context: ctx, cancel: cancel}
	if r, e := ClassifyGaussianBayes(c, in, model, o); r != nil || !errors.Is(e, context.Canceled) {
		t.Fatal("no partial MRF posterior", e)
	}
}

func TestGaussianMRFRecoversPositivePriorUnderflow(t *testing.T) {
	// Independent log-odds: center Gaussian evidence is -40^2/2=-800.
	// Two neighbors favor class0 with probability rounding to1; each contributes
	// +1000. The synchronous first-step center log-odds is1200, hence posterior1.
	in := probabilityFixture([]float64{-40, 0, -40})
	model := []GaussianClass{{-40, 1, .5}, {0, 1, .5}}
	o := probabilityOptions()
	unary, e := ClassifyGaussianBayes(context.Background(), in, model, o)
	if e != nil {
		t.Fatal(e)
	}
	if unary.Posteriors[2] != 0 {
		t.Fatal("fixture did not exercise public float underflow")
	}
	o.MRF = MRFOptions{Beta: 1000, Iterations: 1, Damping: 1, Tolerance: 1e-12}
	// Admission still covers exactly three dense float64 arrays (output, log unary,
	// next), not four. A limit one byte below the conservative charge refuses.
	bytes := int64(3)*(2*24+3) + 2*1024 + 16384
	o.Limits.MaxWorkingBytes = bytes - 1
	if r, e := ClassifyGaussianBayes(context.Background(), in, model, o); r != nil || !errors.Is(e, ErrProbabilisticLimit) {
		t.Fatal("log scratch must be admitted", e)
	}
	o.Limits.MaxWorkingBytes = bytes
	r, e := ClassifyGaussianBayes(context.Background(), in, model, o)
	if e != nil {
		t.Fatal(e)
	}
	expected := 1 / (1 + math.Exp(-(-800.0 + 2000.0)))
	probabilityNear(t, r.Posteriors[2], expected, 0)
	if r.Labels[1] != 1 || r.WorkingBytes != bytes {
		t.Fatal("positive prior cannot become impossible", r)
	}
	o.Priors = []float64{0, 1}
	r, e = ClassifyGaussianBayes(context.Background(), in, model, o)
	if e != nil || r.Posteriors[2] != 0 || r.Labels[1] != 2 {
		t.Fatal("actual zero prior must remain impossible", e)
	}
}
