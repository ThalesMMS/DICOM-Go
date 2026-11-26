package render

import (
	"errors"
	"testing"
)

func TestRenderFrameValuesMatchesThePlainRenderForTheFrameValues(t *testing.T) {
	stack := bentTubeStack()
	frame := stack.Frames[4]
	window := WindowLevel{Center: 120, Width: 240}
	values, err := FrameModalityValues(frame)
	if err != nil {
		t.Fatal(err)
	}
	cache := NewRenderCache(1 << 20)
	plain, err := cache.RenderFrame(frame, window)
	if err != nil {
		t.Fatal(err)
	}
	same, err := cache.RenderFrameValues(frame, window, values)
	if err != nil {
		t.Fatal(err)
	}
	for i := range same.Pix {
		r, _, _, _ := plain.At(i%same.Rect.Dx(), i/same.Rect.Dx()).RGBA()
		if uint8(r>>8) != same.Pix[i] {
			t.Fatalf("pixel %d = %d, plain %d", i, same.Pix[i], r>>8)
		}
	}
	shifted := append([]float32(nil), values...)
	for i := range shifted {
		shifted[i] = 240
	}
	bright, err := cache.RenderFrameValues(frame, window, shifted)
	if err != nil || bright.Pix[0] != 255 {
		t.Fatalf("replaced values: %v %v", bright.Pix[0], err)
	}
	if _, err := cache.RenderFrameValues(frame, window, values[:3]); err == nil {
		t.Fatal("a short value slice was accepted")
	}
	if _, err := cache.RenderFrameValues(nil, window, values); err == nil || errors.Is(err, ErrNotGrayscaleFrame) {
		t.Fatalf("nil frame: %v", err)
	}
}
