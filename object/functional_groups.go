package object

import (
	"errors"
	"fmt"
	"sort"

	"github.com/ThalesMMS/dicom-go/core"
)

// ErrFunctionalGroupConflict reports a functional group macro present in both
// the Shared and the Per-Frame Functional Groups Sequence, which PS3.3
// C.7.6.16 forbids. The per-frame item is still returned with it.
var ErrFunctionalGroupConflict = errors.New("dicom: functional group macro is in both the shared and the per-frame functional groups")

// FunctionalGroupSource tells where a functional group macro was found.
type FunctionalGroupSource string

const (
	FunctionalGroupNone     FunctionalGroupSource = ""
	FunctionalGroupPerFrame FunctionalGroupSource = "per-frame"
	FunctionalGroupShared   FunctionalGroupSource = "shared"
)

// HasFunctionalGroups reports whether the object carries a Shared or a
// Per-Frame Functional Groups Sequence (an enhanced multi-frame IOD).
func (o *Object) HasFunctionalGroups() bool {
	return o != nil && (o.Has(tagSharedFunctionalGroups) || o.Has(tagPerFrameFunctionalGroups))
}

// FunctionalGroupItem returns the first item of the functional group macro
// sequence macro (for example Pixel Value Transformation, 0028,9145) that
// applies to the zero-based frameIndex: the item of the frame's Per-Frame
// Functional Groups item when present, otherwise the Shared Functional
// Groups item. A macro found in both is returned from the per-frame group
// together with ErrFunctionalGroupConflict. An out-of-range frameIndex only
// consults the shared group.
//
// This is the one functional group resolver of the module: geometry, SEG,
// Enhanced to Classic conversion, per-frame pixel value transformations,
// NIfTI export, Real World Value Mapping and DICOMDIR records use it, and
// multiframe.Concatenation applies it across the instances of a
// Concatenation.
func (o *Object) FunctionalGroupItem(frameIndex int, macro core.Tag) (*Object, FunctionalGroupSource, error) {
	items, source, err := o.FunctionalGroupItems(frameIndex, macro)
	if len(items) == 0 {
		return nil, source, err
	}
	return items[0], source, err
}

// FunctionalGroupItems is FunctionalGroupItem for macros with several items
// (such as Real World Value Mapping): every item of the sequence that applies
// to the frame.
func (o *Object) FunctionalGroupItems(frameIndex int, macro core.Tag) ([]*Object, FunctionalGroupSource, error) {
	if o == nil {
		return nil, FunctionalGroupNone, nil
	}
	var shared []*Object
	if group := firstSequenceItem(o, tagSharedFunctionalGroups); group != nil {
		shared = nonNilItems(group, macro)
	}
	frame := o.sequenceItemAt(tagPerFrameFunctionalGroups, frameIndex)
	if items := nonNilItems(frame, macro); len(items) > 0 {
		if len(shared) > 0 {
			return items, FunctionalGroupPerFrame, fmt.Errorf("%w: %s", ErrFunctionalGroupConflict, macro)
		}
		return items, FunctionalGroupPerFrame, nil
	}
	if len(shared) > 0 {
		return shared, FunctionalGroupShared, nil
	}
	return nil, FunctionalGroupNone, nil
}

func nonNilItems(obj *Object, tag core.Tag) []*Object {
	if obj == nil {
		return nil
	}
	items, _ := obj.GetSequence(tag)
	out := items[:0:0]
	for _, item := range items {
		if item != nil {
			out = append(out, item)
		}
	}
	return out
}

// FunctionalGroupConflict is a functional group macro found in the Shared
// Functional Groups Sequence and in the Per-Frame items of Frames (zero-based).
type FunctionalGroupConflict struct {
	Macro  core.Tag
	Frames []int
}

// FunctionalGroupConflicts lists the macros present in both the Shared and a
// Per-Frame Functional Groups item, which PS3.3 C.7.6.16 forbids, in tag
// order.
func (o *Object) FunctionalGroupConflicts() []FunctionalGroupConflict {
	shared := firstSequenceItem(o, tagSharedFunctionalGroups)
	if shared == nil {
		return nil
	}
	frames, _ := o.GetSequence(tagPerFrameFunctionalGroups)
	var conflicts []FunctionalGroupConflict
	for _, element := range shared.Elements() {
		macro := element.Tag()
		if element.VR() != core.VRSQ {
			continue
		}
		var conflicting []int
		for index, frame := range frames {
			if frame != nil && frame.Has(macro) {
				conflicting = append(conflicting, index)
			}
		}
		if len(conflicting) > 0 {
			conflicts = append(conflicts, FunctionalGroupConflict{Macro: macro, Frames: conflicting})
		}
	}
	sort.Slice(conflicts, func(i, j int) bool {
		a, b := conflicts[i].Macro, conflicts[j].Macro
		return a.Group < b.Group || (a.Group == b.Group && a.Element < b.Element)
	})
	return conflicts
}
