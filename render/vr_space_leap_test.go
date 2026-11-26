package render

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// ctPhantomStack is a synthetic CT: air (-1024 HU) around an ellipsoidal body
// of soft tissue with a bone shell and a little deterministic texture, so most
// blocks are empty under clinical presets.
func ctPhantomStack(cols, rows, depth int) *Stack {
	stack := &Stack{PixelSpacing: []float64{0.8, 0.8}, SliceThickness: 1.5}
	for z := 0; z < depth; z++ {
		data := make([]byte, rows*cols*2)
		for y := 0; y < rows; y++ {
			for x := 0; x < cols; x++ {
				dx := (float64(x) - float64(cols-1)/2) / (0.38 * float64(cols))
				dy := (float64(y) - float64(rows-1)/2) / (0.3 * float64(rows))
				dz := (float64(z) - float64(depth-1)/2) / (0.45 * float64(depth))
				r := math.Sqrt(dx*dx + dy*dy + dz*dz)
				stored := 0 // air
				switch {
				case r <= 0.55:
					stored = 1024 + 40 + (x*7+y*13+z*5)%23 // soft tissue
				case r <= 0.7:
					stored = 1024 + 1100 + (x+y+z)%50 // bone
				case r <= 1:
					stored = 1024 - 90 + (x*3+y)%11 // fat
				}
				binary.LittleEndian.PutUint16(data[2*(y*cols+x):], uint16(stored))
			}
		}
		stack.Frames = append(stack.Frames, &Frame{
			Metadata: pixeldata.Metadata{
				Rows: uint16(rows), Columns: uint16(cols), SamplesPerPixel: 1,
				BitsAllocated: 16, BitsStored: 12, HighBit: 11, PhotometricInterpretation: "MONOCHROME2",
			},
			ByteOrder:        binary.LittleEndian,
			PixelBytes:       data,
			Rescale:          Rescale{Slope: 1, Intercept: -1024},
			ImageOrientation: []float64{1, 0, 0, 0, 1, 0},
			ImagePosition:    []float64{0, 0, 1.5 * float64(z)},
		})
	}
	return stack
}

// TestSpaceLeapingKeepsTheImage renders every built-in preset, in its own
// mode and as MIP/MinIP/Average, with and without empty-space skipping and
// requires identical pixels.
func TestSpaceLeapingKeepsTheImage(t *testing.T) {
	vol, err := BuildVolume(ctPhantomStack(37, 29, 21))
	if err != nil {
		t.Fatal(err)
	}
	crop := NewVRClip()
	crop.SetCropBox(Vec3{X: 0.1, Y: 0.2, Z: 0.15}, Vec3{X: 0.8, Y: 0.9, Z: 0.7})
	crop.AddClipPlane(Vec3{X: 0.5, Y: 0.5, Z: 0.7}, 0.9)
	windows := []WindowLevel{{Center: 40, Width: 400}, {Center: 300, Width: 1500}, {Center: -600, Width: 1200}}
	presets := VRPresets()
	for _, mode := range []VRMode{VRModeMIP, VRModeMinIP, VRModeAverage, VRModeRaySum} {
		preset := DefaultVRPreset()
		preset.Name, preset.Mode = fmt.Sprintf("mode%d", mode), mode
		presets = append(presets, preset)
	}
	// Average and RaySum skip the blocks outside a value range (#1181).
	for _, valueRange := range []VRValueRange{{Low: -500, High: 3000, Enabled: true}, {Low: -200, High: 100, Enabled: true}} {
		for _, mode := range []VRMode{VRModeAverage, VRModeRaySum} {
			preset := DefaultVRPreset()
			preset.Name, preset.Mode, preset.ProjectionRange = fmt.Sprintf("mode%d range %v", mode, valueRange), mode, valueRange
			presets = append(presets, preset)
		}
	}
	skipped := 0
	for i, preset := range presets {
		for _, shading := range []bool{false, true} {
			for c, clip := range []*VRClip{nil, crop} {
				cam := NewVRCamera(vol.BoundingRadiusMM())
				cam.FitVolume(vol)
				cam.Rotate(float64(i*17), float64(i%5-2)*7)
				if i%3 == 1 {
					cam.Projection = VRProjectionParallel
				}
				window := windows[i%len(windows)]
				quality := DefaultVRQuality(36, 30)
				got := RenderVR(vol, cam, preset, window, shading, clip, quality).(*image.NRGBA)
				want := RenderVR(vol, cam, preset, window, shading, fullMarch(clip), quality).(*image.NRGBA)
				if !bytes.Equal(got.Pix, want.Pix) {
					t.Errorf("%s shading %v clip %d: image differs from the full march", preset.Name, shading, c)
				}
			}
		}
	}
	// Under Bones B/W only the bone shell is opaque: most blocks are empty.
	prepared, err := vol.PrepareVRVolume(VRPrefilterNone)
	if err != nil {
		t.Fatal(err)
	}
	sampler, err := prepared.newSampler()
	if err != nil {
		t.Fatal(err)
	}
	defer sampler.Close()
	preset := bonesBWPreset()
	lut, window, dataMin, dataMax := vrTransferSetup(prepared, preset, WindowLevel{Center: 40, Width: 400})
	leap, err := newRenderLeap(context.Background(), prepared, sampler, preset, lut, window, dataMin, dataMax)
	if err != nil {
		t.Fatal(err)
	}
	for _, empty := range leap.empty {
		if empty {
			skipped++
		}
	}
	if skipped < len(leap.empty)/3 || skipped == len(leap.empty) {
		t.Errorf("%d of %d blocks empty", skipped, len(leap.empty))
	}
}

