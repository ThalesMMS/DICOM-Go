package spatialreg

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/validation"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

// TestWriteDeformableSpatialRegistrationRoundTripsAndValidates writes a grid
// of analytic displacements with a rigid post-deformation matrix, reads it
// back through the same parser as archived objects, maps points as written
// and passes the PS3.3 module rules and value checks that stand in for
// dicom3tools dciodvfy. DEFORMABLE_REG_OUT, when set, receives the file for
// external tools (dcmdump, gdcmdump).
func TestWriteDeformableSpatialRegistrationRoundTripsAndValidates(t *testing.T) {
	origin := render.Vec3{X: -20, Y: -15, Z: 5}
	dimensions := [3]uint32{6, 5, 4}
	spacing := render.Vec3{X: 8, Y: 7.5, Z: 10}
	var vectors []render.Vec3
	for z := 0; z < 4; z++ {
		for y := 0; y < 5; y++ {
			for x := 0; x < 6; x++ {
				vectors = append(vectors, render.Vec3{X: 1.5 * math.Sin(float64(y)/2), Y: -0.75 * float64(x), Z: 0.25 * float64(z)})
			}
		}
	}
	grid, err := NewGrid(origin, render.Vec3{X: 1}, render.Vec3{Y: 1}, spacing, dimensions, vectors)
	if err != nil {
		t.Fatal(err)
	}
	post := Matrix{Type: string(TransformRigid), Values: render.GeometryAffine{
		0, -1, 0, 30,
		1, 0, 0, -12,
		0, 0, 1, 4,
		0, 0, 0, 1,
	}}
	file, err := WriteDeformable(DeformableWriteOptions{
		Common:             object.DerivedCommon{Source: fixedImageSource(), Now: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)},
		SeriesNumber:       902,
		SeriesDescription:  "Deformable registration",
		ContentLabel:       "DEMONS",
		ContentDescription: "Diffeomorphic demons after a rigid registration",
		ContentTime:        time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC),
		Registrations: []DeformableRegistration{{
			SourceFrameOfReferenceUID: writeMovingFrameUID,
			Post:                      post,
			Grid:                      grid,
			Comment:                   "Diffeomorphic demons, 3 levels; reviewed by the user",
			ReferencedImages: []ImageReference{{
				SeriesInstanceUID: "1.2.826.0.1.3680043.10.543.1091.6",
				SOPClassUID:       "1.2.840.10008.5.1.4.1.1.4",
				SOPInstanceUID:    "1.2.826.0.1.3680043.10.543.1091.7",
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatal(err)
	}
	if dir := os.Getenv("DEFORMABLE_REG_OUT"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, "deformable-registration.dcm"), encoded.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	parsedFile, err := object.ReadFile(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Read(parsedFile)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.RegisteredFrameOfReferenceUID != writeFixedFrameUID || len(parsed.Registrations) != 1 {
		t.Fatalf("registered frame %q with %d registrations", parsed.RegisteredFrameOfReferenceUID, len(parsed.Registrations))
	}
	r := parsed.Registrations[0]
	if r.SourceFrameOfReferenceUID != writeMovingFrameUID || r.Grid == nil || r.Grid.Dimensions != dimensions || len(r.ReferencedSOPInstanceUIDs) != 1 {
		t.Fatalf("registration %+v", r)
	}
	for _, index := range [][3]int{{0, 0, 0}, {5, 4, 3}, {2, 3, 1}} {
		p := origin.Add(render.Vec3{X: float64(index[0]) * spacing.X, Y: float64(index[1]) * spacing.Y, Z: float64(index[2]) * spacing.Z})
		got, err := r.MapRegisteredToSource(p)
		if err != nil {
			t.Fatal(err)
		}
		v := vectors[(index[2]*5+index[1])*6+index[0]]
		want := post.Values.TransformPoint(p.Add(v))
		if got.Sub(want).Length() > 1e-5 {
			t.Fatalf("grid point %v maps to %v, want %v", index, got, want)
		}
	}
	report, err := iod.Validate(parsedFile.Dataset, iod.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.IOD != "Deformable Spatial Registration" {
		t.Fatalf("IOD %q", report.IOD)
	}
	for _, finding := range report.Errors() {
		t.Errorf("IOD: %s", finding)
	}
	values, err := validation.ValidateDataSet(context.Background(), parsedFile.Dataset.ToDataSet(), validation.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range values.Report.Findings {
		if finding.Severity == validation.SeverityError {
			t.Errorf("value: %+v", finding)
		}
	}
}

func TestWriteDeformableSpatialRegistrationRejectsInvalidInput(t *testing.T) {
	grid, err := NewGrid(render.Vec3{}, render.Vec3{X: 1}, render.Vec3{Y: 1}, render.Vec3{X: 1, Y: 1, Z: 1}, [3]uint32{1, 1, 1}, []render.Vec3{{}})
	if err != nil {
		t.Fatal(err)
	}
	common := object.DerivedCommon{Source: fixedImageSource()}
	skewed := identityMatrix()
	skewed.Type = string(TransformRigid)
	skewed.Values[1] = 0.5
	for name, registrations := range map[string][]DeformableRegistration{
		"none":           nil,
		"same frame":     {{SourceFrameOfReferenceUID: writeFixedFrameUID, Grid: grid}},
		"no deformation": {{SourceFrameOfReferenceUID: writeMovingFrameUID}},
		"skewed post":    {{SourceFrameOfReferenceUID: writeMovingFrameUID, Grid: grid, Post: skewed}},
	} {
		if _, err := WriteDeformable(DeformableWriteOptions{Common: common, Registrations: registrations}); !errors.Is(err, ErrWrite) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
