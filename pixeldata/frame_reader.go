package pixeldata

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"strings"
	"sync"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	// ErrFrameLayout reports Pixel Data whose frames a FrameReader cannot
	// locate or crop safely in the stored layout.
	ErrFrameLayout = errors.New("dicom: frames cannot be located in Pixel Data")
	// ErrRegionUnsupported is returned by a FrameRegionDecoder that cannot
	// decode part of this frame; FrameReader then decodes the whole frame
	// and crops it.
	ErrRegionUnsupported = errors.New("dicom: codec cannot decode a frame region")
	// ErrInvalidRegion reports an empty region or one outside the frame.
	ErrInvalidRegion = errors.New("dicom: invalid frame region")
)

// FrameRegionDecoder is implemented by codecs that emit part of one frame
// without materializing a whole-frame output, such as RLE (stopping after
// the last row needed) and JPEG 2000 (decoding only overlapping tiles).
// codestream is the frame's borrowed, read-only encoded bytes. metadata describes that one
// frame; the result holds the region's samples in the layout the codec's
// full decode produces, and ErrRegionUnsupported asks for a full decode.
type FrameRegionDecoder interface {
	DecodeFrameRegion(ctx context.Context, codestream []byte, metadata Metadata, region image.Rectangle) (Frames, error)
}

// FrameReaderOptions configures a FrameReader.
type FrameReaderOptions struct {
	// Registry decodes encapsulated syntaxes; nil uses DefaultRegistry.
	Registry Registry
	// Limits bound each decoded frame (zero fields use the defaults of
	// DecompressDataSetContext).
	Limits DecompressLimits
	// MaxFrames and MaxFragments bound the frame index (zero: the
	// encapsulated package defaults).
	MaxFrames, MaxFragments int
}

// FrameReader reads single frames, or regions of them, from Pixel Data it
// has only indexed: native frames are read at their offset, and encapsulated
// frames are located from the Items (one per frame, the offset tables, or the
// JPEG, JPEG-LS and JPEG 2000 codestream markers) and decoded on their own.
// Over a file read with deferred Pixel Data, memory holds one frame, or one
// region of a native frame, at a time.
type FrameReader struct {
	file     *object.File
	syntax   transfer.Syntax
	metadata Metadata
	options  FrameReaderOptions
	// proxy is the data set's elements for decoding one frame: without
	// Pixel Data and offset tables, Number of Frames 1.
	proxy       []core.Element
	pixelHeader core.ElementHeader
	// Codec takes a complete Object, whose read methods may populate caches.
	// Each active decode therefore borrows an exclusive facade; subsequent
	// frames reuse it without rebuilding the metadata maps and private scope.
	decodeProxies  sync.Pool
	decodeOnce     sync.Once
	decodeMetadata Metadata
	decodeLimits   DecompressLimits
	decodeErr      error
	// rleInput is one private, nonblocking lease for direct multiframe
	// RLE reads. Concurrent reads can instead use their own encoded bytes.
	rleInputMu sync.Mutex
	rleInput   []byte

	native      io.ReaderAt // native Pixel Data at nativeOffset
	nativeOrder binary.ByteOrder
	nativeStart int64
	valueLength int64

	fragments  encapsulated.Source // encapsulated Items after the Basic Offset Table
	basic      []byte
	plan       *encapsulated.Plan // JPEG family; else one Item per frame
	inputBytes int64
	closer     io.Closer
	collection []byte // one bounded JPEG 2000 Part 2 Annex J collection
}

// OpenFrameReader opens the Part 10 file at path with its Pixel Data
// deferred and indexes its frames; Close releases the file.
func OpenFrameReader(ctx context.Context, path string, options FrameReaderOptions) (*FrameReader, error) {
	f, err := object.OpenSourceFile(path)
	if err != nil {
		return nil, err
	}
	file, err := object.ReadFileWithOptions(f, object.ReadFileOptions{DeferPixelData: true})
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	reader, err := NewFrameReader(ctx, file, f, options)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	reader.closer = f
	return reader, nil
}

