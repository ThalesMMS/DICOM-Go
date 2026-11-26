package pixeldata

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	ErrTranscodeUnsupported       = errors.New("dicom: transfer syntax transcode is unsupported")
	ErrTranscodeResourceLimit     = errors.New("dicom: transcode resource limit exceeded")
	ErrTranscodeLossyDisallowed   = errors.New("dicom: lossy transcode requires explicit opt-in")
	ErrTranscodeSourceChanged     = errors.New("dicom: transcode source changed")
	ErrTranscodeDestinationUnsafe = errors.New("dicom: transcode destination is unsafe")
	ErrTranscodeTransaction       = errors.New("dicom: transcode transaction failed")
)

var (
	tagFloatPixelData              = core.NewTag(0x7FE0, 0x0008)
	tagDoubleFloatPixelData        = core.NewTag(0x7FE0, 0x0009)
	tagLossyImageCompression       = core.NewTag(0x0028, 0x2110)
	tagLossyImageCompressionRatio  = core.NewTag(0x0028, 0x2112)
	tagLossyImageCompressionMethod = core.NewTag(0x0028, 0x2114)
	tagImageType                   = core.NewTag(0x0008, 0x0008)
	tagSOPInstanceUID              = core.NewTag(0x0008, 0x0018)
	tagMediaStorageSOPInstanceUID  = core.NewTag(0x0002, 0x0003)
)

const (
	defaultTranscodeMaxFrames      = 10000
	defaultTranscodeMaxPixels      = uint64(1) << 30
	defaultTranscodeMaxInputBytes  = int64(1) << 30
	defaultTranscodeMaxOutputBytes = int64(1) << 30
	defaultTranscodeMaxFragments   = 100000
	defaultTranscodeMaxElements    = 1000000
	defaultTranscodeMaxDepth       = 64
	defaultTranscodeMaxExpansion   = int64(1024)
	defaultTranscodeMaxDuration    = 5 * time.Minute
)

// TranscodeLimits bound all input traversal, frame processing, and output
// construction. Zero fields use finite defaults; the public API has no
// implicit unlimited mode.
type TranscodeLimits struct {
	MaxFrames         int
	MaxPixels         uint64
	MaxInputBytes     int64
	MaxOutputBytes    int64
	MaxFragments      int
	MaxElements       int
	MaxDepth          int
	MaxExpansionRatio int64
	MaxDuration       time.Duration
}

