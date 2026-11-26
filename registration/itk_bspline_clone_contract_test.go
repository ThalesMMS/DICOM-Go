package registration

import (
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// ITKBSplineTransform.CopyingClone constructs equivalent transforms from
// domain geometry, fixed parameters and a clone. Cubic splines reproduce
// affine coefficient fields at interior points, giving an analytic oracle.
func TestITKBSplineCopiesCoefficientGeometry(t *testing.T) {
	origin := render.Vec3{X: 5, Y: -9, Z: 2}
	spacing := render.Vec3{X: 2, Y: 4, Z: 8}
	direction := Matrix3{{0, -1, 0}, {1, 0, 0}, {0, 0, 1}}
	size := [3]int{5, 6, 7}
	original, err := NewBSpline(size, origin, spacing, direction)
	must(t, err)
	count := size[0] * size[1] * size[2]
	parameters := make([]float64, 3*count)
	for k := 0; k < size[2]; k++ {
		for j := 0; j < size[1]; j++ {
			for i := 0; i < size[0]; i++ {
				index := (k*size[1]+j)*size[0] + i
				x, y, z := float64(i), float64(j), float64(k)
				parameters[index] = 1 + 2*x + y/2 - z
				parameters[count+index] = -y/2 + 2*z
				parameters[2*count+index] = 3 - x + 3*y + z/4
			}
		}
	}
	must(t, original.SetParameters(parameters))
	// Continuous coefficient index (1.5,2.25,3.5) in the rotated grid.
	point := render.Vec3{X: -4, Y: -6, Z: 30}
	want := render.Vec3{X: -2.375, Y: -.125, Z: 39.125}
	assertITKPoint(t, original.TransformPoint(point), want, 1e-12)
	wantFixed := []float64{5, 6, 7, 5, -9, 2, 2, 4, 8, 0, -1, 0, 1, 0, 0, 0, 0, 1}

	tests := []struct {
		name string
		copy func(*testing.T) *BSpline
	}{
		{"clone", func(t *testing.T) *BSpline {
			clone, ok := original.Clone().(*BSpline)
			if !ok {
				t.Fatal("clone changed transform family")
			}
			return clone
		}},
		{"fixed-parameters", func(t *testing.T) *BSpline {
			clone := NewBSplineIdentityForTest(t)
			must(t, clone.SetFixedParameters(original.FixedParameters()))
			must(t, clone.SetParameters(original.Parameters()))
			return clone
		}},
		{"physical-domain", func(t *testing.T) *BSpline {
			// The domain starts one coefficient cell inside the grid on each axis.
			clone, err := NewBSplineForDomain(render.Vec3{X: 1, Y: -7, Z: 10}, render.Vec3{X: 4, Y: 12, Z: 32}, direction, [3]int{2, 3, 4})
			must(t, err)
			must(t, clone.SetParameters(original.Parameters()))
			return clone
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copyOfOriginal := test.copy(t)
			if !slices.Equal(copyOfOriginal.FixedParameters(), wantFixed) || !slices.Equal(copyOfOriginal.Parameters(), parameters) {
				t.Fatal("coefficient geometry or layout changed while copying")
			}
			assertITKPoint(t, copyOfOriginal.TransformPoint(point), want, 1e-12)
			update := make([]float64, 3*count)
			for i := 0; i < count; i++ {
				update[i] = 4
			}
			must(t, copyOfOriginal.UpdateParameters(update, 1))
			assertITKPoint(t, copyOfOriginal.TransformPoint(point), want.Add(render.Vec3{X: 4}), 1e-12)
			assertITKPoint(t, original.TransformPoint(point), want, 1e-12)
			if !slices.Equal(original.Parameters(), parameters) || !slices.Equal(original.FixedParameters(), wantFixed) {
				t.Fatal("coefficient updates in the copy changed the original")
			}
		})
	}
}
