package sr

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestFromMeasurementsMapsLengthAngleAndAreaToReportItems(t *testing.T) {
	report, err := FromMeasurements(MeasurementExportOptions{
		SOPInstanceUID:      "1.2.826.0.1.3680043.9.7433.99.1",
		StudyInstanceUID:    "1.2.3.9102",
		SeriesInstanceUID:   "1.2.3.9123.9103",
		FrameOfReferenceUID: "1.2.3.9107",
		ContentDate:         "20260622",
		ContentTime:         "091500",
		Measurements: []ViewerMeasurement{
			{
				Tracking:    TrackingIdentifier{UID: "1.2.826.0.1.3680043.9.7433.99.11", Identifier: "Length 1"},
				Kind:        MeasurementKindLength,
				Value:       12.5,
				SourceImage: ImageReference{SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "1.2.3.9117.1"},
				Spatial:     SpatialReference{FrameOfReferenceUID: "1.2.3.9107", GraphicType: GraphicTypePolyline3D, Coordinates: []Point3D{{X: 1, Y: 2, Z: 3}, {X: 4, Y: 5, Z: 6}}},
			},
			{
				Tracking:    TrackingIdentifier{UID: "1.2.826.0.1.3680043.9.7433.99.12", Identifier: "Angle 1"},
				Kind:        MeasurementKindAngle,
				Value:       90,
				SourceImage: ImageReference{SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "1.2.3.9117.1"},
			},
			{
				Tracking:    TrackingIdentifier{UID: "1.2.826.0.1.3680043.9.7433.99.13", Identifier: "Area 1"},
				Kind:        MeasurementKindArea,
				Value:       25,
				SourceImage: ImageReference{SOPClassUID: "1.2.840.10008.5.1.4.1.1.2.1", SOPInstanceUID: "1.2.3.9117.2", Frames: []int{3}},
			},
		},
	})
	if err != nil {
		t.Fatalf("FromMeasurements: %v", err)
	}

	if report.SOPClassUID != Comprehensive3DSRStorage {
		t.Fatalf("SOPClassUID = %q, want Comprehensive3DSRStorage", report.SOPClassUID)
	}
	if report.ContentDate != "20260622" || report.ContentTime != "091500" {
		t.Fatalf("content date/time = %q/%q, want explicit values", report.ContentDate, report.ContentTime)
	}
	if report.StudyInstanceUID != "1.2.3.9102" || report.SeriesInstanceUID != "1.2.3.9123.9103" || report.FrameOfReferenceUID != "1.2.3.9107" {
		t.Fatalf("report identity = %q/%q/%q, want study/series/frame", report.StudyInstanceUID, report.SeriesInstanceUID, report.FrameOfReferenceUID)
	}
	if len(report.Groups) != 3 {
		t.Fatalf("groups = %d, want one group per measurement", len(report.Groups))
	}
	checks := []struct {
		index       int
		concept     string
		unit        string
		imageSOP    string
		frameNumber int
	}{
		{index: 0, concept: "Distance", unit: "mm", imageSOP: "1.2.3.9117.1"},
		{index: 1, concept: "Angle", unit: "deg", imageSOP: "1.2.3.9117.1"},
		{index: 2, concept: "Area", unit: "mm2", imageSOP: "1.2.3.9117.2", frameNumber: 3},
	}
	for _, check := range checks {
		got := report.Groups[check.index].Measurements[0]
		if got.ConceptName.CodeMeaning != check.concept {
			t.Fatalf("measurement %d concept = %+v, want %s", check.index, got.ConceptName, check.concept)
		}
		if got.Units.CodeValue != check.unit {
			t.Fatalf("measurement %d unit = %+v, want %s", check.index, got.Units, check.unit)
		}
		if got.Image.SOPInstanceUID != check.imageSOP {
			t.Fatalf("measurement %d image ref = %+v, want SOP %s", check.index, got.Image, check.imageSOP)
		}
		if check.frameNumber != 0 && (len(got.Image.Frames) != 1 || got.Image.Frames[0] != check.frameNumber) {
			t.Fatalf("measurement %d frame refs = %+v, want %d", check.index, got.Image.Frames, check.frameNumber)
		}
	}
}

func TestFromMeasurementsRejectsMissingSourceImageReference(t *testing.T) {
	_, err := FromMeasurements(MeasurementExportOptions{
		SOPInstanceUID:    "1.2.826.0.1.3680043.9.7433.99.2",
		StudyInstanceUID:  "1.2.3.9102",
		SeriesInstanceUID: "1.2.3.9123.9103",
		Measurements: []ViewerMeasurement{{
			Tracking:    TrackingIdentifier{UID: "1.2.826.0.1.3680043.9.7433.99.21", Identifier: "Length 1"},
			Kind:        MeasurementKindLength,
			Value:       12.5,
			SourceImage: ImageReference{SOPClassUID: "1.2.840.10008.5.1.4.1.1.2"},
		}},
	})
	if !errors.Is(err, ErrMissingReference) {
		t.Fatalf("FromMeasurements error = %v, want ErrMissingReference", err)
	}
}

