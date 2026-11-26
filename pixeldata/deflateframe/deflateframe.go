package deflateframe

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// UID is the Deflated Image Frame Compression transfer syntax UID.
const UID = "1.2.840.10008.1.2.8.1"

var (
	// ErrExpansionLimit reports a deflate stream that inflates to more bytes
	// than the frame size declared by the image metadata.
	ErrExpansionLimit = errors.New("dicom: deflated frame exceeds the expected frame size")
	// ErrShortFrame reports a deflate stream that ends before the declared
	// frame size was produced.
	ErrShortFrame = errors.New("dicom: deflated frame is shorter than the expected frame size")
	// ErrInvalidStream reports corrupt, truncated or empty deflate data.
	ErrInvalidStream = errors.New("dicom: invalid deflated frame stream")
	// ErrTrailingData reports more than one pad byte after the end of the
	// deflate stream in a fragment.
	ErrTrailingData = errors.New("dicom: unexpected bytes after deflated frame stream")
	// ErrUnsupportedMetadata reports image metadata this codec cannot size.
	ErrUnsupportedMetadata = errors.New("dicom: unsupported metadata for Deflated Image Frame Compression")
)

// maxFragmentLength is the largest even value an Item length can carry.
const maxFragmentLength = uint64(0xFFFFFFFE)

// inflateChunk bounds the work done between cancellation checks.
const inflateChunk = 256 << 10

// ExpansionLimitError is returned when a frame's deflate stream inflates past
// Limit bytes, the frame size declared by Rows, Columns, Samples per Pixel and
// Bits Allocated. It carries no pixel or patient values.
type ExpansionLimitError struct {
	// Frame is the zero-based frame index, or -1 for a standalone stream.
	Frame int
	// Limit is the expected frame size in bytes.
	Limit int64
}

func (e *ExpansionLimitError) Error() string {
	if e == nil {
		return ErrExpansionLimit.Error()
	}
	if e.Frame < 0 {
		return fmt.Sprintf("%s (limit %d bytes)", ErrExpansionLimit, e.Limit)
	}
	return fmt.Sprintf("%s: frame %d (limit %d bytes)", ErrExpansionLimit, e.Frame, e.Limit)
}

// Unwrap classifies the error as ErrExpansionLimit and as a pixel data size
// mismatch.
func (*ExpansionLimitError) Unwrap() []error {
	return []error{ErrExpansionLimit, pixeldata.ErrPixelDataSizeMismatch}
}

// Codec decodes Deflated Image Frame Compression pixel data.
type Codec struct{}

var _ pixeldata.ContextCodec = (*Codec)(nil)

// New returns a Deflated Image Frame Compression codec.
func New() *Codec { return &Codec{} }

// Register registers the decoder in registry.
func Register(registry pixeldata.Registry) error {
	if registry == nil {
		return pixeldata.ErrCodecRegistryNil
	}
	return registry.RegisterCodec(UID, New())
}

// RegisterDefault registers the decoder in pixeldata.DefaultRegistry.
func RegisterDefault() error {
	return pixeldata.RegisterCodec(UID, New())
}

// Decode decodes every frame (see DecodeContext).
func (c *Codec) Decode(pixel pixeldata.PixelData, obj *object.Object) (pixeldata.Frames, error) {
	return c.DecodeContext(context.Background(), pixel, obj)
}

