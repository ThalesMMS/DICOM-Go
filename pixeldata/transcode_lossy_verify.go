package pixeldata

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	ErrTranscodeVerificationOptions   = errors.New("dicom: invalid lossy verification options")
	ErrTranscodePixelError            = errors.New("dicom: lossy pixel error exceeds limit")
	ErrTranscodeComparisonUnsupported = errors.New("dicom: lossy sample representations cannot be compared")
)

// LossyVerificationReport aggregates a successful comparison of newly encoded
// lossy frames. MaximumError is max(abs(mean(source)-mean(decoded))) across
// every square block, component and frame, in stored-sample units. A block of
// side one gives the maximum absolute sample error. Blocks includes partial
// right/bottom blocks and counts each component separately. MaximumSampleError
// always records the unaveraged maximum, even when block errors cancel.
// No modality rescale, palette, color-space conversion or display transform is
// applied by the comparator. This metric does not measure clinical adequacy.
type LossyVerificationReport struct {
	MaximumError       float64
	MaximumSampleError int64
	Limit              float64
	AverageBlockSize   int
	Frames             int
	Blocks             int64
}

// LossyVerificationError exposes only aggregate errors and the one-based frame
// number; it contains no sample values, patient attributes or backend text.
type LossyVerificationError struct {
	Frame            int
	Measured         float64
	Limit            float64
	AverageBlockSize int
}

func (e *LossyVerificationError) Error() string {
	if e == nil {
		return ErrTranscodePixelError.Error()
	}
	return fmt.Sprintf("%s: frame=%d measured=%g limit=%g stored-sample-units block-side=%d", ErrTranscodePixelError, e.Frame, e.Measured, e.Limit, e.AverageBlockSize)
}
func (*LossyVerificationError) Unwrap() error { return ErrTranscodePixelError }

// ResolveTranscodeVerificationOptions validates the optional lossy metric,
// copies its numeric limit and resolves an enabled zero block side to one.
// Other transcode policies and codec availability are validated by the actual
// operation. Adapters can use this to snapshot policy before later callbacks.
func ResolveTranscodeVerificationOptions(options TranscodeOptions) (TranscodeOptions, error) {
	if options.AverageBlockSize < 0 || options.MaxPixelError == nil && options.AverageBlockSize != 0 {
		return options, ErrTranscodeVerificationOptions
	}
	if options.MaxPixelError == nil {
		return options, nil
	}
	limit := *options.MaxPixelError
	if math.IsNaN(limit) || math.IsInf(limit, 0) || limit < 0 || options.SkipVerification {
		return options, ErrTranscodeVerificationOptions
	}
	options.MaxPixelError = &limit
	if options.AverageBlockSize == 0 {
		options.AverageBlockSize = 1
	}
	return options, nil
}

func addLossyVerification(aggregate **LossyVerificationReport, frame *LossyVerificationReport) {
	if frame == nil {
		return
	}
	if *aggregate == nil {
		copy := *frame
		*aggregate = &copy
		return
	}
	current := *aggregate
	current.MaximumError = max(current.MaximumError, frame.MaximumError)
	current.MaximumSampleError = max(current.MaximumSampleError, frame.MaximumSampleError)
	current.Frames += frame.Frames
	current.Blocks += frame.Blocks
}

// encodedFrameProxy borrows headers and encoded bytes for one bounded decode.
func encodedFrameProxy(header *object.Object, encoded []byte) *object.Object {
	elements := removeElements(header.Elements(), core.TagPixelData, tagNumberOfFrames)
	elements = append(elements,
		core.Element{Header: core.ElementHeader{Tag: tagNumberOfFrames, VR: core.VRIS}, Value: core.StringValue{"1"}},
		core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true}, Value: core.FragmentSequence{Fragments: [][]byte{encoded}}})
	proxy := object.FromElements(elements, nil)
	proxy.SetValueByteOrder(binary.LittleEndian)
	return proxy
}

// verifyTranscodedFrame shares the existing decode check: no second decoder
// call is made when lossy error measurement is enabled.
func verifyTranscodedFrame(ctx context.Context, sourceHeader, outputHeader *object.Object, encoded, native []byte, target transfer.Syntax, options TranscodeOptions, limits TranscodeLimits, frame int, lossless bool) (*LossyVerificationReport, error) {
	proxy := encodedFrameProxy(outputHeader, encoded)
	limits.MaxInputBytes = limits.MaxOutputBytes
	if lossless || options.MaxPixelError == nil {
		decoded, err := canonicalNativePixelBytes(ctx, proxy, target, options.DecoderRegistry, limits)
		if err != nil {
			return nil, err
		}
		if lossless && !bytes.Equal(decoded, native) {
			return nil, fmt.Errorf("%w: lossless Pixel Data changed", ErrIncompatiblePixelData)
		}
		return nil, nil
	}
	decoded, _, err := DecompressDataSetContext(ctx, proxy, target, DecompressOptions{
		Registry: options.DecoderRegistry, TargetTransferSyntax: transfer.ExplicitVRLittleEndian,
		Limits: DecompressLimits{MaxFrames: 1, MaxPixels: int64(limits.MaxPixels), MaxInputBytes: limits.MaxInputBytes,
			MaxNativeBytes: limits.MaxOutputBytes, MaxExpansionRatio: limits.MaxExpansionRatio},
	})
	if err != nil {
		return nil, err
	}
	defer decoded.Close()
	outputFrames, err := ExtractNativeFramesView(decoded)
	if err != nil {
		return nil, err
	}
	if len(outputFrames.Data) != 1 {
		return nil, ErrTranscodeComparisonUnsupported
	}
	sourceMetadata, err := ExtractMetadata(sourceHeader)
	if err != nil {
		return nil, err
	}
	measured, err := compareLossyFrame(ctx, native, sourceMetadata, outputFrames.Data[0], outputFrames.Metadata, options.AverageBlockSize)
	if err != nil {
		return nil, err
	}
	measured.Limit = *options.MaxPixelError
	if measured.MaximumError > measured.Limit {
		return nil, &LossyVerificationError{Frame: frame + 1, Measured: measured.MaximumError, Limit: measured.Limit, AverageBlockSize: measured.AverageBlockSize}
	}
	return &measured, nil
}