func TestFromMeasurementsRejectsMissingStudyOrSeriesIdentity(t *testing.T) {
	_, err := FromMeasurements(MeasurementExportOptions{
		SOPInstanceUID: "1.2.826.0.1.3680043.9.7433.99.4",
		Measurements: []ViewerMeasurement{{
			Tracking:    TrackingIdentifier{UID: "1.2.826.0.1.3680043.9.7433.99.41", Identifier: "Length 1"},
			Kind:        MeasurementKindLength,
			Value:       12.5,
			SourceImage: ImageReference{SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "1.2.3.9117.1"},
		}},
	})
	if !errors.Is(err, ErrMissingReference) {
		t.Fatalf("FromMeasurements error = %v, want ErrMissingReference", err)
	}
}

func TestFromMeasurementsRejectsSpatialReferenceWithoutFrameOfReference(t *testing.T) {
	_, err := FromMeasurements(MeasurementExportOptions{
		SOPInstanceUID:    "1.2.826.0.1.3680043.9.7433.99.5",
		StudyInstanceUID:  "1.2.3.9102",
		SeriesInstanceUID: "1.2.3.9123.9103",
		Measurements: []ViewerMeasurement{{
			Tracking:    TrackingIdentifier{UID: "1.2.826.0.1.3680043.9.7433.99.51", Identifier: "Length 1"},
			Kind:        MeasurementKindLength,
			Value:       12.5,
			SourceImage: ImageReference{SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "1.2.3.9117.1"},
			Spatial:     SpatialReference{GraphicType: GraphicTypePoint3D, Coordinates: []Point3D{{X: 1, Y: 2, Z: 3}}},
		}},
	})
	if !errors.Is(err, ErrMissingReference) {
		t.Fatalf("FromMeasurements error = %v, want ErrMissingReference", err)
	}
}

func TestFromMeasurementsProducesDeterministicEncodedDatasets(t *testing.T) {
	opts := MeasurementExportOptions{
		SOPInstanceUID:    "1.2.826.0.1.3680043.9.7433.99.3",
		StudyInstanceUID:  "1.2.3.9102",
		SeriesInstanceUID: "1.2.3.9123.9103",
		ContentDate:       "20260622",
		ContentTime:       "092000",
		Measurements: []ViewerMeasurement{{
			Tracking:    TrackingIdentifier{UID: "1.2.826.0.1.3680043.9.7433.99.31", Identifier: "Length 1"},
			Kind:        MeasurementKindLength,
			Value:       8,
			SourceImage: ImageReference{StudyInstanceUID: "1.2.3.9102", SeriesInstanceUID: "1.2.3.9123.9117", SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "1.2.3.9117.1"},
		}},
	}
	first := encodedMeasurementReport(t, opts)
	second := encodedMeasurementReport(t, opts)
	if !bytes.Equal(first, second) {
		t.Fatal("encoded measurement reports differ for identical input")
	}
}

func encodedMeasurementReport(t *testing.T, opts MeasurementExportOptions) []byte {
	t.Helper()
	report, err := FromMeasurements(opts)
	if err != nil {
		t.Fatalf("FromMeasurements: %v", err)
	}
	file, err := WriteMeasurementReport(report)
	if err != nil {
		t.Fatalf("WriteMeasurementReport: %v", err)
	}
	var out bytes.Buffer
	if err := object.WriteFile(&out, file); err != nil {
		t.Fatalf("object.WriteFile: %v", err)
	}
	return out.Bytes()
}

// Tracking UIDs derived from the report UID are valid UIDs even when the
// report UID cannot be extended, and they stay deterministic.
func TestDerivedTrackingUIDsAreValidAndDeterministic(t *testing.T) {
	long := "1.2.826.0.1.3680043.10.543." + strings.Repeat("9", 64-len("1.2.826.0.1.3680043.10.543."))
	for _, tc := range []struct{ report, want string }{
		{"1.2.826.0.1.3680043.10.543.7", "1.2.826.0.1.3680043.10.543.7.3"},
		{long, ""},
		{"1.2.826.0.1.3680043.10.543.RT", ""},
	} {
		got := derivedTrackingUID(tc.report, 3)
		if !core.IsValidUID(got) || (tc.want != "" && got != tc.want) {
			t.Fatalf("report %q: tracking UID %q", tc.report, got)
		}
		if tc.want == "" && (!strings.HasPrefix(got, "2.25.") || got != derivedTrackingUID(tc.report, 3) || got == derivedTrackingUID(tc.report, 4)) {
			t.Fatalf("report %q: fallback %q is not a stable per-measurement 2.25 UID", tc.report, got)
		}
	}
}

