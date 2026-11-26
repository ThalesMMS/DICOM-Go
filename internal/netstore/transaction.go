package netstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/nofollow"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Per-call operations permit deterministic I/O fault testing without global hooks.
type saveOperations struct {
	write           func(io.Writer, *object.File) error
	sync            func(*os.File) error
	syncDirectory   func(*os.File) error
	close           func(*os.File) error
	publish         func(*os.File, string, string, os.FileInfo) (bool, error)
	publishRelative func(*os.File, *os.File, string, string, os.FileInfo) (bool, error)
}

func defaultSaveOperations() saveOperations {
	return defaultSaveOperationsWith(SaveOptions{})
}

func defaultSaveOperationsWith(opts SaveOptions) saveOperations {
	write := func(w io.Writer, file *object.File) error {
		return object.WriteFileWithOptions(w, file, object.WriteFileOptions{AETitles: opts.AETitles})
	}
	return saveOperations{write: write, sync: (*os.File).Sync, syncDirectory: (*os.File).Sync, close: (*os.File).Close, publish: nofollow.PublishClosedAt, publishRelative: nofollow.PublishClosedBetweenAt}
}

func savePart10(ctx context.Context, outDir string, dataset *object.Object, syntax transfer.Syntax, ops saveOperations) (string, error) {
	class, instance, err := dataSetIdentity(dataset)
	if err != nil {
		return "", err
	}
	stream, err := beginStream(ctx, outDir, instance, ops)
	if err != nil {
		return "", err
	}
	file := part10File(dataset, syntax, class, instance)
	if err := ops.write(stream, file); err != nil {
		return "", stream.abort(failure("write instance", err))
	}
	return stream.Commit()
}

// StreamFile is a received instance being written: a private temporary in
// the output directory that Commit publishes as <SOP Instance UID>.dcm (or a
// numbered collision name) with the same guarantees as SavePart10, and
// Abort discards. Writes check the context between 32 KiB chunks.
type StreamFile struct {
	ctx        context.Context
	outDir     string
	instance   string
	parent     *os.File
	parentInfo os.FileInfo
	file       *os.File
	temporary  string
	ops        saveOperations
	finished   bool
}

// BeginStream reserves the temporary of a streamed instance (#1050: C-STORE
// data sets written as received). The caller writes the complete Part 10
// file, then calls Commit or Abort.
func BeginStream(ctx context.Context, outDir, sopInstanceUID string) (*StreamFile, error) {
	if !core.IsValidUID(sopInstanceUID) {
		return nil, ErrInvalidIdentity
	}
	return beginStream(ctx, outDir, sopInstanceUID, defaultSaveOperations())
}

func beginStream(ctx context.Context, outDir, instance string, ops saveOperations) (*StreamFile, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, failure("prepare", err)
	}
	parent, err := nofollow.OpenDirectory(outDir)
	if err != nil {
		return nil, failure("open output", err)
	}
	parentInfo, err := parent.Stat()
	if err != nil {
		return nil, errors.Join(failure("inspect output", err), parent.Close())
	}
	var f *os.File
	var temporary string
	for attempt := 0; attempt < 10; attempt++ {
		var token [16]byte
		if _, err = rand.Read(token[:]); err != nil {
			return nil, errors.Join(failure("prepare", err), parent.Close())
		}
		temporary = ".netstore-" + hex.EncodeToString(token[:]) + ".partial"
		f, err = nofollow.CreateAt(parent, temporary)
		if !errors.Is(err, os.ErrExist) {
			break
		}
	}
	if err != nil {
		return nil, errors.Join(failure("create temporary", err), parent.Close())
	}
	stream := &StreamFile{ctx: ctx, outDir: outDir, instance: instance, parent: parent, parentInfo: parentInfo, file: f, temporary: temporary, ops: ops}
	if err := protectInstanceFile(f); err != nil {
		return nil, stream.abort(failure("protect temporary", err))
	}
	return stream, nil
}

// Write appends to the temporary.
func (s *StreamFile) Write(p []byte) (int, error) {
	if s.finished {
		return 0, os.ErrClosed
	}
	return contextWriter{ctx: s.ctx, destination: s.file}.Write(p)
}

