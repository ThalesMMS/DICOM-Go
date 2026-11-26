package dimse

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"

	"github.com/ThalesMMS/dicom-go/index"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// CoercedStoreSource freezes bounded, independent metadata-coerced data sets
// before exposing their descriptor. Each advertised representation invokes the
// callback once on first successful Inspect; Open only reads the frozen bytes.
// Close releases retained bytes and prevents further opens. Already opened
// handles remain usable until their own Close. No temporary files are created.
type CoercedStoreSource struct {
	base       StoreSource
	opts       StoreCoercionOptions
	gate       chan struct{}
	descriptor *StoreDescriptor
	payloads   map[string][]byte
	closed     bool
	opened     int
	charged    int64
}

// StoreCoercionBudget bounds aggregate frozen data-set bytes across sources.
// It is safe to share concurrently. Reservations occur before preparation;
// failures roll back, and bytes stay charged until the source and all opened
// handles close. Borrowed storage, clones, callback allocations and Go overhead
// are outside this retained-payload budget.
type StoreCoercionBudget struct {
	mu            sync.Mutex
	maximum, used int64
}

func NewStoreCoercionBudget(maximum int64) (*StoreCoercionBudget, error) {
	if maximum <= 0 {
		return nil, ErrStoreInvalidOptions
	}
	return &StoreCoercionBudget{maximum: maximum}, nil
}

