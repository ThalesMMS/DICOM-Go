package pixeldata

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/decoderecovery"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	ErrUnsupportedDecompression = errors.New("dicom: unsupported still-image decompression")
	ErrDecompressResourceLimit  = errors.New("dicom: decompression resource limit exceeded")
	ErrInvalidDecompressLimits  = errors.New("dicom: invalid decompression limits")
)

const (
	defaultDecompressMaxFrames            = 100_000
	defaultDecompressMaxPixels      int64 = 256 * 1024 * 1024
	defaultDecompressMaxNativeBytes       = 512 << 20
	defaultDecompressMaxInputBytes        = 512 << 20
	defaultDecompressExpansionRatio       = 1024
)

var (
	tagFileMetaTransferSyntaxUID             = core.NewTag(0x0002, 0x0010)
	tagExtendedOffsetTable                   = core.NewTag(0x7FE0, 0x0001)
	tagExtendedOffsetTableLengths            = core.NewTag(0x7FE0, 0x0002)
	tagEncapsulatedPixelDataValueTotalLength = core.NewTag(0x7FE0, 0x0003)
)

type DecompressOptions struct {
	// Registry supplies optional still-image pixel codecs. When nil, the package
	// DefaultRegistry is used for compressed transfer syntaxes. Native and
	// Encapsulated Uncompressed inputs do not require a registry.
	Registry Registry
	// TargetTransferSyntax selects the native uncompressed transfer syntax for
	// the returned dataset/file. The zero value defaults to Explicit VR Little
	// Endian. Supported targets are Explicit VR Little Endian and Implicit VR
	// Little Endian.
	TargetTransferSyntax transfer.Syntax
	// Limits bounds work and allocations. Zero-valued fields use finite defaults.
	Limits DecompressLimits
	// Recovery enables opt-in tolerances for malformed encoded pixel data.
	// The zero value is strict.
	Recovery DecodeRecovery
	// Report, when non-nil, receives the codec diagnostics of the decode,
	// including every recovery that was applied.
	Report *DecompressReport
	// JPEGColorConversion selects the color policy of lossy JPEG decoding;
	// the zero value is JPEGColorGuess. The decompressed data set carries the
	// Photometric Interpretation of the decoded samples.
	JPEGColorConversion JPEGColorConversion
}

