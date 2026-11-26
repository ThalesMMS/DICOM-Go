package pixeldata

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/derivation"
	"github.com/ThalesMMS/dicom-go/dictionary/uid"
	"github.com/ThalesMMS/dicom-go/internal/encaplayout"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// PlanarOutputPolicy selects the sample layout of native transcode output.
type PlanarOutputPolicy uint8

const (
	// PlanarOutputDefault preserves native copies and the decoder's layout.
	PlanarOutputDefault PlanarOutputPolicy = iota
	// PlanarOutputIOD selects plane order for retired Hardcopy Color and
	// YBR_FULL Ultrasound (1996) IODs, pixel order for other color images.
	// Plane order for native US YBR_FULL is a compatibility choice: current
	// PS3.3 C.8.5.6.1.16 permits either order for native output.
	PlanarOutputIOD
	PlanarOutputInterleaved
	PlanarOutputPlanar
)

// TranscodeUIDPolicy selects output SOP Instance identity. New lossy
// generations and Secondary Capture conversions always require new identity.
type TranscodeUIDPolicy uint8

const (
	TranscodeUIDDefault TranscodeUIDPolicy = iota
	TranscodeUIDAlways
	TranscodeUIDPreserve
)

var tagOutputSOPClass = core.NewTag(0x0008, 0x0016)

func explicitEncapsulation(o TranscodeOptions) bool {
	return o.MaxFragmentBytes != 0 || o.OffsetTable != object.OffsetTableAuto
}

func validateTranscodeOutputOptions(dataset *object.Object, target transfer.Syntax, o TranscodeOptions) error {
	if err := validateOutputPolicy(o); err != nil {
		return err
	}
	target, err := canonicalTargetTransferSyntax(target)
	if err != nil {
		return err
	}
	if !target.Encapsulated && explicitEncapsulation(o) {
		return fmt.Errorf("%w: encapsulation options require an encapsulated target", ErrTranscodeUnsupported)
	}
	if target.Encapsulated && o.PlanarOutput != PlanarOutputDefault {
		return fmt.Errorf("%w: planar output policy requires a native target", ErrTranscodeUnsupported)
	}
	if jpeg2000Part2(target.UID) && (explicitEncapsulation(o) || o.CompressIcons) {
		return fmt.Errorf("%w: Annex J output requires one collection with empty offset tables and native icons", ErrTranscodeUnsupported)
	}
	if o.SecondaryCapture {
		return validateSecondaryCapture(dataset)
	}
	return nil
}

func validateOutputPolicy(o TranscodeOptions) error {
	if err := validateTranscodeUIDRoot(o.UIDRoot); err != nil {
		return err
	}
	if o.UIDRoot != "" && o.UIDPolicy == TranscodeUIDPreserve {
		return fmt.Errorf("%w: UID root conflicts with preserved SOP identity", ErrTranscodeUnsupported)
	}
	if o.MaxFragmentBytes < 0 || o.MaxFragmentBytes > int64(encaplayout.MaxItem) || o.MaxFragmentBytes != 0 && (o.MaxFragmentBytes < 2 || o.MaxFragmentBytes%2 != 0) {
		return fmt.Errorf("%w: fragment cap must be zero or an even Item length", ErrTranscodeUnsupported)
	}
	if o.OffsetTable > object.OffsetTableEmpty || o.PlanarOutput > PlanarOutputPlanar || o.UIDPolicy > TranscodeUIDPreserve {
		return fmt.Errorf("%w: unknown output policy", ErrTranscodeUnsupported)
	}
	if o.SecondaryCapture && o.UIDPolicy == TranscodeUIDPreserve {
		return fmt.Errorf("%w: Secondary Capture requires a new UID", ErrTranscodeUnsupported)
	}
	return nil
}

