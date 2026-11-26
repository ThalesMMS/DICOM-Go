package mesh

import (
	"fmt"
	"math"
)

func (r *booleanRun) preflight() error {
	scale := 0.0
	var labels [2][]int
	var boxes [2][]qualityBox
	for input, in := range r.inputs {
		if in.Mesh == nil || len(in.Reference) > 4096 {
			return fmt.Errorf("%w: nil mesh/source reference", ErrBooleanGeometry)
		}
		n, t := int64(len(in.Mesh.Vertices)), int64(len(in.Mesh.Triangles))
		if n > r.o.MaxVertices {
			return &QualityLimitError{"boolean input vertices", n, r.o.MaxVertices}
		}
		if t > r.o.MaxTriangles {
			return &QualityLimitError{"boolean input triangles", t, r.o.MaxTriangles}
		}
		if t == 0 && n != 0 {
			return fmt.Errorf("%w: unused vertices in empty boundary", ErrBooleanGeometry)
		}
		if len(in.VertexScalars) > MaxVertexScalarAttributes {
			return fmt.Errorf("%w: scalar attribute count", ErrBooleanGeometry)
		}
		if err := r.reserve(n*64 + t*1024 + 32768); err != nil {
			return err
		}
		if err := r.charge(&r.work, n*int64(len(in.VertexScalars)+1), r.o.MaxWork, "boolean work"); err != nil {
			return err
		}
		if err := validateVertexScalars(in.VertexScalars, len(in.Mesh.Vertices), false); err != nil {
			return err
		}
		if in.FaceColors != nil && len(in.FaceColors) != len(in.Mesh.Triangles) {
			return fmt.Errorf("%w: face color count", ErrBooleanGeometry)
		}
		if err := r.validateSolid(in.Mesh, false); err != nil {
			return err
		}
		for _, p := range in.Mesh.Vertices {
			for _, v := range p {
				scale = math.Max(scale, math.Abs(float64(v)))
			}
		}
		if err := r.charge(&r.work, n+24*t, r.o.MaxWork, "boolean component work"); err != nil {
			return err
		}
		var count int
		labels[input], count = ConnectedComponents(in.Mesh)
		if err := r.ctx.Err(); err != nil {
			return err
		}
		if err := r.orientation(in.Mesh, labels[input], count); err != nil {
			return err
		}
		boxes[input] = make([]qualityBox, len(in.Mesh.Triangles))
		r.cuts[input] = make([][]booleanCut, len(in.Mesh.Triangles))
		for id := range in.Mesh.Triangles {
			if err := r.step(); err != nil {
				return err
			}
			tri := qualityTriangle(in.Mesh, uint32(id))
			box := qualityBox{Lo: tri[0], Hi: tri[0]}
			for _, p := range tri[1:] {
				for a := 0; a < 3; a++ {
					box.Lo[a] = math.Min(box.Lo[a], p[a])
					box.Hi[a] = math.Max(box.Hi[a], p[a])
				}
			}
			boxes[input][id] = box
		}
	}
	r.eps = math.Max(scale*128*(math.Nextafter(1, 2)-1), math.SmallestNonzeroFloat64*1e6)
	if r.o.ToleranceMM < 16*r.eps {
		return fmt.Errorf("%w: tolerance below coordinate roundoff", ErrBooleanGeometry)
	}
	if len(r.inputs[0].Mesh.Triangles) > 0 && len(r.inputs[1].Mesh.Triangles) > 0 {
		a, b := r.inputs[0].VertexScalars, r.inputs[1].VertexScalars
		if len(a) != len(b) {
			return fmt.Errorf("%w: unsupported scalar schema mismatch", ErrBooleanGeometry)
		}
		for i := range a {
			if a[i].Name != b[i].Name || a[i].Units != b[i].Units {
				return fmt.Errorf("%w: unsupported scalar name/unit mismatch", ErrBooleanGeometry)
			}
		}
	}
	return r.crossInputs(labels, boxes)
}

