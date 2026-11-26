package sr

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const (
	kosRetrieveURLA = "https://pacs.example.org/dicom-web/studies/1.2.3.10/series/1.2.3.11"
	kosRetrieveURLB = "https://pacs.example.org/dicom-web/studies/1.2.3.10/series/1.2.3.21"
)

// evidenceSeriesItems returns the Referenced Series Sequence items of the
// single evidence study of obj.
func evidenceSeriesItems(t *testing.T, obj *object.Object) []core.DataSet {
	t.Helper()
	evidence, ok := obj.Get(tagCurrentRequestedProcedureEvidence)
	if !ok {
		t.Fatal("no Current Requested Procedure Evidence Sequence")
	}
	studies := evidence.Value.(core.SequenceValue).Items
	if len(studies) != 1 {
		t.Fatalf("%d evidence studies", len(studies))
	}
	for _, element := range studies[0].Elements {
		if element.Tag() == tagReferencedSeriesSequence {
			return element.Value.(core.SequenceValue).Items
		}
	}
	t.Fatal("no Referenced Series Sequence")
	return nil
}

func itemString(t *testing.T, item core.DataSet, tag core.Tag) (string, core.VR, bool) {
	t.Helper()
	for _, element := range item.Elements {
		if element.Tag() == tag {
			values := element.Value.(core.StringValue)
			if len(values) != 1 {
				t.Fatalf("%s has %d values", tag, len(values))
			}
			return values[0], element.Header.VR, true
		}
	}
	return "", "", false
}

// withEvidenceRetrieveURL returns obj with url added to every evidence
// series, as another CP-2595 writer would put it.
func withEvidenceRetrieveURL(t *testing.T, obj *object.Object, url string) *object.Object {
	t.Helper()
	out := object.FromElements(obj.Elements(), nil)
	evidence, _ := obj.Get(tagCurrentRequestedProcedureEvidence)
	study := evidence.Value.(core.SequenceValue).Items[0]
	var studyElements []core.Element
	for _, element := range study.Elements {
		if element.Tag() == tagReferencedSeriesSequence {
			var series []core.DataSet
			for _, item := range element.Value.(core.SequenceValue).Items {
				elements := append([]core.Element(nil), item.Elements...)
				series = append(series, core.DataSet{Elements: append(elements, strElem(tagRetrieveURL, core.VRUR, url))})
			}
			element = seqElement(tagReferencedSeriesSequence, series...)
		}
		studyElements = append(studyElements, element)
	}
	out.Put(seqElement(tagCurrentRequestedProcedureEvidence, core.DataSet{Elements: studyElements}))
	return out
}

func roundTripDataSet(t *testing.T, obj *object.Object, syntax transfer.Syntax) *object.Object {
	t.Helper()
	var wire bytes.Buffer
	if err := object.WriteDataSet(&wire, obj, syntax); err != nil {
		t.Fatal(err)
	}
	back, err := object.ReadDataSet(bytes.NewReader(wire.Bytes()), syntax)
	if err != nil {
		t.Fatal(err)
	}
	return back
}

// The Retrieve URL of a referenced series (Hierarchical Series Reference
// Macro, CP-2595) is written in the KOS evidence and read back on every
// reference of that series.
func TestKOSEvidenceRetrieveURLRoundTrip(t *testing.T) {
	refs := []ImageReference{
		{StudyInstanceUID: "1.2.3.10", SeriesInstanceUID: "1.2.3.11", SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "1.2.3.12", RetrieveURL: kosRetrieveURLA},
		{StudyInstanceUID: "1.2.3.10", SeriesInstanceUID: "1.2.3.11", SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "1.2.3.13", RetrieveURL: kosRetrieveURLA},
		{StudyInstanceUID: "1.2.3.10", SeriesInstanceUID: "1.2.3.21", SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", SOPInstanceUID: "1.2.3.22"},
	}
	doc := NewKeyObjectSelection("1.2.3.100", refs, nil)
	doc.Title = KOSTitleManifest
	dataset, err := doc.Dataset()
	if err != nil {
		t.Fatal(err)
	}
	series := evidenceSeriesItems(t, dataset)
	if len(series) != 2 {
		t.Fatalf("%d evidence series", len(series))
	}
	if url, vr, ok := itemString(t, series[0], tagRetrieveURL); !ok || vr != core.VRUR || url != kosRetrieveURLA {
		t.Fatalf("first series Retrieve URL = %q %s %t", url, vr, ok)
	}
	if _, _, ok := itemString(t, series[1], tagRetrieveURL); ok {
		t.Fatal("series without a Retrieve URL got one")
	}

	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian} {
		back, err := ReadDocument(roundTripDataSet(t, dataset, syntax))
		if err != nil {
			t.Fatal(err)
		}
		if back.Title.CodeValue != "113030" || back.Title.CodingSchemeDesignator != "DCM" {
			t.Fatalf("title = %+v", back.Title)
		}
		if got := KeyObjectSelectionImages(back); !reflect.DeepEqual(got, refs) {
			t.Fatalf("%v references = %+v, want %+v", syntax, got, refs)
		}
	}

	conflicting := append([]ImageReference(nil), refs...)
	conflicting[1].RetrieveURL = kosRetrieveURLB
	if _, err := NewKeyObjectSelection("1.2.3.101", conflicting, nil).Dataset(); err == nil {
		t.Fatal("one series with two Retrieve URLs was written")
	}
	invalid := append([]ImageReference(nil), refs...)
	invalid[2].RetrieveURL = "https://pacs.example.org/a series"
	if _, err := NewKeyObjectSelection("1.2.3.102", invalid, nil).Dataset(); err == nil {
		t.Fatal("a Retrieve URL with a space was written")
	}
}

// The strict rejection-note reader accepts evidence series carrying a
// Retrieve URL, written by NewKOSRejection or by another writer, and returns
// it with each reference.
func TestKOSRejectionAcceptsEvidenceRetrieveURL(t *testing.T) {
	o := kosTestOptions()
	o.References[0].RetrieveURL = kosRetrieveURLA
	dataset, err := NewKOSPatientSafetyRejection(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		note, err := ReadKOSRejection(context.Background(), roundTripDataSet(t, dataset, syntax), KOSRejectionLimits{})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(note.References, o.References) {
			t.Fatalf("references = %+v, want %+v", note.References, o.References)
		}
	}

	plain, err := NewKOSPatientSafetyRejection(context.Background(), kosTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	note, err := ReadKOSRejection(context.Background(), withEvidenceRetrieveURL(t, plain, kosRetrieveURLB), KOSRejectionLimits{})
	if err != nil {
		t.Fatalf("rejection note with evidence Retrieve URL refused: %v", err)
	}
	for _, reference := range note.References {
		if reference.RetrieveURL != kosRetrieveURLB {
			t.Fatalf("reference %+v lost the Retrieve URL", reference)
		}
	}
	if _, err := ReadKOSRejection(context.Background(), withEvidenceRetrieveURL(t, plain, "not a url"), KOSRejectionLimits{}); err == nil {
		t.Fatal("Retrieve URL with spaces accepted")
	}

	conflicting := kosTestOptions()
	conflicting.References = append(conflicting.References, conflicting.References[0])
	conflicting.References[3].SOPInstanceUID = "1.2.3.23"
	conflicting.References[3].RetrieveURL = kosRetrieveURLA
	if _, err := NewKOSPatientSafetyRejection(context.Background(), conflicting); err == nil {
		t.Fatal("one series with and without a Retrieve URL was written")
	}
}
