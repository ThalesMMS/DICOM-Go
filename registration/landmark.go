package registration

// Point-based registration (#1077): closed-form landmark fits and iterative
// closest point. The rigid fit is Horn's unit-quaternion solution ("Closed-
// form solution of absolute orientation using unit quaternions", JOSA A
// 1987), the similarity scale is the least-squares one of Umeyama (IEEE
// PAMI 1991) and the affine fit is ordinary least squares, the three modes
// of vtkLandmarkTransform and ITK's LandmarkBasedTransformInitializer
// (behaviour only; written from the papers).

import (
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/internal/linalg"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/spatialreg"
)

// ErrLandmarks reports landmark sets that cannot determine a transform:
// different sizes, too few points, non-finite coordinates or a degenerate
// (collinear, or for affine coplanar) configuration.
var ErrLandmarks = errors.New("registration: invalid landmarks")

// LandmarkMode selects the transform family of a landmark fit.
type LandmarkMode int

const (
	// LandmarkRigid fits a rotation and translation (Horn); three
	// non-collinear pairs at least.
	LandmarkRigid LandmarkMode = iota
	// LandmarkSimilarity adds one isotropic scale (Umeyama).
	LandmarkSimilarity
	// LandmarkAffine fits a general linear map and translation by least
	// squares; four non-coplanar pairs at least.
	LandmarkAffine
)

func (m LandmarkMode) String() string {
	switch m {
	case LandmarkRigid:
		return "rigid"
	case LandmarkSimilarity:
		return "similarity"
	case LandmarkAffine:
		return "affine"
	default:
		return fmt.Sprintf("LandmarkMode(%d)", int(m))
	}
}

// LandmarkResult is a landmark fit.
type LandmarkResult struct {
	// Transform maps the fixed points onto the moving points (the ITK
	// convention of this package): a *VersorRigid3D, *Similarity3D or
	// *Affine centred on the fixed centroid. DICOMMatrix turns it into the
	// Frame of Reference Transformation Matrix of a Spatial Registration.
	Transform MatrixTransform
	// FRE is the fiducial registration error: the root mean square of
	// Residuals in mm.
	FRE float64
	// Residuals holds |T(fixed[i]) − moving[i]| per pair.
	Residuals []float64
}

// RegisterLandmarks fits the transform of the given mode that maps each
// fixed point onto its moving counterpart with the least squared error.
func RegisterLandmarks(fixed, moving []render.Vec3, mode LandmarkMode) (LandmarkResult, error) {
	if len(fixed) != len(moving) {
		return LandmarkResult{}, fmt.Errorf("%w: %d fixed and %d moving points", ErrLandmarks, len(fixed), len(moving))
	}
	need := 3
	if mode == LandmarkAffine {
		need = 4
	}
	if mode < LandmarkRigid || mode > LandmarkAffine {
		return LandmarkResult{}, fmt.Errorf("%w: unknown mode %d", ErrLandmarks, mode)
	}
	if len(fixed) < need {
		return LandmarkResult{}, fmt.Errorf("%w: %s fit needs at least %d pairs, got %d", ErrLandmarks, mode, need, len(fixed))
	}
	for i := range fixed {
		if !finiteVec(fixed[i]) || !finiteVec(moving[i]) {
			return LandmarkResult{}, fmt.Errorf("%w: pair %d is not finite", ErrLandmarks, i+1)
		}
	}
	fc, mc := centroid(fixed), centroid(moving)
	// Spread of the fixed points: collinear sets leave the rotation about
	// their line free, coplanar sets leave the affine normal free.
	var cov [3][3]float64
	for _, p := range fixed {
		d := p.Sub(fc)
		v := [3]float64{d.X, d.Y, d.Z}
		for r := 0; r < 3; r++ {
			for c := 0; c < 3; c++ {
				cov[r][c] += v[r] * v[c]
			}
		}
	}
	spread, _, err := linalg.SymmetricEigen3(cov)
	if err != nil {
		return LandmarkResult{}, fmt.Errorf("%w: %v", ErrLandmarks, err)
	}
	largest := math.Max(spread[0], math.Max(spread[1], spread[2]))
	if !(largest > 0) {
		return LandmarkResult{}, fmt.Errorf("%w: the fixed points coincide", ErrLandmarks)
	}
	sorted := sort3(spread)
	if sorted[1] < 1e-10*largest {
		return LandmarkResult{}, fmt.Errorf("%w: the fixed points are collinear", ErrLandmarks)
	}
	if mode == LandmarkAffine && sorted[0] < 1e-10*largest {
		return LandmarkResult{}, fmt.Errorf("%w: an affine fit needs non-coplanar fixed points", ErrLandmarks)
	}

	var m Matrix3
	kind := spatialreg.TransformRigid
	switch mode {
	case LandmarkRigid, LandmarkSimilarity:
		m, err = hornRotation(fixed, moving, fc, mc)
		if err != nil {
			return LandmarkResult{}, err
		}
		if mode == LandmarkSimilarity {
			num, den := 0.0, 0.0
			for i := range fixed {
				f := fixed[i].Sub(fc)
				num += moving[i].Sub(mc).Dot(m.MulVec(f))
				den += f.Dot(f)
			}
			scale := num / den
			if !(scale > 0) {
				return LandmarkResult{}, fmt.Errorf("%w: the similarity scale is not positive", ErrLandmarks)
			}
			m = m.Scale(scale)
			kind = spatialreg.TransformRigidScale
		}
	case LandmarkAffine:
		m, err = leastSquaresLinear(fixed, moving, fc, mc)
		if err != nil {
			return LandmarkResult{}, err
		}
		kind = spatialreg.TransformAffine
	}
	offset := mc.Sub(m.MulVec(fc))
	transform, err := transformFromLinear(m, offset, kind, fc)
	if err != nil {
		return LandmarkResult{}, fmt.Errorf("%w: %v", ErrLandmarks, err)
	}
	result := LandmarkResult{Transform: transform, Residuals: make([]float64, len(fixed))}
	sum := 0.0
	for i := range fixed {
		r := transform.TransformPoint(fixed[i]).Sub(moving[i]).Length()
		result.Residuals[i] = r
		sum += r * r
	}
	result.FRE = math.Sqrt(sum / float64(len(fixed)))
	return result, nil
}

