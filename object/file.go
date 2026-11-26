package object

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dcmtime"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/encoding"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	ErrMissingPreamble       = errors.New("dicom: missing Part 10 preamble or DICM marker")
	ErrMissingTransferSyntax = errors.New("dicom: missing TransferSyntaxUID in file meta information")
	ErrMissingSOPClassUID    = errors.New("dicom: missing SOP Class UID")
	ErrMissingSOPInstanceUID = errors.New("dicom: missing SOP Instance UID")
	ErrNilFile               = errors.New("dicom: nil file")
	ErrFileMeta              = errors.New("dicom: file meta error")
	ErrDataSet               = errors.New("dicom: data set error")
	// ErrDeferredSequenceValueWrite reports a deferred value nested in a
	// sequence item that cannot be written: its source location is lost, or
	// the writer (WriteDataSetWithValidation) cannot stream it.
	ErrDeferredSequenceValueWrite      = errors.New("dicom: deferred value nested in a sequence cannot be written")
	ErrInvalidFileMetaGroupLength      = errors.New("dicom: invalid file meta group length element")
	ErrInvalidFileMetaGroupLengthValue = errors.New("dicom: invalid file meta group length value")
	ErrInvalidPreambleLength           = errors.New("dicom: invalid Part 10 preamble length")
	ErrDeferredValueRequiresSeekable   = errors.New("dicom: deferred value reading requires an io.ReadSeeker source")
)

const mediaStorageDirectoryStorageUID = "1.2.840.10008.1.3.10"

type File struct {
	Preamble       []byte
	Meta           *Object
	Dataset        *Object
	TransferSyntax transfer.Syntax

	source                   io.Closer
	transferSyntaxResolution *TransferSyntaxResolution
	partialRead              *PartialRead
	incompletePixelDataRead  *IncompletePixelData
}

// BulkDataSource is a defined-length stream returned by BulkDataResolver.
type BulkDataSource = parser.BulkDataSource

// BulkDataResolver opens one externally referenced Bulk Data value for
// WriteFileWithOptions.
type BulkDataResolver = parser.BulkDataResolver

// SequenceWriteLimits bounds structural planning for defined SQ/item writes.
type SequenceWriteLimits = parser.SequenceWriteLimits

type WriteFileOptions struct {
	SequenceLimits SequenceWriteLimits
	// BulkDataSize is metadata only and must not open or consume the source.
	BulkDataSize func(core.BulkDataValue) (int64, error)
	// DeflateLevel selects raw dataset Deflate compression (0..9). Nil keeps
	// flate.DefaultCompression. It is invalid for other transfer syntaxes.
	DeflateLevel *int
	// Padding aligns the logical Part 10 prefix plus dataset, and Item Values.
	// Deflate output is not guaranteed to have an aligned physical size.
	Padding PaddingOptions
	// Preamble provides the 128-byte Part 10 preamble. A nil value writes a
	// zero-filled preamble.
	Preamble []byte
	// DefaultMissingTransferSyntax enables a controlled fallback for incomplete
	// files that have neither File.TransferSyntax nor file meta
	// TransferSyntaxUID. When enabled, WriteFileWithOptions writes the dataset
	// using Implicit VR Little Endian. The zero value keeps strict validation.
	DefaultMissingTransferSyntax bool
	// OverrideMissingTransferSyntax selects an explicit fallback for incomplete
	// files that have neither File.TransferSyntax nor file meta
	// TransferSyntaxUID. When set, it is resolved through the transfer syntax
	// registry and takes precedence over DefaultMissingTransferSyntax.
	OverrideMissingTransferSyntax string
	// OmitReconciledDataSetSOPUIDs preserves the absence of SOP Class UID and
	// SOP Instance UID in the encoded data set while still requiring them in
	// File Meta Information. This is intended for the Basic Directory IOD,
	// whose File-set UID is conveyed as the Media Storage SOP Instance UID.
	// The zero value retains the historical reconciliation behavior.
	OmitReconciledDataSetSOPUIDs bool
	// AllowPartial confirms that a File from a partial read
	// (ReadFileOptions.AllowPartialRead) may be written; without it such a
	// File is refused with ErrPartialFile. It never authorizes writing
	// diagnosed incomplete Pixel Data, which requires explicit normalization.
	AllowPartial bool
	// BulkDataResolver materializes core.BulkDataValue references while writing
	// the data set, including references nested in sequence items. The writer
	// owns and closes every non-nil source Reader returned by the resolver.
	BulkDataResolver BulkDataResolver
	// SequenceLength chooses undefined (the default), preserved or defined
	// lengths for sequences and items.
	SequenceLength SequenceLengthPolicy
	// GroupLength controls Group Length attributes outside File Meta, whose
	// group length is always written.
	GroupLength GroupLengthPolicy
	// MetaMode chooses how File Meta Information is rebuilt; the zero value
	// is FileMetaUpdate.
	MetaMode FileMetaMode
	// AETitles, when set, are recorded in File Meta Information.
	AETitles FileMetaAETitles
	// KeepPartialByteOrderUnits applies when the data set's values are held
	// in the other byte order than the written syntax (an object read in
	// Explicit VR Big Endian written in a Little Endian syntax, or the other
	// way round). A fixed-width value whose length is not a whole number of
	// its units, such as a malformed private FD of 4 bytes, is refused by
	// default; with this option its whole units are reversed and the trailing
	// partial unit is written unchanged, as a value written in its own byte
	// order keeps its bytes.
	KeepPartialByteOrderUnits bool
}

// ImplementationClassUID and ImplementationVersionName identify dicom-go in
// File Meta Information (0002,0012) and (0002,0013), as in association
// negotiation (net/ul).
const (
	ImplementationClassUID    = "1.2.826.0.1.3680043.10.543.1"
	ImplementationVersionName = "DICOMGO"
)

// FileMetaMode chooses how WriteFileWithOptions builds File Meta Information
// (group 0002), after DCMTK's file write modes. In every mode Media Storage
// SOP Class and Instance UID follow the data set and Transfer Syntax UID the
// encoding actually written.
type FileMetaMode int

const (
	// FileMetaUpdate, the default, keeps the other File Meta elements (Source
	// AE Title, Private Information and so on) and records dicom-go, the
	// writer, as Implementation Class UID and Version Name.
	FileMetaUpdate FileMetaMode = iota
	// FileMetaCreate builds File Meta Information anew: the required
	// elements, dicom-go's identity and the AETitles of the options only.
	FileMetaCreate
	// FileMetaPreserve keeps every existing File Meta element, another
	// implementation's identity included; what is missing is added as in
	// FileMetaUpdate.
	FileMetaPreserve
)

// FileMetaAETitles are the AE titles recorded in File Meta Information when
// set (PS3.10 7.1): Source (0002,0016) is the AE that wrote the file,
// Sending (0002,0017) the AE that sent the instance over the network to the
// AE that wrote it, and Receiving (0002,0018) the AE that received it.
type FileMetaAETitles struct {
	Source, Sending, Receiving string
}

// ErrInvalidFileMetaOptions reports an unknown FileMetaMode or an AE title
// that is not a valid AE value (1 to 16 characters of the default character
// repertoire without backslash, not all spaces).
var ErrInvalidFileMetaOptions = errors.New("dicom: invalid File Meta options")

type fileMetaOptions struct {
	mode     FileMetaMode
	aeTitles FileMetaAETitles
}

func (o fileMetaOptions) validate() error {
	if o.mode < FileMetaUpdate || o.mode > FileMetaPreserve {
		return fmt.Errorf("%w: mode %d", ErrInvalidFileMetaOptions, o.mode)
	}
	return o.aeTitles.Validate()
}

// Validate reports ErrInvalidFileMetaOptions for a set title that is not a
// valid AE value.
func (t FileMetaAETitles) Validate() error {
	for _, title := range []string{t.Source, t.Sending, t.Receiving} {
		if title != "" && !validAETitle(title) {
			return fmt.Errorf("%w: AE title %q", ErrInvalidFileMetaOptions, title)
		}
	}
	return nil
}

// validAETitle is the AE title rule of ul.ValidateAETitle (PS3.5 Table
// 6.2-1) applied to a File Meta value as written. Object parsing does not
// depend on the network packages, so the rule is repeated here and a ups test
// keeps the two in step.
func validAETitle(title string) bool {
	if len(title) > 16 || strings.TrimSpace(title) == "" {
		return false
	}
	for i := 0; i < len(title); i++ {
		if title[i] < 0x20 || title[i] > 0x7e || title[i] == '\\' {
			return false
		}
	}
	return true
}

// SequenceLengthPolicy chooses how sequences and items encode their length.
type SequenceLengthPolicy = parser.LengthPolicy

const (
	// SequenceLengthUndefined writes undefined lengths with delimiters.
	SequenceLengthUndefined = parser.LengthPolicyUndefined
	// SequenceLengthPreserve keeps the form each sequence was read with.
	SequenceLengthPreserve = parser.LengthPolicyPreserve
	// SequenceLengthDefined writes explicit lengths (dcmconv +e).
	SequenceLengthDefined = parser.LengthPolicyDefined
)

// GroupLengthPolicy controls Group Length (gggg,0000) attributes.
type GroupLengthPolicy = parser.GroupLengthPolicy

