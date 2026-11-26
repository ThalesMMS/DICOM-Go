package dicomweb

import (
	"errors"
	"fmt"
	"io"
	"math"
	"sync"

	"github.com/ThalesMMS/dicom-go/index"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// ErrStoreReencodeUnsupported reports a Part 10 payload whose transfer
// syntax ExplicitLittleEndianStorePayload cannot re-encode: encapsulated
// (compressed) Pixel Data would need a codec.
var ErrStoreReencodeUnsupported = errors.New("dicomweb: STOW-RS payload cannot be re-encoded in Explicit VR Little Endian")

// StoreRequiresExplicitLittleEndian reports whether a Part 10 object in the
// transfer syntax uid must be re-encoded before STOW-RS sends it as
// application/dicom. PS3.18 8.7.3 excludes Implicit VR Little Endian and
// Explicit VR Big Endian from the web services; Explicit VR Little Endian is
// the default of application/dicom.
func StoreRequiresExplicitLittleEndian(uid string) bool {
	switch transfer.NormalizeUID(uid) {
	case transfer.ImplicitVRLittleEndian.UID, transfer.ExplicitVRBigEndian.UID:
		return true
	default:
		return false
	}
}

// StoreCanReencodeExplicitLittleEndian reports whether
// ExplicitLittleEndianStorePayload re-encodes an object in uid without a
// codec: the native syntaxes Implicit and Explicit VR Little Endian,
// Explicit VR Big Endian and Deflated Explicit VR Little Endian.
func StoreCanReencodeExplicitLittleEndian(uid string) bool {
	switch transfer.NormalizeUID(uid) {
	case transfer.ImplicitVRLittleEndian.UID, transfer.ExplicitVRLittleEndian.UID,
		transfer.ExplicitVRBigEndian.UID, transfer.DeflatedExplicitVRLittleEndian.UID:
		return true
	default:
		return false
	}
}

// storeReencodeDeflateRatio bounds the inflated size of a Deflated source,
// as the DIMSE store re-encoding does.
const storeReencodeDeflateRatio = 1024

// storeReencodeUnknownSizeElementBytes bounds a value held in memory when
// the source length is unknown.
const storeReencodeUnknownSizeElementBytes = 64 << 20

// ExplicitLittleEndianStorePayload returns the Part 10 object of source
// re-encoded in Explicit VR Little Endian, as a stream for StoreInstance.Open.
// Every data set element keeps its value; the File Meta Information records
// the new transfer syntax and dicom-go as the implementation that wrote it.
// Values of an unknown VR become UN. Native Pixel Data streams from source
// without being held in memory, except from a Deflated source. size is the
// source length and bounds the parse.
//
// The returned reader owns source and closes it. An encapsulated source
// returns ErrStoreReencodeUnsupported; source is then closed too.
func ExplicitLittleEndianStorePayload(source io.ReadSeekCloser, size int64) (io.ReadCloser, error) {
	if source == nil {
		return nil, errors.New("dicomweb: nil STOW-RS payload source")
	}
	header, err := object.ReadPart10Header(source, object.ReadFileOptions{})
	if err != nil {
		_ = source.Close()
		return nil, fmt.Errorf("read STOW-RS payload header: %w", err)
	}
	sourceUID := transfer.NormalizeUID(header.TransferSyntax.UID)
	if !StoreCanReencodeExplicitLittleEndian(sourceUID) {
		_ = source.Close()
		return nil, fmt.Errorf("%w: source transfer syntax %s", ErrStoreReencodeUnsupported, sourceUID)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		_ = source.Close()
		return nil, err
	}
	deflated := sourceUID == transfer.DeflatedExplicitVRLittleEndian.UID
	bigEndian := sourceUID == transfer.ExplicitVRBigEndian.UID
	file, err := object.ReadFileWithOptions(source, storeReencodeReadOptions(size, deflated, bigEndian))
	if err != nil {
		_ = source.Close()
		return nil, fmt.Errorf("read STOW-RS payload: %w", err)
	}
	// The writer reverses the values of a Big Endian data set, deferred ones
	// included, into the Little Endian order of the destination syntax. A
	// malformed value that is not a whole number of units, which the parser
	// accepts, keeps its trailing partial unit as a Little Endian source keeps
	// its bytes, so it does not refuse the upload.
	file.TransferSyntax = transfer.ExplicitVRLittleEndian
	reader, writer := io.Pipe()
	payload := &reencodedStorePayload{reader: reader, done: make(chan struct{}), source: source}
	go func() {
		defer close(payload.done)
		_ = writer.CloseWithError(object.WriteFileWithOptions(writer, file, object.WriteFileOptions{KeepPartialByteOrderUnits: true}))
	}()
	return payload, nil
}

// ExplicitLittleEndianStorePayloadSize returns the length in bytes of the
// payload ExplicitLittleEndianStorePayload returns for source and size, for
// StoreInstance.Size: the re-encoded object can be longer than its source,
// up to storeReencodeDeflateRatio times for a Deflated one. It encodes the
// object once and discards the bytes, so it reads the whole source. Errors
// are those ExplicitLittleEndianStorePayload or its reader would return.
// source is closed.
func ExplicitLittleEndianStorePayloadSize(source io.ReadSeekCloser, size int64) (int64, error) {
	payload, err := ExplicitLittleEndianStorePayload(source, size)
	if err != nil {
		return 0, err
	}
	written, copyErr := io.Copy(io.Discard, payload)
	closeErr := payload.Close()
	if copyErr != nil {
		return 0, fmt.Errorf("re-encode STOW-RS payload: %w", copyErr)
	}
	if closeErr != nil {
		return 0, closeErr
	}
	return written, nil
}

// storeReencodeReadOptions bounds the parse of a source of size bytes. A
// Deflated source holds every value in memory: an inflated stream has no
// source offsets to replay. Other sources defer Pixel Data, which the writer
// streams from the source (reversing Big Endian values while it copies them).
// Little Endian sources also defer other large OB, OD, OF, OW and UN values;
// the parser holds large values of other VRs (OL, OV, SV, UV or UC, for
// instance) in memory. A Big Endian source holds every value but its Pixel
// Data in memory.
//
// No single element can be longer than the whole parse, so an element is
// bounded by the parse limit (the source length, or its inflation budget),
// not by a fixed ceiling: a deferred value is never held in memory, and the
// values held are bounded by that same budget, the memory a materialized
// source needs anyway.
func storeReencodeReadOptions(size int64, deflated, bigEndian bool) object.ReadFileOptions {
	materialize := deflated || bigEndian
	limits := index.DefaultOptions().Limits
	options := object.ReadFileOptions{
		MaxSequenceDepth: limits.MaxSequenceDepth,
		MaxElements:      limits.MaxElements,
		MaxFragments:     limits.MaxFragments,
		DeferPixelData:   !deflated,
	}
	if !materialize {
		options.InlineValueBytesThreshold = 1 << 20
	}
	switch {
	case deflated && size > 0 && size <= math.MaxInt64/storeReencodeDeflateRatio:
		options.MaxTotalBytes = size * storeReencodeDeflateRatio
	case size > 0 && size < math.MaxInt64:
		options.MaxTotalBytes = size + 1
	}
	options.MaxElementBytes = options.MaxTotalBytes
	if options.MaxTotalBytes == 0 && materialize {
		// Without a source length, a value held in memory keeps a ceiling.
		options.MaxElementBytes = storeReencodeUnknownSizeElementBytes
	}
	return options
}

type reencodedStorePayload struct {
	reader *io.PipeReader
	done   chan struct{}
	source io.Closer
	once   sync.Once
	err    error
}

func (p *reencodedStorePayload) Read(data []byte) (int, error) {
	return p.reader.Read(data)
}

// Close stops the encoder, waits for it and closes the source.
func (p *reencodedStorePayload) Close() error {
	p.once.Do(func() {
		_ = p.reader.Close()
		<-p.done
		p.err = p.source.Close()
	})
	return p.err
}
