package render

import (
	"image"
	"image/color"
	"testing"
)

// cutPlaneScene is a 24³ volume of 100 (one corner voxel of 0 sets the data
// minimum) seen head-on along +Y, the direction a cut plane at y = 0.5 faces.
func cutPlaneScene(t *testing.T) (*Volume, VRCamera) {
	t.Helper()
	vol, err := BuildVolume(stack16(24, true, func(int) Rescale { return Rescale{Slope: 1} }, func(x, y, z int) int {
		if x == 0 && y == 0 && z == 0 {
			return 0
		}
		return 100
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vol.Close() })
	cam := NewVRCamera(vol.BoundingRadiusMM())
	cam.Pitch = 0
	return vol, cam
}

func cutPlanePreset(alpha float64) VRPreset {
	return VRPreset{
		Mode: VRModeDVR,
		TF: NewVRTransferFunction(
			[]TFColorStop{{HU: -1000, R: 1, G: 1, B: 1}, {HU: 3000, R: 1, G: 1, B: 1}},
			[]TFAlphaStop{{HU: -1000, A: alpha}, {HU: 3000, A: alpha}},
		),
	}
}

func centreGray(img image.Image) uint8 {
	b := img.Bounds()
	return img.(*image.NRGBA).NRGBAAt(b.Dx()/2, b.Dy()/2).R
}

// TestCutPlaneCapShowsTheSection (#1067): a ray entering through a cut
// plane's face composites the section's windowed gray first, as the MPR
// shows it; plain clip planes, rays entering through the volume's faces and a
// zero cap opacity draw no cap.
func TestCutPlaneCapShowsTheSection(t *testing.T) {
	vol, cam := cutPlaneScene(t)
	window := WindowLevel{Center: 100, Width: 200} // 100 is mid-gray
	quality := VRQuality{Width: 33, Height: 33, MaxSteps: DefaultVRMaxSteps}
	transparent := cutPlanePreset(0)
	render := func(configure func(*VRClip)) uint8 {
		clip := NewVRClip()
		configure(clip)
		return centreGray(RenderVR(vol, cam, transparent, window, false, clip, quality))
	}
	// Keep the far half, y >= 0.5: -y + 0.5 <= 0.
	if got := render(func(c *VRClip) { c.AddCutPlane(Vec3{Y: -1}, -0.5); c.SetCapOpacity(1) }); got < 126 || got > 129 {
		t.Fatalf("opaque cap = %d, want mid-gray 128", got)
	}
	half := uint8(to8(linearToSRGB(0.5 * srgbToLinear(0.5))))
	if got := render(func(c *VRClip) { c.AddCutPlane(Vec3{Y: -1}, -0.5); c.SetCapOpacity(0.5) }); got < half-1 || got > half+1 {
		t.Fatalf("half cap = %d, want %d", got, half)
	}
	// A cap window of its own (the MPR's) maps 100 to white.
	if got := render(func(c *VRClip) {
		c.AddCutPlane(Vec3{Y: -1}, -0.5)
		c.SetCapOpacity(1)
		c.SetCapWindow(WindowLevel{Center: 50, Width: 100})
	}); got != 255 {
		t.Fatalf("cap with its own window = %d, want white", got)
	}
	for name, configure := range map[string]func(*VRClip){
		"plain clip plane": func(c *VRClip) { c.AddClipPlane(Vec3{Y: -1}, -0.5); c.SetCapOpacity(1) },
		"zero opacity":     func(c *VRClip) { c.AddCutPlane(Vec3{Y: -1}, -0.5) },
		// Keeping the near half, rays enter through the volume's face.
		"entry through the volume": func(c *VRClip) { c.AddCutPlane(Vec3{Y: 1}, 0.5); c.SetCapOpacity(1) },
	} {
		if got := render(configure); got != 0 {
			t.Fatalf("%s drew a cap: %d", name, got)
		}
	}
	// The cap follows the full per-sample march.
	clip := NewVRClip()
	clip.AddCutPlane(Vec3{Y: -1}, -0.5)
	clip.SetCapOpacity(1)
	narrowed := RenderVR(vol, cam, transparent, window, false, clip, quality)
	full := RenderVR(vol, cam, transparent, window, false, fullMarch(clip), quality)
	if !sameImage(narrowed, full) {
		t.Fatal("cap differs between the narrowed and the full march")
	}
}

// TestCutPlaneLightsItsFace: a uniform opaque volume has no gradient, so a
// plain clip plane leaves its cut face at ambient light; a cut plane reads
// the clipped side as the data minimum and lights the face as a surface
// facing the viewer (VTK's UseClippedVoxelIntensity).
func TestCutPlaneLightsItsFace(t *testing.T) {
	vol, cam := cutPlaneScene(t)
	window := WindowLevel{Center: 100, Width: 200}
	quality := VRQuality{Width: 33, Height: 33, MaxSteps: DefaultVRMaxSteps}
	opaque := cutPlanePreset(1)
	plain := NewVRClip()
	plain.AddClipPlane(Vec3{Y: -1}, -0.5)
	cut := NewVRClip()
	cut.AddCutPlane(Vec3{Y: -1}, -0.5)
	flat := centreGray(RenderVR(vol, cam, opaque, window, true, plain, quality))
	lit := centreGray(RenderVR(vol, cam, opaque, window, true, cut, quality))
	if lit <= flat+40 {
		t.Fatalf("cut face %d not lit above the flat clip face %d", lit, flat)
	}
	surfacePlain := centreGray(RenderVRSurface(vol, cam, 50, surfaceWhite, true, plain, quality))
	surfaceCut := centreGray(RenderVRSurface(vol, cam, 50, surfaceWhite, true, cut, quality))
	if surfaceCut <= surfacePlain+40 {
		t.Fatalf("surface cut face %d not lit above the flat clip face %d", surfaceCut, surfacePlain)
	}
}

var surfaceWhite = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
