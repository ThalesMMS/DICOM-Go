package mesh

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
)

func TestBooleanNonconvexAndRigidCoordinates(t *testing.T) {
	a := BooleanInput{Mesh: plane1497Concave()}
	b := BooleanInput{Mesh: booleanBox([3]float32{.5, .5, -.5}, [3]float32{2, 2, .5})}
	for _, tc := range []struct {
		op      BooleanOperation
		v, area float64
	}{{BooleanUnion, 11, 36}, {BooleanIntersection, 1.25, 8.5}, {BooleanDifference, 8.75, 38.5}} {
		r, err := Boolean(context.Background(), a, b, tc.op, BooleanOptions{})
		if err != nil {
			t.Fatal(tc.op, err)
		}
		assertBooleanSolid(t, r, tc.v, tc.area, 1e-5)
	}
	// Rigid transformations must not turn world coordinates into grid/voxel
	// membership. The oracle remains the analytic overlap in the first test.
	for _, angle := range []float64{.31, .73} {
		ma := booleanBox([3]float32{0, 0, 0}, [3]float32{2, 2, 2})
		mb := booleanBox([3]float32{1, .5, .25}, [3]float32{3, 2.5, 2.25})
		for _, m := range []*Mesh{ma, mb} {
			for i, v := range m.Vertices {
				m.Vertices[i] = [3]float32{float32(float64(v[0])*math.Cos(angle) - float64(v[1])*math.Sin(angle) + 7.25), float32(float64(v[0])*math.Sin(angle) + float64(v[1])*math.Cos(angle) - 3.5), v[2] + .125}
			}
		}
		r, err := Boolean(context.Background(), BooleanInput{Mesh: ma}, BooleanInput{Mesh: mb}, BooleanIntersection, BooleanOptions{ToleranceMM: 2e-6})
		if err != nil {
			t.Fatal("rotated", angle, err)
		}
		assertBooleanSolid(t, r, 2.625, 11.75, 4e-5)
	}
}

func TestBooleanAnalyticSpheres(t *testing.T) {
	a, b := plane1497Sphere(), plane1497Sphere()
	shift := [3]float32{.631, .217, .119}
	for i, p := range b.Vertices {
		for axis := range p {
			b.Vertices[i][axis] = p[axis] + shift[axis]
		}
	}
	d := math.Sqrt(float64(shift[0])*float64(shift[0]) + float64(shift[1])*float64(shift[1]) + float64(shift[2])*float64(shift[2]))
	v := 4 * math.Pi / 3
	intersection := math.Pi * (4 + d) * (2 - d) * (2 - d) / 12
	for _, tc := range []struct {
		op           BooleanOperation
		volume, area float64
	}{{BooleanUnion, 2*v - intersection, 4*math.Pi + 2*math.Pi*d}, {BooleanIntersection, intersection, 4*math.Pi - 2*math.Pi*d}, {BooleanDifference, v - intersection, 4 * math.Pi}} {
		t.Run(string(tc.op), func(t *testing.T) {
			r, err := Boolean(context.Background(), BooleanInput{Mesh: a}, BooleanInput{Mesh: b}, tc.op, BooleanOptions{})
			if err != nil {
				t.Fatal(err)
			}
			// The input tessellation, not the operation, approximates a sphere:
			// 12 polar bands/24 azimuth segments; fixed analytic bounds precede execution.
			if math.Abs(r.Mesh.Volume()-tc.volume) > .05*tc.volume || math.Abs(r.Mesh.Area()-tc.area) > .03*tc.area {
				t.Fatalf("sphere: volume %g/%g area %g/%g", r.Mesh.Volume(), tc.volume, r.Mesh.Area(), tc.area)
			}
			if !r.Mesh.CheckClosure().Closed() {
				t.Fatal("open sphere Boolean")
			}
			t.Logf("faces=%d volume=%g analytic=%g area=%g analytic=%g work=%d candidates=%d scratch=%d output=%d", len(r.Faces), r.Mesh.Volume(), tc.volume, r.Mesh.Area(), tc.area, r.Work, r.Candidates, r.ScratchBytes, r.OutputBytes)
		})
	}
}

