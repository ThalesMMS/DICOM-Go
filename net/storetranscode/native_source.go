package storetranscode

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/index"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// NativeResources is a caller-owned aggregate budget, shared across sources and
// sessions. Decode/materialization slots end before a handle is returned. Spool
// reservations remain until Close and never wait for another network handle:
// a pipelined session may need its next Open before draining its first response.
// These are logical live bytes, not a process RSS guarantee. Codec allocations
// are additionally bounded by TranscodeLimits; callers budget their overhead.
type NativeResources struct {
	gate                chan struct{}
	mu                  sync.Mutex
	maxSpool, usedSpool int64
}

func NewNativeResources(maxConcurrentPreparations int, maxSpoolBytes int64) (*NativeResources, error) {
	if maxConcurrentPreparations <= 0 || maxSpoolBytes <= 0 {
		return nil, ErrOptions
	}
	return &NativeResources{gate: make(chan struct{}, maxConcurrentPreparations), maxSpool: maxSpoolBytes}, nil
}

// SpoolBytes returns the currently reserved upper bound, including preparations.
func (r *NativeResources) SpoolBytes() int64 { r.mu.Lock(); defer r.mu.Unlock(); return r.usedSpool }
func (r *NativeResources) reserve(n int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > r.maxSpool-r.usedSpool {
		return false
	}
	r.usedSpool += n
	return true
}
func (r *NativeResources) release(n int64) { r.mu.Lock(); defer r.mu.Unlock(); r.usedSpool -= n }

// NativeOptions enables original-first retrieval with native LE fallback. The
// registry is caller-owned; no codec global is consulted. Inspect only checks
// registration, not decodability of a particular instance. Preparation errors
// (including missing runtime) happen before a C-STORE command. No lossy encoding
// is performed. Resources can be shared across associations.
type NativeOptions struct {
	Transcode         pixeldata.TranscodeOptions
	DictionaryOptions object.ReadFileOptions
	SpoolDirectory    string
	Resources         *NativeResources
	// BeforeSourceRead optionally authorizes generic source inspection and
	// revalidates immediately before snapshotting, after waiting for resources.
	BeforeSourceRead func(context.Context) error
	// BeforeStore is passed to DIMSE to revalidate just before sending C-STORE.
	BeforeStore func(context.Context) error
}

// NativeSource snapshots the original representation lazily. Native fallback
// decodes once only when selected, writes an immutable spool, then releases all
// dataset/codec resources before returning its network handle. Inspect does not
// decode, re-encode, retain handles, or allocate a spool.
type NativeSource struct {
	base              dimse.StoreSource
	opts              NativeOptions
	mu                sync.Mutex
	input, descriptor *dimse.StoreDescriptor
	digest            *[32]byte
	path              *nativePath
}

var _ dimse.StoreTransferSyntaxSource = (*NativeSource)(nil)

func NewNativeSource(base dimse.StoreSource, opts NativeOptions) (*NativeSource, error) {
	if base == nil || opts.Transcode.AllowLossy || opts.Transcode.ForceReencode || opts.Transcode.Rewrite != nil || opts.Transcode.CompressIcons {
		return nil, ErrOptions
	}
	limits, err := pixeldata.ResolveTranscodeLimits(opts.Transcode.Limits)
	if err != nil {
		return nil, err
	}
	if limits.MaxInputBytes > math.MaxInt64-limits.MaxOutputBytes {
		return nil, ErrOptions
	}
	opts.Transcode.Limits = limits
	if opts.Resources == nil {
		opts.Resources, err = NewNativeResources(1, limits.MaxInputBytes+limits.MaxOutputBytes)
		if err != nil {
			return nil, err
		}
	}
	return &NativeSource{base: base, opts: opts}, nil
}

