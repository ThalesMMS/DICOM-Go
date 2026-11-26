// Package realworld maps stored pixel values to real-world values through the
// Real World Value Mapping (PS3.3 C.7.6.16.2.11) and, on request, the Philips
// MR private scale. Nothing here changes stored values, the Modality LUT or
// the default display; callers opt in per use.
package realworld

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	// ErrInvalidMapping reports a Real World Value Mapping item that has
	// neither a usable linear transform nor LUT, or a mapped range that is
	// inconsistent with its LUT.
	ErrInvalidMapping = errors.New("dicom: invalid real world value mapping")
	// ErrIncompleteMapping reports a linear mapping without First/Last
	// Value Mapped (Type 1); it is returned with RangeMissing set.
	ErrIncompleteMapping = errors.New("dicom: real world value mapping lacks first/last value mapped")
)

var (
	tagRealWorldValueMappingSequence = core.NewTag(0x0040, 0x9096)
	tagFirstValueMapped              = core.NewTag(0x0040, 0x9216)
	tagLastValueMapped               = core.NewTag(0x0040, 0x9211)
	tagDoubleFirstValueMapped        = core.NewTag(0x0040, 0x9214)
	tagDoubleLastValueMapped         = core.NewTag(0x0040, 0x9213)
	tagRealWorldValueLUTData         = core.NewTag(0x0040, 0x9212)
	tagRealWorldValueIntercept       = core.NewTag(0x0040, 0x9224)
	tagRealWorldValueSlope           = core.NewTag(0x0040, 0x9225)
	tagLUTExplanation                = core.NewTag(0x0028, 0x3003)
	tagLUTLabel                      = core.NewTag(0x0040, 0x9210)
	tagMeasurementUnitsCodeSequence  = core.NewTag(0x0040, 0x08EA)
	tagQuantityDefinitionSequence    = core.NewTag(0x0040, 0x9220)
	tagValueType                     = core.NewTag(0x0040, 0xA040)
	tagConceptNameCodeSequence       = core.NewTag(0x0040, 0xA043)
	tagConceptCodeSequence           = core.NewTag(0x0040, 0xA168)
	tagCodeValue                     = core.NewTag(0x0008, 0x0100)
	tagCodingSchemeDesignator        = core.NewTag(0x0008, 0x0102)
	tagCodeMeaning                   = core.NewTag(0x0008, 0x0104)
	tagSharedFunctionalGroups        = core.NewTag(0x5200, 0x9229)
	tagPerFrameFunctionalGroups      = core.NewTag(0x5200, 0x9230)
)

// Code is one coded entry (Code Value, Coding Scheme Designator, Code
// Meaning).
type Code struct {
	CodeValue              string
	CodingSchemeDesignator string
	CodeMeaning            string
}

// Unit is the Measurement Units Code Sequence item of a mapping (UCUM for
// standard units).
type Unit = Code

// Empty reports a code with no value, scheme or meaning.
func (c Code) Empty() bool {
	return c.CodeValue == "" && c.CodingSchemeDesignator == "" && c.CodeMeaning == ""
}

// Mapping is one Real World Value Mapping item: stored values from First to
// Last Value Mapped map linearly (Slope, Intercept) or through LUT.
type Mapping struct {
	FirstValueMapped float64
	LastValueMapped  float64
	Slope            float64
	Intercept        float64
	// LUT holds one real-world value per stored value from FirstValueMapped;
	// when present it replaces the linear transform.
	LUT         []float64
	Label       string
	Explanation string
	Units       Unit
	// Quantity is the coded quantity of the Quantity Definition Sequence
	// (0040,9220) item named (246205007, SCT, "Quantity"), such as
	// (113041, DCM, "Apparent Diffusion Coefficient"); empty when absent.
	Quantity Code
	// RangeMissing marks a linear mapping written without First/Last Value
	// Mapped; Map then applies it to every stored value.
	RangeMissing bool
}

// Map returns the real-world value of a stored value, and false when the
// value is outside the mapped range.
func (m Mapping) Map(stored float64) (float64, bool) {
	if math.IsNaN(stored) || !m.RangeMissing && (stored < m.FirstValueMapped || stored > m.LastValueMapped) {
		return 0, false
	}
	if m.LUT != nil {
		index := int(stored - m.FirstValueMapped)
		if index < 0 || index >= len(m.LUT) {
			return 0, false
		}
		return m.LUT[index], true
	}
	return stored*m.Slope + m.Intercept, true
}

// Name is the text that identifies the mapping to a reader: LUT Label,
// then LUT Explanation, then the Quantity meaning, then "RWV".
func (m Mapping) Name() string {
	for _, name := range []string{m.Label, m.Explanation, m.Quantity.CodeMeaning} {
		if name != "" {
			return name
		}
	}
	return "RWV"
}

