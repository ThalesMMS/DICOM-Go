package render

import (
	"context"
	"errors"
	"math"
	"testing"
)

func smoothField(x, y, z float64) float64 {
	return 100*math.Sin(0.45*x+0.2) + 60*math.Cos(0.35*y)*math.Sin(0.3*z+0.5) + 4*z
}

func gridSample(size [3]int, f func(x, y, z int) float64) []float64 {
	values := make([]float64, size[0]*size[1]*size[2])
	for z := 0; z < size[2]; z++ {
		for y := 0; y < size[1]; y++ {
			for x := 0; x < size[0]; x++ {
				values[(z*size[1]+y)*size[0]+x] = f(x, y, z)
			}
		}
	}
	return values
}

func trilinearGrid(values []float64, size [3]int, p Vec3) float64 {
	base := [3]int{}
	frac := [3]float64{}
	for axis, v := range [3]float64{p.X, p.Y, p.Z} {
		base[axis] = min(int(math.Floor(v)), size[axis]-2)
		frac[axis] = v - float64(base[axis])
	}
	sum := 0.0
	for corner := 0; corner < 8; corner++ {
		w := 1.0
		at := base
		for axis := 0; axis < 3; axis++ {
			if corner>>axis&1 == 1 {
				w *= frac[axis]
				at[axis]++
			} else {
				w *= 1 - frac[axis]
			}
		}
		sum += w * values[(at[2]*size[1]+at[1])*size[0]+at[0]]
	}
	return sum
}

// TestCubicBSplineInterpolatesTheSamples: the prefiltered interpolant
// reproduces every sample of its box, for whole grids, sub-boxes and the
// short lines of the exact causal initialization (1 to 3 samples).
func TestCubicBSplineInterpolatesTheSamples(t *testing.T) {
	noisy := func(x, y, z int) float64 {
		return smoothField(float64(x), float64(y), float64(z)) + float64((x*7+y*13+z*29)%17) - 8
	}
	for _, test := range []struct {
		min, size [3]int
	}{
		{[3]int{}, [3]int{29, 17, 11}},
		{[3]int{3, 2, 1}, [3]int{9, 6, 5}},
		{[3]int{}, [3]int{1, 2, 3}},
		{[3]int{}, [3]int{40, 1, 1}},
	} {
		spline, err := NewCubicBSplineContext(context.Background(), test.min, test.size, noisy)
		if err != nil {
			t.Fatal(err)
		}
		for z := 0; z < test.size[2]; z++ {
			for y := 0; y < test.size[1]; y++ {
				for x := 0; x < test.size[0]; x++ {
					gx, gy, gz := test.min[0]+x, test.min[1]+y, test.min[2]+z
					want := noisy(gx, gy, gz)
					got := spline.At(Vec3{X: float64(gx), Y: float64(gy), Z: float64(gz)})
					if math.Abs(got-want) > 1e-4*(1+math.Abs(want)) {
						t.Fatalf("box %v+%v sample (%d,%d,%d) = %v, want %v", test.min, test.size, gx, gy, gz, got, want)
					}
				}
			}
		}
	}
	// A constant stays constant everywhere (the weights sum to one).
	constant, err := NewCubicBSplineContext(context.Background(), [3]int{}, [3]int{7, 5, 4}, func(int, int, int) float64 { return -1024 })
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []Vec3{{X: 0.3, Y: 1.7, Z: 2.2}, {X: 6, Y: 4, Z: 3}, {X: 3.5, Y: 0, Z: 0.5}} {
		if got := constant.At(p); math.Abs(got+1024) > 1e-3 {
			t.Fatalf("constant at %v = %v", p, got)
		}
	}
}

// TestCubicBSplineBeatsTrilinearOnASmoothField: between the samples of a
// smooth field the B-spline error is a fraction of trilinear's.
func TestCubicBSplineBeatsTrilinearOnASmoothField(t *testing.T) {
	size := [3]int{32, 28, 24}
	values := gridSample(size, func(x, y, z int) float64 { return smoothField(float64(x), float64(y), float64(z)) })
	spline, err := NewCubicBSplineContext(context.Background(), [3]int{}, size, func(x, y, z int) float64 {
		return values[(z*size[1]+y)*size[0]+x]
	})
	if err != nil {
		t.Fatal(err)
	}
	var linearSquared, splineSquared float64
	count := 0
	for z := 4.0; z < float64(size[2]-5); z += 0.83 {
		for y := 4.0; y < float64(size[1]-5); y += 0.71 {
			for x := 4.0; x < float64(size[0]-5); x += 0.67 {
				p := Vec3{X: x + 0.13, Y: y + 0.29, Z: z + 0.41}
				want := smoothField(p.X, p.Y, p.Z)
				linearSquared += math.Pow(trilinearGrid(values, size, p)-want, 2)
				splineSquared += math.Pow(spline.At(p)-want, 2)
				count++
			}
		}
	}
	linear, cubic := math.Sqrt(linearSquared/float64(count)), math.Sqrt(splineSquared/float64(count))
	if !(cubic < linear/4) {
		t.Fatalf("RMS error: B-spline %.4f, trilinear %.4f over %d points; want the B-spline below a quarter", cubic, linear, count)
	}
	t.Logf("RMS error over %d points: B-spline %.4f, trilinear %.4f", count, cubic, linear)
}

