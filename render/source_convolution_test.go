package render

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

func TestSourceConvolutionSignedLayoutFractionalRescaleAndRewindow(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		t.Run(order.String(), func(t *testing.T) {
			source := sourceConvolutionGray16(order, 12, 13, true, []int{-21, 10, 100})
			source.Rescale = Rescale{Slope: .5, Intercept: -10}
			kernel := SourceKernel3x3{Weights: [9]float64{3: 1, 5: 1}, Divisor: 2}
			original := append([]byte(nil), source.PixelBytes...)
			filtered, err := PrepareFrameConvolution3x3Context(context.Background(), source, kernel)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(source.PixelBytes, original) {
				t.Fatal("convolution edited acquired pixels")
			}
			if filtered.MemoryBytes() != 3*8 {
				t.Fatalf("retained payload = %d", filtered.MemoryBytes())
			}
			for i, want := range []float64{-12.75, 9.75, 17.5} {
				if filtered.values[i] != want {
					t.Errorf("modality value %d = %g, want %g", i, filtered.values[i], want)
				}
			}
			// Overwrite acquired bytes only after preparation to verify the
			// result owns its samples. Both windows reuse the same result.
			clear(source.PixelBytes)
			for _, window := range []WindowLevel{
				{Center: 0, Width: 30, Function: display.VOILinearExact},
				{Center: -12.75, Width: 2, Function: display.VOILinearExact},
			} {
				got := renderConvolvedForTest(t, filtered, window).(*image.Gray)
				for i, value := range []float64{-12.75, 9.75, 17.5} {
					want := WindowedGray(value, window)
					if got.Pix[i] != want {
						t.Errorf("window %v at %d = %d, want %d", window, i, got.Pix[i], want)
					}
				}
			}
		})
	}
}

func TestSourceConvolutionPhysicalSaturationAndMidrange(t *testing.T) {
	kernel := SourceKernel3x3{Weights: [9]float64{4: 32}, Divisor: 1}
	for _, tc := range []struct {
		name   string
		source *Frame
		kernel SourceKernel3x3
		want   []float64
	}{
		{"signed-16", sourceConvolutionGray16(binary.LittleEndian, 12, 11, true, []int{-2048, 2047}), kernel, []float64{-32768, 32767}},
		{"unsigned-16", sourceConvolutionGray16(binary.LittleEndian, 12, 11, false, []int{1000, 4095}), kernel, []float64{32000, 65535}},
		{"unsigned-8-word-intermediate", sourceConvolutionGray8([]byte{30, 200}, false), kernel, []float64{960, 6400}},
		{"unsigned-midrange-uniform-border", sourceConvolutionGray16(binary.LittleEndian, 12, 11, false, []int{42, 42}), SourceKernel3x3{Weights: [9]float64{3: -1, 4: 1}, Divisor: 1, MidrangeBias: true}, []float64{2048, 2048}},
		{"signed-midrange-zero", sourceConvolutionGray8([]byte{252, 252}, true), SourceKernel3x3{Weights: [9]float64{3: -1, 4: 1}, Divisor: 1, MidrangeBias: true}, []float64{0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filtered, err := PrepareFrameConvolution3x3Context(context.Background(), tc.source, tc.kernel)
			if err != nil {
				t.Fatal(err)
			}
			for i, want := range tc.want {
				if filtered.values[i] != want {
					t.Errorf("stored response %d = %g, want %g", i, filtered.values[i], want)
				}
			}
		})
	}
}

func TestSourceConvolutionKeepsSourcePaddingBlackAndConvolvesItsNeighbors(t *testing.T) {
	frame := sourceConvolutionGray16(binary.LittleEndian, 16, 15, true, []int{-1000, 7, 11})
	frame.Metadata.PhotometricInterpretation = "MONOCHROME1"
	frame.PixelPadding = &pixeldata.PixelPadding{Value: -1001, RangeLimit: -1000, HasRange: true}
	kernel := SourceKernel3x3{Weights: [9]float64{3: 1, 4: 1, 5: 1}, Divisor: 3}
	filtered, err := PrepareFrameConvolution3x3Context(context.Background(), frame, kernel)
	if err != nil {
		t.Fatal(err)
	}
	if !math.IsNaN(filtered.values[0]) || filtered.values[1] != -982.0/3 || filtered.values[2] != 29.0/3 {
		t.Fatalf("filtered padding and neighbors = %v", filtered.values)
	}
	lut, err := display.NewLUT([]int{2, 0, 8}, []uint16{14, 108})
	if err != nil {
		t.Fatal(err)
	}
	present := func(window WindowLevel) []byte {
		return renderConvolvedForTest(t, filtered, window).(*image.Gray).Pix
	}
	if got := present(WindowLevel{LUT: lut}); !bytes.Equal(got, []byte{0, 241, 147}) {
		t.Fatalf("VOI LUT and MONOCHROME1 inversion = %v", got)
	}
	frame.DefaultWindow.LUT = lut
	owned, err := PrepareFrameConvolution3x3Context(context.Background(), frame, kernel)
	if err != nil {
		t.Fatal(err)
	}
	lut.Entries[0] = 255
	if got := renderConvolvedForTest(t, owned, WindowLevel{}).(*image.Gray).Pix; !bytes.Equal(got, []byte{0, 241, 147}) {
		t.Fatalf("source LUT mutation changed detached result: %v", got)
	}
}

