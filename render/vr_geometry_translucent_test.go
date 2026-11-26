package render

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func translucentSquare(y float64, colour RGBA, opacity float64) VRGeometryMesh {
	return VRGeometryMesh{
		Vertices:  []Vec3{{X: -5, Y: y, Z: -5}, {X: 5, Y: y, Z: -5}, {X: 5, Y: y, Z: 5}, {X: -5, Y: y, Z: 5}},
		Triangles: [][3]uint32{{0, 1, 2}, {0, 2, 3}},
		Color:     colour,
		Opacity:   opacity,
	}
}

// TestRasterizeVRGeometryKeepsTranslucentFragments (#1236): translucent
// faces in front of the opaque depth become sorted, premultiplied
// fragments; those behind it are dropped; every pixel of a square, its
// diagonal included, gets exactly one fragment from it.
func TestRasterizeVRGeometryKeepsTranslucentFragments(t *testing.T) {
	for _, projection := range []VRCameraProjection{VRProjectionPerspective, VRProjectionParallel} {
		cam := NewVRCamera(20)
		cam.Pitch = 0
		cam.Projection = projection
		geometry := &VRGeometry{Meshes: []VRGeometryMesh{
			translucentSquare(8, RGBA{R: 1, G: 1, B: 1}, 0.5), // behind the opaque one
			translucentSquare(3, RGBA{G: 1}, 0.25),
			translucentSquare(6, RGBA{B: 1}, 0),
			translucentSquare(-2, RGBA{R: 1}, 0.5),
		}}
		if !geometry.Translucent() || geometry.Meshes[2].Translucent() {
			t.Fatal("translucency flags")
		}
		const w, h = 64, 48
		depth, err := RasterizeVRGeometryContext(context.Background(), cam, w, h, geometry)
		if err != nil {
			t.Fatal(err)
		}
		if !depth.HasFragments() {
			t.Fatal("no fragments")
		}
		rayGen := newVRRayGenerator(cam, w, h)
		inside := 0
		for py := 0; py < h; py++ {
			row := rayGen.rowBase(py)
			for px := 0; px < w; px++ {
				o, d := rayGen.ray(px, row)
				fragments := depth.Fragments(px, py, nil)
				for i := 1; i < len(fragments); i++ {
					if !(fragments[i].T > fragments[i-1].T) {
						t.Fatalf("%v pixel %d,%d fragments out of order or repeated: %+v", projection, px, py, fragments)
					}
				}
				tOpaque, _, opaqueHit := depth.At(px, py)
				for _, f := range fragments {
					if opaqueHit && f.T >= tOpaque {
						t.Fatalf("%v pixel %d,%d fragment behind the opaque depth", projection, px, py)
					}
				}
				covered := true
				for _, y := range []float64{-2, 3, 6} {
					p := o.Add(d.Scale((y - o.Y) / d.Y))
					covered = covered && math.Abs(p.X) < 4.8 && math.Abs(p.Z) < 4.8
				}
				if !covered {
					continue
				}
				inside++
				if !opaqueHit || len(fragments) != 2 {
					t.Fatalf("%v pixel %d,%d: opaque %v, %d fragments %+v (red at %v, green at %v)", projection, px, py, opaqueHit, len(fragments), fragments, (-2-o.Y)/d.Y, (3-o.Y)/d.Y)
				}
				red, green := fragments[0], fragments[1]
				if math.Abs(red.T-(-2-o.Y)/d.Y) > 1e-4*red.T || math.Abs(green.T-(3-o.Y)/d.Y) > 1e-4*green.T {
					t.Fatalf("%v pixel %d,%d depths %v %v", projection, px, py, red.T, green.T)
				}
				// Premultiplied: the specular highlight adds to every channel.
				if red.Color.A != 0.5 || green.Color.A != 0.25 || red.Color.R <= red.Color.G || red.Color.G != red.Color.B || red.Color.R > 0.5*1.25 ||
					green.Color.G <= green.Color.R || green.Color.R != green.Color.B || green.Color.G > 0.25*1.25 {
					t.Fatalf("%v pixel %d,%d colours %+v %+v", projection, px, py, red.Color, green.Color)
				}
			}
		}
		if inside == 0 {
			t.Fatalf("%v: no pixel inside the squares", projection)
		}
	}
}