// DecodeRecovery lists opt-in decode tolerances. Each applied recovery is
// reported as a diagnostic code; none is ever applied silently.
type DecodeRecovery struct {
	// NativePlanarLayout overrides the declared layout of native unsigned
	// 8/16-bit color samples. Their byte count cannot identify their layout;
	// select this only for a producer whose actual layout is known.
	NativePlanarLayout NativePlanarRecovery
	// NativeOWByteSwap reverses each pair of bytes in native OW Pixel Data
	// with 8/16 allocated bits. This producer-specific repair is not detectable
	// and also reverses conforming input if enabled for it.
	NativeOWByteSwap bool
	// RLEReversedByteSegments reads each component's RLE byte segments least
	// significant first, rather than the standard most significant first.
	// Channel order is unchanged. Applies only above 8 allocated bits and
	// cannot be distinguished from conforming samples in the encoded stream.
	RLEReversedByteSegments bool
	// WrongPaletteAttributeTags accepts a complete unambiguous legacy palette
	// at (0028,1111-1113)/(0028,1211-1213) and replaces its tags with the
	// standard (0028,1101-1103)/(0028,1201-1203). Mixed/conflicting sets fail.
	WrongPaletteAttributeTags bool
	// ConvertRetiredColorModels normalizes unsigned full-precision native
	// ARGB/CMYK/HSV8/16 to interleaved RGB. ARGB uses the existing palette
	// override semantics (alpha is an index, not opacity); CMYK uses additive
	// ink subtraction and HSV maps binary hue bins [0,2^precision) to one
	// turn, truncating the resulting RGB channels to stored integers.
	// These explicit legacy conventions are not ICC color management.
	ConvertRetiredColorModels bool
	// IncompleteData explicitly normalizes only complete frames from a source
	// read with object.ReadFileOptions.AllowIncompletePixelData. It never fills
	// missing pixels and is not enabled by DetectableDecodeRecovery.
	IncompleteData bool
	// IgnoreInconsistentBasicOffsetTable replaces a Basic Offset Table that
	// contradicts Number of Frames or the Item layout with the bounded
	// marker scan (JPEG, JPEG-LS and JPEG 2000 codestreams). Exactly Number
	// of Frames codestreams must be found.
	IgnoreInconsistentBasicOffsetTable bool
	// DefaultImagePixelModule assumes Samples per Pixel 1 and Photometric
	// Interpretation MONOCHROME2 when they are absent, as GDCM does for
	// ACR-NEMA objects, provided the rest of the Image Pixel Module is present
	// and native Pixel Data holds one sample per pixel and cannot hold three.
	// A three-dimensional ACR-NEMA image without Number of Frames takes it
	// from Planes (0028,0012). The decompressed data set carries the values
	// explicitly. Native legacy images with Recognition Code
	// ACRNEMA_LIBIDO_1.0/1.1 also recover their reversed Rows/Columns and,
	// for the exact unsigned 24-bit layout, interleaved RGB8 metadata. The
	// consumed Recognition Code is removed and each repair is diagnosed.
	DefaultImagePixelModule bool
	// RepairPhotometricInterpretation normalizes native Image Pixel metadata
	// from known Photometric Interpretation/sample-count contracts. Missing or
	// unrecognized photometrics assume MONOCHROME2, RGB or retired ARGB from
	// declared Samples per Pixel 1, 3 or 4. The caller must know the producer
	// stores that layout: byte counts cannot distinguish RGB from YBR/HSV or
	// ARGB from CMYK. Known photometrics may correct absent/inconsistent sample
	// counts only when native byte size agrees exactly (plus a zero pad).
	// Invalid monochrome Planar Configuration 1 is ignored; inferred ARGB also
	// assumes interleaved data, while explicit valid planar ARGB is preserved.
	// Every changed attribute is diagnosed. Compressed input and ambiguous or
	// malformed metadata are refused. Strict and legacy defaults are unchanged.
	RepairPhotometricInterpretation bool
	// Unpack12BitPacked reads native Pixel Data with Bits Allocated 12,
	// which PS3.5 8.1.1 does not allow, as 16 bits allocated, as GDCM does.
	// A value of exactly 3 bytes per 2 samples (one trailing pad byte
	// allowed) is unpacked: the first sample is the first byte plus the low
	// nibble of the second as its high 4 bits, the next is the high nibble of
	// the second byte plus the third byte as its high 8 bits (ACR-NEMA
	// packing), reported as DiagnosticUnpacked12Bit. A value of exactly 2
	// bytes per sample already holds 16-bit words and only Bits Allocated
	// changes, reported as DiagnosticBitsAllocated12As16. Bits Stored and
	// High Bit are kept. Any other length is refused with
	// ErrPixelDataSizeMismatch.
	Unpack12BitPacked bool
	// JPEGLosslessEncoderQuirks retries a JPEG Lossless frame that fails
	// strict decoding while accepting known encoder quirks: a nonzero
	// spectral selection end (Se) in SOS, entropy fill bits that are not all
	// ones, and then 16 extra bits after a difference of category 16, which
	// T.81 H.1.2.2 defines without additional bits (the Cornell encoder bug;
	// the bits are read as an ordinary 16-bit extension, as dcmdjpeg +wc
	// does). Only the quirks the frame needs are applied, each one reported.
	JPEGLosslessEncoderQuirks bool
	// JPEGLosslessPredictor6Overflow decodes predictor 6 as encoders that
	// overflowed 16-bit integers computed it: Ra and Rc are taken as signed
	// 16-bit values in Rb + ((Ra - Rc) >> 1), as dcmdjpeg +w6 does. It only
	// changes 16-bit scans, cannot be detected from the stream and misdecodes
	// conforming predictor 6 images, so enable it only for sources known to
	// be affected. Frames it changed report
	// DiagnosticJPEGLosslessPredictor6Overflow.
	JPEGLosslessPredictor6Overflow bool
	// JPEGSequentialScanQuirks accepts lossy sequential DCT scans whose
	// spectral selection or successive approximation fields are not 0, 63,
	// 0, 0 (T.81 B.2.3), as libjpeg does with a warning; the fields are
	// ignored and DiagnosticJPEGScanParametersIgnored is reported.
	JPEGSequentialScanQuirks bool
	// JPEGLSJAIThresholds decodes JPEG-LS frames of more than 12 bits that
	// do not set their thresholds in an LSE segment with the thresholds the
	// JAI ImageIO encoder (and dcm4che versions using it) wrote them with:
	// the T.87 C.2.4.1.1.1 defaults without the min(MAXVAL, 4095) of FACTOR
	// (13 bits: T1 34, T2 131, T3 548 instead of 18, 67, 276), as the LSE
	// segment GDCM FixJAIBugJPEGLS and dcm4che insert. Under the standard
	// thresholds such frames fail or, worse, decode to wrong samples, and
	// conforming frames decode wrongly under these, so the bug cannot be
	// detected: enable it only for sources known to be affected.
	// Frames it applies to report DiagnosticJPEGLSJAIThresholds.
	JPEGLSJAIThresholds bool
	// JPEGLSHPColorTransforms accepts the nonstandard APP8 mrfx HP1/HP2/HP3
	// color transforms only for qualified lossless unsigned RGB8/RGB16,
	// full-range precision and ILV=1/2. Strict decoding remains the default.
	// Each applied transform and the implementation used are reported as
	// DiagnosticJPEGLSHP1/HP2/HP3 and DiagnosticJPEGLSHPBackendPureGo/CharLS.
	JPEGLSHPColorTransforms bool
	// RLESegmentOverrun decodes RLE segments whose runs overrun the segment
	// or the frame: a literal run cut by the segment end keeps the bytes
	// present, runs past the frame are clipped and short segments are
	// zero-filled, as GDCM does (reported as DiagnosticRLESegmentOverrun).
	RLESegmentOverrun bool
	// ReconcileWithCodestream replaces Rows, Columns, Samples per Pixel,
	// Photometric Interpretation, Planar Configuration, Bits Allocated, Bits
	// Stored and High Bit that are absent or disagree with the header of the
	// first codestream: the frame header of JPEG Baseline, Extended and
	// Lossless, and the SIZ marker segment of JPEG 2000 (Part 1, Part 2 and
	// HTJ2K), whose component sign also decides Pixel Representation, as GDCM
	// does. DecompressReport.Reconciliations lists every replacement, and the
	// decompressed data set carries the codestream values.
	ReconcileWithCodestream bool
	// UnwrapJP2Container decodes the codestream of JPEG 2000 Pixel Data that
	// holds JP2 files (PS3.5 A.4.4 requires the bare codestream): a single
	// frame whose fragments form one JP2 file, or one JP2 file per frame.
	// DiagnosticJP2ContainerUnwrapped reports it.
	UnwrapJP2Container bool
	// NativePixelDataUnderEncapsulated reads Pixel Data that is a
	// defined-length value of exactly the native size (Rows x Columns x
	// Samples x Bits Allocated x Frames) as native pixels although the
	// transfer syntax declares a compressed, encapsulated one, as GDCM does;
	// DiagnosticNativePixelDataUnderEncapsulated reports it. The decompressed
	// data set carries the native target syntax.
	NativePixelDataUnderEncapsulated bool
	// DefinedLengthFragments reads Pixel Data that an encapsulated transfer
	// syntax requires with an undefined length but that was written with a
	// defined one, holding the Items themselves: when the value is exactly a
	// Basic Offset Table Item and at least one fragment Item (each with a
	// defined length inside the value), optionally ended by a Sequence
	// Delimitation Item, the Items are read as the fragment sequence, as
	// GDCM does; DiagnosticDefinedLengthFragments reports it. Values with
	// bytes left over, truncated Items or more than 100000 Items are not
	// changed.
	DefinedLengthFragments bool
	// RepairBitCountMasks replaces Bits Allocated and Bits Stored written as
	// a bit mask (2^n-1 above 16, such as 0xFFFF for 16) by the count n, and
	// a High Bit written as 2^n-2 (0xFFFE) by n-1, as some FUJIFILM CR
	// devices write them and GDCM repairs them; only when the counts agree
	// with each other and with the size of native Pixel Data. Reported as
	// DiagnosticBitCountMasks with one DecompressReport.Reconciliations entry
	// per replaced attribute. Under the same conditions it replaces a High
	// Bit of 0 with Bits Stored above 1, which places no Bits Stored field in
	// the container, by Bits Stored minus one, as the MFSPLIT export of a
	// Siemens MAGNETOM Skyra writes it and GDCM repairs it; reported as
	// DiagnosticHighBitZero with a HighBit reconciliation.
	RepairBitCountMasks bool
}

