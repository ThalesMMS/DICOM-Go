package spatialreg

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/validation"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

const (
	writeStudyUID       = "1.2.826.0.1.3680043.10.543.1077.1"
	writeFixedSeriesUID = "1.2.826.0.1.3680043.10.543.1077.2"
	writeFixedFrameUID  = "1.2.826.0.1.3680043.10.543.1077.3"
	writeMovingFrameUID = "1.2.826.0.1.3680043.10.543.1077.4"
)

// fixedImageSource is a synthetic CT image of the registered series.
func fixedImageSource() *object.Object {
	return derivedio.Object(
		derivedio.UI(derivedio.TagSOPClassUID, "1.2.840.10008.5.1.4.1.1.2"),
		derivedio.UI(derivedio.TagSOPInstanceUID, "1.2.826.0.1.3680043.10.543.1077.5"),
		derivedio.Str(core.NewTag(0x0010, 0x0010), core.VRPN, "SYNTHETIC^PHANTOM"),
		derivedio.LO(core.NewTag(0x0010, 0x0020), "PHANTOM-1077"),
		derivedio.Str(core.NewTag(0x0010, 0x0030), core.VRDA, ""),
		derivedio.CS(core.NewTag(0x0010, 0x0040), "O"),
		derivedio.UI(derivedio.TagStudyInstanceUID, writeStudyUID),
		derivedio.Str(core.NewTag(0x0008, 0x0020), core.VRDA, "20260926"),
		derivedio.Str(core.NewTag(0x0008, 0x0030), core.VRTM, "101500"),
		derivedio.Str(core.NewTag(0x0008, 0x0090), core.VRPN, ""),
		derivedio.SH(core.NewTag(0x0020, 0x0010), "1077"),
		derivedio.SH(core.NewTag(0x0008, 0x0050), ""),
		derivedio.CS(derivedio.TagModality, "CT"),
		derivedio.UI(derivedio.TagSeriesInstanceUID, writeFixedSeriesUID),
		derivedio.UI(derivedio.TagFrameOfReferenceUID, writeFixedFrameUID),
		derivedio.LO(core.NewTag(0x0020, 0x1040), ""),
	)
}

func TestWriteSpatialRegistrationRoundTripsAndValidates(t *testing.T) {
	// Source to registered: rotate 10° about z, then shift.
	c, s := 0.984807753012208, 0.17364817766693033
	matrix := Matrix{Type: string(TransformRigid), Values: render.GeometryAffine{
		c, -s, 0, 12.5,
		s, c, 0, -4,
		0, 0, 1, 30,
		0, 0, 0, 1,
	}}
	file, err := Write(WriteOptions{
		Common:             object.DerivedCommon{Source: fixedImageSource(), Now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)},
		SeriesNumber:       901,
		SeriesDescription:  "Landmark registration",
		ContentDescription: "Rigid landmark fit of 5 pairs",
		ContentCreatorName: "SYNTHETIC^READER",
		ContentTime:        time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC),
		Registrations: []MatrixRegistration{{
			SourceFrameOfReferenceUID: writeMovingFrameUID,
			Matrix:                    matrix,
			Comment:                   "Rigid landmark fit of 5 pairs, FRE 0.84 mm",
			ReferencedImages: []ImageReference{{
				SeriesInstanceUID: "1.2.826.0.1.3680043.10.543.1077.6",
				SOPClassUID:       "1.2.840.10008.5.1.4.1.1.4",
				SOPInstanceUID:    "1.2.826.0.1.3680043.10.543.1077.7",
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
	parsedFile, err := object.ReadFile(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ReadAffine(parsedFile)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.RegisteredFrameOfReferenceUID != writeFixedFrameUID || len(parsed.Registrations) != 2 {
		t.Fatalf("registered frame %q with %d registrations", parsed.RegisteredFrameOfReferenceUID, len(parsed.Registrations))
	}
	moving := parsed.Registrations[1]
	if moving.SourceFrameOfReferenceUID != writeMovingFrameUID || moving.Type != TransformRigid || len(moving.ReferencedSOPInstanceUIDs) != 1 {
		t.Fatalf("moving registration %+v", moving)
	}
	point, err := moving.MapSourceToRegistered(render.Vec3{X: 10})
	if err != nil {
		t.Fatal(err)
	}
	if want := (render.Vec3{X: 10*c + 12.5, Y: 10*s - 4, Z: 30}); point.Sub(want).Length() > 1e-9 {
		t.Fatalf("mapped %+v, want %+v", point, want)
	}

	// The PS3.3 module rules (generated from the DocBook) and the value
	// checks stand in for dicom3tools dciodvfy.
	report, err := iod.Validate(parsedFile.Dataset, iod.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.IOD != "Spatial Registration" {
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
	if got := derivedio.CleanString(parsedFile.Dataset, derivedio.TagModality); got != "REG" {
		t.Fatalf("Modality %q", got)
	}
	refs := derivedio.Sequence(parsedFile.Dataset, tagReferencedSeriesSequence)
	if len(refs) != 1 || derivedio.CleanUID(refs[0], derivedio.TagSeriesInstanceUID) != "1.2.826.0.1.3680043.10.543.1077.6" {
		t.Fatalf("Common Instance Reference %v", refs)
	}
}

func TestWriteSpatialRegistrationRejectsInvalidInput(t *testing.T) {
	identity := identityMatrix()
	identity.Type = string(TransformRigid)
	skewed := identityMatrix()
	skewed.Type = string(TransformRigid)
	skewed.Values[1] = 0.5
	base := func() WriteOptions {
		return WriteOptions{
			Common:        object.DerivedCommon{Source: fixedImageSource()},
			Registrations: []MatrixRegistration{{SourceFrameOfReferenceUID: writeMovingFrameUID, Matrix: identity}},
		}
	}
	for name, mutate := range map[string]func(*WriteOptions){
		"no registration": func(o *WriteOptions) { o.Registrations = nil },
		"no frame":        func(o *WriteOptions) { o.Common.Source = nil },
		"same frame":      func(o *WriteOptions) { o.Registrations[0].SourceFrameOfReferenceUID = writeFixedFrameUID },
		"not rigid":       func(o *WriteOptions) { o.Registrations[0].Matrix = skewed },
		"bad label":       func(o *WriteOptions) { o.ContentLabel = "lower case" },
		"bad reference":   func(o *WriteOptions) { o.Registrations[0].ReferencedImages = []ImageReference{{SOPInstanceUID: "1.2"}} },
	} {
		opts := base()
		mutate(&opts)
		if _, err := Write(opts); !errors.Is(err, ErrWrite) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Write(base()); err != nil {
		t.Fatalf("valid options: %v", err)
	}
	opts := base()
	opts.ContentLabel = "LANDMARKS_1"
	file, err := Write(opts)
	if err != nil || !strings.HasPrefix(derivedio.CleanUID(file.Dataset, derivedio.TagSOPInstanceUID), "2.25.") {
		t.Fatalf("generated UID: %v", err)
	}
}
