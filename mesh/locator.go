package mesh

import (
	"cmp"
	"math"
	"slices"
	"sync"
)

// Point and ray queries against a surface (#1254): the closest point of a
// mesh to any point in space, its signed distance and the first triangle a
// ray meets, answered by a bounding-volume hierarchy of the triangles. The
// closest point of a triangle is found from the Voronoi regions of its
// corners and edges; the sign of a distance comes from the angle-weighted
// pseudo-normal of the nearest feature (J. A. Bærentzen and H. Aanæs,
// "Signed Distance Computation Using the Angle Weighted Pseudonormal", IEEE
// TVCG 2005), which is exact on a closed, consistently wound surface. VTK's
// vtkCellLocator and vtkDistancePolyDataFilter informed the scope only; no
// third-party code is copied.

// Locator answers closest-point, signed-distance and ray queries against
// the triangles of one mesh. It is safe for concurrent use; the mesh must
// not change while it is in use.
type Locator struct {
	m    *Mesh
	tree *triangleBVH

	pseudoOnce    sync.Once
	vertexNormals [][3]float64
	edgeNormals   map[uint64][3]float64
	// weld maps each vertex to the lowest-indexed vertex at exactly its
	// position; nil when no two vertices coincide (the identity).
	weld []uint32
}

// SurfacePoint is a point on a triangle of a mesh.
type SurfacePoint struct {
	// Point is the position in the mesh's coordinates (patient mm).
	Point [3]float64
	// Triangle is the index of the triangle holding the point and
	// Barycentric the weights of its three corners, summing to 1.
	Triangle    uint32
	Barycentric [3]float64
	// Distance is the distance from the query point, or the ray parameter
	// for RayHit.
	Distance float64
}

// NearestVertex is the corner of the point's triangle with the largest
// weight, the vertex closest to the point along the triangle.
func (s SurfacePoint) NearestVertex(m *Mesh) uint32 {
	t := m.Triangles[s.Triangle]
	best := 0
	for k := 1; k < 3; k++ {
		if s.Barycentric[k] > s.Barycentric[best] {
			best = k
		}
	}
	return t[best]
}

// VertexPoint is the surface point at vertex v of m, on the first triangle
// using it; ok is false for a vertex no triangle uses.
func VertexPoint(m *Mesh, v uint32) (SurfacePoint, bool) {
	if m == nil || int(v) >= len(m.Vertices) {
		return SurfacePoint{}, false
	}
	for i, t := range m.Triangles {
		for k := 0; k < 3; k++ {
			if t[k] == v {
				var weights [3]float64
				weights[k] = 1
				return SurfacePoint{Point: m.point(v), Triangle: uint32(i), Barycentric: weights}, true
			}
		}
	}
	return SurfacePoint{}, false
}

// NewLocator builds the hierarchy of m's triangles, in O(T log T) time and
// about 100 bytes per triangle.
//
// The pseudo-normals that sign distances are keyed by position, not by
// vertex index: vertices at exactly the same coordinates are welded for
// them, so a triangle soup (each triangle with its own three vertices, as
// ReadSTL produces) is signed like the same surface welded, near its edges
// and corners too. Only exact coincidence welds and the mesh is not
// changed; a mesh without coincident vertices keeps the per-index
// pseudo-normals, and so its results, exactly. Vertices split apart at one
// position on purpose (a non-manifold pinch made manifold) are welded back
// for the sign too; there the surface is not a manifold and no pseudo-normal
// is exact, but the sign no longer depends on which copy the closest
// triangle uses. The weld is made with the pseudo-normals, at the first
// signed query, in O(V log V) time; it keeps 4 bytes per vertex only when
// some vertices coincide.
func NewLocator(m *Mesh) (*Locator, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	l := &Locator{m: m}
	if len(m.Triangles) > 0 {
		l.tree = newTriangleBVH(m)
	}
	return l, nil
}

