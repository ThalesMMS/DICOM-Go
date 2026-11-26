package registration

import (
	"errors"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// itkAffineTransformTest distinguishes points (including the centre and
// translation) from displacement vectors, and checks inverse parameters.
func TestITKAffinePointVectorAndInverse(t *testing.T) {
	center := render.Vec3{X: 3, Y: -2, Z: 1}
	translation := render.Vec3{X: 4, Y: -3, Z: 2}
	point := render.Vec3{X: 5, Y: 1, Z: -3}
	vector := render.Vec3{X: 2, Y: 3, Z: -4}
	translatedCenter := render.Vec3{X: 7, Y: -5, Z: 3}
	tests := []struct {
		name   string
		matrix Matrix3
		point  render.Vec3
		vector render.Vec3
	}{
		{"translation", Identity3(), render.Vec3{X: 9, Y: -2, Z: -1}, render.Vec3{X: 2, Y: 3, Z: -4}},
		{"isotropic-scale", Matrix3{{2, 0, 0}, {0, 2, 0}, {0, 0, 2}}, render.Vec3{X: 11, Y: 1, Z: -5}, render.Vec3{X: 4, Y: 6, Z: -8}},
		{"anisotropic-scale", Matrix3{{2, 0, 0}, {0, .5, 0}, {0, 0, 4}}, render.Vec3{X: 11, Y: -3.5, Z: -13}, render.Vec3{X: 4, Y: 1.5, Z: -16}},
		{"rotation", Matrix3{{0, -1, 0}, {1, 0, 0}, {0, 0, 1}}, render.Vec3{X: 4, Y: -3, Z: -1}, render.Vec3{X: -3, Y: 2, Z: -4}},
		{"shear", Matrix3{{1, 2, -.5}, {0, 1, .25}, {0, 0, 1}}, render.Vec3{X: 17, Y: -3, Z: -1}, render.Vec3{X: 10, Y: 2, Z: -4}},
		{"reflection", Matrix3{{-1, 0, 0}, {0, 1, 0}, {0, 0, 1}}, render.Vec3{X: 5, Y: -2, Z: -1}, render.Vec3{X: -2, Y: 3, Z: -4}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forward := NewAffine()
			must(t, forward.SetMatrix(test.matrix))
			must(t, forward.SetTranslation(translation))
			must(t, forward.SetCenter(center))
			assertITKPoint(t, forward.TransformPoint(center), translatedCenter, 1e-12)
			assertITKPoint(t, forward.TransformPoint(point), test.point, 1e-12)
			assertITKPoint(t, forward.TransformVector(vector), test.vector, 1e-12)
			assertITKPoint(t, forward.GeometryAffine().TransformPoint(point), test.point, 1e-12)

			inverse, err := forward.Inverse()
			must(t, err)
			assertITKPoint(t, inverse.TransformPoint(test.point), point, 1e-12)
			assertITKPoint(t, inverse.TransformPoint(translatedCenter), center, 1e-12)
			assertITKPoint(t, inverse.TransformVector(test.vector), vector, 1e-12)
			restored, err := inverse.Inverse()
			must(t, err)
			if !slices.Equal(restored.Parameters(), forward.Parameters()) || !slices.Equal(restored.FixedParameters(), forward.FixedParameters()) {
				t.Fatalf("double inverse parameters %v / %v, want %v / %v",
					restored.Parameters(), restored.FixedParameters(), forward.Parameters(), forward.FixedParameters())
			}

			forward.SetIdentity()
			if forward.Center() != center || forward.Translation() != (render.Vec3{}) || forward.Offset() != (render.Vec3{}) {
				t.Fatalf("identity retained centre, translation and offset as %+v, %+v, %+v", forward.Center(), forward.Translation(), forward.Offset())
			}
			assertITKPoint(t, forward.TransformPoint(point), point, 0)
			assertITKPoint(t, inverse.TransformPoint(test.point), point, 1e-12)
		})
	}
}

// A forward affine may be evaluated before an optimizer makes it invertible.
func TestITKAffineInverseRejectsCollapsedAxes(t *testing.T) {
	tests := []struct {
		name   string
		matrix Matrix3
	}{
		{"rank-zero", Matrix3{}},
		{"dependent-rows", Matrix3{{1, 2, 0}, {2, 4, 0}, {0, 0, 1}}},
		{"ill-conditioned", Matrix3{{1, 0, 0}, {0, 1, 0}, {0, 0, 1e-12}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forward := NewAffine()
			must(t, forward.SetMatrix(test.matrix))
			center := render.Vec3{X: -1, Y: 2, Z: 3}
			must(t, forward.SetCenter(center))
			got, err := forward.Inverse()
			if got != nil || !errors.Is(err, ErrSingularMatrix) {
				t.Fatalf("Inverse() = %v, %v, want nil and ErrSingularMatrix", got, err)
			}
			if forward.Matrix() != test.matrix || forward.Center() != center {
				t.Fatalf("rejected inversion changed the forward transform: %v at %+v", forward.Matrix(), forward.Center())
			}
			assertITKPoint(t, forward.TransformPoint(center), center, 1e-12)
		})
	}
}

// itkTestTransformGetInverse uses a general affine result for inverse mappings
// that do not have a ScaleVersor or ScaleSkewVersor parameterization.
func TestITKScaleVersorInvertsAsAffine(t *testing.T) {
	type scaleTransform interface {
		MatrixTransform
		Inverse() (*Affine, error)
	}
	tests := []struct {
		name   string
		model  scaleTransform
		skew   []float64
		mapped render.Vec3
	}{
		{"scale", NewScaleVersor3D(), nil, render.Vec3{X: 8, Y: -4.5, Z: 15}},
		{"scale-and-skew", NewScaleSkewVersor3D(), []float64{.25, 0, -.5, 0, .125, -.25}, render.Vec3{X: 7.5, Y: -5.5, Z: 15.75}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			must(t, test.model.SetFixedParameters([]float64{1, -3, 5}))
			must(t, test.model.SetParameters(append([]float64{0, 0, 0, 4, 0, 2, 1.5, .75, 2}, test.skew...)))
			point := render.Vec3{X: 3, Y: -5, Z: 9}
			assertITKPoint(t, test.model.TransformPoint(point), test.mapped, 1e-12)
			inverse, err := test.model.Inverse()
			must(t, err)
			if inverse.Center() != test.model.Center() {
				t.Fatalf("inverse centre %+v, want %+v", inverse.Center(), test.model.Center())
			}
			assertITKPoint(t, inverse.TransformPoint(test.mapped), point, 1e-12)
			other := render.Vec3{X: 4, Y: 1, Z: -3}
			assertITKPoint(t, test.model.TransformPoint(inverse.TransformPoint(other)), other, 1e-12)
		})
	}
}

func assertITKPoint(t *testing.T, got, want render.Vec3, tolerance float64) {
	t.Helper()
	if !finiteVec(got) {
		t.Fatalf("got non-finite point %+v, want %+v", got, want)
	}
	assertVecNear(t, got, want, tolerance)
}