// DecompressReport receives value-free decode diagnostics.
type DecompressReport struct {
	// Incomplete is set only after successful complete-prefix normalization.
	Incomplete  *IncompleteFrameRecovery
	Diagnostics []string
	// Reconciliations lists the attributes DecodeRecovery.ReconcileWithCodestream
	// replaced.
	Reconciliations []MetadataReconciliation
}

// DecodeContextWithRecovery returns ctx carrying r for codecs that frame
// encapsulated Pixel Data (see pixeldata/encapsulated.WithRecovery).
func DecodeContextWithRecovery(ctx context.Context, r DecodeRecovery) context.Context {
	return decoderecovery.With(ctx, decoderecovery.Options{
		IgnoreInconsistentBasicOffsetTable: r.IgnoreInconsistentBasicOffsetTable,
		JPEGLosslessEncoderQuirks:          r.JPEGLosslessEncoderQuirks,
		JPEGLosslessPredictor6Overflow:     r.JPEGLosslessPredictor6Overflow,
		RLESegmentOverrun:                  r.RLESegmentOverrun,
		RLEReversedByteSegments:            r.RLEReversedByteSegments,
		JPEGSequentialScanQuirks:           r.JPEGSequentialScanQuirks,
		JPEGLSJAIThresholds:                r.JPEGLSJAIThresholds,
		JPEGLSHPColorTransforms:            r.JPEGLSHPColorTransforms,
	})
}

// DecompressLimits bounds still-image decompression. Zero-valued fields use
// the finite values returned by DefaultDecompressLimits.
type DecompressLimits struct {
	// MaxFrames bounds Number of Frames.
	MaxFrames int
	// MaxPixels bounds Rows * Columns * Number of Frames, excluding samples.
	MaxPixels int64
	// MaxNativeBytes bounds the predicted decoded Pixel Data payload.
	MaxNativeBytes int64
	// MaxInputBytes bounds native bytes or the encapsulated BOT plus fragments.
	MaxInputBytes int64
	// MaxExpansionRatio bounds ceil(predicted native bytes / input bytes) for
	// compressed encapsulated inputs.
	MaxExpansionRatio int64
}

// DefaultDecompressLimits returns the finite limits used by zero-value options.
func DefaultDecompressLimits() DecompressLimits {
	return DecompressLimits{
		MaxFrames:         defaultDecompressMaxFrames,
		MaxPixels:         defaultDecompressMaxPixels,
		MaxNativeBytes:    defaultDecompressMaxNativeBytes,
		MaxInputBytes:     defaultDecompressMaxInputBytes,
		MaxExpansionRatio: defaultDecompressExpansionRatio,
	}
}

// DecompressResourceLimitError describes a bounded decompression rejection.
// Limit is one of the fixed DecompressLimits field names and contains no DICOM
// metadata, path, or patient information.
type DecompressResourceLimitError struct {
	Limit   string
	Actual  int64
	Maximum int64
}

func (e *DecompressResourceLimitError) Error() string {
	if e == nil {
		return ErrDecompressResourceLimit.Error()
	}
	return fmt.Sprintf("%s: %s actual=%d maximum=%d", ErrDecompressResourceLimit, e.Limit, e.Actual, e.Maximum)
}

func (e *DecompressResourceLimitError) Unwrap() error {
	return ErrDecompressResourceLimit
}

// ContextCodec is an optional extension implemented by codecs that can honor
// cancellation while decoding. Existing Codec implementations remain valid;
// context-aware decompression prefers this interface when it is available.
type ContextCodec interface {
	// DecodeContext has the same borrowed-input and owned-output contract as
	// Codec.Decode and additionally observes ctx cancellation.
	DecodeContext(context.Context, PixelData, *object.Object) (Frames, error)
}

// DecompressFile returns a new File whose Pixel Data is native uncompressed
// bytes. Supported still-image compressed syntaxes are decoded through the
// supplied registry; native and Encapsulated Uncompressed inputs are normalized
// without a pixel codec. Compression/recompression is intentionally out of
// scope.
func DecompressFile(file *object.File, opts DecompressOptions) (*object.File, error) {
	return DecompressFileContext(context.Background(), file, opts)
}

// DecompressFileContext is DecompressFile with cancellation support.
func DecompressFileContext(ctx context.Context, file *object.File, opts DecompressOptions) (*object.File, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if file == nil {
		return nil, fmt.Errorf("%w: file is nil", ErrMissingMetadata)
	}
	if partial, present := file.PartialRead(); present {
		info, certified := file.IncompletePixelData()
		if !opts.Recovery.IncompleteData || !certified || partial.Tag != core.TagPixelData || partial.Offset != info.FailureOffset || !errors.Is(partial.Err, object.ErrIncompletePixelData) {
			return nil, object.ErrPartialFile
		}
	}
	source, err := sourceTransferSyntax(file)
	if err != nil {
		return nil, err
	}
	dataset, target, err := DecompressDataSetContext(ctx, file.Dataset, source, opts)
	if err != nil {
		return nil, err
	}
	return &object.File{
		Preamble:       core.CloneBytes(file.Preamble),
		Meta:           fileMetaWithTransferSyntax(file.Meta, target),
		Dataset:        dataset,
		TransferSyntax: target,
	}, nil
}

// DecompressDataSet returns a new dataset and native uncompressed target
// transfer syntax. The input source syntax describes how Pixel Data should be
// decoded; the returned syntax defaults to Explicit VR Little Endian.
func DecompressDataSet(dataset *object.Object, source transfer.Syntax, opts DecompressOptions) (*object.Object, transfer.Syntax, error) {
	return DecompressDataSetContext(context.Background(), dataset, source, opts)
}

