package parser

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var ErrUnsupportedUndefinedLength = errors.New("dicom: undefined length values are not supported by this parser")
var ErrNonZeroReservedBytes = errors.New("dicom: explicit VR long-form reserved bytes must be zero")
var ErrUnexpectedSequenceControlTag = errors.New("dicom: unexpected sequence control tag")
var ErrUnexpectedDelimiterLength = errors.New("dicom: delimiter length must be zero")
var ErrMissingBasicOffsetTable = errors.New("dicom: encapsulated Pixel Data is missing the Basic Offset Table item")

var (
	tagFloatPixelData       = core.NewTag(0x7FE0, 0x0008)
	tagDoubleFloatPixelData = core.NewTag(0x7FE0, 0x0009)
	tagWaveformData         = core.NewTag(0x5400, 0x1010)
	tagVectorGridData       = core.NewTag(0x0064, 0x0009)
)

const (
	definedValueReadChunkSize   = 32 << 10
	smallValueArenaLimit        = 256
	smallValueArenaInitialBytes = 256
	smallValueArenaBlockBytes   = 4 << 10
)

type OddLengthPolicy uint8

const (
	RejectOddLength OddLengthPolicy = iota
	AcceptOddLength
)

type seqTokenType uint8

const (
	seqTokenTypeSequence seqTokenType = iota
	seqTokenTypePixelSequence
	seqTokenTypeItem
)

type seqToken struct {
	typ                    seqTokenType
	tag                    core.Tag // sequences: the element tag, for diagnostics
	items                  int      // sequences: items started so far
	length                 core.Length
	baseOffset             uint64
	implicitVRLittleEndian bool
	// explicitBigEndian marks an item opened by a byte-swapped Item tag
	// (RecoverDelimiterAnomalies): its content is Explicit VR Big Endian.
	explicitBigEndian bool
	privateScope      *dictionary.PrivateReservations
	// definedIndex is the stack index of the innermost frame at or below this
	// one with a defined length, and itemIndex that of the innermost item
	// frame; -1 when there is none. They are fixed when the frame is pushed
	// so the enclosing container and item are found in O(1) per element
	// header however deep the nesting is.
	definedIndex int
	itemIndex    int
}

const (
	// DefaultMaxSequenceDepth is the combined sequence and item nesting depth
	// a zero ReaderOptions.MaxSequenceDepth allows: 64 nested sequences with
	// their items, the level DCMTK also enforces by default. Real data sets
	// stay far below it; deeper input is refused with ErrMaxDepthExceeded.
	DefaultMaxSequenceDepth = 128
	// UnlimitedSequenceDepth disables the nesting limit when set as
	// ReaderOptions.MaxSequenceDepth. Parsing stays linear in the depth, but
	// callers reading untrusted input should keep a finite limit.
	UnlimitedSequenceDepth = -1
)

