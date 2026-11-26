package render

import (
	"math"
	"testing"
)

func validVRFusionState() VRFusionState {
	return VRFusionState{OverlayGeneration: 2, OverlayDimensions: [3]uint32{5, 6, 7}, PatientToOverlayIndex: GeometryAffine{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}, DomainMin: -100, DomainMax: 1000, Opacity: .5, OpacityUnitDistanceMM: 1, SampleCount: 2, Samples: []TransferSample{{R: 1, A: .1}, {B: 1, A: .5}}}
}

func TestVRFusionV23OwnsSecondaryPresentationAndRejectsMalformedRequests(t *testing.T) {
	s := validVRViewState()
	s.ContractVersion, s.StructSize = ViewStateContractVersionV23, ViewStateHeaderSizeV23
	s.VR.OpacityUnitDistanceMM = 1
	s.VR.BackgroundRGBA = [4]float64{0, 0, 0, 1}
	s.VR.Fusion = validVRFusionState()
	frozen, err := FreezeViewState(s)
	if err != nil {
		t.Fatal(err)
	}
	s.VR.Fusion.Samples[0].A = .9
	if frozen.VR.Fusion.Samples[0].A != .1 {
		t.Fatal("frozen fusion table aliases caller")
	}
	if generation, dimensions := frozen.OverlaySource(); generation != 2 || dimensions != [3]uint32{5, 6, 7} {
		t.Fatal("secondary identity omitted")
	}
	for _, change := range []func(*ViewState){
		func(v *ViewState) {
			v.ContractVersion, v.StructSize = ViewStateContractVersionV22, ViewStateHeaderSizeV22
		},
		func(v *ViewState) { v.VR.Mode = VRModeAverage },
		func(v *ViewState) { v.VR.Fusion.OverlayGeneration = 0 },
		func(v *ViewState) { v.VR.Fusion.OverlayDimensions[1] = 0 },
		func(v *ViewState) { v.VR.Fusion.Opacity = math.NaN() },
		func(v *ViewState) { v.VR.Fusion.Opacity = 0 },
		func(v *ViewState) { v.VR.Fusion.Opacity = 1.1 },
		func(v *ViewState) { v.VR.Fusion.OpacityUnitDistanceMM = 0 },
		func(v *ViewState) { v.VR.Fusion.PatientToOverlayIndex[5] = 0 },
		func(v *ViewState) { v.VR.Fusion.SampleCount = MaxTransferLUTSamples + 1 },
		func(v *ViewState) { v.VR.Fusion.Samples = nil },
	} {
		v := frozen
		change(&v)
		if ValidateViewState(v) == nil {
			t.Fatal("malformed fusion admitted")
		}
	}
	mpr := validMPRViewState()
	mpr.ContractVersion, mpr.StructSize = ViewStateContractVersionV23, ViewStateHeaderSizeV23
	mpr.VR.Fusion = frozen.VR.Fusion
	if ValidateViewState(mpr) == nil {
		t.Fatal("MPR carries a VR fusion table")
	}
}
