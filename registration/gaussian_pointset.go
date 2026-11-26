package registration

import (
	"context"
	"errors"
	"fmt"
	"github.com/ThalesMMS/dicom-go/render"
	"math"
	"math/rand"
)

var (
	// ErrGaussianPointSet reports invalid geometry, density parameters or arithmetic.
	ErrGaussianPointSet = errors.New("registration: invalid Gaussian pointset metric")
	// ErrPointSetWorkLimit reports the finite node-visit quota of one evaluation.
	ErrPointSetWorkLimit = errors.New("registration: Gaussian pointset work limit exceeded")
)

// GaussianPointSetOptions describes patient LPS coordinates and density policy.
// Neighbors is K (zero: five). SigmaMM must be positive and finite, with finite
// positive sigma² and reciprocal. Fixed/MovingLabels, when both provided, only
// admit matching labels; every sampled fixed label must have at least K moving
// points. Duplicate coordinates are retained as Gaussian density multiplicity.
// SampleCount zero visits all fixed points; positive draws with replacement once
// using Seed. Workers is a ceiling; the implementation is deterministic serial.
type GaussianPointSetOptions struct {
	SigmaMM                                       float64
	Neighbors, MaxNeighbors, SampleCount, Workers int
	Seed                                          int64
	FrameOfReference                              string
	FixedLabels, MovingLabels                     []uint32
	Limits                                        MetricLimits
	// MaxNodeVisits bounds total visited KD nodes per evaluation (zero: 2^26).
	MaxNodeVisits uint64
}

// GaussianPointSetMetric is a fixed-to-moving, one-way negative log local
// Gaussian density. For each mapped fixed point y=T(x), its K nearest eligible
// moving neighbors q contribute exp(-||y-q||²/(2*sigma²)); the value is minus
// the mean log of their average. No 3D normalization constant is included.
// It is not ICP and does not infer semantic correspondences or clinical accuracy.
type GaussianPointSetMetric struct {
	fixed                     []render.Vec3
	samples                   []int
	fixedLabels, movingLabels []uint32
	tree                      *kdTree
	sigma2                    float64
	neighbors                 int
	maxVisits                 uint64
	info                      componentInfo
}

