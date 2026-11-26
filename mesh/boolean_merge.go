package mesh

import (
	"fmt"
	"math"
)

// Keep construction in float64 until redundant partition edges are gone.
// Rounding every temporary subdivision to Mesh's float32 format can collapse
// a tiny artificial edge even when the final solid is well representable.
type booleanWorking struct {
	r        *booleanRun
	vertices []planeVertex
}

func (w *booleanWorking) add(p [3]float64) (uint32, error) {
	for i, q := range w.vertices {
		if err := w.r.step(); err != nil {
			return 0, err
		}
		d := sub3(p, q.p)
		if dot3(d, d) <= w.r.eps*w.r.eps {
			return uint32(i), nil
		}
	}
	if err := booleanGrow(w.r, &w.vertices, w.r.o.MaxWorkingVertices, false); err != nil {
		return 0, err
	}
	id := uint32(len(w.vertices))
	w.vertices = append(w.vertices, planeVertex{p: p})
	return id, nil
}

func (r *booleanRun) conformWorking(ids []uint32, w *booleanWorking, candidates []uint32, cuts []booleanEdgePoint) ([]uint32, error) {
	var boundary []uint32
	for j, a := range ids {
		b := ids[(j+1)%len(ids)]
		if err := booleanGrow(r, &boundary, r.o.MaxPolygonVertices, false); err != nil {
			return nil, err
		}
		boundary = append(boundary, a)
		pa, pb := w.vertices[a].p, w.vertices[b].p
		edge := sub3(pb, pa)
		length2 := dot3(edge, edge)
		if length2 <= r.eps*r.eps {
			return nil, fmt.Errorf("%w: collapsed construction edge", ErrBooleanGeometry)
		}
		cuts = cuts[:0]
		for _, id := range candidates {
			if err := r.step(); err != nil {
				return nil, err
			}
			if id == a || id == b {
				continue
			}
			point := w.vertices[id].p
			t := dot3(sub3(point, pa), edge) / length2
			if t <= 0 || t >= 1 {
				continue
			}
			d := sub3(point, add3(pa, scale3(edge, t)))
			if dot3(d, d) > 64*r.eps*r.eps {
				continue
			}
			at := len(cuts)
			cuts = append(cuts, booleanEdgePoint{})
			for at > 0 && cuts[at-1].t > t {
				if err := r.step(); err != nil {
					return nil, err
				}
				cuts[at] = cuts[at-1]
				at--
			}
			cuts[at] = booleanEdgePoint{id, t}
		}
		for _, cut := range cuts {
			if err := booleanGrow(r, &boundary, r.o.MaxPolygonVertices, false); err != nil {
				return nil, err
			}
			boundary = append(boundary, cut.id)
		}
	}
	return boundary, nil
}

func (r *booleanRun) simplifyWorking(ids []uint32, w *booleanWorking) ([]uint32, error) {
	for changed := true; changed && len(ids) > 3; {
		changed = false
		for i, id := range ids {
			if err := r.step(); err != nil {
				return nil, err
			}
			a, b := w.vertices[ids[(i+len(ids)-1)%len(ids)]].p, w.vertices[ids[(i+1)%len(ids)]].p
			edge := sub3(b, a)
			l2 := dot3(edge, edge)
			if l2 <= r.eps*r.eps {
				return nil, fmt.Errorf("%w: collapsed loop", ErrBooleanGeometry)
			}
			t := dot3(sub3(w.vertices[id].p, a), edge) / l2
			if t <= 0 || t >= 1 {
				continue
			}
			d := sub3(w.vertices[id].p, add3(a, scale3(edge, t)))
			if dot3(d, d) <= 64*r.eps*r.eps {
				copy(ids[i:], ids[i+1:])
				ids = ids[:len(ids)-1]
				changed = true
				break
			}
		}
	}
	return ids, nil
}

type booleanFaceLoops struct {
	source *booleanPolygon
	loops  []SectionPolyline
}