// TestFromMeasurementsWritesBidimensionalAxesInOneGroup covers #1074: the
// long and short axes of a bidimensional measurement are two NUM items of one
// TID 1501 group, coded from CID 7470, that read back unchanged.
func TestFromMeasurementsWritesBidimensionalAxesInOneGroup(t *testing.T) {
	image := ImageReference{StudyInstanceUID: "1.2.3.9102", SeriesInstanceUID: "1.2.3.9103", SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "1.2.3.9117.1"}
	axis := func(kind MeasurementKind, value float64, a, b Point3D) ViewerMeasurement {
		return ViewerMeasurement{
			Kind: kind, Value: value, SourceImage: image,
			Spatial: SpatialReference{FrameOfReferenceUID: "1.2.3.9107", GraphicType: GraphicTypePolyline3D, Coordinates: []Point3D{a, b}},
		}
	}
	long := axis(MeasurementKindLongAxis, 20, Point3D{X: 0, Y: 0, Z: 5}, Point3D{X: 20, Y: 0, Z: 5})
	long.Tracking = TrackingIdentifier{Identifier: "Target lesion 1"}
	long.GroupMeasurements = []ViewerMeasurement{axis(MeasurementKindShortAxis, 12, Point3D{X: 8, Y: -6, Z: 5}, Point3D{X: 8, Y: 6, Z: 5})}
	report, err := FromMeasurements(MeasurementExportOptions{
		SOPInstanceUID: "1.2.826.0.1.3680043.9.7433.1074.1", StudyInstanceUID: "1.2.3.9102",
		SeriesInstanceUID: "1.2.826.0.1.3680043.9.7433.1074.2", FrameOfReferenceUID: "1.2.3.9107",
		ContentDate: "20260927", ContentTime: "101500",
		Measurements: []ViewerMeasurement{long},
	})
	if err != nil {
		t.Fatalf("FromMeasurements: %v", err)
	}
	file, err := WriteMeasurementReport(report)
	if err != nil {
		t.Fatalf("WriteMeasurementReport: %v", err)
	}
	read, err := ReadMeasurementReport(file.Dataset)
	if err != nil {
		t.Fatalf("ReadMeasurementReport: %v", err)
	}
	if len(read.Groups) != 1 || len(read.Groups[0].Measurements) != 2 {
		t.Fatalf("groups = %+v, want one group with two measurements", read.Groups)
	}
	want := []struct {
		concept CodedEntry
		value   float64
	}{{LongAxisConcept(), 20}, {ShortAxisConcept(), 12}}
	for i, measurement := range read.Groups[0].Measurements {
		if measurement.ConceptName.CodeValue != want[i].concept.CodeValue || measurement.ConceptName.CodingSchemeDesignator != "SCT" ||
			measurement.Value != want[i].value || measurement.Units.CodeValue != "mm" || len(measurement.Spatial.Coordinates) != 2 {
			t.Fatalf("measurement %d = %+v, want %s %v mm with two coordinates", i, measurement, want[i].concept.CodeMeaning, want[i].value)
		}
	}
	nested := long
	nested.GroupMeasurements = []ViewerMeasurement{long}
	if _, err := FromMeasurements(MeasurementExportOptions{
		SOPInstanceUID: "1.2.826.0.1.3680043.9.7433.1074.3", StudyInstanceUID: "1.2.3.9102", SeriesInstanceUID: "1.2.3.9103",
		Measurements: []ViewerMeasurement{nested},
	}); err == nil {
		t.Fatal("nested group measurements were accepted")
	}
}

