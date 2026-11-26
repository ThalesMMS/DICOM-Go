package registration

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

var (
	ErrLandmarkWarpInput      = errors.New("registration: invalid landmark warp input")
	ErrLandmarkWarpLimit      = errors.New("registration: landmark warp resource limit")
	ErrLandmarkWarpSystem     = errors.New("registration: degenerate or ill-conditioned landmark warp")
	ErrLandmarkWarpDerivative = errors.New("registration: radial warp derivative undefined at weighted center")
)

// LandmarkWarpOptions selects the 3D polyharmonic kernel -r with an affine
// displacement tail. Regularization is nonnegative in physical mm; zero
// interpolates. Zero limits select finite defaults, never unlimited.
type LandmarkWarpOptions struct {
	Regularization                                                      float64
	MaxLandmarks                                                        int
	MaxMatrixBytes, MaxScratchBytes, MaxSolveWork, MaxQueryInteractions int64
	MaxQueries                                                          int
	MaxQueryBytes                                                       int64
	MaxCondition                                                        float64
}

// LandmarkWarpDiagnostics describes the normalized augmented system. Condition
// is ||A||inf*||inverse(A)||inf computed numerically, not a rigorous certificate.
// Bytes/Work are logical numeric payload/admission bounds, not allocator/RSS.
type LandmarkWarpDiagnostics struct {
	Condition                            float64
	MatrixBytes, ScratchBytes, SolveWork int64
	ScaleMM, RegularizationMM, FRE       float64
	Residuals                            []float64
}

type LandmarkWarpResult struct {
	Warp        *LandmarkWarp3D
	Diagnostics LandmarkWarpDiagnostics
}

// LandmarkWarp3D is an immutable nonparametric fixed→moving physical-space map.
// It deliberately does not implement Transform/MatrixTransform: no inverse,
// parameter gradient, matrix serialization or diffeomorphism is asserted.
type LandmarkWarp3D struct {
	centers           []render.Vec3
	weights           []render.Vec3
	affine            [4]render.Vec3
	origin, low, high render.Vec3
	scale             float64
	options           LandmarkWarpOptions
}

func warpOptions(o LandmarkWarpOptions) (LandmarkWarpOptions, error) {
	if !finite(o.Regularization) || o.Regularization < 0 || o.MaxLandmarks < 0 || o.MaxMatrixBytes < 0 || o.MaxScratchBytes < 0 || o.MaxSolveWork < 0 || o.MaxQueryInteractions < 0 || o.MaxQueries < 0 || o.MaxQueryBytes < 0 || !finite(o.MaxCondition) || o.MaxCondition < 0 {
		return o, ErrLandmarkWarpInput
	}
	if o.MaxLandmarks == 0 {
		o.MaxLandmarks = 128
	}
	if o.MaxLandmarks > 1024 {
		return o, ErrLandmarkWarpLimit
	}
	if o.MaxMatrixBytes == 0 {
		o.MaxMatrixBytes = 16 << 20
	}
	if o.MaxScratchBytes == 0 {
		o.MaxScratchBytes = 16 << 20
	}
	if o.MaxSolveWork == 0 {
		o.MaxSolveWork = 64 << 20
	}
	if o.MaxQueryInteractions == 0 {
		o.MaxQueryInteractions = 1 << 20
	}
	if o.MaxQueries == 0 {
		o.MaxQueries = 4096
	}
	if o.MaxQueryBytes == 0 {
		o.MaxQueryBytes = 1 << 20
	}
	if o.MaxCondition == 0 {
		o.MaxCondition = 1e10
	}
	if o.MaxCondition < 1 {
		return o, ErrLandmarkWarpInput
	}
	return o, nil
}

