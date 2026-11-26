package storetranscode

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func incrementalProfile(source, target transfer.Syntax, options pixeldata.TranscodeOptions) bool {
	if target.Encapsulated && target.UID != transfer.EncapsulatedUncompressedExplicitVRLittleEndian.UID && options.EncoderRegistry != nil {
		if encoder, ok := options.EncoderRegistry.GetEncoder(target.UID); ok {
			if !encoder.Capabilities().Lossless {
				return false
			}
			if !options.SkipVerification && pixeldata.CheckCodecAvailability(options.DecoderRegistry, target.UID) != nil {
				return false
			}
		}
	}
	return source.IsLittleEndian() && !source.Deflated && target.IsLittleEndian() && !target.Deflated && !options.AllowLossy && !options.SecondaryCapture && options.UIDPolicy == pixeldata.TranscodeUIDDefault && options.UIDRoot == "" && options.Rewrite == nil && options.RecoverUN == nil && options.Recovery == (pixeldata.DecodeRecovery{}) && !options.CompressIcons
}

// prepareIncremental consumes an immutable bounded snapshot and retains the
// verified Part10 output. Sending skips framing without another output copy.
// Ineligibility is the only reason a caller may choose its resident route.
func prepareIncremental(ctx context.Context, inputPath string, inputOffset int64, outputPath string, source, target transfer.Syntax, options pixeldata.TranscodeOptions, dict dictionary.DataDictionary, budget *spoolBudget) (output *object.File, report pixeldata.TranscodeReport, outputOffset, outputBytes int64, err error) {
	if budget.remaining <= 0 {
		return nil, report, 0, 0, ErrResourceLimit
	}
	// Resident adapters historically serialize with WriteDataSet defaults. Keep
	// that policy instead of newly applying path-only serialization settings.
	options.WriteOptions = object.WriteFileOptions{}
	measured, err := pixeldata.TranscodeDataSetPath(ctx, inputPath, outputPath, source, target, options, pixeldata.DataSetPathOptions{SourceOffset: inputOffset, Dictionary: dict, MaxTemporaryBytes: budget.remaining})
	report = measured.TranscodeReport
	if err != nil {
		return nil, report, 0, 0, err
	}
	f, err := os.Open(outputPath)
	if err != nil {
		return nil, report, 0, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, report, 0, 0, err
	}
	if info.Size() > budget.remaining {
		_ = f.Close()
		return nil, report, 0, 0, ErrResourceLimit
	}
	budget.remaining -= info.Size()
	limits := options.Limits
	ro := object.ReadFileOptions{Dictionary: dict, DeferPixelData: true, MaxTotalBytes: limits.MaxOutputBytes, MaxElementBytes: limits.MaxOutputBytes, MaxPixelDataBytes: limits.MaxOutputBytes, MaxElements: limits.MaxElements, MaxFragments: limits.MaxFragments, MaxSequenceDepth: limits.MaxDepth}
	header, err := object.ReadPart10Header(&contextReadSeeker{ctx: ctx, ReadSeeker: f}, ro)
	if err == nil && (header.DataSetOffset < 0 || header.DataSetOffset >= info.Size() || header.TransferSyntax.UID != target.UID) {
		err = dimse.ErrStoreInvalidSource
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err == nil {
		output, err = object.ReadFileWithOptions(&contextReadSeeker{ctx: ctx, ReadSeeker: f}, ro)
	}
	if err != nil {
		_ = f.Close()
		return nil, report, 0, 0, err
	}
	// ReadFile borrows its reader; hold the file until the caller has inspected
	// the detached output metadata. It cannot outlive this preparation function.
	closeErr := f.Close()
	if closeErr != nil {
		return nil, report, 0, 0, errors.Join(closeErr, output.Close())
	}
	return output, report, header.DataSetOffset, info.Size() - header.DataSetOffset, nil
}
