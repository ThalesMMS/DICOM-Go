package mesh

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"testing"
)

// TestClosestOnTriangleIsTheNearestPoint compares the Voronoi-region
// closest point with a dense sampling of random triangles.
func TestClosestOnTriangleIsTheNearestPoint(t *testing.T) {
	rng := rand.New(rand.NewSource(1254))
	random := func() [3]float64 { return [3]float64{rng.Float64()*4 - 2, rng.Float64()*4 - 2, rng.Float64()*4 - 2} }
	for trial := 0; trial < 300; trial++ {
		a, b, c, p := random(), random(), random(), scale3(random(), 2)
		q, w := closestOnTriangle(p, a, b, c)
		if w[0] < 0 || w[1] < 0 || w[2] < 0 || math.Abs(w[0]+w[1]+w[2]-1) > 1e-12 {
			t.Fatalf("weights %v", w)
		}
		at := add3(add3(scale3(a, w[0]), scale3(b, w[1])), scale3(c, w[2]))
		if distance3(at, q) > 1e-9 {
			t.Fatalf("weights give %v, point %v", at, q)
		}
		d := distance3(p, q)
		const steps = 60
		for i := 0; i <= steps; i++ {
			for j := 0; i+j <= steps; j++ {
				u, v := float64(i)/steps, float64(j)/steps
				s := add3(add3(scale3(a, 1-u-v), scale3(b, u)), scale3(c, v))
				if distance3(p, s) < d-1e-9 {
					t.Fatalf("trial %d: sample %v is nearer (%.9f) than %v (%.9f)", trial, s, distance3(p, s), q, d)
				}
			}
		}
	}
	// A degenerate triangle is a segment.
	q, _ := closestOnTriangle([3]float64{1, 1, 0}, [3]float64{0, 0, 0}, [3]float64{2, 0, 0}, [3]float64{4, 0, 0})
	if q != [3]float64{1, 0, 0} {
		t.Fatalf("degenerate triangle: %v", q)
	}
}

// TestLocatorMatchesBruteForce: the hierarchy finds the same nearest
// distance as a scan of every triangle.
func TestLocatorMatchesBruteForce(t *testing.T) {
	block := pittedBlock(t)
	locator, err := NewLocator(block)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 200; trial++ {
		p := [3]float64{rng.Float64()*50 - 5, rng.Float64()*50 - 5, rng.Float64()*40 - 5}
		s, ok := locator.ClosestPoint(p)
		if !ok {
			t.Fatal("no closest point")
		}
		want := math.Inf(1)
		for _, tri := range block.Triangles {
			q, _ := closestOnTriangle(p, block.point(tri[0]), block.point(tri[1]), block.point(tri[2]))
			want = math.Min(want, distance3(p, q))
		}
		if math.Abs(s.Distance-want) > 1e-12 {
			t.Fatalf("point %v: locator %.12f, brute force %.12f", p, s.Distance, want)
		}
	}
}

// TestLocatorSignedDistanceAndRays on a sphere: points at known radii read
// their radial distance (within the polyhedron's sag) with the sign of
// their side, including directions through vertices and edges, and rays
// from outside hit the near side.
func TestLocatorSignedDistanceAndRays(t *testing.T) {
	const radius = 20.0
	sphere := icosphere(radius, 4, [3]float64{})
	locator, err := NewLocator(sphere)
	if err != nil {
		t.Fatal(err)
	}
	sag := 0.05 // the polyhedron lies within 0.05 mm inside the sphere
	directions := [][3]float64{}
	for _, v := range sphere.Vertices[:40] {
		directions = append(directions, [3]float64{float64(v[0]), float64(v[1]), float64(v[2])})
	}
	for _, tri := range sphere.Triangles[:40] {
		directions = append(directions, add3(sphere.point(tri[0]), sphere.point(tri[1]))) // edge midpoints
	}
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 200; i++ {
		directions = append(directions, [3]float64{rng.NormFloat64(), rng.NormFloat64(), rng.NormFloat64()})
	}
	for _, d := range directions {
		u := scale3(d, 1/math.Sqrt(dot3(d, d)))
		for _, r := range []float64{5, 15, 19.5, 20.5, 26, 40} {
			got, _, ok := locator.SignedDistance(scale3(u, r))
			if !ok {
				t.Fatal("no distance")
			}
			want := r - radius
			if got < want-1e-9 || got > want+sag {
				t.Fatalf("r=%v along %v: signed distance %.5f, want %.5f (+%v)", r, u, got, want, sag)
			}
		}
		hit, ok := locator.RayHit(scale3(u, 60), scale3(u, -1), 0)
		if !ok || hit.Distance < 40-1e-9 || hit.Distance > 40+sag {
			t.Fatalf("ray along %v: %+v %v", u, hit, ok)
		}
		if v := hit.NearestVertex(sphere); int(v) >= len(sphere.Vertices) {
			t.Fatalf("nearest vertex %d", v)
		}
	}
	if _, ok := locator.RayHit([3]float64{60, 60, 0}, [3]float64{0, 0, 1}, 0); ok {
		t.Fatal("a ray beside the sphere hit it")
	}
	if _, ok := locator.RayHit([3]float64{60, 0, 0}, [3]float64{-1, 0, 0}, 10); ok {
		t.Fatal("a ray stopped short of the sphere hit it")
	}
}

