package registration

import (
	"errors"
	"math"
	"math/rand"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// testTransforms returns one configured instance of every family, with
// non-trivial centres so the centre handling is exercised.
func testTransforms(t *testing.T) map[string]Transform {
	t.Helper()
	center := render.Vec3{X: 12, Y: -7, Z: 30}

	zxy := NewEuler3D()
	must(t, zxy.SetFixedParameters([]float64{center.X, center.Y, center.Z, 0}))
	must(t, zxy.SetParameters([]float64{0.3, -0.4, 0.7, 5, -3, 2}))

	zyx := NewEuler3D()
	must(t, zyx.SetFixedParameters([]float64{center.X, center.Y, center.Z, 1}))
	must(t, zyx.SetParameters([]float64{-0.5, 0.25, 1.1, -4, 6, 1}))

	versor := NewVersorRigid3D()
	must(t, versor.SetFixedParameters([]float64{center.X, center.Y, center.Z}))
	must(t, versor.SetParameters([]float64{0.2, -0.3, 0.4, 1, 2, 3}))

	similarity := NewSimilarity3D()
	must(t, similarity.SetFixedParameters([]float64{center.X, center.Y, center.Z}))
	must(t, similarity.SetParameters([]float64{-0.1, 0.35, 0.2, -2, 0.5, 4, 1.3}))

	affine := NewAffine()
	must(t, affine.SetFixedParameters([]float64{center.X, center.Y, center.Z}))
	must(t, affine.SetParameters([]float64{1.1, 0.2, -0.1, 0.05, 0.9, 0.3, -0.2, 0.1, 1.2, 3, -1, 2}))

	composite, err := NewComposite(zxy.Clone(), similarity.Clone(), affine.Clone())
	must(t, err)
	frozen, err := NewComposite(versor.Clone(), zyx.Clone(), affine.Clone())
	must(t, err)
	must(t, frozen.SetOptimized(0, false))
	nested, err := NewComposite(frozen.Clone(), similarity.Clone())
	must(t, err)

	return map[string]Transform{
		"euler-zxy":        zxy,
		"euler-zyx":        zyx,
		"versor-rigid":     versor,
		"similarity":       similarity,
		"affine":           affine,
		"composite":        composite,
		"composite-frozen": frozen,
		"composite-nested": nested,
	}
}

var testPoints = []render.Vec3{
	{X: 0, Y: 0, Z: 0},
	{X: 12, Y: -7, Z: 30},
	{X: 100, Y: -40, Z: 25},
	{X: -63.5, Y: 18.25, Z: -120},
}

// TestJacobianWithRespectToParametersMatchesFiniteDifferences checks every
// column of every family's parameter Jacobian against central differences
// of TransformPoint through SetParameters.
func TestJacobianWithRespectToParametersMatchesFiniteDifferences(t *testing.T) {
	for name, transform := range testTransforms(t) {
		t.Run(name, func(t *testing.T) {
			var jac Jacobian
			for _, p := range testPoints {
				transform.JacobianWithRespectToParameters(p, &jac)
				if jac.Columns != transform.NumberOfParameters() || len(jac.Values) != 3*jac.Columns {
					t.Fatalf("Jacobian shape %d/%d, want %d columns", jac.Columns, len(jac.Values), transform.NumberOfParameters())
				}
				numeric := finiteDifferenceJacobian(t, transform, p, 1e-6)
				for row := 0; row < 3; row++ {
					for column := 0; column < jac.Columns; column++ {
						got, want := jac.At(row, column), numeric.At(row, column)
						if math.Abs(got-want) > 1e-5*(1+math.Abs(want)) {
							t.Fatalf("point %+v J[%d][%d] = %.10g, finite difference %.10g", p, row, column, got, want)
						}
					}
				}
			}
		})
	}
}

// TestJacobianWithRespectToPositionMatchesFiniteDifferences checks ∂T/∂x.
func TestJacobianWithRespectToPositionMatchesFiniteDifferences(t *testing.T) {
	const h = 1e-4
	for name, transform := range testTransforms(t) {
		t.Run(name, func(t *testing.T) {
			for _, p := range testPoints {
				got := transform.JacobianWithRespectToPosition(p)
				axes := [3]render.Vec3{{X: h}, {Y: h}, {Z: h}}
				for column, axis := range axes {
					d := transform.TransformPoint(p.Add(axis)).Sub(transform.TransformPoint(p.Sub(axis))).Scale(1 / (2 * h))
					want := [3]float64{d.X, d.Y, d.Z}
					for row := 0; row < 3; row++ {
						if math.Abs(got[row][column]-want[row]) > 1e-7*(1+math.Abs(want[row])) {
							t.Fatalf("point %+v ∂T%d/∂x%d = %.10g, finite difference %.10g", p, row, column, got[row][column], want[row])
						}
					}
				}
			}
		})
	}
}

// TestJacobianReusesStorage verifies that a Jacobian big enough for the
// transform is reused without allocating.
func TestJacobianReusesStorage(t *testing.T) {
	transform := testTransforms(t)["similarity"]
	jac := NewJacobian(12)
	p := testPoints[2]
	allocations := testing.AllocsPerRun(100, func() {
		transform.JacobianWithRespectToParameters(p, &jac)
	})
	if allocations != 0 {
		t.Fatalf("JacobianWithRespectToParameters allocated %.0f times", allocations)
	}
	if jac.Columns != 7 {
		t.Fatalf("columns = %d, want 7", jac.Columns)
	}
}

func TestParametersRoundTripAndRejection(t *testing.T) {
	for name, transform := range testTransforms(t) {
		t.Run(name, func(t *testing.T) {
			parameters := transform.Parameters()
			fixed := transform.FixedParameters()
			clone := transform.Clone()
			must(t, clone.SetFixedParameters(fixed))
			must(t, clone.SetParameters(parameters))
			for _, p := range testPoints {
				assertVecNear(t, clone.TransformPoint(p), transform.TransformPoint(p), 1e-9)
			}
			got := clone.Parameters()
			for i := range parameters {
				if math.Abs(got[i]-parameters[i]) > 1e-12 {
					t.Fatalf("parameter %d = %g, want %g", i, got[i], parameters[i])
				}
			}

			before := transform.TransformPoint(testPoints[2])
			if err := transform.SetParameters(parameters[:len(parameters)-1]); !errors.Is(err, ErrInvalidParameters) {
				t.Fatalf("short parameters: err = %v", err)
			}
			bad := append([]float64(nil), parameters...)
			bad[len(bad)-1] = math.NaN()
			if err := transform.SetParameters(bad); !errors.Is(err, ErrInvalidParameters) {
				t.Fatalf("NaN parameter: err = %v", err)
			}
			if err := transform.SetFixedParameters(fixed[:1]); !errors.Is(err, ErrInvalidParameters) {
				t.Fatalf("short fixed parameters: err = %v", err)
			}
			if err := transform.UpdateParameters(make([]float64, len(parameters)), math.Inf(1)); !errors.Is(err, ErrInvalidParameters) {
				t.Fatalf("infinite factor: err = %v", err)
			}
			assertVecNear(t, transform.TransformPoint(testPoints[2]), before, 0)
		})
	}
}

func TestVersorParametersRejectOutsideUnitBall(t *testing.T) {
	// A right part on or outside the unit sphere is pulled just inside it,
	// as in ITK, so direct parameter steps always give a rotation.
	v := NewVersorRigid3D()
	must(t, v.SetParameters([]float64{0, 0, 1, 0, 0, 0}))
	if q := v.Versor(); math.Abs(q.Z-1) > 1e-9 || q.W < 0 {
		t.Fatalf("unit right part gave %+v", q)
	}
	must(t, v.SetParameters([]float64{0.8, 0.8, 0, 0, 0, 0}))
	if q := v.Versor(); math.Abs(q.X-math.Sqrt(0.5)) > 1e-9 || math.Abs(q.Y-math.Sqrt(0.5)) > 1e-9 {
		t.Fatalf("right part of norm > 1 gave %+v", q)
	}
	assertMatrixNear(t, v.Matrix().Mul(v.Matrix().Transpose()), Identity3(), 1e-9)
	s := NewSimilarity3D()
	for _, scale := range []float64{0, -1} {
		if err := s.SetParameters([]float64{0, 0, 0, 0, 0, 0, scale}); !errors.Is(err, ErrInvalidParameters) {
			t.Fatalf("scale %g: err = %v", scale, err)
		}
	}
	if err := s.UpdateParameters([]float64{0, 0, 0, 0, 0, 0, -2}, 1); !errors.Is(err, ErrInvalidParameters) {
		t.Fatalf("scale step to -1: err = %v", err)
	}
	if s.Scale() != 1 {
		t.Fatalf("rejected update changed scale to %g", s.Scale())
	}
}

func TestInverseRoundTrips(t *testing.T) {
	transforms := testTransforms(t)
	inverses := map[string]func() (Transform, error){
		"euler-zxy": func() (Transform, error) { return transforms["euler-zxy"].(*Euler3D).Inverse() },
		"euler-zyx": func() (Transform, error) { return transforms["euler-zyx"].(*Euler3D).Inverse() },
		"versor": func() (Transform, error) {
			return transforms["versor-rigid"].(*VersorRigid3D).Inverse()
		},
		"similarity": func() (Transform, error) {
			return transforms["similarity"].(*Similarity3D).Inverse()
		},
		"affine": func() (Transform, error) { return transforms["affine"].(*Affine).Inverse() },
	}
	forward := map[string]Transform{
		"euler-zxy": transforms["euler-zxy"], "euler-zyx": transforms["euler-zyx"],
		"versor": transforms["versor-rigid"], "similarity": transforms["similarity"], "affine": transforms["affine"],
	}
	for name, inverse := range inverses {
		t.Run(name, func(t *testing.T) {
			inv, err := inverse()
			must(t, err)
			if inv.Kind() != forward[name].Kind() {
				t.Fatalf("inverse kind %s, want %s", inv.Kind(), forward[name].Kind())
			}
			for _, p := range testPoints {
				assertVecNear(t, inv.TransformPoint(forward[name].TransformPoint(p)), p, 1e-9)
			}
			// The inverse keeps the centre as fixed parameters (ITK).
			got, want := inv.FixedParameters(), forward[name].FixedParameters()
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("inverse fixed parameters %v, want %v", got, want)
				}
			}
		})
	}
	singular := NewAffine()
	must(t, singular.SetMatrix(Matrix3{{1, 2, 3}, {2, 4, 6}, {0, 0, 1}}))
	if _, err := singular.Inverse(); !errors.Is(err, ErrSingularMatrix) {
		t.Fatalf("singular inverse: err = %v", err)
	}
}

