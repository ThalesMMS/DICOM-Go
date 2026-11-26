package roi

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestCompareMasksDiscretePhysicalCases(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		first  [][3]int
		second [][3]int
		want   OverlapMetrics
	}{
		{
			name:  "disjoint voxels offset on all axes",
			first: [][3]int{{0, 0, 0}}, second: [][3]int{{2, 1, 1}},
			// The centers are offset by (1, 2, 3) mm in physical space.
			want: OverlapMetrics{VolumeA: 3, VolumeB: 4, HausdorffMM: math.Sqrt(14), HD95MM: math.Sqrt(14), MeanSurfaceDistanceMM: math.Sqrt(14)},
		},
		{
			name:  "one shared voxel with unequal populations",
			first: [][3]int{{1, 1, 0}}, second: [][3]int{{1, 1, 0}, {2, 1, 0}, {3, 1, 0}},
			want: OverlapMetrics{Dice: 0.5, Jaccard: 1.0 / 3, VolumeA: 3, VolumeB: 9, HausdorffMM: 1, HD95MM: 1, MeanSurfaceDistanceMM: 0.375},
		},
		{
			name:  "opposite edges of irregular slice gap",
			first: [][3]int{{0, 0, 1}}, second: [][3]int{{0, 0, 2}},
			// Slice centers are 0, 3 and 8 mm. The middle extent is 4 mm
			// and the last extent is 5 mm, each with 1 mm² in-plane area.
			want: OverlapMetrics{VolumeA: 4, VolumeB: 5, HausdorffMM: 5, HD95MM: 5, MeanSurfaceDistanceMM: 5},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewVolumeMask(5, 3, 3, [3]float64{0.5, 2, 3})
			a.Z[2] = 8
			b := a.Clone()
			for _, p := range tc.first {
				a.Voxels[a.index(p[0], p[1], p[2])] = true
			}
			for _, p := range tc.second {
				b.Voxels[b.index(p[0], p[1], p[2])] = true
			}
			got, err := CompareMasks(ctx, a, b)
			if err != nil {
				t.Fatal(err)
			}
			assertPhysicalOverlapMetrics(t, got, tc.want)
			swapped, err := CompareMasks(ctx, b, a)
			if err != nil {
				t.Fatal(err)
			}
			want := tc.want
			want.VolumeA, want.VolumeB = want.VolumeB, want.VolumeA
			assertPhysicalOverlapMetrics(t, swapped, want)
		})
	}
}

func TestCompareMasksSurfaceMetrics(t *testing.T) {
	ctx := context.Background()
	t.Run("solid interior is omitted from both directed distance samples", func(t *testing.T) {
		a := NewVolumeMask(5, 5, 5, [3]float64{1, 1, 1})
		b := a.Clone()
		for z := 1; z <= 3; z++ {
			for y := 1; y <= 3; y++ {
				for x := 1; x <= 3; x++ {
					a.Voxels[a.index(x, y, z)] = true
				}
			}
		}
		b.Voxels[b.index(2, 2, 2)] = true
		got, err := CompareMasks(ctx, a, b)
		if err != nil {
			t.Fatal(err)
		}
		// The cube has six face centers 1 mm away, twelve edge voxels
		// √2 mm away and eight corners √3 mm away. In the reverse
		// direction the central voxel is 1 mm from the nearest face.
		assertPhysicalOverlapMetrics(t, got, OverlapMetrics{
			Dice: 1.0 / 14, Jaccard: 1.0 / 27, VolumeA: 27, VolumeB: 1,
			HausdorffMM: math.Sqrt(3), HD95MM: math.Sqrt(3),
			MeanSurfaceDistanceMM: (7 + 12*math.Sqrt2 + 8*math.Sqrt(3)) / 27,
		})
	})
	t.Run("HD95 uses the larger directed nearest rank", func(t *testing.T) {
		a := NewVolumeMask(50, 3, 2, [3]float64{1, 2, 1.5})
		b := a.Clone()
		for x := 4; x <= 21; x++ {
			a.Voxels[a.index(x, 1, 0)] = true
			b.Voxels[b.index(x, 1, 0)] = true
		}
		for _, x := range []int{22, 23, 49} {
			a.Voxels[a.index(x, 1, 0)] = true
		}
		b.Voxels[b.index(0, 1, 0)] = true
		got, err := CompareMasks(ctx, a, b)
		if err != nil {
			t.Fatal(err)
		}
		// Forward has 18 zeros and 1, 2, 28 mm (rank 20 = 2 mm).
		// Reverse has 18 zeros and 4 mm (rank 19 = 4 mm).
		assertPhysicalOverlapMetrics(t, got, OverlapMetrics{
			Dice: 0.9, Jaccard: 18.0 / 22, VolumeA: 63, VolumeB: 57,
			HausdorffMM: 28, HD95MM: 4, MeanSurfaceDistanceMM: 35.0 / 40,
		})
	})
}

