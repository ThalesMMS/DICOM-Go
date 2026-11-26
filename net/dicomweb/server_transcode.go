package dicomweb

import (
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

// RetrieveTranscodePolicy configures the opt-in identity-preserving retrieval
// profile. Zero limits use pixeldata's finite defaults, clipped by ServerLimits.
// Registries/codecs are borrowed, must remain stable for the Server lifetime,
// and must honor context. All option values/slices are copied by NewServer.
// Newly lossy encoding, UID changes, repairs, rewrites and verification opt-out
// are refused. An acceptable stored lossy representation may pass through.
type RetrieveTranscodePolicy struct {
	Options pixeldata.TranscodeOptions
	// TargetTransferSyntaxUIDs is a finite allowlist for actual conversions.
	// Empty means Explicit VR Little Endian. Stored syntax pass-through does not
	// require an encoder or this allowlist; Accept must still allow that syntax.
	TargetTransferSyntaxUIDs []string
	// MaxConcurrentJobs bounds staging, codec work and response frame retention.
	// Zero means two, clipped to MaxConcurrentRequests. The maximum is 64.
	MaxConcurrentJobs int
	// PreferStoredSyntax delivers the stored bytes when the client accepts
	// them as much as a conversion, as for `transfer-syntax=*`. By default
	// such a tie selects the first allowlisted target.
	PreferStoredSyntax bool
}

func normalizeRetrieveTranscodePolicy(policy *RetrieveTranscodePolicy, limits ServerLimits, uri bool) (*RetrieveTranscodePolicy, error) {
	if policy == nil && !uri {
		return nil, nil
	}
	var p RetrieveTranscodePolicy
	if policy != nil {
		p = *policy
	}
	if p.MaxConcurrentJobs < 0 || p.MaxConcurrentJobs > 64 || len(p.TargetTransferSyntaxUIDs) > 64 {
		return nil, fmt.Errorf("%w: retrieval policy limit", ErrInvalidServerOptions)
	}
	if p.MaxConcurrentJobs == 0 {
		p.MaxConcurrentJobs = 2
	}
	p.MaxConcurrentJobs = min(p.MaxConcurrentJobs, limits.MaxConcurrentRequests)
	o := p.Options
	// Validate finite configuration before copying caller-owned buffers. Invalid
	// policies must not become silently acceptable on an as-stored response.
	if (len(o.WriteOptions.Preamble) != 0 && len(o.WriteOptions.Preamble) != 128) || o.UIDPolicy > pixeldata.TranscodeUIDPreserve || o.PlanarOutput > pixeldata.PlanarOutputPlanar || o.OffsetTable > object.OffsetTableEmpty || o.Durability > pixeldata.TranscodeAtomic || o.MaxFragmentBytes < 0 || o.MaxFragmentBytes > 0xFFFFFFFE || (o.MaxFragmentBytes != 0 && (o.MaxFragmentBytes < 2 || o.MaxFragmentBytes%2 != 0)) || o.Pipeline.Workers < 0 || o.Pipeline.MaxInFlightFrames < 0 || o.Pipeline.MaxInFlightBytes < 0 || o.Pipeline.MaxEncodedFrameBytes < 0 {
		return nil, fmt.Errorf("%w: retrieval output policy", ErrInvalidServerOptions)
	}
	if err := object.ValidateSerializationOptions(transfer.DeflatedExplicitVRLittleEndian, o.WriteOptions.DeflateLevel, o.WriteOptions.Padding); err != nil {
		return nil, fmt.Errorf("%w: retrieval serialization policy", ErrInvalidServerOptions)
	}
	if o.WriteOptions.SequenceLength > object.SequenceLengthDefined || o.WriteOptions.GroupLength > object.GroupLengthRemove || o.WriteOptions.MetaMode < object.FileMetaUpdate || o.WriteOptions.MetaMode > object.FileMetaPreserve || o.WriteOptions.AETitles.Validate() != nil {
		return nil, fmt.Errorf("%w: retrieval file policy", ErrInvalidServerOptions)
	}
	if o.AllowLossy || o.ForceReencode || o.SecondaryCapture || o.UIDPolicy == pixeldata.TranscodeUIDAlways || o.SkipVerification || o.MaxPixelError != nil || o.AverageBlockSize != 0 || o.Rewrite != nil || o.RecoverUN != nil || o.Recovery != (pixeldata.DecodeRecovery{}) || o.ExtractEmbeddedOverlays || o.CodestreamTranscoder != nil || o.WriteOptions.BulkDataResolver != nil || o.WriteOptions.AllowPartial || o.WriteOptions.DefaultMissingTransferSyntax || o.WriteOptions.OverrideMissingTransferSyntax != "" || o.WriteOptions.OmitReconciledDataSetSOPUIDs {
		return nil, fmt.Errorf("%w: retrieval requires unchanged identity and verified samples", ErrInvalidServerOptions)
	}
	o.UIDPolicy = pixeldata.TranscodeUIDPreserve
	o.WriteOptions.Preamble = append([]byte(nil), o.WriteOptions.Preamble...)
	if o.WriteOptions.DeflateLevel != nil {
		v := *o.WriteOptions.DeflateLevel
		o.WriteOptions.DeflateLevel = &v
	}
	resolved, err := pixeldata.ResolveTranscodeLimits(o.Limits)
	if err != nil {
		return nil, fmt.Errorf("%w: retrieval transcode limits", ErrInvalidServerOptions)
	}
	resolved.MaxInputBytes = min(resolved.MaxInputBytes, limits.MaxPartBytes, math.MaxInt64-1)
	resolved.MaxOutputBytes = min(resolved.MaxOutputBytes, limits.MaxPartBytes, limits.MaxResponseBytes, math.MaxInt64-1)
	resolved.MaxFrames = min(resolved.MaxFrames, limits.MaxFrames)
	resolved.MaxElements = min(resolved.MaxElements, limits.MaxJSONValues)
	resolved.MaxDepth = min(resolved.MaxDepth, limits.MaxJSONDepth)
	resolved.MaxDuration = min(resolved.MaxDuration, limits.MaxDuration)
	o.Limits = resolved
	if _, err := pixeldata.ResolveTranscodeVerificationOptions(o); err != nil {
		return nil, fmt.Errorf("%w: retrieval verification policy", ErrInvalidServerOptions)
	}
	p.Options = o
	if len(p.TargetTransferSyntaxUIDs) == 0 {
		p.TargetTransferSyntaxUIDs = []string{transfer.ExplicitVRLittleEndian.UID}
	} else {
		p.TargetTransferSyntaxUIDs = append([]string(nil), p.TargetTransferSyntaxUIDs...)
	}
	seen := make(map[string]bool, len(p.TargetTransferSyntaxUIDs))
	for _, uid := range p.TargetTransferSyntaxUIDs {
		if len(uid) > limits.MaxUIDBytes || !webServiceTransferSyntaxAllowed(uid) || transfer.NormalizeUID(uid) != uid || seen[uid] {
			return nil, fmt.Errorf("%w: retrieval target syntax", ErrInvalidServerOptions)
		}
		seen[uid] = true
		syntax, _ := transfer.DefaultRegistry.Get(uid)
		if err := object.ValidateSerializationOptions(syntax, o.WriteOptions.DeflateLevel, o.WriteOptions.Padding); err != nil {
			return nil, fmt.Errorf("%w: retrieval target serialization policy", ErrInvalidServerOptions)
		}
	}
	return &p, nil
}

// retrieveLeaseCheckBytes is how much of a staged file prepared retrieval
// reads between two validations of the read lease. Parsing reads a data set
// a few bytes at a time, and each validation can be costly for the
// application (reading its policy, for one), so validating every read made
// a small object cost thousands of validations.
const retrieveLeaseCheckBytes = 256 << 10

// retrieveContextFile preserves seek/random access for deferred parsing. Native
// filesystem calls are finite but not context-interruptible; checks surround IO.
// Every access sees cancellation, an earlier denial and the lease's expiry;
// the lease is validated before the first access and again once
// retrieveLeaseCheckBytes more bytes were read. Response bytes are checked
// separately as they are written.
type retrieveContextFile struct {
	File stagedSource
	ctx  context.Context
	// mu guards the count: a frame reader can call ReadAt concurrently.
	mu        sync.Mutex
	validated bool
	unchecked int64
	// reads counts the reads of the file, which tests bound.
	reads atomic.Int64
}

func (f *retrieveContextFile) check(read int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unchecked += int64(read)
	if f.validated && f.unchecked < retrieveLeaseCheckBytes {
		lease, _ := f.ctx.Value(readLeaseKey{}).(*requestReadLease)
		if lease == nil {
			return f.ctx.Err()
		}
		if err := lease.contextError(f.ctx); err != nil {
			return err
		}
		if !lease.validAt(time.Now()) {
			return lease.deny()
		}
		return nil
	}
	if err := CheckReadLease(f.ctx); err != nil {
		return err
	}
	f.validated, f.unchecked = true, 0
	return nil
}

// Close closes the staged object.
func (f *retrieveContextFile) Close() error { return f.File.Close() }

// stagedSource is a staged object: a file of the spool directory or, for a
// small object, memory.
type stagedSource interface {
	io.Reader
	io.Seeker
	io.ReaderAt
	io.Closer
}

// retrieveMemoryStageBytes is the largest object prepared retrieval stages in
// memory rather than in a file of the spool directory. Creating, opening and
// removing a job directory and its file cost more than parsing a small
// object, so a series of hundreds of small objects took seconds longer to
// deliver than without the policy. A staged object is written to a file
// only when it is converted. Prepared jobs are bounded by
// RetrieveTranscodePolicy.MaxConcurrentJobs, and so is this memory.
const retrieveMemoryStageBytes = 8 << 20

// memoryStaged is an object staged in memory.
type memoryStaged struct{ *bytes.Reader }

func (memoryStaged) Close() error { return nil }

func (f *retrieveContextFile) Read(p []byte) (int, error) {
	f.reads.Add(1)
	if err := f.check(0); err != nil {
		return 0, err
	}
	n, e := f.File.Read(p)
	if err := f.check(n); err != nil {
		return 0, err
	}
	return n, e
}
func (f *retrieveContextFile) Seek(o int64, w int) (int64, error) {
	if err := f.check(0); err != nil {
		return 0, err
	}
	return f.File.Seek(o, w)
}
func (f *retrieveContextFile) ReadAt(p []byte, o int64) (int, error) {
	f.reads.Add(1)
	if err := f.check(0); err != nil {
		return 0, err
	}
	n, e := f.File.ReadAt(p, o)
	if err := f.check(n); err != nil {
		return 0, err
	}
	return n, e
}

// retrieveBufferBytes is the read size of the buffer the parsers of prepared
// retrieval read a staged file through.
const retrieveBufferBytes = 64 << 10

// bufferedRetrieveFile reads a staged file through a buffer, for the parsers
// of prepared retrieval: they read a few bytes for each element and item
// header, and each read of the file is a system call and a lease check, so
// an object of 100,000 items took seconds to parse. Its reads of the file
// pass the lease and context checks of retrieveContextFile; its own reads
// see cancellation. It tracks its offset itself and reads the file with
// ReadAt, so it leaves the file's offset alone.
type bufferedRetrieveFile struct {
	file *retrieveContextFile
	size int64
	// mu guards the buffer and offset: deferred values may be read with
	// ReadAt while a parse runs.
	mu     sync.Mutex
	offset int64
	start  int64 // file offset of buffer[0]
	buffer []byte
}

func newBufferedRetrieveFile(file *retrieveContextFile, size int64) *bufferedRetrieveFile {
	return &bufferedRetrieveFile{file: file, size: size, buffer: make([]byte, 0, retrieveBufferBytes)}
}

// readAtLocked copies from offset into p, through the buffer for reads
// shorter than it.
func (b *bufferedRetrieveFile) readAtLocked(p []byte, offset int64) (int, error) {
	if err := b.file.ctx.Err(); err != nil {
		return 0, err
	}
	if offset < 0 {
		return 0, ErrBackend
	}
	read := 0
	for read < len(p) && offset < b.size {
		if offset >= b.start && offset < b.start+int64(len(b.buffer)) {
			n := copy(p[read:], b.buffer[offset-b.start:])
			read, offset = read+n, offset+int64(n)
			continue
		}
		if len(p)-read >= cap(b.buffer) {
			n, err := b.file.ReadAt(p[read:], offset)
			read, offset = read+n, offset+int64(n)
			if err != nil && !errors.Is(err, io.EOF) {
				return read, err
			}
			if n == 0 {
				break
			}
			continue
		}
		n, err := b.file.ReadAt(b.buffer[:cap(b.buffer)], offset)
		b.start, b.buffer = offset, b.buffer[:n]
		if err != nil && !errors.Is(err, io.EOF) {
			return read, err
		}
		if n == 0 {
			break
		}
	}
	if read < len(p) {
		return read, io.EOF
	}
	return read, nil
}

func (b *bufferedRetrieveFile) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	n, err := b.readAtLocked(p, b.offset)
	b.offset += int64(n)
	if n > 0 && errors.Is(err, io.EOF) {
		err = nil
	}
	return n, err
}

