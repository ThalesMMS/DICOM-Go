package jpegxl

import (
	"bytes"
	"errors"
	"image"
	stdjpeg "image/jpeg"
	"math"
	"testing"
)

// stdDecode renders a JPEG with the Go standard library as interleaved
// samples with the given channel count.
func stdDecode(t *testing.T, jpg []byte, channels int) []int32 {
	t.Helper()
	img, err := stdjpeg.Decode(bytes.NewReader(jpg))
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	out := make([]int32, 0, b.Dx()*b.Dy()*channels)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if channels == 1 {
				out = append(out, int32(r>>8))
				continue
			}
			out = append(out, int32(r>>8), int32(g>>8), int32(bl>>8))
		}
	}
	return out
}

func encodeStdJPEG(t testing.TB, img *Image, quality int) []byte {
	t.Helper()
	rect := image.Rect(0, 0, img.Width, img.Height)
	var src image.Image
	if img.Channels == 1 {
		g := image.NewGray(rect)
		for i, v := range img.Samples {
			g.Pix[i] = uint8(v)
		}
		src = g
	} else {
		rgba := image.NewRGBA(rect)
		for i := 0; i < img.Width*img.Height; i++ {
			copy(rgba.Pix[4*i:], []uint8{uint8(img.Samples[3*i]), uint8(img.Samples[3*i+1]), uint8(img.Samples[3*i+2]), 0xFF})
		}
		src = rgba
	}
	var b bytes.Buffer
	if err := stdjpeg.Encode(&b, src, &stdjpeg.Options{Quality: quality}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func psnr(a, b []int32, maxVal float64) float64 {
	var se float64
	for i := range a {
		d := float64(a[i] - b[i])
		se += d * d
	}
	if se == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(maxVal*maxVal*float64(len(a))/se)
}

func maxDiff(a, b []int32) int32 {
	var m int32
	for i := range a {
		m = max(m, abs32(a[i]-b[i]))
	}
	return m
}

// checkJPEGRender compares a rendered JPEG-compatible frame with the Go JPEG
// decoder's rendering of the same coefficients. Grey differs only by IDCT
// rounding; colour also differs by chroma upsampling (the Go decoder
// replicates samples, this package interpolates as libjxl does).
func checkJPEGRender(t *testing.T, tc any, got, ref []int32, channels int) {
	t.Helper()
	if channels == 1 {
		if d := maxDiff(got, ref); d > 2 {
			t.Errorf("%+v: max difference from the Go JPEG decoder %d > 2", tc, d)
		}
		return
	}
	if p := psnr(got, ref, 255); p < 33 {
		t.Errorf("%+v: PSNR against the Go JPEG decoder %.1f dB < 33", tc, p)
	}
}

func TestEncodeLossyRendersLikeTheEquivalentJPEG(t *testing.T) {
	cases := []struct{ w, h, c, quality int }{
		{1, 1, 1, 90}, {64, 48, 1, 90}, {300, 270, 1, 75}, {33, 21, 3, 90}, {257, 129, 3, 95}, {16, 16, 3, 60},
	}
	for _, tc := range cases {
		img := syntheticImage(tc.w, tc.h, tc.c, 8, int64(tc.w+tc.h))
		data, err := EncodeLossy(img, LossyOptions{Quality: tc.quality})
		if err != nil {
			t.Fatalf("%+v: encode: %v", tc, err)
		}
		if _, err := ReconstructJPEG(data); !errors.Is(err, ErrNoReconstructionData) {
			t.Fatalf("%+v: lossy file must not carry JPEG reconstruction data: %v", tc, err)
		}
		got, err := Decode(data)
		if err != nil {
			t.Fatalf("%+v: decode: %v", tc, err)
		}
		if got.Width != tc.w || got.Height != tc.h || got.Channels != tc.c || got.BitsPerSample != 8 {
			t.Fatalf("%+v: decoded %dx%dx%d %d bits", tc, got.Width, got.Height, got.Channels, got.BitsPerSample)
		}
		checkJPEGRender(t, tc, got.Samples, stdDecode(t, encodeStdJPEG(t, img, tc.quality), tc.c), tc.c)
		if p := psnr(got.Samples, img.Samples, 255); p < 26 {
			t.Errorf("%+v: PSNR %.1f dB against the source", tc, p)
		}
		t.Logf("%+v: %d bytes, PSNR %.1f dB", tc, len(data), psnr(got.Samples, img.Samples, 255))
	}
}

func TestEncodeLossyRejectsInvalidInput(t *testing.T) {
	img := syntheticImage(8, 8, 1, 8, 1)
	for name, tc := range map[string]struct {
		img  *Image
		opts LossyOptions
	}{
		"nil":      {nil, LossyOptions{}},
		"quality":  {img, LossyOptions{Quality: 101}},
		"bits":     {syntheticImage(8, 8, 1, 12, 1), LossyOptions{}},
		"channels": {&Image{Width: 1, Height: 1, Channels: 2, BitsPerSample: 8, Samples: []int32{0, 0}}, LossyOptions{}},
		"range":    {&Image{Width: 1, Height: 1, Channels: 1, BitsPerSample: 8, Samples: []int32{256}}, LossyOptions{}},
		"count":    {&Image{Width: 2, Height: 1, Channels: 1, BitsPerSample: 8, Samples: []int32{1}}, LossyOptions{}},
	} {
		if _, err := EncodeLossy(tc.img, tc.opts); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: err = %v, want ErrUnsupported", name, err)
		}
	}
}

func TestRecompressJPEGRoundTripsSyntheticJPEGs(t *testing.T) {
	cases := []struct{ w, h, c, quality int }{
		{1, 1, 3, 90}, {64, 48, 1, 90}, {33, 21, 3, 90}, {257, 129, 3, 95}, {600, 40, 3, 60}, {300, 270, 3, 75},
	}
	for _, tc := range cases {
		img := syntheticImage(tc.w, tc.h, tc.c, 8, int64(tc.w*tc.h))
		jpg := encodeStdJPEG(t, img, tc.quality)
		jxl, err := RecompressJPEG(jpg)
		if tc.c == 1 {
			// The Go encoder writes an unused chroma table in grey JPEGs,
			// which a JPEG XL frame cannot carry (libjxl refuses it too).
			if !errors.Is(err, ErrNotReversible) {
				t.Fatalf("%+v: err = %v, want ErrNotReversible", tc, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%+v: recompress: %v", tc, err)
		}
		back, err := ReconstructJPEG(jxl)
		if err != nil {
			t.Fatalf("%+v: reconstruct: %v", tc, err)
		}
		if !bytes.Equal(back, jpg) {
			t.Fatalf("%+v: reconstructed JPEG differs", tc)
		}
		got, err := Decode(jxl)
		if err != nil {
			t.Fatalf("%+v: decode: %v", tc, err)
		}
		checkJPEGRender(t, tc, got.Samples, stdDecode(t, jpg, tc.c), tc.c)
		t.Logf("%+v: JPEG %d bytes -> JPEG XL %d bytes", tc, len(jpg), len(jxl))
	}
}
