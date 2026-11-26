package mpps

import (
	"context"
	"crypto/sha256"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

// Instance is one stored Performed Procedure Step. Attributes holds the
// Attribute List as received (N-CREATE, then each N-SET merged in), without
// the command's SOP Class/Instance UIDs.
type Instance struct {
	SOPInstanceUID string
	Status         string
	Attributes     core.DataSet
	Version        uint64
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// CreateDigest identifies the N-CREATE Attribute List, for the optional
	// identical-replay policy.
	CreateDigest [sha256.Size]byte
}

// DataSet returns the attributes as a caller-owned object.
func (i Instance) DataSet() *object.Object {
	return object.FromDataSet(cloneDataSet(i.Attributes), std.Dictionary)
}

// Clone returns a copy that shares no slices with i.
func (i Instance) Clone() Instance {
	i.Attributes = cloneDataSet(i.Attributes)
	return i
}

func cloneDataSet(ds core.DataSet) core.DataSet {
	out := ds
	out.Elements = make([]core.Element, len(ds.Elements))
	for index, element := range ds.Elements {
		switch value := element.Value.(type) {
		case core.SequenceValue:
			items := make([]core.DataSet, len(value.Items))
			for j, item := range value.Items {
				items[j] = cloneDataSet(item)
			}
			element.Value = core.SequenceValue{Items: items}
		case core.StringValue:
			element.Value = append(core.StringValue(nil), value...)
		case core.RawValue:
			element.Value = core.RawValue(append([]byte(nil), value...))
		}
		out.Elements[index] = element
	}
	return out
}

// Repository persists steps. Create must be atomic and report false for an
// existing UID; CompareAndSwap must compare Version, store next with
// Version+1 atomically, and return ErrConcurrentUpdate on a mismatch. A
// failed call must not leave a partial update. Implementations return clones.
type Repository interface {
	Create(ctx context.Context, instance Instance) (bool, error)
	Get(ctx context.Context, sopInstanceUID string) (Instance, error)
	CompareAndSwap(ctx context.Context, sopInstanceUID string, expectedVersion uint64, next Instance) (Instance, error)
}

// MemoryRepository is a bounded, race-safe reference Repository. It does not
// survive a restart.
type MemoryRepository struct {
	mu    sync.Mutex
	max   int
	items map[string]Instance
}

// NewMemoryRepository holds at most maxInstances steps; 0 means 1000.
func NewMemoryRepository(maxInstances int) (*MemoryRepository, error) {
	if maxInstances < 0 {
		return nil, ErrResourceLimit
	}
	if maxInstances == 0 {
		maxInstances = 1000
	}
	return &MemoryRepository{max: maxInstances, items: map[string]Instance{}}, nil
}

func (r *MemoryRepository) Create(ctx context.Context, instance Instance) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.items[instance.SOPInstanceUID]; ok {
		return false, nil
	}
	if len(r.items) >= r.max {
		return false, ErrResourceLimit
	}
	instance = instance.Clone()
	instance.Version = 1
	r.items[instance.SOPInstanceUID] = instance
	return true, nil
}

func (r *MemoryRepository) Get(ctx context.Context, uid string) (Instance, error) {
	if err := ctx.Err(); err != nil {
		return Instance{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	instance, ok := r.items[uid]
	if !ok {
		return Instance{}, ErrNoSuchInstance
	}
	return instance.Clone(), nil
}

func (r *MemoryRepository) CompareAndSwap(ctx context.Context, uid string, expected uint64, next Instance) (Instance, error) {
	if err := ctx.Err(); err != nil {
		return Instance{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.items[uid]
	if !ok {
		return Instance{}, ErrNoSuchInstance
	}
	if current.Version != expected || next.SOPInstanceUID != uid {
		return Instance{}, ErrConcurrentUpdate
	}
	next = next.Clone()
	next.Version = expected + 1
	r.items[uid] = next
	return next.Clone(), nil
}