func centroid(points []render.Vec3) render.Vec3 {
	var sum render.Vec3
	for _, p := range points {
		sum = sum.Add(p)
	}
	return sum.Scale(1 / float64(len(points)))
}

func sort3(v [3]float64) [3]float64 {
	if v[0] > v[1] {
		v[0], v[1] = v[1], v[0]
	}
	if v[1] > v[2] {
		v[1], v[2] = v[2], v[1]
	}
	if v[0] > v[1] {
		v[0], v[1] = v[1], v[0]
	}
	return v
}

// hornRotation returns the rotation R minimising Σ|R(fᵢ−f̄) − (mᵢ−m̄)|²: the
// unit quaternion is the eigenvector of the largest eigenvalue of Horn's
// symmetric 4×4 matrix built from the cross-covariance.
func hornRotation(fixed, moving []render.Vec3, fc, mc render.Vec3) (Matrix3, error) {
	var s [3][3]float64 // s[a][b] = Σ f'_a m'_b
	for i := range fixed {
		f := fixed[i].Sub(fc)
		g := moving[i].Sub(mc)
		fv := [3]float64{f.X, f.Y, f.Z}
		gv := [3]float64{g.X, g.Y, g.Z}
		for a := 0; a < 3; a++ {
			for b := 0; b < 3; b++ {
				s[a][b] += fv[a] * gv[b]
			}
		}
	}
	sxx, sxy, sxz := s[0][0], s[0][1], s[0][2]
	syx, syy, syz := s[1][0], s[1][1], s[1][2]
	szx, szy, szz := s[2][0], s[2][1], s[2][2]
	n := [4][4]float64{
		{sxx + syy + szz, syz - szy, szx - sxz, sxy - syx},
		{syz - szy, sxx - syy - szz, sxy + syx, szx + sxz},
		{szx - sxz, sxy + syx, -sxx + syy - szz, syz + szy},
		{sxy - syx, szx + sxz, syz + szy, -sxx - syy + szz},
	}
	values, vectors, err := linalg.SymmetricEigen4(n)
	if err != nil {
		return Matrix3{}, fmt.Errorf("%w: %v", ErrLandmarks, err)
	}
	best := 0
	for i := 1; i < 4; i++ {
		if values[i] > values[best] {
			best = i
		}
	}
	w, x, y, z := vectors[0][best], vectors[1][best], vectors[2][best], vectors[3][best]
	norm := math.Sqrt(w*w + x*x + y*y + z*z)
	if !(norm > 0) {
		return Matrix3{}, fmt.Errorf("%w: no rotation", ErrLandmarks)
	}
	w, x, y, z = w/norm, x/norm, y/norm, z/norm
	return Matrix3{
		{w*w + x*x - y*y - z*z, 2 * (x*y - w*z), 2 * (x*z + w*y)},
		{2 * (x*y + w*z), w*w - x*x + y*y - z*z, 2 * (y*z - w*x)},
		{2 * (x*z - w*y), 2 * (y*z + w*x), w*w - x*x - y*y + z*z},
	}, nil
}

// leastSquaresLinear returns M minimising Σ|M(fᵢ−f̄) − (mᵢ−m̄)|²:
// M = (Σ m'f'ᵀ)(Σ f'f'ᵀ)⁻¹.
func leastSquaresLinear(fixed, moving []render.Vec3, fc, mc render.Vec3) (Matrix3, error) {
	var mf, ff Matrix3
	for i := range fixed {
		f := fixed[i].Sub(fc)
		g := moving[i].Sub(mc)
		fv := [3]float64{f.X, f.Y, f.Z}
		gv := [3]float64{g.X, g.Y, g.Z}
		for r := 0; r < 3; r++ {
			for c := 0; c < 3; c++ {
				mf[r][c] += gv[r] * fv[c]
				ff[r][c] += fv[r] * fv[c]
			}
		}
	}
	inv, err := ff.Inverse()
	if err != nil {
		return Matrix3{}, fmt.Errorf("%w: %v", ErrLandmarks, err)
	}
	return mf.Mul(inv), nil
}
