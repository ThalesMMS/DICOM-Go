package parser

import (
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/core"
)

// GroupLengthPolicy controls Group Length (gggg,0000) attributes outside the
// File Meta group when writing a data set and its sequence items.
type GroupLengthPolicy uint8

const (
	// GroupLengthPreserve writes Group Length attributes as they are (the
	// historical behaviour).
	GroupLengthPreserve GroupLengthPolicy = iota
	// GroupLengthRecalculate keeps the Group Length attributes that are
	// present and writes their correct values (dcmconv +g=).
	GroupLengthRecalculate
	// GroupLengthCreate writes a correct Group Length for every group
	// (dcmconv +g).
	GroupLengthCreate
	// GroupLengthRemove omits every Group Length (dcmconv -g).
	GroupLengthRemove
)

// countingWriter counts bytes and discards them.
type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// groupLengthExempt reports groups whose Group Length is never changed:
// command (0000), File Meta (0002) and item delimitation (FFFE) groups.
func groupLengthExempt(group uint16) bool {
	return group == 0x0000 || group == 0x0002 || group == 0xFFFE
}

// GroupLengthElements applies the writer's GroupLengthPolicy to elements of
// one data set, which must be in the order they will be written. Group
// lengths are computed from encoded metadata as this writer would,
// including the policy inside nested items. lengthOf gives the encoded
// length of elements this writer cannot encode (deferred values, Value ==
// nil); it may be nil when there are none.
func (w *Writer) GroupLengthElements(elements []core.Element, lengthOf func(core.Element) (int64, error)) ([]core.Element, error) {
	policy := w.opts.GroupLength
	if w.sequenceErr != nil {
		return nil, w.sequenceErr
	}
	if policy == GroupLengthPreserve {
		return elements, nil
	}
	if err := w.admitGroupScratch(len(elements)); err != nil {
		return nil, err
	}
	present := make(map[uint16]bool, len(elements))
	kept := make([]core.Element, 0, len(elements))
	for _, el := range elements {
		tag := el.Tag()
		if tag.Element == 0x0000 && !groupLengthExempt(tag.Group) {
			present[tag.Group] = true
			continue
		}
		kept = append(kept, el)
	}
	if policy == GroupLengthRemove {
		return kept, nil
	}

	// Measure the same primitive/layout rules without reading any payload.
	child, err := w.childForDataSet(nil, core.DataSet{Elements: kept})
	if err != nil {
		return nil, err
	}
	child.measuring = true
	lengths := make(map[uint16]int64, len(kept))
	for _, el := range kept {
		var n int64
		if el.Value == nil {
			if lengthOf == nil {
				return nil, fmt.Errorf("dicom: group length of %s needs its value", el.Tag())
			}
			n, err = lengthOf(el)
			if el.Tag() == tagSpecificCharacterSet {
				return nil, fmt.Errorf("dicom: deferred Specific Character Set cannot be measured")
			}
		} else {
			n, err = child.encodedElementSize(el, nil)
		}
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, fmt.Errorf("dicom: negative encoded length")
		}
		group := el.Tag().Group
		lengths[group], err = checkedWriteAdd(lengths[group], n)
		if err != nil {
			return nil, err
		}
	}

	out := make([]core.Element, 0, len(kept)+len(lengths))
	written := make(map[uint16]bool, len(kept))
	for _, el := range kept {
		group := el.Tag().Group
		if !written[group] && !groupLengthExempt(group) && (policy == GroupLengthCreate || present[group]) {
			written[group] = true
			if lengths[group] > math.MaxUint32 {
				return nil, fmt.Errorf("dicom: group %04X is longer than a Group Length can hold", group)
			}
			out = append(out, core.Element{Header: core.ElementHeader{Tag: core.NewTag(group, 0x0000), VR: core.VRUL}, Value: core.Uint32Value{uint32(lengths[group])}})
		}
		out = append(out, el)
	}
	return out, nil
}

// itemElements returns the elements of a sequence item with the group
// length policy applied.
func (w *Writer) itemElements(ds core.DataSet) ([]core.Element, error) {
	return w.PrepareDataSetElements(ds.Elements, func(el core.Element) (int64, error) { return w.encodedElementSize(el, &ds) })
}