const (
	// GroupLengthPreserve writes Group Length attributes as they are.
	GroupLengthPreserve = parser.GroupLengthPreserve
	// GroupLengthRecalculate corrects the Group Length attributes present.
	GroupLengthRecalculate = parser.GroupLengthRecalculate
	// GroupLengthCreate writes a Group Length for every group.
	GroupLengthCreate = parser.GroupLengthCreate
	// GroupLengthRemove omits every Group Length.
	GroupLengthRemove = parser.GroupLengthRemove
)

// WriteDataSetOptions controls WriteDataSetWithOptions.
type WriteDataSetOptions struct {
	SequenceLimits SequenceWriteLimits
	BulkDataSize   func(core.BulkDataValue) (int64, error)
	// DeflateLevel and Padding have the same meaning as WriteFileOptions,
	// except top-level alignment starts at dataset offset zero.
	DeflateLevel     *int
	Padding          PaddingOptions
	SequenceLength   SequenceLengthPolicy
	GroupLength      GroupLengthPolicy
	BulkDataResolver BulkDataResolver
}

var (
	tagFileMetaInformationGroupLength = core.NewTag(0x0002, 0x0000)
	tagFileMetaInformationVersion     = core.NewTag(0x0002, 0x0001)
	tagMediaStorageSOPClassUID        = core.NewTag(0x0002, 0x0002)
	tagMediaStorageSOPInstanceUID     = core.NewTag(0x0002, 0x0003)
	tagTransferSyntaxUID              = core.NewTag(0x0002, 0x0010)
	tagImplementationClassUID         = core.NewTag(0x0002, 0x0012)
	tagImplementationVersionName      = core.NewTag(0x0002, 0x0013)
	tagSourceAETitle                  = core.NewTag(0x0002, 0x0016)
	tagSendingAETitle                 = core.NewTag(0x0002, 0x0017)
	tagReceivingAETitle               = core.NewTag(0x0002, 0x0018)

	tagSOPClassUID    = core.NewTag(0x0008, 0x0016)
	tagSOPInstanceUID = core.NewTag(0x0008, 0x0018)
	tagWaveformData   = core.NewTag(0x5400, 0x1010)
)

const (
	part10PreambleLength   = 128
	part10Magic            = "DICM"
	implementationClassUID = ImplementationClassUID
)

func (f *File) Get(tag core.Tag) (core.Element, bool) {
	return f.objectForTag(tag).Get(tag)
}

func (f *File) Has(tag core.Tag) bool {
	return f.objectForTag(tag).Has(tag)
}

func (f *File) GetRaw(tag core.Tag) ([]byte, bool) {
	return f.objectForTag(tag).GetRaw(tag)
}

func (f *File) GetString(tag core.Tag) (string, bool) {
	return f.objectForTag(tag).GetString(tag)
}

func (f *File) GetStrings(tag core.Tag) ([]string, bool) {
	return f.objectForTag(tag).GetStrings(tag)
}

func (f *File) LookupString(tag core.Tag) (string, error) {
	return f.objectForTag(tag).LookupString(tag)
}

func (f *File) LookupStrings(tag core.Tag) ([]string, error) {
	return f.objectForTag(tag).LookupStrings(tag)
}

func (f *File) CharacterSet() (encoding.SpecificCharacterSet, error) {
	if f == nil {
		return encoding.SpecificCharacterSet{}, fmt.Errorf("dicom: nil file")
	}
	if f.Dataset == nil {
		return encoding.SpecificCharacterSet{}, fmt.Errorf("dicom: file has no dataset")
	}
	return f.Dataset.CharacterSet()
}

func (f *File) GetPersonName(tag core.Tag) (core.PersonName, bool) {
	return f.objectForTag(tag).GetPersonName(tag)
}

func (f *File) GetPersonNames(tag core.Tag) ([]core.PersonName, bool) {
	return f.objectForTag(tag).GetPersonNames(tag)
}

func (f *File) GetUID(tag core.Tag) (string, bool) {
	return f.objectForTag(tag).GetUID(tag)
}

func (f *File) GetUIDs(tag core.Tag) ([]string, bool) {
	return f.objectForTag(tag).GetUIDs(tag)
}

func (f *File) GetInt(tag core.Tag) (int64, error) {
	return f.objectForTag(tag).GetInt(tag)
}

func (f *File) GetInts(tag core.Tag) ([]int64, error) {
	return f.objectForTag(tag).GetInts(tag)
}

func (f *File) GetFloat(tag core.Tag) (float64, error) {
	return f.objectForTag(tag).GetFloat(tag)
}

func (f *File) GetFloats(tag core.Tag) ([]float64, error) {
	return f.objectForTag(tag).GetFloats(tag)
}

func (f *File) LookupDate(tag core.Tag) (dcmtime.Date, error) {
	return f.objectForTag(tag).LookupDate(tag)
}

func (f *File) LookupTime(tag core.Tag) (dcmtime.Time, error) {
	return f.objectForTag(tag).LookupTime(tag)
}

func (f *File) LookupDateTime(tag core.Tag) (dcmtime.Datetime, error) {
	return f.objectForTag(tag).LookupDateTime(tag)
}

func (f *File) GetSequence(tag core.Tag) ([]*Object, bool) {
	return f.objectForTag(tag).GetSequence(tag)
}

func (f *File) SetTextOptions(opts TextOptions) {
	if f == nil {
		return
	}
	if f.Meta != nil {
		f.Meta.SetTextOptions(opts)
	}
	if f.Dataset != nil {
		f.Dataset.SetTextOptions(opts)
	}
}

