package mesh

import (
	"container/heap"
	"fmt"
	"math"
)

// Quadric decimation (#1014), after Garland and Heckbert, "Surface
// simplification using quadric error metrics" (1997), and VTK's
// vtkQuadricDecimation: edges collapse in order of the quadric error of the
// merged vertex (the sum of squared distances to the planes of the original
// triangles around it), placed where that error is least. Written from the
// published method; no VTK code is copied.

// DecimateOptions configures DecimateQuadric.
type DecimateOptions struct {
	// TargetReduction is the fraction of the triangles to remove, in [0, 1).
	TargetReduction float64
	// MaxError, with LimitError, stops the decimation before a collapse
	// whose quadric error (mm², a sum of squared plane distances) exceeds
	// it, as VTK's MaximumError; zero then allows no collapse at all.
	MaxError   float64
	LimitError bool
	// PreserveBoundary keeps the vertices of open boundaries in place and
	// never collapses an edge that touches one.
	PreserveBoundary bool
}

// DecimateReport describes a decimation.
type DecimateReport struct {
	TrianglesBefore, TrianglesAfter int
	// MaxQuadricError is the largest quadric error of an applied collapse.
	MaxQuadricError float64
}

// quadric is the symmetric 4x4 plane quadric, upper triangle row-major:
// a2 ab ac ad b2 bc bd c2 cd d2.
type quadric [10]float64

func planeQuadric(n [3]float64, d float64) quadric {
	a, b, c := n[0], n[1], n[2]
	return quadric{a * a, a * b, a * c, a * d, b * b, b * c, b * d, c * c, c * d, d * d}
}

func (q quadric) add(o quadric) quadric {
	for i := range q {
		q[i] += o[i]
	}
	return q
}

func (q quadric) scale(s float64) quadric {
	for i := range q {
		q[i] *= s
	}
	return q
}

func (q quadric) error(p [3]float64) float64 {
	x, y, z := p[0], p[1], p[2]
	return q[0]*x*x + 2*q[1]*x*y + 2*q[2]*x*z + 2*q[3]*x + q[4]*y*y + 2*q[5]*y*z + 2*q[6]*y + q[7]*z*z + 2*q[8]*z + q[9]
}

// minimum solves grad = 0 for the point of least error; ok is false when
// the quadric is singular (flat or linear neighbourhoods).
func (q quadric) minimum() ([3]float64, bool) {
	a := [3][3]float64{{q[0], q[1], q[2]}, {q[1], q[4], q[5]}, {q[2], q[5], q[7]}}
	b := [3]float64{-q[3], -q[6], -q[8]}
	det := a[0][0]*(a[1][1]*a[2][2]-a[1][2]*a[2][1]) - a[0][1]*(a[1][0]*a[2][2]-a[1][2]*a[2][0]) + a[0][2]*(a[1][0]*a[2][1]-a[1][1]*a[2][0])
	scale := math.Abs(a[0][0]) + math.Abs(a[1][1]) + math.Abs(a[2][2])
	if scale == 0 || math.Abs(det) < 1e-10*scale*scale*scale {
		return [3]float64{}, false
	}
	solve := func(col int) float64 {
		m := a
		for r := 0; r < 3; r++ {
			m[r][col] = b[r]
		}
		return (m[0][0]*(m[1][1]*m[2][2]-m[1][2]*m[2][1]) - m[0][1]*(m[1][0]*m[2][2]-m[1][2]*m[2][0]) + m[0][2]*(m[1][0]*m[2][1]-m[1][1]*m[2][0])) / det
	}
	return [3]float64{solve(0), solve(1), solve(2)}, true
}

type collapse struct {
	cost   float64
	u, v   uint32
	target [3]float64
	stamp  [2]uint32
}

type collapseHeap []collapse

