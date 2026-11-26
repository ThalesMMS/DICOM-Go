package render

import (
	"context"
	"encoding/binary"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// ImageInterpolator and TestImageResliceMapperBorder exercise interpolation
// near input boundaries. The asymmetric profile exposes which cubic taps are
// extended and distinguishes extension from admitting an outside sample.
func TestVTKReferenceInterpolationAtVoxelFaces(t *testing.T) {
	profile := []int{40, 4, 100, 8}
	volume, err := BuildVolume(projectionPhantomStack(4, 4, 4, Vec3{X: 1, Y: 1, Z: 1},
		func(x, y, z int) int { return profile[x] + 2*profile[y] + 4*profile[z] }))
	if err != nil {
		t.Fatal(err)
	}
	defer volume.Close()
	reader, err := volume.AcquireReader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	for axis, scale := range []float64{1, 2, 4} {
		t.Run([]string{"x", "y", "z"}[axis], func(t *testing.T) {
			for _, tc := range []struct {
				name       string
				coordinate float64
				valid      bool
				want       [3]float64 // nearest, linear, cubic
			}{
				{"outside-low", -.51, false, [3]float64{}},
				{"half-voxel-low", -.5, true, [3]float64{40, 40, 40}},
				{"first-voxel", 0, true, [3]float64{40, 40, 40}},
				{"cubic-low", .25, true, [3]float64{40, 31, 3896.0 / 128}},
				{"nearest-tie-low", .5, true, [3]float64{4, 22, 16}},
				{"nearest-tie-high", 2.5, true, [3]float64{8, 54, 60}},
				{"cubic-high", 2.75, true, [3]float64{8, 31, 3704.0 / 128}},
				{"last-voxel", 3, true, [3]float64{8, 8, 8}},
				{"half-voxel-high", 3.5, true, [3]float64{8, 8, 8}},
				{"outside-high", 3.51, false, [3]float64{}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					position := [3]float64{1, 1, 1}
					position[axis] = tc.coordinate
					point := Vec3{X: position[0], Y: position[1], Z: position[2]}
					for i, mode := range []Interpolation{InterpolationNearest, InterpolationLinear, InterpolationCubic} {
						got, valid := reader.SampleAt(point, mode)
						// The fixed coordinates contribute the value at index 1.
						want := (7-scale)*4 + scale*tc.want[i]
						if valid != tc.valid || valid && got != want {
							t.Errorf("interpolation %d at %v: value %g (valid %v), want %g (valid %v)", mode, point, got, valid, want, tc.valid)
						}
					}
				})
			}
		})
	}
}

// ResliceSlabModes and ReslicePermuteSlab exercise reductions followed by
// windowing. Here the requested rays can partly cross the lower/upper slice
// bounds or entirely miss beside the volume; MONOCHROME1 still leaves misses
// black. Trapezoid weights belong to the requested sites, so a missing
// endpoint contributes zero weight and each accepted interior site keeps its
// full weight. The scalar values and both windowed images have a numerical
// oracle.
func TestVTKReferencePartialSlabProjectionBeforeWindow(t *testing.T) {
	for _, format := range []struct {
		name      string
		bits      uint16
		signed    uint16
		byteOrder binary.ByteOrder
		rescale   Rescale
	}{
		{"unsigned8", 8, 0, binary.LittleEndian, Rescale{Slope: 2, Intercept: -60}},
		{"signed8-negative-slope", 8, 1, binary.LittleEndian, Rescale{Slope: -2, Intercept: 12}},
		{"unsigned16-little-endian", 16, 0, binary.LittleEndian, Rescale{Slope: .5, Intercept: -16384}},
		{"signed16-big-endian", 16, 1, binary.BigEndian, Rescale{Slope: -2, Intercept: 12}},
	} {
		t.Run(format.name, func(t *testing.T) {
			for _, photometric := range []string{"MONOCHROME1", "MONOCHROME2"} {
				t.Run(photometric, func(t *testing.T) {
					stack := &Stack{PixelSpacing: []float64{1, 1}, SliceThickness: 2}
					for z, level := range []float64{-60, 140, 20, -20} {
						data := make([]byte, 8*int(format.bits/8))
						for y := range 2 {
							for x := range 4 {
								modality := level + float64(x*8+y*16)
								stored := int((modality - format.rescale.Intercept) / format.rescale.Slope)
								index := y*4 + x
								if format.bits == 8 {
									data[index] = byte(stored)
								} else {
									format.byteOrder.PutUint16(data[2*index:], uint16(stored))
								}
							}
						}
						frame := volumeTestFrame(2, 4, data, 2*z)
						frame.Metadata.BitsAllocated = format.bits
						frame.Metadata.BitsStored = format.bits
						frame.Metadata.HighBit = format.bits - 1
						frame.Metadata.PixelRepresentation = format.signed
						frame.Metadata.PhotometricInterpretation = photometric
						frame.ByteOrder = format.byteOrder
						frame.Rescale = format.rescale
						stack.Frames = append(stack.Frames, frame)
					}
					volume, err := BuildVolume(stack)
					if err != nil {
						t.Fatal(err)
					}
					defer volume.Close()
					checkVTKReferencePartialSlab(t, volume, photometric == "MONOCHROME1")
				})
			}
		})
	}
}

