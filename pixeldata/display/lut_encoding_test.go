package display

import (
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
)

func wordBytes(order binary.ByteOrder, words ...uint16) []byte {
	out := make([]byte, 2*len(words))
	for i, word := range words {
		order.PutUint16(out[2*i:], word)
	}
	return out
}

func TestDecodeLUTDataEncodings(t *testing.T) {
	le := binary.LittleEndian
	tests := []struct {
		name        string
		raw         []byte
		order       binary.ByteOrder
		count, bits int
		legacy      bool
		want        []uint16
		wantBits    int
		adjustments []LUTAdjustment
		err         error
	}{
		{name: "8 bits packed", raw: []byte{10, 20, 30, 40}, count: 4, bits: 8, want: []uint16{10, 20, 30, 40}, wantBits: 8},
		{name: "8 bits packed odd count padded", raw: []byte{10, 20, 30, 0}, count: 3, bits: 8, want: []uint16{10, 20, 30}, wantBits: 8},
		{name: "8 bits packed odd length", raw: []byte{10, 20, 30}, count: 3, bits: 8, want: []uint16{10, 20, 30}, wantBits: 8},
		{name: "8 bits packed big endian words", raw: wordBytes(binary.BigEndian, 0x140A, 0x001E), order: binary.BigEndian, count: 3, bits: 8, want: []uint16{10, 20, 30}, wantBits: 8},
		{name: "8 bits single entry", raw: wordBytes(le, 0x00FF), count: 1, bits: 8, want: []uint16{255}, wantBits: 8},
		{name: "8 bits one per word", raw: wordBytes(le, 10, 20, 30), count: 3, bits: 8, want: []uint16{10, 20, 30}, wantBits: 8, adjustments: []LUTAdjustment{LUTAdjustmentOneEntryPerWord}},
		{name: "10 bits", raw: wordBytes(le, 0, 512, 1023), count: 3, bits: 10, want: []uint16{0, 512, 1023}, wantBits: 10},
		{name: "12 bits", raw: wordBytes(le, 0, 2048, 4095), count: 3, bits: 12, want: []uint16{0, 2048, 4095}, wantBits: 12},
		{name: "16 bits", raw: wordBytes(le, 0, 0x8000, 0xFFFF), count: 3, bits: 16, want: []uint16{0, 0x8000, 0xFFFF}, wantBits: 16},
		{name: "16 bits holding 8-bit values is kept", raw: wordBytes(le, 0, 128, 255), count: 3, bits: 16, want: []uint16{0, 128, 255}, wantBits: 16},
		{name: "16 bits holding 8-bit values legacy", raw: wordBytes(le, 0, 128, 255), count: 3, bits: 16, legacy: true, want: []uint16{0, 128, 255}, wantBits: 8, adjustments: []LUTAdjustment{LUTAdjustmentEightBitValues}},
		{name: "16 bits with repeated bytes legacy", raw: wordBytes(le, 0, 0x8080, 0xFFFF), count: 3, bits: 16, legacy: true, want: []uint16{0, 0x80, 0xFF}, wantBits: 8, adjustments: []LUTAdjustment{LUTAdjustmentEightBitValues}},
		{name: "8 bits holding 16-bit values", raw: wordBytes(le, 0, 0x1234, 0xFFFF), count: 3, bits: 8, err: ErrLegacyLUTEncoding},
		{name: "8 bits holding 16-bit values legacy", raw: wordBytes(le, 0, 0x1234, 0xFFFF), count: 3, bits: 8, legacy: true, want: []uint16{0, 0x1234, 0xFFFF}, wantBits: 16, adjustments: []LUTAdjustment{LUTAdjustmentSixteenBitValues}},
		{name: "8 bits with repeated bytes", raw: wordBytes(le, 0, 0x8080, 0xFFFF), count: 3, bits: 8, err: ErrLegacyLUTEncoding},
		{name: "8 bits with repeated bytes legacy", raw: wordBytes(le, 0, 0x8080, 0xFFFF), count: 3, bits: 8, legacy: true, want: []uint16{0, 0x80, 0xFF}, wantBits: 8, adjustments: []LUTAdjustment{LUTAdjustmentOneEntryPerWord, LUTAdjustmentMasked}},
		{name: "12 bits with larger entries", raw: wordBytes(le, 0, 5000), count: 2, bits: 12, err: ErrLegacyLUTEncoding},
		{name: "12 bits with larger entries legacy", raw: wordBytes(le, 0, 5000), count: 2, bits: 12, legacy: true, want: []uint16{0, 5000 & 4095}, wantBits: 12, adjustments: []LUTAdjustment{LUTAdjustmentMasked}},
		{name: "bits below 8", raw: wordBytes(le, 0, 300, 1000), count: 3, bits: 7, err: ErrInvalidLUTDescriptor},
		{name: "bits below 8 legacy", raw: wordBytes(le, 0, 300, 1000), count: 3, bits: 7, legacy: true, want: []uint16{0, 300, 1000}, wantBits: 10, adjustments: []LUTAdjustment{LUTAdjustmentBitsInferred}},
		{name: "bits above 16 legacy", raw: wordBytes(le, 0, 100), count: 2, bits: 20, legacy: true, want: []uint16{0, 100}, wantBits: 8, adjustments: []LUTAdjustment{LUTAdjustmentBitsInferred}},
		{name: "short data", raw: wordBytes(le, 0, 1000, 2000), count: 7, bits: 16, err: ErrInvalidLUTData},
		{name: "short data legacy", raw: wordBytes(le, 0, 1000, 2000), count: 7, bits: 16, legacy: true, want: []uint16{0, 1000, 2000}, wantBits: 16, adjustments: []LUTAdjustment{LUTAdjustmentDataLength}},
		{name: "long data legacy", raw: wordBytes(le, 0, 1000, 2000), count: 2, bits: 16, legacy: true, want: []uint16{0, 1000, 2000}, wantBits: 16, adjustments: []LUTAdjustment{LUTAdjustmentDataLength}},
		{name: "packed under 16 bits", raw: []byte{10, 20, 30, 40}, count: 4, bits: 16, err: ErrLegacyLUTEncoding},
		{name: "packed under 16 bits legacy", raw: []byte{10, 20, 30, 40}, count: 4, bits: 16, legacy: true, want: []uint16{10, 20, 30, 40}, wantBits: 8, adjustments: []LUTAdjustment{LUTAdjustmentPacked}},
		{name: "no data", raw: nil, count: 2, bits: 16, err: ErrInvalidLUTData},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entries, bits, adjustments, err := decodeLUTData(tc.raw, tc.order, tc.count, tc.bits, LUTOptions{Legacy: tc.legacy})
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("error = %v, want %v", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if !reflect.DeepEqual(entries, tc.want) || bits != tc.wantBits || !reflect.DeepEqual(adjustments, tc.adjustments) {
				t.Fatalf("got %v, %d bits, %v; want %v, %d bits, %v", entries, bits, adjustments, tc.want, tc.wantBits, tc.adjustments)
			}
		})
	}
}

func TestEncodeLUTDataRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		bits    int
		entries []uint16
	}{
		{8, []uint16{0, 1, 2, 255}},
		{8, []uint16{0, 128, 255}},
		{10, []uint16{0, 512, 1023}},
		{16, []uint16{0, 0x8000, 0xFFFF}},
	} {
		raw := EncodeLUTData(tc.entries, tc.bits)
		entries, bits, adjustments, err := decodeLUTData(raw, binary.LittleEndian, len(tc.entries), tc.bits, LUTOptions{})
		if err != nil || bits != tc.bits || len(adjustments) != 0 || !reflect.DeepEqual(entries, tc.entries) {
			t.Fatalf("%d bits %v: decoded %v, %d bits, %v, %v", tc.bits, tc.entries, entries, bits, adjustments, err)
		}
	}
}

func TestNewLUTBitsPerEntry(t *testing.T) {
	if _, err := NewLUT([]int{2, 0, 12}, []uint16{0, 4095}); err != nil {
		t.Fatalf("12-bit LUT: %v", err)
	}
	if _, err := NewLUT([]int{2, 0, 12}, []uint16{0, 4096}); !errors.Is(err, ErrInvalidLUTData) {
		t.Fatalf("12-bit LUT with 4096: error = %v, want ErrInvalidLUTData", err)
	}
	if _, err := NewLUT([]int{2, 0, 17}, []uint16{0, 1}); !errors.Is(err, ErrInvalidLUTDescriptor) {
		t.Fatalf("17-bit LUT: error = %v, want ErrInvalidLUTDescriptor", err)
	}
}

func voiLUTObject(descriptor []uint16, data core.Element) *object.Object {
	item := core.DataSet{Elements: []core.Element{
		dicomtest.Uint16Element(tagLUTDescriptor, core.VRUS, nil, descriptor...),
		data,
	}}
	return object.FromElements([]core.Element{dicomtest.NewSequenceElement(tagVOILUTSequence, item)}, nil)
}

