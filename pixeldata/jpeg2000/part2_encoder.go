package jpeg2000

import (
	"context"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/internal/j2kpart2"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Part2Encoder writes one Annex J component collection covering all frames
// of an admitted monochrome image. It is safe for concurrent use.
type Part2Encoder struct {
	encoder *Encoder
	uid     string
}

// RegisterPart2Encoder adds one explicitly selected collection encoder to the
// caller's registry. Decoder registration remains a separate choice.
func RegisterPart2Encoder(registry pixeldata.EncoderRegistry, opts Options) error {
	if registry == nil {
		return pixeldata.ErrEncoderRegistryNil
	}
	encoder, err := NewPart2Encoder(opts)
	if err != nil {
		return err
	}
	return registry.RegisterEncoder(encoder.uid, encoder)
}

// NewPart2Encoder builds an explicit .92 reversible dependency or .93
// irreversible decorrelation encoder. Levels and StepSize have the same
// meaning as for Part 1. ColorTransform is not applicable across frames.
func NewPart2Encoder(opts Options) (*Part2Encoder, error) {
	uid := transfer.NormalizeUID(opts.TransferSyntaxUID)
	if opts.ColorTransform {
		return nil, unsupported("ColorTransform")
	}
	switch uid {
	case transfer.JPEG2000Part2Lossless.UID:
		opts.TransferSyntaxUID = transfer.JPEG2000LosslessOnly.UID
	case transfer.JPEG2000Part2.UID:
		opts.TransferSyntaxUID = transfer.JPEG2000.UID
	default:
		return nil, unsupported("TransferSyntaxUID")
	}
	encoder, err := NewEncoder(opts)
	if err != nil {
		return nil, err
	}
	return &Part2Encoder{encoder: encoder, uid: uid}, nil
}

func (e *Part2Encoder) Capabilities() pixeldata.EncoderCapabilities {
	if e == nil || e.encoder == nil {
		return pixeldata.EncoderCapabilities{}
	}
	caps := e.encoder.Capabilities()
	caps.TransferSyntaxUID = e.uid
	caps.SamplesPerPixel = []uint16{1}
	caps.PhotometricInterpretations = []string{"MONOCHROME1", "MONOCHROME2"}
	if !caps.Lossless {
		caps.BitsAllocated = []uint16{8}
	}
	return caps
}

// EncodeFrame is deliberately refused. A Part 2 codestream describes a
// collection, so encoding each frame independently would mislabel it.
func (e *Part2Encoder) EncodeFrame(ctx context.Context, _ []byte, _ pixeldata.Metadata) (pixeldata.EncodedFrame, error) {
	if ctx != nil && ctx.Err() != nil {
		return pixeldata.EncodedFrame{}, ctx.Err()
	}
	return pixeldata.EncodedFrame{}, unsupported("component collection required")
}

// EncodeFrameCollection accepts 2–16 monochrome frames with at most 2^21
// stored samples in total. Reversible input has 1–16 stored bits in 8- or
// 16-bit containers; irreversible input has eight bits in an 8-bit container.
// Both support signed and unsigned data. Output is an unpadded codestream.
func (e *Part2Encoder) EncodeFrameCollection(ctx context.Context, frames [][]byte, metadata pixeldata.Metadata) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e == nil || e.encoder == nil {
		return nil, pixeldata.ErrEncoderNil
	}
	if metadata.SamplesPerPixel != 1 {
		return nil, unsupported("SamplesPerPixel")
	}
	pi := strings.ToUpper(strings.TrimSpace(metadata.PhotometricInterpretation))
	if pi != "MONOCHROME1" && pi != "MONOCHROME2" {
		return nil, unsupported("PhotometricInterpretation")
	}
	if metadata.PlanarConfigurationPresent {
		return nil, unsupported("PlanarConfiguration")
	}
	if len(frames) != metadata.NumberOfFrames || len(frames) < 2 || len(frames) > j2kpart2.MaxFrames {
		return nil, unsupported("NumberOfFrames")
	}
	profile := j2kpart2.Profile{
		Frames: len(frames), Bits: int(metadata.BitsStored),
		Signed: metadata.PixelRepresentation == 1, Reversible: e.encoder.coding.reversible,
	}
	if !profile.Valid() || !profile.Reversible && metadata.BitsAllocated != 8 {
		return nil, unsupported("BitsStored")
	}
	samples := int64(metadata.Rows) * int64(metadata.Columns) * int64(len(frames))
	if samples > j2kpart2.MaxSamples {
		return nil, fmt.Errorf("%w: Annex J collection samples", pixeldata.ErrTranscodeResourceLimit)
	}
	// Check every native length before unpacking any collection storage.
	for _, frame := range frames {
		if int64(len(frame)) != metadata.FrameSize() {
			return nil, pixeldata.ErrPixelDataSizeMismatch
		}
	}
	img := image{width: int(metadata.Columns), height: int(metadata.Rows), depth: profile.CodedBits(), signed: true}
	img.planes = make([][]int32, len(frames))
	for i, frame := range frames {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		unpacked, err := e.encoder.unpack(frame, metadata)
		if err != nil {
			return nil, err
		}
		img.planes[i] = unpacked.planes[0]
	}
	// Work backwards so the predictor always sees the native predecessor.
	for i := len(img.planes) - 1; i > 0; i-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for j, value := range img.planes[i-1] {
			img.planes[i][j] -= value
		}
	}
	for j := range img.planes[0] {
		img.planes[0][j] -= profile.Shift()
	}
	coding := e.encoder.coding
	coding.annexJ = &profile
	return encodeImage(ctx, img, coding)
}
