package render

import "fmt"

// VolumeProgressivePreviewPlan separates unchanged even-slice input from the
// averaged display target. No source samples are read by this planner.
type VolumeProgressivePreviewPlan struct {
	Original, EvenSlices, Preview VolumeDescriptor
	InPlaneFactors                [2]uint32
}

// PlanVolumeProgressivePreview preserves the library's 2x2 in-plane average,
// floor-sized odd borders, even z selection and half-block physical origin.
// A loading descriptor may have generation zero; output keeps that generation.
func PlanVolumeProgressivePreview(d VolumeDescriptor) (VolumeProgressivePreviewPlan, error) {
	check := d
	if check.VolumeGeneration == 0 {
		check.VolumeGeneration = 1
	}
	if err := ValidateVolumeDescriptor(check); err != nil {
		return VolumeProgressivePreviewPlan{}, err
	}
	if d.Components != 1 {
		return VolumeProgressivePreviewPlan{}, fmt.Errorf("%w: progressive preview requires scalar input", ErrInvalidVolumeSnapshot)
	}
	p := VolumeProgressivePreviewPlan{Original: d, EvenSlices: d, InPlaneFactors: [2]uint32{min(2, d.Dimensions[0]), min(2, d.Dimensions[1])}}
	p.EvenSlices.Dimensions[2] = d.Dimensions[2]/2 + d.Dimensions[2]%2
	zFactor := uint32(1)
	if p.EvenSlices.Dimensions[2] > 1 {
		zFactor = 2
	}
	p.EvenSlices.SpacingMM[2] *= float64(zFactor)
	for row := 0; row < 3; row++ {
		p.EvenSlices.IndexToPatientLPS[row*4+2] *= float64(zFactor)
	}
	var ok bool
	p.EvenSlices.PatientLPSToIndex, ok = p.EvenSlices.IndexToPatientLPS.Inverse()
	if !ok {
		return VolumeProgressivePreviewPlan{}, fmt.Errorf("%w: even-slice affine", ErrInvalidVolumeSnapshot)
	}
	bytesPerSample := uint64(4)
	if d.ScalarFormat == VolumeScalarI16StoredLE || d.ScalarFormat == VolumeScalarU16StoredLE {
		bytesPerSample = 2
	}
	p.EvenSlices.RowStrideBytes = uint64(d.Dimensions[0]) * bytesPerSample
	p.EvenSlices.SliceStrideBytes = p.EvenSlices.RowStrideBytes * uint64(d.Dimensions[1])
	p.EvenSlices.ByteLength = p.EvenSlices.SliceStrideBytes * uint64(p.EvenSlices.Dimensions[2])
	p.Preview = p.EvenSlices
	// Like the headless preview, this is a separately owned normalized root.
	// The plan retains the original identity instead of inventing a store child.
	p.Preview.ParentGeneration = 0
	p.Preview.Derivation = VolumeDerivationNormalized
	p.Preview.ScalarFormat, p.Preview.SampleDomain = VolumeScalarF32ModalityLE, VolumeSampleDomainModality
	p.Preview.RescaleSlope, p.Preview.RescaleIntercept = 1, 0
	for axis, factor := range p.InPlaneFactors {
		p.Preview.Dimensions[axis] /= factor
		p.Preview.SpacingMM[axis] *= float64(factor)
		for row := 0; row < 3; row++ {
			p.Preview.IndexToPatientLPS[row*4+3] += d.IndexToPatientLPS[row*4+axis] * float64(factor-1) / 2
			p.Preview.IndexToPatientLPS[row*4+axis] *= float64(factor)
		}
	}
	p.Preview.PatientLPSToIndex, ok = p.Preview.IndexToPatientLPS.Inverse()
	if !ok {
		return VolumeProgressivePreviewPlan{}, fmt.Errorf("%w: preview affine", ErrInvalidVolumeSnapshot)
	}
	p.Preview.RowStrideBytes = uint64(p.Preview.Dimensions[0]) * 4
	if p.Preview.RowStrideBytes > ^uint64(0)/uint64(p.Preview.Dimensions[1]) {
		return VolumeProgressivePreviewPlan{}, fmt.Errorf("%w: preview slice byte count overflows", ErrInvalidVolumeSnapshot)
	}
	p.Preview.SliceStrideBytes = p.Preview.RowStrideBytes * uint64(p.Preview.Dimensions[1])
	if p.Preview.SliceStrideBytes > ^uint64(0)/uint64(p.Preview.Dimensions[2]) {
		return VolumeProgressivePreviewPlan{}, fmt.Errorf("%w: preview byte count overflows", ErrInvalidVolumeSnapshot)
	}
	p.Preview.ByteLength = p.Preview.SliceStrideBytes * uint64(p.Preview.Dimensions[2])
	for _, target := range []VolumeDescriptor{p.EvenSlices, p.Preview} {
		if target.VolumeGeneration == 0 {
			target.VolumeGeneration = 1
		}
		if err := ValidateVolumeDescriptor(target); err != nil {
			return VolumeProgressivePreviewPlan{}, err
		}
	}
	return p, nil
}