// TestBlockGridBoundsSamples checks that every trilinear sample lies within
// its block's bounds, with the edge voxels repeated past the border.
func TestBlockGridBoundsSamples(t *testing.T) {
	vol, err := BuildVolume(ctPhantomStack(19, 11, 9))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := vol.PrepareVRVolume(VRPrefilterNone)
	if err != nil {
		t.Fatal(err)
	}
	sampler, err := prepared.newSampler()
	if err != nil {
		t.Fatal(err)
	}
	defer sampler.Close()
	grid, err := buildVRBlockGrid(context.Background(), sampler)
	if err != nil {
		t.Fatal(err)
	}
	if grid.nx != 3 || grid.ny != 2 || grid.nz != 2 {
		t.Fatalf("grid %dx%dx%d", grid.nx, grid.ny, grid.nz)
	}
	// The packed float32 fast path and the payload path agree.
	if len(sampler.values) == 0 && !sampler.packed16() {
		t.Fatal("the phantom has no packed values")
	}
	decoding := *sampler
	decoding.values = nil
	decoding.signed16, decoding.unsigned16 = nil, nil
	slow, err := buildVRBlockGrid(context.Background(), &decoding)
	if err != nil {
		t.Fatal(err)
	}
	for i := range grid.min {
		if grid.min[i] != slow.min[i] || grid.max[i] != slow.max[i] {
			t.Fatalf("block %d: fast [%v,%v], payload [%v,%v]", i, grid.min[i], grid.max[i], slow.min[i], slow.max[i])
		}
	}
	leap := newVRSpaceLeap(grid, sampler)
	for k := 0; k <= 40; k++ {
		for j := 0; j <= 40; j++ {
			for i := 0; i <= 40; i++ {
				tex := Vec3{X: float64(i) / 40, Y: float64(j) / 40, Z: float64(k) / 40}
				value, ok := sampler.textureAt(tex)
				block := leap.block(tex)
				if !ok || block < 0 {
					t.Fatalf("tex %v: sample %v block %d", tex, ok, block)
				}
				if value < float64(grid.min[block]) || value > float64(grid.max[block]) {
					t.Fatalf("tex %v: %v outside block %d [%v,%v]", tex, value, block, grid.min[block], grid.max[block])
				}
			}
		}
	}
}

// TestSpaceLeapStaysInBlock checks that leap only passes samples whose voxel
// position is in the block, for rays at many angles.
func TestSpaceLeapStaysInBlock(t *testing.T) {
	grid := &vrBlockGrid{nx: 4, ny: 4, nz: 4}
	leap := &vrSpaceLeap{grid: grid, scale: Vec3{X: 31, Y: 31, Z: 31}}
	size := Vec3{X: 62, Y: 31, Z: 46.5}
	boxMin := size.Scale(-0.5)
	for trial := 0; trial < 2000; trial++ {
		angle := float64(trial) * 0.0137
		d := Vec3{X: math.Cos(angle), Y: math.Sin(angle * 1.7), Z: math.Sin(angle * 0.3)}
		if trial%10 == 0 {
			d.Y = 0
		}
		d = d.Normalize()
		o := Vec3{X: -d.X * 80, Y: -d.Y*80 + math.Sin(angle*5)*10, Z: -d.Z*80 + math.Cos(angle*3)*10}
		t0, t1, hit := intersectAABB(o, d, boxMin, boxMin.Scale(-1))
		if !hit {
			continue
		}
		a, b := leap.rayVoxel(o, d, boxMin, size)
		step := 0.37
		for tt := t0; tt <= t1; tt += step {
			pw := o.Add(d.Scale(tt))
			tex := Vec3{X: (pw.X - boxMin.X) / size.X, Y: (pw.Y - boxMin.Y) / size.Y, Z: (pw.Z - boxMin.Z) / size.Z}
			block := leap.block(tex)
			if block < 0 {
				continue
			}
			end := leap.leap(block, a, b, tt, t1, step)
			for s := tt + step; s <= end; s += step {
				pw := o.Add(d.Scale(s))
				tex := Vec3{X: (pw.X - boxMin.X) / size.X, Y: (pw.Y - boxMin.Y) / size.Y, Z: (pw.Z - boxMin.Z) / size.Z}
				if got := leap.block(tex); got != block {
					t.Fatalf("trial %d: leap from t=%v to %v passes t=%v in block %d, not %d", trial, tt, end, s, got, block)
				}
			}
		}
	}
}
