package mesh

import (
	"context"
	"errors"
	"math"
	"testing"
)

// flatGrid is an n×n vertex grid of spacing h in the plane z = 0, each
// square split along its diagonal, wound counter-clockwise seen from +z.
func flatGrid(n int, h float64) *Mesh {
	m := &Mesh{}
	for j := 0; j < n; j++ {
		for i := 0; i < n; i++ {
			m.Vertices = append(m.Vertices, [3]float32{float32(float64(i) * h), float32(float64(j) * h), 0})
		}
	}
	for j := 0; j+1 < n; j++ {
		for i := 0; i+1 < n; i++ {
			a := uint32(j*n + i)
			b, c, d := a+1, a+uint32(n), a+uint32(n)+1
			m.Triangles = append(m.Triangles, [3]uint32{a, b, d}, [3]uint32{a, d, c})
		}
	}
	return m
}

func gridVertex(n, i, j int) uint32 { return uint32(j*n + i) }

// icosphere is the subdivided icosahedron of the given radius around
// center, with outward winding and unit normals.
func icosphere(radius float64, subdivisions int, center [3]float64) *Mesh {
	phi := (1 + math.Sqrt(5)) / 2
	unit := [][3]float64{
		{-1, phi, 0}, {1, phi, 0}, {-1, -phi, 0}, {1, -phi, 0},
		{0, -1, phi}, {0, 1, phi}, {0, -1, -phi}, {0, 1, -phi},
		{phi, 0, -1}, {phi, 0, 1}, {-phi, 0, -1}, {-phi, 0, 1},
	}
	for i := range unit {
		unit[i] = scale3(unit[i], 1/math.Sqrt(dot3(unit[i], unit[i])))
	}
	faces := [][3]uint32{
		{0, 11, 5}, {0, 5, 1}, {0, 1, 7}, {0, 7, 10}, {0, 10, 11},
		{1, 5, 9}, {5, 11, 4}, {11, 10, 2}, {10, 7, 6}, {7, 1, 8},
		{3, 9, 4}, {3, 4, 2}, {3, 2, 6}, {3, 6, 8}, {3, 8, 9},
		{4, 9, 5}, {2, 4, 11}, {6, 2, 10}, {8, 6, 7}, {9, 8, 1},
	}
	for s := 0; s < subdivisions; s++ {
		mid := map[uint64]uint32{}
		midpoint := func(a, b uint32) uint32 {
			key := edgeID(a, b)
			if v, ok := mid[key]; ok {
				return v
			}
			p := add3(unit[a], unit[b])
			unit = append(unit, scale3(p, 1/math.Sqrt(dot3(p, p))))
			mid[key] = uint32(len(unit) - 1)
			return mid[key]
		}
		next := make([][3]uint32, 0, 4*len(faces))
		for _, f := range faces {
			ab, bc, ca := midpoint(f[0], f[1]), midpoint(f[1], f[2]), midpoint(f[2], f[0])
			next = append(next, [3]uint32{f[0], ab, ca}, [3]uint32{f[1], bc, ab}, [3]uint32{f[2], ca, bc}, [3]uint32{ab, bc, ca})
		}
		faces = next
	}
	m := &Mesh{Triangles: faces}
	for _, u := range unit {
		p := add3(center, scale3(u, radius))
		m.Vertices = append(m.Vertices, [3]float32{float32(p[0]), float32(p[1]), float32(p[2])})
		m.Normals = append(m.Normals, [3]float32{float32(u[0]), float32(u[1]), float32(u[2])})
	}
	return m
}

