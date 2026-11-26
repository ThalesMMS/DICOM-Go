package dicomweb

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// DefaultPart10BulkDataThreshold is the value length, in bytes, above which
// Part10Metadata replaces a binary or numeric value by a BulkDataURI.
const DefaultPart10BulkDataThreshold = 1024

const (
	defaultPart10MetadataElements     = 1 << 20
	defaultPart10MetadataElementBytes = 64 << 20
	defaultPart10MetadataBytes        = 64 << 20
	defaultPart10MetadataFragments    = 1 << 20
)

// Part10MetadataOptions configures Part10Metadata and Part10BulkData. Use the
// same options for both: a BulkDataURI made by Part10Metadata names the value
// at the path Part10BulkData then resolves with them.
type Part10MetadataOptions struct {
	// BulkDataThreshold selects the values replaced by a BulkDataURI: binary
	// (OB, OD, OF, OL, OV, OW, UN) and numeric (FL, FD, SL, SS, SV, UL, US,
	// UV) values longer than this many bytes, at any depth. Root Pixel Data,
	// Float Pixel Data and Double Float Pixel Data are always replaced.
	// Zero selects DefaultPart10BulkDataThreshold; negative is invalid.
	BulkDataThreshold int64
	// BulkDataURI returns the same-origin BulkDataURI of the value at path,
	// for example ServiceRoot + "/bulkdata/" + a backend token that contains
	// FormatBulkDataPath(path). Part10Metadata requires it.
	BulkDataURI func(path []parser.BulkDataPathStep) (string, error)
	// MaxElements, MaxElementBytes, MaxFragments and MaxDatasetBytes bound the
	// parse and the dataset of one object: the element count, one value kept
	// in memory (values that stay inline), the encapsulated Pixel Data
	// fragments and the encoded DICOM JSON. Zero selects finite defaults.
	MaxElements     int
	MaxElementBytes int64
	MaxFragments    int
	MaxDatasetBytes int64
}

// Part10Source is a seekable, randomly readable Part 10 object such as an
// *os.File.
type Part10Source interface {
	io.ReadSeeker
	io.ReaderAt
	io.Closer
}

var part10BulkDataVRs = []core.VR{
	core.VROB, core.VROD, core.VROF, core.VROL, core.VROV, core.VROW, core.VRUN,
	core.VRFL, core.VRFD, core.VRSL, core.VRSS, core.VRSV, core.VRUL, core.VRUS, core.VRUV,
}

func (o Part10MetadataOptions) readOptions() (object.ReadFileOptions, error) {
	threshold := o.BulkDataThreshold
	if threshold < 0 || o.MaxElements < 0 || o.MaxElementBytes < 0 || o.MaxFragments < 0 || o.MaxDatasetBytes < 0 {
		return object.ReadFileOptions{}, fmt.Errorf("%w: invalid Part 10 metadata options", ErrInvalidServerOptions)
	}
	if threshold == 0 {
		threshold = DefaultPart10BulkDataThreshold
	}
	rules := make([]parser.BulkDataRule, 0, len(part10BulkDataVRs)+3)
	for _, vr := range part10BulkDataVRs {
		rules = append(rules, parser.BulkDataRule{VR: vr, Threshold: threshold})
	}
	for _, element := range []uint16{0x0008, 0x0009, 0x0010} {
		rules = append(rules, parser.BulkDataRule{Path: []parser.BulkDataPathStep{{Tag: core.NewTag(0x7FE0, element), ItemIndex: -1}}})
	}
	elements, elementBytes, fragments := o.MaxElements, o.MaxElementBytes, o.MaxFragments
	if elements == 0 {
		elements = defaultPart10MetadataElements
	}
	if elementBytes == 0 {
		elementBytes = defaultPart10MetadataElementBytes
	}
	if fragments == 0 {
		fragments = defaultPart10MetadataFragments
	}
	return object.ReadFileOptions{
		BulkDataDescriptor: &parser.BulkDataDescriptor{Rules: rules},
		// Encapsulated Pixel Data is not selected by a descriptor rule.
		DeferPixelData:  true,
		MaxElements:     elements,
		MaxElementBytes: elementBytes,
		MaxFragments:    fragments,
	}, nil
}

// readPart10Deferred parses source from its start with every value
// Part10Metadata replaces left on the source.
func readPart10Deferred(ctx context.Context, source io.ReadSeeker, opts Part10MetadataOptions) (*object.File, error) {
	if source == nil {
		return nil, ErrBackend
	}
	readOptions, err := opts.readOptions()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nil, ErrBackend
	}
	header, err := object.ReadPart10Header(source, object.ReadFileOptions{})
	if err != nil {
		return nil, ErrBackend
	}
	// A deflated data set has no source offsets to name its values by.
	if header.TransferSyntax.Deflated {
		return nil, ErrUnsupported
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nil, ErrBackend
	}
	file, err := object.ReadFileWithOptions(source, readOptions)
	if err != nil {
		return nil, part10ReadError(err)
	}
	if file == nil || file.Dataset == nil {
		return nil, ErrBackend
	}
	return file, ctx.Err()
}

