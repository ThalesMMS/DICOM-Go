package dicomweb

import (
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sync"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// convertedObjectSlack covers what a conversion writes besides the data set
// elements it re-encodes: File Meta Information naming the new transfer
// syntax and this implementation, and the new Pixel Data header and padding.
const convertedObjectSlack = 4 << 10

// SourceOutputBound returns an upper bound on the bytes the server delivers,
// for this request, for an object of storedSize bytes stored in
// transferSyntaxUID.
//
// A backend that checks a collection against its part and response limits
// before yielding the first part, as AcceptsSource lets it check the
// syntaxes, budgets this bound rather than the stored size. On the source
// request of prepared retrieval (ServerOptions.RetrieveTranscode) a lossless
// conversion can deliver several times the stored bytes, decompression in
// particular, and the server enforces its limits only while it writes, once
// the response has begun.
//
// The bound is storedSize when the server delivers the stored bytes. When it
// may convert the object, SourceOutputBound opens it with open, which it
// closes, and reads it up to its Pixel Data. The bound is then the decoded
// samples (the stored Pixel Data value of a native source) plus three times
// every other byte, which covers re-encoding each element, item and
// sequence header in Explicit VR with undefined-length sequences, plus a
// fixed allowance for the File Meta Information. A Deflated source is
// inflated to count its bytes.
//
// It returns ErrResourceLimit for an object larger than the policy's
// conversion input, or whose bound exceeds its conversion output
// (TranscodeLimits.MaxInputBytes and MaxOutputBytes, clipped to
// MaxPartBytes and MaxResponseBytes), ErrUnsupported for an object the
// server cannot deliver in a syntax the client accepts, and errors from
// open, cancellation and the read lease as they are. Like AcceptsSource it
// errs only towards refusal, and an object within the bound can still fail
// on its content. On any other request it is storedSize when
// AcceptsOriginal accepts the syntax.
func (request RetrieveRequest) SourceOutputBound(ctx context.Context, transferSyntaxUID string, storedSize int64, open func() (io.ReadSeekCloser, error)) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if storedSize < 1 {
		return 0, ErrBackend
	}
	n := request.prepared
	if n == nil || n.server == nil || n.server.retrievePolicy == nil {
		if !request.AcceptsOriginal(transferSyntaxUID) {
			return 0, ErrUnsupported
		}
		return storedSize, nil
	}
	limits := n.server.retrievePolicy.Options.Limits
	stored, known := transfer.DefaultRegistry.Get(transfer.NormalizeUID(transferSyntaxUID))
	if !known {
		return 0, ErrUnsupported
	}
	// Prepared retrieval stages every object, converted or not.
	if storedSize > limits.MaxInputBytes {
		return 0, ErrResourceLimit
	}
	converts, err := n.mayConvert(stored, syntaxImpliesLossy(stored))
	if err != nil {
		return 0, err
	}
	if !converts {
		return storedSize, nil
	}
	if open == nil {
		return 0, ErrBackend
	}
	if err := CheckReadLease(ctx); err != nil {
		return 0, err
	}
	source, err := open()
	if err != nil {
		return 0, err
	}
	if source == nil {
		return 0, ErrBackend
	}
	defer source.Close()
	bound, converts, err := n.convertedBound(ctx, contextReadSeeker{ctx: ctx, ReadSeeker: source}, stored, storedSize)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, err
	}
	if err := CheckReadLease(ctx); err != nil {
		return 0, err
	}
	if !converts {
		return storedSize, nil
	}
	if bound > limits.MaxOutputBytes {
		return 0, ErrResourceLimit
	}
	return max(bound, storedSize), nil
}

// mayConvert reports whether negotiation can select a syntax other than the
// stored one for a source in stored. Like AcceptsSource it requires the
// object to be deliverable with the lossy state known so far; an object not
// known to be lossy may still prove lossy, so that state counts towards a
// conversion as well.
func (n *preparedNegotiation) mayConvert(stored transfer.Syntax, lossy bool) (bool, error) {
	target, err := n.server.retrieveTargetFor(stored, lossy, n.accept, n.frames, n.multipart, n.isURI)
	if err != nil {
		return false, ErrUnsupported
	}
	if target.UID != stored.UID {
		return true, nil
	}
	if !lossy {
		if target, err := n.server.retrieveTargetFor(stored, true, n.accept, n.frames, n.multipart, n.isURI); err == nil && target.UID != stored.UID {
			return true, nil
		}
	}
	return false, nil
}

