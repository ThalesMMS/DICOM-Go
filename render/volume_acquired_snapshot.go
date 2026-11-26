package render

import (
	"context"
	"fmt"
	"io"
)

// AcquiredVolumeDescriptor describes decoded samples in acquisition-index
// order. It deliberately has no patient affine: irregular slices must use
// their explicit regularization plan before a backend presents a regular grid.
// Scalar normalization/copying does not perform spatial interpolation.
type AcquiredVolumeDescriptor struct {
	ResidencyKey                                 VolumeResidencyKey
	Dimensions                                   [3]uint32
	ScalarFormat                                 VolumeScalarFormat
	SampleDomain                                 VolumeSampleDomain
	RowStrideBytes, SliceStrideBytes, ByteLength uint64
	RescaleSlope, RescaleIntercept               float64
}

// AcquiredVolumeLease retains the immutable decoded acquisition, not a CPU
// regularized generation. It cannot be converted to a patient-affine snapshot.
type AcquiredVolumeLease struct {
	lease *VolumeLease
	plan  VolumeRegularizationPlan
}

// AcquireAcquiredSnapshotContext decodes and retains the original sorted
// frames without resampling. Call EstimateDirectPreparation before acquisition
// to admit source decode memory. Unsupported metadata and cancellation refuse
// before decode. CPU regularization caches and public canonical snapshots are
// unaffected; GPU consumers must build the plan's target themselves.
func (v *Volume) AcquireAcquiredSnapshotContext(ctx context.Context) (*AcquiredVolumeLease, error) {
	if v == nil {
		return nil, fmt.Errorf("render: nil volume")
	}
	if v.closed.Load() {
		return nil, ErrVolumeStoreClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	plan, err := PlanVolumeRegularization(v.geometry)
	if err != nil {
		return nil, err
	}
	lease, err := v.acquireDirectSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = lease.Release()
		return nil, err
	}
	return &AcquiredVolumeLease{lease: lease, plan: plan}, nil
}

func (l *AcquiredVolumeLease) Release() error {
	if l == nil {
		return nil
	}
	return l.lease.Release()
}

func (l *AcquiredVolumeLease) Close() error { return l.Release() }

// AcquiredVolumeSnapshot is valid only while its acquisition lease is alive.
// Payload methods expose a transport copy; no mutable voxel slice or nominal
// patient affine crosses this API boundary.
type AcquiredVolumeSnapshot struct{ acquired *AcquiredVolumeLease }

func (l *AcquiredVolumeLease) Snapshot() (AcquiredVolumeSnapshot, error) {
	if l == nil || l.lease == nil {
		return AcquiredVolumeSnapshot{}, ErrVolumeLeaseReleased
	}
	if _, err := l.lease.record(); err != nil {
		return AcquiredVolumeSnapshot{}, err
	}
	return AcquiredVolumeSnapshot{acquired: l}, nil
}

func (s AcquiredVolumeSnapshot) source() (VolumeSnapshot, error) {
	if s.acquired == nil || s.acquired.lease == nil {
		return VolumeSnapshot{}, ErrVolumeLeaseReleased
	}
	return s.acquired.lease.Snapshot()
}

func (s AcquiredVolumeSnapshot) Descriptor() (AcquiredVolumeDescriptor, error) {
	source, err := s.source()
	if err != nil {
		return AcquiredVolumeDescriptor{}, err
	}
	d, err := source.Descriptor()
	if err != nil {
		return AcquiredVolumeDescriptor{}, err
	}
	key := source.ResidencyKey()
	if !key.Valid() {
		return AcquiredVolumeDescriptor{}, ErrVolumeLeaseReleased
	}
	return AcquiredVolumeDescriptor{ResidencyKey: key, Dimensions: d.Dimensions, ScalarFormat: d.ScalarFormat, SampleDomain: d.SampleDomain, RowStrideBytes: d.RowStrideBytes, SliceStrideBytes: d.SliceStrideBytes, ByteLength: d.ByteLength, RescaleSlope: d.RescaleSlope, RescaleIntercept: d.RescaleIntercept}, nil
}

// RegularizationPlan returns an owned metadata copy. Mutating it never changes
// this lease or another consumer's acquisition mapping.
func (s AcquiredVolumeSnapshot) RegularizationPlan() (VolumeRegularizationPlan, error) {
	if _, err := s.source(); err != nil {
		return VolumeRegularizationPlan{}, err
	}
	return s.acquired.plan.Freeze()
}

func (s AcquiredVolumeSnapshot) ResidencyKey() VolumeResidencyKey {
	if s.acquired == nil {
		return VolumeResidencyKey{}
	}
	return s.acquired.lease.ResidencyKey()
}

func (s AcquiredVolumeSnapshot) WritePayloadTo(dst io.Writer) error {
	source, err := s.source()
	if err != nil {
		return err
	}
	return source.WritePayloadTo(dst)
}

// WriteModalityF32Context normalizes individual acquired samples for transports
// that cannot retain packed stored values. It never interpolates coordinates.
func (s AcquiredVolumeSnapshot) WriteModalityF32Context(ctx context.Context, dst io.Writer) error {
	source, err := s.source()
	if err != nil {
		return err
	}
	return source.WriteModalityF32Context(ctx, dst)
}
