package registration

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

func neighborhoodTestImage(t testing.TB, size [3]int, spacing render.Vec3, fn func(int, int, int) float32) *Image {
	t.Helper()
	data := make([]float32, size[0]*size[1]*size[2])
	for z := 0; z < size[2]; z++ {
		for y := 0; y < size[1]; y++ {
			for x := 0; x < size[0]; x++ {
				data[(z*size[1]+y)*size[0]+x] = fn(x, y, z)
			}
		}
	}
	im, err := NewImage(size, data, render.Vec3{}, spacing, Identity3())
	if err != nil {
		t.Fatal(err)
	}
	return im
}

func TestNeighborhoodCorrelationAnalyticalClippedEdges(t *testing.T) {
	f := neighborhoodTestImage(t, [3]int{3, 1, 1}, render.Vec3{X: 1, Y: 1, Z: 1}, func(x, y, z int) float32 { return []float32{1, 2, 4}[x] })
	m := neighborhoodTestImage(t, [3]int{3, 1, 1}, f.spacing, func(x, y, z int) float32 { return []float32{1, 3, 2}[x] })
	ncc, err := NewNeighborhoodCorrelation(f, m, MetricDomain{}, NeighborhoodCorrelationOptions{Radius: [3]int{1, 0, 0}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ncc.Value(NewTranslation3D())
	if err != nil || math.Abs(got+59.0/84) > 1e-14 {
		t.Fatalf("local clipped correlation=%g/%v want -59/84", got, err)
	}
	global, _ := NewCorrelation(f, m, MetricDomain{})
	v, _ := global.Value(NewTranslation3D())
	if math.Abs(v+3.0/28) > 1e-14 || math.Abs(got-v) < .5 {
		t.Fatal("local metric collapsed to global correlation")
	}
}

func TestNeighborhoodCorrelationPhysicalRadiusAndFullDerivative(t *testing.T) {
	f := neighborhoodTestImage(t, [3]int{9, 9, 9}, render.Vec3{X: .7, Y: 1.1, Z: 1.6}, func(x, y, z int) float32 {
		return float32(math.Sin(.4*float64(x)) + .2*float64(y*y) + .13*float64(z*z))
	})
	m := neighborhoodTestImage(t, f.size, f.spacing, func(x, y, z int) float32 {
		return float32(math.Cos(.3*float64(x)) + .17*float64(y*y) + .09*float64(z*z) + .02*float64(x*y))
	})
	angle := math.Pi / 7
	direction := Matrix3{{math.Cos(angle), -math.Sin(angle), 0}, {math.Sin(angle), math.Cos(angle), 0}, {0, 0, 1}}
	f, _ = NewImage(f.size, f.data, render.Vec3{X: 2, Y: -3, Z: 4}, f.spacing, direction)
	m, _ = NewImage(m.size, m.data, f.origin, m.spacing, direction)
	points := []render.Vec3{f.Point(render.Vec3{X: 3, Y: 3, Z: 3}), f.Point(render.Vec3{X: 5, Y: 4, Z: 5})}
	c, err := NewNeighborhoodCorrelation(f, m, MetricDomain{Points: points}, NeighborhoodCorrelationOptions{RadiusMM: [3]float64{1.5, 2.3, 3.3}})
	if err != nil {
		t.Fatal(err)
	}
	if c.radius != [3]int{2, 2, 2} {
		t.Fatal(c.radius)
	}
	tr := NewTranslation3D()
	_ = tr.SetParameters([]float64{.31, .27, .19})
	v, g, err := c.ValueAndDerivative(tr)
	if err != nil {
		t.Fatal(err)
	}
	only, err := c.Value(tr)
	if err != nil || only != v {
		t.Fatal("value/derivative objective mismatch", only, v, err)
	}
	const h = 1e-5
	for p := range g {
		a, b := tr.Clone(), tr.Clone()
		ap, bp := a.Parameters(), b.Parameters()
		ap[p] += h
		bp[p] -= h
		_ = a.SetParameters(ap)
		_ = b.SetParameters(bp)
		plus, e1 := c.Value(a)
		minus, e2 := c.Value(b)
		numeric := (plus - minus) / (2 * h)
		if e1 != nil || e2 != nil || math.Abs(g[p]-numeric) > 1e-7 {
			t.Fatalf("physical derivative %d=%g numerical=%g errors=%v/%v", p, g[p], numeric, e1, e2)
		}
	}
	points[0] = render.Vec3{X: math.NaN()}
	if again, err := c.Value(tr); err != nil || again != v {
		t.Fatal("sparse centers were not captured", again, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			again, der, err := c.ValueAndDerivative(tr)
			if err != nil || again != v {
				t.Error(again, err)
			}
			for p := range der {
				if der[p] != g[p] {
					t.Error("concurrent derivative changed")
				}
			}
		}()
	}
	wg.Wait()
}

func TestNeighborhoodCorrelationMasksConstantsAndNonfinite(t *testing.T) {
	f := neighborhoodTestImage(t, [3]int{5, 4, 1}, render.Vec3{X: 1, Y: 1, Z: 1}, func(x, y, z int) float32 { return float32(x*x + 3*y) })
	m := neighborhoodTestImage(t, f.size, f.spacing, func(x, y, z int) float32 { return 2*f.At(x, y, z) + 100000 })
	opts := NeighborhoodCorrelationOptions{Radius: [3]int{1, 1, 0}}
	c, err := NewNeighborhoodCorrelation(f, m, MetricDomain{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := c.Value(NewTranslation3D()); err != nil || math.Abs(v+1) > 1e-12 {
		t.Fatal("large-offset linear relation", v, err)
	}
	constant := neighborhoodTestImage(t, f.size, f.spacing, func(x, y, z int) float32 { return 3 })
	empty := neighborhoodTestImage(t, f.size, f.spacing, func(x, y, z int) float32 { return 0 })
	for _, tc := range []struct {
		moving    *Image
		domain    MetricDomain
		transform Transform
	}{
		{constant, MetricDomain{}, NewTranslation3D()},
		{m, MetricDomain{FixedMask: empty}, NewTranslation3D()},
		{m, MetricDomain{MovingMask: empty}, NewTranslation3D()},
		{m, MetricDomain{Points: []render.Vec3{}}, NewTranslation3D()},
	} {
		metric, err := NewNeighborhoodCorrelation(f, tc.moving, tc.domain, opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := metric.Value(tc.transform); !errors.Is(err, ErrTooFewSamples) {
			t.Fatal("undefined correlation accepted", err)
		}
	}
	far := NewTranslation3D()
	_ = far.SetParameters([]float64{100, 0, 0})
	if _, err := c.Value(far); !errors.Is(err, ErrTooFewSamples) {
		t.Fatal("no-overlap", err)
	}
	for _, bad := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
		m.data[6] = bad
		if _, err := c.Value(NewTranslation3D()); !errors.Is(err, ErrNeighborhoodCorrelation) {
			t.Fatal("nonfinite pair", err)
		}
	}
	m.data[6] = 2*f.data[6] + 100000
	mask := neighborhoodTestImage(t, f.size, f.spacing, func(x, y, z int) float32 {
		if x > 0 && x < 4 {
			return 1
		}
		return 0
	})
	masked, err := NewNeighborhoodCorrelation(f, m, MetricDomain{FixedMask: mask, MovingMask: mask}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := masked.Value(NewTranslation3D()); err != nil || math.Abs(v+1) > 1e-12 {
		t.Fatal("pair masks", v, err)
	}
	mask.data[6] = float32(math.NaN())
	if _, err := masked.Value(NewTranslation3D()); !errors.Is(err, ErrNeighborhoodCorrelation) {
		t.Fatal("nonfinite mask", err)
	}
}

type neighborhoodCancelTransform struct {
	Transform
	cancel    context.CancelFunc
	remaining int
}

func (t *neighborhoodCancelTransform) TransformPoint(p render.Vec3) render.Vec3 {
	t.remaining--
	if t.remaining == 0 {
		t.cancel()
	}
	return t.Transform.TransformPoint(p)
}

func TestNeighborhoodCorrelationAdmissionAndCancellation(t *testing.T) {
	f := neighborhoodTestImage(t, [3]int{8, 8, 8}, render.Vec3{X: 1, Y: 1, Z: 1}, func(x, y, z int) float32 { return float32(x*x + y*y + z) })
	for _, o := range []NeighborhoodCorrelationOptions{
		{}, {Radius: [3]int{-1, 1, 1}}, {Radius: [3]int{1, 1, 1}, RadiusMM: [3]float64{1, 1, 1}}, {RadiusMM: [3]float64{math.NaN(), 1, 1}}, {Radius: [3]int{1, 1, 1}, MinimumVariance: -1},
	} {
		if _, err := NewNeighborhoodCorrelation(f, f, MetricDomain{}, o); !errors.Is(err, ErrNeighborhoodCorrelation) {
			t.Fatal("bad option", o, err)
		}
	}
	for _, o := range []NeighborhoodCorrelationOptions{
		{Radius: [3]int{1, 1, 1}, MaxCenters: 1}, {Radius: [3]int{3, 3, 3}, MaxNeighborhoodSamples: 5}, {Radius: [3]int{1, 1, 1}, MaxWorkingBytes: 32}, {Radius: [3]int{1, 1, 1}, MaxWork: 1}, {Radius: [3]int{int(^uint(0) >> 1), 1, 1}},
	} {
		if _, err := NewNeighborhoodCorrelation(f, f, MetricDomain{}, o); !errors.Is(err, ErrNeighborhoodCorrelationLimit) {
			t.Fatal("limit", o, err)
		}
	}
	c, err := NewNeighborhoodCorrelation(f, f, MetricDomain{}, NeighborhoodCorrelationOptions{Radius: [3]int{1, 1, 1}, MaxWork: 512 * 27})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.ValueAndDerivative(NewTranslation3D()); !errors.Is(err, ErrNeighborhoodCorrelationLimit) {
		t.Fatal("derivative work not admitted", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.ValueContext(ctx, NewTranslation3D()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	c, err = NewNeighborhoodCorrelation(f, f, MetricDomain{}, NeighborhoodCorrelationOptions{Radius: [3]int{1, 1, 1}})
	if err != nil {
		t.Fatal(err)
	}
	var missing *Translation3D
	if _, err := c.Value(missing); !errors.Is(err, ErrNeighborhoodCorrelation) {
		t.Fatal("typed nil transform", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	tr := &neighborhoodCancelTransform{Transform: NewTranslation3D(), cancel: cancel, remaining: 10}
	if v, g, err := c.ValueAndDerivativeContext(ctx, tr); v != 0 || g != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("partial publication on cancel=%g/%v/%v", v, g, err)
	}
}