func TestCenterKeepsTranslationAndOffsetKeepsMapping(t *testing.T) {
	e := NewEuler3D()
	must(t, e.SetParameters([]float64{0.1, 0.2, 0.3, 4, 5, 6}))
	c := render.Vec3{X: 10, Y: 20, Z: 30}
	must(t, e.SetCenter(c))
	// T(c) = c + t whatever the rotation.
	assertVecNear(t, e.TransformPoint(c), c.Add(render.Vec3{X: 4, Y: 5, Z: 6}), 1e-12)

	// SetOffset makes the mapping exactly M·x + offset for the current
	// centre, adjusting the translation parameters.
	before := e.TransformPoint(testPoints[2])
	offset := e.Offset()
	must(t, e.SetCenter(render.Vec3{X: -50, Y: 3, Z: 8}))
	must(t, e.SetOffset(offset))
	assertVecNear(t, e.TransformPoint(testPoints[2]), before, 1e-9)
}

func TestAdditiveUpdates(t *testing.T) {
	for _, transform := range []Transform{testTransforms(t)["euler-zxy"], testTransforms(t)["affine"]} {
		before := transform.Parameters()
		update := make([]float64, len(before))
		for i := range update {
			update[i] = float64(i+1) * 0.01
		}
		must(t, transform.UpdateParameters(update, 0.5))
		after := transform.Parameters()
		for i := range before {
			if math.Abs(after[i]-(before[i]+0.5*update[i])) > 1e-12 {
				t.Fatalf("%s parameter %d = %g, want %g", transform.Kind(), i, after[i], before[i]+0.5*update[i])
			}
		}
	}
}

