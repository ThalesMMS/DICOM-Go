package parser

import (
	"errors"
	"fmt"
	"math"
	"unsafe"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
)

// SequenceWriteLimits bounds structural planning. It does not bound caller
// data, primitive encoding scratch, custom rules, or allocations by hooks.
// Zero selects the finite defaults; negative limits are invalid.
type SequenceWriteLimits struct {
	MaxDepth              int
	MaxPlanNodes          int
	MaxPlanBytes          int64
	MaxRetainedValueBytes int64
}

const (
	DefaultMaxWriteDepth               = 128
	DefaultMaxWritePlanNodes           = 1_000_000
	DefaultMaxWritePlanBytes     int64 = 64 << 20
	DefaultMaxRetainedValueBytes int64 = 32 << 20
)

var (
	ErrWritePlanLimit           = errors.New("dicom: sequence write planning limit exceeded")
	ErrEncodedLengthUnavailable = errors.New("dicom: encoded value length unavailable without reading the source")
	ErrGroupLengthAfterHook     = errors.New("dicom: Group Length inconsistent after serialization hook")
)

func normalizeSequenceLimits(l SequenceWriteLimits) (SequenceWriteLimits, error) {
	if l.MaxDepth < 0 || l.MaxPlanNodes < 0 || l.MaxPlanBytes < 0 || l.MaxRetainedValueBytes < 0 {
		return l, fmt.Errorf("%w: limits must not be negative", ErrWritePlanLimit)
	}
	if l.MaxDepth == 0 {
		l.MaxDepth = DefaultMaxWriteDepth
	}
	if l.MaxPlanNodes == 0 {
		l.MaxPlanNodes = DefaultMaxWritePlanNodes
	}
	if l.MaxPlanBytes == 0 {
		l.MaxPlanBytes = DefaultMaxWritePlanBytes
	}
	if l.MaxRetainedValueBytes == 0 {
		l.MaxRetainedValueBytes = DefaultMaxRetainedValueBytes
	}
	return l, nil
}

func checkedWriteAdd(a, b int64) (int64, error) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, fmt.Errorf("dicom: encoded length overflow")
	}
	return a + b, nil
}

func checkedWriteProduct(n int, width uintptr) (int64, error) {
	if n < 0 || width == 0 || width > math.MaxInt64 || uint64(n) > uint64(math.MaxInt64)/uint64(width) {
		return 0, ErrWritePlanLimit
	}
	return int64(n) * int64(width), nil
}

type writePlanBudget struct {
	limits                   SequenceWriteLimits
	nodes                    int
	bytes, retained, padding int64
}

func (b *writePlanBudget) reserve(n int, width uintptr) error {
	if n < 0 || n > b.limits.MaxPlanNodes-b.nodes {
		return ErrWritePlanLimit
	}
	bytes, err := checkedWriteProduct(n, width)
	if err != nil || bytes > b.limits.MaxPlanBytes-b.bytes {
		return ErrWritePlanLimit
	}
	b.nodes += n
	b.bytes += bytes
	return nil
}

func (b *writePlanBudget) reserveBytes(n int64) error {
	if n < 0 || n > b.limits.MaxPlanBytes-b.bytes {
		return ErrWritePlanLimit
	}
	b.bytes += n
	return nil
}

func (w *Writer) checkWriteDepth(depth int) error {
	if w.sequenceErr != nil {
		return w.sequenceErr
	}
	if depth > w.sequenceLimits.MaxDepth {
		return fmt.Errorf("%w: combined sequence/item depth exceeds %d", ErrWritePlanLimit, w.sequenceLimits.MaxDepth)
	}
	return nil
}

