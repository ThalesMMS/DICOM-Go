package render

import (
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// VolumePreparationEstimate describes the direct decoded payload before any
// pixels are read. Source buffers and external codec-runtime workspace retain
// their own accounting; TransientBytes allows two expanded scalar planes per
// possible decode worker. It is an admission estimate, not a process RSS bound.
type VolumePreparationEstimate struct {
	ScalarFormat                 VolumeScalarFormat
	PayloadBytes, TransientBytes uint64
	DecodeWorkers                int
	RequiresResampling           bool
}

func (v *Volume) EstimateDirectPreparation() (VolumePreparationEstimate, error) {
	if v == nil || v.closed.Load() {
		return VolumePreparationEstimate{}, ErrVolumeStoreClosed
	}
	count, ok := checkedVoxelCount(v.Cols, v.Rows, v.Depth)
	if !ok {
		return VolumePreparationEstimate{}, fmt.Errorf("render: volume dimensions overflow")
	}
	scalarSize := uint64(4)
	format := VolumeScalarF32ModalityLE
	if plan := planPacked16(v.slices); plan.ok {
		scalarSize = 2
		format = VolumeScalarU16StoredLE
		if plan.signed {
			format = VolumeScalarI16StoredLE
		}
	}
	payload, ok := checkedMul64(uint64(count), scalarSize)
	if !ok {
		return VolumePreparationEstimate{}, fmt.Errorf("render: payload estimate overflow")
	}
	workers := min(v.Depth, parallel.DefaultWorkers())
	if count < 64<<10 {
		workers = 1
	}
	plane, ok := checkedMul64(uint64(v.Cols), uint64(v.Rows))
	if !ok {
		return VolumePreparationEstimate{}, fmt.Errorf("render: plane estimate overflow")
	}
	transient, ok := checkedMul64(plane, uint64(max(1, workers))*8)
	if !ok || payload > math.MaxInt64 || transient > math.MaxInt64-payload {
		return VolumePreparationEstimate{}, fmt.Errorf("render: preparation estimate overflow")
	}
	return VolumePreparationEstimate{ScalarFormat: format, PayloadBytes: payload, TransientBytes: transient, DecodeWorkers: workers, RequiresResampling: v.geometry.RequiresResampling}, nil
}

// OnSnapshotRelease attaches a bookkeeping callback to the prepared canonical
// generation. It runs exactly once outside store locks, after its last lease
// and pin have released and the generation has been removed. It retains no
// extra voxel lease and does not prepare an absent snapshot.
func (v *Volume) OnSnapshotRelease(release func()) error {
	if v == nil || release == nil {
		return fmt.Errorf("render: missing release observer")
	}
	v.snapshotMu.Lock()
	store, generation := v.store, v.generation
	v.snapshotMu.Unlock()
	if store == nil || generation == 0 {
		return ErrVolumeStoreClosed
	}
	store.mu.Lock()
	record := store.records[generation]
	if record == nil {
		store.mu.Unlock()
		return ErrVolumeStoreClosed
	}
	record.onReleased = append(record.onReleased, release)
	store.mu.Unlock()
	return nil
}
