package dicomweb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/ThalesMMS/dicom-go/object"
)

// BulkDataStaging creates independent, private stages. maxBytes is finite and
// positive. Implementations must enforce it on writes, retain no complete
// in-memory copy, and remove unpublished data on Close, also after errors.
// Staging is temporary ownership, not a shared representation cache.
type BulkDataStaging interface {
	BeginBulkData(context.Context, int64) (BulkDataStage, error)
}

// BulkDataStage owns an unpublished representation. Publish transfers ownership
// of an exact-length Reader to the caller; only the resolver calls it, after the
// complete HTTP response has validated. Close discards unpublished bytes. After
// successful Publish, Close must leave the transferred Reader intact. Write,
// Publish and Close must serialize. A failed Publish transfers no resources.
type BulkDataStage interface {
	io.Writer
	Publish(context.Context) (object.BulkDataSource, error)
	Close() error
}

// FileBulkDataStaging uses a unique 0700 directory and a 0600 file per stage.
// Directory is a caller-controlled temporary parent (empty uses os.TempDir).
// MaxBytes further narrows the client's finite per-stage limit; zero uses that
// limit. Disk use is bounded per stage, not by an aggregate session quota.
type FileBulkDataStaging struct {
	Directory string
	MaxBytes  int64
}

// BeginBulkData creates one private stage without publishing a reader.
func (s FileBulkDataStaging) BeginBulkData(ctx context.Context, maxBytes int64) (BulkDataStage, error) {
	if ctx == nil || maxBytes <= 0 || s.MaxBytes < 0 {
		return nil, fmt.Errorf("bulk staging requires a context and positive byte limit")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.MaxBytes > 0 && s.MaxBytes < maxBytes {
		maxBytes = s.MaxBytes
	}
	dir, err := os.MkdirTemp(s.Directory, "dicom-bulk-")
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(dir, "value-")
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(dir))
	}
	return &fileBulkDataStage{ctx: ctx, reader: &fileBulkDataReader{file: file, dir: dir}, limit: maxBytes}, nil
}

// StagedBulkDataResolver implements object.BulkDataURIResolver using a caller's
// staging backend. Client.ResolveBulkDataURI keeps its buffered behavior.
type StagedBulkDataResolver struct {
	Client     Client
	Staging    BulkDataStaging
	ChunkBytes int
}

