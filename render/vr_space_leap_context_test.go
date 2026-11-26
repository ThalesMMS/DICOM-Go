package render

import (
	"bytes"
	"context"
	"errors"
	"image"
	"math"
	"testing"
)

func TestSpaceLeapLabelBoundaryOpacityAndOnlyInside(t *testing.T) {
	const n = 25
	labels := make([]uint16, n*n*n)
	labels[(8*n+8)*n+8] = 300
	styles := make([]VRLabelStyle, 301)
	styles[300] = labelRed
	mask, err := NewVRLabelMap(n, n, n, labels, styles, false)
	if err != nil {
		t.Fatal(err)
	}
	if mask.StorageBits() != 16 {
		t.Fatal("fixture lost 16-bit label identity")
	}
	nonempty := 0
	for _, flag := range mask.blockFlags {
		if flag&vrBlockLabelOpacity != 0 {
			nonempty++
		}
	}
	if nonempty != 8 {
		t.Fatal("nearest-neighbour boundary occupancy did not cover eight blocks", nonempty)
	}
	for _, inside := range []bool{false, true} {
		volume, camera, clip := labelTestScene(t, func(x, y, z int) uint16 {
			if x >= 8 && x < 11 && y >= 8 && y < 11 && z >= 8 && z < 11 {
				return 300
			}
			return 0
		}, styles, inside)
		defer volume.Close()
		camera.FitVolume(volume)
		camera.Rotate(17, -9)
		quality := DefaultVRQuality(41, 37)
		quality.Jitter = true
		for _, preset := range []VRPreset{transparentPreset(), opaqueAbovePreset()} {
			got := RenderVR(volume, camera, preset, WindowLevel{Center: 128, Width: 256}, true, clip, quality).(*image.NRGBA)
			want := RenderVR(volume, camera, preset, WindowLevel{Center: 128, Width: 256}, true, fullMarch(clip), quality).(*image.NRGBA)
			if !bytes.Equal(got.Pix, want.Pix) {
				t.Fatal("label occupancy changed pixels", inside)
			}
		}
	}
}

func TestSpaceLeapFusionKeepsImageAndOverlapAndReducesSampling(t *testing.T) {
	primary := fusionCTVolume(t)
	prepared, err := primary.PrepareVRVolume(VRPrefilterNone)
	if err != nil {
		t.Fatal(err)
	}
	reduced := false
	for _, angle := range []float64{0, 27, 90} {
		secondary := fusionPETVolume(t, angle, 1.4, 2.1, fusionHotSpot)
		for _, projection := range []VRCameraProjection{VRProjectionPerspective, VRProjectionParallel} {
			camera := fusionCamera(primary, projection, 13, -8)
			quality := DefaultVRQuality(57, 43)
			quality.Jitter = true
			for _, clip := range []*VRClip{nil, func() *VRClip {
				c := NewVRClip()
				c.SetCropBox(Vec3{X: .1, Y: .1, Z: .1}, Vec3{X: .9, Y: .9, Z: .9})
				c.SetCapOpacity(.7)
				return c
			}()} {
				layer := fusionLayer(secondary)
				got, actual, err := RenderFusedVRPreparedContext(context.Background(), prepared, layer, camera, transparentPreset(), fusionWindow(), false, clip, quality)
				want, full, referenceErr := RenderFusedVRPreparedContext(context.Background(), prepared, layer, camera, transparentPreset(), fusionWindow(), false, fullMarch(clip), quality)
				if (err == nil) != (referenceErr == nil) || !bytes.Equal(got.(*image.NRGBA).Pix, want.(*image.NRGBA).Pix) {
					t.Fatal("fusion grid changed image or overlap", angle, projection, err, referenceErr)
				}
				if actual.SecondarySamples < full.SecondarySamples {
					reduced = true
				}
				t.Logf("angle %.0f projection %v crop %v secondary samples %d/%d", angle, projection, clip != nil, actual.SecondarySamples, full.SecondarySamples)
			}
		}
	}
	if !reduced {
		t.Fatal("fusion did not avoid secondary sampling")
	}
	// A transparent overlapping overlay must not become a no-overlap error.
	layer := fusionLayer(fusionPETVolume(t, 0, 1, 1, Vec3{X: 500, Y: 500, Z: 500}))
	camera := fusionCamera(primary, VRProjectionPerspective, 0, 0)
	_, report, err := RenderFusedVRPreparedContext(context.Background(), prepared, layer, camera, transparentPreset(), fusionWindow(), false, nil, DefaultVRQuality(39, 35))
	if errors.Is(err, ErrFusionNoOverlap) || report.SecondaryInBounds == 0 {
		t.Fatal("transparent overlap was lost", report, err)
	}
	// Unknown callback interiors retain full marching and callback failures.
	layer.PrimaryToSecondary = func(point Vec3) (Vec3, error) { return point, nil }
	got, _, err := RenderFusedVRPreparedContext(context.Background(), prepared, layer, camera, transparentPreset(), fusionWindow(), false, nil, DefaultVRQuality(39, 35))
	want, _, referenceErr := RenderFusedVRPreparedContext(context.Background(), prepared, layer, camera, transparentPreset(), fusionWindow(), false, fullMarch(nil), DefaultVRQuality(39, 35))
	if (err == nil) != (referenceErr == nil) || !bytes.Equal(got.(*image.NRGBA).Pix, want.(*image.NRGBA).Pix) {
		t.Fatal("unbounded callback selected a corner-only shortcut")
	}
}

