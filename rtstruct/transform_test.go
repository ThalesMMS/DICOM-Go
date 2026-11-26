package rtstruct

import (
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// TestTransformMapsContoursThroughASyntheticTransform: a rotation of 30°
// about z with a shift moves every contour point analytically, keeps the
// ROI metadata, drops the SOP identity and image references, and a refused
// point fails the transform.
func TestTransformMapsContoursThroughASyntheticTransform(t *testing.T) {
	set := &StructureSet{
		SOPInstanceUID: "1.2.3", SeriesInstanceUID: "1.2.4", FrameOfReferenceUID: "1.2.5", Label: "PLAN",
		ROIs: []ROI{{Number: 4, Name: "GTV", ColorRGB: [3]int{255, 0, 0}, Contours: []Contour{{
			GeometricType:    ContourClosedPlanar,
			Points:           []Point3D{{X: 10, Y: 0, Z: 5}, {X: 20, Y: 0, Z: 5}, {X: 20, Y: 10, Z: 5}},
			ReferencedImages: []ReferencedImage{{SOPInstanceUID: "1.2.6"}},
		}}}},
	}
	angle := 30 * math.Pi / 180
	rotate := func(p render.Vec3) (render.Vec3, error) {
		return render.Vec3{X: math.Cos(angle)*p.X - math.Sin(angle)*p.Y + 4, Y: math.Sin(angle)*p.X + math.Cos(angle)*p.Y - 7, Z: p.Z + 2}, nil
	}
	out, err := set.Transform("1.2.9", rotate)
	if err != nil {
		t.Fatal(err)
	}
	if out.FrameOfReferenceUID != "1.2.9" || out.SOPInstanceUID != "" || out.Label != "PLAN" || out.ROIs[0].Name != "GTV" || out.ROIs[0].ColorRGB != [3]int{255, 0, 0} {
		t.Fatalf("transformed set %+v", out)
	}
	contour := out.ROIs[0].Contours[0]
	if contour.ReferencedImages != nil || contour.GeometricType != ContourClosedPlanar {
		t.Fatalf("contour %+v", contour)
	}
	for i, p := range set.ROIs[0].Contours[0].Points {
		want, _ := rotate(render.Vec3{X: p.X, Y: p.Y, Z: p.Z})
		got := contour.Points[i]
		if math.Abs(got.X-want.X)+math.Abs(got.Y-want.Y)+math.Abs(got.Z-want.Z) > 1e-12 {
			t.Fatalf("point %d = %+v, want %+v", i, got, want)
		}
	}
	// Distances between points are preserved by the rigid motion.
	a, b := contour.Points[0], contour.Points[1]
	if d := math.Hypot(math.Hypot(b.X-a.X, b.Y-a.Y), b.Z-a.Z); math.Abs(d-10) > 1e-12 {
		t.Fatalf("edge length %g", d)
	}
	if set.ROIs[0].Contours[0].Points[0].X != 10 || set.SOPInstanceUID != "1.2.3" {
		t.Fatal("the source structure set changed")
	}
	refuse := func(render.Vec3) (render.Vec3, error) { return render.Vec3{}, errors.New("outside the grid") }
	if _, err := set.Transform("1.2.9", refuse); !errors.Is(err, ErrTransform) {
		t.Fatalf("refused point: %v", err)
	}
	nonFinite := func(render.Vec3) (render.Vec3, error) { return render.Vec3{X: math.NaN()}, nil }
	if _, err := set.Transform("1.2.9", nonFinite); !errors.Is(err, ErrTransform) {
		t.Fatalf("non-finite point: %v", err)
	}
}
