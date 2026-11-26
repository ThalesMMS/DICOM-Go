package registration

import (
	"fmt"

	"github.com/ThalesMMS/dicom-go/render"
)

// VersorRigid3D is a rigid transform parameterized by the right part of a
// versor and a translation, after ITK's VersorRigid3DTransform:
//
//	parameters       [vx, vy, vz, tx, ty, tz]  (v = axis·sin(θ/2))
//	fixed parameters [cx, cy, cz]
//
// T(x) = R(v)·(x − c) + c + t, with the versor's scalar part
// √(1 − |v|²) ≥ 0, so the parameters cover rotations up to π. The rotation
// part of UpdateParameters composes rotations instead of adding (see
// UpdateParameters), which keeps optimizer steps on the unit sphere.
type VersorRigid3D struct {
	matrixOffset
	versor      Versor
	derivatives [3]Matrix3
}

// NewVersorRigid3D returns the identity transform centred at the origin.
func NewVersorRigid3D() *VersorRigid3D {
	v := &VersorRigid3D{matrixOffset: identityMatrixOffset(), versor: IdentityVersor()}
	v.computeMatrix()
	return v
}

// Kind returns KindVersorRigid3D.
func (v *VersorRigid3D) Kind() Kind { return KindVersorRigid3D }

// Versor returns the rotation, with W ≥ 0.
func (v *VersorRigid3D) Versor() Versor { return v.versor }

// SetVersor sets the rotation, keeping centre and translation. The versor
// is normalized and given W ≥ 0.
func (v *VersorRigid3D) SetVersor(q Versor) error {
	n, ok := q.normalized()
	if !ok {
		return fmt.Errorf("%w: zero or non-finite versor", ErrInvalidParameters)
	}
	v.versor = n.canonical()
	v.computeMatrix()
	v.computeOffset()
	return nil
}

// SetRotation sets the rotation by angle radians about axis.
func (v *VersorRigid3D) SetRotation(axis render.Vec3, angle float64) error {
	q, err := VersorFromAxisAngle(axis, angle)
	if err != nil {
		return err
	}
	return v.SetVersor(q)
}

// SetIdentity resets rotation and translation, keeping the centre.
func (v *VersorRigid3D) SetIdentity() {
	v.versor = IdentityVersor()
	v.translation = render.Vec3{}
	v.computeMatrix()
	v.computeOffset()
}

// SetMatrix sets the rotation from a proper rotation matrix, keeping the
// translation. It rejects matrices that are not orthogonal within
// DefaultOrthogonalityTolerance or that are reflections.
func (v *VersorRigid3D) SetMatrix(m Matrix3) error {
	return v.SetMatrixWithTolerance(m, DefaultOrthogonalityTolerance)
}

// SetMatrixWithTolerance is SetMatrix with a caller-chosen tolerance on
// M·Mᵀ = I; the stored rotation is the versor's, orthonormal to rounding.
func (v *VersorRigid3D) SetMatrixWithTolerance(m Matrix3, tolerance float64) error {
	q, err := VersorFromMatrix(m, tolerance)
	if err != nil {
		return err
	}
	v.versor = q
	v.computeMatrix()
	v.computeOffset()
	return nil
}

// NumberOfParameters returns 6.
func (v *VersorRigid3D) NumberOfParameters() int { return 6 }

// Parameters returns [vx, vy, vz, tx, ty, tz].
func (v *VersorRigid3D) Parameters() []float64 {
	t := v.translation
	return []float64{v.versor.X, v.versor.Y, v.versor.Z, t.X, t.Y, t.Z}
}

// SetParameters sets [vx, vy, vz, tx, ty, tz]. As in ITK, a right part
// whose norm reaches 1 − 1e-10 is scaled just inside the unit ball, so
// optimizers that step the parameters directly always get a rotation.
func (v *VersorRigid3D) SetParameters(parameters []float64) error {
	if err := checkLength(parameters, 6, "VersorRigid3D parameters"); err != nil {
		return err
	}
	q, err := versorFromParameters(parameters)
	if err != nil {
		return err
	}
	v.versor = q
	v.translation = vecFrom(parameters[3:])
	v.computeMatrix()
	v.computeOffset()
	return nil
}

// FixedParameters returns the centre [cx, cy, cz].
func (v *VersorRigid3D) FixedParameters() []float64 { return v.centerFixedParameters() }

// SetFixedParameters sets the centre, keeping the translation.
func (v *VersorRigid3D) SetFixedParameters(fixed []float64) error {
	if err := checkLength(fixed, 3, "VersorRigid3D fixed parameters"); err != nil {
		return err
	}
	return v.SetCenter(vecFrom(fixed))
}

// UpdateParameters applies an optimizer step as ITK's
// VersorRigid3DTransform::UpdateTransformParameters does: the new rotation
// is R(v)·Rot(u, factor·|u|) with u = update[0:3] (the step rotation about
// u is applied first), and the translation is advanced by
// factor·update[3:6]. Unlike ITK, a composed versor with a negative scalar
// part is negated before its right part is stored.
func (v *VersorRigid3D) UpdateParameters(update []float64, factor float64) error {
	if err := checkUpdate(update, 6, factor); err != nil {
		return err
	}
	q, err := composeVersorUpdate(v.versor, update, factor)
	if err != nil {
		return err
	}
	translation := v.translation.Add(vecFrom(update[3:]).Scale(factor))
	if !finiteVec(translation) {
		return fmt.Errorf("%w: translation update overflowed", ErrInvalidParameters)
	}
	v.versor = q
	v.translation = translation
	v.computeMatrix()
	v.computeOffset()
	return nil
}

// JacobianWithRespectToParameters writes ∂T(p)/∂[vx, vy, vz, tx, ty, tz].
// The rotation columns are singular for a rotation by π (W = 0).
func (v *VersorRigid3D) JacobianWithRespectToParameters(point render.Vec3, out *Jacobian) {
	out.reset(6)
	rotationTranslationJacobian(out, v.derivatives[:], point.Sub(v.center), 3)
}

// Clone returns an independent copy.
func (v *VersorRigid3D) Clone() Transform {
	c := *v
	return &c
}

// Inverse returns the VersorRigid3D mapping moving points back to fixed
// points, with the same centre.
func (v *VersorRigid3D) Inverse() (*VersorRigid3D, error) {
	_, offset, err := v.inverse()
	if err != nil {
		return nil, err
	}
	out := NewVersorRigid3D()
	out.center = v.center
	if err := out.SetVersor(v.versor.Conjugate()); err != nil {
		return nil, err
	}
	if err := out.SetOffset(offset); err != nil {
		return nil, err
	}
	return out, nil
}

func (v *VersorRigid3D) computeMatrix() {
	v.matrix = v.versor.Matrix()
	v.derivatives = v.versor.rightPartDerivatives()
}
