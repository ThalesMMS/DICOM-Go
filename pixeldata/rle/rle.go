package rle

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/decoderecovery"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// UID is the DICOM RLE Lossless transfer syntax UID.
const UID = "1.2.840.10008.1.2.5"

var (
	ErrInvalidHeader              = errors.New("dicom: invalid RLE header")
	ErrInvalidSegmentCount        = errors.New("dicom: invalid RLE segment count")
	ErrInvalidSegmentOffset       = errors.New("dicom: invalid RLE segment offset")
	ErrSegmentDecodeFailed        = errors.New("dicom: RLE segment decode failed")
	ErrUnsupportedBitsAllocated   = errors.New("dicom: unsupported RLE BitsAllocated")
	ErrUnsupportedSamplesPerPixel = errors.New("dicom: unsupported RLE SamplesPerPixel")
)

// Codec decodes DICOM RLE Lossless encapsulated pixel data.
type Codec struct{}

// New returns a DICOM RLE Lossless pixel data codec.
func New() *Codec {
	return &Codec{}
}

// Register registers the RLE Lossless codec in the provided pixel data registry.
func Register(registry pixeldata.Registry) error {
	if registry == nil {
		return pixeldata.ErrCodecRegistryNil
	}
	return registry.RegisterCodec(UID, New())
}

// RegisterDefault registers the RLE Lossless codec in pixeldata.DefaultRegistry.
func RegisterDefault() error {
	return pixeldata.RegisterCodec(UID, New())
}

// Decode decodes RLE Lossless frames (see DecodeContext).
func (c *Codec) Decode(pixel pixeldata.PixelData, obj *object.Object) (pixeldata.Frames, error) {
	return c.DecodeContext(context.Background(), pixel, obj)
}

// DecodeContext decodes each RLE frame while honoring cancellation between
// frames, segments, and PackBits runs. A frame may span several fragments
// (PS3.5 A.4): they are grouped by the Basic Offset Table, concatenated for a
// single frame, or split where the next RLE header starts. Samples of up to
// 15 segments are decoded (Bits Allocated a multiple of 8) and returned
// interleaved, little-endian, whatever Planar Configuration says; one-sample
// images ignore Planar Configuration.
func (*Codec) DecodeContext(ctx context.Context, pixel pixeldata.PixelData, obj *object.Object) (pixeldata.Frames, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.Frames{}, err
	}
	if !pixel.Encapsulated {
		return pixeldata.Frames{}, fmt.Errorf("%w: RLE requires encapsulated pixel data", pixeldata.ErrIncompatiblePixelData)
	}

	metadata, err := pixeldata.ExtractMetadata(obj)
	if err != nil {
		return pixeldata.Frames{}, err
	}
	if err := validateDecodeMetadata(metadata); err != nil {
		return pixeldata.Frames{}, err
	}

	expectedSegments := int(metadata.SamplesPerPixel) * int(metadata.BitsAllocated) / 8
	frameFragments, err := groupFrames(pixel.Sequence, metadata.NumberOfFrames, expectedSegments)
	if err != nil {
		return pixeldata.Frames{}, err
	}
	tolerant := decoderecovery.From(ctx).RLESegmentOverrun
	overrun := false
	frames := make([][]byte, len(frameFragments))
	for i, fragment := range frameFragments {
		if err := ctx.Err(); err != nil {
			return pixeldata.Frames{}, err
		}
		frame, recovered, err := decodeFrame(
			ctx,
			fragment,
			int(metadata.Rows),
			int(metadata.Columns),
			int(metadata.SamplesPerPixel),
			int(metadata.BitsAllocated),
			tolerant,
		)
		if err != nil {
			return pixeldata.Frames{}, fmt.Errorf("decode RLE frame %d: %w", i, err)
		}
		overrun = overrun || recovered
		frames[i] = frame
	}

	out := pixeldata.Frames{
		Rows:    int(metadata.Rows),
		Columns: int(metadata.Columns),
		Data:    frames,
	}
	if overrun {
		out.Diagnostics = append(out.Diagnostics, pixeldata.DiagnosticRLESegmentOverrun)
	}
	if decoderecovery.From(ctx).RLEReversedByteSegments && metadata.BitsAllocated > 8 {
		out.Diagnostics = append(out.Diagnostics, pixeldata.DiagnosticRLEReversedByteSegments)
	}
	return out, nil
}

