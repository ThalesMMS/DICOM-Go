package sr

import (
	"context"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
)

// Exercise the source metadata path to a serialized TID 1600 IMAGE. Code
// identities for the expected descriptors come from DCMTK's tsrcmr.cc and
// the CID 244, 10013 and 10033 tables in dcmsr/libcmr.
func TestDCMTKReferenceImageLibrarySourceTerms(t *testing.T) {
	type termCase struct {
		term string
		code CodedEntry
	}
	fields := []struct {
		name        string
		tag         core.Tag
		concept     string
		unmapped    ImageLibraryExtractionFinding
		knownValues []termCase
	}{
		{"laterality", core.NewTag(0x0020, 0x0062), "111027", ImageLibraryExtractionFinding{1602, "3", "unmapped-defined-term"}, []termCase{
			{"R", CodedEntry{"24028007", "SCT", "Right"}},
			{"L", CodedEntry{"7771000", "SCT", "Left"}},
			{"B", CodedEntry{"51440002", "SCT", "Bilateral"}},
			{"U", CodedEntry{"66459002", "SCT", "Unilateral"}},
		}},
		{"ct-acquisition", core.NewTag(0x0018, 0x9302), "113820", ImageLibraryExtractionFinding{1605, "1", "unmapped-defined-term"}, []termCase{
			{"SEQUENCED", CodedEntry{"113804", "DCM", "Sequenced Acquisition"}},
			{"SPIRAL", CodedEntry{"116152004", "SCT", "Spiral Acquisition"}},
			{"CONSTANT_ANGLE", CodedEntry{"113805", "DCM", "Constant Angle Acquisition"}},
			{"STATIONARY", CodedEntry{"113806", "DCM", "Stationary Acquisition"}},
			{"FREE", CodedEntry{"113807", "DCM", "Free Acquisition"}},
		}},
		{"ct-reconstruction", core.NewTag(0x0018, 0x9315), "113961", ImageLibraryExtractionFinding{1605, "2", "unmapped-defined-term"}, []termCase{
			{"FILTER_BACK_PROJ", CodedEntry{"113962", "DCM", "Filtered Back Projection"}},
			{"ITERATIVE", CodedEntry{"113963", "DCM", "Iterative Reconstruction"}},
		}},
	}
	for _, field := range fields {
		t.Run(field.name, func(t *testing.T) {
			for _, tc := range field.knownValues {
				t.Run(tc.term, func(t *testing.T) {
					source := ilSource("CT", ilStrings(field.tag, core.VRCS, tc.term),
						ilStrings(tagStudyInstanceUID, core.VRUI, "1.2.3.4"), ilStrings(tagSeriesInstanceUID, core.VRUI, "1.2.3.5"))
					got, err := ExtractImageLibraryEntry(context.Background(), source, []int{1}, ImageLibraryOptions{})
					if err != nil {
						t.Fatal(err)
					}
					if descriptor := ilDescriptor(t, got, field.concept); descriptor.Code != tc.code ||
						descriptor.ValueType != ValueCode || descriptor.RelationshipType != RelationshipHasAcqContext {
						t.Fatalf("descriptor = %+v, want acquisition context CODE %+v", descriptor, tc.code)
					}
					roundTrip := ilRoundTrip(t, ilReport(ImageLibraryGroup{Entries: []ImageLibraryEntry{got.Entry}}))
					if len(roundTrip.ImageLibraryGroups) != 1 || len(roundTrip.ImageLibraryGroups[0].Entries) != 1 {
						t.Fatal("round trip lost the IMAGE entry")
					}
					entry := roundTrip.ImageLibraryGroups[0].Entries[0]
					if entry.Image.SOPInstanceUID != "1.2.3.121" || !slices.Equal(entry.Image.Frames, []int{1}) {
						t.Fatalf("round trip changed IMAGE identity: %+v", entry.Image)
					}
					if descriptor := ilDescriptor(t, ImageLibraryExtraction{Entry: entry}, field.concept); descriptor.Code != tc.code {
						t.Fatalf("round-trip code = %+v, want %+v", descriptor.Code, tc.code)
					}
				})
			}
			t.Run("unmapped-term", func(t *testing.T) {
				source := ilSource("CT", ilStrings(field.tag, core.VRCS, "XYZ"))
				got, err := ExtractImageLibraryEntry(context.Background(), source, nil, ImageLibraryOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Contains(got.Findings, field.unmapped) {
					t.Fatalf("missing finding for unmapped source term: %+v", got.Findings)
				}
				for _, descriptor := range got.Entry.Descriptors {
					if descriptor.ConceptName.CodeValue == field.concept {
						t.Fatalf("unmapped term produced a coded descriptor: %+v", descriptor)
					}
				}
			})
		})
	}
}