// Real World Value Mapping statistics of an ROI travel as further NUMs of
// the area's group, each with the mapping's quantity and units and its
// Derivation; units are required.
func TestFromMeasurementsWritesRealWorldValuesWithDerivation(t *testing.T) {
	image := ImageReference{StudyInstanceUID: "1.2.3.9102", SeriesInstanceUID: "1.2.3.9103", SOPClassUID: "1.2.840.10008.5.1.4.1.1.4.1", SOPInstanceUID: "1.2.3.9117.1", Frames: []int{2}}
	adc := CodedEntry{CodeValue: "113041", CodingSchemeDesignator: "DCM", CodeMeaning: "Apparent Diffusion Coefficient"}
	units := CodedEntry{CodeValue: "um2/s", CodingSchemeDesignator: "UCUM", CodeMeaning: "um2/s"}
	area := ViewerMeasurement{
		Kind: MeasurementKindArea, Value: 4, SourceImage: image,
		Spatial: SpatialReference{FrameOfReferenceUID: "1.2.3.9107", GraphicType: GraphicTypeMultipoint3D, Coordinates: []Point3D{{X: 0, Y: 0, Z: 1}, {X: 2, Y: 2, Z: 1}}},
	}
	derivations := []CodedEntry{MeanDerivation(), MinimumDerivation(), MaximumDerivation(), StandardDeviationDerivation()}
	for i, derivation := range derivations {
		area.GroupMeasurements = append(area.GroupMeasurements, ViewerMeasurement{
			Kind: MeasurementKindRealWorldValue, ConceptName: adc, Units: units, Derivation: derivation,
			Value: float64(700 + i), SourceImage: image,
		})
	}
	report, err := FromMeasurements(MeasurementExportOptions{
		SOPInstanceUID: "1.2.826.0.1.3680043.9.7433.1260.1", StudyInstanceUID: "1.2.3.9102",
		SeriesInstanceUID: "1.2.826.0.1.3680043.9.7433.1260.2", FrameOfReferenceUID: "1.2.3.9107",
		Measurements: []ViewerMeasurement{area},
	})
	if err != nil {
		t.Fatalf("FromMeasurements: %v", err)
	}
	file, err := WriteMeasurementReport(report)
	if err != nil {
		t.Fatalf("WriteMeasurementReport: %v", err)
	}
	read, err := ReadMeasurementReport(file.Dataset)
	if err != nil {
		t.Fatalf("ReadMeasurementReport: %v", err)
	}
	if len(read.Groups) != 1 || len(read.Groups[0].Measurements) != 5 {
		t.Fatalf("groups = %+v, want one group with area and four values", read.Groups)
	}
	wantMeanings := []string{"Mean", "Minimum", "Maximum", "Standard Deviation"}
	for i, measurement := range read.Groups[0].Measurements[1:] {
		if measurement.ConceptName != adc || measurement.Units != units || measurement.Value != float64(700+i) ||
			measurement.Derivation.CodeMeaning != wantMeanings[i] || measurement.Derivation.CodingSchemeDesignator != "SCT" ||
			len(measurement.Image.Frames) != 1 || measurement.Image.Frames[0] != 2 {
			t.Fatalf("value %d = %+v", i, measurement)
		}
	}
	noUnits := area
	noUnits.GroupMeasurements = []ViewerMeasurement{{Kind: MeasurementKindRealWorldValue, Value: 1, SourceImage: image}}
	if _, err := FromMeasurements(MeasurementExportOptions{
		SOPInstanceUID: "1.2.826.0.1.3680043.9.7433.1260.3", StudyInstanceUID: "1.2.3.9102", SeriesInstanceUID: "1.2.3.9103",
		Measurements: []ViewerMeasurement{noUnits},
	}); !errors.Is(err, ErrInvalidMeasurementReport) {
		t.Fatalf("real world value without units: %v", err)
	}
}

func TestSpatialPointsAndGraphicTypePointCounts(t *testing.T) {
	a, b, c := Point3D{X: 1}, Point3D{Y: 1}, Point3D{Z: 1}
	polygon := SpatialReference{GraphicType: GraphicTypePolygon3D, Coordinates: []Point3D{a, b, c, a}}
	if points, ok := SpatialPoints(polygon); !ok || len(points) != 3 {
		t.Fatalf("polygon points = %v, %v; want the three distinct vertices", points, ok)
	}
	legacy := SpatialReference{GraphicType: GraphicTypePoint3D, Coordinates: []Point3D{a, b}}
	if points, ok := SpatialPoints(legacy); !ok || len(points) != 2 {
		t.Fatalf("legacy multi-point POINT = %v, %v", points, ok)
	}
	if _, ok := SpatialPoints(SpatialReference{GraphicType: "ELLIPSOID", Coordinates: []Point3D{a}}); ok {
		t.Fatal("unsupported graphic type accepted")
	}
	for _, tc := range []struct {
		ref  SpatialReference
		want bool
	}{
		{SpatialReference{GraphicType: GraphicTypePoint3D, Coordinates: []Point3D{a}}, true},
		{legacy, false},
		{SpatialReference{GraphicType: GraphicTypePolyline3D, Coordinates: []Point3D{a, b}}, true},
		{SpatialReference{GraphicType: GraphicTypeMultipoint3D, Coordinates: []Point3D{a, b, c}}, true},
		{polygon, true},
		{SpatialReference{GraphicType: GraphicTypePolygon3D, Coordinates: []Point3D{a, b, c}}, false},
	} {
		tc.ref.FrameOfReferenceUID = "1.2.3"
		if err := validateMeasurementSpatialReference(tc.ref); (err == nil) != tc.want {
			t.Fatalf("%s with %d points: error %v, want valid=%v", tc.ref.GraphicType, len(tc.ref.Coordinates), err, tc.want)
		}
	}
}
