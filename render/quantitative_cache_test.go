package render

import (
	"bytes"
	"errors"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"image"
	"math"
	"testing"
)

func TestQuantitativeCachePreservesBinary64VOIAndBudget(t *testing.T) {
	for _, value := range []float64{.001, -.001, 1e-18, math.SmallestNonzeroFloat64} {
		frame := testRenderFrame([]byte{1, 1, 1, 1})
		frame.Rescale = Rescale{Slope: value}
		frame.PreserveModalityPrecision = true
		window := WindowLevel{Center: value, Width: display.ManualWindowMinimum(display.VOILinearExact, display.WindowDomain{Min: value, Max: value, Known: true}), Function: display.VOILinearExact}
		cache := NewRenderCache(64)
		for i := 0; i < 3; i++ {
			direct, e := RenderFrame(frame, window)
			if e != nil {
				t.Fatal(e)
			}
			cached, e := cache.RenderFrame(frame, window)
			if e != nil {
				t.Fatal(e)
			}
			if !bytes.Equal(direct.(*image.Gray).Pix, cached.(*image.Gray).Pix) {
				t.Fatalf("cached/direct differ value%g", value)
			}
			wide, e := cache.RenderFrameGray16(frame, window)
			if e != nil {
				t.Fatal(e)
			}
			if wide.Gray16At(0, 0).Y < 30000 || wide.Gray16At(0, 0).Y > 36000 {
				t.Fatal("gray16 precision lost")
			}
			mapped, e := cache.RenderFrameRemapped(frame, window, func(v float64) float64 { return v })
			if e != nil || !bytes.Equal(mapped.Pix, cached.(*image.Gray).Pix) {
				t.Fatal("identity remap precision lost", e)
			}
		}
		if cache.Stats().DecodedBytes != 32 {
			t.Fatalf("decoded budget%+v", cache.Stats())
		}
		if _, e := FrameModalityValues(frame); !errors.Is(e, ErrModalityPrecisionRequired) {
			t.Fatal("float32-only export silently quantized")
		}
		if _, e := cache.RenderFrameValues(frame, window, []float32{1, 1, 1, 1}); !errors.Is(e, ErrModalityPrecisionRequired) {
			t.Fatal("float32-only filter silently quantized")
		}
		legacy, e := decodeSliceFrame(frame)
		if e != nil || legacy.grayscale64 != nil || len(legacy.grayscale) != 4 {
			t.Fatal("volume decoder changed precision contract")
		}
		cache.EvictNonPinned()
		if cache.Stats().DecodedBytes != 0 {
			t.Fatal("precision plane not evicted")
		}
	}
}

func TestQuantitativeCachePinnedOversizeAccountsEightBytesPerPixel(t *testing.T) {
	frame := testRenderFrame([]byte{1, 1, 1, 1})
	frame.PreserveModalityPrecision = true
	frame.Rescale = Rescale{Slope: .001}
	window := WindowLevel{Center: .001, Width: 1e-18, Function: display.VOILinearExact}
	cache := NewRenderCache(1)
	if _, e := cache.RenderFrame(frame, window); e != nil {
		t.Fatal(e)
	}
	if stats := cache.Stats(); stats.DecodedBytes != 0 || stats.WindowBytes != 0 {
		t.Fatalf("unpinned oversized plane admitted: %+v", stats)
	}
	cache.SetPinnedFrames([]PinnedFrame{{Frame: frame, Window: window}})
	if _, e := cache.RenderFrame(frame, window); e != nil {
		t.Fatal(e)
	}
	stats := cache.Stats()
	if stats.DecodedBytes != 32 || stats.WindowBytes != 4 || stats.PinnedBytes != 36 {
		t.Fatalf("8N pinned accounting: %+v", stats)
	}
	if freed := cache.EvictNonPinned(); freed != 0 {
		t.Fatalf("evicted pinned precision plane: %d", freed)
	}
	cache.SetPinnedFrames(nil)
	stats = cache.Stats()
	if stats.DecodedBytes != 0 || stats.WindowBytes != 0 || stats.PinnedBytes != 0 {
		t.Fatalf("oversized plane survived unpin: %+v", stats)
	}
}
