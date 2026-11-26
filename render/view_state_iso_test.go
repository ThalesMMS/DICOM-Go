package render

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

// TestValidateViewStateV11IsoSurfaces (#1228): the iso-surface mode needs
// V11 and one to MaxVRIsoSurfaces surfaces with finite values, display
// colours in 0..1 and opacities in (0, 1]; other modes carry none, and
// FreezeViewState owns the surfaces.
func TestValidateViewStateV11IsoSurfaces(t *testing.T) {
	state := validVRViewState()
	state.ContractVersion, state.StructSize = ViewStateContractVersionV11, ViewStateHeaderSizeV11
	state.VR.OpacityUnitDistanceMM = 1
	state.VR.BackgroundRGBA = [4]float64{0, 0, 0, 1}
	state.VR.Mode = VRModeIsoSurface
	state.VR.IsoSurfaces = []VRIsoSurfaceState{
		{Value: -300, Color: [3]float64{0.9, 0.7, 0.6}, Opacity: 0.4},
		{Value: 300, Color: [3]float64{1, 1, 0.9}, Opacity: 1},
	}
	if err := ValidateViewState(state); err != nil {
		t.Fatalf("V11 iso-surfaces rejected: %v", err)
	}
	for name, mutate := range map[string]func(*ViewState){
		"V10": func(s *ViewState) {
			s.ContractVersion, s.StructSize = ViewStateContractVersionV10, ViewStateHeaderSizeV10
		},
		"header":       func(s *ViewState) { s.StructSize = ViewStateHeaderSizeV10 },
		"no surface":   func(s *ViewState) { s.VR.IsoSurfaces = nil },
		"too many":     func(s *ViewState) { s.VR.IsoSurfaces = make([]VRIsoSurfaceState, MaxVRIsoSurfaces+1) },
		"zero opacity": func(s *ViewState) { s.VR.IsoSurfaces = []VRIsoSurfaceState{{Value: 1, Opacity: 0}} },
		"opacity":      func(s *ViewState) { s.VR.IsoSurfaces = []VRIsoSurfaceState{{Value: 1, Opacity: 1.5}} },
		"colour": func(s *ViewState) {
			s.VR.IsoSurfaces = []VRIsoSurfaceState{{Value: 1, Color: [3]float64{2, 0, 0}, Opacity: 1}}
		},
		"surfaces in DVR": func(s *ViewState) {
			s.VR.Mode = VRModeDVR
		},
	} {
		mutated := state
		mutated.VR.IsoSurfaces = append([]VRIsoSurfaceState(nil), state.VR.IsoSurfaces...)
		mutate(&mutated)
		if err := ValidateViewState(mutated); !errors.Is(err, ErrInvalidViewState) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	frozen, err := FreezeViewState(state)
	if err != nil {
		t.Fatal(err)
	}
	state.VR.IsoSurfaces[0].Value = 1000
	if frozen.VR.IsoSurfaces[0].Value != -300 {
		t.Fatal("FreezeViewState shares the iso-surfaces with the caller")
	}
	if encoded, err := json.Marshal(validVRViewState()); err != nil || bytes.Contains(encoded, []byte(`"IsoSurfaces"`)) {
		t.Fatalf("surface-free view state serializes IsoSurfaces: %s, %v", encoded, err)
	}
}
