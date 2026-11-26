package spatialreg

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// PS3.3 C.20.3.1.1 evaluates the displacement at the registered point.
// The pre-matrix sets the base coordinates for adding that displacement,
// and the post-matrix acts on the resulting sum.
func TestReferenceDeformableRegistrationUsesRegisteredGridPoint(t *testing.T) {
	pre := translationMatrix(5, -2, 1)
	post := render.GeometryAffine{
		0, -1, 0, -3,
		1, 0, 0, 5,
		0, 0, 1, 8,
		0, 0, 0, 1,
	}
	vectors := []render.Vec3{
		{}, {X: 2, Z: 1}, {X: 4, Z: 2},
		{Y: 4, Z: 1}, {X: 2, Y: 4, Z: 2}, {X: 4, Y: 4, Z: 3},
	}
	fixture := deformableFixture(binary.LittleEndian, [3]uint32{3, 2, 1}, render.Vec3{X: 2, Y: 3, Z: 4},
		[6]float64{0, 1, 0, -1, 0, 0}, vectors, &pre, &post)
	var wire bytes.Buffer
	if err := object.WriteDataSet(&wire, fixture, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	dataset, err := object.ReadDataSet(&wire, transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ReadDataset(dataset)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Registrations) != 1 {
		t.Fatalf("registration count = %d, want 1", len(parsed.Registrations))
	}
	reg := parsed.Registrations[0]
	for _, tc := range []struct {
		name        string
		point, want render.Vec3
		err         error
	}{
		{name: "grid origin", want: render.Vec3{X: -1, Y: 10, Z: 9}},
		{name: "grid corner", point: render.Vec3{X: -3, Y: 4}, want: render.Vec3{X: -9, Y: 11, Z: 12}},
		{name: "interior", point: render.Vec3{X: -1.5, Y: 1}, want: render.Vec3{X: -4, Y: 9.5, Z: 10}},
		{name: "second row sample", point: render.Vec3{Y: 2}, want: render.Vec3{X: -3, Y: 12, Z: 10}},
		{name: "past the oriented edge", point: render.Vec3{X: -3.01}, err: ErrOutsideDomain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := reg.MapRegisteredToSource(tc.point)
			if !errors.Is(err, tc.err) {
				t.Fatalf("map error = %v, want %v", err, tc.err)
			}
			if tc.err == nil && got != tc.want {
				t.Errorf("mapped point = %v, want %v", got, tc.want)
			}
		})
	}
}
