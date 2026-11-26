package object

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// ErrSharedFileSink rejects streaming sinks shared by a parallel file batch.
// They have per-file lifecycle and cannot be rolled back with the batch.
var ErrSharedFileSink = errors.New("dicom: batch opening does not support shared streaming sinks")

// OpenFilesContext opens an explicit ordered list of Part 10 files with at most
// workers active reads (<=0 uses the module's shared helper budget). It performs
// no directory discovery, series selection, geometry inference or pixel decode.
// Independent file reads share the process-wide budget with CPU kernels.
//
// Success transfers all Files to the caller, which must Close each one. Any
// error/cancellation joins the workers, closes every opened/deferred source and
// returns no partial batch. Parse errors identify the first failing input index.
// Cancellation is checked between file reads, not inside a blocking OS read.
//
// Options and their dictionaries/descriptors must stay immutable and support
// concurrent reads; use workers=1 for stateful custom dictionaries. Set the
// per-file read limits explicitly for untrusted inputs and bound the input list
// and total retained bytes externally. FrameSink and EncapsulatedSink are
// rejected because they have mutable per-file lifecycle.
func OpenFilesContext(ctx context.Context, paths []string, options ReadFileOptions, workers int) ([]*File, error) {
	return openFilesContext(ctx, paths, options, workers, OpenFileWithOptions)
}

func openFilesContext(ctx context.Context, paths []string, options ReadFileOptions, workers int, open func(string, ReadFileOptions) (*File, error)) ([]*File, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.FrameSink != nil || options.EncapsulatedSink != nil {
		return nil, ErrSharedFileSink
	}
	files := make([]*File, len(paths))
	readErrors := make([]error, len(paths))
	var first atomic.Int64
	first.Store(int64(len(paths)))
	err := parallel.Do(ctx, len(paths), workers, func(index int) {
		if int64(index) > first.Load() {
			return
		}
		file, err := open(paths[index], options)
		files[index], readErrors[index] = file, err
		if err != nil {
			for previous := first.Load(); int64(index) < previous; previous = first.Load() {
				if first.CompareAndSwap(previous, int64(index)) {
					break
				}
			}
		}
	})
	if err == nil {
		if index := int(first.Load()); index < len(paths) {
			err = fmt.Errorf("dicom: open input %d (%s): %w", index, paths[index], readErrors[index])
		}
	}
	if err != nil {
		for _, file := range files {
			if closeErr := file.Close(); closeErr != nil {
				err = errors.Join(err, closeErr)
			}
		}
		return nil, err
	}
	return files, nil
}
