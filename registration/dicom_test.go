package registration

import (
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/spatialreg"
)

// TestDICOMMatrixSurvivesDecimalStringStorage writes the matrix of each
// family as 16-character Decimal Strings into a synthetic Spatial
// Registration data set, reads it back with spatialreg and recovers the
// fixed-to-moving mapping.
func TestDICOMMatrixSurvivesDecimalStringStorage(t *testing.T) {
	var (
		tagRegistrationSequence          = core.NewTag(0x0070, 0x0308)
		tagMatrixRegistrationSequence    = core.NewTag(0x0070, 0x0309)
		tagMatrixSequence                = core.NewTag(0x0070, 0x030A)
		tagFrameTransformationMatrix     = core.NewTag(0x3006, 0x00C6)
		tagFrameTransformationMatrixType = core.NewTag(0x0070, 0x030C)
	)
	for name, transform := range testTransforms(t) {
		t.Run(name, func(t *testing.T) {
			matrix, err := DICOMMatrix(transform)
			must(t, err)
			dataset := derivedio.Object(
				derivedio.UI(derivedio.TagSOPClassUID, spatialreg.SpatialRegistrationStorage),
				derivedio.UI(derivedio.TagSOPInstanceUID, "1.2.826.0.1.3680043.10.1083.1"),
				derivedio.UI(derivedio.TagFrameOfReferenceUID, "1.2.826.0.1.3680043.10.1083.2"),
				derivedio.Seq(tagRegistrationSequence, derivedio.DataSet(
					derivedio.UI(derivedio.TagFrameOfReferenceUID, "1.2.826.0.1.3680043.10.1083.3"),
					derivedio.Seq(tagMatrixRegistrationSequence, derivedio.DataSet(
						derivedio.Seq(tagMatrixSequence, derivedio.DataSet(
							derivedio.CS(tagFrameTransformationMatrixType, matrix.Type),
							derivedio.DS(tagFrameTransformationMatrix, matrix.Values[:]...),
						)),
					)),
				)),
			)
			parsed, err := spatialreg.ReadAffineDataset(dataset)
			must(t, err)
			registration := &parsed.Registrations[0]
			if string(registration.Type) != matrix.Type {
				t.Fatalf("read type %q, want %q", registration.Type, matrix.Type)
			}
			back, err := FromAffineRegistration(registration, render.Vec3{})
			must(t, err)
			for _, p := range testPoints {
				want := transform.TransformPoint(p)
				assertVecNear(t, back.TransformPoint(p), want, 1e-6*(1+want.Length()))
			}
		})
	}
}