// NewFrameReader indexes the frames of file. When file was read with
// DeferPixelData, source must be the seekable input it was read from, which
// must stay open and unchanged while frames are read; with materialized
// Pixel Data source may be nil. The file's dataset and its value storage must
// also remain unchanged for the reader's lifetime.
func NewFrameReader(ctx context.Context, file *object.File, source io.ReaderAt, options FrameReaderOptions) (*FrameReader, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if file == nil || file.Dataset == nil {
		return nil, fmt.Errorf("%w: nil file", ErrMissingMetadata)
	}
	syntax, err := strictFileTransferSyntax(file)
	if err != nil {
		return nil, err
	}
	dataset := file.Dataset
	pixel, ok := dataset.Get(core.TagPixelData)
	if !ok {
		return nil, ErrPixelDataNotFound
	}
	metadata, err := ExtractMetadata(dataset)
	if err != nil {
		return nil, err
	}
	if options.MaxFrames < 0 || options.MaxFragments < 0 {
		return nil, fmt.Errorf("%w: %w: negative frame index limit", ErrFrameLayout, encapsulated.ErrResourceLimit)
	}
	indexDefaults := encapsulated.Defaults()
	if options.MaxFrames == 0 {
		options.MaxFrames = indexDefaults.MaxFrames
	}
	if options.MaxFragments == 0 {
		options.MaxFragments = indexDefaults.MaxFragments
	}
	if metadata.NumberOfFrames > options.MaxFrames {
		if syntax.Encapsulated {
			return nil, fmt.Errorf("%w: %w: frame count %d exceeds %d", ErrFrameLayout, encapsulated.ErrResourceLimit, metadata.NumberOfFrames, options.MaxFrames)
		}
		return nil, fmt.Errorf("%w: %w", ErrFrameLayout, decompressLimitError("MaxFrames", int64(metadata.NumberOfFrames), int64(options.MaxFrames)))
	}
	if metadata.BitsAllocated < 8 && (metadata.BitsAllocated != 1 || syntax.Encapsulated) {
		return nil, fmt.Errorf("%w: BitsAllocated %d packs frames across bytes", ErrFrameLayout, metadata.BitsAllocated)
	}
	r := &FrameReader{file: file, syntax: syntax, metadata: metadata, options: options, pixelHeader: pixel.Header}
	for _, element := range dataset.Elements() {
		switch element.Tag() {
		case core.TagPixelData, tagExtendedOffsetTable, tagExtendedOffsetTableLengths, tagEncapsulatedPixelDataValueTotalLength:
			continue
		case tagNumberOfFrames:
			element = core.Element{Header: core.ElementHeader{Tag: tagNumberOfFrames, VR: core.VRIS}, Value: core.StringValue{"1"}}
		}
		r.proxy = append(r.proxy, element)
	}
	if !syntax.Encapsulated {
		if err := r.indexNative(file, pixel, source); err != nil {
			return nil, err
		}
		return r, nil
	}
	if err := r.indexEncapsulated(ctx, file, pixel, source); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *FrameReader) indexNative(file *object.File, pixel core.Element, source io.ReaderAt) error {
	r.nativeOrder = binary.LittleEndian
	if r.syntax.ByteOrder == binary.BigEndian {
		r.nativeOrder = binary.BigEndian
	}
	switch value := pixel.Value.(type) {
	case core.RawValue:
		r.native, r.valueLength = bytes.NewReader(value), int64(len(value))
	case nil:
		locations := file.ValueLocations(core.TagPixelData)
		if source == nil || len(locations) != 1 || pixel.Header.Length.IsUndefined() {
			return fmt.Errorf("%w: deferred Pixel Data needs its seekable source", ErrFrameLayout)
		}
		r.native, r.nativeStart, r.valueLength = source, locations[0].ValueOffset, locations[0].Length
	default:
		return fmt.Errorf("%w: native Pixel Data value %T", ErrFrameLayout, pixel.Value)
	}
	total := r.metadata.TotalSize()
	if r.metadata.BitsAllocated == 1 {
		total, _ = nativePackedByteCount(r.metadata)
	}
	if total <= 0 || (r.nativeOWByteLanes() && total == math.MaxInt64) {
		return fmt.Errorf("%w: invalid native Pixel Data dimensions", ErrFrameLayout)
	}
	if r.nativeOWByteLanes() {
		total += total % 2
		if r.valueLength%2 != 0 {
			return fmt.Errorf("%w: Big Endian OW Pixel Data ends with an incomplete word", ErrPixelDataSizeMismatch)
		}
	}
	if r.valueLength < total {
		return fmt.Errorf("%w: %d bytes of Pixel Data, need %d for %d frames", ErrPixelDataSizeMismatch, r.valueLength, total, r.metadata.NumberOfFrames)
	}
	r.inputBytes = r.valueLength
	return nil
}