// validateDecodeMetadata checks the image metadata the decoder supports.
func validateDecodeMetadata(metadata pixeldata.Metadata) error {
	if metadata.SamplesPerPixel == 0 || metadata.SamplesPerPixel > 3 {
		return fmt.Errorf("%w: SamplesPerPixel=%d", ErrUnsupportedSamplesPerPixel, metadata.SamplesPerPixel)
	}
	if !supportedBitsAllocated(int(metadata.BitsAllocated), int(metadata.SamplesPerPixel)) {
		return fmt.Errorf("%w: BitsAllocated=%d SamplesPerPixel=%d", ErrUnsupportedBitsAllocated, metadata.BitsAllocated, metadata.SamplesPerPixel)
	}
	if metadata.BitsStored == 0 || metadata.BitsStored > metadata.BitsAllocated || metadata.HighBit != metadata.BitsStored-1 {
		return fmt.Errorf("%w: invalid stored-bit metadata", pixeldata.ErrIncompatiblePixelData)
	}
	if metadata.PixelRepresentation != 0 && metadata.PixelRepresentation != 1 {
		return fmt.Errorf("%w: PixelRepresentation=%d", pixeldata.ErrUnsupportedPixelRepresentation, metadata.PixelRepresentation)
	}
	if !supportedPhotometricInterpretation(metadata) {
		return fmt.Errorf(
			"%w: PhotometricInterpretation=%s SamplesPerPixel=%d",
			pixeldata.ErrUnsupportedPhotometricInterpretation,
			strings.TrimSpace(metadata.PhotometricInterpretation),
			metadata.SamplesPerPixel,
		)
	}
	photometric := strings.ToUpper(strings.TrimSpace(metadata.PhotometricInterpretation))
	// RLE segments are color-by-plane by definition; Planar Configuration
	// only describes the decoded layout and is irrelevant for one sample.
	if metadata.SamplesPerPixel == 3 && metadata.PlanarConfigurationPresent && metadata.PlanarConfiguration > 1 {
		return fmt.Errorf("%w: invalid PlanarConfiguration", pixeldata.ErrUnsupportedPlanarConfiguration)
	}
	if metadata.PixelRepresentation == 1 && photometric != "MONOCHROME1" && photometric != "MONOCHROME2" {
		return fmt.Errorf("%w: PixelRepresentation=%d", pixeldata.ErrUnsupportedPixelRepresentation, metadata.PixelRepresentation)
	}

	return nil
}

// supportedBitsAllocated reports whole-byte samples within the 15 segments
// an RLE header can list (PS3.5 G.3.1).
func supportedBitsAllocated(bitsAllocated, samplesPerPixel int) bool {
	return bitsAllocated >= 8 && bitsAllocated%8 == 0 && samplesPerPixel*bitsAllocated/8 <= 15
}

// groupFrames returns the encoded bytes of every frame: one fragment per
// frame, fragments grouped by the Basic Offset Table, all fragments of a
// single frame, or fragments split where a plausible RLE header with the
// expected segment count begins.
func groupFrames(sequence core.FragmentSequence, numberOfFrames, expectedSegments int) ([][]byte, error) {
	fragments := sequence.Fragments
	if len(fragments) == 0 {
		return nil, fmt.Errorf("%w: no RLE frame fragments", ErrInvalidHeader)
	}
	if numberOfFrames <= 0 {
		numberOfFrames = 1
	}
	if numberOfFrames == len(fragments) {
		return fragments, nil
	}
	if numberOfFrames > len(fragments) {
		return nil, fmt.Errorf("%w: NumberOfFrames=%d fragments=%d", pixeldata.ErrPixelDataSizeMismatch, numberOfFrames, len(fragments))
	}
	if numberOfFrames == 1 {
		return [][]byte{concatFragments(fragments)}, nil
	}
	if starts, ok := offsetTableStarts(sequence, numberOfFrames); ok {
		return splitAt(fragments, starts), nil
	}
	var starts []int
	for i, fragment := range fragments {
		if i == 0 || looksLikeHeader(fragment, expectedSegments) {
			starts = append(starts, i)
		}
	}
	if len(starts) != numberOfFrames {
		return nil, fmt.Errorf("%w: NumberOfFrames=%d fragments=%d, no Basic Offset Table and %d RLE headers", pixeldata.ErrPixelDataSizeMismatch, numberOfFrames, len(fragments), len(starts))
	}
	return splitAt(fragments, starts), nil
}

