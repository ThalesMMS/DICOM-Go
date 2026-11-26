package mesh

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strconv"
	"sync"
	"testing"
)

func plane1497Box(lo, hi [3]float32) *Mesh {
	m := &Mesh{Vertices: [][3]float32{{lo[0], lo[1], lo[2]}, {hi[0], lo[1], lo[2]}, {hi[0], hi[1], lo[2]}, {lo[0], hi[1], lo[2]}, {lo[0], lo[1], hi[2]}, {hi[0], lo[1], hi[2]}, {hi[0], hi[1], hi[2]}, {lo[0], hi[1], hi[2]}}, Triangles: [][3]uint32{{0, 2, 1}, {0, 3, 2}, {4, 5, 6}, {4, 6, 7}, {0, 1, 5}, {0, 5, 4}, {1, 2, 6}, {1, 6, 5}, {2, 3, 7}, {2, 7, 6}, {3, 0, 4}, {3, 4, 7}}}
	return m
}
func plane1497Merge(ms ...*Mesh) *Mesh {
	out := &Mesh{}
	for _, m := range ms {
		off := uint32(len(out.Vertices))
		out.Vertices = append(out.Vertices, m.Vertices...)
		for _, t := range m.Triangles {
			out.Triangles = append(out.Triangles, [3]uint32{t[0] + off, t[1] + off, t[2] + off})
		}
	}
	return out
}
func plane1497Reverse(m *Mesh) {
	for i := range m.Triangles {
		m.Triangles[i][1], m.Triangles[i][2] = m.Triangles[i][2], m.Triangles[i][1]
	}
}
func plane1497Copy(m *Mesh) *Mesh {
	return &Mesh{Vertices: append([][3]float32(nil), m.Vertices...), Normals: append([][3]float32(nil), m.Normals...), Triangles: append([][3]uint32(nil), m.Triangles...)}
}
func plane1497Sphere() *Mesh {
	const rings, segments = 12, 24
	m := &Mesh{Vertices: [][3]float32{{0, 0, 1}}}
	for j := 1; j < rings; j++ {
		theta := math.Pi * float64(j) / rings
		for i := 0; i < segments; i++ {
			phi := 2 * math.Pi * float64(i) / segments
			m.Vertices = append(m.Vertices, [3]float32{float32(math.Sin(theta) * math.Cos(phi)), float32(math.Sin(theta) * math.Sin(phi)), float32(math.Cos(theta))})
		}
	}
	south := uint32(len(m.Vertices))
	m.Vertices = append(m.Vertices, [3]float32{0, 0, -1})
	for i := 0; i < segments; i++ {
		a, b := uint32(1+i), uint32(1+(i+1)%segments)
		m.Triangles = append(m.Triangles, [3]uint32{0, a, b})
	}
	for j := 0; j < rings-2; j++ {
		for i := 0; i < segments; i++ {
			a, b := uint32(1+j*segments+i), uint32(1+j*segments+(i+1)%segments)
			c, d := a+segments, b+segments
			m.Triangles = append(m.Triangles, [3]uint32{a, c, d}, [3]uint32{a, d, b})
		}
	}
	for i := 0; i < segments; i++ {
		a, b := uint32(1+(rings-2)*segments+i), uint32(1+(rings-2)*segments+(i+1)%segments)
		m.Triangles = append(m.Triangles, [3]uint32{a, south, b})
	}
	return m
}
func plane1497Concave() *Mesh {
	xy := [][2]float32{{0, 0}, {3, 0}, {3, 1}, {1, 1}, {1, 3}, {0, 3}}
	m := &Mesh{}
	for _, z := range []float32{-1, 1} {
		for _, p := range xy {
			m.Vertices = append(m.Vertices, [3]float32{p[0], p[1], z})
		}
	}
	for _, t := range [][3]uint32{{0, 1, 3}, {1, 2, 3}, {0, 3, 5}, {3, 4, 5}} {
		m.Triangles = append(m.Triangles, [3]uint32{t[0], t[2], t[1]}, [3]uint32{t[0] + 6, t[1] + 6, t[2] + 6})
	}
	for i := 0; i < 6; i++ {
		a, b := uint32(i), uint32((i+1)%6)
		m.Triangles = append(m.Triangles, [3]uint32{a, b, b + 6}, [3]uint32{a, b + 6, a + 6})
	}
	return m
}

