package render

import (
	"errors"
	"image"
	"math"
	"testing"
)

// labelTestScene is a 24³ sphere (value 255 inside radius 8) seen along +Y
// (screen right = +X) with labels from label(x, y, z).
func labelTestScene(t *testing.T, label func(x, y, z int) uint16, styles []VRLabelStyle, onlyInside bool) (*Volume, VRCamera, *VRClip) {
	t.Helper()
	vol, err := BuildVolume(sphereStack(24, 8))
	if err != nil {
		t.Fatal(err)
	}
	labels := make([]uint16, vol.Cols*vol.Rows*vol.Depth)
	for z := 0; z < vol.Depth; z++ {
		for y := 0; y < vol.Rows; y++ {
			for x := 0; x < vol.Cols; x++ {
				labels[(z*vol.Rows+y)*vol.Cols+x] = label(x, y, z)
			}
		}
	}
	m, err := NewVRLabelMap(vol.Cols, vol.Rows, vol.Depth, labels, styles, onlyInside)
	if err != nil {
		t.Fatal(err)
	}
	clip := NewVRClip()
	clip.SetLabels(m)
	cam := NewVRCamera(vol.BoundingRadiusMM())
	cam.Pitch = 0
	return vol, cam, clip
}

func transparentPreset() VRPreset {
	tf := NewVRTransferFunction(
		[]TFColorStop{{HU: 0, R: 1, G: 1, B: 1}, {HU: 255, R: 1, G: 1, B: 1}},
		[]TFAlphaStop{{HU: 0, A: 0}, {HU: 255, A: 0}},
	)
	return VRPreset{Name: "transparent", TF: tf, Mode: VRModeDVR}
}

var (
	labelRed   = VRLabelStyle{Color: RGBA{R: 1}, Opacity: 1, Blend: 1, Visible: true}
	labelGreen = VRLabelStyle{Color: RGBA{G: 1}, Opacity: 1, Blend: 1, Visible: true}
)

func centrePixel(img image.Image) (r, g, b uint8) {
	c := img.(*image.NRGBA).NRGBAAt(img.Bounds().Dx()/2, img.Bounds().Dy()/2)
	return c.R, c.G, c.B
}

// TestVRLabelsRenderSegmentsWithOcclusion: with a transparent transfer
// function only the labels are drawn, and the nearer segment hides the one
// behind it from either side.
func TestVRLabelsRenderSegmentsWithOcclusion(t *testing.T) {
	vol, cam, clip := labelTestScene(t, func(x, y, z int) uint16 {
		if y < 12 {
			return 1
		}
		return 2
	}, []VRLabelStyle{{}, labelRed, labelGreen}, false)
	window := WindowLevel{Center: 128, Width: 256}
	quality := DefaultVRQuality(32, 32)
	if r, g, b := centrePixel(RenderVR(vol, cam, transparentPreset(), window, false, nil, quality)); r != 0 || g != 0 || b != 0 {
		t.Fatalf("transparent volume without labels = %d,%d,%d, want background", r, g, b)
	}
	if r, g, b := centrePixel(RenderVR(vol, cam, transparentPreset(), window, false, clip, quality)); r != 255 || g != 0 || b != 0 {
		t.Fatalf("front segment seen along +Y = %d,%d,%d, want red", r, g, b)
	}
	cam.Yaw = math.Pi
	if r, g, b := centrePixel(RenderVR(vol, cam, transparentPreset(), window, false, clip, quality)); r != 0 || g != 255 || b != 0 {
		t.Fatalf("front segment seen along -Y = %d,%d,%d, want green", r, g, b)
	}
	// Hiding the green segment shows the red one behind it.
	hidden := labelGreen
	hidden.Visible = false
	_, _, clip = labelTestScene(t, func(x, y, z int) uint16 {
		if y < 12 {
			return 1
		}
		return 2
	}, []VRLabelStyle{{}, labelRed, hidden}, false)
	if r, g, b := centrePixel(RenderVR(vol, cam, transparentPreset(), window, false, clip, quality)); r != 255 || g != 0 || b != 0 {
		t.Fatalf("with the front segment hidden = %d,%d,%d, want red", r, g, b)
	}
}

