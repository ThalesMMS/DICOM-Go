package object

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	oaPatientName = core.NewTag(0x0010, 0x0010)
	oaPatientID   = core.NewTag(0x0010, 0x0020)
	oaInstitution = core.NewTag(0x0008, 0x0080)
	oaReferenced  = core.NewTag(0x0008, 0x1140)
	oaRefSOP      = core.NewTag(0x0008, 0x1155)
)

func oaObject() *Object {
	return FromElements([]core.Element{
		newStringElement(tagSOPClassUID, core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		newStringElement(tagSOPInstanceUID, core.VRUI, "1.2.3.4"),
		newStringElement(oaInstitution, core.VRLO, ""),
		newStringElement(oaPatientName, core.VRPN, "OLD^NAME"),
		newStringElement(oaPatientID, core.VRLO, "ID-1"),
		{Header: core.ElementHeader{Tag: oaReferenced, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
			newStringElement(oaRefSOP, core.VRUI, "1.2.3.9"),
		}}}}},
	}, std.Dictionary)
}

func originalItems(t *testing.T, obj *Object) []*Object {
	t.Helper()
	items, ok := obj.GetSequence(tagOriginalAttributesSequence)
	if !ok {
		t.Fatal("no Original Attributes Sequence")
	}
	return items
}

func TestUpdateWithPolicy(t *testing.T) {
	src := FromElements([]core.Element{
		newStringElement(oaInstitution, core.VRLO, "SITE"),
		newStringElement(oaPatientName, core.VRPN, "NEW^NAME"),
		newStringElement(core.NewTag(0x0010, 0x0030), core.VRDA, "20000101"),
		newStringElement(oaPatientID, core.VRLO, "ID-1"),
	}, std.Dictionary)
	tests := []struct {
		policy  UpdatePolicy
		changed []core.Tag
	}{
		{UpdateOverwrite, []core.Tag{oaInstitution, oaPatientName, core.NewTag(0x0010, 0x0030)}},
		{UpdateSupplement, []core.Tag{core.NewTag(0x0010, 0x0030)}},
		{UpdateMerge, []core.Tag{oaInstitution, core.NewTag(0x0010, 0x0030)}},
		{UpdateReplace, []core.Tag{oaInstitution, oaPatientName}},
	}
	for _, tt := range tests {
		obj := oaObject()
		modified, err := UpdateWithPolicy(obj, src, tt.policy)
		if err != nil {
			t.Fatal(err)
		}
		if got := elementTags(modified); !equalTags(got, tt.changed) {
			t.Fatalf("policy %d modified %v, want %v", tt.policy, got, tt.changed)
		}
		for _, element := range modified {
			if element.Tag() == core.NewTag(0x0010, 0x0030) && !elementEmpty(element) {
				t.Fatal("an added attribute is not recorded empty")
			}
		}
	}
}

func elementTags(elements []core.Element) []core.Tag {
	tags := make([]core.Tag, len(elements))
	for i, element := range elements {
		tags[i] = element.Tag()
	}
	return tags
}

