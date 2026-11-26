package registration

// Scenarios ported from ITK (Apache-2.0, Copyright NumFOCUS; commit
// 35b196e1) Modules/Core/Transform/test: itkEuler3DTransformTest.cxx,
// itkEuler3DTransformGTest.cxx, itkVersorRigid3DTransformTest.cxx,
// itkSimilarity3DTransformTest.cxx and itkAffineTransformTest.cxx. The
// inputs, expected values and tolerances are ITK's; the assertions are
// re-expressed here and no ITK code is copied. Modifications: exceptions
// become returned errors, and ITK's Compose onto an identity transform is
// expressed as SetMatrix of the composed matrix.

import (
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

func degrees(d float64) float64 { return d * math.Atan(1) / 45 }

func TestITKEuler3DRotationTranslationAndOrder(t *testing.T) {
	const epsilon = 1e-10
	e := NewEuler3D()
	if e.Offset() != (render.Vec3{}) {
		t.Fatalf("identity offset %+v", e.Offset())
	}
	ax, ay, az := degrees(15), degrees(10), degrees(5)
	must(t, e.SetRotation(ax, ay, az))
	rx, _ := axisRotation(0, ax)
	ry, _ := axisRotation(1, ay)
	rz, _ := axisRotation(2, az)
	p := render.Vec3{X: 10, Y: -5, Z: 3}
	// The default order is Rz·Rx·Ry.
	assertVecNear(t, e.TransformPoint(p), rz.Mul(rx).Mul(ry).MulVec(p), epsilon)

	// Changing the order after setting the angles equals setting the
	// angles after changing the order.
	must(t, e.SetOrder(EulerZYX))
	e2 := NewEuler3D()
	must(t, e2.SetOrder(EulerZYX))
	must(t, e2.SetRotation(ax, ay, az))
	assertVecNear(t, e.TransformPoint(p), e2.TransformPoint(p), epsilon)
	assertVecNear(t, e.TransformPoint(p), rz.Mul(ry).Mul(rx).MulVec(p), epsilon)

	must(t, e.SetRotation(0, 0, 0))
	offset := render.Vec3{X: 1, Y: -4, Z: 8}
	must(t, e.SetOffset(offset))
	assertVecNear(t, e.TransformPoint(p), p.Add(offset), epsilon)
}

func TestITKEuler3DParametersFixedParametersAndJacobian(t *testing.T) {
	e := NewEuler3D()
	must(t, e.SetParameters([]float64{0, 1, 2, 3, 4, 5}))
	for i, v := range e.Parameters() {
		if v != float64(i) {
			t.Fatalf("parameters %v", e.Parameters())
		}
	}

	// Three fixed parameters (older files) keep the order; four set it.
	must(t, e.SetFixedParameters([]float64{0, 0, 0}))
	must(t, e.SetOrder(EulerZYX))
	if got := e.FixedParameters(); len(got) != 4 || got[3] != 1 {
		t.Fatalf("fixed parameters %v, want [0 0 0 1]", got)
	}
	must(t, e.SetFixedParameters([]float64{0, 0, 0, 0}))
	if got := e.FixedParameters(); got[3] != 0 || e.Order() != EulerZXY {
		t.Fatalf("fixed parameters %v, want [0 0 0 0]", got)
	}

	// At the centre the rotation columns vanish.
	var jac Jacobian
	e.JacobianWithRespectToParameters(render.Vec3{}, &jac)
	want := []float64{
		0, 0, 0, 1, 0, 0,
		0, 0, 0, 0, 1, 0,
		0, 0, 0, 0, 0, 1,
	}
	for i := range want {
		if jac.Values[i] != want[i] {
			t.Fatalf("Jacobian at the centre %v, want %v", jac.Values, want)
		}
	}

	// ITK's central difference with delta 0.001 and tolerance 1e-5.
	for _, order := range []EulerOrder{EulerZYX, EulerZXY} {
		must(t, e.SetOrder(order))
		parameters := []float64{0.2 / 180 * math.Pi, -1.0 / 180 * math.Pi, 2.4 / 180 * math.Pi, 5, 6, 8}
		must(t, e.SetParameters(parameters))
		point := render.Vec3{X: 1, Y: 1.5, Z: 2.6}
		e.JacobianWithRespectToParameters(point, &jac)
		numeric := finiteDifferenceJacobian(t, e, point, 0.001)
		for i := range jac.Values {
			if math.Abs(jac.Values[i]-numeric.Values[i]) > 1e-5 {
				t.Fatalf("%s Jacobian element %d = %g, finite difference %g", order, i, jac.Values[i], numeric.Values[i])
			}
		}
	}
}

// TestITKEuler3DAnglesFromMatrix is ITK's "Angle from matrix" check in both
// orders: angles (0.2, 0.1, 0.3) survive a trip through the matrix.
func TestITKEuler3DAnglesFromMatrix(t *testing.T) {
	for _, order := range []EulerOrder{EulerZXY, EulerZYX} {
		e := NewEuler3D()
		must(t, e.SetOrder(order))
		must(t, e.SetRotation(0.2, 0.1, 0.3))
		identity := NewEuler3D()
		must(t, identity.SetOrder(order))
		composed, err := NewComposite(e, identity)
		must(t, err)
		affine, err := composed.CollapseToAffine()
		must(t, err)
		t2 := NewEuler3D()
		must(t, t2.SetOrder(order))
		must(t, t2.SetMatrix(affine.Matrix()))
		got := t2.Parameters()
		for k, want := range []float64{0.2, 0.1, 0.3} {
			if math.Abs(got[k]-want) > 0.0001 {
				t.Fatalf("%s angles %v, want [0.2 0.1 0.3]", order, got[:3])
			}
		}
	}
}

func TestITKEuler3DSetMatrixAndInverse(t *testing.T) {
	const epsilon = 1e-10
	e := NewEuler3D()
	if err := e.SetMatrix(Matrix3{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}}); !errors.Is(err, ErrInvalidMatrix) {
		t.Fatalf("non-orthogonal matrix: err = %v", err)
	}
	a := 1.0 / 180 * math.Pi
	must(t, e.SetMatrix(Matrix3{{math.Cos(a), -math.Sin(a), 0}, {math.Sin(a), math.Cos(a), 0}, {0, 0, 1}}))

	want := []float64{0, 0, a, 0, 0, 0}
	source := NewEuler3D()
	must(t, source.SetParameters(want))
	t3 := NewEuler3D()
	must(t, t3.SetMatrix(source.Matrix()))
	for k, v := range t3.Parameters() {
		if math.Abs(v-want[k]) > epsilon {
			t.Fatalf("parameters %v, want %v", t3.Parameters(), want)
		}
	}

	inverse, err := source.Inverse()
	must(t, err)
	inverseMatrix, err := source.Matrix().Inverse()
	must(t, err)
	fromMatrix := NewEuler3D()
	must(t, fromMatrix.SetMatrix(inverseMatrix))
	for k, v := range inverse.Parameters() {
		if math.Abs(v-fromMatrix.Parameters()[k]) > epsilon {
			t.Fatalf("inverse parameters %v, want %v", inverse.Parameters(), fromMatrix.Parameters())
		}
	}
}

