package mesh

import (
	"context"
	"math"
	"testing"
)

// soupOf gives every triangle of m its own three vertices, as ReadSTL does.
func soupOf(m *Mesh) *Mesh {
	soup := &Mesh{}
	for _, t := range m.Triangles {
		base := uint32(len(soup.Vertices))
		for _, v := range t {
			soup.Vertices = append(soup.Vertices, m.Vertices[v])
		}
		soup.Triangles = append(soup.Triangles, [3]uint32{base, base + 1, base + 2})
	}
	return soup
}

// tetrahedronQueries are points nearest to an edge or a corner of the
// tetrahedron m, outside (in the feature's normal cone) and just inside,
// with their true side: inside a convex solid is behind every face plane.
func tetrahedronQueries(m *Mesh) (points [][3]float64, inside []bool) {
	var normals, origins [][3]float64
	for _, t := range m.Triangles {
		a, b, c := m.point(t[0]), m.point(t[1]), m.point(t[2])
		n := cross3(sub3(b, a), sub3(c, a))
		normals = append(normals, scale3(n, 1/math.Sqrt(dot3(n, n))))
		origins = append(origins, a)
	}
	var centroid [3]float64
	for v := range m.Vertices {
		centroid = add3(centroid, scale3(m.point(uint32(v)), 0.25))
	}
	add := func(feature, direction [3]float64) {
		direction = scale3(direction, 1/math.Sqrt(dot3(direction, direction)))
		for _, step := range []float64{1e-3, 0.05, 0.4} {
			points = append(points, add3(feature, scale3(direction, step)))
		}
		toward := sub3(centroid, feature)
		points = append(points, add3(feature, scale3(toward, 1e-3)), add3(feature, scale3(toward, 0.02)))
	}
	for i, t := range m.Triangles {
		for k := 0; k < 3; k++ {
			a, b := t[k], t[(k+1)%3]
			// The face across edge (a, b).
			j := -1
			for f, u := range m.Triangles {
				if f != i && (u[0] == a || u[1] == a || u[2] == a) && (u[0] == b || u[1] == b || u[2] == b) {
					j = f
				}
			}
			if j < i {
				continue
			}
			mid := scale3(add3(m.point(a), m.point(b)), 0.5)
			quarter := add3(scale3(m.point(a), 0.75), scale3(m.point(b), 0.25))
			for _, w := range []float64{0.5, 0.1, 0.9} {
				direction := add3(scale3(normals[i], w), scale3(normals[j], 1-w))
				add(mid, direction)
				add(quarter, direction)
			}
		}
	}
	for v := range m.Vertices {
		var direction [3]float64
		for i, t := range m.Triangles {
			if t[0] == uint32(v) || t[1] == uint32(v) || t[2] == uint32(v) {
				direction = add3(direction, normals[i])
			}
		}
		add(m.point(uint32(v)), direction)
	}
	for _, p := range points {
		in := true
		for i := range normals {
			if dot3(sub3(p, origins[i]), normals[i]) >= 0 {
				in = false
			}
		}
		inside = append(inside, in)
	}
	return points, inside
}

