package object

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Attribute coercion with an audit trail (#1164): every top-level value
// replaced, added or removed by an edit or a coercion rule is recorded in
// Original Attributes Sequence (0400,0561), PS3.3 C.12.1.1.9.

var (
	tagOriginalAttributesSequence = core.NewTag(0x0400, 0x0561)
	tagModifiedAttributesSequence = core.NewTag(0x0400, 0x0550)
	tagAttributeModificationDT    = core.NewTag(0x0400, 0x0562)
	tagModifyingSystem            = core.NewTag(0x0400, 0x0563)
	tagSourceOfPreviousValues     = core.NewTag(0x0400, 0x0564)
	tagReasonForModification      = core.NewTag(0x0400, 0x0565)
)

// Reasons for the Attribute Modification (0400,0565).
const (
	// ModificationCoerce: values replaced to fit the local environment, for
	// example by a receive rule.
	ModificationCoerce = "COERCE"
	// ModificationCorrect: values replaced to correct an error.
	ModificationCorrect = "CORRECT"
)

// ErrOriginalAttributes reports an Original Attributes item that cannot be
// recorded.
var ErrOriginalAttributes = errors.New("dicom: invalid Original Attributes record")

// UpdatePolicy decides which attributes of a source UpdateWithPolicy copies.
type UpdatePolicy int

const (
	// UpdateOverwrite copies every source attribute.
	UpdateOverwrite UpdatePolicy = iota
	// UpdateSupplement copies only attributes the target lacks.
	UpdateSupplement
	// UpdateMerge copies attributes the target lacks or has empty.
	UpdateMerge
	// UpdateReplace copies only attributes the target already has.
	UpdateReplace
)

// OriginalAttributesRecord describes one Original Attributes Sequence item.
type OriginalAttributesRecord struct {
	// Modified holds the previous value of every changed attribute; an
	// attribute that did not exist is recorded with an empty value.
	Modified []core.Element
	// Reason is ModificationCoerce or ModificationCorrect.
	Reason string
	// System names the modifying system (Modifying System, LO).
	System string
	// Source names the source of the previous values (Source of Previous
	// Values, LO); empty when unknown.
	Source string
	// Time is the modification time; zero uses time.Now.
	Time time.Time
}

// UpdateWithPolicy copies the top-level attributes of src selected by policy
// into obj and returns the previous values of the attributes whose value
// changed (empty elements for those obj lacked), ready for
// AppendOriginalAttributes. Original Attributes Sequence and File Meta
// elements of src are never copied.
func UpdateWithPolicy(obj, src *Object, policy UpdatePolicy) ([]core.Element, error) {
	if obj == nil || src == nil {
		return nil, fmt.Errorf("dicom: nil object passed to UpdateWithPolicy")
	}
	var modified []core.Element
	for _, element := range src.SortedElements() {
		tag := element.Tag()
		if tag == tagOriginalAttributesSequence || tag.Group == 0x0002 {
			continue
		}
		existing, present := obj.Get(tag)
		switch policy {
		case UpdateSupplement:
			if present {
				continue
			}
		case UpdateMerge:
			if present && !elementEmpty(existing) {
				continue
			}
		case UpdateReplace:
			if !present {
				continue
			}
		case UpdateOverwrite:
		default:
			return nil, fmt.Errorf("dicom: unknown update policy %d", policy)
		}
		if present && elementsEqual(existing, element) {
			continue
		}
		modified = append(modified, previousValue(existing, present, element))
		obj.Put(element)
	}
	return modified, nil
}

// ModifiedAttributes compares the top-level attributes of before and after
// and returns the previous value of every attribute added, removed or
// changed (empty elements for added ones), in tag order. Original Attributes
// Sequence itself is ignored.
func ModifiedAttributes(before, after *Object) []core.Element {
	if before == nil || after == nil {
		return nil
	}
	seen := map[core.Tag]bool{}
	var modified []core.Element
	for _, element := range before.SortedElements() {
		tag := element.Tag()
		seen[tag] = true
		if tag == tagOriginalAttributesSequence {
			continue
		}
		if current, ok := after.Get(tag); !ok || !elementsEqual(element, current) {
			modified = append(modified, element)
		}
	}
	for _, element := range after.SortedElements() {
		if tag := element.Tag(); !seen[tag] && tag != tagOriginalAttributesSequence {
			modified = append(modified, previousValue(core.Element{}, false, element))
		}
	}
	sort.SliceStable(modified, func(i, j int) bool { return modified[i].Tag().Less(modified[j].Tag()) })
	return modified
}

// AppendOriginalAttributes adds one item to the Original Attributes
// Sequence of obj recording the previous values in record. Nothing is added
// when record.Modified is empty.
func AppendOriginalAttributes(obj *Object, record OriginalAttributesRecord) error {
	if obj == nil {
		return fmt.Errorf("dicom: nil object passed to AppendOriginalAttributes")
	}
	if len(record.Modified) == 0 {
		return nil
	}
	reason := strings.TrimSpace(record.Reason)
	if reason != ModificationCoerce && reason != ModificationCorrect {
		return fmt.Errorf("%w: reason %q is not COERCE or CORRECT", ErrOriginalAttributes, record.Reason)
	}
	system := strings.TrimSpace(record.System)
	if system == "" || len(system) > 64 {
		return fmt.Errorf("%w: Modifying System must hold 1 to 64 characters", ErrOriginalAttributes)
	}
	if len(record.Source) > 64 {
		return fmt.Errorf("%w: Source of Previous Values is longer than 64 characters", ErrOriginalAttributes)
	}
	at := record.Time
	if at.IsZero() {
		at = time.Now()
	}
	modified := append([]core.Element(nil), record.Modified...)
	sort.SliceStable(modified, func(i, j int) bool { return modified[i].Tag().Less(modified[j].Tag()) })
	if charset, ok := obj.Get(tagSpecificCharacterSet); ok && !hasTag(modified, tagSpecificCharacterSet) {
		// The previous values are encoded in the data set's character set.
		modified = append([]core.Element{charset}, modified...)
	}
	item := core.DataSet{Elements: []core.Element{
		{Header: core.ElementHeader{Tag: tagModifiedAttributesSequence, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: modified}}}},
		newStringElement(tagAttributeModificationDT, core.VRDT, at.Format("20060102150405.000000-0700")),
		newStringElement(tagModifyingSystem, core.VRLO, system),
		newStringElement(tagSourceOfPreviousValues, core.VRLO, strings.TrimSpace(record.Source)),
		newStringElement(tagReasonForModification, core.VRCS, reason),
	}}
	var items []core.DataSet
	if existing, ok := obj.Get(tagOriginalAttributesSequence); ok {
		sequence, ok := existing.Value.(core.SequenceValue)
		if !ok {
			return fmt.Errorf("%w: existing Original Attributes Sequence is not a sequence", ErrOriginalAttributes)
		}
		items = append(items, sequence.Items...)
	}
	items = append(items, item)
	obj.Put(core.Element{Header: core.ElementHeader{Tag: tagOriginalAttributesSequence, VR: core.VRSQ}, Value: core.SequenceValue{Items: items}})
	return nil
}

