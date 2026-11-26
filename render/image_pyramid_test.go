package render

import (
	"image"
	"image/color"
	"math"
	"testing"
)

func stripes(width, height, period int) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if x%period == period-1 {
				img.SetGray(x, y, color.Gray{Y: 255})
			}
		}
	}
	return img
}

// amplitude is the peak-to-peak range of the first row's red channel.
func amplitude(img image.Image) int {
	b := img.Bounds()
	low, high := 255, 0
	for x := b.Min.X; x < b.Max.X; x++ {
		r, _, _, _ := img.At(x, b.Min.Y).RGBA()
		v := int(r >> 8)
		low, high = min(low, v), max(high, v)
	}
	return high - low
}

// decimate is what the viewer did: keep every factor-th pixel (the GPU's
// bilinear sampling of a texture reduced 4× reads at most two neighbours).
func decimate(img image.Image, factor int) *image.Gray {
	b := img.Bounds()
	out := image.NewGray(image.Rect(0, 0, b.Dx()/factor, b.Dy()/factor))
	for y := 0; y < out.Rect.Dy(); y++ {
		for x := 0; x < out.Rect.Dx(); x++ {
			r, _, _, _ := img.At(b.Min.X+factor*x, b.Min.Y+factor*y).RGBA()
			out.SetGray(x, y, color.Gray{Y: uint8(r >> 8)})
		}
	}
	return out
}

// TestPyramidRemovesHighFrequencyAliasing reduces fine test patterns 4×, as
// a 4k mammogram fitted to a ~1000-pixel viewport is: plain decimation
// keeps (or even turns into a coarse moiré) the full contrast of detail the
// display cannot resolve, the box pyramid averages it away.
func TestPyramidRemovesHighFrequencyAliasing(t *testing.T) {
	for _, tc := range []struct {
		name         string
		period       int
		maxAmplitude int
	}{
		{"1-pixel lines", 2, 0},
		{"period 4", 4, 0},
		{"period 3 moiré", 3, 64},
		{"period 5 moiré", 5, 64},
	} {
		src := stripes(4096, 16, tc.period)
		level, factor := NewImagePyramid(src).ForDisplay(1000, 4)
		if factor != 4 || level.Bounds().Dx() != 1024 {
			t.Fatalf("%s: level %v factor %d", tc.name, level.Bounds(), factor)
		}
		if got := amplitude(level); got > tc.maxAmplitude {
			t.Errorf("%s: pyramid keeps amplitude %d", tc.name, got)
		}
		if got := amplitude(decimate(src, 4)); tc.period != 4 && got < 255 && tc.period != 2 {
			t.Errorf("%s: decimation amplitude %d; the pattern does not alias", tc.name, got)
		}
		// The mean brightness is preserved.
		mean := 0.0
		for x := 0; x < level.Bounds().Dx(); x++ {
			r, _, _, _ := level.At(x, 0).RGBA()
			mean += float64(r>>8) / float64(level.Bounds().Dx())
		}
		if want := 255.0 / float64(tc.period); math.Abs(mean-want) > 2 {
			t.Errorf("%s: mean %.1f, want %.1f", tc.name, mean, want)
		}
	}
}