func TestCompareMasksRefusesInvalidGridAndUnavailableSurface(t *testing.T) {
	a := NewVolumeMask(4, 3, 2, [3]float64{0.5, 1.5, 2.5})
	a.Voxels[0] = true
	for _, tc := range []struct {
		name   string
		change func(*VolumeMask)
		want   error
	}{
		{"offset axis", func(m *VolumeMask) { m.Z[0], m.Z[1] = 1, 3.5 }, ErrInvalidVolumeMask},
		{"changed spacing", func(m *VolumeMask) { m.X[1] = 0.6 }, ErrInvalidVolumeMask},
		{"repeated coordinate", func(m *VolumeMask) { m.Y[1] = m.Y[0] }, ErrInvalidVolumeMask},
		{"nonfinite coordinate", func(m *VolumeMask) { m.X[3] = math.NaN() }, ErrInvalidVolumeMask},
		{"truncated voxel storage", func(m *VolumeMask) { m.Voxels = m.Voxels[1:] }, ErrInvalidVolumeMask},
		{"second mask empty", func(m *VolumeMask) { clear(m.Voxels) }, ErrEmptyMask},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := a.Clone()
			tc.change(b)
			if _, err := CompareMasks(context.Background(), a, b); !errors.Is(err, tc.want) {
				t.Fatalf("CompareMasks error = %v, want %v", err, tc.want)
			}
		})
	}
	t.Run("both masks empty", func(t *testing.T) {
		b := a.Clone()
		clear(b.Voxels)
		if _, err := CompareMasks(context.Background(), b, b.Clone()); !errors.Is(err, ErrEmptyMask) {
			t.Fatalf("CompareMasks error = %v, want %v", err, ErrEmptyMask)
		}
	})
	t.Run("cancelled comparison leaves both masks available", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		b := a.Clone()
		if _, err := CompareMasks(ctx, a, b); !errors.Is(err, context.Canceled) {
			t.Fatalf("CompareMasks error = %v, want context.Canceled", err)
		}
		if !reflect.DeepEqual(a, b) || a.Count() != 1 || !a.Voxels[0] {
			t.Fatalf("comparison modified its inputs: %v and %v", a.Voxels, b.Voxels)
		}
	})
}

func assertPhysicalOverlapMetrics(t *testing.T, got, want OverlapMetrics) {
	t.Helper()
	for _, value := range []struct {
		name      string
		got, want float64
	}{
		{"Dice", got.Dice, want.Dice}, {"Jaccard", got.Jaccard, want.Jaccard},
		{"volume A", got.VolumeA, want.VolumeA}, {"volume B", got.VolumeB, want.VolumeB},
		{"Hausdorff", got.HausdorffMM, want.HausdorffMM}, {"HD95", got.HD95MM, want.HD95MM},
		{"mean surface distance", got.MeanSurfaceDistanceMM, want.MeanSurfaceDistanceMM},
	} {
		if !(math.Abs(value.got-value.want) <= 1e-12) {
			t.Errorf("%s = %.15g, want %.15g", value.name, value.got, value.want)
		}
	}
}

func TestDistanceTransformBackgroundOnThinAndUniformGrids(t *testing.T) {
	for _, tc := range []struct {
		name    string
		size    [3]int
		targets [][3]int
		lookFor bool
	}{
		{"depth one", [3]int{4, 5, 1}, [][3]int{{2, 2, 0}}, false},
		{"width one with two sites", [3]int{1, 4, 3}, [][3]int{{0, 1, 2}, {0, 3, 0}}, false},
		{"height one", [3]int{4, 1, 3}, [][3]int{{0, 0, 2}}, false},
		{"singleton is target", [3]int{1, 1, 1}, [][3]int{{0, 0, 0}}, false},
		{"singleton has no target", [3]int{1, 1, 1}, nil, true},
		{"full has no background target", [3]int{3, 2, 2}, nil, false},
		{"empty has no foreground target", [3]int{3, 2, 2}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mask := NewVolumeMask(tc.size[0], tc.size[1], tc.size[2], [3]float64{1, 2, 4})
			for i := range mask.Voxels {
				mask.Voxels[i] = !tc.lookFor
			}
			for _, p := range tc.targets {
				mask.Voxels[mask.index(p[0], p[1], p[2])] = tc.lookFor
			}
			got, err := mask.DistanceTransform(context.Background(), tc.lookFor)
			if err != nil || len(got) != len(mask.Voxels) {
				t.Fatalf("DistanceTransform returned %d samples, err = %v", len(got), err)
			}
			for z := range tc.size[2] {
				for y := range tc.size[1] {
					for x := range tc.size[0] {
						want := math.Inf(1)
						for _, p := range tc.targets {
							dx, dy, dz := float64(x-p[0]), float64(2*(y-p[1])), float64(4*(z-p[2]))
							want = math.Min(want, math.Hypot(math.Hypot(dx, dy), dz))
						}
						if value := got[mask.index(x, y, z)]; value != want && !(math.Abs(value-want) <= 1e-12) {
							t.Errorf("distance at (%d, %d, %d) = %g, want %g", x, y, z, value, want)
						}
					}
				}
			}
		})
	}
}
