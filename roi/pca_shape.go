package roi

import (
	"context"
	"errors"
	"fmt"
	"math"
)

var (
	ErrPCAShapeInvalid = errors.New("roi: invalid research PCA shape model")
	ErrPCAShapeLimit   = errors.New("roi: research PCA shape resource limit")
	ErrPCAShapeOutside = errors.New("roi: PCA shape point outside model domain")
)

const PCAShapeConvention = "negative-inside-mm"

// PCAShapeGeometry describes a regular orthonormal physical grid, x fastest.
// Direction is row-major; its columns are voxel axes in patient-space mm.
// All geometry fields, including a singleton z spacing, must be explicit.
type PCAShapeGeometry struct {
	Dimensions [3]int     `json:"dimensions"`
	SpacingMM  [3]float64 `json:"spacingMM"`
	OriginMM   [3]float64 `json:"originMM"`
	Direction  [9]float64 `json:"direction"`
}

// PCAShapeLimits bound owned arrays and deterministic numerical work. Borrowed
// input arrays and runtime/RSS are excluded; all zero fields select defaults.
type PCAShapeLimits struct {
	MaxVoxels, MaxComponents, MaxSamples int
	MaxBytes, MaxWork                    int64
}

func (l PCAShapeLimits) normalize() (PCAShapeLimits, error) {
	if l.MaxVoxels < 0 || l.MaxComponents < 0 || l.MaxSamples < 0 || l.MaxBytes < 0 || l.MaxWork < 0 {
		return l, ErrPCAShapeInvalid
	}
	if l.MaxVoxels == 0 {
		l.MaxVoxels = 1 << 20
	}
	if l.MaxComponents == 0 {
		l.MaxComponents = 8
	}
	if l.MaxSamples == 0 {
		l.MaxSamples = 16
	}
	if l.MaxBytes == 0 {
		l.MaxBytes = 256 << 20
	}
	if l.MaxWork == 0 {
		l.MaxWork = 1 << 28
	}
	if l.MaxVoxels > 4<<20 || l.MaxComponents > 16 || l.MaxSamples > 32 || l.MaxBytes > 512<<20 || l.MaxWork > 1<<30 {
		return l, ErrPCAShapeLimit
	}
	return l, nil
}
func pcaContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
func pcaFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func pcaRotation(r [9]float64) bool {
	for _, v := range r {
		if !pcaFinite(v) || math.Abs(v) > 1+1e-10 {
			return false
		}
	}
	for a := 0; a < 3; a++ {
		for b := 0; b < 3; b++ {
			dot := 0.0
			for j := 0; j < 3; j++ {
				dot += r[j*3+a] * r[j*3+b]
			}
			want := 0.0
			if a == b {
				want = 1
			}
			if math.Abs(dot-want) > 1e-10 {
				return false
			}
		}
	}
	det := r[0]*(r[4]*r[8]-r[5]*r[7]) - r[1]*(r[3]*r[8]-r[5]*r[6]) + r[2]*(r[3]*r[7]-r[4]*r[6])
	return math.Abs(det-1) < 1e-10
}
func (g PCAShapeGeometry) count(l PCAShapeLimits) (int, error) {
	n := 1
	if !pcaRotation(g.Direction) {
		return 0, fmt.Errorf("%w: orthonormal right-handed direction required", ErrPCAShapeInvalid)
	}
	for a, d := range g.Dimensions {
		if d <= 0 {
			return 0, ErrPCAShapeInvalid
		}
		if d > l.MaxVoxels/n {
			return 0, ErrPCAShapeLimit
		}
		n *= d
		if !pcaFinite(g.SpacingMM[a]) || g.SpacingMM[a] < 1e-6 || g.SpacingMM[a] > 1e6 || !pcaFinite(g.OriginMM[a]) || math.Abs(g.OriginMM[a])+float64(d)*g.SpacingMM[a] > 1e9 {
			return 0, fmt.Errorf("%w: physical grid", ErrPCAShapeInvalid)
		}
	}
	for a := 0; a < 3; a++ {
		extent := math.Abs(g.OriginMM[a])
		for b := 0; b < 3; b++ {
			extent += math.Abs(g.Direction[3*a+b]) * float64(g.Dimensions[b]-1) * g.SpacingMM[b]
		}
		if extent > 1e9 {
			return 0, ErrPCAShapeInvalid
		}
	}
	return n, nil
}
func (g PCAShapeGeometry) point(x, y, z int) [3]float64 {
	i := [3]float64{float64(x) * g.SpacingMM[0], float64(y) * g.SpacingMM[1], float64(z) * g.SpacingMM[2]}
	p := g.OriginMM
	for a := 0; a < 3; a++ {
		for b := 0; b < 3; b++ {
			p[a] += g.Direction[3*a+b] * i[b]
		}
	}
	return p
}

