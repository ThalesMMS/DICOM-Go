package registration

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ThalesMMS/dicom-go/optimize"
	"github.com/ThalesMMS/dicom-go/render"
	"math"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

func gaussianFixture() ([]render.Vec3, []render.Vec3) {
	f := []render.Vec3{{X: 0, Y: 0, Z: 0}, {X: 3, Y: 0, Z: 0}, {X: 0, Y: 4, Z: 0}, {X: 3, Y: 4, Z: 0}, {X: 0, Y: 0, Z: 5}, {X: 3, Y: 0, Z: 5}, {X: 0, Y: 4, Z: 5}, {X: 3, Y: 4, Z: 5}}
	m := make([]render.Vec3, len(f))
	for i, p := range f {
		m[i] = p.Add(render.Vec3{X: .3, Y: -.2, Z: .1})
	}
	return f, m
}
func gaussianMetric(t testing.TB, f, m []render.Vec3, o GaussianPointSetOptions) *GaussianPointSetMetric {
	t.Helper()
	if o.FrameOfReference == "" {
		o.FrameOfReference = "phantom"
	}
	metric, e := NewGaussianPointSetMetric(context.Background(), f, m, o)
	if e != nil {
		t.Fatal(e)
	}
	return metric
}
func gaussianNear(t testing.TB, a, b, tol float64) {
	t.Helper()
	if math.Abs(a-b) > tol {
		t.Fatalf("got %.15g want %.15g", a, b)
	}
}