func part10ReadError(err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, parser.ErrMaxElementsExceeded), errors.Is(err, parser.ErrMaxElementBytesExceeded),
		errors.Is(err, parser.ErrMaxFragmentsExceeded), errors.Is(err, parser.ErrMaxDepthExceeded):
		return ErrResourceLimit
	default:
		return ErrBackend
	}
}

// Part10Metadata returns the WADO-RS metadata of the Part 10 object in source
// (PS3.18 section 10.4.1.1.2): its data set without File Meta Information and
// group lengths, as DICOM JSON, with the values that opts selects replaced by
// the BulkDataURI that opts.BulkDataURI returns. Those values are never read.
// Objects in any non-deflated transfer syntax are described, Implicit VR
// Little Endian and Explicit VR Big Endian included; a deflated one gives
// ErrUnsupported. InlineBinary is in Little Endian byte order, as the bulk data
// of Part10BulkData is. source is read from its start and is not closed.
func Part10Metadata(ctx context.Context, source io.ReadSeeker, opts Part10MetadataOptions) (Dataset, error) {
	if opts.BulkDataURI == nil {
		return nil, fmt.Errorf("%w: BulkDataURI is required", ErrInvalidServerOptions)
	}
	file, err := readPart10Deferred(ctx, source, opts)
	if err != nil {
		return nil, err
	}
	maximum := opts.MaxDatasetBytes
	if maximum == 0 {
		maximum = defaultPart10MetadataBytes
	}
	encoded, err := dicomjson.MarshalContext(ctx, file.Dataset, dicomjson.Options{
		OmitGroupLength: true,
		// Values that stay inline are Little Endian, the dicomjson default,
		// like the bulk data Part10BulkData returns for the same VRs above
		// the threshold.
		Limits: dicomjson.Limits{MaxJSONBytes: maximum, MaxInlineBinaryBytes: maximum, MaxTotalBinaryBytes: maximum},
		// Every value the parse left on the source becomes a URI, so the
		// fallback that would read it into memory never runs.
		MaxMaterializedValueBytes: 1,
		BulkDataReferenceURIFunc: func(_ context.Context, ref object.BulkDataReference) (string, error) {
			uri, err := opts.BulkDataURI(ref.Path)
			if err == nil && uri == "" {
				err = ErrBackend
			}
			return uri, err
		},
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, dicomjson.ErrMaxJSONBytesExceeded) || errors.Is(err, dicomjson.ErrMaxInlineBinaryBytesExceeded) || errors.Is(err, dicomjson.ErrMaxTotalBinaryBytesExceeded) {
			return nil, ErrResourceLimit
		}
		return nil, ErrBackend
	}
	var dataset Dataset
	if err := decodeJSON(encoded, &dataset); err != nil {
		return nil, ErrBackend
	}
	return dataset, nil
}

// Part10BulkData returns the value at path of the Part 10 object in source,
// a value Part10Metadata with the same opts replaced by a BulkDataURI, as
// application/octet-stream (PS3.18 section 8.7.3.5.4): numeric and OW, OF,
// OD, OL and OV values of an Explicit VR Big Endian object are returned in
// Little Endian byte order, by the rules of pixeldata.TranscodeDataSet
// (native OW Pixel Data with Bits Allocated 32 or 64 is reversed at the
// sample width, read in the same data set or item). A path Part10Metadata keeps inline gives
// ErrNotFound. Encapsulated Pixel Data gives ErrUnsupported: its
// octet-stream representation is the decompressed pixel data, which this
// function does not produce. On success the returned part owns source and
// closes it with its Reader; on error the caller still owns source.
func Part10BulkData(ctx context.Context, source Part10Source, path []parser.BulkDataPathStep, opts Part10MetadataOptions) (BulkDataPart, error) {
	if len(path) == 0 || len(path) > parser.MaxBulkDataPathSteps {
		return BulkDataPart{}, ErrNotFound
	}
	file, err := readPart10Deferred(ctx, source, opts)
	if err != nil {
		return BulkDataPart{}, err
	}
	ref, deferred, err := file.Dataset.DeferredBulkDataReference(path)
	if err != nil || !deferred {
		return BulkDataPart{}, ErrNotFound
	}
	if ref.UndefinedLength {
		return BulkDataPart{}, ErrUnsupported
	}
	if ref.Offset < 0 || ref.Length < 0 {
		return BulkDataPart{}, ErrBackend
	}
	section := io.NewSectionReader(source, ref.Offset, ref.Length)
	width := littleEndianWidth(file.Dataset, ref)
	if width <= 1 {
		return BulkDataPart{
			ContentType: "application/octet-stream",
			Reader:      part10BulkReader{Reader: section, Closer: source},
			Size:        ref.Length,
			ReaderAt:    section,
		}, nil
	}
	if ref.Length%int64(width) != 0 {
		return BulkDataPart{}, ErrBackend
	}
	return BulkDataPart{
		ContentType: "application/octet-stream",
		Reader: part10BulkReader{
			Reader: &byteOrderReader{source: section, width: width, remaining: ref.Length},
			Closer: source,
		},
		Size: ref.Length,
	}, nil
}