// TestVersorUpdateComposesRotations checks ITK's versor step: the new
// rotation is R(current)·Rot(u, factor·|u|), and the translation is added.
func TestVersorUpdateComposesRotations(t *testing.T) {
	v := testTransforms(t)["versor-rigid"].(*VersorRigid3D)
	current := v.Versor().Matrix()
	update := []float64{0.02, -0.01, 0.03, 1, -2, 0.5}
	must(t, v.UpdateParameters(update, 2))
	step, err := VersorFromAxisAngle(render.Vec3{X: 0.02, Y: -0.01, Z: 0.03}, 2*math.Sqrt(0.02*0.02+0.01*0.01+0.03*0.03))
	must(t, err)
	assertMatrixNear(t, v.Matrix(), current.Mul(step.Matrix()), 1e-12)
	assertVecNear(t, v.Translation(), render.Vec3{X: 3, Y: -2, Z: 4}, 1e-12)

	// A zero rotation step leaves the rotation alone.
	before := v.Versor()
	must(t, v.UpdateParameters([]float64{0, 0, 0, 0, 0, 0}, 1))
	if v.Versor() != before {
		t.Fatalf("zero step changed versor %+v to %+v", before, v.Versor())
	}

	// A step across π: ITK would keep the right part of a versor with a
	// negative scalar part, mirroring the angle; the composed rotation must
	// be exact instead.
	near := NewVersorRigid3D()
	must(t, near.SetRotation(render.Vec3{Z: 1}, math.Pi-0.05))
	start := near.Matrix()
	must(t, near.UpdateParameters([]float64{0, 0, 0.2, 0, 0, 0}, 1))
	turn, err := VersorFromAxisAngle(render.Vec3{Z: 1}, 0.2)
	must(t, err)
	assertMatrixNear(t, near.Matrix(), start.Mul(turn.Matrix()), 1e-12)
	if near.Versor().W < 0 {
		t.Fatalf("versor not canonical: %+v", near.Versor())
	}

	s := testTransforms(t)["similarity"].(*Similarity3D)
	rotation := s.Versor().Matrix()
	must(t, s.UpdateParameters([]float64{0.01, 0.02, -0.01, 0, 0, 0, 0.1}, 1))
	step, err = VersorFromAxisAngle(render.Vec3{X: 0.01, Y: 0.02, Z: -0.01}, math.Sqrt(0.0006))
	must(t, err)
	assertMatrixNear(t, s.Versor().Matrix(), rotation.Mul(step.Matrix()), 1e-12)
	if math.Abs(s.Scale()-1.4) > 1e-12 {
		t.Fatalf("scale = %g, want 1.4", s.Scale())
	}
}

