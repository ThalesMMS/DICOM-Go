package mesh

import (
	"math"
	"reflect"
	"slices"
	"testing"
)

// VTK's TestConstrainedSmoothingFilter uses prescribed constraints on an
// open sheet, while TestWindowedSincPolylineSmoothing exercises free
// contours. The peak of this triangular fan can move independently of
// its fixed rim; a free rim may move along its own planar contour.
func TestVTKReferenceSmoothingOpenRim(t *testing.T) {
	for _, window := range []struct {
		name string
		kind WindowFunction
	}{{"nuttall", WindowNuttall}, {"blackman", WindowBlackman}, {"hanning", WindowHanning}, {"hamming", WindowHamming}} {
		for _, mode := range []struct {
			name string
			fix  bool
		}{{"fixed", true}, {"free", false}} {
			t.Run(window.name+"/"+mode.name, func(t *testing.T) {
				source := &Mesh{
					Vertices:  [][3]float32{{-2, -2, 0}, {2, -2, 0}, {2, 2, 0}, {-2, 2, 0}, {0, 0, 4}},
					Triangles: [][3]uint32{{0, 1, 4}, {1, 2, 4}, {2, 3, 4}, {3, 0, 4}},
				}
				got, err := SmoothWindowedSinc(source, SmoothOptions{Window: window.kind, FixBoundary: mode.fix})
				if err != nil {
					t.Fatal(err)
				}
				if err := got.Validate(); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(got.Triangles, source.Triangles) {
					t.Fatal("smoothing changed triangle connectivity")
				}
				peak := got.Vertices[4]
				if peak[0] != 0 || peak[1] != 0 || !(peak[2] < 3.9) {
					t.Fatalf("smoothed peak = %v, want a lower peak on the central axis", peak)
				}
				for i, v := range got.Vertices[:4] {
					if mode.fix {
						if v != source.Vertices[i] {
							t.Fatalf("fixed rim vertex %d moved from %v to %v", i, source.Vertices[i], v)
						}
						continue
					}
					if v[2] != 0 || v == source.Vertices[i] {
						t.Fatalf("free rim vertex %d = %v, want in-plane movement from %v", i, v, source.Vertices[i])
					}
				}
			})
		}
	}
}

// VTK's TestQuadricDecimationDegenerateTriangle passes a repeated corner
// into a triangulated plane. DecimateQuadric requires a valid indexed mesh
// at its entry point and returns no output for a malformed model.
func TestVTKReferenceDecimationRejectsMalformedMesh(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(*Mesh)
	}{
		{"repeated-corner", func(m *Mesh) { m.Triangles[0][1] = m.Triangles[0][0] }},
		{"missing-corner", func(m *Mesh) { m.Triangles[0][1] = uint32(len(m.Vertices)) }},
		{"nonfinite-vertex", func(m *Mesh) { m.Vertices[0][0] = float32(math.Inf(1)) }},
		{"short-normals", func(m *Mesh) { m.Normals = make([][3]float32, len(m.Vertices)-1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := gridSheet(4, 4)
			tc.damage(source)
			before := plane1497Copy(source)
			out, report, err := DecimateQuadric(source, DecimateOptions{TargetReduction: 0.5})
			if err == nil || out != nil || report != (DecimateReport{}) {
				t.Fatalf("malformed mesh output = %v, report = %+v, error = %v", out, report, err)
			}
			if !reflect.DeepEqual(source, before) {
				t.Fatal("rejection changed source mesh")
			}
		})
	}
}
