package render

import (
	"context"
	"image"
	"image/color"
	"math"
	"testing"
)

// TestRasterizeVRGeometryDepthIsTheRayParameter (#1068): every covered pixel
// holds the exact parameter where its ray meets the triangle; the rest stay
// empty; the nearer of two overlapping faces wins.
func TestRasterizeVRGeometryDepthIsTheRayParameter(t *testing.T) {
	for _, projection := range []VRCameraProjection{VRProjectionPerspective, VRProjectionParallel} {
		cam := NewVRCamera(20)
		cam.Pitch = 0
		cam.Projection = projection
		square := func(y float64, colour RGBA) VRGeometryMesh {
			return VRGeometryMesh{
				Vertices:  []Vec3{{X: -5, Y: y, Z: -5}, {X: 5, Y: y, Z: -5}, {X: 5, Y: y, Z: 5}, {X: -5, Y: y, Z: 5}},
				Triangles: [][3]uint32{{0, 1, 2}, {0, 2, 3}},
				Color:     colour,
			}
		}
		geometry := &VRGeometry{Meshes: []VRGeometryMesh{square(3, RGBA{G: 1}), square(-2, RGBA{R: 1})}}
		const w, h = 64, 48
		depth, err := RasterizeVRGeometryContext(context.Background(), cam, w, h, geometry)
		if err != nil {
			t.Fatal(err)
		}
		rayGen := newVRRayGenerator(cam, w, h)
		covered := 0
		for py := 0; py < h; py++ {
			row := rayGen.rowBase(py)
			for px := 0; px < w; px++ {
				o, d := rayGen.ray(px, row)
				tHit, colour, hit := depth.At(px, py)
				// The nearer square is the red one at y = -2.
				want := (-2 - o.Y) / d.Y
				p := o.Add(d.Scale(want))
				inside := math.Abs(p.X) < 5 && math.Abs(p.Z) < 5
				if math.Abs(math.Abs(p.X)-5) < 0.2 || math.Abs(math.Abs(p.Z)-5) < 0.2 {
					continue // edge pixels
				}
				if hit != inside {
					t.Fatalf("%v pixel %d,%d hit=%v, want %v", projection, px, py, hit, inside)
				}
				if !hit {
					continue
				}
				covered++
				if math.Abs(tHit-want) > 1e-4*want || colour.R <= colour.G || colour.G != colour.B {
					t.Fatalf("%v pixel %d,%d t=%v colour %+v, want red at %v", projection, px, py, tHit, colour, want)
				}
			}
		}
		if covered == 0 {
			t.Fatalf("%v: the squares cover no pixel", projection)
		}
	}
}

func TestVRTubeAndSphereMeshes(t *testing.T) {
	path := []Vec3{{}, {X: 10}, {X: 10}, {X: 10, Y: 10}, {X: 20, Y: 15, Z: 3}}
	tube := VRTubeMesh(path, 1.5, 8, RGBA{R: 1})
	if len(tube.Vertices) != 4*8 || len(tube.Triangles) != 3*8*2 || len(tube.Normals) != len(tube.Vertices) {
		t.Fatalf("tube has %d vertices, %d triangles", len(tube.Vertices), len(tube.Triangles))
	}
	distinct := []Vec3{path[0], path[1], path[3], path[4]}
	for i, v := range tube.Vertices {
		centre := distinct[i/8]
		if r := v.Sub(centre).Length(); math.Abs(r-1.5) > 1e-9 {
			t.Fatalf("tube vertex %d at radius %v", i, r)
		}
		if math.Abs(tube.Normals[i].Length()-1) > 1e-9 {
			t.Fatalf("tube normal %d not unit", i)
		}
	}
	if empty := VRTubeMesh([]Vec3{{}, {}}, 1, 8, RGBA{}); len(empty.Triangles) != 0 {
		t.Fatal("degenerate path gave a tube")
	}
	sphere := VRSphereMesh(Vec3{X: 1, Y: 2, Z: 3}, 2, RGBA{})
	for _, v := range sphere.Vertices {
		if r := v.Sub(Vec3{X: 1, Y: 2, Z: 3}).Length(); math.Abs(r-2) > 1e-9 {
			t.Fatalf("sphere vertex at radius %v", r)
		}
	}
	geometry := &VRGeometry{Meshes: []VRGeometryMesh{tube, sphere}}
	if geometry.Empty() || geometry.TriangleCount() != len(tube.Triangles)+len(sphere.Triangles) || (&VRGeometry{}).Empty() == false {
		t.Fatal("geometry counts")
	}
}