// ReadFileOptions configures high-level Part 10 reading.
//
// Example:
//
//	file, err := object.ReadFileWithOptions(r, object.ReadFileOptions{
//		MaxElementBytes: 16 << 20,
//		MaxTotalBytes:   128 << 20,
//		MaxSequenceDepth: 32,
//		MaxElements:     100000,
//		MaxFragments:    10000,
//	})
//
// Numeric limit fields default to zero, which means unlimited, except
// MaxSequenceDepth, whose zero value applies parser.DefaultMaxSequenceDepth.
// With StopBeforeTag or AllowIncompletePixelData, zero limits receive finite
// defaults instead.
type ReadFileOptions struct {
	// AllowIncompletePixelData retains terminal root integer Pixel Data at
	// physical EOF and returns an inspected partial File plus an error. Finite
	// limits apply; unsupported read modes are refused. Decompression requires
	// DecodeRecovery.IncompleteData. AllowPartial never authorizes writing this
	// diagnosed input: normalize its complete frames before rewriting.
	AllowIncompletePixelData bool
	// StopBeforeTag enables a bounded Part 10 prefix read on an io.ReadSeeker.
	// The first root header >= this copied threshold is consumed, its value
	// is untouched. A stopped File returns ReadStopError and is incomplete.
	// Nil retains complete reading. Deflated stops use logical coordinates.
	StopBeforeTag *core.Tag
	// PrefixMaxTokens caps dataset headers/control in the selective prefix pass;
	// zero is 1,000,000. It does not count File Meta, which is read completely
	// under its existing separate reader limits.
	// A nonzero value requires StopBeforeTag. Negative values are refused.
	PrefixMaxTokens int
	// BulkDataDescriptor adds VR/length/path deferral on seekable, non-deflated sources.
	BulkDataDescriptor *parser.BulkDataDescriptor
	// Dictionary controls the dictionary used when parsing the main dataset.
	// A nil value defaults to dictionary/std.Dictionary.
	Dictionary dictionary.DataDictionary
	// Private resolution activates only with a PrivateDataDictionary in Dictionary.
	MaxPrivateCreators           int
	MaxPrivateDiagnostics        int
	RejectInvalidPrivateCreators bool
	// FileMetaDictionary controls the dictionary used when parsing the Part 10
	// file meta information. A nil value defaults to dictionary/std.Dictionary.
	FileMetaDictionary dictionary.DataDictionary

	// MaxElementBytes limits a single element value allocation in bytes.
	// A zero value means unlimited.
	MaxElementBytes int64
	// InlineValueBytesThreshold controls when the underlying parser will stop
	// materializing defined-length primitive element values into memory.
	//
	// If > 0 and an OB, OW, OF, OD or UN element value length is strictly
	// greater than this threshold, its bytes will be consumed but the returned
	// core.Element will have a nil Value (metadata-only for that element).
	// Large values of other VRs are materialized, subject to MaxElementBytes.
	// CopyValueTo can stream those skipped bytes only when the reader has a
	// seekable source with an attached value provider. ReadFileWithOptions
	// disables this threshold for non-seekable Part 10 inputs and materializes
	// values instead.
	// DeferNestedValues extends this policy into sequence items; the SQ
	// containers themselves remain materialized.
	//
	// A zero value preserves historical behavior of always materializing
	// defined-length values (subject to MaxElementBytes).
	InlineValueBytesThreshold int64
	// DeferNestedValues extends InlineValueBytesThreshold into sequence items,
	// so large nested OB, OW, OF, OD and UN values (an encapsulated document,
	// overlay or LUT data, a private blob) also stay on the source; values of
	// other VRs nested in items stay materialized. Object.CopyNestedValueTo
	// reads them and WriteFile streams them back. Sequences themselves are
	// always materialized (#278).
	DeferNestedValues bool
	// DeferValueTags, when non-empty, limits InlineValueBytesThreshold to these
	// tags; values of other tags are materialized whatever their length.
	DeferValueTags []core.Tag
	// MaxPixelDataBytes limits native Pixel Data values and the cumulative bytes
	// of encapsulated Pixel Data. When non-zero, it replaces MaxElementBytes for
	// Pixel Data while other elements keep their own ceiling.
	MaxPixelDataBytes int64
	// MaxDeformableVectorGridBytes limits each deformable Vector Grid Data
	// (0064,0009) value before allocation, including values nested in sequences.
	// A zero value leaves these values subject to MaxElementBytes.
	MaxDeformableVectorGridBytes int64
	// MaxTotalBytes limits the absolute logical extent consumed by each parser
	// pass. A zero value means unlimited. Explicit multi-pass recovery can reread
	// a seekable prefix; its separate probe remains capped by this value and by
	// TransferSyntaxProbeOptions.MaxProbeBytes.
	MaxTotalBytes int64
	// MaxSequenceDepth limits combined sequence and item nesting depth; deeper
	// input fails with parser.ErrMaxDepthExceeded. A zero value applies
	// parser.DefaultMaxSequenceDepth (64 nested sequences), a larger value
	// raises the limit and parser.UnlimitedSequenceDepth (any negative value)
	// removes it.
	MaxSequenceDepth int
	// MaxElements limits the number of primitive elements read by each internal
	// parser.Reader used by ReadFileWithOptions: one for file meta and one for
	// the main data set. A zero value means unlimited. MaxTotalBytes remains
	// absolute across both readers because each countingReader starts from
	// BaseOffset.
	MaxElements int
	// MaxFragments limits encapsulated Pixel Data fragments, excluding the Basic
	// Offset Table, per internal parser.Reader instance. In practice this means
	// the main data set reader enforces it, while the file meta reader never
	// encounters fragments. A zero value means unlimited.
	MaxFragments int
	// SkipPixelData consumes Pixel Data values without materializing their bytes.
	// The Pixel Data element remains present with a nil Value.
	SkipPixelData bool
	// DataSetTags selects root attributes for metadata projection. Nil retains
	// the complete historical dataset. Non-nil retains each selected value and
	// its entire sequence subtree, plus character-set/context/private creators.
	// Unselected root headers remain with core.DiscardedValue; their complete
	// grammar is parsed without retaining their payloads or SQ/item collections.
	// Requires SkipPixelData and excludes partial/prefix/deferred/sink modes.
	// This is structural parsing, not uniform/IOD validation. Discarded data
	// cannot be written. Caller-owned original bytes must remain available.
	DataSetTags []core.Tag
	// SeekUnselectedSequences passes each unselected defined-length root
	// sequence of a DataSetTags projection by one seek on a seekable source,
	// instead of parsing its items (parser.ReaderOptions.SeekUnselectedSequences).
	// Non-seekable and deflated input and lengths overrunning the source keep
	// the structural parse and its failures; malformed content inside a
	// sequence passed by seek is not detected. It requires DataSetTags.
	SeekUnselectedSequences bool
	// DeferPixelData consumes top-level integer, Float, and Double Float Pixel
	// Data values without materializing their bytes and keeps the original
	// seekable source available for CopyValueTo/WriteFile round-trips. Native
	// Pixel Data nested in sequence items, such as an Icon Image Sequence, is
	// materialized. This option requires an io.ReadSeeker source.
	DeferPixelData bool
	// DeferWaveformData consumes Waveform Data (5400,1010) values without
	// materializing their bytes, including values nested in Waveform Sequence
	// items. File.ValueLocations returns their source offsets in occurrence
	// order together with the nearest enclosing item offset. This option
	// requires an io.ReadSeeker source.
	DeferWaveformData bool
	// FrameSink receives native Pixel Data frames while the parser reads them.
	// Native Pixel Data is not materialized when a sink is configured.
	FrameSink FrameSink
	// EncapsulatedSink streams top-level encoded Pixel Data through an assembler.
	// DeferPixelData explicitly retains replay from a seekable source; otherwise
	// core.DiscardedValue prevents later serialization of the discarded payload.
	EncapsulatedSink parser.EncapsulatedSink
	// SkipProcessingPixelDataValue is kept for ParseOption source compatibility.
	//
	// Deprecated: this option has no effect. dicom-go preserves raw Pixel Data
	// bytes by default. Use SkipPixelData, DeferPixelData or FrameSink when the
	// reader should skip, defer or stream Pixel Data values.
	SkipProcessingPixelDataValue bool
	// AllowMissingMetaElementGroupLength accepts Part 10 file meta information
	// that starts with other group 0002 elements instead of (0002,0000).
	AllowMissingMetaElementGroupLength bool
	// AllowImplicitVRFileMeta reads File Meta Information written in Implicit
	// VR Little Endian (a group 0002 tag not followed by a valid VR), using
	// the file meta dictionary. The repair is reported on File.Meta as a
	// BoundaryRecovery with reason BoundaryImplicitVRFileMeta. Writing always
	// produces conforming Explicit VR Little Endian File Meta Information.
	AllowImplicitVRFileMeta bool
	// AllowMismatchPixelDataLength is kept for ParseOption source compatibility.
	//
	// Deprecated: this option has no effect. dicom-go preserves raw Pixel Data
	// during reading and validates native frame sizes only when callers
	// explicitly extract or stream frames.
	AllowMismatchPixelDataLength bool
	// SkipMetadataReadOnNewParserInit is kept for ParseOption source compatibility.
	// Part 10 readers still require file meta information.
	SkipMetadataReadOnNewParserInit bool
	// TextOptions configures decoding of textual values in the returned objects.
	TextOptions TextOptions
	// StrictReservedBytes rejects explicit VR reserved bytes that are not zero.
	// The zero value keeps permissive behavior.
	StrictReservedBytes bool
	// OddLengthPolicy controls how odd element lengths are handled.
	// The zero value uses the parser default.
	OddLengthPolicy parser.OddLengthPolicy
	// RecoverContainerOverruns reads a sequence or item whose declared length
	// runs past its enclosing sequence or item only to that container's end,
	// skips an item header with no body left and closes an undefined-length
	// child still open at the end. Object.BoundaryRecoveries lists each
	// repair. Without it these cases fail with parser.ErrContainerOverrun;
	// values crossing a container end fail either way.
	RecoverContainerOverruns bool
	// MaxBoundaryRecoveries caps Object.BoundaryRecoveries; 0 means
	// parser.DefaultMaxBoundaryRecoveries.
	MaxBoundaryRecoveries int
	// RecoverImplicitVRElements reads an element whose VR bytes are invalid
	// in an Explicit VR Little Endian data set as Implicit VR, when its
	// length is plausible; see parser.ReaderOptions. Each switch is listed by
	// Object.BoundaryRecoveries (reason implicit-vr-element).
	RecoverImplicitVRElements bool
	// MaxImplicitVRElements caps those switches; 0 uses the parser default.
	MaxImplicitVRElements int
	// SequenceRecovery selects independent, reported repairs of elements that
	// should be sequences and of misplaced delimiters; see
	// parser.SequenceRecoveryOptions.
	SequenceRecovery parser.SequenceRecoveryOptions
	// AllowPartialRead returns, when the data set cannot be read to the end,
	// the File built from completed elements and sequence-item prefixes,
	// together with a *PartialReadError (errors.Is ErrPartialRead and the
	// original cause). PartialRead.Path/Containers mark incomplete wrappers;
	// truncated primitives and encapsulated Pixel Data are dropped.
	// A partial File is never valid: File.PartialRead describes it,
	// decompression refuses it and writing needs WriteFileOptions.AllowPartial.
	// The transfer syntax recovery readers ignore it.
	AllowPartialRead bool
	// VRRecovery selects independent, reported repairs of wrong VRs
	// and length fields; see parser.VRRecoveryOptions.
	VRRecovery parser.VRRecoveryOptions
	// RecoverDelimiterAnomalies repairs byte-swapped item tags, delimiters
	// with a nonzero length, orphan delimiters and (FFFE,0000) group lengths
	// inside sequences; see parser.ReaderOptions. Each repair is listed by
	// Object.BoundaryRecoveries.
	RecoverDelimiterAnomalies bool
	// AllowZlibDeflate reads a Deflated Explicit VR Little Endian data set
	// that starts with a valid zlib header (RFC 1950) instead of the raw
	// deflate stream PS3.5 A.5 requires, as DCMTK does. The Adler-32 trailer
	// is checked and the use is listed by Object.BoundaryRecoveries. Writing
	// always produces raw deflate.
	AllowZlibDeflate bool
	// MaxDelimiterRecoveries caps those repairs; 0 uses the parser default.
	MaxDelimiterRecoveries int
}

func OpenFile(path string) (*File, error) {
	return OpenFileWithOptions(path, ReadFileOptions{})
}

func OpenFileWithOptions(path string, opts ReadFileOptions) (result *File, err error) {
	if err := projectionReadOptions(opts); err != nil {
		return nil, err
	}
	if err := incompleteReadOptions(opts); err != nil {
		return nil, err
	}
	opts, err = normalizePrefixOptions(opts)
	if err != nil {
		return nil, err
	}
	finishFrames := finalizeFrameSink(&opts)
	defer finishFrames(&err)
	f, err := OpenSourceFile(path)
	if err != nil {
		return nil, err
	}
	closeFile := true
	defer func() {
		if !closeFile {
			return
		}
		if cerr := f.Close(); cerr != nil {
			if err == nil {
				err = cerr
			} else {
				err = errors.Join(err, cerr)
			}
		}
	}()
	result, err = ReadFileWithOptions(f, opts)
	if (err == nil || errors.Is(err, ErrPartialRead)) && result != nil && result.Dataset != nil && result.Dataset.valueProvider != nil && result.Dataset.deferredCount > 0 {
		result.source = f
		closeFile = false
	}
	return result, err
}

