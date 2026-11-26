package pixeldata

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/nofollow"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/validation"
)

// ErrTrailingDataNotRepairable means no safe, byte-preserving cut was found.
var ErrTrailingDataNotRepairable = errors.New("dicom: no repairable trailing data after complete Pixel Data")

// ErrTrailingDataRepairLimit means the repair exceeded a resource limit.
var ErrTrailingDataRepairLimit = errors.New("dicom: trailing data repair resource limit")

const (
	repairInputLimit    = 2 << 30
	repairMetadataLimit = 64 << 20
	repairElementLimit  = 4 << 20
	repairMaxElements   = 100_000
	repairMaxFragments  = 100_000
	repairMaxDepth      = 64
	repairMaxTokens     = 500_000
)

// TrailingDataRepairOptions bounds the source and staged copy size. The zero
// value caps input at 2 GiB. A positive MaxInputBytes can lower that cap.
type TrailingDataRepairOptions struct {
	MaxInputBytes int64
}

// TrailingDataRepairReport contains byte counts without source values.
// Published distinguishes a complete copy with a subsequent durability error
// from an operation that never created the destination.
type TrailingDataRepairReport struct {
	KeptBytes    int64
	RemovedBytes int64
	Published    bool
}

// RepairPart10TrailingData creates a byte-exact prefix copy at a new path
// when a malformed root header immediately follows complete integer Pixel
// Data. It uses the declared, non-Deflated transfer syntax and verifies the
// staged copy with a strict Part 10 read before publishing it create-only.
// Encapsulated data is checked for framing, without decoding its codestream.
// The original must remain stable and the caller must own mutation of the
// destination directory during the operation.
func RepairPart10TrailingData(ctx context.Context, sourcePath, destinationPath string, options TrailingDataRepairOptions) (report TrailingDataRepairReport, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	limit := options.MaxInputBytes
	if limit < 0 || limit > repairInputLimit {
		return report, ErrTrailingDataRepairLimit
	}
	if limit == 0 {
		limit = repairInputLimit
	}

	txn, err := beginTranscodePathTxn(sourcePath, destinationPath)
	if err != nil {
		return report, err
	}
	defer txn.close()
	if txn.previous.exists {
		return report, os.ErrExist
	}
	size := txn.sourceInfo.Size()
	if size > limit {
		return report, ErrTrailingDataRepairLimit
	}
	readOptions := object.ReadFileOptions{
		MaxTotalBytes: limit, MaxElementBytes: repairElementLimit, MaxPixelDataBytes: limit,
		MaxElements: repairMaxElements, MaxFragments: repairMaxFragments, MaxSequenceDepth: repairMaxDepth,
		SkipPixelData: true, DeferPixelData: true, StrictReservedBytes: true,
	}
	source := &transcodeContextFile{File: txn.source, ctx: ctx}
	intact, err := findRepairBoundary(ctx, source, size, readOptions)
	if err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}

	txn.durability = TranscodeDurable
	staged, err := stageTranscodeWith(ctx, txn, TranscodeLimits{MaxOutputBytes: intact}, func(w io.Writer) error {
		_, err := io.CopyBuffer(w, io.NewSectionReader(source, 0, intact), make([]byte, 64<<10))
		return err
	})
	if err != nil {
		return report, err
	}
	defer staged.cleanup(txn.parent)
	stagedSource := &repairMetadataFile{transcodeContextFile: transcodeContextFile{File: staged.file, ctx: ctx}, remaining: repairMetadataLimit}
	file, err := object.ReadFileWithOptions(stagedSource, readOptions)
	if err != nil {
		return report, repairReadFailure(ctx, err)
	}
	if err := verifyRepairPrefix(file, intact); err != nil {
		return report, err
	}
	if err := precommitTranscode(txn, staged); err != nil {
		return report, err
	}
	info, err := staged.file.Stat()
	if err != nil {
		return report, err
	}
	if info.Size() != intact {
		return report, ErrTrailingDataNotRepairable
	}
	// PublishClosedAt needs a closed file. Keep the original identity for
	// cleanup because staged.cleanup cannot stat an already closed handle.
	staged.open, staged.entryOwned = false, false
	defer func() {
		if sameTranscodeEntryIdentityAt(txn.parent, staged.name, info) {
			err = errors.Join(err, nofollow.RemoveAt(txn.parent, staged.name))
		}
	}()
	if err := staged.file.Close(); err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	published, err := nofollow.PublishClosedAt(txn.parent, staged.name, txn.destinationName, info)
	if published {
		report = TrailingDataRepairReport{KeptBytes: intact, RemovedBytes: size - intact, Published: true}
		err = errors.Join(err, syncTranscodeDirectory(txn.parent))
	} else if err == nil {
		err = errors.New("dicom: trailing data repair publication did not create a file")
	}
	return report, err
}