func (b *bufferedRetrieveFile) ReadAt(p []byte, offset int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.readAtLocked(p, offset)
}

func (b *bufferedRetrieveFile) Seek(offset int64, whence int) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.file.check(0); err != nil {
		return 0, err
	}
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += b.offset
	case io.SeekEnd:
		offset += b.size
	default:
		return 0, ErrBackend
	}
	if offset < 0 {
		return 0, ErrBackend
	}
	b.offset = offset
	return offset, nil
}

type preparedRetrieve struct {
	file   *retrieveContextFile
	parsed *object.File
	ref    InstanceRef
	size   int64
}

func (s *Server) readPreparedFile(ctx context.Context, path string, maximum int64, ref InstanceRef) (*preparedRetrieve, error) {
	if err := CheckReadLease(ctx); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrBackend
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, ErrBackend
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, ErrResourceLimit
	}
	return s.readPreparedSource(ctx, f, info.Size(), maximum, ref)
}

// readPreparedSource checks and parses a staged object of size bytes, which
// it closes when it fails.
func (s *Server) readPreparedSource(ctx context.Context, staged stagedSource, size, maximum int64, ref InstanceRef) (*preparedRetrieve, error) {
	if err := CheckReadLease(ctx); err != nil {
		_ = staged.Close()
		return nil, err
	}
	source := &retrieveContextFile{File: staged, ctx: ctx}
	fail := func(err error) (*preparedRetrieve, error) { _ = staged.Close(); return nil, err }
	if size < 0 || size > maximum {
		return fail(ErrResourceLimit)
	}
	l := s.retrievePolicy.Options.Limits
	// A value is bounded by the object, not MaxMetadataBytes, and the
	// fragments are not counted; see preparedElementBytes.
	opts := object.ReadFileOptions{DeferPixelData: true, InlineValueBytesThreshold: 64 << 10, DeferNestedValues: true, MaxElementBytes: preparedElementBytes(maximum), MaxPixelDataBytes: maximum, MaxTotalBytes: maximum, MaxElements: l.MaxElements, MaxSequenceDepth: l.MaxDepth}
	buffered := newBufferedRetrieveFile(source, size)
	header, err := object.ReadPart10Header(buffered, opts)
	if err != nil {
		return fail(classifyRetrieveReadError(ctx, err))
	}
	metaInstance, err := s.preparedSingleUID(header.Meta, core.NewTag(0x0002, 0x0003))
	if err != nil || metaInstance != ref.SOPInstanceUID {
		return fail(ErrBackend)
	}
	metaClass, err := s.preparedSingleUID(header.Meta, core.NewTag(0x0002, 0x0002))
	if err != nil {
		return fail(err)
	}
	metaSyntax, err := s.preparedSingleUID(header.Meta, core.NewTag(0x0002, 0x0010))
	if err != nil || metaSyntax != header.TransferSyntax.UID {
		return fail(ErrBackend)
	}
	if err := s.scanPreparedUIDOccurrences(ctx, buffered, header, maximum); err != nil {
		return fail(err)
	}
	if _, err = buffered.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	if header.TransferSyntax.Deflated {
		// Logical deflated offsets cannot support deferred replay. This pass
		// validates metadata and consumes bounded pixels without retaining them;
		// TranscodePath separately owns any bounded whole-object conversion.
		opts.DeferPixelData, opts.DeferNestedValues, opts.InlineValueBytesThreshold = false, false, 0
		opts.SkipPixelData = true
	}
	parsed, err := object.ReadFileWithOptions(buffered, opts)
	if err != nil {
		if ctx.Err() != nil {
			return fail(ctx.Err())
		}
		return fail(classifyRetrieveReadError(ctx, err))
	}
	if parsed == nil || parsed.Dataset == nil || parsed.Meta == nil {
		return fail(ErrBackend)
	}
	for _, field := range []struct {
		tag      core.Tag
		expected string
	}{{core.NewTag(0x0020, 0x000d), ref.StudyInstanceUID}, {core.NewTag(0x0020, 0x000e), ref.SeriesInstanceUID}, {core.NewTag(0x0008, 0x0018), ref.SOPInstanceUID}, {core.NewTag(0x0008, 0x0016), metaClass}} {
		value, err := s.preparedSingleUID(parsed.Dataset, field.tag)
		if err != nil || value != field.expected {
			return fail(ErrBackend)
		}
	}
	if _, err = source.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	return &preparedRetrieve{file: source, parsed: parsed, ref: ref, size: size}, nil
}

