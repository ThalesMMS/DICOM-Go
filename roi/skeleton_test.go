package roi

import (
	"context"
	"errors"
	"math"
	"testing"
)

// tubeMask fills the voxels within radius (mm) of the polyline points.
func tubeMask(columns, rows, slices int, spacing [3]float64, radius float64, polyline ...[3]float64) *VolumeMask {
	m := NewVolumeMask(columns, rows, slices, spacing)
	for z := 0; z < slices; z++ {
		for y := 0; y < rows; y++ {
			for x := 0; x < columns; x++ {
				p := [3]float64{m.X[x], m.Y[y], m.Z[z]}
				if polylineDistance(p, polyline) <= radius {
					m.Voxels[m.index(x, y, z)] = true
				}
			}
		}
	}
	return m
}

func polylineDistance(p [3]float64, polyline [][3]float64) float64 {
	best := math.Inf(1)
	for i := 1; i < len(polyline); i++ {
		a, b := polyline[i-1], polyline[i]
		var ab, ap [3]float64
		dot, length := 0.0, 0.0
		for k := 0; k < 3; k++ {
			ab[k], ap[k] = b[k]-a[k], p[k]-a[k]
			dot += ab[k] * ap[k]
			length += ab[k] * ab[k]
		}
		t := 0.0
		if length > 0 {
			t = math.Max(0, math.Min(1, dot/length))
		}
		d := 0.0
		for k := 0; k < 3; k++ {
			d += sq(a[k] + t*ab[k] - p[k])
		}
		best = math.Min(best, math.Sqrt(d))
	}
	return best
}

func skeletonVoxels(m *VolumeMask) [][3]int {
	var out [][3]int
	for i, v := range m.Voxels {
		if v {
			out = append(out, [3]int{i % m.Columns, i / m.Columns % m.Rows, i / (m.Columns * m.Rows)})
		}
	}
	return out
}

func (m *VolumeMask) mm(v [3]int) [3]float64 { return [3]float64{m.X[v[0]], m.Y[v[1]], m.Z[v[2]]} }

