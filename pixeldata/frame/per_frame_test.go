package frame

import (
	"encoding/binary"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func us(tag core.Tag, v uint16) core.Element {
	return core.NewRawElement(tag, core.VRUS, binary.LittleEndian.AppendUint16(nil, v))
}

func text(g, e uint16, vr core.VR, values ...string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: core.NewTag(g, e), VR: vr}, Value: core.StringValue(values)}
}

func sq(g, e uint16, items ...core.DataSet) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: core.NewTag(g, e), VR: core.VRSQ}, Value: core.SequenceValue{Items: items}}
}

func pixelValueTransformation(slope, intercept string) core.DataSet {
	return core.DataSet{Elements: []core.Element{sq(0x0028, 0x9145, core.DataSet{Elements: []core.Element{
		text(0x0028, 0x1052, core.VRDS, intercept), text(0x0028, 0x1053, core.VRDS, slope)}})}}
}

// Frames of an enhanced multi-frame object take their own rescale.
func TestExtractFramesPerFrameRescale(t *testing.T) {
	ds := object.FromElements([]core.Element{
		text(0x0008, 0x0016, core.VRUI, "1.2.840.10008.5.1.4.1.1.2.1"),
		text(0x0028, 0x0004, core.VRCS, "MONOCHROME2"),
		text(0x0028, 0x0008, core.VRIS, "2"),
		us(core.NewTag(0x0028, 0x0002), 1), us(core.NewTag(0x0028, 0x0010), 1), us(core.NewTag(0x0028, 0x0011), 1),
		us(core.NewTag(0x0028, 0x0100), 16), us(core.NewTag(0x0028, 0x0101), 16), us(core.NewTag(0x0028, 0x0102), 15),
		us(core.NewTag(0x0028, 0x0103), 0),
		sq(0x5200, 0x9230, pixelValueTransformation("1", "-1024"), pixelValueTransformation("0.5", "-100")),
		core.NewRawElement(core.TagPixelData, core.VROW, []byte{0x10, 0x00, 0x20, 0x00}),
	}, nil)
	frames, err := ExtractFrames(&object.File{Dataset: ds, TransferSyntax: transfer.ExplicitVRLittleEndian})
	if err != nil {
		t.Fatal(err)
	}
	want := []Rescale{{Slope: 1, Intercept: -1024}, {Slope: 0.5, Intercept: -100}}
	for i, f := range frames {
		if f.NativeData.Rescale != want[i] {
			t.Fatalf("frame %d rescale %+v, want %+v", i, f.NativeData.Rescale, want[i])
		}
	}
	// A caller option still overrides the per-frame default.
	frames, _ = ExtractFrames(&object.File{Dataset: ds, TransferSyntax: transfer.ExplicitVRLittleEndian}, WithRescale(3, 4))
	if frames[1].NativeData.Rescale != (Rescale{Slope: 3, Intercept: 4}) {
		t.Fatalf("override %+v", frames[1].NativeData.Rescale)
	}
}