func (r *FrameReader) indexEncapsulated(ctx context.Context, file *object.File, pixel core.Element, source io.ReaderAt) error {
	switch value := pixel.Value.(type) {
	case core.FragmentSequence:
		if len(value.Fragments) > r.options.MaxFragments {
			return fmt.Errorf("%w: %w: fragment count %d exceeds %d", ErrFrameLayout, encapsulated.ErrResourceLimit, len(value.Fragments), r.options.MaxFragments)
		}
		if (uint64(len(value.OffsetTable))+3)/4 > uint64(r.options.MaxFrames) {
			return fmt.Errorf("%w: %w: Basic Offset Table exceeds the frame index limit", ErrFrameLayout, encapsulated.ErrResourceLimit)
		}
		r.basic = value.OffsetTable
		r.fragments = encapsulated.Memory(value.Fragments)
		r.inputBytes = pixelPayloadBytes(PixelData{Sequence: value, Encapsulated: true})
	case nil:
		locations := file.ValueLocations(core.TagPixelData)
		if source == nil || len(locations) != 1 {
			return fmt.Errorf("%w: deferred Pixel Data needs its seekable source", ErrFrameLayout)
		}
		items, basic, err := scanItems(source, locations[0].ValueOffset, locations[0].Length, r.options.MaxFragments, r.options.MaxFrames)
		if err != nil {
			return err
		}
		if r.fragments, err = encapsulated.ReaderAt(source, items); err != nil {
			return fmt.Errorf("%w: %w", ErrFrameLayout, err)
		}
		r.basic, r.valueLength = basic, locations[0].Length
		r.inputBytes = int64(len(basic))
		for _, item := range items {
			r.inputBytes += int64(item.Length)
		}
	default:
		return fmt.Errorf("%w: encapsulated Pixel Data value %T", ErrFrameLayout, pixel.Value)
	}
	frames := r.metadata.NumberOfFrames
	if jpeg2000Part2(r.syntax.UID) && r.fragments.FragmentCount() == 1 && frames > 1 {
		return r.indexPart2Collection(ctx, file)
	}
	if format, ok := streamFrameFormat(r.syntax.UID); ok {
		tables, err := encapsulated.ReadTables(file.Dataset, r.basic, frames)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrFrameLayout, err)
		}
		plan, err := encapsulated.New(ctx, r.fragments, tables, frames, format, encapsulated.Limits{MaxFrames: r.options.MaxFrames, MaxFragments: r.options.MaxFragments})
		if err != nil {
			return fmt.Errorf("%w: %w", ErrFrameLayout, err)
		}
		r.plan = plan
		return nil
	}
	// Other syntaxes (RLE among them) need one Item per frame, and a Basic
	// Offset Table, if any, pointing at those Items.
	if r.fragments.FragmentCount() != frames {
		return fmt.Errorf("%w: %d Items for %d frames", ErrFrameLayout, r.fragments.FragmentCount(), frames)
	}
	if len(r.basic) == 0 {
		return nil
	}
	if len(r.basic) != 4*frames {
		return fmt.Errorf("%w: Basic Offset Table of %d bytes for %d frames", ErrFrameLayout, len(r.basic), frames)
	}
	var next uint64
	for i := 0; i < frames; i++ {
		size, err := r.fragments.FragmentSize(i)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrFrameLayout, err)
		}
		if uint64(binary.LittleEndian.Uint32(r.basic[4*i:])) != next {
			return fmt.Errorf("%w: Basic Offset Table entry %d does not point at Item %d", ErrFrameLayout, i, i)
		}
		next += 8 + size + size%2
	}
	return nil
}

