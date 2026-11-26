package registration

import (
	"fmt"

	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/spatialreg"
)

// DICOMRigidTolerance is the tolerance on M·Mᵀ = I used when a stored RIGID
// or RIGID_SCALE matrix is converted into a versor transform. Stored values
// are Decimal Strings of at most 16 characters and spatialreg accepts RIGID
// matrices within 1e-6, so the rotation is re-orthonormalized through the
// versor; the mapping moves by at most about 1e-6 of the distance to the
// centre.
const DICOMRigidTolerance = 1e-6

// DICOMMatrix returns the Frame of Reference Transformation Matrix that a
// Spatial Registration writer must store for t, where t maps the fixed
// image (the registered Frame of Reference) to the moving image (the Source
// Frame of Reference): the matrix is t's inverse, mapping source to
// registered coordinates. The type is RIGID for Euler3D and VersorRigid3D,
// RIGID_SCALE for Similarity3D, AFFINE for Affine and any other matrix
// transform, and the most general component type for a Composite of them.
// The matrix is checked with spatialreg.ValidateMatrix, so a singular,
// ill-conditioned or type-inconsistent result is an error instead of an
// object that readers would reject.
func DICOMMatrix(t Transform) (spatialreg.Matrix, error) {
	m, offset, kind, err := linearForm(t)
	if err != nil {
		return spatialreg.Matrix{}, err
	}
	inv, err := m.Inverse()
	if err != nil {
		return spatialreg.Matrix{}, fmt.Errorf("DICOM matrix: %w", err)
	}
	values := geometryAffine(inv, inv.MulVec(offset).Scale(-1))
	if err := spatialreg.ValidateMatrix(values, kind); err != nil {
		return spatialreg.Matrix{}, fmt.Errorf("DICOM matrix (%s): %w", kind, err)
	}
	return spatialreg.Matrix{Type: string(kind), Values: values}, nil
}

// DICOMForwardMatrix returns t itself as a Frame of Reference
// Transformation Matrix, mapping registered (fixed) to source (moving)
// coordinates: the direction of the Pre and Post Deformation Matrix
// Registration Sequences of a Deformable Spatial Registration (#1091), which
// act on registered points on their way to the source. It is validated as
// DICOMMatrix is.
func DICOMForwardMatrix(t Transform) (spatialreg.Matrix, error) {
	m, offset, kind, err := linearForm(t)
	if err != nil {
		return spatialreg.Matrix{}, err
	}
	values := geometryAffine(m, offset)
	if err := spatialreg.ValidateMatrix(values, kind); err != nil {
		return spatialreg.Matrix{}, fmt.Errorf("DICOM matrix (%s): %w", kind, err)
	}
	return spatialreg.Matrix{Type: string(kind), Values: values}, nil
}

// FromDICOMMatrix converts one Frame of Reference Transformation Matrix
// (source to registered) into the fixed-to-moving transform it implies,
// where the registered Frame of Reference is the fixed image: the inverse of
// the stored matrix. The matrix is validated for its declared Type first.
// RIGID gives a *VersorRigid3D, RIGID_SCALE a *Similarity3D when the scale
// is isotropic and an *Affine otherwise, and AFFINE an *Affine. center
// becomes the centre of rotation (the fixed parameters); it does not change
// the mapping, only how the parameters describe it.
func FromDICOMMatrix(matrix spatialreg.Matrix, center render.Vec3) (MatrixTransform, error) {
	kind := spatialreg.TransformType(matrix.Type)
	if err := spatialreg.ValidateMatrix(matrix.Values, kind); err != nil {
		return nil, fmt.Errorf("DICOM matrix (%s): %w", kind, err)
	}
	m, offset := splitGeometryAffine(matrix.Values)
	inv, err := m.Inverse()
	if err != nil {
		return nil, fmt.Errorf("DICOM matrix: %w", err)
	}
	return transformFromLinear(inv, inv.MulVec(offset).Scale(-1), kind, center)
}

