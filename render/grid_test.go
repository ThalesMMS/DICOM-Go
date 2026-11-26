package render

import (
	"errors"
	"math"
	"testing"
)

func TestNearestIndexRoundsHalfIntegersUp(t *testing.T) {
	for _, test := range []struct {
		x    float64
		want int
	}{
		{-0.5, 0}, {-0.51, -1}, {-1.5, -1}, {-2.5, -2}, {0.49, 0}, {0.5, 1}, {2.5, 3}, {3.4999, 3},
	} {
		if got := NearestIndex(test.x); got != test.want {
			t.Errorf("NearestIndex(%v) = %d, want %d (math.Round gives %v)", test.x, got, test.want, math.Round(test.x))
		}
	}
}

// TestContinuousIndexInsideBorders pins ITK's half-open [-0.5, n-0.5) domain
// and empty axes (ITK 1295d23939, 63f4ab7dd8).
func TestContinuousIndexInsideBorders(t *testing.T) {
	for _, test := range []struct {
		x    float64
		n    int
		want bool
	}{
		{-0.5, 4, true}, {-0.5000001, 4, false}, {3.4999999, 4, true}, {3.5, 4, false},
		{0, 1, true}, {-0.5, 1, true}, {0.5, 1, false},
		{0, 0, false}, {-0.5, 0, false},
	} {
		if got := ContinuousIndexInside(test.x, test.n); got != test.want {
			t.Errorf("ContinuousIndexInside(%v, %d) = %v, want %v", test.x, test.n, got, test.want)
		}
	}
	if index, ok := NearestIndexInside(3.49, 4); !ok || index != 3 {
		t.Errorf("NearestIndexInside(3.49, 4) = %d, %v", index, ok)
	}
	if _, ok := NearestIndexInside(-0.6, 4); ok {
		t.Error("NearestIndexInside(-0.6, 4) is inside")
	}
	if v, ok := ClampContinuousIndex(-0.5, 4, 0); !ok || v != 0 {
		t.Errorf("ClampContinuousIndex(-0.5) = %v, %v", v, ok)
	}
	if v, ok := ClampContinuousIndex(3.5005, 4, 1e-3); !ok || v != 3 {
		t.Errorf("ClampContinuousIndex within tolerance = %v, %v", v, ok)
	}
	if _, ok := ClampContinuousIndex(3.5, 4, 0); ok {
		t.Error("ClampContinuousIndex(n-0.5) is inside without tolerance")
	}
}

func TestGridPointIndexRoundTripAndInside(t *testing.T) {
	// Oblique, anisotropic, non-orthogonal (sheared) grid.
	c, s := math.Cos(0.3), math.Sin(0.3)
	g, err := NewGrid([3]int{4, 3, 2}, Vec3{X: 10, Y: -5, Z: 2}, [3]float64{0.5, 0.75, 2},
		[3]Vec3{{X: c, Y: s}, {X: -s, Y: c}, Vec3{X: 0.1, Z: 1}.Normalize()})
	if err != nil {
		t.Fatal(err)
	}
	ci := Vec3{X: 2.25, Y: -0.5, Z: 1.4}
	got, ok := g.ContinuousIndex(g.IndexToPoint(ci))
	if !ok || !vecWithin(got, ci, 1e-12) {
		t.Fatalf("round trip = %v, want %v", got, ci)
	}
	index, inside := g.Index(g.IndexToPoint(ci))
	if index != [3]int{2, 0, 1} || !inside {
		t.Fatalf("Index = %v, %v", index, inside)
	}
	for _, test := range []struct {
		ci   Vec3
		want bool
	}{
		{Vec3{X: -0.5, Y: -0.5, Z: -0.5}, true},
		{Vec3{X: 3.49, Y: 2.49, Z: 1.49}, true},
		{Vec3{X: 3.5, Y: 0, Z: 0}, false},
		{Vec3{X: 0, Y: -0.51, Z: 0}, false},
	} {
		if got := g.IsInsideContinuousIndex(test.ci); got != test.want {
			t.Errorf("IsInsideContinuousIndex(%v) = %v, want %v", test.ci, got, test.want)
		}
	}
	// Points a hair inside and outside the half-voxel border.
	if !g.IsInside(g.IndexToPoint(Vec3{X: -0.5 + 1e-9, Y: 2.5 - 1e-9})) || g.IsInside(g.IndexToPoint(Vec3{X: 3.5 + 1e-9})) {
		t.Error("IsInside disagrees with the continuous index near the border")
	}
	empty := g
	empty.Size[2] = 0
	if empty.IsInside(g.IndexToPoint(Vec3{})) {
		t.Error("a grid of zero slices holds a point")
	}
}