// Close releases any source handle kept alive for deferred value streaming.
func (f *File) Close() error {
	if f == nil || f.source == nil {
		return nil
	}
	err := f.source.Close()
	f.source = nil
	return err
}

// ValueLocations returns all deferred raw value locations for tag in dataset
// source order. The returned slice is a copy and can be mutated by the caller.
func (f *File) ValueLocations(tag core.Tag) []parser.ValueLocation {
	if f == nil || f.Dataset == nil {
		return nil
	}
	provider, ok := f.Dataset.valueProvider.(*readerValueProvider)
	if !ok || provider == nil || provider.reader == nil {
		return nil
	}
	return provider.reader.ValueLocations(tag)
}

func ReadFile(r io.Reader) (*File, error) {
	return ReadFileWithOptions(r, ReadFileOptions{})
}

func ReadFileWithOptions(r io.Reader, opts ReadFileOptions) (result *File, err error) {
	if err := projectionReadOptions(opts); err != nil {
		return nil, err
	}
	if err := incompleteReadOptions(opts); err != nil {
		return nil, err
	}
	opts, err = normalizePrefixOptions(opts)
	if err != nil {
		return nil, err
	}
	if opts.StopBeforeTag != nil {
		if _, ok := r.(io.ReadSeeker); !ok {
			return nil, ErrReadPrefixRequiresSeekable
		}
	}
	finishFrames := finalizeFrameSink(&opts)
	defer finishFrames(&err)
	// Preserve ReadFile's historical zero-value behavior.
	if opts.FileMetaDictionary == nil {
		opts.FileMetaDictionary = std.Dictionary
	}
	if opts.Dictionary == nil {
		opts.Dictionary = std.Dictionary
	}
	seekableSource, readerSourceSeekable := r.(io.ReadSeeker)
	var preludeAudit *incompletePreludeAudit
	if opts.AllowIncompletePixelData {
		preludeAudit = &incompletePreludeAudit{source: r}
	}
	if opts.DeferPixelData || opts.DeferWaveformData || opts.BulkDataDescriptor != nil {
		if !readerSourceSeekable {
			return nil, ErrDeferredValueRequiresSeekable
		}
	}

	var (
		preamble      []byte
		meta          *Object
		syntax        transfer.Syntax
		datasetOffset int64
		readerSource  io.Reader
	)
	if readerSourceSeekable {
		streamBase, err := seekableSource.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		headerSource := seekableSource
		if preludeAudit != nil {
			headerSource = incompletePreludeSeeker{incompletePreludeAudit: preludeAudit, seeker: seekableSource}
		}
		header, err := readPart10HeaderAt(headerSource, opts, streamBase)
		if preludeAudit != nil && preludeAudit.failure != nil {
			return nil, preludeAudit.failure
		}
		if err != nil {
			return nil, err
		}
		preamble = header.Preamble
		meta = header.Meta
		syntax = header.TransferSyntax
		datasetOffset = header.DataSetOffset - streamBase
		readerSource = seekableSource
	} else {
		headerSource := r
		if preludeAudit != nil {
			headerSource = preludeAudit
		}
		br := bufio.NewReader(headerSource)
		prefix := make([]byte, part10PreambleLength+len(part10Magic))
		_, prefixErr := io.ReadFull(br, prefix)
		if preludeAudit != nil && preludeAudit.failure != nil {
			return nil, preludeAudit.failure
		}
		if prefixErr != nil {
			return nil, prefixErr
		}
		if string(prefix[part10PreambleLength:]) != part10Magic {
			return nil, ErrMissingPreamble
		}
		var err error
		meta, syntax, datasetOffset, err = readFileMeta(br, int64(len(prefix)), opts)
		if preludeAudit != nil && preludeAudit.failure != nil {
			return nil, preludeAudit.failure
		}
		if err != nil {
			return nil, wrapFileMetaError(err)
		}
		preamble = prefix[:part10PreambleLength]
		readerSource = br
	}
	if preludeAudit != nil && preludeAudit.failure != nil {
		return nil, preludeAudit.failure
	}
	readerReadOpts := opts
	zlibWrapped := false
	if syntax.Deflated {
		if opts.AllowIncompletePixelData {
			return nil, wrapDataSetError(parser.ErrIncompletePixelDataOptions)
		}
		if readerReadOpts.DeferPixelData || readerReadOpts.DeferWaveformData || readerReadOpts.BulkDataDescriptor != nil {
			return nil, wrapDataSetError(errDeflatedDeferredValue())
		}
		inflated, wrapped, err := inflatedDataSet(readerSource, opts.AllowZlibDeflate)
		if err != nil {
			return nil, wrapDataSetError(err)
		}
		defer func() { _ = inflated.Close() }()
		readerSource = inflated
		readerSourceSeekable = false
		zlibWrapped = wrapped
	}
	if !readerSourceSeekable {
		readerReadOpts.InlineValueBytesThreshold = 0
	}
	readerOpts := readerReadOpts.parserReaderOptions(datasetOffset)
	var reader *parser.Reader
	if opts.StopBeforeTag == nil {
		reader = parser.NewReader(readerSource, syntax, readerOpts)
	}
	var dataset core.DataSet
	var stop parser.ReadStop
	var partial *PartialRead
	if opts.StopBeforeTag != nil {
		reader, err = newPrefixReader(readerSource, syntax, readerOpts, opts)
		if err != nil {
			return nil, wrapDataSetError(err)
		}
		dataset, stop, err = reader.ReadDataSetPrefix()
		if err != nil {
			return nil, wrapDataSetError(err)
		}
	} else if opts.AllowPartialRead {
		dataset, err = reader.ReadDataSetPartial()
		if err != nil {
			if isContextError(err) {
				return nil, wrapDataSetError(err)
			}
			partial = newPartialRead(wrapDataSetError(err), len(dataset.Elements))
		}
	} else {
		dataset, err = readDataSetProjection(reader, opts)
		if err != nil {
			if !opts.AllowIncompletePixelData || !errors.Is(err, parser.ErrIncompletePixelData) {
				return nil, wrapDataSetError(err)
			}
			info, certified := reader.IncompletePixelData()
			if !certified {
				return nil, wrapDataSetError(err)
			}
			partial = incompletePartial(info, len(dataset.Elements))
		}
	}
	if preludeAudit != nil && preludeAudit.failure != nil {
		return nil, preludeAudit.failure
	}
	if !syntax.ExplicitVR {
		dataset = resolveImplicitContextualVRs(dataset, readerOpts.Dictionary, syntax.ByteOrder)
	}
	datasetObject := fromParsedDataSetWithTextOptions(dataset, readerOpts.Dictionary, opts.TextOptions)
	datasetObject.SetValueByteOrder(syntax.ByteOrder)
	datasetObject.attachIncompletePixelData(reader)
	datasetObject.privateDiagnostics, datasetObject.privateDiagnosticsTruncated = reader.PrivateDiagnostics()
	datasetObject.boundaryRecoveries, datasetObject.boundaryRecoveriesTruncated = reader.BoundaryRecoveries()
	if zlibWrapped {
		datasetObject.boundaryRecoveries = append([]parser.BoundaryRecovery{zlibDeflateRecovery(datasetOffset)}, datasetObject.boundaryRecoveries...)
	}
	file := &File{
		Preamble:       preamble,
		Meta:           meta,
		Dataset:        datasetObject,
		TransferSyntax: syntax,
	}
	if readerOptionsNeedValueProvider(readerOpts) && readerSourceSeekable && file.Dataset.deferredCount > 0 {
		file.Dataset.setValueProvider(newReaderValueProvider(reader, file.Dataset))
	}
	if stop.Stopped {
		stopErr := &ReadStopError{Stop: stop, Elements: len(dataset.Elements)}
		file.partialRead = &PartialRead{Offset: stop.Offset, Tag: stop.Header.Tag, Elements: len(dataset.Elements), RetainedElements: len(dataset.Elements), Err: fileReadStopCause{stop: stop, elements: len(dataset.Elements)}}
		return file, stopErr
	}
	if partial != nil {
		file.partialRead = partial
		if datasetObject.incompletePixelData != nil {
			file.incompletePixelDataRead = datasetObject.incompletePixelData
		}
		return file, &PartialReadError{Read: clonePartialRead(*partial)}
	}
	return file, nil
}

func readFileMeta(r *bufio.Reader, baseOffset int64, opts ReadFileOptions) (*Object, transfer.Syntax, int64, error) {
	meta, datasetOffset, err := readFileMetaElements(r, baseOffset, opts)
	if err != nil {
		return nil, transfer.Syntax{}, 0, err
	}
	syntax, err := resolveFileMetaTransferSyntax(meta)
	if err != nil {
		return nil, transfer.Syntax{}, 0, err
	}
	return meta, syntax, datasetOffset, nil
}

