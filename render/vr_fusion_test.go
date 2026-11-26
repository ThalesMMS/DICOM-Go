package render

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// The fused fixtures (#1180): a 32³ CT at 1 mm with a faint soft-tissue
// sphere, and PET volumes holding one hot sphere at fusionHotSpot (patient
// mm) on grids of their own: spacing, slice spacing and in-plane orientation
// differ from the CT, so the secondary is resampled at every ray sample.
var fusionHotSpot = Vec3{X: 21, Y: 11, Z: 17}

const fusionHotSpotRadiusMM = 3.5

type fusionGrid struct {
	cols, rows, depth     int
	pixelSpacing, spacing float64
	axisX, axisY, normal  Vec3
	origin                Vec3
}

// buildFusionStack fills every voxel with value(patient point of the voxel).
func buildFusionStack(grid fusionGrid, value func(Vec3) byte) *Stack {
	stack := &Stack{PixelSpacing: []float64{grid.pixelSpacing, grid.pixelSpacing}, SliceThickness: grid.spacing}
	for z := 0; z < grid.depth; z++ {
		position := grid.origin.Add(grid.normal.Scale(float64(z) * grid.spacing))
		data := make([]byte, grid.rows*grid.cols)
		for y := 0; y < grid.rows; y++ {
			for x := 0; x < grid.cols; x++ {
				point := position.Add(grid.axisX.Scale(float64(x) * grid.pixelSpacing)).Add(grid.axisY.Scale(float64(y) * grid.pixelSpacing))
				data[y*grid.cols+x] = value(point)
			}
		}
		stack.Frames = append(stack.Frames, &Frame{
			Metadata: pixeldata.Metadata{
				Rows: uint16(grid.rows), Columns: uint16(grid.cols), SamplesPerPixel: 1,
				BitsAllocated: 8, BitsStored: 8, HighBit: 7, PhotometricInterpretation: "MONOCHROME2",
			},
			ByteOrder:        binary.LittleEndian,
			PixelBytes:       data,
			Rescale:          Rescale{Slope: 1},
			ImageOrientation: []float64{grid.axisX.X, grid.axisX.Y, grid.axisX.Z, grid.axisY.X, grid.axisY.Y, grid.axisY.Z},
			ImagePosition:    []float64{position.X, position.Y, position.Z},
			PixelSpacing:     []float64{grid.pixelSpacing, grid.pixelSpacing},
		})
	}
	return stack
}

func fusionCTVolume(t testing.TB) *Volume {
	t.Helper()
	center := Vec3{X: 15.5, Y: 15.5, Z: 15.5}
	stack := buildFusionStack(fusionGrid{
		cols: 32, rows: 32, depth: 32, pixelSpacing: 1, spacing: 1,
		axisX: Vec3{X: 1}, axisY: Vec3{Y: 1}, normal: Vec3{Z: 1},
	}, func(p Vec3) byte {
		if p.Sub(center).Length() <= 14 {
			return 100
		}
		return 0
	})
	volume, err := BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { volume.Close() })
	return volume
}

// fusionPETVolume builds a PET grid centred on the CT, rotated by degrees
// about the patient Z axis, with the given in-plane and slice spacing.
func fusionPETVolume(t testing.TB, degrees, pixelSpacing, sliceSpacing float64, hotSpot Vec3) *Volume {
	t.Helper()
	angle := degrees * math.Pi / 180
	axisX := Vec3{X: math.Cos(angle), Y: math.Sin(angle)}
	axisY := Vec3{X: -math.Sin(angle), Y: math.Cos(angle)}
	normal := Vec3{Z: 1}
	const extent = 40.0
	cols := int(extent/pixelSpacing) + 1
	depth := int(extent/sliceSpacing) + 1
	center := Vec3{X: 15.5, Y: 15.5, Z: 15.5}
	half := float64(cols-1) * pixelSpacing / 2
	origin := center.Sub(axisX.Scale(half)).Sub(axisY.Scale(half)).Sub(normal.Scale(float64(depth-1) * sliceSpacing / 2))
	stack := buildFusionStack(fusionGrid{
		cols: cols, rows: cols, depth: depth, pixelSpacing: pixelSpacing, spacing: sliceSpacing,
		axisX: axisX, axisY: axisY, normal: normal, origin: origin,
	}, func(p Vec3) byte {
		if p.Sub(hotSpot).Length() <= fusionHotSpotRadiusMM {
			return 255
		}
		return 0
	})
	volume, err := BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { volume.Close() })
	return volume
}