func validateSecondaryCapture(dataset *object.Object) error {
	if dataset == nil {
		return ErrMissingMetadata
	}
	m, err := ExtractMetadata(dataset)
	if err != nil {
		return err
	}
	class, _ := dataset.GetString(tagOutputSOPClass)
	entry, known := uid.LookupStorageSOPClass(class)
	if !known || entry.Category != uid.StorageCategoryImage || strings.Contains(entry.Keyword, "Enhanced") || strings.Contains(entry.Keyword, "MultiFrame") || dataset.Has(core.NewTag(0x5200, 0x9229)) || dataset.Has(core.NewTag(0x5200, 0x9230)) || m.NumberOfFrames != 1 {
		return fmt.Errorf("%w: Secondary Capture requires a classic single-frame Image IOD", ErrTranscodeUnsupported)
	}
	if m.BitsAllocated != 8 && m.BitsAllocated != 16 || m.SamplesPerPixel == 1 && m.PhotometricInterpretation != "MONOCHROME1" && m.PhotometricInterpretation != "MONOCHROME2" && m.PhotometricInterpretation != "PALETTE COLOR" || m.SamplesPerPixel == 3 && (m.BitsAllocated != 8 || m.PixelRepresentation != 0 || m.PhotometricInterpretation != "RGB" && m.PhotometricInterpretation != "YBR_FULL" && m.PhotometricInterpretation != "YBR_FULL_422") || m.SamplesPerPixel != 1 && m.SamplesPerPixel != 3 {
		return fmt.Errorf("%w: Secondary Capture pixel representation", ErrTranscodeUnsupported)
	}
	for _, tag := range []core.Tag{tagOutputSOPClass, tagSOPInstanceUID, core.NewTag(0x0020, 0x000d), core.NewTag(0x0020, 0x000e)} {
		value, _ := dataset.GetString(tag)
		if !core.IsValidUID(core.NormalizeUID(value)) {
			return fmt.Errorf("%w: Secondary Capture requires valid source, study and series UIDs", ErrTranscodeUnsupported)
		}
	}
	if m.PhotometricInterpretation == "PALETTE COLOR" {
		palette, err := display.PaletteFromObject(dataset)
		if err != nil || palette == nil {
			return fmt.Errorf("%w: Secondary Capture requires a valid palette", ErrTranscodeUnsupported)
		}
		for _, channel := range []*display.LUT{palette.Red, palette.Green, palette.Blue} {
			if len(channel.Entries) != len(palette.Red.Entries) || channel.FirstMapped != palette.Red.FirstMapped || channel.BitsPerEntry != palette.Red.BitsPerEntry || channel.BitsPerEntry != 8 && channel.BitsPerEntry != 16 {
				return fmt.Errorf("%w: Secondary Capture palette descriptors disagree", ErrTranscodeUnsupported)
			}
		}
	}
	return nil
}

func outputPlanar(dataset *object.Object, m Metadata, policy PlanarOutputPolicy) (uint16, error) {
	if policy == PlanarOutputDefault || m.SamplesPerPixel == 1 {
		return m.PlanarConfiguration, nil
	}
	if m.SamplesPerPixel != 3 || m.BitsAllocated != 8 && m.BitsAllocated != 16 || m.PlanarConfiguration > 1 || m.PhotometricInterpretation != "RGB" && m.PhotometricInterpretation != "YBR_FULL" {
		return 0, fmt.Errorf("%w: planar output requires full-resolution RGB or YBR_FULL samples", ErrTranscodeUnsupported)
	}
	if policy == PlanarOutputPlanar {
		return 1, nil
	}
	if policy == PlanarOutputInterleaved {
		return 0, nil
	}
	class, _ := dataset.GetString(tagOutputSOPClass)
	class = core.NormalizeUID(class)
	if class == "1.2.840.10008.5.1.1.30" || m.PhotometricInterpretation == "YBR_FULL" && (class == "1.2.840.10008.5.1.4.1.1.6.1" || class == "1.2.840.10008.5.1.4.1.1.3.1") {
		return 1, nil
	}
	return 0, nil
}

func applyNativeOutputPlanar(ctx context.Context, dataset *object.Object, policy PlanarOutputPolicy) error {
	if policy == PlanarOutputDefault {
		return nil
	}
	m, err := ExtractMetadata(dataset)
	if err != nil {
		return err
	}
	target, err := outputPlanar(dataset, m, policy)
	if err != nil {
		return err
	}
	if m.SamplesPerPixel == 1 {
		return nil
	}
	if target != m.PlanarConfiguration {
		frames, err := ExtractNativeFramesView(dataset)
		if err != nil {
			return err
		}
		data := make([]byte, 0, m.TotalSize())
		for _, frame := range frames.Data {
			out, err := reorderNativeFrame(ctx, frame, m, target)
			if err != nil {
				return err
			}
			data = append(data, out...)
		}
		element, _ := dataset.Get(core.TagPixelData)
		dataset.Put(core.NewRawElement(core.TagPixelData, element.VR(), data))
	}
	putOutputPlanar(dataset, target)
	return nil
}

func putOutputPlanar(dataset *object.Object, target uint16) {
	var data [2]byte
	order := dataset.ValueByteOrder()
	if order == nil {
		order = binary.LittleEndian
	}
	order.PutUint16(data[:], target)
	dataset.Put(core.NewRawElement(tagPlanarConfiguration, core.VRUS, data[:]))
}