func booleanAttributedInput(m *Mesh, input int) BooleanInput {
	in := BooleanInput{Mesh: m, Reference: []string{"source-A", "source-B"}[input], FaceColors: make([][3]uint8, len(m.Triangles)), VertexScalars: []VertexScalarAttribute{{Name: "field", Units: "mm", Values: make([]float64, len(m.Vertices))}, {Name: "discontinuous", Values: make([]float64, len(m.Vertices))}}}
	m.Normals = make([][3]float32, len(m.Vertices))
	var center [3]float64
	for _, p := range m.Vertices {
		for axis := range p {
			center[axis] += float64(p[axis]) / float64(len(m.Vertices))
		}
	}
	for i, p := range m.Vertices {
		in.VertexScalars[0].Values[i] = float64(p[0]) + 2*float64(p[1]) - 3*float64(p[2])
		in.VertexScalars[1].Values[i] = float64(10 + input*20)
		n := sub3(m.point(uint32(i)), center)
		n = scale3(n, 1/math.Sqrt(dot3(n, n)))
		m.Normals[i] = [3]float32{float32(n[0]), float32(n[1]), float32(n[2])}
	}
	for i := range in.FaceColors {
		in.FaceColors[i] = [3]uint8{uint8(input + 1), uint8(i), 99}
	}
	return in
}
func TestBooleanCornerAttributesAndOwnedProvenance(t *testing.T) {
	a := booleanAttributedInput(booleanBox([3]float32{0, 0, 0}, [3]float32{2, 2, 2}), 0)
	b := booleanAttributedInput(booleanBox([3]float32{1, .5, .25}, [3]float32{3, 2.5, 2.25}), 1)
	before := [2]BooleanInput{booleanInputCopy(a), booleanInputCopy(b)}
	r, err := Boolean(context.Background(), a, b, BooleanDifference, BooleanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, before[0]) || !reflect.DeepEqual(b, before[1]) {
		t.Fatal("Boolean changed sources")
	}
	seen := [2]bool{}
	for face, src := range r.Faces {
		seen[src.Input] = true
		in := []BooleanInput{a, b}[src.Input]
		original := in.Mesh.Triangles[src.Triangle]
		if !r.SourceNormals[face] || !r.ColorsPresent[face] || r.FaceColors[face] != in.FaceColors[src.Triangle] || src.Reversed != (src.Input == 1) {
			t.Fatal("source face metadata", face, src)
		}
		for corner, bary := range src.Barycentric {
			if math.Abs(bary[0]+bary[1]+bary[2]-1) > 1e-12 {
				t.Fatal("barycentric sum")
			}
			var point, normal [3]float64
			for j, vertex := range original {
				for axis := 0; axis < 3; axis++ {
					point[axis] += bary[j] * float64(in.Mesh.Vertices[vertex][axis])
					normal[axis] += bary[j] * float64(in.Mesh.Normals[vertex][axis])
				}
			}
			normal = scale3(normal, 1/math.Sqrt(dot3(normal, normal)))
			if src.Reversed {
				normal = scale3(normal, -1)
			}
			outPoint := r.Mesh.point(r.Mesh.Triangles[face][corner])
			delta := sub3(outPoint, point)
			if math.Sqrt(dot3(delta, delta)) > r.ToleranceMM {
				t.Fatal("provenance reconstruction")
			}
			for axis := 0; axis < 3; axis++ {
				if math.Abs(float64(r.CornerNormals[face][corner][axis])-normal[axis]) > 1e-6 {
					t.Fatal("normal interpolation/flip")
				}
			}
			if math.Abs(r.CornerScalars[0].Values[face][corner]-(point[0]+2*point[1]-3*point[2])) > 1e-10 || r.CornerScalars[1].Values[face][corner] != float64(10+int(src.Input)*20) {
				t.Fatal("corner scalar blended across source seam")
			}
		}
	}
	if !seen[0] || !seen[1] || r.Mesh.Normals != nil {
		t.Fatal("missing seam source or silently averaged vertex normals")
	}
	color, scalar := r.FaceColors[0], r.CornerScalars[0].Values[0]
	for _, in := range []BooleanInput{a, b} {
		for i := range in.FaceColors {
			in.FaceColors[i] = [3]uint8{}
		}
		for i := range in.VertexScalars {
			for j := range in.VertexScalars[i].Values {
				in.VertexScalars[i].Values[j] = 12345
			}
		}
	}
	if r.FaceColors[0] != color || r.CornerScalars[0].Values[0] != scalar {
		t.Fatal("result borrowed mutable attributes")
	}
}

type booleanCancelContext struct {
	context.Context
	calls, at int
	done      chan struct{}
	once      sync.Once
}

func (c *booleanCancelContext) Done() <-chan struct{} { return c.done }
func (c *booleanCancelContext) Err() error {
	c.calls++
	if c.at > 0 && c.calls >= c.at {
		c.once.Do(func() { close(c.done) })
		return context.Canceled
	}
	return nil
}
func TestBooleanCancellationAcrossPhasesAndAttributeRefusal(t *testing.T) {
	a := BooleanInput{Mesh: booleanBox([3]float32{0, 0, 0}, [3]float32{2, 2, 2})}
	b := BooleanInput{Mesh: booleanBox([3]float32{1, .5, .25}, [3]float32{3, 2.5, 2.25})}
	probe := &booleanCancelContext{Context: context.Background(), done: make(chan struct{})}
	if _, err := Boolean(probe, a, b, BooleanUnion, BooleanOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, at := range []int{30, probe.calls / 3, probe.calls * 2 / 3, probe.calls - 1} {
		ctx := &booleanCancelContext{Context: context.Background(), at: at, done: make(chan struct{})}
		if r, err := Boolean(ctx, a, b, BooleanUnion, BooleanOptions{}); !errors.Is(err, context.Canceled) || r != nil {
			t.Fatal("partial publication on cancellation", at, r, err)
		}
	}
	a.VertexScalars = []VertexScalarAttribute{{Name: "onlyA", Values: make([]float64, len(a.Mesh.Vertices))}}
	if r, err := Boolean(context.Background(), a, b, BooleanUnion, BooleanOptions{}); !errors.Is(err, ErrBooleanGeometry) || r != nil {
		t.Fatal("unsupported schema silently lost", r, err)
	}
}
