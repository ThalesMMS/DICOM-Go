package parser

import (
	"fmt"
	"io"
	"math"

	"github.com/ThalesMMS/dicom-go/core"
)

// PaddingPolicy controls Data Set Trailing Padding (FFFC,FFFC), including
// padding in sequence items. Padding never changes the source elements.
type PaddingPolicy uint8

const (
	PaddingPreserve PaddingPolicy = iota
	PaddingRemove
	PaddingCreate
)

// DefaultMaxPaddingBytes is the finite default budget for generated padding.
const DefaultMaxPaddingBytes int64 = 16 << 20

// PaddingOptions controls padding of serialized data sets. Alignment includes
// the caller's prefix (File Meta and preamble for Part 10); ItemAlignment counts
// only the Item Value, excluding its header and delimiter. Under Deflate these
// are logical lengths before compression, not physical output lengths.
type PaddingOptions struct {
	Policy        PaddingPolicy
	Alignment     uint32
	ItemAlignment uint32
	// MaxAddedBytes counts generated padding headers, values and any associated
	// Group Length header/value across this write. Zero uses 16 MiB.
	MaxAddedBytes int64
}

// ValidatePaddingOptions checks options without consuming data or output.
func ValidatePaddingOptions(opts PaddingOptions) error {
	if opts.Policy > PaddingCreate {
		return fmt.Errorf("dicom: invalid padding policy %d", opts.Policy)
	}
	if opts.MaxAddedBytes < 0 {
		return fmt.Errorf("dicom: padding budget must not be negative")
	}
	for _, alignment := range []uint32{opts.Alignment, opts.ItemAlignment} {
		if alignment%2 != 0 {
			return fmt.Errorf("dicom: padding alignment must be zero or an even number")
		}
		if alignment != 0 && opts.Policy != PaddingCreate {
			return fmt.Errorf("dicom: padding alignment requires PaddingCreate")
		}
	}
	return nil
}

var tagDataSetPadding = core.NewTag(0xFFFC, 0xFFFC)
var tagPaddingGroupLength = core.NewTag(0xFFFC, 0x0000)

type paddingBudget struct{ remaining int64 }

type paddingCounter struct {
	destination io.Writer
	n           int64
}

