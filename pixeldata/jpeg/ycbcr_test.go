package jpeg

import (
	"bytes"
	"image"
	"image/color"
	stdjpeg "image/jpeg"
	"testing"
)

func TestUpsampleRowTriangular(t *testing.T) {
	dst := make([]byte, 6)
	upsampleRow(dst, []byte{0, 100, 200}, 2)
	want := []byte{0, 25, 75, 125, 175, 200}
	if !bytes.Equal(dst, want) {
		t.Fatalf("upsampleRow = %v, want %v", dst, want)
	}
	odd := make([]byte, 5)
	upsampleRow(odd, []byte{0, 100, 200}, 2)
	if !bytes.Equal(odd, want[:5]) {
		t.Fatalf("odd width = %v, want %v", odd, want[:5])
	}
}

func TestUpsample2DTriangular(t *testing.T) {
	dst := make([]byte, 4)
	// Own row weighs 3/4, the nearer neighbour row 1/4, then the same
	// weighting horizontally: sums are in sixteenths before rounding.
	upsample2D(dst, []byte{160, 0}, []byte{0, 0})
	want := []byte{byte((480*4 + 8) >> 4), byte((480*3 + 0 + 7) >> 4), byte((0*3 + 480 + 8) >> 4), byte((0*4 + 7) >> 4)}
	if !bytes.Equal(dst, want) {
		t.Fatalf("upsample2D = %v, want %v", dst, want)
	}
}

func TestYCbCrToRGBFullRange(t *testing.T) {
	cases := []struct {
		y, cb, cr byte
		r, g, b   byte
	}{
		{128, 128, 128, 128, 128, 128},
		{0, 128, 128, 0, 0, 0},
		{255, 128, 128, 255, 255, 255},
		{76, 85, 255, 254, 0, 0},
	}
	for _, c := range cases {
		r, g, b := ycbcrToRGB(c.y, c.cb, c.cr)
		if r != c.r || g != c.g || b != c.b {
			t.Errorf("ycbcrToRGB(%d,%d,%d) = %d,%d,%d want %d,%d,%d", c.y, c.cb, c.cr, r, g, b, c.r, c.g, c.b)
		}
	}
}

// A chroma step between two 4:2:2 blocks must be reconstructed as a ramp,
// not replicated as a hard edge on the block boundary.
func TestSubsampledChromaIsInterpolated(t *testing.T) {
	img := image.NewYCbCr(image.Rect(0, 0, 4, 1), image.YCbCrSubsampleRatio422)
	for i := range img.Y {
		img.Y[i] = 128
	}
	img.Cb[0], img.Cb[1] = 128, 128
	img.Cr[0], img.Cr[1] = 64, 192
	out, ok := ycbcrToRGBFrame(img, 1, 4)
	if !ok {
		t.Fatal("4:2:2 not handled")
	}
	red := []byte{out[0], out[3], out[6], out[9]}
	if !(red[0] < red[1] && red[1] < red[2] && red[2] < red[3]) {
		t.Fatalf("red channel %v is not a ramp", red)
	}
	nearest := color.RGBAModel.Convert(img.At(1, 0)).(color.RGBA)
	if red[1] == nearest.R {
		t.Fatalf("pixel 1 equals the nearest-neighbour value %d", nearest.R)
	}
}

func TestSubsampledJPEGDecodeMatchesFrameConverter(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			c := color.RGBA{R: 200, G: 30, B: 30, A: 255}
			if x >= 8 {
				c = color.RGBA{R: 30, G: 30, B: 200, A: 255}
			}
			src.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := stdjpeg.Encode(&buf, src, &stdjpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	img, err := stdjpeg.Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	ycc, ok := img.(*image.YCbCr)
	if !ok || ycc.SubsampleRatio != image.YCbCrSubsampleRatio420 {
		t.Fatalf("encoder produced %T", img)
	}
	out, ok := ycbcrToRGBFrame(ycc, 16, 16)
	if !ok {
		t.Fatal("4:2:0 not handled")
	}
	// Far from the colour edge the reconstruction returns the source colour.
	for _, x := range []int{1, 14} {
		o := (5*16 + x) * 3
		want := src.RGBAAt(x, 5)
		for i, w := range []byte{want.R, want.G, want.B} {
			if d := int(out[o+i]) - int(w); d < -6 || d > 6 {
				t.Fatalf("pixel (%d,5) channel %d = %d, want about %d", x, i, out[o+i], w)
			}
		}
	}
}