func applyMaterializedOutputOptions(ctx context.Context, source, result *object.Object, target transfer.Syntax, o TranscodeOptions, report *TranscodeReport) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if o.PlanarOutput == PlanarOutputDefault && !explicitEncapsulation(o) && !o.SecondaryCapture && o.UIDPolicy == TranscodeUIDDefault {
		return nil
	}
	limits, err := normalizeTranscodeLimits(o.Limits)
	if err != nil {
		return err
	}
	if err := applyNativeOutputPlanar(ctx, result, o.PlanarOutput); err != nil {
		return err
	}
	if o.PlanarOutput != PlanarOutputDefault {
		report.PixelDataPreserved = false
	}
	if explicitEncapsulation(o) {
		reader, err := NewFrameReader(ctx, &object.File{Dataset: result, TransferSyntax: target}, nil, outputFrameReaderOptions(o, limits))
		if err != nil {
			return err
		}
		lengths := make([]uint64, reader.Metadata().NumberOfFrames)
		for i := range lengths {
			n, err := reader.RawFrameLength(i)
			if err != nil {
				return err
			}
			lengths[i] = uint64(n)
		}
		layout, err := outputLayout(lengths, target, o, limits)
		if err != nil {
			return err
		}
		fragments := make([][]byte, 0, layout.Fragments)
		for i := range lengths {
			frame, err := reader.RawFrame(ctx, i)
			if err != nil {
				return err
			}
			for len(frame) > 0 {
				n := int(encaplayout.FragmentLength(uint64(len(frame)), uint64(o.MaxFragmentBytes)))
				fragments = append(fragments, frame[:n:n])
				frame = frame[n:]
			}
		}
		result.Remove(tagExtendedOffsetTable)
		result.Remove(tagExtendedOffsetTableLengths)
		result.Remove(tagEncapsulatedPixelDataValueTotalLength)
		result.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true}, Value: core.FragmentSequence{OffsetTable: layout.Basic, Fragments: fragments}})
		if layout.Extended != nil {
			result.Put(core.NewRawElement(tagExtendedOffsetTable, core.VROV, layout.Extended))
			result.Put(core.NewRawElement(tagExtendedOffsetTableLengths, core.VROV, layout.Lengths))
		}
	}
	if err := applyTranscodeIdentity(source, result, report, o); err != nil {
		return err
	}
	_, err = measureTranscodeObject(ctx, result, limits)
	return err
}

func outputFrameReaderOptions(o TranscodeOptions, limits TranscodeLimits) FrameReaderOptions {
	return FrameReaderOptions{Registry: o.DecoderRegistry, MaxFrames: limits.MaxFrames, MaxFragments: limits.MaxFragments, Limits: DecompressLimits{MaxPixels: int64(limits.MaxPixels), MaxInputBytes: limits.MaxOutputBytes, MaxNativeBytes: limits.MaxOutputBytes, MaxExpansionRatio: limits.MaxExpansionRatio}}
}

func outputLayout(lengths []uint64, target transfer.Syntax, o TranscodeOptions, limits TranscodeLimits) (encaplayout.Layout, error) {
	layout, err := encaplayout.Plan(lengths, uint64(o.MaxFragmentBytes), encaplayout.Policy(o.OffsetTable), limits.MaxFragments, encaplayout.SingleFragment(target.UID))
	if err != nil {
		if errors.Is(err, encaplayout.ErrFragmentLimit) {
			return layout, transcodeLimitError("fragments")
		}
		return layout, fmt.Errorf("%w: output fragment layout: %v", ErrTranscodeUnsupported, err)
	}
	return layout, nil
}

