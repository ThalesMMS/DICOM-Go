package pixeldata

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// IconTranscodeAction describes the treatment of one icon's pixel payload.
type IconTranscodeAction string

const (
	IconPixelsPreservedNative       IconTranscodeAction = "preserved-native"
	IconPixelsDecompressed          IconTranscodeAction = "decompressed"
	IconPixelsCompressed            IconTranscodeAction = "compressed"
	IconPixelsPreservedEncapsulated IconTranscodeAction = "preserved-encapsulated"
	IconPixelsAbsent                IconTranscodeAction = "absent"
)

// IconTranscodeReport contains value-free operational metadata for an icon.
// MetadataIncomplete marks an unchanged native icon with missing pixel
// metadata. Such pixels can be preserved, but are never inferred or encoded.
type IconTranscodeReport struct {
	Path               PixelDataPath
	Action             IconTranscodeAction
	MetadataIncomplete bool
	NativeBytes        int64
	EncodedBytes       int64
	// DecodeDiagnostics records opt-in metadata/decode repairs for this item.
	DecodeDiagnostics           []string
	EncodeCompatibilityProfiles []EncoderCompatibilityProfile
}

type iconSequenceTranscode struct {
	path     PixelDataPath
	sequence core.SequenceValue
}

func transcodeDataSetIcons(ctx context.Context, dataset *object.Object, source, target transfer.Syntax, options TranscodeOptions) (*object.Object, TranscodeReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, TranscodeReport{}, err
	}
	if dataset == nil {
		return nil, TranscodeReport{}, fmt.Errorf("%w: dataset is nil", ErrMissingMetadata)
	}
	limits, err := normalizeTranscodeLimits(options.Limits)
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	nested, err := hasNestedPixelData(dataset, limits)
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	if !nested {
		return transcodeDataSet(ctx, dataset, source, target, options)
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	// Validate the original items before Object facades can apply last-wins
	// semantics to duplicate tags, including ambiguous Pixel Data or icons.
	structuralNodes := 0
	if err := validateUniqueCoreDataSet(ctx, dataset.ToDataSet(), 1, limits, &structuralNodes); err != nil {
		return nil, TranscodeReport{}, err
	}
	inputLimits := limits
	inputLimits.MaxOutputBytes = limits.MaxInputBytes
	if _, err := measureTranscodeObject(ctx, dataset, inputLimits); err != nil {
		return nil, TranscodeReport{}, err
	}
	source, err = canonicalSourceTransferSyntax(source)
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	target, err = canonicalTargetTransferSyntax(target)
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	main, sequences, err := iconTranscodeParts(ctx, dataset, limits)
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	if len(sequences) == 0 {
		return transcodeDataSet(ctx, dataset, source, target, options)
	}
	var reports []IconTranscodeReport
	var iconBytes int64
	if err := checkIconPixelFrameBudget(ctx, main, sequences, dataset.ValueByteOrder(), limits); err != nil {
		return nil, TranscodeReport{}, err
	}
	for si := range sequences {
		seq := &sequences[si]
		items := make([]core.DataSet, len(seq.sequence.Items))
		for i, item := range seq.sequence.Items {
			if err := ctx.Err(); err != nil {
				return nil, TranscodeReport{}, err
			}
			icon := object.FromDataSet(item, nil)
			icon.SetValueByteOrder(dataset.ValueByteOrder())
			path := append(seq.path.Clone(), PixelDataPathStep{Tag: tagIconImageSequence, ItemIndex: i})
			iconOptions := options
			iconOptions.Limits = limits
			if iconBytes >= limits.MaxOutputBytes {
				return nil, TranscodeReport{}, transcodeLimitError("output_bytes")
			}
			iconOptions.Limits.MaxOutputBytes -= iconBytes
			// Main-image rewrite policy must not reinterpret an independently
			// described palette/color icon or attach derivation to it.
			iconOptions.Rewrite = nil
			converted, report, err := transcodeIcon(ctx, icon, source, target, iconOptions)
			if err != nil {
				return nil, TranscodeReport{}, &TranscodeError{Stage: "icon", Err: err}
			}
			usage, err := measureTranscodeObject(ctx, converted, limits)
			if err != nil {
				return nil, TranscodeReport{}, err
			}
			if usage.bytes > limits.MaxOutputBytes-iconBytes {
				return nil, TranscodeReport{}, transcodeLimitError("output_bytes")
			}
			iconBytes += usage.bytes
			report.Path = path
			reports = append(reports, report)
			items[i] = converted.ToDataSet()
		}
		seq.sequence.Items = items
	}
	if iconBytes >= limits.MaxOutputBytes {
		return nil, TranscodeReport{}, transcodeLimitError("output_bytes")
	}
	options.Limits = limits
	options.Limits.MaxOutputBytes -= iconBytes
	output, report, err := transcodeDataSet(ctx, main, source, target, options)
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	// Recheck the aggregate after recovery/rewrite. Metadata that was not
	// readable on input must not evade the shared pixel/frame budget.
	if err := checkIconPixelFrameBudget(ctx, output, sequences, target.ByteOrder, limits); err != nil {
		return nil, TranscodeReport{}, err
	}
	replacements := make(map[string]core.SequenceValue, len(sequences))
	for _, seq := range sequences {
		replacements[seq.path.String()] = seq.sequence
	}
	restored, err := restoreTranscodeIcons(ctx, output.Elements(), nil, replacements)
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	output = object.FromElements(restored, nil)
	output.SetValueByteOrder(target.ByteOrder)
	if _, err := measureTranscodeObject(ctx, output, limits); err != nil {
		return nil, TranscodeReport{}, err
	}
	report.Icons = reports
	return output, report, nil
}