// DecompressDataSetContext is DecompressDataSet with cancellation support.
func DecompressDataSetContext(ctx context.Context, dataset *object.Object, source transfer.Syntax, opts DecompressOptions) (*object.Object, transfer.Syntax, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, transfer.Syntax{}, err
	}
	if dataset == nil {
		return nil, transfer.Syntax{}, fmt.Errorf("%w: dataset is nil", ErrMissingMetadata)
	}
	info, incomplete := dataset.IncompletePixelData()
	if incomplete && (!opts.Recovery.IncompleteData || incompatibleIncompleteRecovery(opts.Recovery)) {
		return nil, transfer.Syntax{}, object.ErrPartialFile
	}
	limits, err := normalizedDecompressLimits(opts.Limits)
	if err != nil {
		return nil, transfer.Syntax{}, err
	}
	source, err = canonicalSourceTransferSyntax(source)
	if err != nil {
		return nil, transfer.Syntax{}, err
	}
	if !source.Supported && !source.RequiresCodec() {
		return nil, transfer.Syntax{}, fmt.Errorf("%w: source transfer syntax %q is not supported", ErrUnsupportedDecompression, source.UID)
	}
	target, err := decompressionTargetTransferSyntax(opts.TargetTransferSyntax)
	if err != nil {
		return nil, transfer.Syntax{}, err
	}

	var defaulted []string
	var reconciled []MetadataReconciliation
	if opts.Recovery.RepairBitCountMasks {
		var masks []MetadataReconciliation
		var repairs []string
		dataset, masks, repairs = withBitCountMaskRepair(dataset, source)
		defaulted = append(defaulted, repairs...)
		reconciled = append(reconciled, masks...)
	}
	if opts.Recovery.DefinedLengthFragments {
		var split bool
		if dataset, split = definedLengthFragments(dataset, source); split {
			defaulted = append(defaulted, DiagnosticDefinedLengthFragments)
		}
	}
	dataset, legacy, err := withLegacyImageRecovery(dataset, source, opts.Recovery)
	if err != nil {
		return nil, transfer.Syntax{}, err
	}
	defaulted = append(defaulted, legacy...)
	if opts.Recovery.RepairPhotometricInterpretation {
		var repairs []string
		dataset, repairs, err = withPhotometricRecovery(dataset, source, limits)
		if err != nil {
			return nil, transfer.Syntax{}, err
		}
		defaulted = append(defaulted, repairs...)
	}
	if opts.Recovery.UnwrapJP2Container {
		var unwrapped bool
		if dataset, unwrapped = unwrapJP2(dataset, source); unwrapped {
			defaulted = append(defaulted, DiagnosticJP2ContainerUnwrapped)
		}
	}
	if opts.Recovery.NativePixelDataUnderEncapsulated {
		var native bool
		if source, native = nativeUnderEncapsulated(dataset, source); native {
			defaulted = append(defaulted, DiagnosticNativePixelDataUnderEncapsulated)
		}
	}
	if opts.Recovery.ReconcileWithCodestream {
		var fromCodestream []MetadataReconciliation
		dataset, fromCodestream = reconcileWithCodestream(dataset, source)
		reconciled = append(reconciled, fromCodestream...)
		if len(fromCodestream) > 0 {
			defaulted = append(defaulted, DiagnosticReconciledWithCodestream)
		}
	}
	pixel, err := ExtractView(dataset)
	if err != nil {
		return nil, transfer.Syntax{}, err
	}
	lossy := DetectDataSetLossyState(dataset, source)
	inputBytes, ok := decompressionInputBytes(pixel)
	if !ok {
		return nil, transfer.Syntax{}, decompressLimitError("MaxInputBytes", math.MaxInt64, limits.MaxInputBytes)
	}
	metadata, err := ExtractMetadata(dataset)
	if err != nil {
		if inputBytes > limits.MaxInputBytes {
			return nil, transfer.Syntax{}, decompressLimitError("MaxInputBytes", inputBytes, limits.MaxInputBytes)
		}
		return nil, transfer.Syntax{}, err
	}
	if err := checkDecodeAdmission(limits, metadata, source, inputBytes); err != nil {
		return nil, transfer.Syntax{}, err
	}
	var pixelRepairs []string
	dataset, metadata, pixelRepairs, err = withLegacyPixelRepairs(ctx, dataset, source, metadata, opts.Recovery)
	if err != nil {
		return nil, transfer.Syntax{}, err
	}
	defaulted = append(defaulted, pixelRepairs...)
	if requiresLegacyPixelRepair(opts.Recovery) {
		pixel, err = ExtractView(dataset)
		if err != nil {
			return nil, transfer.Syntax{}, err
		}
	}
	var incompleteReport *IncompleteFrameRecovery
	if incomplete {
		dataset, incompleteReport, err = normalizeIncompletePixelData(ctx, dataset, source, pixel, metadata, info, limits)
		if err != nil {
			return nil, transfer.Syntax{}, err
		}
		metadata, err = ExtractMetadata(dataset)
		if err != nil {
			return nil, transfer.Syntax{}, err
		}
		pixel, err = ExtractView(dataset)
		if err != nil {
			return nil, transfer.Syntax{}, err
		}
		defaulted = append(defaulted, DiagnosticIncompletePixelData)
	}
	if opts.Recovery != (DecodeRecovery{}) {
		ctx = DecodeContextWithRecovery(ctx, opts.Recovery)
	}
	if opts.JPEGColorConversion != JPEGColorGuess {
		ctx = WithJPEGColorConversion(ctx, opts.JPEGColorConversion)
	}
	frames, err := decodeForDecompressionContext(ctx, source, pixel, dataset, opts.Registry)
	if err != nil {
		return nil, transfer.Syntax{}, err
	}
	diagnostics := frames.Diagnostics
	lossy = withDecodedLossy(lossy, frames)
	outputMetadata := decompressedOutputMetadata(metadata, source, frames)
	frames, err = littleEndianOutputFramesContext(ctx, outputMetadata, source, frames)
	if err != nil {
		return nil, transfer.Syntax{}, err
	}
	if opts.Recovery.ConvertRetiredColorModels {
		var converted string
		frames, outputMetadata, converted, err = retiredColorFrames(ctx, dataset, source, frames, outputMetadata)
		if err != nil {
			return nil, transfer.Syntax{}, err
		}
		if converted != "" {
			defaulted = append(defaulted, converted)
		}
	}
	raw, err := nativeFrameBytesContext(ctx, outputMetadata, frames)
	if err != nil {
		return nil, transfer.Syntax{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, transfer.Syntax{}, err
	}

	output, err := datasetWithNativePixelData(dataset, outputMetadata, raw, source)
	if err != nil {
		return nil, transfer.Syntax{}, err
	}
	if metadata.PhotometricInterpretation != outputMetadata.PhotometricInterpretation && opts.Recovery.ConvertRetiredColorModels && isRetiredColor(metadata.PhotometricInterpretation) {
		removeRetiredColorAttributes(output)
	}
	stampLossyState(output, lossy, int64(len(raw)), inputBytes)
	if opts.Report != nil {
		opts.Report.Diagnostics = append(append(opts.Report.Diagnostics[:0], defaulted...), diagnostics...)
		opts.Report.Reconciliations = append(opts.Report.Reconciliations[:0], reconciled...)
		opts.Report.Incomplete = incompleteReport
	}
	return output, target, nil
}

