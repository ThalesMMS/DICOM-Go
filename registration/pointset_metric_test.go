package registration

import (
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/render"
	"math"
	"testing"
)

func TestPointSetMetricTransformDerivativeAndCompatibility(t *testing.T) {
	f := multiPoints()
	m := make([]render.Vec3, len(f))
	for i, p := range f {
		m[i] = p.Add(render.Vec3{X: 2, Y: -1, Z: .5})
	}
	metric, e := NewPointSetMetric(context.Background(), f, m, "p", MetricLimits{})
	if e != nil {
		t.Fatal(e)
	}
	tr := NewAffine()
	v, g, e := metric.ValueAndDerivative(tr)
	if e != nil {
		t.Fatal(e)
	}
	multiNear(t, v, 5.25, 1e-13)
	for k := range g {
		a, b := tr.Clone(), tr.Clone()
		p := tr.Parameters()
		p[k] -= 1e-5
		_ = a.SetParameters(p)
		p[k] += 2e-5
		_ = b.SetParameters(p)
		av, _ := metric.Value(a)
		bv, _ := metric.Value(b)
		multiNear(t, g[k], (bv-av)/2e-5, 1e-7)
	}
	f[0].X = 999
	m[0].X = -999
	v2, e := metric.Value(tr)
	if e != nil || v2 != v {
		t.Fatal("borrowed points", e)
	}
	for _, tc := range []struct {
		f, m []render.Vec3
		l    MetricLimits
	}{{nil, nil, MetricLimits{}}, {multiPoints(), nil, MetricLimits{}}, {multiPoints(), multiPoints(), MetricLimits{Points: 2}}, {multiPoints(), multiPoints(), MetricLimits{Bytes: 10}}, {[]render.Vec3{{X: math.NaN()}}, []render.Vec3{{}}, MetricLimits{}}} {
		if _, e := NewPointSetMetric(context.Background(), tc.f, tc.m, "p", tc.l); e == nil {
			t.Fatal("invalid pair admitted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := make([]render.Vec3, 1000)
	large, _ := NewPointSetMetric(context.Background(), n, n, "p", MetricLimits{})
	calls := 0
	hook := &multiCancelTransform{NewTranslation3D(), &calls, cancel}
	if v, g, e := large.ValueAndDerivativeContext(ctx, hook); !errors.Is(e, context.Canceled) || v != 0 || g != nil || calls > 128 {
		t.Fatal(v, g, e, calls)
	}
}
