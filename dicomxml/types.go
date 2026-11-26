// Package dicomxml converts DICOM data sets to and from the PS3.19 Native
// DICOM Model XML representation.
package dicomxml

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const (
	// Namespace is the namespace of the PS3.19 Native DICOM Model.
	Namespace = "http://dicom.nema.org/PS3.19/models/NativeDICOM"
	// MediaType is the PS3.18 media type for Native DICOM Model metadata.
	MediaType = "application/dicom+xml"
)

var (
	ErrInvalidXML             = errors.New("dicomxml: invalid Native DICOM Model XML")
	ErrInvalidLimits          = errors.New("dicomxml: invalid resource limits")
	ErrMaxXMLBytesExceeded    = errors.New("dicomxml: maximum XML bytes exceeded")
	ErrMaxValueBytesExceeded  = errors.New("dicomxml: maximum value bytes exceeded")
	ErrMaxInlineBytesExceeded = errors.New("dicomxml: maximum InlineBinary bytes exceeded")
	ErrMaxBinaryBytesExceeded = errors.New("dicomxml: maximum total binary bytes exceeded")
	ErrMaxDepthExceeded       = errors.New("dicomxml: maximum sequence depth exceeded")
	ErrMaxElementsExceeded    = errors.New("dicomxml: maximum element count exceeded")
	ErrMaxItemsExceeded       = errors.New("dicomxml: maximum sequence item count exceeded")
	ErrBulkDataRejected       = errors.New("dicomxml: BulkData rejected by policy")
	ErrBulkDataResolver       = errors.New("dicomxml: BulkData resolver failed")
	ErrTransferSyntaxRequired = errors.New("dicomxml: transfer syntax required for encapsulated Pixel Data")
)

// Error adds an operation and XML path to a conversion error. Callers can use
// errors.Is to classify the wrapped sentinel or context error.
type Error struct {
	Op   string
	Path string
	Err  error
}