func checkVTKReferencePartialSlab(t *testing.T, volume *Volume, inverted bool) {
	t.Helper()
	for _, tc := range []struct {
		name          string
		centerMM      float64
		interpolation Interpolation
		trapezoid     bool
		min, max, sum float64
		weight        float64
	}{
		// Five sample sites: one is outside and one is clamped to an edge.
		// Lower linear values: [-60, -10, 90, 110]. Upper: [50, 10, -10, -20].
		{"linear-low-rectangular", .5, InterpolationLinear, false, -60, 110, 130, 4},
		{"linear-low-trapezoid", .5, InterpolationLinear, true, -60, 110, 75, 3.5},
		{"linear-high-rectangular", 5.5, InterpolationLinear, false, -20, 50, 30, 4},
		{"linear-high-trapezoid", 5.5, InterpolationLinear, true, -20, 50, 5, 3.5},
		// Lower nearest values: [-60, -60, 140, 140]. Upper: [20, 20, -20, -20].
		{"nearest-low-rectangular", .5, InterpolationNearest, false, -60, 140, 160, 4},
		{"nearest-low-trapezoid", .5, InterpolationNearest, true, -60, 140, 90, 3.5},
		{"nearest-high-rectangular", 5.5, InterpolationNearest, false, -20, 20, 0, 4},
		{"nearest-high-trapezoid", 5.5, InterpolationNearest, true, -20, 20, -10, 3.5},
	} {
		for _, reduction := range []struct {
			name              string
			mode              SlabMode
			base, offsetScale float64
		}{
			{"min", SlabMinIP, tc.min, 1},
			{"max", SlabMIP, tc.max, 1},
			{"mean", SlabAverage, tc.sum / tc.weight, 1},
			{"sum", SlabSum, tc.sum, tc.weight},
		} {
			t.Run(tc.name+"/"+reduction.name, func(t *testing.T) {
				plane := Plane{Origin: Vec3{X: -1, Z: tc.centerMM}, U: Vec3{X: 5}, V: Vec3{Y: 1}}
				options := MPRResliceOptions{Interpolation: tc.interpolation, Slab: SlabSampling{SpacingFraction: .5, Trapezoid: tc.trapezoid}}
				scalar, err := ResliceObliqueSlabScalarWithOptionsContext(context.Background(), volume, plane, 6, 2, 3, reduction.mode, options)
				if err != nil {
					t.Fatal(err)
				}
				narrow := scalar.ApplyWindow(WindowLevel{Width: 80, Function: display.VOILinearExact})
				wide := scalar.ApplyWindow(WindowLevel{Width: 960, Function: display.VOILinearExact})
				for y := range 2 {
					for x := range 6 {
						valid := x >= 1 && x <= 4
						want := reduction.base + float64((x-1)*8+y*16)*reduction.offsetScale
						got, ok := scalar.ValueAt(x, y)
						if ok != valid || ok && !(math.Abs(got-want) <= 1e-9) {
							t.Errorf("(%d,%d): value %g (valid %v), want %g (valid %v)", x, y, got, ok, want, valid)
						}
						gray := func(width float64) uint8 {
							if !valid {
								return 0
							}
							value := uint8(math.Round(255 * min(1, max(0, (want+width/2)/width))))
							if inverted {
								return 255 - value
							}
							return value
						}
						if n, w := grayAt(narrow, x, y), grayAt(wide, x, y); n != gray(80) || w != gray(960) {
							t.Errorf("(%d,%d): gray %d/%d, want %d/%d in narrow/wide windows", x, y, n, w, gray(80), gray(960))
						}
					}
				}
			})
		}
	}
}