// PCAShapeModelData is versioned, caller-owned interchange data. Components
// have unit Euclidean L2 norm (not RMS norm); sigma=sqrt(sample covariance
// eigenvalue). Coefficients therefore express standard deviations. Caller
// deserializers must bound their own input before constructing this model.
type PCAShapeModelData struct {
	Version              int              `json:"version"`
	ID                   string           `json:"id"`
	Provenance           string           `json:"provenance"`
	Convention           string           `json:"convention"`
	Geometry             PCAShapeGeometry `json:"geometry"`
	TrainingSamples      int              `json:"trainingSamples"`
	MeanMM               []float64        `json:"meanMM"`
	Components           [][]float64      `json:"components"`
	StandardDeviationsMM []float64        `json:"standardDeviationsMM"`
}

// PCAShapeModel is immutable after validation. It carries no anatomical
// confidence or clinical qualification. Training/constructor inputs are cloned.
type PCAShapeModel struct{ data PCAShapeModelData }

func pcaField(ctx context.Context, field []float64, n int, signs bool) error {
	if len(field) != n {
		return fmt.Errorf("%w: field length", ErrPCAShapeInvalid)
	}
	negative, positive := false, false
	for i, v := range field {
		if i%4096 == 0 {
			if e := ctx.Err(); e != nil {
				return e
			}
		}
		if !pcaFinite(v) || math.Abs(v) > 1e6 {
			return fmt.Errorf("%w: finite mm field", ErrPCAShapeInvalid)
		}
		negative = negative || v < 0
		positive = positive || v > 0
	}
	if signs && (!negative || !positive) {
		return fmt.Errorf("%w: shape requires inside and outside samples", ErrPCAShapeInvalid)
	}
	return nil
}

// NewPCAShapeModel validates geometry, convention, basis orthonormality and
// finite positive sigmas before cloning. It publishes no model on error/cancel.
func NewPCAShapeModel(ctx context.Context, d PCAShapeModelData, limits PCAShapeLimits) (*PCAShapeModel, error) {
	ctx = pcaContext(ctx)
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	l, e := limits.normalize()
	if e != nil {
		return nil, e
	}
	n, e := d.Geometry.count(l)
	if e != nil {
		return nil, e
	}
	k := len(d.Components)
	if d.Version != 1 || d.Convention != PCAShapeConvention || d.ID == "" || d.Provenance == "" || len(d.ID) > 1024 || len(d.Provenance) > 4096 || d.TrainingSamples < 2 || k >= d.TrainingSamples {
		return nil, ErrPCAShapeInvalid
	}
	if k > l.MaxComponents || d.TrainingSamples > l.MaxSamples || int64(n)*int64(k+1)*8+8192 > l.MaxBytes || int64(n)*int64(k*k+k+1) > l.MaxWork {
		return nil, ErrPCAShapeLimit
	}
	if len(d.StandardDeviationsMM) != k {
		return nil, ErrPCAShapeInvalid
	}
	if e = pcaField(ctx, d.MeanMM, n, true); e != nil {
		return nil, e
	}
	for a, component := range d.Components {
		if !pcaFinite(d.StandardDeviationsMM[a]) || d.StandardDeviationsMM[a] <= 0 || d.StandardDeviationsMM[a] > 1e9 {
			return nil, ErrPCAShapeInvalid
		}
		if e = pcaField(ctx, component, n, false); e != nil {
			return nil, e
		}
		for b := 0; b <= a; b++ {
			dot := 0.0
			for i, v := range component {
				if i%4096 == 0 {
					if e = ctx.Err(); e != nil {
						return nil, e
					}
				}
				dot += v * d.Components[b][i]
			}
			want := 0.0
			if a == b {
				want = 1
			}
			if math.Abs(dot-want) > 1e-8 {
				return nil, fmt.Errorf("%w: components not orthonormal", ErrPCAShapeInvalid)
			}
		}
	}
	copyField := func(src []float64) []float64 {
		out := make([]float64, len(src))
		for i, v := range src {
			if i%4096 == 0 && ctx.Err() != nil {
				return nil
			}
			out[i] = v
		}
		return out
	}
	d.MeanMM = copyField(d.MeanMM)
	d.StandardDeviationsMM = append([]float64(nil), d.StandardDeviationsMM...)
	components := make([][]float64, k)
	for a, component := range d.Components {
		components[a] = copyField(component)
	}
	d.Components = components
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	return &PCAShapeModel{data: d}, nil
}

