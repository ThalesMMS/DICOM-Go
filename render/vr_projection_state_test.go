package render

import (
	"math"
	"testing"
)

func TestVRProjectionV21RequiresPhysicalReferenceAndValidRange(t *testing.T) {
	s := validVRViewState()
	s.VR.OpacityUnitDistanceMM = 1
	s.VR.BackgroundRGBA = [4]float64{0, 0, 0, 1}
	s.ContractVersion, s.StructSize = ViewStateContractVersionV21, ViewStateHeaderSizeV21
	s.VR.Mode = VRModeRaySum
	s.VR.RaySumReferenceMM = 12
	s.VR.ProjectionRange = VRValueRange{Enabled: true, Low: -100, High: 500}
	if err := ValidateViewState(s); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		v := s
		v.VR.RaySumReferenceMM = bad
		if ValidateViewState(v) == nil {
			t.Fatal("invalid physical reference admitted", bad)
		}
	}
	v := s
	v.ContractVersion, v.StructSize = ViewStateContractVersionV20, ViewStateHeaderSizeV20
	if ValidateViewState(v) == nil {
		t.Fatal("Ray Sum sent to legacy contract")
	}
	v = s
	v.VR.ProjectionRange.Low = 501
	if ValidateViewState(v) == nil {
		t.Fatal("reversed range admitted")
	}
	v = s
	v.VR.Mode = VRModeAverage
	v.VR.RaySumReferenceMM = 0
	if err := ValidateViewState(v); err != nil {
		t.Fatal("ranged Mean refused", err)
	}
	v.ContractVersion, v.StructSize = ViewStateContractVersionV20, ViewStateHeaderSizeV20
	if ValidateViewState(v) == nil {
		t.Fatal("ranged Mean sent to legacy contract")
	}
	v = s
	v.VR.Mode = VRModeDVR
	v.VR.RaySumReferenceMM = 0
	if ValidateViewState(v) == nil {
		t.Fatal("DVR admitted projection range")
	}
}