// TestGeodesicOnAPlaneIsTheStraightLine (#1254): on a flat grid the
// geodesic is the straight segment. The edge graph overestimates it by
// several percent; with the default Steiner points the path is within 1%
// and never shorter.
func TestGeodesicOnAPlaneIsTheStraightLine(t *testing.T) {
	const n = 41
	m := flatGrid(n, 1)
	from, to := gridVertex(n, 2, 3), gridVertex(n, 37, 21)
	want := math.Hypot(35, 18)
	edges, err := NewGeodesicGraph(m, GeodesicOptions{EdgesOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	edgePath, err := edges.ShortestPath(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := NewGeodesicGraph(m, GeodesicOptions{})
	if err != nil {
		t.Fatal(err)
	}
	path, err := graph.ShortestPath(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("straight %.4f mm, edge graph %.4f (+%.2f%%), default Steiner points %.4f (+%.3f%%)",
		want, edgePath.Length, 100*(edgePath.Length/want-1), path.Length, 100*(path.Length/want-1))
	if edgePath.Length < 1.03*want {
		t.Fatalf("edge-graph path %.4f is unexpectedly close to the straight line %.4f", edgePath.Length, want)
	}
	if path.Length < want-1e-9 || path.Length > 1.01*want {
		t.Fatalf("geodesic %.4f, want within 1%% above %.4f", path.Length, want)
	}
	requirePathOnMesh(t, m, path)
	if path.Points[0] != m.point(from) || path.Points[len(path.Points)-1] != m.point(to) {
		t.Fatalf("path runs from %v to %v", path.Points[0], path.Points[len(path.Points)-1])
	}
	for _, p := range path.Points {
		if p[2] != 0 {
			t.Fatalf("path leaves the plane at %v", p)
		}
	}
	// Seven points per edge include the default three, so the path can
	// only shorten.
	fine, err := NewGeodesicGraph(m, GeodesicOptions{SteinerPoints: 7})
	if err != nil {
		t.Fatal(err)
	}
	finePath, err := fine.ShortestPath(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if finePath.Length > path.Length+1e-9 || finePath.Length < want-1e-9 {
		t.Fatalf("7 Steiner points give %.4f, default %.4f, exact %.4f", finePath.Length, path.Length, want)
	}
}

// TestGeodesicOnASphereIsTheGreatCircle (#1254): distances from a vertex
// of a 50 mm sphere follow the great-circle arcs R·θ within 1.5%, and a
// path stays on the sphere.
func TestGeodesicOnASphereIsTheGreatCircle(t *testing.T) {
	const radius = 50.0
	m := icosphere(radius, 4, [3]float64{})
	graph, err := NewGeodesicGraph(m, GeodesicOptions{})
	if err != nil {
		t.Fatal(err)
	}
	source := uint32(0)
	distances, err := graph.Distances(context.Background(), []uint32{source}, 0)
	if err != nil {
		t.Fatal(err)
	}
	u := m.point(source)
	worst := 0.0
	for v, d := range distances {
		p := m.point(uint32(v))
		cos := dot3(u, p) / (radius * radius)
		arc := radius * math.Acos(math.Max(-1, math.Min(1, cos)))
		worst = math.Max(worst, math.Abs(d-arc)/(math.Pi*radius))
		if v != int(source) && d < 0.99*arc {
			t.Fatalf("vertex %d: geodesic %.3f shorter than the arc %.3f allows", v, d, arc)
		}
	}
	t.Logf("worst deviation from the great circle %.3f%% of the half circumference", 100*worst)
	if worst > 0.015 {
		t.Fatalf("worst deviation %.4f of the half circumference", worst)
	}
	// The path to the far side.
	target := uint32(0)
	far := 1.0
	for v := range m.Vertices {
		if c := dot3(u, m.point(uint32(v))) / (radius * radius); c < far && c > -0.5 {
			far, target = c, uint32(v)
		}
	}
	path, err := graph.ShortestPath(context.Background(), source, target)
	if err != nil {
		t.Fatal(err)
	}
	arc := radius * math.Acos(far)
	if math.Abs(path.Length-arc) > 0.015*arc {
		t.Fatalf("path %.3f mm, great circle %.3f mm", path.Length, arc)
	}
	requirePathOnMesh(t, m, path)
	if math.Abs(path.Length-distances[target]) > 1e-9 {
		t.Fatalf("path %.6f and distance field %.6f differ", path.Length, distances[target])
	}
}

// TestGeodesicBetweenSurfacePoints: points inside triangles; on one
// triangle the path is the segment.
func TestGeodesicBetweenSurfacePoints(t *testing.T) {
	const n = 21
	m := flatGrid(n, 2)
	locator, err := NewLocator(m)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := NewGeodesicGraph(m, GeodesicOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a, ok := locator.ClosestPoint([3]float64{3.3, 2.7, 5})
	if !ok || a.Distance != 5 {
		t.Fatalf("closest point %+v", a)
	}
	b, _ := locator.ClosestPoint([3]float64{3.6, 2.9, -1})
	if a.Triangle != b.Triangle {
		t.Fatalf("points on triangles %d and %d", a.Triangle, b.Triangle)
	}
	path, err := graph.ShortestPathBetween(context.Background(), a, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(path.Points) != 2 || math.Abs(path.Length-math.Hypot(0.3, 0.2)) > 1e-9 {
		t.Fatalf("same-triangle path %+v", path)
	}
	c, _ := locator.ClosestPoint([3]float64{33.1, 25.4, 0})
	path, err = graph.ShortestPathBetween(context.Background(), a, c)
	if err != nil {
		t.Fatal(err)
	}
	want := math.Hypot(33.1-3.3, 25.4-2.7)
	if path.Length < want-1e-9 || path.Length > 1.01*want {
		t.Fatalf("path %.4f, straight %.4f", path.Length, want)
	}
	requirePathOnMesh(t, m, path)
}

func TestGeodesicFailures(t *testing.T) {
	two := &Mesh{
		Vertices:  [][3]float32{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}, {5, 0, 0}, {6, 0, 0}, {5, 1, 0}, {9, 9, 9}},
		Triangles: [][3]uint32{{0, 1, 2}, {3, 4, 5}},
	}
	graph, err := NewGeodesicGraph(two, GeodesicOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := graph.ShortestPath(context.Background(), 0, 4); !errors.Is(err, ErrNoPath) {
		t.Fatalf("disjoint components: %v", err)
	}
	if _, err := graph.ShortestPath(context.Background(), 0, 6); !errors.Is(err, ErrInvalidField) {
		t.Fatalf("unused vertex: %v", err)
	}
	distances, err := graph.Distances(context.Background(), []uint32{0}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !math.IsInf(distances[4], 1) || distances[1] != 1 {
		t.Fatalf("distances %v", distances)
	}
	if _, err := graph.Distances(context.Background(), nil, 0); !errors.Is(err, ErrInvalidField) {
		t.Fatalf("no source: %v", err)
	}
	if _, err := NewGeodesicGraph(two, GeodesicOptions{SteinerPoints: MaxGeodesicSteinerPoints + 1}); !errors.Is(err, ErrInvalidField) {
		t.Fatalf("too many Steiner points: %v", err)
	}
	big := flatGrid(200, 1)
	graph, err = NewGeodesicGraph(big, GeodesicOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := graph.ShortestPath(ctx, 0, uint32(len(big.Vertices)-1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled path: %v", err)
	}
	if _, err := graph.Distances(ctx, []uint32{0}, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled distances: %v", err)
	}
	// A distance limit leaves the far vertices unreached.
	limited, err := graph.Distances(context.Background(), []uint32{0}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !math.IsInf(limited[len(limited)-1], 1) || limited[5] != 5 {
		t.Fatalf("limited distances: last %v, fifth %v", limited[len(limited)-1], limited[5])
	}
}

// requirePathOnMesh checks that every segment of the path lies in one
// triangle (both ends on its closure) and that Length is the polyline's.
func requirePathOnMesh(t *testing.T, m *Mesh, path *GeodesicPath) {
	t.Helper()
	locator, err := NewLocator(m)
	if err != nil {
		t.Fatal(err)
	}
	length := 0.0
	for i := 1; i < len(path.Points); i++ {
		a, b := path.Points[i-1], path.Points[i]
		length += distance3(a, b)
		mid := scale3(add3(a, b), 0.5)
		if s, _ := locator.ClosestPoint(mid); s.Distance > 1e-4 {
			t.Fatalf("segment %d leaves the surface by %.6f mm", i, s.Distance)
		}
	}
	if math.Abs(length-path.Length) > 1e-9*math.Max(1, length) {
		t.Fatalf("length %.9f, polyline %.9f", path.Length, length)
	}
}