// TranscodeOptions supplies explicit codec registries and safety policy.
// DecoderRegistry is required for compressed inputs and EncoderRegistry is
// required for encoded outputs. Neither falls back to a package global.
type TranscodeOptions struct {
	// BeforeSource and BeforePublish are optional TranscodePath authorization
	// checks. An error aborts before opening or publishing, respectively.
	BeforeSource  func(context.Context) error
	BeforePublish func(context.Context) error

	// WriteOptions applies only to TranscodePath's final serialization. APIs
	// returning Object/File do not serialize and leave these options to the
	// caller's object.WriteFileWithOptions. It does not configure pixel codecs.
	WriteOptions object.WriteFileOptions
	// MaxFragmentBytes and OffsetTable control top-level encapsulated output.
	// Zero keeps the stored layout on an equivalent-syntax copy, and one
	// Fragment per frame with automatic tables after an encode. A positive
	// fragment cap includes Item padding and must be even and at least 2.
	// Native targets reject explicit encapsulation options.
	MaxFragmentBytes int64
	OffsetTable      object.OffsetTablePolicy
	// PlanarOutput controls native color output, preserving the historical
	// layout at zero. Encapsulated targets reject explicit planar policies;
	// their codec determines the representation.
	PlanarOutput PlanarOutputPolicy
	// SecondaryCapture converts a compatible single-frame image to classic
	// Secondary Capture with Conversion Type WSD and a new SOP Instance UID.
	// Multi-frame and functional-group IODs are explicitly unsupported.
	SecondaryCapture bool
	// UIDPolicy leaves existing lossless/copy identity unchanged at zero;
	// a new lossy generation always receives a new UID. Preserve conflicts
	// with SecondaryCapture or a newly lossy encode and is refused.
	// UIDRoot selects an organizational root only for newly generated SOP
	// Instance UIDs. Empty preserves UUID generation. It never remaps Study,
	// Series or predecessor references and conflicts with UIDPreserve.
	UIDRoot         string
	UIDPolicy       TranscodeUIDPolicy
	DecoderRegistry Registry
	EncoderRegistry EncoderRegistry
	Limits          TranscodeLimits
	AllowLossy      bool
	// Pipeline controls bounded frame work in TranscodePath's streaming route.
	// In-memory adapters continue to use their bounded whole-object route.
	Pipeline TranscodePipelineOptions
	// SkipVerification omits TranscodePath's additional top-level output decode/pixel
	// comparison. False preserves verification; structural readback, codec
	// output validation, limits and atomic publication always remain mandatory.
	SkipVerification bool
	// MaxPixelError opts into lossy verification in stored-sample units. Nil
	// preserves the default policy; zero requires zero measured error (exact
	// samples only when AverageBlockSize is zero or one).
	// The finite, nonnegative value is copied at the public call boundary.
	// It cannot be combined with SkipVerification. Comparison uses the native
	// samples delivered to the encoder (after any explicit Rewrite), without
	// Rescale, LUT, VOI or display transforms. See LossyVerificationReport.
	MaxPixelError *float64
	// AverageBlockSize is the square block side for the difference of sample
	// means, separately per component and frame. Zero means one (per sample).
	// Partial edge blocks use their actual area. A positive value requires
	// MaxPixelError. Opposite signed errors within a block can cancel.
	AverageBlockSize int
	// Durability applies only to TranscodePath's filesystem publication.
	Durability TranscodeDurability
	// ForceReencode disables the equivalent-transfer-syntax pixel fast path.
	ForceReencode bool
	// CompressIcons encodes Icon Image Sequence pixels with the target codec.
	// By default icons are kept or decoded to native pixels, as PS3.5 A.4
	// permits even when top-level Pixel Data is encapsulated. Native targets
	// always receive native icons. This option requires a lossless encoder;
	// it never silently changes icon samples or infers missing metadata.
	CompressIcons bool
	// ExtractEmbeddedOverlays moves overlays stored in unused bits of the
	// pixel samples (Overlay Bits Allocated > 1) to 1-bit Overlay Data
	// (60xx,3000) and clears those bits before encoding, since encoders keep
	// only Bits Stored. Without it such an encoded target fails with
	// ErrTranscodeEmbeddedOverlay; native targets keep the bits unchanged.
	ExtractEmbeddedOverlays bool
	// RecoverUN, when set, first gives UN elements with a known, unambiguous
	// VR that VR (object.Object.RecoverUN with these options): Private
	// Creators encoded as UN become LO, public attributes take their
	// dictionary VR and private ones their PrivateDataDictionary VR, so an
	// Explicit VR target does not keep UN where the VR is known (as
	// gdcmconv -X/-U does). The recovery is reported in
	// TranscodeReport.UNRecovery; without it the output keeps the received
	// VRs.
	RecoverUN *object.UNRecoveryOptions
	// Rewrite, when set, changes the stored pixel representation between
	// decoding and encoding (RewriteNative): the source is decoded to native
	// Explicit VR Little Endian, rewritten, then encoded to the target, so
	// for example PALETTE COLOR, YBR_FULL or planar RGB can reach encoders
	// that accept only interleaved RGB. Reported in TranscodeReport.Rewrite.
	Rewrite *NativeRewrite
	// Recovery enables the opt-in decode tolerances of DecompressOptions for
	// the source; applied ones are reported in
	// TranscodeReport.DecodeDiagnostics. With
	// NativePixelDataUnderEncapsulated, native Pixel Data declared under a
	// compressed syntax is transcoded as native, so the output declares the
	// target syntax correctly.
	Recovery DecodeRecovery
	// CodestreamTranscoder, when set and able to convert the source syntax to
	// the target, rewrites the encoded frames directly instead of decoding
	// and re-encoding pixels (for example JPEG <-> JPEG XL JPEG
	// Recompression). ForceReencode disables it.
	CodestreamTranscoder CodestreamTranscoder
}