func fusionPrimaryPreset() VRPreset {
	tf := NewNormalizedVRTransferFunction(
		[]TFColorStop{{HU: 0, R: 1, G: 1, B: 1}, {HU: 1, R: 1, G: 1, B: 1}},
		[]TFAlphaStop{{HU: 0, A: 0}, {HU: 0.3, A: 0}, {HU: 0.35, A: 0.03}, {HU: 1, A: 0.03}},
		1,
	)
	return VRPreset{Name: "fusion-ct", TF: tf, Mode: VRModeDVR}
}

func fusionWindow() WindowLevel { return WindowLevel{Center: 127.5, Width: 255} }

func fusionLayer(pet *Volume) VRFusionLayer {
	return VRFusionLayer{
		Volume:  pet,
		TF:      NewVRFusionPaletteTransferFunction(display.HotIronLUT(), 1),
		Window:  fusionWindow(),
		Opacity: 1,
	}
}

func fusionCamera(ct *Volume, projection VRCameraProjection, yaw, pitch float64) VRCamera {
	cam := NewVRCamera(ct.BoundingRadiusMM())
	cam.FitVolume(ct)
	cam.Yaw, cam.Pitch = yaw, pitch
	cam.Projection = projection
	return cam
}

// projectVRPoint is the pixel whose ray passes through the render-space
// point q (the inverse of vrRayGenerator).
func projectVRPoint(cam VRCamera, q Vec3, w, h int) (float64, float64) {
	right, up, forward := cam.basis()
	aspect := float64(w) / float64(h)
	tanHalf := math.Tan(cam.FovY / 2)
	var ndcx, ndcy float64
	if cam.Projection.normalized() == VRProjectionParallel {
		v := q.Sub(cam.Position())
		ndcx, ndcy = v.Dot(right)/cam.Distance, v.Dot(up)/cam.Distance
	} else {
		v := q.Sub(cam.Position())
		f := v.Dot(forward)
		ndcx, ndcy = v.Dot(right)/f, v.Dot(up)/f
	}
	px := (ndcx/(aspect*tanHalf)+1)*float64(w)/2 - 0.5
	py := (1-ndcy/tanHalf)*float64(h)/2 - 0.5
	return px, py
}

// patientToVRRender maps a patient point into the render space of vol.
func patientToVRRender(vol *Volume, p Vec3) Vec3 {
	d := p.Sub(vol.Center())
	return Vec3{X: d.Dot(vol.AxisX), Y: d.Dot(vol.AxisY), Z: d.Dot(vol.Normal)}
}

// differenceCentroid is the centroid of the per-pixel colour difference of
// two frames, and the total difference.
func differenceCentroid(t *testing.T, a, b image.Image) (x, y, total float64) {
	t.Helper()
	bounds := a.Bounds()
	for py := bounds.Min.Y; py < bounds.Max.Y; py++ {
		for px := bounds.Min.X; px < bounds.Max.X; px++ {
			ar, ag, ab, _ := a.At(px, py).RGBA()
			br, bg, bb, _ := b.At(px, py).RGBA()
			weight := math.Abs(float64(ar)-float64(br)) + math.Abs(float64(ag)-float64(bg)) + math.Abs(float64(ab)-float64(bb))
			x += weight * float64(px-bounds.Min.X)
			y += weight * float64(py-bounds.Min.Y)
			total += weight
		}
	}
	if total == 0 {
		t.Fatal("fused frame does not differ from the primary frame")
	}
	return x / total, y / total, total
}