func hasTag(elements []core.Element, tag core.Tag) bool {
	for _, element := range elements {
		if element.Tag() == tag {
			return true
		}
	}
	return false
}

// previousValue is the recorded previous value: existing when present,
// otherwise an empty element of the new attribute's tag and VR.
func previousValue(existing core.Element, present bool, replacement core.Element) core.Element {
	if present {
		return existing
	}
	vr := replacement.VR()
	if vr == core.VRSQ {
		return core.Element{Header: core.ElementHeader{Tag: replacement.Tag(), VR: vr}, Value: core.SequenceValue{}}
	}
	return core.Element{Header: core.ElementHeader{Tag: replacement.Tag(), VR: vr}, Value: core.RawValue{}}
}

func elementEmpty(element core.Element) bool {
	switch value := element.Value.(type) {
	case nil:
		return !element.Header.HasLength() || element.Header.Length == 0
	case core.SequenceValue:
		return len(value.Items) == 0
	case core.StringValue:
		for _, part := range value {
			if strings.TrimSpace(part) != "" {
				return false
			}
		}
		return true
	case core.RawValue:
		return len(value) == 0
	}
	encoded, ok := encodedElement(element)
	return ok && len(encoded) <= 12 && encodedValueLength(encoded) == 0
}

// elementsEqual compares VR and encoded value.
func elementsEqual(a, b core.Element) bool {
	if a.Tag() != b.Tag() || a.VR() != b.VR() {
		return false
	}
	if a.Value == nil || b.Value == nil {
		return a.Value == nil && b.Value == nil && a.Header.Length == b.Header.Length
	}
	encodedA, okA := encodedElement(a)
	encodedB, okB := encodedElement(b)
	if !okA || !okB {
		return fmt.Sprint(a.Value) == fmt.Sprint(b.Value)
	}
	return bytes.Equal(encodedA, encodedB)
}

func encodedElement(element core.Element) ([]byte, bool) {
	var buf bytes.Buffer
	if err := parser.NewWriter(&buf, transfer.ExplicitVRLittleEndian).WriteElement(element); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

// encodedValueLength reads the length field of an Explicit VR Little Endian
// element header.
func encodedValueLength(encoded []byte) int {
	if len(encoded) < 8 {
		return 0
	}
	vr := core.VR(encoded[4:6])
	if vr.UsesLongExplicitLength() && len(encoded) >= 12 {
		return int(uint32(encoded[8]) | uint32(encoded[9])<<8 | uint32(encoded[10])<<16 | uint32(encoded[11])<<24)
	}
	return int(uint16(encoded[6]) | uint16(encoded[7])<<8)
}