// scanItems reads the Item headers of an encapsulated value: the Basic
// Offset Table's bytes and the ranges of the other Items.
func scanItems(source io.ReaderAt, offset, length int64, maxItems, maxFrames int) ([]encapsulated.ItemRange, []byte, error) {
	if offset < 0 || length < 0 || offset > math.MaxInt64-length {
		return nil, nil, fmt.Errorf("%w: invalid encapsulated value extent", ErrFrameLayout)
	}
	var (
		header [8]byte
		items  []encapsulated.ItemRange
		basic  []byte
		first  = true
		end    = offset + length
	)
	for {
		if end-offset < 8 {
			return nil, nil, fmt.Errorf("%w: Items run past the value", ErrFrameLayout)
		}
		if _, err := source.ReadAt(header[:], offset); err != nil {
			return nil, nil, err
		}
		tag := core.NewTag(binary.LittleEndian.Uint16(header[0:]), binary.LittleEndian.Uint16(header[2:]))
		itemLength := binary.LittleEndian.Uint32(header[4:])
		offset += 8
		if tag == core.TagSequenceDelimitationItem {
			return items, basic, nil
		}
		if tag != core.TagItem || itemLength == math.MaxUint32 || int64(itemLength) > end-offset {
			return nil, nil, fmt.Errorf("%w: malformed Item", ErrFrameLayout)
		}
		if first {
			if uint64(itemLength) > uint64(math.MaxInt) || (uint64(itemLength)+3)/4 > uint64(maxFrames) {
				return nil, nil, fmt.Errorf("%w: %w: Basic Offset Table exceeds the frame index limit", ErrFrameLayout, encapsulated.ErrResourceLimit)
			}
			basic = make([]byte, itemLength)
			if _, err := source.ReadAt(basic, offset); err != nil {
				return nil, nil, err
			}
			first = false
		} else {
			if len(items) >= maxItems {
				return nil, nil, fmt.Errorf("%w: %w: fragment count exceeds %d", ErrFrameLayout, encapsulated.ErrResourceLimit, maxItems)
			}
			items = append(items, encapsulated.ItemRange{Offset: offset, Length: uint64(itemLength)})
		}
		offset += int64(itemLength)
	}
}

// Close releases the file OpenFrameReader opened and any reusable input
// buffer. Call it after reads have finished.
func (r *FrameReader) Close() error {
	if r == nil {
		return nil
	}
	r.rleInput = nil
	r.collection = nil
	if r.closer == nil {
		return nil
	}
	err := r.closer.Close()
	r.closer = nil
	return err
}

// Metadata describes the stored Pixel Data, all frames.
func (r *FrameReader) Metadata() Metadata { return r.metadata }

// TransferSyntax is the syntax the Pixel Data is stored in.
func (r *FrameReader) TransferSyntax() transfer.Syntax { return r.syntax }

// File is the indexed file; its Pixel Data may be deferred.
func (r *FrameReader) File() *object.File { return r.file }

func (r *FrameReader) checkIndex(i int) error {
	if i < 0 || i >= r.metadata.NumberOfFrames {
		return fmt.Errorf("%w: frame %d of %d", ErrInvalidMetadata, i, r.metadata.NumberOfFrames)
	}
	return nil
}