// preparedElementBytes is the element limit of the parses of prepared
// retrieval for an object of at most maximum bytes: the object itself. Its
// parses keep values longer than their inline threshold out of memory, and
// MaxTotalBytes bounds the values they hold, as for a Deflated source that
// is inflated, so a per-element limit below the object would only refuse
// objects, including objects delivered as stored, that a server without the
// policy delivers. The backend's preflight (SourceOutputBound) does not read
// an object delivered as stored and so could not apply a lower limit before
// the first part. For the same reason these parses do not count Pixel Data
// fragments, which they skip: an object delivered as stored is never
// decoded, and a conversion is bounded by TranscodeLimits.MaxFragments, in
// TranscodePath and in SourceOutputBound before the first part.
func preparedElementBytes(maximum int64) int64 {
	return maximum
}

// UI is ASCII and VM1 for these identity fields. Read only bounded inline
// values; never use GetString's first-component behavior or deferred replay.
func (s *Server) preparedSingleUID(obj *object.Object, tag core.Tag) (string, error) {
	e, ok := obj.Get(tag)
	if !ok || e.Header.VR != core.VRUI {
		return "", ErrBackend
	}
	maximum := min(s.limits.MaxUIDBytes, 64)
	maximum += maximum % 2 // encoded UI padding must still fit its 64-byte VR
	if e.Header.HasLength() && uint64(e.Header.Length) > uint64(maximum) {
		return "", ErrBackend
	}
	var uid string
	switch value := e.Value.(type) {
	case core.RawValue:
		if len(value) > maximum {
			return "", ErrBackend
		}
		uid = string(value)
	case core.StringValue:
		if len(value) != 1 || len(value[0]) > maximum {
			return "", ErrBackend
		}
		uid = value[0]
	default:
		return "", ErrBackend
	}
	uid = core.NormalizeUID(uid)
	if s.validateUID(uid) != nil {
		return "", ErrBackend
	}
	return uid, nil
}

