package render

import (
	"math"
	"testing"
)

func TestVRTransfer2DStateV22OwnsTableAndRejectsMalformedContracts(t *testing.T) {
	table, err := NewVRTransfer2D(2, 2, -100, 100, 50, []RGBA{{A: .1}, {R: 1, A: .2}, {G: 1, A: .3}, {B: 1, A: .4}})
	if err != nil {
		t.Fatal(err)
	}
	s := validVRViewState()
	s.ContractVersion, s.StructSize = ViewStateContractVersionV22, ViewStateHeaderSizeV22
	s.VR.OpacityUnitDistanceMM = 1
	s.VR.BackgroundRGBA = [4]float64{0, 0, 0, 1}
	s.VR.Transfer2D = VRTransfer2DStateFromTable(table)
	frozen, err := FreezeViewState(s)
	if err != nil {
		t.Fatal(err)
	}
	s.VR.Transfer2D.Samples[0].A = .9
	if frozen.VR.Transfer2D.Samples[0].A != .1 {
		t.Fatal("frozen classification aliases caller")
	}
	if bytes, err := frozen.VR.Transfer2D.PayloadBytes(); err != nil || bytes != 64 {
		t.Fatal(bytes, err)
	}
	mpr := validMPRViewState()
	mpr.ContractVersion, mpr.StructSize = ViewStateContractVersionV22, ViewStateHeaderSizeV22
	mpr.VR.Transfer2D = frozen.VR.Transfer2D
	if ValidateViewState(mpr) == nil {
		t.Fatal("MPR admitted a VR classification sidecar")
	}
	for _, mutate := range []func(*ViewState){
		func(v *ViewState) {
			v.ContractVersion, v.StructSize = ViewStateContractVersionV21, ViewStateHeaderSizeV21
		},
		func(v *ViewState) { v.VR.Mode = VRModeMIP },
		func(v *ViewState) { v.VR.Transfer2D.Samples = nil },
		func(v *ViewState) { v.VR.Transfer2D.Width = MaxVRTransfer2DSide + 1 },
		func(v *ViewState) { v.VR.Transfer2D.Height = 1 },
		func(v *ViewState) { v.VR.Transfer2D.IntensityMax = v.VR.Transfer2D.IntensityMin },
		func(v *ViewState) { v.VR.Transfer2D.GradientMax = math.Inf(1) },
	} {
		v := frozen
		mutate(&v)
		if ValidateViewState(v) == nil {
			t.Fatal("malformed 2D contract admitted")
		}
	}
	for _, bad := range []float32{-1, 2, float32(math.NaN()), float32(math.Inf(1))} {
		v, err := FreezeViewState(frozen)
		if err != nil {
			t.Fatal(err)
		}
		v.VR.Transfer2D.Samples[0].R = bad
		if ValidateViewState(v) == nil {
			t.Fatal("invalid table channel admitted", bad)
		}
	}
}
