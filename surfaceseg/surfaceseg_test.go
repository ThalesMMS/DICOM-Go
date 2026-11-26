package surfaceseg

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/commontest"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/mesh"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/seg"
	"github.com/ThalesMMS/dicom-go/validation"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

const (
	testStudyUID  = "1.2.826.0.1.3680043.10.1.1047.1"
	testSeriesUID = "1.2.826.0.1.3680043.10.1.1251.2"
	testSOPUID    = "1.2.826.0.1.3680043.10.1.1251.3"
	ctImage       = "1.2.840.10008.5.1.4.1.1.2"
)

// twoSegmentMeshes are the SurfaceNets surfaces of two touching boxes on a
// 0.8 × 0.8 × 1.5 mm grid: closed, outward, with normals.
func twoSegmentMeshes(t testing.TB) []*mesh.Mesh {
	t.Helper()
	size := [3]int{14, 12, 10}
	labels := make([]uint32, size[0]*size[1]*size[2])
	for z := 2; z < 8; z++ {
		for y := 2; y < 10; y++ {
			for x := 2; x < 12; x++ {
				label := uint32(1)
				if x >= 7 {
					label = 2
				}
				labels[(z*size[1]+y)*size[0]+x] = label
			}
		}
	}
	field := &mesh.DenseLabelField{Size: size, Affine: mesh.GridAffine(render.Vec3{X: -20, Y: 10, Z: 5}, mesh.IdentityAxes, [3]float64{0.8, 0.8, 1.5}), Labels: labels}
	surface, err := mesh.ExtractLabels(context.Background(), field, mesh.SurfaceNetsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return []*mesh.Mesh{surface.LabelMesh(1), surface.LabelMesh(2)}
}

func testSources() []seg.ReferencedImage {
	return []seg.ReferencedImage{
		{SeriesInstanceUID: "1.2.826.0.1.3680043.10.1.1251.10", SOPClassUID: ctImage, SOPInstanceUID: "1.2.826.0.1.3680043.10.1.1251.11"},
		{SeriesInstanceUID: "1.2.826.0.1.3680043.10.1.1251.10", SOPClassUID: ctImage, SOPInstanceUID: "1.2.826.0.1.3680043.10.1.1251.12"},
	}
}

func testDocument(t testing.TB) *Document {
	t.Helper()
	meshes := twoSegmentMeshes(t)
	doc, err := FromMeshes(FromMeshesOptions{
		SOPInstanceUID: testSOPUID, StudyInstanceUID: testStudyUID, SeriesInstanceUID: testSeriesUID,
		FrameOfReferenceUID: commontest.FrameOfReferenceUID,
		Segments: []MeshSegment{
			{Segment: seg.Segment{Label: "Liver"}, ColorRGB: [3]uint8{220, 120, 60}, Mesh: meshes[0]},
			{Segment: seg.Segment{Label: "Tumour", AlgorithmType: seg.AlgorithmSemiautomatic, AlgorithmName: "Region growing"}, ColorRGB: [3]uint8{40, 200, 90}, Mesh: meshes[1], Opacity: 0.5},
		},
		Algorithm:    SurfaceNetsAlgorithm("dicom-go mesh"),
		SourceImages: testSources(),
	})
	if err != nil {
		t.Fatal(err)
	}
	doc.Common = object.DerivedCommon{Source: commontest.Source()}
	doc.Identification.ContentDescription = "Synthetic surfaces"
	return doc
}

// roundTrip writes doc as a Part 10 file and reads it back.
func roundTrip(t testing.TB, doc *Document) (*Document, *object.File) {
	t.Helper()
	file, err := Write(doc)
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatal(err)
	}
	parsed, err := object.ReadFile(&encoded)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Read(parsed.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	return back, parsed
}

// TestSurfaceSegmentationRoundTrip (#1251): two SurfaceNets segment
// surfaces survive Write and Read exactly (points, normals, triangles),
// with their colours, opacity, closure, segment descriptions, algorithm
// and references, and the instance has no IOD or value error.
func TestSurfaceSegmentationRoundTrip(t *testing.T) {
	doc := testDocument(t)
	back, parsed := roundTrip(t, doc)
	commontest.Check(t, parsed.Dataset, true)
	report, err := iod.Validate(parsed.Dataset, iod.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.IOD != "Surface Segmentation" {
		t.Fatalf("IOD %q", report.IOD)
	}
	for _, finding := range report.Errors() {
		t.Errorf("IOD: %s", finding)
	}
	values, err := validation.ValidateDataSet(context.Background(), parsed.Dataset.ToDataSet(), validation.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range values.Report.Findings {
		if finding.Severity == validation.SeverityError {
			t.Errorf("value: %+v", finding)
		}
	}
	if back.SOPInstanceUID != testSOPUID || back.StudyInstanceUID != testStudyUID || back.SeriesInstanceUID != testSeriesUID || back.FrameOfReferenceUID != commontest.FrameOfReferenceUID {
		t.Fatalf("UIDs %+v", back)
	}
	if back.Identification.ContentLabel != DefaultContentLabel || back.Identification.ContentDescription != "Synthetic surfaces" {
		t.Fatalf("identification %+v", back.Identification)
	}
	if len(back.Surfaces) != 2 || len(back.Segments) != 2 {
		t.Fatalf("%d surfaces, %d segments", len(back.Surfaces), len(back.Segments))
	}
	for i, surface := range back.Surfaces {
		want := doc.Surfaces[i].Mesh
		got := surface.Mesh
		if len(got.Vertices) != len(want.Vertices) || len(got.Triangles) != len(want.Triangles) || len(got.Normals) != len(want.Normals) {
			t.Fatalf("surface %d sizes %d/%d/%d, want %d/%d/%d", i+1, len(got.Vertices), len(got.Triangles), len(got.Normals), len(want.Vertices), len(want.Triangles), len(want.Normals))
		}
		for v := range want.Vertices {
			if got.Vertices[v] != want.Vertices[v] || got.Normals[v] != want.Normals[v] {
				t.Fatalf("surface %d vertex %d changed", i+1, v)
			}
		}
		for k := range want.Triangles {
			if got.Triangles[k] != want.Triangles[k] {
				t.Fatalf("surface %d triangle %d changed", i+1, k)
			}
		}
		if surface.Number != i+1 || surface.FiniteVolume != Yes || surface.Manifold != Yes || surface.PresentationType != PresentationSurface || surface.Processing != nil {
			t.Fatalf("surface %d attributes %+v", i+1, surface)
		}
		if surface.RecommendedDisplayCIELab != doc.Segments[i].RecommendedDisplayCIELab {
			t.Fatalf("surface %d colour %v, want %v", i+1, surface.RecommendedDisplayCIELab, doc.Segments[i].RecommendedDisplayCIELab)
		}
		if !got.CheckClosure().Closed() || math.Abs(got.Volume()-want.Volume()) > 1e-9 {
			t.Fatalf("surface %d closure or volume changed", i+1)
		}
	}
	if math.Abs(back.Surfaces[0].Opacity-1) > 1e-7 || math.Abs(back.Surfaces[1].Opacity-0.5) > 1e-7 {
		t.Fatalf("opacities %v and %v", back.Surfaces[0].Opacity, back.Surfaces[1].Opacity)
	}
	for i, segment := range back.Segments {
		want := doc.Segments[i]
		if segment.Segment != want.Segment {
			t.Fatalf("segment %d description %+v, want %+v", i+1, segment.Segment, want.Segment)
		}
		if len(segment.Surfaces) != 1 || segment.Surfaces[0].Number != i+1 || segment.Surfaces[0].Algorithm != SurfaceNetsAlgorithm("dicom-go mesh") {
			t.Fatalf("segment %d surfaces %+v", i+1, segment.Surfaces)
		}
		if sources := segment.Surfaces[0].SourceInstances; len(sources) != 2 || sources[1].SOPInstanceUID != testSources()[1].SOPInstanceUID || sources[1].SOPClassUID != ctImage {
			t.Fatalf("segment %d sources %+v", i+1, sources)
		}
	}
	if len(back.ReferencedImages) != 2 || back.ReferencedImages[0].SeriesInstanceUID != testSources()[0].SeriesInstanceUID {
		t.Fatalf("referenced images %+v", back.ReferencedImages)
	}
	// Writing what was read gives the same document again.
	back.Common = doc.Common
	again, _ := roundTrip(t, back)
	if len(again.Surfaces) != 2 || again.Surfaces[1].Opacity != back.Surfaces[1].Opacity || again.Segments[1].Segment != back.Segments[1].Segment {
		t.Fatal("a second round trip changed the document")
	}
}

// TestSurfaceSegmentationParts (#1251): the segments come back as coloured
// mesh parts that WriteGLB and WriteOBJParts accept.
func TestSurfaceSegmentationParts(t *testing.T) {
	doc := testDocument(t)
	back, _ := roundTrip(t, doc)
	parts, err := back.TriangleParts()
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0].Name != "Liver" || parts[1].Name != "Tumour" {
		t.Fatalf("parts %+v", parts)
	}
	for i, want := range [][3]uint8{{220, 120, 60}, {40, 200, 90}} {
		for k := 0; k < 3; k++ {
			if d := int(parts[i].Color[k]) - int(want[k]); d < -1 || d > 1 {
				t.Fatalf("part %d colour %v, want %v", i, parts[i].Color, want)
			}
		}
		if parts[i].Mesh != back.Surfaces[i].Mesh {
			t.Fatalf("part %d is not its surface", i)
		}
	}
	var glb, obj bytes.Buffer
	if err := mesh.WriteGLB(&glb, parts, mesh.GLBOptions{Generator: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := mesh.WriteOBJParts(&obj, parts, "parts.mtl", "test"); err != nil {
		t.Fatal(err)
	}
	if glb.Len() == 0 || !strings.Contains(obj.String(), "Tumour") {
		t.Fatal("empty exports")
	}
	// A segment made of two surfaces merges them.
	back.Segments[0].Surfaces = append(back.Segments[0].Surfaces, SurfaceReference{Number: 2, Algorithm: back.Segments[1].Surfaces[0].Algorithm})
	merged, _, ok := back.SegmentMesh(1)
	if !ok || len(merged.Triangles) != len(back.Surfaces[0].Mesh.Triangles)+len(back.Surfaces[1].Mesh.Triangles) {
		t.Fatal("two surfaces of one segment were not merged")
	}
	if _, _, ok := back.SegmentMesh(9); ok {
		t.Fatal("a missing segment has a mesh")
	}
}

// TestReadOtherPrimitives: strips (with their alternating winding), fans,
// facets and the retired 16-bit lists read as triangles; edges, points and
// lines are preserved without becoming triangles.
func TestReadOtherPrimitives(t *testing.T) {
	doc := testDocument(t)
	file, err := Write(doc)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the first surface's mesh with a square of four points.
	square := &mesh.Mesh{Vertices: [][3]float32{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}, {1, 1, 0}}}
	ow := func(values ...uint16) core.Element {
		return derivedio.Raw(tagPrimitivePointIndexList, core.VROW, derivedio.Uint16Bytes(values))
	}
	surfaces, _ := file.Dataset.GetSequence(tagSurfaceSequence)
	items := make([]core.DataSet, len(surfaces))
	for i, item := range surfaces {
		items[i] = item.ToDataSet()
	}
	first := object.FromElements(items[0].Elements, nil)
	first.Put(derivedio.Seq(tagSurfacePointsSequence, pointsItem(square.Vertices)))
	first.Put(derivedio.Seq(tagSurfaceNormalsSequence))
	first.Put(derivedio.Seq(tagMeshPrimitivesSequence, derivedio.DataSet(
		derivedio.Raw(tagTrianglePointIndexList, core.VROW, derivedio.Uint16Bytes([]uint16{1, 2, 3})),
		derivedio.Raw(tagEdgePointIndexList, core.VROW, derivedio.Uint16Bytes([]uint16{1, 2})),
		derivedio.Raw(tagLongVertexPointIndex, core.VROL, []byte{4, 0, 0, 0}),
		derivedio.Seq(tagTriangleStripSequence, derivedio.DataSet(ow(1, 2, 3, 4))),
		derivedio.Seq(tagTriangleFanSequence, derivedio.DataSet(ow(1, 2, 4, 3))),
		derivedio.Seq(tagFacetSequence, derivedio.DataSet(ow(1, 2, 4))),
		derivedio.Seq(tagLineSequence, derivedio.DataSet(ow(1, 4))),
	)))
	items[0] = first.ToDataSet()
	file.Dataset.Put(derivedio.Seq(tagSurfaceSequence, items...))
	back, err := Read(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	got := back.Surfaces[0]
	want := [][3]uint32{{0, 1, 2}, {0, 1, 2}, {2, 1, 3}, {0, 1, 3}, {0, 3, 2}, {0, 1, 3}}
	if len(got.Mesh.Triangles) != len(want) {
		t.Fatalf("triangles %v, want %v", got.Mesh.Triangles, want)
	}
	for i := range want {
		if got.Mesh.Triangles[i] != want[i] {
			t.Fatalf("triangles %v, want %v", got.Mesh.Triangles, want)
		}
	}
	if got.IgnoredPrimitives != 0 || len(got.Primitives.Vertices) != 1 || got.Primitives.Vertices[0] != 3 || len(got.Primitives.Edges) != 1 || got.Primitives.Edges[0] != (Edge{0, 1}) || len(got.Primitives.Lines) != 1 || len(got.Primitives.Lines[0]) != 2 || got.Primitives.Lines[0][1] != 3 {
		t.Fatalf("primitives not preserved: %+v", got.Primitives)
	}
	for _, tri := range got.Mesh.Triangles {
		n := triangleNormalZ(got.Mesh, tri)
		if n <= 0 {
			t.Fatalf("triangle %v winds clockwise", tri)
		}
	}
}

func triangleNormalZ(m *mesh.Mesh, t [3]uint32) float32 {
	a, b, c := m.Vertices[t[0]], m.Vertices[t[1]], m.Vertices[t[2]]
	return (b[0]-a[0])*(c[1]-a[1]) - (b[1]-a[1])*(c[0]-a[0])
}

func TestWriteValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Document)
	}{
		{"no SOP Instance UID", func(d *Document) { d.SOPInstanceUID = "" }},
		{"no Frame of Reference", func(d *Document) { d.FrameOfReferenceUID = " " }},
		{"malformed Study Instance UID", func(d *Document) { d.StudyInstanceUID = "study-1" }},
		{"malformed referenced image", func(d *Document) { d.ReferencedImages[0].SOPInstanceUID = "img-1" }},
		{"no surfaces", func(d *Document) { d.Surfaces = nil }},
		{"no segments", func(d *Document) { d.Segments = nil }},
		{"surface numbers out of order", func(d *Document) { d.Surfaces[1].Number = 5 }},
		{"missing referenced surface", func(d *Document) { d.Segments[0].Surfaces[0].Number = 3 }},
		{"repeated segment number", func(d *Document) { d.Segments[1].Number = d.Segments[0].Number }},
		{"segment without label", func(d *Document) { d.Segments[0].Label = "" }},
		{"segment without surfaces", func(d *Document) { d.Segments[0].Surfaces = nil }},
		{"incomplete category", func(d *Document) { d.Segments[0].SegmentedPropertyCategory.CodeMeaning = "" }},
		{"automatic without algorithm name", func(d *Document) { d.Segments[1].AlgorithmName = "" }},
		{"incomplete generation algorithm", func(d *Document) { d.Segments[0].Surfaces[0].Algorithm.Version = "" }},
		{"opacity above one", func(d *Document) { d.Surfaces[0].Opacity = 1.5 }},
		{"presentation type", func(d *Document) { d.Surfaces[0].PresentationType = "SOLID" }},
		{"manifold value", func(d *Document) { d.Surfaces[0].Manifold = "MAYBE" }},
		{"non-finite point", func(d *Document) { d.Surfaces[0].Mesh.Vertices[0][1] = float32(math.NaN()) }},
		{"triangle beyond the points", func(d *Document) {
			d.Surfaces[0].Mesh.Triangles[0][2] = uint32(len(d.Surfaces[0].Mesh.Vertices))
		}},
		{"normals for some points", func(d *Document) { d.Surfaces[0].Mesh.Normals = d.Surfaces[0].Mesh.Normals[:3] }},
		{"source without SOP Class", func(d *Document) { d.Segments[0].Surfaces[0].SourceInstances[0].SOPClassUID = "" }},
		{"processing without algorithm", func(d *Document) { d.Surfaces[0].Processing = &Processing{Ratio: 0.5} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := testDocument(t)
			c.mutate(doc)
			if _, err := Write(doc); !errors.Is(err, ErrInvalidObject) {
				t.Fatalf("Write: %v", err)
			}
		})
	}
	// Processed surfaces and explicit attributes are kept.
	doc := testDocument(t)
	doc.Surfaces[0].Processing = &Processing{Ratio: 0.25, Description: "Quadric decimation", Algorithm: Algorithm{Family: seg.CodedEntry{CodeValue: "123104", CodingSchemeDesignator: "DCM", CodeMeaning: "Morphological Operations"}, Name: "DecimateQuadric", Version: "1"}}
	doc.Surfaces[0].PresentationType = PresentationWireframe
	doc.Surfaces[0].Comments = "decimated"
	back, parsed := roundTrip(t, doc)
	if p := back.Surfaces[0].Processing; p == nil || math.Abs(p.Ratio-0.25) > 1e-7 || p.Description != "Quadric decimation" || p.Algorithm.Name != "DecimateQuadric" {
		t.Fatalf("processing %+v", back.Surfaces[0].Processing)
	}
	if back.Surfaces[0].PresentationType != PresentationWireframe || back.Surfaces[0].Comments != "decimated" {
		t.Fatalf("surface %+v", back.Surfaces[0])
	}
	report, err := iod.Validate(parsed.Dataset, iod.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range report.Errors() {
		t.Errorf("IOD with processing: %s", finding)
	}
}

func TestReadRejectsInconsistentData(t *testing.T) {
	encode := func(t *testing.T) *object.File {
		file, err := Write(testDocument(t))
		if err != nil {
			t.Fatal(err)
		}
		return file
	}
	surfaceItems := func(file *object.File) []*object.Object {
		items, _ := file.Dataset.GetSequence(tagSurfaceSequence)
		return items
	}
	setSurfaces := func(file *object.File, items []*object.Object) {
		sets := make([]core.DataSet, len(items))
		for i, item := range items {
			sets[i] = item.ToDataSet()
		}
		file.Dataset.Put(derivedio.Seq(tagSurfaceSequence, sets...))
	}
	cases := []struct {
		name   string
		mutate func(*object.File)
		want   error
	}{
		{"another SOP class", func(f *object.File) { f.Dataset.Put(derivedio.UI(derivedio.TagSOPClassUID, seg.SegmentationStorage)) }, ErrUnsupportedSOPClass},
		{"number of surfaces", func(f *object.File) { f.Dataset.Put(derivedio.UL(tagNumberOfSurfaces, 3)) }, ErrInvalidObject},
		{"point count", func(f *object.File) {
			items := surfaceItems(f)
			points, _ := items[0].GetSequence(tagSurfacePointsSequence)
			points[0].Put(derivedio.UL(tagNumberOfSurfacePoints, 7))
			items[0].Put(derivedio.Seq(tagSurfacePointsSequence, points[0].ToDataSet()))
			setSurfaces(f, items)
		}, ErrInvalidObject},
		{"zero index", func(f *object.File) {
			items := surfaceItems(f)
			items[0].Put(derivedio.Seq(tagMeshPrimitivesSequence, derivedio.DataSet(derivedio.Raw(tagLongTrianglePointIndex, core.VROL, make([]byte, 12)))))
			setSurfaces(f, items)
		}, ErrInvalidObject},
		{"missing surface", func(f *object.File) {
			setSurfaces(f, surfaceItems(f)[:1])
			f.Dataset.Put(derivedio.UL(tagNumberOfSurfaces, 1))
		}, ErrInvalidObject},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			file := encode(t)
			c.mutate(file)
			if _, err := Read(file.Dataset); !errors.Is(err, c.want) {
				t.Fatalf("Read: %v, want %v", err, c.want)
			}
		})
	}
	if _, err := Read(nil); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("nil: %v", err)
	}
}