// Object deliberately indexes parsed tags by identity. A separate selective
// parser pass catches duplicate identity occurrences in the original bytes,
// before pass-through can expose an ambiguous object to a different parser.
// Only seven top-level UIDs are counted; nested item identities are not bound
// to the requested instance. Pixel values are skipped, never decoded.
func (s *Server) scanPreparedUIDOccurrences(ctx context.Context, source *bufferedRetrieveFile, header object.Part10Header, maximum int64) error {
	l := s.retrievePolicy.Options.Limits
	opts := parser.ReaderOptions{Dictionary: std.Dictionary, MaxElementBytes: preparedElementBytes(maximum), MaxPixelDataBytes: maximum, MaxTotalBytes: maximum, MaxElements: l.MaxElements, MaxSequenceDepth: l.MaxDepth, SkipPixelData: true, InlineValueBytesThreshold: 65, DeferNestedValues: true}
	scan := func(input io.Reader, syntax transfer.Syntax, base int64, tags []core.Tag) error {
		seen := make(map[core.Tag]bool, len(tags))
		opts.BaseOffset = base
		reader, err := parser.NewSelectiveReader(ctx, input, syntax, opts, parser.SelectiveReaderOptions{Select: func(_ context.Context, path validation.Path, h core.ElementHeader, _ int64) (parser.SelectiveDisposition, error) {
			if len(path) == 1 {
				for _, tag := range tags {
					if h.Tag == tag {
						maximumUID := min(s.limits.MaxUIDBytes, 64)
						maximumUID += maximumUID % 2
						if seen[tag] || h.VR != core.VRUI || h.Length.IsUndefined() || uint64(h.Length) > uint64(maximumUID) {
							return parser.SelectiveSkip, ErrBackend
						}
						seen[tag] = true
					}
				}
			}
			if h.Tag == core.TagPixelData {
				return parser.SelectiveSkip, nil
			}
			if h.VR == core.VRSQ || h.Length.IsUndefined() || dictionary.IsPrivateCreatorTag(h.Tag) {
				return parser.SelectiveMaterialize, nil
			}
			return parser.SelectiveSkip, nil
		}})
		if err != nil {
			return classifyRetrieveReadError(ctx, err)
		}
		for {
			_, err := reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return classifyRetrieveReadError(ctx, err)
			}
		}
		for _, tag := range tags {
			if !seen[tag] {
				return ErrBackend
			}
		}
		return ctx.Err()
	}
	if header.DataSetOffset < 132 || header.DataSetOffset > maximum {
		return ErrResourceLimit
	}
	meta := io.NewSectionReader(source, 132, header.DataSetOffset-132)
	if err := scan(meta, transfer.ExplicitVRLittleEndian, 132, []core.Tag{core.NewTag(0x0002, 0x0002), core.NewTag(0x0002, 0x0003), core.NewTag(0x0002, 0x0010)}); err != nil {
		return err
	}
	if _, err := source.Seek(header.DataSetOffset, io.SeekStart); err != nil {
		return err
	}
	var input io.Reader = source
	if header.TransferSyntax.Deflated {
		inflated := flate.NewReader(source)
		defer inflated.Close()
		input = inflated
	}
	return scan(input, header.TransferSyntax, header.DataSetOffset, []core.Tag{core.NewTag(0x0008, 0x0016), core.NewTag(0x0008, 0x0018), core.NewTag(0x0020, 0x000d), core.NewTag(0x0020, 0x000e)})
}

func classifyRetrieveReadError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	for _, limit := range []error{parser.ErrMaxDepthExceeded, parser.ErrMaxElementBytesExceeded, parser.ErrMaxElementsExceeded, parser.ErrMaxFragmentsExceeded, parser.ErrMaxPixelDataBytesExceeded, parser.ErrMaxTotalBytesExceeded} {
		if errors.Is(err, limit) {
			return ErrResourceLimit
		}
	}
	return ErrBackend
}