// readFileMetaElements parses only the Part 10 File Meta structure. Keeping
// syntax resolution separate lets the explicit recovery API preserve the
// original meta object and exact dataset offset when the UID itself is absent
// or unknown. The strict reader above intentionally retains its historical
// return shape and errors.
func readFileMetaElements(r *bufio.Reader, baseOffset int64, opts ReadFileOptions) (*Object, int64, error) {
	metaOpts := opts
	metaOpts.InlineValueBytesThreshold = 0
	metaOpts.SkipPixelData = false
	metaOpts.DeferPixelData = false
	metaOpts.DeferWaveformData = false
	metaOpts.BulkDataDescriptor = nil
	metaOpts.FrameSink = nil
	metaOpts.EncapsulatedSink = nil
	if opts.FileMetaDictionary != nil {
		metaOpts.Dictionary = opts.FileMetaDictionary
	}
	metaReaderOpts := metaOpts.parserReaderOptions(baseOffset)
	if opts.AllowImplicitVRFileMeta {
		if head, err := r.Peek(6); err == nil && implicitVRFileMetaHead(head) {
			meta, end, err := readFileMetaElementsByGroup(r, parser.NewReader(r, transfer.ImplicitVRLittleEndian, metaReaderOpts), metaReaderOpts)
			if err != nil {
				return nil, 0, err
			}
			meta.recordImplicitVRFileMeta(baseOffset, end)
			return meta, end, nil
		}
	}
	if opts.AllowMissingMetaElementGroupLength {
		return readFileMetaElementsByGroup(r, parser.NewReader(r, transfer.ExplicitVRLittleEndian, metaReaderOpts), metaReaderOpts)
	}
	return readDeclaredFileMeta(r, metaReaderOpts)
}

func readFileMetaElementsByGroup(r *bufio.Reader, metaReader *parser.Reader, metaReaderOpts parser.ReaderOptions) (*Object, int64, error) {
	var metaElements []core.Element
	for {
		next, err := r.Peek(4)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		if binary.LittleEndian.Uint16(next[:2]) != 0x0002 {
			break
		}
		tok, err := metaReader.Next()
		if err != nil {
			return nil, 0, err
		}
		if tok.Kind != parser.TokenElement {
			return nil, 0, fmt.Errorf("dicom: unexpected file meta token %s", tok.Kind)
		}
		metaElements = append(metaElements, tok.Element)
	}
	if len(metaElements) == 0 {
		return nil, 0, ErrInvalidFileMetaGroupLength
	}
	meta := FromDataSet(core.DataSet{Elements: metaElements}, metaReaderOpts.Dictionary)
	return meta, metaReader.Position(), nil
}

func resolveFileMetaTransferSyntax(meta *Object) (transfer.Syntax, error) {
	uid, ok := meta.GetString(core.NewTag(0x0002, 0x0010))
	if !ok {
		return transfer.Syntax{}, ErrMissingTransferSyntax
	}
	uid = transfer.NormalizeUID(uid)
	if uid == "" {
		return transfer.Syntax{}, ErrMissingTransferSyntax
	}
	syntax, ok := transfer.DefaultRegistry.Get(uid)
	if !ok {
		return transfer.Syntax{}, fmt.Errorf("%w: %q", transfer.ErrUnknownTransferSyntax, uid)
	}
	if !syntax.Supported {
		return transfer.Syntax{}, unsupportedTransferSyntaxError("reader", syntax)
	}
	return syntax, nil
}

func ReadDataSet(r io.Reader, syntax transfer.Syntax) (*Object, error) {
	return ReadDataSetWithOptions(r, syntax, ReadFileOptions{})
}

func ReadDataSetWithOptions(r io.Reader, syntax transfer.Syntax, opts ReadFileOptions) (result *Object, err error) {
	if err := projectionReadOptions(opts); err != nil {
		return nil, err
	}
	if err := incompleteReadOptions(opts); err != nil {
		return nil, err
	}
	if err := rejectPrefixOptions(opts); err != nil {
		return nil, err
	}
	finishFrames := finalizeFrameSink(&opts)
	defer finishFrames(&err)
	syntax, err = validateReadableSyntax(syntax)
	if err != nil {
		return nil, err
	}
	_, seekable := r.(io.ReadSeeker)
	if (opts.DeferPixelData || opts.DeferWaveformData || opts.BulkDataDescriptor != nil) && !seekable {
		return nil, ErrDeferredValueRequiresSeekable
	}
	obj, _, err := readDataSetObject(r, syntax, opts, 0, seekable)
	return obj, err
}

func readDataSetObject(r io.Reader, syntax transfer.Syntax, opts ReadFileOptions, baseOffset int64, streamValues bool) (*Object, *parser.Reader, error) {
	if opts.AllowIncompletePixelData && syntax.Deflated {
		return nil, nil, parser.ErrIncompletePixelDataOptions
	}
	readerSource := r
	legacy, isLegacy := transfer.LegacySyntax(syntax.UID)
	if isLegacy && (opts.FrameSink != nil || opts.EncapsulatedSink != nil) {
		return nil, nil, ErrLegacySyntaxFrameSink
	}
	zlibWrapped := false
	if syntax.Deflated {
		if opts.DeferPixelData || opts.DeferWaveformData || opts.BulkDataDescriptor != nil {
			return nil, nil, errDeflatedDeferredValue()
		}
		inflated, wrapped, err := inflatedDataSet(r, opts.AllowZlibDeflate)
		if err != nil {
			return nil, nil, err
		}
		defer func() { _ = inflated.Close() }()
		readerSource = inflated
		streamValues = false
		zlibWrapped = wrapped
	}
	readerOpts := opts.datasetParserReaderOptions(baseOffset, streamValues)
	reader := parser.NewReader(readerSource, syntax, readerOpts)
	dataset, err := readDataSetProjection(reader, opts)
	if err != nil && (!opts.AllowIncompletePixelData || !errors.Is(err, parser.ErrIncompletePixelData)) {
		return nil, reader, err
	}
	if err != nil {
		if _, certified := reader.IncompletePixelData(); !certified {
			return nil, reader, err
		}
	}
	if !syntax.ExplicitVR {
		dataset = resolveImplicitContextualVRs(dataset, readerOpts.Dictionary, syntax.ByteOrder)
	}
	valueOrder := syntax.ByteOrder
	if isLegacy {
		dataset = normalizeLegacyDataSet(dataset, legacy)
		valueOrder = binary.LittleEndian
	}
	obj := fromParsedDataSetWithTextOptions(dataset, readerOpts.Dictionary, opts.TextOptions)
	obj.SetValueByteOrder(valueOrder)
	obj.attachIncompletePixelData(reader)
	obj.privateDiagnostics, obj.privateDiagnosticsTruncated = reader.PrivateDiagnostics()
	obj.boundaryRecoveries, obj.boundaryRecoveriesTruncated = reader.BoundaryRecoveries()
	if zlibWrapped {
		obj.boundaryRecoveries = append([]parser.BoundaryRecovery{zlibDeflateRecovery(baseOffset)}, obj.boundaryRecoveries...)
	}
	if readerOptionsNeedValueProvider(readerOpts) && streamValues && obj.deferredCount > 0 {
		provider := newReaderValueProvider(reader, obj)
		if isLegacy {
			provider.swapWidth = legacySwapWidths(dataset, legacy)
		}
		obj.setValueProvider(provider)
	}
	if info, incomplete := obj.IncompletePixelData(); incomplete {
		return obj, reader, &PartialReadError{Read: *incompletePartial(info, len(dataset.Elements))}
	}
	return obj, reader, nil
}

func OpenDataSet(path string, syntax transfer.Syntax) (*Object, error) {
	return OpenDataSetWithOptions(path, syntax, ReadFileOptions{})
}

func OpenDataSetWithOptions(path string, syntax transfer.Syntax, opts ReadFileOptions) (result *Object, err error) {
	if err := projectionReadOptions(opts); err != nil {
		return nil, err
	}
	if err := incompleteReadOptions(opts); err != nil {
		return nil, err
	}
	if err := rejectPrefixOptions(opts); err != nil {
		return nil, err
	}
	finishFrames := finalizeFrameSink(&opts)
	defer finishFrames(&err)
	f, err := OpenSourceFile(path)
	if err != nil {
		return nil, err
	}
	closeFile := true
	defer func() {
		if !closeFile {
			return
		}
		if cerr := f.Close(); cerr != nil {
			if err == nil {
				err = cerr
			} else {
				err = errors.Join(err, cerr)
			}
		}
	}()
	result, err = ReadDataSetWithOptions(f, syntax, opts)
	if (err == nil || errors.Is(err, ErrPartialRead)) && result != nil && result.valueProvider != nil && result.deferredCount > 0 {
		result.source = f
		closeFile = false
	}
	return result, err
}

func WriteFile(w io.Writer, file *File) error {
	return WriteFileWithOptions(w, file, WriteFileOptions{})
}

// RebuildFileMeta resynchronizes the file-meta object with the current dataset
// and transfer syntax without copying the dataset or reparsing the whole file.
// It applies FileMetaUpdate.
func (f *File) RebuildFileMeta() error {
	return f.RebuildFileMetaWithOptions(WriteFileOptions{})
}