// offsetTableStarts maps Basic Offset Table entries to the index of the
// fragment each frame starts at (offsets count 8-byte Item headers).
func offsetTableStarts(sequence core.FragmentSequence, frames int) ([]int, bool) {
	table := sequence.OffsetTable
	if len(table) != 4*frames {
		return nil, false
	}
	positions := make(map[uint64]int, len(sequence.Fragments))
	var position uint64
	for i, fragment := range sequence.Fragments {
		positions[position] = i
		position += 8 + uint64(len(fragment))
	}
	starts := make([]int, frames)
	for i := range starts {
		index, ok := positions[uint64(binary.LittleEndian.Uint32(table[4*i:]))]
		if !ok || (i > 0 && index <= starts[i-1]) || (i == 0 && index != 0) {
			return nil, false
		}
		starts[i] = index
	}
	return starts, true
}

func splitAt(fragments [][]byte, starts []int) [][]byte {
	out := make([][]byte, len(starts))
	for i, start := range starts {
		end := len(fragments)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		out[i] = concatFragments(fragments[start:end])
	}
	return out
}

func concatFragments(fragments [][]byte) []byte {
	if len(fragments) == 1 {
		return fragments[0]
	}
	size := 0
	for _, fragment := range fragments {
		size += len(fragment)
	}
	out := make([]byte, 0, size)
	for _, fragment := range fragments {
		out = append(out, fragment...)
	}
	return out
}

// looksLikeHeader reports an RLE header listing expectedSegments segments,
// the first at offset 64 and the rest increasing, with unused slots zero.
func looksLikeHeader(fragment []byte, expectedSegments int) bool {
	if len(fragment) < 64 || int(binary.LittleEndian.Uint32(fragment)) != expectedSegments {
		return false
	}
	previous := uint32(0)
	for i := 0; i < 15; i++ {
		offset := binary.LittleEndian.Uint32(fragment[4+4*i:])
		switch {
		case i == 0 && offset != 64:
			return false
		case i < expectedSegments && offset < previous:
			return false
		case i >= expectedSegments && offset != 0:
			return false
		}
		previous = offset
	}
	return true
}

func supportedPhotometricInterpretation(metadata pixeldata.Metadata) bool {
	photometric := strings.ToUpper(strings.TrimSpace(metadata.PhotometricInterpretation))
	switch metadata.SamplesPerPixel {
	case 1:
		return photometric == "MONOCHROME1" || photometric == "MONOCHROME2" || photometric == "PALETTE COLOR"
	case 3:
		return photometric == "RGB" || photometric == "YBR_FULL"
	default:
		return false
	}
}

func parseHeader(fragment []byte) ([]uint32, error) {
	if len(fragment) < 64 {
		return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidHeader, len(fragment))
	}
	if uint64(len(fragment)) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("%w: fragment too large: %d bytes", ErrInvalidHeader, len(fragment))
	}

	count := binary.LittleEndian.Uint32(fragment[:4])
	if count == 0 || count > 15 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidSegmentCount, count)
	}

	offsets := make([]uint32, int(count)+1)
	for i := 0; i < int(count); i++ {
		offset := binary.LittleEndian.Uint32(fragment[4+i*4 : 8+i*4])
		if offset < 64 || offset > uint32(len(fragment)) {
			return nil, fmt.Errorf("%w: segment %d offset=%d fragment_length=%d", ErrInvalidSegmentOffset, i, offset, len(fragment))
		}
		if i > 0 && offset < offsets[i-1] {
			return nil, fmt.Errorf("%w: segment %d offset=%d previous_offset=%d", ErrInvalidSegmentOffset, i, offset, offsets[i-1])
		}
		offsets[i] = offset
	}
	offsets[len(offsets)-1] = uint32(len(fragment))

	return offsets, nil
}