// Data returns an owned model snapshot under the same constructor limits.
func (m *PCAShapeModel) Data(ctx context.Context, l PCAShapeLimits) (PCAShapeModelData, error) {
	if m == nil {
		return PCAShapeModelData{}, ErrPCAShapeInvalid
	}
	copy, e := NewPCAShapeModel(ctx, m.data, l)
	if e != nil {
		return PCAShapeModelData{}, e
	}
	return copy.data, nil
}

// PCAShapePose maps world points to the canonical model domain: R*x+t. Only
// explicit rigid transforms are accepted, so distances stay in millimetres.
type PCAShapePose struct {
	Rotation      [9]float64
	TranslationMM [3]float64
}

func IdentityPCAShapePose() PCAShapePose {
	return PCAShapePose{Rotation: [9]float64{1, 0, 0, 0, 1, 0, 0, 0, 1}}
}
func (p PCAShapePose) validate() error {
	if !pcaRotation(p.Rotation) {
		return ErrPCAShapeInvalid
	}
	for _, v := range p.TranslationMM {
		if !pcaFinite(v) || math.Abs(v) > 1e9 {
			return ErrPCAShapeInvalid
		}
	}
	return nil
}

type PCAShapeOutsidePolicy uint8

const (
	PCARejectOutside PCAShapeOutsidePolicy = iota
	// PCANearestBoundary explicitly clamps sampling to the nearest grid centre.
	// Extrapolated counts are disclosed; this is not a true SDF outside the grid.
	PCANearestBoundary
)

type PCAShapeEvaluation struct {
	FieldMM        []float64
	OutsideSamples int
}

func (m *PCAShapeModel) parameters(coeff []float64, pose PCAShapePose, policy PCAShapeOutsidePolicy) error {
	if m == nil || len(coeff) != len(m.data.Components) || policy > PCANearestBoundary {
		return ErrPCAShapeInvalid
	}
	if e := pose.validate(); e != nil {
		return e
	}
	for _, v := range coeff {
		if !pcaFinite(v) || math.Abs(v) > 100 {
			return ErrPCAShapeInvalid
		}
	}
	return nil
}
func (m *PCAShapeModel) value(coeff []float64, pose PCAShapePose, policy PCAShapeOutsidePolicy, point [3]float64) (float64, bool, error) {
	var p, q [3]float64
	for a := 0; a < 3; a++ {
		if !pcaFinite(point[a]) || math.Abs(point[a]) > 1e9 {
			return 0, false, ErrPCAShapeInvalid
		}
		p[a] = pose.TranslationMM[a]
		for b := 0; b < 3; b++ {
			p[a] += pose.Rotation[3*a+b] * point[b]
		}
	}
	g := m.data.Geometry
	outside := false
	var lo, hi [3]int
	var f [3]float64
	for a := 0; a < 3; a++ {
		for b := 0; b < 3; b++ {
			q[a] += g.Direction[3*b+a] * (p[b] - g.OriginMM[b])
		}
		q[a] /= g.SpacingMM[a]
		last := float64(g.Dimensions[a] - 1)
		if q[a] < -1e-8 || q[a] > last+1e-8 {
			outside = true
		}
		q[a] = math.Max(0, math.Min(last, q[a]))
		lo[a] = int(math.Floor(q[a]))
		hi[a] = min(lo[a]+1, g.Dimensions[a]-1)
		f[a] = q[a] - float64(lo[a])
	}
	if outside && policy == PCARejectOutside {
		return 0, true, ErrPCAShapeOutside
	}
	value := 0.0
	for z := 0; z < 2; z++ {
		for y := 0; y < 2; y++ {
			for x := 0; x < 2; x++ {
				pos := lo
				w := 1.0
				for a, b := range [3]int{x, y, z} {
					if b == 1 {
						pos[a] = hi[a]
						w *= f[a]
					} else {
						w *= 1 - f[a]
					}
				}
				i := (pos[2]*g.Dimensions[1]+pos[1])*g.Dimensions[0] + pos[0]
				v := m.data.MeanMM[i]
				for a, c := range coeff {
					v += c * m.data.StandardDeviationsMM[a] * m.data.Components[a][i]
				}
				value += w * v
			}
		}
	}
	if !pcaFinite(value) || math.Abs(value) > 1e6 {
		return 0, outside, ErrPCAShapeInvalid
	}
	return value, outside, nil
}