// RebuildFileMetaWithOptions is RebuildFileMeta with the MetaMode and
// AETitles of opts.
func (f *File) RebuildFileMetaWithOptions(opts WriteFileOptions) error {
	if f == nil {
		return ErrNilFile
	}
	syntax, err := resolveWriteTransferSyntax(f)
	if err != nil {
		return err
	}
	meta, err := buildFileMetaElements(f, syntax, fileMetaOptions{mode: opts.MetaMode, aeTitles: opts.AETitles})
	if err != nil {
		return err
	}
	f.Meta = FromElements(meta, std.Dictionary)
	f.TransferSyntax = syntax
	return nil
}

func WriteFileWithOptions(w io.Writer, file *File, opts WriteFileOptions) error {
	if err := parser.ValidateSequenceWriteLimits(opts.SequenceLimits); err != nil {
		return err
	}
	if file != nil && file.Dataset != nil && file.Dataset.HasDiscardedValues() {
		return core.ErrDiscardedValue
	}
	if file == nil {
		return fmt.Errorf("dicom: nil file")
	}
	if file.incompletePixelDataRead != nil || file.partialRead != nil && !opts.AllowPartial {
		return ErrPartialFile
	}
	if _, incomplete := file.Dataset.IncompletePixelData(); incomplete {
		return ErrPartialFile
	}
	if opts.Preamble != nil && len(opts.Preamble) != part10PreambleLength {
		return ErrInvalidPreambleLength
	}

	syntax, err := resolveWriteTransferSyntaxWithOptions(file, opts)
	if err != nil {
		return err
	}
	level, err := serializationLevel(syntax, opts.DeflateLevel, opts.Padding)
	if err != nil {
		return err
	}
	meta, err := prepareFileMeta(file, syntax, fileMetaOptions{mode: opts.MetaMode, aeTitles: opts.AETitles})
	if err != nil {
		return err
	}
	dataset, err := prepareWritableDataSet(file, meta.elements, opts.OmitReconciledDataSetSOPUIDs)
	if err != nil {
		return err
	}

	preamble := opts.Preamble
	if preamble == nil {
		switch len(file.Preamble) {
		case 0:
			preamble = make([]byte, part10PreambleLength)
		case part10PreambleLength:
			preamble = file.Preamble
		default:
			return ErrInvalidPreambleLength
		}
	}
	buffered := bufio.NewWriter(w)
	if err := writeAll(buffered, preamble); err != nil {
		return err
	}
	if err := writeAll(buffered, []byte(part10Magic)); err != nil {
		return err
	}

	if err := writeAll(buffered, meta.encoded); err != nil {
		return err
	}

	if err := writeDataSetWithSerializationOptions(buffered, dataset, syntax, parser.WriterOptions{BulkDataResolver: opts.BulkDataResolver, BulkDataSize: opts.BulkDataSize, SequenceLimits: opts.SequenceLimits, LengthPolicy: opts.SequenceLength, GroupLength: opts.GroupLength, Padding: opts.Padding}, level, int64(part10PreambleLength+len(part10Magic)+len(meta.encoded)), opts.KeepPartialByteOrderUnits); err != nil {
		return err
	}
	return buffered.Flush()
}

func WriteDataSet(w io.Writer, obj *Object, syntax transfer.Syntax) (err error) {
	if _, incomplete := obj.IncompletePixelData(); incomplete {
		return ErrPartialFile
	}
	buffered := bufio.NewWriter(w)
	if err := writeDataSet(buffered, obj, syntax); err != nil {
		return err
	}
	return buffered.Flush()
}

// WriteDataSetWithOptions writes a data set without File Meta, with explicit
// sequence/group length, raw dataset Deflate level and padding policies.
func WriteDataSetWithOptions(w io.Writer, obj *Object, syntax transfer.Syntax, opts WriteDataSetOptions) error {
	if _, incomplete := obj.IncompletePixelData(); incomplete {
		return ErrPartialFile
	}
	if err := parser.ValidateSequenceWriteLimits(opts.SequenceLimits); err != nil {
		return err
	}
	level, err := serializationLevel(syntax, opts.DeflateLevel, opts.Padding)
	if err != nil {
		return err
	}
	buffered := bufio.NewWriter(w)
	if err := writeDataSetWithSerializationOptions(buffered, obj, syntax, parser.WriterOptions{BulkDataResolver: opts.BulkDataResolver, BulkDataSize: opts.BulkDataSize, SequenceLimits: opts.SequenceLimits, LengthPolicy: opts.SequenceLength, GroupLength: opts.GroupLength, Padding: opts.Padding}, level, 0, false); err != nil {
		return err
	}
	return buffered.Flush()
}

func writeDataSet(w io.Writer, obj *Object, syntax transfer.Syntax) (err error) {
	return writeDataSetWithOptions(w, obj, syntax, parser.WriterOptions{})
}

func writeDataSetWithOptions(w io.Writer, obj *Object, syntax transfer.Syntax, writerOpts parser.WriterOptions) (err error) {
	level, err := serializationLevel(syntax, nil, writerOpts.Padding)
	if err != nil {
		return err
	}
	return writeDataSetWithSerializationOptions(w, obj, syntax, writerOpts, level, 0, false)
}

// writeDataSetWithSerializationOptions writes obj in syntax. keepPartial
// writes the trailing partial unit of a value reversed to the other byte
// order unchanged instead of refusing the value
// (WriteFileOptions.KeepPartialByteOrderUnits).
func writeDataSetWithSerializationOptions(w io.Writer, obj *Object, syntax transfer.Syntax, writerOpts parser.WriterOptions, level int, prefix int64, keepPartial bool) (err error) {
	if obj == nil {
		return fmt.Errorf("dicom: nil object passed to WriteDataSet")
	}
	if obj.HasDiscardedValues() {
		return core.ErrDiscardedValue
	}
	characterSet, err := obj.characterSetForVR(core.VRLO)
	if err != nil {
		return fmt.Errorf("dicom: resolve dataset Specific Character Set for writing: %w", err)
	}
	if writerOpts.DeferredValue == nil {
		writerOpts.DeferredValue = obj.nestedDeferredValue
	}
	if writerOpts.DeferredValueLength == nil {
		writerOpts.DeferredValueLength = obj.nestedDeferredLength
	}
	syntax, err = validateWritableSyntax(syntax)
	if err != nil {
		return err
	}
	if err := obj.CheckPixelDataEncoding(syntax); err != nil {
		return err
	}
	original := obj.SortedElements()
	sourceOrder := obj.ValueByteOrder()
	reverse := valueOrderDiffers(sourceOrder, syntax.ByteOrder)
	sampleWidth := 0
	if reverse {
		// Raw values are held in the order of the syntax the object came
		// from; the writer encodes them in the destination order.
		sampleWidth = pixelDataSampleWidth(original, sourceOrder)
		if original, err = reverseElementsByteOrder(original, sourceOrder, keepPartial); err != nil {
			return err
		}
		writerOpts.DeferredValue = reversingDeferredValue(writerOpts.DeferredValue, syntax.ByteOrder, keepPartial)
	}
	if syntax.Deflated {
		deflater, err := newPaddedDeflaterWithLevel(w, level)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := deflater.Close(); cerr != nil {
				if err == nil {
					err = cerr
				} else {
					err = errors.Join(err, cerr)
				}
			}
		}()
		w = deflater
	}
	count := &serializationCounter{destination: w}
	w = count
	writerOpts.CharacterSet = characterSet
	writer := parser.NewWriterWithOptions(w, syntax, writerOpts)
	elements, err := writer.PrepareDataSetElements(original, func(el core.Element) (int64, error) {
		n, err := obj.deferredEncodedLength(el)
		if err != nil {
			return 0, err
		}
		header := int64(8)
		if syntax.ExplicitVR && (el.VR() == "" || el.VR().UsesLongExplicitLength()) {
			header = 12
		}
		if n > math.MaxInt64-header {
			return 0, fmt.Errorf("dicom: deferred encoded length overflow")
		}
		return header + n, nil
	})
	if err != nil {
		return err
	}
	for _, el := range elements {
		if el.Value == nil {
			if err := writeDeferredElement(writer, obj, el, reverse, sampleWidth, keepPartial); err != nil {
				return err
			}
			continue
		}
		if err := writer.WriteElement(el); err != nil {
			return err
		}
	}
	return writer.WriteDataSetPadding(count.n, prefix, original)
}

func hasDeferredSequenceValue(obj *Object) bool {
	if obj == nil {
		return false
	}
	provider, ok := obj.valueProvider.(*readerValueProvider)
	if !ok || provider == nil || provider.reader == nil {
		return false
	}
	return hasNestedNilValue(obj.Elements(), false)
}

func hasNestedNilValue(elements []core.Element, nested bool) bool {
	for _, element := range elements {
		if nested && isNestedDeferred(element) {
			return true
		}
		sequence, ok := element.Value.(core.SequenceValue)
		if !ok {
			continue
		}
		for _, item := range sequence.Items {
			if hasNestedNilValue(item.Elements, true) {
				return true
			}
		}
	}
	return false
}

func (f *File) objectForTag(tag core.Tag) *Object {
	if f == nil {
		return nil
	}
	if tag.Group == 0x0002 {
		return f.Meta
	}
	return f.Dataset
}

