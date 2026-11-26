package deid

import (
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

const (
	maxDeflatedPathBytes         int64 = 2 << 30
	maxDeflatedPathMetadataBytes int64 = 128 << 20
)

// openPathSource owns both the original descriptor and any alternate replay
// source. A deferred-value refusal from a seekable Part 10 file permits only
// the Deflate fallback using that same open original.
func openPathSource(ctx context.Context, path string, deferPixels bool) (*object.File, func() error, error) {
	source, err := object.OpenSourceFile(path)
	if err != nil {
		return nil, nil, err
	}
	return openOwnedMetadataSource(ctx, source, deferPixels)
}

func openOwnedMetadataSource(ctx context.Context, source *os.File, deferPixels bool) (*object.File, func() error, error) {
	file, err := object.ReadFileWithOptions(source, object.ReadFileOptions{DeferPixelData: deferPixels})
	if err == nil {
		return file, source.Close, nil
	}
	if !deferPixels || !errors.Is(err, object.ErrDeferredValueRequiresSeekable) {
		return nil, nil, errors.Join(err, source.Close())
	}
	file, release, err := openDeflatedPathSource(ctx, source)
	if err = errors.Join(err, source.Close()); err != nil {
		if release != nil {
			err = errors.Join(err, release())
		}
		return nil, nil, err
	}
	return file, release, nil
}

func openDeflatedPathSource(ctx context.Context, source *os.File) (_ *object.File, _ func() error, err error) {
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	header, err := object.ReadPart10Header(source, object.ReadFileOptions{})
	if err != nil {
		return nil, nil, err
	}
	if header.TransferSyntax.UID != transfer.DeflatedExplicitVRLittleEndian.UID {
		return nil, nil, object.ErrDeferredValueRequiresSeekable
	}
	stat, err := source.Stat()
	if err != nil {
		return nil, nil, err
	}
	compressedBytes := stat.Size() - header.DataSetOffset
	if compressedBytes <= 0 {
		return nil, nil, io.ErrUnexpectedEOF
	}
	// Leave room for the framing overhead of raw streams with stored blocks.
	if compressedBytes > maxDeflatedPathBytes+(1<<20) {
		return nil, nil, fmt.Errorf("deid: compressed dataset: %w", parser.ErrMaxTotalBytesExceeded)
	}
	spool, release, err := createDeflatedSpool()
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, release())
		}
	}()
	compressed := io.NewSectionReader(source, header.DataSetOffset, compressedBytes)
	inflatedBytes, err := spoolDeflatedDataSet(ctx, spool, compressed, maxDeflatedPathBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("deid: inflate dataset: %w", err)
	}
	logical := contextualPathSection{SectionReader: io.NewSectionReader(spool, 0, inflatedBytes), ctx: ctx}
	if err := boundDeflatedMetadata(ctx, logical, maxDeflatedPathMetadataBytes); err != nil {
		return nil, nil, fmt.Errorf("deid: read inflated dataset: %w", err)
	}
	if _, err := logical.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	dataset, err := object.ReadDataSetWithOptions(logical, transfer.ExplicitVRLittleEndian, object.ReadFileOptions{
		DeferPixelData:    true,
		MaxTotalBytes:     inflatedBytes + 1, // Probe EOF at the exact logical end.
		MaxElementBytes:   maxDeflatedPathMetadataBytes,
		MaxPixelDataBytes: maxDeflatedPathBytes,
		MaxElements:       DefaultProfileLimits().MaxElements,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("deid: read inflated dataset: %w", err)
	}
	return &object.File{Preamble: header.Preamble, Meta: header.Meta, Dataset: dataset, TransferSyntax: header.TransferSyntax}, release, nil
}

// boundDeflatedMetadata visits headers and item framing without retaining
// values. Only deferred, top-level pixel values are excluded from the budget.
func boundDeflatedMetadata(ctx context.Context, source contextualPathSection, maximum int64) error {
	limits := DefaultProfileLimits()
	maxTokens := 2 * (limits.MaxElements + limits.MaxItems)
	metadataBytes := source.Size()
	reader, err := parser.NewSelectiveReader(ctx, source, transfer.ExplicitVRLittleEndian, parser.ReaderOptions{
		MaxTotalBytes: source.Size() + 1,
	}, parser.SelectiveReaderOptions{MaxTokens: maxTokens, Select: func(_ context.Context, path validation.Path, header core.ElementHeader, _ int64) (parser.SelectiveDisposition, error) {
		if header.VR == core.VRSQ || header.Length.IsUndefined() {
			return parser.SelectiveMaterialize, nil
		}
		if len(path) == 1 {
			switch header.Tag {
			case core.TagPixelData, tagFloatPixelData, tagDoubleFloatPixelData:
				metadataBytes -= int64(header.Length)
			}
		}
		return parser.SelectiveSkip, nil
	}})
	if err != nil {
		return err
	}
	for {
		if _, err := reader.Next(); err != nil {
			if err != io.EOF {
				return err
			}
			if metadataBytes > maximum {
				return ErrProfileResourceLimit
			}
			return nil
		}
	}
}

func spoolDeflatedDataSet(ctx context.Context, spool io.Writer, source io.Reader, maximum int64) (int64, error) {
	inflated := flate.NewReader(contextReader{ctx: ctx, reader: source})
	defer inflated.Close()
	cancellable := contextReader{ctx: ctx, reader: inflated}
	n, err := io.Copy(spool, io.LimitReader(cancellable, maximum))
	if err == nil && n == maximum {
		// Check for clean end of stream without writing an excess byte.
		var extra [1]byte
		switch _, err = io.ReadFull(cancellable, extra[:]); err {
		case nil:
			err = parser.ErrMaxTotalBytesExceeded
		case io.EOF:
			err = nil
		}
	}
	return n, err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}

// The parser needs Size and Seek for bounds and deferred replay.
type contextualPathSection struct {
	*io.SectionReader
	ctx context.Context
}

func (s contextualPathSection) Read(data []byte) (int, error) {
	return (contextReader{ctx: s.ctx, reader: s.SectionReader}).Read(data)
}
