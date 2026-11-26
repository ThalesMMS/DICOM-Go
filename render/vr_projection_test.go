package render

import (
	"context"
	"encoding/binary"
	"errors"
	"image"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// projectionPhantomStack is a synthetic CT (stored = HU + 1024) with the
// given voxel spacing whose HU at each voxel is hu(x, y, z).
func projectionPhantomStack(cols, rows, depth int, spacing Vec3, hu func(x, y, z int) int) *Stack {
	stack := &Stack{PixelSpacing: []float64{spacing.Y, spacing.X}, SliceThickness: spacing.Z}
	for z := 0; z < depth; z++ {
		data := make([]byte, rows*cols*2)
		for y := 0; y < rows; y++ {
			for x := 0; x < cols; x++ {
				binary.LittleEndian.PutUint16(data[2*(y*cols+x):], uint16(hu(x, y, z)+1024))
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
			ImagePosition:    []float64{0, 0, spacing.Z * float64(z)},
		})
	}
	return stack
}

func projectionSampler(t *testing.T, stack *Stack) (*Volume, *volumeSampler) {
	t.Helper()
	vol, err := BuildVolume(stack)
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
	t.Cleanup(sampler.Close)
	return vol, sampler
}

// TestRaySumIsProportionalToTraversedLength: through a phantom of uniform
// density every ray's sum is the density times the chord it traverses, to
// within one step, along the axes and obliquely, through and off the centre.
func TestRaySumIsProportionalToTraversedLength(t *testing.T) {
	const density = 100.0
	vol, sampler := projectionSampler(t, projectionPhantomStack(41, 23, 17, Vec3{X: 0.8, Y: 0.7, Z: 1.5},
		func(int, int, int) int { return int(density) }))
	size := vol.PhysicalSize()
	boxMin := size.Scale(-0.5)
	projection := vrProjection{mode: VRModeRaySum}
	const step = 0.05
	directions := []Vec3{{X: 1}, {Y: 1}, {Z: 1}, {X: 1, Y: 1, Z: 1}, {X: 0.3, Y: -1, Z: 0.2}, {X: -2, Y: 0.5, Z: 1}}
	offsets := []Vec3{{}, {X: 3, Y: -2, Z: 1}, {X: -6, Y: 4, Z: -3}}
	rays := 0
	for _, direction := range directions {
		d := direction.Normalize()
		for _, offset := range offsets {
			o := offset.Sub(d.Scale(1000))
			t0, t1, hit := intersectAABB(o, d, boxMin, boxMin.Scale(-1))
			if !hit {
				continue
			}
			rays++
			length := t1 - t0
			acc, count := accumulateProjection(context.Background(), sampler, o, d, t0, t1, step, boxMin, size, projection, nil, nil)
			if count == 0 {
				t.Fatalf("ray %v+%v: no samples", direction, offset)
			}
			if want := density * length; math.Abs(acc-want) > density*step {
				t.Errorf("ray %v+%v: sum %.4f HU·mm over %.3f mm, want %.4f ± %.1f", direction, offset, acc, length, want, density*step)
			}
			if want := density * step * float64(count); math.Abs(acc-want) > 1e-9*want {
				t.Errorf("ray %v+%v: sum %v, want %d samples × %v", direction, offset, acc, count, want)
			}
		}
	}
	if rays < len(directions)*2 {
		t.Fatalf("only %d rays hit the phantom", rays)
	}
}

// TestRaySumAndAverageValueRangeExcludeAir: half air, half 40 HU tissue
// along x. With the range [-500, 3000] the ray-sum is the tissue chord's
// integral (plus the analytic share of the interpolated interface inside the
// range) and the mean is the tissue's windowed value; without it, air
// dominates both.
func TestRaySumAndAverageValueRangeExcludeAir(t *testing.T) {
	const cols, boundary, sx = 40, 15, 0.9
	vol, sampler := projectionSampler(t, projectionPhantomStack(cols, 9, 7, Vec3{X: sx, Y: 1, Z: 1},
		func(x, _, _ int) int {
			if x < boundary {
				return -1000
			}
			return 40
		}))
	size := vol.PhysicalSize()
	boxMin := size.Scale(-0.5)
	d := Vec3{X: 1}
	o := Vec3{X: -1000}
	t0, t1, hit := intersectAABB(o, d, boxMin, boxMin.Scale(-1))
	if !hit {
		t.Fatal("ray misses")
	}
	const step = 0.01
	valueRange := VRValueRange{Low: -500, High: 3000, Enabled: true}
	// Between voxels boundary-1 and boundary the value is -1000 + 1040 f;
	// the part with f >= 500/1040 lies in the range.
	f0 := 500.0 / 1040
	interface_ := sx * ((-1000*1 + 520*1) - (-1000*f0 + 520*f0*f0))
	wantSum := 40*float64(cols-1-boundary)*sx + interface_
	sum, count := accumulateProjection(context.Background(), sampler, o, d, t0, t1, step, boxMin, size,
		vrProjection{mode: VRModeRaySum, valueRange: valueRange}, nil, nil)
	if count == 0 || math.Abs(sum-wantSum) > 1040*step {
		t.Fatalf("ranged ray-sum %.3f HU·mm (%d samples), want %.3f", sum, count, wantSum)
	}
	full, _ := accumulateProjection(context.Background(), sampler, o, d, t0, t1, step, boxMin, size,
		vrProjection{mode: VRModeRaySum}, nil, nil)
	if full > -10000 {
		t.Fatalf("unranged ray-sum %.3f HU·mm should be dominated by air", full)
	}

	window := WindowLevel{Center: 40, Width: 400}
	ranged, ok := marchProjectionContext(context.Background(), sampler, o, d, t0, t1, step, boxMin, size,
		vrProjection{mode: VRModeAverage, window: window, valueRange: valueRange}, nil, nil)
	if !ok || math.Abs(ranged-windowedUnit(40, window)) > 0.01 {
		t.Fatalf("ranged average %v, want about %v", ranged, windowedUnit(40, window))
	}
	unranged, _ := marchProjectionContext(context.Background(), sampler, o, d, t0, t1, step, boxMin, size,
		vrProjection{mode: VRModeAverage, window: window}, nil, nil)
	if unranged > ranged-0.15 {
		t.Fatalf("unranged average %v should include the dark air (ranged %v)", unranged, ranged)
	}
	// A range that excludes every sample leaves the ray empty.
	if _, ok := marchProjectionContext(context.Background(), sampler, o, d, t0, t1, step, boxMin, size,
		vrProjection{mode: VRModeAverage, window: window, valueRange: VRValueRange{Low: 500, High: 600, Enabled: true}}, nil, nil); ok {
		t.Fatal("a range without samples should leave the ray empty")
	}
}

// TestRenderVRRaySumFrame renders a uniform phantom in a parallel view along
// x: the centre pixel shows the density × chord over the diagonal through
// the window, and a range without samples shows the background.
func TestRenderVRRaySumFrame(t *testing.T) {
	const density = 100.0
	vol, err := BuildVolume(projectionPhantomStack(33, 21, 13, Vec3{X: 1, Y: 1, Z: 2},
		func(int, int, int) int { return int(density) }))
	if err != nil {
		t.Fatal(err)
	}
	size := vol.PhysicalSize()
	cam := NewVRCamera(vol.BoundingRadiusMM())
	cam.FitVolume(vol)
	cam.SetOrientation(Vec3{X: -1})
	cam.Projection = VRProjectionParallel
	preset := DefaultVRPreset()
	preset.Mode = VRModeRaySum
	mean := density * size.X / size.Length()
	window := WindowLevel{Center: mean, Width: 4 * mean}
	quality := VRQuality{Width: 31, Height: 31, MaxSteps: DefaultVRMaxSteps}
	img, err := RenderVRContext(context.Background(), vol, cam, preset, window, false, nil, quality)
	if err != nil {
		t.Fatal(err)
	}
	gray := int(img.(*image.NRGBA).NRGBAAt(15, 15).R)
	if want := int(to8(windowedUnit(mean, window))); gray < want-1 || gray > want+1 {
		t.Fatalf("centre gray %d, want %d", gray, want)
	}
	preset.Inverse = true
	inverse, _ := RenderVRContext(context.Background(), vol, cam, preset, window, false, nil, quality)
	if got := int(inverse.(*image.NRGBA).NRGBAAt(15, 15).R); got < 255-gray-1 || got > 255-gray+1 {
		t.Fatalf("inverse centre gray %d, want about %d", got, 255-gray)
	}
	preset.Inverse = false
	preset.ProjectionRange = VRValueRange{Low: 500, High: 1000, Enabled: true}
	empty, _ := RenderVRContext(context.Background(), vol, cam, preset, window, false, nil, quality)
	if got := empty.(*image.NRGBA).NRGBAAt(15, 15); got.R != 0 || got.G != 0 || got.B != 0 {
		t.Fatalf("a ray without in-range samples shows %v, want the black background", got)
	}
}

func TestVRValueRangeValidation(t *testing.T) {
	vol, err := BuildVolume(projectionPhantomStack(9, 9, 9, Vec3{X: 1, Y: 1, Z: 1}, func(int, int, int) int { return 0 }))
	if err != nil {
		t.Fatal(err)
	}
	cam := NewVRCamera(vol.BoundingRadiusMM())
	for _, valueRange := range []VRValueRange{
		{Low: 10, High: -10, Enabled: true},
		{Low: math.NaN(), High: 1, Enabled: true},
		{Low: 0, High: math.Inf(1), Enabled: true},
	} {
		if valueRange.Valid() {
			t.Fatalf("%+v should be invalid", valueRange)
		}
		preset := DefaultVRPreset()
		preset.Mode, preset.ProjectionRange = VRModeAverage, valueRange
		if _, err := RenderVRContext(context.Background(), vol, cam, preset, WindowLevel{Center: 0, Width: 100}, false, nil, VRQuality{Width: 4, Height: 4}); !errors.Is(err, ErrInvalidVRValueRange) {
			t.Fatalf("%+v: err %v, want ErrInvalidVRValueRange", valueRange, err)
		}
	}
	if !(VRValueRange{Low: 10, High: 10, Enabled: true}).Valid() || !(VRValueRange{Low: 10, High: -10}).Valid() {
		t.Fatal("a point range and a disabled range are valid")
	}
}

func TestVRPresetCPUOnlyProjection(t *testing.T) {
	ranged := VRValueRange{Low: -500, High: 3000, Enabled: true}
	for _, test := range []struct {
		mode       VRMode
		valueRange VRValueRange
		usesRange  bool
		cpuOnly    bool
	}{
		{VRModeDVR, ranged, false, false},
		{VRModeMIP, ranged, false, false},
		{VRModeMinIP, ranged, false, false},
		{VRModeAverage, VRValueRange{}, false, false},
		{VRModeAverage, ranged, true, true},
		{VRModeRaySum, VRValueRange{}, false, true},
		{VRModeRaySum, ranged, true, true},
	} {
		preset := VRPreset{Mode: test.mode, ProjectionRange: test.valueRange}
		if got := preset.UsesValueRange(); got != test.usesRange {
			t.Errorf("mode %d range %v: UsesValueRange %v", test.mode, test.valueRange.Enabled, got)
		}
		if got := preset.CPUOnlyProjection(); got != test.cpuOnly {
			t.Errorf("mode %d range %v: CPUOnlyProjection %v", test.mode, test.valueRange.Enabled, got)
		}
	}
}

// TestValueRangeLeapSkipsOutOfRangeBlocks: with a range excluding air, the
// air blocks of the CT phantom are skipped for Average and RaySum.
func TestValueRangeLeapSkipsOutOfRangeBlocks(t *testing.T) {
	vol, err := BuildVolume(ctPhantomStack(37, 29, 21))
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
	for _, mode := range []VRMode{VRModeAverage, VRModeRaySum} {
		preset := DefaultVRPreset()
		preset.Mode = mode
		window := WindowLevel{Center: 40, Width: 400}
		lut, window, dataMin, dataMax := vrTransferSetup(prepared, preset, window)
		if leap, err := newRenderLeap(context.Background(), prepared, sampler, preset, lut, window, dataMin, dataMax); err != nil || leap != nil {
			t.Fatalf("mode %d without a range: leap %v, err %v", mode, leap, err)
		}
		preset.ProjectionRange = VRValueRange{Low: -500, High: 3000, Enabled: true}
		leap, err := newRenderLeap(context.Background(), prepared, sampler, preset, lut, window, dataMin, dataMax)
		if err != nil || leap == nil {
			t.Fatalf("mode %d: leap %v, err %v", mode, leap, err)
		}
		skipped := 0
		for block := range leap.grid.min {
			if leap.skipsProjectionSample(block, mode, 0, 0) {
				skipped++
			}
		}
		if skipped == 0 || skipped == len(leap.grid.min) {
			t.Fatalf("mode %d: %d of %d blocks outside the range", mode, skipped, len(leap.grid.min))
		}
	}
}

// TestViewStateRejectsRaySum: no view-state contract carries the ray-sum,
// so GPU adapters must decline it before building one.
func TestViewStateRejectsRaySum(t *testing.T) {
	state := validVRViewState()
	if err := ValidateViewState(state); err != nil {
		t.Fatal(err)
	}
	state.VR.Mode = VRModeRaySum
	if err := ValidateViewState(state); !errors.Is(err, ErrInvalidViewState) {
		t.Fatalf("ray-sum view state: err = %v", err)
	}
}