// FitLandmarkWarp3D fits paired 3D landmarks. No result.Warp is returned on
// any error; diagnostics obtained before a failure may still be returned.
func FitLandmarkWarp3D(ctx context.Context, fixed, moving []render.Vec3, o LandmarkWarpOptions) (result LandmarkWarpResult, err error) {
	if ctx == nil {
		return result, ErrLandmarkWarpInput
	}
	if err = ctx.Err(); err != nil {
		return
	}
	o, err = warpOptions(o)
	if err != nil {
		return
	}
	n := len(fixed)
	if n != len(moving) || n < 4 {
		return result, ErrLandmarkWarpInput
	}
	if n > o.MaxLandmarks {
		return result, ErrLandmarkWarpLimit
	}
	d := int64(n + 4)
	width := 2*d + 3
	// Hard1024 admission makes these checked arithmetic bounds representable.
	matrixBytes := 8 * d * d
	scratchBytes := 8*d*(d+3) + int64(n)*104
	work := d * d * width
	result.Diagnostics = LandmarkWarpDiagnostics{MatrixBytes: matrixBytes, ScratchBytes: scratchBytes, SolveWork: work, RegularizationMM: o.Regularization}
	if matrixBytes > o.MaxMatrixBytes || scratchBytes > o.MaxScratchBytes || work > o.MaxSolveWork {
		return result, ErrLandmarkWarpLimit
	}
	low, high := fixed[0], fixed[0]
	for i, p := range fixed {
		if err = ctx.Err(); err != nil {
			return
		}
		if !finiteVec(p) || !finiteVec(moving[i]) {
			return result, fmt.Errorf("%w: nonfinite pair %d", ErrLandmarkWarpInput, i)
		}
		low = render.Vec3{X: math.Min(low.X, p.X), Y: math.Min(low.Y, p.Y), Z: math.Min(low.Z, p.Z)}
		high = render.Vec3{X: math.Max(high.X, p.X), Y: math.Max(high.Y, p.Y), Z: math.Max(high.Z, p.Z)}
		for j := 0; j < i; j++ {
			if p == fixed[j] {
				return result, fmt.Errorf("%w: duplicate fixed pair %d and %d (conflict=%t)", ErrLandmarkWarpInput, j, i, moving[j] != moving[i])
			}
		}
	}
	origin := low.Scale(.5).Add(high.Scale(.5))
	span := high.Sub(low)
	scale := math.Max(span.X, math.Max(span.Y, span.Z))
	if !(scale > 0) || !finite(scale) {
		return result, ErrLandmarkWarpSystem
	}
	result.Diagnostics.ScaleMM = scale
	w := &LandmarkWarp3D{centers: make([]render.Vec3, n), weights: make([]render.Vec3, n), origin: origin, low: low, high: high, scale: scale, options: o}
	dim, stride := int(d), int(width)
	a := make([]float64, dim*stride)
	norm := 0.0
	lambda := o.Regularization / scale
	if !finite(lambda) {
		return result, ErrLandmarkWarpSystem
	}
	for i, p := range fixed {
		if err = ctx.Err(); err != nil {
			return
		}
		w.centers[i] = p.Sub(origin).Scale(1 / scale)
		if !finiteVec(w.centers[i]) {
			return result, ErrLandmarkWarpSystem
		}
	}
	for i := 0; i < dim; i++ {
		if err = ctx.Err(); err != nil {
			return
		}
		if i < n {
			q := w.centers[i]
			for j := 0; j < n; j++ {
				a[i*stride+j] = -warpNorm(q.Sub(w.centers[j]))
			}
			a[i*stride+i] += lambda
			affine := [4]float64{1, q.X, q.Y, q.Z}
			for j, v := range affine {
				a[i*stride+n+j] = v
				a[(n+j)*stride+i] = v
			}
			rhs := moving[i].Sub(fixed[i]).Scale(1 / scale)
			if !finiteVec(rhs) {
				return result, ErrLandmarkWarpSystem
			}
			a[i*stride+2*dim] = rhs.X
			a[i*stride+2*dim+1] = rhs.Y
			a[i*stride+2*dim+2] = rhs.Z
		}
		a[i*stride+dim+i] = 1
	}
	for i := 0; i < dim; i++ {
		row := 0.0
		for j := 0; j < dim; j++ {
			row += math.Abs(a[i*stride+j])
		}
		norm = math.Max(norm, row)
	}
	// General indefinite saddle-point system: partial pivoting, not SPD Cholesky.
	for k := 0; k < dim; k++ {
		if err = ctx.Err(); err != nil {
			return
		}
		pivot := k
		for i := k + 1; i < dim; i++ {
			if math.Abs(a[i*stride+k]) > math.Abs(a[pivot*stride+k]) {
				pivot = i
			}
		}
		pv := a[pivot*stride+k]
		if !finite(pv) || math.Abs(pv) <= 1e-15*norm {
			return result, fmt.Errorf("%w: rank/pivot %d", ErrLandmarkWarpSystem, k)
		}
		if pivot != k {
			for j := 0; j < stride; j++ {
				a[k*stride+j], a[pivot*stride+j] = a[pivot*stride+j], a[k*stride+j]
			}
		}
		for j := 0; j < stride; j++ {
			a[k*stride+j] /= pv
		}
		for i := 0; i < dim; i++ {
			if err = ctx.Err(); err != nil {
				return
			}
			if i == k {
				continue
			}
			f := a[i*stride+k]
			for j := 0; j < stride; j++ {
				a[i*stride+j] -= f * a[k*stride+j]
			}
		}
	}
	invNorm := 0.0
	for i := 0; i < dim; i++ {
		row := 0.0
		for j := 0; j < dim; j++ {
			row += math.Abs(a[i*stride+dim+j])
		}
		invNorm = math.Max(invNorm, row)
	}
	condition := norm * invNorm
	result.Diagnostics.Condition = condition
	if !finite(condition) || condition > o.MaxCondition {
		return result, fmt.Errorf("%w: normalized condition %g", ErrLandmarkWarpSystem, condition)
	}
	for i := 0; i < dim; i++ {
		v := render.Vec3{X: a[i*stride+2*dim], Y: a[i*stride+2*dim+1], Z: a[i*stride+2*dim+2]}
		if !finiteVec(v) {
			return result, ErrLandmarkWarpSystem
		}
		if i < n {
			w.weights[i] = v
		} else {
			w.affine[i-n] = v
		}
	}
	result.Diagnostics.Residuals = make([]float64, n)
	rms := 0.0
	for i, p := range fixed {
		var got render.Vec3
		got, err = w.point(ctx, p)
		if err != nil {
			return
		}
		v := warpNorm(got.Sub(moving[i]))
		if !finite(v) {
			return result, ErrLandmarkWarpSystem
		}
		result.Diagnostics.Residuals[i] = v
		rms = math.Hypot(rms, v)
	}
	result.Diagnostics.FRE = rms / math.Sqrt(float64(n))
	if !finite(result.Diagnostics.FRE) {
		return result, ErrLandmarkWarpSystem
	}
	if err = ctx.Err(); err != nil {
		return
	}
	result.Warp = w
	return
}