func TestITKEuler3DGTest(t *testing.T) {
	for size := 0; size < 3; size++ {
		e := NewEuler3D()
		if err := e.SetFixedParameters(make([]float64, size)); !errors.Is(err, ErrInvalidParameters) {
			t.Fatalf("%d fixed parameters: err = %v", size, err)
		}
	}
	e := NewEuler3D()
	center := render.Vec3{X: 200, Y: 400, Z: 300}
	must(t, e.SetCenter(center))
	e.SetIdentity()
	if e.Center() != center {
		t.Fatalf("SetIdentity moved the centre to %+v", e.Center())
	}
}

func TestITKVersorRigid3DRotation(t *testing.T) {
	const epsilon = 1e-12
	// 120° about (1, 1, 1) permutes the axes x → y, y → z, z → x.
	v := NewVersorRigid3D()
	axis := render.Vec3{X: 1, Y: 1, Z: 1}
	angle := degrees(120)
	must(t, v.SetRotation(axis, angle))
	assertVecNear(t, v.Offset(), render.Vec3{}, epsilon)
	q, err := VersorFromAxisAngle(axis, angle)
	must(t, err)
	p := render.Vec3{X: 1, Y: 4, Z: 9}
	assertVecNear(t, v.TransformPoint(p), q.Matrix().MulVec(p), epsilon)
	assertVecNear(t, v.TransformPoint(p), render.Vec3{X: 9, Y: 1, Z: 4}, 1e-9)
	assertVecNear(t, v.TransformVector(p), q.Matrix().MulVec(p), epsilon)
}