// requireCurve checks a thinned skeleton: one 26-connected component whose
// voxels lie within tolerance of the true centreline.
func requireCurve(t *testing.T, skeleton *VolumeMask, centreline [][3]float64, tolerance float64) {
	t.Helper()
	voxels := skeletonVoxels(skeleton)
	if len(voxels) == 0 {
		t.Fatal("empty skeleton")
	}
	for _, v := range voxels {
		if d := polylineDistance(skeleton.mm(v), centreline); d > tolerance {
			t.Fatalf("skeleton voxel %v is %.2f mm off the centreline", v, d)
		}
	}
	g := newSkeletonGraph(skeleton)
	dist, _, err := g.dijkstra(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for n, d := range dist {
		if math.IsInf(d, 1) {
			t.Fatalf("skeleton voxel %v is disconnected", skeleton.mm(skeletonVoxels(skeleton)[n]))
		}
	}
}

func TestThinStraightAndObliqueTubesGiveCentredOneVoxelCurves(t *testing.T) {
	for name, c := range map[string]struct {
		mask       *VolumeMask
		centreline [][3]float64
	}{
		"axial cylinder": {
			tubeMask(21, 21, 40, [3]float64{1, 1, 1}, 4, [3]float64{10, 10, 3}, [3]float64{10, 10, 36}),
			[][3]float64{{10, 10, 3}, {10, 10, 36}},
		},
		"oblique anisotropic tube": {
			tubeMask(40, 36, 24, [3]float64{0.8, 0.8, 1.5}, 3.5, [3]float64{5, 5, 5}, [3]float64{26, 22, 30}),
			[][3]float64{{5, 5, 5}, {26, 22, 30}},
		},
		"bent tube": {
			tubeMask(40, 40, 16, [3]float64{1, 1, 1}, 3, [3]float64{4, 8, 8}, [3]float64{20, 8, 8}, [3]float64{20, 32, 8}, [3]float64{34, 32, 8}),
			[][3]float64{{4, 8, 8}, {20, 8, 8}, {20, 32, 8}, {34, 32, 8}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			skeleton, err := c.mask.Thin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			requireCurve(t, skeleton, c.centreline, 1.5)
			pruned, err := PruneSkeleton(context.Background(), skeleton, 5)
			if err != nil {
				t.Fatal(err)
			}
			// A single curve has exactly two ends and no branch point.
			g := newSkeletonGraph(pruned)
			removed := make([]bool, len(g.nodes))
			ends := 0
			for n := range g.nodes {
				switch d := g.degree(n, removed); {
				case d == 1:
					ends++
				case d > 2:
					t.Fatalf("pruned curve has a voxel of degree %d", d)
				}
			}
			if ends != 2 {
				t.Fatalf("pruned curve has %d ends", ends)
			}
			path, err := SkeletonPath(context.Background(), pruned, SkeletonPathOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(path) != len(g.nodes) {
				t.Fatalf("path has %d of %d curve voxels", len(path), len(g.nodes))
			}
			first, last := pruned.mm(path[0]), pruned.mm(path[len(path)-1])
			a, b := c.centreline[0], c.centreline[len(c.centreline)-1]
			// Thinning stops about a tube radius short of each rounded cap.
			if !(near(first, a, 5) && near(last, b, 5) || near(first, b, 5) && near(last, a, 5)) {
				t.Fatalf("path ends %v..%v, want near %v and %v", first, last, a, b)
			}
		})
	}
}

func near(a, b [3]float64, tolerance float64) bool {
	return math.Sqrt(sq(a[0]-b[0])+sq(a[1]-b[1])+sq(a[2]-b[2])) <= tolerance
}

func TestThinKeepsTopologyOfRing(t *testing.T) {
	m := NewVolumeMask(30, 30, 9, [3]float64{1, 1, 1})
	for z := 0; z < 9; z++ {
		for y := 0; y < 30; y++ {
			for x := 0; x < 30; x++ {
				r := math.Hypot(float64(x)-14.5, float64(y)-14.5)
				if math.Hypot(r-9, float64(z)-4) <= 2.5 {
					m.Voxels[m.index(x, y, z)] = true
				}
			}
		}
	}
	skeleton, err := m.Thin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pruned, err := PruneSkeleton(context.Background(), skeleton, 10)
	if err != nil {
		t.Fatal(err)
	}
	g := newSkeletonGraph(pruned)
	removed := make([]bool, len(g.nodes))
	for n := range g.nodes {
		if d := g.degree(n, removed); d != 2 {
			t.Fatalf("ring skeleton voxel %v has degree %d", pruned.mm(skeletonVoxels(pruned)[n]), d)
		}
		v := pruned.mm(skeletonVoxels(pruned)[n])
		if r := math.Hypot(v[0]-14.5, v[1]-14.5); math.Abs(r-9) > 1.5 || math.Abs(v[2]-4) > 1 {
			t.Fatalf("ring skeleton voxel %v is off the circle", v)
		}
	}
	if len(g.nodes) < 40 {
		t.Fatalf("ring skeleton has only %d voxels", len(g.nodes))
	}
}

func TestPruneSkeletonRemovesShortSpursAndKeepsBranches(t *testing.T) {
	// A long tube with a short side bud and a long side branch.
	m := tubeMask(50, 30, 13, [3]float64{1, 1, 1}, 2.5,
		[3]float64{4, 10, 6}, [3]float64{46, 10, 6})
	branch := tubeMask(50, 30, 13, [3]float64{1, 1, 1}, 2.5, [3]float64{30, 10, 6}, [3]float64{30, 26, 6})
	bud := tubeMask(50, 30, 13, [3]float64{1, 1, 1}, 2.5, [3]float64{15, 10, 6}, [3]float64{15, 15, 6})
	for i := range m.Voxels {
		m.Voxels[i] = m.Voxels[i] || branch.Voxels[i] || bud.Voxels[i]
	}
	skeleton, err := m.Thin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pruned, err := PruneSkeleton(context.Background(), skeleton, 6)
	if err != nil {
		t.Fatal(err)
	}
	endsOf := func(s *VolumeMask) [][3]float64 {
		g := newSkeletonGraph(s)
		removed := make([]bool, len(g.nodes))
		var ends [][3]float64
		for n, voxel := range g.nodes {
			if g.degree(n, removed) == 1 {
				ends = append(ends, s.mm([3]int{voxel % s.Columns, voxel / s.Columns % s.Rows, voxel / (s.Columns * s.Rows)}))
			}
		}
		return ends
	}
	if raw := endsOf(skeleton); len(raw) < 4 {
		t.Fatalf("thinning left no bud spur to prune: ends %v", raw)
	}
	ends := endsOf(pruned)
	if len(ends) != 3 {
		t.Fatalf("pruned skeleton ends %v, want the tube's two ends and the branch tip", ends)
	}
	for _, e := range ends {
		if e[1] > 11.5 && e[1] < 20 && math.Abs(e[0]-15) < 3 {
			t.Fatalf("the short bud survived at %v", e)
		}
	}
	// Between explicit ends the path follows the main tube.
	from, to := [3]float64{4, 10, 6}, [3]float64{46, 10, 6}
	path, err := SkeletonPath(context.Background(), pruned, SkeletonPathOptions{From: &from, To: &to})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range path {
		if p := pruned.mm(v); math.Abs(p[1]-10) > 1.5 || math.Abs(p[2]-6) > 1.5 {
			t.Fatalf("main path leaves the tube at %v", p)
		}
	}
	if !near(pruned.mm(path[0]), from, 4) || !near(pruned.mm(path[len(path)-1]), to, 4) {
		t.Fatalf("path runs %v..%v", pruned.mm(path[0]), pruned.mm(path[len(path)-1]))
	}
}

func TestSkeletonInputErrorsAndCancellation(t *testing.T) {
	if _, err := (&VolumeMask{}).Thin(context.Background()); !errors.Is(err, ErrInvalidVolumeMask) {
		t.Fatalf("invalid mask: %v", err)
	}
	m := tubeMask(12, 12, 12, [3]float64{1, 1, 1}, 3, [3]float64{6, 6, 1}, [3]float64{6, 6, 10})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Thin(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
	if _, err := PruneSkeleton(context.Background(), m, -1); !errors.Is(err, ErrInvalidVolumeMask) {
		t.Fatalf("negative branch length: %v", err)
	}
	empty := NewVolumeMask(4, 4, 4, [3]float64{1, 1, 1})
	if _, err := SkeletonPath(context.Background(), empty, SkeletonPathOptions{}); !errors.Is(err, ErrSkeletonPath) {
		t.Fatalf("empty skeleton: %v", err)
	}
	one := [3]float64{}
	if _, err := SkeletonPath(context.Background(), m, SkeletonPathOptions{From: &one}); !errors.Is(err, ErrInvalidVolumeMask) {
		t.Fatalf("one end: %v", err)
	}
	two := NewVolumeMask(10, 1, 1, [3]float64{1, 1, 1})
	two.Voxels[0], two.Voxels[9] = true, true
	a, b := [3]float64{0, 0, 0}, [3]float64{9, 0, 0}
	if _, err := SkeletonPath(context.Background(), two, SkeletonPathOptions{From: &a, To: &b}); !errors.Is(err, ErrSkeletonPath) {
		t.Fatalf("disconnected ends: %v", err)
	}
}
