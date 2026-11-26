package pixeldata

import (
	"context"
	"fmt"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type resolvedFrameEncoder struct {
	encoder      FrameEncoder
	capabilities EncoderCapabilities
	target       transfer.Syntax
}

type frameTransformInput struct {
	frames         *NativeFrames
	metadata       Metadata
	encoder        resolvedFrameEncoder
	maxOutputBytes int64
	verify         func(int, []byte, encodedMetadataDelta, []byte) (*LossyVerificationReport, error)
}

type transformedFrames struct {
	fragments            [][]byte
	delta                encodedMetadataDelta
	encodedBytes         int64
	lossless             bool
	lossyMethod          string
	verification         *LossyVerificationReport
	compatibilityProfile EncoderCompatibilityProfile
}

func resolveFrameEncoder(target transfer.Syntax, metadata Metadata, options TranscodeOptions) (resolvedFrameEncoder, error) {
	if target.MediaPayload || IsJPIPReferencedTransferSyntax(target.UID) {
		return resolvedFrameEncoder{}, fmt.Errorf("%w: target is not a still-image encoder syntax", ErrTranscodeUnsupported)
	}
	if options.EncoderRegistry == nil {
		return resolvedFrameEncoder{}, encoderAvailabilityError(ErrEncoderRegistryNil, target.UID, nil)
	}
	encoder, ok := options.EncoderRegistry.GetEncoder(target.UID)
	if !ok {
		return resolvedFrameEncoder{}, CheckEncoderAvailability(options.EncoderRegistry, target.UID)
	}
	capabilities, err := encoderCapabilities(encoder)
	if err != nil || transfer.NormalizeUID(capabilities.TransferSyntaxUID) != target.UID || !validEncoderCapabilities(capabilities) {
		return resolvedFrameEncoder{}, ErrEncoderCapabilitiesInvalid
	}
	if !capabilities.Lossless && !options.AllowLossy {
		return resolvedFrameEncoder{}, ErrTranscodeLossyDisallowed
	}
	if !capabilities.Lossless && options.UIDPolicy == TranscodeUIDPreserve {
		return resolvedFrameEncoder{}, fmt.Errorf("%w: a new lossy generation requires a new UID", ErrTranscodeUnsupported)
	}
	if err := validateEncoderMetadata(capabilities, metadata); err != nil {
		return resolvedFrameEncoder{}, err
	}
	return resolvedFrameEncoder{encoder: encoder, capabilities: capabilities, target: target}, nil
}

func transcodeWithEncoder(ctx context.Context, dataset *object.Object, metadata Metadata, target transfer.Syntax, options TranscodeOptions, limits TranscodeLimits, report TranscodeReport) (*object.Object, TranscodeReport, error) {
	resolved, err := resolveFrameEncoder(target, metadata, options)
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	frames, err := ExtractNativeFramesView(dataset)
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	if jpeg2000Part2(target.UID) {
		return transcodeCollection(ctx, dataset, frames, resolved, options, limits, report)
	}
	input := frameTransformInput{
		frames:         frames,
		metadata:       metadata,
		encoder:        resolved,
		maxOutputBytes: limits.MaxOutputBytes,
	}
	if !resolved.capabilities.Lossless && options.MaxPixelError != nil {
		if err := CheckCodecAvailability(options.DecoderRegistry, target.UID); err != nil {
			return nil, TranscodeReport{}, err
		}
		input.verify = func(i int, encoded []byte, delta encodedMetadataDelta, native []byte) (*LossyVerificationReport, error) {
			header := object.FromElements(encodedMetadataElements(dataset, &delta), nil)
			header.SetValueByteOrder(dataset.ValueByteOrder())
			start := time.Now()
			measured, err := verifyTranscodedFrame(ctx, dataset, header, encoded, native, target, options, limits, i, false)
			report.Timings.VerifyWorkerTime += time.Since(start)
			return measured, err
		}
	}
	transformed, err := transformFrames(ctx, input)
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	draft, encodedBytes, err := rewriteEncodedDataSet(ctx, encodedRewriteInput{
		source: dataset, metadata: metadata, fragments: transformed.fragments, delta: &transformed.delta, limits: limits,
	})
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	result, err := detachDataSet(ctx, draft, limits)
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	report.EncodedBytes = encodedBytes
	report.Lossy = !transformed.lossless
	report.LossyVerification = transformed.verification
	report.EncodeCompatibilityProfiles = encoderCompatibilityProfiles(transformed.compatibilityProfile)
	if transformed.verification != nil {
		report.Verification = TranscodeVerificationPixelError
	}
	if report.Lossy {
		if err := finalizeLossyDataSet(result, metadata.TotalSize(), encodedBytes, transformed.lossyMethod, options.UIDRoot); err != nil {
			return nil, TranscodeReport{}, err
		}
	}
	return result.object, report, nil
}

func transformFrames(ctx context.Context, input frameTransformInput) (transformedFrames, error) {
	fragments := make([][]byte, len(input.frames.Data))
	var state frameEncodeState
	var verification *LossyVerificationReport
	for i, frame := range input.frames.Data {
		encoded, err := state.encode(ctx, input.encoder, input.metadata, input.maxOutputBytes, i, frame)
		if err != nil {
			return transformedFrames{}, err
		}
		fragments[i] = encoded
		if input.verify != nil {
			measured, err := input.verify(i, encoded, state.delta(), frame)
			if err != nil {
				return transformedFrames{}, err
			}
			addLossyVerification(&verification, measured)
		}
	}
	return transformedFrames{
		fragments:            fragments,
		delta:                state.delta(),
		encodedBytes:         state.encodedTotal,
		lossless:             input.encoder.capabilities.Lossless,
		lossyMethod:          input.encoder.capabilities.LossyMethod,
		verification:         verification,
		compatibilityProfile: state.compatibilityProfile,
	}, nil
}

// frameEncodeState encodes frames one at a time, checking the output budget
// and that every frame reports the metadata of the first; the in-memory and
// streaming transcoders share it.
type frameEncodeState struct {
	encodedTotal         int64
	outputPI             string
	outputPlanar         *uint16
	compatibilityProfile EncoderCompatibilityProfile
}

// encode returns frame i encoded, detached from encoder memory.
func (s *frameEncodeState) encode(ctx context.Context, encoder resolvedFrameEncoder, metadata Metadata, maxOutputBytes int64, i int, frame []byte) ([]byte, error) {
	encoded, err := encodeTranscodeFrame(ctx, encoder, metadata, maxOutputBytes, frame)
	if err != nil {
		return nil, err
	}
	if err := s.accept(metadata, encoder.capabilities, maxOutputBytes, i, encoded); err != nil {
		return nil, err
	}
	return encoded.Data, nil
}

// encodeTranscodeFrame validates one frame without shared aggregate state.
// Its returned bytes and metadata are detached before ordered consumption.
func encodeTranscodeFrame(ctx context.Context, encoder resolvedFrameEncoder, metadata Metadata, maxFrameBytes int64, frame []byte) (EncodedFrame, error) {
	if err := ctx.Err(); err != nil {
		return EncodedFrame{}, err
	}
	encoded, err := callFrameEncoder(ctx, encoder.encoder, frame, metadata)
	if err != nil {
		return EncodedFrame{}, &EncoderEncodeError{Err: err, TransferSyntaxUID: encoder.target.UID}
	}
	if err := ctx.Err(); err != nil {
		return EncodedFrame{}, err
	}
	if len(encoded.Data) == 0 {
		return EncodedFrame{}, ErrEncoderOutputInvalid
	}
	encoded, err = validateEncodedMetadataDelta(metadata, encoder.capabilities, encoded)
	if err != nil {
		return EncodedFrame{}, err
	}
	if int64(len(encoded.Data)) > maxFrameBytes {
		return EncodedFrame{}, transcodeLimitError("encoded_frame_bytes")
	}
	encoded.Data = core.CloneBytes(encoded.Data)
	return encoded, nil
}

func (s *frameEncodeState) accept(metadata Metadata, capabilities EncoderCapabilities, maxOutputBytes int64, i int, encoded EncodedFrame) error {
	var err error
	encoded, err = validateEncodedMetadataDelta(metadata, capabilities, encoded)
	if err != nil {
		return err
	}
	if int64(len(encoded.Data)) > maxOutputBytes-s.encodedTotal {
		return transcodeLimitError("output_bytes")
	}
	s.encodedTotal += int64(len(encoded.Data))
	if i == 0 {
		s.compatibilityProfile = encoded.CompatibilityProfile
		s.outputPI = encoded.PhotometricInterpretation
		if encoded.PlanarConfiguration != nil {
			value := *encoded.PlanarConfiguration
			s.outputPlanar = &value
		}
	} else if s.outputPI != encoded.PhotometricInterpretation || !sameOptionalUint16(s.outputPlanar, encoded.PlanarConfiguration) || s.compatibilityProfile != encoded.CompatibilityProfile {
		return fmt.Errorf("%w: inconsistent encoder metadata", ErrEncoderOutputInvalid)
	}
	return nil
}

func (s *frameEncodeState) delta() encodedMetadataDelta {
	return encodedMetadataDelta{photometric: s.outputPI, planar: s.outputPlanar}
}

func validateEncodedMetadataDelta(metadata Metadata, capabilities EncoderCapabilities, encoded EncodedFrame) (EncodedFrame, error) {
	if err := validateEncoderCompatibility(capabilities, encoded); err != nil {
		return EncodedFrame{}, err
	}
	photometric := normalizedPhotometric(encoded.PhotometricInterpretation)
	if photometric != "" {
		if photometric != normalizedPhotometric(metadata.PhotometricInterpretation) && !containsPhotometric(capabilities.OutputPhotometricInterpretations, photometric) {
			return EncodedFrame{}, fmt.Errorf("%w: PhotometricInterpretation transform is undeclared", ErrEncoderOutputInvalid)
		}
		switch metadata.SamplesPerPixel {
		case 1:
			if photometric != "MONOCHROME1" && photometric != "MONOCHROME2" && photometric != "PALETTE COLOR" {
				return EncodedFrame{}, fmt.Errorf("%w: PhotometricInterpretation", ErrEncoderOutputInvalid)
			}
		case 3:
			switch photometric {
			case "RGB", "YBR_FULL", "YBR_FULL_422", "YBR_RCT", "YBR_ICT":
			default:
				return EncodedFrame{}, fmt.Errorf("%w: PhotometricInterpretation", ErrEncoderOutputInvalid)
			}
		default:
			return EncodedFrame{}, fmt.Errorf("%w: SamplesPerPixel", ErrEncoderOutputInvalid)
		}
		encoded.PhotometricInterpretation = photometric
	}
	if encoded.PlanarConfiguration != nil {
		if metadata.SamplesPerPixel == 1 || *encoded.PlanarConfiguration > 1 {
			return EncodedFrame{}, fmt.Errorf("%w: PlanarConfiguration", ErrEncoderOutputInvalid)
		}
		if !metadata.PlanarConfigurationPresent || *encoded.PlanarConfiguration != metadata.PlanarConfiguration && !containsUint16(capabilities.OutputPlanarConfigurations, *encoded.PlanarConfiguration) {
			return EncodedFrame{}, fmt.Errorf("%w: PlanarConfiguration transform is undeclared", ErrEncoderOutputInvalid)
		}
		value := *encoded.PlanarConfiguration
		encoded.PlanarConfiguration = &value
	}
	return encoded, nil
}

func containsPhotometric(values []string, want string) bool {
	for _, value := range values {
		if normalizedPhotometric(value) == want {
			return true
		}
	}
	return false
}

func sameOptionalUint16(a, b *uint16) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
