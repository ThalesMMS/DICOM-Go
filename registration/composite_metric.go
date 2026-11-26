package registration

import (
	"context"
	"errors"
	"fmt"
	"reflect"
)

// ErrMultiMetric reports incompatible components or an invalid finite budget.
var ErrMultiMetric = errors.New("registration: invalid multi-metric composition")

// MetricLimits bounds logical payload and evaluation workspace, not process RSS.
// Zero fields select finite defaults. Workers is a ceiling; current components
// execute serially, giving identical reduction order for every admitted ceiling.
type MetricLimits struct {
	Components, Points, Channels, Parameters, Workers int
	Bytes                                             uint64
}

func (l MetricLimits) checked() (MetricLimits, error) {
	if l.Components < 0 || l.Components > 4096 || l.Points < 0 || l.Channels < 0 || l.Parameters < 0 || l.Workers < 0 || l.Parameters > 4096 {
		return l, ErrMultiMetric
	}
	if l.Components == 0 {
		l.Components = 64
	}
	if l.Points == 0 {
		l.Points = 1 << 24
	}
	if l.Channels == 0 {
		l.Channels = 64
	}
	if l.Parameters == 0 {
		l.Parameters = 4096
	}
	if l.Workers == 0 {
		l.Workers = 64
	}
	if l.Bytes == 0 {
		l.Bytes = 256 << 20
	}
	return l, nil
}

// MetricComponent is a cancellable library component. Its closed descriptor
// prevents an opaque callback from bypassing the advertised payload admission.
// Existing Metric implementations and APIs remain available separately.
type MetricComponent interface {
	Metric
	ValueContext(context.Context, Transform) (float64, error)
	ValueAndDerivativeContext(context.Context, Transform) (float64, []float64, error)
	componentInfo() componentInfo
	evaluateComponent(context.Context, Transform, bool) (float64, []float64, int, error)
}
type componentInfo struct {
	frame, units                 string
	points, channels, parameters int
	bytes                        uint64
}

// WeightedMetric labels a component. Weight must be finite and nonnegative.
// Zero excludes it completely, including domain and resource admission. Scale
// is a positive constant divisor (zero means one), never a gradient norm.
type WeightedMetric struct {
	Label         string
	Metric        MetricComponent
	Weight, Scale float64
}

// MetricContribution exposes raw and weighted values, physical frame, units,
// attempted samples and actual overlap. Error identifies a failed component;
// diagnostics may precede an error but no partial objective is published.
type MetricContribution struct {
	Label, FrameOfReference, Units      string
	Weight, Scale, Value, WeightedValue float64
	Points, Overlap                     int
	Excluded                            bool
	Error                               error
}
type MultiMetricResult struct {
	Value         float64
	Derivative    []float64
	Contributions []MetricContribution
}

// CompositeMetric minimizes sum(weight * value / scale). Components must use
// one explicit patient LPS frame. Fixed transform parameters are part of the
// parameterization contract, including rotation center/order. Caller transforms
// must not be mutated concurrently; components own detached input snapshots.
type CompositeMetric struct {
	components []WeightedMetric
	prototype  Transform
	limits     MetricLimits
}