type ReaderOptions struct {
	// AllowIncompletePixelData retains terminal root integer Pixel Data at
	// physical EOF for inspection under finite limits. ReadDataSet and ReadAll
	// return retained data plus IncompletePixelDataError; low-level token users
	// must inspect Reader.IncompletePixelData. Selective/validated/sink/deferred
	// pixel modes and other grammar repairs are incompatible. Zero is strict.
	AllowIncompletePixelData bool
	// BulkDataDescriptor selects additional source-backed primitive values.
	BulkDataDescriptor *BulkDataDescriptor
	Dictionary         dictionary.DataDictionary
	// Creator resolution is enabled only when Dictionary includes a PrivateDataDictionary.
	// Limits default to 4096 reservations per dataset/item and 128 diagnostics.
	MaxPrivateCreators           int
	MaxPrivateDiagnostics        int
	RejectInvalidPrivateCreators bool
	// MaxElementBytes limits a single defined-length element value allocation in
	// bytes. Use it to reject suspiciously large values before allocating
	// memory. A zero value means unlimited.
	MaxElementBytes int64
	// InlineValueBytesThreshold controls when the reader will materialize a
	// defined-length primitive value into memory.
	//
	// If InlineValueBytesThreshold is > 0 and an OB, OW, OF, OD or UN element
	// has a defined length strictly greater than this threshold, the reader
	// will not allocate a value buffer and will instead return an element token
	// with a nil Value. Values of other VRs (OL, OV, SV, UV, UC or text, for
	// instance) are always materialized, subject to MaxElementBytes.
	//
	// A zero value preserves the historical behavior of always materializing
	// defined-length primitive values (subject to MaxElementBytes).
	InlineValueBytesThreshold int64
	// DeferNestedValues extends InlineValueBytesThreshold into sequence items.
	// A nested OB, OW, OF, OD or UN value longer than the threshold is skipped
	// like a top-level one, and its ValueLocation carries the enclosing item
	// offset; nested values of other VRs stay materialized. Sequences
	// themselves are always materialized.
	DeferNestedValues bool
	// DeferValueTags, when non-empty, limits InlineValueBytesThreshold to
	// these tags: values of other tags are materialized whatever their length.
	DeferValueTags []core.Tag
	// MaxPixelDataBytes limits native Pixel Data values and the cumulative bytes
	// of an encapsulated Pixel Data value, including its Basic Offset Table. When
	// set, this limit replaces MaxElementBytes for Pixel Data value tags. A zero
	// value leaves Pixel Data subject to MaxElementBytes.
	MaxPixelDataBytes int64
	// MaxDeformableVectorGridBytes limits each Vector Grid Data (0064,0009)
	// value before allocation, including values nested in sequences. A zero
	// value leaves the element subject to MaxElementBytes.
	MaxDeformableVectorGridBytes int64
	// MaxTotalBytes limits the total number of bytes consumed from the source.
	// Use it to bound parser work on untrusted input. A zero value means
	// unlimited.
	MaxTotalBytes int64
	BaseOffset    int64
	// MaxSequenceDepth limits the combined nesting depth of sequence and item
	// frames tracked in seqDelimiters. The count includes both sequence and
	// item starts (encapsulated Pixel Data counts as a sequence), so a limit
	// of 2n admits n nested sequences. Deeper input fails with
	// ErrMaxDepthExceeded. A zero value uses DefaultMaxSequenceDepth; a larger
	// value raises the limit and UnlimitedSequenceDepth (any negative value)
	// removes it. Parsing cost is linear in the depth either way.
	MaxSequenceDepth int
	// MaxElements limits the number of primitive elements returned by the
	// reader. Use it to bound work on very large or malformed data sets. A zero
	// value means unlimited.
	MaxElements int
	// MaxFragments limits encapsulated Pixel Data fragments, excluding the
	// Basic Offset Table. Use it to guard against pathological fragment counts.
	// A zero value means unlimited.
	MaxFragments        int
	StrictReservedBytes bool
	OddLengthPolicy     OddLengthPolicy
	// SkipPixelData consumes integer, Float, and Double Float Pixel Data payload
	// bytes without materializing them. The returned element keeps its header and
	// has a nil Value.
	SkipPixelData bool
	// SeekUnselectedSequences extends ReadDataSetProjected: an unselected root
	// sequence with a defined length is passed by one seek instead of parsing
	// its items, when the source is seekable with a known end and the sequence
	// ends inside the source and the MaxTotalBytes ceiling. Its header remains
	// with core.DiscardedValue. Undefined-length sequences, non-seekable and
	// deflated sources and lengths overrunning the source are parsed as without
	// the option, so their failures are unchanged. The content of a sequence
	// passed by seek is not parsed: its malformed items, element limits and
	// recovery diagnostics are not observed. Other reads ignore the option.
	SeekUnselectedSequences bool
	// DeferPixelData consumes top-level integer, Float, and Double Float Pixel
	// Data payload bytes without materializing them, while preserving enough
	// stream position information for object-level APIs to replay the original
	// value from a seekable source. Native Pixel Data nested in sequence items
	// (for example Icon Image Sequence) is materialized.
	DeferPixelData bool
	// DeferWaveformData consumes Waveform Data (5400,1010) payload bytes without
	// materializing them, including occurrences nested inside Waveform Sequence
	// items. Recorded ValueLocations preserve occurrence order and the nearest
	// enclosing item offset so multiplex groups can be bound without relying on
	// tag order alone. The source must be seekable for callers to replay them.
	DeferWaveformData bool
	// FrameSink receives native Pixel Data frames as they are read. When set,
	// native Pixel Data is split and emitted without materializing the complete
	// Pixel Data value.
	FrameSink FrameSink
	// EncapsulatedSink consumes top-level encapsulated Items for bounded assembly.
	// Without DeferPixelData the result contains core.DiscardedValue.
	EncapsulatedSink EncapsulatedSink
	// RecoverContainerOverruns repairs sequences and items whose declared
	// length runs past their enclosing defined-length sequence or item: they
	// are read only to the container end, an item header with no body left is
	// not recorded, and an undefined-length child still open at the container
	// end is closed there. Each repair is listed by BoundaryRecoveries. Values
	// crossing a container end are refused in every mode. Without the option
	// every such case is an ErrContainerOverrun error.
	RecoverContainerOverruns bool
	// MaxBoundaryRecoveries caps BoundaryRecoveries; 0 means
	// DefaultMaxBoundaryRecoveries.
	MaxBoundaryRecoveries int
	// RecoverImplicitVRElements reads an element of an Explicit VR Little
	// Endian data set whose two VR bytes are not a known VR as an Implicit VR
	// element: the VR comes from the dictionary and the four bytes after the
	// tag are its length. The switch is taken only when that length fits the
	// enclosing defined-length container and the source; never for Pixel
	// Data. Each switch is reported as a BoundaryImplicitVRElement recovery.
	// Recovery applies to that element only; subsequent headers remain Explicit
	// VR. A malformed mixed stream can return to Explicit VR immediately.
	RecoverImplicitVRElements bool
	// SequenceRecovery selects independent, reported repairs of elements
	// that should be sequences and of misplaced delimiters (see
	// SequenceRecoveryOptions). The zero value is strict.
	SequenceRecovery SequenceRecoveryOptions
	// VRRecovery selects independent, reported repairs of wrong VRs and
	// length fields (see VRRecoveryOptions). The zero value is strict.
	VRRecovery VRRecoveryOptions
	// RecoverDelimiterAnomalies repairs sequence framing written by broken
	// encoders, reporting each repair as a BoundaryRecovery:
	//   - swapped-item-tag: (FEFF,00E0), (FEFF,0DE0) and (FEFF,DDE0) inside
	//     a sequence or item are read as the Item, Item Delimitation and
	//     Sequence Delimitation tags written in the wrong byte order;
	//   - delimiter-length-ignored: an Item or Sequence Delimitation Item
	//     with a nonzero length is accepted with no value consumed;
	//   - orphan-delimiter-skipped: a delimiter with no open undefined-length
	//     item or sequence to close (for example after a defined-length
	//     sequence) is skipped;
	//   - item-group-length-skipped: a (FFFE,0000) group length element in a
	//     sequence is skipped.
	// At most MaxDelimiterRecoveries repairs are made in one read.
	RecoverDelimiterAnomalies bool
	// MaxDelimiterRecoveries caps RecoverDelimiterAnomalies repairs; 0 means
	// DefaultMaxDelimiterRecoveries. Exceeding it fails with
	// ErrMaxDelimiterRecoveries.
	MaxDelimiterRecoveries int
	// MaxImplicitVRElements caps the number of such switches in one read;
	// 0 means DefaultMaxImplicitVRElements. Exceeding it fails with
	// ErrMaxImplicitVRElements.
	MaxImplicitVRElements int
}

