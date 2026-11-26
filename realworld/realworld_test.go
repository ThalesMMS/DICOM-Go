package realworld

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func text(tag core.Tag, vr core.VR, values ...string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue(values)}
}

func fd(tag core.Tag, values ...float64) core.Element {
	var raw []byte
	for _, v := range values {
		raw = binary.LittleEndian.AppendUint64(raw, math.Float64bits(v))
	}
	return core.NewRawElement(tag, core.VRFD, raw)
}

func us(tag core.Tag, v uint16) core.Element {
	return core.NewRawElement(tag, core.VRUS, binary.LittleEndian.AppendUint16(nil, v))
}

func ss(tag core.Tag, v int16) core.Element {
	return core.NewRawElement(tag, core.VRSS, binary.LittleEndian.AppendUint16(nil, uint16(v)))
}

func sq(tag core.Tag, items ...core.DataSet) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRSQ}, Value: core.SequenceValue{Items: items}}
}

func units(code, meaning string) core.Element {
	return sq(tagMeasurementUnitsCodeSequence, core.DataSet{Elements: []core.Element{
		text(tagCodeValue, core.VRSH, code), text(tagCodingSchemeDesignator, core.VRSH, "UCUM"), text(tagCodeMeaning, core.VRLO, meaning)}})
}

func linear(first, last uint16, slope, intercept float64, code string) core.DataSet {
	return core.DataSet{Elements: []core.Element{
		us(tagFirstValueMapped, first), us(tagLastValueMapped, last),
		fd(tagRealWorldValueIntercept, intercept), fd(tagRealWorldValueSlope, slope),
		text(tagLUTLabel, core.VRSH, "ADC"), units(code, "um2/s"),
	}}
}

func TestLinearAndLUTMappings(t *testing.T) {
	lutItem := core.DataSet{Elements: []core.Element{
		ss(tagFirstValueMapped, -1), ss(tagLastValueMapped, 1),
		fd(tagRealWorldValueLUTData, 10, 20, 40), text(tagLUTExplanation, core.VRLO, "T1 map"), units("ms", "milliseconds"),
	}}
	obj := object.FromElements([]core.Element{sq(tagRealWorldValueMappingSequence, linear(0, 4095, 0.5, -10, "um2/s"), lutItem)}, nil)
	mappings, err := MappingsAt(obj, 0)
	if err != nil || len(mappings) != 2 {
		t.Fatalf("mappings %+v %v", mappings, err)
	}
	if v, ok := mappings[0].Map(100); !ok || v != 40 || mappings[0].Units.CodeValue != "um2/s" || mappings[0].Label != "ADC" {
		t.Fatalf("linear %v %v %+v", v, ok, mappings[0])
	}
	if _, ok := mappings[0].Map(5000); ok {
		t.Fatal("value outside the mapped range was mapped")
	}
	if v, ok := mappings[1].Map(-1); !ok || v != 10 || mappings[1].Explanation != "T1 map" || mappings[1].Units.CodeValue != "ms" {
		t.Fatalf("LUT %v %v %+v", v, ok, mappings[1])
	}
	if v, ok := mappings[1].Map(1); !ok || v != 40 {
		t.Fatalf("LUT last %v %v", v, ok)
	}
}

func TestMappingsPerFrameThenShared(t *testing.T) {
	frame := func(items ...core.DataSet) core.DataSet {
		return core.DataSet{Elements: []core.Element{sq(tagRealWorldValueMappingSequence, items...)}}
	}
	obj := object.FromElements([]core.Element{
		sq(tagRealWorldValueMappingSequence, linear(0, 10, 9, 9, "1")),
		sq(tagSharedFunctionalGroups, frame(linear(0, 10, 1, 0, "Hz"))),
		sq(tagPerFrameFunctionalGroups, frame(linear(0, 10, 2, 0, "ms")), core.DataSet{}),
	}, nil)
	perFrame, _ := MappingsAt(obj, 0)
	shared, _ := MappingsAt(obj, 1)
	if len(perFrame) != 1 || perFrame[0].Slope != 2 || len(shared) != 1 || shared[0].Slope != 1 {
		t.Fatalf("per-frame %+v shared %+v", perFrame, shared)
	}
}