func TestSourceConvolutionRGBPerChannelNativeDepth(t *testing.T) {
	kernel := SourceKernel3x3{Weights: [9]float64{3: -1, 4: 2}, Divisor: 1}
	for _, tc := range []struct {
		name                               string
		data                               []byte
		allocated, stored, highBit, planar uint16
		order                              binary.ByteOrder
		want                               color.RGBA
	}{
		{"packed-8", []byte{20, 120, 0, 200, 5, 141}, 8, 8, 7, 0, nil, color.RGBA{255, 0, 255, 255}},
		{"planar-8", []byte{20, 200, 120, 5, 0, 141}, 8, 8, 7, 1, nil, color.RGBA{255, 0, 255, 255}},
		{"packed-16", sourceConvolutionRGB16(binary.BigEndian, []int{100, 3000, 2000, 3000, 100, 2010}), 16, 12, 13, 0, binary.BigEndian, color.RGBA{255, 0, 126, 255}},
		{"planar-16", sourceConvolutionRGB16(binary.LittleEndian, []int{100, 3000, 3000, 100, 2000, 2010}), 16, 12, 13, 1, binary.LittleEndian, color.RGBA{255, 0, 126, 255}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := &Frame{Metadata: pixeldata.Metadata{
				Rows: 1, Columns: 2, BitsAllocated: tc.allocated, BitsStored: tc.stored, HighBit: tc.highBit,
				SamplesPerPixel: 3, PhotometricInterpretation: "RGB", PlanarConfiguration: tc.planar,
			}, ByteOrder: tc.order, PixelBytes: tc.data}
			filtered, err := PrepareFrameConvolution3x3Context(context.Background(), frame, kernel)
			if err != nil {
				t.Fatal(err)
			}
			img := renderConvolvedForTest(t, filtered, WindowLevel{Center: -5000, Width: 1}).(*image.RGBA)
			if got := img.RGBAAt(1, 0); got != tc.want {
				t.Errorf("filtered pixel = %v, want %v", got, tc.want)
			}
			clear(img.Pix)
			if got := renderConvolvedForTest(t, filtered, WindowLevel{}).(*image.RGBA).RGBAAt(1, 0); got != tc.want {
				t.Errorf("consumer modified the resident result: %v", got)
			}
		})
	}
}

// A directional kernel distinguishes correlation from flipped convolution.
// Check every row, including worker-chunk transitions, against a direct oracle.
func TestSourceConvolutionReplicatedEdgesAndParallelRows(t *testing.T) {
	const columns, rows = 19, 73
	pixels := make([]byte, columns*rows)
	for y := 0; y < rows; y++ {
		for x := 0; x < columns; x++ {
			pixels[y*columns+x] = byte((x*x + 7*y + x*y) % 251)
		}
	}
	source := sourceConvolutionGray8(pixels, false)
	source.Metadata.Columns, source.Metadata.Rows = columns, rows
	kernel := SourceKernel3x3{Weights: [9]float64{1, 2, -1, 3, 4, -2, 7, 8, 0}, Divisor: 5, Bias: 100}
	got, err := PrepareFrameConvolution3x3Context(context.Background(), source, kernel)
	if err != nil {
		t.Fatal(err)
	}
	clampIndex := func(value, end int) int {
		if value < 0 {
			return 0
		}
		if value >= end {
			return end - 1
		}
		return value
	}
	for y := 0; y < rows; y++ {
		for x := 0; x < columns; x++ {
			var sum float64
			for row := 0; row < 3; row++ {
				sy := clampIndex(y+row-1, rows)
				for col := 0; col < 3; col++ {
					sx := clampIndex(x+col-1, columns)
					sum += float64(pixels[sy*columns+sx]) * kernel.Weights[row*3+col]
				}
			}
			want := max(0, sum/5+100)
			if v := got.values[y*columns+x]; v != want {
				t.Fatalf("at (%d,%d) got %g, want %g", x, y, v, want)
			}
		}
	}
}