func TestRenderFusedVRHotSpotProjectsToItsPatientPosition(t *testing.T) {
	ct := fusionCTVolume(t)
	prepared, err := ct.PrepareVRVolume(VRPrefilterNone)
	if err != nil {
		t.Fatal(err)
	}
	const size = 64
	quality := VRQuality{Width: size, Height: size, MaxSteps: DefaultVRMaxSteps}
	cases := []struct {
		name                  string
		degrees, pixel, slice float64
		projection            VRCameraProjection
		yaw, pitch            float64
	}{
		{name: "same grid", degrees: 0, pixel: 1, slice: 1, projection: VRProjectionPerspective},
		{name: "coarser PET, parallel", degrees: 0, pixel: 2, slice: 3, projection: VRProjectionParallel},
		{name: "rotated PET, oblique view", degrees: 30, pixel: 1.5, slice: 2, projection: VRProjectionPerspective, yaw: 0.6, pitch: 0.35},
		{name: "rotated PET, parallel lateral", degrees: -20, pixel: 2.5, slice: 2, projection: VRProjectionParallel, yaw: math.Pi / 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pet := fusionPETVolume(t, tc.degrees, tc.pixel, tc.slice, fusionHotSpot)
			cam := fusionCamera(ct, tc.projection, tc.yaw, tc.pitch)
			primary, err := RenderVRPreparedContext(context.Background(), prepared, cam, fusionPrimaryPreset(), fusionWindow(), false, nil, quality)
			if err != nil {
				t.Fatal(err)
			}
			fused, report, err := RenderFusedVRPreparedContext(context.Background(), prepared, fusionLayer(pet), cam, fusionPrimaryPreset(), fusionWindow(), false, nil, quality)
			if err != nil {
				t.Fatal(err)
			}
			if report.SecondarySamples == 0 || report.SecondaryInBounds == 0 {
				t.Fatalf("report = %+v", report)
			}
			gotX, gotY, _ := differenceCentroid(t, fused, primary)
			wantX, wantY := projectVRPoint(cam, patientToVRRender(ct, fusionHotSpot), size, size)
			if math.Hypot(gotX-wantX, gotY-wantY) > 1.5 {
				t.Fatalf("hot spot at (%.2f, %.2f), want its projection (%.2f, %.2f)", gotX, gotY, wantX, wantY)
			}
			// The hot spot reads warm: red dominates blue at its pixel.
			r, _, b, _ := fused.At(int(math.Round(wantX)), int(math.Round(wantY))).RGBA()
			if r <= b {
				t.Fatalf("hot spot pixel r=%d b=%d, want the hot-iron colour", r>>8, b>>8)
			}
		})
	}
}

func TestRenderFusedVRAppliesTheRegistrationMapper(t *testing.T) {
	ct := fusionCTVolume(t)
	prepared, err := ct.PrepareVRVolume(VRPrefilterNone)
	if err != nil {
		t.Fatal(err)
	}
	// The PET hot spot sits 6 mm along +X of where the CT anatomy is; the
	// registration maps a CT point p to the PET point p + 6 X, so the fused
	// hot spot must be drawn at fusionHotSpot - 6 X.
	shift := Vec3{X: 6}
	pet := fusionPETVolume(t, 15, 2, 2, fusionHotSpot)
	layer := fusionLayer(pet)
	layer.PrimaryToSecondary = func(p Vec3) (Vec3, error) { return p.Add(shift), nil }
	const size = 64
	quality := VRQuality{Width: size, Height: size, MaxSteps: DefaultVRMaxSteps}
	cam := fusionCamera(ct, VRProjectionParallel, 0, 0)
	primary := RenderVRPrepared(prepared, cam, fusionPrimaryPreset(), fusionWindow(), false, nil, quality)
	fused, _, err := RenderFusedVRPreparedContext(context.Background(), prepared, layer, cam, fusionPrimaryPreset(), fusionWindow(), false, nil, quality)
	if err != nil {
		t.Fatal(err)
	}
	gotX, gotY, _ := differenceCentroid(t, fused, primary)
	wantX, wantY := projectVRPoint(cam, patientToVRRender(ct, fusionHotSpot.Sub(shift)), size, size)
	if math.Hypot(gotX-wantX, gotY-wantY) > 1.5 {
		t.Fatalf("registered hot spot at (%.2f, %.2f), want (%.2f, %.2f)", gotX, gotY, wantX, wantY)
	}
}