// littleEndianWidth returns the unit whose bytes are reversed to give the
// Little Endian form of a source value, or 0 when it already is.
func littleEndianWidth(root *object.Object, ref object.BulkDataReference) int {
	// The GE private syntax keeps only Pixel Data in Big Endian inside a
	// Little Endian data set; the object records that unit.
	if ref.TransferSyntaxUID == transfer.PrivateGEImplicitVRBigEndianPixelData.UID {
		return ref.NormalizationWidth
	}
	if ref.ByteOrder != binary.BigEndian {
		return 0
	}
	if ref.Tag == core.TagPixelData && ref.VR == core.VROW {
		// Bits Allocated of the data set or item that holds the value; an
		// Icon Image Sequence item never inherits its parent's.
		item := root
		for _, step := range ref.Path[:len(ref.Path)-1] {
			items, ok := item.GetSequence(step.Tag)
			if !ok || step.ItemIndex >= len(items) {
				return valueencode.ByteOrderWidth(ref.VR)
			}
			item = items[step.ItemIndex]
		}
		if bits, err := item.GetInt(core.NewTag(0x0028, 0x0100)); err == nil && (bits == 32 || bits == 64) {
			return int(bits / 8)
		}
	}
	return valueencode.ByteOrderWidth(ref.VR)
}

type part10BulkReader struct {
	io.Reader
	io.Closer
}

// byteOrderReader reverses the bytes of each width-sized unit of a value
// whose length is a multiple of width.
type byteOrderReader struct {
	source    io.Reader
	width     int
	remaining int64
	buffer    []byte
	pending   []byte
}

func (r *byteOrderReader) Read(p []byte) (int, error) {
	if len(r.pending) == 0 {
		if r.remaining == 0 {
			return 0, io.EOF
		}
		if r.buffer == nil {
			r.buffer = make([]byte, 32<<10) // a multiple of every unit width
		}
		chunk := r.buffer[:min(int64(len(r.buffer)), r.remaining)]
		if _, err := io.ReadFull(r.source, chunk); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		r.remaining -= int64(len(chunk))
		for unit := 0; unit+r.width <= len(chunk); unit += r.width {
			for i, j := unit, unit+r.width-1; i < j; i, j = i+1, j-1 {
				chunk[i], chunk[j] = chunk[j], chunk[i]
			}
		}
		r.pending = chunk
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// FormatBulkDataPath encodes a concrete bulk data path as URI path segments:
// one "GGGGEEEE.N" segment per ancestor sequence item (zero-based N) and a
// final "GGGGEEEE" for the value, with uppercase hexadecimal tags. For
// example, a value nested in the first item of a sequence formats as
// "00540016.0/00181072". ParseBulkDataPath reverses it.
func FormatBulkDataPath(path []parser.BulkDataPathStep) (string, error) {
	if len(path) == 0 || len(path) > parser.MaxBulkDataPathSteps {
		return "", ErrInvalidRequest
	}
	segments := make([]string, len(path))
	for i, step := range path {
		tag := fmt.Sprintf("%04X%04X", step.Tag.Group, step.Tag.Element)
		switch {
		case i == len(path)-1 && step.ItemIndex == -1:
			segments[i] = tag
		case i < len(path)-1 && step.ItemIndex >= 0:
			segments[i] = tag + "." + strconv.Itoa(step.ItemIndex)
		default:
			return "", ErrInvalidRequest
		}
	}
	return strings.Join(segments, "/"), nil
}

// ParseBulkDataPath decodes the segments FormatBulkDataPath produces. It
// accepts only that canonical form and returns ErrInvalidRequest otherwise.
func ParseBulkDataPath(text string) ([]parser.BulkDataPathStep, error) {
	segments := strings.Split(text, "/")
	if text == "" || len(segments) > parser.MaxBulkDataPathSteps {
		return nil, ErrInvalidRequest
	}
	path := make([]parser.BulkDataPathStep, len(segments))
	for i, segment := range segments {
		tagText, indexText, nested := strings.Cut(segment, ".")
		if nested == (i == len(segments)-1) || len(tagText) != 8 || strings.ToUpper(tagText) != tagText {
			return nil, ErrInvalidRequest
		}
		value, err := strconv.ParseUint(tagText, 16, 32)
		if err != nil {
			return nil, ErrInvalidRequest
		}
		step := parser.BulkDataPathStep{Tag: core.NewTag(uint16(value>>16), uint16(value)), ItemIndex: -1}
		if nested {
			index, err := strconv.Atoi(indexText)
			if err != nil || index < 0 || strconv.Itoa(index) != indexText {
				return nil, ErrInvalidRequest
			}
			step.ItemIndex = index
		}
		if step.Tag.Group == 0xFFFE || step.Tag.Group == 0x0002 {
			return nil, ErrInvalidRequest
		}
		path[i] = step
	}
	return path, nil
}
