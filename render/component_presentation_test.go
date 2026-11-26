package render

import (
	"errors"
	"math"
	"testing"
)

func TestComponentPresentationVersionOwnershipAndChannelMatching(t *testing.T) {
	s := validMPRViewState()
	s.ContractVersion, s.StructSize = ViewStateContractVersionV26, ViewStateHeaderSizeV26
	s.Components = ComponentPresentation{Mode: ComponentPresentationRGB, RGBMaximum: 255, OpacityScale: 1}
	d := testVolumeDescriptor(2, 2, 2)
	d.ContractVersion, d.Components = VolumeSnapshotContractVersionV2, 3
	d.RowStrideBytes, d.SliceStrideBytes, d.ByteLength = 24, 48, 96
	if err := ValidateComponentViewForVolume(d, s); err != nil {
		t.Fatal(err)
	}
	s.ContractVersion, s.StructSize = ViewStateContractVersionV25, ViewStateHeaderSizeV25
	if !errors.Is(ValidateViewState(s), ErrInvalidViewState) {
		t.Fatal("RGB presentation accepted by V25")
	}
	s.ContractVersion, s.StructSize = ViewStateContractVersionV26, ViewStateHeaderSizeV26
	s.MPR.ScalarValidity = true
	if !errors.Is(ValidateViewState(s), ErrInvalidViewState) {
		t.Fatal("color output accepted as quantitative scalar MPR")
	}
	s.MPR.ScalarValidity = false
	s.Components = ComponentPresentation{}
	if !errors.Is(ValidateComponentViewForVolume(d, s), ErrInvalidViewState) {
		t.Fatal("implicit scalar selection accepted")
	}
	lut := validVRViewState().VR.TransferLUT
	s.Components = ComponentPresentation{Mode: ComponentPresentationIndependent, OpacityScale: 1, Transfers: []TransferLUT{lut, lut, lut}, Weights: []float64{1, .5, .25}}
	frozen, err := FreezeViewState(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateComponentViewForVolume(d, frozen); err != nil {
		t.Fatal(err)
	}
	s.Components.Transfers[0].Samples[0].R = .123
	s.Components.Weights[0] = .75
	if frozen.Components.Transfers[0].Samples[0].R == .123 || frozen.Components.Weights[0] != 1 {
		t.Fatal("component request did not own its transfer data")
	}
	frozen.Components.Transfers = frozen.Components.Transfers[:2]
	frozen.Components.Weights = frozen.Components.Weights[:2]
	if !errors.Is(ValidateComponentViewForVolume(d, frozen), ErrInvalidViewState) {
		t.Fatal("transfer count mismatch accepted")
	}
}

func TestComponentPresentationRejectsNonFiniteAndAmbiguousMaterial(t *testing.T) {
	for _, p := range []ComponentPresentation{
		{Mode: ComponentPresentationRGB, RGBMaximum: math.NaN(), OpacityScale: 1},
		{Mode: ComponentPresentationRGB, RGBMaximum: 255, OpacityScale: math.Inf(1)},
		{Mode: ComponentPresentationRGB, RGBMaximum: 255, OpacityScale: 1, Weights: []float64{1}},
		{Mode: ComponentPresentationScalar, RGBMaximum: 255},
		{Mode: ComponentPresentationMode(99), OpacityScale: 1},
		{Mode: ComponentPresentationIndependent, OpacityScale: 1, Transfers: []TransferLUT{{}, {}}, Weights: []float64{1, 1}},
	} {
		if !errors.Is(p.Validate(), ErrInvalidViewState) {
			t.Fatalf("invalid material accepted %+v", p)
		}
	}
}