// TestDICOMMatrixIsTheInverse pins the direction: a moving image shifted by
// +10 mm in x relative to the fixed image has T(x) = x + (10, 0, 0), and the
// stored matrix maps moving (source) points back to fixed (registered)
// points, so its translation is −10 mm.
func TestDICOMMatrixIsTheInverse(t *testing.T) {
	shift := NewVersorRigid3D()
	must(t, shift.SetTranslation(render.Vec3{X: 10}))
	matrix, err := DICOMMatrix(shift)
	must(t, err)
	if matrix.Type != string(spatialreg.TransformRigid) {
		t.Fatalf("type %q, want RIGID", matrix.Type)
	}
	want := render.GeometryAffine{1, 0, 0, -10, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
	if matrix.Values != want {
		t.Fatalf("matrix %v, want %v", matrix.Values, want)
	}
}

// TestDICOMForwardMatrixIsTheTransform: the deformable post matrix maps as
// the transform itself, the inverse of DICOMMatrix.
func TestDICOMForwardMatrixIsTheTransform(t *testing.T) {
	euler := NewEuler3D()
	must(t, euler.SetCenter(render.Vec3{X: 5, Y: -3, Z: 2}))
	must(t, euler.SetParameters([]float64{0.1, -0.05, 0.2, 10, -4, 7}))
	forward, err := DICOMForwardMatrix(euler)
	must(t, err)
	backward, err := DICOMMatrix(euler)
	must(t, err)
	if forward.Type != string(spatialreg.TransformRigid) {
		t.Fatalf("type %q", forward.Type)
	}
	for _, p := range []render.Vec3{{}, {X: 30, Y: -12, Z: 44}} {
		if got, want := forward.Values.TransformPoint(p), euler.TransformPoint(p); got.Sub(want).Length() > 1e-9 {
			t.Fatalf("forward maps %v to %v, want %v", p, got, want)
		}
		if back := backward.Values.TransformPoint(forward.Values.TransformPoint(p)); back.Sub(p).Length() > 1e-9 {
			t.Fatalf("forward then DICOMMatrix gives %v for %v", back, p)
		}
	}
}

func TestDICOMMatrixRoundTripsEveryFamily(t *testing.T) {
	transforms := testTransforms(t)
	cases := []struct {
		name     string
		kind     spatialreg.TransformType
		wantType func(MatrixTransform) bool
	}{
		{"euler-zxy", spatialreg.TransformRigid, isVersorRigid},
		{"euler-zyx", spatialreg.TransformRigid, isVersorRigid},
		{"versor-rigid", spatialreg.TransformRigid, isVersorRigid},
		{"similarity", spatialreg.TransformRigidScale, isSimilarity},
		{"affine", spatialreg.TransformAffine, isAffine},
		{"composite", spatialreg.TransformAffine, isAffine},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transform := transforms[tc.name]
			matrix, err := DICOMMatrix(transform)
			must(t, err)
			if matrix.Type != string(tc.kind) {
				t.Fatalf("type %q, want %q", matrix.Type, tc.kind)
			}
			must(t, spatialreg.ValidateMatrix(matrix.Values, tc.kind))
			for _, p := range testPoints {
				// Source (moving) coordinates map back to registered (fixed).
				assertVecNear(t, matrix.Values.TransformPoint(transform.TransformPoint(p)), p, 1e-9)
			}

			center := render.Vec3{X: -4, Y: 9, Z: 17}
			back, err := FromDICOMMatrix(matrix, center)
			must(t, err)
			if !tc.wantType(back) {
				t.Fatalf("FromDICOMMatrix returned %s", back.Kind())
			}
			if back.Center() != center {
				t.Fatalf("centre %+v, want %+v", back.Center(), center)
			}
			for _, p := range testPoints {
				assertVecNear(t, back.TransformPoint(p), transform.TransformPoint(p), 1e-9)
			}

			// The same through a spatialreg registration item.
			registration := &spatialreg.AffineRegistration{
				Type:               tc.kind,
				Matrices:           []spatialreg.Matrix{matrix},
				SourceToRegistered: matrix.Values,
				RegisteredToSource: back.GeometryAffine(),
			}
			fromItem, err := FromAffineRegistration(registration, center)
			must(t, err)
			for _, p := range testPoints {
				assertVecNear(t, fromItem.TransformPoint(p), transform.TransformPoint(p), 1e-9)
			}
		})
	}
}

func TestDICOMMatrixCompositeType(t *testing.T) {
	rigid := NewEuler3D()
	must(t, rigid.SetParameters([]float64{0.1, 0.2, 0.3, 1, 2, 3}))
	versor := NewVersorRigid3D()
	must(t, versor.SetParameters([]float64{0.1, 0, 0.2, -1, 0, 4}))
	similarity := NewSimilarity3D()
	must(t, similarity.SetParameters([]float64{0, 0.1, 0, 0, 0, 0, 1.5}))
	cases := []struct {
		transforms []Transform
		want       spatialreg.TransformType
	}{
		{[]Transform{rigid, versor}, spatialreg.TransformRigid},
		{[]Transform{rigid, similarity}, spatialreg.TransformRigidScale},
		{[]Transform{rigid, NewAffine(), versor}, spatialreg.TransformAffine},
		{nil, spatialreg.TransformRigid},
	}
	for _, tc := range cases {
		c, err := NewComposite(tc.transforms...)
		must(t, err)
		matrix, err := DICOMMatrix(c)
		must(t, err)
		if matrix.Type != string(tc.want) {
			t.Fatalf("%d transforms: type %q, want %q", len(tc.transforms), matrix.Type, tc.want)
		}
	}
}