// TranscodeReport contains value-free operational metadata.
type TranscodeReport struct {
	// IdentityChanged reports caller-requested SOP identity changes (a new
	// lossy generation is additionally reported by Lossy).
	IdentityChanged    bool
	Frames             int
	NativeBytes        int64
	EncodedBytes       int64
	PixelDataPreserved bool
	Lossy              bool
	// These fields report TranscodePath's applied policies, including explicit
	// verification opt-out. PeakInFlightBytes is a conservative package-buffer
	// reservation, not measured RSS; the disk spool has a separate output cap.
	Verification TranscodeVerification
	// LossyVerification records the measured error when MaxPixelError was
	// applied successfully to a newly lossy representation. Nil means the
	// lossy error metric was not applied, including pass-through outputs.
	LossyVerification  *LossyVerificationReport
	Durability         TranscodeDurability
	FileSynced         bool
	DirectorySynced    bool
	WorkerLimit        int
	PeakInFlightFrames int
	PeakInFlightBytes  int64
	Timings            TranscodeTimings
	// Streaming distinguishes the bounded frame route from whole-object
	// fallback (recovery, rewrite, deflated/big-endian and other layouts).
	Streaming bool
	// EmbeddedOverlaysExtracted counts overlays moved out of the pixel
	// samples (TranscodeOptions.ExtractEmbeddedOverlays).
	EmbeddedOverlaysExtracted int
	// UNRecovery reports TranscodeOptions.RecoverUN; nil when not requested.
	UNRecovery *object.UNRecoveryReport
	// Rewrite reports TranscodeOptions.Rewrite; nil when not requested.
	Rewrite *NativeRewriteReport
	// DecodeDiagnostics lists the TranscodeOptions.Recovery tolerances that
	// were applied (DecompressReport.Diagnostics codes).
	DecodeDiagnostics []string
	// EncodeCompatibilityProfiles identifies explicitly selected output
	// outside a standard DICOM attribute profile, without patient values.
	EncodeCompatibilityProfiles []EncoderCompatibilityProfile
	// Icons records the encoding policy applied to each Icon Image Sequence
	// item. Paths contain only tags and zero-based sequence item indexes.
	Icons []IconTranscodeReport
}

// TranscodeError identifies a stable stage without echoing paths, patient
// values, or backend-controlled text.
type TranscodeError struct {
	Stage string
	Err   error
}

func (e *TranscodeError) Error() string {
	if e == nil || e.Stage == "" {
		return "dicom: transcode failed"
	}
	return "dicom: transcode failed during " + e.Stage
}

