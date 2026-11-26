package registration

import (
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// Similarity3D is a versor rotation, an isotropic scale and a translation,
// after ITK's Similarity3DTransform:
//
//	parameters       [vx, vy, vz, tx, ty, tz, s]
//	fixed parameters [cx, cy, cz]
//
// T(x) = s·R(v)·(x − c) + c + t with s > 0. UpdateParameters composes the
// rotation as VersorRigid3D does and adds the translation and scale steps.
type Similarity3D struct {
	matrixOffset
	versor      Versor
	scale       float64
	rotation    Matrix3
	derivatives [3]Matrix3
}

// NewSimilarity3D returns the identity transform (scale 1) centred at the
// origin.
func NewSimilarity3D() *Similarity3D {
	s := &Similarity3D{matrixOffset: identityMatrixOffset(), versor: IdentityVersor(), scale: 1}
	s.computeMatrix()
	return s
}

// Kind returns KindSimilarity3D.
func (s *Similarity3D) Kind() Kind { return KindSimilarity3D }

// Versor returns the rotation, with W ≥ 0.
func (s *Similarity3D) Versor() Versor { return s.versor }

// Scale returns the isotropic scale factor.
func (s *Similarity3D) Scale() float64 { return s.scale }

// SetVersor sets the rotation, keeping scale, centre and translation.
func (s *Similarity3D) SetVersor(q Versor) error {
	n, ok := q.normalized()
	if !ok {
		return fmt.Errorf("%w: zero or non-finite versor", ErrInvalidParameters)
	}
	s.versor = n.canonical()
	s.computeMatrix()
	s.computeOffset()
	return nil
}

// SetRotation sets the rotation by angle radians about axis.
func (s *Similarity3D) SetRotation(axis render.Vec3, angle float64) error {
	q, err := VersorFromAxisAngle(axis, angle)
	if err != nil {
		return err
	}
	return s.SetVersor(q)
}

// SetScale sets the isotropic scale, which must be finite and positive.
func (s *Similarity3D) SetScale(scale float64) error {
	if !finite(scale) || scale <= 0 {
		return fmt.Errorf("%w: similarity scale %g must be finite and positive", ErrInvalidParameters, scale)
	}
	s.scale = scale
	s.computeMatrix()
	s.computeOffset()
	return nil
}

// SetIdentity resets rotation, scale and translation, keeping the centre.
func (s *Similarity3D) SetIdentity() {
	s.versor = IdentityVersor()
	s.scale = 1
	s.translation = render.Vec3{}
	s.computeMatrix()
	s.computeOffset()
}

// SetMatrix sets rotation and scale from a similarity matrix s·R, keeping
// the translation: s is the cube root of the determinant, which must be
// positive, and M/s must be orthogonal within DefaultOrthogonalityTolerance.
func (s *Similarity3D) SetMatrix(m Matrix3) error {
	return s.SetMatrixWithTolerance(m, DefaultOrthogonalityTolerance)
}

// SetMatrixWithTolerance is SetMatrix with a caller-chosen tolerance on the
// orthogonality of M/s.
func (s *Similarity3D) SetMatrixWithTolerance(m Matrix3, tolerance float64) error {
	q, scale, err := similarityFromMatrix(m, tolerance)
	if err != nil {
		return err
	}
	s.versor, s.scale = q, scale
	s.computeMatrix()
	s.computeOffset()
	return nil
}

// NumberOfParameters returns 7.
func (s *Similarity3D) NumberOfParameters() int { return 7 }

// Parameters returns [vx, vy, vz, tx, ty, tz, s].
func (s *Similarity3D) Parameters() []float64 {
	t := s.translation
	return []float64{s.versor.X, s.versor.Y, s.versor.Z, t.X, t.Y, t.Z, s.scale}
}

// SetParameters sets [vx, vy, vz, tx, ty, tz, s]; see
// VersorRigid3D.SetParameters for the versor, and the scale must be
// positive.
func (s *Similarity3D) SetParameters(parameters []float64) error {
	if err := checkLength(parameters, 7, "Similarity3D parameters"); err != nil {
		return err
	}
	if parameters[6] <= 0 {
		return fmt.Errorf("%w: similarity scale %g must be positive", ErrInvalidParameters, parameters[6])
	}
	q, err := versorFromParameters(parameters)
	if err != nil {
		return err
	}
	s.versor = q
	s.translation = vecFrom(parameters[3:])
	s.scale = parameters[6]
	s.computeMatrix()
	s.computeOffset()
	return nil
}

// FixedParameters returns the centre [cx, cy, cz].
func (s *Similarity3D) FixedParameters() []float64 { return s.centerFixedParameters() }

// SetFixedParameters sets the centre, keeping the translation.
func (s *Similarity3D) SetFixedParameters(fixed []float64) error {
	if err := checkLength(fixed, 3, "Similarity3D fixed parameters"); err != nil {
		return err
	}
	return s.SetCenter(vecFrom(fixed))
}

// UpdateParameters composes the rotation step as VersorRigid3D does and
// adds factor·update to the translation and the scale; a step that makes
// the scale non-positive is rejected and leaves the transform unchanged.
func (s *Similarity3D) UpdateParameters(update []float64, factor float64) error {
	if err := checkUpdate(update, 7, factor); err != nil {
		return err
	}
	scale := s.scale + factor*update[6]
	if !finite(scale) || scale <= 0 {
		return fmt.Errorf("%w: similarity scale step to %g", ErrInvalidParameters, scale)
	}
	translation := s.translation.Add(vecFrom(update[3:]).Scale(factor))
	if !finiteVec(translation) {
		return fmt.Errorf("%w: translation update overflowed", ErrInvalidParameters)
	}
	q, err := composeVersorUpdate(s.versor, update, factor)
	if err != nil {
		return err
	}
	s.versor, s.translation, s.scale = q, translation, scale
	s.computeMatrix()
	s.computeOffset()
	return nil
}

// JacobianWithRespectToParameters writes ∂T(p)/∂[vx, vy, vz, tx, ty, tz, s].
func (s *Similarity3D) JacobianWithRespectToParameters(point render.Vec3, out *Jacobian) {
	out.reset(7)
	relative := point.Sub(s.center)
	rotationTranslationJacobian(out, s.derivatives[:], relative, 3)
	out.setColumn(6, s.rotation.MulVec(relative))
}

// Clone returns an independent copy.
func (s *Similarity3D) Clone() Transform {
	c := *s
	return &c
}

// Inverse returns the Similarity3D mapping moving points back to fixed
// points (rotation inverted, scale 1/s), with the same centre.
func (s *Similarity3D) Inverse() (*Similarity3D, error) {
	_, offset, err := s.inverse()
	if err != nil {
		return nil, err
	}
	out := NewSimilarity3D()
	out.center = s.center
	out.versor = s.versor.Conjugate()
	out.scale = 1 / s.scale
	out.computeMatrix()
	if err := out.SetOffset(offset); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Similarity3D) computeMatrix() {
	s.rotation = s.versor.Matrix()
	s.matrix = s.rotation.Scale(s.scale)
	s.derivatives = s.versor.rightPartDerivatives()
	for k := range s.derivatives {
		s.derivatives[k] = s.derivatives[k].Scale(s.scale)
	}
}

// similarityFromMatrix splits m = s·R with s = ∛det(m) > 0.
func similarityFromMatrix(m Matrix3, tolerance float64) (Versor, float64, error) {
	if !m.Finite() {
		return Versor{}, 0, fmt.Errorf("%w: non-finite similarity matrix", ErrInvalidMatrix)
	}
	det := m.Determinant()
	if !(det > 0) || !finite(det) {
		return Versor{}, 0, fmt.Errorf("%w: similarity matrix determinant %g must be positive", ErrInvalidMatrix, det)
	}
	scale := math.Cbrt(det)
	q, err := VersorFromMatrix(m.Scale(1/scale), tolerance)
	if err != nil {
		return Versor{}, 0, err
	}
	return q, scale, nil
}
