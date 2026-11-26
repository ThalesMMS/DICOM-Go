package pixeldata

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// ErrNoCompleteFrames means that the inspected bytes contain no complete image.
var ErrNoCompleteFrames = errors.New("dicom: incomplete Pixel Data has no complete frames")

const DiagnosticIncompletePixelData = "incomplete-pixel-data-complete-prefix"

// IncompleteFrameRecovery describes successful complete-prefix normalization.
// It does not claim that the original instance or its clinical content is complete.
type IncompleteFrameRecovery struct {
	DeclaredBytes, AvailableBytes  int64
	Kind                           string
	ValueOffset                    int64
	DeclaredFrames, RetainedFrames int
	DiscardedFragments             int
	DiscardedBytes                 int64
}

func incompatibleIncompleteRecovery(r DecodeRecovery) bool {
	return requiresLegacyPixelRepair(r) || r.IgnoreInconsistentBasicOffsetTable || r.DefaultImagePixelModule || r.RepairPhotometricInterpretation || r.Unpack12BitPacked || r.ReconcileWithCodestream || r.UnwrapJP2Container || r.NativePixelDataUnderEncapsulated || r.DefinedLengthFragments || r.RepairBitCountMasks
}

func normalizeIncompletePixelData(ctx context.Context, dataset *object.Object, source transfer.Syntax, pixel PixelData, metadata Metadata, info object.IncompletePixelData, limits DecompressLimits) (*object.Object, *IncompleteFrameRecovery, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := incompleteMaterializedMetadata(ctx, dataset.Elements(), 0); err != nil {
		return nil, nil, err
	}
	actual, ok := decompressionInputBytes(pixel)
	if !ok || actual != info.AvailableBytes {
		return nil, nil, fmt.Errorf("%w: inspected Pixel Data was changed", ErrIncompatiblePixelData)
	}
	report := &IncompleteFrameRecovery{Kind: info.Kind, ValueOffset: info.ValueOffset, DeclaredBytes: info.DeclaredBytes, AvailableBytes: info.AvailableBytes, DeclaredFrames: metadata.NumberOfFrames, DiscardedBytes: info.DiscardedBytes, DiscardedFragments: info.DiscardedFragments}
	elem, ok := dataset.Get(core.TagPixelData)
	if !ok {
		return nil, nil, ErrPixelDataNotFound
	}
	var value core.Value
	if !source.Encapsulated {
		if pixel.Encapsulated || info.Kind != "native-value-eof" || !elem.Header.LengthSet || int64(elem.Header.Length) != info.DeclaredBytes || metadata.BitsAllocated < 8 || metadata.BitsAllocated%8 != 0 {
			return nil, nil, fmt.Errorf("%w: unsupported incomplete native layout", ErrIncompatiblePixelData)
		}
		size, total := metadata.FrameSize(), metadata.TotalSize()
		if size <= 0 || total <= 0 || total == math.MaxInt64 || info.DeclaredBytes != total+total%2 || int64(len(pixel.Raw)) > total {
			return nil, nil, ErrIncompatiblePixelData
		}
		retained := int64(len(pixel.Raw)) / size
		if retained < 1 {
			return nil, nil, ErrNoCompleteFrames
		}
		report.RetainedFrames = int(retained)
		keep := retained * size
		report.DiscardedBytes += int64(len(pixel.Raw)) - keep
		value = core.RawValue(pixel.Raw[:int(keep):int(keep)])
	} else {
		if !pixel.Encapsulated || info.DeclaredBytes != -1 || len(pixel.Sequence.Fragments) != info.RetainedFragments {
			return nil, nil, ErrIncompatiblePixelData
		}
		format := encapsulated.JPEG
		switch source.UID {
		case transfer.JPEGBaseline.UID, transfer.JPEGExtended.UID, transfer.JPEGLosslessNonHierarchical.UID, transfer.JPEGLosslessSV1.UID:
		case transfer.JPEGLSLossless.UID, transfer.JPEGLSNearLossless.UID:
			format = encapsulated.JPEGLS
		default:
			return nil, nil, fmt.Errorf("%w: incomplete framing profile", ErrUnsupportedDecompression)
		}
		tables, err := encapsulated.ReadTables(dataset, pixel.Sequence.OffsetTable, metadata.NumberOfFrames)
		if err != nil {
			return nil, nil, err
		}
		_, prefix, err := encapsulated.CompletePrefix(ctx, encapsulated.Memory(pixel.Sequence.Fragments), tables, metadata.NumberOfFrames, format, encapsulated.Limits{MaxFrames: limits.MaxFrames, MaxBytes: uint64(limits.MaxInputBytes), MaxFrameBytes: uint64(limits.MaxInputBytes)})
		if errors.Is(err, encapsulated.ErrFrameCount) {
			return nil, nil, errors.Join(ErrNoCompleteFrames, err)
		}
		if err != nil {
			return nil, nil, err
		}
		report.RetainedFrames = prefix.RetainedFrames
		report.DiscardedFragments += prefix.DiscardedFragments
		report.DiscardedBytes += int64(prefix.DiscardedBytes)
		seq := pixel.Sequence
		seq.Fragments = seq.Fragments[:prefix.RetainedFragments:prefix.RetainedFragments]
		if len(seq.OffsetTable) > 0 {
			seq.OffsetTable = seq.OffsetTable[: prefix.RetainedFrames*4 : prefix.RetainedFrames*4]
		}
		value = seq
	}
	if report.RetainedFrames != metadata.NumberOfFrames {
		// These attributes require a separately defined projection of frame metadata.
		// Refuse them rather than carrying invalid original frame references.
		for _, tag := range []core.Tag{
			core.NewTag(0x5200, 0x9230), core.NewTag(0x0020, 0x9161), core.NewTag(0x0020, 0x9162), core.NewTag(0x0020, 0x9163), core.NewTag(0x0020, 0x9221), core.NewTag(0x0020, 0x9222), core.NewTag(0x0028, 0x0009), core.NewTag(0x0018, 0x1065),
		} {
			if _, present := dataset.Get(tag); present {
				return nil, nil, fmt.Errorf("%w: unsupported frame-dependent metadata", ErrIncompatiblePixelData)
			}
		}
		for _, e := range dataset.Elements() {
			if e.Tag().Group >= 0x6000 && e.Tag().Group <= 0x60ff && e.Tag().Group%2 == 0 && (e.Tag().Element == 0x0015 || e.Tag().Element == 0x0051) {
				return nil, nil, ErrIncompatiblePixelData
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	// A separate read-only facade preserves input elements while the ordinary
	// strict decoder checks every qualified frame. Output construction owns bytes.
	out := object.FromElements(dataset.Elements(), nil)
	out.SetValueByteOrder(dataset.ValueByteOrder())
	if report.RetainedFrames != metadata.NumberOfFrames {
		out.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0008), VR: core.VRIS}, Value: core.StringValue{strconv.Itoa(report.RetainedFrames)}})
	}
	elem.Value = value
	if !pixel.Encapsulated {
		elem.Header.Length = core.Length(len(value.(core.RawValue)))
		elem.Header.LengthSet = true
	}
	out.Put(elem)
	return out, report, nil
}

// FromElements cannot carry deferred source replay into the independent result.
// Walk nested items too; the depth bound also refuses caller-created cycles.
func incompleteMaterializedMetadata(ctx context.Context, elements []core.Element, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 64 {
		return fmt.Errorf("%w: metadata nesting exceeds incomplete-data profile", ErrIncompatiblePixelData)
	}
	for _, element := range elements {
		if err := ctx.Err(); err != nil {
			return err
		}
		if element.Value == nil {
			return fmt.Errorf("%w: materialize deferred metadata before incomplete-data normalization", ErrIncompatiblePixelData)
		}
		if sequence, ok := element.Value.(core.SequenceValue); ok {
			for _, item := range sequence.Items {
				if err := incompleteMaterializedMetadata(ctx, item.Elements, depth+1); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
