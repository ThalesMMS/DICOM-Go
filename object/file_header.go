package object

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Part10Header is the parsed prelude of a DICOM Part 10 stream. DataSetOffset
// is the absolute seeker position of the first dataset byte.
type Part10Header struct {
	Preamble       []byte
	Meta           *Object
	TransferSyntax transfer.Syntax
	DataSetOffset  int64
}

// ReadPart10Header reads the Part 10 preamble and File Meta Information from
// the source's current position. It leaves source positioned at the first
// dataset byte and never closes the caller-owned source.
func ReadPart10Header(source io.ReadSeeker, opts ReadFileOptions) (Part10Header, error) {
	if source == nil {
		return Part10Header{}, fmt.Errorf("dicom: read Part 10 header: nil source")
	}
	streamBase, err := source.Seek(0, io.SeekCurrent)
	if err != nil {
		return Part10Header{}, err
	}
	return readPart10HeaderAt(source, opts, streamBase)
}

func readPart10HeaderAt(source io.ReadSeeker, opts ReadFileOptions, streamBase int64) (Part10Header, error) {
	if opts.FileMetaDictionary == nil {
		opts.FileMetaDictionary = std.Dictionary
	}
	if opts.Dictionary == nil {
		opts.Dictionary = std.Dictionary
	}
	prefix := make([]byte, part10PreambleLength+len(part10Magic))
	prefixSource := io.Reader(source)
	var limitedPrefix *io.LimitedReader
	if opts.MaxTotalBytes > 0 {
		limitedPrefix = &io.LimitedReader{R: source, N: opts.MaxTotalBytes}
		prefixSource = limitedPrefix
	}
	if _, err := io.ReadFull(prefixSource, prefix); err != nil {
		if limitedPrefix != nil && limitedPrefix.N == 0 &&
			(errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
			return Part10Header{}, parser.ErrMaxTotalBytesExceeded
		}
		return Part10Header{}, err
	}
	if string(prefix[part10PreambleLength:]) != part10Magic {
		return Part10Header{}, ErrMissingPreamble
	}

	meta, relativeDataSetOffset, err := readPart10FileMetaElements(source, int64(len(prefix)), opts)
	if err != nil {
		return Part10Header{}, wrapFileMetaError(err)
	}
	syntax, err := resolveFileMetaTransferSyntax(meta)
	if err != nil {
		return Part10Header{}, wrapFileMetaError(err)
	}
	absoluteDataSetOffset := streamBase + relativeDataSetOffset
	if _, err := source.Seek(absoluteDataSetOffset, io.SeekStart); err != nil {
		return Part10Header{}, err
	}
	return Part10Header{
		Preamble:       prefix[:part10PreambleLength:part10PreambleLength],
		Meta:           meta,
		TransferSyntax: syntax,
		DataSetOffset:  absoluteDataSetOffset,
	}, nil
}

// readerOnly prevents parser.Reader's sized-source optimization from reading
// beyond the File Meta Information while retaining exact read errors.
type readerOnly struct {
	io.Reader
}

func readPart10FileMetaElements(source io.ReadSeeker, baseOffset int64, opts ReadFileOptions) (*Object, int64, error) {
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
	readerOpts := metaOpts.parserReaderOptions(baseOffset)
	if opts.AllowImplicitVRFileMeta {
		implicit, err := fileMetaIsImplicitVR(source)
		if err != nil {
			return nil, 0, err
		}
		if implicit {
			metaReader := parser.NewReader(readerOnly{source}, transfer.ImplicitVRLittleEndian, readerOpts)
			meta, end, err := readPart10FileMetaElementsByGroup(source, metaReader, readerOpts)
			if err != nil {
				return nil, 0, err
			}
			meta.recordImplicitVRFileMeta(baseOffset, end)
			return meta, end, nil
		}
	}
	if opts.AllowMissingMetaElementGroupLength {
		metaReader := parser.NewReader(readerOnly{source}, transfer.ExplicitVRLittleEndian, readerOpts)
		return readPart10FileMetaElementsByGroup(source, metaReader, readerOpts)
	}
	return readDeclaredFileMeta(source, readerOpts)
}

func readDeclaredFileMeta(source io.Reader, opts parser.ReaderOptions) (*Object, int64, error) {
	checked := &fileMetaGroupSource{source: source}
	reader := parser.NewReader(checked, transfer.ExplicitVRLittleEndian, opts)
	first, err := checked.next(reader, opts.MaxTotalBytes, true)
	if err != nil {
		return nil, 0, err
	}
	raw, ok := first.Element.RawBytes()
	if first.Kind != parser.TokenElement || !ok || len(raw) != 4 {
		return nil, 0, ErrInvalidFileMetaGroupLengthValue
	}
	length := int64(binary.LittleEndian.Uint32(raw))
	opts.BaseOffset = reader.Position()
	if opts.MaxTotalBytes > 0 && length > opts.MaxTotalBytes-opts.BaseOffset {
		return nil, 0, &parser.ParseError{
			Op: parser.OpCheckTotalBytes, Offset: opts.BaseOffset, Tag: tagFileMetaInformationGroupLength,
			Err: parser.ErrMaxTotalBytesExceeded,
		}
	}

	declared := &io.LimitedReader{R: source, N: length}
	checked.source = declared
	reader = parser.NewReader(checked, transfer.ExplicitVRLittleEndian, opts)
	elements := []core.Element{first.Element}
	for declared.N > 0 {
		token, err := checked.next(reader, opts.MaxTotalBytes, false)
		if err != nil {
			return nil, 0, err
		}
		if token.Kind != parser.TokenElement {
			return nil, 0, fmt.Errorf("%w: unexpected file meta token %s", ErrInvalidFileMetaGroupLength, token.Kind)
		}
		elements = append(elements, token.Element)
	}
	return FromDataSet(core.DataSet{Elements: elements}, opts.Dictionary), reader.Position(), nil
}

