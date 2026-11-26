package mesh

import (
	"math"
	"reflect"
	"slices"
	"testing"
)

// TestCleanPolyData2 in VTK distinguishes repeated IDs from cells that
// degenerate only after welding. For triangle-only meshes, also check that
// welding preserves opposing winding while removing cyclic duplicates.
func TestVTKReferenceCleanPhysicalTolerance(t *testing.T) {
	for _, tc := range []struct {
		name      string
		firstX    float32
		candidate [3]float32
		tolerance float64
		merged    bool
	}{
		{"exact-coincidence", -1, [3]float32{-1, 0, 0}, 0, true},
		{"near-but-zero-tolerance", -1, [3]float32{-0.875, 0, 0}, 0, false},
		{"neighbor-cell-across-zero", -0.125, [3]float32{0.125, 0, 0}, 0.25, true},
		{"exact-radius", -1, [3]float32{-0.75, 0, 0}, 0.25, true},
		{"just-above-radius", -1, [3]float32{math.Nextafter32(-0.75, 0), 0, 0}, 0.25, false},
		{"diagonal-outside-radius", -1, [3]float32{-0.75, 0.25, 0}, 0.25, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &Mesh{
				Vertices:  [][3]float32{{tc.firstX, 0, 0}, {1, 0, 0}, {-1, 1, 0}, tc.candidate, {0, 0, 3}},
				Triangles: [][3]uint32{{0, 1, 2}, {1, 2, 3}, {0, 3, 1}, {0, 2, 1}},
			}
			before := plane1497Copy(source)
			got, report := Clean(source, tc.tolerance)
			wantVertices := source.Vertices[:4]
			wantTriangles := source.Triangles
			wantReport := CleanReport{UnusedVertices: 1}
			if tc.merged {
				wantVertices = source.Vertices[:3]
				wantTriangles = [][3]uint32{{0, 1, 2}, {0, 2, 1}}
				wantReport = CleanReport{MergedVertices: 1, DegenerateTriangles: 1, DuplicateTriangles: 1, UnusedVertices: 1}
			}
			if !slices.Equal(got.Vertices, wantVertices) || !slices.Equal(got.Triangles, wantTriangles) || report != wantReport {
				t.Fatalf("vertices = %v, triangles = %v, report = %+v; want %v, %v, %+v", got.Vertices, got.Triangles, report, wantVertices, wantTriangles, wantReport)
			}
			got.Vertices[0] = [3]float32{99}
			got.Triangles[0] = [3]uint32{}
			if !reflect.DeepEqual(source, before) {
				t.Fatal("cleaned mesh aliases source storage")
			}
		})
	}
}

// Like the VTK point-connectivity tests, the model has separated regions.
// A small chain touches only at vertex IDs and wins by triangle count. A
// coincident point with a different ID joins the chain only after welding.
func TestVTKReferencePointConnectivityAfterWelding(t *testing.T) {
	for _, tc := range []struct {
		name       string
		weld       bool
		components int
		ranks      []int
		faces      int
		area       float64
	}{
		{"indexed-points", false, 3, []int{1, 0, 2, 1, 0, 0}, 3, 1.5},
		{"welded-points", true, 2, []int{1, 0, 0, 1, 0, 0}, 4, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &Mesh{
				Vertices: [][3]float32{
					{0, 0, 0}, {1, 0, 0}, {1, 1, 0}, {2, 1, 0}, {2, 2, 0}, {3, 2, 0}, {3, 3, 0},
					{100, 100, 0}, {200, 100, 0}, {200, 200, 0}, {100, 200, 0},
					{2, 2, 0}, {1, 2, 0}, {1, 3, 0},
				},
				Triangles: [][3]uint32{{7, 8, 9}, {4, 5, 6}, {11, 12, 13}, {7, 9, 10}, {0, 1, 2}, {2, 3, 4}},
			}
			if tc.weld {
				source, _ = Clean(source, 0)
			}
			ranks, count := ConnectedComponents(source)
			if count != tc.components || !slices.Equal(ranks, tc.ranks) {
				t.Fatalf("found %d components with ranks %v, want %d and %v", count, ranks, tc.components, tc.ranks)
			}
			largest := LargestComponent(source)
			if err := largest.Validate(); err != nil {
				t.Fatal(err)
			}
			if area, _ := massProperties(largest); len(largest.Triangles) != tc.faces || area != tc.area {
				t.Fatalf("largest component has %d faces and %g mm² area, want %d and %g", len(largest.Triangles), area, tc.faces, tc.area)
			}
		})
	}
}