// The pseudo-normals were keyed by vertex index, so on a triangle soup the
// normal of an edge or corner was the normal of one face, and a point
// outside near a sharp edge or corner came out negative. Welded by position,
// the soup is signed like the welded mesh, with the same distance.
func TestLocatorSignsTriangleSoupLikeTheWeldedMesh(t *testing.T) {
	for _, tc := range []struct {
		name     string
		vertices [][3]float32
	}{
		{"regular", [][3]float32{{1, 1, 1}, {1, -1, -1}, {-1, 1, -1}, {-1, -1, 1}}},
		// A flat tetrahedron: the edges of its base are sharp.
		{"sharp", [][3]float32{{0, 0, 0}, {10, 0, 0}, {0, 10, 0}, {1, 1, 0.3}}},
	} {
		welded := &Mesh{Vertices: tc.vertices, Triangles: [][3]uint32{{0, 2, 1}, {0, 1, 3}, {0, 3, 2}, {1, 2, 3}}}
		// Wind every face outward.
		var centroid [3]float64
		for v := range welded.Vertices {
			centroid = add3(centroid, scale3(welded.point(uint32(v)), 0.25))
		}
		for i, tri := range welded.Triangles {
			a, b, c := welded.point(tri[0]), welded.point(tri[1]), welded.point(tri[2])
			if dot3(cross3(sub3(b, a), sub3(c, a)), sub3(a, centroid)) < 0 {
				welded.Triangles[i] = [3]uint32{tri[0], tri[2], tri[1]}
			}
		}
		soup := soupOf(welded)
		weldedLocator, err := NewLocator(welded)
		if err != nil {
			t.Fatal(err)
		}
		soupLocator, err := NewLocator(soup)
		if err != nil {
			t.Fatal(err)
		}
		points, inside := tetrahedronQueries(welded)
		for i, p := range points {
			want, _, ok := weldedLocator.SignedDistance(p)
			if !ok {
				t.Fatalf("%s: query %v unanswered", tc.name, p)
			}
			if (want < 0) != inside[i] {
				t.Fatalf("%s: welded query %v: signed distance %g, inside %t", tc.name, p, want, inside[i])
			}
			got, s, ok := soupLocator.SignedDistance(p)
			if !ok {
				t.Fatalf("%s: soup query %v unanswered", tc.name, p)
			}
			if (got < 0) != inside[i] || math.Abs(got-want) > 1e-12*math.Max(1, math.Abs(want)) {
				t.Fatalf("%s: soup query %v (nearest %v, weights %v): signed distance %g, welded %g, inside %t", tc.name, p, s.Point, s.Barycentric, got, want, inside[i])
			}
		}
		// VertexDistances signs through the same pseudo-normals.
		probe := &Mesh{Vertices: make([][3]float32, 0, len(points))}
		for _, p := range points {
			probe.Vertices = append(probe.Vertices, [3]float32{float32(p[0]), float32(p[1]), float32(p[2])})
		}
		for v := 0; v+2 < len(probe.Vertices); v += 3 {
			probe.Triangles = append(probe.Triangles, [3]uint32{uint32(v), uint32(v + 1), uint32(v + 2)})
		}
		opts := SurfaceDistanceOptions{Signed: true}
		want, err := weldedLocator.VertexDistances(context.Background(), probe, opts)
		if err != nil {
			t.Fatal(err)
		}
		got, err := soupLocator.VertexDistances(context.Background(), probe, opts)
		if err != nil {
			t.Fatal(err)
		}
		for v := range want {
			if (got[v] < 0) != (want[v] < 0) || math.Abs(got[v]-want[v]) > 1e-12*math.Max(1, math.Abs(want[v])) {
				t.Fatalf("%s: vertex %d: soup distance %g, welded %g", tc.name, v, got[v], want[v])
			}
		}
	}
}

// Welding is by exact position only: a mesh with distinct positions keeps
// the identity, and coincident vertices map to the lowest index.
func TestWeldCoincidentIsExact(t *testing.T) {
	distinct := &Mesh{Vertices: [][3]float32{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}, {0, 0, math.Nextafter32(0, 1)}}}
	if w := weldCoincident(distinct); w != nil {
		t.Fatalf("distinct positions welded: %v", w)
	}
	negZero := float32(math.Copysign(0, -1))
	m := &Mesh{Vertices: [][3]float32{{1, 2, 3}, {0, 0, 0}, {1, 2, 3}, {negZero, 0, 0}, {1, 2, 3.0000002}, {1, 2, 3}}}
	want := []uint32{0, 1, 0, 1, 4, 0}
	got := weldCoincident(m)
	for v := range want {
		if got[v] != want[v] {
			t.Fatalf("weld %v, want %v", got, want)
		}
	}
}
