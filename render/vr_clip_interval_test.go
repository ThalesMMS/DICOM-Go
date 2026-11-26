package render

import (
	"bytes"
	"fmt"
	"image"
	"math"
	"math/rand"
	"testing"
)

// fullMarch returns a copy of clip that marches whole rays and rejects
// samples one by one (the renderer before rayInterval).
func fullMarch(clip *VRClip) *VRClip {
	if clip == nil {
		return &VRClip{fullRays: true}
	}
	full := *clip
	full.fullRays = true
	return &full
}

func testClips(vol *Volume) map[string]*VRClip {
	crop := NewVRClip()
	crop.SetCropBox(Vec3{X: 0.2, Y: 0.1, Z: 0.3}, Vec3{X: 0.7, Y: 0.8, Z: 0.65})
	thin := NewVRClip()
	thin.SetCropBox(Vec3{X: 0.5, Y: 0, Z: 0}, Vec3{X: 0.5, Y: 1, Z: 1})
	planes := NewVRClip()
	planes.AddClipPlane(Vec3{X: 0.6, Y: 0.8, Z: 0}, 0.9)
	planes.AddClipPlane(Vec3{X: 0, Y: -0.3, Z: 1}, 0.4)
	all := NewVRClip()
	all.SetCropBox(Vec3{X: 0.1, Y: 0.1, Z: 0.1}, Vec3{X: 0.9, Y: 0.9, Z: 0.9})
	all.AddClipPlane(Vec3{X: -1, Y: 0, Z: 0}, -0.25)
	cut := NewVRCutMask(vol)
	for x := 0; x < 20; x++ {
		for y := 0; y < 48; y++ {
			cut.Set(x, y, 10, true)
		}
	}
	all.SetCut(cut)
	nothing := NewVRClip()
	nothing.AddClipPlane(Vec3{X: 0, Y: 0, Z: 1}, -1)
	return map[string]*VRClip{"crop": crop, "thin-crop": thin, "planes": planes, "crop-planes-cut": all, "nothing": nothing}
}

// TestRayIntervalKeepsTheImage checks that clipping rays before the march
// renders exactly the images of the per-sample march, for every mode and
// projection.
func TestRayIntervalKeepsTheImage(t *testing.T) {
	vol, err := BuildVolume(sphereStack(48, 16))
	if err != nil {
		t.Fatal(err)
	}
	window := WindowLevel{Center: 128, Width: 256}
	for name, clip := range testClips(vol) {
		for _, mode := range []VRMode{VRModeDVR, VRModeMIP, VRModeMinIP, VRModeAverage} {
			for _, projection := range []VRCameraProjection{VRProjectionPerspective, VRProjectionParallel, VRProjectionEndoscopy} {
				for _, yaw := range []float64{0, 37, 90} {
					label := fmt.Sprintf("%s/mode%d/projection%d/yaw%v", name, mode, projection, yaw)
					cam := NewVRCamera(vol.BoundingRadiusMM())
					cam.FitVolume(vol)
					cam.Projection = projection
					cam.Rotate(yaw, yaw/3)
					preset := opaqueAbovePreset()
					preset.Mode = mode
					quality := DefaultVRQuality(40, 32)
					got := RenderVR(vol, cam, preset, window, mode == VRModeDVR, clip, quality).(*image.NRGBA)
					want := RenderVR(vol, cam, preset, window, mode == VRModeDVR, fullMarch(clip), quality).(*image.NRGBA)
					if !bytes.Equal(got.Pix, want.Pix) {
						t.Errorf("%s: image differs from the full march", label)
					}
				}
			}
		}
	}
}

// TestRayIntervalContainsKeptSamples checks the invariant behind the
// identity on random rays and clips: every sample of the full grid that keep
// accepts lies in the clipped march.
func TestRayIntervalContainsKeptSamples(t *testing.T) {
	random := rand.New(rand.NewSource(1002))
	size := Vec3{X: 40, Y: 30, Z: 20}
	boxMin := size.Scale(-0.5)
	unit := func() float64 { return random.Float64()*1.4 - 0.2 }
	skipped := 0
	for trial := 0; trial < 4000; trial++ {
		clip := NewVRClip()
		if trial%3 != 2 {
			lo := Vec3{X: unit(), Y: unit(), Z: unit()}
			hi := Vec3{X: lo.X + random.Float64()*0.6, Y: lo.Y + random.Float64()*0.6, Z: lo.Z + random.Float64()*0.6}
			clip.SetCropBox(lo, hi)
		}
		for p := random.Intn(4); p > 0; p-- {
			clip.AddClipPlane(Vec3{X: random.NormFloat64(), Y: random.NormFloat64(), Z: random.NormFloat64()}, random.NormFloat64())
		}
		o := Vec3{X: random.NormFloat64() * 60, Y: random.NormFloat64() * 60, Z: random.NormFloat64() * 60}
		d := Vec3{X: random.NormFloat64(), Y: random.NormFloat64(), Z: random.NormFloat64()}
		switch trial % 7 {
		case 0:
			d.X = 0 // parallel to a pair of faces
		case 1:
			d.Y, d.Z = 1e-13, 0
		}
		d = d.Normalize()
		t0, t1, hit := intersectAABB(o, d, boxMin, boxMin.Scale(-1))
		if !hit {
			continue
		}
		step := size.Length() / 96
		start, end, visible := clip.clipRay(o, d, boxMin, size, t0, t1, step)
		for tt := t0; tt <= t1; tt += step {
			pw := o.Add(d.Scale(tt))
			tex := Vec3{X: (pw.X - boxMin.X) / size.X, Y: (pw.Y - boxMin.Y) / size.Y, Z: (pw.Z - boxMin.Z) / size.Z}
			if !clip.keep(tex) {
				continue
			}
			if !visible || tt < start || tt > end {
				t.Fatalf("trial %d: kept sample t=%v outside the clipped march [%v,%v] (visible %v)", trial, tt, start, end, visible)
			}
		}
		if !visible || end-start < t1-t0 {
			skipped++
		}
	}
	if skipped == 0 {
		t.Error("no ray was shortened")
	}
	if lo, hi, visible := (*VRClip)(nil).rayInterval(Vec3{}, Vec3{X: 1}, boxMin, size); !visible || !math.IsInf(lo, -1) || !math.IsInf(hi, 1) {
		t.Errorf("nil clip interval [%v,%v] %v", lo, hi, visible)
	}
}