// checkIconPixelFrameBudget bounds the frames and pixels of main, the image
// without its icons, and of every icon in sequences together against
// limits.MaxFrames and MaxPixels, and refuses an icon of more than one
// frame. Image attributes that cannot be read do not count. The whole-object
// route of an object with Icon Image Sequences checks the source with it
// before it converts anything and the result again afterwards, and
// CheckTranscodeDecodeLimits applies it before a conversion starts.
func checkIconPixelFrameBudget(ctx context.Context, main *object.Object, sequences []iconSequenceTranscode, order binary.ByteOrder, limits TranscodeLimits) error {
	var pixels uint64
	var frames int
	account := func(m Metadata) error {
		if m.NumberOfFrames <= 0 || m.NumberOfFrames > limits.MaxFrames-frames {
			return transcodeLimitError("icon_pixels_or_frames")
		}
		area := uint64(m.Rows) * uint64(m.Columns)
		if area > 0 && uint64(m.NumberOfFrames) > (limits.MaxPixels-pixels)/area {
			return transcodeLimitError("icon_pixels_or_frames")
		}
		pixels += area * uint64(m.NumberOfFrames)
		frames += m.NumberOfFrames
		return nil
	}
	if m, err := ExtractMetadata(main); err == nil {
		if err := account(m); err != nil {
			return err
		}
	}
	for _, seq := range sequences {
		for _, item := range seq.sequence.Items {
			if err := ctx.Err(); err != nil {
				return err
			}
			icon := object.FromDataSet(item, nil)
			icon.SetValueByteOrder(order)
			m, err := ExtractMetadata(icon)
			if err != nil {
				continue
			}
			if m.NumberOfFrames != 1 {
				return &TranscodeError{Stage: "icon", Err: ErrTranscodeUnsupported}
			}
			if err := account(m); err != nil {
				return err
			}
		}
	}
	return nil
}

// iconTranscodeParts splits dataset as the whole-object route does for an
// object with Icon Image Sequences: the image without its icons and the icon
// sequences. It returns no sequences for an object without nested Pixel
// Data, which that route converts like any other.
func iconTranscodeParts(ctx context.Context, dataset *object.Object, limits TranscodeLimits) (*object.Object, []iconSequenceTranscode, error) {
	nested, err := hasNestedPixelData(dataset, limits)
	if err != nil || !nested {
		return nil, nil, err
	}
	var sequences []iconSequenceTranscode
	stripped, err := stripTranscodeIcons(ctx, dataset.Elements(), nil, &sequences)
	if err != nil || len(sequences) == 0 {
		return nil, nil, err
	}
	main := object.FromElements(stripped, nil)
	main.SetValueByteOrder(dataset.ValueByteOrder())
	return main, sequences, nil
}

// stripTranscodeIcons leaves an empty sequence marker at each original path.
// The top-level pipeline can preserve/rewrite it without handling icon pixels.
func stripTranscodeIcons(ctx context.Context, elements []core.Element, path PixelDataPath, sequences *[]iconSequenceTranscode) ([]core.Element, error) {
	out := append([]core.Element(nil), elements...)
	for i := range out {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		seq, ok := out[i].Value.(core.SequenceValue)
		if !ok {
			continue
		}
		if out[i].Tag() == tagIconImageSequence {
			*sequences = append(*sequences, iconSequenceTranscode{path: path.Clone(), sequence: seq})
			out[i].Value = core.SequenceValue{}
			out[i].Header.Length = core.UndefinedLength
			out[i].Header.LengthSet = true
			continue
		}
		items := make([]core.DataSet, len(seq.Items))
		for j, item := range seq.Items {
			next := append(path.Clone(), PixelDataPathStep{Tag: out[i].Tag(), ItemIndex: j})
			elements, err := stripTranscodeIcons(ctx, item.Elements, next, sequences)
			if err != nil {
				return nil, err
			}
			items[j] = core.DataSet{Elements: elements}
		}
		seq.Items = items
		out[i].Value = seq
	}
	return out, nil
}

