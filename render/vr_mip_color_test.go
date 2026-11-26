package render

import (
	"context"
	"image"
	"image/color"
	"testing"
)

func TestColoredMIPSelectsRawMaximumBeyondWindowAndIgnoresOpacity(t *testing.T) {
	values := []int{-100, 80, 200, 500, 500}
	volume, err := BuildVolume(projectionPhantomStack(9, 9, 5, Vec3{X: 1, Y: 1, Z: 2}, func(x, y, z int) int { return values[z] }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = volume.Close() })
	camera := NewVRCamera(volume.BoundingRadiusMM())
	camera.FitVolume(volume)
	camera.SetOrientation(Vec3{Z: -1})
	camera.Projection = VRProjectionParallel
	preset := DefaultVRPreset()
	preset.Mode, preset.ColoredMIP = VRModeMIP, true
	preset.TF = NewVRTransferFunction([]TFColorStop{{HU: -100, R: 1}, {HU: 80, R: 1}, {HU: 200, G: 1}, {HU: 500, B: 1}}, []TFAlphaStop{{HU: -100}, {HU: 500}})
	window := WindowLevel{Center: 50, Width: 100}
	quality := VRQuality{Width: 31, Height: 31, MaxSteps: DefaultVRMaxSteps}
	for _, inverse := range []bool{false, true} {
		preset.Inverse = inverse
		frame, err := RenderVRContext(context.Background(), volume, camera, preset, window, true, nil, quality)
		if err != nil {
			t.Fatal(err)
		}
		got := frame.(*image.NRGBA).NRGBAAt(15, 15)
		want := color.NRGBA{B: 255, A: 255}
		if inverse {
			want = color.NRGBA{R: 255, G: 255, A: 255}
		}
		if got != want {
			t.Fatalf("inverse%v: raw maximum mapped to %v, want %v", inverse, got, want)
		}
	}
	preset.ColoredMIP, preset.Inverse = false, false
	frame, err := RenderVRContext(context.Background(), volume, camera, preset, window, false, nil, quality)
	if err != nil {
		t.Fatal(err)
	}
	if got := frame.(*image.NRGBA).NRGBAAt(15, 15); got != (color.NRGBA{R: 255, G: 255, B: 255, A: 255}) {
		t.Fatal("grayscale MIP changed", got)
	}
}
