package render

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// WriteBrickContext streams the voxels of one brick (#1246) as tightly
// packed little-endian rows, x fastest, then y, then z: the stored 16-bit
// samples of an I16_STORED_LE or U16_STORED_LE snapshot when stored16 is
// true, as WriteStored16Context writes them, otherwise float32 modality
// values as WriteModalityF32Context writes them. Its bytes equal the brick's
// voxels cut from those whole-volume streams. brick must lie inside the
// snapshot's dimensions.
func (s VolumeSnapshot) WriteBrickContext(ctx context.Context, dst io.Writer, brick VolumeBrick, stored16 bool) error {
	if dst == nil {
		return fmt.Errorf("%w: nil brick writer", ErrInvalidVolumeSnapshot)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	record, err := s.record()
	if err != nil {
		return err
	}
	descriptor := record.descriptor
	for axis := range brick.Origin {
		end := uint64(brick.Origin[axis]) + uint64(brick.Size[axis])
		if brick.Size[axis] == 0 || end > uint64(descriptor.Dimensions[axis]) {
			return fmt.Errorf("%w: brick %d outside the %v volume", ErrInvalidVolumeBricks, brick.Index, descriptor.Dimensions)
		}
	}
	isStored16 := descriptor.ScalarFormat == VolumeScalarI16StoredLE || descriptor.ScalarFormat == VolumeScalarU16StoredLE
	if stored16 && !isStored16 {
		return fmt.Errorf("%w: %s is not a 16-bit stored format", ErrInvalidVolumeSnapshot, descriptor.ScalarFormat)
	}
	if !stored16 {
		if err := ValidateModalityF32Conversion(descriptor); err != nil {
			return err
		}
	}
	scalarSize, ok := descriptor.ScalarFormat.scalarSize()
	if !ok {
		return fmt.Errorf("%w: unknown scalar format", ErrInvalidVolumeSnapshot)
	}
	texelBytes := 4
	if stored16 {
		texelBytes = 2
	}
	width := int(brick.Size[0])
	row := make([]byte, width*texelBytes)
	for z := brick.Origin[2]; z < brick.Origin[2]+brick.Size[2]; z++ {
		for y := brick.Origin[1]; y < brick.Origin[1]+brick.Size[1]; y++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			rowOffset := uint64(z)*descriptor.SliceStrideBytes + uint64(y)*descriptor.RowStrideBytes
			for i := 0; i < width; i++ {
				x := uint64(brick.Origin[0]) + uint64(i)
				offset := rowOffset + x*scalarSize
				if stored16 {
					value, ok := storedSample16(record.payload, offset)
					if !ok {
						return fmt.Errorf("%w: stored sample unavailable at %d,%d,%d", ErrInvalidVolumeSnapshot, x, y, z)
					}
					binary.LittleEndian.PutUint16(row[2*i:], value)
					continue
				}
				value, available := record.payload.modalityAt(offset, descriptor)
				if !available {
					return fmt.Errorf("%w: modality sample unavailable at %d,%d,%d", ErrInvalidVolumeSnapshot, x, y, z)
				}
				value32 := float32(value)
				if descriptor.ScalarFormat != VolumeScalarF32ModalityLE &&
					(math.IsNaN(value) || math.IsInf(value, 0) || math.IsInf(float64(value32), 0)) {
					return fmt.Errorf("%w: modality sample out of finite float32 range at %d,%d,%d", ErrInvalidVolumeSnapshot, x, y, z)
				}
				binary.LittleEndian.PutUint32(row[4*i:], math.Float32bits(value32))
			}
			if err := writeExact(dst, row); err != nil {
				return err
			}
		}
	}
	return nil
}

// storedSample16 is the stored 16-bit sample at a payload byte offset.
func storedSample16(payload volumePayload, offset uint64) (uint16, bool) {
	switch payload := payload.(type) {
	case *packed16VolumePayload:
		if offset%2 != 0 || offset/2 >= uint64(payload.count()) {
			return 0, false
		}
		if payload.signed != nil {
			return uint16(payload.signed[offset/2]), true
		}
		return payload.unsigned[offset/2], true
	case *byteVolumePayload:
		end, ok := checkedAdd64(offset, 2)
		if !ok || end > uint64(len(payload.data)) {
			return 0, false
		}
		return binary.LittleEndian.Uint16(payload.data[offset:end]), true
	default:
		return 0, false
	}
}
