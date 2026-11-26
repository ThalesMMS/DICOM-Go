package mesh

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
)

func subdivisionTestOptions(length float64) SubdivisionOptions {
	return SubdivisionOptions{MaxEdgeLengthMM: length, MaxVertices: 100000, MaxTriangles: 200000, MaxBytes: 64 << 20, MaxPasses: 16}
}
func subdivisionPlane() *Mesh {
	return &Mesh{Vertices: [][3]float32{{0, 0, 0}, {2, 0, 0}, {2, 1, 0}, {0, 1, 0}, {10, 0, 0}, {10.1, 0, 0}, {10, .1, 0}}, Triangles: [][3]uint32{{0, 1, 2}, {0, 2, 3}, {4, 5, 6}}}
}
func subdivisionTetra() *Mesh {
	return &Mesh{Vertices: [][3]float32{{0, 0, 0}, {2, 0, 0}, {0, 2, 0}, {0, 0, 2}}, Triangles: [][3]uint32{{0, 2, 1}, {0, 1, 3}, {0, 3, 2}, {1, 2, 3}}}
}
func subdivisionArea(m *Mesh) float64 {
	sum := 0.
	for _, f := range m.Triangles {
		n := cross3(sub3(subdivisionPoint(m, f[1]), subdivisionPoint(m, f[0])), sub3(subdivisionPoint(m, f[2]), subdivisionPoint(m, f[0])))
		sum += .5 * math.Sqrt(dot3(n, n))
	}
	return sum
}
func subdivisionSignedVolume(m *Mesh) float64 {
	v := 0.
	for _, f := range m.Triangles {
		v += dot3(subdivisionPoint(m, f[0]), cross3(subdivisionPoint(m, f[1]), subdivisionPoint(m, f[2]))) / 6
	}
	return v
}