// DecodeContext inflates each frame into a freshly allocated buffer of exactly
// the native frame size. Frames are returned in the native layout of the
// source (Planar Configuration and bit packing are unchanged), since the
// deflated byte stream is the native frame. One fragment per frame is the
// standard layout; a single frame split across fragments, or frames grouped
// by the Basic Offset Table, are also accepted.
func (*Codec) DecodeContext(ctx context.Context, pixel pixeldata.PixelData, obj *object.Object) (pixeldata.Frames, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.Frames{}, err
	}
	if !pixel.Encapsulated {
		return pixeldata.Frames{}, fmt.Errorf("%w: Deflated Image Frame Compression requires encapsulated pixel data", pixeldata.ErrIncompatiblePixelData)
	}
	metadata, err := pixeldata.ExtractMetadata(obj)
	if err != nil {
		return pixeldata.Frames{}, err
	}
	frameSize, err := expectedFrameSize(metadata)
	if err != nil {
		return pixeldata.Frames{}, err
	}
	streams, err := groupFrames(pixel.Sequence, metadata.NumberOfFrames)
	if err != nil {
		return pixeldata.Frames{}, err
	}
	out := pixeldata.Frames{
		Rows:    int(metadata.Rows),
		Columns: int(metadata.Columns),
		Data:    make([][]byte, len(streams)),
	}
	for i, stream := range streams {
		if err := ctx.Err(); err != nil {
			return pixeldata.Frames{}, err
		}
		frame, nonZeroPad, err := inflate(ctx, stream, frameSize, i)
		if err != nil {
			return pixeldata.Frames{}, err
		}
		if nonZeroPad {
			out.AddDiagnostic(pixeldata.DiagnosticNonZeroFramePad)
		}
		out.Data[i] = frame
	}
	return out, nil
}

// InflateFrame inflates one fragment holding a raw DEFLATE stream (plus an
// optional single pad byte) to exactly frameSize bytes. A stream producing
// more than frameSize bytes fails with *ExpansionLimitError without
// materializing the excess.
func InflateFrame(fragment []byte, frameSize int) ([]byte, error) {
	if frameSize <= 0 {
		return nil, fmt.Errorf("%w: frame size %d", ErrUnsupportedMetadata, frameSize)
	}
	frame, _, err := inflate(context.Background(), fragment, frameSize, -1)
	return frame, err
}

// DeflateFrame compresses one native frame as a raw DEFLATE stream at the
// given compress/flate level and pads it to even length with one NULL byte.
func DeflateFrame(frame []byte, level int) ([]byte, error) {
	return deflate(context.Background(), frame, level)
}

func inflate(ctx context.Context, stream []byte, frameSize, index int) (frame []byte, nonZeroPad bool, err error) {
	if len(stream) == 0 {
		return nil, false, frameError(index, ErrInvalidStream, "empty fragment")
	}
	// bytes.Reader is an io.ByteReader, so the inflater consumes exactly the
	// bytes of the stream and the remainder can be checked for padding.
	source := bytes.NewReader(stream)
	inflater := flate.NewReader(source)
	defer func() { _ = inflater.Close() }()

	frame = make([]byte, frameSize)
	filled := 0
	for filled < frameSize {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		end := min(filled+inflateChunk, frameSize)
		n, readErr := inflater.Read(frame[filled:end])
		filled += n
		if readErr == io.EOF {
			if filled < frameSize {
				return nil, false, frameError(index, ErrShortFrame, fmt.Sprintf("%d of %d bytes", filled, frameSize))
			}
			break
		}
		if readErr != nil {
			return nil, false, frameError(index, ErrInvalidStream, "corrupt or truncated deflate data")
		}
	}
	// The frame is full; the stream must end here. Ask for one more byte
	// only, so a bomb never inflates past frameSize+1.
	var extra [1]byte
	for {
		n, readErr := inflater.Read(extra[:])
		if n > 0 {
			return nil, false, &ExpansionLimitError{Frame: index, Limit: int64(frameSize)}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, false, frameError(index, ErrInvalidStream, "corrupt or truncated deflate data")
		}
	}
	switch remaining := source.Len(); {
	case remaining == 0:
	case remaining == 1:
		nonZeroPad = stream[len(stream)-1] != 0
	default:
		return nil, false, frameError(index, ErrTrailingData, fmt.Sprintf("%d bytes", remaining))
	}
	return frame, nonZeroPad, nil
}