// FromAffineRegistration converts a validated Spatial Registration item into
// the fixed-to-moving transform over its registered-to-source direction,
// which is the composition of every Matrix Sequence item already inverted by
// spatialreg. The transform family follows the most general item type as in
// FromDICOMMatrix; a product of anisotropic RIGID_SCALE items, which need not
// keep orthogonal columns, becomes an *Affine.
func FromAffineRegistration(r *spatialreg.AffineRegistration, center render.Vec3) (MatrixTransform, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: nil registration", ErrUnsupportedTransform)
	}
	if !r.RegisteredToSource.Finite() {
		return nil, fmt.Errorf("%w: registered-to-source matrix is not finite", ErrInvalidMatrix)
	}
	m, offset := splitGeometryAffine(r.RegisteredToSource)
	return transformFromLinear(m, offset, r.Type, center)
}

func transformFromLinear(m Matrix3, offset render.Vec3, kind spatialreg.TransformType, center render.Vec3) (MatrixTransform, error) {
	if !finiteVec(center) {
		return nil, fmt.Errorf("%w: non-finite centre", ErrInvalidParameters)
	}
	switch kind {
	case spatialreg.TransformRigid:
		v := NewVersorRigid3D()
		v.center = center
		if err := v.SetMatrixWithTolerance(m, DICOMRigidTolerance); err != nil {
			return nil, err
		}
		if err := v.SetOffset(offset); err != nil {
			return nil, err
		}
		return v, nil
	case spatialreg.TransformRigidScale:
		s := NewSimilarity3D()
		s.center = center
		if err := s.SetMatrixWithTolerance(m, DICOMRigidTolerance); err == nil {
			if err := s.SetOffset(offset); err != nil {
				return nil, err
			}
			return s, nil
		}
		// Anisotropic RIGID_SCALE: keep the exact matrix.
	case spatialreg.TransformAffine:
	default:
		return nil, fmt.Errorf("%w: unsupported DICOM matrix type %q", ErrUnsupportedTransform, kind)
	}
	a := NewAffine()
	a.center = center
	if err := a.SetMatrix(m); err != nil {
		return nil, err
	}
	if err := a.SetOffset(offset); err != nil {
		return nil, err
	}
	return a, nil
}

// linearForm returns the matrix, offset and DICOM type of a linear transform
// or a composite of linear transforms.
func linearForm(t Transform) (Matrix3, render.Vec3, spatialreg.TransformType, error) {
	switch v := t.(type) {
	case nil:
		return Matrix3{}, render.Vec3{}, "", fmt.Errorf("%w: nil transform", ErrUnsupportedTransform)
	case *Composite:
		m, offset, err := v.linear()
		if err != nil {
			return Matrix3{}, render.Vec3{}, "", err
		}
		return m, offset, compositeType(v), nil
	case MatrixTransform:
		return v.Matrix(), v.Offset(), transformType(v), nil
	default:
		return Matrix3{}, render.Vec3{}, "", fmt.Errorf("%w: %s has no Frame of Reference Transformation Matrix", ErrUnsupportedTransform, t.Kind())
	}
}

func transformType(t Transform) spatialreg.TransformType {
	switch v := t.(type) {
	case *Euler3D, *VersorRigid3D, *Translation3D:
		return spatialreg.TransformRigid
	case *Similarity3D:
		return spatialreg.TransformRigidScale
	case *Composite:
		return compositeType(v)
	default:
		return spatialreg.TransformAffine
	}
}

func compositeType(c *Composite) spatialreg.TransformType {
	rank := map[spatialreg.TransformType]int{spatialreg.TransformRigid: 0, spatialreg.TransformRigidScale: 1, spatialreg.TransformAffine: 2}
	out := spatialreg.TransformRigid
	for _, t := range c.transforms {
		if kind := transformType(t); rank[kind] > rank[out] {
			out = kind
		}
	}
	return out
}

func geometryAffine(m Matrix3, offset render.Vec3) render.GeometryAffine {
	return render.GeometryAffine{
		m[0][0], m[0][1], m[0][2], offset.X,
		m[1][0], m[1][1], m[1][2], offset.Y,
		m[2][0], m[2][1], m[2][2], offset.Z,
		0, 0, 0, 1,
	}
}

func splitGeometryAffine(a render.GeometryAffine) (Matrix3, render.Vec3) {
	return Matrix3{
		{a[0], a[1], a[2]},
		{a[4], a[5], a[6]},
		{a[8], a[9], a[10]},
	}, render.Vec3{X: a[3], Y: a[7], Z: a[11]}
}