// convertedBound reads source up to its Pixel Data and bounds the converted
// object; false means the attributes it read select the stored syntax.
func (n *preparedNegotiation) convertedBound(ctx context.Context, source io.ReadSeeker, stored transfer.Syntax, storedSize int64) (int64, bool, error) {
	s := n.server
	l := s.retrievePolicy.Options.Limits
	threshold := core.NewTag(0x7FE0, 0x0008) // Float, Double Float and integer Pixel Data
	// Like the reads of delivery, these do not count fragments: the
	// conversion's fragment limit is a conversion limit, which
	// admitConversion applies, so that an object above it is delivered as
	// stored where the client accepts that, exactly as delivery decides.
	opts := object.ReadFileOptions{StopBeforeTag: &threshold, InlineValueBytesThreshold: 64 << 10, DeferNestedValues: true,
		MaxElementBytes: preparedElementBytes(storedSize), MaxTotalBytes: storedSize, MaxElements: l.MaxElements, MaxSequenceDepth: l.MaxDepth}
	if stored.Deflated {
		opts.InlineValueBytesThreshold, opts.DeferNestedValues = 0, false
	}
	file, err := object.ReadFileWithOptions(source, opts)
	if err != nil && !errors.Is(err, object.ErrReadStopped) {
		return 0, false, classifyRetrieveReadError(ctx, err)
	}
	if file == nil || file.Dataset == nil {
		return 0, false, ErrBackend
	}
	defer file.Close()
	if file.TransferSyntax.UID != stored.UID {
		return 0, false, ErrBackend
	}
	lossy := retrieveSourceLossy(file)
	if converts, err := n.mayConvert(stored, lossy); err != nil || !converts {
		return 0, false, err
	}
	if target, err := s.retrieveTargetFor(stored, lossy, n.accept, n.frames, n.multipart, n.isURI); err == nil && target.UID != stored.UID {
		// The conversion limits that depend on the content (frames, pixels,
		// expansion ratio) refuse the object, or deliver it as stored, here
		// as the server will once it reads the object, not after the
		// response has begun.
		delivered, err := n.admitConversion(ctx, source, file, opts, target, lossy)
		if err != nil {
			return 0, false, err
		}
		if delivered.UID == stored.UID {
			return 0, false, nil
		}
	}

	total := storedSize // data set bytes as the conversion reads them
	if stored.Deflated {
		if _, err := source.Seek(0, io.SeekStart); err != nil {
			return 0, false, ErrBackend
		}
		header, err := object.ReadPart10Header(source, object.ReadFileOptions{MaxTotalBytes: storedSize})
		if err != nil {
			return 0, false, classifyRetrieveReadError(ctx, err)
		}
		inflated, err := inflatedBytes(source, header.DataSetOffset, l.MaxOutputBytes)
		if err != nil {
			return 0, false, err
		}
		total = header.DataSetOffset + inflated
	}
	pixelStored, pixelOut := int64(0), int64(0)
	if stop, stopped := file.ReadStop(); stopped && isPixelDataTag(stop.Header.Tag) {
		header := int64(8)
		if stored.ExplicitVR {
			header = 12 // OB, OW, OF and OD headers have the long form
		}
		switch {
		case stop.Header.Length != core.UndefinedLength:
			pixelStored = int64(stop.Header.Length)
		case stored.Encapsulated && !stored.Deflated:
			end, err := encapsulatedPixelEnd(source, stop.Offset+header, storedSize, l.MaxFragments, stored.ByteOrder)
			if err != nil {
				return 0, false, err
			}
			pixelStored = end - stop.Offset - header
		default:
			return 0, false, ErrBackend
		}
		pixelOut = pixelStored + 1
		if stored.Encapsulated {
			if pixelOut, err = decodedPixelBytes(file.Dataset); err != nil {
				return 0, false, err
			}
		}
	}
	nonPixel := total - pixelStored
	if nonPixel < 0 || pixelOut < 0 {
		return 0, false, ErrBackend
	}
	if nonPixel > (math.MaxInt64-convertedObjectSlack-pixelOut)/3 {
		return 0, false, ErrResourceLimit
	}
	return 3*nonPixel + pixelOut + convertedObjectSlack, true, nil
}

