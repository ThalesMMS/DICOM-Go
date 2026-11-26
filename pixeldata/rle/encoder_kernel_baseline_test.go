// Frozen encoder from b16b39d9, renamed only as an independent byte-output oracle.
package rle

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

const (
	baselineRleHeaderLength      = 64
	baselineMaxRLEFragmentLength = uint64(1<<32 - 2)
)

// baselineEncoder encodes canonical native frames as DICOM RLE Lossless frames.
// baselineEncoder has no mutable state and is safe for concurrent use.
type baselineEncoder struct{}

var _ pixeldata.FrameEncoder = (*baselineEncoder)(nil)

// baselineNewEncoder returns a pure Go DICOM RLE Lossless frame encoder.
func baselineNewEncoder() *baselineEncoder {
	return &baselineEncoder{}
}

// baselineRegisterEncoder registers a new RLE encoder in registry.
func baselineRegisterEncoder(registry pixeldata.EncoderRegistry) error {
	if registry == nil {
		return pixeldata.ErrEncoderRegistryNil
	}
	return registry.RegisterEncoder(UID, baselineNewEncoder())
}

// Capabilities describes the canonical native frames accepted by baselineEncoder.
func (*baselineEncoder) Capabilities() pixeldata.EncoderCapabilities {
	return pixeldata.EncoderCapabilities{
		TransferSyntaxUID:          UID,
		BitsAllocated:              baselineEncoderBitsAllocated(),
		PixelRepresentations:       []uint16{0, 1},
		SamplesPerPixel:            []uint16{1, 3},
		PhotometricInterpretations: []string{"MONOCHROME1", "MONOCHROME2", "PALETTE COLOR", "RGB", "YBR_FULL"},
		Lossless:                   true,
		SupportsMultiFrame:         true,
		Backend:                    "pure-go",
	}
}