// Mesh returns the located mesh.
func (l *Locator) Mesh() *Mesh { return l.m }

// ClosestPoint returns the point of the surface nearest to p; ok is false
// for a mesh without triangles or a query with a NaN or infinite
// coordinate. A query far enough away for its squared distances to
// overflow (a coordinate beyond 2^510) is answered in scaled coordinates,
// still at the nearest point; its Distance is +Inf only when the distance
// itself exceeds the float64 range.
func (l *Locator) ClosestPoint(p [3]float64) (SurfacePoint, bool) {
	if l == nil || l.tree == nil || !finite3(p) {
		return SurfacePoint{}, false
	}
	stack := make([]int32, 0, 64)
	if farQuery(p) {
		return l.closestFar(p, &stack)
	}
	return l.closest(p, math.Inf(1), &stack)
}

// farQuery reports whether a squared distance from p to a float32 mesh may
// overflow: below 2^510 on every axis, it stays under 3·2^1022.
func farQuery(p [3]float64) bool {
	const limit = 0x1p510
	return math.Abs(p[0]) >= limit || math.Abs(p[1]) >= limit || math.Abs(p[2]) >= limit
}

// closestFar is ClosestPoint for a far query. The query and the mesh are
// scaled by a power of two (exactly) so that the query is near 2^380, which
// keeps the products in closestOnTriangle finite, and the triangles are
// compared by |q|² - 2p·q, the squared distance less |p|²: the squared
// distances themselves would round to the same value for every triangle.
func (l *Locator) closestFar(p [3]float64, stack *[]int32) (SurfacePoint, bool) {
	m, tree := l.m, l.tree
	_, exponent := math.Frexp(math.Max(math.Abs(p[0]), math.Max(math.Abs(p[1]), math.Abs(p[2]))))
	scale := math.Ldexp(1, 380-exponent)
	p = scale3(p, scale)
	// key is |q|² - 2p·q, and boxKey its minimum over a node's box, reached
	// at the box point nearest to p on every axis.
	key := func(q [3]float64) float64 {
		return q[0]*(q[0]-2*p[0]) + q[1]*(q[1]-2*p[1]) + q[2]*(q[2]-2*p[2])
	}
	boxKey := func(node *bvhNode) float64 {
		var q [3]float64
		for k := 0; k < 3; k++ {
			q[k] = min(max(p[k], node.lo[k]*scale), node.hi[k]*scale)
		}
		return key(q)
	}
	best := math.Inf(1)
	var out SurfacePoint
	found := false
	*stack = append((*stack)[:0], 0)
	for len(*stack) > 0 {
		index := (*stack)[len(*stack)-1]
		*stack = (*stack)[:len(*stack)-1]
		node := &tree.nodes[index]
		if found && boxKey(node) >= best {
			continue
		}
		if node.count == 0 {
			near, far := node.first, node.first+1
			if boxKey(&tree.nodes[far]) < boxKey(&tree.nodes[near]) {
				near, far = far, near
			}
			*stack = append(*stack, far, near)
			continue
		}
		for _, ti := range tree.order[node.first : node.first+node.count] {
			t := m.Triangles[ti]
			q, weights := closestOnTriangle(p, scale3(m.point(t[0]), scale), scale3(m.point(t[1]), scale), scale3(m.point(t[2]), scale))
			if k := key(q); !found || k < best {
				best, found = k, true
				out = SurfacePoint{Point: q, Triangle: ti, Barycentric: weights}
			}
		}
	}
	d := sub3(p, out.Point)
	out.Point = scale3(out.Point, 1/scale)
	out.Distance = math.Sqrt(dot3(d, d)) / scale
	return out, found
}

