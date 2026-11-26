package encapdoc

import (
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"testing"
)

func TestModelMetadataPhysicalUnitsAndExplicitReferences(t *testing.T) {
	file, err := NewOBJ(ModelOptions{DocumentOptions: documentOptions("2.25.1471001", []byte("v 0 0 0\nv 1 0 0\nv 0 1 0\nf 1 2 3\n")), MeasurementUnits: Code{Value: "cm", Scheme: "UCUM", Meaning: "centimeter"}})
	if err != nil {
		t.Fatal(err)
	}
	file.Dataset.Put(object.SequenceElement(core.NewTag(0x0008, 0x114A), object.FromElements([]core.Element{stringElement(core.NewTag(0x0008, 0x1150), core.VRUI, EncapsulatedMTLStorage), stringElement(core.NewTag(0x0008, 0x1155), core.VRUI, "2.25.1471002"), stringElement(core.NewTag(0x0068, 0x7005), core.VRUR, "./materials/red.mtl")}, nil)))
	metadata, err := ReadModelMetadata(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.MillimetresPerUnit != 10 || metadata.FrameOfReferenceUID == "" || len(metadata.ICCProfile) == 0 || len(metadata.References) != 1 || metadata.References[0].RelativeURI != "materials/red.mtl" {
		t.Fatal(metadata)
	}
	file.Dataset.Remove(tagMeasurementUnitsCodeSeq)
	if _, err := ReadModelMetadata(file.Dataset); err == nil {
		t.Fatal("missing scale guessed")
	}
}
func TestModelMetadataRefusesUnsupportedScaleAndMissingFOR(t *testing.T) {
	file, err := NewSTL(ModelOptions{DocumentOptions: documentOptions("2.25.1471003", binarySTL(1)), MeasurementUnits: Code{Value: "[in_i]", Scheme: "UCUM", Meaning: "inch"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadModelMetadata(file.Dataset); err == nil {
		t.Fatal("non-CID7063 units accepted")
	}
	file.Dataset.Put(object.SequenceElement(tagMeasurementUnitsCodeSeq, object.FromElements([]core.Element{stringElement(tagCodeValue, core.VRSH, "mm"), stringElement(tagCodingSchemeDesignator, core.VRSH, "UCUM"), stringElement(tagCodeMeaning, core.VRLO, "millimeter")}, nil)))
	file.Dataset.Remove(tagFrameOfReferenceUID)
	if _, err := ReadModelMetadata(file.Dataset); err == nil {
		t.Fatal("FOR inferred")
	}
}
func TestModelRelativeURIIsOnlyAnExplicitComparisonKey(t *testing.T) {
	for _, uri := range []string{"file:///a.mtl", "/a.mtl", "//server/a.mtl", "../a.mtl", "a/../b.mtl", "a%2f..%2fb.mtl", "https://server/a.mtl", "c:/a.mtl", "a b.mtl", "a%20b.mtl", "a\\b.mtl", "a.exe", "a.mtl?x=1", "a.mtl#fragment"} {
		if _, err := ModelRelativeURI(uri); err == nil {
			t.Fatal("unsafe URI accepted", uri)
		}
	}
	if got, err := ModelRelativeURI("./materials/a.mtl"); err != nil || got != "materials/a.mtl" {
		t.Fatal(got, err)
	}
}