func (e *Error) Error() string {
	if e.Path == "" {
		return "dicomxml: " + e.Op + ": " + e.Err.Error()
	}
	return "dicomxml: " + e.Op + " at " + e.Path + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// Limits bounds XML conversion. A zero field uses the corresponding safe
// default. Negative fields are invalid. To accept larger data, callers must
// opt in by setting explicit larger limits.
type Limits struct {
	MaxXMLBytes          int64
	MaxValueBytes        int
	MaxInlineBinaryBytes int64
	MaxTotalBinaryBytes  int64
	MaxSequenceDepth     int
	MaxElements          int
	MaxSequenceItems     int
}

// DefaultLimits returns the finite resource limits used by zero-valued
// Options and UnmarshalOptions.
func DefaultLimits() Limits {
	return Limits{
		MaxXMLBytes:          64 << 20,
		MaxValueBytes:        16 << 20,
		MaxInlineBinaryBytes: 32 << 20,
		MaxTotalBinaryBytes:  64 << 20,
		MaxSequenceDepth:     64,
		MaxElements:          1_000_000,
		MaxSequenceItems:     100_000,
	}
}

// Options configures Native DICOM Model marshaling.
type Options struct {
	Pretty bool
	// OmitGroupLength is retained for option symmetry with dicomjson. Native
	// DICOM Model output always omits group-length elements as PS3.19 requires,
	// regardless of this value.
	OmitGroupLength bool
	Dictionary      dictionary.DataDictionary
	// ByteOrder controls raw numeric and binary Value Field interpretation.
	// Nil uses object.Object.ValueByteOrder.
	ByteOrder binary.ByteOrder
	// SourceByteOrderInlineBinary keeps the InlineBinary of OW, OF, OD, OL
	// and OV values, and the data passed to BulkDataURIFunc, in ByteOrder. By
	// default they are written in Little Endian, the byte order PS3.18 section
	// 8.7.3 gives the Native DICOM Model documents of web services: when
	// ByteOrder is Big Endian, bytes are reversed in units of the VR width,
	// except OW Pixel Data with Bits Allocated 32 or 64 in the same data set
	// or item, which is reversed at the sample width, by the rules of
	// pixeldata.TranscodeDataSet. UnmarshalOptions.SourceByteOrderInlineBinary
	// reads such a document back.
	SourceByteOrderInlineBinary bool
	// BulkDataURIFunc may replace an inline binary Value Field with a URI.
	// Returning an empty URI keeps InlineBinary. Existing core.BulkDataValue
	// references are always preserved and never resolved by Marshal. data is
	// in the byte order InlineBinary would have.
	BulkDataURIFunc func(tag core.Tag, vr core.VR, data []byte) string
	Limits          Limits
	// RecoverPrivate opts in to exporting objects whose private data
	// elements have a missing or invalid Private Creator, which the Native
	// DICOM Model cannot represent (PS3.19 requires privateCreator). With it,
	// a creator whose value is only invalid for trailing NUL padding is
	// written without the NULs, a private data element that still has no
	// creator is omitted, and one in a second block reserved by the same
	// creator is merged into the first block (omitted if the first block
	// holds its offset). Each recovery is passed to PrivateRecovered, which
	// must then be set. By default Marshal rejects such an element.
	RecoverPrivate   bool
	PrivateRecovered func(PrivateRecovery)
	// RecoverText opts in to exporting text values that XML 1.0 cannot hold
	// only because of NUL padding at the end of their Value Field, which some
	// producers write instead of spaces: the trailing NULs (and spaces) of
	// the last value, or of the last Person Name component group, are
	// trimmed. Each recovery is passed to TextRecovered, which must then be
	// set. By default Marshal rejects any value containing a character XML
	// 1.0 cannot represent (NUL and the other C0 controls except TAB, LF and
	// CR, U+FFFE, U+FFFF, invalid UTF-8), which encoding/xml would otherwise
	// silently replace with U+FFFD. The NUL padding of a Private Creator is
	// recovered by RecoverPrivate.
	RecoverText   bool
	TextRecovered func(TextRecovery)
}

// TextRecoveryKind names what RecoverText did to a value.
type TextRecoveryKind uint8

const (
	// TextNULPaddingTrimmed: a text value padded with NUL bytes was written
	// without its trailing NULs and spaces. Text values read through the
	// object API no longer carry trailing NUL padding (#2032), so this
	// recovery applies only to values whose padding reached the encoder.
	TextNULPaddingTrimmed TextRecoveryKind = iota + 1
)

func (k TextRecoveryKind) String() string {
	switch k {
	case TextNULPaddingTrimmed:
		return "text NUL padding trimmed"
	default:
		return "unknown text recovery"
	}
}

// TextRecovery reports one recovery applied under Options.RecoverText.
type TextRecovery struct {
	Kind TextRecoveryKind
	// Path locates the value, as in Error.Path, for example
	// /NativeDicomModel/DicomAttribute[@tag='00100020']/Value[1] or
	// .../PersonName[1]/Alphabetic.
	Path string
	// Tag is the data element's tag as in the source object (not the
	// gggg00ee form written for private data elements).
	Tag core.Tag
}

// PrivateRecoveryKind names what RecoverPrivate did to an element.
type PrivateRecoveryKind uint8

const (
	// PrivateCreatorNULTrimmed: a Private Creator padded with NUL bytes was
	// written as LO without them. Trailing NUL padding is removed when the
	// creator is read (#2032), so this applies only to padding that reached
	// the encoder.
	PrivateCreatorNULTrimmed PrivateRecoveryKind = iota + 1
	// PrivateElementOmitted: a private data element without a valid Private
	// Creator, or whose offset its creator's first block already holds, was
	// left out of the XML.
	PrivateElementOmitted
	// PrivateElementMerged: a private data element in a second block of the
	// same creator was written under that creator, so it is read back into
	// the creator's first block.
	PrivateElementMerged
)

func (k PrivateRecoveryKind) String() string {
	switch k {
	case PrivateCreatorNULTrimmed:
		return "private creator NUL padding trimmed"
	case PrivateElementOmitted:
		return "private data element omitted"
	case PrivateElementMerged:
		return "private data element merged into its creator's first block"
	default:
		return "unknown private recovery"
	}
}

// PrivateRecovery reports one recovery applied under Options.RecoverPrivate.
type PrivateRecovery struct {
	Kind PrivateRecoveryKind
	// Path locates the element, as in Error.Path.
	Path string
	Tag  core.Tag
	// Creator is the creator written or merged into, when there is one.
	Creator string
}

// DefaultOptions returns standards-compliant, bounded marshaling options.
func DefaultOptions() Options {
	return Options{Pretty: true, OmitGroupLength: true, Limits: DefaultLimits()}
}

// BulkDataPolicy controls whether decoding preserves, rejects, or explicitly
// resolves a BulkData reference. PreserveBulkData performs no network or file
// access and is the default.
type BulkDataPolicy uint8

const (
	PreserveBulkData BulkDataPolicy = iota
	RejectBulkData
	ResolveBulkData
)

// BulkDataReference describes one PS3.19 BulkData element.
type BulkDataReference struct {
	Tag  core.Tag
	VR   core.VR
	URI  string
	UUID string
}

// BulkDataResolver opens content for a BulkData reference. It is called only
// under ResolveBulkData. The decoder closes the returned reader and applies
// binary byte limits while reading it.
type BulkDataResolver func(context.Context, BulkDataReference) (io.ReadCloser, error)

// UnmarshalOptions configures Native DICOM Model decoding.
type UnmarshalOptions struct {
	TextOptions object.TextOptions
	// AllowMissingKeyword accepts a known DICOM tag without its PS3.6 keyword.
	// The default is strict because PS3.19 requires keyword when the recipient's
	// dictionary knows the data element.
	AllowMissingKeyword bool
	// AllowForeignRootAttributes accepts a NativeDicomModel root element
	// without xml:space="preserve" and with namespace prefix declarations or
	// XML Schema instance attributes such as xsi:schemaLocation, as some
	// DICOMweb servers write it. The default is strict: only the default
	// namespace and xml:space are accepted.
	AllowForeignRootAttributes bool
	// ByteOrder controls raw numeric and binary Value Field construction. Nil
	// uses TransferSyntax.ByteOrder, then Little Endian when no syntax is known.
	// The InlineBinary of OW, OF, OD, OL and OV values, and the data a
	// BulkDataResolver returns for them, are read as Little Endian and, when
	// ByteOrder is Big Endian, reversed as Options.SourceByteOrderInlineBinary
	// describes. Bits Allocated of OW Pixel Data is taken from the same data
	// set or item; Walk sees it only when it precedes Pixel Data.
	ByteOrder binary.ByteOrder
	// SourceByteOrderInlineBinary reads that InlineBinary and BulkData as
	// already in ByteOrder, as Options.SourceByteOrderInlineBinary writes it.
	SourceByteOrderInlineBinary bool
	// TransferSyntax is needed only to reconstruct encapsulated Pixel Data
	// carried in InlineBinary.
	TransferSyntax   transfer.Syntax
	BulkDataPolicy   BulkDataPolicy
	BulkDataResolver BulkDataResolver
	Limits           Limits
}

// DefaultUnmarshalOptions returns bounded options that preserve BulkData
// references without dereferencing them.
func DefaultUnmarshalOptions() UnmarshalOptions {
	return UnmarshalOptions{ByteOrder: binary.LittleEndian, Limits: DefaultLimits()}
}

// Attribute is one top-level Native DICOM XML attribute emitted by Walk.
// Element uses the tag written in XML. For private data it is the normalized
// gggg00ee tag; PrivateCreator supplies its identity. Sequence item tags have
// already been allocated within their own datasets. The callback owns Value.
// Path identifies the XML attribute independently of private block allocation.
type Attribute struct {
	Element        core.Element
	PrivateCreator string
	Path           string
}

// Walk delivers top-level attributes without retaining their values in an
// Object. It is equivalent to WalkContext with context.Background().
func Walk(r io.Reader, dict dictionary.DataDictionary, opts UnmarshalOptions, visit func(Attribute) error) error {
	return WalkContext(context.Background(), r, dict, opts, visit)
}

// WalkContext converts one top-level attribute at a time. A sequence attribute
// retains its items until that attribute is delivered; callers may retain or
// discard values. Small tag/creator metadata is retained until the dataset ends
// to validate duplicate tags and private allocation, including out-of-order
// creators. A callback can precede a later document error; successful return is
// the document-completion signal. Callback errors stop decoding and are wrapped
// in *Error, preserving errors.Is. With ResolveBulkData, top-level private
// references use their normalized XML tag in BulkDataReference.Tag; nested
// references use their item dataset's allocated tags. Limits and cancellation
// apply throughout. A reader's blocking Read still requires caller cooperation.
func WalkContext(ctx context.Context, r io.Reader, dict dictionary.DataDictionary, opts UnmarshalOptions, visit func(Attribute) error) error {
	if visit == nil {
		return &Error{Op: "decode", Err: fmt.Errorf("%w: nil attribute visitor", ErrInvalidXML)}
	}
	_, err := decodeContext(ctx, r, dict, opts, visit)
	return err
}
