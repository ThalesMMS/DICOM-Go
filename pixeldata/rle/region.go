package rle

import (
	"context"
	"fmt"
	"image"

	"github.com/ThalesMMS/dicom-go/internal/decoderecovery"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// DecodeFrameRegion implements pixeldata.FrameRegionDecoder. RLE segments
// hold each byte of the samples for the whole frame row after row, so every
// segment is decoded only up to the region's last row, and only the region's
// columns are kept: the rows below the region are never decoded and the
// output holds only the region. The samples are interleaved little-endian,
// as DecodeContext returns them. codestream is one frame (one RLE header).
func (*Codec) DecodeFrameRegion(ctx context.Context, codestream []byte, metadata pixeldata.Metadata, region image.Rectangle) (pixeldata.Frames, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.Frames{}, err
	}
	if err := validateDecodeMetadata(metadata); err != nil {
		return pixeldata.Frames{}, err
	}
	rows, cols := int(metadata.Rows), int(metadata.Columns)
	if region.Empty() || !region.In(image.Rect(0, 0, cols, rows)) {
		return pixeldata.Frames{}, fmt.Errorf("%w: %v in %dx%d", pixeldata.ErrInvalidRegion, region, cols, rows)
	}
	samples, bytesPerSample := int(metadata.SamplesPerPixel), int(metadata.BitsAllocated)/8
	segments := samples * bytesPerSample
	offsets, err := parseHeader(codestream)
	if err != nil {
		return pixeldata.Frames{}, err
	}
	if len(offsets)-1 != segments {
		return pixeldata.Frames{}, fmt.Errorf("%w: got %d want %d", ErrInvalidSegmentCount, len(offsets)-1, segments)
	}
	tolerant := decoderecovery.From(ctx).RLESegmentOverrun
	width, height := region.Dx(), region.Dy()
	skip, stop := region.Min.Y*cols, region.Max.Y*cols
	for segment := 0; segment < segments; segment++ {
		start, end := int(offsets[segment]), int(offsets[segment+1])
		if end < start || end > len(codestream) {
			return pixeldata.Frames{}, fmt.Errorf("%w: segment %d offset range %d..%d", ErrInvalidSegmentOffset, segment, start, end)
		}
		if !tolerant && !canFill(end-start, stop) {
			return pixeldata.Frames{}, fmt.Errorf("%w: segment %d: %d bytes cannot decode to %d", ErrSegmentDecodeFailed, segment, end-start, stop)
		}
	}
	step := samples * bytesPerSample
	out := make([]byte, width*height*step)
	// band holds the region's rows of one segment and is reused.
	band := make([]byte, 0, stop-skip)
	recovered := false
	// Segments hold each sample's bytes most significant first.
	for segment := 0; segment < segments; segment++ {
		start, end := int(offsets[segment]), int(offsets[segment+1])
		rowsOfRegion, segmentRecovered, err := decodePackBitsRange(ctx, codestream[start:end], skip, stop, tolerant, band)
		if err != nil {
			return pixeldata.Frames{}, fmt.Errorf("%w: segment %d", err, segment)
		}
		recovered = recovered || segmentRecovered
		sample, byteOffset := segment/bytesPerSample, bytesPerSample-1-segment%bytesPerSample
		if decoderecovery.From(ctx).RLEReversedByteSegments {
			byteOffset = segment % bytesPerSample
		}
		for y := 0; y < height; y++ {
			src := rowsOfRegion[y*cols+region.Min.X : y*cols+region.Max.X]
			dst := (y*width)*step + sample*bytesPerSample + byteOffset
			for _, value := range src {
				out[dst] = value
				dst += step
			}
		}
	}
	frames := pixeldata.Frames{Rows: height, Columns: width, Data: [][]byte{out}}
	if recovered {
		frames.Diagnostics = append(frames.Diagnostics, pixeldata.DiagnosticRLESegmentOverrun)
	}
	if decoderecovery.From(ctx).RLEReversedByteSegments && bytesPerSample > 1 {
		frames.Diagnostics = append(frames.Diagnostics, pixeldata.DiagnosticRLEReversedByteSegments)
	}
	return frames, nil
}

// decodePackBitsRange decodes one PackBits segment only as far as stop and
// returns the decoded bytes [skip, stop) in buf's storage: the bytes before
// skip are decoded but not kept, and a run crossing stop is cut there. A
// segment that ends before stop is an error, or zero-filled when tolerant.
func decodePackBitsRange(ctx context.Context, segment []byte, skip, stop int, tolerant bool, buf []byte) ([]byte, bool, error) {
	out := buf[:0]
	position := 0
	keep := func(data []byte, value byte, n int) {
		from, to := max(position, skip), min(position+n, stop)
		for p := from; p < to; p++ {
			if data != nil {
				out = append(out, data[p-position])
			} else {
				out = append(out, value)
			}
		}
		position += n
	}
	recovered := false
	for i := 0; i < len(segment) && position < stop; {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		h := int(int8(segment[i]))
		i++
		switch {
		case h >= 0:
			n := h + 1
			if i+n > len(segment) {
				if !tolerant {
					return nil, false, fmt.Errorf("%w: truncated literal run", ErrSegmentDecodeFailed)
				}
				n, recovered = len(segment)-i, true
			}
			keep(segment[i:i+n], 0, n)
			i += n
		case h >= -127:
			if i >= len(segment) {
				if !tolerant {
					return nil, false, fmt.Errorf("%w: truncated repeat run", ErrSegmentDecodeFailed)
				}
				recovered = true
				i = len(segment)
				break
			}
			keep(nil, segment[i], 1-h)
			i++
		}
	}
	if position < stop {
		if !tolerant {
			return nil, false, fmt.Errorf("%w: decoded %d bytes, need %d", ErrSegmentDecodeFailed, position, stop)
		}
		for len(out) < stop-skip {
			out = append(out, 0)
		}
		recovered = true
	}
	return out, recovered, nil
}
