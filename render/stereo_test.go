package render

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

func TestStereoEyeGeometrySymmetryConvergenceZeroAndResize(t *testing.T) {
	center := validVRViewState()
	center.VR.OpacityUnitDistanceMM = 1
	center.VR.BackgroundRGBA = [4]float64{0, 0, 0, 1}
	center.ContractVersion = ViewStateContractVersionV16
	center.StructSize = ViewStateHeaderSizeV16
	center.OutputWidth = 801
	center.OutputHeight = 600
	center.VR.PositionLPS = [3]float64{10, -100, 20}
	center.VR.FocalPointLPS = [3]float64{10, 0, 20}
	center.VR.ViewUpLPS = [3]float64{0, 0, 1}
	center.VR.ImageSampleDistance = 1
	center.VR.Stereo = StereoPresentationState{Mode: StereoSideBySide, EyeSeparationMM: 6, ConvergenceMM: 100}
	before := center
	eyes, err := DeriveStereoEyeViews(center)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(center, before) {
		t.Fatal("central state mutated")
	}
	if eyes.Left.VR.PositionLPS != [3]float64{7, -100, 20} || eyes.Right.VR.PositionLPS != [3]float64{13, -100, 20} || eyes.ConvergenceLPS != [3]float64{10, 0, 20} {
		t.Fatal(eyes)
	}
	if eyes.Left.OutputWidth != 401 || eyes.Right.OutputWidth != 401 || eyes.Left.OutputHeight != 600 {
		t.Fatal("odd/HiDPI width plan", eyes)
	}
	for _, eye := range []ViewState{eyes.Left, eyes.Right} {
		if eye.VR.FocalPointLPS != eyes.ConvergenceLPS || eye.VolumeGeneration != center.VolumeGeneration || eye.ViewGeneration != center.ViewGeneration || eye.VR.Stereo.Mode != StereoMono {
			t.Fatal("generation/convergence mismatch")
		}
	}
	center.VR.Stereo.Mode = StereoRedCyan
	center.VR.Stereo.EyeSeparationMM = 0
	zero, err := DeriveStereoEyeViews(center)
	if err != nil {
		t.Fatal(err)
	}
	expected := center
	expected.VR.Stereo = StereoPresentationState{}
	if !reflect.DeepEqual(zero.Left, expected) || !reflect.DeepEqual(zero.Right, expected) {
		t.Fatal("zero separation changed central view")
	}
	center.OutputWidth = 1602
	center.OutputHeight = 1200
	center.VR.Stereo.Mode = StereoSideBySide
	doubled, err := DeriveStereoEyeViews(center)
	if err != nil || doubled.Left.OutputWidth != 801 || doubled.Left.OutputHeight != 1200 {
		t.Fatal("HiDPI plan", err)
	}
	center.VR.Stereo = StereoPresentationState{}
	data, _ := json.Marshal(center)
	if bytes.Contains(data, []byte(`"Stereo"`)) {
		t.Fatal("zero stereo changed legacy JSON")
	}
}
func TestStereoRejectsInvalidOrIncompatibleInputs(t *testing.T) {
	for _, s := range []StereoPresentationState{{Mode: 3}, {Mode: StereoRedCyan, ConvergenceMM: 0}, {Mode: StereoRedCyan, ConvergenceMM: 100, EyeSeparationMM: 101}, {Mode: StereoSideBySide, ConvergenceMM: 100, EyeSeparationMM: -1}, {Mode: StereoSideBySide, ConvergenceMM: math.Inf(1)}, {Mode: StereoSideBySide, ConvergenceMM: 100, EyeSeparationMM: math.NaN()}} {
		if s.Validate() == nil {
			t.Fatal("invalid configuration accepted", s)
		}
	}
	state := validVRViewState()
	state.VR.OpacityUnitDistanceMM = 1
	state.VR.BackgroundRGBA = [4]float64{0, 0, 0, 1}
	state.VR.Stereo = StereoPresentationState{Mode: StereoRedCyan, EyeSeparationMM: 6, ConvergenceMM: 100}
	if ValidateViewState(state) == nil {
		t.Fatal("old worker contract accepted stereo")
	}
	state.ContractVersion = ViewStateContractVersionV16
	state.StructSize = ViewStateHeaderSizeV16
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var restored ViewState
	if err := json.Unmarshal(data, &restored); err != nil || restored.VR.Stereo != state.VR.Stereo {
		t.Fatal("stereo JSON", err)
	}
	state.Kind = ViewKindMPR
	if ValidateViewState(state) == nil {
		t.Fatal("MPR stereo accepted")
	}
}