// TestHalveBoxGeometry checks the BinShrink geometry on a ramp: output
// pixel x of a 2^k shrink has the value of source coordinate
// ShrunkPixelCenter(x, 2^k), so its origin moves by (f−1)/2 spacings.
func TestHalveBoxGeometry(t *testing.T) {
	if ShrunkPixelCenter(0, 2) != 0.5 || ShrunkPixelCenter(3, 4) != 13.5 || ShrunkPixelCenter(2, 1) != 2 {
		t.Fatal("ShrunkPixelCenter")
	}
	ramp := image.NewGray(image.Rect(0, 0, 63, 5))
	for y := 0; y < 5; y++ {
		for x := 0; x < 63; x++ {
			ramp.SetGray(x, y, color.Gray{Y: uint8(4 * x)})
		}
	}
	pyramid := NewImagePyramid(ramp)
	for _, factor := range []int{2, 4} {
		level, got := pyramid.ForDisplay(63/factor, 5/factor)
		if got != factor {
			t.Fatalf("factor %d, want %d", got, factor)
		}
		if want := image.Rect(0, 0, 63/factor, 5/factor); level.Bounds() != want {
			t.Fatalf("factor %d: bounds %v, want %v (floor, as BinShrink)", factor, level.Bounds(), want)
		}
		for x := 0; x < level.Bounds().Dx(); x++ {
			r, _, _, _ := level.At(x, 0).RGBA()
			if want := 4 * ShrunkPixelCenter(x, factor); float64(r>>8) != want {
				t.Fatalf("factor %d pixel %d = %d, want %.1f", factor, x, r>>8, want)
			}
		}
	}
	if HalveBox(image.NewGray(image.Rect(0, 0, 1, 9))) != nil {
		t.Fatal("a one-pixel-wide image was halved")
	}
}

func TestPyramidLevelSelection(t *testing.T) {
	pyramid := NewImagePyramid(image.NewGray(image.Rect(0, 0, 4096, 3328)))
	for _, tc := range []struct {
		width, height, factor int
	}{
		{900, 730, 4}, {1100, 700, 2}, {2048, 1664, 2}, {2049, 10, 1}, {5000, 5000, 1}, {0, 0, 1}, {1, 1, 2048},
	} {
		level, factor := pyramid.ForDisplay(tc.width, tc.height)
		if factor != tc.factor || level.Bounds().Dx() != 4096/factor {
			t.Errorf("display %dx%d: factor %d (%v), want %d", tc.width, tc.height, factor, level.Bounds(), tc.factor)
		}
	}
	if pyramid.Source().Bounds().Dx() != 4096 {
		t.Fatal("source replaced")
	}
}

// TestHalveBoxPixelFormats checks that every input format averages alike,
// and that transparent pixels do not darken a block.
func TestHalveBoxPixelFormats(t *testing.T) {
	values := []uint8{10, 200, 37, 91}
	gray := image.NewGray(image.Rect(0, 0, 2, 2))
	rgba := image.NewRGBA(gray.Rect)
	nrgba := image.NewNRGBA(gray.Rect)
	gray16 := image.NewGray16(gray.Rect)
	for i, v := range values {
		x, y := i%2, i/2
		gray.SetGray(x, y, color.Gray{Y: v})
		rgba.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		nrgba.SetNRGBA(x, y, color.NRGBA{R: v, G: v, B: v, A: 255})
		gray16.SetGray16(x, y, color.Gray16{Y: uint16(v) * 257})
	}
	want := uint8((10 + 200 + 37 + 91 + 2) / 4)
	for name, img := range map[string]image.Image{"gray": gray, "rgba": rgba, "nrgba": nrgba, "generic": gray16} {
		if got := HalveBox(img).RGBAAt(0, 0); got != (color.RGBA{R: want, G: want, B: want, A: 255}) {
			t.Errorf("%s: %v, want %d", name, got, want)
		}
	}
	half := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	half.SetNRGBA(0, 0, color.NRGBA{R: 200, G: 200, B: 200, A: 255})
	half.SetNRGBA(1, 0, color.NRGBA{R: 200, G: 200, B: 200, A: 255})
	if got := HalveBox(half).RGBAAt(0, 0); got.A != 128 || got.R != 100 {
		t.Fatalf("half-transparent block %v, want premultiplied 100 at alpha 128", got)
	}
}

// BenchmarkPyramidForDisplay measures the frame-change cost of a 4096×3328
// mammogram fitted to a 1000-pixel viewport (two halvings).
func BenchmarkPyramidForDisplay(b *testing.B) {
	src := image.NewNRGBA(image.Rect(0, 0, 4096, 3328))
	for i := range src.Pix {
		src.Pix[i] = uint8(i * 7)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, factor := NewImagePyramid(src).ForDisplay(1000, 800); factor != 4 {
			b.Fatal(factor)
		}
	}
}