// maxPackBitsExpansion bounds the bytes one PackBits byte can decode to: a
// two-byte replicate run yields 128.
const maxPackBitsExpansion = 64

// canFill reports whether a PackBits segment of n bytes can decode to size
// bytes. Strict decoding checks it before allocating from the declared
// frame size, so a tiny segment cannot make it allocate a large frame.
func canFill(n, size int) bool {
	return uint64(n)*maxPackBitsExpansion >= uint64(size)
}

func decodePackBits(segment []byte, expectedSize int) ([]byte, error) {
	out, _, err := decodePackBitsContext(context.Background(), segment, expectedSize, false)
	return out, err
}

// decodePackBitsContext decodes one PackBits segment to expectedSize bytes
// (see unpackBits).
func decodePackBitsContext(ctx context.Context, segment []byte, expectedSize int, tolerant bool) (out []byte, recovered bool, err error) {
	if expectedSize < 0 {
		return nil, false, fmt.Errorf("%w: invalid expected size %d", ErrSegmentDecodeFailed, expectedSize)
	}
	if !tolerant && !canFill(len(segment), expectedSize) {
		return nil, false, fmt.Errorf("%w: %d segment bytes cannot decode to %d", ErrSegmentDecodeFailed, len(segment), expectedSize)
	}
	out = make([]byte, expectedSize)
	recovered, err = unpackBits(ctx, segment, out, 0, 1, expectedSize, tolerant)
	if err != nil {
		return nil, false, err
	}
	return out, recovered, nil
}

// unpackBits decodes one PackBits segment to size bytes written to
// dst[first], dst[first+step], ...; dst must be zeroed. Trailing bytes after
// the last run (segment padding) are ignored. With tolerant, a literal run
// cut by the segment end keeps the bytes present, a run past size is
// clipped and a short segment is left zero-filled, and recovered reports
// that one of these was needed.
func unpackBits(ctx context.Context, segment, dst []byte, first, step, size int, tolerant bool) (recovered bool, err error) {
	written := 0
	for i := 0; i < len(segment) && written < size; {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		h := int(int8(segment[i]))
		i++

		switch {
		case h >= 0:
			n := h + 1
			if i+n > len(segment) {
				if !tolerant {
					return false, fmt.Errorf("%w: truncated literal run", ErrSegmentDecodeFailed)
				}
				n, recovered = len(segment)-i, true
			}
			if n > size-written {
				if !tolerant {
					return false, fmt.Errorf("%w: literal run exceeds expected size", ErrSegmentDecodeFailed)
				}
				n, recovered = size-written, true
			}
			if step == 1 {
				copy(dst[first+written:], segment[i:i+n])
			} else {
				d := first + written*step
				for _, value := range segment[i : i+n] {
					dst[d] = value
					d += step
				}
			}
			written += n
			i += n
		case h >= -127:
			if i >= len(segment) {
				if !tolerant {
					return false, fmt.Errorf("%w: truncated repeat run", ErrSegmentDecodeFailed)
				}
				recovered = true
				break
			}
			n := 1 - h
			value := segment[i]
			i++
			if n > size-written {
				if !tolerant {
					return false, fmt.Errorf("%w: repeat run exceeds expected size", ErrSegmentDecodeFailed)
				}
				n, recovered = size-written, true
			}
			if step == 1 {
				run := dst[first+written : first+written+n]
				for j := range run {
					run[j] = value
				}
			} else {
				d := first + written*step
				for range n {
					dst[d] = value
					d += step
				}
			}
			written += n
		default:
			// h == -128 is a no-op in PackBits.
		}
	}
	if written != size {
		if !tolerant {
			return false, fmt.Errorf("%w: decoded %d bytes, expected %d", ErrSegmentDecodeFailed, written, size)
		}
		recovered = true
	}
	return recovered, nil
}

