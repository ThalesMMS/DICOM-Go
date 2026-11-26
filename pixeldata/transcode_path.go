package pixeldata

import (
	"context"
	"errors"
	"time"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// TranscodePath reads, transcodes, validates, and atomically publishes a Part
// 10 file. The source is never replaced and temporary files are private.
//
// A little-endian source whose frames can be located without decoding (native
// Pixel Data, one Item per frame, or a JPEG, JPEG-LS or JPEG 2000 codestream
// layout) is transcoded frame by frame: Pixel Data is read, decoded, encoded,
// verified in bounded windows, with ordered encoded frames spooled to a private
// file beside the destination. Native-target decoding preserves the serial
// one-frame write route for Workers=0/1; explicit parallel decoding uses a
// private ordered spool. Pipeline bounds apply to package-managed frame
// buffers, independently of the total disk spool/output limits. RecoverUN,
// Rewrite, Recovery, embedded
// overlays, big-endian or deflated syntaxes and other layouts take the
// in-memory route; both produce the same file. The limits still bound the
// whole input and output.
func TranscodePath(ctx context.Context, sourcePath, destinationPath string, target transfer.Syntax, options TranscodeOptions) (TranscodeReport, error) {
	if !transcodePathSupported() {
		return TranscodeReport{}, ErrTranscodeUnsupported
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return TranscodeReport{}, err
	}
	var err error
	options, err = snapshotTranscodeWriteOptions(options, target)
	if err != nil {
		return TranscodeReport{}, err
	}
	options, err = ResolveTranscodeVerificationOptions(options)
	if err != nil {
		return TranscodeReport{}, err
	}
	if err := validateTranscodePolicy(options); err != nil {
		return TranscodeReport{}, err
	}
	limits, err := normalizeTranscodeLimits(options.Limits)
	if err != nil {
		return TranscodeReport{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	if options.BeforeSource != nil {
		if err := options.BeforeSource(ctx); err != nil {
			return TranscodeReport{}, err
		}
	}
	txn, err := beginTranscodePathTxn(sourcePath, destinationPath)
	if err != nil {
		return TranscodeReport{}, err
	}
	defer txn.close()
	txn.durability = options.Durability
	txn.beforePublish = options.BeforePublish
	if report, err := transcodePathStream(ctx, txn, target, options, limits); !errors.Is(err, errStreamIneligible) {
		return report, err
	}
	readOptions := object.ReadFileOptions{
		MaxElementBytes:  limits.MaxInputBytes,
		MaxTotalBytes:    limits.MaxInputBytes,
		MaxSequenceDepth: limits.MaxDepth,
		MaxElements:      limits.MaxElements,
		MaxFragments:     limits.MaxFragments,
	}
	file, err := object.ReadFileWithOptions(&transcodeContextFile{File: txn.source, ctx: ctx}, readOptions)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return TranscodeReport{}, contextErr
		}
		return TranscodeReport{}, &TranscodeError{Stage: "read", Err: ErrTranscodeTransaction}
	}
	sourceSyntax, err := strictFileTransferSyntax(file)
	if err != nil {
		return TranscodeReport{}, err
	}
	targetSyntax, err := canonicalTargetTransferSyntax(target)
	if err != nil {
		return TranscodeReport{}, err
	}
	if err := validateTargetVerificationCodec(sourceSyntax, targetSyntax, options); err != nil {
		return TranscodeReport{}, err
	}
	output, report, err := TranscodeFile(ctx, file, target, options)
	if err != nil {
		return TranscodeReport{}, err
	}
	if !report.PixelDataPreserved && options.SkipVerification {
		report.Verification = TranscodeVerificationSkipped
	} else if !report.Lossy && !report.PixelDataPreserved {
		start := time.Now()
		if err := verifyLosslessTranscodeWithRepairs(ctx, file.Dataset, file.TransferSyntax, output.Dataset, output.TransferSyntax, options, limits); err != nil {
			return TranscodeReport{}, err
		}
		report.Timings.VerifyWorkerTime = time.Since(start)
		report.Verification = TranscodeVerificationPixelExact
	} else if report.Lossy && !report.PixelDataPreserved && report.LossyVerification == nil {
		start := time.Now()
		outputLimits := limits
		outputLimits.MaxInputBytes = limits.MaxOutputBytes
		if _, err := canonicalNativePixelBytes(ctx, output.Dataset, output.TransferSyntax, options.DecoderRegistry, outputLimits); err != nil {
			return TranscodeReport{}, err
		}
		report.Timings.VerifyWorkerTime = time.Since(start)
		report.Verification = TranscodeVerificationDecode
	}
	staged, err := stageTranscodeFileWithWriteOptions(ctx, txn, output, limits, options.WriteOptions)
	if err != nil {
		return TranscodeReport{}, err
	}
	defer staged.cleanup(txn.parent)
	if err := validateStagedTranscode(ctx, staged, limits); err != nil {
		return TranscodeReport{}, err
	}
	if err := ctx.Err(); err != nil {
		return TranscodeReport{}, err
	}
	if err := precommitTranscode(txn, staged); err != nil {
		return TranscodeReport{}, err
	}
	reportAvailable, err := publishStagedTranscode(ctx, txn, staged)
	applyTranscodePublicationReport(&report, txn)
	if err != nil {
		if reportAvailable {
			return report, err
		}
		return TranscodeReport{}, err
	}
	return report, nil
}