func (e *TranscodeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// DefaultTranscodeLimits returns the finite limits used by zero-valued
// TranscodeOptions.
func DefaultTranscodeLimits() TranscodeLimits {
	return TranscodeLimits{
		MaxFrames:         defaultTranscodeMaxFrames,
		MaxPixels:         defaultTranscodeMaxPixels,
		MaxInputBytes:     defaultTranscodeMaxInputBytes,
		MaxOutputBytes:    defaultTranscodeMaxOutputBytes,
		MaxFragments:      defaultTranscodeMaxFragments,
		MaxElements:       defaultTranscodeMaxElements,
		MaxDepth:          defaultTranscodeMaxDepth,
		MaxExpansionRatio: defaultTranscodeMaxExpansion,
		MaxDuration:       defaultTranscodeMaxDuration,
	}
}

// ResolveTranscodeLimits validates limits and replaces zero fields with the
// transcoder's finite defaults. Adapters can use the same bounds for preparation
// and input parsing without duplicating transcode policy.
func ResolveTranscodeLimits(limits TranscodeLimits) (TranscodeLimits, error) {
	return normalizeTranscodeLimits(limits)
}

// TranscodeDataSet returns a detached data set encoded for target. Input is
// borrowed and never mutated.
func TranscodeDataSet(ctx context.Context, dataset *object.Object, source, target transfer.Syntax, options TranscodeOptions) (*object.Object, TranscodeReport, error) {
	var err error
	options, err = ResolveTranscodeVerificationOptions(options)
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	if options.SecondaryCapture {
		if ctx == nil {
			ctx = context.Background()
		}
		limits, err := normalizeTranscodeLimits(options.Limits)
		if err != nil {
			return nil, TranscodeReport{}, err
		}
		bounded, cancel := context.WithTimeout(ctx, limits.MaxDuration)
		defer cancel()
		ctx = bounded
		limits.MaxOutputBytes = limits.MaxInputBytes
		if _, err := measureTranscodeObject(ctx, dataset, limits); err != nil {
			return nil, TranscodeReport{}, err
		}
	}
	if err := validateTranscodeOutputOptions(dataset, target, options); err != nil {
		return nil, TranscodeReport{}, err
	}
	result, report, err := transcodeDataSetIcons(ctx, dataset, source, target, options)
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	if err := applyMaterializedOutputOptions(ctx, dataset, result, target, options, &report); err != nil {
		return nil, TranscodeReport{}, err
	}
	return result, report, nil
}

// transcodeDataSet processes top-level pixels; its input has icon sequences
// temporarily removed so their independently described pixels cannot be
// mistaken for top-level frames or endian-swapped twice.
func transcodeDataSet(ctx context.Context, dataset *object.Object, source, target transfer.Syntax, options TranscodeOptions) (*object.Object, TranscodeReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, TranscodeReport{}, err
	}
	if dataset == nil {
		return nil, TranscodeReport{}, fmt.Errorf("%w: dataset is nil", ErrMissingMetadata)
	}
	if options.RecoverUN != nil {
		recovered, unReport, err := dataset.RecoverUN(ctx, *options.RecoverUN)
		if err != nil {
			return nil, TranscodeReport{}, &TranscodeError{Stage: "un_recovery", Err: err}
		}
		options.RecoverUN = nil
		output, report, err := transcodeDataSet(ctx, recovered, source, target, options)
		if err != nil {
			return nil, TranscodeReport{}, err
		}
		report.UNRecovery = &unReport
		return output, report, nil
	}
	if options.Recovery.DefinedLengthFragments {
		options.Recovery.DefinedLengthFragments = false
		if split, ok := definedLengthFragments(dataset, source); ok {
			output, report, err := transcodeDataSet(ctx, split, source, target, options)
			if err != nil {
				return nil, TranscodeReport{}, err
			}
			report.DecodeDiagnostics = append([]string{DiagnosticDefinedLengthFragments}, report.DecodeDiagnostics...)
			return output, report, nil
		}
	}
	if options.Recovery.NativePixelDataUnderEncapsulated {
		options.Recovery.NativePixelDataUnderEncapsulated = false
		if native, ok := nativeUnderEncapsulated(dataset, source); ok {
			output, report, err := transcodeDataSet(ctx, dataset, native, target, options)
			if err != nil {
				return nil, TranscodeReport{}, err
			}
			report.DecodeDiagnostics = append([]string{DiagnosticNativePixelDataUnderEncapsulated}, report.DecodeDiagnostics...)
			return output, report, nil
		}
	}
	if options.Rewrite != nil {
		rewrite := *options.Rewrite
		options.Rewrite = nil
		native, decodeReport, err := transcodeDataSet(ctx, dataset, source, transfer.ExplicitVRLittleEndian, options)
		if err != nil {
			return nil, TranscodeReport{}, err
		}
		rewritten, rewriteReport, err := RewriteNative(ctx, native, rewrite)
		if err != nil {
			return nil, TranscodeReport{}, &TranscodeError{Stage: "rewrite", Err: err}
		}
		options.Recovery = DecodeRecovery{}
		output, report, err := transcodeDataSet(ctx, rewritten, transfer.ExplicitVRLittleEndian, target, options)
		if err != nil {
			return nil, TranscodeReport{}, err
		}
		report.Rewrite = &rewriteReport
		report.DecodeDiagnostics = append(decodeReport.DecodeDiagnostics, report.DecodeDiagnostics...)
		return output, report, nil
	}
	limits, err := normalizeTranscodeLimits(options.Limits)
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
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
	legacyRepair := requiresLegacyPixelRepair(options.Recovery)
	hasPixels := hasAnyPixelData(dataset)
	plan := buildTranscodePlan(source, target, options.ForceReencode || legacyRepair, hasPixels)
	// A metadata-only parent may still own separately repaired icon images.
	if legacyRepair && hasPixels && plan.mode == transcodeNativeClone {
		plan.mode = transcodeDecodeNative
	}
	validated, err := validateTranscodeInput(dataset, plan, limits)
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	if plan.mode == transcodeNativeClone {
		return cloneNativeTranscode(ctx, validated, limits)
	}
	metadata, pixel := validated.metadata, validated.pixel
	if plan.mode == transcodeEquivalentClone {
		clone, cloneErr := cloneDetachedObject(ctx, dataset, equivalentCloneLimits(limits))
		if cloneErr != nil {
			return nil, TranscodeReport{}, cloneErr
		}
		return clone, TranscodeReport{
			Frames:             metadata.NumberOfFrames,
			NativeBytes:        metadata.TotalSize(),
			EncodedBytes:       pixelPayloadBytes(pixel),
			PixelDataPreserved: true,
		}, nil
	}
	if usesCodestreamTranscoder(plan, options) {
		return transcodeCodestreams(ctx, dataset, metadata, pixel, plan, options, limits)
	}

	if source.Encapsulated && source.UID != transfer.EncapsulatedUncompressedExplicitVRLittleEndian.UID && options.DecoderRegistry == nil {
		return nil, TranscodeReport{}, codecAvailabilityError(ErrCodecRegistryNil, source.UID, nil)
	}
	var decodeReport DecompressReport
	decompressed, _, err := DecompressDataSetContext(ctx, dataset, source, DecompressOptions{
		Registry:             options.DecoderRegistry,
		TargetTransferSyntax: transfer.ExplicitVRLittleEndian,
		Recovery:             options.Recovery,
		Report:               &decodeReport,
		Limits:               transcodeDecompressLimits(limits),
	})
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	metadata, err = ExtractMetadata(decompressed)
	if err != nil {
		return nil, TranscodeReport{}, err
	}
	nativeBytes := metadata.TotalSize()
	if nativeBytes <= 0 || nativeBytes > limits.MaxOutputBytes {
		return nil, TranscodeReport{}, transcodeLimitError("native_bytes")
	}
	report := TranscodeReport{Frames: metadata.NumberOfFrames, NativeBytes: nativeBytes, DecodeDiagnostics: decodeReport.Diagnostics}

	switch plan.mode {
	case transcodeDecodeNative:
		if target.UID == transfer.ExplicitVRBigEndian.UID {
			elements, swapErr := transcodeElementsToLittleEndian(decompressed.Elements(), binary.LittleEndian)
			if swapErr != nil {
				return nil, TranscodeReport{}, swapErr
			}
			result := object.FromElements(elements, nil)
			result.SetValueByteOrder(binary.BigEndian)
			result, cloneErr := cloneDetachedObject(ctx, result, outputCloneLimits(limits))
			if cloneErr != nil {
				return nil, TranscodeReport{}, cloneErr
			}
			return result, report, nil
		}
		result, cloneErr := cloneDetachedObject(ctx, decompressed, outputCloneLimits(limits))
		if cloneErr != nil {
			return nil, TranscodeReport{}, cloneErr
		}
		result.SetValueByteOrder(binary.LittleEndian)
		return result, report, nil
	case transcodeDecodeEncapsulatedUncompressed:
		frames, frameErr := ExtractNativeFramesView(decompressed)
		if frameErr != nil {
			return nil, TranscodeReport{}, frameErr
		}
		fragments := cloneFrames(frames.Data)
		draft, encodedBytes, encodeErr := rewriteEncodedDataSet(ctx, encodedRewriteInput{
			source: decompressed, metadata: metadata, fragments: fragments, limits: limits,
		})
		if encodeErr != nil {
			return nil, TranscodeReport{}, encodeErr
		}
		result, encodeErr := detachDataSet(ctx, draft, limits)
		if encodeErr != nil {
			return nil, TranscodeReport{}, encodeErr
		}
		report.EncodedBytes = encodedBytes
		return result.object, report, nil
	case transcodeDecodeEncode:
		moved, extracted, overlayErr := moveEmbeddedOverlays(decompressed, options.ExtractEmbeddedOverlays)
		if overlayErr != nil {
			return nil, TranscodeReport{}, overlayErr
		}
		report.EmbeddedOverlaysExtracted = extracted
		return transcodeWithEncoder(ctx, moved, metadata, target, options, limits, report)
	default:
		panic("pixeldata: invalid transcode plan")
	}
}

// TranscodeCoreDataSet is the core.DataSet adapter for TranscodeDataSet. The
// returned data set is detached from the input, including nested values.
func TranscodeCoreDataSet(ctx context.Context, dataset core.DataSet, source, target transfer.Syntax, options TranscodeOptions) (core.DataSet, TranscodeReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	limits, err := normalizeTranscodeLimits(options.Limits)
	if err != nil {
		return core.DataSet{}, TranscodeReport{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	structuralNodes := 0
	if err := validateUniqueCoreDataSet(ctx, dataset, 1, limits, &structuralNodes); err != nil {
		return core.DataSet{}, TranscodeReport{}, err
	}
	result, report, err := TranscodeDataSet(ctx, object.FromDataSet(dataset, nil), source, target, options)
	if err != nil {
		return core.DataSet{}, TranscodeReport{}, err
	}
	return result.ToDataSet(), report, nil
}
