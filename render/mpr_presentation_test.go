package render

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

func presentedMPRViewState(mode MPRPresentationMode) ViewState {
	state := validMPRViewState()
	state.ContractVersion, state.StructSize = ViewStateContractVersionV12, ViewStateHeaderSizeV12
	state.MPR.Presentation = MPRPresentationState{
		Mode: mode,
		Base: MPRLayerPresentation{
			WindowCenter: 40, WindowWidth: 400, Alpha: 1,
			OpacityRamp: []FusionOpacityPoint{{0, 0}, {0.5, 0.25}, {1, 1}},
		},
	}
	if mode == MPRPresentationFusion {
		state.MPR.Presentation.Base.OpacityRamp = nil
		state.MPR.Presentation.Overlay = MPRLayerPresentation{
			WindowCenter: 3, WindowWidth: 6, Alpha: 0.5, TransparentBelow: 1, Bands: 8,
			Palette: PackMPRPresentationPalette(display.HotIronLUT()),
		}
		state.MPR.Presentation.Fusion = NewMPRFusionComposite(FusionCompositeMode{}, state.MPR.Presentation.Overlay)
		state.MPR.Presentation.Fusion.OverlayGeneration = 7
		state.MPR.Presentation.Fusion.OverlayDimensions = [3]uint32{4, 4, 4}
		state.MPR.Presentation.Fusion.TargetToOverlayIndex = GeometryAffine{0.5, 0, 0, 1, 0, 0.5, 0, 2, 0, 0, 0.5, 3, 0, 0, 0, 1}
	}
	return state
}