func TestVOIFromObjectReadsPacked8BitLUT(t *testing.T) {
	obj := voiLUTObject([]uint16{3, 0, 8}, core.NewRawElement(tagLUTData, core.VROW, []byte{0, 128, 255, 0}))
	voi, err := VOIFromObject(obj)
	if err != nil {
		t.Fatalf("VOIFromObject: %v", err)
	}
	if want := []uint16{0, 128, 255}; !reflect.DeepEqual(voi.LUT.Entries, want) || voi.LUT.BitsPerEntry != 8 || len(voi.LUT.Adjustments) != 0 {
		t.Fatalf("LUT = %+v, want %v at 8 bits", voi.LUT, want)
	}
	mapper := NewVOIByteMapper(voi)
	for value, want := range []uint8{0, 128, 255} {
		if got := mapper.Byte(float64(value)); got != want {
			t.Fatalf("Byte(%d) = %d, want %d", value, got, want)
		}
	}
}

func TestLUTReadersTakeLegacyOption(t *testing.T) {
	// LUT Data one word short of its descriptor.
	obj := voiLUTObject([]uint16{4, 0, 16}, dicomtest.Uint16Element(tagLUTData, core.VRUS, nil, 0, 1000, 2000))
	if _, err := VOIFromObject(obj); !errors.Is(err, ErrLegacyLUTEncoding) {
		t.Fatalf("VOIFromObject: error = %v, want ErrLegacyLUTEncoding", err)
	}
	voi, err := VOIFromObject(obj, LUTOptions{Legacy: true})
	if err != nil || voi.LUT == nil || !reflect.DeepEqual(voi.LUT.Adjustments, []LUTAdjustment{LUTAdjustmentDataLength}) {
		t.Fatalf("VOIFromObject legacy = %+v, %v", voi.LUT, err)
	}
	if vois, err := VOIsAt(obj, 0, LUTOptions{Legacy: true}); err != nil || len(vois) != 1 || vois[0].LUT == nil {
		t.Fatalf("VOIsAt legacy = %+v, %v", vois, err)
	}
	if voi, err := VOIAt(obj, 0, LUTOptions{Legacy: true}); err != nil || voi.LUT == nil {
		t.Fatalf("VOIAt legacy = %+v, %v", voi, err)
	}

	modality := object.FromElements([]core.Element{dicomtest.NewSequenceElement(tagModalityLUTSequence, core.DataSet{Elements: []core.Element{
		dicomtest.Uint16Element(tagLUTDescriptor, core.VRUS, nil, 4, 0, 16),
		dicomtest.Uint16Element(tagLUTData, core.VRUS, nil, 0, 1000, 2000),
	}})}, nil)
	if _, err := ModalityLUTAt(modality, 0); !errors.Is(err, ErrLegacyLUTEncoding) {
		t.Fatalf("ModalityLUTAt: error = %v, want ErrLegacyLUTEncoding", err)
	}
	if out, err := ModalityLUTAt(modality, 0, LUTOptions{Legacy: true}); err != nil || out.LUT == nil || len(out.LUT.Entries) != 3 {
		t.Fatalf("ModalityLUTAt legacy = %+v, %v", out.LUT, err)
	}
}

func paletteObject(bits uint16, data func(tag core.Tag) core.Element) *object.Object {
	return object.FromElements([]core.Element{
		dicomtest.Uint16Element(tagRedPaletteDesc, core.VRUS, nil, 3, 0, bits),
		data(tagRedPaletteData),
		dicomtest.Uint16Element(tagGreenPaletteDesc, core.VRUS, nil, 3, 0, bits),
		data(tagGreenPaletteData),
		dicomtest.Uint16Element(tagBluePaletteDesc, core.VRUS, nil, 3, 0, bits),
		data(tagBluePaletteData),
	}, nil)
}

func renderPalettePixels(t *testing.T, palette *PaletteColorLUT) []uint8 {
	t.Helper()
	img, err := RenderColor(ColorFrame{
		Rows: 1, Columns: 3, Pixels: []byte{0, 1, 2},
		Photometric: "PALETTE COLOR", SamplesPerPixel: 1,
		Format:  PixelFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7},
		Palette: palette,
	})
	if err != nil {
		t.Fatalf("RenderColor: %v", err)
	}
	return []uint8{img.RGBAAt(0, 0).R, img.RGBAAt(1, 0).R, img.RGBAAt(2, 0).R}
}

