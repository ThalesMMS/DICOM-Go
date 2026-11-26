package index

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

var ErrStringPoolClosed = errors.New("dicom index string pool closed")

// StringPoolOptions limits retained unique UTF-8 payload and map entries.
// Zero selects 16384 entries / 4 MiB. The byte budget excludes map/runtime
// overhead and is not an RSS limit. Negative limits are invalid.
type StringPoolOptions struct {
	MaxEntries int
	MaxBytes   int64
}

// StringPoolStats contains no field values, paths or patient information.
type StringPoolStats struct {
	Entries                 int
	Bytes                   int64
	Hits, Inserts, Bypasses uint64
	Closed                  bool
}

// StringPool belongs to one consumer batch. It never reads sources or retains
// Records. Read/ReadPath and all scanner defaults remain independent of it.
// Methods are concurrent-safe. Call Close when the batch ends; emitted strings
// remain valid while consumers retain them. Close does not revoke those strings.
type StringPool struct {
	mu      sync.Mutex
	values  map[string]string
	options StringPoolOptions
	stats   StringPoolStats
}

func NewStringPool(options StringPoolOptions) (*StringPool, error) {
	if options.MaxEntries < 0 || options.MaxBytes < 0 {
		return nil, fmt.Errorf("%w: string pool limits", ErrInvalidOptions)
	}
	if options.MaxEntries == 0 {
		options.MaxEntries = 16384
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = 4 << 20
	}
	return &StringPool{options: options, values: make(map[string]string)}, nil
}

// ShareRecord returns equal builtin metadata with repeated strings shared.
// It copies Patient before replacing strings, preserving the input Record.
// Geometry, Selected, Missing and Diagnostics are unchanged borrowed fields;
// this is not a deep-copy API. Source paths and selected elements are not pooled.
// The caller must not concurrently mutate an input Record or its nested fields.
// Full pools bypass sharing without changing content or failing indexing.
// Cancellation returns a zero Record and an error; bounded strings already
// admitted by that call can remain in the pool until Close. No partial Record
// is published. Cancellation is checked between fields and after locking;
// it cannot interrupt a single Go hash/copy or a mutex acquisition in progress.
func (p *StringPool) ShareRecord(ctx context.Context, input Record) (Record, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	if p == nil {
		return Record{}, ErrStringPoolClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	if p.stats.Closed {
		return Record{}, ErrStringPoolClosed
	}
	out := input
	if input.Patient != nil {
		patient := *input.Patient
		out.Patient = &patient
	}
	fields := []*string{
		&out.FileMeta.TransferSyntaxUID, &out.FileMeta.TransferSyntaxName,
		&out.FileMeta.MediaStorageSOPClassUID, &out.FileMeta.MediaStorageSOPInstanceUID,
		&out.Study.InstanceUID, &out.Study.AccessionNumber, &out.Study.Date, &out.Study.Time, &out.Study.Description,
		&out.Series.InstanceUID, &out.Series.Modality, &out.Series.Number, &out.Series.Date, &out.Series.Time, &out.Series.Description,
		&out.Instance.SOPClassUID, &out.Instance.SOPInstanceUID, &out.Instance.Number,
	}
	if out.Patient != nil {
		fields = append(fields, &out.Patient.Name, &out.Patient.ID, &out.Patient.BirthDate, &out.Patient.Sex)
	}
	for _, field := range fields {
		if err := ctx.Err(); err != nil {
			return Record{}, err
		}
		value := *field
		if value == "" {
			continue
		}
		// Reject giant strings before hashing or cloning. Empty and bypassed fields
		// preserve their existing immutable backing storage without pool ownership.
		if int64(len(value)) > p.options.MaxBytes {
			p.stats.Bypasses++
			continue
		}
		if shared, ok := p.values[value]; ok {
			*field = shared
			p.stats.Hits++
			continue
		}
		if p.stats.Entries >= p.options.MaxEntries || int64(len(value)) > p.options.MaxBytes-p.stats.Bytes {
			p.stats.Bypasses++
			continue
		}
		shared := strings.Clone(value) // never retain a larger substring backing allocation
		p.values[shared] = shared
		p.stats.Entries++
		p.stats.Bytes += int64(len(shared))
		p.stats.Inserts++
		*field = shared
	}
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	return out, nil
}

func (p *StringPool) Stats() StringPoolStats {
	if p == nil {
		return StringPoolStats{Closed: true}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}

// Close drops the pool's ownership, is idempotent, and does not invalidate
// references previously returned to the consumer. Subsequent sharing is refused.
func (p *StringPool) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.values = nil
	p.stats.Entries = 0
	p.stats.Bytes = 0
	p.stats.Closed = true
	return nil
}
