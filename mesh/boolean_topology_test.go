package mesh

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
)

func TestBooleanFaceHoleAndSmallCavityPreservation(t *testing.T) {
	a := BooleanInput{Mesh: booleanBox([3]float32{0, 0, 0}, [3]float32{10, 10, 10})}
	b := BooleanInput{Mesh: booleanBox([3]float32{6, 2, -1}, [3]float32{7, 3, .5})}
	for _, tc := range []struct {
		op      BooleanOperation
		v, area float64
	}{{BooleanUnion, 1001, 604}, {BooleanIntersection, .5, 4}, {BooleanDifference, 999.5, 602}} {
		r, err := Boolean(context.Background(), a, b, tc.op, BooleanOptions{})
		if err != nil {
			t.Fatal(tc.op, err)
		}
		assertBooleanSolid(t, r, tc.v, tc.area, 1e-5)
	}
	outer := BooleanInput{Mesh: booleanBox([3]float32{-1, -1, -1}, [3]float32{1, 1, 1})}
	inner := BooleanInput{Mesh: booleanBox([3]float32{.2, .2, .2}, [3]float32{.2001, .2001, .2001})}
	r, err := Boolean(context.Background(), outer, inner, BooleanDifference, BooleanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	labels, count := ConnectedComponents(r.Mesh)
	if count != 2 {
		t.Fatal("small cavity was removed", count)
	}
	var signed [2]float64
	for i, tri := range r.Mesh.Triangles {
		a, b, c := r.Mesh.point(tri[0]), r.Mesh.point(tri[1]), r.Mesh.point(tri[2])
		signed[labels[i]] += dot3(a, cross3(b, c)) / 6
	}
	if !(signed[0] > 0 && signed[1] < 0) || math.Abs(signed[1]+inner.Mesh.Volume()) > 1e-17 {
		t.Fatal("cavity orientation or volume changed", signed, inner.Mesh.Volume())
	}
	// A coarser physical ambiguity bound must decline, never fill the cavity.
	if out, err := Boolean(context.Background(), outer, inner, BooleanDifference, BooleanOptions{ToleranceMM: .001}); err == nil || out != nil {
		t.Fatal("coarse tolerance silently erased a cavity", out, err)
	}
}

func TestBooleanRejectsBowTieAndUnsupportedAttributes(t *testing.T) {
	first := booleanBox([3]float32{0, 0, 0}, [3]float32{1, 1, 1})
	second := booleanBox([3]float32{1, 1, 1}, [3]float32{2, 2, 2})
	joined := plane1497Merge(first, second)
	// Join the coincident corners topologically: every edge is still closed,
	// but the shared vertex has two circular face fans.
	for i, tri := range joined.Triangles {
		for j, id := range tri {
			if id == 8 {
				joined.Triangles[i][j] = 6
			}
		}
	}
	if !joined.CheckClosure().Closed() {
		t.Fatal("fixture must have closed edges")
	}
	far := BooleanInput{Mesh: booleanBox([3]float32{4, 4, 4}, [3]float32{5, 5, 5})}
	if r, err := Boolean(context.Background(), BooleanInput{Mesh: joined}, far, BooleanUnion, BooleanOptions{}); !errors.Is(err, ErrBooleanGeometry) || r != nil {
		t.Fatal("bow tie accepted", r, err)
	}
	for _, mutation := range []func(*BooleanInput){
		func(a *BooleanInput) { a.FaceColors = make([][3]uint8, 1) },
		func(a *BooleanInput) { a.Mesh.Normals = make([][3]float32, 1) },
		func(a *BooleanInput) {
			a.Mesh.Normals = make([][3]float32, len(a.Mesh.Vertices))
			a.Mesh.Normals[0][0] = float32(math.Inf(1))
		},
		func(a *BooleanInput) {
			a.VertexScalars = []VertexScalarAttribute{{Name: "bad", Values: []float64{math.NaN()}}}
		},
	} {
		in := BooleanInput{Mesh: plane1497Copy(first)}
		mutation(&in)
		if r, err := Boolean(context.Background(), in, far, BooleanUnion, BooleanOptions{}); err == nil || r != nil {
			t.Fatal("invalid attribute published", r, err)
		}
	}
}

func TestBooleanPrecisionAndOptionRefusals(t *testing.T) {
	a := BooleanInput{Mesh: booleanBox([3]float32{10000, 0, 0}, [3]float32{10002, 2, 2})}
	b := BooleanInput{Mesh: booleanBox([3]float32{0, 0, .25}, [3]float32{2, 2, 2.25})}
	for i, p := range b.Mesh.Vertices {
		b.Mesh.Vertices[i][0] = float32(10001 + float64(p[0])*math.Cos(.31) - float64(p[1])*math.Sin(.31))
		b.Mesh.Vertices[i][1] = float32(.5 + float64(p[0])*math.Sin(.31) + float64(p[1])*math.Cos(.31))
	}
	// New intersection positions cannot be represented at micrometre tolerance
	// this far from the origin. The API must refuse rather than round silently.
	if r, err := Boolean(context.Background(), a, b, BooleanIntersection, BooleanOptions{}); !errors.Is(err, ErrBooleanGeometry) || r != nil {
		t.Fatal("unrepresentable intersection published", r, err)
	}
	a.Mesh = booleanBox([3]float32{0, 0, 0}, [3]float32{2, 2, 2})
	b.Mesh = booleanBox([3]float32{1, .5, .25}, [3]float32{3, 2.5, 2.25})
	for _, o := range []BooleanOptions{{ToleranceMM: math.NaN()}, {ToleranceMM: math.Inf(1)}, {ToleranceMM: -1}, {MaxWork: -1}, {MaxWorkingVertices: -1}, {MaxPlanesPerFace: 4097}, {MaxResultVertices: math.MaxUint32 + 1}} {
		if r, err := Boolean(context.Background(), a, b, BooleanUnion, o); !errors.Is(err, ErrBooleanGeometry) || r != nil {
			t.Fatal("invalid option published", o, r, err)
		}
	}
	if r, err := Boolean(context.Background(), a, b, "unknown", BooleanOptions{}); !errors.Is(err, ErrBooleanGeometry) || r != nil {
		t.Fatal("invalid operation published", r, err)
	}
	intersecting := plane1497Merge(a.Mesh, b.Mesh)
	if r, err := Boolean(context.Background(), BooleanInput{Mesh: intersecting}, BooleanInput{Mesh: &Mesh{}}, BooleanUnion, BooleanOptions{}); !errors.Is(err, ErrBooleanGeometry) || r != nil {
		t.Fatal("self-intersecting input published", r, err)
	}
}

func TestBooleanDeterministicConcurrentBorrowing(t *testing.T) {
	a := BooleanInput{Mesh: booleanBox([3]float32{0, 0, 0}, [3]float32{2, 2, 2})}
	b := BooleanInput{Mesh: booleanBox([3]float32{1, .5, .25}, [3]float32{3, 2.5, 2.25})}
	want, err := Boolean(context.Background(), a, b, BooleanUnion, BooleanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := Boolean(context.Background(), a, b, BooleanUnion, BooleanOptions{})
			if err != nil {
				t.Error(err)
				return
			}
			if !reflect.DeepEqual(got, want) {
				t.Error("request ordering changed output/provenance")
			}
		}()
	}
	wg.Wait()
}
