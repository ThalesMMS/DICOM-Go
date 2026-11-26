package render

import (
	"context"
	"math"
	"testing"
)

// TestBinShrinkVolumeOriginSpacingAndAverages (#1066): shrinking by f keeps
// floor(n/f) voxels of f times the spacing, moves the origin (f-1)/2 input
// voxels along every axis (ITK BinShrink), and averages each block, so a
// linear field keeps its value at every output voxel's patient position.
func TestBinShrinkVolumeOriginSpacingAndAverages(t *testing.T) {
	stack := stack16(7, true, func(int) Rescale { return Rescale{Slope: 1, Intercept: -1024} }, ctValue)
	stack.PixelSpacing = []float64{0.7, 0.5} // row spacing, column spacing
	for _, frame := range stack.Frames {
		frame.ImagePosition[2] *= 1.5
	}
	source, err := BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	for _, f := range []int{2, 3} {
		shrunk, err := BinShrinkVolumeContext(context.Background(), source, [3]int{f, f, f})
		if err != nil {
			t.Fatal(err)
		}
		if want := 7 / f; shrunk.Cols != want || shrunk.Rows != want || shrunk.Depth != want {
			t.Fatalf("f=%d dimensions %dx%dx%d, want %d", f, shrunk.Cols, shrunk.Rows, shrunk.Depth, want)
		}
		if math.Abs(shrunk.ColSpacing-0.5*float64(f)) > 1e-9 || math.Abs(shrunk.RowSpacing-0.7*float64(f)) > 1e-9 ||
			math.Abs(shrunk.SliceSpacing-1.5*float64(f)) > 1e-9 {
			t.Fatalf("f=%d spacing %v,%v,%v", f, shrunk.ColSpacing, shrunk.RowSpacing, shrunk.SliceSpacing)
		}
		shift := float64(f-1) / 2
		wantOrigin := Vec3{X: shift * 0.5, Y: shift * 0.7, Z: shift * 1.5}
		if shrunk.Origin.Sub(wantOrigin).Length() > 1e-9 {
			t.Fatalf("f=%d origin %v, want %v ((f-1)/2 spacing)", f, shrunk.Origin, wantOrigin)
		}
		sampler, err := newVolumeSamplerWithError(shrunk)
		if err != nil {
			t.Fatal(err)
		}
		for z := 0; z < shrunk.Depth; z++ {
			for y := 0; y < shrunk.Rows; y++ {
				for x := 0; x < shrunk.Cols; x++ {
					// Output voxel i is input continuous index f·i + (f-1)/2.
					in := source.PatientToVoxel(shrunk.VoxelToPatient(Vec3{X: float64(x), Y: float64(y), Z: float64(z)}))
					wantIn := Vec3{X: float64(f*x) + shift, Y: float64(f*y) + shift, Z: float64(f*z) + shift}
					if in.Sub(wantIn).Length() > 1e-9 {
						t.Fatalf("f=%d voxel %d,%d,%d maps to input %v, want %v", f, x, y, z, in, wantIn)
					}
					got, _ := sampler.valueAt(x, y, z)
					want := in.X*37 + in.Y*11 + in.Z*5 - 1000 - 1024
					if math.Abs(got-want) > 1e-3 {
						t.Fatalf("f=%d voxel %d,%d,%d = %v, want block mean %v", f, x, y, z, got, want)
					}
				}
			}
		}
		sampler.Close()
		if err := shrunk.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestInteractiveVolumeIsCachedAndClosedWithItsSource: the 2× level is built
// once, keeps thin axes, holds an eighth of the float32 voxels, and closes
// with its source.
func TestInteractiveVolumeIsCachedAndClosedWithItsSource(t *testing.T) {
	stack := stack16(8, true, func(int) Rescale { return Rescale{Slope: 1} }, ctValue)
	stack.Frames = stack.Frames[:3] // 8×8×3: z is too thin to shrink
	source, err := BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	first, err := source.InteractiveVolumeContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, _ := source.InteractiveVolumeContext(context.Background())
	if first != second || first == source {
		t.Fatal("interaction level is not cached")
	}
	if first.Cols != 4 || first.Rows != 4 || first.Depth != 3 {
		t.Fatalf("interaction level %dx%dx%d, want 4x4x3", first.Cols, first.Rows, first.Depth)
	}
	if got := source.interactiveVolumeBytes(); got != 4*4*3*4 {
		t.Fatalf("interaction level bytes %d", got)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if !first.closed.Load() {
		t.Fatal("interaction level outlived its source")
	}
	if _, err := source.InteractiveVolumeContext(context.Background()); err == nil {
		t.Fatal("closed volume built an interaction level")
	}
	tiny, err := BuildVolume(stack16(3, true, func(int) Rescale { return Rescale{Slope: 1} }, ctValue))
	if err != nil {
		t.Fatal(err)
	}
	defer tiny.Close()
	if level, err := tiny.InteractiveVolumeContext(context.Background()); err != nil || level != tiny {
		t.Fatalf("3³ interaction level = %p, %v; want the volume itself", level, err)
	}
}