func normalizedDecompressLimits(limits DecompressLimits) (DecompressLimits, error) {
	if limits.MaxFrames < 0 || limits.MaxPixels < 0 || limits.MaxNativeBytes < 0 || limits.MaxInputBytes < 0 || limits.MaxExpansionRatio < 0 {
		return DecompressLimits{}, ErrInvalidDecompressLimits
	}
	defaults := DefaultDecompressLimits()
	if limits.MaxFrames == 0 {
		limits.MaxFrames = defaults.MaxFrames
	}
	if limits.MaxPixels == 0 {
		limits.MaxPixels = defaults.MaxPixels
	}
	if limits.MaxNativeBytes == 0 {
		limits.MaxNativeBytes = defaults.MaxNativeBytes
	}
	if limits.MaxInputBytes == 0 {
		limits.MaxInputBytes = defaults.MaxInputBytes
	}
	if limits.MaxExpansionRatio == 0 {
		limits.MaxExpansionRatio = defaults.MaxExpansionRatio
	}
	return limits, nil
}

// checkDecodeAdmission applies limits to a source of metadata whose Pixel
// Data holds inputBytes (native bytes, or the Basic Offset Table plus the
// fragments), before anything is decoded: the input bytes, Number of Frames,
// Rows * Columns * Number of Frames, the predicted decoded bytes and, for a
// compressed encapsulated source, their expansion ratio. Whole-object
// decompression, the streaming transcode, FrameReader's per-frame admission
// and CheckTranscodeDecodeLimits all admit through it, so a preflight refuses
// exactly what decoding would.
func checkDecodeAdmission(limits DecompressLimits, metadata Metadata, source transfer.Syntax, inputBytes int64) error {
	if inputBytes < 0 {
		return decompressLimitError("MaxInputBytes", math.MaxInt64, limits.MaxInputBytes)
	}
	if inputBytes > limits.MaxInputBytes {
		return decompressLimitError("MaxInputBytes", inputBytes, limits.MaxInputBytes)
	}
	if metadata.Rows == 0 || metadata.Columns == 0 || metadata.SamplesPerPixel == 0 || metadata.BitsAllocated == 0 || metadata.NumberOfFrames <= 0 {
		return fmt.Errorf("%w: rows=%d columns=%d samples_per_pixel=%d bits_allocated=%d number_of_frames=%d",
			ErrInvalidMetadata, metadata.Rows, metadata.Columns, metadata.SamplesPerPixel, metadata.BitsAllocated, metadata.NumberOfFrames)
	}
	if metadata.NumberOfFrames > limits.MaxFrames {
		return decompressLimitError("MaxFrames", int64(metadata.NumberOfFrames), int64(limits.MaxFrames))
	}
	pixels, ok := checkedDecompressProduct(int64(metadata.Rows), int64(metadata.Columns), int64(metadata.NumberOfFrames))
	if !ok {
		return decompressLimitError("MaxPixels", math.MaxInt64, limits.MaxPixels)
	}
	if pixels > limits.MaxPixels {
		return decompressLimitError("MaxPixels", pixels, limits.MaxPixels)
	}
	nativeBytes, ok := estimatedDecompressNativeBytes(metadata, source)
	if !ok || nativeBytes <= 0 {
		return decompressLimitError("MaxNativeBytes", math.MaxInt64, limits.MaxNativeBytes)
	}
	if nativeBytes > limits.MaxNativeBytes {
		return decompressLimitError("MaxNativeBytes", nativeBytes, limits.MaxNativeBytes)
	}
	if source.Encapsulated && source.UID != transfer.EncapsulatedUncompressedExplicitVRLittleEndian.UID {
		if ratio := decompressionExpansionRatio(nativeBytes, inputBytes); ratio > limits.MaxExpansionRatio {
			return decompressLimitError("MaxExpansionRatio", ratio, limits.MaxExpansionRatio)
		}
	}
	return nil
}

func decompressLimitError(limit string, actual, maximum int64) error {
	return &DecompressResourceLimitError{Limit: limit, Actual: actual, Maximum: maximum}
}

func checkedDecompressProduct(values ...int64) (int64, bool) {
	product := int64(1)
	for _, value := range values {
		if value <= 0 || product > math.MaxInt64/value {
			return 0, false
		}
		product *= value
	}
	return product, true
}

func estimatedDecompressNativeBytes(metadata Metadata, source transfer.Syntax) (int64, bool) {
	if metadata.BitsAllocated == 1 && !source.Encapsulated {
		return nativePackedByteCount(metadata)
	}
	nativeBytes := metadata.TotalSize()
	if nativeBytes <= 0 {
		return 0, false
	}
	if !isSubsampled422(metadata.PhotometricInterpretation) {
		return nativeBytes, true
	}
	switch uid := transfer.NormalizeUID(source.UID); {
	case uid == transfer.JPEGBaseline.UID, uid == transfer.JPEGExtended.UID:
	case transfer.IsJPEG2000TransferSyntax(uid):
		// A 4:2:2 JPEG 2000 codestream decodes to full-resolution RGB.
	default:
		return nativeBytes, true
	}
	if metadata.BitsAllocated < 8 {
		return nativeBytes, true
	}
	expandedBytes, ok := checkedDecompressProduct(
		int64(metadata.Rows),
		int64(metadata.Columns),
		int64(metadata.SamplesPerPixel),
		int64(metadata.BytesPerSample()),
		int64(metadata.NumberOfFrames),
	)
	if !ok {
		return 0, false
	}
	if expandedBytes > nativeBytes {
		return expandedBytes, true
	}
	return nativeBytes, true
}

