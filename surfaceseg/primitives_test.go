package surfaceseg

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/mesh"
	"github.com/ThalesMMS/dicom-go/object"
)

func primitiveDocument(t testing.TB, kind string, retired bool) *Document {
	t.Helper()
	d := testDocument(t)
	d.Surfaces, d.Segments = d.Surfaces[:1], d.Segments[:1]
	s := &d.Surfaces[0]
	s.Mesh = &mesh.Mesh{Vertices: [][3]float32{{-5, 10, 20}, {1, 10, 20}, {1, 15, 20}, {-5, 15, 20}}}
	s.RetiredIndexLists = retired
	if kind == "points" || kind == "mixed" {
		s.Primitives.Vertices = []uint32{3, 0, 2}
	}
	if kind == "lines" || kind == "mixed" {
		s.Primitives.Edges = []Edge{{3, 1}, {0, 2}, {0, 0}}
		s.Primitives.Lines = []Polyline{{2, 0, 1, 3}, {0, 2, 2, 1, 0}}
	}
	if kind == "mixed" {
		s.Mesh.Triangles = [][3]uint32{{0, 1, 2}, {0, 2, 3}}
	}
	return d
}

func equalPrimitives(a, b Primitives) bool {
	if !slices.Equal(a.Vertices, b.Vertices) || !slices.Equal(a.Edges, b.Edges) || len(a.Lines) != len(b.Lines) {
		return false
	}
	for i := range a.Lines {
		if !slices.Equal(a.Lines[i], b.Lines[i]) {
			return false
		}
	}
	return true
}

func TestNonTrianglePrimitivesRoundTrip(t *testing.T) {
	for _, kind := range []string{"points", "lines", "mixed"} {
		for _, retired := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/long", true: "/retired"}[retired], func(t *testing.T) {
				d := primitiveDocument(t, kind, retired)
				back, file := roundTrip(t, d)
				defer file.Close()
				s := back.Surfaces[0]
				m := d.Surfaces[0].Mesh
				if !equalPrimitives(s.Primitives, d.Surfaces[0].Primitives) || !slices.Equal(s.Mesh.Vertices, m.Vertices) || !slices.Equal(s.Mesh.Normals, m.Normals) || !slices.Equal(s.Mesh.Triangles, m.Triangles) || s.IgnoredPrimitives != 0 || s.RetiredIndexLists != retired {
					t.Fatalf("geometry changed: %+v", s)
				}
				if !reflect.DeepEqual(back.Segments, d.Segments) || !reflect.DeepEqual(back.ReferencedImages, d.ReferencedImages) || s.RecommendedDisplayCIELab != d.Surfaces[0].RecommendedDisplayCIELab {
					t.Fatal("surface association, source references or colour changed")
				}
				if kind != "mixed" && (s.FiniteVolume != No || s.Manifold != No || len(s.Mesh.Triangles) != 0) {
					t.Fatal("point/line data became a solid or triangles")
				}
				if parts, err := back.TriangleParts(); !errors.Is(err, ErrNonTrianglePrimitives) || len(parts) != 0 {
					t.Fatalf("mesh export silently dropped primitives: parts=%d err=%v", len(parts), err)
				}
				// A second serialization preserves semantic data and the chosen encoding.
				again, second := roundTrip(t, back)
				defer second.Close()
				if !equalPrimitives(again.Surfaces[0].Primitives, s.Primitives) {
					t.Fatal("second round trip changed primitives")
				}
			})
		}
	}
}

func TestNonTrianglePrimitivesRejectInvalidData(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Primitives
	}{
		{"vertex outside points", Primitives{Vertices: []uint32{4}}},
		{"edge outside points", Primitives{Edges: []Edge{{0, 4}}}},
		{"short line", Primitives{Lines: []Polyline{{1}}}},
		{"empty line", Primitives{Lines: []Polyline{{}}}},
		{"line outside points", Primitives{Lines: []Polyline{{0, 4}}}},
	} {
		t.Run("write/"+tc.name, func(t *testing.T) {
			d := primitiveDocument(t, "points", false)
			d.Surfaces[0].Primitives = tc.p
			if f, err := Write(d); !errors.Is(err, ErrInvalidObject) || f != nil {
				t.Fatalf("file=%v err=%v", f, err)
			}
		})
	}
	for _, tc := range []struct {
		name string
		e    core.Element
	}{
		{"vertex outside points", derivedio.Raw(tagLongVertexPointIndex, core.VROL, []byte{5, 0, 0, 0})},
		{"incomplete edge pair", derivedio.Raw(tagLongEdgePointIndex, core.VROL, []byte{1, 0, 0, 0})},
		{"line short", derivedio.Seq(tagLineSequence, derivedio.DataSet(derivedio.Raw(tagLongPrimitivePointIndex, core.VROL, []byte{1, 0, 0, 0})))},
		{"line empty", derivedio.Seq(tagLineSequence, derivedio.DataSet(derivedio.Raw(tagLongPrimitivePointIndex, core.VROL, nil)))},
		{"line outside points", derivedio.Seq(tagLineSequence, derivedio.DataSet(derivedio.Raw(tagLongPrimitivePointIndex, core.VROL, []byte{1, 0, 0, 0, 5, 0, 0, 0})))},
		{"malformed index bytes", derivedio.Raw(tagLongEdgePointIndex, core.VROL, []byte{1, 0, 0})},
	} {
		t.Run("read/"+tc.name, func(t *testing.T) {
			f, err := Write(primitiveDocument(t, "mixed", false))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			changeFirstSurface(t, f.Dataset, func(s *object.Object) {
				p := derivedio.Sequence(s, tagMeshPrimitivesSequence)[0]
				p.Put(tc.e)
				s.Put(derivedio.Seq(tagMeshPrimitivesSequence, p.ToDataSet()))
			})
			if d, err := Read(f.Dataset); !errors.Is(err, ErrInvalidObject) || d != nil {
				t.Fatalf("document=%v err=%v", d, err)
			}
		})
	}
}