func applyTranscodeIdentity(source, result *object.Object, report *TranscodeReport, o TranscodeOptions) error {
	if o.UIDPolicy == TranscodeUIDPreserve && report.Lossy {
		return fmt.Errorf("%w: a new lossy generation requires a new UID", ErrTranscodeUnsupported)
	}
	if !o.SecondaryCapture && o.UIDPolicy != TranscodeUIDAlways {
		return nil
	}
	report.IdentityChanged = true
	step := derivation.Derivation{Description: "New SOP Instance created by transfer syntax conversion"}
	if predecessor, err := derivation.SourceOf(source, &derivation.SourceImageForProcessing); err == nil {
		step.Sources = []derivation.Source{predecessor}
	}
	if o.SecondaryCapture {
		if err := validateSecondaryCapture(result); err != nil {
			return err
		}
		step.Description = "Converted to single-frame Secondary Capture"
		result.Put(core.Element{Header: core.ElementHeader{Tag: tagOutputSOPClass, VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}})
		result.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0064), VR: core.VRCS}, Value: core.StringValue{"WSD"}})
		result.Put(core.Element{Header: core.ElementHeader{Tag: tagImageType, VR: core.VRCS}, Value: core.StringValue{"DERIVED", "SECONDARY"}})
		result.Remove(tagNumberOfFrames)
		result.Remove(core.NewTag(0x0028, 0x0009))
		for _, attr := range []struct {
			tag core.Tag
			vr  core.VR
		}{
			{core.NewTag(0x0010, 0x0010), core.VRPN}, {core.NewTag(0x0010, 0x0020), core.VRLO}, {core.NewTag(0x0010, 0x0030), core.VRDA}, {core.NewTag(0x0010, 0x0040), core.VRCS},
			{core.NewTag(0x0008, 0x0020), core.VRDA}, {core.NewTag(0x0008, 0x0030), core.VRTM}, {core.NewTag(0x0008, 0x0090), core.VRPN}, {core.NewTag(0x0020, 0x0010), core.VRSH}, {core.NewTag(0x0008, 0x0050), core.VRSH},
			{core.NewTag(0x0020, 0x0011), core.VRIS}, {core.NewTag(0x0020, 0x0013), core.VRIS}, {core.NewTag(0x0020, 0x0020), core.VRCS}, {core.NewTag(0x0008, 0x0023), core.VRDA}, {core.NewTag(0x0008, 0x0033), core.VRTM}, {core.NewTag(0x0008, 0x002a), core.VRDT},
		} {
			if !result.Has(attr.tag) {
				result.Put(core.Element{Header: core.ElementHeader{Tag: attr.tag, VR: attr.vr}, Value: core.StringValue{}})
			}
		}
	}
	if err := derivation.ApplyTo(result, step); err != nil {
		return err
	}
	if !report.Lossy {
		newUID, err := newTranscodeUID(o.UIDRoot)
		if err != nil {
			return &TranscodeError{Stage: "uid", Err: ErrTranscodeTransaction}
		}
		result.Put(core.Element{Header: core.ElementHeader{Tag: tagSOPInstanceUID, VR: core.VRUI}, Value: core.StringValue{newUID}})
	}
	return nil
}

// rawOutputFrameSource repacks existing codestreams one frame at a time,
// without a codec or a new lossy generation.
type rawOutputFrameSource struct {
	ctx    context.Context
	reader *FrameReader
}

func (f *rawOutputFrameSource) Frames() int                      { return f.reader.Metadata().NumberOfFrames }
func (f *rawOutputFrameSource) FrameLength(i int) (int64, error) { return f.reader.RawFrameLength(i) }
func (f *rawOutputFrameSource) WriteFrameTo(i int, w io.Writer) (int64, error) {
	frame, err := f.reader.RawFrame(f.ctx, i)
	if err != nil {
		return 0, err
	}
	n, err := w.Write(frame)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	return int64(n), err
}

func elementText(dataset *object.Object, tag core.Tag) string {
	if dataset == nil {
		return ""
	}
	value, _ := dataset.GetString(tag)
	return value
}

func (s *transcodeStream) nativeOutputHeader(decoded *object.Object) (*object.Object, error) {
	header := s.nativeHeader(decoded)
	if s.options.PlanarOutput == PlanarOutputDefault {
		return header, nil
	}
	m, err := ExtractMetadata(header)
	if err != nil {
		return nil, err
	}
	target, err := outputPlanar(header, m, s.options.PlanarOutput)
	if err != nil {
		return nil, err
	}
	if m.SamplesPerPixel > 1 {
		putOutputPlanar(header, target)
	}
	return header, nil
}

func (s *transcodeStream) nativeOutputFrame(ctx context.Context, decoded *object.Object, frame []byte) ([]byte, error) {
	if s.options.PlanarOutput == PlanarOutputDefault {
		return frame, nil
	}
	m, err := ExtractMetadata(decoded)
	if err != nil {
		return nil, err
	}
	target, err := outputPlanar(decoded, m, s.options.PlanarOutput)
	if err != nil {
		return nil, err
	}
	if m.SamplesPerPixel == 1 {
		return frame, nil
	}
	return reorderNativeFrame(ctx, frame, m, target)
}

func (s *transcodeStream) frameWriteOptions(source object.FrameSource) (object.FrameWriteOptions, error) {
	if !s.target.Encapsulated {
		return object.FrameWriteOptions{WriteFileOptions: s.options.WriteOptions}, nil
	}
	lengths := make([]uint64, source.Frames())
	for i := range lengths {
		n, err := source.FrameLength(i)
		if err != nil {
			return object.FrameWriteOptions{}, err
		}
		if n <= 0 {
			return object.FrameWriteOptions{}, ErrPixelDataSizeMismatch
		}
		lengths[i] = uint64(n)
	}
	layout, err := outputLayout(lengths, s.target, s.options, s.limits)
	if err != nil {
		return object.FrameWriteOptions{}, err
	}
	s.outputFragments = layout.Fragments
	return object.FrameWriteOptions{WriteFileOptions: s.options.WriteOptions, OffsetTable: s.options.OffsetTable, MaxFragmentBytes: s.options.MaxFragmentBytes, MaxFragments: s.limits.MaxFragments}, nil
}
