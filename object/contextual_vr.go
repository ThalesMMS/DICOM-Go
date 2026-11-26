package object

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
)

// ErrAmbiguousVR reports an element whose dictionary VR is contextual (for
// example "US or SS") and whose concrete VR cannot be derived from its data
// set. Serializers must not guess it.
var ErrAmbiguousVR = errors.New("dicom: contextual VR cannot be resolved")

var (
	tagPixelRepresentation = core.NewTag(0x0028, 0x0103)
	tagBitsAllocatedVR     = core.NewTag(0x0028, 0x0100)
	tagLUTDescriptorVR     = core.NewTag(0x0028, 0x3002)
)

// Contextual VR rules (PS3.5 Annex A.1 and the IOD rules it defers to):
//
//   - "US or SS" (dictionary token xs): US when Pixel Representation
//     (0028,0103) is 0 and SS when it is 1. Pixel Representation is taken from
//     the element's own data set or item, else from the nearest enclosing data
//     set. An item's value never applies to its siblings. The bytes are the
//     same width either way; descriptors whose first/third values are defined
//     as unsigned must still be read that way by consumers.
//   - "OB or OW" Pixel Data: OB for encapsulated values and Bits Allocated of 8
//     or less, otherwise OW. Other "OB or OW" attributes and Overlay Data: OW.
//   - "US or SS or OW" LUT Data: US when LUT Descriptor's first value is 1,
//     otherwise OW (PS3.5 A.1 permits US or OW).
//
// Implicit VR readers label a contextual element with the dictionary's first
// alternative. After parsing, resolveImplicitContextualVRs replaces only that
// label for "US or SS" when Pixel Representation is known; an explicit VR read
// from the stream is never overwritten.
type contextualVRResolver struct {
	dict  dictionary.DataDictionary
	order binary.ByteOrder
	// authoring resolves empty VRs and fails when context is missing; the
	// implicit read mode relabels dictionary defaults and keeps them otherwise.
	authoring bool
}

func resolveImplicitContextualVRs(ds core.DataSet, dict dictionary.DataDictionary, order binary.ByteOrder) core.DataSet {
	if dict == nil {
		return ds
	}
	r := contextualVRResolver{dict: dict, order: order}
	elements, _, _ := r.elements(ds.Elements, nil)
	ds.Elements = elements
	return ds
}

// ResolveContextualVRs assigns concrete VRs to elements whose VR is empty and
// whose dictionary VR is contextual, using the rules documented above and the
// object's value byte order. Elements that already carry a VR are unchanged.
// When the required context (Pixel Representation, Bits Allocated or LUT
// Descriptor) is missing or invalid, it returns ErrAmbiguousVR and leaves the
// object unchanged.
func (o *Object) ResolveContextualVRs() error {
	if o == nil {
		return fmt.Errorf("dicom: nil object")
	}
	dict := o.dict
	if dict == nil {
		return fmt.Errorf("%w: object has no dictionary", ErrAmbiguousVR)
	}
	r := contextualVRResolver{dict: dict, order: o.ValueByteOrder(), authoring: true}
	elements := o.Elements()
	scope := []pixelRepresentationScope{r.pixelRepresentation(elements)}
	var updates []core.Element
	for _, element := range elements {
		updated, changed, err := r.element(element, elements, scope)
		if err != nil {
			return err
		}
		if changed {
			updates = append(updates, updated)
		}
	}
	for _, updated := range updates {
		o.Put(updated)
	}
	return nil
}

// pixelRepresentationScope is one data set's Pixel Representation: absent,
// a valid 0/1 value, or an invalid value reported only when it is needed.
type pixelRepresentationScope struct {
	value   uint16
	present bool
	err     error
}

// elements resolves one data set. ancestors holds the Pixel Representation
// scopes of enclosing data sets, nearest first.
func (r contextualVRResolver) elements(elements []core.Element, ancestors []pixelRepresentationScope) ([]core.Element, bool, error) {
	scope := append([]pixelRepresentationScope{r.pixelRepresentation(elements)}, ancestors...)
	var out []core.Element
	for i, element := range elements {
		updated, changed, err := r.element(element, elements, scope)
		if err != nil {
			return nil, false, err
		}
		if !changed {
			continue
		}
		if out == nil {
			out = append([]core.Element(nil), elements...)
		}
		out[i] = updated
	}
	if out == nil {
		return elements, false, nil
	}
	return out, true, nil
}