func warpNorm(v render.Vec3) float64 { return math.Hypot(math.Hypot(v.X, v.Y), v.Z) }
func (w *LandmarkWarp3D) valid(ctx context.Context, p render.Vec3) error {
	if ctx == nil || w == nil || len(w.centers) < 4 || !finiteVec(p) {
		return ErrLandmarkWarpInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if int64(len(w.centers)) > w.options.MaxQueryInteractions {
		return ErrLandmarkWarpLimit
	}
	return nil
}
func (w *LandmarkWarp3D) point(ctx context.Context, p render.Vec3) (render.Vec3, error) {
	q := p.Sub(w.origin).Scale(1 / w.scale)
	if !finiteVec(q) {
		return render.Vec3{}, ErrLandmarkWarpInput
	}
	v := w.affine[0].Add(w.affine[1].Scale(q.X)).Add(w.affine[2].Scale(q.Y)).Add(w.affine[3].Scale(q.Z))
	for i, c := range w.centers {
		if i%32 == 0 {
			if err := ctx.Err(); err != nil {
				return render.Vec3{}, err
			}
		}
		v = v.Add(w.weights[i].Scale(-warpNorm(q.Sub(c))))
	}
	out := p.Add(v.Scale(w.scale))
	if !finiteVec(out) {
		return render.Vec3{}, ErrLandmarkWarpSystem
	}
	if err := ctx.Err(); err != nil {
		return render.Vec3{}, err
	}
	return out, nil
}

// PointContext evaluates one point without query scratch or shared mutation.
func (w *LandmarkWarp3D) PointContext(ctx context.Context, p render.Vec3) (render.Vec3, error) {
	if err := w.valid(ctx, p); err != nil {
		return render.Vec3{}, err
	}
	return w.point(ctx, p)
}

// JacobianContext gives the position Jacobian away from weighted kernel centers.
// The -r kernel has a cusp at r=0; no zero gradient is invented there.
func (w *LandmarkWarp3D) JacobianContext(ctx context.Context, p render.Vec3) (Matrix3, error) {
	if err := w.valid(ctx, p); err != nil {
		return Matrix3{}, err
	}
	q := p.Sub(w.origin).Scale(1 / w.scale)
	if !finiteVec(q) {
		return Matrix3{}, ErrLandmarkWarpInput
	}
	j := Identity3()
	for c := 0; c < 3; c++ {
		v := w.affine[c+1]
		j[0][c] += v.X
		j[1][c] += v.Y
		j[2][c] += v.Z
	}
	for i, c := range w.centers {
		if i%32 == 0 {
			if err := ctx.Err(); err != nil {
				return Matrix3{}, err
			}
		}
		d := q.Sub(c)
		r := warpNorm(d)
		v := w.weights[i]
		if r == 0 {
			if v != (render.Vec3{}) {
				return Matrix3{}, ErrLandmarkWarpDerivative
			}
			continue
		}
		grad := [3]float64{-d.X / r, -d.Y / r, -d.Z / r}
		for k, g := range grad {
			j[0][k] += v.X * g
			j[1][k] += v.Y * g
			j[2][k] += v.Z * g
		}
	}
	if !j.Finite() {
		return Matrix3{}, ErrLandmarkWarpSystem
	}
	if err := ctx.Err(); err != nil {
		return Matrix3{}, err
	}
	return j, nil
}

// LandmarkWarpSample is a sampled observation, not a global topology proof.
// OutsideLandmarkBox reports AABB extrapolation only, not convex-hull membership.
// A derivative cusp leaves JacobianDefined=false and no fold certification.
type LandmarkWarpSample struct {
	Mapped                                                   render.Vec3
	Jacobian                                                 Matrix3
	Determinant                                              float64
	JacobianDefined, NonPositiveJacobian, OutsideLandmarkBox bool
}

// InspectContext evaluates a bounded batch transactionally. Errors return nil
// samples. Cusp samples are retained explicitly with JacobianDefined=false.
func (w *LandmarkWarp3D) InspectContext(ctx context.Context, points []render.Vec3) ([]LandmarkWarpSample, error) {
	if ctx == nil || w == nil || len(w.centers) < 4 || !(w.scale > 0) {
		return nil, ErrLandmarkWarpInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n := len(points)
	if n > w.options.MaxQueries || int64(n) > w.options.MaxQueryInteractions/int64(len(w.centers)) || int64(n) > w.options.MaxQueryBytes/112 {
		return nil, ErrLandmarkWarpLimit
	}
	for _, p := range points {
		if err := w.valid(ctx, p); err != nil {
			return nil, err
		}
	}
	out := make([]LandmarkWarpSample, n)
	for i, p := range points {
		v, err := w.PointContext(ctx, p)
		if err != nil {
			return nil, err
		}
		j, err := w.JacobianContext(ctx, p)
		if err != nil && !errors.Is(err, ErrLandmarkWarpDerivative) {
			return nil, err
		}
		s := LandmarkWarpSample{Mapped: v, OutsideLandmarkBox: p.X < w.low.X || p.X > w.high.X || p.Y < w.low.Y || p.Y > w.high.Y || p.Z < w.low.Z || p.Z > w.high.Z}
		if err == nil {
			s.Jacobian = j
			s.Determinant = j.Determinant()
			if !finite(s.Determinant) {
				return nil, ErrLandmarkWarpSystem
			}
			s.JacobianDefined = true
			s.NonPositiveJacobian = s.Determinant <= 0
		}
		out[i] = s
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
