package render

import (
	"context"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/imagefilter"
)

// Render-only vessel enhancement for angiography MIP and VR (#1099). The
// multiscale Frangi vesselness of the modality values (imagefilter.Vesselness:
// bright tubes, α = β = 0.5, no structureness weight and no |λ3| scaling, so
// the measure is a dimensionless shape score between 0 and 1 − e⁻² ≈ 0.86)
// becomes a weight w per voxel by a linear ramp, and each voxel is faded
// towards the volume minimum by 1 − w:
//
//	w = clamp((V − VesselEnhanceRampLow) / (VesselEnhanceRampHigh − VesselEnhanceRampLow), 0, 1)
//	out = floor + (value − floor)·w
//
// Contrast-filled vessels keep their modality values (so HU transfer
// functions and windows still apply) out to about their radius, while plates
// (cortical bone, skull: V ≈ 0), the centres of blobs (bone, organs: V ≈ 0.16
// with β = 0.5) and flat regions fade to the floor (air in CT). Because the
// output never exceeds the source value, tube-like noise in soft tissue can
// only keep part of its own low value, which angiography transfer functions
// and windows leave dark. Tubular bone such as ribs, and the shell a sphere's
// Hessian shows at intermediate radii, still respond: the enhancement
// separates vessels from most bone but is no segmentation, and its output is
// a display derivation that must not be measured. Dark vessels (black-blood
// MR) are not enhanced.

// Vessel enhancement parameters.
const (
	// VesselEnhanceMinSigmaMM and VesselEnhanceMaxSigmaMM bound the scales:
	// solid vessels answer best at σ = r/√2, so the range covers radii from
	// about 1 mm (coronary and cerebral arteries) to 11 mm (the aorta); the
	// smallest scale is raised to the finest voxel spacing.
	VesselEnhanceMinSigmaMM = 0.75
	VesselEnhanceMaxSigmaMM = 8
	// VesselEnhanceSigmaSteps is the number of logarithmic scales.
	VesselEnhanceSigmaSteps = 5
	// VesselEnhanceRampLow and VesselEnhanceRampHigh map the vesselness to
	// the weight: 0 at or below Low, 1 at or above High.
	VesselEnhanceRampLow  = 0.2
	VesselEnhanceRampHigh = 0.6
	// VesselEnhanceWorkingBytes bounds the slab buffers of the vesselness
	// computation, beyond the source copy and the output.
	VesselEnhanceWorkingBytes = 384 << 20
)

// VesselEnhanceSigmasMM returns the scales VesselEnhance evaluates for a
// voxel spacing (mm).
func VesselEnhanceSigmasMM(spacingMM [3]float64) []float64 {
	finest := math.Inf(1)
	for _, s := range spacingMM {
		if s > 0 {
			finest = math.Min(finest, s)
		}
	}
	minimum := VesselEnhanceMinSigmaMM
	if !math.IsInf(finest, 1) {
		minimum = math.Max(minimum, finest)
	}
	if minimum >= VesselEnhanceMaxSigmaMM {
		return []float64{VesselEnhanceMaxSigmaMM}
	}
	sigmas, err := imagefilter.VesselnessSigmas(minimum, VesselEnhanceMaxSigmaMM, VesselEnhanceSigmaSteps, false)
	if err != nil {
		return []float64{VesselEnhanceMaxSigmaMM}
	}
	return sigmas
}

// VesselEnhanceWeight maps a Frangi vesselness to the weight VesselEnhance
// applies.
func VesselEnhanceWeight(vesselness float64) float64 {
	return math.Max(0, math.Min(1, (vesselness-VesselEnhanceRampLow)/(VesselEnhanceRampHigh-VesselEnhanceRampLow)))
}

// VesselEnhance returns the vessel-enhanced copy of an x-fastest volume of
// modality values with dims voxels and the given spacing (mm). values is
// not modified. A 2-D image (dims[2] == 1) uses the two-dimensional Frangi
// objectness.
func VesselEnhance(ctx context.Context, values []float32, dims [3]int, spacingMM [3]float64) ([]float32, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := checkPrefilterInput(values, dims); err != nil {
		return nil, err
	}
	for axis, s := range spacingMM {
		if dims[axis] > 1 && !(s > 0 && s <= math.MaxFloat64) {
			return nil, fmt.Errorf("render: invalid voxel spacing %v mm", s)
		}
	}
	src := &imagefilter.Volume{Dims: dims, SpacingMM: spacingMM, Data: values}
	res, err := imagefilter.Vesselness(ctx, src, imagefilter.VesselnessOptions{
		Measure:         imagefilter.VesselnessFrangi,
		Structure:       imagefilter.StructureTube,
		SigmasMM:        VesselEnhanceSigmasMM(spacingMM),
		Gamma:           -1,
		MaxWorkingBytes: VesselEnhanceWorkingBytes,
	})
	if err != nil {
		return nil, err
	}
	out := res.Response.Data
	floor := values[0]
	for _, v := range values {
		floor = min(floor, v)
	}
	plane := dims[0] * dims[1]
	err = parallelRowsContext(ctx, dims[2], func(z int) {
		for i := z * plane; i < (z+1)*plane; i++ {
			w := float32(VesselEnhanceWeight(float64(out[i])))
			out[i] = floor + (values[i]-floor)*w
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