func changeFirstSurface(t testing.TB, obj *object.Object, change func(*object.Object)) {
	t.Helper()
	surfaces := derivedio.Sequence(obj, tagSurfaceSequence)
	change(surfaces[0])
	items := make([]core.DataSet, len(surfaces))
	for i, s := range surfaces {
		items[i] = s.ToDataSet()
	}
	obj.Put(derivedio.Seq(tagSurfaceSequence, items...))
}

func TestNonTrianglePrimitivesResourceLimits(t *testing.T) {
	f, err := Write(primitiveDocument(t, "mixed", false))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, limits := range []ReadLimits{{MaxPoints: 3}, {MaxTriangles: 1}, {MaxPrimitives: 1}, {MaxIndices: 1}, {MaxBytes: 100}} {
		if d, err := ReadWithLimits(f.Dataset, limits); !errors.Is(err, ErrResourceLimitExceeded) || d != nil {
			t.Fatalf("limits=%+v document=%v err=%v", limits, d, err)
		}
	}
	if _, err := ReadWithLimits(f.Dataset, ReadLimits{MaxBytes: -1}); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("negative budget: %v", err)
	}
	changeFirstSurface(t, f.Dataset, func(s *object.Object) {
		p := derivedio.Sequence(s, tagSurfacePointsSequence)[0]
		p.Put(derivedio.UL(tagNumberOfSurfacePoints, ^uint32(0)))
		s.Put(derivedio.Seq(tagSurfacePointsSequence, p.ToDataSet()))
	})
	if d, err := Read(f.Dataset); !errors.Is(err, ErrResourceLimitExceeded) || d != nil {
		t.Fatalf("hostile count document=%v err=%v", d, err)
	}
}

func TestNonTrianglePrimitivesLongIndexBoundary(t *testing.T) {
	d := primitiveDocument(t, "points", false)
	d.Surfaces[0].Mesh.Vertices = make([][3]float32, 65536)
	d.Surfaces[0].Primitives.Vertices = []uint32{65535, 0}
	back, file := roundTrip(t, d)
	defer file.Close()
	if !slices.Equal(back.Surfaces[0].Primitives.Vertices, []uint32{65535, 0}) {
		t.Fatal("32-bit point index truncated")
	}
	d.Surfaces[0].RetiredIndexLists = true
	if _, err := Write(d); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("retired index overflow: %v", err)
	}
}

func TestNonTrianglePrimitivesMixedIndexRepresentations(t *testing.T) {
	for _, populatedLong := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty Long", true: "conflicting Long"}[populatedLong], func(t *testing.T) {
			f, err := Write(primitiveDocument(t, "points", true))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			changeFirstSurface(t, f.Dataset, func(s *object.Object) {
				p := derivedio.Sequence(s, tagMeshPrimitivesSequence)[0]
				var raw []byte
				if populatedLong {
					raw = []byte{1, 0, 0, 0}
				}
				p.Put(derivedio.Raw(tagLongVertexPointIndex, core.VROL, raw))
				s.Put(derivedio.Seq(tagMeshPrimitivesSequence, p.ToDataSet()))
			})
			d, err := Read(f.Dataset)
			if populatedLong {
				if !errors.Is(err, ErrInvalidObject) || d != nil {
					t.Fatalf("ambiguous index lists silently accepted: %v", err)
				}
				return
			}
			if err != nil || !slices.Equal(d.Surfaces[0].Primitives.Vertices, []uint32{3, 0, 2}) {
				t.Fatalf("populated retired list lost behind empty Long list: %v", err)
			}
			back, second := roundTrip(t, d)
			defer second.Close()
			if !slices.Equal(back.Surfaces[0].Primitives.Vertices, d.Surfaces[0].Primitives.Vertices) {
				t.Fatal("normalization changed point order")
			}
		})
	}
}