// Independent incidence oracle: each boundary has one use, each interior two
// opposite uses. Geometric T-junction test checks every non-endpoint vertex.
func subdivisionCheckConformity(t *testing.T, m *Mesh, maxLength float64) int {
	t.Helper()
	uses := map[[2]uint32][][2]uint32{}
	for _, f := range m.Triangles {
		for i := 0; i < 3; i++ {
			a, b := f[i], f[(i+1)%3]
			k := [2]uint32{min(a, b), max(a, b)}
			uses[k] = append(uses[k], [2]uint32{a, b})
		}
	}
	boundary := 0
	for k, edges := range uses {
		if len(edges) == 1 {
			boundary++
		} else if len(edges) != 2 || edges[0][0] != edges[1][1] || edges[0][1] != edges[1][0] {
			t.Fatalf("nonconformant edge %v: %v", k, edges)
		}
		a, b := subdivisionPoint(m, k[0]), subdivisionPoint(m, k[1])
		ab := sub3(b, a)
		length2 := dot3(ab, ab)
		if math.Sqrt(length2) > maxLength {
			t.Fatalf("edge length %g exceeds %g", math.Sqrt(length2), maxLength)
		}
		for i := range m.Vertices {
			if uint32(i) == k[0] || uint32(i) == k[1] {
				continue
			}
			p := subdivisionPoint(m, uint32(i))
			ap := sub3(p, a)
			u := dot3(ap, ab) / length2
			if u > 1e-6 && u < 1-1e-6 {
				d := sub3(ap, [3]float64{u * ab[0], u * ab[1], u * ab[2]})
				if dot3(d, d) < 1e-14 {
					t.Fatalf("T-junction vertex%d edge%v", i, k)
				}
			}
		}
	}
	return boundary
}
func subdivisionBoundaryLoops(t *testing.T, m *Mesh) int {
	t.Helper()
	uses := map[[2]uint32]int{}
	for _, f := range m.Triangles {
		for i := 0; i < 3; i++ {
			a, b := f[i], f[(i+1)%3]
			uses[[2]uint32{min(a, b), max(a, b)}]++
		}
	}
	adjacent := map[uint32][]uint32{}
	for edge, n := range uses {
		if n == 1 {
			adjacent[edge[0]] = append(adjacent[edge[0]], edge[1])
			adjacent[edge[1]] = append(adjacent[edge[1]], edge[0])
		}
	}
	for v, neighbors := range adjacent {
		if len(neighbors) != 2 {
			t.Fatalf("boundary vertex%d degree%d", v, len(neighbors))
		}
	}
	seen := map[uint32]bool{}
	loops := 0
	for v := range adjacent {
		if seen[v] {
			continue
		}
		loops++
		stack := []uint32{v}
		seen[v] = true
		for len(stack) > 0 {
			x := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, y := range adjacent[x] {
				if !seen[y] {
					seen[y] = true
					stack = append(stack, y)
				}
			}
		}
	}
	return loops
}
func TestSubdivisionOpenClosedAnalytic(t *testing.T) {
	for _, m := range []*Mesh{subdivisionPlane(), subdivisionTetra()} {
		o := subdivisionTestOptions(.6)
		before := &Mesh{Vertices: append([][3]float32(nil), m.Vertices...), Triangles: append([][3]uint32(nil), m.Triangles...)}
		r, e := SubdivideEdges(nil, m, o)
		if e != nil {
			t.Fatal(e)
		}
		if !reflect.DeepEqual(m, before) {
			t.Fatal("source mutated")
		}
		if r.Report.Stop != "complete" || r.Report.MaxEdgeLengthMM > o.MaxEdgeLengthMM {
			t.Fatal(r.Report)
		}
		boundary := subdivisionCheckConformity(t, r.Mesh, o.MaxEdgeLengthMM)
		if subdivisionBoundaryLoops(t, m) != subdivisionBoundaryLoops(t, r.Mesh) || meshTopology(m).euler() != meshTopology(r.Mesh).euler() {
			t.Fatal("boundary loops/Euler changed")
		}

		_, a := ConnectedComponents(m)
		_, b := ConnectedComponents(r.Mesh)
		if a != b {
			t.Fatalf("components%d/%d", a, b)
		}
		if math.Abs(subdivisionArea(r.Mesh)-subdivisionArea(m)) > 1e-6 {
			t.Fatal("surface area changed")
		}
		for i, f := range r.Mesh.Triangles {
			parent := m.Triangles[r.ParentFaces[i]]
			a, b, c := subdivisionPoint(m, parent[0]), subdivisionPoint(m, parent[1]), subdivisionPoint(m, parent[2])
			normal := cross3(sub3(b, a), sub3(c, a))
			for _, vi := range f {
				p := subdivisionPoint(r.Mesh, vi)
				distance := math.Abs(dot3(sub3(p, a), normal)) / math.Sqrt(dot3(normal, normal))
				e1, e2, ap := sub3(b, a), sub3(c, a), sub3(p, a)
				d00, d01, d11 := dot3(e1, e1), dot3(e1, e2), dot3(e2, e2)
				d20, d21 := dot3(ap, e1), dot3(ap, e2)
				denominator := d00*d11 - d01*d01
				u, w := (d11*d20-d01*d21)/denominator, (d00*d21-d01*d20)/denominator
				if u < -1e-6 || w < -1e-6 || u+w > 1+1e-6 {
					t.Fatal("child outside original parent triangle")
				}

				if distance > 1e-6 {
					t.Fatalf("distance to original parent plane=%g", distance)
				}
			}
			childNormal := cross3(sub3(subdivisionPoint(r.Mesh, f[1]), subdivisionPoint(r.Mesh, f[0])), sub3(subdivisionPoint(r.Mesh, f[2]), subdivisionPoint(r.Mesh, f[0])))
			if dot3(childNormal, normal) <= 0 {
				t.Fatal("flipped winding")
			}
		}
		if a == 1 {
			if boundary != 0 || meshTopology(m).euler() != meshTopology(r.Mesh).euler() || math.Abs(subdivisionSignedVolume(r.Mesh)-subdivisionSignedVolume(m)) > 1e-6 {
				t.Fatal("closed topology/volume")
			}
		} else {
			if boundary == 0 {
				t.Fatal("open boundary lost")
			}
			count := 0
			for _, f := range r.Mesh.Triangles {
				if r.ParentFaces[count] == 2 && !reflect.DeepEqual(f, m.Triangles[2]) {
					t.Fatal("short disconnected face refined")
				}
				count++
			}
		}
		again, e := SubdivideEdges(nil, r.Mesh, o)
		if e != nil || !reflect.DeepEqual(again.Mesh, r.Mesh) || again.Report.Passes != 0 {
			t.Fatalf("repeat stability %v", e)
		}
		t.Logf("input=%d/%d output=%d/%d passes=%d chargedBytes=%d maxedge=%g", len(m.Vertices), len(m.Triangles), len(r.Mesh.Vertices), len(r.Mesh.Triangles), r.Report.Passes, r.Report.AllocatedBytes, r.Report.MaxEdgeLengthMM)
	}
}
func TestSubdivisionAllMasks(t *testing.T) {
	m := &Mesh{Vertices: [][3]float32{{0, 0, 0}, {2, 0, 0}, {0, 2, 0}, {1, 0, 0}, {1, 1, 0}, {0, 1, 0}}}
	for mask := 0; mask < 8; mask++ {
		mid := [3]uint32{math.MaxUint32, math.MaxUint32, math.MaxUint32}
		splits := 0
		for i := 0; i < 3; i++ {
			if mask&(1<<i) != 0 {
				mid[i] = uint32(3 + i)
				splits++
			}
		}
		children, n := subdivisionChildren([3]uint32{0, 1, 2}, mid)
		if n != 1+splits {
			t.Fatal("mask count")
		}
		out := &Mesh{Vertices: m.Vertices, Triangles: children[:n]}
		incidence := map[[2]uint32]int{}
		for _, f := range out.Triangles {
			for i := 0; i < 3; i++ {
				a, b := f[i], f[(i+1)%3]
				incidence[[2]uint32{min(a, b), max(a, b)}]++
			}
		}
		boundary, interior := 0, 0
		for _, count := range incidence {
			switch count {
			case 1:
				boundary++
			case 2:
				interior++
			default:
				t.Fatal("mask nonmanifold")
			}
		}
		if boundary != 3+splits || interior != n-1 {
			t.Fatalf("mask%d incidence boundary%d interior%d", mask, boundary, interior)
		}

		if subdivisionArea(out) != 2 {
			t.Fatalf("mask%d area", mask)
		}
		for _, f := range out.Triangles {
			a, b, c := subdivisionPoint(out, f[0]), subdivisionPoint(out, f[1]), subdivisionPoint(out, f[2])
			if cross3(sub3(b, a), sub3(c, a))[2] <= 0 {
				t.Fatalf("mask%d winding", mask)
			}
		}
	}
}
func TestSubdivisionAttributes(t *testing.T) {
	m := subdivisionTetra()
	m.Normals = make([][3]float32, len(m.Vertices))
	for i := range m.Normals {
		m.Normals[i] = [3]float32{0, 0, 1}
	}
	o := subdivisionTestOptions(.7)
	o.VertexAttributes = []SubdivisionVertexAttribute{{Attribute: VertexScalarAttribute{Name: "position_x", Units: "mm", Values: []float64{0, 2, 0, 0}}, Policy: SubdivisionLinear}, {Attribute: VertexScalarAttribute{Name: "category", Values: []float64{7, 7, 7, 7}}, Policy: SubdivisionEqualOnly}}
	o.FaceAttributes = []VertexScalarAttribute{{Name: "label", Values: []float64{11, 12, 13, 14}}}
	r, e := SubdivideEdges(nil, m, o)
	if e != nil {
		t.Fatal(e)
	}
	for i, v := range r.Mesh.Vertices {
		if r.VertexAttributes[0].Attribute.Values[i] != float64(v[0]) || r.VertexAttributes[1].Attribute.Values[i] != 7 || r.Mesh.Normals[i] != [3]float32{0, 0, 1} {
			t.Fatal("vertex interpolation")
		}
	}
	for i, p := range r.ParentFaces {
		if r.FaceAttributes[0].Values[i] != o.FaceAttributes[0].Values[p] {
			t.Fatal("face attributes")
		}
	}
	if len(o.VertexAttributes[0].Attribute.Values) != 4 {
		t.Fatal("source attr changed")
	}
	o.VertexAttributes[1].Attribute.Values[0] = 8
	if r, e := SubdivideEdges(nil, m, o); r != nil || !errors.Is(e, ErrInvalidField) {
		t.Fatal("categorical interpolation silently accepted")
	}
	o.VertexAttributes[1].Attribute.Values[0] = 7
	copy(m.Normals, [][3]float32{{0, 0, 1}, {0, 0, -1}, {0, 0, 1}, {0, 0, 1}})
	if r, e := SubdivideEdges(nil, m, o); r != nil || !errors.Is(e, ErrSubdivisionPrecision) {
		t.Fatal("singular normal accepted")
	}
}
func TestSubdivisionBudgetsCancellation(t *testing.T) {
	m := subdivisionTetra()
	o := subdivisionTestOptions(.7)
	r, e := SubdivideEdges(nil, m, o)
	if e != nil {
		t.Fatal(e)
	}
	o.MaxBytes = r.Report.AllocatedBytes
	if _, e = SubdivideEdges(nil, m, o); e != nil {
		t.Fatal("exact byte budget", e)
	}
	o.MaxBytes--
	if r, e = SubdivideEdges(nil, m, o); r != nil || !errors.Is(e, ErrBudgetExceeded) {
		t.Fatal("minusone bytes accepted")
	}
	for _, kind := range []string{"vertices", "triangles", "passes"} {
		o = subdivisionTestOptions(.1)
		switch kind {
		case "vertices":
			o.MaxVertices = 4
		case "triangles":
			o.MaxTriangles = 4
		case "passes":
			o.MaxPasses = 1
		}
		r, e = SubdivideEdges(nil, m, o)
		var refusal *SubdivisionError
		if r != nil || !errors.Is(e, ErrBudgetExceeded) || !errors.As(e, &refusal) {
			t.Fatal(kind, e)
		}
		if kind == "passes" && refusal.Report.Passes != 1 {
			t.Fatal(refusal.Report)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, e = SubdivideEdges(ctx, m, subdivisionTestOptions(.7)); r != nil || !errors.Is(e, context.Canceled) {
		t.Fatal("cancelled publishes")
	}
	// This first counted case cancels during preflight. The next case
	// proves cancellation after a complete conformant generation pass.
	ctx2 := &subdivisionCountingContext{Context: context.Background(), remaining: 40}
	if r, e = SubdivideEdges(ctx2, subdivisionGrid(16), subdivisionTestOptions(.2)); r != nil || !errors.Is(e, context.Canceled) {
		t.Fatal("in-operation cancellation", e)
	}
	probeOptions := subdivisionTestOptions(.1)
	probeOptions.MaxPasses = 1
	probe := &subdivisionProgressContext{Context: context.Background()}
	probeResult, probeErr := SubdivideEdges(probe, m, probeOptions)
	var probeRefusal *SubdivisionError
	if probeResult != nil || !errors.Is(probeErr, ErrBudgetExceeded) || !errors.As(probeErr, &probeRefusal) || probeRefusal.Report.Passes != 1 || probe.Calls == 0 {
		t.Fatalf("calibration did not finish one conformant pass: %v", probeErr)
	}
	generationContext := &subdivisionProgressContext{Context: context.Background(), CancelAt: probe.Calls}
	generated, generatedErr := SubdivideEdges(generationContext, m, subdivisionTestOptions(.1))
	var generationRefusal *SubdivisionError
	if generated != nil || !errors.Is(generatedErr, context.Canceled) || !errors.As(generatedErr, &generationRefusal) || generationRefusal.Report.Passes != 1 || generationRefusal.Report.Stop != "cancelled" {
		t.Fatalf("post-generation cancellation exposes output or misses completed-pass checkpoint: %v", generatedErr)
	}
}

// No production hook: calibrate a deterministic call-local context count
// using an intentional one-pass budget refusal. Its final context check is
// in the second edge scan, after one complete output generation. Cancelling
// at that same checkpoint with a larger pass budget must expose no mesh.
type subdivisionProgressContext struct {
	context.Context
	Calls, CancelAt int
}

func (c *subdivisionProgressContext) Err() error {
	c.Calls++
	if c.CancelAt > 0 && c.Calls >= c.CancelAt {
		return context.Canceled
	}
	return nil
}

type subdivisionCountingContext struct {
	context.Context
	remaining int
}

func (c *subdivisionCountingContext) Err() error {
	c.remaining--
	if c.remaining < 0 {
		return context.Canceled
	}
	return nil
}
func TestSubdivisionInvalidPrecision(t *testing.T) {
	empty, err := SubdivideEdges(nil, &Mesh{}, subdivisionTestOptions(1))
	if err != nil || len(empty.Mesh.Triangles) != 0 || empty.Report.Passes != 0 {
		t.Fatal("empty", err)
	}
	short := &Mesh{Vertices: [][3]float32{{0, 0, 0}, {.1, 0, 0}, {0, .1, 0}}, Triangles: [][3]uint32{{0, 1, 2}}}
	copyShort, err := SubdivideEdges(nil, short, subdivisionTestOptions(1))
	if err != nil || !reflect.DeepEqual(copyShort.Mesh, short) || copyShort.Report.Passes != 0 {
		t.Fatal("short mesh changed", err)
	}
	nonmanifold := &Mesh{Vertices: [][3]float32{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}, {0, 0, 1}, {0, -1, 0}}, Triangles: [][3]uint32{{0, 1, 2}, {1, 0, 3}, {0, 1, 4}}}
	bowtie := &Mesh{Vertices: [][3]float32{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}, {0, 0, 1}, {0, -1, 0}}, Triangles: [][3]uint32{{0, 1, 2}, {0, 3, 4}}}
	for _, bad := range []*Mesh{nonmanifold, bowtie} {
		if r, e := SubdivideEdges(nil, bad, subdivisionTestOptions(1)); r != nil || !errors.Is(e, ErrInvalidField) {
			t.Fatal("nonmanifold accepted", e)
		}
	}

	for _, change := range []func(*Mesh){func(m *Mesh) { m.Triangles[0][0] = 100 }, func(m *Mesh) { m.Vertices[0][0] = float32(math.NaN()) }, func(m *Mesh) { m.Vertices[2] = m.Vertices[1] }, func(m *Mesh) { m.Triangles[1] = m.Triangles[0] }, func(m *Mesh) { m.Triangles[0][0], m.Triangles[0][1] = m.Triangles[0][1], m.Triangles[0][0] }} {
		m := subdivisionTetra()
		change(m)
		if r, e := SubdivideEdges(nil, m, subdivisionTestOptions(1)); r != nil || !errors.Is(e, ErrInvalidField) {
			t.Fatal("invalid accepted", e)
		}
	}
	m := &Mesh{Vertices: [][3]float32{{1, 0, 0}, {math.Nextafter32(1, 2), 0, 0}, {1, 1, 0}}, Triangles: [][3]uint32{{0, 1, 2}}}
	if r, e := SubdivideEdges(nil, m, subdivisionTestOptions(1e-8)); r != nil || !errors.Is(e, ErrSubdivisionPrecision) {
		t.Fatal("precision stagnation", e)
	}
	for _, v := range []float64{0, -1, math.Inf(1), math.NaN()} {
		if r, e := SubdivideEdges(nil, subdivisionTetra(), subdivisionTestOptions(v)); r != nil || !errors.Is(e, ErrInvalidField) {
			t.Fatal("invalid threshold", e)
		}
	}
}
func subdivisionGrid(n int) *Mesh {
	m := &Mesh{}
	for y := 0; y <= n; y++ {
		for x := 0; x <= n; x++ {
			m.Vertices = append(m.Vertices, [3]float32{float32(x), float32(y), 0})
		}
	}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			a := uint32(y*(n+1) + x)
			b := a + 1
			c := a + uint32(n+1)
			d := c + 1
			m.Triangles = append(m.Triangles, [3]uint32{a, b, d}, [3]uint32{a, d, c})
		}
	}
	return m
}