func restoreTranscodeIcons(ctx context.Context, elements []core.Element, path PixelDataPath, sequences map[string]core.SequenceValue) ([]core.Element, error) {
	for i := range elements {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		seq, ok := elements[i].Value.(core.SequenceValue)
		if !ok {
			continue
		}
		if elements[i].Tag() == tagIconImageSequence {
			replacement, ok := sequences[path.String()]
			if !ok {
				return nil, ErrTranscodeUnsupported
			}
			elements[i].Value = replacement
			continue
		}
		for j := range seq.Items {
			next := append(path.Clone(), PixelDataPathStep{Tag: elements[i].Tag(), ItemIndex: j})
			if _, err := restoreTranscodeIcons(ctx, seq.Items[j].Elements, next, sequences); err != nil {
				return nil, err
			}
		}
		elements[i].Value = seq
	}
	return elements, nil
}

func transcodeIcon(ctx context.Context, icon *object.Object, source, target transfer.Syntax, options TranscodeOptions) (*object.Object, IconTranscodeReport, error) {
	pixel, err := ExtractView(icon)
	if err == ErrPixelDataNotFound {
		nativeSource, nativeTarget := source, target
		if source.Encapsulated {
			nativeSource = transfer.ExplicitVRLittleEndian
		}
		if target.Encapsulated {
			nativeTarget = transfer.ExplicitVRLittleEndian
		}
		clone, _, err := transcodeDataSet(ctx, icon, nativeSource, nativeTarget, options)
		return clone, IconTranscodeReport{Action: IconPixelsAbsent}, err
	}
	if err != nil {
		return nil, IconTranscodeReport{}, err
	}
	iconSource := source
	if !pixel.Encapsulated && source.Encapsulated {
		// PS3.5 A.4: a native icon in an encapsulated dataset is LE.
		iconSource = transfer.ExplicitVRLittleEndian
	}
	iconTarget := target
	if target.Encapsulated && !options.CompressIcons {
		iconTarget = transfer.ExplicitVRLittleEndian
	}
	_, metadataErr := ExtractMetadata(icon)
	var diagnostics []string
	if metadataErr != nil && options.Recovery.DefaultImagePixelModule {
		if icon, diagnostics, err = withLegacyImageRecovery(icon, iconSource, options.Recovery); err != nil {
			return nil, IconTranscodeReport{}, err
		}
		_, metadataErr = ExtractMetadata(icon)
	}
	if m, err := ExtractMetadata(icon); err == nil && m.NumberOfFrames != 1 {
		return nil, IconTranscodeReport{}, ErrTranscodeUnsupported
	}
	if !pixel.Encapsulated && metadataErr == nil {
		if _, err := ExtractNativeFramesView(icon); err != nil {
			return nil, IconTranscodeReport{}, err
		}
	}
	if iconTarget.Encapsulated && options.CompressIcons {
		m, err := ExtractMetadata(icon)
		if err != nil {
			return nil, IconTranscodeReport{}, err
		}
		if iconTarget.UID != transfer.EncapsulatedUncompressedExplicitVRLittleEndian.UID && (iconSource.UID != iconTarget.UID || options.ForceReencode || requiresLegacyPixelRepair(options.Recovery)) {
			resolved, err := resolveFrameEncoder(iconTarget, m, options)
			if err != nil {
				return nil, IconTranscodeReport{}, err
			}
			if !resolved.capabilities.Lossless {
				return nil, IconTranscodeReport{}, fmt.Errorf("%w: icon encoding requires a lossless codec", ErrTranscodeLossyDisallowed)
			}
		}
	}
	converted, r, err := transcodeDataSet(ctx, icon, iconSource, iconTarget, options)
	if err != nil {
		return nil, IconTranscodeReport{}, err
	}
	if !r.PixelDataPreserved {
		// Verify an icon before it can join an output, just as TranscodePath
		// verifies top-level frames. This also requires a target decoder.
		if err := verifyLosslessTranscodeWithRepairs(ctx, icon, iconSource, converted, iconTarget, options, options.Limits); err != nil {
			return nil, IconTranscodeReport{}, err
		}
	}
	action := IconPixelsPreservedNative
	switch {
	case iconTarget.Encapsulated && r.PixelDataPreserved:
		action = IconPixelsPreservedEncapsulated
	case iconTarget.Encapsulated:
		action = IconPixelsCompressed
	case pixel.Encapsulated:
		action = IconPixelsDecompressed
	}
	// A native icon's OW payload follows the overall target byte order.
	converted.SetValueByteOrder(iconTarget.ByteOrder)
	if iconTarget.ByteOrder == nil {
		converted.SetValueByteOrder(binary.LittleEndian)
	}
	diagnostics = append(diagnostics, r.DecodeDiagnostics...)
	return converted, IconTranscodeReport{Action: action, MetadataIncomplete: metadataErr != nil, NativeBytes: r.NativeBytes, EncodedBytes: r.EncodedBytes, DecodeDiagnostics: diagnostics, EncodeCompatibilityProfiles: append([]EncoderCompatibilityProfile(nil), r.EncodeCompatibilityProfiles...)}, nil
}
