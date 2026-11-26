package render

import (
	"context"
	"math"
	"testing"
)

func TestVRGeometryVertexColorsInterpolateAndRejectMalformedAttributes(t *testing.T) {
	cam := NewVRCamera(20)
	cam.Pitch = 0
	cam.Projection = VRProjectionParallel
	m := VRGeometryMesh{Vertices: []Vec3{{X: -5, Z: -5}, {X: 5, Z: -5}, {Z: 5}}, Triangles: [][3]uint32{{0, 1, 2}}, Color: RGBA{R: 1}, Colors: [][3]float32{{0, 1, 0}, {0, 0, 1}, {0, 1, 0}}}
	depth, err := RasterizeVRGeometryContext(context.Background(), cam, 64, 48, &VRGeometry{Meshes: []VRGeometryMesh{m}})
	if err != nil {
		t.Fatal(err)
	}
	_, rgb, hit := depth.At(32, 24)
	if !hit || rgb.G <= rgb.R || rgb.B <= rgb.R {
		t.Fatalf("per-vertex colour was replaced with uniform red: %+v %v", rgb, hit)
	}
	for _, bad := range [][][3]float32{{{1, 0, 0}}, {{float32(math.NaN()), 0, 0}, {0, 0, 0}, {0, 0, 0}}, {{-1, 0, 0}, {0, 0, 0}, {0, 0, 0}}, {{2, 0, 0}, {0, 0, 0}, {0, 0, 0}}} {
		m.Colors = bad
		if got, err := RasterizeVRGeometryContext(context.Background(), cam, 64, 48, &VRGeometry{Meshes: []VRGeometryMesh{m}}); err == nil || got != nil {
			t.Fatal("invalid colours rendered partially")
		}
	}
}