// TestVersorFromMatrixRoundTrip covers every Shepperd branch, including
// rotations by π and just below it, where ITK's conversion flips the sign
// of the scalar part.
func TestVersorFromMatrixRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1083))
	angles := []float64{0, 1e-9, 0.3, 1, 2, 3, math.Pi - 1e-3, math.Pi}
	axes := []render.Vec3{{X: 1}, {Y: 1}, {Z: 1}, {X: 1, Y: 1, Z: 1}, {X: -0.2, Y: 0.9, Z: -0.4}}
	for i := 0; i < 50; i++ {
		axes = append(axes, render.Vec3{X: rng.NormFloat64(), Y: rng.NormFloat64(), Z: rng.NormFloat64()})
		angles = append(angles, rng.Float64()*math.Pi)
	}
	for _, axis := range axes {
		for _, angle := range angles {
			q, err := VersorFromAxisAngle(axis, angle)
			must(t, err)
			m := q.Matrix()
			back, err := VersorFromMatrix(m, 1e-12)
			must(t, err)
			assertMatrixNear(t, back.Matrix(), m, 1e-12)
			if back.W < 0 {
				t.Fatalf("axis %+v angle %g: W = %g < 0", axis, angle, back.W)
			}
		}
	}
	if _, err := VersorFromMatrix(Matrix3{{-1, 0, 0}, {0, 1, 0}, {0, 0, 1}}, 1e-10); !errors.Is(err, ErrInvalidMatrix) {
		t.Fatalf("reflection: err = %v", err)
	}
}

