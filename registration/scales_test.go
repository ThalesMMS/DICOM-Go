package registration

import (
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

func relativeError(got, want float64) float64 { return math.Abs(got-want) / math.Abs(want) }

// TestPhysicalShiftScalesMatchITK reproduces
// itkRegistrationParameterScalesFromPhysicalShiftTest on a 100×100 image of
// unit spacing: for the identity affine, a matrix entry of column c moves
// the corner (99, 99) by 0.01·99, so its scale is 99² = 9801, the
// translations' 1 (and here the z column, which moves no corner of a single
// slice, the smallest non-zero scale, 1); the identity parameters as a step
// move that corner by 0.01·√(99²+99²), a step scale of 140.007.
func TestPhysicalShiftScalesMatchITK(t *testing.T) {
	image, err := NewImage([3]int{100, 100, 1}, make([]float32, 100*100), render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3())
	if err != nil {
		t.Fatal(err)
	}
	estimator := PhysicalShiftScales{Transform: NewAffine(), Points: CornerPoints(image)}
	scales, err := estimator.Scales()
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{9801, 9801, 1, 9801, 9801, 1, 9801, 9801, 1, 1, 1, 1}
	for i := range want {
		if relativeError(scales[i], want[i]) > 0.01 {
			t.Fatalf("scales %v, want %v", scales, want)
		}
	}
	step, err := estimator.StepScale(NewAffine().Parameters())
	if err != nil || relativeError(step, math.Sqrt(99*99+99*99)) > 0.01 {
		t.Fatalf("step scale %v (%v), want 140.007", step, err)
	}
	if MaximumStepSize(image) != 1 || len(CornerPoints(image)) != 4 {
		t.Fatalf("maximum step %v, %d corners", MaximumStepSize(image), len(CornerPoints(image)))
	}
	if _, err := (PhysicalShiftScales{Transform: NewAffine()}).Scales(); !errors.Is(err, ErrNoSamplePoints) {
		t.Fatalf("no points: %v", err)
	}
}

// TestPhysicalShiftScalesOfARigidTransform: for an Euler transform centred
// in a 3D volume, a rotation's scale is the squared distance of the
// farthest corner from its axis, and a translation's 1.
func TestPhysicalShiftScalesOfARigidTransform(t *testing.T) {
	image, err := NewImage([3]int{11, 11, 11}, make([]float32, 11*11*11), render.Vec3{}, render.Vec3{X: 2, Y: 2, Z: 2}, Identity3())
	if err != nil {
		t.Fatal(err)
	}
	transform := NewEuler3D()
	_ = transform.SetCenter(render.Vec3{X: 10, Y: 10, Z: 10})
	scales, err := PhysicalShiftScales{Transform: transform, Points: CornerPoints(image)}.Scales()
	if err != nil {
		t.Fatal(err)
	}
	// A corner is 10 mm from the centre along each axis: √200 from any
	// rotation axis, so a 0.01 rad rotation moves it by about 0.01·√200.
	for i, want := range []float64{200, 200, 200, 1, 1, 1} {
		if relativeError(scales[i], want) > 0.01 {
			t.Fatalf("scales %v", scales)
		}
	}
}
