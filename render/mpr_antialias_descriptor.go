package render

import "math"

// MPRBinShrink2xDescriptor plans the existing 2x block-mean geometry without
// reading samples. Both pixel axes must undersample the coarsest voxel axis.
// Odd and small axes retain exact sampling, avoiding discarded edge bins.
// The returned descriptor describes a private modality-float32 presentation
// resource, not a new quantitative/source generation.
func MPRBinShrink2xDescriptor(source VolumeDescriptor, view ViewState) (VolumeDescriptor, bool) {
	if ValidateVolumeDescriptor(source) != nil || ValidateViewState(view) != nil {
		return source, false
	}
	if !view.MPR.Antialias2x || view.Kind != ViewKindMPR || view.MPR.SlabMode != SlabNone || view.MPR.Interpolation == InterpolationNearest || view.MPR.Presentation.Mode == MPRPresentationFusion {
		return source, false
	}
	if view.OutputWidth < 2 || view.OutputHeight < 2 {
		return source, false
	}
	for _, n := range source.Dimensions {
		if n < 4 || n%2 != 0 {
			return source, false
		}
	}
	largest := 0.0
	for axis := 0; axis < 3; axis++ {
		a := source.IndexToPatientLPS
		spacing := math.Hypot(math.Hypot(a[axis], a[axis+4]), a[axis+8])
		if spacing <= 0 || math.IsNaN(spacing) || math.IsInf(spacing, 0) {
			return source, false
		}
		largest = math.Max(largest, spacing)
	}
	norm := func(p [3]float64) float64 { return math.Hypot(math.Hypot(p[0], p[1]), p[2]) }
	if norm(view.MPR.PixelStepULPS) < 2*largest || norm(view.MPR.PixelStepVLPS) < 2*largest {
		return source, false
	}
	out := source
	for axis := 0; axis < 3; axis++ {
		out.Dimensions[axis] /= 2
		out.SpacingMM[axis] *= 2
	}
	out.Components = 1
	out.ScalarFormat = VolumeScalarF32ModalityLE
	out.SampleDomain = VolumeSampleDomainModality
	out.RescaleSlope = 1
	out.RescaleIntercept = 0
	out.RowStrideBytes = uint64(out.Dimensions[0]) * 4
	out.SliceStrideBytes = out.RowStrideBytes * uint64(out.Dimensions[1])
	out.ByteLength = out.SliceStrideBytes * uint64(out.Dimensions[2])
	for row := 0; row < 3; row++ {
		i := row * 4
		out.IndexToPatientLPS[i+3] += 0.5 * (source.IndexToPatientLPS[i] + source.IndexToPatientLPS[i+1] + source.IndexToPatientLPS[i+2])
		for col := 0; col < 3; col++ {
			out.IndexToPatientLPS[i+col] *= 2
			out.PatientLPSToIndex[i+col] *= 0.5
		}
		out.PatientLPSToIndex[i+3] = source.PatientLPSToIndex[i+3]*0.5 - 0.25
	}
	return out, true
}