func decodeFragment(fragment []byte, rows, cols, samplesPerPixel, bitsAllocated int) ([]byte, error) {
	out, _, err := decodeFrame(context.Background(), fragment, rows, cols, samplesPerPixel, bitsAllocated, false)
	return out, err
}

// decodeFrame decodes one RLE frame to interleaved little-endian samples.
// Segments are decoded in place into the frame, the only buffer allocated;
// strict decoding first refuses segments too short to fill it.
func decodeFrame(ctx context.Context, fragment []byte, rows, cols, samplesPerPixel, bitsAllocated int, tolerant bool) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if !supportedBitsAllocated(bitsAllocated, samplesPerPixel) {
		return nil, false, fmt.Errorf("%w: BitsAllocated=%d", ErrUnsupportedBitsAllocated, bitsAllocated)
	}
	if rows <= 0 || cols <= 0 || samplesPerPixel <= 0 || samplesPerPixel > 3 {
		return nil, false, fmt.Errorf(
			"%w: rows=%d columns=%d samples_per_pixel=%d",
			ErrInvalidSegmentCount,
			rows,
			cols,
			samplesPerPixel,
		)
	}

	bytesPerSample := bitsAllocated / 8
	expectedSegments := samplesPerPixel * bytesPerSample

	offsets, err := parseHeader(fragment)
	if err != nil {
		return nil, false, err
	}
	if actualSegments := len(offsets) - 1; actualSegments != expectedSegments {
		return nil, false, fmt.Errorf("%w: got %d want %d", ErrInvalidSegmentCount, actualSegments, expectedSegments)
	}

	maxInt := int64(^uint(0) >> 1)
	pixelsPerSegment64 := int64(rows) * int64(cols)
	frameSize64 := pixelsPerSegment64 * int64(samplesPerPixel) * int64(bytesPerSample)
	if pixelsPerSegment64 <= 0 || frameSize64 <= 0 || pixelsPerSegment64 > maxInt || frameSize64 > maxInt {
		return nil, false, fmt.Errorf(
			"%w: rows=%d columns=%d samples_per_pixel=%d bits_allocated=%d",
			ErrInvalidSegmentCount,
			rows,
			cols,
			samplesPerPixel,
			bitsAllocated,
		)
	}

	pixelsPerSegment := int(pixelsPerSegment64)
	for segmentIndex := range expectedSegments {
		segmentStart, segmentEnd := int(offsets[segmentIndex]), int(offsets[segmentIndex+1])
		if segmentEnd < segmentStart {
			return nil, false, fmt.Errorf("%w: segment %d offset range %d..%d", ErrInvalidSegmentOffset, segmentIndex, segmentStart, segmentEnd)
		}
		if !tolerant && !canFill(segmentEnd-segmentStart, pixelsPerSegment) {
			return nil, false, fmt.Errorf("%w: segment %d: %d bytes cannot decode to %d", ErrSegmentDecodeFailed, segmentIndex, segmentEnd-segmentStart, pixelsPerSegment)
		}
	}

	// Segments hold each sample's bytes most significant first; the output
	// interleaves samples with little-endian bytes.
	out := make([]byte, int(frameSize64))
	step := samplesPerPixel * bytesPerSample
	recovered := false
	for segmentIndex := range expectedSegments {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		sample, byteOffset := segmentIndex/bytesPerSample, bytesPerSample-1-segmentIndex%bytesPerSample
		if decoderecovery.From(ctx).RLEReversedByteSegments {
			byteOffset = segmentIndex % bytesPerSample
		}
		segment := fragment[offsets[segmentIndex]:offsets[segmentIndex+1]]
		segmentRecovered, err := unpackBits(ctx, segment, out, sample*bytesPerSample+byteOffset, step, pixelsPerSegment, tolerant)
		if err != nil {
			return nil, false, fmt.Errorf("%w: segment %d", err, segmentIndex)
		}
		recovered = recovered || segmentRecovered
	}

	return out, recovered, nil
}