// Evaluate implements M(T(x))+sum(c_i*sigma_i*P_i(T(x))) with trilinear
// interpolation. It matches the documented ITK PCA representation; it does not
// promise exact signed distances after combining components or extrapolation.
func (m *PCAShapeModel) Evaluate(ctx context.Context, coeff []float64, pose PCAShapePose, points [][3]float64, policy PCAShapeOutsidePolicy, limits PCAShapeLimits) (*PCAShapeEvaluation, error) {
	ctx = pcaContext(ctx)
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if e := m.parameters(coeff, pose, policy); e != nil {
		return nil, e
	}
	l, e := limits.normalize()
	if e != nil {
		return nil, e
	}
	if len(coeff) > l.MaxComponents || len(points) > l.MaxVoxels || int64(len(points))*8+8192 > l.MaxBytes || int64(len(points))*int64(len(coeff)+1)*8 > l.MaxWork {
		return nil, ErrPCAShapeLimit
	}
	r := &PCAShapeEvaluation{FieldMM: make([]float64, len(points))}
	for i, p := range points {
		if i%1024 == 0 {
			if e = ctx.Err(); e != nil {
				return nil, e
			}
		}
		v, out, e := m.value(coeff, pose, policy, p)
		if e != nil {
			return nil, e
		}
		r.FieldMM[i] = v
		if out {
			r.OutsideSamples++
		}
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	return r, nil
}

type PCAShapeFit struct {
	ModelID             string
	CoefficientsSD      []float64
	ResidualRMSMM       float64
	OutsideDistribution bool
}

// Fit projects a canonical-grid field onto the fixed basis without optimizing
// pose. Explicit thresholds flag residual/coefficients; they are not confidence.
func (m *PCAShapeModel) Fit(ctx context.Context, geometry PCAShapeGeometry, field []float64, maxCoefficientSD, maxResidualMM float64, limits PCAShapeLimits) (*PCAShapeFit, error) {
	ctx = pcaContext(ctx)
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if m == nil || geometry != m.data.Geometry || !(maxCoefficientSD > 0 && maxCoefficientSD <= 100) || !(maxResidualMM > 0 && maxResidualMM <= 1e6) || !pcaFinite(maxCoefficientSD) || !pcaFinite(maxResidualMM) {
		return nil, ErrPCAShapeInvalid
	}
	l, e := limits.normalize()
	if e != nil {
		return nil, e
	}
	n, e := geometry.count(l)
	if e != nil {
		return nil, e
	}
	k := len(m.data.Components)
	if k > l.MaxComponents || int64(n)*int64(k+1)*2 > l.MaxWork || int64(k)*8+8192 > l.MaxBytes {
		return nil, ErrPCAShapeLimit
	}
	if e = pcaField(ctx, field, n, true); e != nil {
		return nil, e
	}
	r := &PCAShapeFit{ModelID: m.data.ID, CoefficientsSD: make([]float64, k)}
	for a, u := range m.data.Components {
		dot := 0.0
		for i, v := range u {
			if i%4096 == 0 {
				if e = ctx.Err(); e != nil {
					return nil, e
				}
			}
			dot += (field[i] - m.data.MeanMM[i]) * v
		}
		r.CoefficientsSD[a] = dot / m.data.StandardDeviationsMM[a]
		if math.Abs(r.CoefficientsSD[a]) > maxCoefficientSD {
			r.OutsideDistribution = true
		}
	}
	sum := 0.0
	for i, v := range field {
		if i%4096 == 0 {
			if e = ctx.Err(); e != nil {
				return nil, e
			}
		}
		recon := m.data.MeanMM[i]
		for a, c := range r.CoefficientsSD {
			recon += c * m.data.StandardDeviationsMM[a] * m.data.Components[a][i]
		}
		delta := v - recon
		sum += delta * delta
	}
	r.ResidualRMSMM = math.Sqrt(sum / float64(n))
	r.OutsideDistribution = r.OutsideDistribution || r.ResidualRMSMM > maxResidualMM
	if !pcaFinite(r.ResidualRMSMM) {
		return nil, ErrPCAShapeInvalid
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	return r, nil
}
