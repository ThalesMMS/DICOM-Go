package display

import (
	"encoding/binary"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
)

// lutSequence is a LUT sequence item whose descriptor's first value mapped is
// the 16-bit pattern of first, encoded with descriptorVR.
func lutSequence(tag core.Tag, descriptorVR core.VR, first int, entries ...uint16) core.Element {
	descriptor := make([]byte, 6)
	binary.LittleEndian.PutUint16(descriptor[0:], uint16(len(entries)))
	binary.LittleEndian.PutUint16(descriptor[2:], uint16(first))
	binary.LittleEndian.PutUint16(descriptor[4:], 16)
	return dicomtest.NewSequenceElement(tag, core.DataSet{Elements: []core.Element{
		core.NewRawElement(tagLUTDescriptor, descriptorVR, descriptor),
		core.NewRawElement(tagLUTData, core.VROW, EncodeLUTData(entries, 16)),
	}})
}

func TestLUTFirstValueMappedSign(t *testing.T) {
	us := func(tag core.Tag, value uint16) core.Element {
		return dicomtest.Uint16Element(tag, core.VRUS, nil, value)
	}
	ds := func(tag core.Tag, value string) core.Element {
		return dicomtest.NewStringElement(tag, core.VRDS, value)
	}
	signedCT := []core.Element{us(tagBitsStored, 16), us(tagPixelRepresentation, 1)}
	unsignedCT := []core.Element{us(tagBitsStored, 12), us(tagPixelRepresentation, 0)}
	rescale := []core.Element{ds(tagRescaleIntercept, "-1024"), ds(tagRescaleSlope, "1")}
	with := func(groups ...[]core.Element) []core.Element {
		var out []core.Element
		for _, group := range groups {
			out = append(out, group...)
		}
		return out
	}
	tests := []struct {
		name     string
		elements []core.Element
		read     func(*object.Object) (*LUT, error)
		want     int
	}{
		{name: "modality LUT, signed pixels, US descriptor", elements: with(signedCT, []core.Element{lutSequence(tagModalityLUTSequence, core.VRUS, -1024, 0, 1)}), read: readModalityLUT, want: -1024},
		{name: "modality LUT, signed pixels, SS descriptor", elements: with(signedCT, []core.Element{lutSequence(tagModalityLUTSequence, core.VRSS, -1024, 0, 1)}), read: readModalityLUT, want: -1024},
		{name: "modality LUT, unsigned pixels, US descriptor", elements: with(unsignedCT, []core.Element{lutSequence(tagModalityLUTSequence, core.VRUS, 0xFC00, 0, 1)}), read: readModalityLUT, want: 0xFC00},
		{name: "VOI LUT, signed pixels, US descriptor", elements: with(signedCT, []core.Element{lutSequence(tagVOILUTSequence, core.VRUS, -1024, 0, 1)}), read: readVOILUT, want: -1024},
		{name: "VOI LUT, rescale below zero, US descriptor", elements: with(unsignedCT, rescale, []core.Element{lutSequence(tagVOILUTSequence, core.VRUS, -1024, 0, 1)}), read: readVOILUT, want: -1024},
		{name: "VOI LUT, unsigned pixels, US descriptor", elements: with(unsignedCT, []core.Element{lutSequence(tagVOILUTSequence, core.VRUS, 0xFC00, 0, 1)}), read: readVOILUT, want: 0xFC00},
		{name: "VOI LUT, unsigned pixels, SS descriptor", elements: with(unsignedCT, []core.Element{lutSequence(tagVOILUTSequence, core.VRSS, -1024, 0, 1)}), read: readVOILUT, want: -1024},
		{name: "VOI LUT after a Modality LUT", elements: with(signedCT, []core.Element{lutSequence(tagModalityLUTSequence, core.VRUS, -1024, 0, 1), lutSequence(tagVOILUTSequence, core.VRUS, 0xFC00, 0, 1)}), read: readVOILUT, want: 0xFC00},
		{name: "palette, signed pixels, US descriptor", elements: with(signedCT, paletteElements(0xFC00)), read: readRedPalette, want: -1024},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lut, err := tc.read(object.FromElements(tc.elements, nil))
			if err != nil {
				t.Fatal(err)
			}
			if lut.FirstMapped != tc.want {
				t.Fatalf("FirstMapped = %d, want %d", lut.FirstMapped, tc.want)
			}
		})
	}
}