func frameMediaForSyntax(uid string) string {
	for _, media := range []string{"application/octet-stream", "application/x-deflate", "image/jpeg", "image/dicom-rle", "image/jls", "image/jp2", "image/jpx", "image/jphc", "image/jxl", "video/mpeg", "video/mp4", "video/h265"} {
		if frameMediaTypeMatchesTransferSyntax(media, uid) {
			return media
		}
	}
	return ""
}

// preparedNegotiation is what prepared retrieval negotiates for the client,
// carried on the as-stored source request it sends the backend.
type preparedNegotiation struct {
	server                   *Server
	accept                   []MediaPreference
	frames, multipart, isURI bool
}

// AcceptsSource reports whether the server can deliver an object stored in
// transferSyntaxUID for this request. On the source request of prepared
// retrieval (ServerOptions.RetrieveTranscode), whose Accept asks for the
// stored bytes, it applies the client's preferences and the policy's
// lossless conversions, so Implicit VR Little Endian and Explicit VR Big
// Endian sources count when the client accepts Explicit VR Little Endian.
//
// It errs only towards false, so that a backend can call it for every member
// of a collection before yielding the first part and never start a response
// it cannot finish. A stored lossy object may pass through where the client
// omits the transfer syntax; AcceptsSource counts that exception only for a
// syntax that is lossy by itself (JPEG Baseline and Extended, video), since
// the Lossy Image Compression attribute of other syntaxes is known once the
// object is read. An object it accepts can still fail on its content, as a
// corrupt or undecodable codestream does. On any other request it is
// AcceptsOriginal.
func (request RetrieveRequest) AcceptsSource(transferSyntaxUID string) bool {
	n := request.prepared
	if n == nil || n.server == nil || n.server.retrievePolicy == nil {
		return request.AcceptsOriginal(transferSyntaxUID)
	}
	stored, known := transfer.DefaultRegistry.Get(transfer.NormalizeUID(transferSyntaxUID))
	if !known {
		return false
	}
	_, err := n.server.retrieveTargetFor(stored, syntaxImpliesLossy(stored), n.accept, n.frames, n.multipart, n.isURI)
	return err == nil
}

// syntaxImpliesLossy is what the transfer syntax alone says about loss.
func syntaxImpliesLossy(syntax transfer.Syntax) bool {
	return pixeldata.DetectDataSetLossyState(object.New(nil), syntax).Lossy
}

// retrieveSourceLossy is the lossy state negotiation assumes for a read
// source. A stored object counts as lossy at least when its syntax does, so
// an object AcceptsSource accepted negotiates the same way once read.
func retrieveSourceLossy(source *object.File) bool {
	return pixeldata.DetectLossyState(source).Lossy || syntaxImpliesLossy(source.TransferSyntax)
}

func (s *Server) retrieveTargetFor(source transfer.Syntax, storedLossy bool, accept []MediaPreference, frames, multipartRequired, uri bool) (transfer.Syntax, error) {
	// Stored pass-through remains available, including lossy and media payloads.
	candidates := append(append([]string(nil), s.retrievePolicy.TargetTransferSyntaxUIDs...), source.UID)
	return s.retrieveTargetAmong(candidates, source, storedLossy, accept, frames, multipartRequired, uri)
}

// admitRetrieveConversion decides what prepared retrieval delivers for a
// read source that negotiation would convert to target. It applies the
// policy's content-derived transcode limits (frames, pixels and expansion
// ratio, see pixeldata.CheckTranscodeDecodeLimits) before anything is
// decoded or sent. When they refuse the conversion it delivers the stored
// syntax if the client accepts that syntax too, as it would when no decoder
// were configured, and answers ErrResourceLimit otherwise. SourceOutputBound
// and prepareRetrievePart both decide through it, so a collection the
// backend checked before its first part is delivered as it was budgeted.
// pixelSource is the seekable input file was read from with its Pixel Data
// deferred; a native source needs neither.
func (s *Server) admitRetrieveConversion(ctx context.Context, file *object.File, pixelSource io.ReaderAt, target transfer.Syntax, lossy bool, accept []MediaPreference, frames, multipartRequired, uri bool) (transfer.Syntax, error) {
	err := pixeldata.CheckTranscodeDecodeLimits(ctx, file, pixelSource, target, s.retrievePolicy.Options)
	if err == nil {
		return target, nil
	}
	if ctx.Err() != nil {
		return transfer.Syntax{}, ctx.Err()
	}
	if leaseErr := CheckReadLease(ctx); leaseErr != nil {
		return transfer.Syntax{}, leaseErr
	}
	if !pixeldata.IsTranscodeLimitError(err) {
		return transfer.Syntax{}, classifyRetrieveTranscodeError(ctx, err)
	}
	stored := file.TransferSyntax
	if delivered, err := s.retrieveTargetAmong([]string{stored.UID}, stored, lossy, accept, frames, multipartRequired, uri); err == nil {
		return delivered, nil
	}
	return transfer.Syntax{}, ErrResourceLimit
}

