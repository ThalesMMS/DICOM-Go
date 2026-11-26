package render_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"sync"
	"testing"

	r "github.com/ThalesMMS/dicom-go/render"
)

type viewStateJSONFixture struct {
	SchemaVersion int    `json:"schema_version"`
	Base          string `json:"base"`
	Toolchain     string `json:"toolchain"`
	Capture       string `json:"capture"`
	Cases         []struct {
		Name     string `json:"name"`
		JSON     string `json:"json"`
		Indented string `json:"indented"`
		Encoder  string `json:"encoder"`
		SHA256   string `json:"sha256"`
	} `json:"cases"`
}

func loadViewStateJSONFixture(t *testing.T) viewStateJSONFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/view_state_json_native.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture viewStateJSONFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.SchemaVersion != 1 || fixture.Base != "342312b0608b301fa1ff6e9df2d2572508d43ce3" || fixture.Toolchain != "go1.27.1" || fixture.Capture == "" {
		t.Fatalf("unexpected native fixture provenance: %+v", fixture)
	}
	return fixture
}

// Expected bytes were captured using public types and encoding/json on the
// unmodified native baseline, before any of the new MarshalJSON methods existed.
func TestViewStateJSONNativeFixture(t *testing.T) {
	fixture := loadViewStateJSONFixture(t)
	inputs := viewStateJSONInputs()
	if len(fixture.Cases) != len(inputs) || len(inputs) != 46 {
		t.Fatalf("fixture/input count: %d/%d, want 46", len(fixture.Cases), len(inputs))
	}
	for i, input := range inputs {
		expected := fixture.Cases[i]
		if expected.Name != input.name || expected.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(expected.JSON))) {
			t.Fatalf("case %d mismatched input/provenance: %q/%q", i, input.name, expected.Name)
		}
		t.Run(input.name, func(t *testing.T) {
			for _, value := range []any{input.value, viewStateJSONPointer(input.value)} {
				compact, err := json.Marshal(value)
				if err != nil || string(compact) != expected.JSON {
					t.Fatalf("compact bytes differ from native baseline:\ngot  %s\nwant %s\nerror %v", compact, expected.JSON, err)
				}
				indented, err := json.MarshalIndent(value, "", "  ")
				if err != nil || string(indented) != expected.Indented {
					t.Fatalf("indented bytes differ from native baseline: %v\ngot %s\nwant %s", err, indented, expected.Indented)
				}
				var b bytes.Buffer
				encoder := json.NewEncoder(&b)
				encoder.SetEscapeHTML(false)
				if err := encoder.Encode(value); err != nil || b.String() != expected.Encoder {
					t.Fatalf("Encoder bytes differ from native baseline: %v\ngot %s\nwant %s", err, b.String(), expected.Encoder)
				}
			}
		})
	}
}

func viewStateJSONPointer(value any) any {
	switch value := value.(type) {
	case r.MPRViewState:
		return &value
	case r.VRViewState:
		return &value
	case r.MPRPresentationState:
		return &value
	case r.ViewState:
		return &value
	default:
		return value // The fixture also contains typed nil pointers.
	}
}

