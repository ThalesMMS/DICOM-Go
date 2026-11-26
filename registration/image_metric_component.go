package registration

import (
	"context"
	"fmt"
	"github.com/ThalesMMS/dicom-go/render"
	"math/rand"
)

// ImageMetricKind selects an exact scalar objective; correlation is negative
// squared normalized correlation, matching NewCorrelation.
type ImageMetricKind int

const (
	ImageMeanSquares ImageMetricKind = iota
	ImageCorrelation
)

// ImageComponentOptions identifies a channel and its physical coordinate frame.
// SampleCount zero uses the exact supplied domain. Positive counts draw with
// replacement using Seed, once at construction; masks apply during evaluation.
// Units describes the raw objective (e.g. HU²), not an automatic unit conversion.
type ImageComponentOptions struct {
	Kind                    ImageMetricKind
	FrameOfReference, Units string
	SampleCount             int
	Seed                    int64
	Limits                  MetricLimits
}
type ImageMetricComponent struct {
	evaluator *evaluator
	kind      ImageMetricKind
	info      componentInfo
}

// NewImageMetricComponent snapshots images, masks and sample points only after
// finite admission. Inputs must remain stable for the duration of construction.
func NewImageMetricComponent(ctx context.Context, fixed, moving *Image, domain MetricDomain, o ImageComponentOptions) (*ImageMetricComponent, error) {
	l, err := o.Limits.checked()
	if err != nil {
		return nil, err
	}
	if ctx == nil || fixed == nil || moving == nil || o.FrameOfReference == "" || len(o.FrameOfReference) > 1024 || len(o.Units) > 256 || o.Units == "" || o.Kind < ImageMeanSquares || o.Kind > ImageCorrelation || o.SampleCount < 0 || domain.Workers < 0 || domain.Workers > l.Workers {
		return nil, ErrMultiMetric
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	count := len(fixed.data)
	if domain.Points != nil {
		count = len(domain.Points)
	}
	if count == 0 || count > l.Points || o.SampleCount > l.Points {
		return nil, ErrMultiMetric
	}
	for i, v := range domain.Points {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if !finiteVec(v) {
			return nil, ErrMultiMetric
		}
	}
	outputCount := count
	if o.SampleCount > 0 {
		outputCount = o.SampleCount
	}
	// Logical retained payload plus conservative header allowance. No full sample
	// buffers are created during evaluation; parameter scratch is admitted by the composite.
	bytes := uint64(4096) + uint64(l.Parameters)*128
	for _, im := range []*Image{fixed, moving, domain.FixedMask, domain.MovingMask} {
		if im != nil {
			if len(im.data) == 0 {
				return nil, ErrMultiMetric
			}
			if uint64(len(im.data)) > l.Bytes/4 {
				return nil, ErrMultiMetric
			}
			n := uint64(len(im.data)) * 4
			if bytes > l.Bytes || n > l.Bytes-bytes {
				return nil, ErrMultiMetric
			}
			bytes += n
		}
	}
	pointBytes := uint64(0)
	if domain.Points != nil || o.SampleCount > 0 {
		if uint64(outputCount) > l.Bytes/24 {
			return nil, ErrMultiMetric
		}
		pointBytes = uint64(outputCount) * 24
	}
	if bytes > l.Bytes || pointBytes > l.Bytes-bytes {
		return nil, ErrMultiMetric
	}
	bytes += pointBytes
	copyImage := func(im *Image) (*Image, error) {
		if im == nil {
			return nil, nil
		}
		out := *im
		out.data = make([]float32, len(im.data))
		for i, v := range im.data {
			if i%256 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if !finiteSlice([]float64{float64(v)}) {
				return nil, ErrMultiMetric
			}
			out.data[i] = v
		}
		return &out, nil
	}
	f, err := copyImage(fixed)
	if err != nil {
		return nil, err
	}
	mv, err := copyImage(moving)
	if err != nil {
		return nil, err
	}
	fm, err := copyImage(domain.FixedMask)
	if err != nil {
		return nil, err
	}
	mm, err := copyImage(domain.MovingMask)
	if err != nil {
		return nil, err
	}
	d := MetricDomain{FixedMask: fm, MovingMask: mm, Workers: domain.Workers}
	if domain.Points != nil || o.SampleCount > 0 {
		d.Points = make([]render.Vec3, outputCount)
		rng := rand.New(rand.NewSource(o.Seed))
		for i := range d.Points {
			if i%256 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			j := i
			if o.SampleCount > 0 {
				j = rng.Intn(count)
			}
			if domain.Points != nil {
				d.Points[i] = domain.Points[j]
			} else {
				s := fixed.size
				d.Points[i] = fixed.Point(render.Vec3{X: float64(j % s[0]), Y: float64(j / s[0] % s[1]), Z: float64(j / (s[0] * s[1]))})
			}
			if !finiteVec(d.Points[i]) {
				return nil, ErrMultiMetric
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &ImageMetricComponent{&evaluator{f, mv, d}, o.Kind, componentInfo{frame: o.FrameOfReference, units: o.Units, points: outputCount, channels: 1, parameters: l.Parameters, bytes: bytes}}, nil
}
func (m *ImageMetricComponent) componentInfo() componentInfo { return m.info }
func (m *ImageMetricComponent) Value(t Transform) (float64, error) {
	v, _, _, e := m.evaluateComponent(context.Background(), t, false)
	return v, e
}
func (m *ImageMetricComponent) ValueContext(ctx context.Context, t Transform) (float64, error) {
	v, _, _, e := m.evaluateComponent(ctx, t, false)
	return v, e
}
func (m *ImageMetricComponent) ValueAndDerivative(t Transform) (float64, []float64, error) {
	return m.ValueAndDerivativeContext(context.Background(), t)
}
func (m *ImageMetricComponent) ValueAndDerivativeContext(ctx context.Context, t Transform) (float64, []float64, error) {
	v, g, _, e := m.evaluateComponent(ctx, t, true)
	return v, g, e
}
func (m *ImageMetricComponent) evaluateComponent(ctx context.Context, t Transform, derivatives bool) (float64, []float64, int, error) {
	if m == nil || m.evaluator == nil || ctx == nil || multiNilTransform(t) || t.NumberOfParameters() < 1 || t.NumberOfParameters() > m.info.parameters {
		return 0, nil, 0, ErrMultiMetric
	}
	p := t.NumberOfParameters()
	g := make([]float64, p)
	var j Jacobian
	var moments correlationPartial
	if derivatives && m.kind == ImageCorrelation {
		moments.g = make([]float64, p)
		moments.fg = make([]float64, p)
		moments.mg = make([]float64, p)
	}
	sum := 0.0
	n := 0
	for i := 0; i < m.evaluator.pointCount(); i++ {
		if i%64 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, nil, n, err
			}
		}
		s, ok, sampleErr := m.sampleAt(i, t, derivatives)
		if sampleErr != nil {
			return 0, nil, n, sampleErr
		}
		if !ok {
			continue
		}
		if !finiteSlice([]float64{s.fixed, s.moving}) || derivatives && !finiteVec(s.gradient) {
			return 0, nil, n, ErrMultiMetric
		}
		n++
		if m.kind == ImageMeanSquares {
			diff := s.moving - s.fixed
			sum += diff * diff
			if derivatives {
				accumulateDerivative(g, &j, t, s, 2*diff)
			}
			continue
		}
		moments.f += s.fixed
		moments.m += s.moving
		moments.ff += s.fixed * s.fixed
		moments.mm += s.moving * s.moving
		moments.fm += s.fixed * s.moving
		if derivatives {
			t.JacobianWithRespectToParameters(s.fixedPoint, &j)
			for k := 0; k < p; k++ {
				v := s.gradient.X*j.At(0, k) + s.gradient.Y*j.At(1, k) + s.gradient.Z*j.At(2, k)
				moments.g[k] += v
				moments.fg[k] += s.fixed * v
				moments.mg[k] += s.moving * v
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, n, err
	}
	if n == 0 {
		return 0, nil, n, ErrTooFewSamples
	}
	value := sum / float64(n)
	if m.kind == ImageMeanSquares {
		for k := range g {
			g[k] /= float64(n)
		}
	} else {
		nf := float64(n)
		f, mv := moments.f/nf, moments.m/nf
		ff, mm, fm := moments.ff-nf*f*f, moments.mm-nf*mv*mv, moments.fm-nf*f*mv
		if n < 2 || ff <= 0 || mm <= 0 {
			return 0, nil, n, ErrTooFewSamples
		}
		value = -fm * fm / (ff * mm)
		if derivatives {
			scale := -2 * fm / (ff * mm)
			for k := range g {
				g[k] = scale * (moments.fg[k] - f*moments.g[k] - fm/mm*(moments.mg[k]-mv*moments.g[k]))
			}
		}
	}
	if !finiteSlice([]float64{value}) || !finiteSlice(g) {
		return 0, nil, n, fmt.Errorf("%w: nonfinite image evaluation", ErrMultiMetric)
	}
	if !derivatives {
		g = nil
	}
	return value, g, n, nil
}

// sampleAt validates coordinates before interpolation or integer mask sampling.
// Each physical point is mapped once. Finite out-of-bounds coordinates are
// ordinary non-overlap; nonfinite coordinates invalidate the complete objective.
func (m *ImageMetricComponent) sampleAt(i int, t Transform, gradients bool) (sample, bool, error) {
	e := m.evaluator
	var point render.Vec3
	var fixed float64
	if e.domain.Points != nil {
		point = e.domain.Points[i]
		if !finiteVec(point) {
			return sample{}, false, ErrMultiMetric
		}
		index := e.fixed.ContinuousIndex(point)
		if !finiteVec(index) {
			return sample{}, false, ErrMultiMetric
		}
		if !e.fixed.inside(index) {
			return sample{}, false, nil
		}
		fixed = e.fixed.linearAt(index)
	} else {
		size := e.fixed.size
		point = e.fixed.Point(render.Vec3{X: float64(i % size[0]), Y: float64(i / size[0] % size[1]), Z: float64(i / (size[0] * size[1]))})
		if !finiteVec(point) {
			return sample{}, false, ErrMultiMetric
		}
		fixed = float64(e.fixed.data[i])
	}
	inside, err := multiMaskAt(e.domain.FixedMask, point)
	if err != nil || !inside {
		return sample{}, false, err
	}
	mapped := t.TransformPoint(point)
	if !finiteVec(mapped) {
		return sample{}, false, ErrMultiMetric
	}
	index := e.moving.ContinuousIndex(mapped)
	if !finiteVec(index) {
		return sample{}, false, ErrMultiMetric
	}
	if !e.moving.inside(index) {
		return sample{}, false, nil
	}
	inside, err = multiMaskAt(e.domain.MovingMask, mapped)
	if err != nil || !inside {
		return sample{}, false, err
	}
	out := sample{fixedPoint: point, fixed: fixed, moving: e.moving.linearAt(index)}
	if gradients {
		out.gradient = e.moving.gradientAt(index)
	}
	return out, true, nil
}

// multiMaskAt checks floating bounds BEFORE conversion to int. Checking only
// finiteness is insufficient: a finite 1e20 index also overflows a native int.
// This helper preserves the existing nearest-mask [-.5,size-.5) domain while
// leaving the legacy evaluator unchanged.
func multiMaskAt(im *Image, point render.Vec3) (bool, error) {
	if im == nil {
		return true, nil
	}
	index := im.ContinuousIndex(point)
	if !finiteVec(index) {
		return false, ErrMultiMetric
	}
	if !im.inside(index) {
		return false, nil
	}
	return im.At(int(index.X+.5), int(index.Y+.5), int(index.Z+.5)) != 0, nil
}