func (s *Server) retrieveTargetAmong(candidates []string, source transfer.Syntax, storedLossy bool, accept []MediaPreference, frames, multipartRequired, uri bool) (transfer.Syntax, error) {
	stored := source.UID
	bestQuality := -1.0
	bestSpecificity := -1
	var best transfer.Syntax
	for _, uid := range candidates {
		syntax, known := transfer.DefaultRegistry.Get(uid)
		if !known || !webServiceTransferSyntaxAllowed(uid) {
			continue
		}
		if uid != stored {
			if source.MediaPayload {
				continue // no video/image extraction or implicit decoder substitution
			}
			if source.Encapsulated && source.UID != transfer.EncapsulatedUncompressedExplicitVRLittleEndian.UID && !source.MediaPayload {
				registry := s.retrievePolicy.Options.DecoderRegistry
				if registry == nil {
					continue
				}
				if _, ok := registry.GetCodec(stored); !ok {
					continue
				}
			}
			if syntax.Encapsulated && syntax.UID != transfer.EncapsulatedUncompressedExplicitVRLittleEndian.UID {
				registry := s.retrievePolicy.Options.EncoderRegistry
				if registry == nil {
					continue
				}
				encoder, ok := registry.GetEncoder(uid)
				if !ok || encoder == nil || !encoder.Capabilities().Lossless {
					continue
				}
				decoder := s.retrievePolicy.Options.DecoderRegistry
				if decoder == nil {
					continue
				}
				if _, ok := decoder.GetCodec(uid); !ok {
					continue // verified encoded output requires its decoder as well
				}
			}
		}
		media := "application/dicom"
		if frames {
			if syntax.MediaPayload {
				continue // video is one representation, not independent image frames
			}
			media = frameMediaForSyntax(uid)
			if media == "" {
				continue
			}
		}
		// URI carries an explicit/default syntax. RS objects default to EVRLE,
		// with the stored lossy exception. Frames default separately per MIME.
		filtered := make([]MediaPreference, 0, len(accept))
		for _, p := range accept {
			// A MIME exclusion applies to every matching encoding. Only positive
			// preferences without a syntax are narrowed to the default encoding.
			if p.TransferSyntaxUID == "" && p.Quality > 0 {
				if frames {
					if defaultUID := defaultFrameSyntax(p.MediaType); defaultUID != "" && uid != defaultUID {
						continue
					}
				} else if uid != transfer.ExplicitVRLittleEndian.UID && !(uid == stored && storedLossy && !uri) {
					continue
				}
			}
			filtered = append(filtered, p)
		}
		preference, ok := matchingMediaPreference(filtered, media, uid, multipartRequired)
		if !ok {
			continue
		}
		specificity, _ := mediaPreferenceSpecificity(preference, media, uid)
		storedTie := s.retrievePolicy.PreferStoredSyntax && uid == stored && preference.Quality == bestQuality && specificity == bestSpecificity
		if preference.Quality > bestQuality || (preference.Quality == bestQuality && specificity > bestSpecificity) || storedTie {
			best, bestQuality, bestSpecificity = syntax, preference.Quality, specificity
		}
	}
	if bestQuality < 0 {
		return transfer.Syntax{}, ErrUnsupported
	}
	return best, nil
}

func (s *Server) prepareRetrievePart(ctx context.Context, request RetrieveRequest, part RetrievePart, frames, uri bool, yield func(*preparedRetrieve) error) error {
	if part.Reader == nil {
		return ErrBackend
	}
	return withReaderClosed(part.Reader, func() (err error) {
		if err := CheckReadLease(ctx); err != nil {
			return err
		}
		l := s.retrievePolicy.Options.Limits
		if part.Size < -1 || part.Size > l.MaxInputBytes {
			return ErrResourceLimit
		}
		if _, err := normalizedDICOMPartContentType(part.ContentType, part.TransferSyntaxUID); err != nil {
			return ErrBackend
		}
		declaredSyntax := part.TransferSyntaxUID
		if part.ContentType != "" {
			_, params, err := mime.ParseMediaType(part.ContentType)
			if err != nil {
				return ErrBackend
			}
			for key := range params {
				if key != "transfer-syntax" {
					return ErrBackend
				}
			}
			if uid := params["transfer-syntax"]; uid != "" {
				if declaredSyntax != "" && declaredSyntax != uid {
					return ErrBackend
				}
				declaredSyntax = uid
			}
		}
		ref := part.Ref
		if request.Level == RetrieveLevelInstance && ref == (InstanceRef{}) {
			ref = InstanceRef{StudyInstanceUID: request.StudyInstanceUID, SeriesInstanceUID: request.SeriesInstanceUID, SOPInstanceUID: request.SOPInstanceUID}
		}
		if !retrieveRefMatches(request, ref) {
			return ErrBackend
		}
		for _, uid := range []string{ref.StudyInstanceUID, ref.SeriesInstanceUID, ref.SOPInstanceUID} {
			if s.validateUID(uid) != nil {
				return ErrBackend
			}
		}
		select {
		case s.retrieveSem <- struct{}{}:
			defer func() { <-s.retrieveSem }()
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := CheckReadLease(ctx); err != nil {
			return err
		}
		// The private job directory is made when a file is first needed.
		directory, activeKey := "", ""
		defer func() {
			if directory == "" {
				return
			}
			if os.RemoveAll(directory) != nil {
				err = errors.Join(err, ErrBackend)
			}
			activeRetrieveDirectories.Delete(activeKey)
		}() // private server-created job directory only
		spoolFile := func(name string, write func(io.Writer) error) (string, error) {
			if directory == "" {
				made, err := os.MkdirTemp(s.options.SpoolDirectory, ".dicomweb-retrieve-")
				if err != nil {
					return "", ErrBackend
				}
				directory, activeKey = made, retrieveDirectoryKey(made)
				activeRetrieveDirectories.Store(activeKey, struct{}{})
			}
			path := filepath.Join(directory, name)
			spool, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return "", ErrBackend
			}
			writeErr := write(spool)
			closeErr := spool.Close()
			if writeErr != nil {
				return "", writeErr
			}
			if closeErr != nil {
				return "", ErrBackend
			}
			return path, nil
		}
		var source *preparedRetrieve
		var memory []byte
		input := ""
		if part.Size >= 0 && part.Size <= retrieveMemoryStageBytes {
			var staged bytes.Buffer
			staged.Grow(int(part.Size))
			written, copyErr := copyBounded(ctx, &staged, part.Reader, part.Size)
			if errors.Is(copyErr, ErrResourceLimit) {
				return ErrBackend // more than the declared size
			}
			if copyErr != nil {
				return copyErr
			}
			if written != part.Size {
				return ErrBackend
			}
			memory = staged.Bytes()
			source, err = s.readPreparedSource(ctx, memoryStaged{bytes.NewReader(memory)}, part.Size, l.MaxInputBytes, ref)
		} else {
			input, err = spoolFile("source.dcm", func(spool io.Writer) error {
				written, err := copyBounded(ctx, spool, part.Reader, l.MaxInputBytes)
				if err != nil {
					return err
				}
				if part.Size >= 0 && written != part.Size {
					return ErrBackend
				}
				return nil
			})
			if err != nil {
				return err
			}
			source, err = s.readPreparedFile(ctx, input, l.MaxInputBytes, ref)
		}
		if err != nil {
			return err
		}
		defer source.file.Close()
		if declaredSyntax != "" && declaredSyntax != source.parsed.TransferSyntax.UID {
			return ErrBackend
		}
		multipartRequired := request.Level != RetrieveLevelInstance || frames
		lossy := retrieveSourceLossy(source.parsed)
		target, err := s.retrieveTargetFor(source.parsed.TransferSyntax, lossy, request.Accept, frames, multipartRequired, uri)
		if err != nil {
			return err
		}
		if target.UID != source.parsed.TransferSyntax.UID {
			// Refuse, or deliver as stored, an object the conversion limits
			// refuse before the conversion starts, as SourceOutputBound did.
			if target, err = s.admitRetrieveConversion(ctx, source.parsed, source.file, target, lossy, request.Accept, frames, multipartRequired, uri); err != nil {
				return err
			}
		}
		if target.UID == source.parsed.TransferSyntax.UID {
			return yield(source)
		}
		if input == "" {
			if input, err = spoolFile("source.dcm", func(spool io.Writer) error {
				_, err := spool.Write(memory)
				return err
			}); err != nil {
				return err
			}
		}
		output := filepath.Join(directory, "output.dcm")
		if err := CheckReadLease(ctx); err != nil {
			return err
		}
		report, err := pixeldata.TranscodePath(ctx, input, output, target, s.retrievePolicy.Options)
		if err != nil {
			return classifyRetrieveTranscodeError(ctx, err)
		}
		if report.IdentityChanged || report.Lossy || report.Verification == pixeldata.TranscodeVerificationSkipped {
			return ErrUnsupported
		}
		prepared, err := s.readPreparedFile(ctx, output, l.MaxOutputBytes, ref)
		if err != nil {
			return err
		}
		defer prepared.file.Close()
		if prepared.parsed.TransferSyntax.UID != target.UID {
			return ErrBackend
		}
		return yield(prepared)
	})
}

