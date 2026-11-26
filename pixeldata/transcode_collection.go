package pixeldata

import (
	"context"
	"fmt"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/j2kpart2"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func jpeg2000Part2(uid string) bool {
	return uid == transfer.JPEG2000Part2Lossless.UID || uid == transfer.JPEG2000Part2.UID
}

func transcodeCollection(ctx context.Context, dataset *object.Object, frames *NativeFrames, resolved resolvedFrameEncoder, options TranscodeOptions, limits TranscodeLimits, report TranscodeReport) (*object.Object, TranscodeReport, error) {
	registered := resolved.encoder
	if entry, ok := registered.(registeredFrameEncoder); ok {
		registered = entry.encoder
	}
	encoder, ok := registered.(FrameCollectionEncoder)
	if !ok {
		return nil, TranscodeReport{}, fmt.Errorf("%w: JPEG 2000 Part 2 requires a collection encoder", ErrTranscodeUnsupported)
	}
	if !resolved.capabilities.Lossless && resolved.target.UID == transfer.JPEG2000Part2Lossless.UID {
		return nil, TranscodeReport{}, ErrEncoderCapabilitiesInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, TranscodeReport{}, err
	}
	m := frames.Metadata
	if len(frames.Data) < 2 || len(frames.Data) > j2kpart2.MaxFrames ||
		int64(m.Rows)*int64(m.Columns)*int64(len(frames.Data)) > j2kpart2.MaxSamples {
		return nil, TranscodeReport{}, transcodeLimitError("collection_samples")
	}
	encoded, err := callCollectionEncoder(ctx, encoder, frames)
	if err != nil {
		return nil, TranscodeReport{}, &EncoderEncodeError{Err: err, TransferSyntaxUID: resolved.target.UID}
	}
	if err := ctx.Err(); err != nil {
		return nil, TranscodeReport{}, err
	}
	if int64(len(encoded))+int64(len(encoded)&1) > limits.MaxOutputBytes {
		return nil, TranscodeReport{}, transcodeLimitError("encoded_collection_bytes")
	}
	layout, err := j2kpart2.Inspect(encoded)
	if err != nil || layout.Frames != m.NumberOfFrames || layout.Rows != int(m.Rows) || layout.Columns != int(m.Columns) ||
		layout.Bits != int(m.BitsStored) || layout.Signed != (m.PixelRepresentation == 1) || layout.Reversible != resolved.capabilities.Lossless {
		return nil, TranscodeReport{}, fmt.Errorf("%w: invalid Annex J collection", ErrEncoderOutputInvalid)
	}
	draft, encodedBytes, err := rewriteEncodedDataSet(ctx, encodedRewriteInput{
		source: dataset, metadata: m, fragments: [][]byte{encoded}, collection: true, limits: limits,
	})
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	result, err := detachDataSet(ctx, draft, limits)
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	report.EncodedBytes = encodedBytes
	report.Lossy = !resolved.capabilities.Lossless
	if report.Lossy && options.MaxPixelError != nil {
		start := time.Now()
		measurement, err := verifyLossyCollection(ctx, frames, result.object, resolved.target, options, limits)
		if err != nil {
			return nil, TranscodeReport{}, err
		}
		report.Timings.VerifyWorkerTime += time.Since(start)
		report.LossyVerification = measurement
		report.Verification = TranscodeVerificationPixelError
	}
	if report.Lossy {
		if err := finalizeLossyDataSet(result, m.TotalSize(), encodedBytes, resolved.capabilities.LossyMethod, options.UIDRoot); err != nil {
			return nil, TranscodeReport{}, err
		}
	}
	return result.object, report, nil
}

func callCollectionEncoder(ctx context.Context, encoder FrameCollectionEncoder, frames *NativeFrames) (data []byte, err error) {
	defer func() {
		if recover() != nil {
			data, err = nil, ErrEncoderOutputInvalid
		}
	}()
	data, err = encoder.EncodeFrameCollection(ctx, cloneFrames(frames.Data), frames.Metadata)
	return core.CloneBytes(data), err
}

func verifyLossyCollection(ctx context.Context, source *NativeFrames, output *object.Object, target transfer.Syntax, options TranscodeOptions, limits TranscodeLimits) (*LossyVerificationReport, error) {
	if err := CheckCodecAvailability(options.DecoderRegistry, target.UID); err != nil {
		return nil, err
	}
	native, _, err := DecompressDataSetContext(ctx, output, target, DecompressOptions{
		Registry: options.DecoderRegistry, TargetTransferSyntax: transfer.ExplicitVRLittleEndian,
		Limits: DecompressLimits{MaxFrames: limits.MaxFrames, MaxPixels: int64(limits.MaxPixels), MaxInputBytes: limits.MaxOutputBytes,
			MaxNativeBytes: limits.MaxOutputBytes, MaxExpansionRatio: limits.MaxExpansionRatio},
	})
	if err != nil {
		return nil, err
	}
	defer native.Close()
	decoded, err := ExtractNativeFramesView(native)
	if err != nil {
		return nil, err
	}
	if len(decoded.Data) != len(source.Data) {
		return nil, ErrTranscodeComparisonUnsupported
	}
	var aggregate *LossyVerificationReport
	for i, frame := range source.Data {
		measured, err := compareLossyFrame(ctx, frame, source.Metadata, decoded.Data[i], decoded.Metadata, options.AverageBlockSize)
		if err != nil {
			return nil, err
		}
		measured.Limit = *options.MaxPixelError
		if measured.MaximumError > measured.Limit {
			return nil, &LossyVerificationError{Frame: i + 1, Measured: measured.MaximumError, Limit: measured.Limit, AverageBlockSize: measured.AverageBlockSize}
		}
		addLossyVerification(&aggregate, &measured)
	}
	return aggregate, nil
}
