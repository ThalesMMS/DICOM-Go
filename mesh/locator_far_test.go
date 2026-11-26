package mesh

import (
	"math"
	"testing"
)

// A query so far away that every squared distance overflows to +Inf was
// pruned at the root (box distance >= +Inf), so ClosestPoint reported no
// point on a mesh with triangles. It now returns the nearest triangle.
func TestLocatorFarQueryReturnsTheNearestPoint(t *testing.T) {
	// Two unit squares facing x, at x = 0 and x = 10.
	m := &Mesh{
		Vertices: [][3]float32{
			{0, 0, 0}, {0, 1, 0}, {0, 1, 1}, {0, 0, 1},
			{10, 0, 0}, {10, 1, 0}, {10, 1, 1}, {10, 0, 1},
		},
		Triangles: [][3]uint32{{0, 1, 2}, {0, 2, 3}, {4, 5, 6}, {4, 6, 7}},
	}
	locator, err := NewLocator(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		p        [3]float64
		x        float64
		distance float64
	}{
		{[3]float64{1e200, 0.5, 0.5}, 10, 1e200 - 10},
		{[3]float64{-1e200, 0.5, 0.5}, 0, 1e200},
		{[3]float64{math.MaxFloat64, 0.25, 0.75}, 10, math.MaxFloat64},
		{[3]float64{-math.MaxFloat64, 2, 3}, 0, math.MaxFloat64},
		{[3]float64{3e160, 3e160, 0.5}, 10, math.Hypot(3e160-10, 3e160-1)},
		// Within range the result is unchanged.
		{[3]float64{11, 0.5, 0.5}, 10, 1},
	} {
		s, ok := locator.ClosestPoint(tc.p)
		if !ok {
			t.Fatalf("query %v: no closest point", tc.p)
		}
		if s.Point[0] != tc.x || s.Triangle/2 != uint32(tc.x/10) {
			t.Fatalf("query %v: point %v on triangle %d, want the square at x = %v", tc.p, s.Point, s.Triangle, tc.x)
		}
		if math.IsInf(s.Distance, 0) || math.Abs(s.Distance-tc.distance) > 1e-12*tc.distance {
			t.Fatalf("query %v: distance %g, want %g", tc.p, s.Distance, tc.distance)
		}
		if w := s.Barycentric; math.Abs(w[0]+w[1]+w[2]-1) > 1e-12 {
			t.Fatalf("query %v: weights %v", tc.p, w)
		}
		if d, _, ok := locator.SignedDistance(tc.p); !ok || math.Abs(d) != s.Distance {
			t.Fatalf("query %v: signed distance %g, %t", tc.p, d, ok)
		}
	}
	// A distance beyond the float64 range is +Inf, at the nearest point.
	s, ok := locator.ClosestPoint([3]float64{math.MaxFloat64, math.MaxFloat64, 0})
	if !ok || s.Point[0] != 10 || !math.IsInf(s.Distance, 1) {
		t.Fatalf("overflowing distance: %+v %t", s, ok)
	}
}

// Queries with a NaN or infinite coordinate have no nearest point.
func TestLocatorRejectsNonFiniteQueries(t *testing.T) {
	locator, err := NewLocator(&Mesh{Vertices: [][3]float32{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}}, Triangles: [][3]uint32{{0, 1, 2}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range [][3]float64{
		{math.NaN(), 0, 0}, {0, math.Inf(1), 0}, {0, 0, math.Inf(-1)}, {math.Inf(1), math.NaN(), 1},
	} {
		if s, ok := locator.ClosestPoint(p); ok {
			t.Fatalf("query %v: closest point %+v", p, s)
		}
		if d, _, ok := locator.SignedDistance(p); ok {
			t.Fatalf("query %v: signed distance %g", p, d)
		}
	}
}