func deflate(ctx context.Context, frame []byte, level int) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(frame) == 0 {
		return nil, fmt.Errorf("%w: empty frame", pixeldata.ErrPixelDataSizeMismatch)
	}
	var buf bytes.Buffer
	deflater, err := flate.NewWriter(&buf, level)
	if err != nil {
		return nil, fmt.Errorf("%w: compression level %d", pixeldata.ErrUnsupportedEncoderMetadata, level)
	}
	for start := 0; start < len(frame); start += inflateChunk {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(start+inflateChunk, len(frame))
		if _, err := deflater.Write(frame[start:end]); err != nil {
			return nil, err
		}
	}
	if err := deflater.Close(); err != nil {
		return nil, err
	}
	if buf.Len()%2 != 0 {
		buf.WriteByte(0)
	}
	if uint64(buf.Len()) > maxFragmentLength {
		return nil, fmt.Errorf("%w: encoded frame length", pixeldata.ErrEncoderOutputInvalid)
	}
	return buf.Bytes(), nil
}

func frameError(index int, kind error, detail string) error {
	if index < 0 {
		return fmt.Errorf("%w: %s", kind, detail)
	}
	return fmt.Errorf("%w: frame %d: %s", kind, index, detail)
}

// expectedFrameSize is the native frame size the deflated stream must
// reproduce: Rows x Columns x Samples per Pixel x Bits Allocated / 8, rounded
// up to a whole byte for single-bit frames (4:2:2 YBR stores two samples per
// pixel natively).
func expectedFrameSize(metadata pixeldata.Metadata) (int, error) {
	if metadata.Rows == 0 || metadata.Columns == 0 || metadata.SamplesPerPixel == 0 {
		return 0, fmt.Errorf("%w: empty frame geometry", pixeldata.ErrInvalidMetadata)
	}
	if !supportedBitsAllocated(metadata.BitsAllocated) {
		return 0, fmt.Errorf("%w: BitsAllocated=%d", ErrUnsupportedMetadata, metadata.BitsAllocated)
	}
	if metadata.SamplesPerPixel != 1 && metadata.SamplesPerPixel != 3 && metadata.SamplesPerPixel != 4 {
		return 0, fmt.Errorf("%w: SamplesPerPixel=%d", ErrUnsupportedMetadata, metadata.SamplesPerPixel)
	}
	if metadata.NumberOfFrames < 1 {
		return 0, fmt.Errorf("%w: NumberOfFrames=%d", pixeldata.ErrInvalidMetadata, metadata.NumberOfFrames)
	}
	size := metadata.FrameSize()
	if size <= 0 || uint64(size) > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("%w: frame size", pixeldata.ErrInvalidMetadata)
	}
	return int(size), nil
}

// supportedBitsAllocated accepts the widths Pixel Data (7FE0,0010) can carry
// under this transfer syntax: single-bit and whole-byte samples up to 64 bits.
func supportedBitsAllocated(bits uint16) bool {
	return bits == 1 || bits >= 8 && bits <= 64 && bits%8 == 0
}

// groupFrames returns the deflate stream of every frame.
func groupFrames(sequence core.FragmentSequence, numberOfFrames int) ([][]byte, error) {
	fragments := sequence.Fragments
	if len(fragments) == 0 {
		return nil, fmt.Errorf("%w: no frame fragments", ErrInvalidStream)
	}
	switch {
	case numberOfFrames == len(fragments):
		return fragments, nil
	case numberOfFrames > len(fragments):
		return nil, fmt.Errorf("%w: NumberOfFrames=%d fragments=%d", pixeldata.ErrPixelDataSizeMismatch, numberOfFrames, len(fragments))
	case numberOfFrames == 1:
		return [][]byte{concat(fragments)}, nil
	}
	starts, ok := offsetTableStarts(sequence, numberOfFrames)
	if !ok {
		return nil, fmt.Errorf("%w: NumberOfFrames=%d fragments=%d without a usable Basic Offset Table", pixeldata.ErrPixelDataSizeMismatch, numberOfFrames, len(fragments))
	}
	out := make([][]byte, numberOfFrames)
	for i, start := range starts {
		end := len(fragments)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		out[i] = concat(fragments[start:end])
	}
	return out, nil
}

