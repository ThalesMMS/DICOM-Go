package display

import (
	"image"
	"image/color"
	"testing"
)

func TestPresentationSubImagePreservesParentPixels(t *testing.T) {
	lut := &LUT{BitsPerEntry: 8, Entries: []uint16{9, 45, 70, 201}}
	cases := []struct {
		name         string
		presentation Presentation
		wantGray8    [4]uint8
		wantGray16   [4]uint8 // LUT outputs normalized to the 8-bit scale.
	}{
		{
			name:         "lut",
			presentation: Presentation{LUT: lut},
			wantGray8:    [4]uint8{9, 45, 70, 201},
			wantGray16:   [4]uint8{9, 45, 70, 201},
		},
		{
			name:         "invert",
			presentation: Presentation{Invert: true},
			wantGray8:    [4]uint8{255, 254, 253, 252},
			wantGray16:   [4]uint8{255, 170, 85, 0},
		},
		{
			name:         "lut_then_invert",
			presentation: Presentation{LUT: lut, Invert: true},
			wantGray8:    [4]uint8{246, 210, 185, 54},
			wantGray16:   [4]uint8{246, 210, 185, 54},
		},
	}
	parentBounds := image.Rect(-4, 6, 3, 11)
	subBounds := image.Rect(-2, 7, 0, 9)
	const sentinel = 123

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gray8 := image.NewGray(parentBounds)
			gray16 := image.NewGray16(parentBounds)
			for y := parentBounds.Min.Y; y < parentBounds.Max.Y; y++ {
				for x := parentBounds.Min.X; x < parentBounds.Max.X; x++ {
					source8, source16 := uint8(sentinel), uint16(sentinel*257)
					if image.Pt(x, y).In(subBounds) {
						index := (y-subBounds.Min.Y)*subBounds.Dx() + x - subBounds.Min.X
						source8, source16 = uint8(index), uint16(index*21845)
					}
					gray8.SetGray(x, y, color.Gray{Y: source8})
					gray16.SetGray16(x, y, color.Gray16{Y: source16})
				}
			}

			tc.presentation.Apply(gray8.SubImage(subBounds).(*image.Gray))
			tc.presentation.ApplyGray16(gray16.SubImage(subBounds).(*image.Gray16))

			for y := parentBounds.Min.Y; y < parentBounds.Max.Y; y++ {
				for x := parentBounds.Min.X; x < parentBounds.Max.X; x++ {
					want8, want16 := uint8(sentinel), uint16(sentinel*257)
					if image.Pt(x, y).In(subBounds) {
						index := (y-subBounds.Min.Y)*subBounds.Dx() + x - subBounds.Min.X
						want8, want16 = tc.wantGray8[index], uint16(tc.wantGray16[index])*257
					}
					if got := gray8.GrayAt(x, y).Y; got != want8 {
						t.Errorf("Gray parent pixel (%d,%d) = %d, want %d", x, y, got, want8)
					}
					if got := gray16.Gray16At(x, y).Y; got != want16 {
						t.Errorf("Gray16 parent pixel (%d,%d) = %d, want %d", x, y, got, want16)
					}
				}
			}
		})
	}
}