// closest is ClosestPoint among the triangles nearer than bound, with a
// reusable traversal stack.
func (l *Locator) closest(p [3]float64, bound float64, stack *[]int32) (SurfacePoint, bool) {
	m, tree := l.m, l.tree
	best2 := bound * bound
	var out SurfacePoint
	found := false
	*stack = append((*stack)[:0], 0)
	for len(*stack) > 0 {
		index := (*stack)[len(*stack)-1]
		*stack = (*stack)[:len(*stack)-1]
		node := &tree.nodes[index]
		// Until a triangle is found, a box exactly at the bound may still
		// hold one there.
		if d2 := boxDistance2(p, node.lo, node.hi); d2 > best2 || found && d2 == best2 {
			continue
		}
		if node.count == 0 {
			// Visit the nearer child first (pushed last).
			near, far := node.first, node.first+1
			if boxDistance2(p, tree.nodes[far].lo, tree.nodes[far].hi) < boxDistance2(p, tree.nodes[near].lo, tree.nodes[near].hi) {
				near, far = far, near
			}
			*stack = append(*stack, far, near)
			continue
		}
		for _, ti := range tree.order[node.first : node.first+node.count] {
			t := m.Triangles[ti]
			q, weights := closestOnTriangle(p, m.point(t[0]), m.point(t[1]), m.point(t[2]))
			d := sub3(p, q)
			if d2 := dot3(d, d); d2 < best2 || !found && d2 <= best2 {
				best2, found = d2, true
				out = SurfacePoint{Point: q, Triangle: ti, Barycentric: weights}
			}
		}
	}
	out.Distance = math.Sqrt(best2)
	return out, found
}

// SignedDistance is the distance from p to the surface, negative inside:
// on the side the angle-weighted pseudo-normal of the nearest feature
// (face, edge or vertex) points away from. The sign is meaningful for a
// closed, consistently wound mesh (outward normals); ok is false for a
// mesh without triangles or a query with a NaN or infinite coordinate
// (see ClosestPoint).
func (l *Locator) SignedDistance(p [3]float64) (float64, SurfacePoint, bool) {
	s, ok := l.ClosestPoint(p)
	if !ok {
		return 0, s, false
	}
	return l.sign(p, s) * s.Distance, s, true
}

// sign is -1 when p lies behind the pseudo-normal of the feature of s.
func (l *Locator) sign(p [3]float64, s SurfacePoint) float64 {
	if s.Distance == 0 {
		return 1
	}
	l.pseudoOnce.Do(l.buildPseudoNormals)
	t := l.m.Triangles[s.Triangle]
	var zero [3]bool
	zeros := 0
	for k := 0; k < 3; k++ {
		if s.Barycentric[k] <= 0 {
			zero[k] = true
			zeros++
		}
	}
	var normal [3]float64
	switch zeros {
	case 2:
		for k := 0; k < 3; k++ {
			if !zero[k] {
				normal = l.vertexNormals[t[k]]
			}
		}
	case 1:
		for k := 0; k < 3; k++ {
			if zero[k] {
				normal = l.edgeNormals[edgeID(l.welded(t[(k+1)%3]), l.welded(t[(k+2)%3]))]
			}
		}
	default:
		normal = cross3(sub3(l.m.point(t[1]), l.m.point(t[0])), sub3(l.m.point(t[2]), l.m.point(t[0])))
	}
	d := sub3(p, s.Point)
	if farQuery(p) {
		// Scaled down, the direction cannot overflow.
		d = sub3(scale3(p, 0x1p-600), scale3(s.Point, 0x1p-600))
	}
	if dot3(d, normal) < 0 {
		return -1
	}
	return 1
}