func (o ReadFileOptions) parserReaderOptions(baseOffset int64) parser.ReaderOptions {
	dict := std.Dictionary
	if o.Dictionary != nil {
		dict = o.Dictionary
	}
	return parser.ReaderOptions{
		AllowIncompletePixelData:     o.AllowIncompletePixelData,
		Dictionary:                   dict,
		MaxPrivateCreators:           o.MaxPrivateCreators,
		MaxPrivateDiagnostics:        o.MaxPrivateDiagnostics,
		RejectInvalidPrivateCreators: o.RejectInvalidPrivateCreators,
		MaxElementBytes:              o.MaxElementBytes,
		MaxPixelDataBytes:            o.MaxPixelDataBytes,
		MaxDeformableVectorGridBytes: o.MaxDeformableVectorGridBytes,
		MaxTotalBytes:                o.MaxTotalBytes,
		BaseOffset:                   baseOffset,
		MaxSequenceDepth:             o.MaxSequenceDepth,
		MaxElements:                  o.MaxElements,
		MaxFragments:                 o.MaxFragments,
		InlineValueBytesThreshold:    o.InlineValueBytesThreshold,
		DeferNestedValues:            o.DeferNestedValues,
		DeferValueTags:               o.DeferValueTags,
		BulkDataDescriptor:           o.BulkDataDescriptor,
		StrictReservedBytes:          o.StrictReservedBytes,
		OddLengthPolicy:              o.OddLengthPolicy,
		SkipPixelData:                o.SkipPixelData,
		SeekUnselectedSequences:      o.SeekUnselectedSequences,
		DeferPixelData:               o.DeferPixelData,
		DeferWaveformData:            o.DeferWaveformData,
		FrameSink:                    o.FrameSink,
		EncapsulatedSink:             o.EncapsulatedSink,
		RecoverContainerOverruns:     o.RecoverContainerOverruns,
		MaxBoundaryRecoveries:        o.MaxBoundaryRecoveries,
		RecoverImplicitVRElements:    o.RecoverImplicitVRElements,
		MaxImplicitVRElements:        o.MaxImplicitVRElements,
		RecoverDelimiterAnomalies:    o.RecoverDelimiterAnomalies,
		VRRecovery:                   o.VRRecovery,
		SequenceRecovery:             o.SequenceRecovery,
		MaxDelimiterRecoveries:       o.MaxDelimiterRecoveries,
	}
}

func (o ReadFileOptions) datasetParserReaderOptions(baseOffset int64, streamValues bool) parser.ReaderOptions {
	opts := o.parserReaderOptions(baseOffset)
	if !streamValues {
		opts.InlineValueBytesThreshold = 0
	}
	return opts
}

type preparedFileMeta struct {
	elements []core.Element
	encoded  []byte
}

func prepareFileMeta(file *File, syntax transfer.Syntax, opts fileMetaOptions) (preparedFileMeta, error) {
	elements, err := buildFileMetaElements(file, syntax, opts)
	if err != nil {
		return preparedFileMeta{}, err
	}
	var encoded bytes.Buffer
	metaWriter := parser.NewWriter(&encoded, transfer.ExplicitVRLittleEndian)
	for _, el := range elements {
		if err := metaWriter.WriteElement(el); err != nil {
			return preparedFileMeta{}, err
		}
	}
	return preparedFileMeta{elements: elements, encoded: encoded.Bytes()}, nil
}

func buildFileMetaElements(file *File, syntax transfer.Syntax, opts fileMetaOptions) ([]core.Element, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	metaByTag := make(map[core.Tag]core.Element)
	if file != nil && file.Meta != nil {
		for _, el := range file.Meta.Elements() {
			if el.Tag().Group != 0x0002 {
				continue
			}
			metaByTag[el.Tag()] = el
		}
	}

	sopClassUID, sopClassRead, err := resolveFileMetaUID(file, metaByTag, tagSOPClassUID, tagMediaStorageSOPClassUID, ErrMissingSOPClassUID)
	if err != nil {
		return nil, err
	}
	sopInstanceUID, sopInstanceRead, err := resolveFileMetaUID(file, metaByTag, tagSOPInstanceUID, tagMediaStorageSOPInstanceUID, ErrMissingSOPInstanceUID)
	if err != nil {
		return nil, err
	}

	if opts.mode == FileMetaCreate {
		metaByTag = make(map[core.Tag]core.Element)
	}
	metaByTag[tagMediaStorageSOPClassUID] = derivedUIDElement(tagMediaStorageSOPClassUID, sopClassUID, sopClassRead)
	metaByTag[tagMediaStorageSOPInstanceUID] = derivedUIDElement(tagMediaStorageSOPInstanceUID, sopInstanceUID, sopInstanceRead)
	metaByTag[tagTransferSyntaxUID] = newStringElement(tagTransferSyntaxUID, core.VRUI, syntax.UID)

	// The writer's identity, unless FileMetaPreserve keeps another one.
	if existing := normalizedElementString(metaByTag[tagImplementationClassUID]); opts.mode == FileMetaPreserve && existing != "" {
		metaByTag[tagImplementationClassUID] = derivedUIDElement(tagImplementationClassUID, existing, !isAuthoredText(metaByTag[tagImplementationClassUID]))
	} else {
		metaByTag[tagImplementationClassUID] = derivedUIDElement(tagImplementationClassUID, ImplementationClassUID, false)
		metaByTag[tagImplementationVersionName] = newStringElement(tagImplementationVersionName, core.VRSH, ImplementationVersionName)
	}
	for _, title := range []struct {
		tag   core.Tag
		value string
	}{{tagSourceAETitle, opts.aeTitles.Source}, {tagSendingAETitle, opts.aeTitles.Sending}, {tagReceivingAETitle, opts.aeTitles.Receiving}} {
		if title.value != "" {
			metaByTag[title.tag] = newStringElement(title.tag, core.VRAE, strings.TrimSpace(title.value))
		}
	}

	if _, ok := metaByTag[tagFileMetaInformationVersion]; !ok {
		metaByTag[tagFileMetaInformationVersion] = core.NewRawElement(tagFileMetaInformationVersion, core.VROB, []byte{0x00, 0x01})
	}

	meta := make([]core.Element, 0, len(metaByTag))
	for tag, el := range metaByTag {
		if tag == tagFileMetaInformationGroupLength {
			continue
		}
		meta = append(meta, el)
	}
	sortElementsByTag(meta)

	var groupLength uint64
	for _, el := range meta {
		valueLength, ok := el.CalculatedLength()
		if !ok || valueLength.IsUndefined() {
			return nil, fmt.Errorf("dicom: file meta element %s has no defined length", el.Tag())
		}
		headerLength := uint64(8)
		if el.VR().UsesLongExplicitLength() {
			headerLength = 12
		} else if uint64(valueLength) > uint64(^uint16(0)) {
			return nil, fmt.Errorf("dicom: file meta element %s length %d exceeds uint16", el.Tag(), valueLength)
		}
		groupLength += headerLength + uint64(valueLength)
	}
	if groupLength > uint64(^uint32(0)) {
		return nil, fmt.Errorf("dicom: file meta group length %d exceeds uint32", groupLength)
	}

	groupLengthElement := ulElementLittleEndian(tagFileMetaInformationGroupLength, uint32(groupLength))
	elements := make([]core.Element, len(meta)+1)
	elements[0] = groupLengthElement
	copy(elements[1:], meta)

	return elements, nil
}

func prepareWritableDataSet(file *File, meta []core.Element, omitReconciledSOPUIDs bool) (*Object, error) {
	dict := std.Dictionary
	if file != nil && file.Dataset != nil && file.Dataset.dict != nil {
		dict = file.Dataset.dict
	}

	dataset := New(dict)
	if file != nil && file.Dataset != nil {
		dataset = cloneObjectForWrite(file.Dataset, dict)
	}

	metaObject := FromElements(meta, dict)
	if omitReconciledSOPUIDs {
		mediaClass, ok := metaObject.GetUID(tagMediaStorageSOPClassUID)
		if !ok || transfer.NormalizeUID(mediaClass) != mediaStorageDirectoryStorageUID {
			return nil, fmt.Errorf("dicom: omission of reconciled dataset SOP UIDs requires Media Storage Directory Storage")
		}
		return dataset, nil
	}

	injected := false
	for _, pair := range []struct {
		datasetTag core.Tag
		metaTag    core.Tag
	}{
		{datasetTag: tagSOPClassUID, metaTag: tagMediaStorageSOPClassUID},
		{datasetTag: tagSOPInstanceUID, metaTag: tagMediaStorageSOPInstanceUID},
	} {
		if value, ok := dataset.GetString(pair.datasetTag); ok && transfer.NormalizeUID(value) != "" {
			continue
		}
		value, ok := metaObject.GetString(pair.metaTag)
		if !ok || transfer.NormalizeUID(value) == "" {
			return nil, fmt.Errorf("dicom: missing reconciled dataset value for %s", pair.datasetTag)
		}
		dataset.Put(newStringElement(pair.datasetTag, core.VRUI, transfer.NormalizeUID(value)))
		injected = true
	}

	if injected {
		sorted := FromElementsWithTextOptions(dataset.SortedElements(), dict, dataset.text)
		sorted.SetValueByteOrder(dataset.ValueByteOrder())
		sorted.valueProvider = dataset.valueProvider
		return sorted, nil
	}
	return dataset, nil
}

