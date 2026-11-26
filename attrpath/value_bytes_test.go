package attrpath

import (
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestEditBytesOwnershipAndOrder(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		o := object.New(std.Dictionary)
		o.SetValueByteOrder(order)
		s, e := NewEditSession(context.Background(), o, EditSessionOptions{})
		if e != nil {
			t.Fatal(e)
		}
		payload := []byte{0x34, 0x12, 0xcd, 0xab}
		if _, e = s.InsertBytes(MustParse("Rows"), payload, binary.LittleEndian); e != nil {
			t.Fatal(e)
		}
		payload[0] = 0
		values, err := o.GetInts(core.NewTag(0x0028, 0x0010))
		if err != nil || !reflect.DeepEqual(values, []int64{0x1234, 0xabcd}) {
			t.Fatalf("%v %v", values, err)
		}
	}
}
func TestEditBytesInheritedCharsetAndNewItems(t *testing.T) {
	o := object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0005), VR: core.VRCS}, Value: core.StringValue{"ISO_IR 100"}}}, std.Dictionary)
	o.SetValueByteOrder(binary.BigEndian)
	s, e := NewEditSession(context.Background(), o, EditSessionOptions{})
	if e != nil {
		t.Fatal(e)
	}
	path := MustParse("ReferencedStudySequence[0].PatientName")
	if _, e = s.InsertBytes(path, []byte{'A', 0xe9}, binary.LittleEndian); e != nil {
		t.Fatal(e)
	}
	items, _ := o.GetSequence(core.NewTag(0x0008, 0x1110))
	v, e := items[0].LookupString(core.NewTag(0x0010, 0x0010))
	if e != nil || v != "Aé" {
		t.Fatalf("%q %v", v, e)
	}
	if _, e = s.InsertBytes(MustParse("ReferencedStudySequence[0].Rows"), []byte{0x34, 0x12}, binary.LittleEndian); e != nil {
		t.Fatal(e)
	}
	items, _ = o.GetSequence(core.NewTag(0x0008, 0x1110))
	values, _ := items[0].GetInts(core.NewTag(0x0028, 0x0010))
	if !reflect.DeepEqual(values, []int64{0x1234}) {
		t.Fatal(values)
	}
}
func TestEditBytesRefusalsBeforePublication(t *testing.T) {
	for _, tc := range []struct {
		path    string
		payload []byte
	}{{"Rows", []byte{1}}, {"DoseRateSet", []byte{1, 0}}, {"ReferencedStudySequence", []byte{}}, {"(7777,1001)", []byte{1, 0}}} {
		o := object.New(std.Dictionary)
		s, e := NewEditSession(context.Background(), o, EditSessionOptions{})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.InsertBytes(MustParse(tc.path), tc.payload, binary.LittleEndian); e == nil {
			t.Fatalf("accepted %s", tc.path)
		}
	}
	o := object.New(std.Dictionary)
	s, e := NewEditSession(context.Background(), o, EditSessionOptions{Limits: EditLimits{MaxValueBytes: 2}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.InsertBytes(MustParse("Rows"), []byte{1, 0, 2, 0}, binary.LittleEndian); !errors.Is(e, ErrLimit) {
		t.Fatal(e)
	}
	if o.Has(core.NewTag(0x0028, 0x0010)) {
		t.Fatal("over-budget value committed")
	}
	if _, e = s.InsertBytes(MustParse("PixelData"), []byte{1, 0}, binary.LittleEndian); !errors.Is(e, ErrUnknownVR) {
		t.Fatal(e)
	}
}