// offsetTableStarts maps Basic Offset Table entries, which count the 8-byte
// Item headers, to the index of the fragment each frame starts at.
func offsetTableStarts(sequence core.FragmentSequence, frames int) ([]int, bool) {
	table := sequence.OffsetTable
	if len(table) != 4*frames {
		return nil, false
	}
	index := make(map[uint64]int, len(sequence.Fragments))
	var position uint64
	for i, fragment := range sequence.Fragments {
		index[position] = i
		position += 8 + uint64(len(fragment))
	}
	starts := make([]int, frames)
	for i := range starts {
		at, ok := index[uint64(binary.LittleEndian.Uint32(table[4*i:]))]
		if !ok || i == 0 && at != 0 || i > 0 && at <= starts[i-1] {
			return nil, false
		}
		starts[i] = at
	}
	return starts, true
}

func concat(fragments [][]byte) []byte {
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

// Encoder deflates canonical native frames, one frame per fragment. It has no
// mutable state and is safe for concurrent use.
type Encoder struct {
	level int
}

var _ pixeldata.FrameEncoder = (*Encoder)(nil)

// NewEncoder returns an encoder using flate.DefaultCompression.
func NewEncoder() *Encoder { return &Encoder{level: flate.DefaultCompression} }

// NewEncoderLevel returns an encoder using a compress/flate level from
// flate.HuffmanOnly to flate.BestCompression. Every level is lossless.
func NewEncoderLevel(level int) (*Encoder, error) {
	if level < flate.HuffmanOnly || level > flate.BestCompression {
		return nil, fmt.Errorf("%w: compression level %d", pixeldata.ErrUnsupportedEncoderMetadata, level)
	}
	return &Encoder{level: level}, nil
}

// RegisterEncoder registers a default-level encoder in registry.
func RegisterEncoder(registry pixeldata.EncoderRegistry) error {
	if registry == nil {
		return pixeldata.ErrEncoderRegistryNil
	}
	return registry.RegisterEncoder(UID, NewEncoder())
}

// encoderPhotometricInterpretations lists the native layouts whose frame size
// pixeldata.Metadata.FrameSize describes exactly.
var encoderPhotometricInterpretations = []string{
	"MONOCHROME1", "MONOCHROME2", "PALETTE COLOR", "RGB", "YBR_FULL", "YBR_FULL_422",
}

// Capabilities describes the native frames Encoder accepts. The deflated
// stream is the native frame itself, so photometric interpretation and planar
// configuration are preserved unchanged.
func (*Encoder) Capabilities() pixeldata.EncoderCapabilities {
	return pixeldata.EncoderCapabilities{
		TransferSyntaxUID:          UID,
		BitsAllocated:              []uint16{1, 8, 16, 24, 32, 40, 48, 56, 64},
		PixelRepresentations:       []uint16{0, 1},
		SamplesPerPixel:            []uint16{1, 3},
		PhotometricInterpretations: append([]string(nil), encoderPhotometricInterpretations...),
		Lossless:                   true,
		SupportsMultiFrame:         true,
		Backend:                    "pure-go",
	}
}

// EncodeFrame deflates one native frame of exactly the metadata frame size.
func (e *Encoder) EncodeFrame(ctx context.Context, frame []byte, metadata pixeldata.Metadata) (pixeldata.EncodedFrame, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	if !supportedEncoderPhotometric(metadata.PhotometricInterpretation) {
		return pixeldata.EncodedFrame{}, &pixeldata.UnsupportedEncoderMetadataError{Field: "PhotometricInterpretation"}
	}
	frameSize, err := expectedFrameSize(metadata)
	if err != nil {
		return pixeldata.EncodedFrame{}, &pixeldata.UnsupportedEncoderMetadataError{Field: "FrameSize"}
	}
	if len(frame) != frameSize {
		return pixeldata.EncodedFrame{}, fmt.Errorf("%w: native frame length", pixeldata.ErrPixelDataSizeMismatch)
	}
	level := flate.DefaultCompression
	if e != nil {
		level = e.level
	}
	encoded, err := deflate(ctx, frame, level)
	if err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	return pixeldata.EncodedFrame{Data: encoded}, nil
}

func supportedEncoderPhotometric(value string) bool {
	value = strings.ToUpper(strings.TrimSpace(value))
	for _, supported := range encoderPhotometricInterpretations {
		if value == supported {
			return true
		}
	}
	return false
}