// ReadBack rewinds the temporary for reading what was written (a check
// before Commit); later writes append after it again.
func (s *StreamFile) ReadBack() (io.Reader, error) {
	if s.finished {
		return nil, os.ErrClosed
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return readThenSeekEnd{file: s.file}, nil
}

type readThenSeekEnd struct{ file *os.File }

func (r readThenSeekEnd) Read(p []byte) (int, error) {
	n, err := r.file.Read(p)
	if err == io.EOF {
		if _, seekErr := r.file.Seek(0, io.SeekEnd); seekErr != nil {
			return n, seekErr
		}
	}
	return n, err
}

// Abort discards the temporary.
func (s *StreamFile) Abort() error {
	return s.abort(nil)
}

func (s *StreamFile) abort(result error) error {
	if s.finished {
		return result
	}
	s.finished = true
	closeErr := s.file.Close()
	if errors.Is(closeErr, os.ErrClosed) {
		closeErr = nil
	}
	if err := errors.Join(closeErr, nofollow.RemoveAt(s.parent, s.temporary)); err != nil {
		result = &Error{Operation: "cleanup", cause: errors.Join(result, err)}
	}
	if err := s.parent.Close(); err != nil && result == nil {
		result = failure("cleanup", err)
	}
	return result
}

// Commit syncs, closes and publishes the instance, returning its path.
func (s *StreamFile) Commit() (path string, result error) {
	return s.commitTarget("")
}

func (s *StreamFile) commitTarget(relative string) (path string, result error) {
	if s.finished {
		return "", os.ErrClosed
	}
	s.finished = true
	published := false
	defer func() {
		if err := nofollow.RemoveAt(s.parent, s.temporary); err != nil {
			result = &Error{Operation: "cleanup", Published: published, cause: errors.Join(result, err)}
		}
		s.parent.Close()
	}()
	closed := false
	defer func() {
		if !closed {
			s.file.Close()
		}
	}()
	if err := s.ctx.Err(); err != nil {
		return "", failure("write instance", err)
	}
	var target *relativeTarget
	if relative != "" {
		var err error
		target, err = s.prepareRelativeTarget(relative)
		if err != nil {
			return "", failure("prepare layout", err)
		}
		defer target.close()
	}
	if err := s.ops.sync(s.file); err != nil {
		return "", failure("sync instance", err)
	}
	info, err := s.file.Stat()
	if err != nil {
		return "", failure("inspect temporary", err)
	}
	closed = true
	if err := s.ops.close(s.file); err != nil {
		return "", failure("close instance", err)
	}
	// Reopen using the same no-follow traversal, not a textual prefix comparison.
	// Creation, publication and cleanup stay anchored to the original descriptor.
	current, err := nofollow.OpenDirectory(s.outDir)
	if err != nil {
		return "", failure("verify output", errors.Join(ErrUnsafeDirectory, err))
	}
	currentInfo, statErr := current.Stat()
	closeErr := current.Close()
	if statErr != nil || closeErr != nil || !os.SameFile(s.parentInfo, currentInfo) {
		return "", failure("verify output", errors.Join(ErrUnsafeDirectory, statErr, closeErr))
	}
	if target != nil {
		if err := target.verify(s.parent); err != nil {
			return "", failure("verify output", err)
		}
	}
	for i := 0; i < 1000; i++ {
		if err := s.ctx.Err(); err != nil {
			return "", failure("publish instance", err)
		}
		name := instanceName(s.instance, i)
		destinationParent := s.parent
		relativeName := name
		if target != nil {
			name = relativeCollisionName(target.base, i)
			relativeName = filepath.Join(target.directory, name)
			destinationParent = target.parent
			published, err = s.ops.publishRelative(s.parent, destinationParent, s.temporary, name, info)
		} else {
			published, err = s.ops.publish(s.parent, s.temporary, name, info)
		}
		if published {
			path = filepath.Join(s.outDir, relativeName)
			if err != nil {
				return path, &Error{Operation: "finalize instance", Published: true, cause: err}
			}
			// Content was synced before publication. On Unix also sync the directory
			// entry. Windows does not provide the same directory-fsync contract.
			if runtime.GOOS != "windows" {
				if err := s.ops.syncDirectory(destinationParent); err != nil {
					return path, &Error{Operation: "sync publication", Published: true, cause: err}
				}
			}
			return path, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", failure("publish instance", err)
		}
	}
	return "", ErrCollisionLimit
}

type contextWriter struct {
	ctx         context.Context
	destination io.Writer
}

func (w contextWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if err := w.ctx.Err(); err != nil {
			return written, err
		}
		chunk := min(len(p), 32<<10)
		n, err := w.destination.Write(p[:chunk])
		written += n
		if err != nil {
			return written, err
		}
		if n != chunk {
			return written, io.ErrShortWrite
		}
		p = p[n:]
	}
	return written, nil
}