// buildPseudoNormals computes the angle-weighted vertex normals and the
// edge normals (the sum of the unit normals of the faces sharing the edge),
// over the vertices welded by position.
func (l *Locator) buildPseudoNormals() {
	m := l.m
	l.weld = weldCoincident(m)
	l.vertexNormals = make([][3]float64, len(m.Vertices))
	l.edgeNormals = make(map[uint64][3]float64, 3*len(m.Triangles)/2)
	for _, t := range m.Triangles {
		p := [3][3]float64{m.point(t[0]), m.point(t[1]), m.point(t[2])}
		n := cross3(sub3(p[1], p[0]), sub3(p[2], p[0]))
		length := math.Sqrt(dot3(n, n))
		if length == 0 {
			continue
		}
		n = scale3(n, 1/length)
		w := [3]uint32{l.welded(t[0]), l.welded(t[1]), l.welded(t[2])}
		for k := 0; k < 3; k++ {
			a, b := sub3(p[(k+1)%3], p[k]), sub3(p[(k+2)%3], p[k])
			angle := math.Atan2(math.Sqrt(dot3(cross3(a, b), cross3(a, b))), dot3(a, b))
			l.vertexNormals[w[k]] = add3(l.vertexNormals[w[k]], scale3(n, angle))
			id := edgeID(w[k], w[(k+1)%3])
			l.edgeNormals[id] = add3(l.edgeNormals[id], n)
		}
	}
	// Every vertex of a welded group carries the group's normal, so that
	// vertexNormals stays indexed by vertex.
	for v, root := range l.weld {
		l.vertexNormals[v] = l.vertexNormals[root]
	}
}

// welded is the representative of vertex v among the vertices at its
// position.
func (l *Locator) welded(v uint32) uint32 {
	if l.weld == nil {
		return v
	}
	return l.weld[v]
}