func TestAppendOriginalAttributesRecordsPreviousValues(t *testing.T) {
	obj := oaObject()
	before := oaObject()
	obj.Put(newStringElement(oaPatientName, core.VRPN, "NEW^NAME"))
	obj.Remove(oaPatientID)
	obj.Put(newStringElement(core.NewTag(0x0010, 0x0040), core.VRCS, "O"))
	modified := ModifiedAttributes(before, obj)
	if want := []core.Tag{oaPatientName, oaPatientID, core.NewTag(0x0010, 0x0040)}; !equalTags(elementTags(modified), want) {
		t.Fatalf("ModifiedAttributes = %v, want %v", elementTags(modified), want)
	}
	at := time.Date(2026, 10, 7, 12, 30, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		if err := AppendOriginalAttributes(obj, OriginalAttributesRecord{Modified: modified, Reason: ModificationCorrect, System: "Twin Viewer", Source: "SOURCE AE", Time: at}); err != nil {
			t.Fatal(err)
		}
	}
	items := originalItems(t, obj)
	if len(items) != 2 {
		t.Fatalf("items = %d, want one per record", len(items))
	}
	item := items[0]
	for tag, want := range map[core.Tag]string{
		tagReasonForModification:   "CORRECT",
		tagModifyingSystem:         "Twin Viewer",
		tagSourceOfPreviousValues:  "SOURCE AE",
		tagAttributeModificationDT: "20261007123000.000000+0000",
	} {
		if got, _ := item.GetString(tag); got != want {
			t.Fatalf("%s = %q, want %q", tag, got, want)
		}
	}
	previous, ok := item.GetSequence(tagModifiedAttributesSequence)
	if !ok || len(previous) != 1 {
		t.Fatal("Modified Attributes Sequence must hold one item")
	}
	if name, _ := previous[0].GetString(oaPatientName); name != "OLD^NAME" {
		t.Fatalf("previous Patient Name = %q", name)
	}
	if id, _ := previous[0].GetString(oaPatientID); id != "ID-1" {
		t.Fatalf("previous Patient ID = %q", id)
	}
	if sex, ok := previous[0].Get(core.NewTag(0x0010, 0x0040)); !ok || !elementEmpty(sex) {
		t.Fatal("added Patient Sex is not recorded empty")
	}
	var out bytes.Buffer
	if err := WriteDataSet(&out, obj, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatalf("record does not write: %v", err)
	}

	if err := AppendOriginalAttributes(obj, OriginalAttributesRecord{Modified: modified, Reason: "EDIT", System: "x"}); !errors.Is(err, ErrOriginalAttributes) {
		t.Fatalf("invalid reason error = %v", err)
	}
	if err := AppendOriginalAttributes(obj, OriginalAttributesRecord{Modified: modified, Reason: ModificationCoerce}); !errors.Is(err, ErrOriginalAttributes) {
		t.Fatalf("missing system error = %v", err)
	}
}

func TestCoerceAttributesComposesRules(t *testing.T) {
	obj := oaObject()
	obj.Put(newStringElement(tagSpecificCharacterSet, core.VRCS, "ISO 2022 IR 100"))
	merge := FromElements([]core.Element{newStringElement(oaInstitution, core.VRLO, "SITE")}, std.Dictionary)
	modified, err := CoerceAttributes(obj, OriginalAttributesRecord{Reason: ModificationCoerce, System: "Twin Viewer", Source: "MODALITY"},
		MergeAttributes{Attributes: merge, Policy: UpdateMerge},
		NullifyAttributes{Tags: []core.Tag{oaPatientID, core.NewTag(0x0010, 0x1000)}},
		RemapUIDs{UIDs: map[string]string{"1.2.3.4": "2.25.1", "1.2.3.9": "2.25.9"}},
		TrimISO2022CharacterSet{},
		NullifyAttributes{Tags: []core.Tag{oaInstitution}},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.Tag{oaInstitution, oaPatientID, tagSOPInstanceUID, oaReferenced, tagSpecificCharacterSet}
	if !equalTags(elementTags(modified), want) {
		t.Fatalf("modified = %v, want %v", elementTags(modified), want)
	}
	if got, _ := obj.GetUID(tagSOPInstanceUID); got != "2.25.1" {
		t.Fatalf("SOP Instance UID = %q", got)
	}
	refs, _ := obj.GetSequence(oaReferenced)
	if got, _ := refs[0].GetUID(oaRefSOP); got != "2.25.9" {
		t.Fatalf("nested UID = %q", got)
	}
	if got, _ := obj.GetString(tagSpecificCharacterSet); got != "ISO_IR 100" {
		t.Fatalf("Specific Character Set = %q", got)
	}
	previous, _ := originalItems(t, obj)[0].GetSequence(tagModifiedAttributesSequence)
	if got, _ := previous[0].GetString(tagSpecificCharacterSet); got != "ISO 2022 IR 100" {
		t.Fatalf("recorded character set = %q, want the original", got)
	}
	if institution, _ := previous[0].Get(oaInstitution); !elementEmpty(institution) {
		t.Fatal("first value of Institution Name (empty) was not the one recorded")
	}

	untouched := oaObject()
	if modified, err := CoerceAttributes(untouched, OriginalAttributesRecord{Reason: ModificationCoerce, System: "x"}, RemapUIDs{UIDs: map[string]string{"9.9": "1.1"}}); err != nil || len(modified) != 0 || untouched.Has(tagOriginalAttributesSequence) {
		t.Fatalf("no-op coercion recorded %v (%v)", modified, err)
	}
}