func decompressionInputBytes(pixel PixelData) (int64, bool) {
	if !pixel.Encapsulated {
		return int64(len(pixel.Raw)), true
	}
	total := int64(len(pixel.Sequence.OffsetTable))
	for _, fragment := range pixel.Sequence.Fragments {
		length := int64(len(fragment))
		if total > math.MaxInt64-length {
			return 0, false
		}
		total += length
	}
	return total, true
}

func decompressionExpansionRatio(nativeBytes, inputBytes int64) int64 {
	if nativeBytes <= 0 {
		return 0
	}
	if inputBytes <= 0 {
		return math.MaxInt64
	}
	ratio := nativeBytes / inputBytes
	if nativeBytes%inputBytes != 0 {
		ratio++
	}
	return ratio
}

func sourceTransferSyntax(file *object.File) (transfer.Syntax, error) {
	if file == nil {
		return transfer.Syntax{}, fmt.Errorf("%w: file is nil", ErrMissingMetadata)
	}
	fileUID := transfer.NormalizeUID(file.TransferSyntax.UID)
	var metaUID string
	if file.Meta != nil {
		if uid, ok := file.Meta.GetString(tagFileMetaTransferSyntaxUID); ok {
			metaUID = transfer.NormalizeUID(uid)
		}
	}
	// A recovery reader records its decision; a File whose syntax is the
	// recorded one may disagree with the File Meta value it recovered from.
	resolution, recovered := file.TransferSyntaxResolution()
	recovered = recovered && transfer.NormalizeUID(resolution.Syntax.UID) == fileUID
	if fileUID != "" && metaUID != "" && fileUID != metaUID && !recovered {
		return transfer.Syntax{}, fmt.Errorf("%w: file transfer syntax does not match File Meta Information", ErrIncompatiblePixelData)
	}
	if fileUID != "" {
		return canonicalSourceTransferSyntax(file.TransferSyntax)
	}
	if metaUID != "" {
		return canonicalSourceTransferSyntax(transfer.Syntax{UID: metaUID})
	}
	return transfer.Syntax{}, fmt.Errorf("%w: source transfer syntax is empty", ErrIncompatiblePixelData)
}

func canonicalSourceTransferSyntax(syntax transfer.Syntax) (transfer.Syntax, error) {
	uid := transfer.NormalizeUID(syntax.UID)
	if uid == "" {
		return transfer.Syntax{}, fmt.Errorf("%w: source transfer syntax is empty", ErrIncompatiblePixelData)
	}
	if got, ok := transfer.DefaultRegistry.Get(uid); ok {
		return got, nil
	}
	return transfer.Syntax{}, fmt.Errorf("%w: %q", transfer.ErrUnknownTransferSyntax, uid)
}

func decompressionTargetTransferSyntax(syntax transfer.Syntax) (transfer.Syntax, error) {
	uid := transfer.NormalizeUID(syntax.UID)
	if uid == "" {
		return transfer.ExplicitVRLittleEndian, nil
	}
	target, ok := transfer.DefaultRegistry.Get(uid)
	if !ok {
		return transfer.Syntax{}, fmt.Errorf("%w: %q", transfer.ErrUnknownTransferSyntax, uid)
	}
	switch target.UID {
	case transfer.ExplicitVRLittleEndian.UID, transfer.ImplicitVRLittleEndian.UID:
		return target, nil
	default:
		return transfer.Syntax{}, fmt.Errorf("%w: target transfer syntax %q is not a supported native little-endian syntax", ErrUnsupportedDecompression, target.UID)
	}
}

func decodeForDecompressionContext(ctx context.Context, source transfer.Syntax, pixel PixelData, dataset *object.Object, registry Registry) (Frames, error) {
	if err := ctx.Err(); err != nil {
		return Frames{}, err
	}
	switch {
	case source.Encapsulated && !pixel.Encapsulated:
		return Frames{}, fmt.Errorf("%w: transfer syntax %q expects encapsulated pixel data", ErrIncompatiblePixelData, source.UID)
	case !source.Encapsulated && pixel.Encapsulated:
		return Frames{}, fmt.Errorf("%w: transfer syntax %q expects native pixel data", ErrIncompatiblePixelData, source.UID)
	}
	if registry == nil {
		if !source.Encapsulated || source.UID == transfer.EncapsulatedUncompressedExplicitVRLittleEndian.UID {
			registry = NewMemoryRegistry()
		} else {
			registry = DefaultRegistry
		}
	}
	if registry == nil {
		return Frames{}, codecAvailabilityError(ErrCodecRegistryNil, source.UID, nil)
	}
	return decodeFramesWithRegistry(ctx, registry, source.UID, pixel, dataset)
}

func nativeFrameBytesContext(ctx context.Context, metadata Metadata, frames Frames) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if frames.Rows != int(metadata.Rows) || frames.Columns != int(metadata.Columns) {
		return nil, fmt.Errorf(
			"%w: decoded frame geometry=%dx%d metadata=%dx%d",
			ErrPixelDataSizeMismatch,
			frames.Rows,
			frames.Columns,
			metadata.Rows,
			metadata.Columns,
		)
	}
	if len(frames.Data) != metadata.NumberOfFrames {
		return nil, fmt.Errorf(
			"%w: decoded frame count=%d metadata NumberOfFrames=%d",
			ErrPixelDataSizeMismatch,
			len(frames.Data),
			metadata.NumberOfFrames,
		)
	}

	frameSize := metadata.FrameSize()
	totalSize := metadata.TotalSize()
	var frameBits int64
	if metadata.BitsAllocated == 1 {
		frameBits = packedFrameBits(metadata)
		totalSize, _ = nativePackedByteCount(metadata)
	}
	maxInt := int64(^uint(0) >> 1)
	if frameSize <= 0 || totalSize <= 0 || frameSize > maxInt || totalSize > maxInt {
		return nil, fmt.Errorf(
			"%w: rows=%d columns=%d samples_per_pixel=%d bits_allocated=%d number_of_frames=%d",
			ErrInvalidMetadata,
			metadata.Rows,
			metadata.Columns,
			metadata.SamplesPerPixel,
			metadata.BitsAllocated,
			metadata.NumberOfFrames,
		)
	}

	raw := make([]byte, int(totalSize))
	for i, frame := range frames.Data {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(frame) != int(frameSize) {
			return nil, fmt.Errorf(
				"%w: decoded frame %d has %d bytes, want %d",
				ErrPixelDataSizeMismatch,
				i,
				len(frame),
				frameSize,
			)
		}
		if frameBits%8 != 0 {
			for offset := int64(0); offset < frameBits; {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				count := min(frameBits-offset, 64<<10)
				copyPackedBits(raw, int64(i)*frameBits+offset, frame, offset, count)
				offset += count
			}
		} else {
			copy(raw[int64(i)*frameSize:], frame)
		}
	}
	return raw, nil
}

