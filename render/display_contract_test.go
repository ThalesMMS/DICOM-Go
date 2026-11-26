package render

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func TestAutoWindowFiltersBeforeSignedRescale(t *testing.T) {
	values := []int{-16, -10, -8, 15, -16, -6, 1, 9, 14, -2, 2, -12}
	var pixels []byte
	for _, value := range values {
		// Embed a signed five-bit value in bits 8..12. The high and low
		// uncoded bits have nonzero values and must not enter the histogram.
		pixels = binary.BigEndian.AppendUint16(pixels, uint16(value&31)<<8|0xa035)
	}
	frame := &Frame{
		Metadata: pixeldata.Metadata{
			Rows: 3, Columns: 4, SamplesPerPixel: 1,
			BitsAllocated: 16, BitsStored: 5, HighBit: 12, PixelRepresentation: 1,
			PhotometricInterpretation: "MONOCHROME2",
		},
		PixelBytes: pixels, ByteOrder: binary.BigEndian,
		Rescale:      Rescale{Slope: -2, Intercept: 3},
		PixelPadding: &pixeldata.PixelPadding{Value: -16, RangeLimit: -10, HasRange: true},
	}
	for _, tc := range []struct {
		name string
		opts AutoWindowOptions
		want WindowLevel
	}{
		{"inclusive-padding-endpoints", AutoWindowOptions{}, WindowLevel{Center: -3, Width: 48}},
		{"region-clipped-to-image", AutoWindowOptions{Region: image.Rect(-4, 0, 3, 2)}, WindowLevel{Center: 11, Width: 20}},
		{"region-and-predicate", AutoWindowOptions{Region: image.Rect(0, 1, 3, 3), Include: func(x, y int) bool {
			return x > 0
		}}, WindowLevel{Center: 8, Width: 18}},
		{"trim-extreme-values", AutoWindowOptions{Strategy: AutoWindowMinMaxIgnoreExtremes}, WindowLevel{Center: -4, Width: 42}},
		{"trim-quartiles-of-remaining-samples", AutoWindowOptions{Strategy: AutoWindowHistogram, HistogramIgnore: .25}, WindowLevel{Center: -5, Width: 40}},
		{"nonfinite-trim-defaults-to-zero", AutoWindowOptions{Strategy: AutoWindowHistogram, HistogramIgnore: math.NaN()}, WindowLevel{Center: -4, Width: 50}},
		{"trim-clamped-to-half", AutoWindowOptions{Strategy: AutoWindowHistogram, HistogramIgnore: 1.5}, WindowLevel{}},
		{"region-has-only-padding", AutoWindowOptions{Region: image.Rect(0, 0, 2, 1)}, WindowLevel{}},
		{"region-outside-frame", AutoWindowOptions{Region: image.Rect(5, 5, 10, 10)}, WindowLevel{}},
		{"only-one-value-after-padding", AutoWindowOptions{Region: image.Rect(1, 0, 3, 1)}, WindowLevel{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := AutoWindowWith(frame, tc.opts)
			if ok != (tc.want.Width > 0) || got != tc.want {
				t.Fatalf("window = %+v, %v; want %+v", got, ok, tc.want)
			}
		})
	}
	if low, high, ok := PixelValueRange(frame); !ok || low != -27 || high != 19 {
		t.Fatalf("calibrated range = %g..%g, %v; want -27..19", low, high, ok)
	}
}