// TestITKVersorRigid3DJacobian checks ITK's theoretical Jacobian: identity
// versor, centre (31, 62, 93), translation (8, 7, 6), point (10, 20, −10).
func TestITKVersorRigid3DJacobian(t *testing.T) {
	const epsilon = 1e-12
	v := NewVersorRigid3D()
	must(t, v.SetRotation(render.Vec3{X: 1, Y: 1, Z: 1}, degrees(30)))
	center := render.Vec3{X: 31, Y: 62, Z: 93}
	must(t, v.SetCenter(center))
	assertVecNear(t, v.TransformPoint(center), center, epsilon)

	parameters := []float64{0, 0, 0, 8, 7, 6}
	must(t, v.SetParameters(parameters))
	for k, got := range v.Parameters() {
		if math.Abs(got-parameters[k]) > 1e-8 {
			t.Fatalf("parameters %v, want %v", v.Parameters(), parameters)
		}
	}
	var jac Jacobian
	v.JacobianWithRespectToParameters(render.Vec3{X: 10, Y: 20, Z: -10}, &jac)
	want := []float64{
		0, -206, 84, 1, 0, 0,
		206, 0, -42, 0, 1, 0,
		-84, 42, 0, 0, 0, 1,
	}
	for i := range want {
		if math.Abs(jac.Values[i]-want[i]) > 1e-5 {
			t.Fatalf("Jacobian %v, want %v", jac.Values, want)
		}
	}

	// SetIdentity keeps the centre and zeroes the parameters.
	must(t, v.SetRotation(render.Vec3{X: 1, Y: 1, Z: 1}, degrees(30)))
	v.SetIdentity()
	for _, got := range v.Parameters() {
		if math.Abs(got) > 1e-8 {
			t.Fatalf("identity parameters %v", v.Parameters())
		}
	}
	if v.Center() != center {
		t.Fatalf("SetIdentity moved the centre to %+v", v.Center())
	}
}

func TestITKVersorRigid3DSetMatrix(t *testing.T) {
	const epsilon = 1e-12
	v := NewVersorRigid3D()
	if err := v.SetMatrix(Matrix3{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}}); !errors.Is(err, ErrInvalidMatrix) {
		t.Fatalf("non-orthogonal matrix: err = %v", err)
	}
	a := 1.0 / 180 * math.Pi
	must(t, v.SetMatrix(Matrix3{{math.Cos(a), -math.Sin(a), 0}, {math.Sin(a), math.Cos(a), 0}, {0, 0, 1}}))

	want := []float64{0, 0, math.Sin(0.5 * a), 0, 0, 0}
	source := NewVersorRigid3D()
	must(t, source.SetParameters(want))
	t2 := NewVersorRigid3D()
	must(t, t2.SetMatrix(source.Matrix()))
	for k, got := range t2.Parameters() {
		if math.Abs(got-want[k]) > epsilon {
			t.Fatalf("parameters %v, want %v", t2.Parameters(), want)
		}
	}
	if _, err := source.Inverse(); err != nil {
		t.Fatal(err)
	}
}