func readModalityLUT(obj *object.Object) (*LUT, error) {
	out, err := ModalityLUTFromObject(obj)
	return out.LUT, err
}

func readVOILUT(obj *object.Object) (*LUT, error) {
	out, err := VOIFromObject(obj)
	return out.LUT, err
}

func readRedPalette(obj *object.Object) (*LUT, error) {
	palette, err := PaletteFromObject(obj)
	if err != nil {
		return nil, err
	}
	return palette.Red, nil
}

func paletteElements(first uint16) []core.Element {
	var out []core.Element
	for _, tags := range [][2]core.Tag{{tagRedPaletteDesc, tagRedPaletteData}, {tagGreenPaletteDesc, tagGreenPaletteData}, {tagBluePaletteDesc, tagBluePaletteData}} {
		out = append(out,
			dicomtest.Uint16Element(tags[0], core.VRUS, nil, 2, first, 16),
			core.NewRawElement(tags[1], core.VROW, EncodeLUTData([]uint16{0, 0xFFFF}, 16)))
	}
	return out
}

func TestVOIAtUsesTheFrameRescaleForTheLUTSign(t *testing.T) {
	// Unsigned stored pixels whose per-frame rescale reaches below zero: the
	// Frame VOI LUT's US first value mapped is signed.
	perFrame := core.DataSet{Elements: []core.Element{
		dicomtest.NewSequenceElement(tagPixelValueTransformationSequence, core.DataSet{Elements: []core.Element{
			dicomtest.NewStringElement(tagRescaleIntercept, core.VRDS, "-1024"),
			dicomtest.NewStringElement(tagRescaleSlope, core.VRDS, "1"),
		}}),
		dicomtest.NewSequenceElement(tagFrameVOILUTSequence, core.DataSet{Elements: []core.Element{
			lutSequence(tagVOILUTSequence, core.VRUS, -1024, 0, 1),
		}}),
	}}
	obj := object.FromElements([]core.Element{
		dicomtest.Uint16Element(tagBitsStored, core.VRUS, nil, 12),
		dicomtest.Uint16Element(tagPixelRepresentation, core.VRUS, nil, 0),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0008), core.VRIS, "1"),
		dicomtest.NewSequenceElement(core.NewTag(0x5200, 0x9230), perFrame),
	}, nil)
	for name, read := range map[string]func() (*LUT, error){
		"VOIAt": func() (*LUT, error) { out, err := VOIAt(obj, 0); return out.LUT, err },
		"VOIsAt": func() (*LUT, error) {
			out, err := VOIsAt(obj, 0)
			if len(out) == 0 {
				return nil, err
			}
			return out[0].LUT, err
		},
	} {
		lut, err := read()
		if err != nil || lut == nil || lut.FirstMapped != -1024 {
			t.Fatalf("%s: LUT %+v, %v; want FirstMapped -1024", name, lut, err)
		}
	}
}

func TestConstantVOILUTRendersItsLevel(t *testing.T) {
	lut, err := NewLUT([]int{3, 0, 12}, []uint16{2048, 2048, 2048})
	if err != nil {
		t.Fatal(err)
	}
	img, err := RenderGray(Frame{
		Rows: 1, Columns: 3, Pixels: []byte{0, 1, 2},
		Format: PixelFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7},
		VOI:    VOILUT{LUT: lut},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, value := range img.Pix {
		if value != 128 {
			t.Fatalf("pixel %d = %d, want 128 (a constant LUT at mid level)", i, value)
		}
	}
}
