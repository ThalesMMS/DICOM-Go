package seg

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// ErrInvalidEncapsulation identifies invalid frame counts, Items or offset
// tables in the strict compressed SEG profile.
var ErrInvalidEncapsulation = errors.New("dicom/seg: invalid encapsulated Pixel Data")

// PixelDataReadOptions opts into bounded, cancellable SEG reading with an
// explicit transfer syntax. Read and ReadWithOptions remain native-only.
type PixelDataReadOptions struct {
	TransferSyntax transfer.Syntax
	// Registry is caller-owned. Compressed input requires a registered
	// ContextCodec; nil never falls back to the package global registry.
	Registry pixeldata.Registry
	// Limits bound frame/pixel counts, compressed input, decoded pixel bytes
	// and expansion ratio before decode. Zero fields use finite defaults from
	// pixeldata.DefaultDecompressLimits. MaxPixels also bounds materialized
	// sample/mask work; MaxNativeBytes is not a bound on total process RSS.
	Limits pixeldata.DecompressLimits
}

// ReadContext reads native SEG, Deflated Image Frame BINARY (1 bit), or RLE
// FRACTIONAL/LABELMAP (8/16 bits), without quantization or decode recovery.
// PS3.3/PS3.5 2026d define the profile. Each compressed frame must occupy one
// even-length Item; populated BOT/EOT must agree with that exact layout and
// every frame must have a corresponding Per-frame Functional Groups item.
// RLE 1-bit, other codecs and compressed writing are outside this profile.
//
// The dataset and registry are borrowed and must remain stable while reading.
// A custom codec must honor context, dimensions and its owned-output contract.
// Failure/cancellation returns no partial Document.
func ReadContext(ctx context.Context, obj *object.Object, opts PixelDataReadOptions) (*Document, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limits, err := segReadLimits(opts.Limits)
	if err != nil {
		return nil, err
	}
	source, ok := transfer.DefaultRegistry.Get(opts.TransferSyntax.UID)
	if !ok || source.UID == "" {
		return nil, fmt.Errorf("%w: explicit known transfer syntax required", ErrUnsupportedPixelData)
	}
	doc, err := readDocumentHeader(obj)
	if err != nil {
		return nil, err
	}
	metadata, err := strictSEGMetadata(obj, doc, limits, source.Encapsulated)
	if err != nil {
		return nil, err
	}
	pixel, err := pixeldata.ExtractView(obj)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidObject, err)
	}
	if source.Encapsulated != pixel.Encapsulated {
		return nil, fmt.Errorf("%w: %w", ErrUnsupportedPixelData, pixeldata.ErrIncompatiblePixelData)
	}
	if !source.Encapsulated {
		switch source.UID {
		case transfer.ImplicitVRLittleEndian.UID, transfer.ExplicitVRLittleEndian.UID,
			transfer.ExplicitVRBigEndian.UID, transfer.DeflatedExplicitVRLittleEndian.UID:
		default:
			return nil, ErrUnsupportedPixelData
		}
		if int64(len(pixel.Raw)) > limits.MaxInputBytes {
			return nil, segReadLimit("MaxInputBytes", int64(len(pixel.Raw)), limits.MaxInputBytes)
		}
		expected := metadata.TotalSize()
		if metadata.BitsAllocated == 1 {
			expected = (int64(metadata.Rows)*int64(metadata.Columns)*int64(metadata.NumberOfFrames) + 7) / 8
		}
		if int64(len(pixel.Raw)) < expected {
			return nil, ErrTruncatedPixelData
		}
		if int64(len(pixel.Raw)) > expected+(expected&1) {
			return nil, fmt.Errorf("%w: native Pixel Data exceeds declared frames", ErrInvalidObject)
		}
	} else {
		switch source.UID {
		case transfer.DeflatedImageFrameCompression.UID:
			if doc.SegmentationType != SegmentationTypeBinary || doc.SOPClassUID != SegmentationStorage {
				return nil, fmt.Errorf("%w: Deflated Image Frame profile requires BINARY", ErrUnsupportedPixelData)
			}
		case transfer.RLELossless.UID:
			if doc.SegmentationType != SegmentationTypeFractional && doc.SegmentationType != SegmentationTypeLabelMap {
				return nil, fmt.Errorf("%w: RLE profile requires FRACTIONAL or LABELMAP", ErrUnsupportedPixelData)
			}
		default:
			return nil, fmt.Errorf("%w: transfer syntax outside the lossless SEG profile", ErrUnsupportedPixelData)
		}
		if err := validateSEGFragments(ctx, obj, pixel.Sequence, metadata, limits); err != nil {
			return nil, err
		}
		if opts.Registry == nil {
			return nil, fmt.Errorf("%w: %w", ErrUnsupportedPixelData, pixeldata.ErrCodecRegistryNil)
		}
		codec, exists := opts.Registry.GetCodec(source.UID)
		if !exists || codec == nil {
			return nil, fmt.Errorf("%w: %w", ErrUnsupportedPixelData, pixeldata.ErrCodecNotFound)
		}
		if _, contextual := codec.(pixeldata.ContextCodec); !contextual {
			return nil, fmt.Errorf("%w: context-aware decoder required", ErrUnsupportedPixelData)
		}
		// Do not inherit tolerant codec options from the caller's context.
		ctx = pixeldata.DecodeContextWithRecovery(ctx, pixeldata.DecodeRecovery{})
		var report pixeldata.DecompressReport
		// The native output packs binary samples continuously across frames.
		obj, _, err = pixeldata.DecompressDataSetContext(ctx, obj, source, pixeldata.DecompressOptions{
			Registry: opts.Registry, Limits: limits, Report: &report,
		})
		if err != nil {
			if errors.Is(err, pixeldata.ErrDecompressResourceLimit) {
				return nil, fmt.Errorf("%w: %w", ErrResourceLimitExceeded, err)
			}
			return nil, err
		}
		if len(report.Diagnostics) != 0 {
			return nil, fmt.Errorf("%w: compressed frame requires recovery", ErrInvalidEncapsulation)
		}
	}
	doc, _, err = readDocumentPixelsContext(ctx, obj, doc, ReadOptions{}, true)
	if err != nil {
		return nil, err
	}
	if err := validateReadSamplesContext(ctx, doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func segReadLimits(l pixeldata.DecompressLimits) (pixeldata.DecompressLimits, error) {
	if l.MaxFrames < 0 || l.MaxPixels < 0 || l.MaxNativeBytes < 0 || l.MaxInputBytes < 0 || l.MaxExpansionRatio < 0 {
		return l, pixeldata.ErrInvalidDecompressLimits
	}
	d := pixeldata.DefaultDecompressLimits()
	if l.MaxFrames == 0 {
		l.MaxFrames = d.MaxFrames
	}
	if l.MaxPixels == 0 {
		l.MaxPixels = d.MaxPixels
	}
	if l.MaxNativeBytes == 0 {
		l.MaxNativeBytes = d.MaxNativeBytes
	}
	if l.MaxInputBytes == 0 {
		l.MaxInputBytes = d.MaxInputBytes
	}
	if l.MaxExpansionRatio == 0 {
		l.MaxExpansionRatio = d.MaxExpansionRatio
	}
	return l, nil
}

func segReadLimit(name string, value, maximum int64) error {
	return fmt.Errorf("%w: %w", ErrResourceLimitExceeded, &pixeldata.DecompressResourceLimitError{Limit: name, Actual: value, Maximum: maximum})
}

func strictSEGMetadata(obj *object.Object, doc *Document, l pixeldata.DecompressLimits, frameLocalBits bool) (pixeldata.Metadata, error) {
	frames, err := obj.GetInt(derivedio.TagNumberOfFrames)
	if err != nil || frames <= 0 {
		return pixeldata.Metadata{}, fmt.Errorf("%w: invalid Number of Frames", ErrInvalidObject)
	}
	if frames > int64(l.MaxFrames) {
		return pixeldata.Metadata{}, segReadLimit("MaxFrames", frames, int64(l.MaxFrames))
	}
	if int64(len(derivedio.Sequence(obj, tagPerFrameFunctionalGroups))) != frames {
		return pixeldata.Metadata{}, fmt.Errorf("%w: Number of Frames differs from Functional Groups", ErrInvalidObject)
	}
	pixels := int64(doc.Rows) * int64(doc.Columns)
	if pixels > l.MaxPixels/frames {
		return pixeldata.Metadata{}, segReadLimit("MaxPixels", pixels*frames, l.MaxPixels)
	}
	bits := int64(1)
	if doc.SegmentationType == SegmentationTypeFractional {
		bits = 8
	}
	if doc.SOPClassUID == LabelMapSegmentationStorage {
		bits = int64(doc.LabelMapBitsAllocated)
	}
	for tag, want := range map[core.Tag]int64{
		derivedio.TagBitsAllocated: bits, derivedio.TagBitsStored: bits,
		derivedio.TagHighBit: bits - 1, derivedio.TagPixelRepresentation: 0,
		derivedio.TagSamplesPerPixel: 1,
	} {
		got, err := obj.GetInt(tag)
		if err != nil || got != want {
			return pixeldata.Metadata{}, fmt.Errorf("%w: inconsistent Image Pixel Module", ErrUnsupportedPixelData)
		}
	}
	photometric := derivedio.CleanString(obj, core.NewTag(0x0028, 0x0004))
	if photometric != "MONOCHROME2" && !(doc.SOPClassUID == LabelMapSegmentationStorage && photometric == "PALETTE COLOR") {
		return pixeldata.Metadata{}, fmt.Errorf("%w: incompatible Photometric Interpretation", ErrUnsupportedPixelData)
	}
	native := ((pixels*bits + 7) / 8) * frames
	if bits == 1 && !frameLocalBits {
		native = (pixels*frames + 7) / 8
	}
	if native > l.MaxNativeBytes || native > int64(math.MaxInt) {
		return pixeldata.Metadata{}, segReadLimit("MaxNativeBytes", native, min(l.MaxNativeBytes, int64(math.MaxInt)))
	}
	return pixeldata.ExtractMetadata(obj)
}

func validateSEGFragments(ctx context.Context, obj *object.Object, sequence core.FragmentSequence, m pixeldata.Metadata, l pixeldata.DecompressLimits) error {
	if len(sequence.Fragments) != m.NumberOfFrames {
		return fmt.Errorf("%w: %w", ErrInvalidEncapsulation, encapsulated.ErrFrameCount)
	}
	input := int64(len(sequence.OffsetTable))
	if input > l.MaxInputBytes {
		return segReadLimit("MaxInputBytes", input, l.MaxInputBytes)
	}
	if len(sequence.OffsetTable) != 0 && len(sequence.OffsetTable) != 4*m.NumberOfFrames {
		return fmt.Errorf("%w: invalid Basic Offset Table count", ErrInvalidEncapsulation)
	}
	tables, err := encapsulated.ReadTables(obj, sequence.OffsetTable, m.NumberOfFrames)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidEncapsulation, err)
	}
	if tables.ExtendedPresent != tables.LengthsPresent || tables.ExtendedPresent && len(tables.Basic) != 0 {
		return fmt.Errorf("%w: inconsistent Extended Offset Table", ErrInvalidEncapsulation)
	}
	var position uint64
	for i, fragment := range sequence.Fragments {
		if err := ctx.Err(); err != nil {
			return err
		}
		size := uint64(len(fragment))
		if size < 2 || size&1 != 0 || size > math.MaxUint32-1 {
			return fmt.Errorf("%w: invalid frame Item length", ErrInvalidEncapsulation)
		}
		if int64(size) > l.MaxInputBytes-input {
			return segReadLimit("MaxInputBytes", input+int64(size), l.MaxInputBytes)
		}
		input += int64(size)
		if len(tables.Basic) != 0 && uint64(binary.LittleEndian.Uint32(tables.Basic[4*i:])) != position {
			return fmt.Errorf("%w: Basic Offset Table does not match frame Items", ErrInvalidEncapsulation)
		}
		if tables.ExtendedPresent {
			length := tables.Lengths[i]
			if tables.Extended[i] != position || length == 0 || length > size || size-length > 1 ||
				size != length && (length&1 == 0 || fragment[len(fragment)-1] != 0) {
				return fmt.Errorf("%w: Extended Offset Table does not match frame Items", ErrInvalidEncapsulation)
			}
		}
		position += 8 + size
	}
	native := m.TotalSize()
	ratio := native / input
	if native%input != 0 {
		ratio++
	}
	if ratio > l.MaxExpansionRatio {
		return segReadLimit("MaxExpansionRatio", ratio, l.MaxExpansionRatio)
	}
	return nil
}

func validateReadSamplesContext(ctx context.Context, doc *Document) error {
	described := make(map[int]bool, len(doc.Segments))
	for _, segment := range doc.Segments {
		described[segment.Number] = true
	}
	for _, frame := range doc.Frames {
		if err := ctx.Err(); err != nil {
			return err
		}
		for i, sample := range frame.Fractional {
			if i&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			if int(sample) > doc.MaxFractionalValue {
				return fmt.Errorf("%w: fractional sample exceeds declared maximum", ErrInvalidObject)
			}
		}
		for i, label := range frame.LabelMap {
			if i&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			if !described[int(label)] {
				return fmt.Errorf("%w: label has no described segment", ErrInvalidObject)
			}
		}
	}
	return ctx.Err()
}
