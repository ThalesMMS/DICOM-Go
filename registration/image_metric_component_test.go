package registration

import (
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/render"
	"math"
	"testing"
)

func TestImageMetricComponentCorrelationAndAdmission(t *testing.T) {
	f, m := itkGaussians(t)
	points := []render.Vec3{{X: 70.31, Y: 50.27}, {X: 80.21, Y: 60.31}, {X: 90.27, Y: 70.11}, {X: 100.17, Y: 80.19}}
	d := MetricDomain{Points: points}
	c, e := NewImageMetricComponent(context.Background(), f, m, d, ImageComponentOptions{Kind: ImageCorrelation, FrameOfReference: "p", Units: "dimensionless"})
	if e != nil {
		t.Fatal(e)
	}
	old, _ := NewCorrelation(f, m, d)
	tr := NewTranslation3D()
	_ = tr.SetParameters([]float64{.23, .19, 0})
	v, g, e := c.ValueAndDerivative(tr)
	if e != nil {
		t.Fatal(e)
	}
	ov, og, e := old.ValueAndDerivative(tr)
	if e != nil {
		t.Fatal(e)
	}
	multiNear(t, v, ov, 1e-12)
	for k := range g {
		multiNear(t, g[k], og[k], 1e-12)
	}
	for k := 0; k < 2; k++ {
		a, b := tr.Clone(), tr.Clone()
		p := tr.Parameters()
		p[k] -= 1e-4
		_ = a.SetParameters(p)
		p[k] += 2e-4
		_ = b.SetParameters(p)
		av, _ := c.Value(a)
		bv, _ := c.Value(b)
		multiNear(t, g[k], (bv-av)/2e-4, 1e-7)
	}
	for _, o := range []ImageComponentOptions{{FrameOfReference: "p", Units: "u", Limits: MetricLimits{Bytes: 10}}, {FrameOfReference: "p", Units: "u", SampleCount: -1}, {FrameOfReference: "p", Units: "u", Kind: 9}, {Units: "u"}} {
		if _, e := NewImageMetricComponent(context.Background(), f, m, d, o); e == nil {
			t.Fatal(o)
		}
	}
	bad := MetricDomain{Points: []render.Vec3{{X: math.NaN()}, {X: 1}}}
	if _, e := NewImageMetricComponent(context.Background(), f, m, bad, ImageComponentOptions{FrameOfReference: "p", Units: "u", SampleCount: 1}); e == nil {
		t.Fatal("unselected invalid point admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := NewImageMetricComponent(ctx, f, m, d, ImageComponentOptions{FrameOfReference: "p", Units: "u"}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
