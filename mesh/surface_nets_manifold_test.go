package mesh

import (
	"context"
	"math"
	"math/rand"
	"testing"
)

// manifoldDefects counts the edges of m used by more than two triangles and
// the vertices whose triangles form more than one fan (sets of triangles
// joined through shared edges around the vertex).
func manifoldDefects(m *Mesh) (edges, vertices int) {
	incidence := map[uint64]int{}
	around := make([][]uint32, len(m.Vertices))
	for i, t := range m.Triangles {
		for k := 0; k < 3; k++ {
			incidence[edgeID(t[k], t[(k+1)%3])]++
			around[t[k]] = append(around[t[k]], uint32(i))
		}
	}
	for _, count := range incidence {
		if count > 2 {
			edges++
		}
	}
	for v, triangles := range around {
		if len(triangles) == 0 {
			continue
		}
		// Join the triangles around v that share an edge from v.
		parent := make([]int, len(triangles))
		for i := range parent {
			parent[i] = i
		}
		var find func(int) int
		find = func(i int) int {
			for parent[i] != i {
				parent[i] = parent[parent[i]]
				i = parent[i]
			}
			return i
		}
		first := map[uint32]int{}
		for i, ti := range triangles {
			for _, u := range m.Triangles[ti] {
				if u == uint32(v) {
					continue
				}
				if j, ok := first[u]; ok {
					parent[find(i)] = find(j)
				} else {
					first[u] = i
				}
			}
		}
		fans := 0
		for i := range parent {
			if find(i) == i {
				fans++
			}
		}
		if fans > 1 {
			vertices++
		}
	}
	return edges, vertices
}

// requireManifoldLabels checks that every label's surface is closed and a
// manifold: at most two triangles per edge and one fan around each vertex.
func requireManifoldLabels(t *testing.T, name string, surface *LabelSurface) {
	t.Helper()
	if edges, _ := manifoldDefects(surface.Mesh); edges != 0 && len(surface.Labels) == 1 {
		t.Fatalf("%s: %d edges of the surface have more than two triangles", name, edges)
	}
	for _, label := range surface.Labels {
		m := surface.LabelMesh(label)
		requireClosed(t, name, m)
		if edges, vertices := manifoldDefects(m); edges != 0 || vertices != 0 {
			t.Fatalf("%s: label %d has %d non-manifold edges and %d non-manifold vertices", name, label, edges, vertices)
		}
	}
}

// Two voxels of one label touching only along an edge or at a corner were
// joined by one vertex per dual cell, which put four triangles on an edge
// (or two fans around a vertex); each now gets its own vertices.
func TestSurfaceNetsEdgeAndCornerContactsAreManifold(t *testing.T) {
	for _, tc := range []struct {
		name     string
		voxels   map[[3]int]uint32
		vertices int
	}{
		{"edge", map[[3]int]uint32{{1, 1, 1}: 1, {2, 2, 1}: 1}, 16},
		{"corner", map[[3]int]uint32{{1, 1, 1}: 1, {2, 2, 2}: 1}, 16},
		{"edge between columns", map[[3]int]uint32{{1, 1, 1}: 1, {1, 1, 2}: 1, {2, 2, 1}: 1, {2, 2, 2}: 1}, 24},
		{"edge of two labels", map[[3]int]uint32{{1, 1, 1}: 1, {2, 2, 1}: 2}, 16},
		{"corner of two labels", map[[3]int]uint32{{1, 1, 1}: 2, {2, 2, 2}: 1}, 16},
	} {
		f := labelField([3]int{4, 4, 4}, [3]float64{1, 1, 1}, func(x, y, z int) uint32 { return tc.voxels[[3]int{x, y, z}] })
		for _, iterations := range []int{-1, 0} {
			surface, err := ExtractLabels(context.Background(), f, SurfaceNetsOptions{Iterations: iterations})
			if err != nil {
				t.Fatal(err)
			}
			if surface.UnresolvedCells != 0 {
				t.Fatalf("%s: %d unresolved cells", tc.name, surface.UnresolvedCells)
			}
			if edges, vertices := manifoldDefects(surface.Mesh); edges != 0 || vertices != 0 {
				t.Fatalf("%s: %d non-manifold edges and %d non-manifold vertices", tc.name, edges, vertices)
			}
			requireManifoldLabels(t, tc.name, surface)
			if got := len(surface.Mesh.Vertices); got != tc.vertices {
				t.Fatalf("%s: %d vertices, want %d", tc.name, got, tc.vertices)
			}
			if iterations < 0 {
				for label, count := range voxelCounts(f) {
					if got := signedVolume(surface.LabelMesh(label)); math.Abs(got-float64(count)) > 1e-9 {
						t.Fatalf("%s: label %d volume %g, want %d", tc.name, label, got, count)
					}
				}
			}
		}
	}
}

