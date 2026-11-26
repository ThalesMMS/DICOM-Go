package registration

import (
	"errors"
	"fmt"

	"github.com/ThalesMMS/dicom-go/render"
)

// Kind names a transform family with ITK's class name, as written in ITK
// transform files (without the scalar/dimension template suffix).
type Kind string

const (
	KindTranslation       Kind = "TranslationTransform"
	KindScaleVersor3D     Kind = "ScaleVersor3DTransform"
	KindScaleSkewVersor3D Kind = "ScaleSkewVersor3DTransform"
	KindEuler2D           Kind = "Euler2DTransform"
	KindSimilarity2D      Kind = "Similarity2DTransform"
	KindEuler3D           Kind = "Euler3DTransform"
	KindVersorRigid3D     Kind = "VersorRigid3DTransform"
	KindSimilarity3D      Kind = "Similarity3DTransform"
	KindAffine            Kind = "AffineTransform"
	KindComposite         Kind = "CompositeTransform"
)

var (
	// ErrInvalidParameters reports a parameter vector of the wrong length,
	// a non-finite value or a value outside the transform's domain (a
	// value outside the domain required by that particular transform family).
	ErrInvalidParameters = errors.New("dicom/registration: invalid parameters")
	// ErrInvalidMatrix reports a non-finite matrix or one that does not
	// belong to the transform family (not a rotation, not a similarity).
	ErrInvalidMatrix = errors.New("dicom/registration: invalid matrix")
	// ErrSingularMatrix reports a singular or ill-conditioned matrix that
	// cannot be inverted reliably.
	ErrSingularMatrix = errors.New("dicom/registration: singular matrix")
	// ErrUnsupportedTransform reports a transform that an operation cannot
	// represent, such as a non-linear component in a matrix conversion.
	ErrUnsupportedTransform = errors.New("dicom/registration: unsupported transform")
)

// DefaultOrthogonalityTolerance is the per-element tolerance on M·Mᵀ = I
// accepted by the rigid and similarity SetMatrix methods, ITK's
// MatrixOrthogonalityTolerance for double.
const DefaultOrthogonalityTolerance = 1e-10

// Transform is a parametric spatial transform mapping fixed-image points to
// moving-image points (see the package documentation for the direction).
//
// Parameters and FixedParameters return copies. SetParameters and
// SetFixedParameters validate the whole vector before changing anything, so
// a rejected vector leaves the transform untouched. UpdateParameters applies
// an optimizer step p ← p ⊕ factor·update; for most families ⊕ is addition,
// while versor families compose rotations (see VersorRigid3D).
//
// JacobianWithRespectToParameters writes ∂T(p)/∂parameters into out,
// resizing it to NumberOfParameters columns and reusing its storage when
// possible, so a caller can evaluate it per sample without allocating.
// JacobianWithRespectToPosition returns ∂T(p)/∂p.
//
// A Transform is not safe for concurrent mutation; concurrent read-only
// calls (TransformPoint, the Jacobians with distinct out values) are safe.
type Transform interface {
	Kind() Kind
	TransformPoint(point render.Vec3) render.Vec3
	NumberOfParameters() int
	Parameters() []float64
	SetParameters(parameters []float64) error
	FixedParameters() []float64
	SetFixedParameters(fixed []float64) error
	UpdateParameters(update []float64, factor float64) error
	JacobianWithRespectToParameters(point render.Vec3, out *Jacobian)
	JacobianWithRespectToPosition(point render.Vec3) Matrix3
	Clone() Transform
}

// MatrixTransform is a Transform of the form x ↦ M·x + offset, with the
// offset derived from a centre of rotation c and a translation t as
// offset = t + c − M·c.
type MatrixTransform interface {
	Transform
	Matrix() Matrix3
	Offset() render.Vec3
	Center() render.Vec3
	Translation() render.Vec3
	GeometryAffine() render.GeometryAffine
}

// matrixOffset is the state shared by the matrix transforms, after ITK's
// MatrixOffsetTransformBase: the matrix, the centre (fixed parameters), the
// translation (parameters) and the derived offset.
type matrixOffset struct {
	matrix      Matrix3
	center      render.Vec3
	translation render.Vec3
	offset      render.Vec3
}

func identityMatrixOffset() matrixOffset {
	return matrixOffset{matrix: Identity3()}
}

