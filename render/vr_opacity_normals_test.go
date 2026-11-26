package render

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"
)

func TestOpacityNormalsFollowMappedBandAndPlateau(t *testing.T) {
	vol, err := BuildVolume(gradientColumnStack(3, 9, 3))
	if err != nil {
		t.Fatal(err)
	}
	defer vol.Close()
	sampler, ok := newVolumeSampler(vol)
	if !ok {
		t.Fatal("no sampler")
	}
	defer sampler.Close()
	preset := VRPreset{TF: NewNormalizedVRTransferFunction(
		[]TFColorStop{{HU: 0, R: 1}, {HU: 1, R: 1}},
		[]TFAlphaStop{{HU: 0, A: 0}, {HU: 0.5, A: 1}, {HU: 1, A: 0}}, 1)}
	window := WindowLevel{Center: 4, Width: 8}
	lut := preset.TF.BakeWindowLUT(window, 4096)
	for _, test := range []struct{ x, want float64 }{{2, 0.5}, {4, 0}, {6, -0.5}} {
		tex := Vec3{X: test.x / 8, Y: 0.5, Z: 0.5}
		g := dvrOpacityGradient(sampler, lut, tex, preset, window, 0, 8, nil, 0)
		if math.Abs(g.X-test.want) > 1e-3 || g.Y != 0 || g.Z != 0 {
			t.Fatalf("x=%g opacity gradient=%+v, want %g", test.x, g, test.want)
		}
		if scalar := sampler.gradientIndexAt(tex); scalar.X != 2 {
			t.Fatalf("scalar gradient=%+v", scalar)
		}
	}
	// A 2D classifier constant along its gradient axis has the same mapped
	// alpha field. Its normal uses classified alpha rather than the 1D TF.
	table, err := NewVRTransfer2D(3, 2, 0, 8, 10, []RGBA{{A: 0}, {A: 1}, {A: 0}, {A: 0}, {A: 1}, {A: 0}})
	if err != nil {
		t.Fatal(err)
	}
	preset.Transfer2D = table
	g := dvrOpacityGradient(sampler, lut, Vec3{X: 0.75, Y: 0.5, Z: 0.5}, preset, window, 0, 8, nil, 0)
	if math.Abs(g.X+0.5) > 1e-12 {
		t.Fatalf("2D classifier normal=%+v", g)
	}
	preset.Transfer2D = nil
	// A constant alpha field has no surface, even where scalar density varies
	// or a cut substitutes its minimum. The scalar normal must not leak in.
	preset.TF = NewNormalizedVRTransferFunction([]TFColorStop{{HU: 0, R: 1}}, []TFAlphaStop{{HU: 0, A: 0.5}}, 1)
	lut = preset.TF.BakeWindowLUT(window, 4096)
	clip := NewVRClip()
	clip.AddCutPlane(Vec3{X: 1}, 0.5)
	for _, tex := range []Vec3{{X: 0, Y: 0.5, Z: 0.5}, {X: 0.5, Y: 0.5, Z: 0.5}, {X: 1, Y: 0.5, Z: 0.5}} {
		g := dvrOpacityGradient(sampler, lut, tex, preset, window, 0, 8, clip, 0)
		if g != (Vec3{}) {
			t.Fatalf("constant alpha produced normal %+v", g)
		}
		lit, spec := vol.shadeVRWithGradient(g, Vec3{X: 1}, BasicClinicalVRLightingMaterial())
		if lit != BasicClinicalVRLightingMaterial().Ambient || spec != 0 {
			t.Fatalf("flat alpha lighting=%g/%g", lit, spec)
		}
	}
}

func TestValidateVROpacityNormalsAndJSONCompatibility(t *testing.T) {
	state := validVRViewState()
	data, err := json.Marshal(state)
	if err != nil || bytes.Contains(data, []byte("OpacityNormals")) {
		t.Fatalf("old JSON=%s/%v", data, err)
	}
	state.VR.OpacityNormals = true
	if ValidateViewState(state) == nil {
		t.Fatal("old contract accepted opacity normals")
	}
	state.ContractVersion, state.StructSize = ViewStateContractVersionV13, ViewStateHeaderSizeV13
	state.VR.OpacityUnitDistanceMM = 1
	state.VR.BackgroundRGBA = [4]float64{0, 0, 0, 1}
	if err := ValidateViewState(state); err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(state)
	var restored ViewState
	if err != nil || json.Unmarshal(data, &restored) != nil || !restored.VR.OpacityNormals {
		t.Fatalf("round trip=%s/%v", data, err)
	}
	state.VR.Mode = VRModeMIP
	if ValidateViewState(state) == nil {
		t.Fatal("projection accepted DVR normal selection")
	}
}

func TestOpacityNormalsLungInterfacesMatchAnalyticAlpha(t *testing.T) {
	stack := gradientColumnStack(3, 9, 3)
	for _, frame := range stack.Frames {
		frame.Rescale = Rescale{Slope: 100, Intercept: -900}
	}
	vol, err := BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	defer vol.Close()
	sampler, ok := newVolumeSampler(vol)
	if !ok {
		t.Fatal("no sampler")
	}
	defer sampler.Close()
	preset := VRPreset{TF: NewLungAdvancedVRTransferFunction(), OpacityNormals: true}
	window := WindowLevel{Center: -537.80029296875, Width: 408.62545776367188}
	lut := preset.TF.BakeWindowLUT(window, 4096)
	for _, x := range []float64{3, 5, 8} {
		alpha := func(column float64) float64 { return preset.TF.AlphaAt(VRWindowCoordinate(-900+100*column, window)) }
		want := alpha(min(8, x+1)) - alpha(max(0, x-1))
		got := dvrOpacityGradient(sampler, lut, Vec3{X: x / 8, Y: 0.5, Z: 0.5}, preset, window, -900, -100, nil, -900)
		if math.Abs(got.X-want) > 1e-4 || got.Y != 0 || got.Z != 0 {
			t.Fatalf("Lung column %g gradient %+v, analytic %g", x, got, want)
		}
		if x == 3 && got.X <= 0 || x == 5 && got.X >= 0 || x == 8 && got.X != 0 {
			t.Fatalf("Lung interface direction/plateau at %g: %+v", x, got)
		}
	}
}
