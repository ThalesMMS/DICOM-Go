package render

import (
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func TestRenderFrameRemappedMatchesPlainRenderAndShiftsValues(t *testing.T) {
	values := []uint16{0, 100, 1000, 1500, 2048, 3000, 4095}
	frame := gray16TestFrame("MONOCHROME2", values)
	stored := slices.Clone(frame.PixelBytes)
	window := WindowLevel{Center: 1500, Width: 2000}
	cache := NewRenderCache(1 << 20)
	plain, err := cache.RenderFrame(frame, window)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*RenderCache{nil, cache} {
		for name, remap := range map[string]ModalityRemap{"nil": nil, "identity": func(v float64) float64 { return v }} {
			got, err := c.RenderFrameRemapped(frame, window, remap)
			if err != nil {
				t.Fatal(err)
			}
			for x := range values {
				if a, b := got.GrayAt(x, 0).Y, renderCacheGrayAt(plain, x, 0); a != b {
					t.Fatalf("%s remap, cache %v: pixel %d = %d, plain render %d", name, c != nil, x, a, b)
				}
			}
		}
	}
	// Adding 500 to every value is the same as lowering the window centre
	// by 500.
	shifted, err := cache.RenderFrameRemapped(frame, window, func(v float64) float64 { return v + 500 })
	if err != nil {
		t.Fatal(err)
	}
	lowered, err := RenderFrame(frame, WindowLevel{Center: 1000, Width: 2000})
	if err != nil {
		t.Fatal(err)
	}
	for x := range values {
		if a, b := shifted.GrayAt(x, 0).Y, renderCacheGrayAt(lowered, x, 0); a != b {
			t.Fatalf("pixel %d: remapped %d, lowered window %d", x, a, b)
		}
	}
	// NaN from a remap renders black; the stored pixels and the cached
	// plain render are untouched.
	nan, err := cache.RenderFrameRemapped(frame, window, func(float64) float64 { return math.NaN() })
	if err != nil {
		t.Fatal(err)
	}
	if nan.GrayAt(3, 0).Y != 0 {
		t.Fatalf("NaN rendered as %d", nan.GrayAt(3, 0).Y)
	}
	if !slices.Equal(stored, frame.PixelBytes) {
		t.Fatal("remapped render modified the stored pixel data")
	}
	again, err := cache.RenderFrame(frame, window)
	if err != nil {
		t.Fatal(err)
	}
	for x := range values {
		if renderCacheGrayAt(again, x, 0) != renderCacheGrayAt(plain, x, 0) {
			t.Fatal("remapped render changed the cached plain render")
		}
	}
}

func TestRenderFrameRemappedInvertsMonochrome1AndRejectsColor(t *testing.T) {
	values := []uint16{0, 2000, 4095}
	remap := func(v float64) float64 { return v / 2 }
	mono2, err := (*RenderCache)(nil).RenderFrameRemapped(gray16TestFrame("MONOCHROME2", values), WindowLevel{}, remap)
	if err != nil {
		t.Fatal(err)
	}
	mono1, err := (*RenderCache)(nil).RenderFrameRemapped(gray16TestFrame("MONOCHROME1", values), WindowLevel{}, remap)
	if err != nil {
		t.Fatal(err)
	}
	for x := range values {
		if a, b := mono2.GrayAt(x, 0).Y, mono1.GrayAt(x, 0).Y; int(a)+int(b) != 255 {
			t.Fatalf("pixel %d: MONOCHROME2 %d + MONOCHROME1 %d != 255", x, a, b)
		}
	}
	color := &Frame{
		Metadata: pixeldata.Metadata{
			Rows: 1, Columns: 1, SamplesPerPixel: 3, BitsAllocated: 8, BitsStored: 8, HighBit: 7,
			PhotometricInterpretation: "RGB",
		},
		ByteOrder:  binary.LittleEndian,
		PixelBytes: []byte{10, 20, 30},
	}
	if _, err := NewRenderCache(1<<20).RenderFrameRemapped(color, WindowLevel{}, remap); !errors.Is(err, ErrNotGrayscaleFrame) {
		t.Fatalf("color frame: %v", err)
	}
	if _, err := FrameModalityValues(color); !errors.Is(err, ErrNotGrayscaleFrame) {
		t.Fatalf("color modality values: %v", err)
	}
	if _, err := FrameModalityValues(nil); err == nil {
		t.Fatal("nil frame decoded")
	}
}

func TestFrameModalityValuesAppliesRescaleAndIsOwnedByCaller(t *testing.T) {
	frame := gray16TestFrame("MONOCHROME2", []uint16{0, 10, 4095})
	frame.Rescale = Rescale{Slope: 2, Intercept: -1000}
	got, err := FrameModalityValues(frame)
	if err != nil {
		t.Fatal(err)
	}
	if want := []float32{-1000, -980, 7190}; !slices.Equal(got, want) {
		t.Fatalf("modality values %v, want %v", got, want)
	}
	got[0] = 12345
	again, err := FrameModalityValues(frame)
	if err != nil {
		t.Fatal(err)
	}
	if again[0] != -1000 {
		t.Fatalf("modifying the returned values changed a later decode: %v", again)
	}
}
