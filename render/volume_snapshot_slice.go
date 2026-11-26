package render

import (
	"context"
	"fmt"
)

// ReadModalitySliceF32 fills dst with slice z in the modality domain as
// float32 samples, x-fastest without row padding: dst[y*Dimensions[0]+x] is
// voxel (x, y, z). dst must hold at least Dimensions[0]*Dimensions[1]
// samples. Stored-value payloads are rescaled; packed float32 payloads are
// copied row by row. Unlike ModalityAt, the lease is validated once per
// slice, so concurrent readers (for example parallel mesh extraction) can
// stream a volume one slice at a time without copying it whole.
//
// It lives beside volume_snapshot.go, whose contents are pinned by the
// Twin-Viewer WebGPU synchronization manifest.
func (s VolumeSnapshot) ReadModalitySliceF32(z uint32, dst []float32) error {
	return s.ReadModalitySliceF32Context(context.Background(), z, dst)
}

// ReadModalitySliceF32Context is the cancellable slice reader. A cancelled
// read may leave a prefix of dst filled; callers must discard that slice.
func (s VolumeSnapshot) ReadModalitySliceF32Context(ctx context.Context, z uint32, dst []float32) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	record, err := s.record()
	if err != nil {
		return err
	}
	descriptor := record.descriptor
	if err := ValidateModalityF32Conversion(descriptor); err != nil {
		return err
	}
	if z >= descriptor.Dimensions[2] {
		return fmt.Errorf("%w: slice %d outside %d slices", ErrInvalidVolumeSnapshot, z, descriptor.Dimensions[2])
	}
	cols, rows := uint64(descriptor.Dimensions[0]), uint64(descriptor.Dimensions[1])
	if plane, ok := checkedMul64(cols, rows); !ok || plane > uint64(len(dst)) {
		return fmt.Errorf("%w: slice buffer of %d samples for %dx%d", ErrInvalidVolumeSnapshot, len(dst), cols, rows)
	}
	scalarSize, ok := descriptor.ScalarFormat.scalarSize()
	if !ok {
		return fmt.Errorf("%w: unknown scalar format", ErrInvalidVolumeSnapshot)
	}
	zOffset := uint64(z) * descriptor.SliceStrideBytes
	for y := uint64(0); y < rows; y++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		row := dst[y*cols : (y+1)*cols]
		rowOffset := zOffset + y*descriptor.RowStrideBytes
		if payload, packed := record.payload.(*float32VolumePayload); packed && rowOffset%4 == 0 {
			start := rowOffset / 4
			if start+cols <= uint64(len(payload.data)) {
				copy(row, payload.data[start:start+cols])
				continue
			}
		}
		for x := uint64(0); x < cols; x++ {
			if x%4096 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			value, available := record.payload.modalityAt(rowOffset+x*scalarSize, descriptor)
			if !available {
				return fmt.Errorf("%w: modality sample unavailable at %d,%d,%d", ErrInvalidVolumeSnapshot, x, y, z)
			}
			row[x] = float32(value)
		}
	}
	return ctx.Err()
}