// RawFrame returns frame i in the syntax's byte order or the encoded
// codestream (its Items joined). Packed native samples start at bit zero.
// Big Endian OW with up to eight bits allocated is re-aligned to an independent
// word stream, including a trailing pad byte when its logical length is odd.
func (r *FrameReader) RawFrame(ctx context.Context, i int) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := r.checkIndex(i); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !r.syntax.Encapsulated {
		return r.rawNativeFrame(ctx, i)
	}
	if r.collection != nil {
		return nil, fmt.Errorf("%w: a component collection has no independent frame codestream", ErrFrameLayout)
	}
	if r.plan != nil {
		view, err := r.plan.Frame(ctx, i)
		if err != nil {
			return nil, err
		}
		if view.Borrowed {
			return append([]byte(nil), view.Data...), nil
		}
		return view.Data, nil
	}
	size, err := r.fragments.FragmentSize(i)
	if err != nil {
		return nil, err
	}
	frame := make([]byte, size)
	if err := r.readFrameFragment(ctx, i, frame); err != nil {
		return nil, err
	}
	return frame, nil
}

func (r *FrameReader) readFrameFragment(ctx context.Context, i int, frame []byte) error {
	for read := 0; read < len(frame); {
		n, err := r.fragments.ReadFragmentAt(ctx, i, frame[read:], int64(read))
		if n < 0 || n > len(frame)-read {
			return io.ErrUnexpectedEOF
		}
		read += n
		if err != nil && (read < len(frame) || !errors.Is(err, io.EOF)) {
			return err
		}
		if n == 0 && read < len(frame) {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}

// RawFrameLength is the length of RawFrame(i) without reading the frame,
// from the native frame size or the encapsulated Item index.
func (r *FrameReader) RawFrameLength(i int) (int64, error) {
	if err := r.checkIndex(i); err != nil {
		return 0, err
	}
	if !r.syntax.Encapsulated {
		length := r.metadata.FrameSize()
		if r.nativeOWByteLanes() {
			length += length % 2
		}
		return length, nil
	}
	if r.collection != nil {
		return 0, fmt.Errorf("%w: a component collection has no independent frame codestream", ErrFrameLayout)
	}
	if r.plan != nil {
		length, ok := r.plan.FrameLength(i)
		if !ok {
			return 0, fmt.Errorf("%w: frame %d", ErrFrameLayout, i+1)
		}
		return int64(length), nil
	}
	size, err := r.fragments.FragmentSize(i)
	if err != nil {
		return 0, err
	}
	return int64(size), nil
}

// decodeFrameObject also builds a native data set for the transcode callers
// that need it. Frame only needs samples and metadata, and skips that work.
func (r *FrameReader) decodeFrameObject(ctx context.Context, i int, report *DecompressReport) (*object.Object, []byte, error) {
	decoded, frame, _, err := r.decodeFrame(ctx, i, report, true)
	return decoded, frame, err
}

func (r *FrameReader) newDecodeProxy() *object.Object {
	proxy := object.FromElements(r.proxy, nil)
	proxy.SetValueByteOrder(r.nativeByteOrder())
	return proxy
}

func (r *FrameReader) prepareDecode() {
	limits := r.options.Limits
	limits.MaxFrames = 1
	r.decodeLimits, r.decodeErr = normalizedDecompressLimits(limits)
	if r.decodeErr != nil {
		return
	}
	if !r.syntax.Supported && !r.syntax.RequiresCodec() {
		r.decodeErr = fmt.Errorf("%w: source transfer syntax %q is not supported", ErrUnsupportedDecompression, r.syntax.UID)
		return
	}
	proxy := r.newDecodeProxy()
	r.decodeMetadata, r.decodeErr = ExtractMetadata(proxy)
	if r.decodeErr == nil && r.syntax.ByteOrder == binary.BigEndian {
		// The former whole-object path also rejected malformed binary values
		// elsewhere in a Big Endian data set. Validate that immutable metadata
		// once, even when Frame no longer constructs the converted data set.
		_, _, r.decodeErr = transcodeElementsToLittleEndianCopy(r.proxy, binary.BigEndian)
	}
	if r.decodeErr == nil {
		r.decodeProxies.Put(proxy)
	}
}

// checkDecodeFrameLimits applies the same per-frame admission as
// DecompressDataSetContext, before RawFrame allocates or reads the payload.
// Number of Frames is already 1 in the cached decode metadata.
func (r *FrameReader) checkDecodeFrameLimits(inputBytes int64) error {
	return checkDecodeAdmission(r.decodeLimits, r.decodeMetadata, r.syntax, inputBytes)
}

func checkFrameReadLimits(limits DecompressLimits, metadata Metadata, inputBytes, nativeBytes int64) error {
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
	pixels, ok := checkedDecompressProduct(int64(metadata.Rows), int64(metadata.Columns), int64(metadata.NumberOfFrames))
	if !ok {
		return decompressLimitError("MaxPixels", math.MaxInt64, limits.MaxPixels)
	}
	if pixels > limits.MaxPixels {
		return decompressLimitError("MaxPixels", pixels, limits.MaxPixels)
	}
	if nativeBytes <= 0 {
		return decompressLimitError("MaxNativeBytes", math.MaxInt64, limits.MaxNativeBytes)
	}
	if nativeBytes > limits.MaxNativeBytes {
		return decompressLimitError("MaxNativeBytes", nativeBytes, limits.MaxNativeBytes)
	}
	return nil
}

func (r *FrameReader) checkNativeRequestLimits(i int, region image.Rectangle, regional bool, layout BufferLayout) error {
	limits := r.options.Limits
	limits.MaxFrames = 1
	limits, err := normalizedDecompressLimits(limits)
	if err != nil {
		return err
	}
	if !regional && r.nativeOrder == binary.BigEndian {
		// Retain validation of other binary attributes before decoding a
		// complete Big Endian image as in the original whole-frame path.
		r.decodeOnce.Do(r.prepareDecode)
		if r.decodeErr != nil {
			return r.decodeErr
		}
	}
	inputBytes, err := r.nativeRequestInputBytes(i, region, regional)
	if err != nil {
		return err
	}
	return checkFrameReadLimits(limits, layout.Metadata, inputBytes, layout.Bytes)
}

func (r *FrameReader) admittedFrameInputBytes(ctx context.Context, i int) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	r.decodeOnce.Do(r.prepareDecode)
	if r.decodeErr != nil {
		return 0, r.decodeErr
	}
	inputBytes, err := r.RawFrameLength(i)
	if err != nil {
		return 0, err
	}
	if err := r.checkDecodeFrameLimits(inputBytes); err != nil {
		return 0, err
	}
	return inputBytes, nil
}

func (r *FrameReader) readAdmittedFrame(ctx context.Context, i int) ([]byte, error) {
	if _, err := r.admittedFrameInputBytes(ctx, i); err != nil {
		return nil, err
	}
	return r.RawFrame(ctx, i)
}

func (r *FrameReader) decodeFrame(ctx context.Context, i int, report *DecompressReport, withObject bool) (*object.Object, []byte, Metadata, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := r.checkIndex(i); err != nil {
		return nil, nil, Metadata{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, Metadata{}, err
	}
	if r.collection != nil {
		return r.decodePart2CollectionFrame(ctx, i, report, withObject)
	}
	r.decodeOnce.Do(r.prepareDecode)
	if r.decodeErr != nil {
		return nil, nil, Metadata{}, r.decodeErr
	}
	if r.native != nil && r.nativeSharedUnits() {
		raw, metadata, err := r.readNative(ctx, i, image.Rectangle{}, false)
		if err != nil || !withObject {
			return nil, raw, metadata, err
		}
		proxy := r.newDecodeProxy()
		decoded, err := datasetWithNativePixelData(proxy, metadata, raw, r.syntax)
		if err != nil {
			return nil, nil, Metadata{}, err
		}
		if report != nil {
			report.Diagnostics = report.Diagnostics[:0]
			report.Reconciliations = report.Reconciliations[:0]
		}
		inputBytes, err := r.nativeRequestInputBytes(i, image.Rectangle{}, false)
		if err != nil {
			return nil, nil, Metadata{}, err
		}
		stampLossyState(decoded, DetectDataSetLossyState(proxy, r.syntax), int64(len(raw)), inputBytes)
		return decoded, raw, metadata, nil
	}
	inputBytes, err := r.RawFrameLength(i)
	if err != nil {
		return nil, nil, Metadata{}, err
	}
	if err := r.checkDecodeFrameLimits(inputBytes); err != nil {
		return nil, nil, Metadata{}, err
	}
	frame, err := r.RawFrame(ctx, i)
	if err != nil {
		return nil, nil, Metadata{}, err
	}
	// RawFrame owns its bytes, and codecs borrow them only until Decode returns.
	pixel := core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: r.pixelHeader.VR, Length: core.Length(len(frame)), LengthSet: true}, Value: core.RawValue(frame)}
	if r.syntax.Encapsulated {
		pixel = core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true},
			Value: core.FragmentSequence{Fragments: [][]byte{frame}}}
	}
	proxy, _ := r.decodeProxies.Get().(*object.Object)
	if proxy == nil {
		proxy = r.newDecodeProxy()
	}
	proxy.Put(pixel)
	defer func() {
		// Do not keep any frame payload alive in the pool between calls.
		proxy.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: r.pixelHeader.VR}})
		r.decodeProxies.Put(proxy)
	}()
	view, err := ExtractView(proxy)
	if err != nil {
		return nil, nil, Metadata{}, err
	}
	// Resolve the registry on each call: callers may register/replace a codec
	// after indexing. The shared dispatch preserves ContextCodec, recovery and
	// color options carried by ctx, typed availability errors and diagnostics.
	frames, err := decodeForDecompressionContext(ctx, r.syntax, view, proxy, r.options.Registry)
	if err != nil {
		return nil, nil, Metadata{}, err
	}
	if report != nil {
		report.Diagnostics = append(report.Diagnostics[:0], frames.Diagnostics...)
		report.Reconciliations = report.Reconciliations[:0]
	}
	metadata := decompressedOutputMetadata(r.decodeMetadata, r.syntax, frames)
	frames, err = littleEndianOutputFramesContext(ctx, metadata, r.syntax, frames)
	if err != nil {
		return nil, nil, Metadata{}, err
	}
	raw, err := nativeFrameBytesContext(ctx, metadata, frames)
	if err != nil {
		return nil, nil, Metadata{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, Metadata{}, err
	}
	if !withObject {
		return nil, raw, metadata, nil
	}
	decoded, err := datasetWithNativePixelData(proxy, metadata, raw, r.syntax)
	if err != nil {
		return nil, nil, Metadata{}, err
	}
	stampLossyState(decoded, withDecodedLossy(DetectDataSetLossyState(proxy, r.syntax), frames), int64(len(raw)), int64(len(frame)))
	return decoded, raw, metadata, nil
}

