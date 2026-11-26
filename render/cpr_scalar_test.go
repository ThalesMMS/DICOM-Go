package render

import (
	"context"
	"math"
	"testing"
)

// TestRenderCPRScalarTransverseMatchesVolume checks that every transverse
// sample equals the volume at the plane position the result reports.
func TestRenderCPRScalarTransverseMatchesVolume(t *testing.T) {
	vol, err := BuildVolume(gradientColumnStack(12, 12, 6))
	if err != nil {
		t.Fatal(err)
	}
	path := NewCPRPath([]Vec3{vol.VoxelToPatient(Vec3{X: 6, Y: 2, Z: 1}), vol.VoxelToPatient(Vec3{X: 6, Y: 9, Z: 4})})
	scalar, err := RenderCPRScalar(context.Background(), CPRRequest{Mode: CPRTransverse, Volume: vol, Path: path, Width: 9, CrossSpacing: 0.8, ArcLength: 3, RotationDegrees: 30})
	if err != nil {
		t.Fatal(err)
	}
	if scalar.Width != 9 || scalar.Height != 9 || scalar.Plane == nil || scalar.ColumnSpacing != 0.8 || scalar.RowSpacing != 0.8 {
		t.Fatalf("scalar %dx%d plane %v spacing %v %v", scalar.Width, scalar.Height, scalar.Plane, scalar.ColumnSpacing, scalar.RowSpacing)
	}
	if got := scalar.Plane.U.Length() / 8; math.Abs(got-0.8) > 1e-9 {
		t.Fatalf("plane pixel spacing %v", got)
	}
	valid := 0
	for j := 0; j < 9; j++ {
		for i := 0; i < 9; i++ {
			k := j*9 + i
			want, ok := vol.TrilinearAt(scalar.Plane.At(float64(i)/8, float64(j)/8))
			if ok != scalar.Valid[k] || ok && math.Abs(want-scalar.Values[k]) > 1e-9 {
				t.Fatalf("pixel %d,%d: %v %v, want %v %v", i, j, scalar.Values[k], scalar.Valid[k], want, ok)
			}
			if ok {
				valid++
			}
		}
	}
	if valid == 0 {
		t.Fatal("no sample inside the volume")
	}
}

// TestRenderCPRScalarAgreesWithRenderCPR maps the scalar straightened view
// through the window and compares it with the windowed render.
func TestRenderCPRScalarAgreesWithRenderCPR(t *testing.T) {
	vol, err := BuildVolume(gradientXZStack(12, 12, 5))
	if err != nil {
		t.Fatal(err)
	}
	path := NewCPRPath([]Vec3{vol.VoxelToPatient(Vec3{X: 6, Y: 2, Z: 1}), vol.VoxelToPatient(Vec3{X: 6, Y: 9, Z: 1})})
	window := WindowLevel{Center: 128, Width: 256}
	for _, mode := range []CPRMode{CPRStraightened, CPRStretched} {
		req := CPRRequest{Mode: mode, Volume: vol, Path: path, Window: window, Width: 7, CrossSpacing: 1, ArcSpacing: 0.5}
		img, err := RenderCPR(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		scalar, err := RenderCPRScalar(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if scalar.Plane != nil || scalar.ColumnSpacing != 0.5 || img.Bounds().Dx() != scalar.Width || img.Bounds().Dy() != scalar.Height {
			t.Fatalf("mode %d: scalar %dx%d, image %v", mode, scalar.Width, scalar.Height, img.Bounds())
		}
		mapper := prepareWindow(window)
		for y := 0; y < scalar.Height; y++ {
			for x := 0; x < scalar.Width; x++ {
				k := y*scalar.Width + x
				want := uint8(0)
				if scalar.Valid[k] {
					want = displayGrayMapped(scalar.Values[k], mapper, vol.Photometric())
				}
				if got := grayAt(img, x, y); got != want {
					t.Fatalf("mode %d pixel %d,%d: windowed %d, scalar %d", mode, x, y, got, want)
				}
			}
		}
	}
}
