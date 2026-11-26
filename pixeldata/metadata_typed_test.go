package pixeldata

import (
	"encoding/binary"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestExtractMetadataAcceptsTypedUSWithoutSerialization(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		obj := object.FromElements(pixelMetadataElements(33, 129, 1, 8, 8, 7, 0, nil), nil)
		obj.Put(core.Element{Header: core.ElementHeader{Tag: tagPhotometricInterpretation, VR: core.VRCS}, Value: core.StringValue{"MONOCHROME2"}})
		for _, element := range obj.Elements() {
			if element.VR() == core.VRUS {
				raw, _ := element.RawBytes()
				element.Value = core.Uint16Value{binary.LittleEndian.Uint16(raw)}
				obj.Put(element)
			}
		}
		obj.SetValueByteOrder(order)
		metadata, err := ExtractMetadata(obj)
		if err != nil || metadata.Rows != 33 || metadata.Columns != 129 || metadata.BitsAllocated != 8 {
			t.Fatalf("typed metadata (%v): %+v %v", order, metadata, err)
		}
	}
}

func TestExtractMetadataTrimsPhotometricPadding(t *testing.T) {
	for _, value := range []string{"MONOCHROME2\x00", " MONOCHROME2 ", "MONOCHROME2"} {
		obj := object.FromElements(pixelMetadataElements(2, 2, 1, 8, 8, 7, 0, nil), nil)
		obj.Put(core.Element{Header: core.ElementHeader{Tag: tagPhotometricInterpretation, VR: core.VRCS}, Value: core.StringValue{value}})
		metadata, err := ExtractMetadata(obj)
		if err != nil || metadata.PhotometricInterpretation != "MONOCHROME2" {
			t.Fatalf("%q: %q %v", value, metadata.PhotometricInterpretation, err)
		}
	}
	obj := object.FromElements(pixelMetadataElements(2, 2, 1, 8, 8, 7, 0, nil), nil)
	obj.Put(core.Element{Header: core.ElementHeader{Tag: tagPhotometricInterpretation, VR: core.VRCS}, Value: core.StringValue{"\x00 "}})
	if _, err := ExtractMetadata(obj); err == nil {
		t.Fatal("padding alone accepted as a Photometric Interpretation")
	}
}

func TestPixelMetadataUSRejectsInvalidVRAndMultiplicity(t *testing.T) {
	for _, tc := range []struct {
		vr    core.VR
		value core.Value
	}{{core.VRUS, core.Uint16Value{}}, {core.VRUS, core.Uint16Value{1, 2}}, {core.VRUS, core.RawValue{1}}, {core.VRUS, core.RawValue{1, 0, 2, 0}}, {core.VRUL, core.RawValue{1, 0}}, {core.VRUS, core.StringValue{"1"}}} {
		obj := object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: tagRows, VR: tc.vr}, Value: tc.value}}, nil)
		if _, ok := getUint16(obj, tagRows); ok {
			t.Fatalf("accepted malformed metadata %s %T", tc.vr, tc.value)
		}
	}
}