// EncodeFrame encodes one little-endian, sample-interleaved native frame.
// The result is one complete RLE frame suitable for one encapsulated fragment.
func (*baselineEncoder) EncodeFrame(ctx context.Context, frame []byte, metadata pixeldata.Metadata) (pixeldata.EncodedFrame, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}

	bytesPerSample, segmentCount, expectedFrameLength, maxEncodedLength, err := baselineValidateEncoderInput(metadata)
	if err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	if uint64(len(frame)) != expectedFrameLength {
		return pixeldata.EncodedFrame{}, fmt.Errorf("%w: native frame length", pixeldata.ErrPixelDataSizeMismatch)
	}

	capacity := maxEncodedLength
	if capacity > uint64(baselineMaxInt()) {
		return pixeldata.EncodedFrame{}, fmt.Errorf("%w: encoded frame length", pixeldata.ErrEncoderOutputInvalid)
	}
	encoded := make([]byte, baselineRleHeaderLength, int(capacity))
	binary.LittleEndian.PutUint32(encoded[0:4], uint32(segmentCount))
	row := make([]byte, int(metadata.Columns))

	for sample := 0; sample < int(metadata.SamplesPerPixel); sample++ {
		for plane := bytesPerSample - 1; plane >= 0; plane-- {
			if err := ctx.Err(); err != nil {
				return pixeldata.EncodedFrame{}, err
			}
			segment := sample*bytesPerSample + bytesPerSample - 1 - plane
			binary.LittleEndian.PutUint32(encoded[4+segment*4:8+segment*4], uint32(len(encoded)))

			for y := 0; y < int(metadata.Rows); y++ {
				if err := ctx.Err(); err != nil {
					return pixeldata.EncodedFrame{}, err
				}
				for x := 0; x < int(metadata.Columns); x++ {
					sampleIndex := (y*int(metadata.Columns)+x)*int(metadata.SamplesPerPixel) + sample
					row[x] = frame[sampleIndex*bytesPerSample+plane]
				}
				encoded = baselineAppendPackBitsRow(encoded, row)
			}
			if len(encoded)%2 != 0 {
				encoded = append(encoded, 0)
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	return pixeldata.EncodedFrame{Data: encoded}, nil
}

func baselineValidateEncoderInput(metadata pixeldata.Metadata) (bytesPerSample int, segmentCount int, frameLength uint64, maxEncodedLength uint64, err error) {
	if metadata.Rows == 0 {
		return 0, 0, 0, 0, baselineUnsupportedEncoderMetadata("Rows")
	}
	if metadata.Columns == 0 {
		return 0, 0, 0, 0, baselineUnsupportedEncoderMetadata("Columns")
	}
	if metadata.SamplesPerPixel != 1 && metadata.SamplesPerPixel != 3 {
		return 0, 0, 0, 0, baselineUnsupportedEncoderMetadata("SamplesPerPixel")
	}
	if !supportedBitsAllocated(int(metadata.BitsAllocated), int(metadata.SamplesPerPixel)) {
		return 0, 0, 0, 0, baselineUnsupportedEncoderMetadata("BitsAllocated")
	}
	if metadata.BitsStored == 0 || metadata.BitsStored > metadata.BitsAllocated {
		return 0, 0, 0, 0, baselineUnsupportedEncoderMetadata("BitsStored")
	}
	if metadata.HighBit != metadata.BitsStored-1 {
		return 0, 0, 0, 0, baselineUnsupportedEncoderMetadata("HighBit")
	}
	if metadata.PixelRepresentation > 1 {
		return 0, 0, 0, 0, baselineUnsupportedEncoderMetadata("PixelRepresentation")
	}
	if metadata.SamplesPerPixel != 1 && metadata.SamplesPerPixel != 3 {
		return 0, 0, 0, 0, baselineUnsupportedEncoderMetadata("SamplesPerPixel")
	}
	if metadata.NumberOfFrames < 1 {
		return 0, 0, 0, 0, baselineUnsupportedEncoderMetadata("NumberOfFrames")
	}
	photometric := strings.ToUpper(strings.TrimSpace(metadata.PhotometricInterpretation))
	switch metadata.SamplesPerPixel {
	case 1:
		// Planar Configuration is irrelevant for one sample.
		if photometric != "MONOCHROME1" && photometric != "MONOCHROME2" && photometric != "PALETTE COLOR" {
			return 0, 0, 0, 0, baselineUnsupportedEncoderMetadata("PhotometricInterpretation")
		}
		if photometric == "PALETTE COLOR" && metadata.PixelRepresentation != 0 {
			return 0, 0, 0, 0, baselineUnsupportedEncoderMetadata("PixelRepresentation")
		}
	case 3:
		if photometric != "RGB" && photometric != "YBR_FULL" {
			return 0, 0, 0, 0, baselineUnsupportedEncoderMetadata("PhotometricInterpretation")
		}
		if metadata.PixelRepresentation != 0 {
			return 0, 0, 0, 0, baselineUnsupportedEncoderMetadata("PixelRepresentation")
		}
		if !metadata.PlanarConfigurationPresent || metadata.PlanarConfiguration != 0 {
			return 0, 0, 0, 0, baselineUnsupportedEncoderMetadata("PlanarConfiguration")
		}
	}

	bytesPerSample = int(metadata.BitsAllocated / 8)
	segmentCount = int(metadata.SamplesPerPixel) * bytesPerSample
	if segmentCount == 0 || segmentCount > 15 {
		return 0, 0, 0, 0, baselineUnsupportedEncoderMetadata("SamplesPerPixel")
	}

	frameLength = uint64(metadata.Rows) * uint64(metadata.Columns) * uint64(metadata.SamplesPerPixel) * uint64(bytesPerSample)
	worstRowLength := uint64(metadata.Columns) + (uint64(metadata.Columns)+127)/128
	worstSegmentLength := uint64(metadata.Rows) * worstRowLength
	if worstSegmentLength%2 != 0 {
		worstSegmentLength++
	}
	maxEncodedLength = baselineRleHeaderLength + uint64(segmentCount)*worstSegmentLength
	if maxEncodedLength > baselineMaxRLEFragmentLength || maxEncodedLength > uint64(baselineMaxInt()) {
		return 0, 0, 0, 0, fmt.Errorf("%w: encoded frame length", pixeldata.ErrEncoderOutputInvalid)
	}
	return bytesPerSample, segmentCount, frameLength, maxEncodedLength, nil
}

// baselineEncoderBitsAllocated lists the whole-byte sample widths an RLE header can
// hold for one sample (up to 15 segments); three samples allow up to 40.
func baselineEncoderBitsAllocated() []uint16 {
	out := make([]uint16, 0, 15)
	for bits := uint16(8); bits <= 120; bits += 8 {
		out = append(out, bits)
	}
	return out
}

func baselineUnsupportedEncoderMetadata(field string) error {
	return &pixeldata.UnsupportedEncoderMetadataError{Field: field}
}

func baselineAppendPackBitsRow(dst, row []byte) []byte {
	literalStart := 0
	for i := 0; i < len(row); {
		runEnd := i + 1
		for runEnd < len(row) && row[runEnd] == row[i] {
			runEnd++
		}
		runLength := runEnd - i
		if runLength < 3 {
			i = runEnd
			continue
		}

		dst = baselineAppendPackBitsLiterals(dst, row[literalStart:i])
		for runLength > 128 {
			dst = append(dst, 0x81, row[i])
			i += 128
			runLength -= 128
		}
		if runLength >= 3 {
			dst = append(dst, byte(int8(1-runLength)), row[i])
			i += runLength
			literalStart = i
			continue
		}
		literalStart = i
		i += runLength
	}
	return baselineAppendPackBitsLiterals(dst, row[literalStart:])
}

func baselineAppendPackBitsLiterals(dst, literals []byte) []byte {
	for len(literals) > 0 {
		length := len(literals)
		if length > 128 {
			length = 128
		}
		dst = append(dst, byte(length-1))
		dst = append(dst, literals[:length]...)
		literals = literals[length:]
	}
	return dst
}

func baselineMaxInt() int {
	return int(^uint(0) >> 1)
}
