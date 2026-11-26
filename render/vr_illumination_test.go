package render

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"
)

func TestVRIlluminationV14ContractAndFreeze(t *testing.T) {
	s := validVRViewState()
	s.ContractVersion, s.StructSize = ViewStateContractVersionV14, ViewStateHeaderSizeV14
	s.VR.OpacityUnitDistanceMM = 1
	s.VR.BackgroundRGBA = [4]float64{0, 0, 0, 1}
	s.VR.Illumination = VRVascularIllumination()
	s.VR.Illumination.SpecularLookup = true
	if err := ValidateViewState(s); err != nil {
		t.Fatal(err)
	}
	frozen, err := FreezeViewState(s)
	if err != nil {
		t.Fatal(err)
	}
	s.VR.Illumination.Lights[0].Intensity = 2
	if frozen.VR.Illumination.Lights[0].Intensity != .7 {
		t.Fatal("shared light state")
	}
	wire, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	var round ViewState
	if err := json.Unmarshal(wire, &round); err != nil {
		t.Fatal(err)
	}
	if round.VR.Illumination != frozen.VR.Illumination || ValidateViewState(round) != nil {
		t.Fatal("roundtrip lost illumination")
	}
	for _, mutate := range []func(*ViewState){
		func(s *ViewState) {
			s.ContractVersion, s.StructSize = ViewStateContractVersionV13, ViewStateHeaderSizeV13
		},
		func(s *ViewState) { s.StructSize = ViewStateHeaderSizeV14 - 1 },
		func(s *ViewState) { s.VR.Mode = VRModeMIP },
		func(s *ViewState) { s.VR.Illumination.LightCount = 4 },
		func(s *ViewState) { s.VR.Illumination.Lights[0].DirectionCamera = [3]float64{} },
		func(s *ViewState) { s.VR.Illumination.Lights[0].Intensity = math.NaN() },
		func(s *ViewState) { s.VR.Illumination.LightCount = 1 },
		func(s *ViewState) { s.VR.Illumination.ToneMapping = 9 },
		func(s *ViewState) { s.VR.SpecularPower = .5 },
		func(s *ViewState) { s.VR.SpecularPower = 129 },
	} {
		v := frozen
		mutate(&v)
		if ValidateViewState(v) == nil {
			t.Fatal("invalid illumination accepted", v.VR.Illumination)
		}
	}
	legacy, err := json.Marshal(validVRViewState())
	if err != nil || bytes.Contains(legacy, []byte(`"Illumination"`)) {
		t.Fatal("legacy JSON changed", err)
	}
}

func TestVRSpecularTableAnalyticErrorAndBounds(t *testing.T) {
	for _, power := range []float64{0, 1, 2, 15, 50, 128} {
		table, err := VRSpecularTable(power)
		if err != nil {
			t.Fatal(err)
		}
		var maxError float64
		for i := 0; i <= 100000; i++ {
			x := float64(i) / 100000
			q := x * 4096
			lo := int(q)
			hi := min(lo+1, 4096)
			a := q - float64(lo)
			got := (1-a)*float64(table[lo]) + a*float64(table[hi])
			maxError = max(maxError, math.Abs(got-math.Pow(x, power)))
		}
		if maxError > 0.000122 {
			t.Fatal(power, maxError)
		}
		t.Logf("power %.0f max absolute error %.9g", power, maxError)
	}
	for _, p := range []float64{-.1, .5, 129, math.Inf(1), math.NaN()} {
		if _, e := VRSpecularTable(p); e == nil {
			t.Fatal("bad power", p)
		}
	}
}
