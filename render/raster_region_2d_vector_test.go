package render

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"math"
	"testing"
)

func TestRegionalBilinearProfileMatchesReferenceBytes(t *testing.T) {
	const sizeX, sizeY = 23, 17
	grid := image.Rect(0, 0, sizeX, sizeY)
	gray := image.NewGray(image.Rect(-7, -3, 2+sizeX, 4+sizeY))
	rgba := image.NewNRGBA(gray.Bounds())
	// Nonzero bounds, non-contiguous backing rows, and zero/partial alpha.
	for y := gray.Rect.Min.Y; y < gray.Rect.Max.Y; y++ {
		for x := gray.Rect.Min.X; x < gray.Rect.Max.X; x++ {
			v := uint8(x*43 + y*73 + x*y*11)
			gray.SetGray(x, y, color.Gray{Y: v})
			rgba.SetNRGBA(x, y, color.NRGBA{R: v, G: v ^ 0x91, B: v ^ 0x3f, A: uint8(x*119 + y*71)})
		}
	}
	bounds := image.Rect(-5, -1, -5+sizeX, -1+sizeY)
	for _, source := range []image.Image{gray.SubImage(bounds), rgba.SubImage(bounds)} {
		for _, test := range []struct {
			name   string
			affine [6]float64
		}{
			{"zoom-crop", [6]float64{2.7, 0, 0, 2.3, -3.5, -4.25}},
			{"flip-x", [6]float64{-3, 0, 0, 2, 47, -2.5}},
			{"flip-y", [6]float64{2, 0, 0, -3, -7.5, 47}},
			{"flip-xy", [6]float64{-2, 0, 0, -3, 40, 47}},
			{"rotated", [6]float64{2.3, .2, -.17, 2.2, -1.5, 6.25}},
			{"borders-and-ties", [6]float64{2, 0, 0, 2, -.5, -.5}},
			{"tie-predecessor", [6]float64{2, 0, 0, 2, math.Nextafter(-.5, 0), -.5}},
			{"reduction", [6]float64{.8, 0, 0, .91, 3.5, 2.75}},
		} {
			t.Run(fmt.Sprintf("%T/%s", source, test.name), func(t *testing.T) {
				region := RasterRegion2D{Image: source, Grid: grid, FullSize: grid.Size()}
				for _, alpha := range []uint8{0, 123, 255} {
					req := RasterViewport2DRequest{Width: 47, Height: 37, Affine: test.affine,
						Background: color.NRGBA{R: 8, G: 9, B: 171, A: alpha}, Sampling: RasterBilinear2D}
					got, err := RasterViewport2D(context.Background(), region, req)
					if err != nil {
						t.Fatal(err)
					}
					req.SourceColor = func(_, _ int, sampled color.NRGBA) color.NRGBA { return sampled }
					want, err := RasterViewport2D(context.Background(), region, req)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(got.Pix, want.Pix) {
						for i, v := range got.Pix {
							if v != want.Pix[i] {
								t.Fatalf("byte %d for background alpha %d = %d, want %d", i, alpha, v, want.Pix[i])
							}
						}
					}
				}
			})
		}
	}
}

func TestRegionalBilinearCropAllowsUnusedHighTapBeyondCrop(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 3, 1))
	copy(img.Pix, []byte{7, 250, 255})
	region := RasterRegion2D{Image: img, Grid: image.Rect(2, 0, 5, 1), FullSize: image.Pt(6, 1)}
	req := RasterViewport2DRequest{Width: 5, Height: 1, Affine: [6]float64{-2, 0, 0, 1, 9.5, 0},
		Sampling: RasterBilinear2D}
	// The first output maps to the last decoded source center. Its high
	// tap lies beyond the end of the crop and has exactly zero weight.
	got, err := RasterViewport2D(context.Background(), region, req)
	if err != nil {
		t.Fatal(err)
	}
	for x, v := range []uint8{255, 253, 250, 129, 7} {
		expected := color.NRGBA{R: v, G: v, B: v, A: 255}
		if got.NRGBAAt(x, 0) != expected {
			t.Errorf("output %d = %v, want %v", x, got.NRGBAAt(x, 0), expected)
		}
	}
}

func BenchmarkRegionalBilinearProfile(b *testing.B) {
	const width, height = 257, 181
	gray := image.NewGray(image.Rect(0, 0, width, height))
	nrgba := image.NewNRGBA(gray.Bounds())
	for i := range gray.Pix {
		v := byte(i*41 + i/width*73)
		gray.Pix[i] = v
		copy(nrgba.Pix[i*4:i*4+4], []byte{v, v ^ 0x91, v ^ 0x3f, 80 + v/2})
	}
	for _, profile := range []struct {
		name     string
		img      image.Image
		sampling RasterSampling2D
		affine   [6]float64
	}{
		{"mono-enlarged", gray, RasterBilinear2D, [6]float64{2.7, 0, 0, 2.3, -3.5, -4.25}},
		{"mono-nearest", gray, RasterNearest2D, [6]float64{2.7, 0, 0, 2.3, -3.5, -4.25}},
		{"mono-rotated", gray, RasterBilinear2D, [6]float64{2.3, .2, -.17, 2.2, -1.5, 6.25}},
		{"color-alpha", nrgba, RasterBilinear2D, [6]float64{2.7, 0, 0, 2.3, -3.5, -4.25}},
	} {
		b.Run(profile.name, func(b *testing.B) {
			source := RasterRegion2D{Image: profile.img, Grid: gray.Bounds(), FullSize: gray.Bounds().Size()}
			req := RasterViewport2DRequest{Width: 493, Height: 321, Affine: profile.affine,
				Sampling: profile.sampling, Background: color.NRGBA{R: 3, G: 9, B: 171, A: 255}}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := RasterViewport2DPresentation(context.Background(), source, req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
