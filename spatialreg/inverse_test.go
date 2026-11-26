package spatialreg

import (
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// fieldGrid samples field at the nodes of an axis-aligned grid.
func fieldGrid(origin render.Vec3, spacing float64, dimensions [3]uint32, field func(x, y, z int, p render.Vec3) render.Vec3) *Grid {
	var vectors []render.Vec3
	for z := 0; z < int(dimensions[2]); z++ {
		for y := 0; y < int(dimensions[1]); y++ {
			for x := 0; x < int(dimensions[0]); x++ {
				p := origin.Add(render.Vec3{X: float64(x) * spacing, Y: float64(y) * spacing, Z: float64(z) * spacing})
				vectors = append(vectors, field(x, y, z, p))
			}
		}
	}
	g, err := NewGrid(origin, render.Vec3{X: 1}, render.Vec3{Y: 1}, render.Vec3{X: spacing, Y: spacing, Z: spacing}, dimensions, vectors)
	if err != nil {
		panic(err)
	}
	return g
}

func TestNewGridValidates(t *testing.T) {
	ok := []render.Vec3{{}, {X: math.NaN(), Y: math.NaN(), Z: math.NaN()}}
	if _, err := NewGrid(render.Vec3{}, render.Vec3{X: 1}, render.Vec3{Y: 1}, render.Vec3{X: 1, Y: 1, Z: 1}, [3]uint32{2, 1, 1}, ok); err != nil {
		t.Fatal(err)
	}
	for name, try := range map[string]func() error{
		"count": func() error {
			_, err := NewGrid(render.Vec3{}, render.Vec3{X: 1}, render.Vec3{Y: 1}, render.Vec3{X: 1, Y: 1, Z: 1}, [3]uint32{3, 1, 1}, ok)
			return err
		},
		"partial NaN": func() error {
			_, err := NewGrid(render.Vec3{}, render.Vec3{X: 1}, render.Vec3{Y: 1}, render.Vec3{X: 1, Y: 1, Z: 1}, [3]uint32{1, 1, 1}, []render.Vec3{{X: math.NaN()}})
			return err
		},
		"directions": func() error {
			_, err := NewGrid(render.Vec3{}, render.Vec3{X: 1}, render.Vec3{X: 1}, render.Vec3{X: 1, Y: 1, Z: 1}, [3]uint32{1, 1, 1}, ok[:1])
			return err
		},
		"spacing": func() error {
			_, err := NewGrid(render.Vec3{}, render.Vec3{X: 1}, render.Vec3{Y: 1}, render.Vec3{X: 1, Z: 1}, [3]uint32{1, 1, 1}, ok[:1])
			return err
		},
	} {
		if err := try(); !errors.Is(err, ErrInvalidObject) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// TestInvertPointMatchesITKsConstantField is itkInvertDisplacementFieldImageFilterTest in
// 3D: a 100 × 100 field of 0.5 mm with displacement (1, 1) inside and zero on
// the boundary; the inverse displacement at node (30, 30) must be (−1, −1)
// within 0.05 and the residual within the test's 0.001 mm.
func TestInvertPointMatchesITKsConstantField(t *testing.T) {
	grid := fieldGrid(render.Vec3{}, 0.5, [3]uint32{100, 100, 3}, func(x, y, _ int, _ render.Vec3) render.Vec3 {
		if x == 0 || y == 0 || x == 99 || y == 99 {
			return render.Vec3{}
		}
		return render.Vec3{X: 1, Y: 1}
	})
	r := &Registration{Pre: identityMatrix(), Post: identityMatrix(), Grid: grid}
	source := render.Vec3{X: 15, Y: 15, Z: 0.5}
	result, err := r.InvertPoint(source, InverseOptions{MaximumIterations: 50, MaxResidual: 0.001})
	if err != nil {
		t.Fatal(err)
	}
	if inverse := result.Registered.Sub(source); inverse.Sub(render.Vec3{X: -1, Y: -1}).Length() > 0.05 {
		t.Fatalf("inverse displacement %v", inverse)
	}
	if result.Residual > 0.001 || result.JacobianDeterminant != 1 {
		t.Fatalf("residual %g, determinant %g", result.Residual, result.JacobianDeterminant)
	}
	forward, err := r.MapRegisteredToSource(result.Registered)
	if err != nil || forward.Sub(source).Length() > 0.001 {
		t.Fatalf("forward of the inverse %v: %v", forward, err)
	}
}

// TestInvertPointMatchesAnalyticInverses: fields with a closed-form inverse
// through pre- and post-matrices. A linear field is interpolated exactly, so
// x = A⁻¹(Post⁻¹(s) − b) solved through (A + K) must be recovered to
// rounding; a smooth sinusoidal field is checked against the forward map.
func TestInvertPointMatchesAnalyticInverses(t *testing.T) {
	angle := 10 * math.Pi / 180
	pre := Matrix{Values: render.GeometryAffine{
		math.Cos(angle), -math.Sin(angle), 0, 4,
		math.Sin(angle), math.Cos(angle), 0, -2,
		0, 0, 1, 3,
		0, 0, 0, 1,
	}}
	post := Matrix{Values: render.GeometryAffine{
		1.1, 0, 0, -7,
		0, 0.9, 0, 5,
		0, 0, 1, 1,
		0, 0, 0, 1,
	}}
	// D(x) = K·x with K a stretch and a shear.
	k := [3][3]float64{{0.3, 0, 0}, {0.2, 0, 0}, {0, 0.1, -0.15}}
	linear := fieldGrid(render.Vec3{X: -50, Y: -50, Z: -50}, 5, [3]uint32{21, 21, 21}, func(_, _, _ int, p render.Vec3) render.Vec3 {
		return render.Vec3{X: k[0][0]*p.X + k[0][1]*p.Y + k[0][2]*p.Z, Y: k[1][0]*p.X + k[1][1]*p.Y + k[1][2]*p.Z, Z: k[2][0]*p.X + k[2][1]*p.Y + k[2][2]*p.Z}
	})
	r := &Registration{Pre: pre, Post: post, Grid: linear}
	a := linearPart(pre.Values)
	var m matrix3
	for i := range m {
		for j := range m[i] {
			m[i][j] = a[i][j] + k[i][j]
		}
	}
	for _, want := range []render.Vec3{{X: 10, Y: -20, Z: 5}, {X: -30, Y: 25, Z: -12}, {}} {
		source, err := r.MapRegisteredToSource(want)
		if err != nil {
			t.Fatal(err)
		}
		result, err := r.InvertPoint(source, InverseOptions{Tolerance: 1e-9})
		if err != nil {
			t.Fatal(err)
		}
		if result.Registered.Sub(want).Length() > 1e-6 {
			t.Fatalf("inverse %v, want %v after %d iterations", result.Registered, want, result.Iterations)
		}
		if want := m.Determinant() / a.Determinant(); math.Abs(result.JacobianDeterminant-want) > 1e-9 {
			t.Fatalf("determinant %g, want %g", result.JacobianDeterminant, want)
		}
	}
	// A smooth field with a displacement up to 4 mm.
	smooth := fieldGrid(render.Vec3{X: -50, Y: -50, Z: -50}, 2.5, [3]uint32{41, 41, 41}, func(_, _, _ int, p render.Vec3) render.Vec3 {
		return render.Vec3{X: 4 * math.Sin(p.Y/15), Y: 3 * math.Cos(p.Z/20), Z: 2 * math.Sin((p.X+p.Y)/25)}
	})
	r.Grid = smooth
	for _, want := range []render.Vec3{{X: 12, Y: -7, Z: 20}, {X: -35, Y: 30, Z: -25}} {
		source, err := r.MapRegisteredToSource(want)
		if err != nil {
			t.Fatal(err)
		}
		registered, err := r.MapSourceToRegistered(source)
		if err != nil {
			t.Fatal(err)
		}
		if registered.Sub(want).Length() > 1e-3 {
			t.Fatalf("inverse %v, want %v", registered, want)
		}
	}
}

// TestInvertPointFailsClosed: a folding field, too few iterations, and a
// point outside the grid are refused rather than mapped.
func TestInvertPointFailsClosed(t *testing.T) {
	folding := fieldGrid(render.Vec3{X: -20, Y: -20, Z: -20}, 2, [3]uint32{21, 21, 21}, func(_, _, _ int, p render.Vec3) render.Vec3 {
		return render.Vec3{X: -1.5 * p.X}
	})
	r := &Registration{Pre: identityMatrix(), Post: identityMatrix(), Grid: folding}
	if _, err := r.MapSourceToRegistered(render.Vec3{X: 3, Y: 1, Z: 2}); !errors.Is(err, ErrFolding) {
		t.Fatalf("folding field: %v", err)
	}
	smooth := fieldGrid(render.Vec3{X: -50, Y: -50, Z: -50}, 2.5, [3]uint32{41, 41, 41}, func(_, _, _ int, p render.Vec3) render.Vec3 {
		return render.Vec3{X: 6 * math.Sin(p.Y/10), Y: 5 * math.Cos(p.X/12)}
	})
	r.Grid = smooth
	if _, err := r.InvertPoint(render.Vec3{X: 10, Y: 10, Z: 0}, InverseOptions{MaximumIterations: 1}); !errors.Is(err, ErrInverseNotConverged) {
		t.Fatalf("one iteration: %v", err)
	}
	if _, err := r.MapSourceToRegistered(render.Vec3{X: 200}); !errors.Is(err, ErrOutsideDomain) {
		t.Fatalf("outside the grid: %v", err)
	}
}

// TestVolumeChangeMapsTheJacobianDeterminant: a linear field's determinant
// is exact everywhere; a folding field is counted.
func TestVolumeChangeMapsTheJacobianDeterminant(t *testing.T) {
	linear := fieldGrid(render.Vec3{}, 2, [3]uint32{8, 9, 10}, func(_, _, _ int, p render.Vec3) render.Vec3 {
		return render.Vec3{X: 0.3 * p.X, Y: 0.2 * p.X, Z: -0.1 * p.Z}
	})
	r := &Registration{Pre: identityMatrix(), Post: identityMatrix(), Grid: linear}
	change, err := r.VolumeChange()
	if err != nil {
		t.Fatal(err)
	}
	if want := 1.3 * 0.9; math.Abs(change.Min-want) > 1e-6 || math.Abs(change.Max-want) > 1e-6 || change.Folded != 0 || len(change.Values) != 8*9*10 {
		t.Fatalf("volume change %g..%g, folded %d", change.Min, change.Max, change.Folded)
	}
	r.Grid = fieldGrid(render.Vec3{}, 2, [3]uint32{5, 5, 5}, func(_, _, _ int, p render.Vec3) render.Vec3 {
		return render.Vec3{X: -1.5 * p.X}
	})
	if change, err = r.VolumeChange(); err != nil || change.Folded != 125 || change.Max > -0.49 {
		t.Fatalf("folding field: %+v %v", change, err)
	}
	if _, err := (&Registration{Pre: identityMatrix(), Post: identityMatrix()}).VolumeChange(); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("no grid: %v", err)
	}
}