func viewStateJSONFields(t *testing.T, value any) map[string]json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func TestViewStateJSONZeroAndEmptySemantics(t *testing.T) {
	for name, value := range map[string]any{
		"mpr": r.MPRViewState{}, "vr": r.VRViewState{}, "presentation": r.MPRPresentationState{},
	} {
		t.Run(name, func(t *testing.T) {
			fields := viewStateJSONFields(t, value)
			for _, optional := range []string{"Presentation", "Labels", "Scattering", "Cut", "Geometry", "GradientOpacity", "IsoSurfaces", "Overlay", "Fusion", "ProjectionRange", "RaySumReferenceMM"} {
				if _, found := fields[optional]; found {
					t.Errorf("zero field %s present", optional)
				}
			}
		})
	}
	fields := viewStateJSONFields(t, r.VRViewState{GradientOpacity: r.VRGradientOpacity{}, IsoSurfaces: []r.VRIsoSurfaceState{}})
	for _, name := range []string{"GradientOpacity", "IsoSurfaces"} {
		if string(fields[name]) != "[]" {
			t.Errorf("non-nil empty %s = %s, want []", name, fields[name])
		}
	}
	fields = viewStateJSONFields(t, r.VRViewState{Labels: r.VRLabelState{Volume: &r.VRLabelMap{}}, Geometry: r.VRGeometryState{Depth: []float32{}}})
	if _, found := fields["Labels"]; !found || string(fields["Geometry"]) != `{"Enabled":0}` {
		t.Fatalf("ignored sidecars must still make enclosing values non-zero: %v", fields)
	}
	for _, name := range []string{"Labels", "Geometry"} {
		if bytes.Contains(fields[name], []byte("Volume")) || bytes.Contains(fields[name], []byte("Depth")) || bytes.Contains(fields[name], []byte("Color")) {
			t.Errorf("ignored sidecar appeared in %s", name)
		}
	}
	fields = viewStateJSONFields(t, r.MPRPresentationState{Overlay: r.MPRLayerPresentation{Palette: []uint32{}}, Fusion: r.MPRFusionState{DifferencePalette: []uint32{}}})
	for _, name := range []string{"Overlay", "Fusion"} {
		if _, found := fields[name]; !found {
			t.Errorf("non-nil empty internal slice must make %s present", name)
		}
		if bytes.Contains(fields[name], []byte("Palette")) {
			t.Errorf("inner omitempty palette must remain omitted in %s", name)
		}
	}
}

func TestViewStateJSONProjectionTransport(t *testing.T) {
	for _, value := range []r.VRViewState{
		{Mode: r.VRModeRaySum, ProjectionRange: r.VRValueRange{Low: -50, High: 350, Enabled: true}, RaySumReferenceMM: 25},
		{ProjectionRange: r.VRValueRange{Low: -50, High: 350}},
	} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded r.VRViewState
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(value, decoded) {
			t.Fatalf("projection transport lost state: %s", data)
		}
	}
}

