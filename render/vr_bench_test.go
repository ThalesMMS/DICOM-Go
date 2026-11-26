package render

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"testing"
)

var benchVRImage image.Image

func BenchmarkRenderVRRaycast(b *testing.B) {
	vol, err := BuildVolume(sphereStack(48, 16))
	if err != nil {
		b.Fatal(err)
	}
	cam := NewVRCamera(vol.BoundingRadiusMM())
	cam.FitVolume(vol)
	preset := opaqueAbovePreset()
	window := WindowLevel{Center: 128, Width: 256}
	quality := DefaultVRQuality(64, 64)
	benchVRImage = RenderVR(vol, cam, preset, window, true, nil, quality) // warm lazy volume caches

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchVRImage = RenderVR(vol, cam, preset, window, true, nil, quality)
	}
}

func BenchmarkRenderVRRaycastColdCache(b *testing.B) {
	stack := sphereStack(48, 16)
	baseVol, err := BuildVolume(stack)
	if err != nil {
		b.Fatal(err)
	}
	cam := NewVRCamera(baseVol.BoundingRadiusMM())
	cam.FitVolume(baseVol)
	preset := opaqueAbovePreset()
	window := WindowLevel{Center: 128, Width: 256}
	quality := DefaultVRQuality(64, 64)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		vol, err := BuildVolume(stack)
		if err != nil {
			b.Fatal(err)
		}
		benchVRImage = RenderVR(vol, cam, preset, window, true, nil, quality)
	}
}

func BenchmarkRenderVROrbitPreview(b *testing.B) {
	vol, err := BuildVolume(sphereStack(48, 16))
	if err != nil {
		b.Fatal(err)
	}
	cam := NewVRCamera(vol.BoundingRadiusMM())
	cam.FitVolume(vol)
	preset := opaqueAbovePreset()
	window := WindowLevel{Center: 128, Width: 256}
	quality := PreviewVRQuality(48, 48)
	benchVRImage = RenderVR(vol, cam, preset, window, true, nil, quality) // warm lazy volume caches

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		frameCam := cam
		frameCam.Rotate(float64((i%6)-3)*8, float64((i%4)-2)*3)
		benchVRImage = RenderVR(vol, frameCam, preset, window, true, nil, quality)
	}
}

// unbalancedVRScene frames a sphere volume so the whole volume box sits in the
// top ~40% of the viewport and the remaining rows miss it. A static row split
// hands every hit row to a few workers while the others idle; issue #1061
// measures dynamic row scheduling against this layout.
func unbalancedVRScene(tb testing.TB) (*Volume, VRCamera, WindowLevel) {
	tb.Helper()
	vol, err := BuildVolume(sphereStack(64, 30))
	if err != nil {
		tb.Fatal(err)
	}
	cam := NewVRCamera(vol.BoundingRadiusMM())
	cam.FitVolume(vol)
	cam.Zoom(0.5)
	cam.Pan(0, -170)
	return vol, cam, WindowLevel{Center: 128, Width: 256}
}

// translucentAbovePreset keeps every ray through the sphere marching the whole
// volume (no early ray termination), so the hit rows dominate the frame cost.
func translucentAbovePreset() VRPreset {
	tf := NewVRTransferFunction(
		[]TFColorStop{{HU: 0, R: 1, G: 0.9, B: 0.8}, {HU: 255, R: 1, G: 0.9, B: 0.8}},
		[]TFAlphaStop{{HU: 0, A: 0}, {HU: 127, A: 0}, {HU: 128, A: 0.01}, {HU: 255, A: 0.01}},
	)
	return VRPreset{Name: "translucent", TF: tf, Mode: VRModeDVR, GradientOpacityScale: 1}
}

// BenchmarkRenderVRUnbalanced renders DVR, MIP and the iso-surface of a scene
// whose object covers only the top rows of a 256x256 viewport (#1061).
func BenchmarkRenderVRUnbalanced(b *testing.B) {
	vol, cam, window := unbalancedVRScene(b)
	quality := DefaultVRQuality(256, 256)
	surfaceColor := color.NRGBA{R: 220, G: 200, B: 160, A: 255}
	for _, c := range []struct {
		name   string
		render func() image.Image
	}{
		{"dvr", func() image.Image {
			return RenderVR(vol, cam, translucentAbovePreset(), window, true, nil, quality)
		}},
		{"mip", func() image.Image {
			return RenderVR(vol, cam, VRPreset{Name: "mip", Mode: VRModeMIP}, window, false, nil, quality)
		}},
		{"surface", func() image.Image {
			return RenderVRSurface(vol, cam, 128, surfaceColor, true, nil, quality)
		}},
	} {
		b.Run(c.name, func(b *testing.B) {
			benchVRImage = c.render() // warm lazy volume caches
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				benchVRImage = c.render()
			}
		})
	}
}

