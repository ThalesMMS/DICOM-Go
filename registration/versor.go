package registration

import (
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// Versor is a unit quaternion W + X·i + Y·j + Z·k representing a rotation
// by angle θ about a unit axis a: (X, Y, Z) = a·sin(θ/2), W = cos(θ/2).
type Versor struct {
	X, Y, Z, W float64
}

// IdentityVersor returns the rotation by zero.
func IdentityVersor() Versor { return Versor{W: 1} }

// VersorFromAxisAngle returns the rotation by angle radians about axis,
// which need not be unit length but must be non-zero.
func VersorFromAxisAngle(axis render.Vec3, angle float64) (Versor, error) {
	norm := axis.Length()
	if !finiteVec(axis) || !finite(angle) || norm == 0 || !finite(norm) {
		return Versor{}, fmt.Errorf("%w: rotation axis must be finite and non-zero", ErrInvalidParameters)
	}
	s := math.Sin(angle/2) / norm
	return Versor{X: axis.X * s, Y: axis.Y * s, Z: axis.Z * s, W: math.Cos(angle / 2)}, nil
}

// VersorFromRightPart returns the versor whose vector part is right and
// whose scalar part is the non-negative √(1 − |right|²), ITK's
// Versor::Set(VectorType). |right| must not exceed one.
func VersorFromRightPart(right render.Vec3) (Versor, error) {
	if !finiteVec(right) {
		return Versor{}, fmt.Errorf("%w: non-finite versor", ErrInvalidParameters)
	}
	n2 := right.Dot(right)
	if n2 > 1 {
		return Versor{}, fmt.Errorf("%w: versor right part has norm %g > 1", ErrInvalidParameters, math.Sqrt(n2))
	}
	return Versor{X: right.X, Y: right.Y, Z: right.Z, W: math.Sqrt(1 - n2)}, nil
}

// VersorFromMatrix converts a proper rotation matrix (orthogonal within
// tolerance, determinant positive) with Shepperd's method: the largest of
// the four squared components is extracted first, which stays accurate for
// rotations near π. The result has W ≥ 0. ITK's Versor::Set(MatrixType)
// negates W in its non-trace branches (angles near π); this conversion does
// not, so the versor always reproduces the matrix.
func VersorFromMatrix(m Matrix3, tolerance float64) (Versor, error) {
	if err := checkRotation(m, tolerance); err != nil {
		return Versor{}, err
	}
	var q Versor
	trace := m[0][0] + m[1][1] + m[2][2]
	switch {
	case trace > m[0][0] && trace > m[1][1] && trace > m[2][2]:
		s := 2 * math.Sqrt(1+trace)
		q = Versor{W: s / 4, X: (m[2][1] - m[1][2]) / s, Y: (m[0][2] - m[2][0]) / s, Z: (m[1][0] - m[0][1]) / s}
	case m[0][0] >= m[1][1] && m[0][0] >= m[2][2]:
		s := 2 * math.Sqrt(1+m[0][0]-m[1][1]-m[2][2])
		q = Versor{X: s / 4, Y: (m[0][1] + m[1][0]) / s, Z: (m[0][2] + m[2][0]) / s, W: (m[2][1] - m[1][2]) / s}
	case m[1][1] >= m[2][2]:
		s := 2 * math.Sqrt(1+m[1][1]-m[0][0]-m[2][2])
		q = Versor{X: (m[0][1] + m[1][0]) / s, Y: s / 4, Z: (m[1][2] + m[2][1]) / s, W: (m[0][2] - m[2][0]) / s}
	default:
		s := 2 * math.Sqrt(1+m[2][2]-m[0][0]-m[1][1])
		q = Versor{X: (m[0][2] + m[2][0]) / s, Y: (m[1][2] + m[2][1]) / s, Z: s / 4, W: (m[1][0] - m[0][1]) / s}
	}
	q, ok := q.normalized()
	if !ok {
		return Versor{}, fmt.Errorf("%w: cannot convert matrix to a versor", ErrInvalidMatrix)
	}
	return q.canonical(), nil
}

// Mul returns the Hamilton product q·r: the rotation that applies r first
// and then q.
func (q Versor) Mul(r Versor) Versor {
	return Versor{
		X: q.W*r.X + q.X*r.W + q.Y*r.Z - q.Z*r.Y,
		Y: q.W*r.Y - q.X*r.Z + q.Y*r.W + q.Z*r.X,
		Z: q.W*r.Z + q.X*r.Y - q.Y*r.X + q.Z*r.W,
		W: q.W*r.W - q.X*r.X - q.Y*r.Y - q.Z*r.Z,
	}
}

// Conjugate returns the inverse rotation of a unit versor.
func (q Versor) Conjugate() Versor { return Versor{X: -q.X, Y: -q.Y, Z: -q.Z, W: q.W} }

// Right returns the vector part (X, Y, Z).
func (q Versor) Right() render.Vec3 { return render.Vec3{X: q.X, Y: q.Y, Z: q.Z} }

// Angle returns the rotation angle in radians, in [0, 2π].
func (q Versor) Angle() float64 { return 2 * math.Atan2(q.Right().Length(), q.W) }

// Axis returns the unit rotation axis, or the zero vector for the identity.
func (q Versor) Axis() render.Vec3 { return q.Right().Normalize() }

// Matrix returns the rotation matrix of a unit versor.
func (q Versor) Matrix() Matrix3 {
	xx, yy, zz := q.X*q.X, q.Y*q.Y, q.Z*q.Z
	xy, xz, xw := q.X*q.Y, q.X*q.Z, q.X*q.W
	yz, yw, zw := q.Y*q.Z, q.Y*q.W, q.Z*q.W
	return Matrix3{
		{1 - 2*(yy+zz), 2 * (xy - zw), 2 * (xz + yw)},
		{2 * (xy + zw), 1 - 2*(xx+zz), 2 * (yz - xw)},
		{2 * (xz - yw), 2 * (yz + xw), 1 - 2*(xx+yy)},
	}
}

// rightPartDerivatives returns ∂R/∂X, ∂R/∂Y and ∂R/∂Z of the rotation
// matrix of a unit versor parameterized by its right part, W being
// √(1 − X² − Y² − Z²) so that ∂W/∂v = −v/W. The derivative is singular at
// W = 0 (a rotation by π), where the right-part parameterization is.
func (q Versor) rightPartDerivatives() [3]Matrix3 {
	x, y, z, w := q.X, q.Y, q.Z, q.W
	// Partial derivatives with W held fixed.
	dx := Matrix3{{0, 2 * y, 2 * z}, {2 * y, -4 * x, -2 * w}, {2 * z, 2 * w, -4 * x}}
	dy := Matrix3{{-4 * y, 2 * x, 2 * w}, {2 * x, 0, 2 * z}, {-2 * w, 2 * z, -4 * y}}
	dz := Matrix3{{-4 * z, -2 * w, 2 * x}, {2 * w, -4 * z, 2 * y}, {2 * x, 2 * y, 0}}
	dw := Matrix3{{0, -2 * z, 2 * y}, {2 * z, 0, -2 * x}, {-2 * y, 2 * x, 0}}
	out := [3]Matrix3{dx, dy, dz}
	components := [3]float64{x, y, z}
	for k := range out {
		chain := -components[k] / w
		for i := 0; i < 3; i++ {
			for j := 0; j < 3; j++ {
				out[k][i][j] += dw[i][j] * chain
			}
		}
	}
	return out
}

func (q Versor) normalized() (Versor, bool) {
	n := math.Sqrt(q.X*q.X + q.Y*q.Y + q.Z*q.Z + q.W*q.W)
	if !finite(n) || n < 1e-20 {
		return Versor{}, false
	}
	return Versor{X: q.X / n, Y: q.Y / n, Z: q.Z / n, W: q.W / n}, true
}

// canonical returns the representative with W ≥ 0; q and −q are the same
// rotation, and the right-part parameterization assumes W ≥ 0.
func (q Versor) canonical() Versor {
	if q.W < 0 {
		return Versor{X: -q.X, Y: -q.Y, Z: -q.Z, W: -q.W}
	}
	return q
}

// versorFromParameters builds the versor of parameters[0:3] as ITK's
// VersorRigid3DTransform::SetParameters does: a right part whose norm
// reaches 1 − 1e-10 (including one outside the ball) is scaled just inside
// the unit ball.
func versorFromParameters(parameters []float64) (Versor, error) {
	right := vecFrom(parameters)
	const epsilon = 1e-10
	if norm := right.Length(); norm >= 1-epsilon {
		right = right.Scale(1 / (norm + epsilon*norm))
	}
	return VersorFromRightPart(right)
}

// composeVersorUpdate returns current·exp(factor·update) as ITK's
// VersorRigid3DTransform::UpdateTransformParameters does: the update is a
// rotation about update[0:3] by factor·|update[0:3]| radians applied before
// the current rotation. The result is canonicalized to W ≥ 0; ITK keeps
// only the right part of a product with W < 0, which silently mirrors the
// rotation angle through π.
func composeVersorUpdate(current Versor, update []float64, factor float64) (Versor, error) {
	axis := vecFrom(update)
	norm := axis.Length()
	step := IdentityVersor()
	if norm != 0 {
		var err error
		step, err = VersorFromAxisAngle(axis, factor*norm)
		if err != nil {
			return Versor{}, err
		}
	}
	next, ok := current.Mul(step).normalized()
	if !ok {
		return Versor{}, fmt.Errorf("%w: versor update degenerated", ErrInvalidParameters)
	}
	return next.canonical(), nil
}
