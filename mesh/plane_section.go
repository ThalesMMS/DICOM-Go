package mesh

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
)

// ErrPlaneGeometry reports an inadmissible solid or an ambiguous section. It
// never accompanies a published partial mesh or section.
var ErrPlaneGeometry = errors.New("mesh: plane geometry")

// Plane is expressed in patient LPS millimetres. Normal need not be unit length.
type Plane struct{ Origin, Normal [3]float64 }

// PlaneOptions admits a single plane and a finite, request-owned operation.
// Zero limits choose finite defaults. ToleranceMM defaults to 1e-6 mm; samples
// in this band are projected onto the plane, not classified by triangle order.
// Inputs, including borrowed attributes, must remain stable until return.
type PlaneOptions struct {
	ToleranceMM                                                                         float64
	KeepPositive                                                                        bool
	MaxVertices, MaxTriangles, MaxResultVertices, MaxResultTriangles, MaxBytes, MaxWork int64
	VertexScalars                                                                       []VertexScalarAttribute
	FaceColors                                                                          [][3]uint8
	CapColor                                                                            [3]uint8
	SourceReference                                                                     string
}

// SectionPolyline indexes Points; the closing vertex is not repeated. Parent
// is the immediately enclosing loop (-1 at depth zero); odd Depth means Hole.
// A tangent edge may produce an open polyline. A tangent isolated vertex is
// counted in TangentVertices. Branching/overlapping contours fail explicitly.
type SectionPolyline struct {
	PointIDs      []uint32
	Closed, Hole  bool
	Depth, Parent int
}

type PlaneSection struct {
	Plane           Plane
	ToleranceMM     float64
	Points          [][3]float64
	Polylines       []SectionPolyline
	VertexScalars   []VertexScalarAttribute
	TangentVertices int
}

// PlaneClipResult distinguishes original faces (SourceTriangles>=0) from caps
// (-1), and carries scalar interpolation and source information for export.
// Normals remain interpolated source shading normals; cap face normals are
// geometric. No clinical, CAD-general or intersection-free guarantee is made.
type PlaneClipResult struct {
	Mesh                *Mesh
	Section             *PlaneSection
	SourceTriangles     []int64
	VertexScalars       []VertexScalarAttribute
	FaceColors          [][3]uint8
	Plane               Plane
	KeepPositive        bool
	SourceReference     string
	ReservedBytes, Work int64
}

type planeKey [2]uint32

func planeEdge(a, b uint32) planeKey {
	if a > b {
		a, b = b, a
	}
	return planeKey{a, b}
}

type planeVertex struct {
	p    [3]float64
	a, b uint32
	w    float64
}
type planeRun struct {
	// Optional representation check used by Boolean's float32 output. The
	// ordinary plane section/clip profile keeps its existing nil policy.
	acceptTriangle func([3]uint32) bool
	ctx            context.Context
	m              *Mesh
	o              PlaneOptions
	plane          Plane
	vertices       []planeVertex
	originals      map[uint32]uint32
	cuts           map[planeKey]uint32
	dist           []float64
	side           []int
	work, reserved int64
}

func planeFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func planeVecFinite(p [3]float64) bool {
	for _, v := range p {
		if !planeFinite(v) {
			return false
		}
	}
	return true
}
func planeOptions(o PlaneOptions) (PlaneOptions, error) {
	if !planeFinite(o.ToleranceMM) || o.ToleranceMM < 0 {
		return o, fmt.Errorf("%w: tolerance", ErrPlaneGeometry)
	}
	if o.ToleranceMM == 0 {
		o.ToleranceMM = 1e-6
	}
	ps := []*int64{&o.MaxVertices, &o.MaxTriangles, &o.MaxResultVertices, &o.MaxResultTriangles, &o.MaxBytes, &o.MaxWork}
	ds := []int64{1000000, 2000000, 2000000, 4000000, 512 << 20, 20000000}
	for i, p := range ps {
		if *p < 0 {
			return o, fmt.Errorf("%w: negative limit", ErrPlaneGeometry)
		}
		if *p == 0 {
			*p = ds[i]
		}
	}
	if len(o.SourceReference) > 4096 {
		return o, fmt.Errorf("%w: source reference length", ErrPlaneGeometry)
	}
	return o, nil
}
func (r *planeRun) step() error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if r.work >= r.o.MaxWork {
		return &QualityLimitError{"plane work", r.work + 1, r.o.MaxWork}
	}
	r.work++
	return nil
}
func planeReserve(n, t int64, a int, normals bool) (int64, error) {
	// Conservative logical simultaneous reservation: topology/maps, quality
	// arrays, clipping/loop/cap scratch, results and interpolation. Runtime/map
	// allocator overhead is not measured RSS. Arithmetic precedes allocations.
	const max = math.MaxInt64
	if t > (max-n)/4 {
		return 0, &QualityLimitError{"plane bytes", max, 0}
	}
	count := n + 4*t
	per := int64(1536 + 64*a)
	if count > (max-32768)/per {
		return 0, &QualityLimitError{"plane bytes", max, 0}
	}
	b := count*per + 32768
	_ = normals // normal slots are included unconditionally in the bound.
	return b, nil
}
func newPlaneRun(ctx context.Context, m *Mesh, p Plane, o PlaneOptions) (*planeRun, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var err error
	o, err = planeOptions(o)
	if err != nil {
		return nil, err
	}
	if m == nil || !planeVecFinite(p.Origin) || !planeVecFinite(p.Normal) {
		return nil, fmt.Errorf("%w: nil mesh/nonfinite plane", ErrPlaneGeometry)
	}
	length := math.Hypot(math.Hypot(p.Normal[0], p.Normal[1]), p.Normal[2])
	if !(length > 0) || !planeFinite(length) {
		return nil, fmt.Errorf("%w: plane normal", ErrPlaneGeometry)
	}
	for i := range p.Normal {
		p.Normal[i] /= length
	}
	n, t := int64(len(m.Vertices)), int64(len(m.Triangles))
	if n > o.MaxVertices {
		return nil, &QualityLimitError{"plane input vertices", n, o.MaxVertices}
	}
	if t > o.MaxTriangles {
		return nil, &QualityLimitError{"plane input triangles", t, o.MaxTriangles}
	}
	if n > math.MaxUint32 || t > math.MaxUint32 {
		return nil, fmt.Errorf("%w: index capacity", ErrPlaneGeometry)
	}
	b, err := planeReserve(n, t, len(o.VertexScalars), m.Normals != nil)
	if err != nil {
		return nil, err
	}
	if b > o.MaxBytes {
		return nil, &QualityLimitError{"plane bytes", b, o.MaxBytes}
	}
	if err = m.Validate(); err != nil {
		return nil, err
	}
	if err = validateVertexScalars(o.VertexScalars, len(m.Vertices), false); err != nil {
		return nil, err
	}
	if o.FaceColors != nil && len(o.FaceColors) != len(m.Triangles) {
		return nil, fmt.Errorf("%w: face colors", ErrPlaneGeometry)
	}
	r := &planeRun{ctx: ctx, m: m, o: o, plane: p, reserved: b}
	if err = r.validateSolid(m, r.o.MaxVertices, r.o.MaxTriangles); err != nil {
		return nil, err
	}
	r.vertices = make([]planeVertex, 0)
	r.originals = make(map[uint32]uint32)
	r.cuts = make(map[planeKey]uint32)
	r.dist = make([]float64, len(m.Vertices))
	r.side = make([]int, len(m.Vertices))
	for i := range m.Vertices {
		if err = r.step(); err != nil {
			return nil, err
		}
		d := dot3(sub3(m.point(uint32(i)), p.Origin), p.Normal)
		if !planeFinite(d) {
			return nil, fmt.Errorf("%w: distance overflow", ErrPlaneGeometry)
		}
		r.dist[i] = d
		if d > o.ToleranceMM {
			r.side[i] = 1
		} else if d < -o.ToleranceMM {
			r.side[i] = -1
		}
	}
	return r, nil
}
func (r *planeRun) validateSolid(m *Mesh, maxVertices, maxTriangles int64) error {
	// Existing quality classifier excludes indexed adjacency but detects
	// nonadjacent crossings/contact/uncertain pairs. Ambiguity is rejected.
	if r.work >= r.o.MaxWork {
		return &QualityLimitError{"plane work", r.work + 1, r.o.MaxWork}
	}
	q, e := AnalyzeQuality(r.ctx, m, QualityOptions{MaxVertices: maxVertices, MaxTriangles: maxTriangles, MaxBytes: r.o.MaxBytes, MaxWork: r.o.MaxWork - r.work, MaxCandidates: r.o.MaxWork - r.work, MaxResults: 1})
	if q != nil {
		r.work += q.Work
	}
	if e != nil {
		return e
	}
	if !q.Complete || !q.IntersectionsConclusive || q.Degenerate.Count != 0 || q.DuplicateFaces.Count != 0 || q.Crossings+q.CoplanarOverlaps+q.Contacts+q.UncertainPairs != 0 {
		return fmt.Errorf("%w: degenerate/intersecting/inconclusive solid", ErrPlaneGeometry)
	}
	if err := r.step(); err != nil {
		return err
	}
	if !m.CheckClosure().Closed() {
		return fmt.Errorf("%w: open/nonmanifold/inconsistently wound solid", ErrPlaneGeometry)
	}
	return r.validateVertexLinks(m)
}