func (r contextualVRResolver) element(element core.Element, siblings []core.Element, scope []pixelRepresentationScope) (core.Element, bool, error) {
	if sequence, ok := element.Value.(core.SequenceValue); ok {
		var items []core.DataSet
		for i, item := range sequence.Items {
			resolved, changed, err := r.elements(item.Elements, scope)
			if err != nil {
				return element, false, err
			}
			if !changed {
				continue
			}
			if items == nil {
				items = append([]core.DataSet(nil), sequence.Items...)
			}
			items[i].Elements = resolved
		}
		if items == nil {
			return element, false, nil
		}
		element.Value = core.SequenceValue{Items: items}
		return element, true, nil
	}
	spec, ok := dictionary.LookupVRSpec(r.dict, element.Tag())
	if !ok || spec.Context() == "" || spec.Context() == dictionary.ContextualVRUP {
		return element, false, nil
	}
	current := element.VR()
	if r.authoring && current != "" {
		return element, false, nil
	}
	if !r.authoring {
		// Only the implicit reader's default label for "US or SS" is replaced.
		if spec.Context() != dictionary.ContextualVRXS || current != core.VRUS {
			return element, false, nil
		}
	}
	vr, err := r.resolve(element, spec.Context(), siblings, scope)
	if err != nil {
		if r.authoring {
			return element, false, err
		}
		return element, false, nil
	}
	if vr == current {
		return element, false, nil
	}
	element.Header.VR = vr
	return element, true, nil
}

func (r contextualVRResolver) resolve(element core.Element, context dictionary.ContextualVR, siblings []core.Element, scope []pixelRepresentationScope) (core.VR, error) {
	switch context {
	case dictionary.ContextualVRXS:
		for _, pr := range scope {
			if pr.err != nil {
				return "", pr.err
			}
			if !pr.present {
				continue
			}
			if pr.value == 1 {
				return core.VRSS, nil
			}
			return core.VRUS, nil
		}
		return "", fmt.Errorf("%w: %s needs Pixel Representation (0028,0103)", ErrAmbiguousVR, element.Tag())
	case dictionary.ContextualVROX, dictionary.ContextualVRPX:
		if element.Tag() != core.TagPixelData {
			return core.VROW, nil
		}
		if _, ok := element.Value.(core.FragmentSequence); ok || element.Header.Length.IsUndefined() && element.Header.HasLength() {
			return core.VROB, nil
		}
		bits, ok, err := r.uint16Value(siblings, tagBitsAllocatedVR, 0)
		if err != nil || !ok {
			return "", fmt.Errorf("%w: Pixel Data needs Bits Allocated (0028,0100)", ErrAmbiguousVR)
		}
		if bits <= 8 {
			return core.VROB, nil
		}
		return core.VROW, nil
	case dictionary.ContextualVRLT:
		entries, ok, err := r.uint16Value(siblings, tagLUTDescriptorVR, 0)
		if err != nil || !ok {
			return "", fmt.Errorf("%w: %s needs LUT Descriptor (0028,3002)", ErrAmbiguousVR, element.Tag())
		}
		if entries == 1 {
			return core.VRUS, nil
		}
		return core.VROW, nil
	default:
		return "", fmt.Errorf("%w: %s has unsupported contextual VR %q", ErrAmbiguousVR, element.Tag(), context)
	}
}

func (r contextualVRResolver) pixelRepresentation(elements []core.Element) pixelRepresentationScope {
	value, ok, err := r.uint16Value(elements, tagPixelRepresentation, 0)
	switch {
	case err != nil:
		return pixelRepresentationScope{err: err}
	case !ok:
		return pixelRepresentationScope{}
	case value > 1:
		return pixelRepresentationScope{err: fmt.Errorf("%w: Pixel Representation %d is neither 0 nor 1", ErrAmbiguousVR, value)}
	default:
		return pixelRepresentationScope{value: value, present: true}
	}
}

func (r contextualVRResolver) uint16Value(elements []core.Element, tag core.Tag, index int) (uint16, bool, error) {
	for _, element := range elements {
		if element.Tag() != tag {
			continue
		}
		switch value := element.Value.(type) {
		case core.Uint16Value:
			if index < len(value) {
				return value[index], true, nil
			}
		case core.Int16Value:
			if index < len(value) {
				return uint16(value[index]), true, nil
			}
		case core.RawValue:
			if len(value) >= 2*(index+1) && len(value)%2 == 0 {
				order := r.order
				if order == nil {
					order = binary.LittleEndian
				}
				return order.Uint16(value[2*index:]), true, nil
			}
		case nil:
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("%w: %s value is not a usable 16-bit integer", ErrAmbiguousVR, tag)
	}
	return 0, false, nil
}