// Matrix returns the linear part M.
func (b *matrixOffset) Matrix() Matrix3 { return b.matrix }

// Center returns the centre of rotation c (the fixed parameters).
func (b *matrixOffset) Center() render.Vec3 { return b.center }

// Translation returns t, the translation applied after the rotation about c.
func (b *matrixOffset) Translation() render.Vec3 { return b.translation }

// Offset returns t + c − M·c, the translation of the equivalent x ↦ M·x +
// offset form.
func (b *matrixOffset) Offset() render.Vec3 { return b.offset }

// TransformPoint returns M·p + offset.
func (b *matrixOffset) TransformPoint(point render.Vec3) render.Vec3 {
	return b.matrix.MulVec(point).Add(b.offset)
}

// TransformVector returns M·v, the image of a displacement.
func (b *matrixOffset) TransformVector(vector render.Vec3) render.Vec3 {
	return b.matrix.MulVec(vector)
}

// JacobianWithRespectToPosition returns M at every point.
func (b *matrixOffset) JacobianWithRespectToPosition(render.Vec3) Matrix3 { return b.matrix }

// GeometryAffine returns the transform as a row-major homogeneous 4×4
// matrix in the fixed-to-moving direction.
func (b *matrixOffset) GeometryAffine() render.GeometryAffine {
	return geometryAffine(b.matrix, b.offset)
}

// SetCenter moves the centre of rotation while keeping the translation, as
// ITK's SetCenter does, so the mapping changes unless M is the identity.
func (b *matrixOffset) SetCenter(center render.Vec3) error {
	if !finiteVec(center) {
		return fmt.Errorf("%w: non-finite centre", ErrInvalidParameters)
	}
	b.center = center
	b.computeOffset()
	return nil
}

// SetTranslation sets t and recomputes the offset.
func (b *matrixOffset) SetTranslation(translation render.Vec3) error {
	if !finiteVec(translation) {
		return fmt.Errorf("%w: non-finite translation", ErrInvalidParameters)
	}
	b.translation = translation
	b.computeOffset()
	return nil
}

// SetOffset sets the offset of the x ↦ M·x + offset form and recomputes the
// translation for the current centre, so the mapping becomes exactly
// M·x + offset.
func (b *matrixOffset) SetOffset(offset render.Vec3) error {
	if !finiteVec(offset) {
		return fmt.Errorf("%w: non-finite offset", ErrInvalidParameters)
	}
	b.offset = offset
	b.computeTranslation()
	return nil
}

func (b *matrixOffset) computeOffset() {
	b.offset = b.translation.Add(b.center).Sub(b.matrix.MulVec(b.center))
}

func (b *matrixOffset) computeTranslation() {
	b.translation = b.offset.Sub(b.center).Add(b.matrix.MulVec(b.center))
}

// centerFixedParameters returns the centre as ITK's three fixed parameters.
func (b *matrixOffset) centerFixedParameters() []float64 {
	return []float64{b.center.X, b.center.Y, b.center.Z}
}

// inverse returns the matrix and offset of the inverse mapping.
func (b *matrixOffset) inverse() (Matrix3, render.Vec3, error) {
	inv, err := b.matrix.Inverse()
	if err != nil {
		return Matrix3{}, render.Vec3{}, err
	}
	return inv, inv.MulVec(b.offset).Scale(-1), nil
}

// rotationTranslationJacobian writes dM/dθk·(p − c) into columns
// 0..len(derivatives)-1 and the identity translation block into the three
// columns starting at translationColumn. out must be zeroed.
func rotationTranslationJacobian(out *Jacobian, derivatives []Matrix3, relative render.Vec3, translationColumn int) {
	for k := range derivatives {
		out.setColumn(k, derivatives[k].MulVec(relative))
	}
	out.set(0, translationColumn, 1)
	out.set(1, translationColumn+1, 1)
	out.set(2, translationColumn+2, 1)
}

func checkLength(values []float64, want int, what string) error {
	if len(values) != want {
		return fmt.Errorf("%w: %s has %d values, want %d", ErrInvalidParameters, what, len(values), want)
	}
	if !finiteSlice(values) {
		return fmt.Errorf("%w: %s contains a non-finite value", ErrInvalidParameters, what)
	}
	return nil
}

func vecFrom(values []float64) render.Vec3 {
	return render.Vec3{X: values[0], Y: values[1], Z: values[2]}
}
