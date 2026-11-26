package parametricmap

import (
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

const (
	maxMappingsPerFrame = 256
	maxMappingLUTValues = 4 << 20
)

type frameMappings struct {
	mappings []Mapping
	quantity []*object.Object
}

func readFrameMappings(obj *object.Object, perFrame []*object.Object, m *Map, options ReadOptions) error {
	sharedGroups := sequence(obj, tagSharedFunctionalGroups)
	if len(sharedGroups) > 1 || obj.HasDuplicateParsedTag(tagSharedFunctionalGroups) ||
		obj.HasDuplicateParsedTag(tagPerFrameFunctionalGroups) {
		return fmt.Errorf("%w: ambiguous functional groups", ErrInvalidObject)
	}
	var shared *object.Object
	if len(sharedGroups) == 1 {
		shared = sharedGroups[0]
	}
	signed := m.Payload != PayloadInteger || derivedio.Int(obj, derivedio.TagPixelRepresentation) == 1
	label := strings.TrimRight(options.MappingLabel, " ")
	remainingLUTValues := maxMappingLUTValues

	var sharedMappings, topMappings, first frameMappings
	for index := range m.Frames {
		// Use the same per-frame then shared macro precedence as
		// Object.FunctionalGroupItems. Resolve containers once to avoid
		// reopening every frame's groups and duplicating shared LUTs.
		var group *object.Object
		if len(perFrame) > 0 {
			group = perFrame[index]
		}
		var resolved frameMappings
		var err error
		switch {
		case group != nil && group.Has(tagRealWorldValueMappingSeq):
			if shared != nil && shared.Has(tagRealWorldValueMappingSeq) {
				return fmt.Errorf("frame %d: %w: %w", index+1, ErrInvalidObject, object.ErrFunctionalGroupConflict)
			}
			resolved, err = decodeFrameMappings(group, m.Payload, signed, label, &remainingLUTValues)
		case shared != nil && shared.Has(tagRealWorldValueMappingSeq):
			if sharedMappings.mappings == nil {
				sharedMappings, err = decodeFrameMappings(shared, m.Payload, signed, label, &remainingLUTValues)
			}
			resolved = sharedMappings
		default:
			if topMappings.mappings == nil {
				topMappings, err = decodeFrameMappings(obj, m.Payload, signed, label, &remainingLUTValues)
			}
			resolved = topMappings
		}
		if err != nil {
			return fmt.Errorf("frame %d: %w", index+1, err)
		}

		primary := resolved.mappings[0]
		if index == 0 {
			m.Units, m.Quantity = primary.Units, primary.Quantity
			first = resolved
		} else if !sameUnits(primary, first.mappings[0]) || !sameQuantity(resolved.quantity, first.quantity) {
			return fmt.Errorf("%w: units or quantity vary across frames", ErrUnsupportedDimensions)
		}
		// The display labels describe the same coded quantity throughout
		// the scalar result. Use the first frame's spelling consistently.
		primary.Units, primary.Quantity = m.Units, m.Quantity
		m.Frames[index].Mapping = primary
		m.Frames[index].AdditionalMappings = resolved.mappings[1:]
	}
	return nil
}

func decodeFrameMappings(container *object.Object, payload PayloadKind, signed bool, label string, remainingLUTValues *int) (frameMappings, error) {
	if container.HasDuplicateParsedTag(tagRealWorldValueMappingSeq) {
		return frameMappings{}, ErrAmbiguousMapping
	}
	items := sequence(container, tagRealWorldValueMappingSeq)
	if len(items) == 0 {
		return frameMappings{}, ErrMissingMapping
	}
	if len(items) > maxMappingsPerFrame {
		return frameMappings{}, fmt.Errorf("%w: mapping item count", ErrMemoryLimit)
	}
	if label != "" {
		var selected *object.Object
		for _, item := range items {
			itemLabel, ok := mappingLabel(item)
			if !ok {
				return frameMappings{}, fmt.Errorf("%w: invalid LUT Label", ErrMissingMapping)
			}
			if itemLabel == label {
				if selected != nil {
					return frameMappings{}, fmt.Errorf("%w: duplicate selected LUT Label", ErrAmbiguousMapping)
				}
				selected = item
			}
		}
		if selected == nil {
			return frameMappings{}, fmt.Errorf("%w: selected LUT Label absent", ErrMissingMapping)
		}
		items = []*object.Object{selected}
	}

	var out frameMappings
	out.mappings = make([]Mapping, 0, len(items))
	for index, item := range items {
		mapping, err := decodeMappingItem(item, payload, signed, remainingLUTValues)
		if err != nil {
			return frameMappings{}, fmt.Errorf("mapping item %d: %w", index+1, err)
		}
		quantity := sequence(item, tagQuantityDefinitionSeq)
		if index == 0 {
			out.quantity = quantity
		} else if !sameUnits(out.mappings[0], mapping) || !sameQuantity(out.quantity, quantity) {
			return frameMappings{}, fmt.Errorf("%w: incompatible units or quantity", ErrAmbiguousMapping)
		}
		out.mappings = append(out.mappings, mapping)
	}
	if len(out.mappings) > 1 {
		sort.Slice(out.mappings, func(i, j int) bool { return out.mappings[i].FirstValue < out.mappings[j].FirstValue })
		for i, mapping := range out.mappings {
			if !mapping.HasRange || (i > 0 && out.mappings[i-1].LastValue >= mapping.FirstValue) {
				return frameMappings{}, fmt.Errorf("%w: intersecting or unspecified mapping domains", ErrAmbiguousMapping)
			}
		}
	}
	return out, nil
}

func decodeMappingItem(item *object.Object, payload PayloadKind, signed bool, remainingLUTValues *int) (Mapping, error) {
	units := sequence(item, tagMeasurementUnitsCodeSeq)
	if len(units) != 1 || item.HasDuplicateParsedTag(tagMeasurementUnitsCodeSeq) {
		return Mapping{}, fmt.Errorf("%w: expected one measurement units item", ErrMissingMapping)
	}
	mapping := Mapping{
		Units:        readCode(units[0]),
		Quantity:     readQuantity(item),
		unitsVersion: derivedio.CleanString(units[0], tagCodingSchemeVersion),
	}
	if !validUnitsCode(units[0]) || mapping.Units.Value == "" || mapping.Units.Scheme == "" {
		return Mapping{}, fmt.Errorf("%w: incomplete measurement units code", ErrMissingMapping)
	}
	if item.HasDuplicateParsedTag(tagQuantityDefinitionSeq) || (item.Has(tagQuantityDefinitionSeq) && sequence(item, tagQuantityDefinitionSeq) == nil) {
		return Mapping{}, fmt.Errorf("%w: malformed quantity definition", ErrMissingMapping)
	}
	var valid bool
	mapping.Label, valid = mappingLabel(item)
	if !valid {
		return Mapping{}, fmt.Errorf("%w: invalid LUT Label", ErrMissingMapping)
	}
	mapping.FirstValue, mapping.LastValue, mapping.HasRange, valid = mappingRange(item, payload, signed)
	if !valid {
		return Mapping{}, fmt.Errorf("%w: invalid stored-value domain", ErrMissingMapping)
	}
	if lut, present := item.Get(tagRWVLUTData); present {
		if payload != PayloadInteger || !mapping.HasRange || item.Has(tagRWVSlope) || item.Has(tagRWVIntercept) ||
			item.HasDuplicateParsedTag(tagRWVLUTData) || lut.VR() != core.VRFD {
			return Mapping{}, fmt.Errorf("%w: invalid lookup table", ErrMissingMapping)
		}
		count := int(mapping.LastValue-mapping.FirstValue) + 1
		if count > *remainingLUTValues {
			return Mapping{}, fmt.Errorf("%w: mapping lookup values", ErrMemoryLimit)
		}
		if lut.Length() != core.Length(count*8) {
			return Mapping{}, fmt.Errorf("%w: lookup table length differs from domain", ErrMissingMapping)
		}
		mapping.LUT, _ = item.GetFloats(tagRWVLUTData)
		if len(mapping.LUT) != count {
			return Mapping{}, fmt.Errorf("%w: unreadable lookup table", ErrMissingMapping)
		}
		*remainingLUTValues -= count
	} else {
		slope, validSlope := mappingFD(item, tagRWVSlope)
		intercept, validIntercept := mappingFD(item, tagRWVIntercept)
		if !validSlope || !validIntercept {
			return Mapping{}, fmt.Errorf("%w: invalid linear coefficients", ErrMissingMapping)
		}
		mapping.Slope, mapping.Intercept = slope, intercept
	}
	return mapping, nil
}

func validUnitsCode(item *object.Object) bool {
	for _, tag := range []core.Tag{tagCodeValue, tagCodingSchemeDesignator, tagCodingSchemeVersion} {
		elem, present := item.Get(tag)
		if !present && tag == tagCodingSchemeVersion {
			continue
		}
		if !present || elem.VR() != core.VRSH || elem.Length() > 16 || item.HasDuplicateParsedTag(tag) {
			return false
		}
		values, err := item.LookupStrings(tag)
		if err != nil || len(values) != 1 || strings.TrimSpace(values[0]) == "" {
			return false
		}
	}
	return true
}

func mappingLabel(item *object.Object) (string, bool) {
	elem, present := item.Get(tagLUTLabel)
	if !present {
		return "", true
	}
	if elem.VR() != core.VRSH || elem.Length() > 16 || item.HasDuplicateParsedTag(tagLUTLabel) {
		return "", false
	}
	values, err := item.LookupStrings(tagLUTLabel)
	if err != nil || len(values) != 1 {
		return "", false
	}
	return strings.TrimRight(values[0], " "), true
}

func mappingRange(item *object.Object, payload PayloadKind, signed bool) (first, last float64, present, valid bool) {
	hasFloat := item.Has(tagDoubleRWVFirstMapped) || item.Has(tagDoubleRWVLastMapped)
	hasInteger := item.Has(tagRWVFirstMapped) || item.Has(tagRWVLastMapped)
	if !hasFloat && !hasInteger {
		// Older producers omitted both ends of unbounded linear mappings.
		return 0, 0, false, true
	}
	var integerFirst, integerLast float64
	if hasInteger {
		var firstOK, lastOK bool
		integerFirst, firstOK = mappingInteger(item, tagRWVFirstMapped, signed)
		integerLast, lastOK = mappingInteger(item, tagRWVLastMapped, signed)
		if !firstOK || !lastOK || integerFirst > integerLast {
			return 0, 0, false, false
		}
	}
	if !hasFloat {
		return integerFirst, integerLast, true, true
	}
	if payload == PayloadInteger {
		return 0, 0, false, false
	}
	first, firstOK := mappingFD(item, tagDoubleRWVFirstMapped)
	last, lastOK := mappingFD(item, tagDoubleRWVLastMapped)
	if !firstOK || !lastOK || first > last || (hasInteger && (first != integerFirst || last != integerLast)) {
		return 0, 0, false, false
	}
	return first, last, true, true
}

func mappingInteger(item *object.Object, tag core.Tag, signed bool) (float64, bool) {
	elem, ok := item.Get(tag)
	if !ok || (elem.VR() != core.VRSS && elem.VR() != core.VRUS) || elem.Length() != 2 || item.HasDuplicateParsedTag(tag) {
		return 0, false
	}
	values, err := item.GetInts(tag)
	if err != nil || len(values) != 1 {
		return 0, false
	}
	// The Implicit VR dictionary can supply US for a signed field.
	if signed {
		return float64(int16(values[0])), true
	}
	return float64(values[0]), values[0] >= 0
}

func mappingFD(item *object.Object, tag core.Tag) (float64, bool) {
	elem, ok := item.Get(tag)
	if !ok || elem.VR() != core.VRFD || elem.Length() != 8 || item.HasDuplicateParsedTag(tag) {
		return 0, false
	}
	values, err := item.GetFloats(tag)
	if err != nil || len(values) != 1 || !finite(values[0]) {
		return 0, false
	}
	return values[0], true
}

func sameUnits(a, b Mapping) bool {
	return a.Units.Value == b.Units.Value && a.Units.Scheme == b.Units.Scheme && a.unitsVersion == b.unitsVersion
}

// Quantity Definition can also encode measurement methods, derivation, and
// modifiers. Compare the complete definition conservatively, including
// values and order, while allowing display meanings for a code to differ.
func sameQuantity(a, b []*object.Object) bool {
	if len(a) != len(b) {
		return false
	}
	for i, item := range a {
		if item != b[i] && !sameQuantityItem(item, b[i]) {
			return false
		}
	}
	return true
}

func sameQuantityItem(a, b *object.Object) bool {
	left, right := a.Elements(), b.Elements()
	if len(left) != len(right) {
		return false
	}
	for i, elem := range left {
		other := right[i]
		if elem.Tag() != other.Tag() || elem.VR() != other.VR() {
			return false
		}
		switch {
		case elem.Tag() == tagCodeMeaning:
			continue
		case elem.VR() == core.VRSQ:
			if !sameQuantity(sequence(a, elem.Tag()), sequence(b, elem.Tag())) {
				return false
			}
		case elem.VR() == core.VRSH || elem.VR() == core.VRCS || elem.VR() == core.VRLO || elem.VR() == core.VRUI || elem.VR() == core.VRUC || elem.VR() == core.VRUR:
			if !reflect.DeepEqual(elem.StringValues(), other.StringValues()) {
				return false
			}
		default:
			if !reflect.DeepEqual(elem.Value, other.Value) {
				return false
			}
		}
	}
	return true
}

func (f Frame) applyMapping(stored float64) (float64, error) {
	if len(f.AdditionalMappings) == 0 || math.IsNaN(stored) {
		return f.Mapping.Apply(stored)
	}
	if math.IsInf(stored, 0) {
		// No one coefficient set applies to an unmeasured infinity.
		return stored, nil
	}
	if stored <= f.Mapping.LastValue {
		return f.Mapping.Apply(stored)
	}
	index := sort.Search(len(f.AdditionalMappings), func(i int) bool {
		return stored <= f.AdditionalMappings[i].LastValue
	})
	if index == len(f.AdditionalMappings) {
		return 0, ErrOutsideMapping
	}
	return f.AdditionalMappings[index].Apply(stored)
}