func (r *FrameReader) nativeByteOrder() binary.ByteOrder {
	if r.syntax.ByteOrder == binary.BigEndian {
		return binary.BigEndian
	}
	return binary.LittleEndian
}

// Frame returns frame i decoded to native little-endian samples, as
// DecompressDataSetContext decodes it, with the metadata of that one frame
// (the decoded Photometric Interpretation and Planar Configuration, Number
// of Frames 1).
func (r *FrameReader) Frame(ctx context.Context, i int) ([]byte, Metadata, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !r.syntax.Encapsulated {
		return r.readNative(ctx, i, image.Rectangle{}, false)
	}
	_, frame, metadata, err := r.decodeFrame(ctx, i, nil, false)
	if err != nil {
		return nil, Metadata{}, err
	}
	// Match ExtractMetadata on the former native output object, including
	// padding supplied by a codec in its output photometric interpretation.
	metadata.PhotometricInterpretation = strings.Trim(metadata.PhotometricInterpretation, " \x00")
	if metadata.PhotometricInterpretation == "" {
		return nil, Metadata{}, missingMetadataError("PhotometricInterpretation", tagPhotometricInterpretation)
	}
	return frame, metadata, err
}

// frameMetadata is the stored metadata of one frame.
func (r *FrameReader) frameMetadata() Metadata {
	m := r.metadata
	m.NumberOfFrames = 1
	return m
}