func classifyRetrieveTranscodeError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if pixeldata.IsTranscodeLimitError(err) {
		return ErrResourceLimit
	}
	if errors.Is(err, pixeldata.ErrTranscodeUnsupported) || errors.Is(err, pixeldata.ErrTranscodeLossyDisallowed) || errors.Is(err, pixeldata.ErrEncoderNotFound) || errors.Is(err, pixeldata.ErrEncoderRegistryNil) || errors.Is(err, pixeldata.ErrCodecNotFound) || errors.Is(err, pixeldata.ErrCodecRegistryNil) || errors.Is(err, pixeldata.ErrUnsupportedEncoderMetadata) || errors.Is(err, pixeldata.ErrPixelDataNotFound) || errors.Is(err, pixeldata.ErrUnsupportedPhotometricInterpretation) || errors.Is(err, pixeldata.ErrUnsupportedPixelRepresentation) || errors.Is(err, pixeldata.ErrUnsupportedPlanarConfiguration) || errors.Is(err, pixeldata.ErrJPIPRetrievalRequired) || errors.Is(err, pixeldata.ErrMediaPayloadNotRenderable) {
		return ErrUnsupported
	}
	return ErrBackend
}

func (s *Server) preparedRetrieve(ctx context.Context, request RetrieveRequest, frames, uri bool, yield func(*preparedRetrieve) error) error {
	if !preparedPreferencesSupported(request.Accept, frames, request.Level != RetrieveLevelInstance || frames) {
		return ErrUnsupported
	}
	backend, ok := s.options.Backend.(RetrieveBackend)
	if !ok {
		return newServerStatusError(http.StatusNotImplemented, "not_implemented", ErrUnsupported)
	}
	sourceRequest := request
	sourceRequest.Accept = []MediaPreference{{MediaType: "application/dicom", TransferSyntaxUID: "*", Quality: 1, Multipart: request.Level != RetrieveLevelInstance}}
	sourceRequest.prepared = &preparedNegotiation{server: s, accept: request.Accept, frames: frames, multipart: request.Level != RetrieveLevelInstance || frames, isURI: uri}
	count := 0
	var failure error
	err := safeRetrieve(backend, ctx, sourceRequest, func(part RetrievePart) error {
		if failure != nil {
			_ = safeCloseReader(part.Reader)
			return failure
		}
		if count >= s.limits.MaxResults || (request.Level == RetrieveLevelInstance && count > 0) {
			_ = safeCloseReader(part.Reader)
			failure = ErrBackend
			return failure
		}
		failure = s.prepareRetrievePart(ctx, request, part, frames, uri, yield)
		if failure == nil {
			count++
		}
		return failure
	})
	if failure != nil {
		return failure
	}
	if err != nil {
		return classifyBackendError(err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Server) servePreparedRetrieve(w *serverResponseWriter, r *http.Request, request RetrieveRequest, uri bool) (count int, err error) {
	var writer *multipart.Writer
	multipartResponse := request.Level != RetrieveLevelInstance
	var staged *responseSpool
	var contentType, contentLocation string
	if request.Level == RetrieveLevelInstance {
		staged, err = newRetrieveResponseSpool(s.options.SpoolDirectory, s.limits.MaxResponseBytes)
		if err != nil {
			return 0, err
		}
		defer func() { err = errors.Join(err, staged.Close()) }()
	}
	err = s.preparedRetrieve(r.Context(), request, false, uri, func(p *preparedRetrieve) error {
		syntax := p.parsed.TransferSyntax.UID
		preference, ok := matchingMediaPreference(request.Accept, "application/dicom", syntax, multipartResponse)
		if !ok {
			return ErrUnsupported
		}
		if count == 0 && request.Level == RetrieveLevelInstance {
			multipartResponse = preference.Multipart && !uri
		}
		location, err := s.instanceLocation(p.ref)
		if err != nil {
			return ErrBackend
		}
		media := mediaTypeWithTransferSyntax("application/dicom", syntax)
		if uri {
			media = "application/dicom"
		}
		var response io.Writer = w
		if staged != nil {
			response = staged
		}
		var destination io.Writer = response
		if multipartResponse {
			if writer == nil {
				writer = multipart.NewWriter(response)
				contentType = `multipart/related; type="application/dicom"; boundary=` + writer.Boundary()
				if staged == nil {
					w.Header().Set("Content-Type", contentType)
				}
			}
			h := textproto.MIMEHeader{}
			h.Set("Content-Type", media)
			h.Set("Content-Location", location)
			destination, err = writer.CreatePart(h)
			if err != nil {
				return err
			}
		} else {
			contentType, contentLocation = media, location
			if p.size > w.remaining {
				return ErrResourceLimit
			}
		}
		if _, err = copyBounded(r.Context(), destination, p.file, min(s.limits.MaxPartBytes, p.size)); err != nil {
			return err
		}
		count++
		return nil
	})
	if err != nil {
		return count, err
	}
	if writer != nil {
		if err = writer.Close(); err != nil {
			return count, err
		}
	}
	if staged != nil {
		if err := staged.Rewind(); err != nil {
			return count, err
		}
		w.Header().Set("Content-Type", contentType)
		if contentLocation != "" {
			w.Header().Set("Content-Location", contentLocation)
		}
		w.Header().Set("Content-Length", strconv.FormatInt(staged.size, 10))
		if _, err := copyBounded(r.Context(), w, staged.file, staged.size); err != nil {
			return count, err
		}
	}
	return count, nil
}

func (s *Server) servePreparedFrames(w *serverResponseWriter, r *http.Request) (count int, err error) {
	parts := resourceParts(r.URL.Path)
	ref := InstanceRef{StudyInstanceUID: parts[1], SeriesInstanceUID: parts[3], SOPInstanceUID: parts[5]}
	for _, uid := range []string{ref.StudyInstanceUID, ref.SeriesInstanceUID, ref.SOPInstanceUID} {
		if err := s.validateUID(uid); err != nil {
			return 0, err
		}
	}
	frames, err := parseFrames(parts[7], s.limits.MaxFrames)
	if err != nil {
		return 0, err
	}
	preferences := parseMediaPreferences(requestAcceptHeader(r), "application/octet-stream")
	if !hasMultipartFramePreference(preferences) {
		return 0, ErrUnsupported
	}
	request := RetrieveRequest{Level: RetrieveLevelInstance, StudyInstanceUID: ref.StudyInstanceUID, SeriesInstanceUID: ref.SeriesInstanceUID, SOPInstanceUID: ref.SOPInstanceUID, Accept: preferences}
	var writer *multipart.Writer
	staged, err := newRetrieveResponseSpool(s.options.SpoolDirectory, s.limits.MaxResponseBytes)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, staged.Close()) }()
	var contentType string
	err = s.preparedRetrieve(r.Context(), request, true, false, func(p *preparedRetrieve) error {
		l := s.retrievePolicy.Options.Limits
		reader, err := pixeldata.NewFrameReader(r.Context(), p.parsed, p.file, pixeldata.FrameReaderOptions{Registry: s.retrievePolicy.Options.DecoderRegistry, MaxFrames: l.MaxFrames, MaxFragments: l.MaxFragments})
		if err != nil {
			return ErrUnsupported
		}
		defer reader.Close()
		var total int64
		for _, n := range frames {
			length, e := reader.RawFrameLength(n - 1)
			if e != nil {
				return ErrInvalidRequest
			}
			if length < 0 || length > s.limits.MaxPartBytes || length > w.remaining-total {
				return ErrResourceLimit
			}
			total += length
		}
		media := frameMediaForSyntax(reader.TransferSyntax().UID)
		if media == "" {
			return ErrUnsupported
		}
		writer = multipart.NewWriter(staged)
		contentType = `multipart/related; type="` + media + `"; boundary=` + writer.Boundary()
		for _, n := range frames {
			length, _ := reader.RawFrameLength(n - 1)
			if err := CheckReadLease(r.Context()); err != nil {
				return err
			}
			data, e := reader.RawFrame(r.Context(), n-1)
			if e != nil {
				return classifyRetrieveTranscodeError(r.Context(), e)
			}
			if int64(len(data)) != length {
				return ErrBackend
			}
			h := textproto.MIMEHeader{}
			h.Set("Content-Type", mediaTypeWithTransferSyntax(media, reader.TransferSyntax().UID))
			h.Set("Content-Location", s.frameLocation(ref, n))
			dst, e := writer.CreatePart(h)
			if e != nil {
				return e
			}
			if _, e = dst.Write(data); e != nil {
				return e
			}
			count++
		}
		return nil
	})
	if err != nil {
		return count, err
	}
	if writer != nil {
		err = writer.Close()
	}
	if err != nil {
		return count, err
	}
	if err := staged.Rewind(); err != nil {
		return count, err
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(staged.size, 10))
	_, err = copyBounded(r.Context(), w, staged.file, staged.size)
	return count, err
}

func newRetrieveResponseSpool(directory string, maximum int64) (*responseSpool, error) {
	f, err := os.CreateTemp(directory, ".dicomweb-retrieve-response-*")
	if err != nil {
		return nil, ErrBackend
	}
	return &responseSpool{file: f, path: f.Name(), maximum: maximum}, nil
}