// Random segmentations: one label is always resolved, and with several
// labels UnresolvedCells is zero exactly when every label's surface is a
// manifold.
func TestSurfaceNetsRandomLabelsAreManifoldOrCounted(t *testing.T) {
	random := rand.New(rand.NewSource(2026))
	unresolved := 0
	for trial := 0; trial < 300; trial++ {
		size := [3]int{2 + random.Intn(6), 2 + random.Intn(6), 2 + random.Intn(6)}
		labels := 1 + trial%4
		density := 0.2 + 0.6*random.Float64()
		f := labelField(size, [3]float64{1, 1, 1}, func(x, y, z int) uint32 {
			if random.Float64() >= density {
				return 0
			}
			return uint32(1 + random.Intn(labels))
		})
		surface, err := ExtractLabels(context.Background(), f, SurfaceNetsOptions{Iterations: -1})
		if err != nil {
			t.Fatal(err)
		}
		defects := 0
		for label, count := range voxelCounts(f) {
			m := surface.LabelMesh(label)
			requireClosed(t, "random", m)
			if got := signedVolume(m); math.Abs(got-float64(count)) > 1e-6 {
				t.Fatalf("trial %d label %d volume %g, want %d", trial, label, got, count)
			}
			edges, vertices := manifoldDefects(m)
			defects += edges + vertices
		}
		if labels == 1 && (defects != 0 || surface.UnresolvedCells != 0) {
			t.Fatalf("trial %d: one label with %d defects, %d unresolved cells", trial, defects, surface.UnresolvedCells)
		}
		if (defects == 0) != (surface.UnresolvedCells == 0) {
			t.Fatalf("trial %d (%d labels): %d manifold defects but %d unresolved cells", trial, labels, defects, surface.UnresolvedCells)
		}
		if surface.UnresolvedCells != 0 {
			unresolved++
		}
	}
	t.Logf("%d of 300 random volumes keep unresolved junctions", unresolved)
}

// A label whose two voxels touch along an edge while a second label fills
// one side and the background the other: both cells around the edge join
// the two pairs of quads, so the edge keeps four of the label's triangles
// and the cells are counted.
func TestSurfaceNetsCountsUnresolvedJunctions(t *testing.T) {
	voxels := map[[3]int]uint32{{0, 0, 0}: 1, {1, 1, 0}: 1, {1, 0, 0}: 2}
	f := labelField([3]int{2, 2, 1}, [3]float64{1, 1, 1}, func(x, y, z int) uint32 { return voxels[[3]int{x, y, z}] })
	surface, err := ExtractLabels(context.Background(), f, SurfaceNetsOptions{Iterations: -1})
	if err != nil {
		t.Fatal(err)
	}
	edges, vertices := manifoldDefects(surface.LabelMesh(1))
	t.Logf("label 1: %d non-manifold edges, %d non-manifold vertices; %d unresolved cells", edges, vertices, surface.UnresolvedCells)
	if surface.UnresolvedCells == 0 || edges+vertices == 0 {
		t.Fatalf("%d unresolved cells for %d edge and %d vertex defects", surface.UnresolvedCells, edges, vertices)
	}
	for label := range voxelCounts(f) {
		requireClosed(t, "junction", surface.LabelMesh(label))
	}
}
