package render

import (
	"fmt"
	"math"
)

const MaxVolumeRegularizationSlices = 65536

// VolumeRegularizationPlan is metadata only. Source voxels form an acquired
// index lattice, not a patient-affine regular grid. Each target plane supplies
// its source index origin; columns/rows advance by the shared index steps.
// Backends must perform interpolation and output generation themselves.
type VolumeRegularizationPlan struct {
	SourceDimensions    [3]uint32
	Target              RegularGridGeometry
	ColumnStep, RowStep Vec3
	SliceOrigins        []Vec3
}

// PlanVolumeRegularization derives the same coordinate mapping used by the
// headless regularizer without decoding, leasing or sampling any pixel data.
func PlanVolumeRegularization(geometry VolumeGeometry) (VolumeRegularizationPlan, error) {
	if geometry.Disposition == GeometryUnsupported || len(geometry.Slices) == 0 || len(geometry.Slices) > MaxVolumeRegularizationSlices {
		return VolumeRegularizationPlan{}, fmt.Errorf("%w: unsupported acquired geometry", ErrNonCorrectableGeometry)
	}
	grid, err := PlanRegularGridGeometry(geometry)
	if err != nil {
		return VolumeRegularizationPlan{}, err
	}
	if grid.Dimensions[2] > MaxVolumeRegularizationSlices {
		return VolumeRegularizationPlan{}, fmt.Errorf("%w: regularization plane bound", ErrInvalidVolumeSnapshot)
	}
	first := geometry.Slices[0]
	source := Volume{Cols: first.Columns, Rows: first.Rows, Depth: len(geometry.Slices), Origin: first.Origin, AxisX: geometry.RowDir, AxisY: geometry.ColDir, Normal: geometry.Normal, ColSpacing: first.ColSpacing, RowSpacing: first.RowSpacing, SliceSpacing: geometry.MeanSpacing, tilt: GantryTiltCorrection{Applied: geometry.GantryTilted}}
	source.positions = make([]float64, source.Depth)
	source.sliceOrigins = make([]Vec3, source.Depth)
	for z, slice := range geometry.Slices {
		if slice.Columns != source.Cols || slice.Rows != source.Rows {
			return VolumeRegularizationPlan{}, fmt.Errorf("%w: differing acquired grids", ErrNonCorrectableGeometry)
		}
		source.positions[z] = slice.Origin.Sub(first.Origin).Dot(geometry.Normal)
		source.sliceOrigins[z] = slice.Origin
		if !finiteBounds(source.positions[z]) || (z > 0 && source.positions[z] <= source.positions[z-1]) {
			return VolumeRegularizationPlan{}, fmt.Errorf("%w: non-increasing acquired positions", ErrNonCorrectableGeometry)
		}
	}
	origin := source.PatientToVoxel(grid.Origin)
	plan := VolumeRegularizationPlan{SourceDimensions: [3]uint32{uint32(source.Cols), uint32(source.Rows), uint32(source.Depth)}, Target: grid, ColumnStep: source.PatientToVoxel(grid.Origin.Add(source.AxisX.Scale(source.ColSpacing))).Sub(origin), RowStep: source.PatientToVoxel(grid.Origin.Add(source.AxisY.Scale(source.RowSpacing))).Sub(origin), SliceOrigins: make([]Vec3, grid.Dimensions[2])}
	for z := range plan.SliceOrigins {
		plan.SliceOrigins[z] = source.PatientToVoxel(grid.Origin.Add(source.Normal.Scale(float64(z) * grid.SliceSpacing)))
	}
	if err := plan.Validate(); err != nil {
		return VolumeRegularizationPlan{}, err
	}
	return plan, nil
}

func (p VolumeRegularizationPlan) Validate() error {
	invalid := func() error { return fmt.Errorf("%w: invalid regularization plan", ErrInvalidVolumeSnapshot) }
	if len(p.SliceOrigins) == 0 || len(p.SliceOrigins) > MaxVolumeRegularizationSlices || len(p.SliceOrigins) != p.Target.Dimensions[2] {
		return invalid()
	}
	for axis, n := range p.SourceDimensions {
		if n == 0 || (axis < 2 && n > math.MaxUint16) || (axis == 2 && n > MaxVolumeRegularizationSlices) {
			return invalid()
		}
	}
	for axis, n := range p.Target.Dimensions {
		if n <= 0 || (axis < 2 && n > math.MaxUint16) || (axis == 2 && n > MaxVolumeRegularizationSlices) {
			return invalid()
		}
	}
	for _, point := range append([]Vec3{p.ColumnStep, p.RowStep}, p.SliceOrigins...) {
		if !finiteBounds(point.X, point.Y, point.Z) {
			return invalid()
		}
	}
	if p.ColumnStep.Length() == 0 || p.RowStep.Length() == 0 {
		return invalid()
	}
	_, err := p.Descriptor(2, 1)
	return err
}

// Descriptor identifies a backend-created modality generation; it contains no
// payload and does not create a VolumeStore record or derive pixels on the CPU.
func (p VolumeRegularizationPlan) Descriptor(generation, parent uint64) (VolumeDescriptor, error) {
	dims := p.Target.Dimensions
	if generation == 0 || parent == 0 || generation == parent || dims[0] <= 0 || dims[1] <= 0 || dims[2] <= 0 || dims[0] > math.MaxUint16 || dims[1] > math.MaxUint16 || dims[2] > MaxVolumeRegularizationSlices {
		return VolumeDescriptor{}, ErrInvalidVolumeSnapshot
	}
	row := uint64(dims[0]) * 4
	slice := row * uint64(dims[1])
	d := VolumeDescriptor{ContractVersion: VolumeSnapshotContractVersion, HeaderSize: VolumeSnapshotHeaderSizeV1, VolumeGeneration: generation, ParentGeneration: parent, Derivation: VolumeDerivationRegularized, Dimensions: [3]uint32{uint32(dims[0]), uint32(dims[1]), uint32(dims[2])}, Components: 1, ScalarFormat: VolumeScalarF32ModalityLE, SampleDomain: VolumeSampleDomainModality, RowStrideBytes: row, SliceStrideBytes: slice, ByteLength: slice * uint64(dims[2]), RescaleSlope: 1, SpacingMM: [3]float64{p.Target.ColSpacing, p.Target.RowSpacing, p.Target.SliceSpacing}, IndexToPatientLPS: p.Target.VoxelToPatientAffine, PatientLPSToIndex: p.Target.PatientToVoxelAffine}
	if err := ValidateVolumeDescriptor(d); err != nil {
		return VolumeDescriptor{}, err
	}
	return d, nil
}

func (p VolumeRegularizationPlan) Freeze() (VolumeRegularizationPlan, error) {
	if err := p.Validate(); err != nil {
		return VolumeRegularizationPlan{}, err
	}
	p.SliceOrigins = append([]Vec3(nil), p.SliceOrigins...)
	return p, nil
}