// TestVRTranslucentFragmentsOverflowBlendUnder: beyond four fragments, the
// farthest are blended under the fourth, keeping the total opacity and the
// nearest fragments' order whatever the drawing order.
func TestVRTranslucentFragmentsOverflowBlendUnder(t *testing.T) {
	cam := NewVRCamera(20)
	cam.Pitch = 0
	cam.Projection = VRProjectionParallel
	var meshes []VRGeometryMesh
	for _, y := range []float64{0, -5, -1, -3, -4, -2} {
		meshes = append(meshes, translucentSquare(y, RGBA{R: 1, G: 1, B: 1}, 0.5))
	}
	depth, err := RasterizeVRGeometryContext(context.Background(), cam, 16, 16, &VRGeometry{Meshes: meshes})
	if err != nil {
		t.Fatal(err)
	}
	fragments := depth.Fragments(8, 8, nil)
	if len(fragments) != VRMaxTranslucentFragments {
		t.Fatalf("%d fragments", len(fragments))
	}
	o, d := newVRRayGenerator(cam, 16, 16).ray(8, newVRRayGenerator(cam, 16, 16).rowBase(8))
	for i, y := range []float64{-5, -4, -3, -2} {
		if want := (y - o.Y) / d.Y; math.Abs(fragments[i].T-want) > 1e-4*want {
			t.Fatalf("fragment %d at %v, want %v", i, fragments[i].T, want)
		}
	}
	if a := compositeVRFragments(fragments, RGBA{}).A; math.Abs(a-(1-math.Pow(0.5, 6))) > 1e-6 {
		t.Fatalf("total opacity %v, want %v", a, 1-math.Pow(0.5, 6))
	}
	if last := fragments[3].Color.A; math.Abs(float64(last)-(1-math.Pow(0.5, 3))) > 1e-6 {
		t.Fatalf("merged fragment alpha %v", last)
	}
}

func TestVRTranslucentGeometryInTheRayCasters(t *testing.T) {
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
	render := func(y, opacity float64, mode VRMode) color.NRGBA {
		clip := NewVRClip()
		sphere := VRSphereMesh(at(y), 3, red)
		sphere.Opacity = opacity
		clip.SetGeometry(&VRGeometry{Meshes: []VRGeometryMesh{sphere}})
		p := preset
		p.Mode = mode
		return centre(RenderVR(vol, cam, p, window, true, clip, quality))
	}
	tissue := centre(RenderVR(vol, cam, preset, window, true, nil, quality))
	opaque := render(5, 0, VRModeDVR)
	half := render(5, 0.5, VRModeDVR)
	between := func(v, a, b uint8) bool { return int(v) > int(min(a, b))+10 && int(v) < int(max(a, b))-10 }
	if !between(half.G, opaque.G, tissue.G) || half.R < opaque.R-2 {
		t.Fatalf("translucent sphere %v, opaque %v, tissue %v", half, opaque, tissue)
	}
	if one := render(5, 1, VRModeDVR); one != opaque {
		t.Fatalf("opacity 1 = %v, opaque = %v", one, opaque)
	}
	// Behind the tissue the ray has already saturated.
	if behind := render(18, 0.5, VRModeDVR); absDiff(behind.R, tissue.R) > 2 || absDiff(behind.G, tissue.G) > 2 {
		t.Fatalf("translucent sphere behind the tissue = %v, tissue %v", behind, tissue)
	}
	// Before the volume, the ray misses nothing: the fragments meet the
	// tissue behind them.
	if outside := render(-8, 0.5, VRModeDVR); !between(outside.G, opaque.G, tissue.G) {
		t.Fatalf("translucent sphere before the volume = %v", outside)
	}
	// A projection has no depth: the fragments lie over it.
	mip := render(5, 0.5, VRModeMIP)
	if plain := centre(RenderVR(vol, cam, VRPreset{Mode: VRModeMIP, TF: preset.TF}, window, true, nil, quality)); !(int(mip.R) > int(mip.G)+10) || mip == plain {
		t.Fatalf("MIP with a translucent sphere = %v, without %v", mip, plain)
	}
	// Surface rendering composites the fragments in front of the surface.
	white := color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	surface := func(y, opacity float64) color.NRGBA {
		clip := NewVRClip()
		sphere := VRSphereMesh(at(y), 3, red)
		sphere.Opacity = opacity
		clip.SetGeometry(&VRGeometry{Meshes: []VRGeometryMesh{sphere}})
		return centre(RenderVRSurface(vol, cam, 500, white, true, clip, quality))
	}
	plainSurface := centre(RenderVRSurface(vol, cam, 500, white, true, nil, quality))
	if front := surface(5, 0.5); !(int(front.R) > int(front.G)+10) || front.G >= plainSurface.G {
		t.Fatalf("surface mode: translucent sphere in front = %v, surface %v", front, plainSurface)
	}
	if behind := surface(18, 0.5); behind != plainSurface {
		t.Fatalf("surface mode: translucent sphere behind = %v, surface %v", behind, plainSurface)
	}
}

