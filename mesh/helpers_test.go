package mesh

import (
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// Test helpers: exact topology checks and mass properties.

type edgeKey struct{ a, b uint32 }

// topology counts, over the directed triangle edges, the edges whose
// reverse is used a different number of times (open or inconsistently
// oriented) and the undirected edges not used exactly twice (non-manifold
// or open).
type topology struct {
	vertices, edges, faces int
	unpaired, nonManifold  int
}

func meshTopology(m *Mesh) topology {
	directed := make(map[edgeKey]int, 3*len(m.Triangles))
	used := make(map[uint32]bool)
	for _, t := range m.Triangles {
		for i := 0; i < 3; i++ {
			directed[edgeKey{t[i], t[(i+1)%3]}]++
			used[t[i]] = true
		}
	}
	top := topology{vertices: len(used), faces: len(m.Triangles)}
	undirected := make(map[edgeKey]bool, len(directed))
	for k, n := range directed {
		reverse := directed[edgeKey{k.b, k.a}]
		if reverse != n {
			top.unpaired++
		}
		u := edgeKey{min(k.a, k.b), max(k.a, k.b)}
		if undirected[u] {
			continue
		}
		undirected[u] = true
		if n != 1 || reverse != 1 {
			top.nonManifold++
		}
	}
	top.edges = len(undirected)
	return top
}

func (t topology) euler() int { return t.vertices - t.edges + t.faces }

// requireClosedManifold fails unless every edge is used exactly twice in
// opposite directions: a watertight, consistently oriented 2-manifold.
func requireClosedManifold(t *testing.T, m *Mesh) topology {
	t.Helper()
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	top := meshTopology(m)
	if top.unpaired != 0 || top.nonManifold != 0 {
		t.Fatalf("mesh is not a closed oriented manifold: %+v", top)
	}
	return top
}

// massProperties returns the area and the signed enclosed volume
// (divergence theorem), in float64.
func massProperties(m *Mesh) (area, volume float64) {
	for _, t := range m.Triangles {
		a, b, c := m.Vertex(int(t[0])), m.Vertex(int(t[1])), m.Vertex(int(t[2]))
		area += b.Sub(a).Cross(c.Sub(a)).Length() / 2
		volume += a.Dot(b.Cross(c)) / 6
	}
	return area, volume
}

func triangleSoupMass(tris []render.STLTriangle) (area, volume float64) {
	for _, t := range tris {
		area += t.B.Sub(t.A).Cross(t.C.Sub(t.A)).Length() / 2
		volume += t.A.Dot(t.B.Cross(t.C)) / 6
	}
	return area, volume
}

func relClose(got, want, tol float64) bool {
	return math.Abs(got-want) <= tol*math.Max(1, math.Abs(want))
}

// identityAffine maps index (x, y, z) to (x, y, z) mm.
var identityAffine = GridAffine(render.Vec3{}, IdentityAxes, [3]float64{1, 1, 1})

// normal returns the (non-unit) right-hand normal of triangle i.
func triangleNormal(m *Mesh, i int) render.Vec3 {
	t := m.Triangles[i]
	a, b, c := m.Vertex(int(t[0])), m.Vertex(int(t[1])), m.Vertex(int(t[2]))
	return b.Sub(a).Cross(c.Sub(a))
}

func vertexNormal(m *Mesh, i int) render.Vec3 {
	n := m.Normals[i]
	return render.Vec3{X: float64(n[0]), Y: float64(n[1]), Z: float64(n[2])}
}