// TestITKSimilarity3DJacobian is ITK's theoretical Jacobian with scale 10:
// the rotation columns scale by 10 and the scale column is R·(p − c).
func TestITKSimilarity3DJacobian(t *testing.T) {
	s := NewSimilarity3D()
	must(t, s.SetRotation(render.Vec3{X: 1, Y: 1, Z: 1}, degrees(30)))
	center := render.Vec3{X: 31, Y: 62, Z: 93}
	must(t, s.SetCenter(center))
	assertVecNear(t, s.TransformPoint(center), center, 1e-12)

	parameters := []float64{0, 0, 0, 8, 7, 6, 10}
	must(t, s.SetParameters(parameters))
	for k, got := range s.Parameters() {
		if math.Abs(got-parameters[k]) > 1e-8 {
			t.Fatalf("parameters %v, want %v", s.Parameters(), parameters)
		}
	}
	var jac Jacobian
	s.JacobianWithRespectToParameters(render.Vec3{X: 10, Y: 20, Z: -10}, &jac)
	want := []float64{
		0, -2060, 840, 1, 0, 0, -21,
		2060, 0, -420, 0, 1, 0, -42,
		-840, 420, 0, 0, 0, 1, -103,
	}
	for i := range want {
		if math.Abs(jac.Values[i]-want[i]) > 1e-5 {
			t.Fatalf("Jacobian %v, want %v", jac.Values, want)
		}
	}
}

func TestITKAffineRotationInverseAndJacobians(t *testing.T) {
	// Rotate3D about (.707, .707, .707) by one radian, and its inverse.
	q, err := VersorFromAxisAngle(render.Vec3{X: .707, Y: .707, Z: .707}, 1)
	must(t, err)
	a := NewAffine()
	must(t, a.SetMatrix(q.Matrix()))
	rotation := Matrix3{
		{0.69353487057876, -0.33259093488348, 0.63905606430472},
		{0.63905606430472, 0.69353487057876, -0.33259093488348},
		{-0.33259093488348, 0.63905606430472, 0.69353487057876},
	}
	assertMatrixNear(t, a.Matrix(), rotation, 1e-13)
	inverse, err := a.Inverse()
	must(t, err)
	assertMatrixNear(t, inverse.Matrix(), rotation.Transpose(), 1e-13)

	var jac Jacobian
	jacobianAffine := NewAffine()
	jacobianAffine.JacobianWithRespectToParameters(render.Vec3{X: 5, Y: 10, Z: 15}, &jac)
	want := []float64{
		5, 10, 15, 0, 0, 0, 0, 0, 0, 1, 0, 0,
		0, 0, 0, 5, 10, 15, 0, 0, 0, 0, 1, 0,
		0, 0, 0, 0, 0, 0, 5, 10, 15, 0, 0, 1,
	}
	for i := range want {
		if jac.Values[i] != want[i] {
			t.Fatalf("Jacobian %v, want %v", jac.Values, want)
		}
	}

	m := Matrix3{{1, 2, 3}, {4, 5, 6}, {7, 8, 8}}
	must(t, jacobianAffine.SetMatrix(m))
	if got := jacobianAffine.JacobianWithRespectToPosition(render.Vec3{X: 5, Y: 10, Z: 15}); got != m {
		t.Fatalf("position Jacobian %v, want %v", got, m)
	}
	inv, err := jacobianAffine.Inverse()
	must(t, err)
	assertMatrixNear(t, inv.Matrix(), Matrix3{{-8. / 3., 8. / 3., -1}, {10. / 3., -13. / 3., 2}, {-1, 2, -1}}, 1e-13)
}