func (r *booleanRun) validateSolid(m *Mesh, output bool) error {
	if err := r.step(); err != nil {
		return err
	}
	if m.Normals != nil && len(m.Normals) != len(m.Vertices) {
		return fmt.Errorf("%w: normal count", ErrBooleanGeometry)
	}
	if len(m.Triangles) == 0 {
		if len(m.Vertices) != 0 {
			return fmt.Errorf("%w: vertices without boundary", ErrBooleanGeometry)
		}
		return nil
	}
	maxV, maxT, tol, altitude := r.o.MaxVertices, r.o.MaxTriangles, 0.0, r.o.ToleranceMM
	if output {
		maxV, maxT, tol, altitude = r.o.MaxResultVertices, r.o.MaxResultTriangles, 0, r.eps*8
	}
	if r.work >= r.o.MaxWork {
		return &QualityLimitError{"boolean work", r.work + 1, r.o.MaxWork}
	}
	if r.candidates >= r.o.MaxCandidates {
		return &QualityLimitError{"boolean candidates", r.candidates + 1, r.o.MaxCandidates}
	}
	if r.scratch >= r.o.MaxScratchBytes {
		return &QualityLimitError{"boolean scratch bytes", r.scratch + 1, r.o.MaxScratchBytes}
	}
	q, err := AnalyzeQuality(r.ctx, m, QualityOptions{DistanceToleranceMM: tol, MaxVertices: maxV, MaxTriangles: maxT, MaxBytes: r.o.MaxScratchBytes - r.scratch, MaxWork: r.o.MaxWork - r.work, MaxCandidates: r.o.MaxCandidates - r.candidates, MaxResults: 1})
	if q != nil {
		if e := r.charge(&r.work, q.Work, r.o.MaxWork, "boolean quality work"); e != nil {
			return e
		}
		if e := r.charge(&r.candidates, q.Candidates, r.o.MaxCandidates, "boolean candidates"); e != nil {
			return e
		}
		if e := r.reserve(q.ReservedBytes); e != nil {
			return e
		}
	}
	if err != nil {
		return err
	}
	if !q.Complete || !q.IntersectionsConclusive || q.Degenerate.Count+q.DuplicateFaces.Count+q.Crossings+q.CoplanarOverlaps+q.Contacts+q.UncertainPairs != 0 {
		return fmt.Errorf("%w: boundary quality (degenerate=%d duplicate=%d crossing=%d coplanar=%d contact=%d uncertain=%d)", ErrBooleanGeometry, q.Degenerate.Count, q.DuplicateFaces.Count, q.Crossings, q.CoplanarOverlaps, q.Contacts, q.UncertainPairs)
	}
	if err := r.charge(&r.work, int64(len(m.Triangles))*3, r.o.MaxWork, "boolean closure work"); err != nil {
		return err
	}
	if !m.CheckClosure().Closed() {
		return fmt.Errorf("%w: open/nonmanifold/inconsistent boundary", ErrBooleanGeometry)
	}
	p := &planeRun{ctx: r.ctx, o: PlaneOptions{MaxWork: r.o.MaxWork - r.work}}
	err = p.validateVertexLinks(m)
	if e := r.charge(&r.work, p.work, r.o.MaxWork, "boolean vertex links"); e != nil {
		return e
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBooleanGeometry, err)
	}
	for id := range m.Triangles {
		if err := r.step(); err != nil {
			return err
		}
		t := qualityTriangle(m, uint32(id))
		n := cross3(sub3(t[1], t[0]), sub3(t[2], t[0]))
		longest := 0.0
		for i := 0; i < 3; i++ {
			v := sub3(t[(i+1)%3], t[i])
			longest = math.Max(longest, math.Sqrt(dot3(v, v)))
		}
		if math.Sqrt(dot3(n, n))/longest <= altitude {
			return fmt.Errorf("%w: face altitude within resolution", ErrBooleanGeometry)
		}
	}
	return r.adjacentIntersections(m)
}