// SequenceRecoveryOptions are opt-in repairs in the style of DCMTK's
// dcmConvertUndefinedLengthOBOWtoSQ, dcmConvertVOILUTSequenceOWtoSQ and
// dcmReplaceWrongDelimitationItem, and of GDCM's NOSHADOWSEQ reading. Each
// applied repair is a BoundaryRecovery. Implicit VR private elements with an
// undefined length are always read as sequences (UN, PS3.5 6.2.2), which
// covers DCMTK's dcmReadImplPrivAttribMaxLengthAsSQ.
type SequenceRecoveryOptions struct {
	// UndefinedLengthOBOWAsSQ reads an OB or OW element other than Pixel Data
	// whose length is undefined as a sequence when its content starts with
	// an Item tag.
	UndefinedLengthOBOWAsSQ bool
	// VOILUTSequenceOWAsSQ reads VOI LUT Sequence (0028,3010) written with a
	// VR other than SQ as a sequence.
	VOILUTSequenceOWAsSQ bool
	// ReplaceWrongDelimiter reads a Sequence Delimitation Item found where an
	// undefined-length item must end as an Item Delimitation Item, and an Item
	// Delimitation Item found where an undefined-length sequence must end as
	// a Sequence Delimitation Item (DCMTK dcmReplaceWrongDelimitationItem).
	ReplaceWrongDelimiter bool
	// PrivateSequenceAsBytes checks a defined-length private SQ before
	// reading it; if its content does not parse, the element is kept as a UN
	// value with its original bytes instead of failing the read.
	PrivateSequenceAsBytes bool
}

