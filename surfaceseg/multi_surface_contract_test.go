package surfaceseg

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/mesh"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/seg"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// The segment references both surfaces in reverse order. Their geometry and
// encoding differ so selecting by item position, or restarting the read
// budget for each surface, cannot produce a passing result.
func TestReferenceSurfaceSegmentationAggregatesSiblingGeometry(t *testing.T) {
	doc := primitiveDocument(t, "mixed", false)
	first := &doc.Surfaces[0]
	first.Mesh.Normals = [][3]float32{{0, 0, 1}, {0, 0, 1}, {0, 0, 1}, {0, 0, 1}}
	first.Mesh.Triangles = [][3]uint32{{0, 1, 2}}
	first.Primitives = Primitives{Vertices: []uint32{3}, Edges: []Edge{{0, 2}}, Lines: []Polyline{{2, 0, 1, 3}}}
	first.VectorAccuracy = []float32{0.125}
	second := *first
	second.Number = 2
	second.Mesh = &mesh.Mesh{
		Vertices:  [][3]float32{{6, 10, 20}, {6, 10, 25}, {6, 16, 25}, {6, 16, 20}},
		Normals:   [][3]float32{{-1, 0, 0}, {-1, 0, 0}, {-1, 0, 0}, {-1, 0, 0}},
		Triangles: [][3]uint32{{0, 1, 3}},
	}
	second.Primitives = Primitives{Vertices: []uint32{2}, Edges: []Edge{{1, 3}}, Lines: []Polyline{{3, 0, 2, 1}}}
	second.VectorAccuracy = []float32{0.25, 0.5, 0.75}
	second.RetiredIndexLists = true
	doc.Surfaces = append(doc.Surfaces, second)
	ref := doc.Segments[0].Surfaces[0]
	ref.Number = 1
	ref.SourceInstances = []seg.ReferencedImage{{SOPClassUID: ctImage, SOPInstanceUID: "2.25.66.1", Frames: []int{2, 4}}}
	otherRef := ref
	otherRef.Number = 2
	otherRef.SourceInstances = []seg.ReferencedImage{{SOPClassUID: ctImage, SOPInstanceUID: "2.25.66.2", Frames: []int{1, 3}}}
	doc.Segments[0].Surfaces = []SurfaceReference{otherRef, ref}
	file, err := Write(doc)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	// Each surface has four points and four normals (three float32
	// components apiece), one each of vertex, edge, triangle and line,
	// and ten indices. Count wire data plus its decoded representation.
	const xyzBytes = 2 * (4 + 4) * 3 * 4
	const extraPerSurface = 2*4 + 3*4 + 24 // fixed edge, triangle and line storage
	const totalBytes = 2*(xyzBytes+extraPerSurface) + 10*(4+4) + 10*(2+4) + 2*(1+3)*4
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian} {
		t.Run(syntax.Name, func(t *testing.T) {
			var encoded bytes.Buffer
			if err := object.WriteDataSet(&encoded, file.Dataset, syntax); err != nil {
				t.Fatal(err)
			}
			dataset, err := object.ReadDataSet(&encoded, syntax)
			if err != nil {
				t.Fatal(err)
			}
			limits := ReadLimits{MaxPoints: 8, MaxTriangles: 2, MaxPrimitives: 8, MaxIndices: 20, MaxBytes: totalBytes}
			for _, tc := range []struct {
				name   string
				reduce func(*ReadLimits)
			}{
				{"points", func(l *ReadLimits) { l.MaxPoints-- }},
				{"triangles", func(l *ReadLimits) { l.MaxTriangles-- }},
				{"primitives", func(l *ReadLimits) { l.MaxPrimitives-- }},
				{"indices", func(l *ReadLimits) { l.MaxIndices-- }},
				{"bytes", func(l *ReadLimits) { l.MaxBytes-- }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					below := limits
					tc.reduce(&below)
					got, err := ReadWithLimits(dataset, below)
					if got != nil || !errors.Is(err, ErrResourceLimitExceeded) {
						t.Fatalf("ReadWithLimits(%+v): document=%t, error=%v", below, got != nil, err)
					}
				})
			}
			atLimit, err := ReadWithLimits(dataset, limits)
			if err != nil {
				t.Fatalf("ReadWithLimits(%+v): %v", limits, err)
			}
			refs := atLimit.Segments[0].Surfaces
			if len(refs) != 2 || refs[0].Number != 2 || refs[1].Number != 1 {
				t.Fatalf("surface references: %+v", refs)
			}
			for i, want := range []seg.ReferencedImage{otherRef.SourceInstances[0], ref.SourceInstances[0]} {
				sources := refs[i].SourceInstances
				if len(sources) != 1 || sources[0].SOPClassUID != want.SOPClassUID || sources[0].SOPInstanceUID != want.SOPInstanceUID || !slices.Equal(sources[0].Frames, want.Frames) {
					t.Fatalf("reference %d sources %+v, want %+v", i, sources, want)
				}
			}
			for i, want := range doc.Surfaces {
				got, ok := atLimit.Surface(i + 1)
				if !ok || got.RetiredIndexLists != want.RetiredIndexLists || !slices.Equal(got.VectorAccuracy, want.VectorAccuracy) || !slices.Equal(got.Mesh.Normals, want.Mesh.Normals) {
					t.Fatalf("surface %d accuracy, normals or index width lost: %+v", i+1, got)
				}
			}
			segment := atLimit.Segments[0].Number
			got, _, ok := atLimit.SegmentMesh(segment)
			wantVertices := append(slices.Clone(second.Mesh.Vertices), first.Mesh.Vertices...)
			if !ok || got == nil || !slices.Equal(got.Vertices, wantVertices) || !slices.Equal(got.Triangles, [][3]uint32{{0, 1, 3}, {4, 5, 6}}) {
				t.Fatalf("segment mesh geometry: %+v", got)
			}
			wantPrimitives := Primitives{Vertices: []uint32{2, 7}, Edges: []Edge{{1, 3}, {4, 6}}, Lines: []Polyline{{3, 0, 2, 1}, {6, 4, 5, 7}}}
			if got := atLimit.SegmentPrimitives(segment); !equalPrimitives(got, wantPrimitives) {
				t.Fatalf("segment primitives %+v, want %+v", got, wantPrimitives)
			}
		})
	}
}