// TestSurfaceDistancesBetweenConcentricSpheres (#1254): spheres of radius
// 20 and 23 mm are 3 mm apart everywhere, A inside B.
func TestSurfaceDistancesBetweenConcentricSpheres(t *testing.T) {
	a := icosphere(20, 4, [3]float64{})
	b := icosphere(23, 4, [3]float64{})
	report, err := SurfaceDistances(context.Background(), a, b, SurfaceDistanceOptions{Signed: true})
	if err != nil {
		t.Fatal(err)
	}
	for i, d := range report.AToB {
		if d > -2.95 || d < -3-1e-9 {
			t.Fatalf("A vertex %d: signed distance %.4f, want -3 (inside B)", i, d)
		}
	}
	for i, d := range report.BToA {
		if d < 2.95 || d > 3.06 {
			t.Fatalf("B vertex %d: signed distance %.4f, want +3 (outside A)", i, d)
		}
	}
	for _, v := range []float64{report.Hausdorff, report.MeanSurfaceDistance, report.AToBSummary.Mean, report.BToASummary.RMS, report.AToBSummary.Percentile95} {
		if math.Abs(v-3) > 0.06 {
			t.Fatalf("summary %.4f, want 3: %+v", v, report)
		}
	}
	self, err := SurfaceDistances(context.Background(), a, a, SurfaceDistanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if self.Hausdorff != 0 || self.MeanSurfaceDistance != 0 {
		t.Fatalf("a mesh against itself: %+v", self)
	}
}

// TestSurfaceDistancesOfAShiftedSphere (#1254): a 20 mm sphere against
// itself shifted 3 mm along x. The Hausdorff distance is the shift and the
// mean distance the analytic surface average of | |p - c| - R |.
func TestSurfaceDistancesOfAShiftedSphere(t *testing.T) {
	const radius, shift = 20.0, 3.0
	a := icosphere(radius, 5, [3]float64{})
	b := icosphere(radius, 5, [3]float64{shift, 0, 0})
	report, err := SurfaceDistances(context.Background(), a, b, SurfaceDistanceOptions{Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	// Area is uniform in cos θ on a sphere.
	const steps = 200000
	mean := 0.0
	for i := 0; i < steps; i++ {
		u := -1 + 2*(float64(i)+0.5)/steps
		mean += math.Abs(math.Sqrt(radius*radius+shift*shift-2*radius*shift*u)-radius) / steps
	}
	t.Logf("Hausdorff %.4f mm (3), mean %.4f mm (analytic %.4f), 95th percentile %.4f", report.Hausdorff, report.MeanSurfaceDistance, mean, report.AToBSummary.Percentile95)
	if math.Abs(report.Hausdorff-shift) > 0.02 {
		t.Fatalf("Hausdorff %.4f, want %.1f", report.Hausdorff, shift)
	}
	if math.Abs(report.MeanSurfaceDistance-mean) > 0.01*mean+0.01 {
		t.Fatalf("mean %.4f, analytic %.4f", report.MeanSurfaceDistance, mean)
	}
	if math.Abs(report.AToBSummary.Mean-report.BToASummary.Mean) > 0.01 {
		t.Fatalf("directed means %.4f and %.4f differ for a symmetric pair", report.AToBSummary.Mean, report.BToASummary.Mean)
	}
	again, err := SurfaceDistances(context.Background(), a, b, SurfaceDistanceOptions{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	for i := range again.AToB {
		if again.AToB[i] != report.AToB[i] {
			t.Fatal("distances depend on the worker count")
		}
	}
}

func TestSurfaceDistanceFailures(t *testing.T) {
	a := icosphere(10, 2, [3]float64{})
	if _, err := SurfaceDistances(context.Background(), a, &Mesh{}, SurfaceDistanceOptions{}); !errors.Is(err, ErrInvalidField) {
		t.Fatalf("empty mesh: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := SurfaceDistances(ctx, a, a, SurfaceDistanceOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	if _, ok := (&Locator{m: &Mesh{}}).ClosestPoint([3]float64{}); ok {
		t.Fatal("closest point of an empty mesh")
	}
}