// UnitText is the unit as displayed next to a value: the UCUM code itself
// (a UCUM code is its own symbol, "mm2/s"), empty for the dimensionless
// UCUM "1", and for other schemes the Code Meaning, then the Code Value.
func (m Mapping) UnitText() string {
	units := m.Units
	if strings.EqualFold(units.CodingSchemeDesignator, "UCUM") && units.CodeValue != "" {
		if units.CodeValue == "1" {
			return ""
		}
		return units.CodeValue
	}
	if units.CodeMeaning != "" {
		return units.CodeMeaning
	}
	return units.CodeValue
}

// Value is the real-world value of one stored value through one mapping;
// Index is the mapping's position in the slice given to ValuesFor.
type Value struct {
	Index   int
	Mapping Mapping
	Value   float64
}

// ValuesFor maps stored through every mapping whose range holds it, in
// sequence order. A stored value may have several real-world values (for
// example one item per quantity, or items whose ranges overlap); none is
// preferred, so callers show each one with its own name and unit.
func ValuesFor(mappings []Mapping, stored float64) []Value {
	var out []Value
	for index, mapping := range mappings {
		if value, ok := mapping.Map(stored); ok && !math.IsNaN(value) && !math.IsInf(value, 0) {
			out = append(out, Value{Index: index, Mapping: mapping, Value: value})
		}
	}
	return out
}

// MappingsAt returns every Real World Value Mapping that applies to the
// zero-based frameIndex: the sequence in the frame's Per-Frame Functional
// Groups item, then in the Shared Functional Groups, then at the top level.
// Items that cannot be used are skipped and reported in the error.
func MappingsAt(obj *object.Object, frameIndex int) ([]Mapping, error) {
	items := mappingItems(obj, frameIndex)
	var out []Mapping
	var errs []error
	for i, item := range items {
		mapping, err := mappingFromItem(item)
		if err != nil {
			errs = append(errs, fmt.Errorf("item %d: %w", i, err))
			if !mapping.RangeMissing {
				continue
			}
		}
		out = append(out, mapping)
	}
	return out, errors.Join(errs...)
}

func mappingItems(obj *object.Object, frameIndex int) []*object.Object {
	if obj == nil {
		return nil
	}
	if items, _, _ := obj.FunctionalGroupItems(frameIndex, tagRealWorldValueMappingSequence); len(items) > 0 {
		return items
	}
	items, _ := obj.GetSequence(tagRealWorldValueMappingSequence)
	return items
}

func mappingFromItem(item *object.Object) (Mapping, error) {
	var m Mapping
	first, okFirst := firstNumber(item, tagDoubleFirstValueMapped, tagFirstValueMapped)
	last, okLast := firstNumber(item, tagDoubleLastValueMapped, tagLastValueMapped)
	lut, lutErr := numbers(item, tagRealWorldValueLUTData)
	hasLUT := lutErr == nil && len(lut) > 0
	switch {
	case okFirst && okLast && last >= first:
		m.FirstValueMapped, m.LastValueMapped = first, last
	case !okFirst && !okLast && !hasLUT:
		// Some writers omit the Type 1 range of a linear mapping; it is
		// kept, flagged, and reported.
		slope, okSlope := firstNumber(item, tagRealWorldValueSlope)
		intercept, okIntercept := firstNumber(item, tagRealWorldValueIntercept)
		if !okSlope || !okIntercept {
			return m, fmt.Errorf("%w: neither LUT nor slope and intercept", ErrInvalidMapping)
		}
		m.Slope, m.Intercept, m.RangeMissing = slope, intercept, true
		m.FirstValueMapped, m.LastValueMapped = math.Inf(-1), math.Inf(1)
		fillDescription(&m, item)
		return m, ErrIncompleteMapping
	default:
		return m, fmt.Errorf("%w: first/last value mapped", ErrInvalidMapping)
	}
	if hasLUT {
		if float64(len(lut)) != last-first+1 {
			return m, fmt.Errorf("%w: %d LUT entries for %g..%g", ErrInvalidMapping, len(lut), first, last)
		}
		m.LUT = lut
	} else {
		slope, okSlope := firstNumber(item, tagRealWorldValueSlope)
		intercept, okIntercept := firstNumber(item, tagRealWorldValueIntercept)
		if !okSlope || !okIntercept {
			return m, fmt.Errorf("%w: neither LUT nor slope and intercept", ErrInvalidMapping)
		}
		m.Slope, m.Intercept = slope, intercept
	}
	fillDescription(&m, item)
	return m, nil
}

func fillDescription(m *Mapping, item *object.Object) {
	m.Label = trimmed(item, tagLUTLabel)
	m.Explanation = trimmed(item, tagLUTExplanation)
	m.Units = firstCode(item, tagMeasurementUnitsCodeSequence)
	m.Quantity = quantity(item)
}