// findRepairBoundary reuses bounded parser tokens, which distinguish root
// Pixel Data from icons and advance over actual item lengths and delimiters.
func findRepairBoundary(ctx context.Context, source *transcodeContextFile, size int64, opts object.ReadFileOptions) (int64, error) {
	header, err := object.ReadPart10Header(source, opts)
	if err != nil {
		return 0, repairReadFailure(ctx, err)
	}
	if header.TransferSyntax.Deflated {
		return 0, ErrTrailingDataNotRepairable
	}
	parserOptions := parser.ReaderOptions{
		Dictionary: std.Dictionary, BaseOffset: header.DataSetOffset, StrictReservedBytes: true,
		MaxTotalBytes: opts.MaxTotalBytes, MaxElementBytes: opts.MaxElementBytes, MaxPixelDataBytes: opts.MaxPixelDataBytes,
		MaxElements: opts.MaxElements, MaxFragments: opts.MaxFragments, MaxSequenceDepth: opts.MaxSequenceDepth,
		SkipPixelData: true,
	}
	var pixelOffset int64
	seenRoot := make(map[core.Tag]bool)
	reader, err := parser.NewSelectiveReader(ctx, source, header.TransferSyntax, parserOptions, parser.SelectiveReaderOptions{
		MaxTokens: repairMaxTokens,
		Select: func(_ context.Context, path validation.Path, h core.ElementHeader, offset int64) (parser.SelectiveDisposition, error) {
			if len(path) == 1 {
				if h.Tag.Group < 8 || h.Tag.Group == 0xffff || seenRoot[h.Tag] {
					return parser.SelectiveStop, nil
				}
				seenRoot[h.Tag] = true
				if h.Tag == core.TagPixelData {
					if h.Length.IsUndefined() != header.TransferSyntax.Encapsulated ||
						(h.VR != core.VROB && h.VR != core.VROW) || (header.TransferSyntax.Encapsulated && h.VR != core.VROB) {
						return parser.SelectiveStop, nil
					}
					pixelOffset = offset
					if header.TransferSyntax.Encapsulated {
						return parser.SelectiveMaterialize, nil
					}
				}
			}
			if h.VR == core.VRSQ || (h.VR == core.VRUN && h.Length.IsUndefined()) {
				return parser.SelectiveMaterialize, nil
			}
			return parser.SelectiveSkip, nil
		},
	})
	if err != nil {
		return 0, repairReadFailure(ctx, err)
	}
	for {
		token, err := reader.Next()
		if err != nil {
			return 0, repairReadFailure(ctx, err)
		}
		if token.Kind == parser.TokenStop {
			return 0, ErrTrailingDataNotRepairable
		}
		if pixelOffset == 0 || token.Offset != pixelOffset {
			continue
		}
		if token.Kind == parser.TokenStartPixelSequence {
			if err := readRepairFragments(reader); err != nil {
				return 0, repairReadFailure(ctx, err)
			}
		}
		end := reader.Position()
		if end >= size {
			return 0, ErrTrailingDataNotRepairable
		}
		if err := checkRepairTail(ctx, source, header, end, size); err != nil {
			return 0, err
		}
		return end, nil
	}
}

func readRepairFragments(r *parser.Reader) error {
	// The ordinary token path keeps each Item frame bounded. A separate
	// count requires a Basic Offset Table and at least one nonempty fragment.
	for items := 0; ; items++ {
		tok, err := r.Next()
		if err != nil {
			return err
		}
		switch tok.Kind {
		case parser.TokenElement:
			if !tok.Header.Tag.IsItem() || (items == 0 && tok.Header.Length%4 != 0) || (items > 0 && (tok.Header.Length == 0 || tok.Header.Length%2 != 0)) {
				return ErrTrailingDataNotRepairable
			}
		case parser.TokenEndSequence:
			if items >= 2 && tok.Header.Tag.IsSequenceDelimitationItem() {
				return nil
			}
			return ErrTrailingDataNotRepairable
		default:
			return ErrTrailingDataNotRepairable
		}
	}
}