// Independent exhaustive reference: stable sort every eligible moving point,
// directly evaluate Gaussian exponentials for small, non-underflow fixtures.
// It shares neither the KD search nor the shifted-log numerical implementation.
func gaussianDirect(f, m []render.Vec3, tr Transform, sigma float64, k int, fl, ml []uint32) (float64, []float64) {
	sum := 0.0
	g := make([]float64, tr.NumberOfParameters())
	var j Jacobian
	for i, p := range f {
		q := tr.TransformPoint(p)
		type neighbor struct {
			index int
			d2    float64
		}
		ns := []neighbor{}
		for n, target := range m {
			if fl != nil && fl[i] != ml[n] {
				continue
			}
			d := q.Sub(target)
			ns = append(ns, neighbor{n, d.Dot(d)})
		}
		sort.Slice(ns, func(a, b int) bool { return ns[a].d2 < ns[b].d2 || ns[a].d2 == ns[b].d2 && ns[a].index < ns[b].index })
		density := 0.0
		for _, n := range ns[:k] {
			density += math.Exp(-n.d2 / (2 * sigma * sigma))
		}
		sum -= math.Log(density / float64(k))
		tr.JacobianWithRespectToParameters(p, &j)
		for _, n := range ns[:k] {
			w := math.Exp(-n.d2/(2*sigma*sigma)) / density
			d := q.Sub(m[n.index])
			for c := range g {
				g[c] += w * (d.X*j.At(0, c) + d.Y*j.At(1, c) + d.Z*j.At(2, c)) / (sigma * sigma)
			}
		}
	}
	for c := range g {
		g[c] /= float64(len(f))
	}
	return sum / float64(len(f)), g
}
func TestGaussianPointSetDirectAndFiniteDifferences(t *testing.T) {
	f, m := gaussianFixture()
	tr := NewAffine()
	params := tr.Parameters()
	params[9], params[10], params[11] = .11, -.07, .03
	_ = tr.SetParameters(params)
	for _, k := range []int{1, 3, 8} {
		metric := gaussianMetric(t, f, m, GaussianPointSetOptions{SigmaMM: 1.1, Neighbors: k})
		v, g, e := metric.ValueAndDerivative(tr)
		if e != nil {
			t.Fatal(e)
		}
		rv, rg := gaussianDirect(f, m, tr, 1.1, k, nil, nil)
		gaussianNear(t, v, rv, 1e-12)
		for i := range g {
			gaussianNear(t, g[i], rg[i], 1e-12)
		}
		only, e := metric.Value(tr)
		if e != nil {
			t.Fatal(e)
		}
		gaussianNear(t, v, only, 1e-14)
		for i := range g {
			a, b := tr.Clone(), tr.Clone()
			p := tr.Parameters()
			p[i] -= 1e-6
			_ = a.SetParameters(p)
			p[i] += 2e-6
			_ = b.SetParameters(p)
			av, e := metric.Value(a)
			if e != nil {
				t.Fatal(e)
			}
			bv, e := metric.Value(b)
			if e != nil {
				t.Fatal(e)
			}
			gaussianNear(t, g[i], (bv-av)/2e-6, 1e-7)
		}
	}
}
func TestGaussianPointSetLabelsDuplicatesAndKDTies(t *testing.T) {
	f := []render.Vec3{{}}
	m := []render.Vec3{{X: 1}, {X: -1}, {X: 1}, {X: 0, Y: 1}}
	metric := gaussianMetric(t, f, m, GaussianPointSetOptions{SigmaMM: 1, Neighbors: 2})
	visits := uint64(0)
	ns, e := metric.tree.gaussianNearest(context.Background(), render.Vec3{}, 2, nil, 0, make([]gaussianNeighbor, 0, 2), &visits, 100)
	if e != nil || ns[0].index != 0 || ns[1].index != 1 {
		t.Fatal(ns, e)
	}
	// Exact duplicate multiplicity changes density relative to two distinct centers.
	a := gaussianMetric(t, f, []render.Vec3{{}, {X: 2}}, GaussianPointSetOptions{SigmaMM: 1, Neighbors: 2})
	b := gaussianMetric(t, f, []render.Vec3{{}, {}, {X: 2}}, GaussianPointSetOptions{SigmaMM: 1, Neighbors: 3})
	av, _ := a.Value(NewTranslation3D())
	bv, _ := b.Value(NewTranslation3D())
	if bv >= av {
		t.Fatal("density multiplicity ignored", av, bv)
	}
	labels := GaussianPointSetOptions{SigmaMM: 1, Neighbors: 1, FixedLabels: []uint32{7}, MovingLabels: []uint32{8, 7, 8, 8}}
	restricted := gaussianMetric(t, f, m, labels)
	v, g, e := restricted.ValueAndDerivative(NewTranslation3D())
	if e != nil {
		t.Fatal(e)
	}
	rv, rg := gaussianDirect(f, m, NewTranslation3D(), 1, 1, labels.FixedLabels, labels.MovingLabels)
	gaussianNear(t, v, rv, 1e-14)
	for i := range g {
		gaussianNear(t, g[i], rg[i], 1e-14)
	}
	labels.FixedLabels = []uint32{99}
	if _, e := NewGaussianPointSetMetric(context.Background(), f, m, labels); e == nil {
		t.Fatal("missing label admitted")
	}
}
func TestGaussianPointSetAdmissionStableLogsAndNonfinite(t *testing.T) {
	f, m := gaussianFixture()
	base := GaussianPointSetOptions{SigmaMM: 1, Neighbors: 1, FrameOfReference: "p"}
	for _, sigma := range []float64{0, -1, math.NaN(), math.Inf(1), 1e-300, 1e300} {
		o := base
		o.SigmaMM = sigma
		if _, e := NewGaussianPointSetMetric(context.Background(), f, m, o); e == nil {
			t.Fatal(sigma)
		}
	}
	for _, o := range []GaussianPointSetOptions{{SigmaMM: 1, Neighbors: 9, FrameOfReference: "p"}, {SigmaMM: 1, Neighbors: 1, FrameOfReference: "p", Limits: MetricLimits{Points: 8}}, {SigmaMM: 1, Neighbors: 1, FrameOfReference: "p", Limits: MetricLimits{Bytes: 64}}, {SigmaMM: 1, Neighbors: 1, FrameOfReference: "p", Workers: 65}, {SigmaMM: 1, Neighbors: 1, FrameOfReference: "p", SampleCount: -1}, {SigmaMM: 1, Neighbors: 1, FrameOfReference: "p", MaxNeighbors: 1025}, {SigmaMM: 1, Neighbors: 1, FrameOfReference: "p", FixedLabels: []uint32{1}}} {
		if _, e := NewGaussianPointSetMetric(context.Background(), f, m, o); e == nil {
			t.Fatal(o)
		}
	}
	for _, cloud := range [][]render.Vec3{nil, {{X: math.NaN()}}, {{X: math.Inf(1)}}} {
		if _, e := NewGaussianPointSetMetric(context.Background(), cloud, m, base); e == nil {
			t.Fatal(cloud)
		}
	}
	stable := gaussianMetric(t, []render.Vec3{{}}, []render.Vec3{{X: 100}}, base)
	v, g, e := stable.ValueAndDerivative(NewTranslation3D())
	if e != nil {
		t.Fatal(e)
	}
	gaussianNear(t, v, 5000, 1e-12)
	gaussianNear(t, g[0], -100, 1e-12)
	overflow := gaussianMetric(t, []render.Vec3{{X: 1e308}}, []render.Vec3{{X: -1e308}}, base)
	if v, g, e := overflow.ValueAndDerivative(NewTranslation3D()); !errors.Is(e, ErrGaussianPointSet) || v != 0 || g != nil {
		t.Fatal(v, g, e)
	}
	ratio := base
	ratio.SigmaMM = 1e-150
	tiny := gaussianMetric(t, []render.Vec3{{}}, []render.Vec3{{X: 1e10}}, ratio)
	if v, g, e := tiny.ValueAndDerivative(NewTranslation3D()); !errors.Is(e, ErrGaussianPointSet) || v != 0 || g != nil {
		t.Fatal(v, g, e)
	}
}
func TestGaussianPointSetDeterminismSnapshotsAndComposite(t *testing.T) {
	f, m := gaussianFixture()
	var firstValue float64
	var firstGradient []float64
	var firstSamples []int
	for _, workers := range []int{1, 2, 4} {
		o := GaussianPointSetOptions{SigmaMM: .8, Neighbors: 3, SampleCount: 19, Seed: 42, Workers: workers}
		metric := gaussianMetric(t, f, m, o)
		v, g, e := metric.ValueAndDerivative(NewTranslation3D())
		if e != nil {
			t.Fatal(e)
		}
		if workers == 1 {
			firstValue, firstGradient, firstSamples = v, g, append([]int(nil), metric.samples...)
		} else if v != firstValue || !reflect.DeepEqual(g, firstGradient) || !reflect.DeepEqual(metric.samples, firstSamples) {
			t.Fatal("worker changed result")
		}
		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				vv, gg, e := metric.ValueAndDerivative(NewTranslation3D())
				if e != nil || vv != v || !reflect.DeepEqual(gg, g) {
					t.Error(vv, gg, e)
				}
			}()
		}
		wg.Wait()
		composed, e := NewCompositeMetric(NewTranslation3D(), []WeightedMetric{{Label: "density", Metric: metric, Weight: 1}}, MetricLimits{})
		if e != nil {
			t.Fatal(e)
		}
		r, e := composed.Evaluate(context.Background(), NewTranslation3D(), true)
		if e != nil || r.Value != v || !reflect.DeepEqual(r.Derivative, g) || r.Contributions[0].Overlap != 19 {
			t.Fatal(r, e)
		}
	}
	o := GaussianPointSetOptions{SigmaMM: 1, Neighbors: 1, SampleCount: 20, Seed: 1}
	a := gaussianMetric(t, f, m, o)
	o.Seed = 2
	b := gaussianMetric(t, f, m, o)
	if reflect.DeepEqual(a.samples, b.samples) {
		t.Fatal("seed ignored")
	}
	v, _ := a.Value(NewTranslation3D())
	f[0].X = 999
	m[0].X = -999
	vv, e := a.Value(NewTranslation3D())
	if e != nil || vv != v {
		t.Fatal("borrowed data")
	}
}

