package sr

import (
	"bytes"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// PS3.16 TID 2010 content and PS3.3 C.17.2.3 evidence describe different
// levels: content can select frames while procedure evidence lists SOPs.
func TestReferenceKOSRetainsContentScopeAndDeduplicatesEvidence(t *testing.T) {
	const study = "2.25.531"
	const firstSeries = "2.25.532"
	const secondSeries = "2.25.533"
	const ct = "1.2.840.10008.5.1.4.1.1.2"
	const enhancedCT = "1.2.840.10008.5.1.4.1.1.2.1"
	refs := []ImageReference{
		{StudyInstanceUID: study, SeriesInstanceUID: secondSeries, SOPClassUID: enhancedCT, SOPInstanceUID: "2.25.534", Frames: []int{3, 8}},
		{StudyInstanceUID: study, SeriesInstanceUID: firstSeries, SOPClassUID: ct, SOPInstanceUID: "2.25.535"},
		{StudyInstanceUID: study, SeriesInstanceUID: secondSeries, SOPClassUID: enhancedCT, SOPInstanceUID: "2.25.534", Frames: []int{2}},
		{StudyInstanceUID: study, SeriesInstanceUID: firstSeries, SOPClassUID: ct, SOPInstanceUID: "2.25.536"},
	}
	doc := NewKeyObjectSelection("2.25.537", refs, nil)
	doc.ContentDate, doc.ContentTime = "20250307", "130721"
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian} {
		t.Run(syntax.UID, func(t *testing.T) {
			var wire bytes.Buffer
			if err := object.WriteDataSet(&wire, mustDocumentDataset(t, doc), syntax); err != nil {
				t.Fatal(err)
			}
			dataset, err := object.ReadDataSet(&wire, syntax)
			if err != nil {
				t.Fatal(err)
			}
			studies, _ := dataset.GetSequence(tagCurrentRequestedProcedureEvidence)
			if len(studies) != 1 {
				t.Fatalf("procedure evidence has %d studies, want 1", len(studies))
			}
			if uid, _ := studies[0].GetUID(tagStudyInstanceUID); uid != study {
				t.Errorf("evidence study UID = %q, want %q", uid, study)
			}
			series, _ := studies[0].GetSequence(tagReferencedSeriesSequence)
			if len(series) != 2 {
				t.Fatalf("procedure evidence has %d series, want 2", len(series))
			}
			for i, want := range []struct {
				uid, class string
				instances  []string
			}{
				{secondSeries, enhancedCT, []string{"2.25.534"}},
				{firstSeries, ct, []string{"2.25.535", "2.25.536"}},
			} {
				uid, _ := series[i].GetUID(tagSeriesInstanceUID)
				items, _ := series[i].GetSequence(tagRefSOPSequence)
				if uid != want.uid || len(items) != len(want.instances) {
					t.Fatalf("series %d UID/count = %q/%d, want %q/%d", i, uid, len(items), want.uid, len(want.instances))
				}
				for j, item := range items {
					instance, _ := item.GetUID(tagRefSOPInstanceUID)
					class, _ := item.GetUID(tagRefSOPClassUID)
					if instance != want.instances[j] || class != want.class {
						t.Errorf("series %d item %d = %q/%q, want %q/%q", i, j, class, instance, want.class, want.instances[j])
					}
					if _, present := item.Get(tagRefFrameNumber); present {
						t.Errorf("series %d item %d contains a frame number in procedure evidence", i, j)
					}
				}
			}
			round, err := ReadDocument(dataset)
			if err != nil {
				t.Fatal(err)
			}
			if round.Title.CodeValue != "113000" || round.Title.CodingSchemeDesignator != "DCM" {
				t.Errorf("title = %+v, want DCM 113000", round.Title)
			}
			if got := KeyObjectSelectionImages(round); !slices.EqualFunc(got, refs, equalReferenceKOSImage) {
				t.Fatalf("readback selected frames or content order changed: %+v, want %+v", got, refs)
			}
		})
	}
}

func equalReferenceKOSImage(got, want ImageReference) bool {
	return got.StudyInstanceUID == want.StudyInstanceUID && got.SeriesInstanceUID == want.SeriesInstanceUID &&
		got.SOPClassUID == want.SOPClassUID && got.SOPInstanceUID == want.SOPInstanceUID && slices.Equal(got.Frames, want.Frames)
}