func TestFromDICOMMatrixAnisotropicRigidScaleIsAffine(t *testing.T) {
	values := render.GeometryAffine{2, 0, 0, 5, 0, 1, 0, 0, 0, 0, 0.5, -3, 0, 0, 0, 1}
	back, err := FromDICOMMatrix(spatialreg.Matrix{Type: string(spatialreg.TransformRigidScale), Values: values}, render.Vec3{})
	must(t, err)
	if back.Kind() != KindAffine {
		t.Fatalf("anisotropic RIGID_SCALE gave %s", back.Kind())
	}
	source := render.Vec3{X: 1, Y: 2, Z: 3}
	assertVecNear(t, values.TransformPoint(back.TransformPoint(source)), source, 1e-12)
}

// TestFromAffineRegistrationComposedRigidScale: two anisotropic RIGID_SCALE
// items compose into a matrix without orthogonal columns, which the reader
// accepts item by item and which converts to an Affine.
func TestFromAffineRegistrationComposedRigidScale(t *testing.T) {
	q, err := VersorFromAxisAngle(render.Vec3{Z: 1}, 0.6)
	must(t, err)
	stretch := Matrix3{{2, 0, 0}, {0, 1, 0}, {0, 0, 1}}
	composed := stretch.Mul(q.Matrix()).Mul(Matrix3{{1, 0, 0}, {0, 3, 0}, {0, 0, 1}})
	inverse, err := composed.Inverse()
	must(t, err)
	r := &spatialreg.AffineRegistration{
		Type:               spatialreg.TransformRigidScale,
		SourceToRegistered: geometryAffine(composed, render.Vec3{X: 1}),
		RegisteredToSource: geometryAffine(inverse, inverse.MulVec(render.Vec3{X: 1}).Scale(-1)),
	}
	back, err := FromAffineRegistration(r, render.Vec3{})
	must(t, err)
	if back.Kind() != KindAffine {
		t.Fatalf("composed anisotropic RIGID_SCALE gave %s", back.Kind())
	}
	for _, p := range testPoints {
		assertVecNear(t, r.SourceToRegistered.TransformPoint(back.TransformPoint(p)), p, 1e-9)
	}
}

// TestFromDICOMMatrixReorthonormalizesRoundedRigid accepts a rotation
// rounded to 16-character Decimal Strings.
func TestFromDICOMMatrixReorthonormalizesRoundedRigid(t *testing.T) {
	q, err := VersorFromAxisAngle(render.Vec3{X: 0.3, Y: -1, Z: 0.2}, 0.7)
	must(t, err)
	m := q.Matrix()
	values := render.GeometryAffine{
		round(m[0][0], 1e-9), round(m[0][1], 1e-9), round(m[0][2], 1e-9), 12.5,
		round(m[1][0], 1e-9), round(m[1][1], 1e-9), round(m[1][2], 1e-9), -3.25,
		round(m[2][0], 1e-9), round(m[2][1], 1e-9), round(m[2][2], 1e-9), 40,
		0, 0, 0, 1,
	}
	back, err := FromDICOMMatrix(spatialreg.Matrix{Type: string(spatialreg.TransformRigid), Values: values}, render.Vec3{X: 100, Y: 100, Z: 100})
	must(t, err)
	assertMatrixNear(t, back.Matrix().Mul(back.Matrix().Transpose()), Identity3(), 1e-14)
	for _, p := range testPoints {
		assertVecNear(t, values.TransformPoint(back.TransformPoint(p)), p, 1e-6)
	}
}

