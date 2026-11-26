package registration

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

func TestPrincipalMomentsIndependentPhysicalPointOracle(t *testing.T) {
	data := []float32{0, 2, 3, 1, 4, 0, 1, 2, 0, 5, 2, 1}
	rot := NewEuler3D()
	_ = rot.SetRotation(.3, -.2, .7)
	origin := render.Vec3{X: 1000000, Y: -700000, Z: 45}
	im, err := NewImage([3]int{3, 2, 2}, data, origin, render.Vec3{X: .7, Y: 2, Z: 4}, rot.Matrix())
	if err != nil {
		t.Fatal(err)
	}
	m, err := ImagePrincipalMoments(nil, im, PrincipalAxisOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var mass float64
	var center render.Vec3
	for i, w := range data {
		p := im.Point(render.Vec3{X: float64(i % 3), Y: float64(i / 3 % 2), Z: float64(i / 6)})
		mass += float64(w)
		center = center.Add(p.Scale(float64(w)))
	}
	center = center.Scale(1 / mass)
	var covariance Matrix3
	for i, w := range data {
		p := im.Point(render.Vec3{X: float64(i % 3), Y: float64(i / 3 % 2), Z: float64(i / 6)}).Sub(center)
		v := [3]float64{p.X, p.Y, p.Z}
		for r := 0; r < 3; r++ {
			for c := 0; c < 3; c++ {
				covariance[r][c] += float64(w) * v[r] * v[c] / mass
			}
		}
	}
	if m.Mass != mass || m.Center.Sub(center).Length() > 1e-8 {
		t.Fatalf("moments center/mass %+v want%v/%v", m, center, mass)
	}
	for r := 0; r < 3; r++ {
		for c := 0; c < 3; c++ {
			if math.Abs(m.Covariance[r][c]-covariance[r][c]) > 1e-8 {
				t.Fatal("physical covariance differs from independent point summation")
			}
		}
	}
	diagonal := m.Axes.Transpose().Mul(m.Covariance).Mul(m.Axes)
	if !m.Axes.orthogonal(1e-12) || math.Abs(m.Axes.Determinant()-1) > 1e-12 {
		t.Fatal("basis not right-handed orthonormal")
	}
	for r := 0; r < 3; r++ {
		for c := 0; c < 3; c++ {
			want := 0.0
			if r == c {
				want = m.Values[r]
			}
			if math.Abs(diagonal[r][c]-want) > 1e-10 {
				t.Fatalf("eigen residual[%d,%d] %g/%g", r, c, diagonal[r][c], want)
			}
		}
	}
}

func TestPrincipalMomentPoliciesCancellationAndDegeneracy(t *testing.T) {
	data := []float32{-2, 0, 1, 3}
	im, _ := NewImage([3]int{4, 1, 1}, data, render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3())
	m, err := ImagePrincipalMoments(nil, im, PrincipalAxisOptions{IntensityOffset: 1})
	if err != nil || m.Mass != 7 || math.Abs(m.Center.X-17.0/7) > 1e-12 {
		t.Fatalf("explicit clipped weights %+v/%v", m, err)
	}
	if _, err = PrincipalAxisCandidates(nil, im, im, PrincipalAxisOptions{}); !errors.Is(err, ErrAmbiguousPrincipalAxes) {
		t.Fatalf("degenerate axes=%v", err)
	}
	if _, err = ImagePrincipalMoments(nil, im, PrincipalAxisOptions{MaxVoxels: 3}); !errors.Is(err, ErrPrincipalAxes) {
		t.Fatal("voxel budget")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = ImagePrincipalMoments(ctx, im, PrincipalAxisOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel before work")
	}
	for _, v := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
		data[0] = v
		if _, err = ImagePrincipalMoments(nil, im, PrincipalAxisOptions{}); !errors.Is(err, ErrPrincipalAxes) {
			t.Fatal("nonfinite sample")
		}
	}
	clear(data)
	if _, err = ImagePrincipalMoments(nil, im, PrincipalAxisOptions{}); !errors.Is(err, ErrNoMass) {
		t.Fatal("zero mass")
	}
}

func TestPrincipalEigenIndependentKnownMatrices(t *testing.T) {
	rot := NewEuler3D()
	_ = rot.SetRotation(.63, -.43, 1.1)
	for _, scale := range []float64{1e-200, 1, 1e200} {
		want := [3]float64{scale, 2 * scale, 9 * scale}
		a := rot.Matrix().Mul(Matrix3{{want[0], 0, 0}, {0, want[1], 0}, {0, 0, want[2]}}).Mul(rot.Matrix().Transpose())
		values, axes, err := principalEigen(a)
		if err != nil {
			t.Fatal(err)
		}
		for i, v := range values {
			if math.Abs(v/want[i]-1) > 1e-12 {
				t.Fatalf("eigen %g/%g", v, want[i])
			}
		}
		if !axes.orthogonal(1e-12) {
			t.Fatal("orthogonality")
		}
	}
}