// Region returns the samples of region (in pixels, within the frame's
// Rows x Columns) of frame i, decoded like Frame and cropped: rows of the
// region's width, interleaved or, for planar native data, plane after plane,
// with metadata whose Rows and Columns are the region's. Native frames read
// only the region's rows. A FrameRegionDecoder produces the region directly
// and may still visit samples used as prediction history. Other frames are
// decoded whole and cropped.
func (r *FrameReader) Region(ctx context.Context, i int, region image.Rectangle) ([]byte, Metadata, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := r.checkIndex(i); err != nil {
		return nil, Metadata{}, err
	}
	bounds := image.Rect(0, 0, int(r.metadata.Columns), int(r.metadata.Rows))
	if region.Empty() || !region.In(bounds) {
		return nil, Metadata{}, fmt.Errorf("%w: %v in %v", ErrInvalidRegion, region, bounds)
	}
	if !r.syntax.Encapsulated {
		if isSubsampled422(r.metadata.PhotometricInterpretation) {
			return nil, Metadata{}, fmt.Errorf("%w: regions of subsampled 4:2:2 Pixel Data are unsupported", ErrFrameLayout)
		}
		return r.nativeRegion(ctx, i, region)
	}
	if decoder, ok := r.regionDecoder(); ok {
		codestream, err := r.readAdmittedFrame(ctx, i)
		if err != nil {
			return nil, Metadata{}, err
		}
		frames, err := decoder.DecodeFrameRegion(ctx, codestream, r.frameMetadata(), region)
		if err == nil {
			return r.checkDecodedPart(frames, region)
		}
		if !errors.Is(err, ErrRegionUnsupported) {
			return nil, Metadata{}, err
		}
	}
	frame, metadata, err := r.Frame(ctx, i)
	if err != nil {
		return nil, Metadata{}, err
	}
	return cropFrame(frame, metadata, region)
}

