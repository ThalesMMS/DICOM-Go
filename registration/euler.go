package registration

import (
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// EulerOrder selects how Euler3D composes its three elementary rotations.
type EulerOrder int

const (
	// EulerZXY is R = Rz·Rx·Ry (rotate about Y, then X, then Z), ITK's
	// default and VTK's order.
	EulerZXY EulerOrder = iota
	// EulerZYX is R = Rz·Ry·Rx (rotate about X, then Y, then Z), ITK's
	// ComputeZYX order.
	EulerZYX
)

func (o EulerOrder) String() string {
	switch o {
	case EulerZXY:
		return "ZXY"
	case EulerZYX:
		return "ZYX"
	default:
		return fmt.Sprintf("EulerOrder(%d)", int(o))
	}
}

// Euler3D is a rigid transform parameterized by three rotation angles in
// radians about the X, Y and Z axes and a translation, after ITK's
// Euler3DTransform:
//
//	parameters       [angleX, angleY, angleZ, tx, ty, tz]
//	fixed parameters [cx, cy, cz, order] (order 0 = ZXY, 1 = ZYX)
//
// T(x) = R·(x − c) + c + t. The angles are updated additively.
type Euler3D struct {
	matrixOffset
	angleX, angleY, angleZ float64
	order                  EulerOrder
	// derivatives caches ∂R/∂angleX, ∂R/∂angleY and ∂R/∂angleZ.
	derivatives [3]Matrix3
}

// NewEuler3D returns the identity Euler3D in ZXY order centred at the
// origin.
func NewEuler3D() *Euler3D {
	e := &Euler3D{matrixOffset: identityMatrixOffset()}
	e.computeMatrix()
	return e
}

// Kind returns KindEuler3D.
func (e *Euler3D) Kind() Kind { return KindEuler3D }

// Angles returns the rotation angles in radians.
func (e *Euler3D) Angles() (x, y, z float64) { return e.angleX, e.angleY, e.angleZ }

// Order returns the rotation order.
func (e *Euler3D) Order() EulerOrder { return e.order }

// SetRotation sets the angles in radians, keeping centre and translation.
func (e *Euler3D) SetRotation(x, y, z float64) error {
	if !finite(x) || !finite(y) || !finite(z) {
		return fmt.Errorf("%w: non-finite Euler angle", ErrInvalidParameters)
	}
	e.angleX, e.angleY, e.angleZ = x, y, z
	e.computeMatrix()
	e.computeOffset()
	return nil
}

// SetOrder changes the rotation order while keeping the angles, so the
// mapping changes (ITK's SetComputeZYX).
func (e *Euler3D) SetOrder(order EulerOrder) error {
	if order != EulerZXY && order != EulerZYX {
		return fmt.Errorf("%w: unknown Euler order %d", ErrInvalidParameters, int(order))
	}
	e.order = order
	e.computeMatrix()
	e.computeOffset()
	return nil
}

// SetIdentity resets the angles and translation, keeping centre and order.
func (e *Euler3D) SetIdentity() {
	e.angleX, e.angleY, e.angleZ = 0, 0, 0
	e.translation = render.Vec3{}
	e.computeMatrix()
	e.computeOffset()
}

// SetMatrix sets the rotation from a proper rotation matrix, keeping the
// translation (so the offset is recomputed) and deriving the angles for the
// current order. It rejects matrices that are not orthogonal within
// DefaultOrthogonalityTolerance or that are reflections.
func (e *Euler3D) SetMatrix(m Matrix3) error {
	return e.SetMatrixWithTolerance(m, DefaultOrthogonalityTolerance)
}

// SetMatrixWithTolerance is SetMatrix with a caller-chosen tolerance on
// M·Mᵀ = I; the stored rotation is rebuilt from the derived angles, so it is
// orthonormal to rounding.
func (e *Euler3D) SetMatrixWithTolerance(m Matrix3, tolerance float64) error {
	if err := checkRotation(m, tolerance); err != nil {
		return err
	}
	e.anglesFromMatrix(m)
	e.computeMatrix()
	e.computeOffset()
	return nil
}

// NumberOfParameters returns 6.
func (e *Euler3D) NumberOfParameters() int { return 6 }

// Parameters returns [angleX, angleY, angleZ, tx, ty, tz].
func (e *Euler3D) Parameters() []float64 {
	t := e.translation
	return []float64{e.angleX, e.angleY, e.angleZ, t.X, t.Y, t.Z}
}

// SetParameters sets [angleX, angleY, angleZ, tx, ty, tz].
func (e *Euler3D) SetParameters(parameters []float64) error {
	if err := checkLength(parameters, 6, "Euler3D parameters"); err != nil {
		return err
	}
	e.angleX, e.angleY, e.angleZ = parameters[0], parameters[1], parameters[2]
	e.translation = vecFrom(parameters[3:])
	e.computeMatrix()
	e.computeOffset()
	return nil
}

// FixedParameters returns [cx, cy, cz, order], order being 1 for ZYX.
func (e *Euler3D) FixedParameters() []float64 {
	order := 0.0
	if e.order == EulerZYX {
		order = 1
	}
	return append(e.centerFixedParameters(), order)
}

// SetFixedParameters accepts the centre alone (three values, keeping the
// order, as older ITK files store it) or the centre and the order flag
// (four values, non-zero meaning ZYX). The translation is kept, as in ITK.
func (e *Euler3D) SetFixedParameters(fixed []float64) error {
	if len(fixed) != 3 && len(fixed) != 4 {
		return fmt.Errorf("%w: Euler3D fixed parameters have %d values, want 3 or 4", ErrInvalidParameters, len(fixed))
	}
	if !finiteSlice(fixed) {
		return fmt.Errorf("%w: non-finite Euler3D fixed parameter", ErrInvalidParameters)
	}
	e.center = vecFrom(fixed)
	if len(fixed) == 4 {
		e.order = EulerZXY
		if fixed[3] != 0 {
			e.order = EulerZYX
		}
		e.computeMatrix()
	}
	e.computeOffset()
	return nil
}

// UpdateParameters adds factor·update to the parameters.
func (e *Euler3D) UpdateParameters(update []float64, factor float64) error {
	return addUpdate(e, update, factor)
}

// JacobianWithRespectToParameters writes ∂T(p)/∂[angles, translation].
func (e *Euler3D) JacobianWithRespectToParameters(point render.Vec3, out *Jacobian) {
	out.reset(6)
	rotationTranslationJacobian(out, e.derivatives[:], point.Sub(e.center), 3)
}

// Clone returns an independent copy.
func (e *Euler3D) Clone() Transform {
	c := *e
	return &c
}

// Inverse returns the Euler3D mapping moving points back to fixed points,
// with the same centre and order.
func (e *Euler3D) Inverse() (*Euler3D, error) {
	m, offset, err := e.inverse()
	if err != nil {
		return nil, err
	}
	out := &Euler3D{matrixOffset: identityMatrixOffset(), order: e.order}
	out.center = e.center
	if err := out.SetMatrixWithTolerance(m, 1e-8); err != nil {
		return nil, err
	}
	if err := out.SetOffset(offset); err != nil {
		return nil, err
	}
	return out, nil
}

func (e *Euler3D) computeMatrix() {
	rx, drx := axisRotation(0, e.angleX)
	ry, dry := axisRotation(1, e.angleY)
	rz, drz := axisRotation(2, e.angleZ)
	if e.order == EulerZYX {
		e.matrix = rz.Mul(ry).Mul(rx)
		e.derivatives = [3]Matrix3{rz.Mul(ry).Mul(drx), rz.Mul(dry).Mul(rx), drz.Mul(ry).Mul(rx)}
		return
	}
	e.matrix = rz.Mul(rx).Mul(ry)
	e.derivatives = [3]Matrix3{rz.Mul(drx).Mul(ry), rz.Mul(rx).Mul(dry), drz.Mul(rx).Mul(ry)}
}

// anglesFromMatrix derives the angles of a rotation matrix for the current
// order. It follows ITK's Euler3DTransform::ComputeMatrixParameters,
// including its gimbal-lock threshold |cos| ≤ 5e-5 where the first angle is
// set to zero; asin arguments are clamped against rounding.
func (e *Euler3D) anglesFromMatrix(m Matrix3) {
	const gimbal = 0.00005
	if e.order == EulerZYX {
		e.angleY = -math.Asin(clampUnit(m[2][0]))
		c := math.Cos(e.angleY)
		if math.Abs(c) > gimbal {
			e.angleX = math.Atan2(m[2][1]/c, m[2][2]/c)
			e.angleZ = math.Atan2(m[1][0]/c, m[0][0]/c)
		} else {
			e.angleX = 0
			e.angleZ = math.Atan2(-m[0][1], m[1][1])
		}
		return
	}
	e.angleX = math.Asin(clampUnit(m[2][1]))
	a := math.Cos(e.angleX)
	if math.Abs(a) > gimbal {
		e.angleY = math.Atan2(-m[2][0]/a, m[2][2]/a)
		e.angleZ = math.Atan2(-m[0][1]/a, m[1][1]/a)
	} else {
		e.angleZ = 0
		e.angleY = math.Atan2(m[1][0], m[0][0])
	}
}

// axisRotation returns the rotation by angle about axis (0 = X, 1 = Y,
// 2 = Z) and its derivative with respect to the angle.
func axisRotation(axis int, angle float64) (Matrix3, Matrix3) {
	c, s := math.Cos(angle), math.Sin(angle)
	switch axis {
	case 0:
		return Matrix3{{1, 0, 0}, {0, c, -s}, {0, s, c}}, Matrix3{{0, 0, 0}, {0, -s, -c}, {0, c, -s}}
	case 1:
		return Matrix3{{c, 0, s}, {0, 1, 0}, {-s, 0, c}}, Matrix3{{-s, 0, c}, {0, 0, 0}, {-c, 0, -s}}
	default:
		return Matrix3{{c, -s, 0}, {s, c, 0}, {0, 0, 1}}, Matrix3{{-s, -c, 0}, {c, -s, 0}, {0, 0, 0}}
	}
}

// checkRotation rejects non-finite, non-orthogonal and reflecting matrices.
func checkRotation(m Matrix3, tolerance float64) error {
	if !m.Finite() || !finite(tolerance) || tolerance < 0 {
		return fmt.Errorf("%w: non-finite rotation matrix or tolerance", ErrInvalidMatrix)
	}
	if !m.orthogonal(tolerance) {
		return fmt.Errorf("%w: matrix is not orthogonal within %g", ErrInvalidMatrix, tolerance)
	}
	if m.Determinant() <= 0 {
		return fmt.Errorf("%w: matrix is a reflection", ErrInvalidMatrix)
	}
	return nil
}

func clampUnit(v float64) float64 { return math.Max(-1, math.Min(1, v)) }

// addUpdate applies p ← p + factor·update through SetParameters.
func addUpdate(t Transform, update []float64, factor float64) error {
	parameters := t.Parameters()
	if err := checkUpdate(update, len(parameters), factor); err != nil {
		return err
	}
	for i := range parameters {
		parameters[i] += factor * update[i]
	}
	return t.SetParameters(parameters)
}

func checkUpdate(update []float64, want int, factor float64) error {
	if len(update) != want {
		return fmt.Errorf("%w: update has %d values, want %d", ErrInvalidParameters, len(update), want)
	}
	if !finite(factor) || !finiteSlice(update) {
		return fmt.Errorf("%w: non-finite update", ErrInvalidParameters)
	}
	return nil
}