// fileMetaGroupSource checks each tag before the parser can consume its value.
// Replay the four tag bytes directly so this also works on streams without
// seeking or reading ahead into the value.
type fileMetaGroupSource struct {
	source  io.Reader
	tag     [4]byte
	pending int
}

func (s *fileMetaGroupSource) next(reader *parser.Reader, limit int64, first bool) (parser.Token, error) {
	if limit > 0 && limit-reader.Position() < int64(len(s.tag)) {
		return parser.Token{}, &parser.ParseError{Op: parser.OpReadTag, Offset: reader.Position(), Err: parser.ErrMaxTotalBytesExceeded}
	}
	if _, err := io.ReadFull(s.source, s.tag[:]); err != nil {
		if !first && errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return parser.Token{}, &parser.ParseError{Op: parser.OpReadTag, Offset: reader.Position(), Err: err}
	}
	tag := core.NewTag(binary.LittleEndian.Uint16(s.tag[:2]), binary.LittleEndian.Uint16(s.tag[2:]))
	if tag.Group != tagFileMetaInformationGroupLength.Group || (first && tag != tagFileMetaInformationGroupLength) {
		return parser.Token{}, &parser.ParseError{Op: parser.OpReadTag, Offset: reader.Position(), Tag: tag, Err: ErrInvalidFileMetaGroupLength}
	}
	s.pending = len(s.tag)
	return reader.Next()
}

func (s *fileMetaGroupSource) Read(p []byte) (int, error) {
	if s.pending == 0 {
		return s.source.Read(p)
	}
	n := copy(p, s.tag[len(s.tag)-s.pending:])
	s.pending -= n
	return n, nil
}

func readPart10FileMetaElementsByGroup(source io.ReadSeeker, metaReader *parser.Reader, readerOpts parser.ReaderOptions) (*Object, int64, error) {
	var metaElements []core.Element
	for {
		position, err := source.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, 0, err
		}
		var tagBytes [4]byte
		_, readErr := io.ReadFull(source, tagBytes[:])
		if _, err := source.Seek(position, io.SeekStart); err != nil {
			return nil, 0, err
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
		if readErr != nil {
			return nil, 0, readErr
		}
		if binary.LittleEndian.Uint16(tagBytes[:2]) != tagFileMetaInformationGroupLength.Group {
			break
		}
		token, err := metaReader.Next()
		if err != nil {
			return nil, 0, err
		}
		if token.Kind != parser.TokenElement {
			return nil, 0, ErrInvalidFileMetaGroupLength
		}
		metaElements = append(metaElements, token.Element)
	}
	if len(metaElements) == 0 {
		return nil, 0, ErrInvalidFileMetaGroupLength
	}
	return FromDataSet(core.DataSet{Elements: metaElements}, readerOpts.Dictionary), metaReader.Position(), nil
}

// fileMetaIsImplicitVR peeks at the first File Meta element: a group 0002 tag
// whose next two bytes are not a known VR is Implicit VR encoded.
func fileMetaIsImplicitVR(source io.ReadSeeker) (bool, error) {
	position, err := source.Seek(0, io.SeekCurrent)
	if err != nil {
		return false, err
	}
	var head [6]byte
	_, readErr := io.ReadFull(source, head[:])
	if _, err := source.Seek(position, io.SeekStart); err != nil {
		return false, err
	}
	if readErr != nil {
		return false, nil
	}
	return implicitVRFileMetaHead(head[:]), nil
}

// implicitVRFileMetaHead reports whether the first six File Meta bytes are a
// group 0002 tag followed by bytes that are not a known VR.
func implicitVRFileMetaHead(head []byte) bool {
	if len(head) < 6 || binary.LittleEndian.Uint16(head[:2]) != tagFileMetaInformationGroupLength.Group {
		return false
	}
	_, err := core.ParseVR(string(head[4:6]))
	return err != nil
}

// recordImplicitVRFileMeta reports the Implicit VR File Meta repair.
func (o *Object) recordImplicitVRFileMeta(start, end int64) {
	o.boundaryRecoveries = append(o.boundaryRecoveries, parser.BoundaryRecovery{
		Path: "(0002,xxxx)", Offset: start, DeclaredLength: -1, AvailableLength: end - start,
		Reason: parser.BoundaryImplicitVRFileMeta,
	})
}