func (r *FrameReader) regionDecoder() (FrameRegionDecoder, bool) {
	if r.collection != nil {
		return nil, false
	}
	codec, ok := r.codec()
	if !ok {
		return nil, false
	}
	decoder, ok := codec.(FrameRegionDecoder)
	return decoder, ok
}

// nativeRegion reads only the rows of region from a native frame.
func (r *FrameReader) nativeRegion(ctx context.Context, i int, region image.Rectangle) ([]byte, Metadata, error) {
	return r.readNative(ctx, i, region, true)
}

// cropFrame cuts region out of a decoded frame laid out as metadata says.
func cropFrame(frame []byte, metadata Metadata, region image.Rectangle) ([]byte, Metadata, error) {
	if isSubsampled422(metadata.PhotometricInterpretation) {
		return nil, Metadata{}, fmt.Errorf("%w: regions of subsampled 4:2:2 Pixel Data are unsupported", ErrFrameLayout)
	}
	if int64(len(frame)) < metadata.FrameSize() {
		return nil, Metadata{}, fmt.Errorf("%w: frame of %d bytes, want %d", ErrPixelDataSizeMismatch, len(frame), metadata.FrameSize())
	}
	sampleBytes := metadata.BytesPerSample()
	columns, rows := int(metadata.Columns), int(metadata.Rows)
	samples := int(metadata.SamplesPerPixel)
	planes, perPixel := 1, samples
	if samples > 1 && metadata.PlanarConfiguration == 1 {
		planes, perPixel = samples, 1
	}
	width := region.Dx() * perPixel * sampleBytes
	out := make([]byte, 0, width*region.Dy()*planes)
	for plane := 0; plane < planes; plane++ {
		for y := region.Min.Y; y < region.Max.Y; y++ {
			start := ((plane*rows+y)*columns + region.Min.X) * perPixel * sampleBytes
			out = append(out, frame[start:start+width]...)
		}
	}
	metadata.Rows, metadata.Columns = uint16(region.Dy()), uint16(region.Dx())
	return out, metadata, nil
}
