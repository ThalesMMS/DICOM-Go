package mesh

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
)

func booleanBox(lo, hi [3]float32) *Mesh { return plane1497Box(lo, hi) }
func booleanInputCopy(in BooleanInput) BooleanInput {
	out := in
	out.Mesh = plane1497Copy(in.Mesh)
	out.FaceColors = append([][3]uint8(nil), in.FaceColors...)
	out.VertexScalars = append([]VertexScalarAttribute(nil), in.VertexScalars...)
	for i := range out.VertexScalars {
		out.VertexScalars[i].Values = append([]float64(nil), in.VertexScalars[i].Values...)
	}
	return out
}
func assertBooleanSolid(t *testing.T, r *BooleanResult, volume, area, tol float64) {
	t.Helper()
	if r == nil || r.Mesh == nil {
		t.Fatal("missing result")
	}
	if err := r.Mesh.Validate(); err != nil {
		t.Fatal(err)
	}
	if !r.Mesh.CheckClosure().Closed() {
		t.Fatal("not closed", r.Mesh.CheckClosure())
	}
	if math.Abs(r.Mesh.Volume()-volume) > tol || math.Abs(r.Mesh.Area()-area) > tol {
		t.Fatalf("volume %g want %g; area %g want %g", r.Mesh.Volume(), volume, r.Mesh.Area(), area)
	}
	if len(r.Faces) != len(r.Mesh.Triangles) || len(r.CornerNormals) != len(r.Faces) || len(r.FaceColors) != len(r.Faces) {
		t.Fatal("attribute/provenance counts")
	}
	if r.Work <= 0 || r.ScratchBytes <= 0 || r.OutputBytes <= 0 {
		t.Fatal("missing quota accounting")
	}
}

func TestBooleanAnalyticBoxesAndMergeSemantics(t *testing.T) {
	a := BooleanInput{Mesh: booleanBox([3]float32{0, 0, 0}, [3]float32{2, 2, 2}), Reference: "A"}
	b := BooleanInput{Mesh: booleanBox([3]float32{1, .5, .25}, [3]float32{3, 2.5, 2.25}), Reference: "B"}
	// Exact analytic overlap: 1 x 1.5 x 1.75. Each box has area24.
	// Removed surface patch per box = 1.5*1.75 + 1*1.75 + 1*1.5.
	intersection := 1.0 * 1.5 * 1.75
	patch := 1.5*1.75 + 1*1.75 + 1*1.5
	for _, tc := range []struct {
		op           BooleanOperation
		volume, area float64
	}{{BooleanUnion, 16 - intersection, 48 - 2*patch}, {BooleanIntersection, intersection, 2 * patch}, {BooleanDifference, 8 - intersection, 24}} {
		t.Run(string(tc.op), func(t *testing.T) {
			ac, bc := booleanInputCopy(a), booleanInputCopy(b)
			r, err := Boolean(context.Background(), a, b, tc.op, BooleanOptions{})
			if err != nil {
				t.Fatal(err)
			}
			assertBooleanSolid(t, r, tc.volume, tc.area, 2e-5)
			if r.Operation != tc.op || r.References != [2]string{"A", "B"} {
				t.Fatal("provenance", r)
			}
			if !reflect.DeepEqual(a, ac) || !reflect.DeepEqual(b, bc) {
				t.Fatal("source mutated")
			}
		})
	}
	merged, _ := Merge([]*Mesh{a.Mesh, b.Mesh}, nil)
	if len(merged.Triangles) != 24 || math.Abs(merged.Volume()-16) > 1e-12 {
		t.Fatal("Merge changed concatenation contract")
	}
}

