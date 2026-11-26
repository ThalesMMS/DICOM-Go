package display

import (
	"encoding/binary"
	"math"
	"slices"
	"testing"
)

func TestDefaultVOIUsesFullModalityRange(t *testing.T) {
	unsigned := PixelFormat{BitsAllocated: 8, BitsStored: 2, HighBit: 1}
	signed := PixelFormat{BitsAllocated: 8, BitsStored: 2, HighBit: 5, Signed: true}
	signedPixels := []byte{0xe5, 0x77, 0xcf, 0x91} // Stored values -2, -1, 0, 1.
	ramp8 := []byte{0, 85, 170, 255}
	ramp16 := []uint16{0, 21845, 43690, 65535}

	lut, err := NewLUT([]int{4, 0, 8}, []uint16{10, 16, 28, 34})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		format   PixelFormat
		pixels   []byte
		modality ModalityLUT
		want8    []byte
		want16   []uint16
	}{
		{"unsigned full domain", unsigned, []byte{0, 1, 2, 3}, ModalityLUT{}, ramp8, ramp16},
		{"unsigned sparse samples", unsigned, []byte{1, 2}, ModalityLUT{}, ramp8[1:3], ramp16[1:3]},
		{"signed fractional rescale", signed, signedPixels, ModalityLUT{Slope: 0.125, Intercept: 1024}, ramp8, ramp16},
		{"negative slope", signed, signedPixels, ModalityLUT{Slope: -0.125, Intercept: 1024}, []byte{255, 170, 85, 0}, []uint16{65535, 43690, 21845, 0}},
		{"small modality span", unsigned, []byte{0, 1, 2, 3}, ModalityLUT{Slope: math.Ldexp(1, -60)}, ramp8, ramp16},
		{"modality LUT", unsigned, []byte{0, 1, 2, 3}, ModalityLUT{Slope: -4, LUT: lut}, []byte{0, 64, 191, 255}, []uint16{0, 16384, 49151, 65535}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// There is no VOI metadata. The renderer scales the possible
			// modality range of the stored field, even if the frame is sparse.
			frame := Frame{Rows: 1, Columns: len(tc.pixels), Pixels: tc.pixels, Format: tc.format, Modality: tc.modality}
			gray, err := RenderGray(frame)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(gray.Pix, tc.want8) {
				t.Errorf("Gray pixels = %v, want %v", gray.Pix, tc.want8)
			}
			wide, err := RenderGray16(frame)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]uint16, len(tc.pixels))
			for i := range got {
				got[i] = binary.BigEndian.Uint16(wide.Pix[2*i:])
			}
			if !slices.Equal(got, tc.want16) {
				t.Errorf("Gray16 pixels = %v, want %v", got, tc.want16)
			}
		})
	}
}