// TestGridValidateRejectsDegenerateGeometry: non-positive spacing and
// singular directions (ITK c4960ee19c, 17a1266e8f).
func TestGridValidateRejectsDegenerateGeometry(t *testing.T) {
	axes := [3]Vec3{{X: 1}, {Y: 1}, {Z: 1}}
	for name, g := range map[string]Grid{
		"zero spacing":     {Size: [3]int{1, 1, 1}, Spacing: [3]float64{1, 0, 1}, Direction: axes},
		"negative spacing": {Size: [3]int{1, 1, 1}, Spacing: [3]float64{1, 1, -1}, Direction: axes},
		"NaN spacing":      {Size: [3]int{1, 1, 1}, Spacing: [3]float64{math.NaN(), 1, 1}, Direction: axes},
		"singular":         {Size: [3]int{1, 1, 1}, Spacing: [3]float64{1, 1, 1}, Direction: [3]Vec3{{X: 1}, {Y: 1}, {X: 1, Y: 1}}},
		"zero direction":   {Size: [3]int{1, 1, 1}, Spacing: [3]float64{1, 1, 1}, Direction: [3]Vec3{{X: 1}, {Y: 1}, {}}},
		"negative size":    {Size: [3]int{1, -1, 1}, Spacing: [3]float64{1, 1, 1}, Direction: axes},
	} {
		if err := g.Validate(); !errors.Is(err, ErrInvalidGrid) {
			t.Errorf("%s: Validate = %v", name, err)
		}
		if _, ok := g.ContinuousIndex(Vec3{}); ok && name == "singular" {
			t.Errorf("%s: ContinuousIndex succeeded", name)
		}
	}
	if err := (Grid{Size: [3]int{0, 0, 0}, Spacing: [3]float64{1, 1, 1}, Direction: axes}).Validate(); err != nil {
		t.Errorf("empty grid rejected: %v", err)
	}
}

func TestGridCongruenceScalesWithSpacing(t *testing.T) {
	axes := [3]Vec3{{X: 1}, {Y: 1}, {Z: 1}}
	g := Grid{Size: [3]int{4, 4, 4}, Spacing: [3]float64{0.5, 0.5, 2}, Direction: axes}
	near := g
	near.Origin = Vec3{X: 0.5 * 0.009}
	if !g.Congruent(near, 0.01, 1e-6) || g.Congruent(near, 0.001, 1e-6) {
		t.Error("origin tolerance is not a fraction of the spacing")
	}
	coarse := g
	coarse.Spacing = [3]float64{50, 50, 200}
	far := coarse
	far.Origin = Vec3{X: 0.4}
	if !coarse.Congruent(far, 0.01, 1e-6) {
		t.Error("0.4 mm on a 50 mm grid is not within 1% of a voxel")
	}
	turned := g
	turned.Direction[0] = Vec3{X: 1, Y: 1e-4}.Normalize()
	if g.Congruent(turned, 0.01, 1e-6) || !g.Congruent(turned, 0.01, 1e-3) {
		t.Error("direction tolerance not applied")
	}
	// A spacing difference accumulates across the grid: 1e-4 of a voxel per
	// voxel is 0.05 voxel at the far edge of 512 (ITK would compare it only
	// against the tolerance).
	wide := Grid{Size: [3]int{512, 512, 1}, Spacing: [3]float64{0.5, 0.5, 1}, Direction: axes}
	drift := wide
	drift.Spacing[0] = 0.5 * (1 + 2e-4)
	if wide.Congruent(drift, DICOMGridCoordinateTolerance, DICOMGridDirectionTolerance) {
		t.Error("spacing drift of 0.1 voxel at the far edge accepted")
	}
	drift.Spacing[0] = 0.5 * (1 + 5e-5)
	if !wide.Congruent(drift, DICOMGridCoordinateTolerance, DICOMGridDirectionTolerance) {
		t.Error("spacing drift of 0.026 voxel at the far edge rejected")
	}
	resized := g
	resized.Size[0] = 5
	if !g.Congruent(resized, 0.01, 1e-6) || g.SameGeometry(resized, 0.01, 1e-6) || !g.SameGeometry(g, 0, 0) {
		t.Error("SameGeometry must compare the size and Congruent must not")
	}
}

func TestVolumeGridMatchesVoxelToPatient(t *testing.T) {
	vol, err := BuildVolume(sphereStack(8, 3))
	if err != nil {
		t.Fatal(err)
	}
	g, ok := vol.Grid()
	if !ok {
		t.Fatal("regular volume has no grid")
	}
	p := Vec3{X: 1.25, Y: 6.5, Z: 3}
	if !vecWithin(g.IndexToPoint(p), vol.VoxelToPatient(p), 1e-9) {
		t.Fatalf("grid %v vs volume %v", g.IndexToPoint(p), vol.VoxelToPatient(p))
	}
}