// maxPrivateSequenceCheckBytes bounds the value PrivateSequenceAsBytes buffers.
const maxPrivateSequenceCheckBytes = 16 << 20

// VRRecoveryOptions are opt-in repairs of VR and length headers, in the style
// of DCMTK's dcmPreferVRFromDataDictionary and
// dcmPreferLengthFieldSizeFromDataDictionary and of GDCM's broken-writer
// workarounds. Each applied repair is a BoundaryRecovery carrying the
// declared and the used VR.
type VRRecoveryOptions struct {
	// MapOXToOW reads the non-standard VR "OX" on Pixel Data as OW.
	MapOXToOW bool
	// PreferDictionaryVR replaces a valid explicit VR of a public element
	// with its single dictionary VR when they differ. The length field size
	// still follows the VR written in the file unless
	// PreferDictionaryLengthFieldSize is also set.
	PreferDictionaryVR bool
	// PreferDictionaryLengthFieldSize reads the length field with the size
	// of the dictionary VR (2 or 4 bytes) when the written VR differs.
	PreferDictionaryLengthFieldSize bool
	// CorrectPrivateULLength reads a private UL element declared with value
	// length 6 as length 4 (Siemens Leonardo writers; the same workaround as
	// GDCM).
	CorrectPrivateULLength bool
	// ShortLengthUN reads a UN element whose reserved bytes are not zero as
	// a 16-bit length element, as GDCM 1.x wrote UN.
	ShortLengthUN bool
	// CorrectGETextLength reads a public text element in Implicit VR Little
	// Endian declared with length 13 as length 10 only when bounded lookahead
	// identifies an ordered, known public header at +10 and no known public
	// tag at +13. The ten value bytes must be ASCII text; Manufacturer and
	// Institution Name are excluded. The next defined, even-length value
	// must fit the source/container/byte budget. Without a known physical
	// source size it must also be fully present in the lookahead, even when
	// a container or byte budget is set. Up to four consecutive text values
	// with the same defect can be recognized within 82 bytes of lookahead;
	// every link must be unambiguous and reach a normal even-length header.
	// Every correction is reported as BoundaryGETextLength. Other odd
	// lengths remain unchanged.
	CorrectGETextLength bool
}

// DefaultMaxDelimiterRecoveries caps RecoverDelimiterAnomalies when
// MaxDelimiterRecoveries is 0.
const DefaultMaxDelimiterRecoveries = 4096

// DefaultMaxImplicitVRElements caps RecoverImplicitVRElements when
// MaxImplicitVRElements is 0.
const DefaultMaxImplicitVRElements = 4096