func TestBooleanDisjointContainmentCavitiesAndComponents(t *testing.T) {
	outer := booleanBox([3]float32{-2, -2, -2}, [3]float32{2, 2, 2})
	inner := booleanBox([3]float32{-.5, -.5, -.5}, [3]float32{.5, .5, .5})
	far := booleanBox([3]float32{5, 5, 5}, [3]float32{6, 6, 6})
	for _, tc := range []struct {
		name       string
		a, b       *Mesh
		op         BooleanOperation
		v, area    float64
		components int
	}{
		{"contained-union", outer, inner, BooleanUnion, 64, 96, 1},
		{"contained-intersection", outer, inner, BooleanIntersection, 1, 6, 1},
		{"cavity", outer, inner, BooleanDifference, 63, 102, 2},
		{"disjoint-union", outer, far, BooleanUnion, 65, 102, 2},
		{"disjoint-intersection", outer, far, BooleanIntersection, 0, 0, 0},
		{"disjoint-difference", outer, far, BooleanDifference, 64, 96, 1},
		{"multiple-components", plane1497Merge(outer, far), inner, BooleanDifference, 64, 108, 3},
		{"empty-identity", &Mesh{}, far, BooleanUnion, 1, 6, 1},
		{"empty-difference", &Mesh{}, far, BooleanDifference, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Boolean(context.Background(), BooleanInput{Mesh: tc.a}, BooleanInput{Mesh: tc.b}, tc.op, BooleanOptions{})
			if err != nil {
				t.Fatal(err)
			}
			assertBooleanSolid(t, r, tc.v, tc.area, 2e-5)
			_, count := ConnectedComponents(r.Mesh)
			if count != tc.components {
				t.Fatal("components", count, tc.components)
			}
			if tc.name == "cavity" {
				// A cavity is an inward shell, not repaired into a positive solid.
				r2, err := Boolean(context.Background(), BooleanInput{Mesh: r.Mesh}, BooleanInput{Mesh: far}, BooleanUnion, BooleanOptions{})
				if err != nil {
					t.Fatal("cavity cannot be consumed as a solid", err)
				}
				assertBooleanSolid(t, r2, 64, 108, 2e-5)
			}
		})
	}
}

func TestBooleanRefusesAmbiguousAndInvalidInputsAtomically(t *testing.T) {
	a := booleanBox([3]float32{0, 0, 0}, [3]float32{2, 2, 2})
	invalid := plane1497Copy(a)
	invalid.Triangles = invalid.Triangles[:11]
	inward := plane1497Copy(a)
	plane1497Reverse(inward)
	nonfinite := plane1497Copy(a)
	nonfinite.Vertices[0][0] = float32(math.NaN())
	for _, tc := range []struct {
		name string
		b    *Mesh
	}{
		{"coincidence", a}, {"coplanar-overlap", booleanBox([3]float32{1, 0, 0}, [3]float32{3, 2, 2})},
		{"face-tangent", booleanBox([3]float32{2, 0, 0}, [3]float32{4, 2, 2})},
		{"point-tangent", booleanBox([3]float32{2, 2, 2}, [3]float32{4, 4, 4})},
		{"open", invalid}, {"inward", inward}, {"nonfinite", nonfinite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Boolean(context.Background(), BooleanInput{Mesh: a}, BooleanInput{Mesh: tc.b}, BooleanUnion, BooleanOptions{})
			if err == nil || r != nil {
				t.Fatal("invalid solid published", err, r)
			}
		})
	}
}

func TestBooleanQuotasAndCancellationAreAtomic(t *testing.T) {
	a := BooleanInput{Mesh: booleanBox([3]float32{0, 0, 0}, [3]float32{2, 2, 2})}
	b := BooleanInput{Mesh: booleanBox([3]float32{1, .5, .25}, [3]float32{3, 2.5, 2.25})}
	for _, o := range []BooleanOptions{{MaxWork: 1}, {MaxCandidates: 1}, {MaxScratchBytes: 64}, {MaxOutputBytes: 100}, {MaxPolygons: 1}, {MaxPlanesPerFace: 1}, {MaxPolygonVertices: 3}, {MaxResultVertices: 2}, {MaxResultTriangles: 2}, {MaxVertices: 2}, {MaxTriangles: 2}, {MaxWorkingVertices: 2}} {
		r, err := Boolean(context.Background(), a, b, BooleanUnion, o)
		if !errors.Is(err, ErrBudgetExceeded) || r != nil {
			t.Fatalf("quota %+v: %v result%v", o, err, r)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, err := Boolean(ctx, a, b, BooleanUnion, BooleanOptions{}); !errors.Is(err, context.Canceled) || r != nil {
		t.Fatal("canceled operation published", r, err)
	}
}
