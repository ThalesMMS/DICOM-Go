package pixeldata

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	// ErrInvalidRedaction reports a redaction request that does not fit the
	// image geometry or pixel format. It never echoes pixel or patient values.
	ErrInvalidRedaction = errors.New("dicom: invalid pixel redaction request")
	// ErrRedactionUnsupported reports pixel data that cannot be redacted
	// exactly (packed or float samples, subsampled native color, a missing
	// Pixel Data element).
	ErrRedactionUnsupported = errors.New("dicom: pixel redaction is unsupported for this pixel data")
	// ErrRedactionNotLossless reports that the redacted frames could not be
	// re-encoded in the source transfer syntax without loss while the caller
	// required it (RedactionOutputLosslessOnly), or that a re-encoded result
	// did not decode to exactly the redacted frames.
	ErrRedactionNotLossless = errors.New("dicom: redacted pixel data cannot be re-encoded losslessly")
)

// RedactionAllFrames as RedactionRegion.Frame applies a rectangle to every
// frame of the image.
const RedactionAllFrames = -1

// DefaultMaxRedactionRegions bounds the rectangles of one request when
// RedactionOptions.MaxRegions is zero.
const DefaultMaxRedactionRegions = 10_000

// RedactionRegion is a rectangle in stored pixel coordinates: X and Y are the
// zero-based column and row of its top-left pixel, Width and Height its size.
// Frame is zero-based, or RedactionAllFrames.
type RedactionRegion struct {
	Frame  int
	X      int
	Y      int
	Width  int
	Height int
}

// RedactionOutput selects how redacted encapsulated pixel data is stored.
// Native sources keep their transfer syntax (big-endian native becomes
// Explicit VR Little Endian). Redaction never re-encodes lossily: lossy
// encoders are never used, and lossy sources are stored native.
type RedactionOutput uint8

const (
	// RedactionOutputLosslessOrNative re-encodes in the source transfer
	// syntax when a lossless encoder for it is registered and otherwise
	// stores native Explicit VR Little Endian. It is the zero value.
	RedactionOutputLosslessOrNative RedactionOutput = iota
	// RedactionOutputLosslessOnly re-encodes in the source transfer syntax
	// with a lossless encoder or fails with ErrRedactionNotLossless.
	RedactionOutputLosslessOnly
	// RedactionOutputNative always stores native Explicit VR Little Endian.
	RedactionOutputNative
)

// RedactionOptions supplies codecs and policy for RedactDataSet.
type RedactionOptions struct {
	// DecoderRegistry decodes compressed sources and verifies re-encoded
	// output. Required for compressed sources.
	DecoderRegistry Registry
	// EncoderRegistry offers lossless encoders for the source syntax. Nil
	// means encapsulated sources are stored native (or fail under
	// RedactionOutputLosslessOnly).
	EncoderRegistry EncoderRegistry
	// Fill is the stored sample value written to every sample of every
	// redacted pixel. Zero, the default, is black for MONOCHROME2 and RGB.
	Fill   int64
	Output RedactionOutput
	Limits TranscodeLimits
	// MaxRegions bounds len(regions); zero uses DefaultMaxRedactionRegions.
	MaxRegions int
}

// RedactionCoverage summarizes which frames a request covers.
type RedactionCoverage struct {
	Frames        int
	FramesCovered int
	// Regions counts rectangles after RedactionAllFrames expansion.
	Regions int
}

// AllFramesCovered reports whether every frame has at least one rectangle.
func (coverage RedactionCoverage) AllFramesCovered() bool {
	return coverage.Frames > 0 && coverage.FramesCovered == coverage.Frames
}

// RedactionReport is the value-free result of RedactDataSet.
type RedactionReport struct {
	RedactionCoverage
	PixelsRedacted          int64
	SourceTransferSyntaxUID string
	OutputTransferSyntaxUID string
	// Reencoded is true when the output kept the encapsulated source syntax.
	Reencoded bool
	// DecodedToNative is true when encapsulated or big-endian pixel data was
	// stored native Explicit VR Little Endian.
	DecodedToNative bool
}