// The VTK TestFillHolesFilter applies a radius limit to a sheet with missing
// cells. Disjoint boxes with open tops distinguish the radius thresholds
// and have known cap areas and outward cap winding.
func TestVTKReferenceFillHolesPhysicalRadius(t *testing.T) {
	smallRadius, largeRadius := math.Sqrt(2), math.Sqrt(18)
	for _, tc := range []struct {
		name   string
		radius float64
		holes  int
		area   float64
	}{
		{"zero", 0, 0, 0},
		{"below-small", math.Nextafter(smallRadius, 0), 0, 0},
		{"at-small", smallRadius, 1, 4},
		{"below-large", math.Nextafter(largeRadius, 0), 1, 4},
		{"at-large", largeRadius, 2, 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			small := openTopBox([3]float32{-1, -1, -2}, [3]float32{1, 1, 0})
			large := openTopBox([3]float32{7, -3, -3}, [3]float32{13, 3, 0})
			source := plane1497Merge(small, large)
			before := plane1497Copy(source)
			out, holes := FillHoles(source, tc.radius)
			if holes != tc.holes || len(out.Triangles) != len(source.Triangles)+4*tc.holes {
				t.Fatalf("filled %d holes, added %d cap faces; want %d holes and %d faces", holes, len(out.Triangles)-len(source.Triangles), tc.holes, tc.holes*4)
			}
			var capArea float64
			for i := len(source.Triangles); i < len(out.Triangles); i++ {
				face := out.Triangles[i]
				normal := triangleNormal(out, i)
				if normal.Z <= 0 || normal.X != 0 || normal.Y != 0 {
					t.Fatalf("cap face %v has normal %v, want +z", face, normal)
				}
				for _, vertex := range face {
					if out.Vertices[vertex][2] != 0 {
						t.Fatalf("cap face %v leaves the z=0 plane", face)
					}
				}
				capArea += normal.Z / 2
			}
			if capArea != tc.area {
				t.Fatalf("cap area = %g mm², want %g", capArea, tc.area)
			}
			boundary := 0
			for edge, uses := range edgeUses(out) {
				switch uses {
				case 1:
					boundary++
				case 2:
				default:
					t.Fatalf("edge %v has %d uses", edge, uses)
				}
			}
			if want := 4 * (2 - tc.holes); boundary != want {
				t.Fatalf("mesh has %d open edges, want %d", boundary, want)
			}
			if tc.holes == 2 {
				requireClosedManifold(t, out)
			}
			out.Vertices[0] = [3]float32{99}
			out.Triangles[0] = [3]uint32{}
			if !reflect.DeepEqual(source, before) {
				t.Fatal("filled mesh aliases source storage")
			}
		})
	}
}

func TestVTKReferenceFillHolesLeavesBranchingBoundaryOpen(t *testing.T) {
	ambiguous := &Mesh{
		Vertices:  [][3]float32{{0, 0, 0}, {1, 0, 0}, {1, 1, 0}, {-1, 0, 0}, {-1, -1, 0}},
		Triangles: [][3]uint32{{0, 1, 2}, {0, 3, 4}},
	}
	box := openTopBox([3]float32{5, -1, -1}, [3]float32{7, 1, 0})
	source := plane1497Merge(ambiguous, box)
	out, holes := FillHoles(source, 20)
	if holes != 1 || len(out.Triangles) != len(source.Triangles)+4 {
		t.Fatalf("filled %d holes, added %d faces; want one square cap", holes, len(out.Triangles)-len(source.Triangles))
	}
	for _, face := range out.Triangles[len(source.Triangles):] {
		for _, vertex := range face {
			if int(vertex) < len(ambiguous.Vertices) {
				t.Fatalf("added face %v to branching boundary", face)
			}
		}
	}
}

func openTopBox(lo, hi [3]float32) *Mesh {
	box := plane1497Box(lo, hi)
	box.Triangles = slices.DeleteFunc(box.Triangles, func(face [3]uint32) bool {
		return box.Vertices[face[0]][2] == hi[2] && box.Vertices[face[1]][2] == hi[2] && box.Vertices[face[2]][2] == hi[2]
	})
	return box
}