func TestRenderFusedVRZeroOpacityEqualsPrimary(t *testing.T) {
	ct := fusionCTVolume(t)
	prepared, err := ct.PrepareVRVolume(VRPrefilterNone)
	if err != nil {
		t.Fatal(err)
	}
	pet := fusionPETVolume(t, 30, 1.5, 2, fusionHotSpot)
	quality := VRQuality{Width: 40, Height: 40, MaxSteps: DefaultVRMaxSteps}
	cam := fusionCamera(ct, VRProjectionPerspective, 0.3, 0.2)
	primary := RenderVRPrepared(prepared, cam, fusionPrimaryPreset(), fusionWindow(), true, nil, quality)
	// A secondary that never contributes (opacity zero, or a window that
	// keeps every value transparent) leaves the primary frame byte-identical.
	for name, layer := range map[string]VRFusionLayer{
		"zero opacity": func() VRFusionLayer { layer := fusionLayer(pet); layer.Opacity = 0; return layer }(),
		"transparent window": func() VRFusionLayer {
			layer := fusionLayer(pet)
			layer.Window = WindowLevel{Center: 1000, Width: 10}
			return layer
		}(),
	} {
		fused, _, err := RenderFusedVRPreparedContext(context.Background(), prepared, layer, cam, fusionPrimaryPreset(), fusionWindow(), true, nil, quality)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(fused.(*image.NRGBA).Pix, primary.(*image.NRGBA).Pix) {
			t.Fatalf("%s: fused frame differs from the primary frame", name)
		}
	}
}

