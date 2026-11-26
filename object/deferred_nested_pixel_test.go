package object

import (
	"bytes"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// A nested Icon Image Sequence Pixel Data element must neither make the
// deferred top-level Pixel Data ambiguous nor become an unreadable deferred
// value inside its item.
func TestDeferPixelDataKeepsNestedIconMaterializedAndTopLevelReplayable(t *testing.T) {
	u16 := func(element, v uint16) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, element), VR: core.VRUS}, Value: core.Uint16Value{v}}
	}
	icon := []byte{0x01, 0x02, 0x03, 0x04}
	pixels := []byte{0x10, 0x20, 0x30, 0x40, 0x50, 0x60, 0x70, 0x80}
	dataset := FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.543.937.9"}},
		u16(0x0100, 16),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0088, 0x0200), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
			u16(0x0100, 8),
			core.NewRawElement(core.TagPixelData, core.VROB, icon),
		}}}}},
		core.NewRawElement(core.TagPixelData, core.VROW, pixels),
	}, nil)
	var encoded bytes.Buffer
	if err := WriteFile(&encoded, &File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	file, err := ReadFileWithOptions(bytes.NewReader(encoded.Bytes()), ReadFileOptions{DeferPixelData: true})
	if err != nil {
		t.Fatal(err)
	}
	if element, ok := file.Dataset.Get(core.TagPixelData); !ok || element.Value != nil {
		t.Fatal("top-level Pixel Data was not deferred")
	}
	var top bytes.Buffer
	if _, err := file.Dataset.CopyValueTo(core.TagPixelData, &top); err != nil || !bytes.Equal(top.Bytes(), pixels) {
		t.Fatalf("top-level replay = % x, %v", top.Bytes(), err)
	}
	items, ok := file.Dataset.GetSequence(core.NewTag(0x0088, 0x0200))
	if !ok || len(items) != 1 {
		t.Fatal("icon item missing")
	}
	if raw, ok := items[0].GetRaw(core.TagPixelData); !ok || !bytes.Equal(raw, icon) {
		t.Fatalf("icon Pixel Data = % x ok=%v", raw, ok)
	}
	var rewritten bytes.Buffer
	if err := WriteFile(&rewritten, file); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rewritten.Bytes(), encoded.Bytes()) {
		t.Fatal("deferred rewrite changed the file")
	}
}