// occlusionScene is a 24³ volume, empty for y < 12 and opaque behind, seen
// head-on along +y: geometry in the empty half is visible, geometry behind
// the opaque tissue is not.
func occlusionScene(t *testing.T) (*Volume, VRCamera, func(yIndex float64) Vec3) {
	t.Helper()
	vol, err := BuildVolume(stack16(24, true, func(int) Rescale { return Rescale{Slope: 1} }, func(x, y, z int) int {
		if y < 12 {
			return 0
		}
		return 1000
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vol.Close() })
	cam := NewVRCamera(vol.BoundingRadiusMM())
	cam.Pitch = 0
	size := vol.PhysicalSize()
	at := func(yIndex float64) Vec3 {
		return Vec3{Y: (yIndex/23 - 0.5) * size.Y}
	}
	return vol, cam, at
}

func TestVRGeometryIsOccludedByTheVolume(t *testing.T) {
	vol, cam, at := occlusionScene(t)
	preset := VRPreset{
		Mode: VRModeDVR,
		TF: NewVRTransferFunction(
			[]TFColorStop{{HU: 0, R: 1, G: 1, B: 1}, {HU: 1000, R: 1, G: 1, B: 1}},
			[]TFAlphaStop{{HU: 0, A: 0}, {HU: 500, A: 0}, {HU: 600, A: 1}, {HU: 1000, A: 1}},
		),
	}
	window := WindowLevel{Center: 500, Width: 1000}
	quality := VRQuality{Width: 33, Height: 33, MaxSteps: DefaultVRMaxSteps}
	red := RGBA{R: 1}
	centre := func(img image.Image) color.NRGBA { return img.(*image.NRGBA).NRGBAAt(16, 16) }
	render := func(y float64, mode VRMode) color.NRGBA {
		clip := NewVRClip()
		clip.SetGeometry(&VRGeometry{Meshes: []VRGeometryMesh{VRSphereMesh(at(y), 3, red)}})
		p := preset
		p.Mode = mode
		return centre(RenderVR(vol, cam, p, window, true, clip, quality))
	}
	tissue := centre(RenderVR(vol, cam, preset, window, true, nil, quality))
	if front := render(5, VRModeDVR); int(front.R)-int(front.G) < 80 {
		t.Fatalf("sphere in front of the tissue = %v, want red", front)
	}
	if behind := render(18, VRModeDVR); behind != tissue {
		t.Fatalf("sphere behind the tissue = %v, want the tissue %v", behind, tissue)
	}
	// Outside the volume, before it: visible.
	if outside := render(-8, VRModeDVR); int(outside.R)-int(outside.G) < 80 {
		t.Fatalf("sphere before the volume = %v, want red", outside)
	}
	// Surface rendering: the iso-surface at y = 12 hides the sphere behind it.
	white := color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	surface := func(y float64) color.NRGBA {
		clip := NewVRClip()
		clip.SetGeometry(&VRGeometry{Meshes: []VRGeometryMesh{VRSphereMesh(at(y), 3, red)}})
		return centre(RenderVRSurface(vol, cam, 500, white, true, clip, quality))
	}
	if front := surface(5); int(front.R)-int(front.G) < 80 {
		t.Fatalf("surface mode: sphere in front = %v, want red", front)
	}
	if behind := surface(18); behind.R != behind.G {
		t.Fatalf("surface mode: sphere behind = %v, want the gray surface", behind)
	}
}
