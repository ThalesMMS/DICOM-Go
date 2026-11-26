package mesh

import (
	"math"
	"sort"
)

// Mesh clean-up, connectivity and hole filling (#1014), after VTK's
// vtkCleanPolyData, vtkPolyDataConnectivityFilter and vtkFillHolesFilter.
// Written from their documented behaviour; no VTK code is copied.

// CleanReport counts what Clean removed.
type CleanReport struct {
	MergedVertices      int
	DegenerateTriangles int
	DuplicateTriangles  int
	UnusedVertices      int
}

// Clean merges vertices closer than tolerance (in mm; 0 merges only
// identical positions) into the first of them, drops the triangles that
// become degenerate (a repeated vertex) and the exact duplicates (the same
// corners in the same cyclic order), and removes the vertices no triangle
// uses. Vertex normals are recomputed when m has them.
func Clean(m *Mesh, tolerance float64) (*Mesh, CleanReport) {
	var report CleanReport
	if m == nil {
		return nil, report
	}
	representative := mergeVertices(m.Vertices, math.Max(tolerance, 0))
	for i, r := range representative {
		if r != uint32(i) {
			report.MergedVertices++
		}
	}
	seen := make(map[[3]uint32]bool, len(m.Triangles))
	used := make([]bool, len(m.Vertices))
	var triangles [][3]uint32
	for _, t := range m.Triangles {
		t = [3]uint32{representative[t[0]], representative[t[1]], representative[t[2]]}
		if t[0] == t[1] || t[1] == t[2] || t[0] == t[2] {
			report.DegenerateTriangles++
			continue
		}
		key := canonicalTriangle(t)
		if seen[key] {
			report.DuplicateTriangles++
			continue
		}
		seen[key] = true
		used[t[0]], used[t[1]], used[t[2]] = true, true, true
		triangles = append(triangles, t)
	}
	out := compactMesh(m.Vertices, triangles, used, m.Normals != nil)
	report.UnusedVertices = len(m.Vertices) - report.MergedVertices - len(out.Vertices)
	return out, report
}

// canonicalTriangle rotates t to start at its smallest index, keeping the
// winding.
func canonicalTriangle(t [3]uint32) [3]uint32 {
	switch {
	case t[1] < t[0] && t[1] < t[2]:
		return [3]uint32{t[1], t[2], t[0]}
	case t[2] < t[0] && t[2] < t[1]:
		return [3]uint32{t[2], t[0], t[1]}
	}
	return t
}

// mergeVertices maps each vertex to the first earlier vertex within
// tolerance (itself when none), through a hash grid of cell tolerance.
func mergeVertices(vertices [][3]float32, tolerance float64) []uint32 {
	representative := make([]uint32, len(vertices))
	if tolerance == 0 {
		first := make(map[[3]float32]uint32, len(vertices))
		for i, v := range vertices {
			if j, ok := first[v]; ok {
				representative[i] = j
				continue
			}
			first[v] = uint32(i)
			representative[i] = uint32(i)
		}
		return representative
	}
	cell := func(v [3]float32) [3]int64 {
		return [3]int64{floorToInt64(float64(v[0]) / tolerance), floorToInt64(float64(v[1]) / tolerance), floorToInt64(float64(v[2]) / tolerance)}
	}
	grid := make(map[[3]int64][]uint32, len(vertices))
	limit := tolerance * tolerance
	for i, v := range vertices {
		c := cell(v)
		representative[i] = uint32(i)
		found := false
		for dz := int64(-1); dz <= 1 && !found; dz++ {
			for dy := int64(-1); dy <= 1 && !found; dy++ {
				for dx := int64(-1); dx <= 1 && !found; dx++ {
					for _, j := range grid[[3]int64{c[0] + dx, c[1] + dy, c[2] + dz}] {
						w := vertices[j]
						x, y, z := float64(v[0]-w[0]), float64(v[1]-w[1]), float64(v[2]-w[2])
						if x*x+y*y+z*z <= limit {
							representative[i], found = j, true
							break
						}
					}
				}
			}
		}
		if !found {
			grid[c] = append(grid[c], uint32(i))
		}
	}
	return representative
}

// floorToInt64 is int64(math.Floor(x)) with the conversions Go leaves to
// the architecture fixed: NaN, the infinities and every value outside
// [-2^63, 2^63) give math.MinInt64, as on amd64 and as the Rust port's
// fmath::f64_to_i64 specifies, rather than saturating as on arm64.
func floorToInt64(x float64) int64 {
	x = math.Floor(x)
	if !(x >= -0x1p63 && x < 0x1p63) {
		return math.MinInt64
	}
	return int64(x)
}

// compactMesh keeps the used vertices, renumbered in order.
func compactMesh(vertices [][3]float32, triangles [][3]uint32, used []bool, normals bool) *Mesh {
	index := make([]uint32, len(vertices))
	out := &Mesh{}
	for i, u := range used {
		if u {
			index[i] = uint32(len(out.Vertices))
			out.Vertices = append(out.Vertices, vertices[i])
		}
	}
	out.Triangles = make([][3]uint32, len(triangles))
	for i, t := range triangles {
		out.Triangles[i] = [3]uint32{index[t[0]], index[t[1]], index[t[2]]}
	}
	if normals {
		out.Normals = make([][3]float32, len(out.Vertices))
		faceNormals(out, nil)
	}
	return out
}