// Edge closure alone admits shells welded at a point. Each used vertex must
// also have one connected circular link. Storage is linear in triangle count
// and covered by the operation's admitted reservation; every scan is charged.
func (r *planeRun) validateVertexLinks(m *Mesh) error {
	links := make(map[uint32][]planeKey)
	for _, t := range m.Triangles {
		for j, v := range t {
			if err := r.step(); err != nil {
				return err
			}
			links[v] = append(links[v], planeKey{t[(j+1)%3], t[(j+2)%3]})
		}
	}
	type node struct {
		neighbors [2]uint32
		degree    int
	}
	for _, edges := range links {
		nodes := make(map[uint32]node)
		for _, e := range edges {
			for j, v := range e {
				if err := r.step(); err != nil {
					return err
				}
				n := nodes[v]
				if n.degree == 2 {
					return fmt.Errorf("%w: branching vertex link", ErrPlaneGeometry)
				}
				n.neighbors[n.degree] = e[1-j]
				n.degree++
				nodes[v] = n
			}
		}
		var start uint32
		first := true
		for v, n := range nodes {
			if err := r.step(); err != nil {
				return err
			}
			if n.degree != 2 {
				return fmt.Errorf("%w: open vertex link", ErrPlaneGeometry)
			}
			if first || v < start {
				start = v
				first = false
			}
		}
		seen := make(map[uint32]bool)
		current, previous := start, start
		for {
			if err := r.step(); err != nil {
				return err
			}
			if seen[current] {
				break
			}
			seen[current] = true
			n := nodes[current]
			next := n.neighbors[0]
			if next == previous {
				next = n.neighbors[1]
			}
			previous, current = current, next
		}
		if current != start || len(seen) != len(nodes) {
			return fmt.Errorf("%w: disconnected vertex link", ErrPlaneGeometry)
		}
	}
	return r.ctx.Err()
}
func (r *planeRun) vertex(v planeVertex) (uint32, error) {
	if int64(len(r.vertices)) >= r.o.MaxResultVertices {
		return 0, &QualityLimitError{"plane result vertices", int64(len(r.vertices)) + 1, r.o.MaxResultVertices}
	}
	if len(r.vertices) >= math.MaxUint32 {
		return 0, fmt.Errorf("%w: result index capacity", ErrPlaneGeometry)
	}
	id := uint32(len(r.vertices))
	r.vertices = append(r.vertices, v)
	return id, nil
}
func (r *planeRun) original(a uint32) (uint32, error) {
	if id, ok := r.originals[a]; ok {
		return id, nil
	}
	p := r.m.point(a)
	if r.side[a] == 0 {
		for k := range p {
			p[k] -= r.dist[a] * r.plane.Normal[k]
		}
	}
	id, e := r.vertex(planeVertex{p: p, a: a, b: a})
	if e == nil {
		r.originals[a] = id
	}
	return id, e
}
func (r *planeRun) cut(a, b uint32) (uint32, error) {
	if r.side[a] == 0 {
		return r.original(a)
	}
	if r.side[b] == 0 {
		return r.original(b)
	}
	key := planeEdge(a, b)
	if id, ok := r.cuts[key]; ok {
		return id, nil
	}
	a, b = key[0], key[1]
	da, db := r.dist[a], r.dist[b]
	den := da - db
	if !planeFinite(den) || den == 0 {
		return 0, fmt.Errorf("%w: intersection denominator", ErrPlaneGeometry)
	}
	w := da / den
	if !(w > 0 && w < 1) {
		return 0, fmt.Errorf("%w: intersection parameter", ErrPlaneGeometry)
	}
	pa, pb := r.m.point(a), r.m.point(b)
	var p [3]float64
	for i := range p {
		p[i] = (1-w)*pa[i] + w*pb[i]
	}
	if !planeVecFinite(p) {
		return 0, fmt.Errorf("%w: intersection overflow", ErrPlaneGeometry)
	}
	id, e := r.vertex(planeVertex{p: p, a: a, b: b, w: w})
	if e == nil {
		r.cuts[key] = id
	}
	return id, e
}
func (r *planeRun) scalars(ids []uint32) ([]VertexScalarAttribute, error) {
	out := make([]VertexScalarAttribute, len(r.o.VertexScalars))
	for a, s := range r.o.VertexScalars {
		out[a] = VertexScalarAttribute{Name: s.Name, Units: s.Units, Values: make([]float64, len(ids))}
		for i, id := range ids {
			if e := r.step(); e != nil {
				return nil, e
			}
			v := r.vertices[id]
			out[a].Values[i] = (1-v.w)*s.Values[v.a] + v.w*s.Values[v.b]
			if !planeFinite(out[a].Values[i]) {
				return nil, fmt.Errorf("%w: scalar interpolation overflow", ErrPlaneGeometry)
			}
		}
	}
	return out, nil
}

