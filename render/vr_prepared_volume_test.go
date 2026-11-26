package render

import (
	"context"
	"image"
	"math"
	"sync"
	"testing"
)

func TestVRPreparedVolumeBasicSmooth5x5ReferenceCacheAndSourceIsolation(t *testing.T) {
	const rows, cols, depth = 4, 5, 3
	stack := &Stack{PixelSpacing: []float64{1, 1}, SliceThickness: 1}
	sourceValues := make([][][]float64, depth)
	for z := 0; z < depth; z++ {
		data := make([]byte, rows*cols)
		sourceValues[z] = make([][]float64, rows)
		for y := 0; y < rows; y++ {
			sourceValues[z][y] = make([]float64, cols)
			for x := 0; x < cols; x++ {
				value := byte(z*70 + y*10 + x)
				data[y*cols+x] = value
				sourceValues[z][y][x] = float64(value)
			}
		}
		stack.Frames = append(stack.Frames, volumeTestFrame(rows, cols, data, z))
	}
	volume, err := BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	defer volume.Close() //nolint:errcheck

	none, err := volume.PrepareVRVolume(VRPrefilterNone)
	if err != nil {
		t.Fatal(err)
	}
	noneAgain, err := volume.PrepareVRVolume(VRPrefilterNone)
	if err != nil {
		t.Fatal(err)
	}
	if none != noneAgain || none.ResidencyKey() != none.SourceIdentity {
		t.Fatal("raw VR preparation was not reused as the canonical source")
	}

	filtered, err := volume.PrepareVRVolume(VRPrefilterBasicSmooth5x5)
	if err != nil {
		t.Fatal(err)
	}
	filteredAgain, err := volume.PrepareVRVolume(VRPrefilterBasicSmooth5x5)
	if err != nil {
		t.Fatal(err)
	}
	if filtered != filteredAgain {
		t.Fatal("filtered VR preparation was rebuilt for the same content")
	}
	if filtered.SourceIdentity != none.SourceIdentity || filtered.ResidencyKey() == none.ResidencyKey() {
		t.Fatalf("prepared identities source=%+v raw=%+v filtered=%+v", filtered.SourceIdentity, none.ResidencyKey(), filtered.ResidencyKey())
	}
	if got := filtered.Descriptor().Derivation; got != VolumeDerivationVRPrefiltered {
		t.Fatalf("filtered derivation = %s", got)
	}

	lease, err := filtered.AcquireSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := lease.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for z := 0; z < depth; z++ {
		for y := 0; y < rows; y++ {
			for x := 0; x < cols; x++ {
				want := referenceBasicSmooth5x5(sourceValues[z], x, y)
				got, ok := snapshot.ModalityAt(uint32(x), uint32(y), uint32(z))
				if !ok || math.Abs(got-want) > 1e-5 {
					t.Fatalf("filtered[%d,%d,%d] = %g/%v, want %g", x, y, z, got, ok, want)
				}
			}
		}
	}
	_ = lease.Release()

	// The render-only preparation must leave the canonical 2D/MPR payload exact.
	sourceLease, err := volume.AcquireSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	source, err := sourceLease.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for z := 0; z < depth; z++ {
		for y := 0; y < rows; y++ {
			for x := 0; x < cols; x++ {
				got, ok := source.ModalityAt(uint32(x), uint32(y), uint32(z))
				if !ok || got != sourceValues[z][y][x] {
					t.Fatalf("source[%d,%d,%d] changed to %g/%v", x, y, z, got, ok)
				}
			}
		}
	}
	_ = sourceLease.Release()
}

func TestVRPreparedVolumeConcurrentRequestsShareOneGeneration(t *testing.T) {
	volume, err := BuildVolume(gradientXZStack(12, 12, 4))
	if err != nil {
		t.Fatal(err)
	}
	defer volume.Close() //nolint:errcheck
	const callers = 12
	results := make([]*VRPreparedVolume, callers)
	errs := make([]error, callers)
	var group sync.WaitGroup
	for index := range callers {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			results[index], errs[index] = volume.PrepareVRVolumeContext(context.Background(), VRPrefilterBasicSmooth5x5)
		}(index)
	}
	group.Wait()
	for index := range callers {
		if errs[index] != nil {
			t.Fatalf("caller %d: %v", index, errs[index])
		}
		if results[index] != results[0] {
			t.Fatalf("caller %d received a distinct preparation", index)
		}
	}
}