// BenchmarkRenderVRClipped renders with a crop box and a clip plane, marching
// only each ray's clipped interval ("interval") or the whole ray with
// per-sample rejection ("full", the renderer before #1002).
func BenchmarkRenderVRClipped(b *testing.B) {
	vol, err := BuildVolume(sphereStack(96, 32))
	if err != nil {
		b.Fatal(err)
	}
	cam := NewVRCamera(vol.BoundingRadiusMM())
	cam.FitVolume(vol)
	window := WindowLevel{Center: 128, Width: 256}
	quality := DefaultVRQuality(128, 128)
	clip := NewVRClip()
	clip.SetCropBox(Vec3{X: 0.25, Y: 0.25, Z: 0.25}, Vec3{X: 0.75, Y: 0.75, Z: 0.75})
	clip.AddClipPlane(Vec3{X: 0.6, Y: 0.8, Z: 0}, 0.8)
	for _, mode := range []VRMode{VRModeDVR, VRModeMIP} {
		preset := opaqueAbovePreset()
		preset.Mode = mode
		for _, variant := range []struct {
			name string
			clip *VRClip
		}{{"interval", clip}, {"full", fullMarch(clip)}} {
			b.Run(fmt.Sprintf("mode%d/%s", mode, variant.name), func(b *testing.B) {
				benchVRImage = RenderVR(vol, cam, preset, window, true, variant.clip, quality)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					benchVRImage = RenderVR(vol, cam, preset, window, true, variant.clip, quality)
				}
			})
		}
	}
}

// BenchmarkRenderVRSpaceLeaping renders a 192x192x128 CT phantom (air around
// the body) with empty-space skipping ("leap") and with the full per-sample
// march ("full", the renderer before #1003).
func BenchmarkRenderVRSpaceLeaping(b *testing.B) {
	vol, err := BuildVolume(ctPhantomStack(192, 192, 128))
	if err != nil {
		b.Fatal(err)
	}
	cam := NewVRCamera(vol.BoundingRadiusMM())
	cam.FitVolume(vol)
	quality := DefaultVRQuality(160, 160)
	mip := mipBWPreset(PresetMIPBW, false)
	for _, c := range []struct {
		name   string
		preset VRPreset
		window WindowLevel
	}{
		{"bonesBW", bonesBWPreset(), WindowLevel{Center: 300, Width: 1500}},
		{"bonesSkin", DefaultVRPreset(), WindowLevel{Center: 40, Width: 400}},
		{"mip", mip, WindowLevel{Center: 300, Width: 1500}},
	} {
		for _, variant := range []struct {
			name string
			clip *VRClip
		}{{"leap", nil}, {"full", fullMarch(nil)}} {
			b.Run(c.name+"/"+variant.name, func(b *testing.B) {
				benchVRImage = RenderVR(vol, cam, c.preset, c.window, true, variant.clip, quality)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					benchVRImage = RenderVR(vol, cam, c.preset, c.window, true, variant.clip, quality)
				}
			})
		}
	}
}

// BenchmarkVRBlockGrid measures building the min/max grid of a 192x192x128
// volume, paid once per prepared volume on its first render.
func BenchmarkVRBlockGrid(b *testing.B) {
	vol, err := BuildVolume(ctPhantomStack(192, 192, 128))
	if err != nil {
		b.Fatal(err)
	}
	prepared, err := vol.PrepareVRVolume(VRPrefilterNone)
	if err != nil {
		b.Fatal(err)
	}
	sampler, err := prepared.newSampler()
	if err != nil {
		b.Fatal(err)
	}
	defer sampler.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := buildVRBlockGrid(context.Background(), sampler); err != nil {
			b.Fatal(err)
		}
	}
}
