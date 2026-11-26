package registration

import (
	"context"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// BSplineJacobianBound bounds the real spline represented by the coefficients,
// in its convex full-support region: 1 <= grid index < GridSize-2. The existing
// identity extension outside support is excluded. Norm/determinants are
// dimensionless; origin/dimensions are millimetres along Direction's columns.
// This is a sufficient condition, not a test that detects every injective map.
type BSplineJacobianBound struct {
	DisplacementGradientNormUpper float64
	PositiveJacobianCertified     bool
	DeterminantLower              float64
	FullSupportOriginMM           render.Vec3
	FullSupportDimensionsMM       render.Vec3
	Direction                     Matrix3
}

// JacobianBound computes adjacent coefficient-difference bounds, transformed
// to physical coordinates and combined in a conservative Frobenius norm q.
// If q<1 with a positive representable lower bound, det(I+grad D)>=(1-q)^3
// and x+D(x) is injective on the convex full-support region. Arithmetic is
// rounded outward; overflow yields an uncertified bound. No dense field or
// sample/Jacobian arrays are allocated. Concurrent coefficient mutation is invalid.
func (b *BSpline) JacobianBound(ctx context.Context) (BSplineJacobianBound, error) {
	if ctx == nil || b == nil || len(b.coefficients) == 0 {
		return BSplineJacobianBound{}, fmt.Errorf("%w: Jacobian bound context/spline", ErrInvalidParameters)
	}
	if err := ctx.Err(); err != nil {
		return BSplineJacobianBound{}, err
	}
	if !b.physicalToIndex.Finite() {
		return BSplineJacobianBound{}, fmt.Errorf("%w: non-finite Jacobian-bound geometry", ErrInvalidParameters)
	}
	count := b.count()
	strides := [3]int{1, b.size[0], b.size[0] * b.size[1]}
	var delta [3][3]float64
	for d := 0; d < 3; d++ {
		v := b.coefficients[d*count : (d+1)*count]
		for z := 0; z < b.size[2]; z++ {
			for y := 0; y < b.size[1]; y++ {
				for x := 0; x < b.size[0]; x++ {
					i := x + y*strides[1] + z*strides[2]
					if i&1023 == 0 {
						if err := ctx.Err(); err != nil {
							return BSplineJacobianBound{}, err
						}
					}
					if !finite(v[i]) {
						return BSplineJacobianBound{}, fmt.Errorf("%w: non-finite Jacobian-bound coefficient", ErrInvalidParameters)
					}
					at := [3]int{x, y, z}
					for a := 0; a < 3; a++ {
						if at[a]+1 < b.size[a] {
							delta[d][a] = math.Max(delta[d][a], jacobianAbsDifferenceUpper(v[i+strides[a]], v[i]))
						}
					}
				}
			}
		}
	}
	q := jacobianGradientNormUpper(delta, b.physicalToIndex)
	out := BSplineJacobianBound{DisplacementGradientNormUpper: q, FullSupportOriginMM: b.origin.Add(b.direction.MulVec(b.spacing)), FullSupportDimensionsMM: render.Vec3{X: float64(b.size[0]-3) * b.spacing.X, Y: float64(b.size[1]-3) * b.spacing.Y, Z: float64(b.size[2]-3) * b.spacing.Z}, Direction: b.direction}
	out.DeterminantLower = jacobianDeterminantLowerFromNorm(q)
	out.PositiveJacobianCertified = out.DeterminantLower > 0 && !math.IsInf(out.DeterminantLower, 0)
	if err := ctx.Err(); err != nil {
		return BSplineJacobianBound{}, err
	}
	return out, nil
}
func jacobianAbsDifferenceUpper(a, b float64) float64 {
	if a == b {
		return 0
	}
	return math.Nextafter(math.Abs(a-b), math.Inf(1))
}
func jacobianMultiplyUpper(a, b float64) float64 {
	if a == 0 || b == 0 {
		return 0
	}
	return math.Nextafter(a*b, math.Inf(1))
}
func jacobianAddUpper(a, b float64) float64 {
	if a == 0 {
		return b
	}
	if b == 0 {
		return a
	}
	return math.Nextafter(a+b, math.Inf(1))
}
func jacobianMultiplyLower(a, b float64) float64 {
	v := a * b
	if !(v > 0) {
		return 0
	}
	return math.Nextafter(v, math.Inf(-1))
}
func jacobianGradientNormUpper(delta [3][3]float64, m Matrix3) float64 {
	q := 0.0
	for d := 0; d < 3; d++ {
		for j := 0; j < 3; j++ {
			v := 0.0
			for a := 0; a < 3; a++ {
				v = jacobianAddUpper(v, jacobianMultiplyUpper(delta[d][a], math.Abs(m[a][j])))
			}
			if v > 0 {
				q = math.Nextafter(math.Hypot(q, v), math.Inf(1))
			}
		}
	}
	return q
}

// Conservative determinant interval for a finite constant matrix. This protects
// the Initial guarantee against cancellation in nearly singular affine matrices.
type jacobianInterval struct{ low, high float64 }

func jacobianIntervalProduct(a, b jacobianInterval) jacobianInterval {
	if a.low == 0 && a.high == 0 || b.low == 0 && b.high == 0 {
		return jacobianInterval{}
	}
	values := [4]float64{a.low * b.low, a.low * b.high, a.high * b.low, a.high * b.high}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return jacobianInterval{math.Inf(-1), math.Inf(1)}
		}
		lo = math.Min(lo, v)
		hi = math.Max(hi, v)
	}
	return jacobianInterval{math.Nextafter(lo, math.Inf(-1)), math.Nextafter(hi, math.Inf(1))}
}
func jacobianMatrixDeterminantLower(m Matrix3) float64 {
	if !m.Finite() {
		return math.Inf(-1)
	}
	permutations := [6][3]int{{0, 1, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}, {1, 0, 2}, {0, 2, 1}}
	sum := jacobianInterval{}
	for n, p := range permutations {
		v := jacobianInterval{1, 1}
		for i, j := range p {
			v = jacobianIntervalProduct(v, jacobianInterval{m[i][j], m[i][j]})
		}
		if n >= 3 {
			v = jacobianInterval{-v.high, -v.low}
		}
		if math.IsInf(v.low, 0) || math.IsInf(v.high, 0) {
			return math.Inf(-1)
		}
		if v.low == 0 && v.high == 0 {
			continue
		}
		sum = jacobianInterval{math.Nextafter(sum.low+v.low, math.Inf(-1)), math.Nextafter(sum.high+v.high, math.Inf(1))}
	}
	return sum.low
}

func jacobianDeterminantLowerFromNorm(q float64) float64 {
	if !(q < 1) {
		return 0
	}
	gap := math.Nextafter(1-q, math.Inf(-1))
	return jacobianMultiplyLower(jacobianMultiplyLower(gap, gap), gap)
}