// ValueLocation identifies raw encoded value bytes in the reader's seekable
// source. Offsets use the same absolute coordinate space as Reader.Position.
type ValueLocation struct {
	Tag         core.Tag
	ValueOffset int64
	Length      int64
	// ItemOffset identifies the nearest enclosing sequence Item tag when the
	// value was nested in a sequence. It lets deferred callers bind repeated
	// tags to their parsed item instead of relying on global occurrence order.
	ItemOffset    int64
	ItemOffsetSet bool
}

type Reader struct {
	allowIncompletePixelData                    bool
	incompletePixelData                         *IncompletePixelData
	incompleteTerminal                          bool
	privateRoot                                 *dictionary.PrivateReservations
	privateInitError                            error
	maxPrivateCreators, maxPrivateDiagnostics   int
	rejectInvalidPrivateCreators                bool
	privateDiagnostics                          []PrivateDiagnostic
	privateDiagnosticsTruncated                 bool
	recoverContainerOverruns                    bool
	recoverImplicitVRElements                   bool
	recoverDelimiterAnomalies                   bool
	swappedItemPending                          bool
	vrRecovery                                  VRRecoveryOptions
	seqRecovery                                 SequenceRecoveryOptions
	maxDelimiterRecoveries, delimiterRecoveries int
	maxImplicitVRElements, implicitVRElements   int
	maxBoundaryRecoveries                       int
	boundaryRecoveries                          []BoundaryRecovery
	boundaryRecoveriesTruncated                 bool
	counter                                     *countingReader
	syntax                                      transfer.Syntax
	dec                                         dicomenc.BasicDecoder
	dict                                        dictionary.DataDictionary
	maxElementBytes                             int64
	maxPixelDataBytes                           int64
	maxVectorGridBytes                          int64
	inlineThreshold                             int64
	bulkDataRules                               []BulkDataRule
	valueReferences                             map[ValueLocation]ValueReference
	maxTotalBytes                               int64
	deflateLimitHasTail                         bool
	maxSequenceDepth                            int
	maxElements                                 int
	maxFragments                                int
	elementCount                                int
	fragmentCount                               int
	pixelDataBytes                              int64
	strictReservedBytes                         bool
	oddLengthPolicy                             OddLengthPolicy
	projection                                  *dataSetProjection
	skipPixelData                               bool
	seekUnselectedSequences                     bool
	deferPixelData                              bool
	deferWaveformData                           bool
	deferNestedValues                           bool
	deferValueTags                              map[core.Tag]struct{}
	frameSink                                   FrameSink
	frameSinkClosed                             bool
	encapsulatedSink                            EncapsulatedSink
	encapsulatedSinkClosed                      bool
	encodedPixelActive, encodedPixelSeen        bool
	// stopBeforePixelData is used only by the transfer syntax probe. It returns
	// the Pixel Data header as a terminal token without consuming or inspecting
	// payload bytes. Ordinary Reader APIs leave it false.
	stopBeforePixelData bool
	frameMetadata       frameMetadataState
	// seqDelimiters tracks the combined stack of open sequence and item frames
	// used for depth enforcement and defined-length boundary closure.
	seqDelimiters                   []seqToken
	delimiterCheckPending           bool
	pixelSequenceOffsetTablePending bool

	baseOffset int64
	rootOffset int64
	// sourceEnd is the stream-coordinate position of the physical end of a
	// seekable source, or 0 when the source size is unknown. A defined value
	// length that fits before sourceEnd cannot be a forged huge VL, so it is
	// safe to allocate in a single step.
	sourceEnd int64
	// slurpEligible marks a full-materialization pass over a sized source,
	// where the remaining dataset region may be read into a single buffer and
	// defined-length values returned as sub-slices of it. Like the small-value
	// arena, aliased values share backing memory and are treated as immutable;
	// the dataset buffer stays reachable while any aliased value is alive.
	slurpEligible bool

	valueLocations    map[core.Tag]ValueLocation
	allValueLocations map[core.Tag][]ValueLocation
	// valueLocationGeneration is zero during the initial ordered parse. An
	// explicit rewind advances it and enables seenValueLocations so replayed
	// locations are merged without charging the common first pass for hashing.
	valueLocationGeneration uint64
	seenValueLocations      map[ValueLocation]struct{}
	ambiguousValueLocations map[core.Tag]bool
	// smallValueArena is the current append-only block for primitive values up
	// to smallValueArenaLimit. Returned RawValue slices occupy disjoint regions;
	// a full block is abandoned, never overwritten, so earlier tokens remain
	// valid for their normal lifetime.
	smallValueArena []byte

	validationLifecycle  *readerValidationLifecycle
	selective            *selectiveReaderState
	validationSuppressed int
	lastReservedOffset   int64
	lastReservedNonZero  bool

	// headerInput only batches bytes belonging to the current header. It is
	// enabled for sources whose short-read/error behavior is known; generic
	// readers retain the original field-by-field I/O contract.
	headerInput        headerInput
	headerInputEnabled bool
	headerInputFile    bool
}

