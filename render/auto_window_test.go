package render

import (
	"encoding/binary"
	"image"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// autoWindowFrame is a 16-bit frame from values in row-major order.
func autoWindowFrame(columns int, values []uint16) *Frame {
	data := make([]byte, 2*len(values))
	for i, value := range values {
		binary.LittleEndian.PutUint16(data[2*i:], value)
	}
	return &Frame{
		Metadata: pixeldata.Metadata{
			Rows: uint16(len(values) / columns), Columns: uint16(columns), SamplesPerPixel: 1,
			BitsAllocated: 16, BitsStored: 12, HighBit: 11,
		},
		ByteOrder:  binary.LittleEndian,
		PixelBytes: data,
		Rescale:    Rescale{Slope: 1},
	}
}

func TestAutoWindowStrategies(t *testing.T) {
	// 0 is background, 4000 a saturated pixel, 100..199 the tissue.
	values := []uint16{0, 0, 0, 0, 4000}
	for v := uint16(100); v < 200; v++ {
		values = append(values, v)
	}
	values = append(values, 0) // 106 values
	frame := autoWindowFrame(106, values)
	cases := []struct {
		name string
		opts AutoWindowOptions
		want WindowLevel
	}{
		{"min-max", AutoWindowOptions{}, WindowLevel{Center: 2000.5, Width: 4001}},
		{"ignore-extremes", AutoWindowOptions{Strategy: AutoWindowMinMaxIgnoreExtremes}, WindowLevel{Center: 150, Width: 100}},
		// 106 pixels * 5% = 5: the five zeros from below (so 1..), the
		// saturated pixel and 199..196 from above: 1..196.
		{"histogram-5", AutoWindowOptions{Strategy: AutoWindowHistogram, HistogramIgnore: 0.05}, WindowLevel{Center: 99, Width: 196}},
		{"histogram-0", AutoWindowOptions{Strategy: AutoWindowHistogram}, WindowLevel{Center: 2001, Width: 4002}},
		{"region", AutoWindowOptions{Region: image.Rect(10, 0, 20, 1)}, WindowLevel{Center: 110, Width: 10}},
		{"include", AutoWindowOptions{Include: func(x, y int) bool { return x >= 5 && x < 105 }}, WindowLevel{Center: 150, Width: 100}},
	}
	for _, tc := range cases {
		got, ok := AutoWindowWith(frame, tc.opts)
		if !ok || got != tc.want {
			t.Errorf("%s: %+v %v, want %+v", tc.name, got, ok, tc.want)
		}
	}
	if _, ok := AutoWindowWith(autoWindowFrame(2, []uint16{7, 7}), AutoWindowOptions{}); ok {
		t.Error("constant frame windowed")
	}
	if _, ok := AutoWindowWith(autoWindowFrame(3, []uint16{1, 2, 3}), AutoWindowOptions{Strategy: AutoWindowMinMaxIgnoreExtremes}); ok {
		t.Error("ignoring extremes left one value but windowed")
	}
	if got := AutoWindowHistogram.String(); got != "histogram" {
		t.Errorf("String = %q", got)
	}
}

func TestAutoWindowIgnoresPixelPadding(t *testing.T) {
	values := []uint16{0, 0, 0, 100, 150, 200}
	frame := autoWindowFrame(6, values)
	frame.PixelPadding = &pixeldata.PixelPadding{Value: 0}
	for _, opts := range []AutoWindowOptions{{}, {Strategy: AutoWindowHistogram}} {
		got, ok := AutoWindowWith(frame, opts)
		if !ok || got.Center-got.Width/2 < 99 {
			t.Fatalf("%v: padding reached the window: %+v", opts.Strategy, got)
		}
	}
	frame.PixelPadding = &pixeldata.PixelPadding{Value: 0, RangeLimit: 100, HasRange: true}
	if got, _ := AutoWindow(frame); got != (WindowLevel{Center: 175.5, Width: 51}) {
		t.Fatalf("padding range: %+v", got)
	}
	frame.PixelPadding = &pixeldata.PixelPadding{Value: 0, RangeLimit: 4095, HasRange: true}
	if _, ok := AutoWindow(frame); ok {
		t.Fatal("all-padding frame windowed")
	}
}

func TestAutoWindowSignedAndNegativeSlope(t *testing.T) {
	frame := autoWindowFrame(2, []uint16{0xFFF6, 10}) // -10 and 10 as 16-bit signed
	frame.Metadata.BitsStored, frame.Metadata.HighBit, frame.Metadata.PixelRepresentation = 16, 15, 1
	if got, ok := AutoWindow(frame); !ok || got != (WindowLevel{Center: 0.5, Width: 21}) {
		t.Fatalf("signed: %+v %v", got, ok)
	}
	frame.Rescale = Rescale{Slope: -1}
	if got, ok := AutoWindow(frame); !ok || got != (WindowLevel{Center: 0.5, Width: 21}) {
		t.Fatalf("negative slope: %+v %v", got, ok)
	}
}

func TestPixelValueRange(t *testing.T) {
	frame := autoWindowFrame(4, []uint16{0, 10, 20, 30})
	frame.Rescale = Rescale{Slope: 2, Intercept: -5}
	frame.PixelPadding = &pixeldata.PixelPadding{Value: 0}
	if lo, hi, ok := PixelValueRange(frame); !ok || lo != 15 || hi != 55 {
		t.Fatalf("range %v..%v %v", lo, hi, ok)
	}
	if _, _, ok := PixelValueRange(nil); ok {
		t.Fatal("nil frame has a range")
	}
}
