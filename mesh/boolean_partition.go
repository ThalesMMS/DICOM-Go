package mesh

import (
	"fmt"
	"math"
)

type booleanPolygon struct {
	p         [][3]float64
	n, origin [3]float64
	input     uint8
	face      uint32
	reversed  bool
}
type booleanCut struct {
	n, origin [3]float64
	segment   [2][3]float64
}

func (r *booleanRun) polygon(points [][3]float64, source *booleanPolygon) (*booleanPolygon, error) {
	if int64(len(points)) > r.o.MaxPolygonVertices {
		return nil, &QualityLimitError{"boolean polygon vertices", int64(len(points)), r.o.MaxPolygonVertices}
	}
	if err := r.charge(&r.polygons, 1, r.o.MaxPolygons, "boolean polygons"); err != nil {
		return nil, err
	}
	if err := r.reserve(int64(len(points))*24 + 128); err != nil {
		return nil, err
	}
	p := *source
	p.p = append([][3]float64(nil), points...)
	return &p, nil
}
func (r *booleanRun) sourcePolygons(input uint8) ([]*booleanPolygon, error) {
	m := r.inputs[input].Mesh
	if err := r.reserve(int64(len(m.Triangles)) * 8); err != nil {
		return nil, err
	}
	out := make([]*booleanPolygon, 0, len(m.Triangles))
	for face, t := range m.Triangles {
		if err := r.step(); err != nil {
			return nil, err
		}
		points := [][3]float64{m.point(t[0]), m.point(t[1]), m.point(t[2])}
		p, err := r.polygon(points, &booleanPolygon{n: qualityNormal([3][3]float64{points[0], points[1], points[2]}), origin: points[0], input: input, face: uint32(face)})
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// split uses only a scale-dependent roundoff band; the physical tolerance
// never moves a source vertex onto a plane. Coincident points are retained.
func (r *booleanRun) split(p *booleanPolygon, cutSpec booleanCut) (front, back *booleanPolygon, err error) {
	n, origin := cutSpec.n, cutSpec.origin
	if err = r.step(); err != nil {
		return
	}
	if err = r.reserve(int64(len(p.p)) * 8); err != nil {
		return
	}
	d := make([]float64, len(p.p))
	positive, negative := false, false
	for i, v := range p.p {
		if err = r.step(); err != nil {
			return
		}
		d[i] = dot3(sub3(v, origin), n)
		positive = positive || d[i] > r.eps
		negative = negative || d[i] < -r.eps
	}
	if !positive && !negative {
		err = fmt.Errorf("%w: unresolved coplanar partition", ErrBooleanGeometry)
		return
	}
	if !negative {
		front = p
		return
	}
	if !positive {
		back = p
		return
	}
	if err = r.reserve(int64(len(p.p)+2) * 48); err != nil {
		return
	}
	f, b := make([][3]float64, 0, len(p.p)+2), make([][3]float64, 0, len(p.p)+2)
	for i, v := range p.p {
		if err = r.step(); err != nil {
			return
		}
		j := (i + 1) % len(p.p)
		if d[i] >= -r.eps {
			f = append(f, v)
		}
		if d[i] <= r.eps {
			b = append(b, v)
		}
		if d[i] > r.eps && d[j] < -r.eps || d[i] < -r.eps && d[j] > r.eps {
			weight := d[i] / (d[i] - d[j])
			cut := add3(v, scale3(sub3(p.p[j], v), weight))
			// Use an already certified triangle/triangle endpoint when it lies
			// on this edge. Intersecting nearly parallel subdivision planes
			// again is ill-conditioned and needlessly invents a different seam.
			edge := sub3(p.p[j], v)
			edge2 := dot3(edge, edge)
			var snapped [3]float64
			haveSnapped := false
			for _, endpoint := range cutSpec.segment {
				t := dot3(sub3(endpoint, v), edge) / edge2
				if t < 0 || t > 1 {
					continue
				}
				delta := sub3(endpoint, add3(v, scale3(edge, t)))
				if dot3(delta, delta) > 64*r.eps*r.eps {
					continue
				}
				delta = sub3(endpoint, cut)
				if math.Sqrt(dot3(delta, delta)) > r.o.ToleranceMM {
					continue
				}
				if haveSnapped {
					delta = sub3(endpoint, snapped)
					if dot3(delta, delta) > r.eps*r.eps {
						err = fmt.Errorf("%w: ambiguous intersection endpoint", ErrBooleanGeometry)
						return
					}
				}
				snapped = endpoint
				haveSnapped = true
			}
			if haveSnapped {
				cut = snapped
			}
			f, b = append(f, cut), append(b, cut)
		}
	}
	if len(f) < 3 || len(b) < 3 {
		err = fmt.Errorf("%w: unresolved plane split", ErrBooleanGeometry)
		return
	}
	front, err = r.polygon(f, p)
	if err == nil {
		back, err = r.polygon(b, p)
	}
	return
}

func (r *booleanRun) segmentInterior(p *booleanPolygon, s [2][3]float64) (bool, error) {
	lo, hi := 0.0, 1.0
	for i, a := range p.p {
		if err := r.step(); err != nil {
			return false, err
		}
		edge := sub3(p.p[(i+1)%len(p.p)], a)
		length := math.Sqrt(dot3(edge, edge))
		if length <= r.eps {
			return false, fmt.Errorf("%w: collapsed partition edge", ErrBooleanGeometry)
		}
		d0 := dot3(cross3(edge, sub3(s[0], a)), p.n) / length
		d1 := dot3(cross3(edge, sub3(s[1], a)), p.n) / length
		if d0 < -r.eps && d1 < -r.eps {
			return false, nil
		}
		if d0 < 0 && d1 > d0 {
			lo = math.Max(lo, -d0/(d1-d0))
		}
		if d1 < 0 && d0 > d1 {
			hi = math.Min(hi, d0/(d0-d1))
		}
		if hi <= lo {
			return false, nil
		}
	}
	delta := sub3(s[1], s[0])
	length := math.Sqrt(dot3(delta, delta))
	if (hi-lo)*length <= 16*r.eps {
		return false, nil
	}
	midpoint := add3(s[0], scale3(delta, (lo+hi)/2))
	for i, a := range p.p {
		if err := r.step(); err != nil {
			return false, err
		}
		edge := sub3(p.p[(i+1)%len(p.p)], a)
		length := math.Sqrt(dot3(edge, edge))
		if dot3(cross3(edge, sub3(midpoint, a)), p.n)/length <= r.eps {
			return false, nil
		}
	}
	return true, nil
}

func (r *booleanRun) partition(p *booleanPolygon, inside, reverse bool, out *[]*booleanPolygon) error {
	var parts []*booleanPolygon
	if err := booleanGrow(r, &parts, r.o.MaxPolygons, false); err != nil {
		return err
	}
	parts = append(parts, p)
	for _, cut := range r.cuts[p.input][p.face] {
		var next []*booleanPolygon
		for _, part := range parts {
			crosses, err := r.segmentInterior(part, cut.segment)
			if err != nil {
				return err
			}
			f, b := part, (*booleanPolygon)(nil)
			if crosses {
				f, b, err = r.split(part, cut)
				if err != nil {
					return err
				}
			}
			for _, piece := range []*booleanPolygon{f, b} {
				if piece != nil {
					if err := booleanGrow(r, &next, r.o.MaxPolygons, false); err != nil {
						return err
					}
					next = append(next, piece)
				}
			}
		}
		parts = next
	}
	for _, part := range parts {
		var center [3]float64
		for _, point := range part.p {
			center = add3(center, scale3(point, 1/float64(len(part.p))))
		}
		winding, err := r.winding(r.inputs[1-p.input].Mesh, nil, -1, center)
		if err != nil {
			return err
		}
		if winding < -.5 {
			return fmt.Errorf("%w: negative solid winding", ErrBooleanGeometry)
		}
		if (winding > .5) == inside {
			if err := r.keep(part, reverse, out); err != nil {
				return err
			}
		}
	}
	return nil
}
func (r *booleanRun) keep(p *booleanPolygon, reverse bool, out *[]*booleanPolygon) error {
	if err := booleanGrow(r, out, r.o.MaxPolygons, false); err != nil {
		return err
	}
	if reverse {
		q, err := r.polygon(p.p, p)
		if err != nil {
			return err
		}
		q.reversed = true
		for i, j := 0, len(q.p)-1; i < j; i, j = i+1, j-1 {
			q.p[i], q.p[j] = q.p[j], q.p[i]
		}
		q.n = scale3(q.n, -1)
		p = q
	}
	*out = append(*out, p)
	return nil
}
