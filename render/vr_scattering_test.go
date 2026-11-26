package render

import (
	"image"
	"math"
	"testing"
)

// shadowScene is a 32³ volume with a block (value 255) and optionally a wall
// between the block and a light on the -X side; the camera looks along +Y,
// so the wall is not on the view rays through the block.
func shadowScene(t *testing.T, wall bool) (*Volume, VRCamera, Vec3) {
	t.Helper()
	const n = 32
	stack := &Stack{PixelSpacing: []float64{1, 1}, SliceThickness: 1}
	for z := 0; z < n; z++ {
		data := make([]byte, n*n)
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				block := x >= 16 && x <= 27 && y >= 12 && y <= 20 && z >= 8 && z <= 24
				if block || (wall && x >= 4 && x <= 7) {
					data[y*n+x] = 255
				}
			}
		}
		stack.Frames = append(stack.Frames, volumeTestFrame(n, n, data, z))
	}
	vol, err := BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	cam := NewVRCamera(vol.BoundingRadiusMM())
	cam.Pitch = 0
	target := Vec3{X: 21.5 - 15.5, Y: 12 - 15.5, Z: 16 - 15.5}
	return vol, cam, target
}

func scatteringPreset() VRPreset {
	tf := NewVRTransferFunction(
		[]TFColorStop{{HU: 0, R: 1, G: 1, B: 1}, {HU: 255, R: 1, G: 1, B: 1}},
		[]TFAlphaStop{{HU: 0, A: 0}, {HU: 127, A: 0}, {HU: 128, A: 0.3}, {HU: 255, A: 0.3}},
	)
	return VRPreset{Name: "scatter", TF: tf, Mode: VRModeDVR, ShadingDefault: true}
}

// lightFromMinusX returns the key-light azimuth that puts the light on the
// -X side for the scene camera.
func lightFromMinusX(cam VRCamera) float64 {
	right, up, forward := cam.basis()
	for _, azimuth := range []float64{math.Pi / 2, -math.Pi / 2} {
		if VRScatteringLight(forward, right, up, azimuth, 0).X < -0.99 {
			return azimuth
		}
	}
	return math.NaN()
}

func TestVRScatteringCastsVolumetricShadows(t *testing.T) {
	window := WindowLevel{Center: 128, Width: 256}
	quality := DefaultVRQuality(48, 48)
	render := func(wall bool, scattering VRScattering, quality VRQuality) (image.Image, int) {
		vol, cam, target := shadowScene(t, wall)
		preset := scatteringPreset()
		if scattering.Enabled() {
			scattering.LightAzimuth = lightFromMinusX(cam)
		}
		preset.Scattering = scattering
		img := RenderVR(vol, cam, preset, window, true, nil, quality)
		px, py, visible := cam.Project(target, quality.Width, quality.Height)
		if !visible {
			t.Fatal("target not visible")
		}
		return img, vrBrightness(img.(*image.NRGBA), int(px), int(py))
	}
	on := VRScattering{Blend: 1}
	_, lit := render(false, on, quality)
	_, shadowed := render(true, on, quality)
	t.Logf("block brightness: open %d, behind the wall %d", lit, shadowed)
	if shadowed >= lit-30 {
		t.Fatalf("block behind the wall = %d, open = %d; want a volumetric shadow", shadowed, lit)
	}
	// Without scattering the wall, off the view rays, changes nothing there.
	_, plainOpen := render(false, VRScattering{}, quality)
	_, plainWall := render(true, VRScattering{}, quality)
	if plainOpen != plainWall {
		t.Fatalf("surface shading sees the wall: %d vs %d", plainWall, plainOpen)
	}
	// Interaction frames skip scattering.
	preview := PreviewVRQuality(48, 48)
	a, _ := render(true, on, preview)
	b, _ := render(true, VRScattering{}, preview)
	if !sameImage(a, b) {
		t.Fatal("preview quality rendered scattering")
	}
}

func TestVRScatteringModelFunctions(t *testing.T) {
	if HenyeyGreenstein(0, 0.3) != 1 {
		t.Fatal("isotropic phase is not 1")
	}
	// Forward scattering peaks when the light is behind the sample.
	if !(HenyeyGreenstein(0.6, 1) > HenyeyGreenstein(0.6, -1)) {
		t.Fatal("forward-scattering phase does not peak forward")
	}
	if got := VRScatteringCoefficient(0.5, 0, 0.5); math.Abs(got-1) > 1e-12 {
		t.Fatalf("blend 0.5 in homogeneous tissue = %g, want 1", got)
	}
	if got := VRScatteringCoefficient(2, 0, 1); math.Abs(got-1) > 1e-12 {
		t.Fatalf("blend 2 without gradient = %g, want 1", got)
	}
	if !(VRScatteringCoefficient(0.5, 10, 1) < 0.01) {
		t.Fatal("strong surface gradient keeps the volumetric model")
	}
	light := VRScatteringLight(Vec3{Z: 1}, Vec3{X: 1}, Vec3{Y: 1}, 0, 0)
	if light != (Vec3{Z: -1}) {
		t.Fatalf("headlight direction = %v", light)
	}
	s := VRScattering{Blend: 3, Reach: 2, Anisotropy: 5}.Normalized()
	if s.Blend != 2 || s.Reach != 1 || s.Anisotropy != 0.99 {
		t.Fatalf("normalized = %+v", s)
	}
}

// BenchmarkRenderVRScattering measures the settled-frame cost of scattering
// on a 128³ volume at 256² (the light attenuation volume is 64³ shadow rays
// of up to 64 samples).
func BenchmarkRenderVRScattering(b *testing.B) {
	vol, err := BuildVolume(sphereStack(128, 50))
	if err != nil {
		b.Fatal(err)
	}
	cam := NewVRCamera(vol.BoundingRadiusMM())
	quality := DefaultVRQuality(256, 256)
	window := WindowLevel{Center: 128, Width: 256}
	for _, test := range []struct {
		name       string
		scattering VRScattering
	}{
		{"off", VRScattering{}},
		{"on", VRScattering{Blend: 1, LightAzimuth: 0.5, LightElevation: 0.5}},
	} {
		preset := scatteringPreset()
		preset.Scattering = test.scattering
		b.Run(test.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				RenderVR(vol, cam, preset, window, true, nil, quality)
			}
		})
	}
}