func TestSourceConvolutionRefusesInvalidOrCanceledPreparation(t *testing.T) {
	ctx := context.Background()
	unit := SourceKernel3x3{Weights: [9]float64{4: 1}, Divisor: 1}
	frame := sourceConvolutionGray16(binary.LittleEndian, 16, 15, false, []int{1, 2})
	for _, kernel := range []SourceKernel3x3{
		{}, {Divisor: math.Inf(1)}, {Divisor: 1, Bias: math.NaN()},
		{Divisor: 1, Weights: [9]float64{4: math.MaxFloat64}},
	} {
		if out, err := PrepareFrameConvolution3x3Context(ctx, frame, kernel); out != nil || !errors.Is(err, ErrSourceConvolutionKernel) {
			t.Errorf("kernel %+v gave %v, %v", kernel, out, err)
		}
	}
	large := *frame
	large.Metadata.Columns, large.Metadata.Rows = 12000, 12000
	large.LoadPixels = func(context.Context) (*Frame, error) {
		t.Error("oversized frame should be rejected before loading")
		return nil, nil
	}
	if out, err := PrepareFrameConvolution3x3Context(ctx, &large, unit); out != nil || !errors.Is(err, ErrSourceConvolutionLimit) {
		t.Errorf("oversized result %v, %v", out, err)
	}
	gray, err := PrepareFrameConvolution3x3Context(ctx, frame, unit)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if img, err := gray.RenderContext(canceled, WindowLevel{}); img != nil || !errors.Is(err, context.Canceled) {
		t.Errorf("canceled presentation = %v, %v", img, err)
	}

	canceled, cancel = context.WithCancel(ctx)
	defer cancel()
	frame.ByteOrder = cancelConvolutionOrder{ByteOrder: binary.LittleEndian, cancel: cancel}
	if out, err := PrepareFrameConvolution3x3Context(canceled, frame, unit); out != nil || !errors.Is(err, context.Canceled) {
		t.Errorf("cancellation while reading the row returned %v, %v", out, err)
	}
}

func BenchmarkSourceConvolution3x3(b *testing.B) {
	ctx := context.Background()
	frame := &Frame{
		Metadata: pixeldata.Metadata{Rows: 512, Columns: 512, SamplesPerPixel: 1, BitsAllocated: 16, BitsStored: 16,
			HighBit: 15, PixelRepresentation: 1, PhotometricInterpretation: "MONOCHROME2"},
		PixelBytes: make([]byte, 512*512*2),
	}
	for i := 0; i < len(frame.PixelBytes)/2; i++ {
		binary.LittleEndian.PutUint16(frame.PixelBytes[i*2:], uint16(i))
	}
	kernel := SourceKernel3x3{Weights: [9]float64{1, 2, 1, 2, 4, 2, 1, 2, 1}, Divisor: 16}
	b.Run("Prepare512", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := PrepareFrameConvolution3x3Context(ctx, frame, kernel); err != nil {
				b.Fatal(err)
			}
		}
	})
	filtered, err := PrepareFrameConvolution3x3Context(ctx, frame, kernel)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("Rewindow512", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := filtered.RenderContext(ctx, WindowLevel{Center: float64(i % 128), Width: 512}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func sourceConvolutionGray16(order binary.ByteOrder, bits, highBit uint16, signed bool, values []int) *Frame {
	metadata := pixeldata.Metadata{
		Rows: 1, Columns: uint16(len(values)), BitsAllocated: 16, BitsStored: bits, HighBit: highBit,
		SamplesPerPixel: 1, PhotometricInterpretation: "MONOCHROME2",
	}
	if signed {
		metadata.PixelRepresentation = 1
	}
	bytes := make([]byte, len(values)*2)
	mask := uint16(1<<bits - 1)
	for i, value := range values {
		order.PutUint16(bytes[i*2:], (uint16(value)&mask)<<(highBit+1-bits))
	}
	return &Frame{Metadata: metadata, PixelBytes: bytes, ByteOrder: order}
}

func sourceConvolutionGray8(values []byte, signed bool) *Frame {
	frame := sourceConvolutionGray16(binary.LittleEndian, 8, 7, signed, make([]int, len(values)))
	frame.Metadata.BitsAllocated, frame.PixelBytes = 8, values
	return frame
}

func sourceConvolutionRGB16(order binary.ByteOrder, values []int) []byte {
	pixels := make([]byte, 2*len(values))
	for i, v := range values {
		order.PutUint16(pixels[2*i:], uint16(v)<<2)
	}
	return pixels
}

func renderConvolvedForTest(t *testing.T, prepared *ConvolvedFrame, window WindowLevel) image.Image {
	t.Helper()
	img, err := prepared.RenderContext(context.Background(), window)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

type cancelConvolutionOrder struct {
	binary.ByteOrder
	cancel context.CancelFunc
}

func (o cancelConvolutionOrder) Uint16(data []byte) uint16 {
	o.cancel()
	return o.ByteOrder.Uint16(data)
}