// TestValidateViewStateV12MPRPresentation (#1233): a presented MPR request
// needs V12, a finite window, a sorted ramp, 256-entry palettes and, fused,
// an affine overlay mapping, a composite and an overlay volume; a gray
// presentation carries no overlay. FreezeViewState owns the slices and a
// presentation-free request serializes as before.
func TestValidateViewStateV12MPRPresentation(t *testing.T) {
	for _, mode := range []MPRPresentationMode{MPRPresentationGray, MPRPresentationFusion} {
		state := presentedMPRViewState(mode)
		if err := ValidateViewState(state); err != nil {
			t.Fatalf("mode %d rejected: %v", mode, err)
		}
	}
	for name, test := range map[string]struct {
		mode   MPRPresentationMode
		mutate func(*MPRPresentationState, *ViewState)
	}{
		"V11": {MPRPresentationGray, func(_ *MPRPresentationState, s *ViewState) {
			s.ContractVersion, s.StructSize = ViewStateContractVersionV11, ViewStateHeaderSizeV11
		}},
		"header":          {MPRPresentationGray, func(_ *MPRPresentationState, s *ViewState) { s.StructSize = ViewStateHeaderSizeV11 }},
		"mode":            {MPRPresentationGray, func(p *MPRPresentationState, _ *ViewState) { p.Mode = 3 }},
		"no mode":         {MPRPresentationGray, func(p *MPRPresentationState, _ *ViewState) { p.Mode = MPRPresentationNone }},
		"window":          {MPRPresentationGray, func(p *MPRPresentationState, _ *ViewState) { p.Base.WindowWidth = 0 }},
		"function":        {MPRPresentationGray, func(p *MPRPresentationState, _ *ViewState) { p.Base.WindowFunction = 3 }},
		"alpha":           {MPRPresentationGray, func(p *MPRPresentationState, _ *ViewState) { p.Base.Alpha = 2 }},
		"unsorted ramp":   {MPRPresentationGray, func(p *MPRPresentationState, _ *ViewState) { p.Base.OpacityRamp[2].Position = 0.25 }},
		"palette":         {MPRPresentationGray, func(p *MPRPresentationState, _ *ViewState) { p.Base.Palette = make([]uint32, 255) }},
		"gray overlay":    {MPRPresentationGray, func(p *MPRPresentationState, _ *ViewState) { p.Fusion.OverlayGeneration = 1 }},
		"no overlay":      {MPRPresentationFusion, func(p *MPRPresentationState, _ *ViewState) { p.Fusion.OverlayGeneration = 0 }},
		"empty overlay":   {MPRPresentationFusion, func(p *MPRPresentationState, _ *ViewState) { p.Fusion.OverlayDimensions[1] = 0 }},
		"projective":      {MPRPresentationFusion, func(p *MPRPresentationState, _ *ViewState) { p.Fusion.TargetToOverlayIndex[14] = 1 }},
		"composite":       {MPRPresentationFusion, func(p *MPRPresentationState, _ *ViewState) { p.Fusion.Composite = 9 }},
		"swipe":           {MPRPresentationFusion, func(p *MPRPresentationState, _ *ViewState) { p.Fusion.SwipePosition = 1 }},
		"difference":      {MPRPresentationFusion, func(p *MPRPresentationState, _ *ViewState) { p.Fusion.DifferenceRange = 0 }},
		"overlay palette": {MPRPresentationFusion, func(p *MPRPresentationState, _ *ViewState) { p.Overlay.Palette[3] = 1 << 24 }},
		"difference palette": {MPRPresentationFusion, func(p *MPRPresentationState, _ *ViewState) {
			p.Fusion.Composite, p.Fusion.DifferencePalette = FusionDifference, nil
		}},
	} {
		state := presentedMPRViewState(test.mode)
		state.MPR.Presentation = freezeMPRPresentation(state.MPR.Presentation)
		test.mutate(&state.MPR.Presentation, &state)
		if err := ValidateViewState(state); !errors.Is(err, ErrInvalidViewState) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	vr := validVRViewState()
	vr.MPR.Presentation = presentedMPRViewState(MPRPresentationGray).MPR.Presentation
	if err := ValidateViewState(vr); !errors.Is(err, ErrInvalidViewState) {
		t.Errorf("VR request with an MPR presentation: err = %v", err)
	}

	state := presentedMPRViewState(MPRPresentationFusion)
	frozen, err := FreezeViewState(state)
	if err != nil {
		t.Fatal(err)
	}
	state.MPR.Presentation.Overlay.Palette[0] = 0xffffff
	state.MPR.Presentation.Fusion.DifferencePalette[0] = 0xffffff
	if frozen.MPR.Presentation.Overlay.Palette[0] == 0xffffff || frozen.MPR.Presentation.Fusion.DifferencePalette[0] == 0xffffff {
		t.Fatal("FreezeViewState shares the presentation palettes with the caller")
	}
	if encoded, err := json.Marshal(validMPRViewState()); err != nil || bytes.Contains(encoded, []byte(`"Presentation"`)) {
		t.Fatalf("presentation-free view state serializes Presentation: %s, %v", encoded, err)
	}
	encoded, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ViewState
	if err := json.Unmarshal(encoded, &decoded); err != nil || ValidateViewState(decoded) != nil ||
		len(decoded.MPR.Presentation.Overlay.Palette) != MPRPresentationPaletteEntries {
		t.Fatalf("presented view state does not round-trip through JSON: %v", err)
	}
}

// TestValidateFrameOutputPresentedMPRIsRGBA8: a presented MPR frame is RGBA8.
func TestValidateFrameOutputPresentedMPRIsRGBA8(t *testing.T) {
	state := presentedMPRViewState(MPRPresentationGray)
	frame := FrameOutput{
		ContractVersion: FrameOutputContractVersion, StructSize: FrameOutputHeaderSizeV1,
		Format: FrameFormatRGBA8NRGBATopLeft, Width: 2, Height: 2, StrideBytes: 8,
		CapacityBytes: 16, WrittenBytes: 16, VolumeGeneration: 1, ViewGeneration: 1,
		PresentationGeneration: 1, BackendID: "test", Pixels: make([]byte, 16),
	}
	if err := ValidateFrameOutput(frame, state); err != nil {
		t.Fatalf("presented RGBA8 frame rejected: %v", err)
	}
	frame.Format = FrameFormatGrayF32TopLeft
	if err := ValidateFrameOutput(frame, state); !errors.Is(err, ErrInvalidFrameOutput) {
		t.Fatalf("presented GRAY_F32 frame: err = %v", err)
	}
	if err := ValidateFrameOutput(frame, validMPRViewState()); err != nil {
		t.Fatalf("modality GRAY_F32 frame rejected: %v", err)
	}
}

// TestNewMPRLayerPresentationMatchesTheComposite converts a fusion layer as
// FusedScalarLayers.Composite presents it: the window normalized, MONOCHROME1
// inverted, the palette packed; VOI LUT windows are not presentable.
func TestNewMPRLayerPresentationMatchesTheComposite(t *testing.T) {
	layer, err := NewMPRLayerPresentation(FusionLayerPresentation{
		Window: WindowLevel{Center: 10, Width: -1}, Alpha: 1.5, Bands: 4, TransparentBelow: true,
		Palette: display.HotMetalBlueLUT(),
	}, "MONOCHROME1")
	if err != nil {
		t.Fatal(err)
	}
	want := normalizeWindow(WindowLevel{Center: 10, Width: -1}, defaultScalarVOI())
	if layer.WindowCenter != want.Center || layer.WindowWidth != want.Width || layer.Invert != 1 ||
		layer.Alpha != 1 || layer.Bands != 4 || layer.TransparentBelow != 1 {
		t.Fatalf("layer = %+v", layer)
	}
	palette := display.HotMetalBlueLUT()
	for _, index := range []int{0, 17, 128, 255} {
		r, g, b := palette.At(index)
		if layer.Palette[index] != uint32(r)|uint32(g)<<8|uint32(b)<<16 {
			t.Fatalf("palette[%d] = %#x", index, layer.Palette[index])
		}
	}
	if _, err := NewMPRLayerPresentation(FusionLayerPresentation{Window: WindowLevel{LUT: &display.LUT{}}}, ""); !errors.Is(err, ErrMPRPresentationUnsupported) {
		t.Fatalf("VOI LUT window: err = %v", err)
	}
	fusion := NewMPRFusionComposite(FusionCompositeMode{Kind: FusionDifference}, layer)
	if fusion.CheckerSizePx != DefaultFusionCheckerSizePx || fusion.SwipePosition != 0.5 ||
		fusion.DifferenceRange != layer.WindowWidth || len(fusion.DifferencePalette) != MPRPresentationPaletteEntries {
		t.Fatalf("fusion defaults = %+v", fusion)
	}
}