func absDiff(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

// TestRenderVRTranslucentSurfaceGolden (#1236) freezes a 50% surface over
// DVR: a shaded ball seen through a translucent shell around it, whose back
// half the ball hides. Regenerate with DICOMGO_UPDATE_GOLDEN=1.
func TestRenderVRTranslucentSurfaceGolden(t *testing.T) {
	vol, err := BuildVolume(stack16(32, true, func(int) Rescale { return Rescale{Slope: 1} }, func(x, y, z int) int {
		dx, dy, dz := float64(x)-15.5, float64(y)-15.5, float64(z)-15.5
		r := math.Sqrt(dx*dx + dy*dy + dz*dz)
		return int(1000 * math.Max(0, math.Min(1, (9-r)/2)))
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vol.Close() })
	cam := NewVRCamera(vol.BoundingRadiusMM())
	cam.Yaw, cam.Pitch = 0.5, 0.3
	preset := VRPreset{
		Mode: VRModeDVR,
		TF: NewVRTransferFunction(
			[]TFColorStop{{HU: 0, R: 0.9, G: 0.8, B: 0.6}, {HU: 1000, R: 1, G: 0.95, B: 0.85}},
			[]TFAlphaStop{{HU: 0, A: 0}, {HU: 300, A: 0}, {HU: 700, A: 0.8}, {HU: 1000, A: 1}},
		),
	}
	window := WindowLevel{Center: 500, Width: 1000}
	quality := VRQuality{Width: 48, Height: 48, MaxSteps: DefaultVRMaxSteps}
	shell := VRSphereMesh(Vec3{}, 0.62*vol.PhysicalSize().X, RGBA{R: 0.1, G: 0.45, B: 1})
	shell.Opacity = 0.5
	clip := NewVRClip()
	clip.SetGeometry(&VRGeometry{Meshes: []VRGeometryMesh{shell}})
	img, err := RenderVRContext(context.Background(), vol, cam, preset, window, true, clip, quality)
	if err != nil {
		t.Fatal(err)
	}
	plain := RenderVR(vol, cam, preset, window, true, nil, quality).(*image.NRGBA)
	shell.Opacity = 0
	opaqueClip := NewVRClip()
	opaqueClip.SetGeometry(&VRGeometry{Meshes: []VRGeometryMesh{shell}})
	opaque := RenderVR(vol, cam, preset, window, true, opaqueClip, quality).(*image.NRGBA)
	got := img.(*image.NRGBA)
	c, p, o := got.NRGBAAt(24, 24), plain.NRGBAAt(24, 24), opaque.NRGBAAt(24, 24)
	if !(c.B > p.B+10) || !(c.R > o.R+10) {
		t.Fatalf("centre: translucent %v, ball alone %v, opaque shell %v", c, p, o)
	}
	path := filepath.Join("testdata", "vr_translucent_geometry_v1.png")
	if os.Getenv("DICOMGO_UPDATE_GOLDEN") == "1" {
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, img); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, encoded.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	golden, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if golden.Bounds() != img.Bounds() {
		t.Fatalf("golden bounds %v, frame %v", golden.Bounds(), img.Bounds())
	}
	// Fused multiply-add on some architectures moves a channel by one level.
	const tolerance = 2
	for y := 0; y < 48; y++ {
		for x := 0; x < 48; x++ {
			gr, gg, gb, _ := golden.At(x, y).RGBA()
			ir, ig, ib, _ := img.At(x, y).RGBA()
			for _, pair := range [][2]uint32{{gr, ir}, {gg, ig}, {gb, ib}} {
				if diff := int(pair[0]>>8) - int(pair[1]>>8); diff > tolerance || diff < -tolerance {
					t.Fatalf("pixel (%d,%d) = %d/%d/%d, golden %d/%d/%d", x, y, ir>>8, ig>>8, ib>>8, gr>>8, gg>>8, gb>>8)
				}
			}
		}
	}
}

// TestVRTranslucentFragmentsWithAdaptiveStepAndTransfer2D: fragments keep
// their depth order when the adaptive step (#1231) rejects a long step and
// resumes behind them, and a 2D transfer function (#1232) composites them as
// the 1D function does.
func TestVRTranslucentFragmentsWithAdaptiveStepAndTransfer2D(t *testing.T) {
	vol, cam, at := occlusionScene(t)
	preset := VRPreset{
		Mode: VRModeDVR,
		TF: NewVRTransferFunction(
			[]TFColorStop{{HU: 0, R: 1, G: 1, B: 1}, {HU: 1000, R: 1, G: 1, B: 1}},
			[]TFAlphaStop{{HU: 0, A: 0}, {HU: 500, A: 0}, {HU: 600, A: 1}, {HU: 1000, A: 1}},
		),
	}
	window := WindowLevel{Center: 500, Width: 1000}
	fixed := VRQuality{Width: 33, Height: 33, MaxSteps: DefaultVRMaxSteps}
	adaptive := fixed
	adaptive.AdaptiveStep = VRAdaptiveStep{Max: 4, Min: 0.5}
	centre := func(img image.Image) color.NRGBA { return img.(*image.NRGBA).NRGBAAt(16, 16) }
	frame := func(y float64, p VRPreset, q VRQuality) color.NRGBA {
		clip := NewVRClip()
		sphere := VRSphereMesh(at(y), 3, RGBA{R: 1})
		sphere.Opacity = 0.5
		clip.SetGeometry(&VRGeometry{Meshes: []VRGeometryMesh{sphere}})
		return centre(RenderVR(vol, cam, p, window, true, clip, q))
	}
	// Spheres in the empty half, straddling the tissue boundary and behind
	// it: the long steps over the empty half land in the tissue and resume.
	for _, y := range []float64{5, 9.5, 14} {
		f, a := frame(y, preset, fixed), frame(y, preset, adaptive)
		if absDiff(f.R, a.R) > 3 || absDiff(f.G, a.G) > 3 || absDiff(f.B, a.B) > 3 {
			t.Fatalf("sphere at %v: fixed step %v, adaptive step %v", y, f, a)
		}
	}
	minimum, maximum, _ := vol.HURange()
	table, err := VRTransfer2DFromTransferFunction(preset.TF, window, 256, 4, minimum, maximum, 500)
	if err != nil {
		t.Fatal(err)
	}
	with2D := preset
	with2D.Transfer2D = table
	for _, y := range []float64{5, 14} {
		f, two := frame(y, preset, fixed), frame(y, with2D, fixed)
		if absDiff(f.R, two.R) > 3 || absDiff(f.G, two.G) > 3 || absDiff(f.B, two.B) > 3 {
			t.Fatalf("sphere at %v: 1D %v, 2D transfer %v", y, f, two)
		}
	}
}
