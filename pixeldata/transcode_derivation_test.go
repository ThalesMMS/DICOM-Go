package pixeldata

import (
	"context"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	tagSOPClassUIDForTest        = core.NewTag(0x0008, 0x0016)
	tagDerivationDescriptionTest = core.NewTag(0x0008, 0x2111)
	tagSourceImageSequenceTest   = core.NewTag(0x0008, 0x2112)
	tagDerivationImageSeqTest    = core.NewTag(0x0008, 0x9124)
	tagDerivationCodeSeqTest     = core.NewTag(0x0008, 0x9215)
	tagPurposeOfReferenceTest    = core.NewTag(0x0040, 0xa170)
	tagSharedGroupsTest          = core.NewTag(0x5200, 0x9229)
)

func codeValues(items []*object.Object) []string {
	var out []string
	for _, item := range items {
		v, _ := item.GetString(core.NewTag(0x0008, 0x0100))
		out = append(out, strings.TrimSpace(v))
	}
	return out
}

func lossyTranscode(t *testing.T, source *object.Object) *object.Object {
	t.Helper()
	registry := NewMemoryEncoderRegistry()
	if err := registry.RegisterEncoder(transfer.JPEGBaseline.UID, lossyTestEncoder{}); err != nil {
		t.Fatal(err)
	}
	got, _, err := TranscodeDataSet(context.Background(), source, transfer.ExplicitVRLittleEndian, transfer.JPEGBaseline, TranscodeOptions{EncoderRegistry: registry, AllowLossy: true})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestLossyTranscodeRecordsDerivation checks the derivation a lossy
// transcode records (PS3.3 C.7.6.1.1.5, as DCMTK and GDCM): Derivation Code
// 113040, a Derivation Description, and a Source Image reference to the
// predecessor with purpose 121320; a second step appends its own.
func TestLossyTranscodeRecordsDerivation(t *testing.T) {
	source := transcodeNativeObject([]byte{1, 2, 3, 4})
	source.Put(core.Element{Header: core.ElementHeader{Tag: tagSOPClassUIDForTest, VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}})
	first := lossyTranscode(t, source)
	firstUID, _ := first.GetString(tagSOPInstanceUID)
	if firstUID == "1.2.3.4" {
		t.Fatal("lossy transcode kept the SOP Instance UID")
	}
	codes, _ := first.GetSequence(tagDerivationCodeSeqTest)
	if got := codeValues(codes); len(got) != 1 || got[0] != "113040" {
		t.Fatalf("Derivation Code Sequence %v", got)
	}
	description, _ := first.GetString(tagDerivationDescriptionTest)
	if !strings.HasPrefix(description, "Lossy compression ISO_10918_1, compression ratio ") {
		t.Fatalf("Derivation Description %q", description)
	}
	sources, _ := first.GetSequence(tagSourceImageSequenceTest)
	if len(sources) != 1 {
		t.Fatalf("Source Image Sequence has %d items", len(sources))
	}
	if uid, _ := sources[0].GetString(core.NewTag(0x0008, 0x1155)); uid != "1.2.3.4" {
		t.Fatalf("predecessor %q", uid)
	}
	if purpose, _ := sources[0].GetSequence(tagPurposeOfReferenceTest); len(purpose) != 1 || codeValues(purpose)[0] != "121320" {
		t.Fatalf("Purpose of Reference %v", codeValues(purpose))
	}

	// A second lossy step appends its own description and predecessor.
	second := first
	if err := applyLossyTranscodeMetadata(second, 400, 20, "ISO_14495_1"); err != nil {
		t.Fatal(err)
	}
	codes, _ = second.GetSequence(tagDerivationCodeSeqTest)
	sources, _ = second.GetSequence(tagSourceImageSequenceTest)
	description, _ = second.GetString(tagDerivationDescriptionTest)
	if len(codes) != 1 || len(sources) != 2 || strings.Count(description, "Lossy compression") != 2 {
		t.Fatalf("second step: %d codes, %d sources, %q", len(codes), len(sources), description)
	}
}

// TestLossyTranscodeDerivationInFunctionalGroups records the step in the
// Derivation Image Functional Group of a functional group object and skips
// the predecessor reference when it has no SOP Class UID.
func TestLossyTranscodeDerivationInFunctionalGroups(t *testing.T) {
	source := transcodeNativeObject([]byte{1, 2, 3, 4})
	source.Put(core.Element{Header: core.ElementHeader{Tag: tagSharedGroupsTest, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{}}}})
	got := lossyTranscode(t, source)
	if got.Has(tagSourceImageSequenceTest) || got.Has(tagDerivationCodeSeqTest) {
		t.Fatal("derivation written at the top level of a functional group object")
	}
	shared, _ := got.GetSequence(tagSharedGroupsTest)
	if len(shared) != 1 {
		t.Fatal("Shared Functional Groups Sequence lost")
	}
	derivations, _ := shared[0].GetSequence(tagDerivationImageSeqTest)
	if len(derivations) != 1 {
		t.Fatalf("Derivation Image Sequence has %d items", len(derivations))
	}
	codes, _ := derivations[0].GetSequence(tagDerivationCodeSeqTest)
	sources, ok := derivations[0].GetSequence(tagSourceImageSequenceTest)
	if got := codeValues(codes); len(got) != 1 || got[0] != "113040" || !ok || len(sources) != 0 {
		t.Fatalf("derivation item: codes %v, sources %d (%v)", got, len(sources), ok)
	}
}