func TestCubicBSplineRejectsNonFiniteSamples(t *testing.T) {
	_, err := NewCubicBSplineContext(context.Background(), [3]int{}, [3]int{4, 4, 4}, func(x, y, z int) float64 {
		if x == 2 && y == 1 && z == 3 {
			return math.NaN()
		}
		return 1
	})
	if !errors.Is(err, ErrBSplineSamples) {
		t.Fatalf("NaN sample: err = %v", err)
	}
	if _, err := NewCubicBSplineContext(context.Background(), [3]int{}, [3]int{0, 4, 4}, func(int, int, int) float64 { return 0 }); !errors.Is(err, ErrBSplineSamples) {
		t.Fatalf("empty box: err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewCubicBSplineContext(ctx, [3]int{}, [3]int{4, 4, 4}, func(int, int, int) float64 { return 0 }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: err = %v", err)
	}
}

// TestResampleRegularGridContext: the linear kernel writes RegularGrid's
// voxels exactly; the cubic B-spline reproduces the source slices the
// regular grid passes through and differs between them.
func TestResampleRegularGridContext(t *testing.T) {
	// Slices at z = 0, 1, 2, 4, 5 mm: a gap that needs resampling onto a
	// 1 mm grid with 6 slices, of which 0, 1, 2, 4 and 5 are source slices.
	positions := []float64{0, 1, 2, 4, 5}
	stack := &Stack{PixelSpacing: []float64{1, 1}, SliceThickness: 1}
	for index, z := range positions {
		data := make([]byte, 6*7)
		for y := 0; y < 6; y++ {
			for x := 0; x < 7; x++ {
				data[y*7+x] = byte(60 + 40*math.Sin(0.7*float64(x)+0.4*float64(y)) + 30*math.Cos(1.1*z))
			}
		}
		frame := volumeTestFrame(6, 7, data, index)
		frame.ImagePosition = []float64{0, 0, z}
		stack.Frames = append(stack.Frames, frame)
	}
	vol, err := BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	if !vol.Geometry().RequiresResampling {
		t.Fatal("the gapped stack should need resampling")
	}
	linear, grid, err := vol.ResampleRegularGridContext(context.Background(), RegularGridLinear)
	if err != nil {
		t.Fatal(err)
	}
	if grid.Dimensions != [3]int{7, 6, 6} {
		t.Fatalf("grid %v", grid.Dimensions)
	}
	regular, err := vol.RegularGrid()
	if err != nil {
		t.Fatal(err)
	}
	reader, err := regular.AcquireReader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for z := 0; z < 6; z++ {
		for y := 0; y < 6; y++ {
			for x := 0; x < 7; x++ {
				want, _ := reader.SampleAt(Vec3{X: float64(x), Y: float64(y), Z: float64(z)}, InterpolationNearest)
				if got := linear[(z*6+y)*7+x]; got != float32(want) {
					t.Fatalf("linear voxel (%d,%d,%d) = %v, RegularGrid %v", x, y, z, got, want)
				}
			}
		}
	}
	cubic, cubicGrid, err := vol.ResampleRegularGridContext(context.Background(), RegularGridCubicBSpline)
	if err != nil {
		t.Fatal(err)
	}
	if cubicGrid.Dimensions != grid.Dimensions || cubicGrid.VoxelToPatientAffine != grid.VoxelToPatientAffine {
		t.Fatal("the kernels planned different grids")
	}
	for gridZ, sourceZ := range map[int]int{0: 0, 1: 1, 2: 2, 4: 3, 5: 4} {
		for y := 0; y < 6; y++ {
			for x := 0; x < 7; x++ {
				want := float64(stack.Frames[sourceZ].PixelBytes[y*7+x])
				if got := float64(cubic[(gridZ*6+y)*7+x]); math.Abs(got-want) > 1e-3 {
					t.Fatalf("cubic voxel (%d,%d,%d) = %v, source slice %d %v", x, y, gridZ, got, sourceZ, want)
				}
			}
		}
	}
	differs := false
	for i := 3 * 6 * 7; i < 4*6*7; i++ {
		differs = differs || cubic[i] != linear[i]
	}
	if !differs {
		t.Fatal("the cubic B-spline slice in the gap equals the linear one")
	}
	if _, _, err := vol.ResampleRegularGridContext(context.Background(), RegularGridInterpolation(9)); err == nil {
		t.Fatal("an unknown kernel was accepted")
	}
}