// ConnectedComponents labels each triangle with its component, triangles
// connected through shared vertices (VTK's point connectivity). Components
// are numbered from 0 by decreasing triangle count; count is their number.
func ConnectedComponents(m *Mesh) (component []int, count int) {
	if m == nil {
		return nil, 0
	}
	parent := make([]int32, len(m.Vertices))
	for i := range parent {
		parent[i] = int32(i)
	}
	var find func(int32) int32
	find = func(x int32) int32 {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	for _, t := range m.Triangles {
		a, b, c := find(int32(t[0])), find(int32(t[1])), find(int32(t[2]))
		parent[b] = a
		parent[find(c)] = a
	}
	roots := map[int32]int{}
	sizes := []int{}
	raw := make([]int, len(m.Triangles))
	for i, t := range m.Triangles {
		root := find(int32(t[0]))
		id, ok := roots[root]
		if !ok {
			id = len(sizes)
			roots[root] = id
			sizes = append(sizes, 0)
		}
		sizes[id]++
		raw[i] = id
	}
	order := make([]int, len(sizes))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return sizes[order[i]] > sizes[order[j]] })
	rank := make([]int, len(sizes))
	for r, id := range order {
		rank[id] = r
	}
	for i := range raw {
		raw[i] = rank[raw[i]]
	}
	return raw, len(sizes)
}

// LargestComponent returns the component with the most triangles, as a
// compact mesh (VTK's extraction of the largest region).
func LargestComponent(m *Mesh) *Mesh {
	component, count := ConnectedComponents(m)
	if count == 0 {
		return &Mesh{}
	}
	used := make([]bool, len(m.Vertices))
	var triangles [][3]uint32
	for i, t := range m.Triangles {
		if component[i] == 0 {
			triangles = append(triangles, t)
			used[t[0]], used[t[1]], used[t[2]] = true, true, true
		}
	}
	return compactMesh(m.Vertices, triangles, used, m.Normals != nil)
}

// FillHoles closes the boundary loops whose bounding sphere (about the loop
// centroid) has a radius up to maxRadius mm (VTK's hole size), with a fan
// of triangles to a new vertex at the centroid wound like the surrounding
// surface. Loops through a vertex with several boundary edges out of it are
// left open. It returns the mesh and the number of holes filled.
func FillHoles(m *Mesh, maxRadius float64) (*Mesh, int) {
	if m == nil {
		return nil, 0
	}
	directed := make(map[[2]uint32]bool, 3*len(m.Triangles))
	for _, t := range m.Triangles {
		for i := 0; i < 3; i++ {
			directed[[2]uint32{t[i], t[(i+1)%3]}] = true
		}
	}
	// Boundary edges (a, b) whose twin (b, a) is missing; next[a] = b.
	next := map[uint32]uint32{}
	branching := map[uint32]bool{}
	for e := range directed {
		if directed[[2]uint32{e[1], e[0]}] {
			continue
		}
		if _, ok := next[e[0]]; ok {
			branching[e[0]] = true
		}
		next[e[0]] = e[1]
	}
	out := &Mesh{Vertices: append([][3]float32(nil), m.Vertices...), Triangles: append([][3]uint32(nil), m.Triangles...)}
	visited := map[uint32]bool{}
	starts := make([]uint32, 0, len(next))
	for a := range next {
		starts = append(starts, a)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	filled := 0
	for _, first := range starts {
		if visited[first] {
			continue
		}
		loop := []uint32{first}
		closed, simple := false, !branching[first]
		visited[first] = true
		for current := next[first]; ; current = next[current] {
			if current == first {
				closed = true
				break
			}
			if visited[current] {
				break
			}
			if _, ok := next[current]; !ok {
				break
			}
			if branching[current] {
				simple = false
			}
			visited[current] = true
			loop = append(loop, current)
		}
		if !closed || !simple || len(loop) < 3 {
			continue
		}
		var centre [3]float64
		for _, v := range loop {
			p := m.Vertices[v]
			centre[0] += float64(p[0])
			centre[1] += float64(p[1])
			centre[2] += float64(p[2])
		}
		for axis := range centre {
			centre[axis] /= float64(len(loop))
		}
		radius := 0.0
		for _, v := range loop {
			p := m.Vertices[v]
			d := [3]float64{float64(p[0]) - centre[0], float64(p[1]) - centre[1], float64(p[2]) - centre[2]}
			radius = math.Max(radius, math.Sqrt(dot3(d, d)))
		}
		if radius > maxRadius {
			continue
		}
		c := uint32(len(out.Vertices))
		out.Vertices = append(out.Vertices, [3]float32{float32(centre[0]), float32(centre[1]), float32(centre[2])})
		if len(loop) == 3 {
			// A triangular hole takes one triangle, no centre vertex.
			out.Vertices = out.Vertices[:c]
			out.Triangles = append(out.Triangles, [3]uint32{loop[0], loop[2], loop[1]})
		} else {
			for i, a := range loop {
				b := loop[(i+1)%len(loop)]
				out.Triangles = append(out.Triangles, [3]uint32{b, a, c})
			}
		}
		filled++
	}
	if m.Normals != nil {
		out.Normals = make([][3]float32, len(out.Vertices))
		faceNormals(out, nil)
	}
	return out, filled
}
