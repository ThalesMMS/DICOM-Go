package registration

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// Independent per-cell polynomial basis and derivative (not cubicBSpline or
// the product's coefficient-difference bound). Index is strictly inside support.
func boundedSplinePolynomial(b *BSpline, index render.Vec3) (render.Vec3, Matrix3) {
	position := [3]float64{index.X, index.Y, index.Z}
	var first [3]int
	var w, d [3][4]float64
	for a, t := range position {
		cell := math.Floor(t)
		u := t - cell
		first[a] = int(cell) - 1
		w[a] = [4]float64{math.Pow(1-u, 3) / 6, (3*u*u*u - 6*u*u + 4) / 6, (-3*u*u*u + 3*u*u + 3*u + 1) / 6, u * u * u / 6}
		d[a] = [4]float64{-.5 * (1 - u) * (1 - u), 1.5*u*u - 2*u, -1.5*u*u + u + .5, .5 * u * u}
	}
	var displacement [3]float64
	var perIndex Matrix3
	count := b.count()
	for z := 0; z < 4; z++ {
		for y := 0; y < 4; y++ {
			for x := 0; x < 4; x++ {
				i := first[0] + x + (first[1]+y)*b.size[0] + (first[2]+z)*b.size[0]*b.size[1]
				for component := 0; component < 3; component++ {
					c := b.coefficients[component*count+i]
					displacement[component] += w[0][x] * w[1][y] * w[2][z] * c
					perIndex[component][0] += d[0][x] * w[1][y] * w[2][z] * c
					perIndex[component][1] += w[0][x] * d[1][y] * w[2][z] * c
					perIndex[component][2] += w[0][x] * w[1][y] * d[2][z] * c
				}
			}
		}
	}
	// Explicit cofactor inverse of direction, then divide rows by grid spacing.
	a := b.GridDirection()
	det := a[0][0]*(a[1][1]*a[2][2]-a[1][2]*a[2][1]) - a[0][1]*(a[1][0]*a[2][2]-a[1][2]*a[2][0]) + a[0][2]*(a[1][0]*a[2][1]-a[1][1]*a[2][0])
	inverse := Matrix3{{a[1][1]*a[2][2] - a[1][2]*a[2][1], a[0][2]*a[2][1] - a[0][1]*a[2][2], a[0][1]*a[1][2] - a[0][2]*a[1][1]}, {a[1][2]*a[2][0] - a[1][0]*a[2][2], a[0][0]*a[2][2] - a[0][2]*a[2][0], a[0][2]*a[1][0] - a[0][0]*a[1][2]}, {a[1][0]*a[2][1] - a[1][1]*a[2][0], a[0][1]*a[2][0] - a[0][0]*a[2][1], a[0][0]*a[1][1] - a[0][1]*a[1][0]}}
	sp := b.GridSpacing()
	spacing := [3]float64{sp.X, sp.Y, sp.Z}
	out := Identity3()
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			for k := 0; k < 3; k++ {
				out[i][j] += perIndex[i][k] * inverse[k][j] / (det * spacing[k])
			}
		}
	}
	return render.Vec3{X: displacement[0], Y: displacement[1], Z: displacement[2]}, out
}
func boundedJacDet(a Matrix3) float64 {
	return a[0][0]*(a[1][1]*a[2][2]-a[1][2]*a[2][1]) - a[0][1]*(a[1][0]*a[2][2]-a[1][2]*a[2][0]) + a[0][2]*(a[1][0]*a[2][1]-a[1][1]*a[2][0])
}
func boundedGridPoint(b *BSpline, p render.Vec3) render.Vec3 {
	return b.GridOrigin().Add(b.GridDirection().MulVec(render.Vec3{X: p.X * b.spacing.X, Y: p.Y * b.spacing.Y, Z: p.Z * b.spacing.Z}))
}
func boundedCheckJacobianOracle(t *testing.T, b *BSpline) {
	t.Helper()
	bound, err := b.JacobianBound(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for z := 1.125; z < float64(b.size[2]-2); z += .375 {
		for y := 1.125; y < float64(b.size[1]-2); y += .375 {
			for x := 1.125; x < float64(b.size[0]-2); x += .375 {
				index := render.Vec3{X: x, Y: y, Z: z}
				p := boundedGridPoint(b, index)
				displacement, want := boundedSplinePolynomial(b, index)
				got := b.JacobianWithRespectToPosition(p)
				if b.Displacement(p).Sub(displacement).Length() > 1e-11 {
					t.Fatal("polynomial displacement")
				}
				var frobenius float64
				for i := 0; i < 3; i++ {
					for j := 0; j < 3; j++ {
						if math.Abs(got[i][j]-want[i][j]) > 1e-11 {
							t.Fatalf("Jacobian [%d,%d] %g want%g", i, j, got[i][j], want[i][j])
						}
						v := want[i][j]
						if i == j {
							v -= 1
						}
						frobenius += v * v
					}
				}
				if math.Sqrt(frobenius) > bound.DisplacementGradientNormUpper+1e-11 {
					t.Fatal("gradient upper violated")
				}
				if bound.PositiveJacobianCertified && boundedJacDet(want) < bound.DeterminantLower-1e-10 {
					t.Fatal("determinant lower violated")
				}
			}
		}
	}
}
func TestBSplineJacobianBoundPolynomialAndRefinement(t *testing.T) {
	direction := Matrix3{{.9, -.3, .1}, {.4, 1, .05}, {0, .1, 1.1}}
	b, err := NewBSpline([3]int{6, 5, 4}, render.Vec3{X: 2, Y: -3, Z: 1}, render.Vec3{X: 3, Y: 4, Z: 5}, direction)
	if err != nil {
		t.Fatal(err)
	}
	parameters := b.Parameters()
	count := b.count()
	for component := 0; component < 3; component++ {
		for z := 0; z < b.size[2]; z++ {
			for y := 0; y < b.size[1]; y++ {
				for x := 0; x < b.size[0]; x++ {
					i := x + y*b.size[0] + z*b.size[0]*b.size[1]
					parameters[component*count+i] = .04 * math.Sin(float64(x+2*y+3*z+component))
				}
			}
		}
	}
	if err := b.SetParameters(parameters); err != nil {
		t.Fatal(err)
	}
	before := b.Parameters()
	bound, err := b.JacobianBound(context.Background())
	if err != nil || !bound.PositiveJacobianCertified {
		t.Fatalf("bound %+v %v", bound, err)
	}
	boundedCheckJacobianOracle(t, b)
	fine, err := b.Refine([3]bool{true, true, true})
	if err != nil {
		t.Fatal(err)
	}
	boundedCheckJacobianOracle(t, fine)
	for i, v := range b.Parameters() {
		if v != before[i] {
			t.Fatal("source coefficient changed")
		}
	}
	// Position finite difference is an additional distinct numerical check.
	p := boundedGridPoint(b, render.Vec3{X: 2.3, Y: 1.7, Z: 1.4})
	jac := b.JacobianWithRespectToPosition(p)
	epsilon := 1e-5
	for a := 0; a < 3; a++ {
		delta := [3]float64{}
		delta[a] = epsilon
		d := render.Vec3{X: delta[0], Y: delta[1], Z: delta[2]}
		v := b.TransformPoint(p.Add(d)).Sub(b.TransformPoint(p.Sub(d))).Scale(.5 / epsilon)
		want := [3]float64{v.X, v.Y, v.Z}
		for row := 0; row < 3; row++ {
			if math.Abs(jac[row][a]-want[row]) > 1e-9 {
				t.Fatal("position finite difference")
			}
		}
	}
}
func TestBSplineJacobianBoundDisplacementBoxCanFold(t *testing.T) {
	b, err := NewBSpline([3]int{5, 5, 5}, render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3())
	if err != nil {
		t.Fatal(err)
	}
	p := b.Parameters()
	for z := 0; z < 5; z++ {
		for y := 0; y < 5; y++ {
			for x := 0; x < 5; x++ {
				p[x+5*y+25*z] = -2 * float64(x-2)
			}
		}
	}
	if err := b.SetParameters(p); err != nil {
		t.Fatal(err)
	}
	bound, err := b.JacobianBound(context.Background())
	if err != nil || bound.PositiveJacobianCertified || bound.DeterminantLower != 0 {
		t.Fatalf("fold bound %+v %v", bound, err)
	}
	_, jac := boundedSplinePolynomial(b, render.Vec3{X: 1.5, Y: 1.5, Z: 1.5})
	if math.Abs(boundedJacDet(jac)+1) > 1e-12 {
		t.Fatalf("analytic fold determinant%g", boundedJacDet(jac))
	}
	for _, v := range p {
		if math.Abs(v) > 4 {
			t.Fatal("counterexample exceeds finite box")
		}
	}
}
func TestBSplineJacobianBoundConservativeThresholdAndInitial(t *testing.T) {
	b, err := NewBSpline([3]int{4, 4, 4}, render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3())
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []float64{0, .5, 1 - 128*2.220446049250313e-16, math.Nextafter(1, 0), 1, math.Nextafter(1, 2)} {
		values := b.Parameters()
		for z := 0; z < 4; z++ {
			for y := 0; y < 4; y++ {
				for x := 0; x < 4; x++ {
					if x > 0 {
						values[x+4*y+16*z] = q
					} else {
						values[x+4*y+16*z] = 0
					}
				}
			}
		}
		if err := b.SetParameters(values); err != nil {
			t.Fatal(err)
		}
		result, err := b.JacobianBound(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if result.DisplacementGradientNormUpper < q {
			t.Fatal("rounding understated norm")
		}
		if q <= 1-128*2.220446049250313e-16 {
			if !result.PositiveJacobianCertified || result.DeterminantLower > math.Pow(1-q, 3) {
				t.Fatalf("threshold%g %+v", q, result)
			}
		} else if result.PositiveJacobianCertified {
			t.Fatalf("roundoff threshold falsely certified%g %+v", q, result)
		}
	}
	for _, matrix := range []Matrix3{Identity3(), {{2, .3, 0}, {0, 3, .1}, {0, 0, 4}}, {{.9, -.4, 0}, {.4, .9, 0}, {0, 0, 1}}} {
		lower := jacobianMatrixDeterminantLower(matrix)
		det := boundedJacDet(matrix)
		if lower <= 0 || lower > det {
			t.Fatalf("matrix bound%g actual%g", lower, det)
		}
	}
	if jacobianMatrixDeterminantLower(Matrix3{{-1, 0, 0}, {0, 1, 0}, {0, 0, 1}}) > 0 {
		t.Fatal("reflection certified")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.JacobianBound(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation", err)
	}
	if _, err := b.JacobianBound(nil); !errors.Is(err, ErrInvalidParameters) {
		t.Fatal("nil context", err)
	}
}

func TestBSplineJacobianBoundRejectsNonFiniteUpdate(t *testing.T) {
	b, err := NewBSpline([3]int{4, 4, 4}, render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3())
	if err != nil {
		t.Fatal(err)
	}
	parameters := b.Parameters()
	for i := range parameters {
		parameters[i] = math.MaxFloat64
	}
	if err := b.SetParameters(parameters); err != nil {
		t.Fatal(err)
	}
	// Existing UpdateParameters validates its inputs, but arithmetic overflow
	// can make stored coefficients nonfinite. A certificate must reject them.
	if err := b.UpdateParameters(parameters, 1); err != nil {
		t.Fatal(err)
	}
	bound, err := b.JacobianBound(context.Background())
	if !errors.Is(err, ErrInvalidParameters) || bound.PositiveJacobianCertified {
		t.Fatalf("nonfinite update certificate %+v %v", bound, err)
	}
}
