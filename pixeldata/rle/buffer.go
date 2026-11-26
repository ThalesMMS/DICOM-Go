package rle

import (
	"context"
	"fmt"
	"image"

	"github.com/ThalesMMS/dicom-go/internal/decoderecovery"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

var _ pixeldata.FrameBufferDecoder = (*Codec)(nil)

// BufferMetadata advertises interleaved little-endian output for stored regions.
func (*Codec) BufferMetadata(ctx context.Context, m pixeldata.Metadata, request pixeldata.FrameBufferRequest) (pixeldata.Metadata, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.Metadata{}, err
	}
	region := request.Region
	if request.WholeFrame && region != image.Rect(0, 0, int(m.Columns), int(m.Rows)) {
		return pixeldata.Metadata{}, pixeldata.ErrInvalidRegion
	}
	if err := validateDecodeMetadata(m); err != nil {
		return pixeldata.Metadata{}, err
	}
	if region.Empty() || !region.In(image.Rect(0, 0, int(m.Columns), int(m.Rows))) {
		return pixeldata.Metadata{}, pixeldata.ErrInvalidRegion
	}
	m.Rows, m.Columns, m.NumberOfFrames = uint16(region.Dy()), uint16(region.Dx()), 1
	if m.SamplesPerPixel == 3 {
		m.PlanarConfiguration, m.PlanarConfigurationPresent = 0, true
	}
	return m, nil
}

// DecodeFrameInto borrows both inputs for this call. Output goes directly into
// dst. Whole-frame output uses existing unpackBits with no byte-band scratch.
// Region output uses one byte-band (width of stored rows, height of region) that is
// reused across segments. It never allocates a decoded whole-frame output.
func (c *Codec) DecodeFrameInto(ctx context.Context, codestream []byte, m pixeldata.Metadata, request pixeldata.FrameBufferRequest, dst []byte) (int, []string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	region := request.Region
	outMetadata, err := c.BufferMetadata(ctx, m, request)
	if err != nil {
		return 0, nil, err
	}
	required := outMetadata.FrameSize()
	if int64(len(dst)) < required {
		return 0, nil, &pixeldata.BufferSizeError{Required: required, Provided: int64(len(dst))}
	}
	dst = dst[:int(required):int(required)]
	samples, bytesPerSample := int(m.SamplesPerPixel), m.BytesPerSample()
	segments := samples * bytesPerSample
	offsets, err := parseHeader(codestream)
	if err != nil {
		return 0, nil, err
	}
	if len(offsets)-1 != segments {
		return 0, nil, ErrInvalidSegmentCount
	}
	cols := int(m.Columns)
	skip, stop := region.Min.Y*cols, region.Max.Y*cols
	tolerant := decoderecovery.From(ctx).RLESegmentOverrun
	for seg := 0; seg < segments; seg++ {
		start, end := int(offsets[seg]), int(offsets[seg+1])
		if end < start || end > len(codestream) {
			return 0, nil, ErrInvalidSegmentOffset
		}
		if !tolerant && !canFill(end-start, stop) {
			return 0, nil, ErrSegmentDecodeFailed
		}
	}
	if request.WholeFrame {
		// unpackBits already implements the full-frame strict/recovery contract.
		// Its tolerated short segments require zero-filled destination lanes.
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		clear(dst)
		recovered := false
		step := samples * bytesPerSample
		for seg := 0; seg < segments; seg++ {
			first := (seg/bytesPerSample)*bytesPerSample + bytesPerSample - 1 - seg%bytesPerSample
			if decoderecovery.From(ctx).RLEReversedByteSegments {
				first = (seg/bytesPerSample)*bytesPerSample + seg%bytesPerSample
			}
			healed, err := unpackBits(ctx, codestream[int(offsets[seg]):int(offsets[seg+1])], dst, first, step, int(m.Rows)*cols, tolerant)
			if err != nil {
				return len(dst), nil, err
			}
			recovered = recovered || healed
		}
		var diagnostics []string
		if recovered {
			diagnostics = []string{pixeldata.DiagnosticRLESegmentOverrun}
		}
		if decoderecovery.From(ctx).RLEReversedByteSegments && bytesPerSample > 1 {
			diagnostics = append(diagnostics, pixeldata.DiagnosticRLEReversedByteSegments)
		}
		return len(dst), diagnostics, ctx.Err()
	}
	band := make([]byte, 0, stop-skip)
	highwater := 0
	recovered := false
	step := samples * bytesPerSample
	for seg := 0; seg < segments; seg++ {
		start, end := int(offsets[seg]), int(offsets[seg+1])
		decoded, healed, err := decodePackBitsRange(ctx, codestream[start:end], skip, stop, tolerant, band)
		if err != nil {
			return highwater, nil, fmt.Errorf("%w: segment %d", err, seg)
		}
		band = decoded[:0]
		recovered = recovered || healed
		sample, byteOffset := seg/bytesPerSample, bytesPerSample-1-seg%bytesPerSample
		if decoderecovery.From(ctx).RLEReversedByteSegments {
			byteOffset = seg % bytesPerSample
		}
		for y := 0; y < region.Dy(); y++ {
			if err := ctx.Err(); err != nil {
				return highwater, nil, err
			}
			src := decoded[y*cols+region.Min.X : y*cols+region.Max.X]
			pos := (y*region.Dx())*step + sample*bytesPerSample + byteOffset
			for _, value := range src {
				dst[pos] = value
				highwater = max(highwater, pos+1)
				pos += step
			}
		}
	}
	var diagnostics []string
	if recovered {
		diagnostics = []string{pixeldata.DiagnosticRLESegmentOverrun}
	}
	if decoderecovery.From(ctx).RLEReversedByteSegments && bytesPerSample > 1 {
		diagnostics = append(diagnostics, pixeldata.DiagnosticRLEReversedByteSegments)
	}
	return len(dst), diagnostics, ctx.Err()
}
