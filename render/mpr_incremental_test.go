package render

import (
	"context"
	"image"
	"math"
	"testing"
)

// TestIncrementalResliceMatchesPerPixelTransform checks the row-incremental
// reslice (#1007) against sampling every pixel through PatientToVoxel, on an
// anisotropic affine volume and on a gantry-tilted one (whose transform is
// not affine and keeps the per-point path): scalar, slab and display outputs
// agree to floating-point rounding.
func TestIncrementalResliceMatchesPerPixelTransform(t *testing.T) {
	// Voxels vary along every axis, so an error in any voxel coordinate shows.
	varied := func(stack *Stack) *Stack {
		for z, frame := range stack.Frames {
			for i := range frame.PixelBytes {
				frame.PixelBytes[i] = byte(3*(i%16) + 5*(i/16) + 20*z)
			}
		}
		return stack
	}
	affine, err := BuildVolume(varied(gradientColumnStackWithSpacing(16, 16, 8, 1, 0.5, 3)))
	if err != nil {
		t.Fatal(err)
	}
	origins := make([]Vec3, 8)
	for i := range origins {
		origins[i] = Vec3{Y: float64(i) * 0.25, Z: float64(i)}
	}
	tilted, err := BuildVolume(tiltedGradientRowStack(12, 10, origins, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	// Slices 2% off a uniform step (still one geometry, no resampling): z
	// follows the exact slice positions.
	jitteredStack := varied(gradientColumnStackWithSpacing(16, 16, 8, 1, 0.5, 3))
	for z, frame := range jitteredStack.Frames {
		frame.ImagePosition = []float64{0, 0, 3*float64(z) + 0.06*float64(z%2)}
	}
	jittered, err := BuildVolume(jitteredStack)
	if err != nil {
		t.Fatal(err)
	}
	if !affine.voxelLine(Vec3{}, Vec3{X: 1}).affine || affine.uniformSliceStep() == 0 ||
		tilted.voxelLine(Vec3{}, Vec3{X: 1}).affine ||
		jittered.geometry.RequiresResampling || jittered.uniformSliceStep() != 0 {
		t.Fatal("the fixtures do not cover the uniform, exact-position and tilted paths")
	}
	window := WindowLevel{Center: 128, Width: 256}
	for name, vol := range map[string]*Volume{"affine": affine, "tilted": tilted, "jittered": jittered} {
		plane := obliqueInteriorPlane(vol)
		const w, h = 23, 17
		reader, err := vol.AcquireReader()
		if err != nil {
			t.Fatal(err)
		}
		scalar, err := ResliceObliqueScalarContext(context.Background(), vol, plane, w, h)
		if err != nil {
			t.Fatal(err)
		}
		display := ResliceOblique(vol, plane, w, h, window).(*image.Gray)
		normal := plane.U.Cross(plane.V).Normalize()
		slab, err := ResliceObliqueSlabScalarContext(context.Background(), vol, plane, w, h, 3, SlabMax)
		if err != nil {
			t.Fatal(err)
		}
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				point := plane.At(float64(x)/(w-1), float64(y)/(h-1))
				want, wantOK := reader.SamplePatient(point)
				got, ok := scalar.ValueAt(x, y)
				if ok != wantOK || ok && math.Abs(got-want) > 1e-9 {
					t.Fatalf("%s scalar (%d,%d) = %v %v, per-pixel %v %v", name, x, y, got, ok, want, wantOK)
				}
				if ok {
					mapped := displayGrayMapped(want, prepareWindow(window), "MONOCHROME2")
					if d := int(display.GrayAt(x, y).Y) - int(mapped); d < -1 || d > 1 {
						t.Fatalf("%s display (%d,%d) = %d, per-pixel %d", name, x, y, display.GrayAt(x, y).Y, mapped)
					}
				}
				best, bestOK := math.Inf(-1), false
				for k := -1; k <= 1; k++ {
					if v, ok := reader.SamplePatient(point.Add(normal.Scale(float64(k) * vol.SliceSpacing))); ok {
						best, bestOK = math.Max(best, v), true
					}
				}
				got, ok = slab.ValueAt(x, y)
				if ok != bestOK || ok && math.Abs(got-best) > 1e-9 {
					t.Fatalf("%s slab (%d,%d) = %v %v, per-pixel %v %v", name, x, y, got, ok, best, bestOK)
				}
			}
		}
		reader.Close()
	}
}