func TestInvalidMappingReported(t *testing.T) {
	bad := core.DataSet{Elements: []core.Element{us(tagFirstValueMapped, 0), us(tagLastValueMapped, 3), fd(tagRealWorldValueLUTData, 1, 2)}}
	obj := object.FromElements([]core.Element{sq(tagRealWorldValueMappingSequence, bad, linear(0, 1, 1, 0, "1"))}, nil)
	mappings, err := MappingsAt(obj, 0)
	if !errors.Is(err, ErrInvalidMapping) || len(mappings) != 1 {
		t.Fatalf("%v %+v", err, mappings)
	}
}

// philipsFile writes a Part 10 file with the private scaling and reads it
// back in Implicit VR, so the private elements are UN as in real files.
func philipsFile(t *testing.T, creator001, creator005 string, withScaleSlope bool) *object.Object {
	t.Helper()
	elements := []core.Element{
		text(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.4"),
		text(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.3"),
		text(core.NewTag(0x2005, 0x0010), core.VRLO, creator001),
		text(core.NewTag(0x2005, 0x0014), core.VRLO, creator005),
		text(core.NewTag(0x2005, 0x1409), core.VRDS, "-20"),
		text(core.NewTag(0x2005, 0x140A), core.VRDS, "2.5"),
	}
	if withScaleSlope {
		elements = append(elements, core.NewRawElement(core.NewTag(0x2005, 0x100E), core.VRFL, binary.LittleEndian.AppendUint32(nil, math.Float32bits(4))))
	}
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, &object.File{Dataset: object.FromElements(elements, nil), TransferSyntax: transfer.ImplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	f, err := object.ReadFile(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return f.Dataset
}

func TestPhilipsScale(t *testing.T) {
	for _, creators := range [][2]string{{"Philips MR Imaging DD 001", "Philips MR Imaging DD 005"}, {"PHILIPS MR IMAGING DD 001", "PHILIPS MR IMAGING DD 005"}} {
		scale, ok := PhilipsScaleAt(philipsFile(t, creators[0], creators[1], true), 0)
		if !ok || scale.RescaleIntercept != -20 || scale.RescaleSlope != 2.5 || !scale.HasScaleSlope || scale.ScaleSlope != 4 {
			t.Fatalf("%v: %+v %v", creators, scale, ok)
		}
		if v := scale.DisplayValue(100); v != 230 {
			t.Fatalf("display %v", v)
		}
		if v, ok := scale.FloatingPointValue(100); !ok || v != 23 {
			t.Fatalf("floating point %v %v", v, ok)
		}
	}
	scale, ok := PhilipsScaleAt(philipsFile(t, "Philips MR Imaging DD 001", "Philips MR Imaging DD 005", false), 0)
	if _, fp := scale.FloatingPointValue(1); !ok || fp {
		t.Fatalf("floating point without scale slope: %+v", scale)
	}
	// Another vendor's group 2005 creator is never used.
	if _, ok := PhilipsScaleAt(philipsFile(t, "OTHER", "OTHER VENDOR", true), 0); ok {
		t.Fatal("foreign creator read as Philips")
	}
}

func TestLinearMappingWithoutRangeKeptAndReported(t *testing.T) {
	item := core.DataSet{Elements: []core.Element{fd(tagRealWorldValueIntercept, 0), fd(tagRealWorldValueSlope, 9.5)}}
	obj := object.FromElements([]core.Element{sq(tagRealWorldValueMappingSequence, item)}, nil)
	mappings, err := MappingsAt(obj, 0)
	if !errors.Is(err, ErrIncompleteMapping) || len(mappings) != 1 || !mappings[0].RangeMissing {
		t.Fatalf("%v %+v", err, mappings)
	}
	if v, ok := mappings[0].Map(10); !ok || v != 95 {
		t.Fatalf("map %v %v", v, ok)
	}
}

func codeItem(tag core.Tag, value, scheme, meaning string) core.Element {
	return sq(tag, core.DataSet{Elements: []core.Element{
		text(tagCodeValue, core.VRSH, value), text(tagCodingSchemeDesignator, core.VRSH, scheme), text(tagCodeMeaning, core.VRLO, meaning)}})
}

// Double Float Real World Value First Value Mapped is (0040,9214) and Last
// is (0040,9213); a range written only with them must not come out reversed
// (and therefore rejected).
func TestDoubleFloatRangeTags(t *testing.T) {
	item := core.DataSet{Elements: []core.Element{
		fd(core.NewTag(0x0040, 0x9214), -0.5), fd(core.NewTag(0x0040, 0x9213), 100.5),
		fd(tagRealWorldValueIntercept, 1), fd(tagRealWorldValueSlope, 2),
		text(tagLUTLabel, core.VRSH, "T1"), units("ms", "millisecond"),
	}}
	obj := object.FromElements([]core.Element{sq(tagRealWorldValueMappingSequence, item)}, nil)
	mappings, err := MappingsAt(obj, 0)
	if err != nil || len(mappings) != 1 {
		t.Fatalf("mappings %+v %v", mappings, err)
	}
	if m := mappings[0]; m.FirstValueMapped != -0.5 || m.LastValueMapped != 100.5 {
		t.Fatalf("range %v..%v", m.FirstValueMapped, m.LastValueMapped)
	}
	if v, ok := mappings[0].Map(10); !ok || v != 21 {
		t.Fatalf("map %v %v", v, ok)
	}
}

func TestQuantityNameAndUnitText(t *testing.T) {
	quantityRow := core.DataSet{Elements: []core.Element{
		text(tagValueType, core.VRCS, "CODE"),
		codeItem(tagConceptNameCodeSequence, "246205007", "SCT", "Quantity"),
		codeItem(tagConceptCodeSequence, "113041", "DCM", "Apparent Diffusion Coefficient"),
	}}
	otherRow := core.DataSet{Elements: []core.Element{
		text(tagValueType, core.VRCS, "CODE"),
		codeItem(tagConceptNameCodeSequence, "370129005", "SCT", "Measurement Method"),
		codeItem(tagConceptCodeSequence, "113250", "DCM", "Other"),
	}}
	// No LUT Label or Explanation: the quantity names the mapping.
	withQuantity := linear(0, 10, 1, 0, "um2/s")
	withQuantity.Elements = append(withQuantity.Elements[:4], units("um2/s", "um2/s"), sq(tagQuantityDefinitionSequence, otherRow, quantityRow))
	unitless := core.DataSet{Elements: []core.Element{
		us(tagFirstValueMapped, 0), us(tagLastValueMapped, 10), fd(tagRealWorldValueIntercept, 0), fd(tagRealWorldValueSlope, 1),
		text(tagLUTExplanation, core.VRLO, "Ratio"), units("1", "no units"),
	}}
	private := core.DataSet{Elements: []core.Element{
		us(tagFirstValueMapped, 0), us(tagLastValueMapped, 10), fd(tagRealWorldValueIntercept, 0), fd(tagRealWorldValueSlope, 1),
		sq(tagMeasurementUnitsCodeSequence, core.DataSet{Elements: []core.Element{
			text(tagCodeValue, core.VRSH, "X1"), text(tagCodingSchemeDesignator, core.VRSH, "99LOCAL"), text(tagCodeMeaning, core.VRLO, "counts")}}),
	}}
	obj := object.FromElements([]core.Element{sq(tagRealWorldValueMappingSequence, withQuantity, unitless, private)}, nil)
	mappings, err := MappingsAt(obj, 0)
	if err != nil || len(mappings) != 3 {
		t.Fatalf("mappings %+v %v", mappings, err)
	}
	for i, want := range []struct{ name, unit, quantity string }{
		{"Apparent Diffusion Coefficient", "um2/s", "113041"},
		{"Ratio", "", ""},
		{"RWV", "counts", ""},
	} {
		m := mappings[i]
		if m.Name() != want.name || m.UnitText() != want.unit || m.Quantity.CodeValue != want.quantity {
			t.Errorf("mapping %d: name %q unit %q quantity %+v", i, m.Name(), m.UnitText(), m.Quantity)
		}
	}
}

func TestValuesForEveryApplicableMapping(t *testing.T) {
	mappings := []Mapping{
		{FirstValueMapped: 0, LastValueMapped: 99, Slope: 2, Label: "low"},
		{FirstValueMapped: 100, LastValueMapped: 199, Slope: 3, Label: "high"},
		{FirstValueMapped: 0, LastValueMapped: 199, LUT: nil, Slope: 1, Intercept: -1, Label: "all"},
	}
	for _, tc := range []struct {
		stored float64
		want   []float64
		index  []int
	}{
		{10, []float64{20, 9}, []int{0, 2}},
		{150, []float64{450, 149}, []int{1, 2}},
		{500, nil, nil},
	} {
		values := ValuesFor(mappings, tc.stored)
		if len(values) != len(tc.want) {
			t.Fatalf("stored %v: %+v", tc.stored, values)
		}
		for i := range values {
			if values[i].Value != tc.want[i] || values[i].Index != tc.index[i] {
				t.Fatalf("stored %v value %d: %+v", tc.stored, i, values[i])
			}
		}
	}
}