func (h collapseHeap) Len() int           { return len(h) }
func (h collapseHeap) Less(i, j int) bool { return h[i].cost < h[j].cost }
func (h collapseHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *collapseHeap) Push(x any)        { *h = append(*h, x.(collapse)) }
func (h *collapseHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

type decimator struct {
	pos      [][3]float64
	quadrics []quadric
	tris     [][3]uint32
	alive    []bool
	faces    [][]int32 // triangles around each vertex (may hold dead ones)
	removed  []bool
	boundary []bool
	stamp    []uint32
	// Two independently marked neighbour sets survive a link-condition check.
	// Buffers grow to the largest encountered degree and are reused thereafter.
	neighbourMarks   [2][]uint32
	neighbourBuffers [2][]uint32
	neighbourEpoch   uint32
	heap             collapseHeap
	opts             DecimateOptions
}

// DecimateQuadric removes up to TargetReduction of the triangles of m by
// edge collapses of least quadric error. A collapse is refused when it would
// flip a surrounding triangle, create a non-manifold edge (the link
// condition) or, with LimitError, exceed MaxError. Vertex normals are
// recomputed when m has them.
func DecimateQuadric(m *Mesh, opts DecimateOptions) (*Mesh, DecimateReport, error) {
	if err := m.Validate(); err != nil {
		return nil, DecimateReport{}, err
	}
	if !(opts.TargetReduction >= 0 && opts.TargetReduction < 1) || (opts.LimitError && !(opts.MaxError >= 0)) {
		return nil, DecimateReport{}, fmt.Errorf("%w: target reduction %v, max error %v", ErrInvalidField, opts.TargetReduction, opts.MaxError)
	}
	report := DecimateReport{TrianglesBefore: len(m.Triangles)}
	d := newDecimator(m, opts)
	target := int(math.Ceil(float64(len(m.Triangles)) * (1 - opts.TargetReduction)))
	count := len(m.Triangles)
	for count > target && d.heap.Len() > 0 {
		c := heap.Pop(&d.heap).(collapse)
		if d.removed[c.u] || d.removed[c.v] || d.stamp[c.u] != c.stamp[0] || d.stamp[c.v] != c.stamp[1] {
			continue
		}
		if opts.LimitError && c.cost > opts.MaxError {
			break
		}
		removed, ok := d.collapse(c)
		if !ok {
			continue
		}
		count -= removed
		report.MaxQuadricError = math.Max(report.MaxQuadricError, c.cost)
	}
	out := d.mesh(m.Normals != nil)
	report.TrianglesAfter = len(out.Triangles)
	return out, report, nil
}

func newDecimator(m *Mesh, opts DecimateOptions) *decimator {
	n := len(m.Vertices)
	d := &decimator{
		pos: make([][3]float64, n), quadrics: make([]quadric, n), tris: append([][3]uint32(nil), m.Triangles...),
		alive: make([]bool, len(m.Triangles)), faces: make([][]int32, n), removed: make([]bool, n),
		boundary: make([]bool, n), stamp: make([]uint32, n), opts: opts,
		neighbourMarks: [2][]uint32{make([]uint32, n), make([]uint32, n)},
	}
	for i, v := range m.Vertices {
		d.pos[i] = [3]float64{float64(v[0]), float64(v[1]), float64(v[2])}
	}
	edges := edgeUses(m)
	for i, t := range d.tris {
		d.alive[i] = true
		for _, v := range t {
			d.faces[v] = append(d.faces[v], int32(i))
		}
		normal, area := d.normal(t)
		if area == 0 {
			continue
		}
		q := planeQuadric(normal, -dot3(normal, d.pos[t[0]]))
		for _, v := range t {
			d.quadrics[v] = d.quadrics[v].add(q)
		}
		// Open boundaries get a heavily weighted plane through the edge,
		// perpendicular to the triangle, so they hold their shape.
		for k := 0; k < 3; k++ {
			a, b := t[k], t[(k+1)%3]
			if edges[[2]uint32{min(a, b), max(a, b)}] != 1 {
				continue
			}
			d.boundary[a], d.boundary[b] = true, true
			edge := sub3(d.pos[b], d.pos[a])
			side := cross3(edge, normal)
			if l := math.Sqrt(dot3(side, side)); l > 0 {
				side = [3]float64{side[0] / l, side[1] / l, side[2] / l}
				bq := planeQuadric(side, -dot3(side, d.pos[a])).scale(1000)
				d.quadrics[a] = d.quadrics[a].add(bq)
				d.quadrics[b] = d.quadrics[b].add(bq)
			}
		}
	}
	for e := range edges {
		if c, ok := d.candidate(e[0], e[1]); ok {
			d.heap = append(d.heap, c)
		}
	}
	heap.Init(&d.heap)
	return d
}

func (d *decimator) normal(t [3]uint32) ([3]float64, float64) {
	n := cross3(sub3(d.pos[t[1]], d.pos[t[0]]), sub3(d.pos[t[2]], d.pos[t[0]]))
	l := math.Sqrt(dot3(n, n))
	if l == 0 {
		return [3]float64{}, 0
	}
	return [3]float64{n[0] / l, n[1] / l, n[2] / l}, l / 2
}

// candidate is the collapse of edge (u, v) at its least-error position.
func (d *decimator) candidate(u, v uint32) (collapse, bool) {
	if d.opts.PreserveBoundary && (d.boundary[u] || d.boundary[v]) {
		return collapse{}, false
	}
	q := d.quadrics[u].add(d.quadrics[v])
	target, ok := q.minimum()
	cost := math.Inf(1)
	if ok {
		cost = q.error(target)
	}
	if !ok || cost > q.error(d.pos[u])+1e-12 && cost > q.error(d.pos[v])+1e-12 {
		// Singular or unstable: the best of the endpoints and midpoint.
		mid := [3]float64{(d.pos[u][0] + d.pos[v][0]) / 2, (d.pos[u][1] + d.pos[v][1]) / 2, (d.pos[u][2] + d.pos[v][2]) / 2}
		for _, p := range [][3]float64{d.pos[u], d.pos[v], mid} {
			if e := q.error(p); e < cost || !ok {
				cost, target, ok = e, p, true
			}
		}
	}
	return collapse{cost: math.Max(cost, 0), u: u, v: v, target: target, stamp: [2]uint32{d.stamp[u], d.stamp[v]}}, true
}

// neighbours are the live vertices sharing a live triangle with v.
// slot is 0 or 1. The returned slice is valid until that slot is reused.
func (d *decimator) neighbours(v uint32, slot int) []uint32 {
	d.neighbourEpoch++
	if d.neighbourEpoch == 0 {
		// Epoch wrap must not resurrect stale membership. Clearing both slots
		// leaves any retained neighbour slice intact.
		clear(d.neighbourMarks[0])
		clear(d.neighbourMarks[1])
		d.neighbourEpoch = 1
	}
	marks := d.neighbourMarks[slot]
	out := d.neighbourBuffers[slot][:0]
	for _, f := range d.faces[v] {
		if !d.alive[f] {
			continue
		}
		for _, w := range d.tris[f] {
			if w != v && marks[w] != d.neighbourEpoch {
				marks[w] = d.neighbourEpoch
				out = append(out, w)
			}
		}
	}
	d.neighbourBuffers[slot] = out
	return out
}

// collapse merges v into u at c.target when the link condition holds and no
// surrounding triangle flips; it returns the number of triangles removed.
func (d *decimator) collapse(c collapse) (int, bool) {
	u, v := c.u, c.v
	nu := d.neighbours(u, 0)
	adjacent := false
	for _, w := range nu {
		if w == v {
			adjacent = true
			break
		}
	}
	if !adjacent {
		return 0, false
	}
	d.neighbours(v, 1)
	nvEpoch := d.neighbourEpoch
	shared := 0
	var edgeFaces []int32
	for _, f := range d.faces[u] {
		if d.alive[f] && hasVertex(d.tris[f], v) {
			edgeFaces = append(edgeFaces, f)
		}
	}
	for _, w := range nu {
		if d.neighbourMarks[1][w] == nvEpoch {
			shared++
		}
	}
	// Link condition: the only common neighbours are the opposite corners
	// of the edge's triangles.
	if shared != len(edgeFaces) || len(edgeFaces) == 0 || len(edgeFaces) > 2 {
		return 0, false
	}
	// No surrounding triangle may flip or collapse to nothing.
	for _, vertex := range [2]uint32{u, v} {
		for _, f := range d.faces[vertex] {
			if !d.alive[f] || hasVertex(d.tris[f], u) && hasVertex(d.tris[f], v) {
				continue
			}
			before, _ := d.normal(d.tris[f])
			moved := d.tris[f]
			saved := d.pos[vertex]
			d.pos[vertex] = c.target
			after, area := d.normal(moved)
			d.pos[vertex] = saved
			if area == 0 || dot3(before, after) < 0.2 {
				return 0, false
			}
		}
	}
	for _, f := range edgeFaces {
		d.alive[f] = false
	}
	for _, f := range d.faces[v] {
		if !d.alive[f] {
			continue
		}
		for k := range d.tris[f] {
			if d.tris[f][k] == v {
				d.tris[f][k] = u
			}
		}
		d.faces[u] = append(d.faces[u], f)
	}
	d.faces[v] = nil
	d.removed[v] = true
	d.pos[u] = c.target
	d.quadrics[u] = d.quadrics[u].add(d.quadrics[v])
	d.boundary[u] = d.boundary[u] || d.boundary[v]
	d.stamp[u]++
	// Drop dead triangles from u's list and requeue its edges.
	live := d.faces[u][:0]
	for _, f := range d.faces[u] {
		if d.alive[f] {
			live = append(live, f)
		}
	}
	d.faces[u] = live
	// The new stamp of u retires its queued edges; requeue them.
	for _, w := range d.neighbours(u, 0) {
		if c, ok := d.candidate(u, w); ok {
			heap.Push(&d.heap, c)
		}
	}
	return len(edgeFaces), true
}

func hasVertex(t [3]uint32, v uint32) bool { return t[0] == v || t[1] == v || t[2] == v }

func (d *decimator) mesh(normals bool) *Mesh {
	used := make([]bool, len(d.pos))
	var triangles [][3]uint32
	for i, t := range d.tris {
		if d.alive[i] {
			triangles = append(triangles, t)
			used[t[0]], used[t[1]], used[t[2]] = true, true, true
		}
	}
	vertices := make([][3]float32, len(d.pos))
	for i, p := range d.pos {
		vertices[i] = [3]float32{float32(p[0]), float32(p[1]), float32(p[2])}
	}
	return compactMesh(vertices, triangles, used, normals)
}