// AnalyzeQuality deliberately excludes shared-index adjacency. Inspect it too:
// a legal edge or vertex may not hide a folded, overlapping adjacent face.
func (r *booleanRun) adjacentIntersections(m *Mesh) error {
	for i, a := range m.Triangles {
		for j := i + 1; j < len(m.Triangles); j++ {
			if err := r.step(); err != nil {
				return err
			}
			b := m.Triangles[j]
			shared := 0
			var common uint32
			for _, v := range a {
				for _, w := range b {
					if v == w {
						shared++
						common = v
					}
				}
			}
			if shared == 0 {
				continue
			}
			if err := r.charge(&r.candidates, 1, r.o.MaxCandidates, "boolean candidates"); err != nil {
				return err
			}
			ta, tb := qualityTriangle(m, uint32(i)), qualityTriangle(m, uint32(j))
			kind := qualityIntersect(ta, tb, 0)
			if kind == QualityCrossing || kind == QualityCoplanarOverlap || kind == QualityUncertain {
				return fmt.Errorf("%w: folded adjacent faces", ErrBooleanGeometry)
			}
			if shared != 1 || kind != QualityContact {
				continue
			}
			na, nb := qualityNormal(ta), qualityNormal(tb)
			axis := cross3(na, nb)
			length := math.Sqrt(dot3(axis, axis))
			if length > 128*(math.Nextafter(1, 2)-1) {
				axis = scale3(axis, 1/length)
				eps := qualityRoundoff(ta, tb)
				al, ah, _ := qualityPlaneInterval(ta, qualityDistances(ta, tb[0], nb), axis, eps)
				bl, bh, _ := qualityPlaneInterval(tb, qualityDistances(tb, ta[0], na), axis, eps)
				if math.Min(ah, bh)-math.Max(al, bl) > eps {
					return fmt.Errorf("%w: adjacent faces intersect beyond shared vertex", ErrBooleanGeometry)
				}
			} else {
				for side, t := range [2][3][3]float64{ta, tb} {
					ids, other := a, tb
					if side == 1 {
						ids, other = b, ta
					}
					for v, p := range t {
						if ids[v] != common {
							closest, _ := closestOnTriangle(p, other[0], other[1], other[2])
							d := sub3(p, closest)
							eps := qualityRoundoff(ta, tb)
							if dot3(d, d) <= eps*eps {
								return fmt.Errorf("%w: coplanar adjacent edge overlap", ErrBooleanGeometry)
							}
						}
					}
				}
			}
		}
	}
	return r.ctx.Err()
}

func (r *booleanRun) orientation(m *Mesh, labels []int, count int) error {
	if count == 0 {
		return nil
	}
	if err := r.reserve(int64(count) * 64); err != nil {
		return err
	}
	first := make([]int, count)
	volumes := make([]float64, count)
	for i := range first {
		first[i] = -1
	}
	for id, t := range m.Triangles {
		if err := r.step(); err != nil {
			return err
		}
		c := labels[id]
		if first[c] < 0 {
			first[c] = id
		}
		origin := m.point(m.Triangles[first[c]][0])
		a, b, d := sub3(m.point(t[0]), origin), sub3(m.point(t[1]), origin), sub3(m.point(t[2]), origin)
		volumes[c] += dot3(a, cross3(b, d)) / 6
	}
	for c, face := range first {
		p := m.point(m.Triangles[face][0])
		depth := 0
		for other := 0; other < count; other++ {
			if other != c {
				w, err := r.winding(m, labels, other, p)
				if err != nil {
					return err
				}
				if math.Abs(w) > .5 {
					depth++
				}
			}
		}
		if volumes[c] == 0 || (volumes[c] > 0) != (depth%2 == 0) {
			return fmt.Errorf("%w: component orientation/nesting", ErrBooleanGeometry)
		}
	}
	return nil
}

func (r *booleanRun) winding(m *Mesh, labels []int, component int, p [3]float64) (float64, error) {
	sum := 0.0
	for id, t := range m.Triangles {
		if err := r.step(); err != nil {
			return 0, err
		}
		if component >= 0 && labels[id] != component {
			continue
		}
		a, b, c := sub3(m.point(t[0]), p), sub3(m.point(t[1]), p), sub3(m.point(t[2]), p)
		la, lb, lc := math.Sqrt(dot3(a, a)), math.Sqrt(dot3(b, b)), math.Sqrt(dot3(c, c))
		if la == 0 || lb == 0 || lc == 0 {
			return 0, fmt.Errorf("%w: touching components", ErrBooleanGeometry)
		}
		sum += 2 * math.Atan2(dot3(a, cross3(b, c)), la*lb*lc+dot3(a, b)*lc+dot3(b, c)*la+dot3(c, a)*lb)
	}
	w := sum / (4 * math.Pi)
	if !planeFinite(w) || math.Abs(w-math.Round(w)) > 1e-6 || math.Abs(w) > 1.000001 {
		return 0, fmt.Errorf("%w: inconclusive winding", ErrBooleanGeometry)
	}
	return w, nil
}

