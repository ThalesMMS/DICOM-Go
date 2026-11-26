package dimse

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Spooled C-STORE reception (#1989): the data set of a C-STORE, in a Storage
// SCP or in a C-GET requestor, is written to a temporary file as its P-DATA
// arrives, then parsed from that file with Pixel Data and the other large
// values left on it. The handler receives an ordinary *object.Object whose
// deferred values are read from the file on demand, and object.WriteFile
// streams them back, so the heap a reception needs follows the largest value
// read into memory instead of the data set. The file is removed once the
// C-STORE response has been sent.

// DataSetSpoolPrefix starts the name of every temporary file of a spooled
// reception. A process that dies during a reception leaves such a file in the
// spool directory; an application that owns the directory can remove the old
// ones.
const DataSetSpoolPrefix = ".dimse-spool-"

// DefaultSpoolInlineBytes is the largest value a spooled reception reads into
// memory when no other threshold is set: longer values stay in the spool
// file.
const DefaultSpoolInlineBytes = 64 << 10

// ErrDataSetSpool reports that a data set could not be written to, or read
// back from, its spool file. A C-STORE that fails this way is answered with
// Out of Resources.
var ErrDataSetSpool = errors.New("dicom dimse: data set spool")

// spoolBufferSize batches the PDV-sized writes into the spool file.
const spoolBufferSize = 256 << 10

// spoolDataSet copies the rest of one data set from reader into a new
// temporary file of dir and parses it with its large values deferred. The
// returned release closes and removes the file; the data set must not be
// used after it. maxBytes, when positive, bounds the data set: a larger one
// returns parser.ErrMaxTotalBytesExceeded with the rest left on the reader.
// A deflated data set cannot leave values on a compressed file and is
// parsed from the reader as before.
func spoolDataSet(reader *PDataReader, syntax transfer.Syntax, spool spoolTarget, opts object.ReadFileOptions) (*object.Object, func(), error) {
	dir, inline := spool.dir, spool.inline
	if syntax.Deflated {
		dataset, err := readPDataDataSet(reader, syntax, opts)
		return dataset, func() {}, err
	}
	file, err := os.CreateTemp(dir, DataSetSpoolPrefix+"*")
	if err != nil {
		return nil, func() {}, fmt.Errorf("%w: create: %w", ErrDataSetSpool, err)
	}
	release := func() {
		_ = file.Close()
		removeSpoolFile(file.Name(), spool.onRemoveError)
	}
	maxBytes := opts.MaxTotalBytes
	source := io.Reader(reader)
	if maxBytes > 0 {
		source = io.LimitReader(reader, maxBytes+1)
	}
	buffer := bufio.NewWriterSize(&spoolWriter{file: file}, spoolBufferSize)
	written, err := io.Copy(buffer, source)
	if err == nil {
		err = buffer.Flush()
	}
	if err != nil {
		release()
		var spoolErr *spoolWriteError
		if errors.As(err, &spoolErr) {
			return nil, func() {}, fmt.Errorf("%w: write: %w", ErrDataSetSpool, spoolErr.err)
		}
		return nil, func() {}, err
	}
	if maxBytes > 0 && written > maxBytes {
		release()
		return nil, func() {}, fmt.Errorf("dicom dimse: spooled data set: %w", parser.ErrMaxTotalBytesExceeded)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		release()
		return nil, func() {}, fmt.Errorf("%w: rewind: %w", ErrDataSetSpool, err)
	}
	if inline <= 0 {
		inline = DefaultSpoolInlineBytes
	}
	opts.DeferPixelData = true
	opts.InlineValueBytesThreshold = inline
	opts.DeferNestedValues = true
	dataset, err := object.ReadDataSetWithOptions(file, syntax, opts)
	if err != nil {
		release()
		return nil, func() {}, err
	}
	return dataset, release, nil
}

// spoolTarget is where and how a reception spools its data set.
type spoolTarget struct {
	dir    string
	inline int64
	// onRemoveError receives a spool file that could not be removed.
	onRemoveError func(path string, err error)
}

// spoolRemoveAttempts and spoolRemoveDelay retry the removal of a spool file
// that is briefly held by another process, as an antivirus scanner may hold
// a file that was just written on Windows.
var (
	spoolRemoveAttempts = 5
	spoolRemoveDelay    = 20 * time.Millisecond
)

// removeSpoolFile removes a spool file, retrying with a growing delay; a file
// that stays reports to onError (when set) so the application can remove it
// later.
func removeSpoolFile(path string, onError func(string, error)) {
	var err error
	delay := spoolRemoveDelay
	for attempt := 0; attempt < spoolRemoveAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(delay)
			delay *= 2
		}
		err = spoolRemove(path)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return
		}
	}
	if onError != nil {
		onError(path, err)
	}
}

// spoolRemove removes a spool file; tests replace it.
var spoolRemove = os.Remove

// spoolWriter marks the failures of the spool file, so that they are not
// mistaken for failures of the association.
type spoolWriter struct{ file *os.File }

type spoolWriteError struct{ err error }

func (e *spoolWriteError) Error() string { return e.err.Error() }
func (e *spoolWriteError) Unwrap() error { return e.err }

func (w *spoolWriter) Write(p []byte) (int, error) {
	n, err := w.file.Write(p)
	if err != nil {
		return n, &spoolWriteError{err: err}
	}
	return n, nil
}

type cGetSpoolDirKey struct{}

type cGetSpoolRemoveErrorKey struct{}

// WithCGetSpoolRemoveError makes a C-GET requestor with WithCGetSpoolDir
// report a spool file it could not remove, after retrying, to onError, like
// StorageSCPOptions.OnSpoolRemoveError.
func WithCGetSpoolRemoveError(ctx context.Context, onError func(path string, err error)) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if onError == nil {
		return ctx
	}
	return context.WithValue(ctx, cGetSpoolRemoveErrorKey{}, onError)
}

func cGetSpoolRemoveError(ctx context.Context) func(string, error) {
	if ctx == nil {
		return nil
	}
	onError, _ := ctx.Value(cGetSpoolRemoveErrorKey{}).(func(string, error))
	return onError
}

// WithCGetSpoolDir makes a C-GET requestor write the data set of each C-STORE
// sub-operation to a temporary file of dir as it arrives, like
// StorageSCPOptions.SpoolDir: the store handler receives a data set whose
// Pixel Data and other values larger than DefaultSpoolInlineBytes are read
// from that file, which is removed once the sub-operation is answered. An
// empty dir keeps the in-memory reception.
func WithCGetSpoolDir(ctx context.Context, dir string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if dir == "" {
		return ctx
	}
	return context.WithValue(ctx, cGetSpoolDirKey{}, dir)
}

func cGetSpoolDir(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	dir, _ := ctx.Value(cGetSpoolDirKey{}).(string)
	return dir
}
