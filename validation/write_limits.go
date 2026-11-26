package validation

import (
	"context"
	"fmt"
	"math"
	"unsafe"

	"github.com/ThalesMMS/dicom-go/core"
)

// WriteLimits bounds requested library copy and work backing during one write.
// Zero selects finite defaults. These are logical admission budgets, not heap/RSS
// limits. Allocations made by caller callbacks and opaque values are excluded.
type WriteLimits struct {
	MaxCopiedValueBytes  int64
	MaxWorkBytes         int64
	MaxScopeNodes        int
	MaxHookRegistrations int
	MaxRuleRegistrations int
}

const (
	DefaultMaxWriteCopiedValueBytes  int64 = 64 << 20
	DefaultMaxWriteWorkBytes         int64 = 64 << 20
	DefaultMaxWriteScopeNodes              = 1_000_000
	DefaultMaxWriteHookRegistrations       = 4096
	DefaultMaxWriteRuleRegistrations       = 1024
)

func normalizeWriteLimits(l WriteLimits) (WriteLimits, error) {
	if l.MaxCopiedValueBytes < 0 || l.MaxWorkBytes < 0 || l.MaxScopeNodes < 0 || l.MaxHookRegistrations < 0 || l.MaxRuleRegistrations < 0 {
		return l, ErrInvalidPolicy
	}
	if l.MaxCopiedValueBytes == 0 {
		l.MaxCopiedValueBytes = DefaultMaxWriteCopiedValueBytes
	}
	if l.MaxWorkBytes == 0 {
		l.MaxWorkBytes = DefaultMaxWriteWorkBytes
	}
	if l.MaxScopeNodes == 0 {
		l.MaxScopeNodes = DefaultMaxWriteScopeNodes
	}
	if l.MaxHookRegistrations == 0 {
		l.MaxHookRegistrations = DefaultMaxWriteHookRegistrations
	}
	if l.MaxRuleRegistrations == 0 {
		l.MaxRuleRegistrations = DefaultMaxWriteRuleRegistrations
	}
	return l, nil
}

type writeBudget struct {
	ctx                   context.Context
	limits                WriteLimits
	copied, work          int64
	nodes                 int
	maxDepth, maxElements int
}

func (b *writeBudget) reserve(copied, work int64, nodes int) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if copied < 0 || work < 0 || nodes < 0 || copied > b.limits.MaxCopiedValueBytes-b.copied || work > b.limits.MaxWorkBytes-b.work || nodes > b.limits.MaxScopeNodes-b.nodes {
		return ErrValidationLimit
	}
	b.copied += copied
	b.work += work
	b.nodes += nodes
	return nil
}
func writeAdd(a, b int64) (int64, error) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, ErrValidationLimit
	}
	return a + b, nil
}
func writeProduct(n int, width uintptr) (int64, error) {
	if n < 0 || width > math.MaxInt64 {
		return 0, ErrValidationLimit
	}
	if width == 0 {
		return 0, nil
	}
	if uint64(n) > uint64(math.MaxInt64)/uint64(width) {
		return 0, ErrValidationLimit
	}
	return int64(n) * int64(width), nil
}

type writeFootprint struct {
	copied, work, scratch, pathWork int64
	nodes                           int
	known                           bool
}

