package pixeldata

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	ErrPixelDataNotFound                    = errors.New("dicom: pixel data element not found")
	ErrMissingMetadata                      = errors.New("dicom: missing required pixel data metadata")
	ErrInvalidMetadata                      = errors.New("dicom: invalid pixel data metadata")
	ErrCodecNotFound                        = errors.New("dicom: no pixel data codec registered for transfer syntax")
	ErrCodecRegistryNil                     = errors.New("dicom: pixel data codec registry is nil")
	ErrCodecNil                             = errors.New("dicom: pixel data codec is nil")
	ErrCodecUIDInvalid                      = errors.New("dicom: pixel data codec transfer syntax UID is empty")
	ErrCodecDecodeFailed                    = errors.New("dicom: pixel data codec decode failed")
	ErrIncompatiblePixelData                = errors.New("dicom: pixel data is incompatible with transfer syntax")
	ErrEncapsulatedPixelData                = errors.New("dicom: native frame extraction does not support encapsulated pixel data")
	ErrPixelDataSizeMismatch                = errors.New("dicom: pixel data size does not match metadata")
	ErrUnsupportedPhotometricInterpretation = errors.New("dicom: unsupported PhotometricInterpretation")
	ErrUnsupportedPixelRepresentation       = errors.New("dicom: unsupported PixelRepresentation")
	ErrUnsupportedPlanarConfiguration       = errors.New("dicom: unsupported PlanarConfiguration")
	ErrJPIPRetrievalRequired                = errors.New("dicom: JPIP pixel data retrieval pipeline required")
	ErrMediaPayloadNotRenderable            = errors.New("dicom: media payload is not renderable as still-image frames")
	tagRows                                 = core.NewTag(0x0028, 0x0010)
	tagColumns                              = core.NewTag(0x0028, 0x0011)
	tagSamplesPerPixel                      = core.NewTag(0x0028, 0x0002)
	tagPlanarConfiguration                  = core.NewTag(0x0028, 0x0006)
	tagBitsAllocated                        = core.NewTag(0x0028, 0x0100)
	tagBitsStored                           = core.NewTag(0x0028, 0x0101)
	tagHighBit                              = core.NewTag(0x0028, 0x0102)
	tagPixelRepresentation                  = core.NewTag(0x0028, 0x0103)
	tagNumberOfFrames                       = core.NewTag(0x0028, 0x0008)
	tagPhotometricInterpretation            = core.NewTag(0x0028, 0x0004)
	tagPixelDataProviderURL                 = core.NewTag(0x0028, 0x7FE0)
)

type PixelData struct {
	Raw          []byte
	Sequence     core.FragmentSequence
	Encapsulated bool
}

type Metadata struct {
	Rows                       uint16
	Columns                    uint16
	SamplesPerPixel            uint16
	BitsAllocated              uint16
	BitsStored                 uint16
	HighBit                    uint16
	PixelRepresentation        uint16
	PlanarConfiguration        uint16
	PlanarConfigurationPresent bool
	NumberOfFrames             int
	PhotometricInterpretation  string
}

// JPIPReference describes the external pixel stream referenced by JPIP
// transfer syntaxes. DICOM stores the URL in Pixel Data Provider URL
// (0028,7FE0); callers that want pixels must provide their own JPIP retrieval
// pipeline.
type JPIPReference struct {
	TransferSyntaxUID    string
	TransferSyntaxName   string
	PixelDataProviderURL string
	NumberOfFrames       int
	HTJ2K                bool
	Deflated             bool
}

// JPIPRetrievalRequiredError reports that the object carries a JPIP pixel-data
// reference and cannot be rendered until an external JPIP client retrieves the
// stream.
type JPIPRetrievalRequiredError struct {
	Reference JPIPReference
}

func (e *JPIPRetrievalRequiredError) Error() string {
	if e == nil {
		return ErrJPIPRetrievalRequired.Error()
	}
	var b strings.Builder
	b.WriteString("dicom: JPIP referenced pixel stream requires external streaming retrieval")
	if e.Reference.TransferSyntaxUID != "" {
		fmt.Fprintf(&b, ": transfer syntax %q", e.Reference.TransferSyntaxUID)
		if e.Reference.TransferSyntaxName != "" {
			fmt.Fprintf(&b, " (%s)", e.Reference.TransferSyntaxName)
		}
	}
	if e.Reference.PixelDataProviderURL != "" {
		b.WriteString("; Pixel Data Provider URL present (redacted)")
	}
	return b.String()
}

func (e *JPIPRetrievalRequiredError) Unwrap() error {
	return ErrJPIPRetrievalRequired
}

// MediaPayloadNotRenderableError reports that Pixel Data carries a supported
// encapsulated media payload, such as MPEG/H.264/HEVC video, that this package
// preserves but does not decode into still-image frames.
type MediaPayloadNotRenderableError struct {
	TransferSyntaxUID  string
	TransferSyntaxName string
	MediaKind          string
	FragmentCount      int
}

func (e *MediaPayloadNotRenderableError) Error() string {
	if e == nil {
		return ErrMediaPayloadNotRenderable.Error()
	}
	kind := strings.TrimSpace(e.MediaKind)
	if kind == "" {
		kind = "media"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "dicom: %s media payload is not renderable as still-image frames", kind)
	if e.TransferSyntaxUID != "" {
		fmt.Fprintf(&b, ": transfer syntax %q", e.TransferSyntaxUID)
		if e.TransferSyntaxName != "" {
			fmt.Fprintf(&b, " (%s)", e.TransferSyntaxName)
		}
	}
	if e.FragmentCount > 0 {
		fmt.Fprintf(&b, "; fragments: %d", e.FragmentCount)
	}
	b.WriteString("; preserve encoded Pixel Data or use an external media pipeline")
	return b.String()
}