func NewGaussianPointSetMetric(ctx context.Context, fixed, moving []render.Vec3, o GaussianPointSetOptions) (*GaussianPointSetMetric, error) {
	l, err := o.Limits.checked()
	if err != nil {
		return nil, err
	}
	if o.Neighbors == 0 {
		o.Neighbors = 5
	}
	if o.MaxNeighbors == 0 {
		o.MaxNeighbors = 1024
	}
	if o.MaxNodeVisits == 0 {
		o.MaxNodeVisits = 1 << 26
	}
	sigma2 := o.SigmaMM * o.SigmaMM
	if ctx == nil || o.FrameOfReference == "" || len(o.FrameOfReference) > 1024 || o.SigmaMM <= 0 || !finiteSlice([]float64{o.SigmaMM, sigma2, 1 / sigma2}) || sigma2 <= 0 || o.Neighbors < 1 || o.MaxNeighbors < 1 || o.MaxNeighbors > 1024 || o.Neighbors > o.MaxNeighbors || o.Neighbors > len(moving) || o.SampleCount < 0 || o.Workers < 0 || o.Workers > l.Workers || len(fixed) == 0 || len(moving) == 0 || len(fixed) > l.Points || len(moving) > l.Points-len(fixed) || o.SampleCount > l.Points {
		return nil, ErrGaussianPointSet
	}
	labels := o.FixedLabels != nil || o.MovingLabels != nil
	if labels && (len(o.FixedLabels) != len(fixed) || len(o.MovingLabels) != len(moving)) {
		return nil, ErrGaussianPointSet
	}
	samples := len(fixed)
	if o.SampleCount > 0 {
		samples = o.SampleCount
	}
	if o.MaxNodeVisits < uint64(samples) {
		return nil, ErrPointSetWorkLimit
	}
	// Conservative logical payload includes snapshots, build indices/scratch,
	// existing kdNode storage, label map, sample indices and bounded query scratch.
	bytes := uint64(8192) + uint64(l.Parameters)*128 + uint64(o.Neighbors)*64
	for _, part := range []struct {
		n    int
		unit uint64
	}{{len(fixed), 32}, {len(moving), 160}, {samples, 8}} {
		if bytes > l.Bytes || uint64(part.n) > (l.Bytes-bytes)/part.unit {
			return nil, ErrGaussianPointSet
		}
		bytes += uint64(part.n) * part.unit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if labels {
		counts := map[uint32]int{}
		for i, label := range o.MovingLabels {
			if i%64 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			counts[label]++
		}
		// Validate every fixed label, including unsampled points; seed cannot hide an invalid domain.
		for i, label := range o.FixedLabels {
			if i%64 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if counts[label] < o.Neighbors {
				return nil, ErrGaussianPointSet
			}
		}
	}
	f, m := make([]render.Vec3, len(fixed)), make([]render.Vec3, len(moving))
	for c, cloud := range [][]render.Vec3{fixed, moving} {
		for i, v := range cloud {
			if i%64 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if !finiteVec(v) {
				return nil, ErrGaussianPointSet
			}
			if c == 0 {
				f[i] = v
			} else {
				m[i] = v
			}
		}
	}
	metric := &GaussianPointSetMetric{fixed: f, samples: make([]int, samples), sigma2: sigma2, neighbors: o.Neighbors, maxVisits: o.MaxNodeVisits, info: componentInfo{frame: o.FrameOfReference, units: "dimensionless negative log density", points: samples, parameters: l.Parameters, bytes: bytes}}
	if labels {
		metric.fixedLabels = make([]uint32, len(f))
		metric.movingLabels = make([]uint32, len(m))
		for i, v := range o.FixedLabels {
			if i%64 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			metric.fixedLabels[i] = v
		}
		for i, v := range o.MovingLabels {
			if i%64 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			metric.movingLabels[i] = v
		}
	}
	rng := rand.New(rand.NewSource(o.Seed))
	for i := range metric.samples {
		if i%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		metric.samples[i] = i
		if o.SampleCount > 0 {
			metric.samples[i] = rng.Intn(len(f))
		}
	}
	tree, err := newGaussianKDTree(ctx, m)
	if err != nil {
		return nil, err
	}
	metric.tree = tree
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return metric, nil
}
func (m *GaussianPointSetMetric) componentInfo() componentInfo { return m.info }
func (m *GaussianPointSetMetric) Value(t Transform) (float64, error) {
	return m.ValueContext(context.Background(), t)
}
func (m *GaussianPointSetMetric) ValueContext(ctx context.Context, t Transform) (float64, error) {
	v, _, _, e := m.evaluateComponent(ctx, t, false)
	return v, e
}
func (m *GaussianPointSetMetric) ValueAndDerivative(t Transform) (float64, []float64, error) {
	return m.ValueAndDerivativeContext(context.Background(), t)
}
func (m *GaussianPointSetMetric) ValueAndDerivativeContext(ctx context.Context, t Transform) (float64, []float64, error) {
	v, g, _, e := m.evaluateComponent(ctx, t, true)
	return v, g, e
}
func (m *GaussianPointSetMetric) evaluateComponent(ctx context.Context, t Transform, derivatives bool) (float64, []float64, int, error) {
	if m == nil || m.tree == nil || len(m.samples) == 0 || ctx == nil || multiNilTransform(t) || t.NumberOfParameters() < 1 || t.NumberOfParameters() > m.info.parameters {
		return 0, nil, 0, ErrGaussianPointSet
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, 0, err
	}
	if len(t.Parameters()) != t.NumberOfParameters() || !finiteSlice(t.Parameters()) || len(t.FixedParameters()) > 4096 || !finiteSlice(t.FixedParameters()) {
		return 0, nil, 0, ErrGaussianPointSet
	}
	g := make([]float64, t.NumberOfParameters())
	neighbors := make([]gaussianNeighbor, 0, m.neighbors)
	var j Jacobian
	sum := 0.0
	visits := uint64(0)
	for at, index := range m.samples {
		if err := ctx.Err(); err != nil {
			return 0, nil, at, err
		}
		point := m.fixed[index]
		mapped := t.TransformPoint(point)
		if !finiteVec(mapped) {
			return 0, nil, at, ErrGaussianPointSet
		}
		label := uint32(0)
		if m.fixedLabels != nil {
			label = m.fixedLabels[index]
		}
		var err error
		neighbors, err = m.tree.gaussianNearest(ctx, mapped, m.neighbors, m.movingLabels, label, neighbors[:0], &visits, m.maxVisits)
		if err != nil {
			return 0, nil, at, err
		}
		// Sequential division avoids an overflowing 2*sigma² denominator.
		base := neighbors[0].d2 / m.sigma2 / 2
		if !finite(base) {
			return 0, nil, at, ErrGaussianPointSet
		}
		weights := 0.0
		for k, n := range neighbors {
			if k%32 == 0 {
				if err := ctx.Err(); err != nil {
					return 0, nil, at, err
				}
			}
			delta := (n.d2 - neighbors[0].d2) / m.sigma2 / 2
			if !finite(delta) {
				return 0, nil, at, ErrGaussianPointSet
			}
			weights += math.Exp(-delta)
		}
		value := base - math.Log(weights) + math.Log(float64(m.neighbors))
		if !finite(value) {
			return 0, nil, at, ErrGaussianPointSet
		}
		sum += value
		if derivatives {
			t.JacobianWithRespectToParameters(point, &j)
			for k, n := range neighbors {
				if k%32 == 0 {
					if err := ctx.Err(); err != nil {
						return 0, nil, at, err
					}
				}
				delta := (n.d2 - neighbors[0].d2) / m.sigma2 / 2
				w := math.Exp(-delta) / weights
				d := mapped.Sub(m.tree.points[n.index]).Scale(w / m.sigma2)
				if !finiteVec(d) {
					return 0, nil, at, ErrGaussianPointSet
				}
				for p := range g {
					g[p] += d.X*j.At(0, p) + d.Y*j.At(1, p) + d.Z*j.At(2, p)
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, len(m.samples), err
	}
	sum /= float64(len(m.samples))
	for p := range g {
		g[p] /= float64(len(m.samples))
	}
	if !finite(sum) || !finiteSlice(g) {
		return 0, nil, len(m.samples), fmt.Errorf("%w: nonfinite density/gradient", ErrGaussianPointSet)
	}
	if !derivatives {
		g = nil
	}
	return sum, g, len(m.samples), nil
}
