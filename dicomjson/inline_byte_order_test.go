package dicomjson

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/object"
)

// TestMarshalLittleEndianInlineBinary checks that Marshal, by default,
// reverses the InlineBinary of Big Endian OW, OF, OD, OL and OV values at the
// VR width, and OW Pixel Data with Bits Allocated 32 at the sample width, and
// that with SourceByteOrderInlineBinary, or for a Little Endian object, the
// value bytes are kept.
func TestMarshalLittleEndianInlineBinary(t *testing.T) {
	little := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	values := []struct {
		tag   core.Tag
		vr    core.VR
		width int
	}{
		{core.NewTag(0x0028, 0x1201), core.VROW, 2},
		{core.NewTag(0x0066, 0x0016), core.VROF, 4},
		{core.NewTag(0x0066, 0x0022), core.VROD, 8},
		{core.NewTag(0x0066, 0x0040), core.VROL, 4},
		{core.NewTag(0x7FE0, 0x0001), core.VROV, 8},
		{core.NewTag(0x0042, 0x0011), core.VROB, 1},
		{core.NewTag(0x0009, 0x1010), core.VRUN, 1},
	}
	icon := core.NewTag(0x0088, 0x0200)
	for _, order := range []binary.ByteOrder{binary.BigEndian, binary.LittleEndian} {
		for _, enabled := range []bool{true, false} {
			encode := func(width int) []byte {
				if order == binary.BigEndian {
					return valueencode.ReverseFixedWidth(little, width)
				}
				return bytes.Clone(little)
			}
			elements := []core.Element{{Header: core.ElementHeader{Tag: icon, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
				core.NewRawElement(core.NewTag(0x0028, 0x0100), core.VRUS, rawUint16ValuesWithOrder(order, 32)),
				core.NewRawElement(core.TagPixelData, core.VROW, encode(4)),
			}}}}}}
			for _, value := range values {
				elements = append(elements, core.NewRawElement(value.tag, value.vr, encode(value.width)))
			}
			obj := object.FromDataSet(core.DataSet{Elements: elements}, std.Dictionary)
			obj.SetValueByteOrder(order)
			encoded, err := Marshal(obj, Options{SourceByteOrderInlineBinary: !enabled})
			if err != nil {
				t.Fatal(err)
			}
			var dataset Dataset
			if err := json.Unmarshal(encoded, &dataset); err != nil {
				t.Fatal(err)
			}
			check := func(element Element, name string, width int) {
				t.Helper()
				got, err := base64.StdEncoding.DecodeString(element.InlineBinary)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				want := little
				if !enabled {
					want = encode(width)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("%v enabled=%t: %s InlineBinary % X, want % X", order, enabled, name, got, want)
				}
			}
			for _, value := range values {
				check(dataset[value.tag.HexString()], value.tag.HexString()+" "+value.vr.String(), value.width)
			}
			items := dataset[icon.HexString()].Value
			if len(items) != 1 {
				t.Fatalf("icon sequence %+v", dataset[icon.HexString()])
			}
			itemJSON, err := json.Marshal(items[0])
			if err != nil {
				t.Fatal(err)
			}
			var item Dataset
			if err := json.Unmarshal(itemJSON, &item); err != nil {
				t.Fatal(err)
			}
			check(item["7FE00010"], "icon Pixel Data", 4)
		}
	}
}
