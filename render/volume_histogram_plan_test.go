package render

import "testing"

func TestVolumeHistogramPlanBoundedLatticeAndPhysicalCovector(t *testing.T) {
	d := VolumeDescriptor{ContractVersion: VolumeSnapshotContractVersion, HeaderSize: VolumeSnapshotHeaderSizeV1, VolumeGeneration: 1, Derivation: VolumeDerivationNormalized, Dimensions: [3]uint32{512, 256, 32}, Components: 1, ScalarFormat: VolumeScalarF32ModalityLE, SampleDomain: VolumeSampleDomainModality, RowStrideBytes: 2048, SliceStrideBytes: 524288, ByteLength: 16777216, RescaleSlope: 1, SpacingMM: [3]float64{.5, 2, 3}, IndexToPatientLPS: GeometryAffine{.5, 0, 0, 0, 0, 2, 0, 0, 0, 0, 3, 0, 0, 0, 0, 1}, PatientLPSToIndex: GeometryAffine{2, 0, 0, 0, 0, .5, 0, 0, 0, 0, 1.0 / 3, 0, 0, 0, 0, 1}}
	p, err := PlanVolumeHistogram(d, 256, 128)
	if err != nil || p.Stride != 2 || p.Lattice != ([3]uint32{256, 128, 16}) || p.Samples != 524288 || p.Voxels != 4194304 || p.Cells != 32768 || p.GradientCovector != ([9]float64{1, 0, 0, 0, .25, 0, 0, 0, 1.0 / 6}) {
		t.Fatal("bounded physical histogram metadata", p, err)
	}
	full, err := PlanVolumeHistogram(d, 256, 0)
	if err != nil || full.Samples != p.Voxels || full.Stride != 1 {
		t.Fatal("1D histogram must count full volume", full, err)
	}
	for _, bins := range [][2]int{{0, 0}, {256, 1}, {4097, 128}, {256, 4097}, {65537, 0}} {
		if _, err := PlanVolumeHistogram(d, bins[0], bins[1]); err == nil {
			t.Fatal("invalid bins accepted", bins)
		}
	}
}