// admitConversion applies admitRetrieveConversion to source, read up to its
// Pixel Data as file with opts. An encapsulated source is read again with
// its Pixel Data deferred, as prepared retrieval reads it, so that its
// frames are indexed from the same layout.
func (n *preparedNegotiation) admitConversion(ctx context.Context, source io.ReadSeeker, file *object.File, opts object.ReadFileOptions, target transfer.Syntax, lossy bool) (transfer.Syntax, error) {
	stored := file.TransferSyntax
	if stored.Encapsulated && !stored.Deflated {
		if _, err := source.Seek(0, io.SeekStart); err != nil {
			return transfer.Syntax{}, ErrBackend
		}
		// Read as prepared retrieval reads it, bounded by the conversion input.
		maximum := n.server.retrievePolicy.Options.Limits.MaxInputBytes
		opts.StopBeforeTag, opts.DeferPixelData, opts.MaxPixelDataBytes, opts.MaxTotalBytes = nil, true, maximum, maximum
		deferred, err := object.ReadFileWithOptions(source, opts)
		if err != nil {
			return transfer.Syntax{}, classifyRetrieveReadError(ctx, err)
		}
		if deferred == nil || deferred.Dataset == nil {
			return transfer.Syntax{}, ErrBackend
		}
		defer deferred.Close()
		if deferred.TransferSyntax.UID != stored.UID {
			return transfer.Syntax{}, ErrBackend
		}
		file = deferred
	}
	return n.server.admitRetrieveConversion(ctx, file, &seekingReaderAt{source: source}, target, lossy, n.accept, n.frames, n.multipart, n.isURI)
}

// seekingReaderAt reads at an offset of a seekable source, one read at a
// time.
type seekingReaderAt struct {
	mu     sync.Mutex
	source io.ReadSeeker
}

func (r *seekingReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.source.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.ReadFull(r.source, p)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	return n, err
}

func isPixelDataTag(tag core.Tag) bool {
	return tag == core.TagPixelData || tag == core.NewTag(0x7FE0, 0x0008) || tag == core.NewTag(0x7FE0, 0x0009)
}

// encapsulatedPixelEnd follows the items of an encapsulated Pixel Data value
// starting at start and returns the offset just after its delimiter.
func encapsulatedPixelEnd(source io.ReadSeeker, start, storedSize int64, maxFragments int, order binary.ByteOrder) (int64, error) {
	if order == nil {
		order = binary.LittleEndian
	}
	position := start
	var item [8]byte
	for range maxFragments + 2 { // the offset table, the fragments and the delimiter
		if _, err := source.Seek(position, io.SeekStart); err != nil {
			return 0, ErrBackend
		}
		if _, err := io.ReadFull(source, item[:]); err != nil {
			return 0, ErrBackend
		}
		group, element, length := order.Uint16(item[0:]), order.Uint16(item[2:]), order.Uint32(item[4:])
		position += int64(len(item))
		switch {
		case group == 0xFFFE && element == 0xE0DD:
			return position, nil
		case group == 0xFFFE && element == 0xE000 && length != uint32(core.UndefinedLength):
			position += int64(length)
			if position > storedSize {
				return 0, ErrBackend
			}
		default:
			return 0, ErrBackend
		}
	}
	return 0, ErrResourceLimit
}

// decodedPixelBytes bounds the native Pixel Data decoding produces, with
// every sample of every frame: a decoder delivers 4:2:2 colour in full.
func decodedPixelBytes(dataset *object.Object) (int64, error) {
	metadata, err := pixeldata.ExtractMetadata(dataset)
	if err != nil {
		return 0, ErrUnsupported
	}
	frames := max(int64(metadata.NumberOfFrames), 1)
	if metadata.BitsAllocated < 8 {
		total := metadata.TotalSize()
		if total <= 0 {
			return 0, ErrResourceLimit
		}
		return total + 1, nil
	}
	frame := int64(metadata.Rows) * int64(metadata.Columns) * int64(metadata.SamplesPerPixel) * int64(metadata.BytesPerSample())
	if frame <= 0 {
		return 0, ErrUnsupported
	}
	if frames > (math.MaxInt64-1)/frame {
		return 0, ErrResourceLimit
	}
	return frame*frames + 1, nil
}

// inflatedBytes counts the bytes of the deflated data set at offset, up to
// maximum+1.
func inflatedBytes(source io.ReadSeeker, offset, maximum int64) (int64, error) {
	if _, err := source.Seek(offset, io.SeekStart); err != nil {
		return 0, ErrBackend
	}
	inflater := flate.NewReader(source)
	defer inflater.Close()
	count, err := io.Copy(io.Discard, io.LimitReader(inflater, maximum+1))
	if err != nil {
		return 0, ErrUnsupported
	}
	return count, nil
}

// contextReadSeeker stops reading once ctx ends.
type contextReadSeeker struct {
	ctx context.Context
	io.ReadSeeker
}

func (r contextReadSeeker) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.ReadSeeker.Read(p)
}
