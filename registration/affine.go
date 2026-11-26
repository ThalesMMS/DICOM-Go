package registration

import (
	"fmt"

	"github.com/ThalesMMS/dicom-go/render"
)

// Affine is a general affine transform, after ITK's AffineTransform:
//
//	parameters       [m00, m01, m02, m10, m11, m12, m20, m21, m22, tx, ty, tz]
//	fixed parameters [cx, cy, cz]
//
// T(x) = M·(x − c) + c + t. The matrix is stored row-major in the
// parameters and updated additively. M may be singular while optimizing;
// Inverse and the DICOM conversion reject singular or ill-conditioned
// matrices.
type Affine struct {
	matrixOffset
}

// NewAffine returns the identity transform centred at the origin.
func NewAffine() *Affine {
	return &Affine{matrixOffset: identityMatrixOffset()}
}

// NewAffineFrom copies the matrix, centre and translation of any matrix
// transform, for example to continue a rigid registration with an affine
// stage.
func NewAffineFrom(t MatrixTransform) (*Affine, error) {
	if t == nil {
		return nil, fmt.Errorf("%w: nil transform", ErrUnsupportedTransform)
	}
	a := NewAffine()
	if err := a.SetMatrix(t.Matrix()); err != nil {
		return nil, err
	}
	if err := a.SetCenter(t.Center()); err != nil {
		return nil, err
	}
	if err := a.SetOffset(t.Offset()); err != nil {
		return nil, err
	}
	return a, nil
}

// Kind returns KindAffine.
func (a *Affine) Kind() Kind { return KindAffine }

// SetMatrix sets M, keeping the translation (the offset is recomputed).
func (a *Affine) SetMatrix(m Matrix3) error {
	if !m.Finite() {
		return fmt.Errorf("%w: non-finite affine matrix", ErrInvalidMatrix)
	}
	a.matrix = m
	a.computeOffset()
	return nil
}

// SetIdentity resets matrix and translation, keeping the centre.
func (a *Affine) SetIdentity() {
	a.matrix = Identity3()
	a.translation = render.Vec3{}
	a.computeOffset()
}

// NumberOfParameters returns 12.
func (a *Affine) NumberOfParameters() int { return 12 }

// Parameters returns the row-major matrix followed by the translation.
func (a *Affine) Parameters() []float64 {
	out := make([]float64, 0, 12)
	for i := range a.matrix {
		out = append(out, a.matrix[i][:]...)
	}
	return append(out, a.translation.X, a.translation.Y, a.translation.Z)
}

// SetParameters sets the row-major matrix and the translation.
func (a *Affine) SetParameters(parameters []float64) error {
	if err := checkLength(parameters, 12, "Affine parameters"); err != nil {
		return err
	}
	for i := 0; i < 3; i++ {
		copy(a.matrix[i][:], parameters[3*i:3*i+3])
	}
	a.translation = vecFrom(parameters[9:])
	a.computeOffset()
	return nil
}

// FixedParameters returns the centre [cx, cy, cz].
func (a *Affine) FixedParameters() []float64 { return a.centerFixedParameters() }

// SetFixedParameters sets the centre, keeping the translation.
func (a *Affine) SetFixedParameters(fixed []float64) error {
	if err := checkLength(fixed, 3, "Affine fixed parameters"); err != nil {
		return err
	}
	return a.SetCenter(vecFrom(fixed))
}

// UpdateParameters adds factor·update to the parameters.
func (a *Affine) UpdateParameters(update []float64, factor float64) error {
	return addUpdate(a, update, factor)
}

// JacobianWithRespectToParameters writes ∂T(p)/∂parameters: the column of
// m_ij holds (p − c)_j in row i, and the translation block is the identity.
func (a *Affine) JacobianWithRespectToParameters(point render.Vec3, out *Jacobian) {
	out.reset(12)
	relative := [3]float64{point.X - a.center.X, point.Y - a.center.Y, point.Z - a.center.Z}
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			out.set(i, 3*i+j, relative[j])
		}
		out.set(i, 9+i, 1)
	}
}

// Clone returns an independent copy.
func (a *Affine) Clone() Transform {
	c := *a
	return &c
}

// Inverse returns the Affine mapping moving points back to fixed points,
// with the same centre. Singular or ill-conditioned matrices return
// ErrSingularMatrix.
func (a *Affine) Inverse() (*Affine, error) {
	m, offset, err := a.inverse()
	if err != nil {
		return nil, err
	}
	out := NewAffine()
	out.center = a.center
	out.matrix = m
	if err := out.SetOffset(offset); err != nil {
		return nil, err
	}
	return out, nil
}
