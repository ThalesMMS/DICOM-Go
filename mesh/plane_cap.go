package mesh

import (
	"context"
	"fmt"
	"math"
)

type planeXY struct{ x, y float64 }

func planeBasis(n [3]float64) ([3]float64, [3]float64) {
	a := [3]float64{1, 0, 0}
	if math.Abs(n[0]) > .8 {
		a = [3]float64{0, 1, 0}
	}
	u := cross3(a, n)
	l := math.Sqrt(dot3(u, u))
	for i := range u {
		u[i] /= l
	}
	return u, cross3(n, u)
}
func planeProject(points [][3]float64, n [3]float64) []planeXY {
	u, v := planeBasis(n)
	out := make([]planeXY, len(points))
	if len(points) == 0 {
		return out
	}
	origin := points[0]
	for i, p := range points {
		d := sub3(p, origin)
		out[i] = planeXY{dot3(d, u), dot3(d, v)}
	}
	return out
}
func planeOrient(a, b, c planeXY) float64 { return (b.x-a.x)*(c.y-a.y) - (b.y-a.y)*(c.x-a.x) }
func planeArea(p []planeXY, ids []uint32) float64 {
	a := 0.0
	if len(ids) == 0 {
		return a
	}
	origin := p[ids[0]]
	for i := range ids {
		a += planeOrient(origin, p[ids[i]], p[ids[(i+1)%len(ids)]])
	}
	return a / 2
}

// The orientation has square-length units. A projected roundoff sliver
// must clear the existing physical tolerance times its diagonal length.
func planeCapEarNondegenerate(a, b, c planeXY, tol float64) bool {
	return planeOrient(a, b, c) > tol*math.Hypot(c.x-a.x, c.y-a.y)
}

// Include boundary vertices when testing an ear: a negative projection ULP
// must not permit a diagonal to skip an existing contour vertex.
func planePointInCapEar(a, b, c, p planeXY, tol float64) bool {
	return planeOrient(a, b, p) >= -tol*math.Hypot(b.x-a.x, b.y-a.y) &&
		planeOrient(b, c, p) >= -tol*math.Hypot(c.x-b.x, c.y-b.y) &&
		planeOrient(c, a, p) >= -tol*math.Hypot(a.x-c.x, a.y-c.y)
}
func planeSame(a, b planeXY) bool { return a == b }
func planeOnSegment(p, a, b planeXY, tol float64) bool {
	l := math.Hypot(b.x-a.x, b.y-a.y)
	if l == 0 {
		return math.Hypot(p.x-a.x, p.y-a.y) <= tol
	}
	return math.Abs(planeOrient(a, b, p)) <= tol*l && p.x >= math.Min(a.x, b.x)-tol && p.x <= math.Max(a.x, b.x)+tol && p.y >= math.Min(a.y, b.y)-tol && p.y <= math.Max(a.y, b.y)+tol
}
func planeSegmentsMeet(a, b, c, d planeXY, tol float64) bool {
	if planeOnSegment(a, c, d, tol) || planeOnSegment(b, c, d, tol) || planeOnSegment(c, a, b, tol) || planeOnSegment(d, a, b, tol) {
		return true
	}
	x, y, z, w := planeOrient(a, b, c), planeOrient(a, b, d), planeOrient(c, d, a), planeOrient(c, d, b)
	return (x < 0) != (y < 0) && (z < 0) != (w < 0)
}
func planeInside(p planeXY, xy []planeXY, ids []uint32) bool {
	inside := false
	for i, a := range ids {
		b := ids[(i+1)%len(ids)]
		u, v := xy[a], xy[b]
		if (u.y > p.y) != (v.y > p.y) && p.x < (v.x-u.x)*(p.y-u.y)/(v.y-u.y)+u.x {
			inside = !inside
		}
	}
	return inside
}
func planeNest(ctx context.Context, points [][3]float64, loops []SectionPolyline, n [3]float64, tol float64, step func() error) error {
	xy := planeProject(points, n)
	for a, l := range loops {
		if !l.Closed {
			continue
		}
		if math.Abs(planeArea(xy, l.PointIDs)) <= tol*tol {
			return fmt.Errorf("%w: zero-area section loop", ErrPlaneGeometry)
		}
		for b := a; b < len(loops); b++ {
			m := loops[b]
			if !m.Closed {
				continue
			}
			for i, id := range l.PointIDs {
				u := l.PointIDs[(i+1)%len(l.PointIDs)]
				for j, jd := range m.PointIDs {
					if e := step(); e != nil {
						return e
					}
					v := m.PointIDs[(j+1)%len(m.PointIDs)]
					if a == b && (i == j || u == jd || id == v) {
						continue
					}
					if planeSegmentsMeet(xy[id], xy[u], xy[jd], xy[v], tol) {
						return fmt.Errorf("%w: touching/intersecting section loops", ErrPlaneGeometry)
					}
				}
			}
		}
	}
	for a := range loops {
		loops[a].Parent = -1
		loops[a].Depth = 0
		loops[a].Hole = false
		if !loops[a].Closed {
			continue
		}
		best := math.Inf(1)
		for b, m := range loops {
			if e := step(); e != nil {
				return e
			}
			if a == b || !m.Closed {
				continue
			}
			if planeInside(xy[loops[a].PointIDs[0]], xy, m.PointIDs) {
				area := math.Abs(planeArea(xy, m.PointIDs))
				if area < best {
					best = area
					loops[a].Parent = b
				}
			}
		}
	}
	for a := range loops {
		if !loops[a].Closed {
			continue
		}
		p := loops[a].Parent
		for p >= 0 {
			if e := step(); e != nil {
				return e
			}
			loops[a].Depth++
			if loops[a].Depth > len(loops) {
				return fmt.Errorf("%w: loop nesting cycle", ErrPlaneGeometry)
			}
			p = loops[p].Parent
		}
		loops[a].Hole = loops[a].Depth%2 == 1
	}
	return ctx.Err()
}
func planeReverse(ids []uint32) {
	for a, b := 0, len(ids)-1; a < b; a, b = a+1, b-1 {
		ids[a], ids[b] = ids[b], ids[a]
	}
}