func TestEncodedJPEGQualityAndIndependentDecode(t *testing.T) {
	const width, height = 19, 13
	pixels := make([]byte, width*height)
	for index := range pixels {
		x, y := index%width, index/width
		pixels[index] = byte(x*37 + y*21 + x*y*13)
	}
	frame := &Frame{
		Metadata: pixeldata.Metadata{
			Rows: height, Columns: width, SamplesPerPixel: 1,
			BitsAllocated: 8, BitsStored: 8, HighBit: 7, PhotometricInterpretation: "MONOCHROME2",
		},
		PixelBytes: pixels, Rescale: Rescale{Slope: 1},
	}
	renderer := mustStandardEncodedRenderer(t, EncodedImageLimits{})
	request := EncodedRequest{
		Frame: frame, Window: WindowLevel{Center: 128, Width: 256}, MediaType: " image/JPEG ",
	}
	encode := func(t *testing.T, quality int) EncodedImage {
		t.Helper()
		req := request
		req.JPEGQuality = quality
		encoded, err := renderer.RenderEncoded(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if encoded.Width != width || encoded.Height != height || encoded.MediaType != MediaTypeJPEG {
			t.Fatalf("encoded representation = %dx%d %q", encoded.Width, encoded.Height, encoded.MediaType)
		}
		return encoded
	}
	squaredError := func(t *testing.T, encoded EncodedImage) int64 {
		t.Helper()
		img, err := jpeg.Decode(bytes.NewReader(encoded.Data))
		if err != nil {
			t.Fatal(err)
		}
		if img.Bounds().Size() != image.Pt(width, height) {
			t.Fatalf("decoded JPEG bounds = %v", img.Bounds())
		}
		var total int64
		for i, source := range pixels {
			value := color.GrayModel.Convert(img.At(i%width, i/width)).(color.Gray).Y
			delta := int64(value) - int64(source)
			total += delta * delta
		}
		return total
	}
	t.Run("quality-endpoints-have-different-fidelity", func(t *testing.T) {
		lowError := squaredError(t, encode(t, 1))
		highError := squaredError(t, encode(t, 100))
		if lowError == 0 || highError >= lowError/10 {
			t.Fatalf("JPEG squared errors: quality 1 = %d, quality 100 = %d", lowError, highError)
		}
	})
	t.Run("default-quality-is-90", func(t *testing.T) {
		if !bytes.Equal(encode(t, 0).Data, encode(t, 90).Data) {
			t.Fatal("implicit and explicit default quality produced different bytes")
		}
	})
	for _, tc := range []struct {
		name    string
		quality int
	}{{"below-range", -1}, {"above-range", 101}} {
		t.Run(tc.name, func(t *testing.T) {
			req := request
			req.JPEGQuality = tc.quality
			got, err := renderer.RenderEncoded(context.Background(), req)
			if !errors.Is(err, ErrInvalidEncodedRequest) || len(got.Data) != 0 {
				t.Fatalf("quality %d returned %d bytes and error %v", tc.quality, len(got.Data), err)
			}
		})
	}
}

func TestEncodedPNGAspectAndCompleteOutputBudget(t *testing.T) {
	source := make([]byte, 9*5)
	for i := range source {
		source[i] = 64
	}
	frame := &Frame{
		Metadata: pixeldata.Metadata{
			Rows: 5, Columns: 9, SamplesPerPixel: 1,
			BitsAllocated: 8, BitsStored: 8, HighBit: 7, PhotometricInterpretation: "MONOCHROME2",
		},
		PixelBytes: source, Rescale: Rescale{Slope: 1},
	}
	renderer := mustStandardEncodedRenderer(t, EncodedImageLimits{})
	for _, tc := range []struct {
		name   string
		crop   image.Rectangle
		resize image.Point
		want   image.Point
	}{
		{"no-dimensions", image.Rectangle{}, image.Point{}, image.Pt(9, 5)},
		{"width-only", image.Rectangle{}, image.Pt(5, 0), image.Pt(5, 3)},
		{"height-only", image.Rectangle{}, image.Pt(0, 3), image.Pt(5, 3)},
		{"height-limits-cropped-portrait", image.Rect(2, 0, 5, 5), image.Pt(3, 2), image.Pt(1, 2)},
		{"nonzero-short-axis-at-one-pixel", image.Rectangle{}, image.Pt(1, 0), image.Pt(1, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := EncodedRequest{
				Frame: frame, Window: WindowLevel{Center: 128, Width: 256},
				Width: tc.resize.X, Height: tc.resize.Y, SourceRect: tc.crop,
			}
			out, err := renderer.RenderEncoded(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if out.MediaType != MediaTypePNG || image.Pt(out.Width, out.Height) != tc.want {
				t.Fatalf("encoded representation = %dx%d %q, want %v PNG", out.Width, out.Height, out.MediaType, tc.want)
			}
			img, err := png.Decode(bytes.NewReader(out.Data))
			if err != nil {
				t.Fatal(err)
			}
			if img.Bounds().Size() != tc.want {
				t.Fatalf("decoded PNG bounds = %v, want size %v", img.Bounds(), tc.want)
			}
			for y := 0; y < tc.want.Y; y++ {
				for x := 0; x < tc.want.X; x++ {
					if got := color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y; got != 64 {
						t.Fatalf("PNG pixel (%d,%d) = %d, want 64", x, y, got)
					}
				}
			}
		})
	}
	t.Run("exact-output-byte-limit", func(t *testing.T) {
		req := EncodedRequest{Frame: frame, Window: WindowLevel{Center: 128, Width: 256}}
		complete, err := renderer.RenderEncoded(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		limit := int64(len(complete.Data))
		got, err := mustStandardEncodedRenderer(t, EncodedImageLimits{MaxOutputBytes: limit}).RenderEncoded(context.Background(), req)
		if err != nil || !bytes.Equal(complete.Data, got.Data) {
			t.Fatalf("exact limit %d returned %d bytes and error %v", limit, len(got.Data), err)
		}
		got, err = mustStandardEncodedRenderer(t, EncodedImageLimits{MaxOutputBytes: limit - 1}).RenderEncoded(context.Background(), req)
		if !errors.Is(err, ErrEncodedImageResourceLimit) || len(got.Data) != 0 {
			t.Fatalf("short limit %d returned %d bytes and error %v", limit-1, len(got.Data), err)
		}
	})
}
