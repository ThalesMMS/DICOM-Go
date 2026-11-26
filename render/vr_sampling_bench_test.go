package render

import (
	"context"
	"image"
	"image/color"
	"testing"
)

// BenchmarkVRPhysicalSampling compares explicit legacy distances with the
// new physical default on one frozen synthetic fixture. Neither variant
// changes the iteration cap; these timings are not the telemetry campaign.
func BenchmarkVRPhysicalSampling(b *testing.B) {
	vol, err := BuildVolume(ctPhantomStack(192, 192, 128))
	if err != nil {
		b.Fatal(err)
	}
	defer vol.Close()
	cam := NewVRCamera(vol.BoundingRadiusMM())
	cam.FitVolume(vol)
	cam.Rotate(18, 11)
	window := WindowLevel{Center: 40, Width: 400}
	for _, mode := range []string{"DVR", "MIP", "surface"} {
		for _, sampling := range []string{"legacy-distance", "physical-half-voxel"} {
			b.Run(mode+"/"+sampling, func(b *testing.B) {
				quality := DefaultVRQuality(128, 128)
				if sampling == "legacy-distance" {
					count := 512.0
					if mode == "surface" {
						count = 192
					}
					quality.SpacingFraction = vol.PhysicalSize().Length() / (count * vol.ColSpacing)
				}
				plan, err := volumeVRRaySampling(vol, quality)
				if err != nil {
					b.Fatal(err)
				}
				preset := DefaultVRPreset()
				if mode == "MIP" {
					preset.Mode = VRModeMIP
				}
				render := func() (image.Image, error) {
					if mode == "surface" {
						return RenderVRSurfaceContext(context.Background(), vol, cam, 300, color.NRGBA{R: 224, G: 204, B: 168, A: 255}, true, nil, quality)
					}
					return RenderVRContext(context.Background(), vol, cam, preset, window, true, nil, quality)
				}
				if _, err := render(); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					var err error
					benchVRImage, err = render()
					if err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(plan.SampleDistanceMM, "mm/sample")
				b.ReportMetric(float64(plan.RequiredSteps), "max-ray-iterations")
			})
		}
	}
}