func (c *paddingCounter) Write(p []byte) (int, error) {
	n, err := c.destination.Write(p)
	c.n += int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

// PrepareDataSetElements applies padding removal and Group Length policy to
// one data set. Call WriteDataSetPadding after writing the returned elements.
// lengthOf has the same contract as GroupLengthElements.
func (w *Writer) PrepareDataSetElements(elements []core.Element, lengthOf func(core.Element) (int64, error)) ([]core.Element, error) {
	if w.sequenceErr != nil {
		return nil, w.sequenceErr
	}
	if w.opts.Padding.Policy != PaddingPreserve {
		if err := w.admitGroupScratch(len(elements)); err != nil {
			return nil, err
		}
	}
	if w.paddingErr != nil {
		return nil, w.paddingErr
	}
	if w.opts.Padding.Policy != PaddingPreserve {
		kept := make([]core.Element, 0, len(elements))
		for _, el := range elements {
			if el.Tag() == tagDataSetPadding {
				continue
			}
			if w.opts.Padding.Policy == PaddingCreate && w.opts.GroupLength != GroupLengthPreserve && el.Tag().Group == 0xFFFC && el.Tag() != tagPaddingGroupLength {
				return nil, fmt.Errorf("dicom: padding with recalculated Group Length does not support other FFFC attributes")
			}
			if w.opts.Padding.Policy == PaddingCreate && w.opts.GroupLength != GroupLengthPreserve && el.Tag() == tagPaddingGroupLength {
				continue
			}
			kept = append(kept, el)
		}
		elements = kept
	}
	return w.GroupLengthElements(elements, lengthOf)
}

// WriteDataSetPadding completes a data set whose elements have already been
// written. valueBytes excludes Item Header/Delimiter; prefixBytes is used only
// for the top-level data set. original is consulted only for an existing FFFC
// Group Length under GroupLengthRecalculate. It is not mutated.
type paddingLayout struct {
	value, added int64
	groupLength  bool
}

func (w *Writer) WriteDataSetPadding(valueBytes, prefixBytes int64, original []core.Element) error {
	plan, err := w.plannedDataSetPadding(valueBytes, prefixBytes, original)
	if err != nil {
		return err
	}
	return w.emitDataSetPadding(plan)
}

func (w *Writer) plannedDataSetPadding(valueBytes, prefixBytes int64, original []core.Element) (paddingLayout, error) {
	if w.sequenceErr != nil {
		return paddingLayout{}, w.sequenceErr
	}
	if w.paddingErr != nil {
		return paddingLayout{}, w.paddingErr
	}
	opts := w.opts.Padding
	if opts.Policy != PaddingCreate {
		return paddingLayout{}, nil
	}
	alignment := opts.Alignment
	if w.inItem {
		alignment, prefixBytes = opts.ItemAlignment, 0
	}
	if alignment == 0 {
		return paddingLayout{}, nil
	}
	if valueBytes < 0 || prefixBytes < 0 || valueBytes > math.MaxInt64-prefixBytes {
		return paddingLayout{}, fmt.Errorf("dicom: padding length overflow")
	}
	n := valueBytes + prefixBytes
	if n%int64(alignment) == 0 {
		return paddingLayout{}, nil
	}
	groupLength := w.opts.GroupLength == GroupLengthCreate
	if w.opts.GroupLength == GroupLengthRecalculate {
		for _, el := range original {
			if el.Tag() == tagPaddingGroupLength {
				groupLength = true
				break
			}
		}
	}
	header := encodedHeaderLength(w.syntax, core.VROB)
	extra := header
	if groupLength {
		extra += encodedHeaderLength(w.syntax, core.VRUL) + 4
	}
	// All input encodings and alignments are even. Reject malformed lengths
	// instead of emitting an odd OB Value Length.
	if n%2 != 0 {
		return paddingLayout{}, fmt.Errorf("dicom: padding requires an even data set length")
	}
	value := (int64(alignment) - ((n%int64(alignment) + extra) % int64(alignment))) % int64(alignment)
	if value > math.MaxUint32-1 || header+value > math.MaxUint32 {
		return paddingLayout{}, fmt.Errorf("dicom: padding exceeds a defined 32-bit length")
	}
	added := extra + value
	limit := opts.MaxAddedBytes
	if limit == 0 {
		limit = DefaultMaxPaddingBytes
	}
	if added > limit || (!w.measuring && added > w.paddingBudget.remaining) {
		return paddingLayout{}, fmt.Errorf("dicom: generated padding exceeds MaxAddedBytes")
	}
	return paddingLayout{value: value, added: added, groupLength: groupLength}, nil
}
func (w *Writer) emitDataSetPadding(plan paddingLayout) error {
	if plan.added == 0 {
		return nil
	}
	if !w.measuring && plan.added > w.paddingBudget.remaining {
		return fmt.Errorf("dicom: generated padding exceeds MaxAddedBytes")
	}
	if !w.measuring {
		w.paddingBudget.remaining -= plan.added
	}
	if plan.groupLength {
		el := core.Element{Header: core.ElementHeader{Tag: tagPaddingGroupLength, VR: core.VRUL}, Value: core.Uint32Value{uint32(encodedHeaderLength(w.syntax, core.VROB) + plan.value)}}
		if err := w.WriteElement(el); err != nil {
			return err
		}
	}
	if err := w.writeHeader(tagDataSetPadding, core.VROB, core.Length(plan.value)); err != nil {
		return err
	}
	var zeros [4096]byte
	value := plan.value
	for value > 0 {
		n := int64(len(zeros))
		if value < n {
			n = value
		}
		if err := writeAll(w.w, zeros[:int(n)]); err != nil {
			return err
		}
		value -= n
	}
	return nil
}
