package render

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// Compact 16-bit volumes (#1065). A stack whose slices all decode to 16-bit
// stored samples with one rescale is kept as packed int16 or uint16 stored
// values with the descriptor's RescaleSlope/RescaleIntercept (the STORED
// domain of the snapshot contract) instead of float32 modality values: half
// the memory, and the modality values are computed exactly in float64 when
// sampled. Other stacks keep the float32 payload.

// packed16VolumePayload holds 16-bit stored samples packed in x-fastest
// order: signed for I16_STORED_LE, unsigned for U16_STORED_LE.
type packed16VolumePayload struct {
	signed   []int16
	unsigned []uint16
}

func (p *packed16VolumePayload) count() int {
	if p == nil {
		return 0
	}
	if p.signed != nil {
		return len(p.signed)
	}
	return len(p.unsigned)
}

func (p *packed16VolumePayload) byteLen() uint64 { return uint64(p.count()) * 2 }

func (p *packed16VolumePayload) copyBytes(dst io.Writer) error {
	if p == nil {
		return nil
	}
	buffer := make([]byte, 0, 64<<10)
	flush := func() error {
		err := writeExact(dst, buffer)
		buffer = buffer[:0]
		return err
	}
	for i, n := 0, p.count(); i < n; i++ {
		var value uint16
		if p.signed != nil {
			value = uint16(p.signed[i])
		} else {
			value = p.unsigned[i]
		}
		buffer = binary.LittleEndian.AppendUint16(buffer, value)
		if len(buffer) == cap(buffer) {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

func (p *packed16VolumePayload) modalityAt(offset uint64, descriptor VolumeDescriptor) (float64, bool) {
	if p == nil || offset%2 != 0 {
		return 0, false
	}
	index := offset / 2
	switch {
	case p.signed != nil && descriptor.ScalarFormat == VolumeScalarI16StoredLE && index < uint64(len(p.signed)):
		return float64(p.signed[index])*descriptor.RescaleSlope + descriptor.RescaleIntercept, true
	case p.unsigned != nil && descriptor.ScalarFormat == VolumeScalarU16StoredLE && index < uint64(len(p.unsigned)):
		return float64(p.unsigned[index])*descriptor.RescaleSlope + descriptor.RescaleIntercept, true
	}
	return 0, false
}

// replacePacked16Owned installs packed 16-bit stored samples as a normalized
// generation; descriptor carries the dimensions, geometry and rescale.
func (s *VolumeStore) replacePacked16Owned(descriptor VolumeDescriptor, payload *packed16VolumePayload) (uint64, error) {
	if payload == nil || (payload.signed == nil) == (payload.unsigned == nil) {
		return 0, fmt.Errorf("%w: packed 16-bit payload needs exactly one sample slice", ErrInvalidVolumeSnapshot)
	}
	descriptor.ScalarFormat = VolumeScalarI16StoredLE
	if payload.unsigned != nil {
		descriptor.ScalarFormat = VolumeScalarU16StoredLE
	}
	descriptor.SampleDomain = VolumeSampleDomainStored
	descriptor.Derivation = VolumeDerivationNormalized
	descriptor.ParentGeneration = 0
	descriptor.VolumeGeneration = 0
	descriptor.RowStrideBytes = uint64(descriptor.Dimensions[0]) * 2
	descriptor.SliceStrideBytes = uint64(descriptor.Dimensions[1]) * descriptor.RowStrideBytes
	descriptor.ByteLength = payload.byteLen()
	if err := validateVolumeDescriptor(descriptor, true); err != nil {
		return 0, err
	}
	// Every stored sample must map into finite float32 modality space for
	// GPU uploads; the check validates a complete descriptor.
	check := descriptor
	check.VolumeGeneration = 1
	if err := ValidateModalityF32Conversion(check); err != nil {
		return 0, err
	}
	return s.replaceOwned(descriptor, payload)
}

// packed16Plan is the compact layout of a stack: the rescale shared by every
// slice and whether stored samples are signed. ok is false for stacks that
// must stay float32.
type packed16Plan struct {
	slope, intercept float64
	signed           bool
	ok               bool
}

// planPacked16 accepts grayscale stacks of 16-bit stored samples with one
// finite, non-zero rescale: signed samples, or unsigned ones of up to 16
// stored bits, whose modality values then fit float32 exactly as float64.
func planPacked16(slices []*Frame) packed16Plan {
	if len(slices) == 0 {
		return packed16Plan{}
	}
	var plan packed16Plan
	for index, slice := range slices {
		if slice == nil {
			return packed16Plan{}
		}
		metadata := effectiveFrameMetadata(slice)
		photometric := normalizedPhotometric(metadata.PhotometricInterpretation)
		if metadata.BitsAllocated != 16 || metadata.SamplesPerPixel > 1 ||
			(photometric != "MONOCHROME1" && photometric != "MONOCHROME2") ||
			metadata.BitsStored == 0 || metadata.BitsStored > 16 {
			return packed16Plan{}
		}
		slope, intercept := slice.Rescale.Slope, slice.Rescale.Intercept
		if slope == 0 || math.IsNaN(slope) || math.IsInf(slope, 0) {
			slope = 1
		}
		if math.IsNaN(intercept) || math.IsInf(intercept, 0) {
			intercept = 0
		}
		signed := metadata.PixelRepresentation != 0
		if index == 0 {
			plan = packed16Plan{slope: slope, intercept: intercept, signed: signed, ok: true}
			continue
		}
		if slope != plan.slope || intercept != plan.intercept || signed != plan.signed {
			return packed16Plan{}
		}
	}
	return plan
}

// decodeVolumePacked16Context decodes the stored samples of every slice.
func decodeVolumePacked16Context(ctx context.Context, v *Volume, plan packed16Plan) (*packed16VolumePayload, error) {
	count, ok := checkedVoxelCount(v.Cols, v.Rows, v.Depth)
	if !ok {
		return nil, fmt.Errorf("%w: volume dimensions overflow", ErrInvalidVolumeSnapshot)
	}
	payload := &packed16VolumePayload{}
	if plan.signed {
		payload.signed = make([]int16, count)
	} else {
		payload.unsigned = make([]uint16, count)
	}
	if err := decodeVolumeSlices(ctx, len(v.slices), count, func(index int) error {
		return decodePacked16SliceInto(ctx, v, payload, index)
	}); err != nil {
		return nil, err
	}
	return payload, nil
}