func TestPaletteLUTEncodings(t *testing.T) {
	words := func(values ...uint16) func(core.Tag) core.Element {
		return func(tag core.Tag) core.Element {
			return core.NewRawElement(tag, core.VROW, wordBytes(binary.LittleEndian, values...))
		}
	}
	tests := []struct {
		name        string
		bits        uint16
		data        func(core.Tag) core.Element
		legacy      bool
		want        []uint8
		adjustments []LUTAdjustment
	}{
		{name: "8 bits packed", bits: 8, data: func(tag core.Tag) core.Element { return core.NewRawElement(tag, core.VROW, []byte{0, 128, 255, 0}) }, want: []uint8{0, 128, 255}},
		{name: "8 bits one per word", bits: 8, data: words(0, 128, 255), want: []uint8{0, 128, 255}, adjustments: []LUTAdjustment{LUTAdjustmentOneEntryPerWord}},
		{name: "12 bits", bits: 12, data: words(0, 2048, 4095), want: []uint8{0, 128, 255}},
		{name: "16 bits", bits: 16, data: words(0, 0x8000, 0xFFFF), want: []uint8{0, 128, 255}},
		{name: "16 bits of 8-bit values as declared", bits: 16, data: words(0, 128, 255), want: []uint8{0, 0, 0}},
		{name: "16 bits of 8-bit values legacy", bits: 16, data: words(0, 128, 255), legacy: true, want: []uint8{0, 128, 255}, adjustments: []LUTAdjustment{LUTAdjustmentEightBitValues}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			palette, err := PaletteFromObject(paletteObject(tc.bits, tc.data), LUTOptions{Legacy: tc.legacy})
			if err != nil {
				t.Fatalf("PaletteFromObject: %v", err)
			}
			if !reflect.DeepEqual(palette.Red.Adjustments, tc.adjustments) {
				t.Fatalf("adjustments = %v, want %v", palette.Red.Adjustments, tc.adjustments)
			}
			if got := renderPalettePixels(t, palette); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("red channel = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPaletteRender16ScalesEveryDepth(t *testing.T) {
	red, _ := NewLUT([]int{2, 0, 12}, []uint16{0, 4095})
	green, _ := NewLUT([]int{2, 0, 8}, []uint16{0, 255})
	blue, _ := NewLUT([]int{2, 0, 16}, []uint16{0, 0xFFFF})
	img, err := RenderColor16(ColorFrame{
		Rows: 1, Columns: 2, Pixels: []byte{0, 1},
		Photometric: "PALETTE COLOR", SamplesPerPixel: 1,
		Format:  PixelFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7},
		Palette: &PaletteColorLUT{Red: red, Green: green, Blue: blue},
	})
	if err != nil {
		t.Fatalf("RenderColor16: %v", err)
	}
	if got := img.RGBA64At(1, 0); got != (color.RGBA64{R: 0xFFFF, G: 0xFFFF, B: 0xFFFF, A: 0xFFFF}) {
		t.Fatalf("pixel 1 = %#v, want full white", got)
	}
}

func TestPresentationLUTOfTenBits(t *testing.T) {
	obj := object.FromElements([]core.Element{dicomtest.NewSequenceElement(tagPresentationLUTSequence, core.DataSet{Elements: []core.Element{
		dicomtest.Uint16Element(tagLUTDescriptor, core.VRUS, nil, 256, 0, 10),
		core.NewRawElement(tagLUTData, core.VROW, func() []byte {
			entries := make([]uint16, 256)
			for i := range entries {
				entries[i] = uint16(1023 - i*4)
			}
			return EncodeLUTData(entries, 10)
		}()),
	}})}, nil)
	pres, err := PresentationFromObject(obj)
	if err != nil {
		t.Fatalf("PresentationFromObject: %v", err)
	}
	img := image.NewGray(image.Rect(0, 0, 3, 1))
	copy(img.Pix, []uint8{0, 128, 255})
	pres.Apply(img)
	if want := []uint8{255, 127, 0}; !reflect.DeepEqual(img.Pix, want) {
		t.Fatalf("P-values = %v, want %v", img.Pix, want)
	}

	img16 := image.NewGray16(image.Rect(0, 0, 1, 1))
	pres.ApplyGray16(img16)
	if got := img16.Gray16At(0, 0).Y; got != 0xFFFF {
		t.Fatalf("16-bit P-value of 0 = %#x, want 0xffff", got)
	}
}