// ValidateRedactionRegions checks regions against the image geometry from
// metadata alone, so a dry run can validate a request before Pixel Data is
// read. It rejects rather than clips a rectangle outside the image.
func ValidateRedactionRegions(metadata Metadata, regions []RedactionRegion, maxRegions int) (RedactionCoverage, error) {
	if maxRegions <= 0 {
		maxRegions = DefaultMaxRedactionRegions
	}
	if len(regions) == 0 {
		return RedactionCoverage{}, fmt.Errorf("%w: no regions", ErrInvalidRedaction)
	}
	if len(regions) > maxRegions {
		return RedactionCoverage{}, fmt.Errorf("%w: %d regions exceed the limit of %d", ErrInvalidRedaction, len(regions), maxRegions)
	}
	frames := metadata.NumberOfFrames
	if frames <= 0 {
		frames = 1
	}
	rows, columns := int(metadata.Rows), int(metadata.Columns)
	if rows <= 0 || columns <= 0 {
		return RedactionCoverage{}, fmt.Errorf("%w: image has no rows or columns", ErrInvalidRedaction)
	}
	covered := make([]bool, frames)
	coverage := RedactionCoverage{Frames: frames}
	for index, region := range regions {
		if region.Width <= 0 || region.Height <= 0 || region.X < 0 || region.Y < 0 ||
			region.X >= columns || region.Y >= rows ||
			region.Width > columns-region.X || region.Height > rows-region.Y {
			return RedactionCoverage{}, fmt.Errorf("%w: region %d is outside the %dx%d image", ErrInvalidRedaction, index+1, columns, rows)
		}
		switch {
		case region.Frame == RedactionAllFrames:
			for frame := range covered {
				covered[frame] = true
			}
			coverage.Regions += frames
		case region.Frame >= 0 && region.Frame < frames:
			covered[region.Frame] = true
			coverage.Regions++
		default:
			return RedactionCoverage{}, fmt.Errorf("%w: region %d names a frame outside %d frame(s)", ErrInvalidRedaction, index+1, frames)
		}
	}
	for _, ok := range covered {
		if ok {
			coverage.FramesCovered++
		}
	}
	return coverage, nil
}

// RedactDataSet returns a detached copy of dataset whose operator-specified
// rectangles are overwritten with options.Fill in every sample, together with
// the transfer syntax the copy is encoded in. Compressed sources are decoded,
// redacted and either re-encoded with a lossless encoder for the source
// syntax (the result is decoded again and must equal the redacted frames
// exactly) or stored native, as options.Output selects; redaction never
// introduces lossy compression. dataset is borrowed and never mutated. Errors
// name only stable stages, region indexes and geometry, never values.
func RedactDataSet(ctx context.Context, dataset *object.Object, source transfer.Syntax, regions []RedactionRegion, options RedactionOptions) (*object.Object, transfer.Syntax, RedactionReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, transfer.Syntax{}, RedactionReport{}, err
	}
	if dataset == nil {
		return nil, transfer.Syntax{}, RedactionReport{}, fmt.Errorf("%w: dataset is nil", ErrMissingMetadata)
	}
	if options.Output > RedactionOutputNative {
		return nil, transfer.Syntax{}, RedactionReport{}, fmt.Errorf("%w: unknown output policy", ErrInvalidRedaction)
	}
	if dataset.Has(tagFloatPixelData) || dataset.Has(tagDoubleFloatPixelData) {
		return nil, transfer.Syntax{}, RedactionReport{}, fmt.Errorf("%w: float pixel data", ErrRedactionUnsupported)
	}
	element, ok := dataset.Get(core.TagPixelData)
	if !ok {
		return nil, transfer.Syntax{}, RedactionReport{}, fmt.Errorf("%w: missing Pixel Data", ErrRedactionUnsupported)
	}
	if element.Value == nil {
		return nil, transfer.Syntax{}, RedactionReport{}, fmt.Errorf("%w: Pixel Data is not loaded", ErrRedactionUnsupported)
	}
	limits, err := normalizeTranscodeLimits(options.Limits)
	if err != nil {
		return nil, transfer.Syntax{}, RedactionReport{}, err
	}
	source, err = canonicalSourceTransferSyntax(source)
	if err != nil {
		return nil, transfer.Syntax{}, RedactionReport{}, err
	}
	if source.MediaPayload {
		return nil, transfer.Syntax{}, RedactionReport{}, fmt.Errorf("%w: media payload transfer syntax", ErrRedactionUnsupported)
	}
	metadata, err := ExtractMetadata(dataset)
	if err != nil {
		return nil, transfer.Syntax{}, RedactionReport{}, err
	}
	coverage, err := ValidateRedactionRegions(metadata, regions, options.MaxRegions)
	if err != nil {
		return nil, transfer.Syntax{}, RedactionReport{}, err
	}

	nativeInPlace := !source.Encapsulated && source.IsLittleEndian()
	var native *object.Object
	if nativeInPlace {
		native, err = cloneDetachedObject(ctx, dataset, equivalentCloneLimits(limits))
	} else {
		native, _, err = TranscodeDataSet(ctx, dataset, source, transfer.ExplicitVRLittleEndian, TranscodeOptions{
			DecoderRegistry: options.DecoderRegistry,
			Limits:          options.Limits,
		})
	}
	if err != nil {
		return nil, transfer.Syntax{}, RedactionReport{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, transfer.Syntax{}, RedactionReport{}, err
	}
	redactedFrames, pixels, err := redactNativeObject(native, regions, options.Fill)
	if err != nil {
		return nil, transfer.Syntax{}, RedactionReport{}, err
	}
	report := RedactionReport{
		RedactionCoverage:       coverage,
		PixelsRedacted:          pixels,
		SourceTransferSyntaxUID: source.UID,
	}
	if nativeInPlace {
		report.OutputTransferSyntaxUID = source.UID
		return native, source, report, nil
	}
	if source.Encapsulated && options.Output != RedactionOutputNative {
		encoded, encodeErr := reencodeRedacted(ctx, native, source, redactedFrames, options, limits)
		switch {
		case encodeErr == nil:
			report.Reencoded = true
			report.OutputTransferSyntaxUID = source.UID
			return encoded, source, report, nil
		case ctx.Err() != nil:
			return nil, transfer.Syntax{}, RedactionReport{}, ctx.Err()
		case errors.Is(encodeErr, errRedactionVerification) || options.Output == RedactionOutputLosslessOnly:
			return nil, transfer.Syntax{}, RedactionReport{}, fmt.Errorf("%w: %v", ErrRedactionNotLossless, encodeErr)
		}
	}
	report.DecodedToNative = true
	report.OutputTransferSyntaxUID = transfer.ExplicitVRLittleEndian.UID
	return native, transfer.ExplicitVRLittleEndian, report, nil
}

