package pixeldata

import (
	"context"
	"errors"
	"io"

	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// ErrTranscodeStreamingIneligible identifies a raw dataset which cannot use the
// qualified incremental route. It is returned before output staging or codec
// execution; it is not a codec, verification, cancellation or quota failure.
var ErrTranscodeStreamingIneligible = errors.New("pixeldata: raw dataset needs resident transcode route")

// DataSetPathOptions supplies framing and a live aggregate temporary-file bound.
// SourceOffset skips caller-owned Part 10 framing when a snapshot contains it.
// MaxTemporaryBytes excludes the caller-owned input; zero selects 2 GiB. Caller
// budgets must additionally charge input snapshots and retained handles.
type DataSetPathOptions struct {
	SourceOffset      int64
	Dictionary        dictionary.DataDictionary
	MaxTemporaryBytes int64
}

// DataSetPathReport reports the same transformation plus observed logical peak
// temporary-file bytes. It is not process RSS or frame-buffer memory; Pipeline
// limits independently bound frames in flight.
type DataSetPathReport struct {
	TranscodeReport
	PeakTemporaryBytes int64
}

// TranscodeDataSetPath incrementally prepares and atomically publishes a Part
// 10 file from stable raw dataset bytes. It shares TranscodePath's planner,
// frame pipeline, verification, output validation and publication. It never
// falls back to resident Pixel Data. A caller may explicitly select its prior
// resident route only on ErrTranscodeStreamingIneligible. Lossy and identity-
// changing policies are outside this entry point's initial qualified profile.
func TranscodeDataSetPath(ctx context.Context, sourcePath, destinationPath string, source, target transfer.Syntax, options TranscodeOptions, dataSetOptions DataSetPathOptions) (result DataSetPathReport, err error) {
	if !transcodePathSupported() {
		return result, ErrTranscodeUnsupported
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if dataSetOptions.SourceOffset < 0 || dataSetOptions.MaxTemporaryBytes < 0 {
		return result, ErrTranscodeResourceLimit
	}
	if options.AllowLossy || options.SecondaryCapture || options.UIDPolicy != TranscodeUIDDefault || options.UIDRoot != "" {
		return result, ErrTranscodeStreamingIneligible
	}
	source, ok := transfer.DefaultRegistry.Get(transfer.NormalizeUID(source.UID))
	if !ok {
		return result, ErrTranscodeUnsupported
	}
	target, err = canonicalTargetTransferSyntax(target)
	if err != nil {
		return result, err
	}
	if target.Encapsulated && target.UID != transfer.EncapsulatedUncompressedExplicitVRLittleEndian.UID && options.EncoderRegistry != nil {
		if encoder, ok := options.EncoderRegistry.GetEncoder(target.UID); ok && !encoder.Capabilities().Lossless {
			return result, ErrTranscodeStreamingIneligible
		}
	}
	if !source.IsLittleEndian() || source.Deflated || !target.IsLittleEndian() || target.Deflated || options.RecoverUN != nil || options.Rewrite != nil || options.Recovery != (DecodeRecovery{}) || options.CompressIcons {
		return result, ErrTranscodeStreamingIneligible
	}
	options, err = snapshotTranscodeWriteOptions(options, target)
	if err != nil {
		return result, err
	}
	options, err = ResolveTranscodeVerificationOptions(options)
	if err != nil {
		return result, err
	}
	if err := validateTranscodePolicy(options); err != nil {
		return result, err
	}
	limits, err := normalizeTranscodeLimits(options.Limits)
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	txn, err := beginTranscodePathTxn(sourcePath, destinationPath)
	if err != nil {
		return result, err
	}
	defer txn.close()
	if dataSetOptions.SourceOffset >= txn.sourceInfo.Size() || txn.sourceInfo.Size()-dataSetOptions.SourceOffset > limits.MaxInputBytes {
		return result, ErrTranscodeResourceLimit
	}
	maximum := dataSetOptions.MaxTemporaryBytes
	if maximum == 0 {
		maximum = 2 << 30
	}
	txn.rawInput = &rawTranscodeInput{syntax: source, dictionary: dataSetOptions.Dictionary, reader: io.NewSectionReader(txn.source, dataSetOptions.SourceOffset, txn.sourceInfo.Size()-dataSetOptions.SourceOffset)}
	txn.temporary = &transcodeTemporaryBudget{maximum: maximum}
	txn.durability = options.Durability
	report, err := transcodePathStream(ctx, txn, target, options, limits)
	result.TranscodeReport = report
	result.PeakTemporaryBytes = txn.temporary.peak
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if errors.Is(err, errStreamIneligible) {
		return result, ErrTranscodeStreamingIneligible
	}
	return result, err
}

type rawTranscodeInput struct {
	syntax     transfer.Syntax
	dictionary dictionary.DataDictionary
	reader     *io.SectionReader
}

type transcodeTemporaryBudget struct{ maximum, used, peak int64 }

func (b *transcodeTemporaryBudget) reserve(n int64) error {
	if n < 0 || n > b.maximum-b.used {
		return ErrTranscodeResourceLimit
	}
	b.used += n
	if b.used > b.peak {
		b.peak = b.used
	}
	return nil
}

func (s *stagedTranscodeFile) reserveAppend(n int64) error {
	if s.temporary == nil {
		return nil
	}
	if err := s.temporary.reserve(n); err != nil {
		return err
	}
	s.charged += n
	return nil
}

type transcodeTemporaryWriter struct{ staged *stagedTranscodeFile }

func (w transcodeTemporaryWriter) Write(data []byte) (int, error) {
	if err := w.staged.reserveAppend(int64(len(data))); err != nil {
		return 0, err
	}
	return w.staged.file.Write(data)
}

type contextSectionReader struct {
	ctx context.Context
	*io.SectionReader
}

func (r *contextSectionReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.SectionReader.Read(p)
}
func (r *contextSectionReader) Seek(offset int64, whence int) (int64, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.SectionReader.Seek(offset, whence)
}

func readRawTranscodeFile(ctx context.Context, input *rawTranscodeInput, limits TranscodeLimits) (*object.File, error) {
	dataset, err := object.ReadDataSetWithOptions(&contextSectionReader{ctx: ctx, SectionReader: input.reader}, input.syntax, object.ReadFileOptions{
		Dictionary: input.dictionary, DeferPixelData: true, MaxTotalBytes: limits.MaxInputBytes,
		MaxElementBytes: limits.MaxInputBytes, MaxPixelDataBytes: limits.MaxInputBytes,
		MaxSequenceDepth: limits.MaxDepth, MaxElements: limits.MaxElements, MaxFragments: limits.MaxFragments,
	})
	if err != nil {
		return nil, err
	}
	return &object.File{Dataset: dataset, TransferSyntax: input.syntax}, nil
}
