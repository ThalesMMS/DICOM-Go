package render

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"
)

func TestMPRBinShrinkDescriptorGeometryAndConservativeSelection(t *testing.T) {
	source := testVolumeDescriptor(64, 32, 16)
	// Rotated anisotropic grid with a nonzero patient origin.
	source.SpacingMM = [3]float64{1, 2, 3}
	source.IndexToPatientLPS = GeometryAffine{0, -2, 0, 10, 1, 0, 0, 20, 0, 0, 3, 30, 0, 0, 0, 1}
	source.PatientLPSToIndex = GeometryAffine{0, 1, 0, -20, -0.5, 0, 0, 5, 0, 0, 1.0 / 3, -10, 0, 0, 0, 1}
	state := validMPRViewState()
	state.ContractVersion = ViewStateContractVersionV15
	state.StructSize = ViewStateHeaderSizeV15
	state.MPR.Antialias2x = true
	state.MPR.PixelStepULPS = [3]float64{6, 0, 0}
	state.MPR.PixelStepVLPS = [3]float64{0, 6, 0}
	target, ok := MPRBinShrink2xDescriptor(source, state)
	if !ok {
		t.Fatal("eligible plan refused")
	}
	if err := ValidateVolumeDescriptor(target); err != nil {
		t.Fatal(err)
	}
	// Centre of the first 2x2x2 block, computed from eight original corners.
	want := [3]float64{9, 20.5, 31.5}
	for row := 0; row < 3; row++ {
		if math.Abs(target.IndexToPatientLPS[row*4+3]-want[row]) > 1e-12 {
			t.Fatal("block centre changed", target)
		}
	}
	if target.Dimensions != [3]uint32{32, 16, 8} || target.ByteLength != 32*16*8*4 {
		t.Fatal(target)
	}
	near := state
	near.MPR.PixelStepVLPS = [3]float64{0, 5.999, 0}
	if _, ok := MPRBinShrink2xDescriptor(source, near); ok {
		t.Fatal("one undersampled axis accepted")
	}
	odd := testVolumeDescriptor(63, 32, 16)
	if _, ok := MPRBinShrink2xDescriptor(odd, state); ok {
		t.Fatal("odd tail discarded")
	}
	data, err := json.Marshal(state)
	if err != nil || !bytes.Contains(data, []byte(`"Antialias2x":true`)) {
		t.Fatal("flag not serialized", err)
	}
	var decoded ViewState
	if err := json.Unmarshal(data, &decoded); err != nil || !decoded.MPR.Antialias2x || ValidateViewState(decoded) != nil {
		t.Fatal("V15 round trip", err)
	}
	old := validMPRViewState()
	data, _ = json.Marshal(old)
	if bytes.Contains(data, []byte("Antialias2x")) {
		t.Fatal("old JSON contract changed")
	}
	for _, mutation := range []func(*ViewState){func(s *ViewState) {
		s.ContractVersion = ViewStateContractVersionV14
		s.StructSize = ViewStateHeaderSizeV14
	}, func(s *ViewState) { s.MPR.Interpolation = InterpolationNearest }, func(s *ViewState) { s.MPR.SlabMode = SlabMIP }, func(s *ViewState) { s.Kind = ViewKindVR }} {
		bad := state
		mutation(&bad)
		if ValidateViewState(bad) == nil {
			t.Fatal("unsupported antialias contract accepted")
		}
	}
}