func NewReader(r io.Reader, syntax transfer.Syntax, opts ReaderOptions) *Reader {
	opts, incompleteInitError := incompleteReadOptions(opts, syntax)
	if opts.MaxSequenceDepth == 0 {
		opts.MaxSequenceDepth = DefaultMaxSequenceDepth
	}
	cr := &countingReader{r: r, pos: opts.BaseOffset, maxTotalBytes: opts.MaxTotalBytes, recordErrors: opts.AllowIncompletePixelData}
	rootOffset := int64(0)
	sourceEnd := int64(0)
	if seeker, ok := r.(io.Seeker); ok {
		if pos, err := seeker.Seek(0, io.SeekCurrent); err == nil {
			rootOffset = pos
			if size, ok := sourcePhysicalSize(r); ok && size >= pos {
				sourceEnd = opts.BaseOffset + (size - pos)
			}
		}
	}
	slurpEligible := sourceEnd > 0 &&
		opts.FrameSink == nil && opts.EncapsulatedSink == nil &&
		!opts.SkipPixelData &&
		!opts.DeferPixelData &&
		!opts.DeferWaveformData &&
		opts.InlineValueBytesThreshold == 0 && opts.BulkDataDescriptor == nil &&
		opts.MaxElementBytes == 0 &&
		opts.MaxPixelDataBytes == 0 &&
		opts.MaxDeformableVectorGridBytes == 0 &&
		opts.MaxTotalBytes == 0
	_, regularFile := r.(*os.File)
	nativeByteOrder := syntax.ByteOrder == nil || syntax.ByteOrder == binary.LittleEndian || syntax.ByteOrder == binary.BigEndian
	reader := &Reader{
		allowIncompletePixelData:     opts.AllowIncompletePixelData,
		maxPrivateCreators:           opts.MaxPrivateCreators,
		maxPrivateDiagnostics:        opts.MaxPrivateDiagnostics,
		rejectInvalidPrivateCreators: opts.RejectInvalidPrivateCreators,
		counter:                      cr,
		headerInputEnabled:           headerInputSource(r, sourceEnd),
		headerInputFile:              regularFile && nativeByteOrder && !opts.AllowIncompletePixelData && !opts.RecoverContainerOverruns && !syntax.Deflated,
		syntax:                       syntax,
		dec:                          dicomenc.NewBasicDecoder(syntax.ByteOrder),
		dict:                         opts.Dictionary,
		maxElementBytes:              opts.MaxElementBytes,
		maxPixelDataBytes:            opts.MaxPixelDataBytes,
		maxVectorGridBytes:           opts.MaxDeformableVectorGridBytes,
		inlineThreshold:              opts.InlineValueBytesThreshold,
		maxTotalBytes:                opts.MaxTotalBytes,
		maxSequenceDepth:             opts.MaxSequenceDepth,
		maxElements:                  opts.MaxElements,
		maxFragments:                 opts.MaxFragments,
		strictReservedBytes:          opts.StrictReservedBytes,
		oddLengthPolicy:              opts.OddLengthPolicy,
		skipPixelData:                opts.SkipPixelData,
		seekUnselectedSequences:      opts.SeekUnselectedSequences,
		deferPixelData:               opts.DeferPixelData,
		deferWaveformData:            opts.DeferWaveformData,
		deferNestedValues:            opts.DeferNestedValues,
		deferValueTags:               tagSet(opts.DeferValueTags),
		frameSink:                    opts.FrameSink,
		encapsulatedSink:             opts.EncapsulatedSink,
		recoverContainerOverruns:     opts.RecoverContainerOverruns,
		maxBoundaryRecoveries:        opts.MaxBoundaryRecoveries,
		recoverImplicitVRElements:    opts.RecoverImplicitVRElements,
		recoverDelimiterAnomalies:    opts.RecoverDelimiterAnomalies,
		vrRecovery:                   opts.VRRecovery,
		seqRecovery:                  opts.SequenceRecovery,
		maxDelimiterRecoveries:       opts.MaxDelimiterRecoveries,
		maxImplicitVRElements:        opts.MaxImplicitVRElements,
		seqDelimiters:                make([]seqToken, 0),
		baseOffset:                   opts.BaseOffset,
		rootOffset:                   rootOffset,
		sourceEnd:                    sourceEnd,
		slurpEligible:                slurpEligible,
	}
	reader.resetPrivateScopes()
	if opts.BulkDataDescriptor != nil {
		_, legacy := transfer.LegacySyntax(syntax.UID)
		var err error
		reader.bulkDataRules, err = compileBulkDataDescriptor(opts.BulkDataDescriptor)
		if err == nil {
			if _, ok := r.(io.ReadSeeker); !ok || syntax.Deflated || legacy || opts.FrameSink != nil || opts.EncapsulatedSink != nil {
				err = fmt.Errorf("%w: source must be seekable, standard non-deflated encoding and without frame sinks", ErrInvalidBulkDataDescriptor)
			}
		}
		if err != nil {
			reader.privateInitError = err
		}
	}
	reader.privateInitError = errors.Join(reader.privateInitError, incompleteInitError)
	return reader
}

