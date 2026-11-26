package iod

import (
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

func (s *fgState) dimensions(obj *object.Object, frames int) error {
	tag := fgTag(0x00209222)
	raw, exists := fgRawSequence(obj, tag)
	if !exists {
		return nil
	}
	if len(raw) == 0 {
		s.add(InvalidFunctionalGroup, tag.String(), tag, "dimension-index-cardinality", "C.7.6.17", "Present Dimension Index Sequence requires one or more items")
		return nil
	}
	if len(raw) > s.opts.MaxOperations/(frames+1) {
		return ErrFunctionalGroupLimit
	}
	dimensions, _ := obj.GetSequence(tag)
	for frame := 0; frame < frames; frame++ {
		holder, _, _ := s.projections[frame].FunctionalGroupItem(0, fgFrameContent)
		if holder != nil && !holder.Has(fgTag(0x00209157)) {
			s.add(MissingFunctionalGroup, fgPath(frame, fgFrameContent)+"[1]/"+fgTag(0x00209157).String(), fgTag(0x00209157), "dimension-index-values-required", "C.7.6.17", "Present Frame Content requires Dimension Index Values when dimensions are explicit")
		}
	}

	for i, d := range dimensions {
		if err := s.work(); err != nil {
			return err
		}
		path := fmt.Sprintf("%s[%d]/", tag, i+1)
		pointer, valid := fgOneAT(d, fgTag(0x00209165))
		if !valid {
			s.add(InvalidFunctionalGroup, path+fgTag(0x00209165).String(), fgTag(0x00209165), "dimension-pointer-vm", "C.7.6.17", "Dimension Index Pointer must be AT VM1")
			continue
		}
		if pointer == fgFrameContent || pointer == fgTag(0x00209157) {
			s.add(InvalidFunctionalGroup, path+pointer.String(), pointer, "dimension-pointer-forbidden", "C.7.6.17", "Dimension Index Pointer shall not index Frame Content or Dimension Index Values")
			continue
		}
		group, hasGroup := fgOneAT(d, fgTag(0x00209167))
		if d.Has(fgTag(0x00209167)) && !hasGroup {
			s.add(InvalidFunctionalGroup, path+fgTag(0x00209167).String(), fgTag(0x00209167), "functional-group-pointer-vm", "C.7.6.17", "Functional Group Pointer must be AT VM1")
			continue
		}
		if pointer.Group%2 != 0 || hasGroup && group.Group%2 != 0 {
			s.add(ManualFunctionalGroup, path+pointer.String(), pointer, "private-dimension-pointer", "C.7.6.17", "Private dimension semantics and creator resolution need a separate profile")
			continue
		}
		if parent, known := fgPointerParent(pointer); known && !hasGroup {
			s.add(InvalidFunctionalGroup, path+pointer.String(), pointer, "functional-group-pointer-required", "C.7.6.17", "Indexed profile attribute requires Functional Group Pointer")
			_ = parent
			continue
		}
		catalog, _ := fgRules(s.opts.IOD)
		macroPointer := false
		for _, r := range catalog {
			if r.tag == pointer {
				macroPointer = true
			}
		}
		if macroPointer && hasGroup {
			s.add(InvalidFunctionalGroup, path+pointer.String(), pointer, "macro-dimension-pointer-location", "C.7.6.17", "Functional Group Pointer shall be absent when the indexed attribute is itself a functional group")
			continue
		}

		ordinals := &fgDimensionOrdinals{states: map[int64]bool{}}
		for frame := 0; frame < frames; frame++ {
			if err := s.work(); err != nil {
				return err
			}
			holder := obj
			if hasGroup {
				item, _, _ := s.projections[frame].FunctionalGroupItem(0, group)
				if item == nil {
					s.checkDimensionOrdinal(frame, i, true, ordinals)
					s.add(ManualFunctionalGroup, fgPath(frame, group), group, "dimension-group-absent", "C.7.6.17", "Indexed values may be absent; consistency of the missing-value ordinal remains manual")
					continue
				}
				holder = item
			}
			present := holder.Has(pointer)
			if macroPointer {
				items, _, _ := s.projections[frame].FunctionalGroupItems(0, pointer)
				present = len(items) > 0
			}
			if present {
				e, _ := holder.Get(pointer)
				if fgIndexedEmpty(e) {
					present = false
				}
			}
			if !present {
				s.checkDimensionOrdinal(frame, i, true, ordinals)
				s.add(ManualFunctionalGroup, fgPath(frame, pointer), pointer, "dimension-pointer-absent", "C.7.6.17", "Indexed values may be absent; consistency of the missing-value ordinal remains manual")
			} else {
				s.checkDimensionOrdinal(frame, i, false, ordinals)
			}
		}
	}
	return nil
}
func fgOneAT(obj *object.Object, tag core.Tag) (core.Tag, bool) {
	e, ok := obj.Get(tag)
	if !ok || e.VR() != core.VRAT {
		return core.Tag{}, false
	}
	if raw, ok := e.RawBytes(); ok && len(raw) != 4 {
		return core.Tag{}, false
	}
	if tags, ok := e.Value.(core.TagValue); ok && len(tags) != 1 {
		return core.Tag{}, false
	}
	tags, ok := obj.GetTags(tag)
	if !ok || len(tags) != 1 {
		return core.Tag{}, false
	}
	return tags[0], true
}

func fgPointerParent(pointer core.Tag) (core.Tag, bool) {
	switch pointer {
	case fgTag(0x00209056), fgTag(0x00209057), fgTag(0x00209128):
		return fgFrameContent, true
	case fgTag(0x00200032):
		return fgTag(0x00209113), true
	case fgTag(0x00200037):
		return fgTag(0x00209116), true
	case fgTag(0x00280030), fgTag(0x00180050), fgTag(0x00180088):
		return fgTag(0x00289110), true
	case fgTag(0x0062000B):
		return fgTag(0x0062000A), true
	}
	return core.Tag{}, false
}

func (s *fgState) checkDimensionOrdinal(frame, dimension int, missing bool, ordinals *fgDimensionOrdinals) {
	holder, _, _ := s.projections[frame].FunctionalGroupItem(0, fgFrameContent)
	if holder == nil {
		return
	}
	tag := fgTag(0x00209157)
	element, present := holder.Get(tag)
	if !present {
		s.add(MissingFunctionalGroup, fgPath(frame, fgFrameContent)+"[1]/"+tag.String(), tag, "dimension-index-values-required", "C.7.6.17", "Present Frame Content requires Dimension Index Values when dimensions are explicit")
		return
	}
	if element.VR() != core.VRUL || !fgPrimitiveWithin(element, MaxConstraintValueBytes) {
		s.add(InvalidFunctionalGroup, fgPath(frame, tag), tag, "dimension-ordinal-encoding", "C.7.6.17", "Dimension index ordinals require bounded UL values")
		return
	}
	values, err := holder.GetInts(tag)
	if err != nil || dimension >= len(values) {
		return
	}
	ordinal := values[dimension]
	if ordinal <= 0 {
		s.add(InvalidFunctionalGroup, fgPath(frame, tag), tag, "dimension-ordinal-positive", "C.7.6.17", "Dimension index ordinals start at one")
		return
	}
	if wasMissing, seen := ordinals.states[ordinal]; seen && wasMissing != missing {
		s.add(InvalidFunctionalGroup, fgPath(frame, tag), tag, "dimension-missing-ordinal-collision", "C.7.6.17", "Missing indexed values require an ordinal different from present indexed values")
	}
	if missing {
		if ordinals.missingOrdinal != 0 && ordinals.missingOrdinal != ordinal {
			s.add(InvalidFunctionalGroup, fgPath(frame, tag), tag, "dimension-missing-ordinal-consistency", "C.7.6.17", "Missing indexed values must use the same missing-value ordinal")
		} else {
			ordinals.missingOrdinal = ordinal
		}
	}
	ordinals.states[ordinal] = missing
}

// Each dimension admits at most one state per frame; lookup stays constant-time.
type fgDimensionOrdinals struct {
	states         map[int64]bool
	missingOrdinal int64
}

func fgIndexedEmpty(e core.Element) bool {
	switch v := e.Value.(type) {
	case core.SequenceValue:
		return len(v.Items) == 0
	case core.BulkDataValue, core.DiscardedValue:
		return false // unresolved values are not materialized empty values
	default:
		size, supported, _ := fgPrimitiveSize(e.Value, MaxConstraintValueBytes)
		return supported && size == 0
	}
}
