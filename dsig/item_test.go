package dsig

import (
	"context"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/internal/testutil"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	tagContentSequenceTest = core.NewTag(0x0040, 0xA730)
	tagTextValue           = core.NewTag(0x0040, 0xA160)
)

// nestedElements adds a two-level Content Sequence to the synthetic object.
func nestedElements() []core.Element {
	text := func(value string) core.DataSet {
		return core.DataSet{Elements: []core.Element{
			dicomtest.StringElement(core.NewTag(0x0040, 0xA040), core.VRCS, "TEXT"),
			dicomtest.StringElement(tagTextValue, core.VRUT, value),
		}}
	}
	container := core.DataSet{Elements: []core.Element{
		dicomtest.StringElement(core.NewTag(0x0040, 0xA040), core.VRCS, "CONTAINER"),
		sequenceElement(tagContentSequenceTest, text("first finding"), text("second finding")),
	}}
	return append(testElements(false), sequenceElement(tagContentSequenceTest, container))
}

func signatureAt(t *testing.T, obj *object.Object, path string) Result {
	t.Helper()
	for _, result := range Verify(obj) {
		if result.Signature.Path == path {
			return result
		}
	}
	t.Fatalf("no signature at %q", path)
	return Result{}
}

// TestSignSequenceItem covers #1175: a signature placed in a sequence item
// (by keyword or tag path, one or two levels down) signs that item only,
// verifies after a Part 10 round trip and coexists with a top-level one.
func TestSignSequenceItem(t *testing.T) {
	pki := testutil.NewTestPKI(t)
	s := newSigner(t, pki, "item-signer.test", "rsa2048")
	obj := newObject(nestedElements(), transfer.ExplicitVRLittleEndian)
	opts := s.options(MACSHA256)
	opts.Item = "ReferencedStudySequence[0]"
	first, err := Sign(obj, opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.Path != "(0008,1110)[0]" || first.MACIDNumber != 1 || len(first.DataElementsSigned) != 2 {
		t.Fatalf("item signature %+v", first)
	}
	opts.Item = "(0040,A730)[0].ContentSequence[1]"
	deep, err := Sign(obj, opts)
	if err != nil {
		t.Fatal(err)
	}
	if deep.Path != "(0040,A730)[0].(0040,A730)[1]" {
		t.Fatalf("deep path %q", deep.Path)
	}
	opts.Item = ""
	if _, err := Sign(obj, opts); err != nil {
		t.Fatal(err)
	}
	read := roundTrip(t, obj, transfer.ExplicitVRLittleEndian)
	requireValid(t, read, 3)

	// The deep item's text is signed by the deep and top-level signatures,
	// not by the Referenced Study item signature.
	changed := roundTrip(t, obj, transfer.ExplicitVRLittleEndian)
	items, _ := changed.GetSequence(tagContentSequenceTest)
	inner, _ := items[0].GetSequence(tagContentSequenceTest)
	inner[1].Put(dicomtest.StringElement(tagTextValue, core.VRUT, "edited finding"))
	items[0].Put(sequenceElement(tagContentSequenceTest, inner[0].ToDataSet(), inner[1].ToDataSet()))
	changed.Put(sequenceElement(tagContentSequenceTest, items[0].ToDataSet()))
	if !signatureAt(t, changed, "(0008,1110)[0]").Valid() || signatureAt(t, changed, "(0040,A730)[0].(0040,A730)[1]").Valid() || signatureAt(t, changed, "").Valid() {
		t.Fatal("an edit inside the deep item must invalidate the deep and top-level signatures only")
	}
}

func TestSignSequenceItemErrors(t *testing.T) {
	pki := testutil.NewTestPKI(t)
	s := newSigner(t, pki, "item-signer.test", "rsa2048")
	for _, item := range []string{"ReferencedStudySequence[3]", "ReferencedStudySequence[*]", "PatientName", "ReferencedImageSequence[0]", "Bogus[0"} {
		obj := newObject(testElements(false), transfer.ExplicitVRLittleEndian)
		opts := s.options(MACSHA256)
		opts.Item = item
		if _, err := Sign(obj, opts); !errors.Is(err, ErrItemNotFound) {
			t.Fatalf("%q: %v", item, err)
		}
		if len(Signatures(obj.Elements())) != 0 {
			t.Fatalf("%q: object changed", item)
		}
	}
	// Profiles apply to the item: the Creator profile needs SOP and study
	// identifiers the Referenced Study item does not have.
	obj := newObject(testElements(false), transfer.ExplicitVRLittleEndian)
	opts := s.options(MACSHA256)
	opts.Item = "ReferencedStudySequence[0]"
	opts.Profile = ProfileCreatorRSA
	if _, err := Sign(obj, opts); !errors.Is(err, ErrProfileViolation) {
		t.Fatalf("profile: %v", err)
	}
	opts.Profile = ProfileBaseRSA
	if _, err := Sign(obj, opts); err != nil {
		t.Fatalf("base profile in an item: %v", err)
	}
}

// TestRemoveSignatures removes one signature (keeping the MAC Parameters
// items others use), then every signature with its sequences.
func TestRemoveSignatures(t *testing.T) {
	pki := testutil.NewTestPKI(t)
	s := newSigner(t, pki, "remove-signer.test", "p256")
	tsa := testutil.NewTestTSA(t, testutil.TestTSAOptions{})
	obj := newObject(nestedElements(), transfer.ExplicitVRLittleEndian)
	first, err := Sign(obj, s.options(MACSHA256))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := TimestampSignature(context.Background(), obj, first, tsa, TimestampRequestOptions{}); err != nil {
		t.Fatal(err)
	}
	second, err := Sign(obj, s.options(MACSHA512))
	if err != nil {
		t.Fatal(err)
	}
	opts := s.options(MACSHA256)
	opts.Item = "ContentSequence[0]"
	if _, err := Sign(obj, opts); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSignature(obj, first); err != nil {
		t.Fatal(err)
	}
	results := requireValid(t, roundTrip(t, obj, transfer.ExplicitVRLittleEndian), 2)
	for _, result := range results {
		if result.Signature.UID == first.UID {
			t.Fatal("removed signature still present")
		}
	}
	if macs := sequenceItems(obj.Elements(), tagMACParametersSequence); len(macs) != 1 {
		t.Fatalf("%d MAC Parameters items left, want 1", len(macs))
	} else if id, _ := uint16Value(macs[0].Elements, tagMACIDNumber); id != second.MACIDNumber {
		t.Fatalf("kept MAC ID %d, want %d", id, second.MACIDNumber)
	}
	if err := RemoveSignature(obj, first); err == nil {
		t.Fatal("removing a missing signature succeeded")
	}
	n, err := RemoveAllSignatures(obj)
	if err != nil || n != 2 {
		t.Fatalf("RemoveAllSignatures = %d, %v", n, err)
	}
	if obj.Has(tagDigitalSignaturesSequence) || obj.Has(tagMACParametersSequence) || len(Signatures(obj.Elements())) != 0 {
		t.Fatal("signature sequences left behind")
	}
	items, _ := obj.GetSequence(tagContentSequenceTest)
	if items[0].Has(tagDigitalSignaturesSequence) || items[0].Has(tagMACParametersSequence) {
		t.Fatal("item signature sequences left behind")
	}
}