func datasetWithNativePixelData(dataset *object.Object, metadata Metadata, raw []byte, source transfer.Syntax) (*object.Object, error) {
	elements := dataset.Elements()
	if source.ByteOrder == binary.BigEndian {
		var err error
		elements, err = transcodeElementsToLittleEndian(elements, binary.BigEndian)
		if err != nil {
			return nil, err
		}
	}
	if source.Encapsulated {
		elements = removeElements(elements,
			tagExtendedOffsetTable,
			tagExtendedOffsetTableLengths,
			tagEncapsulatedPixelDataValueTotalLength,
		)
	}
	replacements := []core.Element{
		core.NewRawElement(core.TagPixelData, nativePixelDataVR(metadata), raw),
		photometricInterpretationElement(metadata.PhotometricInterpretation),
		uint16RawElement(tagSamplesPerPixel, metadata.SamplesPerPixel),
	}
	if metadata.PlanarConfigurationPresent {
		replacements = append(replacements, uint16RawElement(tagPlanarConfiguration, metadata.PlanarConfiguration))
	}
	for _, replacement := range replacements {
		elements = replaceElement(elements, replacement)
	}
	return object.FromElements(elements, nil), nil
}

func removeElements(elements []core.Element, tags ...core.Tag) []core.Element {
	removed := make(map[core.Tag]struct{}, len(tags))
	for _, tag := range tags {
		removed[tag] = struct{}{}
	}
	out := make([]core.Element, 0, len(elements))
	for _, element := range elements {
		if _, ok := removed[element.Tag()]; ok {
			continue
		}
		out = append(out, element)
	}
	return out
}

func nativePixelDataVR(metadata Metadata) core.VR {
	if metadata.BitsAllocated > 8 {
		return core.VROW
	}
	return core.VROB
}

func littleEndianOutputFramesContext(ctx context.Context, metadata Metadata, source transfer.Syntax, frames Frames) (Frames, error) {
	if err := ctx.Err(); err != nil {
		return Frames{}, err
	}
	if source.ByteOrder != binary.BigEndian || metadata.BytesPerSample() <= 1 {
		return frames, nil
	}
	sampleBytes := metadata.BytesPerSample()
	out := Frames{
		Rows:    frames.Rows,
		Columns: frames.Columns,
		Data:    make([][]byte, len(frames.Data)),
	}
	for i, frame := range frames.Data {
		if err := ctx.Err(); err != nil {
			return Frames{}, err
		}
		if len(frame)%sampleBytes != 0 {
			return Frames{}, fmt.Errorf(
				"%w: decoded frame %d has %d bytes, not a multiple of %d-byte samples",
				ErrPixelDataSizeMismatch,
				i,
				len(frame),
				sampleBytes,
			)
		}
		out.Data[i] = reverseFixedWidthValues(frame, sampleBytes)
	}
	return out, nil
}

func decompressedOutputMetadata(metadata Metadata, source transfer.Syntax, frames Frames) Metadata {
	if metadata.SamplesPerPixel != 3 {
		return metadata
	}
	if frames.PhotometricInterpretation != "" {
		// The codec reported the color space of its interleaved output.
		metadata.PhotometricInterpretation = frames.PhotometricInterpretation
		metadata.PlanarConfiguration = 0
		metadata.PlanarConfigurationPresent = true
		return metadata
	}
	switch transfer.NormalizeUID(source.UID) {
	case transfer.RLELossless.UID:
		// RLE decodes color-by-plane segments to interleaved samples.
		metadata.PlanarConfiguration = 0
		metadata.PlanarConfigurationPresent = true
	case transfer.JPEGBaseline.UID, transfer.JPEGExtended.UID:
		switch normalizedPhotometric(metadata.PhotometricInterpretation) {
		case "YBR_FULL":
			metadata.PhotometricInterpretation = "RGB"
			metadata.PlanarConfiguration = 0
			metadata.PlanarConfigurationPresent = true
		case "YBR_FULL_422", "YBR_PARTIAL_422":
			if decodedFramesUseExpandedRGB(metadata, frames) {
				metadata.PhotometricInterpretation = "RGB"
				metadata.PlanarConfiguration = 0
				metadata.PlanarConfigurationPresent = true
			}
		}
	}
	return metadata
}

func decodedFramesUseExpandedRGB(metadata Metadata, frames Frames) bool {
	frameSize, ok := expandedRGBFrameSize(metadata)
	if !ok || len(frames.Data) == 0 {
		return false
	}
	if frameSize > int64(^uint(0)>>1) {
		return false
	}
	for _, frame := range frames.Data {
		if len(frame) != int(frameSize) {
			return false
		}
	}
	return true
}

func expandedRGBFrameSize(metadata Metadata) (int64, bool) {
	if metadata.BitsAllocated < 8 {
		return 0, false
	}
	return int64(metadata.Rows) * int64(metadata.Columns) * 3 * int64(metadata.BytesPerSample()), true
}

func photometricInterpretationElement(value string) core.Element {
	return core.Element{
		Header: core.ElementHeader{Tag: tagPhotometricInterpretation, VR: core.VRCS},
		Value:  core.StringValue{value},
	}
}

func uint16RawElement(tag core.Tag, value uint16) core.Element {
	var raw [2]byte
	binary.LittleEndian.PutUint16(raw[:], value)
	return core.NewRawElement(tag, core.VRUS, raw[:])
}

func normalizedPhotometric(value string) string {
	return strings.Trim(strings.ToUpper(value), " \x00")
}