var errRedactionVerification = errors.New("re-encoded pixel data does not decode to the redacted frames")

// reencodeRedacted encodes the redacted native data set in source with a
// lossless encoder and proves the result decodes to exactly redacted.
func reencodeRedacted(ctx context.Context, native *object.Object, source transfer.Syntax, redacted [][]byte, options RedactionOptions, limits TranscodeLimits) (*object.Object, error) {
	if source.UID != transfer.EncapsulatedUncompressedExplicitVRLittleEndian.UID {
		if options.EncoderRegistry == nil {
			return nil, ErrEncoderRegistryNil
		}
		encoder, ok := options.EncoderRegistry.GetEncoder(source.UID)
		if !ok {
			return nil, encoderAvailabilityError(ErrEncoderNotFound, source.UID, nil)
		}
		capabilities, err := encoderCapabilities(encoder)
		if err != nil {
			return nil, err
		}
		if !capabilities.Lossless {
			return nil, ErrTranscodeLossyDisallowed
		}
	}
	encoded, _, err := TranscodeDataSet(ctx, native, transfer.ExplicitVRLittleEndian, source, TranscodeOptions{
		DecoderRegistry: options.DecoderRegistry,
		EncoderRegistry: options.EncoderRegistry,
		Limits:          options.Limits,
	})
	if err != nil {
		return nil, err
	}
	verifyLimits := limits
	verifyLimits.MaxInputBytes = limits.MaxOutputBytes
	decoded, _, err := DecompressDataSetContext(ctx, encoded, source, DecompressOptions{
		Registry: options.DecoderRegistry,
		Limits: DecompressLimits{
			MaxFrames: verifyLimits.MaxFrames, MaxPixels: int64(verifyLimits.MaxPixels), MaxInputBytes: verifyLimits.MaxInputBytes,
			MaxNativeBytes: verifyLimits.MaxOutputBytes, MaxExpansionRatio: verifyLimits.MaxExpansionRatio,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errRedactionVerification, err)
	}
	frames, err := ExtractNativeFramesView(decoded)
	if err != nil || len(frames.Data) != len(redacted) {
		return nil, errRedactionVerification
	}
	for index := range redacted {
		if !bytes.Equal(frames.Data[index], redacted[index]) {
			return nil, errRedactionVerification
		}
	}
	return encoded, nil
}

// redactNativeObject fills regions in the little-endian native Pixel Data of
// obj, which it owns, and returns the redacted frames.
func redactNativeObject(obj *object.Object, regions []RedactionRegion, fill int64) ([][]byte, int64, error) {
	element, ok := obj.Get(core.TagPixelData)
	if !ok {
		return nil, 0, fmt.Errorf("%w: missing Pixel Data", ErrRedactionUnsupported)
	}
	native, err := ExtractNativeFrames(obj)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrRedactionUnsupported, err)
	}
	metadata := native.Metadata
	switch metadata.BitsAllocated {
	case 8, 16, 32:
	default:
		return nil, 0, fmt.Errorf("%w: BitsAllocated=%d", ErrRedactionUnsupported, metadata.BitsAllocated)
	}
	if isSubsampled422(metadata.PhotometricInterpretation) || normalizedPhotometric(metadata.PhotometricInterpretation) == "YBR_PARTIAL_420" {
		return nil, 0, fmt.Errorf("%w: subsampled native color", ErrRedactionUnsupported)
	}
	if metadata.PlanarConfigurationPresent && metadata.PlanarConfiguration > 1 {
		return nil, 0, fmt.Errorf("%w: PlanarConfiguration=%d", ErrRedactionUnsupported, metadata.PlanarConfiguration)
	}
	sample, err := redactionFillSample(metadata, fill)
	if err != nil {
		return nil, 0, err
	}
	frameCount := len(native.Data)
	rows, columns := int(metadata.Rows), int(metadata.Columns)
	samplesPerPixel := int(metadata.SamplesPerPixel)
	bytesPerSample := metadata.BytesPerSample()
	planar := samplesPerPixel > 1 && metadata.PlanarConfigurationPresent && metadata.PlanarConfiguration == 1
	frames := make([][]byte, frameCount)
	for index := range native.Data {
		frames[index] = core.CloneBytes(native.Data[index])
		if int64(len(frames[index])) < metadata.FrameSize() {
			return nil, 0, fmt.Errorf("%w: truncated frame %d", ErrRedactionUnsupported, index+1)
		}
	}
	var pixels int64
	fillRect := func(frame []byte, region RedactionRegion) {
		for y := region.Y; y < region.Y+region.Height; y++ {
			for x := region.X; x < region.X+region.Width; x++ {
				pixel := y*columns + x
				for s := 0; s < samplesPerPixel; s++ {
					var offset int
					if planar {
						offset = (s*rows*columns + pixel) * bytesPerSample
					} else {
						offset = (pixel*samplesPerPixel + s) * bytesPerSample
					}
					copy(frame[offset:offset+bytesPerSample], sample)
				}
			}
		}
		pixels += int64(region.Width) * int64(region.Height)
	}
	for _, region := range regions {
		if region.Frame == RedactionAllFrames {
			for _, frame := range frames {
				fillRect(frame, region)
			}
			continue
		}
		if region.Frame < 0 || region.Frame >= frameCount {
			return nil, 0, fmt.Errorf("%w: frame outside %d frame(s)", ErrInvalidRedaction, frameCount)
		}
		fillRect(frames[region.Frame], region)
	}
	total := 0
	for _, frame := range frames {
		total += len(frame)
	}
	joined := make([]byte, 0, total+1)
	for _, frame := range frames {
		joined = append(joined, frame...)
	}
	if len(joined)%2 != 0 {
		joined = append(joined, 0)
	}
	obj.Put(core.NewRawElement(core.TagPixelData, element.VR(), joined))
	return frames, pixels, nil
}

// redactionFillSample encodes fill as one little-endian stored sample after
// checking that it is representable in Bits Stored with the image's Pixel
// Representation.
func redactionFillSample(metadata Metadata, fill int64) ([]byte, error) {
	bitsStored := int(metadata.BitsStored)
	if bitsStored <= 0 || bitsStored > int(metadata.BitsAllocated) {
		return nil, fmt.Errorf("%w: BitsStored=%d", ErrRedactionUnsupported, bitsStored)
	}
	var low, high int64
	if metadata.PixelRepresentation == 1 {
		low, high = -(int64(1) << (bitsStored - 1)), int64(1)<<(bitsStored-1)-1
	} else {
		low, high = 0, int64(1)<<bitsStored-1
	}
	if fill < low || fill > high {
		return nil, fmt.Errorf("%w: fill value outside the %d-bit stored range", ErrInvalidRedaction, bitsStored)
	}
	bytesPerSample := metadata.BytesPerSample()
	sample := make([]byte, bytesPerSample)
	value := uint64(fill) // two's complement sign extension for signed samples
	for index := range sample {
		sample[index] = byte(value >> (8 * index))
	}
	return sample, nil
}
