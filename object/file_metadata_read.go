package object

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/validation"
)

// MetadataReadLimits defines byte ceilings relative to the start of the Part
// 10 file. Each limit must be positive.
type MetadataReadLimits struct {
	SourceBytes   int64 // physical extent of the source section
	LogicalBytes  int64 // extent including the inflated dataset, if Deflated
	MetadataBytes int64 // header, dataset element headers and non-pixel values
}

var ErrMetadataReadLimit = errors.New("dicom: Part 10 metadata exceeds resource limits")

// ReadFileMetadata reads detached metadata from the source's current position.
// The caller owns the source and has exclusive access to its position during
// the call. Native pixel values are passed by seek. Deflated pixel values are
// inflated and discarded incrementally. Integer, Float and Double Float Pixel
// Data headers are retained with nil values, including in sequence items.
//
// Resource limits and structural tolerances in opts apply to parsing. Positive
// opts.MaxTotalBytes and opts.MaxElementBytes can further restrict the supplied
// byte ceilings. Deferred values, sinks, projection and partial reads are
// incompatible with this API. The File is a metadata snapshot, not a complete
// representation of the encoded instance.
func ReadFileMetadata(ctx context.Context, source io.ReadSeeker, opts ReadFileOptions, limits MetadataReadLimits) (*File, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if source == nil {
		return nil, fmt.Errorf("dicom: read Part 10 metadata: nil source")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limits.SourceBytes <= 0 || limits.LogicalBytes <= 0 || limits.MetadataBytes <= 0 {
		return nil, ErrMetadataReadLimit
	}
	if err := rejectPrefixOptions(opts); err != nil {
		return nil, err
	}
	if opts.DataSetTags != nil || opts.SeekUnselectedSequences {
		return nil, parser.ErrDataSetProjectionOptions
	}
	if opts.AllowIncompletePixelData {
		return nil, parser.ErrIncompletePixelDataOptions
	}
	if opts.DeferPixelData || opts.DeferWaveformData || opts.BulkDataDescriptor != nil ||
		opts.InlineValueBytesThreshold != 0 || opts.FrameSink != nil || opts.EncapsulatedSink != nil {
		return nil, fmt.Errorf("dicom: metadata-only read cannot defer or stream values")
	}

	if opts.MaxTotalBytes > 0 {
		limits.LogicalBytes = min(limits.LogicalBytes, opts.MaxTotalBytes)
	}
	start, err := source.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	end, err := source.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if start < 0 || end < start || end-start > limits.SourceBytes {
		return nil, ErrMetadataReadLimit
	}
	section := &metadataSection{ctx: ctx, source: source, base: start, size: end - start}
	if _, err := section.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	opts.SkipPixelData = true
	opts.MaxTotalBytes = min(limits.MetadataBytes, limits.LogicalBytes)
	if opts.MaxElementBytes <= 0 {
		opts.MaxElementBytes = limits.MetadataBytes
	} else {
		opts.MaxElementBytes = min(opts.MaxElementBytes, limits.MetadataBytes)
	}
	if opts.MaxPixelDataBytes <= 0 {
		opts.MaxPixelDataBytes = limits.LogicalBytes
	} else {
		opts.MaxPixelDataBytes = min(opts.MaxPixelDataBytes, limits.LogicalBytes)
	}
	if opts.MaxElements <= 0 {
		opts.MaxElements = 100_000
	}
	if opts.MaxFragments <= 0 {
		opts.MaxFragments = 10_000
	}
	if opts.MaxSequenceDepth <= 0 {
		opts.MaxSequenceDepth = 32
	}

	header, err := ReadPart10Header(section, opts)
	if err != nil {
		return nil, metadataReadError(err)
	}
	remaining := limits.MetadataBytes - header.DataSetOffset
	logical := limits.LogicalBytes - header.DataSetOffset
	if remaining <= 0 || logical <= 0 {
		return nil, ErrMetadataReadLimit
	}

	budgetRefused := false
	hooks, err := validation.NewHookChain(validation.HookRegistration{
		Name: "metadata-byte-budget", Points: []validation.HookPoint{validation.HookElementHeaderRead},
		Hook: validation.HookFunc(func(_ context.Context, event validation.HookEvent) (validation.HookDecision, error) {
			h := event.Header
			cost := int64(12) // conservative bound for a primitive or sequence header
			if h.VR != core.VRSQ && !h.Length.IsUndefined() {
				switch h.Tag {
				case core.TagPixelData, core.NewTag(0x7FE0, 0x0008), core.NewTag(0x7FE0, 0x0009):
				default:
					cost += int64(h.Length)
				}
			}
			if cost > remaining {
				budgetRefused = true
				return validation.HookDecision{}, ErrMetadataReadLimit
			}
			remaining -= cost
			return validation.HookDecision{}, nil
		}),
	})
	if err != nil {
		return nil, err
	}

	// Dataset parser errors and recoveries use offsets relative to the
	// dataset. Rebase the section without moving the caller's descriptor.
	section.base += header.DataSetOffset
	section.size -= header.DataSetOffset
	section.pos = 0
	opts.MaxTotalBytes = logical
	maxDepth := opts.MaxSequenceDepth/2 + opts.MaxSequenceDepth%2
	dataset, _, err := ReadDataSetWithValidation(ctx, section, header.TransferSyntax, opts,
		validation.Options{Hooks: hooks, MaxElements: opts.MaxElements, MaxDepth: maxDepth, MaxFindings: 32})
	if err != nil {
		if budgetRefused {
			return nil, errors.Join(ErrMetadataReadLimit, err)
		}
		return nil, metadataReadError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &File{Preamble: header.Preamble, Meta: header.Meta, Dataset: dataset, TransferSyntax: header.TransferSyntax}, nil
}

func metadataReadError(err error) error {
	if errors.Is(err, parser.ErrMaxTotalBytesExceeded) || errors.Is(err, parser.ErrMaxElementBytesExceeded) ||
		errors.Is(err, parser.ErrMaxPixelDataBytesExceeded) || errors.Is(err, parser.ErrMaxElementsExceeded) ||
		errors.Is(err, parser.ErrMaxFragmentsExceeded) || errors.Is(err, parser.ErrMaxDepthExceeded) ||
		errors.Is(err, parser.ErrMaxDeformableVectorGridBytesExceeded) {
		return errors.Join(ErrMetadataReadLimit, err)
	}
	return err
}

// metadataSection exposes the captured physical extent to the parser for
// seek-only skipping while bounding all accesses and observing cancellation.
type metadataSection struct {
	ctx             context.Context
	source          io.ReadSeeker
	base, size, pos int64
}

func (r *metadataSection) Size() int64 { return r.size }

func (r *metadataSection) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.pos >= r.size {
		return 0, io.EOF
	}
	if int64(len(p)) > r.size-r.pos {
		p = p[:r.size-r.pos]
	}
	n, err := r.source.Read(p)
	r.pos += int64(n)
	return n, err
}

func (r *metadataSection) Seek(offset int64, whence int) (int64, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	var origin int64
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		origin = r.pos
	case io.SeekEnd:
		origin = r.size
	default:
		return 0, fmt.Errorf("dicom: invalid metadata source seek mode")
	}
	if offset < -origin || offset > r.size-origin {
		return 0, ErrMetadataReadLimit
	}
	pos := origin + offset
	actual, err := r.source.Seek(r.base+pos, io.SeekStart)
	if err != nil {
		return 0, err
	}
	if actual-r.base != pos {
		return 0, fmt.Errorf("dicom: metadata source seek returned an unexpected position")
	}
	r.pos = pos
	return pos, nil
}