func TestSpaceLeapFusionLargePatientOriginAndEmptyLabelClip(t *testing.T) {
	shift := Vec3{X: 1e5, Y: -1e5, Z: 1e5}
	primary, err := BuildVolume(buildFusionStack(fusionGrid{cols: 33, rows: 33, depth: 33, pixelSpacing: .8, spacing: 1.3, axisX: Vec3{X: 1}, axisY: Vec3{Y: 1}, normal: Vec3{Z: 1}, origin: shift}, func(Vec3) byte { return 0 }))
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	prepared, err := primary.PrepareVRVolume(VRPrefilterNone)
	if err != nil {
		t.Fatal(err)
	}
	angle := math.Pi / 6
	secondary, err := BuildVolume(buildFusionStack(fusionGrid{cols: 49, rows: 49, depth: 37, pixelSpacing: .7, spacing: 1.1, axisX: Vec3{X: math.Cos(angle), Y: math.Sin(angle)}, axisY: Vec3{X: -math.Sin(angle), Y: math.Cos(angle)}, normal: Vec3{Z: 1}, origin: shift.Add(Vec3{X: -5, Y: -10, Z: -6})}, func(point Vec3) byte {
		if point.Sub(shift.Add(Vec3{X: 12, Y: 12, Z: 16})).Length() < 2 {
			return 255
		}
		return 0
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer secondary.Close()
	camera := fusionCamera(primary, VRProjectionPerspective, 17, -13)
	quality := DefaultVRQuality(43, 37)
	quality.Jitter = true
	labels, err := NewVRLabelMap(primary.Cols, primary.Rows, primary.Depth, make([]uint16, primary.Cols*primary.Rows*primary.Depth), []VRLabelStyle{{}}, true)
	if err != nil {
		t.Fatal(err)
	}
	emptyClip := NewVRClip()
	emptyClip.SetLabels(labels)
	for _, clip := range []*VRClip{nil, emptyClip} {
		got, _, err := RenderFusedVRPreparedContext(context.Background(), prepared, fusionLayer(secondary), camera, transparentPreset(), fusionWindow(), false, clip, quality)
		want, _, referenceErr := RenderFusedVRPreparedContext(context.Background(), prepared, fusionLayer(secondary), camera, transparentPreset(), fusionWindow(), false, fullMarch(clip), quality)
		if (err == nil) != (referenceErr == nil) || !bytes.Equal(got.(*image.NRGBA).Pix, want.(*image.NRGBA).Pix) {
			t.Fatal("large patient origin or empty label clip changed image/overlap", err, referenceErr)
		}
	}
}
