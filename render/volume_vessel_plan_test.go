package render

import (
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/imagefilter"
)

func TestVolumeVesselPlanPhysicalScalesSingletonsAndDetachedMetadata(t *testing.T) {
	d := testVolumeDescriptor(16, 12, 73)
	d.SpacingMM = [3]float64{.5, 2, 3}
	d.IndexToPatientLPS = GeometryAffine{.5, 0, 0, 0, 0, 2, 0, 0, 0, 0, 3, 0, 0, 0, 0, 1}
	d.PatientLPSToIndex = GeometryAffine{2, 0, 0, 0, 0, .5, 0, 0, 0, 0, 1.0 / 3, 0, 0, 0, 0, 1}
	before := d
	p, err := PlanVolumeVesselEnhancement(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Scales) != 5 || p.Scales[0].SigmaMM != .75 || p.Scales[4].SigmaMM != 8 || p.Scales[0].ZMargin != 2 || p.Scales[4].ZMargin != 16 || p.ImageDimensions != 3 || p.Alpha != .5 || p.Beta != .5 || p.RampLow != .2 || p.RampHigh != .6 {
		t.Fatal("physical display enhancement contract", p)
	}
	// Physical normalization must give sigma/spacing on each derivative axis.
	for axis, spacing := range d.SpacingMM {
		for order := 0; order <= 2; order++ {
			samples := .75 / spacing
			want, err := imagefilter.PlanRecursiveGaussian(samples, order, math.Pow(samples, float64(order)))
			if err != nil || p.Scales[0].Kernels[axis][order] != want {
				t.Fatal("anisotropic Hessian coefficients", axis, order, err)
			}
		}
	}
	p.Scales[0].Kernels[0][0] = imagefilter.RecursiveGaussianKernel{}
	other, err := PlanVolumeVesselEnhancement(d)
	if err != nil || other.Scales[0].Kernels[0][0] == p.Scales[0].Kernels[0][0] || d != before {
		t.Fatal("metadata is shared or input changed", err)
	}
	flat, err := PlanVolumeVesselEnhancement(testVolumeDescriptor(16, 12, 1))
	if err != nil || flat.ImageDimensions != 2 {
		t.Fatal("2D objectness metadata", err)
	}
	for _, scale := range flat.Scales {
		if scale.ZMargin != 0 || scale.Kernels[2] != ([3]imagefilter.RecursiveGaussianKernel{}) {
			t.Fatal("singleton z axis must not acquire derivative kernels")
		}
	}
	if _, err := PlanVolumeVesselEnhancement(VolumeDescriptor{}); err == nil {
		t.Fatal("invalid descriptor acquired coefficients")
	}
}
