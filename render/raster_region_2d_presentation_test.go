package render

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"testing"
)

// Opaque output is returned as an RGBA image over the same bytes the NRGBA
// raster produces; any non-opaque pixel keeps the NRGBA result.
func TestRasterViewport2DPresentationAliasesOpaqueOutputAsRGBA(t *testing.T) {
	gray := image.NewGray(image.Rect(0, 0, 64, 48))
	for i := range gray.Pix {
		gray.Pix[i] = byte(i * 7)
	}
	translucent := image.NewNRGBA(gray.Rect)
	for i := 0; i < len(translucent.Pix); i += 4 {
		translucent.Pix[i], translucent.Pix[i+1], translucent.Pix[i+2], translucent.Pix[i+3] = byte(i), byte(i/3), 90, 255
	}
	translucent.Pix[3] = 128
	opaqueColor := image.NewNRGBA(gray.Rect)
	copy(opaqueColor.Pix, translucent.Pix)
	opaqueColor.Pix[3] = 255
	affine := [6]float64{3.25, 0, 0, 3.25, 11, 7}
	rotated := [6]float64{0, 2.5, -2.5, 0, 300, 5}
	covered := [6]float64{8, 0, 0, 8, -25, -17}
	coveredFlipped := [6]float64{-8, 0, 0, -8, 450, 330}
	leftMargin := [6]float64{8, 0, 0, 8, 11, -17}
	topMargin := [6]float64{8, 0, 0, 8, -25, 7}
	for _, test := range []struct {
		name       string
		source     image.Image
		affine     [6]float64
		background color.NRGBA
		sampling   RasterSampling2D
		mask       func(int, int, color.NRGBA) color.NRGBA
		opaque     bool
	}{
		{name: "gray axis", source: gray, affine: affine, background: color.NRGBA{A: 255}, sampling: RasterBilinear2D, opaque: true},
		{name: "gray rotated", source: gray, affine: rotated, background: color.NRGBA{A: 255}, sampling: RasterBilinear2D, opaque: true},
		{name: "gray nearest", source: gray, affine: affine, background: color.NRGBA{R: 9, A: 255}, sampling: RasterNearest2D, opaque: true},
		{name: "opaque color", source: opaqueColor, affine: affine, background: color.NRGBA{A: 255}, sampling: RasterBilinear2D, opaque: true},
		{name: "transparent background", source: gray, affine: affine, background: color.NRGBA{}, sampling: RasterBilinear2D},
		{name: "gray covering transparent background", source: gray, affine: covered, sampling: RasterBilinear2D, opaque: true},
		{name: "flipped gray covering transparent background", source: gray, affine: coveredFlipped, sampling: RasterBilinear2D, opaque: true},
		{name: "nearest gray covering transparent background", source: gray, affine: covered, sampling: RasterNearest2D, opaque: true},
		{name: "transparent horizontal margin", source: gray, affine: leftMargin, sampling: RasterBilinear2D},
		{name: "transparent vertical margin", source: gray, affine: topMargin, sampling: RasterBilinear2D},
		{name: "translucent source", source: translucent, affine: affine, background: color.NRGBA{A: 255}, sampling: RasterBilinear2D},
		{name: "translucent mask", source: gray, affine: affine, background: color.NRGBA{A: 255}, sampling: RasterNearest2D, mask: func(x, y int, c color.NRGBA) color.NRGBA {
			if x == 10 && y == 10 {
				c.A = 1
			}
			return c
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := RasterRegion2D{Image: test.source, Grid: gray.Rect, FullSize: gray.Rect.Size()}
			request := RasterViewport2DRequest{Width: 400, Height: 300, Affine: test.affine, Sampling: test.sampling, Background: test.background, SourceColor: test.mask}
			want, err := RasterViewport2D(context.Background(), source, request)
			if err != nil {
				t.Fatal(err)
			}
			if want.Opaque() != test.opaque {
				t.Fatalf("fixture produced opaque=%t, want %t", want.Opaque(), test.opaque)
			}
			got, err := RasterViewport2DPresentation(context.Background(), source, request)
			if err != nil {
				t.Fatal(err)
			}
			var pix []byte
			switch img := got.(type) {
			case *image.RGBA:
				if !test.opaque {
					t.Fatal("non-opaque output was returned as RGBA")
				}
				if img.Rect != want.Rect || img.Stride != want.Stride {
					t.Fatalf("RGBA layout %v/%d, want %v/%d", img.Rect, img.Stride, want.Rect, want.Stride)
				}
				pix = img.Pix
			case *image.NRGBA:
				if test.opaque {
					t.Fatal("opaque output was not returned as RGBA")
				}
				pix = img.Pix
			default:
				t.Fatalf("unexpected %T", got)
			}
			if !bytes.Equal(pix, want.Pix) {
				t.Fatal("presentation pixels differ from RasterViewport2D")
			}
		})
	}
}