// TestEulerSetMatrixRoundTrip recovers the angles of both orders from their
// matrices over the principal range, and matrices from the gimbal-lock
// branch.
func TestEulerSetMatrixRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(949))
	for _, order := range []EulerOrder{EulerZXY, EulerZYX} {
		for i := 0; i < 200; i++ {
			angles := [3]float64{
				(rng.Float64()*2 - 1) * math.Pi,
				(rng.Float64()*2 - 1) * math.Pi,
				(rng.Float64()*2 - 1) * math.Pi,
			}
			// The middle rotation of each order lies in (−π/2, π/2).
			middle := 0
			if order == EulerZYX {
				middle = 1
			}
			angles[middle] = (rng.Float64()*2 - 1) * (math.Pi/2 - 0.01)
			source := NewEuler3D()
			must(t, source.SetOrder(order))
			must(t, source.SetRotation(angles[0], angles[1], angles[2]))
			target := NewEuler3D()
			must(t, target.SetOrder(order))
			must(t, target.SetMatrix(source.Matrix()))
			x, y, z := target.Angles()
			got := [3]float64{x, y, z}
			for k := range angles {
				if math.Abs(got[k]-angles[k]) > 1e-9 {
					t.Fatalf("%s angles %v recovered as %v", order, angles, got)
				}
			}
		}
		// Gimbal lock: the matrix is reproduced even though the angles
		// are not unique.
		locked := NewEuler3D()
		must(t, locked.SetOrder(order))
		if order == EulerZXY {
			must(t, locked.SetRotation(math.Pi/2, 0.4, 0.3))
		} else {
			must(t, locked.SetRotation(0.4, math.Pi/2, 0.3))
		}
		target := NewEuler3D()
		must(t, target.SetOrder(order))
		must(t, target.SetMatrix(locked.Matrix()))
		assertMatrixNear(t, target.Matrix(), locked.Matrix(), 1e-9)
	}
}

func TestSimilaritySetMatrix(t *testing.T) {
	s := NewSimilarity3D()
	q, err := VersorFromAxisAngle(render.Vec3{X: 1, Y: -2, Z: 0.5}, 0.8)
	must(t, err)
	m := q.Matrix().Scale(2.5)
	must(t, s.SetMatrix(m))
	if math.Abs(s.Scale()-2.5) > 1e-12 {
		t.Fatalf("scale = %g", s.Scale())
	}
	assertMatrixNear(t, s.Matrix(), m, 1e-12)
	if err := s.SetMatrix(q.Matrix().Scale(-1)); !errors.Is(err, ErrInvalidMatrix) {
		t.Fatalf("negative determinant: err = %v", err)
	}
	if err := s.SetMatrix(Matrix3{{2, 0, 0}, {0, 1, 0}, {0, 0, 1}}); !errors.Is(err, ErrInvalidMatrix) {
		t.Fatalf("anisotropic scale: err = %v", err)
	}
}

