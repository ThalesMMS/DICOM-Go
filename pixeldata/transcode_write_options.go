package pixeldata

import (
	"context"
	"io"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func snapshotTranscodeWriteOptions(options TranscodeOptions, target transfer.Syntax) (TranscodeOptions, error) {
	target, err := canonicalTargetTransferSyntax(target)
	if err != nil {
		return options, err
	}
	if options.WriteOptions.DeflateLevel != nil {
		level := *options.WriteOptions.DeflateLevel
		options.WriteOptions.DeflateLevel = &level
	}
	if options.WriteOptions.Preamble != nil {
		options.WriteOptions.Preamble = append([]byte(nil), options.WriteOptions.Preamble...)
	}
	if err := object.ValidateSerializationOptions(target, options.WriteOptions.DeflateLevel, options.WriteOptions.Padding); err != nil {
		return options, err
	}
	return options, nil
}

func stageTranscodeFileWithWriteOptions(ctx context.Context, txn *transcodePathTxn, output *object.File, limits TranscodeLimits, options object.WriteFileOptions) (*stagedTranscodeFile, error) {
	return stageTranscodeWith(ctx, txn, limits, func(w io.Writer) error { return object.WriteFileWithOptions(w, output, options) })
}