// TestVRLabelsOnlyInsideRestrictsDVRAndMIP keeps only the left half of the
// sphere (label 1) in DVR and MIP.
func TestVRLabelsOnlyInsideRestrictsDVRAndMIP(t *testing.T) {
	half := func(x, y, z int) uint16 {
		if x < 12 {
			return 1
		}
		return 0
	}
	keepTF := VRLabelStyle{Visible: true} // blend 0: the transfer function's colour
	vol, cam, clip := labelTestScene(t, half, []VRLabelStyle{{}, keepTF}, true)
	window := WindowLevel{Center: 128, Width: 256}
	quality := DefaultVRQuality(32, 32)
	for _, preset := range []VRPreset{opaqueAbovePreset(), {Name: "mip", Mode: VRModeMIP}} {
		full := RenderVR(vol, cam, preset, window, false, nil, quality).(*image.NRGBA)
		inside := RenderVR(vol, cam, preset, window, false, clip, quality).(*image.NRGBA)
		// Columns over the two halves of the sphere on the middle row.
		first, last := -1, -1
		for x := 0; x < 32; x++ {
			if vrBrightness(full, x, 16) > 0 {
				if first < 0 {
					first = x
				}
				last = x
			}
		}
		left, right := first+(last-first)/4, last-(last-first)/4
		if first < 0 || right-left < 2 || vrBrightness(full, left, 16) == 0 || vrBrightness(full, right, 16) == 0 {
			t.Fatalf("%s: sphere not rendered on both sides", preset.Name)
		}
		if vrBrightness(inside, left, 16) != vrBrightness(full, left, 16) {
			t.Fatalf("%s: labelled half changed: %d vs %d", preset.Name, vrBrightness(inside, left, 16), vrBrightness(full, left, 16))
		}
		if vrBrightness(inside, right, 16) != 0 {
			t.Fatalf("%s: unlabelled half rendered in only-inside mode", preset.Name)
		}
	}
}

// TestVRLabelsBlendAndNoOp: blend 0.5 mixes colours, and a map with no
// visible labels leaves the image unchanged.
func TestVRLabelsBlendAndNoOp(t *testing.T) {
	window := WindowLevel{Center: 128, Width: 256}
	quality := DefaultVRQuality(32, 32)
	vol, cam, clip := labelTestScene(t, func(int, int, int) uint16 { return 0 }, []VRLabelStyle{{}, labelRed}, false)
	if !sameImage(RenderVR(vol, cam, opaqueAbovePreset(), window, true, nil, quality), RenderVR(vol, cam, opaqueAbovePreset(), window, true, clip, quality)) {
		t.Fatal("unlabelled volume changed the image")
	}
	halfRed := labelRed
	halfRed.Blend = 0.5
	_, _, clip = labelTestScene(t, func(int, int, int) uint16 { return 1 }, []VRLabelStyle{{}, halfRed}, false)
	r, g, b := centrePixel(RenderVR(vol, cam, opaqueAbovePreset(), window, false, clip, quality))
	// Linear 0.5 of white and red is (1, 0.5, 0.5), sRGB 255,188,188, less
	// the background left by early ray termination at 95%.
	if r < 240 || g < 175 || g > 195 || g != b {
		t.Fatalf("half-blended red on white = %d,%d,%d, want about 255,188,188", r, g, b)
	}
}

func TestNewVRLabelMapValidates(t *testing.T) {
	if _, err := NewVRLabelMap(2, 2, 2, make([]uint8, 7), nil, false); !errors.Is(err, ErrInvalidVRLabelMap) {
		t.Fatalf("short label volume error = %v", err)
	}
	if _, err := NewVRLabelMap(1, 1, 1, []uint16{1}, []VRLabelStyle{{}, {Opacity: 2}}, false); !errors.Is(err, ErrInvalidVRLabelMap) {
		t.Fatalf("opacity 2 error = %v", err)
	}
	m, err := NewVRLabelMap(1, 1, 2, []uint16{3, 300}, []VRLabelStyle{{}, labelRed}, false)
	if err != nil || m.MaxLabel() != 300 || m.StorageBits() != 16 || m.LabelAt(0, 0, 1) != 300 ||
		string(m.LabelBytes()) != string([]byte{3, 0, 44, 1}) {
		t.Fatalf("NewVRLabelMap = %v, %v", m, err)
	}
	small, err := NewVRLabelMap(1, 1, 2, []uint16{3, 200}, nil, false)
	if err != nil || small.StorageBits() != 8 || small.LabelAt(0, 0, 1) != 200 || small.LabelAt(0, 0, 2) != 0 ||
		string(small.LabelBytes()) != string([]byte{3, 200}) {
		t.Fatalf("8-bit NewVRLabelMap = %v, %v", small, err)
	}
	// Labels without a style are hidden.
	if _, ok := m.visibleStyle(300); ok {
		t.Fatal("label without style is visible")
	}
}