func checkRepairTail(ctx context.Context, source io.ReaderAt, header object.Part10Header, end, size int64) error {
	// At most one root header is needed. A syntactically plausible header
	// could be a genuine attribute, including a private one or padding.
	var window [12]byte
	span := window[:min(int64(len(window)), size-end)]
	if _, err := source.ReadAt(span, end); err != nil {
		return err
	}
	if len(span) >= 2 {
		group := header.TransferSyntax.ByteOrder.Uint16(span)
		if group < 8 || group == 0xffff {
			return ctx.Err()
		}
	}
	reader, err := parser.NewSelectiveReader(ctx, bytes.NewReader(span), header.TransferSyntax,
		parser.ReaderOptions{Dictionary: std.Dictionary, StrictReservedBytes: true}, parser.SelectiveReaderOptions{
			Select: func(context.Context, validation.Path, core.ElementHeader, int64) (parser.SelectiveDisposition, error) {
				return parser.SelectiveStop, nil
			},
		})
	if err != nil {
		return err
	}
	_, err = reader.Next()
	var malformed *parser.ParseError
	if errors.As(err, &malformed) && malformed.Tag != core.TagPixelData &&
		(errors.Is(err, io.ErrUnexpectedEOF) || malformed.Op == parser.OpReadVR || errors.Is(err, parser.ErrNonZeroReservedBytes)) {
		return nil
	}
	return repairReadFailure(ctx, err)
}

func verifyRepairPrefix(file *object.File, end int64) error {
	identity, err := file.ValidatePart10Identity()
	if err != nil || identity.HasErrors() {
		return ErrTrailingDataNotRepairable
	}
	pixel, ok := file.Dataset.Get(core.TagPixelData)
	if !ok || file.Dataset.Has(tagFloatPixelData) || file.Dataset.Has(tagDoubleFloatPixelData) ||
		file.Dataset.CheckPixelDataEncoding(file.TransferSyntax) != nil {
		return ErrTrailingDataNotRepairable
	}
	rootEnd := int64(-1)
	for _, location := range file.ValueLocations(core.TagPixelData) {
		if location.ItemOffsetSet {
			continue
		}
		if rootEnd != -1 || location.Length <= 0 {
			return ErrTrailingDataNotRepairable
		}
		rootEnd = location.ValueOffset + location.Length
	}
	if rootEnd != end {
		return ErrTrailingDataNotRepairable
	}
	if file.TransferSyntax.Encapsulated {
		return nil
	}
	metadata, err := ExtractMetadata(file.Dataset)
	if err != nil || metadata.Rows == 0 || metadata.Columns == 0 || metadata.SamplesPerPixel == 0 ||
		metadata.BitsAllocated != 1 && (metadata.BitsAllocated == 0 || metadata.BitsAllocated%8 != 0 || metadata.BitsAllocated > 64) ||
		metadata.BitsStored == 0 || metadata.BitsStored > metadata.BitsAllocated || metadata.HighBit >= metadata.BitsAllocated ||
		metadata.HighBit < metadata.BitsStored-1 || metadata.PixelRepresentation > 1 {
		return ErrTrailingDataNotRepairable
	}
	length := metadata.TotalSize()
	if metadata.BitsAllocated == 1 {
		length, _ = nativePackedByteCount(metadata)
	}
	if length <= 0 || length == math.MaxInt64 || int64(pixel.Header.Length) != length+length%2 {
		return fmt.Errorf("%w: %w", ErrTrailingDataNotRepairable, ErrPixelDataSizeMismatch)
	}
	return nil
}

// The parser seeks over pixels. Charge actual metadata bytes read and retain
// the ordinary per-element allocation limit for an oversized primitive.
type repairMetadataFile struct {
	transcodeContextFile
	remaining int64
}

func (f *repairMetadataFile) Read(p []byte) (int, error) {
	if int64(len(p)) > f.remaining {
		return 0, ErrTrailingDataRepairLimit
	}
	n, err := f.transcodeContextFile.Read(p)
	f.remaining -= int64(n)
	return n, err
}

func repairReadFailure(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	for _, limit := range []error{ErrTrailingDataRepairLimit, parser.ErrMaxElementsExceeded, parser.ErrMaxElementBytesExceeded,
		parser.ErrMaxPixelDataBytesExceeded, parser.ErrMaxDepthExceeded, parser.ErrMaxFragmentsExceeded, parser.ErrMaxTotalBytesExceeded,
		parser.ErrMaxSelectiveTokensExceeded} {
		if errors.Is(err, limit) {
			return ErrTrailingDataRepairLimit
		}
	}
	return ErrTrailingDataNotRepairable
}