func TestViewStateJSONUnmarshalAndVersions(t *testing.T) {
	fixture := loadViewStateJSONFixture(t)
	inputs := viewStateJSONInputs()
	for i, input := range inputs {
		switch original := input.value.(type) {
		case r.ViewState:
			var decoded r.ViewState
			if err := json.Unmarshal([]byte(fixture.Cases[i].JSON), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.ContractVersion != original.ContractVersion || decoded.StructSize != original.StructSize || decoded.Kind != original.Kind {
				t.Fatalf("version/header/kind changed for %s", input.name)
			}
		case r.VRViewState:
			var decoded r.VRViewState
			if err := json.Unmarshal([]byte(fixture.Cases[i].JSON), &decoded); err != nil {
				t.Fatal(err)
			}
			if (decoded.GradientOpacity == nil) != (original.GradientOpacity == nil) || (decoded.IsoSurfaces == nil) != (original.IsoSurfaces == nil) {
				t.Fatalf("nil versus empty changed for %s", input.name)
			}
			if decoded.Labels.Volume != nil || decoded.Geometry.Depth != nil || decoded.Geometry.Color != nil {
				t.Fatalf("decoder created sidecars for %s", input.name)
			}
		}
	}
	vr := inputs[10].value.(r.VRViewState)
	old := vr
	if err := json.Unmarshal([]byte(`{"NearMM":4,"unknown":true,"Labels":{"Generation":0},"GradientOpacity":[]}`), &vr); err != nil {
		t.Fatal(err)
	}
	if vr.NearMM != 4 || vr.Labels.Generation != 0 || vr.Labels.Volume != old.Labels.Volume || !reflect.DeepEqual(vr.Labels.Styles, old.Labels.Styles) || !reflect.DeepEqual(vr.Geometry, old.Geometry) || !reflect.DeepEqual(vr.IsoSurfaces, old.IsoSurfaces) || vr.GradientOpacity == nil || len(vr.GradientOpacity) != 0 {
		t.Fatal("Unmarshal changed merge/unknown-field/sidecar/empty-slice behavior")
	}
	mpr := inputs[7].value.(r.MPRViewState)
	oldMPR := mpr
	if err := json.Unmarshal([]byte(`{"Presentation":{"Mode":1,"Base":{"WindowWidth":9}},"unknown":0}`), &mpr); err != nil {
		t.Fatal(err)
	}
	if mpr.Presentation.Base.WindowWidth != 9 || mpr.Presentation.Base.WindowCenter != oldMPR.Presentation.Base.WindowCenter || !reflect.DeepEqual(mpr.Presentation.Overlay, oldMPR.Presentation.Overlay) || !reflect.DeepEqual(mpr.Presentation.Fusion, oldMPR.Presentation.Fusion) {
		t.Fatal("Unmarshal reset unspecified presentation fields")
	}
	if data, err := json.Marshal(r.ViewState{}); err != nil || len(data) == 0 {
		t.Fatal("Marshal must not introduce ViewState validation")
	}
}

func TestViewStateJSONUnsupportedValues(t *testing.T) {
	for _, number := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		for _, value := range []any{
			r.MPRViewState{PixelOriginLPS: [3]float64{number}},
			r.MPRViewState{Presentation: r.MPRPresentationState{Base: r.MPRLayerPresentation{WindowWidth: number}}},
			r.VRViewState{NearMM: number},
			r.VRViewState{Labels: r.VRLabelState{Styles: []r.VRLabelStyleState{{RGB: [3]float64{number}}}}},
			r.VRViewState{Scattering: r.VRScatteringState{ReachMM: number}},
			r.VRViewState{Cut: r.VRCutPlaneState{CapOpacity: number}},
			r.VRViewState{GradientOpacity: r.VRGradientOpacity{{Magnitude: number}}},
			r.VRViewState{IsoSurfaces: []r.VRIsoSurfaceState{{Opacity: number}}},
			r.MPRPresentationState{Overlay: r.MPRLayerPresentation{Alpha: number}},
			r.MPRPresentationState{Fusion: r.MPRFusionState{DifferenceRange: number}},
		} {
			data, err := json.Marshal(value)
			var unsupported *json.UnsupportedValueError
			var marshaler *json.MarshalerError
			if data != nil || !errors.As(err, &unsupported) || !errors.As(err, &marshaler) {
				t.Errorf("%T %v: output=%s error=%v, want wrapped UnsupportedValueError", value, number, data, err)
			}
		}
		fields := viewStateJSONFields(t, r.VRViewState{Geometry: r.VRGeometryState{Depth: []float32{float32(number)}}})
		if string(fields["Geometry"]) != `{"Enabled":0}` {
			t.Fatal("ignored non-finite sidecar must remain serializable")
		}
	}
}