func (s *NativeSource) Inspect(ctx context.Context) (dimse.StoreDescriptor, error) {
	if s == nil {
		return dimse.StoreDescriptor{}, ErrOptions
	}
	ctx = contextOrBackground(ctx)
	if err := context.Cause(ctx); err != nil {
		return dimse.StoreDescriptor{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.descriptor != nil {
		return cloneNativeDescriptor(*s.descriptor), nil
	}
	if s.path == nil {
		if err := s.checkSourceAccess(ctx); err != nil {
			return dimse.StoreDescriptor{}, err
		}
	}
	input, err := s.base.Inspect(ctx)
	if err != nil {
		return dimse.StoreDescriptor{}, &Error{Cause: err}
	}
	original, ok := transfer.DefaultRegistry.Get(transfer.NormalizeUID(input.TransferSyntaxUID))
	if !ok || !core.IsValidUID(input.SOPClassUID) || !core.IsValidUID(input.SOPInstanceUID) || input.Size < 0 || input.Size > s.opts.Transcode.Limits.MaxInputBytes {
		return dimse.StoreDescriptor{}, &Error{Cause: dimse.ErrStoreInvalidSource}
	}
	output := cloneNativeDescriptor(input)
	output.WritableTransferSyntaxUIDs = []string{original.UID}
	// Unknown generic source sizes receive a conservative finite input bound.
	output.Size = input.Size
	if output.Size == 0 {
		output.Size = s.opts.Transcode.Limits.MaxInputBytes
	}
	output.TransferSyntaxSizes = map[string]int64{original.UID: output.Size}
	producible := original.UID == transfer.ImplicitVRLittleEndian.UID || original.UID == transfer.ExplicitVRLittleEndian.UID || original.UID == transfer.ExplicitVRBigEndian.UID || original.UID == transfer.DeflatedExplicitVRLittleEndian.UID || original.UID == transfer.EncapsulatedUncompressedExplicitVRLittleEndian.UID
	if original.Encapsulated && !original.MediaPayload && !producible && s.opts.Transcode.DecoderRegistry != nil {
		_, producible = s.opts.Transcode.DecoderRegistry.GetCodec(original.UID)
	}
	if producible {
		for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian} {
			if syntax.UID != original.UID {
				output.WritableTransferSyntaxUIDs = append(output.WritableTransferSyntaxUIDs, syntax.UID)
				output.TransferSyntaxSizes[syntax.UID] = s.opts.Transcode.Limits.MaxOutputBytes
			}
		}
	}
	s.input = &input
	s.descriptor = &output
	return cloneNativeDescriptor(output), nil
}

// Open preserves the legacy default by selecting the original representation.
func (s *NativeSource) Open(ctx context.Context) (dimse.OpenedStoreSource, error) {
	d, err := s.Inspect(ctx)
	if err != nil {
		return dimse.OpenedStoreSource{}, err
	}
	syntax, _ := transfer.DefaultRegistry.Get(d.TransferSyntaxUID)
	return s.OpenForTransferSyntax(ctx, syntax)
}

func (s *NativeSource) OpenForTransferSyntax(parent context.Context, target transfer.Syntax) (opened dimse.OpenedStoreSource, err error) {
	defer func() {
		if nativePreparationLimit(err) {
			err = &Error{Cause: errors.Join(err, dimse.ErrStoreResourceLimit)}
		}
	}()
	parent = contextOrBackground(parent)
	descriptor, err := s.Inspect(parent)
	if err != nil {
		return opened, err
	}
	target, ok := transfer.DefaultRegistry.Get(transfer.NormalizeUID(target.UID))
	if !ok || !containsUID(descriptor.WritableTransferSyntaxUIDs, target.UID) {
		return opened, &Error{Cause: dimse.ErrStoreTransferSyntax}
	}
	limits := s.opts.Transcode.Limits
	ctx, cancel := context.WithTimeout(parent, limits.MaxDuration)
	defer cancel()
	// Reserve an upper bound before allocating anything. Fail immediately when
	// other handles occupy quota, instead of deadlocking a pipeline response loop.
	reservation := limits.MaxInputBytes
	if target.UID != descriptor.TransferSyntaxUID {
		reservation += limits.MaxOutputBytes
	}
	resources := s.opts.Resources
	if !resources.reserve(reservation) {
		return opened, &Error{Cause: ErrResourceLimit}
	}
	retained := false
	defer func() {
		if !retained {
			resources.release(reservation)
		}
	}()
	select {
	case resources.gate <- struct{}{}:
	case <-ctx.Done():
		return opened, context.Cause(ctx)
	}
	defer func() { <-resources.gate }()
	if err := s.checkSourceAccess(ctx); err != nil {
		return opened, err
	}
	dir, err := os.MkdirTemp(s.opts.SpoolDirectory, "dicom-native-store-")
	if err != nil {
		return opened, &Error{Cause: err}
	}
	defer func() {
		if !retained {
			_ = os.RemoveAll(dir)
		}
	}()
	inputPath := filepath.Join(dir, "original")
	inputFile, err := os.OpenFile(inputPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return opened, &Error{Cause: err}
	}
	defer inputFile.Close()
	offset, inputSize, digest, err := s.snapshot(ctx, inputFile)
	if err != nil {
		return opened, &Error{Cause: err}
	}
	s.mu.Lock()
	changed := s.digest != nil && *s.digest != digest
	if s.digest == nil {
		s.digest = &digest
	}
	s.mu.Unlock()
	if changed {
		return opened, &Error{Cause: dimse.ErrStoreSourceChanged}
	}
	outputPath, outputOffset := inputPath, offset
	if target.UID != descriptor.TransferSyntaxUID {
		original, _ := transfer.DefaultRegistry.Get(descriptor.TransferSyntaxUID)
		reader := io.NewSectionReader(inputFile, offset, inputSize-offset)
		readOpts := s.opts.DictionaryOptions
		readOpts.MaxTotalBytes = limits.MaxInputBytes
		if original.Deflated {
			readOpts.MaxTotalBytes = nativeLogicalReadLimit(limits, inputSize-offset)
		}
		readOpts.MaxElementBytes = limits.MaxInputBytes
		readOpts.MaxPixelDataBytes = limits.MaxInputBytes
		readOpts.MaxElements = limits.MaxElements
		readOpts.MaxFragments = limits.MaxFragments
		readOpts.MaxSequenceDepth = limits.MaxDepth
		readOpts.DeferPixelData = incrementalProfile(original, target, s.opts.Transcode)
		dataset, readErr := object.ReadDataSetWithOptions(&contextReadSeeker{ctx: ctx, ReadSeeker: reader}, original, readOpts)
		if readErr != nil {
			return opened, &Error{Cause: readErr}
		}
		prepareErr := func() error {
			defer dataset.Close()
			actual, inspectErr := dimse.NewFileStoreSource(&object.File{Dataset: dataset, TransferSyntax: original}, 0).Inspect(ctx)
			if inspectErr != nil || actual.SOPClassUID != descriptor.SOPClassUID || actual.SOPInstanceUID != descriptor.SOPInstanceUID {
				return dimse.ErrStoreSourceChanged
			}
			outputPath = filepath.Join(dir, "native")
			if incrementalProfile(original, target, s.opts.Transcode) {
				preparedFile, _, preparedOffset, _, incrementalErr := prepareIncremental(ctx, inputPath, offset, outputPath, original, target, s.opts.Transcode, readOpts.Dictionary, &spoolBudget{remaining: limits.MaxOutputBytes})
				if incrementalErr == nil {
					actualOutput, inspectErr := dimse.NewFileStoreSource(preparedFile, 0).Inspect(ctx)
					if inspectErr != nil || actualOutput.SOPClassUID != descriptor.SOPClassUID || actualOutput.SOPInstanceUID != descriptor.SOPInstanceUID {
						return errors.Join(dimse.ErrStoreSourceChanged, inspectErr, preparedFile.Close())
					}
					outputOffset = preparedOffset
					return preparedFile.Close()
				}
				if !errors.Is(incrementalErr, pixeldata.ErrTranscodeStreamingIneligible) {
					return incrementalErr
				}
				if err := dataset.Close(); err != nil {
					return err
				}
				if _, err := reader.Seek(0, io.SeekStart); err != nil {
					return err
				}
				readOpts.DeferPixelData = false
				dataset, incrementalErr = object.ReadDataSetWithOptions(&contextReadSeeker{ctx: ctx, ReadSeeker: reader}, original, readOpts)
				if incrementalErr != nil {
					return incrementalErr
				}
				defer dataset.Close()
			}
			outputOffset = 0
			converted, _, transcodeErr := pixeldata.TranscodeDataSet(ctx, dataset, original, target, s.opts.Transcode)
			if transcodeErr != nil {
				return transcodeErr
			}
			defer converted.Close()
			outputPath = filepath.Join(dir, "native")
			_, writeErr := writeRepresentation(ctx, outputPath, converted, target, &spoolBudget{remaining: limits.MaxOutputBytes}, limits.MaxOutputBytes)
			return writeErr
		}()
		if prepareErr != nil {
			return opened, &Error{Cause: prepareErr}
		}
		// Incremental output retains its Part10 framing; resident output is raw.
	}
	if err := context.Cause(ctx); err != nil {
		return opened, err
	}
	if err := inputFile.Close(); err != nil {
		return opened, &Error{Cause: err}
	}
	var mu sync.Mutex
	closed := false
	var closeErr error
	quotaReleased := false
	opened = dimse.OpenedStoreSource{
		Descriptor:  descriptor,
		BeforeStore: s.opts.BeforeStore,
		ReadMetadata: func(readCtx context.Context, options index.Options) (index.Result, error) {
			mu.Lock()
			defer mu.Unlock()
			if closed {
				return index.Result{}, dimse.ErrStoreInvalidSource
			}
			f, err := os.Open(outputPath)
			if err != nil {
				return index.Result{}, &Error{Cause: err}
			}
			defer f.Close()
			info, err := f.Stat()
			if err != nil {
				return index.Result{}, &Error{Cause: err}
			}
			options.RawDataSet = nil
			if outputOffset == 0 || target.UID != descriptor.TransferSyntaxUID {
				options.RawDataSet = &index.RawDataSetOptions{TransferSyntax: target}
			}
			if options.Dictionary == nil {
				options.Dictionary = s.opts.DictionaryOptions.Dictionary
			}
			var metadataInput io.ReaderAt = f
			metadataSize := info.Size()
			if target.UID != descriptor.TransferSyntaxUID && outputOffset > 0 {
				metadataInput = io.NewSectionReader(f, outputOffset, info.Size()-outputOffset)
				metadataSize -= outputOffset
			}
			return index.ReadAt(readCtx, "", metadataInput, metadataSize, options)
		},
		WriteDataSet: func(writeCtx context.Context, w io.Writer, syntax transfer.Syntax) error {
			mu.Lock()
			defer mu.Unlock()
			if closed {
				return dimse.ErrStoreInvalidSource
			}
			if transfer.NormalizeUID(syntax.UID) != target.UID {
				return dimse.ErrStoreTransferSyntax
			}
			f, err := os.Open(outputPath)
			if err != nil {
				return &Error{Cause: err}
			}
			defer f.Close()
			if _, err := f.Seek(outputOffset, io.SeekStart); err != nil {
				return &Error{Cause: err}
			}
			_, err = copyContext(contextOrBackground(writeCtx), w, f)
			if err != nil {
				return &Error{Cause: err}
			}
			return nil
		},
		Close: func() error {
			mu.Lock()
			defer mu.Unlock()
			closed = true
			if !quotaReleased {
				closeErr = os.RemoveAll(dir)
				if closeErr == nil {
					resources.release(reservation)
					quotaReleased = true
				}
			}
			if closeErr != nil {
				return &Error{Cause: closeErr}
			}
			return nil
		},
	}
	retained = true
	return opened, nil
}

func (s *NativeSource) checkSourceAccess(ctx context.Context) (err error) {
	if s.opts.BeforeSourceRead == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			err = dimse.ErrStoreAccessDenied
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	err = s.opts.BeforeSourceRead(ctx)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		return dimse.ErrStoreAccessDenied
	}
	return nil
}