// ResolveBulkDataURI consumes bounded chunks and publishes only after exactly
// one representation and its terminal response have validated. Zero ChunkBytes
// selects32KiB; positive values up to1MiB are allowed. HTTP origin, redirect,
// authentication, media and response policy is the same as RetrieveBulkDataStream.
// The returned Reader owns its staging and must be closed even after read errors.
func (r StagedBulkDataResolver) ResolveBulkDataURI(ctx context.Context, uri string, maxBytes int64) (result object.BulkDataSource, err error) {
	if ctx == nil || r.Staging == nil || maxBytes < 0 || r.ChunkBytes < 0 || r.ChunkBytes > 1<<20 {
		return result, fmt.Errorf("invalid bulk staging configuration")
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	limits, err := responseLimits(r.Client.Options)
	if err != nil {
		return result, newDICOMwebError(ErrorKindRequestFailure, "", 0, err)
	}
	// Refuse an invalid/cross-origin URI before creating temporary files.
	u, uriErr := r.Client.Endpoint.BulkDataURL(uri)
	if uriErr != nil {
		err = uriErr
		return result, err
	}
	limit := limits.maxPartBytes
	if maxBytes > 0 && maxBytes < limit {
		limit = maxBytes
	}
	chunk := r.ChunkBytes
	if chunk == 0 {
		chunk = 32 << 10
	}
	var stage BulkDataStage
	defer func() {
		if stage != nil {
			err = errors.Join(err, stage.Close())
		}
	}()
	count := 0
	var total int64
	err = r.Client.streamResourceWithOptions(ctx, u, acceptBulkData, bulkDataResponsePolicy, func(part LocatedObjectPartStream) error {
		count++
		if count != 1 {
			return &ResponseDecodeError{Kind: ResponseDecodePartLimit, Limit: 1, Value: int64(count)}
		}
		var beginErr error
		stage, beginErr = r.Staging.BeginBulkData(ctx, limit)
		if beginErr != nil {
			return beginErr
		}
		if stage == nil {
			return fmt.Errorf("bulk staging returned no stage")
		}
		buffer := make([]byte, chunk)
		var read int64
		emptyReads := 0
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, readErr := part.Part.Reader.Read(buffer)
			if n > 0 {
				if int64(n) > limit-read {
					return errBulkDataTooLarge
				}
				written, writeErr := stage.Write(buffer[:n])
				if writeErr != nil {
					return writeErr
				}
				if written != n {
					return io.ErrShortWrite
				}
				read += int64(n)
				total = read
				emptyReads = 0
			}
			if readErr != nil {
				if readErr == io.EOF {
					return nil
				}
				return readErr
			}
			if n == 0 {
				emptyReads++
				if emptyReads == 100 {
					return io.ErrNoProgress
				}
			}
		}
	}, streamResourceOptions{sameOriginRedirect: true, requireEOF: true})
	var decodeErr *ResponseDecodeError
	if errors.Is(err, errBulkDataTooLarge) || errors.Is(err, object.ErrBulkDataTooLarge) {
		err = errors.Join(object.ErrBulkDataTooLarge, err)
	} else if errors.As(err, &decodeErr) && decodeErr.Kind == ResponseDecodePartBytes {
		err = fmt.Errorf("%w: %w", object.ErrBulkDataTooLarge, err)
	}
	if err != nil {
		return result, err
	}
	if count != 1 || stage == nil {
		return result, &ResponseDecodeError{Kind: ResponseDecodePartLimit, Limit: 1, Value: int64(count)}
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	result, err = stage.Publish(ctx)
	if err != nil || result.Reader == nil || result.Size != total || result.Size < 0 || result.Size > limit {
		if err == nil {
			err = fmt.Errorf("bulk staging returned an invalid source")
		}
		if result.Reader != nil {
			err = errors.Join(err, result.Reader.Close())
		}
		return object.BulkDataSource{}, err
	}
	return result, nil
}

type fileBulkDataStage struct {
	mu        sync.Mutex
	ctx       context.Context
	reader    *fileBulkDataReader
	limit     int64
	size      int64
	published bool
	closed    bool
}

func (s *fileBulkDataStage) Write(data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.published {
		return 0, os.ErrClosed
	}
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(data)) > s.limit-s.size {
		return 0, object.ErrBulkDataTooLarge
	}
	n, err := s.reader.file.Write(data)
	s.size += int64(n)
	return n, err
}

func (s *fileBulkDataStage) Publish(ctx context.Context) (object.BulkDataSource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil {
		return object.BulkDataSource{}, fmt.Errorf("bulk Publish requires a context")
	}
	if s.closed || s.published {
		return object.BulkDataSource{}, os.ErrClosed
	}
	if err := s.ctx.Err(); err != nil {
		return object.BulkDataSource{}, err
	}
	if err := ctx.Err(); err != nil {
		return object.BulkDataSource{}, err
	}
	if _, err := s.reader.file.Seek(0, io.SeekStart); err != nil {
		return object.BulkDataSource{}, err
	}
	s.published = true
	return object.BulkDataSource{Reader: s.reader, Size: s.size}, nil
}

func (s *fileBulkDataStage) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.published {
		return nil
	}
	s.closed = true
	return s.reader.Close()
}

type fileBulkDataReader struct {
	mu       sync.Mutex
	file     *os.File
	dir      string
	closed   bool
	closeErr error
	removed  bool
}

func (r *fileBulkDataReader) Read(data []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, os.ErrClosed
	}
	return r.file.Read(data)
}

func (r *fileBulkDataReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		r.closeErr = r.file.Close()
	}
	if r.removed {
		return r.closeErr
	}
	err := os.RemoveAll(r.dir)
	if err == nil {
		r.removed = true
	}
	// A failed removal remains owned; later Close can retry it. Never report
	// successful cleanup while files remain. Read and Close serialize.
	return errors.Join(r.closeErr, err)
}
