package registration

import (
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/optimize"
	"github.com/ThalesMMS/dicom-go/render"
	"math"
	"reflect"
	"sync"
	"testing"
)

func multiImage(t *testing.T, axis int) *Image {
	t.Helper()
	d := make([]float32, 12*12)
	for y := 0; y < 12; y++ {
		for x := 0; x < 12; x++ {
			if axis == 0 {
				d[y*12+x] = float32(x)
			} else {
				d[y*12+x] = float32(y)
			}
		}
	}
	im, e := NewImage([3]int{12, 12, 1}, d, render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3())
	if e != nil {
		t.Fatal(e)
	}
	return im
}
func multiPoints() []render.Vec3 {
	return []render.Vec3{{X: 3.25, Y: 4.25}, {X: 6.25, Y: 5.25}, {X: 7.25, Y: 8.25}}
}
func multiChannel(t *testing.T, axis int, workers int) *ImageMetricComponent {
	t.Helper()
	im := multiImage(t, axis)
	m, e := NewImageMetricComponent(context.Background(), im, im, MetricDomain{Points: multiPoints(), Workers: workers}, ImageComponentOptions{FrameOfReference: "phantom", Units: "a.u.²"})
	if e != nil {
		t.Fatal(e)
	}
	return m
}
func multiCompose(t *testing.T, cs []WeightedMetric) *CompositeMetric {
	t.Helper()
	m, e := NewCompositeMetric(NewTranslation3D(), cs, MetricLimits{})
	if e != nil {
		t.Fatal(e)
	}
	return m
}
func multiNear(t *testing.T, a, b, tol float64) {
	t.Helper()
	if math.Abs(a-b) > tol {
		t.Fatalf("got %.15g want %.15g", a, b)
	}
}
func TestMultiMetricZeroUnitAndWeightedFiniteDifference(t *testing.T) {
	a, b := multiChannel(t, 0, 1), multiChannel(t, 1, 4)
	tr := NewTranslation3D()
	_ = tr.SetParameters([]float64{.2, .3, 0})
	single := multiCompose(t, []WeightedMetric{{Label: "excluded", Weight: 0}, {Label: "x", Metric: a, Weight: 1}})
	r, e := single.Evaluate(context.Background(), tr, true)
	if e != nil {
		t.Fatal(e)
	}
	v, g, e := a.ValueAndDerivative(tr)
	if e != nil {
		t.Fatal(e)
	}
	multiNear(t, r.Value, v, 1e-14)
	if !reflect.DeepEqual(g, r.Derivative) || !r.Contributions[0].Excluded {
		t.Fatal(r)
	}
	im := multiImage(t, 0)
	old, e := NewMeanSquares(im, im, MetricDomain{Points: multiPoints()})
	if e != nil {
		t.Fatal(e)
	}
	ov, og, e := old.ValueAndDerivative(tr)
	if e != nil {
		t.Fatal(e)
	}
	multiNear(t, r.Value, ov, 1e-14)
	for k := range og {
		multiNear(t, r.Derivative[k], og[k], 1e-14)
	}
	m := multiCompose(t, []WeightedMetric{{Label: "x", Metric: a, Weight: 2, Scale: 4}, {Label: "y", Metric: b, Weight: 3}})
	r, e = m.Evaluate(context.Background(), tr, true)
	if e != nil {
		t.Fatal(e)
	}
	multiNear(t, r.Value, .5*.2*.2+3*.3*.3, 1e-13)
	multiNear(t, r.Derivative[0], .2, 1e-13)
	multiNear(t, r.Derivative[1], 1.8, 1e-13)
	for k := 0; k < 2; k++ {
		lo, hi := tr.Clone(), tr.Clone()
		p := tr.Parameters()
		p[k] -= 1e-5
		_ = lo.SetParameters(p)
		p[k] += 2e-5
		_ = hi.SetParameters(p)
		lv, _ := m.Value(lo)
		hv, _ := m.Value(hi)
		multiNear(t, r.Derivative[k], (hv-lv)/2e-5, 1e-9)
	}
	if r.Contributions[0].Overlap != 3 || r.Contributions[1].FrameOfReference != "phantom" {
		t.Fatal(r)
	}
}
func TestMultiMetricDomainsNonFiniteBudgetsAndCancel(t *testing.T) {
	a := multiChannel(t, 0, 1)
	good := []WeightedMetric{{Label: "a", Metric: a, Weight: 1}}
	for _, l := range []MetricLimits{{Components: -1}, {Points: 2}, {Channels: 0, Bytes: 4}, {Parameters: 2}, {Workers: -1}} {
		if _, e := NewCompositeMetric(NewTranslation3D(), good, l); e == nil {
			t.Fatalf("accepted %+v", l)
		}
	}
	for _, c := range []WeightedMetric{{Label: "a", Metric: a, Weight: -1}, {Label: "a", Metric: a, Weight: math.NaN()}, {Label: "a", Metric: a, Weight: 1, Scale: math.Inf(1)}, {Label: "", Metric: a, Weight: 1}} {
		if _, e := NewCompositeMetric(NewTranslation3D(), []WeightedMetric{c}, MetricLimits{}); e == nil {
			t.Fatal(c)
		}
	}
	p, _ := NewPointSetMetric(context.Background(), multiPoints(), multiPoints(), "other-frame", MetricLimits{})
	if _, e := NewCompositeMetric(NewTranslation3D(), append(good, WeightedMetric{Label: "p", Metric: p, Weight: 1}), MetricLimits{}); e == nil {
		t.Fatal("frame accepted")
	}
	m := multiCompose(t, good)
	if _, e := m.Evaluate(context.Background(), NewEuler3D(), true); e == nil {
		t.Fatal("family accepted")
	}
	af := NewAffine()
	am, e := NewCompositeMetric(af, good, MetricLimits{})
	if e != nil {
		t.Fatal(e)
	}
	_ = af.SetFixedParameters([]float64{1, 0, 0})
	if _, e := am.Value(af); e == nil {
		t.Fatal("changed fixed center accepted")
	}
	tr := NewTranslation3D()
	_ = tr.SetParameters([]float64{100, 0, 0})
	r, e := m.Evaluate(context.Background(), tr, true)
	if !errors.Is(e, ErrTooFewSamples) || r.Value != 0 || r.Derivative != nil || r.Contributions[0].Error == nil {
		t.Fatal(r, e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := m.Evaluate(ctx, NewTranslation3D(), true); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	im := multiImage(t, 0)
	mask, _ := NewImage(im.size, make([]float32, len(im.data)), im.origin, im.spacing, im.direction)
	empty, e := NewImageMetricComponent(context.Background(), im, im, MetricDomain{FixedMask: mask}, ImageComponentOptions{FrameOfReference: "phantom", Units: "u"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = empty.Value(NewTranslation3D()); !errors.Is(e, ErrTooFewSamples) {
		t.Fatal(e)
	}
	im.data[5] = float32(math.NaN())
	if _, e := NewImageMetricComponent(context.Background(), im, im, MetricDomain{}, ImageComponentOptions{FrameOfReference: "p", Units: "u"}); e == nil {
		t.Fatal("nonfinite accepted")
	}
}
func TestMultiMetricDeterministicWorkersContributionsAndOwnership(t *testing.T) {
	var reference MultiMetricResult
	for _, w := range []int{1, 2, 4} {
		im := multiImage(t, 0)
		o := ImageComponentOptions{FrameOfReference: "phantom", Units: "u²", SampleCount: 77, Seed: 42}
		c, e := NewImageMetricComponent(context.Background(), im, im, MetricDomain{Points: multiPoints(), Workers: w}, o)
		if e != nil {
			t.Fatal(e)
		}
		m := multiCompose(t, []WeightedMetric{{Label: "a", Metric: c, Weight: 1}})
		tr := NewTranslation3D()
		_ = tr.SetParameters([]float64{.2, .3, 0})
		r, e := m.Evaluate(context.Background(), tr, true)
		if e != nil {
			t.Fatal(e)
		}
		if w == 1 {
			reference = r
		} else if !reflect.DeepEqual(r, reference) {
			t.Fatal("worker changed result")
		}
		im.data[0] = 999
		again, e := m.Evaluate(context.Background(), tr, true)
		if e != nil || !reflect.DeepEqual(r, again) {
			t.Fatal("borrowed image", e)
		}
		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out, e := m.Evaluate(context.Background(), tr.Clone(), true)
				if e != nil || !reflect.DeepEqual(out, r) {
					t.Error("concurrent result", e)
				}
			}()
		}
		wg.Wait()
	}
	// Sampling has a reproducible seed, and a changed seed really changes the immutable sample sequence.
	im := multiImage(t, 0)
	a, _ := NewImageMetricComponent(context.Background(), im, im, MetricDomain{}, ImageComponentOptions{FrameOfReference: "p", Units: "u", SampleCount: 20, Seed: 1})
	b, _ := NewImageMetricComponent(context.Background(), im, im, MetricDomain{}, ImageComponentOptions{FrameOfReference: "p", Units: "u", SampleCount: 20, Seed: 2})
	if reflect.DeepEqual(a.evaluator.domain.Points, b.evaluator.domain.Points) {
		t.Fatal("seed ignored")
	}
}
func TestMultiMetricComplementaryAndConflictingPhantoms(t *testing.T) {
	a, b := multiChannel(t, 0, 1), multiChannel(t, 1, 1)
	fixed := multiPoints()
	target := make([]render.Vec3, len(fixed))
	for i, p := range fixed {
		target[i] = p.Add(render.Vec3{X: 1, Y: 2})
	}
	points, e := NewPointSetMetric(context.Background(), fixed, target, "phantom", MetricLimits{})
	if e != nil {
		t.Fatal(e)
	}
	// Orthogonal intensity ramps each constrain one axis, landmarks supply both.
	// Independent quadratic: wx*x²+wy*y²+wp*((x-1)²+(y-2)²).
	for _, wp := range []float64{1, 3} {
		m := multiCompose(t, []WeightedMetric{{Label: "x", Metric: a, Weight: 1}, {Label: "y", Metric: b, Weight: 1}, {Label: "landmarks", Metric: points, Weight: wp}})
		want := []float64{wp / (1 + wp), 2 * wp / (1 + wp), 0}
		tr := NewTranslation3D()
		o := optimize.RegularStepGradientDescent{LearningRate: .2, MinimumStepLength: 1e-8, GradientMagnitudeTolerance: 1e-8, MaximumIterations: 500, Scales: []float64{1, 1, 1}}
		run, e := o.Minimize(Objective{Metric: m, Transform: tr}, []float64{0, 0, 0})
		if e != nil {
			t.Fatal(e)
		}
		for k := range want {
			multiNear(t, run.X[k], want[k], 1e-5)
		}
		_ = tr.SetParameters(want)
		r, e := m.Evaluate(context.Background(), tr, true)
		if e != nil {
			t.Fatal(e)
		}
		multiNear(t, r.Value, want[0]*want[0]+want[1]*want[1]+wp*((want[0]-1)*(want[0]-1)+(want[1]-2)*(want[1]-2)), 1e-12)
	}
	// Two orthogonal ramps with known displacement (1,2,0) are complementary:
	// x alone has no y derivative and y alone has no x derivative.
	fx, mx := multiImage(t, 0), multiImage(t, 0)
	fy, my := multiImage(t, 1), multiImage(t, 1)
	for i := range fx.data {
		fx.data[i] += 1
		fy.data[i] += 2
	}
	x, e := NewImageMetricComponent(context.Background(), fx, mx, MetricDomain{Points: multiPoints()}, ImageComponentOptions{FrameOfReference: "phantom", Units: "u²"})
	if e != nil {
		t.Fatal(e)
	}
	y, e := NewImageMetricComponent(context.Background(), fy, my, MetricDomain{Points: multiPoints()}, ImageComponentOptions{FrameOfReference: "phantom", Units: "u²"})
	if e != nil {
		t.Fatal(e)
	}
	composed := multiCompose(t, []WeightedMetric{{Label: "x", Metric: x, Weight: 1}, {Label: "y", Metric: y, Weight: 1}})
	known := NewTranslation3D()
	_ = known.SetParameters([]float64{1, 2, 0})
	r, e := composed.Evaluate(context.Background(), known, true)
	if e != nil || r.Value != 0 {
		t.Fatal(r, e)
	}
	run, e := (optimize.RegularStepGradientDescent{LearningRate: .2, MinimumStepLength: 1e-8, GradientMagnitudeTolerance: 1e-8, MaximumIterations: 500, Scales: []float64{1, 1, 1}}).Minimize(Objective{Metric: composed, Transform: NewTranslation3D()}, []float64{0, 0, 0})
	if e != nil {
		t.Fatal(e)
	}
	multiNear(t, run.X[0], 1, 1e-5)
	multiNear(t, run.X[1], 2, 1e-5)

}

// cancelTransform causally cancels during a component, not at its boundary.
type multiCancelTransform struct {
	Transform
	calls  *int
	cancel context.CancelFunc
}

func (tr *multiCancelTransform) TransformPoint(p render.Vec3) render.Vec3 {
	*tr.calls++
	if *tr.calls == 70 {
		tr.cancel()
	}
	return tr.Transform.TransformPoint(p)
}
func (tr *multiCancelTransform) Clone() Transform {
	return &multiCancelTransform{tr.Transform.Clone(), tr.calls, tr.cancel}
}
func TestMultiMetricCancelsInsideComponentsAndMethod(t *testing.T) {
	im := multiImage(t, 0)
	c, e := NewImageMetricComponent(context.Background(), im, im, MetricDomain{}, ImageComponentOptions{FrameOfReference: "p", Units: "u²"})
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	tr := &multiCancelTransform{NewTranslation3D(), &calls, cancel}
	m, e := NewCompositeMetric(tr, []WeightedMetric{{Label: "a", Metric: c, Weight: 1}}, MetricLimits{})
	if e != nil {
		t.Fatal(e)
	}
	r, e := m.Evaluate(ctx, tr, true)
	if !errors.Is(e, context.Canceled) || r.Value != 0 || r.Derivative != nil || calls < 70 || calls > 128 {
		t.Fatal(r, e, calls)
	}
	ctx, cancel = context.WithCancel(context.Background())
	calls = 0
	tr = &multiCancelTransform{NewTranslation3D(), &calls, cancel}
	objective := progressObjective{ctx: ctx, objective: Objective{Metric: m, Transform: tr}}
	if _, _, e := objective.ValueAndGradient([]float64{0, 0, 0}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	// The existing level factory consumes the composite without a new optimizer adapter.
	method := Method{Fixed: im, Moving: im, Transform: NewTranslation3D(), Levels: []Level{{Shrink: 1}}, NewMetric: func(f, mv *Image, d MetricDomain) (Metric, error) {
		c, e := NewImageMetricComponent(context.Background(), f, mv, d, ImageComponentOptions{FrameOfReference: "p", Units: "u²"})
		if e != nil {
			return nil, e
		}
		return NewCompositeMetric(NewTranslation3D(), []WeightedMetric{{Label: "image", Metric: c, Weight: 1}}, MetricLimits{})
	}, Optimizer: optimize.RegularStepGradientDescent{Scales: []float64{1, 1, 1}}}
	out, e := method.Run(context.Background())
	if e != nil || out.Value != 0 || len(out.Levels) != 1 {
		t.Fatal(out, e)
	}
}

func TestMultiMetricAggregateLimitsAndExplicitFailures(t *testing.T) {
	a, b := multiChannel(t, 0, 1), multiChannel(t, 1, 1)
	cs := []WeightedMetric{{Label: "a", Metric: a, Weight: 1}, {Label: "b", Metric: b, Weight: 1}}
	for _, l := range []MetricLimits{{Components: 1}, {Channels: 1}, {Points: 5}, {Bytes: 20000}} {
		if _, e := NewCompositeMetric(NewTranslation3D(), cs, l); e == nil {
			t.Fatalf("aggregate admitted %+v", l)
		}
	}
	if _, e := NewCompositeMetric(NewTranslation3D(), []WeightedMetric{{Label: "wrapper", Metric: multiWrappedComponent{a}, Weight: 1}}, MetricLimits{}); e == nil {
		t.Fatal("unadmitted wrapper accepted")
	}
	var nilComponent *PointSetMetric
	if _, e := NewCompositeMetric(NewTranslation3D(), []WeightedMetric{{Label: "nil", Metric: nilComponent, Weight: 1}}, MetricLimits{}); e == nil {
		t.Fatal("nil admitted")
	}
	excluded, e := NewCompositeMetric(NewTranslation3D(), []WeightedMetric{{Label: "a", Metric: a, Weight: 1}, {Label: "nil", Metric: nilComponent, Weight: 0}}, MetricLimits{Channels: 1, Points: 3})
	if e != nil {
		t.Fatal(e)
	}
	if _, e := excluded.Value(NewTranslation3D()); e != nil {
		t.Fatal(e)
	}
	overflow := multiCompose(t, []WeightedMetric{{Label: "huge", Metric: a, Weight: math.MaxFloat64, Scale: math.SmallestNonzeroFloat64}})
	tr := NewTranslation3D()
	_ = tr.SetParameters([]float64{.25, 0, 0})
	r, e := overflow.Evaluate(context.Background(), tr, true)
	if e == nil || r.Value != 0 || r.Derivative != nil || r.Contributions[0].Error == nil {
		t.Fatal(r, e)
	}
}

type multiWrappedComponent struct{ MetricComponent }

// A transform with finite parameters can still overflow or return invalid
// mapped coordinates; this must invalidate the objective, never trim overlap.
type multiNonfiniteTransform struct {
	Transform
	x float64
}

func (tr *multiNonfiniteTransform) TransformPoint(p render.Vec3) render.Vec3 {
	if p.X > 6 {
		return render.Vec3{X: tr.x}
	}
	return tr.Transform.TransformPoint(p)
}
func (tr *multiNonfiniteTransform) Clone() Transform {
	return &multiNonfiniteTransform{tr.Transform.Clone(), tr.x}
}
func TestMultiMetricRejectsNonfiniteMappedSamples(t *testing.T) {
	c := multiChannel(t, 0, 1)
	for _, x := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		tr := &multiNonfiniteTransform{NewTranslation3D(), x}
		m, e := NewCompositeMetric(tr, []WeightedMetric{{Label: "image", Metric: c, Weight: 1}}, MetricLimits{})
		if e != nil {
			t.Fatal(e)
		}
		for _, derivatives := range []bool{false, true} {
			r, e := m.Evaluate(context.Background(), tr, derivatives)
			if !errors.Is(e, ErrMultiMetric) || r.Value != 0 || r.Derivative != nil || len(r.Contributions) != 1 || r.Contributions[0].Overlap != 1 || r.Contributions[0].Error == nil {
				t.Fatal(r, e)
			}
		}
	}
	// A finite mapped point whose moving continuous index overflows is invalid too.
	fixed, moving := multiImage(t, 0), multiImage(t, 0)
	moving.physicalToIndex = Matrix3{{math.MaxFloat64, 0, 0}, {0, 1, 0}, {0, 0, 1}}
	bad, e := NewImageMetricComponent(context.Background(), fixed, moving, MetricDomain{Points: multiPoints()}, ImageComponentOptions{FrameOfReference: "p", Units: "u²"})
	if e != nil {
		t.Fatal(e)
	}
	for _, derivatives := range []bool{false, true} {
		m := multiCompose(t, []WeightedMetric{{Label: "index", Metric: bad, Weight: 1}})
		r, e := m.Evaluate(context.Background(), NewTranslation3D(), derivatives)
		if !errors.Is(e, ErrMultiMetric) || r.Value != 0 || r.Derivative != nil {
			t.Fatal(r, e)
		}
	}
}

func TestMultiMetricMaskCoordinatesBeforeIntegerSampling(t *testing.T) {
	// All geometries are made through the public constructor. Tiny but isotropic
	// spacing is well-conditioned; multiplication by a finite physical point can
	// nevertheless overflow its continuous mask index.
	mask, e := NewImage([3]int{1, 1, 1}, []float32{1}, render.Vec3{}, render.Vec3{X: 1e-10, Y: 1e-10, Z: 1e-10}, Identity3())
	if e != nil {
		t.Fatal(e)
	}
	for _, physicalX := range []float64{1e308, 1e10, -1e10} {
		im, e := NewImage([3]int{1, 1, 1}, []float32{1}, render.Vec3{X: physicalX}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3())
		if e != nil {
			t.Fatal(e)
		}
		for _, which := range []string{"fixed", "moving"} {
			d := MetricDomain{Points: []render.Vec3{{X: physicalX}}}
			if which == "fixed" {
				d.FixedMask = mask
			} else {
				d.MovingMask = mask
			}
			c, e := NewImageMetricComponent(context.Background(), im, im, d, ImageComponentOptions{FrameOfReference: "p", Units: "u²"})
			if e != nil {
				t.Fatal(e)
			}
			m := multiCompose(t, []WeightedMetric{{Label: which, Metric: c, Weight: 1}})
			want := ErrTooFewSamples
			if physicalX == 1e308 {
				want = ErrMultiMetric
			}
			for _, derivatives := range []bool{false, true} {
				r, e := m.Evaluate(context.Background(), NewTranslation3D(), derivatives)
				if !errors.Is(e, want) || r.Value != 0 || r.Derivative != nil || len(r.Contributions) != 1 || r.Contributions[0].Error == nil {
					t.Fatal(physicalX, which, derivatives, r, e)
				}
			}
		}
	}
	// Independent half-voxel nearest-mask boundaries, including the lower tie.
	one, e := NewImage([3]int{1, 1, 1}, []float32{1}, render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3())
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		x    float64
		want bool
	}{{-.5, true}, {-.5001, false}, {.4999, true}, {.5, false}, {1e20, false}, {-1e20, false}} {
		ok, e := multiMaskAt(one, render.Vec3{X: tc.x})
		if e != nil || ok != tc.want {
			t.Fatal(tc, ok, e)
		}
	}
}
