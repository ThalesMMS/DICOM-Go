package display

import (
	"encoding/binary"
	"errors"
	"image/color"
	"testing"
)

func argbFixturePalette() *PaletteColorLUT {
	return &PaletteColorLUT{Red: &LUT{FirstMapped: 1, BitsPerEntry: 8, Entries: []uint16{17}}, Green: &LUT{FirstMapped: 1, BitsPerEntry: 8, Entries: []uint16{34}}, Blue: &LUT{FirstMapped: 1, BitsPerEntry: 8, Entries: []uint16{51}}}
}
func TestARGBRetiredPaletteOverrideIsNotOpacity(t *testing.T) {
	for _, pc := range []int{0, 1} {
		t.Run(string(rune('0'+pc)), func(t *testing.T) {
			pixels := []byte{0, 255, 0, 0, 1, 0, 255, 0}
			if pc == 1 {
				pixels = []byte{0, 1, 255, 0, 0, 255, 0, 0}
			}
			f := ColorFrame{Rows: 1, Columns: 2, Pixels: pixels, Photometric: "ARGB", SamplesPerPixel: 4, PlanarConfiguration: pc, Format: PixelFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7}, Palette: argbFixturePalette()}
			out, err := RenderColor(f)
			if err != nil {
				t.Fatal(err)
			}
			for x, w := range []color.RGBA{{255, 0, 0, 255}, {17, 34, 51, 255}} {
				if got := out.RGBAAt(x, 0); got != w {
					t.Fatalf("pixel %d=%v, want %v", x, got, w)
				}
			}
			out16, err := RenderColor16(f)
			if err != nil {
				t.Fatal(err)
			}
			if got := out16.RGBA64At(1, 0); got != (color.RGBA64{17 * 257, 34 * 257, 51 * 257, 65535}) {
				t.Fatal(got)
			}
		})
	}
}
func TestARGBZeroAlphaDoesNotNeedPalette(t *testing.T) {
	f := ColorFrame{Rows: 1, Columns: 1, Pixels: []byte{0, 127, 64, 32}, Photometric: "ARGB", SamplesPerPixel: 4, Format: PixelFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7}}
	out, err := RenderColor(f)
	if err != nil || out.RGBAAt(0, 0) != (color.RGBA{127, 64, 32, 255}) {
		t.Fatalf("zero palette index is source RGB: %v %v", out, err)
	}
	f.Pixels[0] = 1
	if _, err := RenderColor(f); !errors.Is(err, ErrMissingPalette) {
		t.Fatalf("positive alpha without palettes %v", err)
	}
}
func TestARGB16EndianStoredBitsAndPalettePrecision(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		t.Run(order.String(), func(t *testing.T) {
			pixels := make([]byte, 16)
			for i, v := range []uint16{0, 4095, 0, 0, 1, 0, 4095, 0} {
				order.PutUint16(pixels[i*2:], v)
			}
			f := ColorFrame{Rows: 1, Columns: 2, Pixels: pixels, Photometric: "ARGB", SamplesPerPixel: 4, Format: PixelFormat{BitsAllocated: 16, BitsStored: 12, HighBit: 11, ByteOrder: order}, Palette: &PaletteColorLUT{Red: &LUT{FirstMapped: 1, BitsPerEntry: 16, Entries: []uint16{0x1234}}, Green: &LUT{FirstMapped: 1, BitsPerEntry: 16, Entries: []uint16{0x5678}}, Blue: &LUT{FirstMapped: 1, BitsPerEntry: 16, Entries: []uint16{0x9abc}}}}
			img, err := RenderColor16(f)
			if err != nil {
				t.Fatal(err)
			}
			if img.RGBA64At(0, 0) != (color.RGBA64{65535, 0, 0, 65535}) || img.RGBA64At(1, 0) != (color.RGBA64{0x1234, 0x5678, 0x9abc, 65535}) {
				t.Fatalf("precision %v %v", img.RGBA64At(0, 0), img.RGBA64At(1, 0))
			}
			img8, err := RenderColor(f)
			if err != nil || img8.RGBAAt(1, 0) != (color.RGBA{0x12, 0x56, 0x9a, 255}) {
				t.Fatalf("palette byte depth %v %v", img8, err)
			}
		})
	}
}
func TestARGBRejectsUnsupportedLayout(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*ColorFrame)
		want error
	}{
		{"sample count", func(f *ColorFrame) { f.SamplesPerPixel = 3 }, ErrUnsupportedColorLayout},
		{"signed", func(f *ColorFrame) { f.Format.Signed = true }, ErrUnsupportedColorLayout},
		{"bits", func(f *ColorFrame) { f.Format.BitsAllocated = 32 }, ErrUnsupportedColorLayout},
		{"planar", func(f *ColorFrame) { f.PlanarConfiguration = 2 }, ErrUnsupportedColorLayout},
		{"short", func(f *ColorFrame) { f.Pixels = f.Pixels[:3] }, ErrPixelDataTooShort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := ColorFrame{Rows: 1, Columns: 1, Pixels: []byte{0, 1, 2, 3}, Photometric: "ARGB", SamplesPerPixel: 4, Format: PixelFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7}}
			tc.edit(&f)
			if _, err := RenderColor(f); !errors.Is(err, tc.want) {
				t.Fatal(err)
			}
			if _, err := RenderColor16(f); !errors.Is(err, tc.want) {
				t.Fatal(err)
			}
		})
	}
}
