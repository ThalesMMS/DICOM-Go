package registration

import (
	"context"
	"github.com/ThalesMMS/dicom-go/render"
)

// PointSetMetric compares corresponding fixed-to-moving physical LPS points:
// mean ||T(fixed[i])-moving[i]||², in mm². This is paired least squares, not ICP.
type PointSetMetric struct {
	fixed, moving []render.Vec3
	info          componentInfo
}

func NewPointSetMetric(ctx context.Context, fixed, moving []render.Vec3, frame string, limits MetricLimits) (*PointSetMetric, error) {
	l, e := limits.checked()
	if e != nil {
		return nil, e
	}
	base := uint64(4096) + uint64(l.Parameters)*128
	if base > l.Bytes || uint64(len(fixed)) > (l.Bytes-base)/48 {
		return nil, ErrMultiMetric
	}
	bytes := base + uint64(len(fixed))*48
	if ctx == nil || frame == "" || len(frame) > 1024 || len(fixed) == 0 || len(fixed) != len(moving) || len(fixed) > l.Points || bytes > l.Bytes {
		return nil, ErrMultiMetric
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	f, m := make([]render.Vec3, len(fixed)), make([]render.Vec3, len(moving))
	for i := range f {
		if i%64 == 0 {
			if e := ctx.Err(); e != nil {
				return nil, e
			}
		}
		if !finiteVec(fixed[i]) || !finiteVec(moving[i]) {
			return nil, ErrMultiMetric
		}
		f[i], m[i] = fixed[i], moving[i]
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	return &PointSetMetric{f, m, componentInfo{frame: frame, units: "mm²", points: len(f), channels: 0, parameters: l.Parameters, bytes: bytes}}, nil
}
func (m *PointSetMetric) componentInfo() componentInfo { return m.info }
func (m *PointSetMetric) Value(t Transform) (float64, error) {
	v, _, _, e := m.evaluateComponent(context.Background(), t, false)
	return v, e
}
func (m *PointSetMetric) ValueContext(ctx context.Context, t Transform) (float64, error) {
	v, _, _, e := m.evaluateComponent(ctx, t, false)
	return v, e
}
func (m *PointSetMetric) ValueAndDerivative(t Transform) (float64, []float64, error) {
	return m.ValueAndDerivativeContext(context.Background(), t)
}
func (m *PointSetMetric) ValueAndDerivativeContext(ctx context.Context, t Transform) (float64, []float64, error) {
	v, g, _, e := m.evaluateComponent(ctx, t, true)
	return v, g, e
}
func (m *PointSetMetric) evaluateComponent(ctx context.Context, t Transform, derivatives bool) (float64, []float64, int, error) {
	if m == nil || len(m.fixed) == 0 || ctx == nil || multiNilTransform(t) || t.NumberOfParameters() < 1 || t.NumberOfParameters() > m.info.parameters {
		return 0, nil, 0, ErrMultiMetric
	}
	g := make([]float64, t.NumberOfParameters())
	var j Jacobian
	value := 0.0
	for i, f := range m.fixed {
		if i%64 == 0 {
			if e := ctx.Err(); e != nil {
				return 0, nil, i, e
			}
		}
		d := t.TransformPoint(f).Sub(m.moving[i])
		if !finiteVec(d) {
			return 0, nil, i, ErrMultiMetric
		}
		value += d.X*d.X + d.Y*d.Y + d.Z*d.Z
		if derivatives {
			t.JacobianWithRespectToParameters(f, &j)
			for k := range g {
				g[k] += 2 * (d.X*j.At(0, k) + d.Y*j.At(1, k) + d.Z*j.At(2, k))
			}
		}
	}
	if e := ctx.Err(); e != nil {
		return 0, nil, len(m.fixed), e
	}
	n := float64(len(m.fixed))
	value /= n
	for k := range g {
		g[k] /= n
	}
	if !finiteSlice([]float64{value}) || !finiteSlice(g) {
		return 0, nil, len(m.fixed), ErrMultiMetric
	}
	if !derivatives {
		g = nil
	}
	return value, g, len(m.fixed), nil
}