// The test computes oriented planar shoelace areas independently of cap code.
func plane1497XYArea(s *PlaneSection) float64 {
	area := 0.0
	for _, l := range s.Polylines {
		a := 0.0
		for i, id := range l.PointIDs {
			p, q := s.Points[id], s.Points[l.PointIDs[(i+1)%len(l.PointIDs)]]
			a += p[0]*q[1] - p[1]*q[0]
		}
		a = math.Abs(a / 2)
		if l.Hole {
			area -= a
		} else {
			area += a
		}
	}
	return area
}
func TestPlaneSectionClosedPhantoms(t *testing.T) {
	for _, tc := range []struct {
		name         string
		m            *Mesh
		area, volume float64
	}{{"box", plane1497Box([3]float32{-1, -1, -1}, [3]float32{1, 1, 1}), 4, 4}, {"concave", plane1497Concave(), 5, 5}, {"sphere", plane1497Sphere(), 12 * math.Sin(math.Pi/12), 0}} {
		t.Run(tc.name, func(t *testing.T) {
			before := plane1497Copy(tc.m)
			s, e := SectionByPlane(context.Background(), tc.m, Plane{Normal: [3]float64{0, 0, 2}}, PlaneOptions{})
			if e != nil {
				t.Fatal(e)
			}
			if len(s.Polylines) != 1 || !s.Polylines[0].Closed || s.Polylines[0].Hole {
				t.Fatalf("section %+v", s.Polylines)
			}
			if math.Abs(plane1497XYArea(s)-tc.area) > 2e-6 {
				t.Fatalf("area %g want %g", plane1497XYArea(s), tc.area)
			}
			r, e := ClipClosedByPlane(context.Background(), tc.m, Plane{Normal: [3]float64{0, 0, 1}}, PlaneOptions{KeepPositive: true})
			if e != nil {
				t.Fatal(e)
			}
			requireClosedManifold(t, r.Mesh)
			want := tc.volume
			if want == 0 {
				want = tc.m.Volume() / 2
			}
			if math.Abs(r.Mesh.Volume()-want) > 2e-6 {
				t.Fatalf("volume %g want %g", r.Mesh.Volume(), want)
			}
			if !reflect.DeepEqual(before, tc.m) {
				t.Fatal("source mutation")
			}
			for i, src := range r.SourceTriangles {
				if src < 0 && r.Mesh.triangleNormal(r.Mesh.Triangles[i])[2] >= 0 {
					t.Fatal("cap winding")
				}
			}
		})
	}
}
func TestPlaneClipCavityAndNestedIslands(t *testing.T) {
	outer := plane1497Box([3]float32{-3, -3, -2}, [3]float32{3, 3, 2})
	inner := plane1497Box([3]float32{-2, -2, -1}, [3]float32{2, 2, 1})
	plane1497Reverse(inner)
	for _, tc := range []struct {
		name         string
		m            *Mesh
		area, volume float64
		loops        int
	}{{"cavity", plane1497Merge(outer, inner), 20, 56, 2}, {"island", plane1497Merge(outer, inner, plane1497Box([3]float32{-.5, -.5, -.5}, [3]float32{.5, .5, .5})), 21, 56.5, 3}} {
		t.Run(tc.name, func(t *testing.T) {
			r, e := ClipClosedByPlane(context.Background(), tc.m, Plane{Normal: [3]float64{0, 0, 1}}, PlaneOptions{KeepPositive: true})
			if e != nil {
				t.Fatal(e)
			}
			requireClosedManifold(t, r.Mesh)
			if len(r.Section.Polylines) != tc.loops || math.Abs(plane1497XYArea(r.Section)-tc.area) > 1e-8 || math.Abs(r.Mesh.Volume()-tc.volume) > 1e-8 {
				t.Fatalf("loops %d area %g volume %g", len(r.Section.Polylines), plane1497XYArea(r.Section), r.Mesh.Volume())
			}
			// A cap triangle's centroid must not cover the cavity (except the island).
			for i, src := range r.SourceTriangles {
				if src >= 0 {
					continue
				}
				p := [3]float64{}
				for _, id := range r.Mesh.Triangles[i] {
					for a, v := range r.Mesh.Vertices[id] {
						p[a] += float64(v) / 3
					}
				}
				if math.Abs(p[0]) < 2 && math.Abs(p[1]) < 2 && (tc.loops == 2 || math.Abs(p[0]) > .5 || math.Abs(p[1]) > .5) {
					t.Fatalf("cap fills cavity at %v", p)
				}
			}
		})
	}
}
func TestPlaneSectionTangentCoincidentAndSides(t *testing.T) {
	m := plane1497Box([3]float32{-1, -1, -1}, [3]float32{1, 1, 1})
	for _, tc := range []struct {
		name          string
		p             Plane
		lines, points int
	}{{"face", Plane{Origin: [3]float64{0, 0, 1}, Normal: [3]float64{0, 0, 1}}, 1, 0}, {"edge", Plane{Origin: [3]float64{1, 1, 0}, Normal: [3]float64{1, 1, 0}}, 1, 0}, {"vertex", Plane{Origin: [3]float64{1, 1, 1}, Normal: [3]float64{1, 1, 1}}, 0, 1}, {"empty", Plane{Origin: [3]float64{0, 0, 2}, Normal: [3]float64{0, 0, 1}}, 0, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			s, e := SectionByPlane(context.Background(), m, tc.p, PlaneOptions{})
			if e != nil {
				t.Fatal(e)
			}
			if len(s.Polylines) != tc.lines || s.TangentVertices != tc.points {
				t.Fatalf("%+v", s)
			}
			for _, keep := range []bool{false, true} {
				r, e := ClipClosedByPlane(context.Background(), m, tc.p, PlaneOptions{KeepPositive: keep})
				if e != nil {
					t.Fatal(e)
				}
				want := m.Volume()
				if keep {
					want = 0
				}
				if math.Abs(r.Mesh.Volume()-want) > 1e-8 {
					t.Fatalf("side %v volume %g", keep, r.Mesh.Volume())
				}
			}
		})
	}
	t.Run("projected roundoff sliver", func(t *testing.T) {
		// One ulp above a straight projected diagonal, with positive winding:
		// it must not become an ear, whereas a resolved triangle must.
		a, c := planeXY{0, 0}, planeXY{2, 2}
		b := planeXY{1, math.Nextafter(1, math.Inf(-1))}
		if planeOrient(a, b, c) <= 0 {
			t.Fatal("control requires positive residual orientation")
		}
		if planeCapEarNondegenerate(a, b, c, 1e-6) {
			t.Fatal("roundoff sliver admitted")
		}
		if !planeCapEarNondegenerate(a, planeXY{1, 0}, c, 1e-6) {
			t.Fatal("resolved triangle rejected")
		}
		// A contour point one ULP outside the exact diagonal must still
		// block an ear that would skip its boundary ID.
		a, b, c = planeXY{0, 0}, planeXY{2, 0}, planeXY{0, 2}
		on := planeXY{1, math.Nextafter(1, math.Inf(1))}
		if planeOrient(b, c, on) >= 0 {
			t.Fatal("control requires negative diagonal residual")
		}
		if !planePointInCapEar(a, b, c, on, 1e-6) {
			t.Fatal("boundary ID can be skipped")
		}
		if planePointInCapEar(a, b, c, planeXY{1, 1.1}, 1e-6) {
			t.Fatal("resolved outside point admitted")
		}
	})
	// Oblique exact cut through cube vertices/edges, symmetric complementary solids.
	p := Plane{Normal: [3]float64{1, 1, 1}}
	for _, keep := range []bool{false, true} {
		r, e := ClipClosedByPlane(context.Background(), m, p, PlaneOptions{KeepPositive: keep})
		if e != nil {
			t.Fatal(e)
		}
		requireClosedManifold(t, r.Mesh)
		if math.Abs(r.Mesh.Volume()-4) > 1e-6 {
			t.Fatal(r.Mesh.Volume())
		}
		capArea := 0.0
		for i, face := range r.Mesh.Triangles {
			if r.SourceTriangles[i] != -1 {
				continue
			}
			a, b, c := r.Mesh.point(face[0]), r.Mesh.point(face[1]), r.Mesh.point(face[2])
			n := cross3(sub3(b, a), sub3(c, a))
			area := math.Sqrt(dot3(n, n)) / 2
			if area <= 0 {
				t.Fatal("zero-width oblique cap ear")
			}
			capArea += area
		}
		// The x+y+z=0 cube section is the regular hexagon with six
		// permutations of (1,0,-1), area 3*sqrt(3), on both retained sides.
		if math.Abs(capArea-3*math.Sqrt(3)) > 1e-6 {
			t.Fatalf("oblique cap area %g", capArea)
		}
	}
}
func TestPlaneClipAttributesOwnershipAndDeterminism(t *testing.T) {
	m := plane1497Box([3]float32{-1, -1, -1}, [3]float32{1, 1, 1})
	m.Normals = make([][3]float32, 8)
	v := make([]float64, 8)
	colors := make([][3]uint8, 12)
	for i, p := range m.Vertices {
		m.Normals[i] = [3]float32{0, 0, 1}
		v[i] = 2*float64(p[0]) - 3*float64(p[1]) + 5*float64(p[2]) + 7
	}
	for i := range colors {
		colors[i] = [3]uint8{uint8(i), 12, 34}
	}
	o := PlaneOptions{KeepPositive: true, VertexScalars: []VertexScalarAttribute{{"LINEAR", v, "mm"}}, FaceColors: colors, CapColor: [3]uint8{255, 0, 0}, SourceReference: "synthetic:cube"}
	p := Plane{Origin: [3]float64{0, 0, .25}, Normal: [3]float64{0, 0, 1}}
	r, e := ClipClosedByPlane(context.Background(), m, p, o)
	if e != nil {
		t.Fatal(e)
	}
	for i, q := range r.Mesh.Vertices {
		want := 2*float64(q[0]) - 3*float64(q[1]) + 5*float64(q[2]) + 7
		if math.Abs(r.VertexScalars[0].Values[i]-want) > 1e-10 {
			t.Fatal("scalar interpolation")
		}
	}
	for i, id := range r.SourceTriangles {
		want := o.CapColor
		if id >= 0 {
			want = colors[id]
		}
		if r.FaceColors[i] != want {
			t.Fatal("face provenance color")
		}
	}
	var wg sync.WaitGroup
	for j := 0; j < 4; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr, err := ClipClosedByPlane(context.Background(), m, p, o)
			if err != nil || !reflect.DeepEqual(r, rr) {
				t.Errorf("concurrent/deterministic %v", err)
			}
		}()
	}
	wg.Wait()
	before := v[0]
	r.VertexScalars[0].Values[0] = 123
	r.Mesh.Vertices[0][0] = 123
	r.FaceColors[0][0] = 123
	if v[0] != before || colors[0][0] != 0 || m.Vertices[0][0] != -1 {
		t.Fatal("aliased output")
	}
}