func (e *MediaPayloadNotRenderableError) Unwrap() error {
	return ErrMediaPayloadNotRenderable
}

func (m Metadata) BytesPerSample() int {
	return int((m.BitsAllocated + 7) / 8)
}

// isSubsampled422 reports the photometric interpretations whose native
// pixel data is 4:2:2 subsampled.
func isSubsampled422(photometric string) bool {
	switch normalizedPhotometric(photometric) {
	case "YBR_FULL_422", "YBR_PARTIAL_422":
		return true
	}
	return false
}

func (m Metadata) FrameSize() int64 {
	// Native 4:2:2 (YBR_FULL_422, retired YBR_PARTIAL_422) stores two
	// samples per pixel.
	if isSubsampled422(m.PhotometricInterpretation) && m.BitsAllocated >= 8 {
		samples := int64(m.Rows) * int64(m.Columns) * 2
		return samples * int64(m.BytesPerSample())
	}
	samples := int64(m.Rows) * int64(m.Columns) * int64(m.SamplesPerPixel)
	if m.BitsAllocated < 8 {
		return (samples*int64(m.BitsAllocated) + 7) / 8
	}
	return samples * int64(m.BytesPerSample())
}

// TotalSize returns the sum of the separately stored frame sizes. For one-bit
// frames, a native Value Field can be smaller because it packs across frame
// boundaries. An invalid frame count or int64 overflow yields zero.
func (m Metadata) TotalSize() int64 {
	frameSize := m.FrameSize()
	frames := int64(m.NumberOfFrames)
	if frameSize <= 0 || frames <= 0 || frameSize > math.MaxInt64/frames {
		return 0
	}
	return frameSize * frames
}

type Codec interface {
	// Decode must treat pixel and obj as borrowed, read-only inputs. A codec must
	// not retain either input (or any buffer reachable from them) after Decode
	// returns. Returned frame buffers must own their storage and must not alias
	// pixel or obj. These rules let callers safely decode an ExtractView result
	// without cloning the compressed payload first.
	Decode(pixel PixelData, obj *object.Object) (Frames, error)
}

type Registry interface {
	RegisterCodec(uid string, codec Codec) error
	GetCodec(uid string) (Codec, bool)
	DecodeFrames(uid string, pixel PixelData, obj *object.Object) (Frames, error)
}

// MemoryRegistry is safe for concurrent registration and lookup. Registrations
// are synchronized per instance, and registering an existing UID replaces its
// codec as it has historically done.
type MemoryRegistry struct {
	mu    sync.RWMutex
	byUID map[string]Codec
}

// CodecAvailabilityError describes a missing or unavailable pixel data codec.
type CodecAvailabilityError struct {
	Err                 error
	TransferSyntaxUID   string
	TransferSyntaxName  string
	RegisteredCodecUIDs []string
	Hint                string
}

func (e *CodecAvailabilityError) Error() string {
	if e == nil {
		return "dicom: pixel data codec unavailable"
	}

	var b strings.Builder
	if e.Err != nil {
		b.WriteString(e.Err.Error())
	} else {
		b.WriteString("dicom: pixel data codec unavailable")
	}
	if e.TransferSyntaxUID != "" {
		fmt.Fprintf(&b, ": transfer syntax %q", e.TransferSyntaxUID)
		if e.TransferSyntaxName != "" {
			fmt.Fprintf(&b, " (%s)", e.TransferSyntaxName)
		}
	}
	if e.Hint != "" {
		fmt.Fprintf(&b, "; %s", e.Hint)
	}
	if errors.Is(e.Err, ErrCodecNotFound) {
		if len(e.RegisteredCodecUIDs) == 0 {
			b.WriteString("; registered codecs: none")
		} else {
			fmt.Fprintf(&b, "; registered codecs: %s", strings.Join(e.RegisteredCodecUIDs, ", "))
		}
	}
	return b.String()
}