// caps triangulates visible hole bridges, then ears of the weakly simple
// ring. Original boundary vertex IDs are retained (including collinear
// vertices); duplicate bridge endpoint occurrences refer to the same ID.
func (r *planeRun) caps(loops []SectionPolyline, n [3]float64) ([][3]uint32, error) {
	points := make([][3]float64, len(r.vertices))
	for i, v := range r.vertices {
		points[i] = v.p
	}
	xy := planeProject(points, n)
	if e := planeNest(r.ctx, points, loops, n, r.o.ToleranceMM, r.step); e != nil {
		return nil, e
	}
	for _, l := range loops {
		area := planeArea(xy, l.PointIDs)
		if (area > 0) == l.Hole {
			return nil, fmt.Errorf("%w: inconsistent nested shell winding", ErrPlaneGeometry)
		}
	}
	out := make([][3]uint32, 0)
	for i, l := range loops {
		if l.Hole {
			continue
		}
		ring := append([]uint32(nil), l.PointIDs...)
		expected := planeArea(xy, ring)
		for h, hole := range loops {
			if hole.Parent != i || !hole.Hole {
				continue
			}
			expected += planeArea(xy, hole.PointIDs)
			var e error
			ring, e = r.bridge(xy, ring, hole.PointIDs, loops, i, h)
			if e != nil {
				return nil, e
			}
		}
		triangles, e := r.ears(xy, ring, loops, i)
		if e != nil {
			return nil, e
		}
		area := 0.0
		for _, t := range triangles {
			area += planeOrient(xy[t[0]], xy[t[1]], xy[t[2]]) / 2
		}
		// This is a geometric validation, not an excuse to fill each hole as a disk.
		if math.Abs(area-expected) > math.Max(r.o.ToleranceMM*r.o.ToleranceMM*float64(len(ring)), math.Abs(expected)*1e-9) {
			return nil, fmt.Errorf("%w: cap area mismatch", ErrPlaneGeometry)
		}
		if int64(len(out)+len(triangles)) > r.o.MaxResultTriangles {
			return nil, &QualityLimitError{"plane cap faces", int64(len(out) + len(triangles)), r.o.MaxResultTriangles}
		}
		out = append(out, triangles...)
	}
	return out, nil
}
func (r *planeRun) domain(p planeXY, xy []planeXY, loops []SectionPolyline, outer int) bool {
	if !planeInside(p, xy, loops[outer].PointIDs) {
		return false
	}
	for _, l := range loops {
		if l.Parent == outer && l.Hole && planeInside(p, xy, l.PointIDs) {
			return false
		}
	}
	return true
}
func (r *planeRun) bridge(xy []planeXY, ring, hole []uint32, loops []SectionPolyline, outer, h int) ([]uint32, error) {
	ai, bi := -1, -1
	best := math.Inf(1)
	for i, a := range ring {
		for j, b := range hole {
			if e := r.step(); e != nil {
				return nil, e
			}
			if a == b {
				continue
			}
			dx, dy := xy[a].x-xy[b].x, xy[a].y-xy[b].y
			d := dx*dx + dy*dy
			if d >= best {
				continue
			}
			ok := true
			// Test against all original boundaries and every already installed bridge.
			boundaries := make([][]uint32, 0, len(loops)+1)
			boundaries = append(boundaries, ring)
			for _, l := range loops {
				if l.Closed {
					boundaries = append(boundaries, l.PointIDs)
				}
			}
			for _, ids := range boundaries {
				for k, c := range ids {
					if e := r.step(); e != nil {
						return nil, e
					}
					dd := ids[(k+1)%len(ids)]
					if c == a || c == b || dd == a || dd == b {
						continue
					}
					if planeSegmentsMeet(xy[a], xy[b], xy[c], xy[dd], r.o.ToleranceMM) {
						ok = false
						break
					}
				}
				if !ok {
					break
				}
			}
			mid := planeXY{(xy[a].x + xy[b].x) / 2, (xy[a].y + xy[b].y) / 2}
			if ok && r.domain(mid, xy, loops, outer) {
				ai, bi, best = i, j, d
			}
		}
	}
	if ai < 0 {
		return nil, fmt.Errorf("%w: no visible cavity bridge (hole %d)", ErrPlaneGeometry, h)
	}
	result := make([]uint32, 0, len(ring)+len(hole)+2)
	result = append(result, ring[:ai+1]...)
	for k := 0; k < len(hole); k++ {
		result = append(result, hole[(bi+k)%len(hole)])
	}
	result = append(result, hole[bi], ring[ai])
	result = append(result, ring[ai+1:]...)
	return result, nil
}
func (r *planeRun) ears(xy []planeXY, ring []uint32, loops []SectionPolyline, outer int) ([][3]uint32, error) {
	ids := append([]uint32(nil), ring...)
	triangles := make([][3]uint32, 0, len(ids)-2)
	for len(ids) > 3 {
		found := false
		for i, b := range ids {
			if e := r.step(); e != nil {
				return nil, e
			}
			a, c := ids[(i+len(ids)-1)%len(ids)], ids[(i+1)%len(ids)]
			if a == b || b == c || a == c {
				continue
			}
			// A diagonal midpoint can acquire a tiny positive orientation when
			// an oblique plane is projected. Preserve it as a boundary vertex,
			// but do not remove it as a zero-width ear. Use the existing physical
			// tolerance; domain, area, winding and final closure checks stay strict.
			if !planeCapEarNondegenerate(xy[a], xy[b], xy[c], r.o.ToleranceMM) {
				continue
			}
			if r.acceptTriangle != nil {
				if !r.acceptTriangle([3]uint32{a, b, c}) {
					continue
				}
				if len(ids) == 4 {
					last := [3]uint32{}
					at := 0
					for j, id := range ids {
						if j != i {
							last[at] = id
							at++
						}
					}
					if !planeCapEarNondegenerate(xy[last[0]], xy[last[1]], xy[last[2]], r.o.ToleranceMM) || !r.acceptTriangle(last) {
						continue
					}
				}
			}
			ok := true
			for _, v := range ids {
				if e := r.step(); e != nil {
					return nil, e
				}
				if v == a || v == b || v == c || planeSame(xy[v], xy[a]) || planeSame(xy[v], xy[b]) || planeSame(xy[v], xy[c]) {
					continue
				}
				if planePointInCapEar(xy[a], xy[b], xy[c], xy[v], r.o.ToleranceMM) {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			centroid := planeXY{(xy[a].x + xy[b].x + xy[c].x) / 3, (xy[a].y + xy[b].y + xy[c].y) / 3}
			if !r.domain(centroid, xy, loops, outer) {
				continue
			}
			// A diagonal may not cross any current edge except at its endpoints.
			for k, v := range ids {
				if e := r.step(); e != nil {
					return nil, e
				}
				w := ids[(k+1)%len(ids)]
				if v == a || v == c || w == a || w == c {
					continue
				}
				if planeSegmentsMeet(xy[a], xy[c], xy[v], xy[w], 0) {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			triangles = append(triangles, [3]uint32{a, b, c})
			ids = append(ids[:i], ids[i+1:]...)
			found = true
			break
		}
		if !found {
			return nil, fmt.Errorf("%w: cap triangulation inconclusive", ErrPlaneGeometry)
		}
	}
	if len(ids) != 3 || !planeCapEarNondegenerate(xy[ids[0]], xy[ids[1]], xy[ids[2]], r.o.ToleranceMM) {
		return nil, fmt.Errorf("%w: degenerate final cap ear", ErrPlaneGeometry)
	}
	if r.acceptTriangle != nil && !r.acceptTriangle([3]uint32{ids[0], ids[1], ids[2]}) {
		return nil, fmt.Errorf("%w: unrepresentable final ear", ErrPlaneGeometry)
	}
	center := planeXY{(xy[ids[0]].x + xy[ids[1]].x + xy[ids[2]].x) / 3, (xy[ids[0]].y + xy[ids[1]].y + xy[ids[2]].y) / 3}
	if !r.domain(center, xy, loops, outer) {
		return nil, fmt.Errorf("%w: cap outside solid", ErrPlaneGeometry)
	}
	triangles = append(triangles, [3]uint32{ids[0], ids[1], ids[2]})
	return triangles, nil
}