func TestViewStateJSONConcurrentSourceImmutability(t *testing.T) {
	inputs := viewStateJSONInputs()
	fixture := loadViewStateJSONFixture(t)
	vr := inputs[10].value.(r.VRViewState)
	beforeVR := vr
	beforeVR.Labels.Styles = append([]r.VRLabelStyleState(nil), vr.Labels.Styles...)
	beforeVR.Geometry.Depth = append([]float32(nil), vr.Geometry.Depth...)
	beforeVR.Geometry.Color = append([]uint32(nil), vr.Geometry.Color...)
	beforeVR.GradientOpacity = append(r.VRGradientOpacity(nil), vr.GradientOpacity...)
	beforeVR.IsoSurfaces = append([]r.VRIsoSurfaceState(nil), vr.IsoSurfaces...)
	mpr := inputs[7].value.(r.MPRViewState)
	beforeMPR := mpr
	beforeMPR.Presentation.Base.OpacityRamp = append([]r.FusionOpacityPoint(nil), mpr.Presentation.Base.OpacityRamp...)
	beforeMPR.Presentation.Overlay.Palette = append([]uint32(nil), mpr.Presentation.Overlay.Palette...)
	beforeMPR.Presentation.Fusion.DifferencePalette = append([]uint32(nil), mpr.Presentation.Fusion.DifferencePalette...)
	var wg sync.WaitGroup
	errorsFound := make(chan error, 16)
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := 0; iteration < 16; iteration++ {
				for _, c := range []struct {
					value any
					want  string
				}{{&vr, fixture.Cases[10].JSON}, {&mpr, fixture.Cases[7].JSON}} {
					data, err := json.Marshal(c.value)
					if err != nil || string(data) != c.want {
						errorsFound <- fmt.Errorf("concurrent %T differs from native bytes: %v", c.value, err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	if !reflect.DeepEqual(vr, beforeVR) || !reflect.DeepEqual(mpr, beforeMPR) {
		t.Fatal("MarshalJSON mutated caller fields/slices/sidecars")
	}
}

func TestViewStateJSONWrapperMethodPromotion(t *testing.T) {
	inputs := viewStateJSONInputs()
	fixture := loadViewStateJSONFixture(t)
	mpr := inputs[6].value.(r.MPRViewState)
	vr := inputs[10].value.(r.VRViewState)
	presentation := inputs[8].value.(r.MPRPresentationState)
	for _, c := range []struct {
		name             string
		named, anonymous any
		innerJSON        string
	}{
		{"mpr", struct {
			State r.MPRViewState
			Extra string
		}{mpr, "kept"}, struct {
			r.MPRViewState
			Extra string
		}{mpr, "not encoded"}, fixture.Cases[6].JSON},
		{"vr", struct {
			State r.VRViewState
			Extra string
		}{vr, "kept"}, struct {
			r.VRViewState
			Extra string
		}{vr, "not encoded"}, fixture.Cases[10].JSON},
		{"presentation", struct {
			State r.MPRPresentationState
			Extra string
		}{presentation, "kept"}, struct {
			r.MPRPresentationState
			Extra string
		}{presentation, "not encoded"}, fixture.Cases[8].JSON},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, named := range []any{c.named, viewStateJSONWrapperPointer(c.named)} {
				data, err := json.Marshal(named)
				if want := `{"State":` + c.innerJSON + `,"Extra":"kept"}`; err != nil || string(data) != want {
					t.Fatalf("named wrapper must preserve additional fields: %s, %v", data, err)
				}
			}
			for _, anonymous := range []any{c.anonymous, viewStateJSONWrapperPointer(c.anonymous)} {
				if _, promoted := anonymous.(json.Marshaler); !promoted {
					t.Fatal("anonymous embedding must promote MarshalJSON according to Go method sets")
				}
				data, err := json.Marshal(anonymous)
				if err != nil || string(data) != c.innerJSON {
					t.Fatalf("promoted MarshalJSON must encode only its embedded receiver: %s, %v", data, err)
				}
			}
		})
	}
}

func viewStateJSONWrapperPointer(value any) any {
	pointer := reflect.New(reflect.TypeOf(value))
	pointer.Elem().Set(reflect.ValueOf(value))
	return pointer.Interface()
}

// Inputs are fixed public-API values from the independent native capture.
type viewStateJSONInput struct {
	name  string
	value any
}

func viewStateJSONInputs() []viewStateJSONInput {
	gray := r.MPRPresentationState{Mode: r.MPRPresentationGray, Base: r.MPRLayerPresentation{WindowCenter: 40, WindowWidth: 400, Alpha: 1}}
	fusion := r.MPRPresentationState{
		Mode:    r.MPRPresentationFusion,
		Base:    r.MPRLayerPresentation{WindowCenter: 40, WindowWidth: 400, Alpha: 1, OpacityRamp: []r.FusionOpacityPoint{{Position: 0, Opacity: 0}, {Position: 1, Opacity: 1}}},
		Overlay: r.MPRLayerPresentation{WindowCenter: 3, WindowWidth: 6, Alpha: .5, Palette: []uint32{1, 2, 3}},
		Fusion:  r.MPRFusionState{OverlayGeneration: 7, OverlayDimensions: [3]uint32{2, 3, 4}, TargetToOverlayIndex: r.GeometryAffine{1, 0, 0, 2, 0, 1, 0, 3, 0, 0, 1, 4, 0, 0, 0, 1}, DifferencePalette: []uint32{4, 5, 6}},
	}
	allVR := r.VRViewState{
		PositionLPS: [3]float64{1, 2, 3}, FocalPointLPS: [3]float64{4, 5, 6}, ViewUpLPS: [3]float64{0, 1, 0},
		Projection: r.ProjectionPerspective, NearMM: .1, FarMM: 100,
		Labels:          r.VRLabelState{Generation: 9, Dimensions: [3]uint32{2, 3, 4}, Bits: 8, Styles: []r.VRLabelStyleState{{RGB: [3]float64{.1, .2, .3}, Opacity: .4, Blend: .5, Visible: 1}}, Volume: &r.VRLabelMap{}},
		Scattering:      r.VRScatteringState{Blend: .2, ReachMM: 3, Anisotropy: -.4, LightDirectionLPS: [3]float64{1, 0, 0}},
		Cut:             r.VRCutPlaneState{Planes: 1, ClippedVoxelIntensity: -1024, CapOpacity: .7, CapWindow: [2]float64{40, 400}},
		Geometry:        r.VRGeometryState{Enabled: 1, Depth: []float32{2}, Color: []uint32{0xff112233}},
		GradientOpacity: r.VRGradientOpacity{{Magnitude: 0, Opacity: .25}, {Magnitude: 1, Opacity: 1}},
		IsoSurfaces:     []r.VRIsoSurfaceState{{Value: 2, Color: [3]float64{.1, .2, .3}, Opacity: .4}},
	}
	nz := math.Copysign(0, -1)
	cases := []viewStateJSONInput{
		{"mpr-zero", r.MPRViewState{}},
		{"vr-zero", r.VRViewState{}},
		{"presentation-zero", r.MPRPresentationState{}},
		{"mpr-nil-pointer", (*r.MPRViewState)(nil)},
		{"vr-nil-pointer", (*r.VRViewState)(nil)},
		{"presentation-nil-pointer", (*r.MPRPresentationState)(nil)},
		{"mpr-gray", r.MPRViewState{PixelOriginLPS: [3]float64{1, 2, 3}, Presentation: gray}},
		{"mpr-fusion", r.MPRViewState{Presentation: fusion}},
		{"presentation-gray", gray},
		{"presentation-fusion", fusion},
		{"vr-all", allVR},
		{"vr-labels", r.VRViewState{Labels: allVR.Labels}},
		{"vr-scattering", r.VRViewState{Scattering: allVR.Scattering}},
		{"vr-cut", r.VRViewState{Cut: allVR.Cut}},
		{"vr-geometry", r.VRViewState{Geometry: allVR.Geometry}},
		{"vr-gradient", r.VRViewState{GradientOpacity: allVR.GradientOpacity}},
		{"vr-iso", r.VRViewState{IsoSurfaces: allVR.IsoSurfaces}},
		{"presentation-overlay-only", r.MPRPresentationState{Overlay: r.MPRLayerPresentation{Alpha: .25}}},
		{"presentation-fusion-only", r.MPRPresentationState{Fusion: r.MPRFusionState{OverlayGeneration: 1}}},
		{"vr-gradient-empty", r.VRViewState{GradientOpacity: r.VRGradientOpacity{}}},
		{"vr-iso-empty", r.VRViewState{IsoSurfaces: []r.VRIsoSurfaceState{}}},
		{"vr-label-styles-empty", r.VRViewState{Labels: r.VRLabelState{Styles: []r.VRLabelStyleState{}}}},
		{"vr-label-volume-only", r.VRViewState{Labels: r.VRLabelState{Volume: &r.VRLabelMap{}}}},
		{"vr-geometry-empty-depth", r.VRViewState{Geometry: r.VRGeometryState{Depth: []float32{}}}},
		{"vr-geometry-empty-color", r.VRViewState{Geometry: r.VRGeometryState{Color: []uint32{}}}},
		{"vr-geometry-infinite-sidecar", r.VRViewState{Geometry: r.VRGeometryState{Depth: []float32{float32(math.Inf(1))}}}},
		{"mpr-base-empty-palette", r.MPRViewState{Presentation: r.MPRPresentationState{Base: r.MPRLayerPresentation{Palette: []uint32{}}}}},
		{"mpr-overlay-empty-ramp", r.MPRViewState{Presentation: r.MPRPresentationState{Overlay: r.MPRLayerPresentation{OpacityRamp: []r.FusionOpacityPoint{}}}}},
		{"presentation-overlay-empty-palette", r.MPRPresentationState{Overlay: r.MPRLayerPresentation{Palette: []uint32{}}}},
		{"presentation-fusion-empty-palette", r.MPRPresentationState{Fusion: r.MPRFusionState{DifferencePalette: []uint32{}}}},
		{"vr-disabled-scattering-nonzero", r.VRViewState{Scattering: r.VRScatteringState{LightDirectionLPS: [3]float64{0, 1, 0}}}},
		{"vr-zero-negative-floats", r.VRViewState{Scattering: r.VRScatteringState{Blend: nz, ReachMM: nz, Anisotropy: nz, LightDirectionLPS: [3]float64{nz, nz, nz}}, Cut: r.VRCutPlaneState{ClippedVoxelIntensity: nz, CapOpacity: nz, CapWindow: [2]float64{nz, nz}}}},
		{"mpr-zero-negative-floats", r.MPRViewState{OutsideValue: float32(nz), Presentation: r.MPRPresentationState{Base: r.MPRLayerPresentation{WindowCenter: nz, WindowWidth: nz, Alpha: nz}, Overlay: r.MPRLayerPresentation{WindowCenter: nz, WindowWidth: nz, Alpha: nz}, Fusion: r.MPRFusionState{SwipePosition: nz, DifferenceRange: nz}}}},
		{"view-nested-both", r.ViewState{ContractVersion: r.ViewStateContractVersionV12, StructSize: r.ViewStateHeaderSizeV12, Kind: r.ViewKindMPR, VolumeGeneration: 11, ViewGeneration: 12, PresentationGeneration: 13, OutputWidth: 14, OutputHeight: 15, MPR: r.MPRViewState{Presentation: fusion}, VR: allVR}},
	}
	headers := []uint32{r.ViewStateHeaderSizeV1, r.ViewStateHeaderSizeV2, r.ViewStateHeaderSizeV3, r.ViewStateHeaderSizeV4, r.ViewStateHeaderSizeV5, r.ViewStateHeaderSizeV6, r.ViewStateHeaderSizeV7, r.ViewStateHeaderSizeV8, r.ViewStateHeaderSizeV9, r.ViewStateHeaderSizeV10, r.ViewStateHeaderSizeV11, r.ViewStateHeaderSizeV12}
	for i, size := range headers {
		cases = append(cases, viewStateJSONInput{fmt.Sprintf("view-version-%02d", i+1), r.ViewState{ContractVersion: uint32(i + 1), StructSize: size, Kind: r.ViewKindVR, VolumeGeneration: 2, ViewGeneration: 3, PresentationGeneration: 4, OutputWidth: 5, OutputHeight: 6}})
	}
	return cases
}