func booleanBoxesOverlap(a, b qualityBox, tol float64) bool {
	for i := 0; i < 3; i++ {
		if a.Hi[i] < b.Lo[i]-tol || b.Hi[i] < a.Lo[i]-tol {
			return false
		}
	}
	return true
}
func (r *booleanRun) crossInputs(labels [2][]int, boxes [2][]qualityBox) error {
	type contacts struct{ crossing, contact bool }
	pairs := make(map[[2]int]contacts)
	for i, a := range boxes[0] {
		for j, b := range boxes[1] {
			if err := r.step(); err != nil {
				return err
			}
			if !booleanBoxesOverlap(a, b, r.o.ToleranceMM) {
				continue
			}
			if err := r.charge(&r.candidates, 1, r.o.MaxCandidates, "boolean candidates"); err != nil {
				return err
			}
			ta, tb := qualityTriangle(r.inputs[0].Mesh, uint32(i)), qualityTriangle(r.inputs[1].Mesh, uint32(j))
			kind := qualityIntersect(ta, tb, r.o.ToleranceMM)
			if kind == QualityUncertain || kind == QualityCoplanarOverlap {
				return fmt.Errorf("%w: coplanar overlap/uncertain intersection", ErrBooleanGeometry)
			}
			if kind == "" {
				continue
			}
			if kind == QualityContact && qualityIntersect(ta, tb, 0) != QualityContact {
				return fmt.Errorf("%w: intersection feature within tolerance", ErrBooleanGeometry)
			}
			key := [2]int{labels[0][i], labels[1][j]}
			v, exists := pairs[key]
			if !exists {
				if err := r.reserve(128); err != nil {
					return err
				}
			}
			v.crossing = v.crossing || kind == QualityCrossing
			v.contact = v.contact || kind == QualityContact
			pairs[key] = v
			segment, ok := booleanIntersectionSegment(ta, tb)
			if ok {
				ca := booleanCut{n: qualityNormal(tb), origin: tb[0], segment: segment}
				cb := booleanCut{n: qualityNormal(ta), origin: ta[0], segment: segment}
				if err := booleanGrow(r, &r.cuts[0][i], r.o.MaxPlanesPerFace, false); err != nil {
					return err
				}
				if err := booleanGrow(r, &r.cuts[1][j], r.o.MaxPlanesPerFace, false); err != nil {
					return err
				}
				r.cuts[0][i] = append(r.cuts[0][i], ca)
				r.cuts[1][j] = append(r.cuts[1][j], cb)
			}
		}
	}
	for _, v := range pairs {
		if v.contact && !v.crossing {
			return fmt.Errorf("%w: tangent components", ErrBooleanGeometry)
		}
	}
	return r.ctx.Err()
}

func booleanIntersectionSegment(a, b [3][3]float64) ([2][3]float64, bool) {
	na, nb := qualityNormal(a), qualityNormal(b)
	axis := cross3(na, nb)
	length := math.Sqrt(dot3(axis, axis))
	if length <= 128*(math.Nextafter(1, 2)-1) {
		return [2][3]float64{}, false
	}
	axis = scale3(axis, 1/length)
	eps := qualityRoundoff(a, b)
	anchor := a[0]
	interval := func(t [3][3]float64, d [3]float64) (float64, float64, [2][3]float64, int) {
		lo, hi := math.Inf(1), math.Inf(-1)
		var endpoints [2][3]float64
		count := 0
		add := func(p [3]float64) {
			v := dot3(sub3(p, anchor), axis)
			if v < lo {
				lo = v
				endpoints[0] = p
			}
			if v > hi {
				hi = v
				endpoints[1] = p
			}
			count++
		}
		for i, p := range t {
			j := (i + 1) % 3
			if math.Abs(d[i]) <= eps {
				add(p)
			}
			if d[i] > eps && d[j] < -eps || d[i] < -eps && d[j] > eps {
				q, dp, dq := t[j], d[i], d[j]
				for axis := 0; axis < 3; axis++ {
					if p[axis] != q[axis] {
						if p[axis] > q[axis] {
							p, q, dp, dq = q, p, dq, dp
						}
						break
					}
				}
				add(add3(p, scale3(sub3(q, p), dp/(dp-dq))))
			}
		}
		return lo, hi, endpoints, count
	}
	al, ah, p, ac := interval(a, qualityDistances(a, b[0], nb))
	bl, bh, q, bc := interval(b, qualityDistances(b, a[0], na))
	lo, hi := math.Max(al, bl), math.Min(ah, bh)
	if ac < 2 || bc < 2 || hi-lo <= eps {
		return [2][3]float64{}, false
	}
	ends := p
	if bl > al {
		ends[0] = q[0]
	}
	if bh < ah {
		ends[1] = q[1]
	}
	return ends, true
}
