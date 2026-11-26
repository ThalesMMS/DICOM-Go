package render

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"
)

func TestVolumeGaussianPlanContractAndFiniteBounds(t *testing.T) {
	d := testVolumeDescriptor(16, 16, 16)
	filter := VolumeFilterState{Mode: VolumeFilterGaussian, SigmaMM: 1}
	plan, err := PlanVolumeGaussian(d, filter)
	if err != nil {
		t.Fatal(err)
	}
	for axis := 0; axis < 3; axis++ {
		if plan.Radius[axis] != 3 {
			t.Fatal("physical radius", plan.Radius)
		}
		sum := 0.0
		for _, w := range plan.Weights[axis] {
			sum += float64(w)
		}
		if math.Abs(sum-1) > 1e-7 {
			t.Fatal("kernel normalization", sum)
		}
	}
	state := validMPRViewState()
	legacy, _ := json.Marshal(state)
	if bytes.Contains(legacy, []byte(`"VolumeFilter"`)) {
		t.Fatal("legacy JSON changed")
	}
	state.VolumeFilter = filter
	if ValidateViewState(state) == nil {
		t.Fatal("old contract accepted filter")
	}
	state.ContractVersion = ViewStateContractVersionV17
	state.StructSize = ViewStateHeaderSizeV17
	if err := ValidateViewState(state); err != nil {
		t.Fatal(err)
	}
	basic := VolumeFilterState{Mode: VolumeFilterBasicSmooth5x5}
	if err := basic.Validate(); err != nil {
		t.Fatal(err)
	}
	kernel := VolumeBasicSmoothKernel()
	sum := float32(0)
	for _, row := range kernel {
		for _, value := range row {
			sum += value
		}
	}
	if sum != 60 || kernel[2][2] != 12 {
		t.Fatal("BasicSmooth kernel contract changed", sum)
	}
	for _, bad := range []VolumeFilterState{{Mode: 5}, {Mode: VolumeFilterBasicSmooth5x5, SigmaMM: 1}, {Mode: VolumeFilterGaussian}, {Mode: VolumeFilterGaussian, SigmaMM: math.NaN()}, {Mode: VolumeFilterGaussian, SigmaMM: math.Inf(1)}, {Mode: VolumeFilterNone, SigmaMM: 1}} {
		if bad.Validate() == nil {
			t.Fatal("invalid state", bad)
		}
	}
	d = testVolumeDescriptor(256, 256, 256)
	if _, err := PlanVolumeGaussian(d, VolumeFilterState{Mode: VolumeFilterGaussian, SigmaMM: 100}); err == nil {
		t.Fatal("unbounded kernel admitted")
	}
}

func TestVesselVolumeFilterRequiresExplicitV24(t *testing.T) {
	s := validMPRViewState()
	s.VolumeFilter = VolumeFilterState{Mode: VolumeFilterVesselEnhance}
	for _, version := range []uint32{ViewStateContractVersionV17, ViewStateContractVersionV20, ViewStateContractVersionV23} {
		s.ContractVersion, s.StructSize = version, ViewStateHeaderSizeV24
		if ValidateViewState(s) == nil {
			t.Fatal("legacy contract admitted vessel enhancement", version)
		}
	}
	s.ContractVersion, s.StructSize = ViewStateContractVersionV24, ViewStateHeaderSizeV24
	if err := ValidateViewState(s); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ViewState
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.VolumeFilter != s.VolumeFilter || ValidateViewState(decoded) != nil {
		t.Fatal("vessel state did not round-trip", err)
	}
	s.VolumeFilter.SigmaMM = 1
	if ValidateViewState(s) == nil {
		t.Fatal("vessel enhancement accepted irrelevant sigma")
	}
}

func TestMedianVolumeFilterRequiresExplicitV20(t *testing.T) {
	s := validMPRViewState()
	s.VolumeFilter = VolumeFilterState{Mode: VolumeFilterMedian3x3x3}
	s.ContractVersion, s.StructSize = ViewStateContractVersionV19, ViewStateHeaderSizeV19
	if ValidateViewState(s) == nil {
		t.Fatal("legacy worker admitted median")
	}
	s.ContractVersion, s.StructSize = ViewStateContractVersionV20, ViewStateHeaderSizeV20
	if err := ValidateViewState(s); err != nil {
		t.Fatal(err)
	}
	s.VolumeFilter.SigmaMM = 1
	if ValidateViewState(s) == nil {
		t.Fatal("median accepted irrelevant sigma")
	}
}
