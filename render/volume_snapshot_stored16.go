package render

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"unsafe"
)

// WriteStored16Context streams the stored samples of an I16_STORED_LE or
// U16_STORED_LE snapshot as tightly packed little-endian 16-bit rows, x
// fastest, then y, then z; source row and slice padding is not copied. It is
// the upload boundary of GPU backends that keep 16-bit volume textures and
// apply RescaleSlope and RescaleIntercept while sampling (#1226): half the
// bytes of WriteModalityF32Context. Other scalar formats are rejected.
//
// It lives beside volume_snapshot.go, whose contents are pinned by the
// Twin-Viewer WebGPU synchronization manifest.
func (s VolumeSnapshot) WriteStored16Context(ctx context.Context, dst io.Writer) error {
	if dst == nil {
		return fmt.Errorf("%w: nil stored-sample writer", ErrInvalidVolumeSnapshot)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	record, err := s.record()
	if err != nil {
		return err
	}
	descriptor := record.descriptor
	if descriptor.ScalarFormat != VolumeScalarI16StoredLE && descriptor.ScalarFormat != VolumeScalarU16StoredLE {
		return fmt.Errorf("%w: %s is not a 16-bit stored format", ErrInvalidVolumeSnapshot, descriptor.ScalarFormat)
	}
	rowBytes, ok := checkedMul64(uint64(descriptor.Dimensions[0]), 2)
	if !ok || rowBytes > uint64(math.MaxInt) {
		return fmt.Errorf("%w: stored row size overflow", ErrInvalidVolumeSnapshot)
	}
	var row []byte
	var endian uint16 = 1
	littleEndian := *(*byte)(unsafe.Pointer(&endian)) == 1
	cols := int(descriptor.Dimensions[0])
	for z := uint32(0); z < descriptor.Dimensions[2]; z++ {
		zOffset := uint64(z) * descriptor.SliceStrideBytes
		for y := uint32(0); y < descriptor.Dimensions[1]; y++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			sourceOffset := zOffset + uint64(y)*descriptor.RowStrideBytes
			switch payload := record.payload.(type) {
			case *packed16VolumePayload:
				if sourceOffset%2 != 0 || sourceOffset/2+uint64(cols) > uint64(payload.count()) {
					return fmt.Errorf("%w: stored row unavailable at %d,%d", ErrInvalidVolumeSnapshot, y, z)
				}
				base := int(sourceOffset / 2)
				if littleEndian {
					// The lease holds immutable, native-width samples. io.Writer
					// consumes the slice before returning and must not modify it.
					if payload.signed != nil {
						row = unsafe.Slice((*byte)(unsafe.Pointer(&payload.signed[base])), int(rowBytes))
					} else {
						row = unsafe.Slice((*byte)(unsafe.Pointer(&payload.unsigned[base])), int(rowBytes))
					}
					break
				}
				if row == nil {
					row = make([]byte, int(rowBytes))
				}
				for x := 0; x < cols; x++ {
					var value uint16
					if payload.signed != nil {
						value = uint16(payload.signed[base+x])
					} else {
						value = payload.unsigned[base+x]
					}
					binary.LittleEndian.PutUint16(row[2*x:], value)
				}
			case *byteVolumePayload:
				end, ok := checkedAdd64(sourceOffset, rowBytes)
				if !ok || end > uint64(len(payload.data)) {
					return fmt.Errorf("%w: stored row unavailable at %d,%d", ErrInvalidVolumeSnapshot, y, z)
				}
				row = payload.data[sourceOffset:end]
			default:
				return fmt.Errorf("%w: payload holds no 16-bit stored samples", ErrInvalidVolumeSnapshot)
			}
			if err := writeExact(dst, row); err != nil {
				return err
			}
		}
	}
	return nil
}