func containsUID(uids []string, uid string) bool {
	for _, u := range uids {
		if u == uid {
			return true
		}
	}
	return false
}

func (s *NativeSource) snapshot(ctx context.Context, f *os.File) (offset, size int64, digest [32]byte, err error) {
	if s.path != nil {
		return s.snapshotPath(ctx, f)
	}
	opened, err := s.base.Open(ctx)
	if opened.Close != nil {
		defer func() { err = errors.Join(err, opened.Close()) }()
	}
	if err != nil {
		return 0, 0, digest, err
	}
	if opened.Close == nil || opened.WriteDataSet == nil || !sameDescriptor(*s.input, opened.Descriptor) {
		return 0, 0, digest, dimse.ErrStoreSourceChanged
	}
	original, _ := transfer.DefaultRegistry.Get(s.input.TransferSyntaxUID)
	hash := sha256.New()
	limit := s.opts.Transcode.Limits.MaxInputBytes
	w := &spoolWriter{ctx: ctx, w: io.MultiWriter(f, hash), remaining: limit, budget: &spoolBudget{remaining: limit}}
	if err := opened.WriteDataSet(ctx, w, original); err != nil {
		return 0, 0, digest, err
	}
	copy(digest[:], hash.Sum(nil))
	return 0, limit - w.remaining, digest, nil
}