// quantity reads the Quantity row of the mapping item's Quantity Definition
// Sequence (Real World Value Mapping Item Macro, content items): the CODE item
// whose concept name is (246205007, SCT, "Quantity"), or the retired SNOMED
// RT (G-C1C6, SRT, "Quantity") of older writers.
func quantity(item *object.Object) Code {
	rows, _ := item.GetSequence(tagQuantityDefinitionSequence)
	for _, row := range rows {
		name := firstCode(row, tagConceptNameCodeSequence)
		if name.CodeValue != "246205007" && name.CodeValue != "G-C1C6" {
			continue
		}
		if valueType := trimmed(row, tagValueType); valueType != "" && valueType != "CODE" {
			continue
		}
		if code := firstCode(row, tagConceptCodeSequence); !code.Empty() {
			return code
		}
	}
	return Code{}
}

func firstCode(o *object.Object, tag core.Tag) Code {
	items, ok := o.GetSequence(tag)
	if !ok || len(items) == 0 {
		return Code{}
	}
	return Code{
		CodeValue:              trimmed(items[0], tagCodeValue),
		CodingSchemeDesignator: trimmed(items[0], tagCodingSchemeDesignator),
		CodeMeaning:            trimmed(items[0], tagCodeMeaning),
	}
}

func trimmed(o *object.Object, tag core.Tag) string {
	value, _ := o.GetString(tag)
	return strings.TrimSpace(value)
}

func firstNumber(o *object.Object, tags ...core.Tag) (float64, bool) {
	for _, tag := range tags {
		if values, err := numbers(o, tag); err == nil && len(values) > 0 && !math.IsNaN(values[0]) && !math.IsInf(values[0], 0) {
			return values[0], true
		}
	}
	return 0, false
}

// numbers reads a numeric element whatever its VR: DS and IS text, or
// binary FD, FL, US, SS, UL and SL in the object's value byte order.
func numbers(o *object.Object, tag core.Tag) ([]float64, error) {
	return numbersAs(o, tag, core.VR(""))
}

// numbersAs is numbers with the VR to assume for a UN element (private
// elements read without a dictionary).
func numbersAs(o *object.Object, tag core.Tag, unVR core.VR) ([]float64, error) {
	if o == nil {
		return nil, fmt.Errorf("dicom: element %s not found", tag)
	}
	element, ok := o.Get(tag)
	if !ok {
		return nil, fmt.Errorf("dicom: element %s not found", tag)
	}
	vr := element.VR()
	if vr == core.VRUN && unVR != "" {
		vr = unVR
	}
	switch vr {
	case core.VRDS, core.VRIS:
		text, err := stringValues(o, element)
		if err != nil {
			return nil, err
		}
		out := make([]float64, 0, len(text))
		for _, value := range text {
			n, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil {
				return nil, fmt.Errorf("dicom: element %s: %w", tag, err)
			}
			out = append(out, n)
		}
		return out, nil
	}
	raw, ok := o.GetRaw(tag)
	if !ok {
		return nil, fmt.Errorf("dicom: element %s has no binary value", tag)
	}
	order := o.ValueByteOrder()
	var width int
	var decode func([]byte) float64
	switch vr {
	case core.VRFD:
		width, decode = 8, func(b []byte) float64 { return math.Float64frombits(order.Uint64(b)) }
	case core.VRFL:
		width, decode = 4, func(b []byte) float64 { return float64(math.Float32frombits(order.Uint32(b))) }
	case core.VRUS:
		width, decode = 2, func(b []byte) float64 { return float64(order.Uint16(b)) }
	case core.VRSS:
		width, decode = 2, func(b []byte) float64 { return float64(int16(order.Uint16(b))) }
	case core.VRUL:
		width, decode = 4, func(b []byte) float64 { return float64(order.Uint32(b)) }
	case core.VRSL:
		width, decode = 4, func(b []byte) float64 { return float64(int32(order.Uint32(b))) }
	default:
		return nil, fmt.Errorf("dicom: element %s has non-numeric VR %s", tag, vr)
	}
	if len(raw)%width != 0 {
		return nil, fmt.Errorf("dicom: element %s length %d is not a multiple of %d", tag, len(raw), width)
	}
	out := make([]float64, len(raw)/width)
	for i := range out {
		out[i] = decode(raw[i*width:])
	}
	return out, nil
}

// stringValues returns text values, also for a UN element holding text.
func stringValues(o *object.Object, element core.Element) ([]string, error) {
	if values, ok := o.GetStrings(element.Tag()); ok {
		return values, nil
	}
	raw, ok := o.GetRaw(element.Tag())
	if !ok {
		return nil, fmt.Errorf("dicom: element %s has no text value", element.Tag())
	}
	return strings.Split(strings.TrimRight(string(raw), " \x00"), "\\"), nil
}