// scanWriteDataSet observes known slice lengths only. It never calls Value's
// EncodedLength, String methods or any deferred/source callback.
func (b *writeBudget) scanWriteDataSet(ds core.DataSet) (writeFootprint, error) {
	f := writeFootprint{known: true}
	visited := 0
	add := func(target *int64, n int64) error { v, e := writeAdd(*target, n); *target = v; return e }
	var walk func(core.DataSet, int) error
	walk = func(ds core.DataSet, depth int) error {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		if depth > b.maxDepth {
			return ErrValidationLimit
		}
		if len(ds.Elements) > b.maxElements-visited || len(ds.Elements) > b.limits.MaxScopeNodes-f.nodes {
			return ErrValidationLimit
		}
		visited += len(ds.Elements)
		f.nodes += len(ds.Elements)
		n, e := writeProduct(len(ds.Elements), unsafe.Sizeof(core.Element{}))
		if e != nil {
			return e
		}
		if e = add(&f.work, n); e != nil {
			return e
		}
		for _, el := range ds.Elements {
			p, e := writeProduct(depth+1, unsafe.Sizeof(PathStep{})*16)
			if e != nil {
				return e
			}
			if e = add(&f.pathWork, p); e != nil {
				return e
			}
			var payload int64
			var structure int64
			var text int64
			var components int
			switch v := el.Value.(type) {
			case nil, core.BulkDataValue:
			case core.RawValue:
				payload = int64(len(v))
				if el.VR().IsStringLike() {
					text = int64(len(v))
					if len(v) >= int(^uint(0)>>1) {
						return ErrValidationLimit
					}
					components = len(v) + 1
				}
			case core.StringValue:
				n, e := writeProduct(len(v), unsafe.Sizeof(string("")))
				if e != nil {
					return e
				}
				structure = n
				components = len(v)
				if structure > b.limits.MaxWorkBytes || int64(components) > b.limits.MaxWorkBytes/32 {
					return ErrValidationLimit
				}
				for _, s := range v {
					if e = add(&text, int64(len(s))); e != nil {
						return e
					}
				}
			case core.Uint16Value:
				payload, e = writeProduct(len(v), 2)
				if e != nil {
					return e
				}
			case core.Int16Value:
				payload, e = writeProduct(len(v), 2)
				if e != nil {
					return e
				}
			case core.Uint32Value:
				payload, e = writeProduct(len(v), 4)
				if e != nil {
					return e
				}
			case core.Int32Value:
				payload, e = writeProduct(len(v), 4)
				if e != nil {
					return e
				}
			case core.Uint64Value:
				payload, e = writeProduct(len(v), 8)
				if e != nil {
					return e
				}
			case core.Int64Value:
				payload, e = writeProduct(len(v), 8)
				if e != nil {
					return e
				}
			case core.Float32Value:
				payload, e = writeProduct(len(v), 4)
				if e != nil {
					return e
				}
			case core.Float64Value:
				payload, e = writeProduct(len(v), 8)
				if e != nil {
					return e
				}
			case core.TagValue:
				payload, e = writeProduct(len(v), unsafe.Sizeof(core.Tag{}))
				if e != nil {
					return e
				}
			case core.SequenceValue:
				if len(v.Items) > b.maxElements-visited || len(v.Items) > b.limits.MaxScopeNodes-f.nodes {
					return ErrValidationLimit
				}
				visited += len(v.Items)
				f.nodes += len(v.Items)
				n, e := writeProduct(len(v.Items), unsafe.Sizeof(core.DataSet{}))
				if e != nil {
					return e
				}
				structure = n
				for _, item := range v.Items {
					if e = walk(item, depth+1); e != nil {
						return e
					}
				}
			case core.FragmentSequence:
				n, e := writeProduct(len(v.Fragments), unsafe.Sizeof([]byte(nil)))
				if e != nil {
					return e
				}
				if n > b.limits.MaxWorkBytes {
					return ErrValidationLimit
				}
				structure = n
				payload = int64(len(v.OffsetTable))
				for _, fragment := range v.Fragments {
					if e = add(&payload, int64(len(fragment))); e != nil {
						return e
					}
				}
			default:
				f.known = false
			}
			if payload < 0 {
				return ErrValidationLimit
			}
			if e = add(&f.copied, payload); e != nil {
				return e
			}
			if e = add(&f.work, structure); e != nil {
				return e
			}
			if text > 0 || components > 0 {
				if text > math.MaxInt64/32 || components > math.MaxInt64/32 {
					return ErrValidationLimit
				}
				// Includes encoded-text expansion, conversions, delimiters/PN splits and
				// charset parse scratch. Conservative; no payload is retained here.
				scratch, e := writeAdd(text*32, int64(components)*32)
				if e != nil {
					return e
				}
				if e = add(&f.scratch, scratch); e != nil {
					return e
				}
				if f.scratch > b.limits.MaxWorkBytes {
					return ErrValidationLimit
				}
			}
		}
		return nil
	}
	err := walk(ds, 0)
	return f, err
}
func (b *writeBudget) admitCopy(ds core.DataSet, multiplier int) error {
	f, e := b.scanWriteDataSet(ds)
	if e != nil {
		return e
	}
	if multiplier < 0 || f.copied > math.MaxInt64/int64(max(1, multiplier)) || f.work > math.MaxInt64/int64(max(1, multiplier)) {
		return ErrValidationLimit
	}
	return b.reserve(f.copied*int64(multiplier), f.work*int64(multiplier), 0)
}
func (b *writeBudget) admitTraversal(ds core.DataSet) (writeFootprint, error) {
	f, e := b.scanWriteDataSet(ds)
	if e != nil {
		return f, e
	}
	// Conservative per-node maps, path strings/copies, array backing and maximum
	// depth. Map allocator/bucket overhead and retained caller data are excluded.
	per, e := writeProduct(f.nodes, uintptr(128))
	if e != nil {
		return f, e
	}
	work, e := writeAdd(f.work, per)
	if e == nil {
		work, e = writeAdd(work, f.pathWork)
	}
	if e == nil {
		work, e = writeAdd(work, f.scratch)
	}
	if e != nil {
		return f, e
	}
	return f, b.reserve(0, work, f.nodes)
}
func (b *writeBudget) admitEvent(event HookEvent) error {
	if event.Element != nil {
		if e := b.admitCopy(core.DataSet{Elements: []core.Element{*event.Element}}, 1); e != nil {
			return e
		}
	}
	if event.DataSet != nil {
		if e := b.admitCopy(*event.DataSet, 1); e != nil {
			return e
		}
	}
	bytes, e := writeProduct(len(event.Path), unsafe.Sizeof(PathStep{}))
	if e != nil {
		return e
	}
	total, e := writeScale(bytes, 16)
	if e != nil {
		return e
	}
	total, e = writeAdd(total, int64(unsafe.Sizeof(HookEvent{}))+int64(unsafe.Sizeof(core.ElementHeader{}))+int64(unsafe.Sizeof(Finding{}))*4)
	if e != nil {
		return e
	}
	return b.reserve(0, total, 0)
}
func (b *writeBudget) admitReport(r Report) error {
	if len(r.Findings) > int(^uint(0)>>1)-len(r.Changes) {
		return ErrValidationLimit
	}
	bytes, e := writeProduct(len(r.Findings)+len(r.Changes), unsafe.Sizeof(Finding{}))
	if e != nil {
		return e
	}
	addPath := func(path Path) error {
		n, e := writeProduct(len(path), unsafe.Sizeof(PathStep{}))
		if e == nil {
			n, e = writeScale(n, 3)
		}
		if e != nil {
			return e
		}
		bytes, e = writeAdd(bytes, n)
		return e
	}
	for _, f := range r.Findings {
		if e = addPath(f.Path); e != nil {
			return e
		}
		n, e := writeProduct(len(f.ExpectedVR), unsafe.Sizeof(core.VR("")))
		if e == nil {
			n, e = writeScale(n, 3)
		}
		if e != nil {
			return e
		}
		bytes, e = writeAdd(bytes, n)
		if e != nil {
			return e
		}
	}
	for _, c := range r.Changes {
		if e = addPath(c.Path); e != nil {
			return e
		}
	}
	bytes, e = writeScale(bytes, 12)
	if e != nil {
		return e
	}
	return b.reserve(0, bytes, 0)
}
func writeScale(n, multiplier int64) (int64, error) {
	if n < 0 || multiplier < 0 || multiplier > 0 && n > math.MaxInt64/multiplier {
		return 0, ErrValidationLimit
	}
	return n * multiplier, nil
}

func writeLimitError(detail string) error { return fmt.Errorf("%w: %s", ErrValidationLimit, detail) }

func (b *writeBudget) admitFinding(f Finding, count int) error {
	bytes, e := writeProduct(count, unsafe.Sizeof(Finding{}))
	if e != nil {
		return e
	}
	paths, e := writeProduct(len(f.Path), unsafe.Sizeof(PathStep{}))
	if e != nil {
		return e
	}
	expected, e := writeProduct(len(f.ExpectedVR), unsafe.Sizeof(core.VR("")))
	if e != nil {
		return e
	}
	total, e := writeAdd(bytes, paths)
	if e == nil {
		total, e = writeAdd(total, expected)
	}
	if e != nil {
		return e
	}
	return b.reserve(0, total, 0)
}
func (b *writeBudget) admitChange(c Change, count int) error {
	bytes, e := writeProduct(count, unsafe.Sizeof(Change{}))
	if e != nil {
		return e
	}
	paths, e := writeProduct(len(c.Path), unsafe.Sizeof(PathStep{}))
	if e != nil {
		return e
	}
	total, e := writeAdd(bytes, paths)
	if e != nil {
		return e
	}
	return b.reserve(0, total, 0)
}