func TestCompositeOrderAndCollapse(t *testing.T) {
	translate := NewAffine()
	must(t, translate.SetTranslation(render.Vec3{X: 10}))
	rotate := NewVersorRigid3D()
	must(t, rotate.SetRotation(render.Vec3{Z: 1}, math.Pi/2))
	c, err := NewComposite(translate, rotate)
	must(t, err)
	// Translate first, then rotate by 90° about Z: (1,0,0) → (11,0,0) → (0,11,0).
	assertVecNear(t, c.TransformPoint(render.Vec3{X: 1}), render.Vec3{Y: 11}, 1e-12)
	if c.NumberOfParameters() != 18 || len(c.FixedParameters()) != 6 {
		t.Fatalf("composite has %d parameters and %d fixed", c.NumberOfParameters(), len(c.FixedParameters()))
	}
	// Parameters are concatenated in application order.
	parameters := c.Parameters()
	if parameters[9] != 10 || parameters[12+2] != rotate.Parameters()[2] {
		t.Fatalf("composite parameters %v", parameters)
	}

	collapsed, err := c.CollapseToAffine()
	must(t, err)
	for _, p := range testPoints {
		assertVecNear(t, collapsed.TransformPoint(p), c.TransformPoint(p), 1e-9)
	}

	must(t, c.SetOptimized(0, false))
	if c.NumberOfParameters() != 6 {
		t.Fatalf("frozen composite has %d parameters, want 6", c.NumberOfParameters())
	}
	if err := c.SetOptimized(2, true); !errors.Is(err, ErrInvalidParameters) {
		t.Fatalf("out-of-range index: err = %v", err)
	}
	// An invalid slice is rejected before any component changes.
	scaled, err := NewComposite(NewAffine(), NewSimilarity3D())
	must(t, err)
	before := scaled.TransformPoint(testPoints[3])
	values := scaled.Parameters()
	values[9] = 5    // valid affine translation
	values[12+6] = 0 // invalid similarity scale
	if err := scaled.SetParameters(values); !errors.Is(err, ErrInvalidParameters) {
		t.Fatalf("zero scale: err = %v", err)
	}
	step := make([]float64, len(values))
	step[9], step[12+6] = 1, 2 // scale 1 − 2 = −1
	if err := scaled.UpdateParameters(step, -1); !errors.Is(err, ErrInvalidParameters) {
		t.Fatalf("negative scale step: err = %v", err)
	}
	assertVecNear(t, scaled.TransformPoint(testPoints[3]), before, 0)

	empty, err := NewComposite()
	must(t, err)
	assertVecNear(t, empty.TransformPoint(testPoints[2]), testPoints[2], 0)
	if _, err := NewComposite(nil); !errors.Is(err, ErrUnsupportedTransform) {
		t.Fatalf("nil component: err = %v", err)
	}
}

func finiteDifferenceJacobian(t *testing.T, transform Transform, p render.Vec3, h float64) Jacobian {
	t.Helper()
	parameters := transform.Parameters()
	out := NewJacobian(len(parameters))
	for k := range parameters {
		plus := transform.Clone()
		minus := transform.Clone()
		values := append([]float64(nil), parameters...)
		values[k] += h
		must(t, plus.SetParameters(values))
		values[k] -= 2 * h
		must(t, minus.SetParameters(values))
		d := plus.TransformPoint(p).Sub(minus.TransformPoint(p)).Scale(1 / (2 * h))
		out.setColumn(k, d)
	}
	return out
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func assertVecNear(t *testing.T, got, want render.Vec3, tolerance float64) {
	t.Helper()
	if math.Abs(got.X-want.X) > tolerance || math.Abs(got.Y-want.Y) > tolerance || math.Abs(got.Z-want.Z) > tolerance {
		t.Fatalf("got %+v, want %+v (tolerance %g)", got, want, tolerance)
	}
}

func assertMatrixNear(t *testing.T, got, want Matrix3, tolerance float64) {
	t.Helper()
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			if math.Abs(got[i][j]-want[i][j]) > tolerance {
				t.Fatalf("matrix [%d][%d] = %.15g, want %.15g\ngot  %v\nwant %v", i, j, got[i][j], want[i][j], got, want)
			}
		}
	}
}
