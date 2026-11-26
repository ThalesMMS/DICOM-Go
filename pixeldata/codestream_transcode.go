package pixeldata

import (
	"context"
	"errors"
	"fmt"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// CodestreamTranscoder rewrites the encoded frames of one encapsulated
// transfer syntax as another without decoding pixels, for conversions that
// are exact at the codestream level, such as reversible JPEG to JPEG XL
// recompression and its JPEG reconstruction. Implementations must be safe
// for concurrent use and must treat pixel and obj as borrowed, read-only
// inputs; returned fragments must own their storage.
type CodestreamTranscoder interface {
	// CanTranscodeCodestream reports whether frames encoded under sourceUID
	// can be rewritten under targetUID.
	CanTranscodeCodestream(sourceUID, targetUID string) bool
	// TranscodeCodestreams returns exactly one encoded frame per frame of
	// pixel, in order. The pixels and Image Pixel metadata are unchanged.
	TranscodeCodestreams(ctx context.Context, sourceUID, targetUID string, pixel PixelData, obj *object.Object) ([][]byte, error)
}

// ErrCodestreamTranscoderOutputInvalid reports a CodestreamTranscoder that
// returned a frame count or frame that cannot be stored.
var ErrCodestreamTranscoderOutputInvalid = errors.New("dicom: codestream transcoder returned invalid output")

// usesCodestreamTranscoder reports whether options select a codestream-level
// conversion for plan; ForceReencode always takes the pixel route.
func usesCodestreamTranscoder(plan transcodePlan, options TranscodeOptions) bool {
	return plan.mode == transcodeDecodeEncode && !options.ForceReencode && !requiresLegacyPixelRepair(options.Recovery) &&
		options.CodestreamTranscoder != nil && plan.source.Encapsulated &&
		plan.source.UID != transfer.EncapsulatedUncompressedExplicitVRLittleEndian.UID &&
		options.CodestreamTranscoder.CanTranscodeCodestream(plan.source.UID, plan.target.UID)
}

// transcodeCodestreams replaces the encoded frames of dataset with the
// transcoder's output, keeping every other attribute. No pixel is decoded,
// so the result is neither lossy nor re-encoded.
func transcodeCodestreams(ctx context.Context, dataset *object.Object, metadata Metadata, pixel PixelData, plan transcodePlan, options TranscodeOptions, limits TranscodeLimits) (*object.Object, TranscodeReport, error) {
	fragments, err := callCodestreamTranscoder(ctx, options.CodestreamTranscoder, plan, pixel, dataset)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, TranscodeReport{}, ctxErr
		}
		return nil, TranscodeReport{}, &TranscodeError{Stage: "codestream", Err: err}
	}
	if len(fragments) != metadata.NumberOfFrames {
		return nil, TranscodeReport{}, &TranscodeError{Stage: "codestream", Err: fmt.Errorf("%w: %d frames for NumberOfFrames=%d", ErrCodestreamTranscoderOutputInvalid, len(fragments), metadata.NumberOfFrames)}
	}
	for i, fragment := range fragments {
		if len(fragment) == 0 {
			return nil, TranscodeReport{}, &TranscodeError{Stage: "codestream", Err: fmt.Errorf("%w: empty frame %d", ErrCodestreamTranscoderOutputInvalid, i)}
		}
	}
	draft, encodedBytes, err := rewriteEncodedDataSet(ctx, encodedRewriteInput{
		source: dataset, metadata: metadata, fragments: fragments, limits: limits,
	})
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	result, err := detachDataSet(ctx, draft, limits)
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	return result.object, TranscodeReport{
		Frames:       metadata.NumberOfFrames,
		NativeBytes:  metadata.TotalSize(),
		EncodedBytes: encodedBytes,
	}, nil
}

func callCodestreamTranscoder(ctx context.Context, transcoder CodestreamTranscoder, plan transcodePlan, pixel PixelData, dataset *object.Object) (fragments [][]byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			fragments, err = nil, fmt.Errorf("%w: panic", ErrCodestreamTranscoderOutputInvalid)
		}
	}()
	return transcoder.TranscodeCodestreams(ctx, plan.source.UID, plan.target.UID, pixel, dataset)
}