func (e *CodecAvailabilityError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// CodecDecodeError wraps a registered codec failure with the transfer syntax
// that selected that codec.
type CodecDecodeError struct {
	Err                error
	TransferSyntaxUID  string
	TransferSyntaxName string
}

func (e *CodecDecodeError) Error() string {
	if e == nil {
		return ErrCodecDecodeFailed.Error()
	}
	var b strings.Builder
	b.WriteString(ErrCodecDecodeFailed.Error())
	if e.TransferSyntaxUID != "" {
		fmt.Fprintf(&b, " for transfer syntax %q", e.TransferSyntaxUID)
		if e.TransferSyntaxName != "" {
			fmt.Fprintf(&b, " (%s)", e.TransferSyntaxName)
		}
	}
	return b.String()
}

func (e *CodecDecodeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *CodecDecodeError) Is(target error) bool {
	return target == ErrCodecDecodeFailed || e != nil && e.Err != nil && errors.Is(e.Err, target)
}

type redactedCodecCause struct {
	cause          error
	classification error
}

func (redactedCodecCause) Error() string { return "dicom: redacted codec failure" }
func (e redactedCodecCause) Is(target error) bool {
	return target == e.classification || e.cause != nil && errors.Is(e.cause, target)
}

type Frames struct {
	Rows    int
	Columns int
	Data    [][]byte
	// PhotometricInterpretation, when set, is the photometric
	// interpretation of the decoded samples where a codec changed it (for
	// example RGB after a JPEG YCbCr conversion); decompression writes it.
	PhotometricInterpretation string
	// Lossy, when set, says the codec's output is not the codestream's
	// samples (for example 4:2:2 chroma upsampled and converted to RGB);
	// decompression then records Lossy Image Compression 01.
	Lossy bool
	// Diagnostics lists tolerated deviations and explicitly enabled legacy
	// recoveries a codec applied while decoding, as fixed value-free codes (for example
	// DiagnosticJPEGBaselineUnderExtended). Each code appears once.
	Diagnostics []string
}

// Codec diagnostic codes reported in Frames.Diagnostics.
const (
	// DiagnosticJPEGBaselineUnderExtended: a Baseline (SOF0) frame under the
	// JPEG Extended transfer syntax, a subset of Process 2 & 4.
	DiagnosticJPEGBaselineUnderExtended = "jpeg-sof0-under-extended"
	// DiagnosticNonZeroFramePad: a frame ends with one nonzero Item pad byte
	// after its end marker (PS3.5 A.4 does not fix the pad value).
	DiagnosticNonZeroFramePad = "encapsulated-nonzero-frame-pad"
	// DiagnosticBasicOffsetTableIgnored: an inconsistent Basic Offset Table
	// was replaced by the marker scan (DecodeRecovery opt-in).
	DiagnosticBasicOffsetTableIgnored = "encapsulated-basic-offset-table-ignored"
	// DiagnosticJPEGLosslessSeIgnored: a JPEG Lossless SOS had a nonzero
	// spectral selection end, ignored (DecodeRecovery.JPEGLosslessEncoderQuirks).
	DiagnosticJPEGLosslessSeIgnored = "jpeg-lossless-se-ignored"
	// DiagnosticJPEGLosslessFillBits: entropy fill bits before a marker were
	// not all ones and were accepted.
	DiagnosticJPEGLosslessFillBits = "jpeg-lossless-invalid-fill-bits"
	// DiagnosticJPEGLosslessCategory16ExtraBits: the encoder wrote 16 bits
	// after each difference of category 16, and they were skipped.
	DiagnosticJPEGLosslessCategory16ExtraBits = "jpeg-lossless-category16-extra-bits"
	// DiagnosticJPEGLosslessPredictor6Overflow: predictor 6 was decoded with
	// the 16-bit overflow of defective encoders and changed at least one
	// sample (DecodeRecovery.JPEGLosslessPredictor6Overflow).
	DiagnosticJPEGLosslessPredictor6Overflow = "jpeg-lossless-predictor6-overflow"
	// DiagnosticJPEGLosslessYBRFull422FullResolution: a JPEG Lossless object
	// labelled YBR_FULL_422 had three full resolution (1x1) components; as
	// PS3.5 8.2.1 lets the codestream control decompression, the samples
	// were decoded unconverted and reported as YBR_FULL.
	DiagnosticJPEGLosslessYBRFull422FullResolution = "jpeg-lossless-ybr-full-422-full-resolution"
	// DiagnosticJPEGScanParametersIgnored: a lossy sequential scan had
	// Ss/Se/Ah/Al other than 0/63/0/0, ignored
	// (DecodeRecovery.JPEGSequentialScanQuirks).
	DiagnosticJPEGScanParametersIgnored = "jpeg-sequential-scan-parameters-ignored"
	// DiagnosticJPEGLSJAIThresholds: a JPEG-LS frame of more than 12 bits
	// without LSE thresholds was decoded with the thresholds of the JAI
	// ImageIO encoder (DecodeRecovery.JPEGLSJAIThresholds).
	DiagnosticJPEGLSJAIThresholds = "jpeg-ls-jai-thresholds"
	// DiagnosticJPEGLSHP1/HP2/HP3 identify applied nonstandard HP transforms
	// explicitly enabled by DecodeRecovery.JPEGLSHPColorTransforms.
	DiagnosticJPEGLSHP1 = "jpeg-ls-hp1-color-transform"
	DiagnosticJPEGLSHP2 = "jpeg-ls-hp2-color-transform"
	DiagnosticJPEGLSHP3 = "jpeg-ls-hp3-color-transform"
	// HP backend diagnostics identify the implementation that reconstructed
	// a nonstandard HP frame. Standard JPEG-LS frames do not emit these codes.
	DiagnosticJPEGLSHPBackendPureGo = "jpeg-ls-hp-backend-pure-go"
	DiagnosticJPEGLSHPBackendCharLS = "jpeg-ls-hp-backend-charls"
	// DiagnosticRLESegmentOverrun: RLE runs overran a segment or the frame
	// and were cut or zero-filled (DecodeRecovery.RLESegmentOverrun).
	DiagnosticRLESegmentOverrun = "rle-segment-overrun"
)

// AddDiagnostic appends code once.
func (f *Frames) AddDiagnostic(code string) {
	for _, c := range f.Diagnostics {
		if c == code {
			return
		}
	}
	f.Diagnostics = append(f.Diagnostics, code)
}

// NativeFrames contains native, uncompressed pixel data split by frame. Frame
// slices share one underlying byte array and should not be mutated independently.
// ExtractNativeFrames owns a defensive copy; ExtractNativeFramesView aliases its
// source object and is read-only.
type NativeFrames struct {
	Metadata Metadata
	Data     [][]byte
}

var DefaultRegistry Registry = NewMemoryRegistry()

func NewMemoryRegistry() *MemoryRegistry {
	return &MemoryRegistry{byUID: make(map[string]Codec)}
}

func (r *MemoryRegistry) RegisterCodec(uid string, codec Codec) error {
	if r == nil {
		return ErrCodecRegistryNil
	}
	if codec == nil {
		return ErrCodecNil
	}

	normalizedUID := transfer.NormalizeUID(uid)
	if normalizedUID == "" {
		return ErrCodecUIDInvalid
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.byUID == nil {
		r.byUID = make(map[string]Codec)
	}
	r.byUID[normalizedUID] = codec
	return nil
}

func (r *MemoryRegistry) GetCodec(uid string) (Codec, bool) {
	if r == nil {
		return nil, false
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	codec, ok := r.byUID[transfer.NormalizeUID(uid)]
	return codec, ok
}

// RegisteredCodecUIDs returns a sorted snapshot of registered codec UIDs.
func (r *MemoryRegistry) RegisteredCodecUIDs() []string {
	if r == nil {
		return nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	uids := make([]string, 0, len(r.byUID))
	for uid := range r.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

func (r *MemoryRegistry) DecodeFrames(uid string, pixel PixelData, obj *object.Object) (Frames, error) {
	return r.DecodeFramesContext(context.Background(), uid, pixel, obj)
}

// IsJPIPReferencedTransferSyntax reports whether uid is one of the DICOM JPIP
// or JPIP HTJ2K referenced pixel-stream transfer syntaxes.
func IsJPIPReferencedTransferSyntax(uid string) bool {
	switch transfer.NormalizeUID(uid) {
	case transfer.JPIPReferenced.UID,
		transfer.JPIPReferencedDeflate.UID,
		transfer.JPIPHTJ2KReferenced.UID,
		transfer.JPIPHTJ2KReferencedDeflate.UID:
		return true
	default:
		return false
	}
}

// ExtractJPIPReference extracts the Pixel Data Provider URL and transfer-syntax
// metadata for a JPIP-referenced object. It does not perform network access.
func ExtractJPIPReference(uid string, obj *object.Object) (JPIPReference, error) {
	normalizedUID := transfer.NormalizeUID(uid)
	if !IsJPIPReferencedTransferSyntax(normalizedUID) {
		return JPIPReference{}, fmt.Errorf("%w: transfer syntax %q is not JPIP referenced", ErrIncompatiblePixelData, normalizedUID)
	}
	if obj == nil {
		return JPIPReference{}, fmt.Errorf("%w: dataset is nil", ErrMissingMetadata)
	}
	urlValue, ok := obj.GetString(tagPixelDataProviderURL)
	url := strings.TrimSpace(urlValue)
	if !ok {
		url = ""
	}
	if url == "" {
		return JPIPReference{}, missingMetadataError("PixelDataProviderURL", tagPixelDataProviderURL)
	}
	frames, err := numberOfFrames(obj)
	if err != nil {
		return JPIPReference{}, err
	}
	ref := JPIPReference{
		TransferSyntaxUID:    normalizedUID,
		PixelDataProviderURL: url,
		NumberOfFrames:       frames,
		HTJ2K:                normalizedUID == transfer.JPIPHTJ2KReferenced.UID || normalizedUID == transfer.JPIPHTJ2KReferencedDeflate.UID,
		Deflated:             normalizedUID == transfer.JPIPReferencedDeflate.UID || normalizedUID == transfer.JPIPHTJ2KReferencedDeflate.UID,
	}
	if syntax, ok := transfer.DefaultRegistry.Get(normalizedUID); ok {
		ref.TransferSyntaxName = syntax.Name
	}
	return ref, nil
}

func RegisterCodec(uid string, codec Codec) error {
	if DefaultRegistry == nil {
		return ErrCodecRegistryNil
	}
	return DefaultRegistry.RegisterCodec(uid, codec)
}

func GetCodec(uid string) (Codec, bool, error) {
	if DefaultRegistry == nil {
		return nil, false, ErrCodecRegistryNil
	}
	codec, ok := DefaultRegistry.GetCodec(uid)
	return codec, ok, nil
}

// CheckCodecAvailability reports whether registry contains a codec for uid.
// It never decodes or otherwise inspects pixel data.
func CheckCodecAvailability(registry Registry, uid string) error {
	if registry == nil {
		return codecAvailabilityError(ErrCodecRegistryNil, transfer.NormalizeUID(uid), nil)
	}
	normalizedUID := transfer.NormalizeUID(uid)
	if _, ok := registry.GetCodec(normalizedUID); ok {
		return nil
	}
	var registered []string
	if snapshot, ok := registry.(interface{ RegisteredCodecUIDs() []string }); ok {
		registered = snapshot.RegisteredCodecUIDs()
	}
	return codecAvailabilityError(ErrCodecNotFound, normalizedUID, registered)
}

func DecodeFrames(uid string, pixel PixelData, obj *object.Object) (Frames, error) {
	return DecodeFramesContext(context.Background(), uid, pixel, obj)
}

func mediaPayloadNotRenderableError(uid string, pixel PixelData) error {
	err := &MediaPayloadNotRenderableError{
		TransferSyntaxUID: uid,
		MediaKind:         "video",
		FragmentCount:     len(pixel.Sequence.Fragments),
	}
	if syntax, ok := transfer.DefaultRegistry.Get(uid); ok {
		err.TransferSyntaxName = syntax.Name
	}
	return err
}

func codecAvailabilityError(cause error, uid string, registered []string) error {
	err := &CodecAvailabilityError{
		Err:                 cause,
		TransferSyntaxUID:   uid,
		RegisteredCodecUIDs: append([]string(nil), registered...),
	}
	if syntax, ok := transfer.DefaultRegistry.Get(uid); ok {
		err.TransferSyntaxName = syntax.Name
		err.Hint = codecRegistrationHint(syntax)
	}
	return err
}

func codecDecodeError(uid string, cause error) error {
	err := &CodecDecodeError{
		Err:               cause,
		TransferSyntaxUID: uid,
	}
	if syntax, ok := transfer.DefaultRegistry.Get(uid); ok {
		err.TransferSyntaxName = syntax.Name
	}
	return err
}

func codecRegistrationHint(syntax transfer.Syntax) string {
	switch syntax.UID {
	case transfer.JPEGBaseline.UID:
		return "register the JPEG Baseline codec with pixeldata/jpeg.RegisterDefault or jpeg.Register(registry)"
	case transfer.JPEGExtended.UID:
		return "register the JPEG Extended Process 2/4 codec with pixeldata/jpeg.RegisterDefault or jpeg.Register(registry)"
	case transfer.JPEGLosslessNonHierarchical.UID,
		transfer.JPEGLosslessSV1.UID:
		return "register the JPEG Lossless codec with pixeldata/jpeglossless.RegisterDefault or jpeglossless.Register(registry)"
	case transfer.RLELossless.UID:
		return "register the RLE Lossless codec with pixeldata/rle.RegisterDefault or rle.Register(registry)"
	case transfer.DeflatedImageFrameCompression.UID:
		return "register the Deflated Image Frame Compression codec with pixeldata/deflateframe.RegisterDefault, deflateframe.Register(registry), or pixeldata/builtin.Register(registry)"
	}
	if syntax.UID == transfer.JPEGLSLossless.UID {
		return "register the built-in pure-Go JPEG-LS Lossless codec with pixeldata/jpegls.RegisterDefault, jpegls.Register(registry), or pixeldata/builtin.Register(registry)"
	}
	if syntax.UID == transfer.JPEGLSNearLossless.UID {
		return "register the qualified unsigned pure-Go JPEG-LS Near-Lossless decoder with pixeldata/jpegls.RegisterNearLossless(registry) or pixeldata/builtin.Register(registry); the optional CharLS adapter remains available from examples/codec-adapters/jpegls"
	}
	if transfer.IsJPEG2000TransferSyntax(syntax.UID) {
		return "JPEG 2000 / HTJ2K transfer syntax is recognized, but dicom-go has no default decoder adapter; metadata and encapsulated Pixel Data can be preserved, or register the optional adapter from examples/codec-adapters/jpeg2000 with jpeg2000.RegisterDefault or jpeg2000.Register(registry)"
	}
	if transfer.IsJPEGXLTransferSyntax(syntax.UID) {
		return "JPEG XL transfer syntax is recognized, but dicom-go has no default decoder adapter; metadata and encapsulated Pixel Data can be preserved, or register a compatible pixeldata.Codec for frame rendering"
	}
	if hint := nonStillImageTransferSyntaxHint(syntax.UID); hint != "" {
		return hint
	}
	if syntax.Encapsulated {
		return "register a compatible pixeldata.Codec with pixeldata.RegisterCodec or registry.RegisterCodec"
	}
	return ""
}

func nonStillImageTransferSyntaxHint(uid string) string {
	switch uid {
	case transfer.MPEG2MPML.UID,
		transfer.MPEG2MPMLF.UID,
		transfer.MPEG2MPHL.UID,
		transfer.MPEG2MPHLF.UID,
		transfer.MPEG4HP41.UID,
		transfer.MPEG4HP41F.UID,
		transfer.MPEG4HP41BD.UID,
		transfer.MPEG4HP41BDF.UID,
		transfer.MPEG4HP422D.UID,
		transfer.MPEG4HP422DF.UID,
		transfer.MPEG4HP423D.UID,
		transfer.MPEG4HP423DF.UID,
		transfer.MPEG4HP42STEREO.UID,
		transfer.MPEG4HP42STEREOF.UID,
		transfer.HEVCMP51.UID,
		transfer.HEVCM10P51.UID:
		return "video transfer syntax is recognized but dicom-go does not decode or play video frames; preserve encoded Pixel Data or use an external media pipeline"
	case transfer.JPIPReferenced.UID,
		transfer.JPIPReferencedDeflate.UID,
		transfer.JPIPHTJ2KReferenced.UID,
		transfer.JPIPHTJ2KReferencedDeflate.UID:
		return "streaming/reference transfer syntax is recognized but dicom-go does not fetch, inflate, or resolve external pixel streams; preserve metadata/reference values or use an external streaming pipeline"
	case transfer.SMPTEST211020UncompressedProgressiveActiveVideo.UID,
		transfer.SMPTEST211020UncompressedInterlacedActiveVideo.UID,
		transfer.SMPTEST211030PCMDigitalAudio.UID:
		return "media transfer syntax is recognized but dicom-go does not decode or play SMPTE ST 2110 video or audio streams; use an external media pipeline"
	}
	return ""
}

func Extract(obj *object.Object) (PixelData, error) {
	elem, ok := obj.Get(core.TagPixelData)
	if !ok {
		return PixelData{}, ErrPixelDataNotFound
	}
	return pixelDataFromElement(elem)
}

// ExtractView returns a zero-copy, read-only view of an object's Pixel Data.
// Raw bytes, the basic offset table, the fragment slice, and every fragment
// alias storage owned by obj. Callers must not mutate either the returned
// buffers or obj while the view is in use. Use Extract when ownership or
// mutation isolation is required.
func ExtractView(obj *object.Object) (PixelData, error) {
	elem, ok := obj.Get(core.TagPixelData)
	if !ok {
		return PixelData{}, ErrPixelDataNotFound
	}
	return pixelDataViewFromElement(elem)
}

func pixelDataFromElement(elem core.Element) (PixelData, error) {
	raw, ok := elem.RawBytes()
	if ok {
		return PixelData{Raw: core.CloneBytes(raw)}, nil
	}
	fragments, ok := elem.Value.(core.FragmentSequence)
	if !ok {
		return PixelData{}, errors.New("dicom: pixel data is not a supported value type")
	}
	cloned := core.FragmentSequence{
		OffsetTable: core.CloneBytes(fragments.OffsetTable),
		Fragments:   make([][]byte, len(fragments.Fragments)),
	}
	for i := range fragments.Fragments {
		cloned.Fragments[i] = core.CloneBytes(fragments.Fragments[i])
	}
	return PixelData{
		Sequence:     cloned,
		Encapsulated: true,
	}, nil
}

func pixelDataViewFromElement(elem core.Element) (PixelData, error) {
	if raw, ok := elem.RawBytes(); ok {
		return PixelData{Raw: raw}, nil
	}
	fragments, ok := elem.Value.(core.FragmentSequence)
	if !ok {
		return PixelData{}, errors.New("dicom: pixel data is not a supported value type")
	}
	return PixelData{Sequence: fragments, Encapsulated: true}, nil
}

func ExtractMetadata(obj *object.Object) (Metadata, error) {
	rows, ok := getUint16(obj, tagRows)
	if !ok {
		return Metadata{}, missingMetadataError("Rows", tagRows)
	}
	columns, ok := getUint16(obj, tagColumns)
	if !ok {
		return Metadata{}, missingMetadataError("Columns", tagColumns)
	}
	samplesPerPixel, ok := getUint16(obj, tagSamplesPerPixel)
	if !ok {
		return Metadata{}, missingMetadataError("SamplesPerPixel", tagSamplesPerPixel)
	}
	bitsAllocated, ok := getUint16(obj, tagBitsAllocated)
	if !ok {
		return Metadata{}, missingMetadataError("BitsAllocated", tagBitsAllocated)
	}
	bitsStored, ok := getUint16(obj, tagBitsStored)
	if !ok {
		return Metadata{}, missingMetadataError("BitsStored", tagBitsStored)
	}
	highBit, ok := getUint16(obj, tagHighBit)
	if !ok {
		return Metadata{}, missingMetadataError("HighBit", tagHighBit)
	}
	pixelRepresentation, ok := getUint16(obj, tagPixelRepresentation)
	if !ok {
		return Metadata{}, missingMetadataError("PixelRepresentation", tagPixelRepresentation)
	}
	planarConfiguration, planarConfigurationPresent := getUint16(obj, tagPlanarConfiguration)

	photometricInterpretation, ok := obj.GetString(tagPhotometricInterpretation)
	// Padding is not part of a CS value; some writers pad with NUL instead of
	// a space (an OsiriX export writes "MONOCHROME2\x00").
	photometricInterpretation = strings.Trim(photometricInterpretation, " \x00")
	if !ok || photometricInterpretation == "" {
		return Metadata{}, missingMetadataError("PhotometricInterpretation", tagPhotometricInterpretation)
	}

	numberOfFrames, err := numberOfFrames(obj)
	if err != nil {
		return Metadata{}, err
	}

	return Metadata{
		Rows:                       rows,
		Columns:                    columns,
		SamplesPerPixel:            samplesPerPixel,
		BitsAllocated:              bitsAllocated,
		BitsStored:                 bitsStored,
		HighBit:                    highBit,
		PixelRepresentation:        pixelRepresentation,
		PlanarConfiguration:        planarConfiguration,
		PlanarConfigurationPresent: planarConfigurationPresent,
		NumberOfFrames:             numberOfFrames,
		PhotometricInterpretation:  photometricInterpretation,
	}, nil
}

// ExtractNativeFrames returns native, uncompressed frames from an owned
// buffer. One-bit frames that cross byte boundaries are independently packed
// starting at the least significant bit.
func ExtractNativeFrames(obj *object.Object) (*NativeFrames, error) {
	if _, incomplete := obj.IncompletePixelData(); incomplete {
		return nil, object.ErrPartialFile
	}
	pixel, err := Extract(obj)
	if err != nil {
		return nil, err
	}
	native, err := extractNativeFrames(pixel, obj)
	if err != nil {
		return nil, err
	}

	return &NativeFrames{
		Metadata: native.Metadata,
		Data:     native.Data,
	}, nil
}

// ExtractNativeFramesView is ExtractNativeFrames without the full Pixel Data
// clone. Frames alias the object's Pixel Data when the stored layout permits
// it. Unaligned one-bit frames and Big Endian words holding 8-bit or 1-bit
// samples need a separate buffer. Keep the returned frames and object
// unchanged while the view is in use.
func ExtractNativeFramesView(obj *object.Object) (*NativeFrames, error) {
	pixel, err := ExtractView(obj)
	if err != nil {
		return nil, err
	}
	return extractNativeFrames(pixel, obj)
}

func extractNativeFrames(pixel PixelData, obj *object.Object) (*NativeFrames, error) {
	metadata, frames, err := splitNativeFrames(pixel, obj, obj.ValueByteOrder())
	if err != nil {
		return nil, err
	}
	return &NativeFrames{
		Metadata: metadata,
		Data:     frames,
	}, nil
}

func decodeNativeFrames(pixel PixelData, obj *object.Object, order binary.ByteOrder) (Frames, error) {
	metadata, frames, err := splitNativeFrames(pixel, obj, order)
	if err != nil {
		return Frames{}, err
	}
	if metadata.BitsAllocated == 1 && len(frames) == 1 {
		// The extractors can expose a stored single-frame value as a view.
		// Decoding instead returns a frame with zero unused tail bits.
		if remainder := packedFrameBits(metadata) % 8; remainder != 0 {
			frame := frames[0]
			last := len(frame) - 1
			mask := byte(1<<remainder - 1)
			if frame[last]&^mask != 0 {
				frames[0] = core.CloneBytes(frame)
				frames[0][last] &= mask
			}
		}
	}
	return Frames{
		Rows:    int(metadata.Rows),
		Columns: int(metadata.Columns),
		Data:    frames,
	}, nil
}

func decodeEncapsulatedUncompressedFrames(pixel PixelData, obj *object.Object) (Frames, error) {
	metadata, frames, err := splitEncapsulatedUncompressedFrames(pixel, obj)
	if err != nil {
		return Frames{}, err
	}
	return Frames{
		Rows:    int(metadata.Rows),
		Columns: int(metadata.Columns),
		Data:    frames,
	}, nil
}

func splitNativeFrames(pixel PixelData, obj *object.Object, order binary.ByteOrder) (Metadata, [][]byte, error) {
	if _, incomplete := obj.IncompletePixelData(); incomplete {
		return Metadata{}, nil, object.ErrPartialFile
	}
	metadata, err := ExtractMetadata(obj)
	if err != nil {
		return Metadata{}, nil, err
	}
	frames, err := splitNativeFrameRange(pixel, obj, order, metadata, 0, metadata.NumberOfFrames)
	if err != nil {
		return Metadata{}, nil, err
	}
	return metadata, frames, nil
}

func splitNativeFrameRange(pixel PixelData, obj *object.Object, order binary.ByteOrder, metadata Metadata, first, end int) ([][]byte, error) {
	if pixel.Encapsulated {
		return nil, ErrEncapsulatedPixelData
	}

	frameSize := metadata.FrameSize()
	frameBuffersSize := metadata.TotalSize()
	totalSize := frameBuffersSize
	if metadata.BitsAllocated == 1 {
		totalSize, _ = nativePackedByteCount(metadata)
	}
	maxInt := int64(^uint(0) >> 1)
	if frameSize <= 0 || totalSize <= 0 || frameBuffersSize <= 0 || frameBuffersSize > maxInt {
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

	frameSizeInt := int(frameSize)
	totalSizeInt := int(totalSize)

	// Native Pixel Data from real-world scanners and archives often carries
	// trailing bytes beyond rows*columns*samples (block padding); frames are
	// sliced from the leading expected bytes and such excess is ignored.
	// Missing data is still rejected, as is excess of one full frame or more,
	// which points at wrong frame metadata rather than padding.
	actualSize := int64(len(pixel.Raw))
	excess := actualSize - totalSize
	if excess < 0 || (excess >= frameSize && !(totalSize%2 == 1 && excess == 1)) {
		return nil, fmt.Errorf(
			"%w: expected %d bytes for %d frame(s), got %d",
			ErrPixelDataSizeMismatch,
			totalSizeInt,
			metadata.NumberOfFrames,
			len(pixel.Raw),
		)
	}

	raw := pixel.Raw
	if element, ok := obj.Get(core.TagPixelData); ok && nativeOWByteLanes(order, element.VR(), metadata) {
		if len(raw)%2 != 0 {
			return nil, fmt.Errorf("%w: Big Endian OW Pixel Data has an odd byte length", ErrPixelDataSizeMismatch)
		}
		raw = reverseFixedWidthValues(raw[:totalSizeInt+totalSizeInt%2], 2)
	}
	frames := make([][]byte, end-first)
	if metadata.BitsAllocated == 1 && metadata.NumberOfFrames > 1 {
		frameBits := packedFrameBits(metadata)
		if frameBits%8 != 0 {
			aligned := make([]byte, len(frames)*frameSizeInt)
			for i := range frames {
				start := i * frameSizeInt
				end := start + frameSizeInt
				frames[i] = aligned[start:end:end]
				copyPackedBits(frames[i], 0, raw, int64(first+i)*frameBits, frameBits)
			}
			return frames, nil
		}
	}
	for i := range frames {
		start := (first + i) * frameSizeInt
		frames[i] = raw[start : start+frameSizeInt]
	}

	return frames, nil
}

func splitEncapsulatedUncompressedFrames(pixel PixelData, obj *object.Object) (Metadata, [][]byte, error) {
	metadata, err := ExtractMetadata(obj)
	if err != nil {
		return Metadata{}, nil, err
	}
	if !pixel.Encapsulated {
		return Metadata{}, nil, fmt.Errorf(
			"%w: transfer syntax %q expects encapsulated pixel data",
			ErrIncompatiblePixelData,
			transfer.EncapsulatedUncompressedExplicitVRLittleEndian.UID,
		)
	}

	frameSize := metadata.FrameSize()
	totalSize := metadata.TotalSize()
	maxInt := int64(^uint(0) >> 1)
	if frameSize <= 0 || totalSize <= 0 || frameSize > maxInt || totalSize > maxInt {
		return Metadata{}, nil, fmt.Errorf(
			"%w: rows=%d columns=%d samples_per_pixel=%d bits_allocated=%d number_of_frames=%d",
			ErrInvalidMetadata,
			metadata.Rows,
			metadata.Columns,
			metadata.SamplesPerPixel,
			metadata.BitsAllocated,
			metadata.NumberOfFrames,
		)
	}

	fragments := pixel.Sequence.Fragments
	if len(fragments) == 0 {
		return Metadata{}, nil, fmt.Errorf(
			"%w: expected %d bytes for %d frame(s), got 0 bytes across 0 fragments",
			ErrPixelDataSizeMismatch,
			totalSize,
			metadata.NumberOfFrames,
		)
	}

	frameSizeInt := int(frameSize)
	totalSizeInt := int(totalSize)
	if len(fragments) == metadata.NumberOfFrames {
		oneFragmentPerFrame := true
		for _, fragment := range fragments {
			if len(fragment) != frameSizeInt && !(frameSizeInt%2 != 0 && len(fragment) == frameSizeInt+1) {
				oneFragmentPerFrame = false
				break
			}
		}
		if oneFragmentPerFrame {
			assembled := make([]byte, totalSizeInt)
			frames := make([][]byte, metadata.NumberOfFrames)
			for i, fragment := range fragments {
				start := i * frameSizeInt
				copy(assembled[start:start+frameSizeInt], fragment[:frameSizeInt])
				frames[i] = assembled[start : start+frameSizeInt]
			}
			return metadata, frames, nil
		}
	}

	var fragmentBytes int64
	for _, fragment := range fragments {
		fragmentBytes += int64(len(fragment))
	}
	if !pixelDataLengthMatches(fragmentBytes, totalSize) {
		return Metadata{}, nil, fmt.Errorf(
			"%w: expected %d bytes for %d frame(s), got %d bytes across %d fragments",
			ErrPixelDataSizeMismatch,
			totalSize,
			metadata.NumberOfFrames,
			fragmentBytes,
			len(fragments),
		)
	}

	assembled := make([]byte, 0, totalSizeInt)
	for _, fragment := range fragments {
		assembled = append(assembled, fragment...)
	}
	if len(assembled) > totalSizeInt {
		assembled = assembled[:totalSizeInt]
	}

	frames := make([][]byte, metadata.NumberOfFrames)
	for i := range frames {
		start := i * frameSizeInt
		frames[i] = assembled[start : start+frameSizeInt]
	}

	return metadata, frames, nil
}

func pixelDataLengthMatches(actual, expected int64) bool {
	if actual == expected {
		return true
	}
	return expected%2 == 1 && actual == expected+1
}

func getUint16(obj *object.Object, tag core.Tag) (uint16, bool) {
	elem, ok := obj.Get(tag)
	if !ok || elem.VR() != core.VRUS {
		return 0, false
	}
	if values, ok := elem.Value.(core.Uint16Value); ok {
		if len(values) != 1 {
			return 0, false
		}
		return values[0], true
	}
	raw, ok := elem.RawBytes()
	if !ok || len(raw) != 2 {
		return 0, false
	}
	return obj.ValueByteOrder().Uint16(raw[:2]), true
}

func numberOfFrames(obj *object.Object) (int, error) {
	value, ok := obj.GetString(tagNumberOfFrames)
	v := strings.TrimSpace(value)
	if !ok || v == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > math.MaxInt32 {
		return 0, fmt.Errorf("%w: invalid NumberOfFrames", ErrInvalidMetadata)
	}
	return n, nil
}

func missingMetadataError(name string, tag core.Tag) error {
	return fmt.Errorf("%w: %s (%s)", ErrMissingMetadata, name, tag)
}

// JPEGColorConversion selects how lossy JPEG decoders treat three-component
// frames (the choice DCMTK offers as E_DecompressionColorSpaceConversion).
type JPEGColorConversion int

const (
	// JPEGColorGuess converts YCbCr to RGB when the codestream says it is
	// YCbCr: a JFIF APP0, then an Adobe APP14 transform, or else component
	// identifiers other than 'R', 'G', 'B' (the libjpeg rule, and the
	// default). The decoded frame is RGB.
	JPEGColorGuess JPEGColorConversion = iota
	// JPEGColorPhotometric trusts the DICOM Photometric Interpretation:
	// YBR_FULL and YBR_FULL_422 are converted to RGB, RGB is not converted.
	JPEGColorPhotometric
	// JPEGColorAlways converts every three-component frame from YCbCr.
	JPEGColorAlways
	// JPEGColorNever keeps the stored components (chroma upsampled); a
	// YBR_FULL_422 object decodes to YBR_FULL.
	JPEGColorNever
)

type jpegColorConversionKey struct{}

// WithJPEGColorConversion returns ctx carrying the JPEG color policy for
// codecs (DecompressOptions.JPEGColorConversion sets it for decompression).
func WithJPEGColorConversion(ctx context.Context, conversion JPEGColorConversion) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, jpegColorConversionKey{}, conversion)
}

// JPEGColorConversionFrom returns the JPEG color policy carried by ctx
// (JPEGColorGuess when none).
func JPEGColorConversionFrom(ctx context.Context) JPEGColorConversion {
	if ctx == nil {
		return JPEGColorGuess
	}
	conversion, _ := ctx.Value(jpegColorConversionKey{}).(JPEGColorConversion)
	return conversion
}