// compareLossyFrame interprets samples directly through the shared reader. It
// does not allocate a sample raster or keep per-block/per-sample results.
func compareLossyFrame(ctx context.Context, source []byte, a Metadata, decoded []byte, b Metadata, block int) (LossyVerificationReport, error) {
	if err := ctx.Err(); err != nil {
		return LossyVerificationReport{}, err
	}
	pi := normalizedPhotometric(a.PhotometricInterpretation)
	if a.Rows == 0 || a.Columns == 0 || a.Rows != b.Rows || a.Columns != b.Columns ||
		a.SamplesPerPixel != b.SamplesPerPixel || a.BitsStored != b.BitsStored || a.PixelRepresentation != b.PixelRepresentation ||
		pi != normalizedPhotometric(b.PhotometricInterpretation) || block < 1 {
		return LossyVerificationReport{}, ErrTranscodeComparisonUnsupported
	}
	if !(a.SamplesPerPixel == 1 && (pi == "MONOCHROME1" || pi == "MONOCHROME2") ||
		a.SamplesPerPixel == 3 && (pi == "RGB" || pi == "YBR_FULL")) ||
		a.PixelRepresentation > 1 || a.PlanarConfiguration > 1 || b.PlanarConfiguration > 1 {
		return LossyVerificationReport{}, ErrTranscodeComparisonUnsupported
	}
	ar, err := a.SampleFormat(binary.LittleEndian).Reader()
	if err != nil {
		return LossyVerificationReport{}, fmt.Errorf("%w: sample format", ErrTranscodeComparisonUnsupported)
	}
	br, err := b.SampleFormat(binary.LittleEndian).Reader()
	if err != nil {
		return LossyVerificationReport{}, fmt.Errorf("%w: sample format", ErrTranscodeComparisonUnsupported)
	}
	if int64(len(source)) != a.FrameSize() || int64(len(decoded)) != b.FrameSize() {
		return LossyVerificationReport{}, ErrPixelDataSizeMismatch
	}
	rows, cols, components := int(a.Rows), int(a.Columns), int(a.SamplesPerPixel)
	area := int64(min(block, rows)) * int64(min(block, cols))
	maxDifference := (int64(1) << a.BitsStored) - 1
	if maxDifference <= 0 || area > math.MaxInt64/maxDifference {
		return LossyVerificationReport{}, fmt.Errorf("%w: block accumulator range", ErrTranscodeComparisonUnsupported)
	}
	report := LossyVerificationReport{AverageBlockSize: block, Frames: 1}
	for y := 0; y < rows; {
		yEnd := y + min(block, rows-y)
		for x := 0; x < cols; {
			xEnd := x + min(block, cols-x)
			count := int64(yEnd-y) * int64(xEnd-x)
			for c := 0; c < components; c++ {
				var sum int64
				for by := y; by < yEnd; by++ {
					if err := ctx.Err(); err != nil {
						return LossyVerificationReport{}, err
					}
					for bx := x; bx < xEnd; bx++ {
						if bx&1023 == 0 {
							if err := ctx.Err(); err != nil {
								return LossyVerificationReport{}, err
							}
						}
						pixel := by*cols + bx
						ai := lossySampleIndex(pixel, c, rows*cols, components, a.PlanarConfiguration) * ar.Width()
						bi := lossySampleIndex(pixel, c, rows*cols, components, b.PlanarConfiguration) * br.Width()
						difference := ar.Value(source[ai:]) - br.Value(decoded[bi:])
						sum += difference
						if difference < 0 {
							difference = -difference
						}
						report.MaximumSampleError = max(report.MaximumSampleError, difference)
					}
				}
				if sum < 0 {
					sum = -sum
				}
				report.MaximumError = max(report.MaximumError, float64(sum)/float64(count))
				report.Blocks++
			}
			x = xEnd
		}
		y = yEnd
	}
	return report, ctx.Err()
}

func lossySampleIndex(pixel, component, pixels, components int, planar uint16) int {
	if components > 1 && planar == 1 {
		return component*pixels + pixel
	}
	return pixel*components + component
}
