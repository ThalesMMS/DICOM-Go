package mesh

import (
	"context"
	"math"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// VTK's TestFlyingEdgesExtents and TestOrientedFlyingEdgesPlaneCutter2
// exercise shifted extents and directions. An affine image of a rectangle
// gives independent position, area, border and winding oracles.
func TestVTKReferenceContourPatientExtent(t *testing.T) {
	dims := [3]int{4, 5, 6}
	for _, frame := range []struct {
		name   string
		origin render.Vec3
		steps  [3]render.Vec3
	}{
		{
			name:   "negative-origin",
			origin: render.Vec3{X: -12, Y: -9, Z: -7},
			steps:  [3]render.Vec3{{X: 0.5}, {Y: 2}, {Z: 3}},
		},
		{
			name:   "rotated",
			origin: render.Vec3{X: 12, Y: -9, Z: -7},
			steps:  [3]render.Vec3{{Y: 0.5}, {X: -2}, {Z: 3}},
		},
		{
			name:   "reflected",
			origin: render.Vec3{X: -12, Y: 9, Z: -7},
			steps:  [3]render.Vec3{{Y: -0.5}, {X: -2}, {Z: 3}},
		},
		{
			name:   "sheared",
			origin: render.Vec3{X: -12, Y: -9, Z: 7},
			steps:  [3]render.Vec3{{X: 0.5}, {X: 0.25, Y: 2}, {Y: 0.75, Z: 3}},
		},
	} {
		for axis, axisName := range []string{"x", "y", "z"} {
			t.Run(frame.name+"/"+axisName, func(t *testing.T) {
				const level = 1.25
				a, b, c := frame.steps[0], frame.steps[1], frame.steps[2]
				origin := frame.origin
				field := &FuncField{
					Size: dims,
					Affine: render.GeometryAffine{
						a.X, b.X, c.X, origin.X,
						a.Y, b.Y, c.Y, origin.Y,
						a.Z, b.Z, c.Z, origin.Z,
						0, 0, 0, 1,
					},
					Value: func(x, y, z int) float64 {
						return float64([3]int{x, y, z}[axis]) - level
					},
				}
				got, err := Extract(context.Background(), field, Options{OpenBoundary: true})
				if err != nil {
					t.Fatal(err)
				}
				if err := got.Validate(); err != nil {
					t.Fatal(err)
				}
				j, k := (axis+1)%3, (axis+2)%3
				wantVertices := dims[j] * dims[k]
				wantTriangles := 2 * (dims[j] - 1) * (dims[k] - 1)
				if len(got.Vertices) != wantVertices || len(got.Triangles) != wantTriangles {
					t.Fatalf("mesh has %d vertices and %d triangles, want %d and %d", len(got.Vertices), len(got.Triangles), wantVertices, wantTriangles)
				}
				if len(got.Normals) != wantVertices {
					t.Fatalf("mesh has %d normals, want %d", len(got.Normals), wantVertices)
				}
				var border int
				for edge, uses := range edgeUses(got) {
					switch uses {
					case 1:
						border++
					case 2:
					default:
						t.Fatalf("non-manifold edge %v has %d uses", edge, uses)
					}
				}
				if want := 2 * (dims[j] + dims[k] - 2); border != want {
					t.Fatalf("boundary has %d edges, want %d", border, want)
				}
				planeArea := frame.steps[j].Cross(frame.steps[k])
				wantArea := planeArea.Length() * float64((dims[j]-1)*(dims[k]-1))
				if area, _ := massProperties(got); !relClose(area, wantArea, 1e-6) {
					t.Fatalf("area = %g mm², want %g", area, wantArea)
				}
				wantNormal := planeArea.Normalize()
				if wantNormal.Dot(frame.steps[axis]) > 0 {
					wantNormal = wantNormal.Scale(-1)
				}
				corner := frame.origin.Add(frame.steps[axis].Scale(level))
				for i := range got.Vertices {
					if distance := got.Vertex(i).Sub(corner).Dot(wantNormal); math.Abs(distance) > 1e-6 {
						t.Fatalf("vertex %d is %g mm off the plane", i, distance)
					}
					if n := vertexNormal(got, i); !relClose(n.Sub(wantNormal).Length(), 0, 1e-6) {
						t.Fatalf("vertex %d normal = %v, want %v", i, n, wantNormal)
					}
				}
				for i := range got.Triangles {
					if normal := triangleNormal(got, i); normal.Dot(wantNormal) <= 0 {
						t.Fatalf("triangle %d has normal %v, want outward winding", i, normal)
					}
				}
			})
		}
	}
}

// The VTK SurfaceNets atlas and normals-consistency tests motivate checking
// material pairs at a triple junction. In this atlas every interface is a
// rectangle, so its area and orientation are known without a mesh oracle.
func TestVTKReferenceSurfaceNetsTripleJunction(t *testing.T) {
	const left, lowerRight, upperRight = uint32(17), uint32(70001), uint32(math.MaxUint32)
	for _, frame := range []struct {
		name string
		axes [3]render.Vec3
	}{
		{"translated", IdentityAxes},
		{"swapped-reflected", [3]render.Vec3{{Y: 1}, {X: 1}, {Z: 1}}},
	} {
		for _, selection := range []struct {
			name       string
			requested  []uint32
			labels     []uint32
			interfaces map[[2]uint32]float64
		}{
			{
				name:   "all-labels",
				labels: []uint32{left, lowerRight, upperRight},
				interfaces: map[[2]uint32]float64{
					{0, left}: 106, {0, lowerRight}: 53, {0, upperRight}: 53,
					{left, lowerRight}: 36, {left, upperRight}: 36, {lowerRight, upperRight}: 9,
				},
			},
			{
				name:      "unordered-subset-with-duplicate-and-missing-label",
				requested: []uint32{upperRight, 3, lowerRight, upperRight},
				labels:    []uint32{lowerRight, upperRight},
				interfaces: map[[2]uint32]float64{
					{0, lowerRight}: 89, {0, upperRight}: 89, {lowerRight, upperRight}: 9,
				},
			},
		} {
			t.Run(frame.name+"/"+selection.name, func(t *testing.T) {
				spacing := [3]float64{0.5, 2, 3}
				field := labelField([3]int{4, 4, 3}, spacing, func(x, y, _ int) uint32 {
					if x < 2 {
						return left
					}
					if y < 2 {
						return upperRight
					}
					return lowerRight
				})
				field.Affine = GridAffine(render.Vec3{X: -19, Y: 5, Z: -2}, frame.axes, spacing)
				surface, err := ExtractLabels(context.Background(), field, SurfaceNetsOptions{
					Labels: selection.requested, Iterations: -1,
				})
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(surface.Labels, selection.labels) {
					t.Fatalf("labels = %v, want %v", surface.Labels, selection.labels)
				}
				if err := surface.Mesh.Validate(); err != nil {
					t.Fatal(err)
				}
				if surface.Mesh.Normals != nil || len(surface.Pairs) != len(surface.Mesh.Triangles) {
					t.Fatal("shared boundary geometry has normals or lacks a pair for each triangle")
				}
				gotAreas := make(map[[2]uint32]float64)
				for i, pair := range surface.Pairs {
					normal := triangleNormal(surface.Mesh, i)
					gotAreas[pair] += normal.Length() / 2
					if pair[0] == 0 {
						continue
					}
					wantDirection := frame.axes[0].Scale(-1)
					if pair[0] == lowerRight {
						wantDirection = frame.axes[1]
					}
					if normal.Normalize().Sub(wantDirection).Length() > 1e-6 {
						t.Fatalf("interface %v triangle %d has normal %v, want %v", pair, i, normal, wantDirection)
					}
				}
				if len(gotAreas) != len(selection.interfaces) {
					t.Fatalf("interface areas = %v, want %v", gotAreas, selection.interfaces)
				}
				for pair, want := range selection.interfaces {
					if area := gotAreas[pair]; !relClose(area, want, 1e-6) {
						t.Errorf("interface %v area = %g mm², want %g", pair, area, want)
					}
				}
				for _, label := range selection.labels {
					part := surface.LabelMesh(label)
					requireClosedManifold(t, part)
					wantVolume := 36.0
					if label == left {
						wantVolume = 72
					}
					if volume := signedVolume(part); !relClose(volume, wantVolume, 1e-6) {
						t.Fatalf("label %d signed volume = %g mm³, want %g", label, volume, wantVolume)
					}
					vertex := part.Vertices[0]
					triangle := part.Triangles[0]
					part.Vertices[0] = [3]float32{}
					part.Triangles[0] = [3]uint32{}
					newPart := surface.LabelMesh(label)
					if newPart.Vertices[0] != vertex || newPart.Triangles[0] != triangle {
						t.Fatalf("label %d mesh was altered through a prior extraction", label)
					}
				}
				if !slices.Contains(selection.labels, left) && surface.LabelMesh(left) != nil {
					t.Fatal("excluded label produced a mesh")
				}
			})
		}
	}
}