// Byte-order rules for converting native values between Little and Big Endian:
//
//   - PS3.5 7.3 reverses every binary VR at its value width: AT/OW/SS/US 2,
//     FL/OF/OL/SL/UL 4, FD/OD/OV/SV/UV 8. OB, UN and text are byte streams.
//     Float and Double Float Pixel Data therefore follow OF and OD.
//   - PS3.5 defines OW Pixel Data (7FE0,0010) byte order for 16-bit words and
//     is silent for wider samples under the retired Explicit VR Big Endian
//     syntax. Independent readers such as pydicom order each sample of Bits
//     Allocated 32 or 64 as one big-endian integer, so native OW Pixel Data
//     with those widths reverses at the sample width. Word swapping would
//     transpose the halves of every sample (1249000 would become 250085395).
//   - The sample width comes from Bits Allocated in the same dataset or item.
//     A nested Icon Image Sequence item never inherits its parent's width.
//   - OB Pixel Data and OW Pixel Data with Bits Allocated 16 or less keep the
//     VR rule. A value whose length is not a multiple of its width is refused
//     instead of leaving a partially converted tail.

// transcodeElementsToLittleEndian reverses the byte order of every fixed-width
// binary value. source is the byte order of the input values; the conversion
// is symmetric, and source is needed only to read Bits Allocated.
func transcodeElementsToLittleEndian(elements []core.Element, source binary.ByteOrder) ([]core.Element, error) {
	// dataset.Elements() already returned an owned top-level slice. Update that
	// slice in place and let nested sequences use copy-on-write so the source
	// object remains immutable without cloning unaffected sequence trees.
	pixelWidth, err := pixelDataSampleSwapWidth(elements, source)
	if err != nil {
		return nil, err
	}
	for i := range elements {
		value, changed, err := transcodeValueToLittleEndian(elements[i], pixelWidth, source)
		if err != nil {
			return nil, err
		}
		if changed {
			elements[i].Value = value
		}
	}
	return elements, nil
}

func transcodeValueToLittleEndian(element core.Element, pixelWidth int, source binary.ByteOrder) (core.Value, bool, error) {
	switch v := element.Value.(type) {
	case core.RawValue:
		vr := element.VR()
		width := byteOrderValueWidth(vr)
		if element.Tag() == core.TagPixelData && vr == core.VROW && pixelWidth > width {
			width = pixelWidth
		}
		if width <= 1 {
			return element.Value, false, nil
		}
		if len(v)%width != 0 {
			return nil, false, fmt.Errorf("%w: %s %s value has %d bytes, not a multiple of %d-byte values", ErrPixelDataSizeMismatch, element.Tag(), vr, len(v), width)
		}
		if len(v) == 0 {
			return element.Value, false, nil
		}
		return core.RawValue(reverseFixedWidthValues(v.Bytes(), width)), true, nil
	case core.SequenceValue:
		var items []core.DataSet
		for i, item := range v.Items {
			elements, changed, err := transcodeElementsToLittleEndianCopy(item.Elements, source)
			if err != nil {
				return nil, false, err
			}
			if !changed {
				continue
			}
			if items == nil {
				items = append([]core.DataSet(nil), v.Items...)
			}
			items[i] = item
			items[i].Elements = elements
		}
		if items == nil {
			return element.Value, false, nil
		}
		return core.SequenceValue{Items: items}, true, nil
	default:
		return element.Value, false, nil
	}
}

func transcodeElementsToLittleEndianCopy(elements []core.Element, source binary.ByteOrder) ([]core.Element, bool, error) {
	pixelWidth, err := pixelDataSampleSwapWidth(elements, source)
	if err != nil {
		return nil, false, err
	}
	var out []core.Element
	for i, element := range elements {
		value, changed, err := transcodeValueToLittleEndian(element, pixelWidth, source)
		if err != nil {
			return nil, false, err
		}
		if !changed {
			continue
		}
		if out == nil {
			out = append([]core.Element(nil), elements...)
		}
		out[i].Value = value
	}
	if out == nil {
		return elements, false, nil
	}
	return out, true, nil
}

// pixelDataSampleSwapWidth returns the sample width at which native OW Pixel
// Data in this element list must be reversed, or 0 to use the VR width. It
// reads Bits Allocated only from the same list.
func pixelDataSampleSwapWidth(elements []core.Element, source binary.ByteOrder) (int, error) {
	for _, element := range elements {
		if element.Tag() != tagBitsAllocated {
			continue
		}
		var bitsAllocated uint16
		switch value := element.Value.(type) {
		case core.Uint16Value:
			if len(value) != 1 {
				return 0, fmt.Errorf("%w: BitsAllocated has %d values", ErrInvalidMetadata, len(value))
			}
			bitsAllocated = value[0]
		case core.RawValue:
			if len(value) != 2 {
				return 0, fmt.Errorf("%w: BitsAllocated has %d bytes", ErrInvalidMetadata, len(value))
			}
			if source == nil {
				source = binary.LittleEndian
			}
			bitsAllocated = source.Uint16(value)
		default:
			return 0, nil
		}
		switch bitsAllocated {
		case 32:
			return 4, nil
		case 64:
			return 8, nil
		default:
			return 0, nil
		}
	}
	return 0, nil
}

func byteOrderValueWidth(vr core.VR) int {
	return valueencode.ByteOrderWidth(vr)
}

func reverseFixedWidthValues(data []byte, width int) []byte {
	return valueencode.ReverseFixedWidth(data, width)
}

func fileMetaWithTransferSyntax(meta *object.Object, syntax transfer.Syntax) *object.Object {
	var elements []core.Element
	if meta != nil {
		elements = append(elements, meta.Elements()...)
	}
	elements = replaceElement(elements, core.Element{
		Header: core.ElementHeader{Tag: tagFileMetaTransferSyntaxUID, VR: core.VRUI},
		Value:  core.StringValue{syntax.UID},
	})
	return object.FromElements(elements, nil)
}

func replaceElement(elements []core.Element, replacement core.Element) []core.Element {
	out := make([]core.Element, 0, len(elements)+1)
	replaced := false
	for _, element := range elements {
		if element.Tag() == replacement.Tag() {
			out = append(out, replacement)
			replaced = true
			continue
		}
		out = append(out, element)
	}
	if !replaced {
		out = append(out, replacement)
	}
	return out
}