func TestRenderFusedVRFailures(t *testing.T) {
	ct := fusionCTVolume(t)
	prepared, err := ct.PrepareVRVolume(VRPrefilterNone)
	if err != nil {
		t.Fatal(err)
	}
	pet := fusionPETVolume(t, 0, 2, 2, fusionHotSpot)
	quality := VRQuality{Width: 24, Height: 24, MaxSteps: DefaultVRMaxSteps}
	cam := fusionCamera(ct, VRProjectionPerspective, 0, 0)
	ctx := context.Background()

	t.Run("non-DVR mode renders the primary", func(t *testing.T) {
		preset := fusionPrimaryPreset()
		preset.Mode = VRModeMIP
		img, _, err := RenderFusedVRPreparedContext(ctx, prepared, fusionLayer(pet), cam, preset, fusionWindow(), false, nil, quality)
		if !errors.Is(err, ErrFusedVRUnsupportedMode) {
			t.Fatalf("err = %v", err)
		}
		want := RenderVRPrepared(prepared, cam, preset, fusionWindow(), false, nil, quality)
		if !bytes.Equal(img.(*image.NRGBA).Pix, want.(*image.NRGBA).Pix) {
			t.Fatal("MIP fallback is not the primary frame")
		}
	})
	t.Run("invalid layer", func(t *testing.T) {
		if _, _, err := RenderFusedVRPreparedContext(ctx, prepared, VRFusionLayer{Opacity: 1}, cam, fusionPrimaryPreset(), fusionWindow(), false, nil, quality); err == nil {
			t.Fatal("nil secondary volume accepted")
		}
		layer := fusionLayer(pet)
		layer.Opacity = math.NaN()
		if _, _, err := RenderFusedVRPreparedContext(ctx, prepared, layer, cam, fusionPrimaryPreset(), fusionWindow(), false, nil, quality); err == nil {
			t.Fatal("NaN opacity accepted")
		}
	})
	t.Run("no overlap", func(t *testing.T) {
		layer := fusionLayer(pet)
		layer.PrimaryToSecondary = func(p Vec3) (Vec3, error) { return p.Add(Vec3{X: 500}), nil }
		img, report, err := RenderFusedVRPreparedContext(ctx, prepared, layer, cam, fusionPrimaryPreset(), fusionWindow(), false, nil, quality)
		if !errors.Is(err, ErrFusionNoOverlap) || img == nil || report.SecondaryInBounds != 0 || report.SecondarySamples == 0 {
			t.Fatalf("img=%v report=%+v err=%v", img != nil, report, err)
		}
	})
	t.Run("outside the mapper domain leaves samples empty", func(t *testing.T) {
		layer := fusionLayer(pet)
		layer.PrimaryToSecondary = func(p Vec3) (Vec3, error) {
			if p.X < 16 {
				return Vec3{}, ErrMapperOutsideDomain
			}
			return p, nil
		}
		img, report, err := RenderFusedVRPreparedContext(ctx, prepared, layer, cam, fusionPrimaryPreset(), fusionWindow(), false, nil, quality)
		if err != nil || img == nil || report.SecondaryInBounds == 0 {
			t.Fatalf("report=%+v err=%v", report, err)
		}
	})
	t.Run("mapper failure is atomic", func(t *testing.T) {
		cause := errors.New("singular transform")
		layer := fusionLayer(pet)
		layer.PrimaryToSecondary = func(Vec3) (Vec3, error) { return Vec3{}, cause }
		img, _, err := RenderFusedVRPreparedContext(ctx, prepared, layer, cam, fusionPrimaryPreset(), fusionWindow(), false, nil, quality)
		var mappingErr *FusionMappingError
		if img != nil || !errors.Is(err, ErrFusionMapping) || !errors.Is(err, cause) || !errors.As(err, &mappingErr) {
			t.Fatalf("img=%v err=%v", img != nil, err)
		}
		layer.PrimaryToSecondary = func(Vec3) (Vec3, error) { return Vec3{X: math.NaN()}, nil }
		if _, _, err := RenderFusedVRPreparedContext(ctx, prepared, layer, cam, fusionPrimaryPreset(), fusionWindow(), false, nil, quality); !errors.Is(err, ErrFusionMapping) {
			t.Fatalf("non-finite mapping err = %v", err)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, _, err := RenderFusedVRPreparedContext(cancelled, prepared, fusionLayer(pet), cam, fusionPrimaryPreset(), fusionWindow(), false, nil, quality); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestMixVRFusionSampleAddsOpacitiesAndMixesPremultipliedColours(t *testing.T) {
	r, g, b, a := mixVRFusionSample(1, 0, 0, 0.2, RGBA{R: 0, G: 0, B: 1, A: 0.6})
	if math.Abs(a-0.8) > 1e-12 || math.Abs(r-0.25) > 1e-12 || g != 0 || math.Abs(b-0.75) > 1e-12 {
		t.Fatalf("mix = %v %v %v %v", r, g, b, a)
	}
	// Opacity saturates at one; the colour stays the opacity-weighted mix.
	r, _, b, a = mixVRFusionSample(1, 0, 0, 0.9, RGBA{B: 1, A: 0.3})
	if a != 1 || math.Abs(r-0.75) > 1e-12 || math.Abs(b-0.25) > 1e-12 {
		t.Fatalf("saturated mix = %v %v %v", r, b, a)
	}
}

func TestNewVRFusionPaletteTransferFunction(t *testing.T) {
	tf := NewVRFusionPaletteTransferFunction(display.HotIronLUT(), 2)
	if tf.Domain() != VRTransferDomainNormalized || tf.OpacityUnitDistanceMM() != 2 {
		t.Fatalf("domain=%v unit=%v", tf.Domain(), tf.OpacityUnitDistanceMM())
	}
	if tf.AlphaAt(0) != 0 || tf.AlphaAt(1) != 1 || math.Abs(tf.AlphaAt(0.5)-0.5) > 1e-12 {
		t.Fatal("opacity is not the linear window ramp")
	}
	r, g, b := tf.ColorAt(0.5)
	red, green, blue := display.HotIronLUT().At(128)
	if math.Abs(r*255-float64(red)) > 1.5 || math.Abs(g*255-float64(green)) > 1.5 || math.Abs(b*255-float64(blue)) > 1.5 {
		t.Fatalf("colour at 0.5 = %v %v %v, palette = %d %d %d", r, g, b, red, green, blue)
	}
	gray := NewVRFusionPaletteTransferFunction(nil, 0)
	if r, g, b := gray.ColorAt(0.25); r != g || g != b || math.Abs(r-0.25) > 1e-12 {
		t.Fatalf("gray ramp = %v %v %v", r, g, b)
	}
}

// TestRenderFusedVRGolden freezes one PET/CT frame (rotated, anisotropic PET
// over a shaded CT). Regenerate with DICOMGO_UPDATE_GOLDEN=1.
func TestRenderFusedVRGolden(t *testing.T) {
	ct := fusionCTVolume(t)
	prepared, err := ct.PrepareVRVolume(VRPrefilterNone)
	if err != nil {
		t.Fatal(err)
	}
	pet := fusionPETVolume(t, 30, 1.5, 2, fusionHotSpot)
	layer := fusionLayer(pet)
	layer.Opacity = 0.8
	cam := fusionCamera(ct, VRProjectionPerspective, 0.6, 0.35)
	img, _, err := RenderFusedVRPreparedContext(context.Background(), prepared, layer, cam, fusionPrimaryPreset(), fusionWindow(), true, nil, VRQuality{Width: 48, Height: 48, MaxSteps: DefaultVRMaxSteps})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "vr_fusion_petct_v1.png")
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

func BenchmarkRenderFusedVR(b *testing.B) {
	ct := fusionCTVolume(b)
	prepared, err := ct.PrepareVRVolume(VRPrefilterNone)
	if err != nil {
		b.Fatal(err)
	}
	pet := fusionPETVolume(b, 30, 1.5, 2, fusionHotSpot)
	cam := fusionCamera(ct, VRProjectionPerspective, 0.6, 0.35)
	quality := VRQuality{Width: 128, Height: 128, MaxSteps: DefaultVRMaxSteps}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := RenderFusedVRPreparedContext(context.Background(), prepared, fusionLayer(pet), cam, fusionPrimaryPreset(), fusionWindow(), true, nil, quality); err != nil {
			b.Fatal(err)
		}
	}
}

// PS3.4 FF.2.3.3.2 applies the same bounded weighting pair to RGB and alpha.
// The fused emission operation has a different alpha and cannot be serialized
// as that compositor while retaining its classifications and appearance.
func TestVRFusionSampleDoesNotMatchStandardVPSCompositor(t *testing.T) {
	r, g, b, alpha := mixVRFusionSample(1, 0, 0, .25, RGBA{R: 0, G: 0, B: 1, A: .5})
	if r != 1.0/3 || g != 0 || b != 2.0/3 || alpha != .75 {
		t.Fatalf("unexpected fused sample %g %g %g %g", r, g, b, alpha)
	}
	for first := 0; first <= 255; first++ {
		for second := 0; second <= 255-first; second++ {
			standardAlpha := (.25*float64(first) + .5*float64(second)) / 255
			if standardAlpha >= alpha {
				t.Fatal("standard bounded weighting unexpectedly matches additive opacity")
			}
		}
	}
}
