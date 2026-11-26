package render

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func TestVRGradientOpacityAtAndValidate(t *testing.T) {
	f := VRGradientOpacity{{Magnitude: 10, Opacity: 0}, {Magnitude: 30, Opacity: 0.8}, {Magnitude: 50, Opacity: 1}}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ magnitude, want float64 }{
		{0, 0}, {10, 0}, {20, 0.4}, {30, 0.8}, {40, 0.9}, {50, 1}, {500, 1}, {math.NaN(), 0},
	} {
		if got := f.At(tc.magnitude); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("At(%v) = %v, want %v", tc.magnitude, got, tc.want)
		}
	}
	if got := VRGradientOpacity(nil).At(5); got != 1 {
		t.Fatalf("empty function = %v, want 1", got)
	}
	for name, bad := range map[string]VRGradientOpacity{
		"decreasing":   {{Magnitude: 5, Opacity: 0}, {Magnitude: 5, Opacity: 1}},
		"opacity":      {{Magnitude: 0, Opacity: 1.5}},
		"negative":     {{Magnitude: -1, Opacity: 0}},
		"non-finite":   {{Magnitude: math.Inf(1), Opacity: 0}},
		"too many":     make(VRGradientOpacity, MaxVRGradientOpacityPoints+1),
		"nan opacity:": {{Magnitude: 1, Opacity: math.NaN()}},
	} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalidVRGradientOpacity) {
			t.Errorf("%s: Validate() = %v", name, err)
		}
	}
}

// The function reads the covariant magnitude: a ramp of one value per column
// is 1/spacing HU/mm whatever the voxel count, while the legacy scale's
// vector counts voxels and ignores the spacing.
func TestGradientOpacityMagnitudeIsCovariant(t *testing.T) {
	for _, spacing := range []float64{0.5, 1, 2} {
		vol, err := BuildVolume(gradientColumnStackWithSpacing(8, 16, 6, 1, spacing, 1.5))
		if err != nil {
			t.Fatal(err)
		}
		sampler, ok := newVolumeSampler(vol)
		if !ok {
			t.Fatal("newVolumeSampler() ok = false")
		}
		tex := Vec3{X: 0.5, Y: 0.5, Z: 0.5}
		indexGradient := sampler.gradientIndexAt(tex)
		if got, want := vol.physicalGradientMagnitude(indexGradient), 1/spacing; math.Abs(got-want) > 1e-9 {
			t.Errorf("spacing %.1f: magnitude %.6f HU/mm, want %.6f", spacing, got, want)
		}
		if legacy := sampler.gradientOpacityVector(indexGradient).Length(); math.Abs(legacy-2*15.0/16) > 1e-9 {
			t.Errorf("spacing %.1f: legacy vector %.6f, want the spacing-free %.6f", spacing, legacy, 2*15.0/16)
		}
		preset := VRPreset{GradientOpacity: VRGradientOpacityRamp(0, 2)}
		if got, want := preset.gradientOpacityFactor(sampler, indexGradient), 0.5/spacing; math.Abs(got-min(want, 1)) > 1e-9 {
			t.Errorf("spacing %.1f: factor %.6f, want %.6f", spacing, got, min(want, 1))
		}
	}
}

// On a sphere phantom the uniform interior has no gradient: an edge ramp
// keeps only the shell, a function that is 1 everywhere changes nothing, and
// a threshold above every gradient hides the phantom.
func TestRenderVRGradientOpacityFunctionOnSpherePhantom(t *testing.T) {
	vol, err := BuildVolume(sphereStack(24, 8))
	if err != nil {
		t.Fatal(err)
	}
	cam := NewVRCamera(vol.BoundingRadiusMM())
	window := WindowLevel{Center: 128, Width: 256}
	q := PreviewVRQuality(32, 32)
	q.Jitter = false
	base := DefaultVRPreset()
	base.GradientOpacityScale = 0
	render := func(f VRGradientOpacity) [4]uint32 {
		preset := base
		preset.GradientOpacity = f
		img := RenderVR(vol, cam, preset, window, false, nil, q)
		r, g, b, a := img.At(16, 16).RGBA()
		return [4]uint32{r, g, b, a}
	}
	plain := render(nil)
	if plain != render(VRGradientOpacity{{Magnitude: 0, Opacity: 1}}) {
		t.Fatal("a function that is 1 everywhere changed the rendering")
	}
	if hidden, background := render(VRGradientOpacity{{Magnitude: 1e6, Opacity: 0}, {Magnitude: 2e6, Opacity: 1}}), render(VRGradientOpacity{{Magnitude: 0, Opacity: 0}}); hidden != background {
		t.Fatalf("threshold above every gradient = %v, want the empty rendering %v", hidden, background)
	}
	edges := render(VRGradientOpacityRamp(5, 60))
	if edges == plain {
		t.Fatal("an edge ramp did not change the sphere's rendering")
	}
	if edges[0] >= plain[0] {
		t.Fatalf("edge ramp centre %v is not dimmer than the opaque sphere %v", edges, plain)
	}
}

func TestValidateVRGradientOpacityFunction(t *testing.T) {
	state := validVRViewState()
	state.VR.GradientOpacityScale = 0
	state.VR.GradientOpacity = VRGradientOpacityRamp(10, 80)
	if err := ValidateViewState(state); err == nil {
		t.Fatal("a gradient opacity function below V10 was accepted")
	}
	state.ContractVersion, state.StructSize = ViewStateContractVersionV10, ViewStateHeaderSizeV10
	state.VR.OpacityUnitDistanceMM = 1
	state.VR.BackgroundRGBA = [4]float64{0, 0, 0, 1}
	if encoded, err := json.Marshal(state); err != nil || !bytes.Contains(encoded, []byte(`"GradientOpacity":[{"Magnitude":10,"Opacity":0}`)) {
		t.Fatalf("function JSON: %s, %v", encoded, err)
	}
	if encoded, err := json.Marshal(validVRViewState()); err != nil || bytes.Contains(encoded, []byte(`"GradientOpacity"`)) {
		t.Fatalf("function-free view state serializes GradientOpacity: %s, %v", encoded, err)
	}
	if err := ValidateViewState(state); err != nil {
		t.Fatalf("V10 function rejected: %v", err)
	}
	state.VR.GradientOpacityScale = 50
	if err := ValidateViewState(state); err == nil {
		t.Fatal("function and scale together were accepted")
	}
	state.VR.GradientOpacityScale = 0
	state.VR.GradientOpacity = VRGradientOpacity{{Magnitude: 10, Opacity: 2}}
	if err := ValidateViewState(state); !errors.Is(err, ErrInvalidViewState) {
		t.Fatalf("invalid function error = %v", err)
	}
}