type plane1497Cancel struct {
	context.Context
	n, after int
}

func (c *plane1497Cancel) Err() error {
	c.n++
	if c.n >= c.after {
		return context.Canceled
	}
	return nil
}
func TestPlaneClipAdmissionCancellationAndInvalidGeometry(t *testing.T) {
	m := plane1497Box([3]float32{-1, -1, -1}, [3]float32{1, 1, 1})
	p := Plane{Normal: [3]float64{0, 0, 1}}
	for name, o := range map[string]PlaneOptions{"vertices": {MaxVertices: 7}, "triangles": {MaxTriangles: 11}, "bytes": {MaxBytes: 1}, "work": {MaxWork: 1}, "output": {MaxResultVertices: 8}} {
		t.Run(name, func(t *testing.T) {
			r, e := ClipClosedByPlane(context.Background(), m, p, o)
			if r != nil || !errors.Is(e, ErrBudgetExceeded) {
				t.Fatalf("%+v %v", r, e)
			}
		})
	}
	for _, after := range []int{1, 25, 120} {
		ctx := &plane1497Cancel{Context: context.Background(), after: after}
		r, e := ClipClosedByPlane(ctx, m, p, PlaneOptions{})
		if r != nil || !errors.Is(e, context.Canceled) {
			t.Fatalf("cancel%d %v", after, e)
		}
	}
	for name, bad := range map[string]*Mesh{"open": {Vertices: m.Vertices, Triangles: m.Triangles[1:]}, "flipped": plane1497Copy(m), "intersection": plane1497Merge(m, plane1497Box([3]float32{0, 0, 0}, [3]float32{2, 2, 2}))} {
		if name == "flipped" {
			bad.Triangles[0][1], bad.Triangles[0][2] = bad.Triangles[0][2], bad.Triangles[0][1]
		}
		r, e := ClipClosedByPlane(context.Background(), bad, p, PlaneOptions{})
		if e == nil || r != nil {
			t.Fatal(name, "invalid accepted")
		}
	}
	t.Run("vertex-only welded shells", func(t *testing.T) {
		a := plane1497Box([3]float32{-1, -1, -1}, [3]float32{1, 1, 1})
		b := plane1497Box([3]float32{1, 1, 1}, [3]float32{3, 3, 3})
		bad := plane1497Merge(a, b)
		// A vertex 6 and B vertex 0 occupy the same corner. Weld only that ID.
		for i := len(a.Triangles); i < len(bad.Triangles); i++ {
			for j, v := range bad.Triangles[i] {
				if v == 8 {
					bad.Triangles[i][j] = 6
				}
			}
		}
		if !bad.CheckClosure().Closed() {
			t.Fatal("fixture must pass edge closure")
		}
		before := plane1497Copy(bad)
		outside := Plane{Origin: [3]float64{0, 0, -2}, Normal: [3]float64{0, 0, 1}}
		if out, err := ClipClosedByPlane(context.Background(), bad, outside, PlaneOptions{KeepPositive: true}); out != nil || !errors.Is(err, ErrPlaneGeometry) {
			t.Fatalf("pinched clip admitted: %+v %v", out, err)
		}
		if out, err := SectionByPlane(context.Background(), bad, outside, PlaneOptions{}); out != nil || !errors.Is(err, ErrPlaneGeometry) {
			t.Fatalf("pinched section admitted: %+v %v", out, err)
		}
		if !reflect.DeepEqual(before, bad) {
			t.Fatal("invalid input mutated")
		}
	})
	for _, pp := range []Plane{{}, {Normal: [3]float64{math.NaN(), 0, 1}}, {Origin: [3]float64{math.Inf(1), 0, 0}, Normal: [3]float64{0, 0, 1}}} {
		if r, e := ClipClosedByPlane(context.Background(), m, pp, PlaneOptions{}); r != nil || e == nil {
			t.Fatal("invalid plane")
		}
	}
	if r, e := ClipClosedByPlane(context.Background(), m, p, PlaneOptions{ToleranceMM: -1}); r != nil || e == nil {
		t.Fatal("invalid options")
	}
	if _, e := planeReserve(math.MaxInt64, math.MaxInt64, 32, true); !errors.Is(e, ErrBudgetExceeded) {
		t.Fatal("overflow")
	}
}

