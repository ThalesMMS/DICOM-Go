package realworld

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Exercise the DICOM wire path behind the endpoint getter regression in
// DCMTK dcmfg/tests/t_real_world_value_mapping.cc.
func TestDCMTKReferenceMappedEndpoints(t *testing.T) {
	firstInteger := core.NewTag(0x0040, 0x9216)
	lastInteger := core.NewTag(0x0040, 0x9211)
	firstDouble := core.NewTag(0x0040, 0x9214)
	lastDouble := core.NewTag(0x0040, 0x9213)
	for _, syntax := range []transfer.Syntax{
		transfer.ImplicitVRLittleEndian, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian,
	} {
		t.Run(syntax.Name, func(t *testing.T) {
			for _, tc := range []struct {
				name        string
				vr          core.VR
				signed      uint16
				first, last float64
			}{
				{"unsigned", core.VRUS, 0, 32768, 65535},
				{"signed", core.VRSS, 1, -32768, -41},
				{"double", core.VRFD, 0, -2.25, 4.75},
			} {
				t.Run(tc.name, func(t *testing.T) {
					order := syntax.ByteOrder
					double := func(tag core.Tag, value float64) core.Element {
						raw := make([]byte, 8)
						order.PutUint64(raw, math.Float64bits(value))
						return core.NewRawElement(tag, core.VRFD, raw)
					}
					integer := func(tag core.Tag, value float64) core.Element {
						raw := make([]byte, 2)
						order.PutUint16(raw, uint16(int(value)))
						return core.NewRawElement(tag, tc.vr, raw)
					}
					var first, last core.Element
					if tc.vr == core.VRFD {
						first, last = double(firstDouble, tc.first), double(lastDouble, tc.last)
					} else {
						first, last = integer(firstInteger, tc.first), integer(lastInteger, tc.last)
					}
					item := core.DataSet{Elements: []core.Element{
						first, last, double(tagRealWorldValueSlope, 2), double(tagRealWorldValueIntercept, -3),
					}}
					mapping := sq(tagRealWorldValueMappingSequence, item)
					group := core.DataSet{Elements: []core.Element{mapping}}
					for _, location := range []struct {
						name string
						elem core.Element
					}{
						{"root", mapping},
						{"shared", sq(tagSharedFunctionalGroups, group)},
						{"second frame", sq(tagPerFrameFunctionalGroups, core.DataSet{}, group)},
					} {
						t.Run(location.name, func(t *testing.T) {
							obj := object.FromElements([]core.Element{
								object.USElement(core.NewTag(0x0028, 0x0103), order, tc.signed),
								location.elem,
							}, std.Dictionary)
							obj.SetValueByteOrder(order)
							var buf bytes.Buffer
							if err := object.WriteDataSet(&buf, obj, syntax); err != nil {
								t.Fatalf("WriteDataSet: %v", err)
							}
							parsed, err := object.ReadDataSet(&buf, syntax)
							if err != nil {
								t.Fatalf("ReadDataSet: %v", err)
							}
							mappings, err := MappingsAt(parsed, 1)
							if err != nil || len(mappings) != 1 {
								t.Fatalf("MappingsAt = %+v, %v", mappings, err)
							}
							m := mappings[0]
							if m.RangeMissing || m.FirstValueMapped != tc.first || m.LastValueMapped != tc.last {
								t.Fatalf("mapping bounds = %+v, want %v..%v", m, tc.first, tc.last)
							}
							for _, v := range []float64{tc.first, (tc.first + tc.last) / 2, tc.last} {
								if got, ok := m.Map(v); !ok || got != 2*v-3 {
									t.Errorf("Map(%v) = %v, %t, want %v, true", v, got, ok, 2*v-3)
								}
							}
							for _, v := range []float64{math.Nextafter(tc.first, math.Inf(-1)), math.Nextafter(tc.last, math.Inf(1))} {
								if _, ok := m.Map(v); ok {
									t.Errorf("out-of-range value %v was mapped", v)
								}
							}
						})
					}
				})
			}
		})
	}
}

func TestDCMTKReferenceInvalidMappedEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name string
		ends []core.Element
	}{
		{"only first", []core.Element{ss(tagFirstValueMapped, -5)}},
		{"only last", []core.Element{us(tagLastValueMapped, 5)}},
		{"inverted signed", []core.Element{ss(tagFirstValueMapped, -5), ss(tagLastValueMapped, -20)}},
		{"inverted double", []core.Element{fd(tagDoubleFirstValueMapped, 2.75), fd(tagDoubleLastValueMapped, 2.5)}},
		{"NaN start", []core.Element{fd(tagDoubleFirstValueMapped, math.NaN()), fd(tagDoubleLastValueMapped, 5)}},
		{"infinite end", []core.Element{fd(tagDoubleFirstValueMapped, -5), fd(tagDoubleLastValueMapped, math.Inf(1))}},
		{"odd short value length", []core.Element{core.NewRawElement(tagFirstValueMapped, core.VRSS, []byte{0xff}), ss(tagLastValueMapped, 5)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := core.DataSet{Elements: append(tc.ends, fd(tagRealWorldValueSlope, 1), fd(tagRealWorldValueIntercept, 1))}
			good := linear(25, 25, 2, 1, "ms")
			obj := object.FromElements([]core.Element{sq(tagRealWorldValueMappingSequence, invalid, good)}, nil)
			mappings, err := MappingsAt(obj, 0)
			if !errors.Is(err, ErrInvalidMapping) || errors.Is(err, ErrIncompleteMapping) || len(mappings) != 1 {
				t.Fatalf("MappingsAt = %+v, %v, want a surviving mapping and ErrInvalidMapping", mappings, err)
			}
			if value, ok := mappings[0].Map(25); !ok || value != 51 {
				t.Fatalf("valid singleton mapping gave %v, %t", value, ok)
			}
		})
	}
}