func TestSubdivisionRepeatAttributeOwnership(t *testing.T) {
	m := subdivisionTetra()
	o := subdivisionTestOptions(.7)
	o.VertexAttributes = []SubdivisionVertexAttribute{{Attribute: VertexScalarAttribute{Name: "tiny", Values: []float64{math.SmallestNonzeroFloat64, math.SmallestNonzeroFloat64, math.SmallestNonzeroFloat64, math.SmallestNonzeroFloat64}}}, {Attribute: VertexScalarAttribute{Name: "region", Values: []float64{9, 9, 9, 9}}, Policy: SubdivisionEqualOnly}}
	o.FaceAttributes = []VertexScalarAttribute{{Name: "phase", Values: []float64{-2, -3, -4, -5}}}
	r, e := SubdivideEdges(nil, m, o)
	if e != nil {
		t.Fatal(e)
	}
	for _, value := range r.VertexAttributes[0].Attribute.Values {
		if value != math.SmallestNonzeroFloat64 {
			t.Fatal("constant subnormal interpolant lost")
		}
	}
	nextOptions := o
	nextOptions.VertexAttributes = r.VertexAttributes
	nextOptions.FaceAttributes = r.FaceAttributes
	again, e := SubdivideEdges(nil, r.Mesh, nextOptions)
	if e != nil || again.Report.Passes != 0 || !reflect.DeepEqual(again.Mesh, r.Mesh) || !reflect.DeepEqual(again.VertexAttributes, r.VertexAttributes) || !reflect.DeepEqual(again.FaceAttributes, r.FaceAttributes) {
		t.Fatal("attribute repeat unstable", e)
	}
	r.Mesh.Vertices[0][0] = 99
	r.VertexAttributes[0].Attribute.Values[0] = 99
	r.FaceAttributes[0].Values[0] = 99
	if m.Vertices[0][0] != 0 || o.VertexAttributes[0].Attribute.Values[0] != math.SmallestNonzeroFloat64 || o.FaceAttributes[0].Values[0] != -2 || again.Mesh.Vertices[0][0] != 0 || again.VertexAttributes[0].Attribute.Values[0] != math.SmallestNonzeroFloat64 || again.FaceAttributes[0].Values[0] != -2 {
		t.Fatal("owned result aliases source/second call")
	}
}