// sliceSource serves the parser from an in-memory copy of the dataset region
// and can hand out zero-copy sub-slices for defined-length values.
type sliceSource struct {
	buf []byte
	off int
	// err is surfaced once buf is exhausted, preserving the original read
	// error of a source that ended early; nil means a clean io.EOF.
	err error
}

func (s *sliceSource) Read(p []byte) (int, error) {
	if s.off >= len(s.buf) {
		if s.err != nil {
			return 0, s.err
		}
		return 0, io.EOF
	}
	n := copy(p, s.buf[s.off:])
	s.off += n
	return n, nil
}

// take returns the next n bytes as an alias of the underlying buffer, or
// false without consuming anything when fewer than n bytes remain.
func (s *sliceSource) take(n int) ([]byte, bool) {
	if n < 0 || n > len(s.buf)-s.off {
		return nil, false
	}
	data := s.buf[s.off : s.off+n : s.off+n]
	s.off += n
	return data, true
}

// ensureSlurp reads the remaining dataset region of a sized source into a
// single buffer and swaps it in as the reader's byte source. It runs at most
// once; on a short read the partial buffer is used and the original error is
// surfaced at the position the source actually ended, so downstream error
// behavior matches a plain streaming read.
func (r *Reader) ensureSlurp() {
	if !r.slurpEligible {
		return
	}
	r.slurpEligible = false
	remaining := r.sourceEnd - r.Position()
	if remaining <= 0 || remaining > int64(^uint(0)>>1) {
		return
	}
	buf := make([]byte, int(remaining))
	n, err := io.ReadFull(r.counter.r, buf)
	src := &sliceSource{buf: buf[:n]}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		src.err = err
	}
	r.counter.r = src
}