func (b *StoreCoercionBudget) reserve(maximum int64) (int64, error) {
	if b == nil {
		return maximum, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	available := b.maximum - b.used
	if available <= 0 {
		return 0, ErrStoreResourceLimit
	}
	if maximum > available {
		maximum = available
	}
	b.used += maximum
	return maximum, nil
}

func (b *StoreCoercionBudget) release(bytes int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= bytes
}

func NewCoercedStoreSource(base StoreSource, opts StoreCoercionOptions) (*CoercedStoreSource, error) {
	if base == nil {
		return nil, ErrStoreInvalidSource
	}
	normalized, err := normalizeStoreCoercionOptions(opts)
	if err != nil {
		return nil, err
	}
	return &CoercedStoreSource{base: base, opts: normalized, gate: make(chan struct{}, 1)}, nil
}

func (s *CoercedStoreSource) Inspect(ctx context.Context) (StoreDescriptor, error) {
	if s == nil {
		return StoreDescriptor{}, ErrStoreInvalidSource
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Once acquired the gate must always be released, even if cancellation
	// races acquisition. Check cancellation after acquiring instead.
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return StoreDescriptor{}, ctx.Err()
	}
	defer func() { <-s.gate }()
	if err := contextError(ctx); err != nil {
		return StoreDescriptor{}, err
	}
	if s.closed {
		return StoreDescriptor{}, ErrStoreInvalidSource
	}
	if s.descriptor != nil {
		return cloneStoreDescriptor(*s.descriptor), nil
	}
	reserved, err := s.opts.Budget.reserve(s.opts.CloneOptions.MaxTotalBytes)
	if err != nil {
		return StoreDescriptor{}, &StoreCoercionError{Cause: err}
	}
	committed := false
	defer func() {
		if !committed {
			s.opts.Budget.release(reserved)
		}
	}()
	opts := s.opts
	opts.CloneOptions.MaxTotalBytes = reserved
	input, err := inspectStoreSource(ctx, s.base)
	if err != nil {
		return StoreDescriptor{}, &StoreCoercionError{Cause: err}
	}
	input, err = normalizeStoreDescriptor(input)
	if err != nil {
		return StoreDescriptor{}, &StoreCoercionError{Cause: err}
	}
	output := cloneStoreDescriptor(input)
	if !storeSyntaxUIDPresent(input.WritableTransferSyntaxUIDs, output.TransferSyntaxUID) {
		output.TransferSyntaxUID = input.WritableTransferSyntaxUIDs[0]
	}
	output.TransferSyntaxSizes = make(map[string]int64, len(input.WritableTransferSyntaxUIDs))
	payloads := make(map[string][]byte, len(input.WritableTransferSyntaxUIDs))
	var total int64
	for i, uid := range input.WritableTransferSyntaxUIDs {
		if err := contextError(ctx); err != nil {
			return StoreDescriptor{}, err
		}
		syntax, _ := transfer.DefaultRegistry.Get(uid)
		var opened OpenedStoreSource
		if selected, ok := s.base.(StoreTransferSyntaxSource); ok {
			opened, err = selected.OpenForTransferSyntax(ctx, syntax)
		} else {
			opened, err = openStoreSource(ctx, s.base)
		}
		if err != nil {
			return StoreDescriptor{}, &StoreCoercionError{Cause: err}
		}
		if err = validateOpenedStoreSource(input, opened); err != nil {
			_ = closeStoreSource(opened.Close)
			return StoreDescriptor{}, &StoreCoercionError{Cause: err}
		}
		options := opts
		options.originalClass, options.originalInstance = input.SOPClassUID, input.SOPInstanceUID
		file, _, prepareErr := prepareStoreCoercion(ctx, opened.WriteDataSet, syntax, options)
		closeErr := closeStoreSource(opened.Close)
		if err := errors.Join(prepareErr, closeErr); err != nil {
			return StoreDescriptor{}, &StoreCoercionError{Cause: err}
		}
		class, instance, _ := coercedStoreIdentity(file.Dataset)
		if i == 0 {
			output.SOPClassUID, output.SOPInstanceUID = class, instance
		} else if class != output.SOPClassUID || instance != output.SOPInstanceUID {
			return StoreDescriptor{}, &StoreCoercionError{Cause: ErrStoreCoercionIdentity}
		}
		buffer := storeCoercionBuffer{maximum: reserved - total, ctx: ctx}
		if err := object.WriteDataSet(&buffer, file.Dataset, syntax); err != nil {
			return StoreDescriptor{}, &StoreCoercionError{Cause: err}
		}
		payloads[uid] = buffer.Bytes()
		output.TransferSyntaxSizes[uid] = int64(buffer.Len())
		total += int64(buffer.Len())
		if uid == output.TransferSyntaxUID {
			output.Size = int64(buffer.Len())
			output.PixelDataHeader, output.PixelDataHeaderSet, err = storePixelDataHeader(file.Dataset)
			if err != nil {
				return StoreDescriptor{}, &StoreCoercionError{Cause: err}
			}
		}
	}
	output, err = normalizeStoreDescriptor(output)
	if err != nil {
		return StoreDescriptor{}, &StoreCoercionError{Cause: err}
	}
	if err := contextError(ctx); err != nil {
		return StoreDescriptor{}, err
	}
	s.descriptor, s.payloads = &output, payloads
	s.charged = total
	s.opts.Budget.release(reserved - total)
	committed = true
	return cloneStoreDescriptor(output), nil
}

func (s *CoercedStoreSource) Open(ctx context.Context) (OpenedStoreSource, error) {
	descriptor, err := s.Inspect(ctx)
	if err != nil {
		return OpenedStoreSource{}, err
	}
	syntax, _ := transfer.DefaultRegistry.Get(descriptor.TransferSyntaxUID)
	return s.OpenForTransferSyntax(ctx, syntax)
}

func (s *CoercedStoreSource) OpenForTransferSyntax(ctx context.Context, syntax transfer.Syntax) (OpenedStoreSource, error) {
	descriptor, err := s.Inspect(ctx)
	if err != nil {
		return OpenedStoreSource{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return OpenedStoreSource{}, ctx.Err()
	}
	if s.closed {
		<-s.gate
		return OpenedStoreSource{}, ErrStoreInvalidSource
	}
	uid := transfer.NormalizeUID(syntax.UID)
	payloads := s.payloads
	selected, ok := payloads[uid]
	if !ok {
		<-s.gate
		return OpenedStoreSource{}, ErrStoreTransferSyntax
	}
	s.opened++
	<-s.gate
	syntax, _ = transfer.DefaultRegistry.Get(uid)
	var mu sync.Mutex
	closed := false
	return OpenedStoreSource{
		Descriptor: descriptor,
		WriteDataSet: func(writeCtx context.Context, dst io.Writer, target transfer.Syntax) error {
			mu.Lock()
			defer mu.Unlock()
			if closed {
				return ErrStoreInvalidSource
			}
			payload, ok := payloads[transfer.NormalizeUID(target.UID)]
			if !ok {
				return ErrStoreTransferSyntax
			}
			_, err := copyStoreDataSet(writeCtx, dst, bytes.NewReader(payload))
			return err
		},
		ReadMetadata: func(readCtx context.Context, options index.Options) (index.Result, error) {
			mu.Lock()
			defer mu.Unlock()
			if closed {
				return index.Result{}, ErrStoreInvalidSource
			}
			options = cloneStoreIndexOptions(options)
			options.RawDataSet = &index.RawDataSetOptions{TransferSyntax: syntax}
			if options.Dictionary == nil {
				options.Dictionary = s.opts.Dictionary
			}
			return index.ReadSeeker(readCtx, "", bytes.NewReader(selected), int64(len(selected)), options)
		},
		Close: func() error {
			mu.Lock()
			defer mu.Unlock()
			if closed {
				return nil
			}
			closed = true
			payloads, selected = nil, nil
			s.gate <- struct{}{}
			s.opened--
			if s.closed && s.opened == 0 {
				s.opts.Budget.release(s.charged)
				s.charged = 0
			}
			<-s.gate
			return nil
		},
	}, nil
}

func (s *CoercedStoreSource) Close() error {
	if s == nil {
		return nil
	}
	s.gate <- struct{}{}
	defer func() { <-s.gate }()
	s.closed, s.payloads, s.descriptor = true, nil, nil
	if s.opened == 0 {
		s.opts.Budget.release(s.charged)
		s.charged = 0
	}
	return nil
}
