package object

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

var (
	// ErrReadStopped identifies a requested StopBeforeTag boundary, not a
	// malformed Data Set. The accompanying File remains incomplete.
	ErrReadStopped                = errors.New("dicom: stopped at requested root header")
	ErrReadPrefixOptions          = errors.New("dicom: invalid prefix read options")
	ErrReadPrefixRequiresSeekable = errors.New("dicom: prefix File reading requires an io.ReadSeeker")
)

// ReadStopError accompanies a deliberately incomplete File. The value-free
// descriptor uses logical parser offsets; Deflate can read ahead physically.
type ReadStopError struct {
	Stop     parser.ReadStop
	Elements int
}

func (e *ReadStopError) Error() string {
	if e == nil {
		return ErrReadStopped.Error()
	}
	return fmt.Sprintf("%s: retained %d complete elements before offset %d (%s)", ErrReadStopped, e.Elements, e.Stop.Offset, e.Stop.Header.Tag)
}

func (*ReadStopError) Unwrap() []error { return []error{ErrReadStopped, ErrPartialRead} }

// ReadStop returns a detached deliberate-stop descriptor. A false result means
// a complete File or a different partial-read cause, not a prefix stop.
func (f *File) ReadStop() (parser.ReadStop, bool) {
	if f == nil || f.partialRead == nil {
		return parser.ReadStop{}, false
	}
	if stopped, ok := f.partialRead.Err.(fileReadStopCause); ok {
		return stopped.stop, true
	}
	return parser.ReadStop{}, false
}

// Store the descriptor as an immutable value inside File. The independently
// returned public error pointer may be modified by its caller; PartialRead.Err
// must not expose an exported mutable pointer into File's stop metadata.
type fileReadStopCause struct {
	stop     parser.ReadStop
	elements int
}

func (e fileReadStopCause) Error() string {
	return (&ReadStopError{Stop: e.stop, Elements: e.elements}).Error()
}
func (fileReadStopCause) Unwrap() []error { return []error{ErrReadStopped, ErrPartialRead} }

func rejectPrefixOptions(opts ReadFileOptions) error {
	if opts.StopBeforeTag != nil || opts.PrefixMaxTokens != 0 {
		return fmt.Errorf("%w: this entrypoint requires a complete dataset", ErrReadPrefixOptions)
	}
	return nil
}

func normalizePrefixOptions(opts ReadFileOptions) (ReadFileOptions, error) {
	if opts.StopBeforeTag == nil {
		if opts.PrefixMaxTokens != 0 {
			return ReadFileOptions{}, fmt.Errorf("%w: PrefixMaxTokens requires StopBeforeTag", ErrReadPrefixOptions)
		}
		return opts, nil
	}
	if opts.FrameSink != nil || opts.EncapsulatedSink != nil || opts.AllowPartialRead {
		return ReadFileOptions{}, fmt.Errorf("%w: sinks and AllowPartialRead cannot accompany StopBeforeTag", ErrReadPrefixOptions)
	}
	if opts.PrefixMaxTokens < 0 || opts.MaxSequenceDepth < 0 || opts.MaxElements < 0 ||
		opts.MaxFragments < 0 || opts.MaxTotalBytes < 0 || opts.MaxElementBytes < 0 ||
		opts.MaxPixelDataBytes < 0 || opts.MaxDeformableVectorGridBytes < 0 ||
		opts.InlineValueBytesThreshold < 0 || opts.MaxPrivateCreators < 0 ||
		opts.MaxPrivateDiagnostics < 0 || opts.MaxBoundaryRecoveries < 0 ||
		opts.MaxImplicitVRElements < 0 || opts.MaxDelimiterRecoveries < 0 {
		return ReadFileOptions{}, fmt.Errorf("%w: prefix limits must be finite and nonnegative", ErrReadPrefixOptions)
	}
	threshold := *opts.StopBeforeTag
	opts.StopBeforeTag = &threshold
	if opts.MaxTotalBytes == 0 {
		opts.MaxTotalBytes = 512 << 20
	}
	if opts.MaxElementBytes == 0 {
		opts.MaxElementBytes = 1 << 20
	}
	if opts.MaxElements == 0 {
		opts.MaxElements = 200_000
	}
	if opts.MaxFragments == 0 {
		opts.MaxFragments = 200_000
	}
	if opts.MaxSequenceDepth == 0 {
		opts.MaxSequenceDepth = parser.DefaultMaxSequenceDepth
	}
	if opts.PrefixMaxTokens == 0 {
		opts.PrefixMaxTokens = 1_000_000
	}
	return opts, nil
}

func newPrefixReader(source io.Reader, syntax transfer.Syntax, readerOpts parser.ReaderOptions, opts ReadFileOptions) (*parser.Reader, error) {
	threshold := *opts.StopBeforeTag
	selectHeader := func(_ context.Context, path validation.Path, header core.ElementHeader, _ int64) (parser.SelectiveDisposition, error) {
		if len(path) == 1 && header.Tag.Compare(threshold) >= 0 {
			return parser.SelectiveStop, nil
		}
		return parser.SelectiveMaterialize, nil
	}
	return parser.NewSelectiveReader(context.Background(), source, syntax, readerOpts,
		parser.SelectiveReaderOptions{Select: selectHeader, MaxTokens: opts.PrefixMaxTokens})
}