// SectionByPlane computes a bounded section without changing the solid.
func SectionByPlane(ctx context.Context, m *Mesh, p Plane, o PlaneOptions) (*PlaneSection, error) {
	r, e := newPlaneRun(ctx, m, p, o)
	if e != nil {
		return nil, e
	}
	s, e := r.section()
	if e != nil {
		return nil, e
	}
	if e = r.ctx.Err(); e != nil {
		return nil, e
	}
	return s, nil
}
func (r *planeRun) section() (*PlaneSection, error) {
	edges := make(map[planeKey]bool)
	coplanar := make(map[planeKey]int)
	tangent := make(map[uint32]bool)
	for _, t := range r.m.Triangles {
		if e := r.step(); e != nil {
			return nil, e
		}
		ids := make([]uint32, 0, 3)
		all := r.side[t[0]] == 0 && r.side[t[1]] == 0 && r.side[t[2]] == 0
		for j, a := range t {
			b := t[(j+1)%3]
			if r.side[a] == 0 {
				id, e := r.original(a)
				if e != nil {
					return nil, e
				}
				ids = append(ids, id)
			}
			if r.side[a]*r.side[b] < 0 {
				id, e := r.cut(a, b)
				if e != nil {
					return nil, e
				}
				ids = append(ids, id)
			}
		}
		if all {
			for j := range ids {
				coplanar[planeEdge(ids[j], ids[(j+1)%3])]++
			}
		} else if len(ids) == 2 && ids[0] != ids[1] {
			edges[planeEdge(ids[0], ids[1])] = true
		} else if len(ids) == 1 {
			tangent[ids[0]] = true
		}
	}
	for k, c := range coplanar {
		if c == 1 {
			edges[k] = true
		} else if c != 2 {
			return nil, fmt.Errorf("%w: coplanar edge multiplicity", ErrPlaneGeometry)
		}
	}
	keys := make([]planeKey, 0, len(edges))
	for k := range edges {
		keys = append(keys, k)
		delete(tangent, k[0])
		delete(tangent, k[1])
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	lines, e := r.walkUndirected(keys)
	if e != nil {
		return nil, e
	}
	used := make(map[uint32]uint32)
	ids := make([]uint32, 0)
	s := &PlaneSection{Plane: r.plane, ToleranceMM: r.o.ToleranceMM, TangentVertices: len(tangent)}
	for _, l := range lines {
		for i, id := range l.PointIDs {
			ni, ok := used[id]
			if !ok {
				ni = uint32(len(ids))
				used[id] = ni
				ids = append(ids, id)
				s.Points = append(s.Points, r.vertices[id].p)
			}
			l.PointIDs[i] = ni
		}
		s.Polylines = append(s.Polylines, l)
	}
	s.VertexScalars, e = r.scalars(ids)
	if e != nil {
		return nil, e
	}
	if e = planeNest(r.ctx, s.Points, s.Polylines, r.plane.Normal, r.o.ToleranceMM, r.step); e != nil {
		return nil, e
	}
	return s, nil
}
func (r *planeRun) walkUndirected(edges []planeKey) ([]SectionPolyline, error) {
	adj := make(map[uint32][]uint32)
	for _, e := range edges {
		adj[e[0]] = append(adj[e[0]], e[1])
		adj[e[1]] = append(adj[e[1]], e[0])
	}
	for _, ns := range adj {
		if len(ns) > 2 {
			return nil, fmt.Errorf("%w: branching section", ErrPlaneGeometry)
		}
	}
	used := make(map[planeKey]bool)
	out := []SectionPolyline{}
	for _, edge := range edges {
		if used[edge] {
			continue
		}
		start := edge[0]
		// Prefer a degree-one endpoint anywhere in this component.
		queue := []uint32{start}
		seen := map[uint32]bool{start: true}
		for len(queue) > 0 {
			if e := r.step(); e != nil {
				return nil, e
			}
			id := queue[0]
			queue = queue[1:]
			if len(adj[id]) == 1 {
				start = id
				break
			}
			for _, n := range adj[id] {
				if !seen[n] {
					seen[n] = true
					queue = append(queue, n)
				}
			}
		}
		l := SectionPolyline{Parent: -1}
		cur := start
		for {
			if e := r.step(); e != nil {
				return nil, e
			}
			l.PointIDs = append(l.PointIDs, cur)
			next := uint32(0)
			found := false
			for _, n := range adj[cur] {
				if !used[planeEdge(cur, n)] {
					next = n
					found = true
					break
				}
			}
			if !found {
				break
			}
			used[planeEdge(cur, next)] = true
			cur = next
			if cur == start {
				l.Closed = true
				break
			}
		}
		if l.Closed && len(l.PointIDs) < 3 {
			return nil, fmt.Errorf("%w: collapsed section", ErrPlaneGeometry)
		}
		out = append(out, l)
	}
	return out, nil
}
