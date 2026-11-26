package sr

import (
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

const (
	testCTImage     = "1.2.840.10008.5.1.4.1.1.2"
	testStudy       = "1.2.826.0.1.3680043.8.498.20260926.60"
	testImageSeries = "1.2.826.0.1.3680043.8.498.20260926.61"
	testImage       = "1.2.826.0.1.3680043.8.498.20260926.62"
)

// imageDocument is a document of sopClassUID whose only content item
// references image.
func imageDocument(sopClassUID string, image ImageReference) *Document {
	doc := &Document{
		SOPClassUID: sopClassUID, SOPInstanceUID: "1.2.826.0.1.3680043.8.498.20260926.63",
		Title:       CodedEntry{CodeValue: "113000", CodingSchemeDesignator: "DCM", CodeMeaning: "Of Interest"},
		ContentDate: "20260926", ContentTime: "120000",
		Content: []ContentItem{{ValueType: ValueImage, RelationshipType: RelationshipContains, Image: image}},
	}
	if sopClassUID == BasicTextSRStorage {
		// Basic Text SR has no IMAGE directly under the root CONTAINER.
		doc.Content = []ContentItem{{
			ValueType: ValueText, RelationshipType: RelationshipContains, Text: "finding",
			ConceptName: CodedEntry{CodeValue: "121071", CodingSchemeDesignator: "DCM", CodeMeaning: "Finding"},
			Children:    []ContentItem{{ValueType: ValueImage, RelationshipType: RelationshipInferredFrom, Image: image}},
		}}
	}
	return doc
}

// imageReport is a measurement report whose only measurement is made on
// image.
func imageReport(image ImageReference) *MeasurementReport {
	return &MeasurementReport{
		SOPInstanceUID: "1.2.826.0.1.3680043.8.498.20260926.64",
		ContentDate:    "20260926", ContentTime: "120000",
		Groups: []MeasurementGroup{{
			Tracking: TrackingIdentifier{Identifier: "Lesion 1", UID: "1.2.826.0.1.3680043.8.498.20260926.65"},
			Measurements: []ReportMeasurement{{
				ConceptName: CodedEntry{CodeValue: "81827009", CodingSchemeDesignator: "SCT", CodeMeaning: "Diameter"},
				Value:       12.5, Units: MillimeterUnit(), Image: image,
			}},
		}},
	}
}

// sourceImage is the image a document is derived from (Common.Source).
func sourceImage() *object.Object {
	return derivedio.Object(
		derivedio.UI(tagSOPClassUID, testCTImage),
		derivedio.UI(tagSOPInstanceUID, testImage),
		derivedio.UI(tagStudyInstanceUID, testStudy),
		derivedio.UI(tagSeriesInstanceUID, testImageSeries),
	)
}

// writeSR writes a document or a measurement report.
type writeSR func(image ImageReference, seriesUID string, source *object.Object) (*object.Object, error)

func writers() map[string]writeSR {
	document := func(sopClassUID string) writeSR {
		return func(image ImageReference, seriesUID string, source *object.Object) (*object.Object, error) {
			doc := imageDocument(sopClassUID, image)
			doc.SeriesInstanceUID = seriesUID
			doc.Common = object.DerivedCommon{Source: source}
			return doc.Dataset()
		}
	}
	return map[string]writeSR{
		"key object selection": document(KeyObjectSelectionDocumentStorage),
		"basic text SR":        document(BasicTextSRStorage),
		"comprehensive SR":     document(ComprehensiveSRStorage),
		"measurement report": func(image ImageReference, seriesUID string, source *object.Object) (*object.Object, error) {
			report := imageReport(image)
			report.SeriesInstanceUID = seriesUID
			report.Common = object.DerivedCommon{Source: source}
			file, err := WriteMeasurementReport(report)
			if err != nil {
				return nil, err
			}
			return file.Dataset, nil
		},
	}
}

func completeImage() ImageReference {
	return ImageReference{StudyInstanceUID: testStudy, SeriesInstanceUID: testImageSeries, SOPClassUID: testCTImage, SOPInstanceUID: testImage}
}

// TestSRWritersWriteTheSeriesAndProcedureAttributes checks the Series
// Instance UID (Type 1, derived when unset), the Referenced Performed
// Procedure Step Sequence (Type 2 in the SR Document and Key Object Document
// Series Modules) and the Performed Procedure Code Sequence (Type 2 in the
// SR Document General Module, which the Key Object Selection IOD lacks).
func TestSRWritersWriteTheSeriesAndProcedureAttributes(t *testing.T) {
	for name, write := range writers() {
		t.Run(name, func(t *testing.T) {
			derived, err := write(completeImage(), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			again, err := write(completeImage(), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			given, err := write(completeImage(), "1.2.826.0.1.3680043.8.498.20260926.66", nil)
			if err != nil {
				t.Fatal(err)
			}
			series, _ := derived.GetUID(tagSeriesInstanceUID)
			if !core.IsValidUID(series) {
				t.Fatalf("derived Series Instance UID %q", series)
			}
			if second, _ := again.GetUID(tagSeriesInstanceUID); second != series {
				t.Fatalf("Series Instance UID %q, then %q", series, second)
			}
			if got, _ := given.GetUID(tagSeriesInstanceUID); got != "1.2.826.0.1.3680043.8.498.20260926.66" {
				t.Fatalf("given Series Instance UID written as %q", got)
			}
			if items, ok := derived.GetSequence(tagReferencedPerformedProcedureStep); !ok || len(items) != 0 {
				t.Fatalf("Referenced Performed Procedure Step Sequence present=%v items=%d, want empty", ok, len(items))
			}
			sopClass, _ := derived.GetUID(tagSOPClassUID)
			items, ok := derived.GetSequence(tagPerformedProcedureCodeSequence)
			if want := sopClass != KeyObjectSelectionDocumentStorage; ok != want || len(items) != 0 {
				t.Fatalf("Performed Procedure Code Sequence present=%v items=%d, want present=%v and empty", ok, len(items), want)
			}
		})
	}
}

// TestSRWritersListEveryImageInTheEvidence checks that each referenced image
// is listed in the Current Requested Procedure Evidence Sequence (PS3.3
// C.17.2.3): with its own study and series, with those of Common.Source when
// it is the source image, or not at all (an error).
func TestSRWritersListEveryImageInTheEvidence(t *testing.T) {
	bare := ImageReference{SOPClassUID: testCTImage, SOPInstanceUID: testImage}
	other := ImageReference{SOPClassUID: testCTImage, SOPInstanceUID: "1.2.826.0.1.3680043.8.498.20260926.67"}
	cases := []struct {
		name    string
		image   ImageReference
		source  *object.Object
		wantErr bool
	}{
		{"complete reference", completeImage(), nil, false},
		{"reference to the source image", bare, sourceImage(), false},
		{"reference without study or series", bare, nil, true},
		{"reference to another image than the source", other, sourceImage(), true},
		{"reference without series", ImageReference{StudyInstanceUID: testStudy, SOPClassUID: testCTImage, SOPInstanceUID: testImage}, nil, true},
	}
	for name, write := range writers() {
		for _, tc := range cases {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				dataset, err := write(tc.image, "", tc.source)
				if tc.wantErr {
					if !errors.Is(err, ErrInvalidDocument) && !errors.Is(err, ErrInvalidMeasurementReport) {
						t.Fatalf("err = %v, want an invalid document or report", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				evidence := readCurrentRequestedProcedureEvidence(&decoder{}, dataset)
				if got := evidence[testImage]; got.studyUID != testStudy || got.seriesUID != testImageSeries {
					t.Fatalf("evidence of %s = %+v, want study %s series %s", testImage, got, testStudy, testImageSeries)
				}
			})
		}
	}
}

// TestMeasurementReportWritesReferencedSegmentNumberAsUS checks the VR of
// Referenced Segment Number (0062,000B) in the Referenced SOP Sequence of
// the Image Reference Macro.
func TestMeasurementReportWritesReferencedSegmentNumberAsUS(t *testing.T) {
	file, err := WriteMeasurementReport(withEvidence(dcmtkOracleReport()))
	if err != nil {
		t.Fatal(err)
	}
	var found []core.VR
	var walk func(items []*object.Object)
	walk = func(items []*object.Object) {
		for _, item := range items {
			if refs, ok := item.GetSequence(tagRefSOPSequence); ok {
				for _, ref := range refs {
					if element, ok := ref.Get(tagReferencedSegmentNumber); ok {
						found = append(found, element.VR())
					}
				}
			}
			children, _ := item.GetSequence(tagContentSequence)
			walk(children)
		}
	}
	walk([]*object.Object{file.Dataset})
	if len(found) != 1 || found[0] != core.VRUS {
		t.Fatalf("Referenced Segment Number VRs %v, want one US", found)
	}
}