// retainedValueSize is allocation-free. It admits logical payload plus
// structural clone requests before validation/CP-246; allocator overhead and
// arbitrary hook/rule allocations remain outside these limits.
func retainedValueSize(el core.Element, limits SequenceWriteLimits, depth int, visited *int, structure *int64) (int64, error) {
	if depth > limits.MaxDepth || *visited >= limits.MaxPlanNodes {
		return 0, ErrWritePlanLimit
	}
	*visited++
	if err := admitCloneStructure(1, unsafe.Sizeof(core.Element{}), structure, limits); err != nil {
		return 0, err
	}
	var n int64
	switch v := el.Value.(type) {
	case core.RawValue:
		n = int64(len(v))
	case core.StringValue:
		if err := admitCloneStructure(len(v), unsafe.Sizeof(""), structure, limits); err != nil {
			return 0, err
		}
		for _, s := range v {
			var err error
			n, err = checkedWriteAdd(n, int64(len(s)))
			if err != nil {
				return 0, err
			}
		}
	case core.SequenceValue:
		if len(v.Items) > limits.MaxPlanNodes-*visited {
			return 0, ErrWritePlanLimit
		}
		*visited += len(v.Items)
		if err := admitCloneStructure(len(v.Items), unsafe.Sizeof(core.DataSet{}), structure, limits); err != nil {
			return 0, err
		}
		for _, item := range v.Items {
			if depth+1 > limits.MaxDepth {
				return 0, ErrWritePlanLimit
			}
			for _, child := range item.Elements {
				size, err := retainedValueSize(child, limits, depth+2, visited, structure)
				if err != nil {
					return 0, err
				}
				n, err = checkedWriteAdd(n, size)
				if err != nil {
					return 0, err
				}
			}
		}
	case core.FragmentSequence:
		if len(v.Fragments) > limits.MaxPlanNodes-*visited {
			return 0, ErrWritePlanLimit
		}
		*visited += len(v.Fragments)
		if err := admitCloneStructure(len(v.Fragments), unsafe.Sizeof([]byte{}), structure, limits); err != nil {
			return 0, err
		}
		n = int64(len(v.OffsetTable))
		for _, fragment := range v.Fragments {
			var err error
			n, err = checkedWriteAdd(n, int64(len(fragment)))
			if err != nil {
				return 0, err
			}
		}
	case core.Uint16Value, core.Int16Value, core.Uint32Value, core.Int32Value, core.Uint64Value, core.Int64Value, core.Float32Value, core.Float64Value, core.TagValue:
		length, err := valueencode.NumericLength(el.Value)
		if err != nil {
			return 0, err
		}
		n = int64(length)
	}
	if n > limits.MaxRetainedValueBytes {
		return 0, ErrWritePlanLimit
	}
	return n, nil
}

func (w *Writer) hookValueFootprint(el core.Element) (int64, int64, error) {
	visited := 0
	var structure int64
	payload, err := retainedValueSize(el, w.sequenceLimits, w.sequenceDepth, &visited, &structure)
	return payload, structure, err
}

func (w *Writer) admitGroupScratch(n int) error {
	// Three exact element arrays and three maps with conservative per-entry
	// charges; runtime buckets/allocator overhead are not a process RSS cap.
	bytes, err := groupScratchBytes(n)
	if err != nil || n > w.sequenceLimits.MaxPlanNodes || bytes > w.sequenceLimits.MaxPlanBytes {
		return ErrWritePlanLimit
	}
	return w.sequenceErr
}

// ValidateSequenceWriteLimits checks options without reading or writing data.
func ValidateSequenceWriteLimits(limits SequenceWriteLimits) error {
	_, err := normalizeSequenceLimits(limits)
	return err
}

func groupScratchBytes(n int) (int64, error) {
	return checkedWriteProduct(n, 3*unsafe.Sizeof(core.Element{})+96)
}

func admitCloneStructure(n int, width uintptr, total *int64, limits SequenceWriteLimits) error {
	bytes, err := checkedWriteProduct(n, width)
	if err != nil || bytes > limits.MaxPlanBytes-*total {
		return ErrWritePlanLimit
	}
	*total += bytes
	return nil
}

// Known active-path isolation creates working and detached values, then
// invokeHook clones the callback input. Keep all three charges until the
// root operation ends, so root/descendant clones cannot each reuse the quota.
func (b *writePlanBudget) reserveHookCopies(payload, structure int64) error {
	p, err := checkedWriteAdd(payload, payload)
	if err == nil {
		p, err = checkedWriteAdd(p, payload)
	}
	if err != nil {
		return ErrWritePlanLimit
	}
	s, err := checkedWriteAdd(structure, structure)
	if err == nil {
		s, err = checkedWriteAdd(s, structure)
	}
	if err != nil {
		return ErrWritePlanLimit
	}
	if p > b.limits.MaxRetainedValueBytes-b.retained || s > b.limits.MaxPlanBytes-b.bytes {
		return ErrWritePlanLimit
	}
	b.retained += p
	b.bytes += s
	return nil
}
func (b *writePlanBudget) reserveHookGrowth(payload, structure, originalPayload, originalStructure int64) error {
	p, s := int64(0), int64(0)
	if payload > originalPayload {
		p = payload - originalPayload
	}
	if structure > originalStructure {
		s = structure - originalStructure
	}
	if p > b.limits.MaxRetainedValueBytes-b.retained || s > b.limits.MaxPlanBytes-b.bytes {
		return ErrWritePlanLimit
	}
	b.retained += p
	b.bytes += s
	return nil
}

// BE/UN normalization shares the root budget with hook clones and layouts.
// Admission is atomic and precedes littleEndianDataSet's backing allocations.
func (b *writePlanBudget) reserveNormalizedValue(payload, structure int64) error {
	if payload < 0 || structure < 0 || payload > b.limits.MaxRetainedValueBytes-b.retained || structure > b.limits.MaxPlanBytes-b.bytes {
		return ErrWritePlanLimit
	}
	b.retained += payload
	b.bytes += structure
	return nil
}