func cloneNativeDescriptor(d dimse.StoreDescriptor) dimse.StoreDescriptor {
	d = cloneDescriptor(d)
	if d.TransferSyntaxSizes != nil {
		bounds := make(map[string]int64, len(d.TransferSyntaxSizes))
		for uid, bound := range d.TransferSyntaxSizes {
			bounds[uid] = bound
		}
		d.TransferSyntaxSizes = bounds
	}
	return d
}

func nativeLogicalReadLimit(limits pixeldata.TranscodeLimits, compressedBytes int64) int64 {
	if compressedBytes <= 0 {
		return 1
	}
	if limits.MaxExpansionRatio <= limits.MaxInputBytes/compressedBytes {
		return min(limits.MaxInputBytes, compressedBytes*limits.MaxExpansionRatio)
	}
	return limits.MaxInputBytes
}

func nativePreparationLimit(err error) bool {
	for _, limit := range []error{ErrResourceLimit, dimse.ErrStoreResourceLimit, pixeldata.ErrTranscodeResourceLimit, pixeldata.ErrDecompressResourceLimit, index.ErrResourceLimit, parser.ErrMaxTotalBytesExceeded, parser.ErrMaxElementBytesExceeded, parser.ErrMaxElementsExceeded, parser.ErrMaxFragmentsExceeded, parser.ErrMaxPixelDataBytesExceeded, parser.ErrMaxDepthExceeded} {
		if errors.Is(err, limit) {
			return true
		}
	}
	return false
}