func (r *booleanRun) mergePieces(pieces []*booleanPolygon) ([]*booleanPolygon, error) {
	if len(pieces) == 0 {
		return nil, nil
	}
	w := &booleanWorking{r: r}
	if err := r.reserve(int64(len(pieces)) * 24); err != nil {
		return nil, err
	}
	polys := make([][]uint32, len(pieces))
	for i, p := range pieces {
		if err := r.reserve(int64(len(p.p)) * 4); err != nil {
			return nil, err
		}
		ids := make([]uint32, 0, len(p.p))
		for _, point := range p.p {
			id, err := w.add(point)
			if err != nil {
				return nil, err
			}
			if len(ids) == 0 || ids[len(ids)-1] != id {
				ids = append(ids, id)
			}
		}
		if len(ids) > 1 && ids[0] == ids[len(ids)-1] {
			ids = ids[:len(ids)-1]
		}
		if len(ids) < 3 {
			return nil, fmt.Errorf("%w: collapsed construction polygon", ErrBooleanGeometry)
		}
		polys[i] = ids
	}
	if err := r.reserve(int64(len(w.vertices)) * 20); err != nil {
		return nil, err
	}
	candidates := make([]uint32, len(w.vertices))
	cuts := make([]booleanEdgePoint, 0, len(w.vertices))
	for i := range candidates {
		candidates[i] = uint32(i)
	}
	for i, ids := range polys {
		boundary, err := r.conformWorking(ids, w, candidates, cuts)
		if err != nil {
			return nil, err
		}
		polys[i] = boundary
	}
	var groups []booleanFaceLoops
	for start := 0; start < len(pieces); {
		end := start + 1
		for end < len(pieces) && pieces[end].input == pieces[start].input && pieces[end].face == pieces[start].face {
			end++
		}
		var triangles [][3]uint32
		for _, ids := range polys[start:end] {
			if len(ids) == 3 {
				if err := booleanGrow(r, &triangles, r.o.MaxResultTriangles, false); err != nil {
					return nil, err
				}
				triangles = append(triangles, [3]uint32{ids[0], ids[1], ids[2]})
				continue
			}
			var center [3]float64
			for _, id := range ids {
				center = add3(center, scale3(w.vertices[id].p, 1/float64(len(ids))))
			}
			cid, err := w.add(center)
			if err != nil {
				return nil, err
			}
			for j, id := range ids {
				if err := booleanGrow(r, &triangles, r.o.MaxResultTriangles, false); err != nil {
					return nil, err
				}
				triangles = append(triangles, [3]uint32{cid, id, ids[(j+1)%len(ids)]})
			}
		}
		if err := r.reserve(int64(len(triangles))*1024 + 32768); err != nil {
			return nil, err
		}
		if r.work >= r.o.MaxWork {
			return nil, &QualityLimitError{"boolean merge work", r.work + 1, r.o.MaxWork}
		}
		p := &planeRun{ctx: r.ctx, vertices: w.vertices, plane: Plane{Normal: pieces[start].n, Origin: pieces[start].origin}, o: PlaneOptions{ToleranceMM: r.eps * 16, MaxWork: r.o.MaxWork - r.work}}
		loops, err := p.boundary(triangles)
		if e := r.charge(&r.work, p.work, r.o.MaxWork, "boolean merge work"); e != nil {
			return nil, e
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrBooleanGeometry, err)
		}
		for i := range loops {
			planeReverse(loops[i].PointIDs) // boundary() prepares cap-facing edges
			loops[i].PointIDs, err = r.simplifyWorking(loops[i].PointIDs, w)
			if err != nil {
				return nil, err
			}
		}
		if err := booleanGrow(r, &groups, r.o.MaxPolygons, false); err != nil {
			return nil, err
		}
		groups = append(groups, booleanFaceLoops{pieces[start], loops})
		start = end
	}
	// Reinsert only genuine surviving boundary nodes, not discarded partition
	// vertices. This preserves cross-face T junctions without resurrecting them.
	if err := r.reserve(int64(len(w.vertices)) * 21); err != nil {
		return nil, err
	}
	used := make([]bool, len(w.vertices))
	candidates = candidates[:0]
	for _, g := range groups {
		for _, l := range g.loops {
			for _, id := range l.PointIDs {
				if err := r.step(); err != nil {
					return nil, err
				}
				if !used[id] {
					used[id] = true
					if err := booleanGrow(r, &candidates, r.o.MaxWorkingVertices, false); err != nil {
						return nil, err
					}
					candidates = append(candidates, id)
				}
			}
		}
	}
	cuts = make([]booleanEdgePoint, 0, len(candidates))
	var out []*booleanPolygon
	for _, g := range groups {
		for i := range g.loops {
			ids, err := r.conformWorking(g.loops[i].PointIDs, w, candidates, cuts)
			if err != nil {
				return nil, err
			}
			g.loops[i].PointIDs = ids
		}
		if err := r.reserve(int64(len(w.vertices))*64 + 32768); err != nil {
			return nil, err
		}
		if err := r.charge(&r.work, int64(len(w.vertices))*3, r.o.MaxWork, "boolean contour projection"); err != nil {
			return nil, err
		}
		if r.work >= r.o.MaxWork {
			return nil, &QualityLimitError{"boolean triangulation work", r.work + 1, r.o.MaxWork}
		}
		p := &planeRun{ctx: r.ctx, vertices: w.vertices, o: PlaneOptions{ToleranceMM: r.eps * 16, MaxWork: r.o.MaxWork - r.work, MaxResultTriangles: r.o.MaxResultTriangles}}
		p.acceptTriangle = func(t [3]uint32) bool {
			var points [3][3]float64
			for i, id := range t {
				for a, v := range w.vertices[id].p {
					points[i][a] = float64(float32(v))
				}
			}
			return dot3(cross3(sub3(points[1], points[0]), sub3(points[2], points[0])), g.source.n) > 0
		}
		triangles, err := p.caps(g.loops, g.source.n)
		if e := r.charge(&r.work, p.work, r.o.MaxWork, "boolean triangulation work"); e != nil {
			return nil, e
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrBooleanGeometry, err)
		}
		for _, t := range triangles {
			points := [][3]float64{w.vertices[t[0]].p, w.vertices[t[1]].p, w.vertices[t[2]].p}
			area := cross3(sub3(points[1], points[0]), sub3(points[2], points[0]))
			if math.Sqrt(dot3(area, area)) == 0 {
				return nil, fmt.Errorf("%w: degenerate merged face", ErrBooleanGeometry)
			}
			q, err := r.polygon(points, g.source)
			if err != nil {
				return nil, err
			}
			if err := booleanGrow(r, &out, r.o.MaxPolygons, false); err != nil {
				return nil, err
			}
			out = append(out, q)
		}
	}
	return out, nil
}