func TestDICOMMatrixRejections(t *testing.T) {
	singular := NewAffine()
	must(t, singular.SetMatrix(Matrix3{{1, 0, 0}, {0, 1, 0}, {0, 0, 0}}))
	if _, err := DICOMMatrix(singular); !errors.Is(err, ErrSingularMatrix) {
		t.Fatalf("singular affine: err = %v", err)
	}
	if _, err := DICOMMatrix(nil); !errors.Is(err, ErrUnsupportedTransform) {
		t.Fatalf("nil transform: err = %v", err)
	}
	c, err := NewComposite(NewAffine(), nonLinear{})
	must(t, err)
	if _, err := DICOMMatrix(c); !errors.Is(err, ErrUnsupportedTransform) {
		t.Fatalf("non-linear component: err = %v", err)
	}
	if _, err := DICOMMatrix(nonLinear{}); !errors.Is(err, ErrUnsupportedTransform) {
		t.Fatalf("non-linear transform: err = %v", err)
	}

	scaled := render.GeometryAffine{2, 0, 0, 0, 0, 2, 0, 0, 0, 0, 2, 0, 0, 0, 0, 1}
	if _, err := FromDICOMMatrix(spatialreg.Matrix{Type: string(spatialreg.TransformRigid), Values: scaled}, render.Vec3{}); !errors.Is(err, spatialreg.ErrInvalidMatrix) {
		t.Fatalf("scaled RIGID: err = %v", err)
	}
	if _, err := FromDICOMMatrix(spatialreg.Matrix{Type: "PERSPECTIVE", Values: scaled}, render.Vec3{}); !errors.Is(err, spatialreg.ErrUnsupportedMatrix) {
		t.Fatalf("unknown type: err = %v", err)
	}
	identity := render.GeometryAffine{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
	if _, err := FromDICOMMatrix(spatialreg.Matrix{Type: string(spatialreg.TransformRigid), Values: identity}, render.Vec3{X: math.NaN()}); !errors.Is(err, ErrInvalidParameters) {
		t.Fatalf("NaN centre: err = %v", err)
	}
	if _, err := FromAffineRegistration(nil, render.Vec3{}); !errors.Is(err, ErrUnsupportedTransform) {
		t.Fatalf("nil registration: err = %v", err)
	}
}

// nonLinear is a minimal non-matrix transform for the rejection paths.
type nonLinear struct{}

func (nonLinear) Kind() Kind { return "Test" }
func (nonLinear) TransformPoint(p render.Vec3) render.Vec3 {
	return render.Vec3{X: p.X * p.X, Y: p.Y, Z: p.Z}
}
func (nonLinear) NumberOfParameters() int                                { return 0 }
func (nonLinear) Parameters() []float64                                  { return nil }
func (nonLinear) SetParameters([]float64) error                          { return nil }
func (nonLinear) FixedParameters() []float64                             { return nil }
func (nonLinear) SetFixedParameters([]float64) error                     { return nil }
func (nonLinear) UpdateParameters([]float64, float64) error              { return nil }
func (nonLinear) JacobianWithRespectToParameters(render.Vec3, *Jacobian) {}
func (nonLinear) JacobianWithRespectToPosition(p render.Vec3) Matrix3 {
	return Matrix3{{2 * p.X, 0, 0}, {0, 1, 0}, {0, 0, 1}}
}
func (n nonLinear) Clone() Transform { return n }

func isVersorRigid(t MatrixTransform) bool { _, ok := t.(*VersorRigid3D); return ok }
func isSimilarity(t MatrixTransform) bool  { _, ok := t.(*Similarity3D); return ok }
func isAffine(t MatrixTransform) bool      { _, ok := t.(*Affine); return ok }

func round(v, step float64) float64 { return math.Round(v/step) * step }