func cloneObjectForWrite(source *Object, dict dictionary.DataDictionary) *Object {
	if source == nil {
		return New(dict)
	}
	clone := FromElementsWithTextOptions(source.Elements(), dict, source.text)
	clone.SetValueByteOrder(source.ValueByteOrder())
	clone.valueProvider = source.valueProvider
	return clone
}

// writeDeferredElement streams the deferred value of el from obj. reverse
// reverses it by unit (sampleWidth is the Pixel Data sample width of obj)
// when obj holds values in the other byte order than the writer, unless the
// provider already produces that value in the writer's order (FrameSource
// Pixel Data). keepPartial is the partial-unit rule of reversingCopy.
func writeDeferredElement(writer *parser.Writer, obj *Object, el core.Element, reverse bool, sampleWidth int, keepPartial bool) error {
	if obj == nil || obj.valueProvider == nil {
		return fmt.Errorf("dicom: element %s has deferred value but no value provider", el.Tag())
	}
	copyValue := func(w io.Writer) (int64, error) {
		return obj.CopyValueTo(el.Tag(), w)
	}
	if reverse && deferredValueNeedsReversal(obj.valueProvider, el.Tag()) {
		copyValue = reversingCopy(el, sampleWidth, keepPartial, copyValue)
	}
	return writer.WriteDeferredElement(el, copyValue)
}

func readerOptionsNeedValueProvider(opts parser.ReaderOptions) bool {
	return opts.InlineValueBytesThreshold > 0 || opts.DeferPixelData || opts.DeferWaveformData || opts.BulkDataDescriptor != nil || opts.FrameSink != nil
}

func errDeflatedDeferredValue() error {
	return fmt.Errorf("dicom: deflated datasets cannot preserve encoded value offsets for deferred value reading: %w", ErrDeferredValueRequiresSeekable)
}

type readStageError struct {
	stage error
	err   error
}

func (e *readStageError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s: %v", e.stage, e.err)
}

func (e *readStageError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return []error{e.stage, e.err}
}

func wrapFileMetaError(err error) error {
	if err == nil || errors.Is(err, ErrFileMeta) {
		return err
	}
	if errors.Is(err, ErrMissingTransferSyntax) ||
		errors.Is(err, transfer.ErrUnknownTransferSyntax) ||
		errors.Is(err, transfer.ErrUnsupportedTransferSyntax) {
		return err
	}
	return &readStageError{stage: ErrFileMeta, err: err}
}

func wrapDataSetError(err error) error {
	if err == nil || errors.Is(err, ErrDataSet) {
		return err
	}
	return &readStageError{stage: ErrDataSet, err: err}
}

func validateReadableSyntax(syntax transfer.Syntax) (transfer.Syntax, error) {
	syntax, known, err := canonicalizeTransferSyntax(syntax)
	if err != nil {
		return transfer.Syntax{}, err
	}
	if syntax.Supported || !known {
		return syntax, nil
	}
	return transfer.Syntax{}, unsupportedTransferSyntaxError("reader", syntax)
}

func validateWritableSyntax(syntax transfer.Syntax) (transfer.Syntax, error) {
	syntax, known, err := canonicalizeTransferSyntax(syntax)
	if err != nil {
		return transfer.Syntax{}, err
	}
	if transfer.NormalizeUID(syntax.UID) == "" {
		return transfer.Syntax{}, ErrMissingTransferSyntax
	}
	if syntax.Supported || !known {
		return syntax, nil
	}
	return transfer.Syntax{}, unsupportedTransferSyntaxError("writer", syntax)
}

func resolveWriteTransferSyntax(file *File) (transfer.Syntax, error) {
	return resolveWriteTransferSyntaxWithOptions(file, WriteFileOptions{})
}

func resolveWriteTransferSyntaxWithOptions(file *File, opts WriteFileOptions) (transfer.Syntax, error) {
	if file != nil {
		if uid := transfer.NormalizeUID(file.TransferSyntax.UID); uid != "" {
			syntax := file.TransferSyntax
			syntax.UID = uid
			return validateWritableSyntax(syntax)
		}
	}

	var uid string
	if file != nil && file.Meta != nil {
		if value, ok := file.Meta.GetString(tagTransferSyntaxUID); ok {
			uid = transfer.NormalizeUID(value)
		}
	}
	if uid == "" {
		if override := transfer.NormalizeUID(opts.OverrideMissingTransferSyntax); override != "" {
			return validateWritableSyntax(transfer.Syntax{UID: override})
		}
		if opts.OverrideMissingTransferSyntax != "" {
			return transfer.Syntax{}, fmt.Errorf("%w: empty OverrideMissingTransferSyntax", ErrMissingTransferSyntax)
		}
		if opts.DefaultMissingTransferSyntax {
			return transfer.ImplicitVRLittleEndian, nil
		}
		return transfer.Syntax{}, ErrMissingTransferSyntax
	}
	return validateWritableSyntax(transfer.Syntax{UID: uid})
}

// resolveFileMetaUID returns the UID for a derived File Meta element and
// whether it came from a value read from a stream rather than authored.
func resolveFileMetaUID(file *File, metaByTag map[core.Tag]core.Element, datasetTag, metaTag core.Tag, missing error) (string, bool, error) {
	if file != nil && file.Dataset != nil {
		if value, ok := file.Dataset.GetString(datasetTag); ok {
			if uid := transfer.NormalizeUID(value); uid != "" {
				element, _ := file.Dataset.Get(datasetTag)
				return uid, !isAuthoredText(element), nil
			}
		}
	}
	if value := normalizedElementString(metaByTag[metaTag]); value != "" {
		return value, !isAuthoredText(metaByTag[metaTag]), nil
	}
	return "", false, missing
}

func isAuthoredText(el core.Element) bool {
	_, ok := el.Value.(core.StringValue)
	return ok
}

// derivedUIDElement copies a UID into File Meta. A UID that was read keeps
// its read-path leniency as raw, NUL-padded bytes, so a legacy file with a
// non-conformant UID can still be rewritten; an authored UID stays text and
// is validated by the writer (PS3.5 9.1).
func derivedUIDElement(tag core.Tag, uid string, read bool) core.Element {
	if !read {
		return newStringElement(tag, core.VRUI, uid)
	}
	raw := []byte(uid)
	if len(raw)%2 != 0 {
		raw = append(raw, 0)
	}
	return core.NewRawElement(tag, core.VRUI, raw)
}

func normalizedElementString(el core.Element) string {
	return transfer.NormalizeUID(el.StringValue())
}

func newStringElement(tag core.Tag, vr core.VR, value string) core.Element {
	return core.Element{
		Header: core.ElementHeader{Tag: tag, VR: vr},
		Value:  core.StringValue{value},
	}
}

func ulElementLittleEndian(tag core.Tag, value uint32) core.Element {
	var raw [4]byte
	binary.LittleEndian.PutUint32(raw[:], value)
	return core.NewRawElement(tag, core.VRUL, raw[:])
}

func sortElementsByTag(elements []core.Element) {
	sort.Slice(elements, func(i, j int) bool {
		if elements[i].Tag().Group != elements[j].Tag().Group {
			return elements[i].Tag().Group < elements[j].Tag().Group
		}
		return elements[i].Tag().Element < elements[j].Tag().Element
	})
}

func writeAll(w io.Writer, buf []byte) error {
	for len(buf) > 0 {
		n, err := w.Write(buf)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		buf = buf[n:]
	}
	return nil
}

func canonicalizeTransferSyntax(syntax transfer.Syntax) (transfer.Syntax, bool, error) {
	uid := transfer.NormalizeUID(syntax.UID)
	if uid != "" {
		if resolved, ok := transfer.DefaultRegistry.Get(uid); ok {
			return resolved, true, nil
		}
		syntax.UID = uid
		if syntax.Deflated || !syntaxHasNativeEncodingHints(syntax) {
			return transfer.Syntax{}, false, fmt.Errorf("%w: %q", transfer.ErrUnknownTransferSyntax, uid)
		}
	}
	if syntax.ByteOrder == nil {
		syntax.ByteOrder = binary.LittleEndian
	}
	return syntax, false, nil
}

// syntaxHasNativeEncodingHints requires unknown transfer.Syntax values to carry
// enough native encoding information to proceed locally. ExplicitVR=true may
// omit ByteOrder because this implementation defaults explicit-VR syntaxes to
// LittleEndian, and the only explicit-VR BigEndian UID is retired. When
// ExplicitVR=false, callers must provide ByteOrder because an unknown Syntax
// UID does not let the reader infer endianness; ImplicitVR with ByteOrder==nil
// is rejected.
func syntaxHasNativeEncodingHints(syntax transfer.Syntax) bool {
	return syntax.ExplicitVR || syntax.ByteOrder != nil
}

func unsupportedTransferSyntaxError(operation string, syntax transfer.Syntax) error {
	if syntax.Name != "" {
		return fmt.Errorf("%w: transfer syntax %q (%s) is registered but not supported by this %s", transfer.ErrUnsupportedTransferSyntax, syntax.UID, syntax.Name, operation)
	}
	return fmt.Errorf("%w: transfer syntax %q is registered but not supported by this %s", transfer.ErrUnsupportedTransferSyntax, syntax.UID, operation)
}

// byteCounter counts written bytes and discards them.
type byteCounter struct{ n int64 }

func (c *byteCounter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}