// The input and output are distinct domains: a valid admitted solid can clip
// to zero, even when the result quota could not hold the original solid.
func TestPlaneClipSeparateInputAndOutputQuotas(t *testing.T) {
	m := plane1497Box([3]float32{-1, -1, -1}, [3]float32{1, 1, 1})
	o := PlaneOptions{KeepPositive: true, MaxVertices: 8, MaxTriangles: 12, MaxResultVertices: 1, MaxResultTriangles: 1}
	p := Plane{Origin: [3]float64{0, 0, 2}, Normal: [3]float64{0, 0, 1}}
	r, e := ClipClosedByPlane(context.Background(), m, p, o)
	if e != nil || r == nil || len(r.Mesh.Vertices) != 0 || len(r.Mesh.Triangles) != 0 {
		t.Fatalf("empty result should fit: %+v %v", r, e)
	}
	s, e := SectionByPlane(context.Background(), m, p, o)
	if e != nil || s == nil || len(s.Points) != 0 {
		t.Fatalf("empty section should fit: %+v %v", s, e)
	}
	o.MaxVertices = 7
	if r, e = ClipClosedByPlane(context.Background(), m, p, o); r != nil || !errors.Is(e, ErrBudgetExceeded) {
		t.Fatalf("input quota lost: %+v %v", r, e)
	}
	o.MaxVertices = 8
	p.Origin = [3]float64{}
	if r, e = ClipClosedByPlane(context.Background(), m, p, o); r != nil || !errors.Is(e, ErrBudgetExceeded) {
		t.Fatalf("result quota lost: %+v %v", r, e)
	}
}
func TestPlaneClipTangentDisconnectedComponent(t *testing.T) {
	p := Plane{Origin: [3]float64{0, 0, 1}, Normal: [3]float64{0, 0, 1}}
	for _, keep := range []bool{false, true} {
		t.Run(strconv.FormatBool(keep), func(t *testing.T) {
			var tangent, solid *Mesh
			if keep {
				tangent = plane1497Box([3]float32{-1, -1, -1}, [3]float32{1, 1, 1})
				solid = plane1497Box([3]float32{-1, -1, 2}, [3]float32{1, 1, 4})
			} else {
				tangent = plane1497Box([3]float32{-1, -1, 1}, [3]float32{1, 1, 3})
				solid = plane1497Box([3]float32{-1, -1, -3}, [3]float32{1, 1, -1})
			}
			m := plane1497Merge(tangent, solid)
			before := plane1497Copy(m)
			r, e := ClipClosedByPlane(context.Background(), m, p, PlaneOptions{KeepPositive: keep})
			if e != nil {
				t.Fatal(e)
			}
			requireClosedManifold(t, r.Mesh)
			if len(r.Mesh.Triangles) != 12 || len(r.Mesh.Vertices) != 8 || math.Abs(r.Mesh.Volume()-8) > 1e-8 {
				t.Fatalf("tangent component resurrected: vertices%d faces%d volume%g", len(r.Mesh.Vertices), len(r.Mesh.Triangles), r.Mesh.Volume())
			}
			for i, id := range r.SourceTriangles {
				if id < 12 {
					t.Fatalf("face%d from removed tangent component or new cap: %d", i, id)
				}
			}
			for _, v := range r.Mesh.Vertices {
				if keep && v[2] < 2 || !keep && v[2] > -1 {
					t.Fatal("tangent vertices retained")
				}
			}
			if len(r.Section.Polylines) != 1 || !r.Section.Polylines[0].Closed || math.Abs(plane1497XYArea(r.Section)-4) > 1e-8 {
				t.Fatal("tangent section lost")
			}
			if !reflect.DeepEqual(before, m) {
				t.Fatal("source mutation")
			}
		})
	}
}
func TestPlaneClipCanceledEmptyNeverPublishes(t *testing.T) {
	m := plane1497Box([3]float32{-1, -1, -1}, [3]float32{1, 1, 1})
	p := Plane{Origin: [3]float64{0, 0, 2}, Normal: [3]float64{0, 0, 1}}
	// Count the deterministic checks of this empty-result path, then cancel
	// exactly at its final publication fence. No production/test hook is added.
	count := &plane1497Cancel{Context: context.Background(), after: int(^uint(0) >> 1)}
	r, e := ClipClosedByPlane(count, m, p, PlaneOptions{KeepPositive: true})
	if e != nil || r == nil || len(r.Mesh.Triangles) != 0 {
		t.Fatalf("counting empty path: %v", e)
	}
	canceled := &plane1497Cancel{Context: context.Background(), after: count.n}
	r, e = ClipClosedByPlane(canceled, m, p, PlaneOptions{KeepPositive: true})
	if r != nil || !errors.Is(e, context.Canceled) {
		t.Fatalf("empty error published nonnil result: %+v %v", r, e)
	}
}