func TestVRPreparedVolumeCanceledBuildIsNotPublished(t *testing.T) {
	volume, err := BuildVolume(gradientXZStack(32, 32, 4))
	if err != nil {
		t.Fatal(err)
	}
	defer volume.Close() //nolint:errcheck
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := volume.PrepareVRVolumeContext(ctx, VRPrefilterBasicSmooth5x5); err == nil {
		t.Fatal("canceled preparation succeeded")
	}
	if prepared, err := volume.PrepareVRVolume(VRPrefilterBasicSmooth5x5); err != nil || prepared == nil {
		t.Fatalf("retry after cancellation = %+v, %v", prepared, err)
	}
}

func TestVRViewportBackgroundIsOpaqueAndLightingGainIsNotNormalized(t *testing.T) {
	preset := VRPreset{Background: RGBA{R: 0.25, G: 0.5, B: 0.75, A: 1}}
	frame := RenderVR(nil, VRCamera{}, preset, WindowLevel{}, false, nil, VRQuality{Width: 3, Height: 2}).(*image.NRGBA)
	want := [4]uint8{
		to8(linearToSRGB(0.25)),
		to8(linearToSRGB(0.5)),
		to8(linearToSRGB(0.75)),
		255,
	}
	for offset := 0; offset < len(frame.Pix); offset += 4 {
		got := [4]uint8{frame.Pix[offset], frame.Pix[offset+1], frame.Pix[offset+2], frame.Pix[offset+3]}
		if got != want {
			t.Fatalf("background pixel %d = %v, want %v", offset/4, got, want)
		}
	}

	volume := &Volume{
		AxisX: Vec3{X: 1}, AxisY: Vec3{Y: 1}, Normal: Vec3{Z: 1},
		ColSpacing: 1, RowSpacing: 1, SliceSpacing: 1,
	}
	material := GlossyVascularVRLightingMaterial()
	lit, specular := volume.shadeVRWithGradient(Vec3{X: 2}, Vec3{X: 1}, material)
	if math.Abs(lit-(material.Ambient+material.Diffuse)) > 1e-12 || math.Abs(specular-material.Specular) > 1e-12 || specular <= 1 {
		t.Fatalf("glossy headlight = %g + %g, want unnormalized %g + %g", lit, specular, material.Ambient+material.Diffuse, material.Specular)
	}
}

// TestHeadlightShadingIsVTKs (#1063): white specular added to the tinted
// ambient + diffuse, two-sided, with the reflection lobe r·v = 2(n·l)²-1.
func TestHeadlightShadingIsVTKs(t *testing.T) {
	material := VRLightingMaterial{Ambient: 0.1, Diffuse: 0.6, Specular: 0.5, SpecularPower: 2}
	eye := Vec3{Z: 1}
	for _, test := range []struct {
		name          string
		normal        Vec3
		lit, specular float64
	}{
		{"facing", Vec3{Z: 1}, 0.7, 0.5},
		{"back face lit as the front", Vec3{Z: -1}, 0.7, 0.5},
		// n·l = cos 45°: r·v = 0, no highlight; Blinn-on-n·v would give 0.25.
		{"45 degrees", Vec3{Y: 1, Z: 1}.Normalize(), 0.1 + 0.6*math.Sqrt2/2, 0},
		// n·l = cos 30°: r·v = 0.5.
		{"30 degrees", Vec3{Y: 0.5, Z: math.Sqrt(3) / 2}, 0.1 + 0.6*math.Sqrt(3)/2, 0.5 * 0.25},
		{"grazing", Vec3{Y: 1}, 0.1, 0},
	} {
		lit, specular := headlightShading(test.normal, eye, material)
		if math.Abs(lit-test.lit) > 1e-12 || math.Abs(specular-test.specular) > 1e-12 {
			t.Errorf("%s: lit %g specular %g, want %g %g", test.name, lit, specular, test.lit, test.specular)
		}
	}
	// The highlight is white: a red sample gains the same specular in every
	// channel instead of a brighter red.
	lit, specular := headlightShading(Vec3{Z: 1}, eye, material)
	if r, g := 1*lit+specular, 0*lit+specular; g != specular || r <= g {
		t.Fatalf("red sample shaded to %g/%g", r, g)
	}
	if vrOpacityThreshold != 1-1.0/255 {
		t.Fatalf("opacity threshold %g", vrOpacityThreshold)
	}
}

func referenceBasicSmooth5x5(values [][]float64, x, y int) float64 {
	sum := 0.0
	for ky := -2; ky <= 2; ky++ {
		sy := min(max(y+ky, 0), len(values)-1)
		for kx := -2; kx <= 2; kx++ {
			sx := min(max(x+kx, 0), len(values[sy])-1)
			sum += values[sy][sx] * basicSmooth5x5Kernel[ky+2][kx+2]
		}
	}
	return sum / 60
}
