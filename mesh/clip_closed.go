package mesh

import (
	"context"
	"fmt"
	"math"
	"sort"
)

// ClipClosedByPlane retains Normal·(p-Origin)>=0 when KeepPositive, <=0
// otherwise. One plane is the supported initial profile. It publishes only a
// validated closed result (or an empty result); errors/cancel return nil.
// Coincident faces remain original faces if their adjacent solid is retained.
func ClipClosedByPlane(ctx context.Context, m *Mesh, p Plane, o PlaneOptions) (*PlaneClipResult, error) {
	r, e := newPlaneRun(ctx, m, p, o)
	if e != nil {
		return nil, e
	}
	s, e := r.section()
	if e != nil {
		return nil, e
	}
	result := &PlaneClipResult{Plane: r.plane, KeepPositive: o.KeepPositive, SourceReference: o.SourceReference, Section: s, ReservedBytes: r.reserved}
	keep := 1
	if !o.KeepPositive {
		keep = -1
	}
	retained, any, e := r.retainedComponents(keep)
	if e != nil {
		return nil, e
	}
	if !any {
		if e = r.ctx.Err(); e != nil {
			return nil, e
		}
		result.Mesh = &Mesh{}
		result.Work = r.work
		return result, nil
	}
	triangles := make([][3]uint32, 0)
	sources := make([]int64, 0)
	appendFace := func(t [3]uint32, source int64) error {
		if e := r.step(); e != nil {
			return e
		}
		if int64(len(triangles)) >= r.o.MaxResultTriangles {
			return &QualityLimitError{"plane result triangles", int64(len(triangles)) + 1, r.o.MaxResultTriangles}
		}
		a, b, c := r.vertices[t[0]].p, r.vertices[t[1]].p, r.vertices[t[2]].p
		n := cross3(sub3(b, a), sub3(c, a))
		if dot3(n, n) == 0 {
			return fmt.Errorf("%w: collapsed clipped face", ErrPlaneGeometry)
		}
		triangles = append(triangles, t)
		sources = append(sources, source)
		return nil
	}
	for tid, t := range m.Triangles {
		if !retained[tid] {
			continue
		}
		if e = r.step(); e != nil {
			return nil, e
		}
		poly := make([]uint32, 0, 4)
		for j, a := range t {
			b := t[(j+1)%3]
			inside := r.side[a]*keep >= 0
			next := r.side[b]*keep >= 0
			if inside {
				id, err := r.original(a)
				if err != nil {
					return nil, err
				}
				poly = append(poly, id)
			}
			if inside != next {
				id, err := r.cut(a, b)
				if err != nil {
					return nil, err
				}
				poly = append(poly, id)
			}
		}
		clean := poly[:0]
		for _, id := range poly {
			if len(clean) == 0 || clean[len(clean)-1] != id {
				clean = append(clean, id)
			}
		}
		if len(clean) > 1 && clean[0] == clean[len(clean)-1] {
			clean = clean[:len(clean)-1]
		}
		poly = clean
		for j := 1; j+1 < len(poly); j++ {
			if e = appendFace([3]uint32{poly[0], poly[j], poly[j+1]}, int64(tid)); e != nil {
				return nil, e
			}
		}
	}
	loops, e := r.boundary(triangles)
	if e != nil {
		return nil, e
	}
	capNormal := r.plane.Normal
	for i := range capNormal {
		capNormal[i] *= -float64(keep)
	}
	if len(loops) > 0 {
		capFaces, err := r.caps(loops, capNormal)
		if err != nil {
			return nil, err
		}
		for _, t := range capFaces {
			if e = appendFace(t, -1); e != nil {
				return nil, e
			}
		}
	}
	// Compact out vertices used only by the section/removed side. Scalar and
	// normal interpolation follow the same original-edge parameter as geometry.
	ids := make([]uint32, 0)
	remap := make(map[uint32]uint32)
	out := &Mesh{}
	if m.Normals != nil {
		out.Normals = make([][3]float32, 0)
	}
	for i, t := range triangles {
		if e = r.step(); e != nil {
			return nil, e
		}
		for j, id := range t {
			v, ok := remap[id]
			if !ok {
				v = uint32(len(ids))
				remap[id] = v
				ids = append(ids, id)
				pv := r.vertices[id]
				var q [3]float32
				for k := range q {
					q[k] = float32(pv.p[k])
					if !planeFinite(float64(q[k])) {
						return nil, fmt.Errorf("%w: float32 output overflow", ErrPlaneGeometry)
					}
				}
				out.Vertices = append(out.Vertices, q)
				if m.Normals != nil {
					var n [3]float64
					for k := range n {
						n[k] = (1-pv.w)*float64(m.Normals[pv.a][k]) + pv.w*float64(m.Normals[pv.b][k])
					}
					l := math.Sqrt(dot3(n, n))
					var nn [3]float32
					if l > 0 {
						for k := range nn {
							nn[k] = float32(n[k] / l)
						}
					}
					out.Normals = append(out.Normals, nn)
				}
			}
			triangles[i][j] = v
		}
	}
	out.Triangles = triangles
	if len(out.Triangles) > 0 {
		if e = r.validateSolid(out, r.o.MaxResultVertices, r.o.MaxResultTriangles); e != nil {
			return nil, e
		}
	}
	if e = r.ctx.Err(); e != nil {
		return nil, e
	}
	result.Mesh = out
	result.SourceTriangles = sources
	result.VertexScalars, e = r.scalars(ids)
	if e != nil {
		return nil, e
	}
	if o.FaceColors != nil {
		result.FaceColors = make([][3]uint8, len(sources))
		for i, id := range sources {
			if id < 0 {
				result.FaceColors[i] = o.CapColor
			} else {
				result.FaceColors[i] = o.FaceColors[id]
			}
		}
	}
	result.Work = r.work
	if e = r.ctx.Err(); e != nil {
		return nil, e
	}
	return result, nil
}
func (r *planeRun) boundary(triangles [][3]uint32) ([]SectionPolyline, error) {
	type edgeUse struct {
		a, b  uint32
		count int
	}
	edges := make(map[planeKey]edgeUse)
	for _, t := range triangles {
		for j, a := range t {
			if e := r.step(); e != nil {
				return nil, e
			}
			b := t[(j+1)%3]
			key := planeEdge(a, b)
			u := edges[key]
			if u.count == 0 {
				u = edgeUse{a, b, 1}
			} else {
				if u.count != 1 || u.a != b || u.b != a {
					return nil, fmt.Errorf("%w: side edge multiplicity/winding", ErrPlaneGeometry)
				}
				u.count = 2
			}
			edges[key] = u
		}
	}
	next := make(map[uint32]uint32)
	incoming := make(map[uint32]bool)
	keys := make([]uint32, 0)
	for _, u := range edges {
		if u.count != 1 {
			continue
		}
		for _, id := range []uint32{u.a, u.b} {
			d := dot3(sub3(r.vertices[id].p, r.plane.Origin), r.plane.Normal)
			if math.Abs(d) > r.o.ToleranceMM*2 {
				return nil, fmt.Errorf("%w: boundary off cut plane", ErrPlaneGeometry)
			}
		}
		// Reverse side edges: the cap uses each shared edge in the opposite direction.
		if _, ok := next[u.b]; ok {
			return nil, fmt.Errorf("%w: branching cap boundary", ErrPlaneGeometry)
		}
		if incoming[u.a] {
			return nil, fmt.Errorf("%w: branching cap boundary", ErrPlaneGeometry)
		}
		next[u.b] = u.a
		incoming[u.a] = true
		keys = append(keys, u.b)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	visited := make(map[uint32]bool)
	loops := make([]SectionPolyline, 0)
	for _, start := range keys {
		if visited[start] {
			continue
		}
		l := SectionPolyline{Closed: true, Parent: -1}
		cur := start
		for {
			if e := r.step(); e != nil {
				return nil, e
			}
			if visited[cur] {
				if cur != start {
					return nil, fmt.Errorf("%w: joined cap loops", ErrPlaneGeometry)
				}
				break
			}
			visited[cur] = true
			l.PointIDs = append(l.PointIDs, cur)
			v, ok := next[cur]
			if !ok {
				return nil, fmt.Errorf("%w: open cap boundary", ErrPlaneGeometry)
			}
			cur = v
		}
		if len(l.PointIDs) < 3 {
			return nil, fmt.Errorf("%w: collapsed cap loop", ErrPlaneGeometry)
		}
		loops = append(loops, l)
	}
	return loops, nil
}

// A disconnected shell that merely touches the plane has no retained solid
// on that side. Classify every indexed component independently: an unrelated
// retained component must not resurrect its coincident zero-volume patch.
func (r *planeRun) retainedComponents(keep int) ([]bool, bool, error) {
	parents := make([]int, len(r.m.Vertices))
	sizes := make([]int, len(parents))
	for i := range parents {
		if e := r.step(); e != nil {
			return nil, false, e
		}
		parents[i], sizes[i] = i, 1
	}
	find := func(a int) (int, error) {
		for parents[a] != a {
			if e := r.step(); e != nil {
				return 0, e
			}
			parents[a] = parents[parents[a]]
			a = parents[a]
		}
		return a, nil
	}
	for _, t := range r.m.Triangles {
		for j := 1; j < 3; j++ {
			if e := r.step(); e != nil {
				return nil, false, e
			}
			a, e := find(int(t[0]))
			if e != nil {
				return nil, false, e
			}
			b, e := find(int(t[j]))
			if e != nil {
				return nil, false, e
			}
			if a != b {
				if sizes[a] < sizes[b] {
					a, b = b, a
				}
				parents[b] = a
				sizes[a] += sizes[b]
			}
		}
	}
	strict := make([]bool, len(parents))
	for i, side := range r.side {
		if e := r.step(); e != nil {
			return nil, false, e
		}
		if side == keep {
			a, e := find(i)
			if e != nil {
				return nil, false, e
			}
			strict[a] = true
		}
	}
	result := make([]bool, len(r.m.Triangles))
	any := false
	for i, t := range r.m.Triangles {
		if e := r.step(); e != nil {
			return nil, false, e
		}
		a, e := find(int(t[0]))
		if e != nil {
			return nil, false, e
		}
		result[i] = strict[a]
		any = any || result[i]
	}
	return result, any, nil
}
