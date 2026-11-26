package frame

import (
	"encoding/binary"
	"errors"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/transfer"
	"image/color"
	"testing"
)

func TestARGBFramePaletteOptionsNativeAndDecoded(t *testing.T) {
	m := pixeldata.Metadata{Rows: 1, Columns: 2, SamplesPerPixel: 4, BitsAllocated: 8, BitsStored: 8, HighBit: 7, NumberOfFrames: 1, PhotometricInterpretation: "ARGB", PlanarConfigurationPresent: true}
	raw := []byte{0, 255, 0, 0, 1, 0, 255, 0}
	palette := &display.PaletteColorLUT{Red: &display.LUT{FirstMapped: 1, BitsPerEntry: 8, Entries: []uint16{17}}, Green: &display.LUT{FirstMapped: 1, BitsPerEntry: 8, Entries: []uint16{34}}, Blue: &display.LUT{FirstMapped: 1, BitsPerEntry: 8, Entries: []uint16{51}}}
	for _, f := range []*Frame{NewNativeFrame(0, raw, m, WithPalette(palette)), NewEncapsulatedFrame(0, raw, m, WithPalette(palette))} {
		img, err := f.GetImage()
		if err != nil {
			t.Fatal(err)
		}
		for x, w := range []color.RGBA{{255, 0, 0, 255}, {17, 34, 51, 255}} {
			if got := color.RGBAModel.Convert(img.At(x, 0)); got != w {
				t.Fatal(got)
			}
		}
	}
	if _, err := NewNativeFrame(0, raw, m).GetImage(); !errors.Is(err, display.ErrMissingPalette) {
		t.Fatalf("positive alpha without palette %v", err)
	}
}
func TestARGBExtractFramesLoadsPalette(t *testing.T) {
	us := func(tag core.Tag, v ...uint16) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRUS}, Value: core.Uint16Value(v)}
	}
	es := []core.Element{us(core.NewTag(0x0028, 0x0010), 1), us(core.NewTag(0x0028, 0x0011), 2), us(core.NewTag(0x0028, 0x0002), 4), us(core.NewTag(0x0028, 0x0006), 0), us(core.NewTag(0x0028, 0x0100), 8), us(core.NewTag(0x0028, 0x0101), 8), us(core.NewTag(0x0028, 0x0102), 7), us(core.NewTag(0x0028, 0x0103), 0), {Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{"ARGB"}}, core.NewRawElement(core.TagPixelData, core.VROB, []byte{0, 255, 0, 0, 1, 0, 255, 0})}
	for i, v := range []byte{17, 34, 51} {
		desc := make([]byte, 6)
		binary.LittleEndian.PutUint16(desc, 1)
		binary.LittleEndian.PutUint16(desc[2:], 1)
		binary.LittleEndian.PutUint16(desc[4:], 8)
		es = append(es, core.NewRawElement(core.NewTag(0x0028, uint16(0x1101+i)), core.VRUS, desc), core.NewRawElement(core.NewTag(0x0028, uint16(0x1201+i)), core.VROW, []byte{v, 0}))
	}
	src := object.FromElements(es, nil)
	fs, err := ExtractFrames(&object.File{Dataset: src, TransferSyntax: transfer.ExplicitVRLittleEndian})
	if err != nil {
		t.Fatal(err)
	}
	img, err := fs[0].GetImage()
	if err != nil {
		t.Fatal(err)
	}
	if got := color.RGBAModel.Convert(img.At(1, 0)); got != (color.RGBA{17, 34, 51, 255}) {
		t.Fatal(got)
	}
}