func NewCompositeMetric(prototype Transform, components []WeightedMetric, limits MetricLimits) (*CompositeMetric, error) {
	l, err := limits.checked()
	if err != nil {
		return nil, err
	}
	if multiNilTransform(prototype) || prototype.NumberOfParameters() < 1 || prototype.NumberOfParameters() > l.Parameters || len(components) == 0 || len(components) > l.Components || len(prototype.FixedParameters()) > 4096 || len(prototype.Parameters()) != prototype.NumberOfParameters() || !finiteSlice(prototype.Parameters()) || !finiteSlice(prototype.FixedParameters()) {
		return nil, ErrMultiMetric
	}
	used := uint64(8192) + uint64(len(components))*2048 + uint64(prototype.NumberOfParameters())*128 + uint64(len(prototype.FixedParameters()))*8
	if used > l.Bytes {
		return nil, ErrMultiMetric
	}
	points, channels := 0, 0
	frame := ""
	labels := map[string]bool{}
	active := 0
	for _, c := range components {
		if c.Label == "" || len(c.Label) > 1024 || labels[c.Label] || !finiteSlice([]float64{c.Weight, c.Scale}) || c.Weight < 0 || c.Scale < 0 {
			return nil, ErrMultiMetric
		}
		labels[c.Label] = true
		if c.Weight == 0 {
			continue
		}
		// Accept only the admitted concrete catalog, including against external
		// wrappers that promote private interface methods through embedding.
		switch c.Metric.(type) {
		case *ImageMetricComponent, *PointSetMetric, *GaussianPointSetMetric:
		default:
			return nil, ErrMultiMetric
		}
		if c.Metric == nil || reflect.ValueOf(c.Metric).IsNil() {
			return nil, ErrMultiMetric
		}
		d := c.Metric.componentInfo()
		if d.frame == "" || d.points < 1 || d.channels < 0 || prototype.NumberOfParameters() > d.parameters {
			return nil, ErrMultiMetric
		}
		if frame == "" {
			frame = d.frame
		}
		if frame != d.frame {
			return nil, fmt.Errorf("%w: component %s frame", ErrMultiMetric, c.Label)
		}
		if d.points > l.Points-points || d.channels > l.Channels-channels || used > l.Bytes || d.bytes > l.Bytes-used {
			return nil, fmt.Errorf("%w: aggregate payload", ErrMultiMetric)
		}
		points += d.points
		channels += d.channels
		used += d.bytes
		active++
	}
	if active == 0 || used > l.Bytes {
		return nil, ErrMultiMetric
	}
	out := append([]WeightedMetric(nil), components...)
	for i := range out {
		if out[i].Scale == 0 {
			out[i].Scale = 1
		}
		if out[i].Weight == 0 {
			out[i].Metric = nil
		}
	}
	return &CompositeMetric{out, prototype.Clone(), l}, nil
}
func (m *CompositeMetric) compatible(t Transform) bool {
	return m != nil && !multiNilTransform(t) && reflect.TypeOf(t) == reflect.TypeOf(m.prototype) && t.Kind() == m.prototype.Kind() && t.NumberOfParameters() == m.prototype.NumberOfParameters() && reflect.DeepEqual(t.FixedParameters(), m.prototype.FixedParameters()) && finiteSlice(t.Parameters())
}
func (m *CompositeMetric) Value(t Transform) (float64, error) {
	r, e := m.Evaluate(context.Background(), t, false)
	return r.Value, e
}
func (m *CompositeMetric) ValueContext(ctx context.Context, t Transform) (float64, error) {
	r, e := m.Evaluate(ctx, t, false)
	return r.Value, e
}
func (m *CompositeMetric) ValueAndDerivative(t Transform) (float64, []float64, error) {
	return m.ValueAndDerivativeContext(context.Background(), t)
}
func (m *CompositeMetric) ValueAndDerivativeContext(ctx context.Context, t Transform) (float64, []float64, error) {
	r, e := m.Evaluate(ctx, t, true)
	return r.Value, r.Derivative, e
}

// Evaluate returns ordered contributions. Any cancellation, invalid overlap or
// nonfinite arithmetic invalidates the complete objective (zero value/nil
// derivative); contributions remain diagnostic only. No component is skipped on
// error, and weights are never silently renormalized.
func (m *CompositeMetric) Evaluate(ctx context.Context, t Transform, derivatives bool) (MultiMetricResult, error) {
	r := MultiMetricResult{}
	if ctx == nil || !m.compatible(t) {
		return r, ErrMultiMetric
	}
	if err := ctx.Err(); err != nil {
		return r, err
	}
	if derivatives {
		r.Derivative = make([]float64, t.NumberOfParameters())
	}
	fail := func(err error) (MultiMetricResult, error) { r.Value = 0; r.Derivative = nil; return r, err }
	for _, c := range m.components {
		d := MetricContribution{Label: c.Label, Weight: c.Weight, Scale: c.Scale, Excluded: c.Weight == 0}
		if d.Excluded {
			r.Contributions = append(r.Contributions, d)
			continue
		}
		info := c.Metric.componentInfo()
		d.FrameOfReference = info.frame
		d.Units = info.units
		d.Points = info.points
		v, g, n, err := c.Metric.evaluateComponent(ctx, t, derivatives)
		d.Value = v
		d.Overlap = n
		if err == nil && (!finiteSlice([]float64{v}) || derivatives && (len(g) != len(r.Derivative) || !finiteSlice(g))) {
			err = ErrMultiMetric
		}
		if err == nil {
			d.WeightedValue = (c.Weight / c.Scale) * v
			r.Value += d.WeightedValue
			for j := range g {
				r.Derivative[j] += (c.Weight / c.Scale) * g[j]
			}
			if !finiteSlice([]float64{r.Value, d.WeightedValue}) || !finiteSlice(r.Derivative) {
				err = ErrMultiMetric
			}
		}
		d.Error = err
		r.Contributions = append(r.Contributions, d)
		if err != nil {
			return fail(fmt.Errorf("component %s: %w", c.Label, err))
		}
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return r, nil
}

func multiNilTransform(t Transform) bool {
	if t == nil {
		return true
	}
	v := reflect.ValueOf(t)
	return v.Kind() == reflect.Ptr && v.IsNil()
}