// weldCoincident maps every vertex of m to the lowest-indexed vertex at
// exactly the same position (+0 and -0 coincide), or returns nil when all
// positions are distinct. It sorts an index permutation rather than hashing
// the positions, so its scratch is 4 bytes per vertex.
func weldCoincident(m *Mesh) []uint32 {
	n := len(m.Vertices)
	if n < 2 {
		return nil
	}
	order := make([]uint32, n)
	for i := range order {
		order[i] = uint32(i)
	}
	comparePosition := func(a, b uint32) int {
		pa, pb := m.Vertices[a], m.Vertices[b]
		for k := 0; k < 3; k++ {
			switch {
			case pa[k] < pb[k]:
				return -1
			case pa[k] > pb[k]:
				return 1
			}
		}
		return 0
	}
	slices.SortFunc(order, func(a, b uint32) int {
		if c := comparePosition(a, b); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	var weld []uint32
	for i := 1; i < n; i++ {
		if comparePosition(order[i-1], order[i]) != 0 {
			continue
		}
		if weld == nil {
			weld = make([]uint32, n)
			for v := range weld {
				weld[v] = uint32(v)
			}
		}
		// Within a group the indexes ascend, so the root is the first.
		weld[order[i]] = weld[order[i-1]]
	}
	return weld
}

// RayHit returns the first point where the ray origin + t·direction, with
// 0 < t <= maxT (any t when maxT is zero or +Inf), meets a triangle from
// either side; Distance is t in units of the normalised direction. ok is
// false when it meets none or the direction is zero.
func (l *Locator) RayHit(origin, direction [3]float64, maxT float64) (SurfacePoint, bool) {
	if l == nil || l.tree == nil {
		return SurfacePoint{}, false
	}
	length := math.Sqrt(dot3(direction, direction))
	if !(length > 0) || math.IsInf(length, 0) {
		return SurfacePoint{}, false
	}
	d := scale3(direction, 1/length)
	if !(maxT > 0) {
		maxT = math.Inf(1)
	}
	stack := make([]int32, 0, 64)
	t, ti, hit := l.tree.closestHit(l.m, origin, d, maxT, math.MaxUint32, 1e-9, &stack)
	if !hit {
		return SurfacePoint{}, false
	}
	tri := l.m.Triangles[ti]
	q := add3(origin, scale3(d, t))
	_, weights := closestOnTriangle(q, l.m.point(tri[0]), l.m.point(tri[1]), l.m.point(tri[2]))
	return SurfacePoint{Point: q, Triangle: ti, Barycentric: weights, Distance: t}, true
}

// edgeID is the undirected key of edge (a, b).
func edgeID(a, b uint32) uint64 {
	if a > b {
		a, b = b, a
	}
	return uint64(a)<<32 | uint64(b)
}

// finite3 reports whether every coordinate of p is finite.
func finite3(p [3]float64) bool {
	for _, v := range p {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// boxDistance2 is the squared distance from p to the box [lo, hi].
func boxDistance2(p, lo, hi [3]float64) float64 {
	var d2 float64
	for k := 0; k < 3; k++ {
		switch {
		case p[k] < lo[k]:
			d2 += (lo[k] - p[k]) * (lo[k] - p[k])
		case p[k] > hi[k]:
			d2 += (p[k] - hi[k]) * (p[k] - hi[k])
		}
	}
	return d2
}

// closestOnTriangle returns the point of triangle (a, b, c) nearest to p
// and its barycentric weights, testing the Voronoi regions of the corners,
// then of the edges, then the face. Weights of exactly 0 mark the corners
// a point on an edge or at a corner does not depend on.
func closestOnTriangle(p, a, b, c [3]float64) ([3]float64, [3]float64) {
	ab, ac, ap := sub3(b, a), sub3(c, a), sub3(p, a)
	d1, d2 := dot3(ab, ap), dot3(ac, ap)
	if d1 <= 0 && d2 <= 0 {
		return a, [3]float64{1, 0, 0}
	}
	bp := sub3(p, b)
	d3, d4 := dot3(ab, bp), dot3(ac, bp)
	if d3 >= 0 && d4 <= d3 {
		return b, [3]float64{0, 1, 0}
	}
	cp := sub3(p, c)
	d5, d6 := dot3(ab, cp), dot3(ac, cp)
	if d6 >= 0 && d5 <= d6 {
		return c, [3]float64{0, 0, 1}
	}
	// Edge regions: p projects inside the edge and lies outside the face
	// across it (the corresponding barycentric area is not positive).
	vc := d1*d4 - d3*d2
	if vc <= 0 && d1 >= 0 && d3 <= 0 {
		v := d1 / (d1 - d3)
		return add3(a, scale3(ab, v)), [3]float64{1 - v, v, 0}
	}
	vb := d5*d2 - d1*d6
	if vb <= 0 && d2 >= 0 && d6 <= 0 {
		w := d2 / (d2 - d6)
		return add3(a, scale3(ac, w)), [3]float64{1 - w, 0, w}
	}
	va := d3*d6 - d5*d4
	if va <= 0 && d4-d3 >= 0 && d5-d6 >= 0 {
		w := (d4 - d3) / ((d4 - d3) + (d5 - d6))
		return add3(b, scale3(sub3(c, b), w)), [3]float64{0, 1 - w, w}
	}
	sum := va + vb + vc
	if !(sum > 0) {
		// A degenerate (zero-area) triangle: the nearest of its edges.
		return closestOnDegenerate(p, a, b, c)
	}
	v, w := vb/sum, vc/sum
	return add3(a, add3(scale3(ab, v), scale3(ac, w))), [3]float64{1 - v - w, v, w}
}

// closestOnDegenerate is the nearest point of the three edges of a
// zero-area triangle.
func closestOnDegenerate(p, a, b, c [3]float64) ([3]float64, [3]float64) {
	corners := [3][3]float64{a, b, c}
	var best [3]float64
	var weights [3]float64
	best2 := math.Inf(1)
	for k := 0; k < 3; k++ {
		u, v := corners[k], corners[(k+1)%3]
		e := sub3(v, u)
		s := 0.0
		if l2 := dot3(e, e); l2 > 0 {
			s = math.Max(0, math.Min(1, dot3(sub3(p, u), e)/l2))
		}
		q := add3(u, scale3(e, s))
		if d := sub3(p, q); dot3(d, d) < best2 {
			best2, best = dot3(d, d), q
			weights = [3]float64{}
			weights[k], weights[(k+1)%3] = 1-s, s
		}
	}
	return best, weights
}