// Err checks trigger cancellation inside a build/query, without sleeps or
// goroutine-count assertions. The context is used only by one serial operation.
type gaussianCountingContext struct {
	context.Context
	calls, limit int
}

func (c *gaussianCountingContext) Err() error {
	c.calls++
	if c.calls >= c.limit {
		return context.Canceled
	}
	return nil
}
func TestGaussianPointSetCancellationAndWorkQuota(t *testing.T) {
	f, m := gaussianFixture()
	ctx := &gaussianCountingContext{Context: context.Background(), limit: 8}
	if _, e := NewGaussianPointSetMetric(ctx, f, m, GaussianPointSetOptions{SigmaMM: 1, Neighbors: 1, FrameOfReference: "p"}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	// Direct construction checks and inner neighbor traversal share the same
	// context contract; a canceled query never publishes partial neighbor results.
	metric := gaussianMetric(t, f, m, GaussianPointSetOptions{SigmaMM: 1, Neighbors: 3})
	ctx = &gaussianCountingContext{Context: context.Background(), limit: 3}
	if v, g, e := metric.ValueAndDerivativeContext(ctx, NewTranslation3D()); !errors.Is(e, context.Canceled) || v != 0 || g != nil {
		t.Fatal(v, g, e)
	}
	quota := gaussianMetric(t, []render.Vec3{{}}, m, GaussianPointSetOptions{SigmaMM: 1, Neighbors: 3, MaxNodeVisits: 1})
	if v, g, e := quota.ValueAndDerivative(NewTranslation3D()); !errors.Is(e, ErrPointSetWorkLimit) || v != 0 || g != nil {
		t.Fatal(v, g, e)
	}
	o := GaussianPointSetOptions{SigmaMM: 1, Neighbors: 1, FrameOfReference: "p", MaxNodeVisits: 1}
	if _, e := NewGaussianPointSetMetric(context.Background(), f, m, o); !errors.Is(e, ErrPointSetWorkLimit) {
		t.Fatal(e)
	}
}
func TestGaussianPointSetKnownTransformDensityPartialAndICP(t *testing.T) {
	f, m := gaussianFixture()
	cases := []struct {
		name          string
		fixed, moving []render.Vec3
	}{{"complete", f, m}, {"different-density", f, append(append([]render.Vec3(nil), m...), m...)}, {"partial", f, m[:6]}, {"fixed-outlier", append(append([]render.Vec3(nil), f...), render.Vec3{X: 10, Y: 11, Z: 12}), m}}
	for _, tc := range cases {
		for _, sigma := range []float64{.4, 1.6} {
			for _, k := range []int{1, 3} {
				t.Run(tc.name+"/"+fmtGaussian(sigma, k), func(t *testing.T) {
					metric := gaussianMetric(t, tc.fixed, tc.moving, GaussianPointSetOptions{SigmaMM: sigma, Neighbors: k})
					o := optimize.RegularStepGradientDescent{LearningRate: .2, MinimumStepLength: 1e-6, GradientMagnitudeTolerance: 1e-7, MaximumIterations: 100, Scales: []float64{1, 1, 1}}
					run, e := o.Minimize(Objective{Metric: metric, Transform: NewTranslation3D()}, []float64{0, 0, 0})
					if e != nil {
						t.Fatal(e)
					}
					tr := NewTranslation3D()
					_ = tr.SetParameters(run.X)
					knownError := tr.TransformPoint(render.Vec3{}).Sub(render.Vec3{X: .3, Y: -.2, Z: .1}).Length()
					rms := gaussianNearestRMS(tc.fixed, tc.moving, tr)
					if !finite(knownError) || !finite(rms) {
						t.Fatal(knownError, rms)
					}
					t.Logf("sigma=%g K=%d value=%g known_translation_error_mm=%g nearest_RMS_mm=%g evaluations=%d", sigma, k, run.Value, knownError, rms, run.Evaluations)
					if (tc.name == "complete" || tc.name == "different-density") && k == 1 && knownError > 1e-4 {
						t.Fatal("known translation not recovered", knownError)
					}
				})
			}
		}
		icp, e := IterativeClosestPoint(context.Background(), tc.fixed, tc.moving, ICPOptions{Initial: NewAffine(), Mode: LandmarkRigid, MaxIterations: 20})
		if e != nil {
			t.Fatal(e)
		}
		t.Logf("%s ICP RMS_mm=%g mean_mm=%g stop=%s iterations=%d", tc.name, icp.RMS, icp.MeanDistance, icp.StopReason, icp.Iterations)
	}
	// Fixed independent pointwise sensitivity, avoiding an optimizer confound.
	a := gaussianMetric(t, f, m, GaussianPointSetOptions{SigmaMM: .4, Neighbors: 1})
	b := gaussianMetric(t, f, m, GaussianPointSetOptions{SigmaMM: 1.6, Neighbors: 1})
	c := gaussianMetric(t, f, m, GaussianPointSetOptions{SigmaMM: 1.6, Neighbors: 3})
	av, _ := a.Value(NewTranslation3D())
	bv, _ := b.Value(NewTranslation3D())
	cv, _ := c.Value(NewTranslation3D())
	gaussianNear(t, av/bv, 16, 1e-10)
	if math.Abs(cv-bv) < 1e-3 {
		t.Fatal("K sensitivity absent")
	}
}
func fmtGaussian(sigma float64, k int) string {
	if sigma < 1 {
		if k == 1 {
			return "sigma0.4/K1"
		}
		return "sigma0.4/K3"
	}
	if k == 1 {
		return "sigma1.6/K1"
	}
	return "sigma1.6/K3"
}
func gaussianNearestRMS(f, m []render.Vec3, tr Transform) float64 {
	sum := 0.0
	for _, p := range f {
		best := math.Inf(1)
		q := tr.TransformPoint(p)
		for _, target := range m {
			d := q.Sub(target)
			best = math.Min(best, d.Dot(d))
		}
		sum += best
	}
	return math.Sqrt(sum / float64(len(f)))
}

// Opt-in externally produced ITK oracle. The fixture must declare the exact
// scalar normalization; ITK expectation pseudo-gradients are not equivalent.
func TestGaussianPointSetITKOracleOptIn(t *testing.T) {
	path := os.Getenv("REGISTRATION_GAUSSIAN_ITK_ORACLE")
	if path == "" {
		t.Skip("external ITK oracle not configured")
	}
	data, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var oracle struct {
		Provider, Version, Formula string
		Fixed, Moving              []render.Vec3
		SigmaMM                    float64
		Neighbors                  int
		Parameters, Gradient       []float64
		Value, Tolerance           float64
	}
	if e := json.Unmarshal(data, &oracle); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(oracle.Provider, "ITK") || oracle.Version == "" || oracle.Formula != "negative-log-mean-topk-isotropic-gaussian-v1" || oracle.Tolerance <= 0 || oracle.Tolerance > 1e-4 || !finite(oracle.Tolerance) {
		t.Fatal("oracle contract missing")
	}
	metric := gaussianMetric(t, oracle.Fixed, oracle.Moving, GaussianPointSetOptions{SigmaMM: oracle.SigmaMM, Neighbors: oracle.Neighbors})
	tr := NewTranslation3D()
	if e := tr.SetParameters(oracle.Parameters); e != nil {
		t.Fatal(e)
	}
	v, g, e := metric.ValueAndDerivative(tr)
	if e != nil {
		t.Fatal(e)
	}
	if len(g) != len(oracle.Gradient) || !finite(oracle.Value) || !finiteSlice(oracle.Gradient) {
		t.Fatal("invalid expected objective")
	}
	gaussianNear(t, v, oracle.Value, oracle.Tolerance)
	for i := range g {
		gaussianNear(t, g[i], oracle.Gradient[i], oracle.Tolerance)
	}
}

func BenchmarkGaussianPointSetBuildAndEvaluate(b *testing.B) {
	f, m := gaussianFixture()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		metric := gaussianMetric(b, f, m, GaussianPointSetOptions{SigmaMM: 1, Neighbors: 3})
		if _, _, e := metric.ValueAndDerivative(NewTranslation3D()); e != nil {
			b.Fatal(e)
		}
	}
}
func BenchmarkGaussianPointSetRegistrationVersusICP(b *testing.B) {
	f, m := gaussianFixture()
	b.Run("Gaussian-translation-sigma1-K1", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			metric := gaussianMetric(b, f, m, GaussianPointSetOptions{SigmaMM: 1, Neighbors: 1})
			_, e := (optimize.RegularStepGradientDescent{LearningRate: .2, MinimumStepLength: 1e-6, MaximumIterations: 100, Scales: []float64{1, 1, 1}}).Minimize(Objective{Metric: metric, Transform: NewTranslation3D()}, []float64{0, 0, 0})
			if e != nil {
				b.Fatal(e)
			}
		}
	})
	b.Run("ICP-rigid", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, e := IterativeClosestPoint(context.Background(), f, m, ICPOptions{Initial: NewAffine(), Mode: LandmarkRigid, MaxIterations: 20}); e != nil {
				b.Fatal(e)
			}
		}
	})
}