// sourcePhysicalSize reports the total size of a source whose physical end is
// knowable without disturbing its read position. It covers regular files and
// in-memory readers that expose a Size method, such as bytes.Reader and
// io.SectionReader.
func sourcePhysicalSize(r io.Reader) (int64, bool) {
	switch src := r.(type) {
	case *os.File:
		info, err := src.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return 0, false
		}
		return info.Size(), true
	case interface{ Size() int64 }:
		return src.Size(), true
	}
	return 0, false
}

type countingReader struct {
	recordErrors  bool
	lastError     error
	firstFailure  error
	r             io.Reader
	pos           int64
	maxTotalBytes int64
	// fence, when positive, is an absolute offset reads must not cross; it
	// bounds element headers to their enclosing defined-length container.
	fence    int64
	fenceHit bool
	// pending holds bytes returned by unread; they are served before r.
	pending []byte
}

// unread returns b to the front of the stream (header repairs that read
// ahead). Callers keep it to a few bytes.
func (r *countingReader) unread(b []byte) {
	r.pending = append(append([]byte(nil), b...), r.pending...)
	r.pos -= int64(len(b))
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.read(p)
	if r.recordErrors && err != nil {
		r.lastError = err
		if r.firstFailure == nil && !terminalEOFCause(err) {
			r.firstFailure = err
		}
	}
	return n, err
}
func (r *countingReader) read(p []byte) (int, error) {
	if len(r.pending) > 0 && len(p) > 0 {
		n := copy(p, r.pending)
		if r.fence > 0 && r.pos+int64(n) > r.fence {
			n = int(max(r.fence-r.pos, 0))
			if n == 0 {
				r.fenceHit = true
				return 0, io.ErrUnexpectedEOF
			}
		}
		r.pending = r.pending[n:]
		r.pos += int64(n)
		return n, nil
	}
	if r.fence > 0 {
		remaining := r.fence - r.pos
		if remaining <= 0 {
			r.fenceHit = true
			return 0, io.ErrUnexpectedEOF
		}
		if int64(len(p)) > remaining {
			p = p[:remaining]
		}
	}
	if r.maxTotalBytes > 0 {
		remaining := r.maxTotalBytes - r.pos
		if remaining <= 0 {
			return 0, ErrMaxTotalBytesExceeded
		}
		if int64(len(p)) > remaining {
			p = p[:remaining]
			n, err := r.r.Read(p)
			r.pos += int64(n)
			if err != nil {
				return n, err
			}
			if int64(n) == remaining {
				return n, ErrMaxTotalBytesExceeded
			}
			return n, nil
		}
	}
	n, err := r.r.Read(p)
	r.pos += int64(n)
	return n, err
}

// seekPast advances past n value bytes by seeking the source instead of
// reading them, so a skipped or deferred value (Pixel Data above all) costs
// one seek rather than reading its whole payload. It applies only when that
// is indistinguishable from reading: the source is seekable with a known end
// (sourceEnd > 0, a regular file or a sized in-memory reader) and the n bytes
// end before the source end, the enclosing container fence and the
// MaxTotalBytes ceiling. Otherwise it reports false and the caller reads the
// bytes, which keeps the exact truncation and limit errors.
func (r *countingReader) seekPast(n, sourceEnd int64) bool {
	if n <= 0 || sourceEnd <= 0 {
		return false
	}
	end := r.pos + n
	if end > sourceEnd || (r.fence > 0 && end > r.fence) || (r.maxTotalBytes > 0 && end >= r.maxTotalBytes) {
		return false
	}
	seeker, ok := r.r.(io.Seeker)
	if !ok {
		return false
	}
	// Bytes returned by unread are already past the source position.
	buffered := min(int64(len(r.pending)), n)
	if remaining := n - buffered; remaining > 0 {
		if _, err := seeker.Seek(remaining, io.SeekCurrent); err != nil {
			return false
		}
	}
	r.pending = r.pending[buffered:]
	r.pos = end
	return true
}

func (r *countingReader) Position() int64 {
	if r == nil {
		return 0
	}
	return r.pos
}

func (r *Reader) Position() int64 {
	if r == nil {
		return 0
	}
	return r.counter.Position()
}
