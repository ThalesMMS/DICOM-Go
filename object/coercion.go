package object

import (
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
)

// AttributeCoercion changes the attributes of a data set and returns the
// previous value of every top-level attribute it changed (an empty element
// for one it added), for AppendOriginalAttributes.
type AttributeCoercion interface {
	Coerce(obj *Object) ([]core.Element, error)
}

// CoercionFunc adapts a function to AttributeCoercion.
type CoercionFunc func(obj *Object) ([]core.Element, error)

// Coerce calls f.
func (f CoercionFunc) Coerce(obj *Object) ([]core.Element, error) { return f(obj) }

// MergeAttributes copies Attributes into the data set under Policy.
type MergeAttributes struct {
	Attributes *Object
	Policy     UpdatePolicy
}

// Coerce implements AttributeCoercion.
func (m MergeAttributes) Coerce(obj *Object) ([]core.Element, error) {
	if m.Attributes == nil {
		return nil, nil
	}
	return UpdateWithPolicy(obj, m.Attributes, m.Policy)
}

// NullifyAttributes empties the listed top-level attributes that are
// present and not already empty.
type NullifyAttributes struct {
	Tags []core.Tag
}

// Coerce implements AttributeCoercion.
func (n NullifyAttributes) Coerce(obj *Object) ([]core.Element, error) {
	var modified []core.Element
	for _, tag := range n.Tags {
		element, ok := obj.Get(tag)
		if !ok || elementEmpty(element) {
			continue
		}
		modified = append(modified, element)
		obj.Put(previousValue(core.Element{}, false, element))
	}
	return modified, nil
}

// RemapUIDs replaces UID values found in UIDs, in UI attributes at any
// depth; a change inside a sequence records the whole top-level sequence.
type RemapUIDs struct {
	UIDs map[string]string
}

// Coerce implements AttributeCoercion.
func (r RemapUIDs) Coerce(obj *Object) ([]core.Element, error) {
	if len(r.UIDs) == 0 {
		return nil, nil
	}
	var modified []core.Element
	for _, element := range obj.SortedElements() {
		if element.Tag() == tagOriginalAttributesSequence {
			continue
		}
		if remapped, changed := r.remap(element); changed {
			modified = append(modified, element)
			obj.Put(remapped)
		}
	}
	return modified, nil
}

func (r RemapUIDs) remap(element core.Element) (core.Element, bool) {
	switch value := element.Value.(type) {
	case core.SequenceValue:
		changed := false
		items := make([]core.DataSet, len(value.Items))
		for i, item := range value.Items {
			elements := make([]core.Element, len(item.Elements))
			for j, child := range item.Elements {
				var childChanged bool
				elements[j], childChanged = r.remap(child)
				changed = changed || childChanged
			}
			items[i] = item
			items[i].Elements = elements
		}
		if changed {
			element.Value = core.SequenceValue{Items: items}
		}
		return element, changed
	}
	if element.VR() != core.VRUI {
		return element, false
	}
	values := element.StringValues()
	changed := false
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = strings.TrimRight(value, "\x00 ")
		if replacement, ok := r.UIDs[out[i]]; ok && replacement != out[i] {
			out[i], changed = replacement, true
		}
	}
	if changed {
		element.Value = core.StringValue(out)
		element.Header.Length = 0
	}
	return element, changed
}

// iso2022SingleByte are the ISO 2022 single-byte terms with an ISO_IR
// equivalent (PS3.3 C.12.1.1.2).
var iso2022SingleByte = map[string]string{
	"ISO 2022 IR 6": "", "ISO 2022 IR 100": "ISO_IR 100", "ISO 2022 IR 101": "ISO_IR 101",
	"ISO 2022 IR 109": "ISO_IR 109", "ISO 2022 IR 110": "ISO_IR 110", "ISO 2022 IR 144": "ISO_IR 144",
	"ISO 2022 IR 127": "ISO_IR 127", "ISO 2022 IR 126": "ISO_IR 126", "ISO 2022 IR 138": "ISO_IR 138",
	"ISO 2022 IR 148": "ISO_IR 148", "ISO 2022 IR 13": "ISO_IR 13", "ISO 2022 IR 166": "ISO_IR 166",
	"ISO 2022 IR 203": "ISO_IR 203",
}

// TrimISO2022CharacterSet replaces a Specific Character Set that uses code
// extensions for at most one single-byte repertoire, and whose text holds
// no escape sequence, with the equivalent term without code extensions
// ("ISO 2022 IR 100" or "\ISO 2022 IR 100" becomes "ISO_IR 100").
type TrimISO2022CharacterSet struct{}

// Coerce implements AttributeCoercion.
func (TrimISO2022CharacterSet) Coerce(obj *Object) ([]core.Element, error) {
	element, ok := obj.Get(tagSpecificCharacterSet)
	if !ok {
		return nil, nil
	}
	target, extended := "", 0
	for _, value := range element.StringValues() {
		value = strings.TrimSpace(value)
		if value == "" || value == "ISO 2022 IR 6" {
			continue
		}
		replacement, ok := iso2022SingleByte[value]
		if !ok {
			return nil, nil
		}
		target, extended = replacement, extended+1
	}
	if extended > 1 || len(element.StringValues()) == 0 || containsEscape(obj.Elements()) {
		return nil, nil
	}
	trimmed := newStringElement(tagSpecificCharacterSet, core.VRCS, target)
	if elementsEqual(element, trimmed) {
		return nil, nil
	}
	obj.Put(trimmed)
	return []core.Element{element}, nil
}

func containsEscape(elements []core.Element) bool {
	for _, element := range elements {
		switch value := element.Value.(type) {
		case core.SequenceValue:
			for _, item := range value.Items {
				if containsEscape(item.Elements) {
					return true
				}
			}
		case core.StringValue:
			for _, part := range value {
				if strings.ContainsRune(part, 0x1b) {
					return true
				}
			}
		case core.RawValue:
			switch element.VR() {
			case core.VRSH, core.VRLO, core.VRST, core.VRLT, core.VRUT, core.VRPN, core.VRUC:
				if strings.ContainsRune(string(value), 0x1b) {
					return true
				}
			}
		}
	}
	return false
}

// CoerceAttributes applies coercions in order and records the previous
// values in one Original Attributes item (record.Modified is ignored): the
// value an attribute had before the first coercion that changed it. It
// returns the recorded elements; nothing is recorded when nothing changed.
func CoerceAttributes(obj *Object, record OriginalAttributesRecord, coercions ...AttributeCoercion) ([]core.Element, error) {
	if obj == nil {
		return nil, fmt.Errorf("dicom: nil object passed to CoerceAttributes")
	}
	var modified []core.Element
	seen := map[core.Tag]bool{}
	for _, coercion := range coercions {
		if coercion == nil {
			continue
		}
		changed, err := coercion.Coerce(obj)
		if err != nil {
			return nil, err
		}
		for _, element := range changed {
			if !seen[element.Tag()] {
				seen[element.Tag()] = true
				modified = append(modified, element)
			}
		}
	}
	record.Modified = modified
	if err := AppendOriginalAttributes(obj, record); err != nil {
		return nil, err
	}
	return modified, nil
}
