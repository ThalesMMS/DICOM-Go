package iod_test

import (
	"encoding/binary"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

func valueElement(tag uint32, vr core.VR, value core.Value) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: core.NewTag(uint16(tag>>16), uint16(tag)), VR: vr}, Value: value}
}
func valueObject(elements ...core.Element) *object.Object { return object.FromElements(elements, nil) }
func valueFindings(report iod.Report) []iod.Finding {
	var out []iod.Finding
	for _, f := range report.Findings {
		if f.Kind == iod.InvalidEnumeratedValue || f.Kind == iod.AdditionalDefinedTerm || f.Kind == iod.InvalidContextMultiplicity {
			out = append(out, f)
		}
	}
	return out
}
func sequenceElement(tag uint32, items ...core.DataSet) core.Element {
	return valueElement(tag, core.VRSQ, core.SequenceValue{Items: items})
}

func TestValueConstraintsDefaultCompatibilityAndWrittenObjects(t *testing.T) {
	for name, obj := range syntheticObjects(t) {
		t.Run(name, func(t *testing.T) {
			before := findings(t, obj, iod.Options{})
			after := findings(t, obj, iod.Options{CheckValues: true})
			if got := valueFindings(after); len(got) != 0 {
				t.Fatalf("written object value findings: %+v", got)
			}
			again := findings(t, obj, iod.Options{})
			if !reflect.DeepEqual(before, again) {
				t.Fatal("opt-in changed legacy findings or object")
			}
		})
	}
}

func TestValueConstraintsPatientSexAndScopedPhotometric(t *testing.T) {
	missing := valueObject()
	before := findings(t, missing, iod.Options{IOD: "CT Image"})
	after := findings(t, missing, iod.Options{IOD: "CT Image", CheckValues: true})
	if !hasFinding(after, iod.MissingType2, "(0010,0040)") || !reflect.DeepEqual(before, after) {
		t.Fatal("value membership changed absent Type 2 handling")
	}
	for _, vr := range []core.VR{core.VRCS} {
		for _, value := range []core.Value{core.StringValue{"X"}, core.RawValue("X ")} {
			obj := valueObject(valueElement(0x00100040, vr, value))
			legacy := findings(t, obj, iod.Options{IOD: "CT Image"})
			if len(valueFindings(legacy)) != 0 {
				t.Fatal("value validation was not opt-in")
			}
			report := findings(t, obj, iod.Options{IOD: "CT Image", CheckValues: true})
			if !hasFinding(report, iod.InvalidEnumeratedValue, "(0010,0040)") {
				t.Fatal("invalid Patient Sex not reported")
			}
			for _, f := range valueFindings(report) {
				if f.Source == nil || f.Source.TableID != "table_C.7-1" || f.Source.Row < 1 || f.Rule == "" || f.Source.SHA256 == "" || strings.Contains(f.Message, "X ") {
					t.Fatalf("source or privacy: %+v", f)
				}
			}
		}
	}
	for _, empty := range []core.Value{core.StringValue{}, core.StringValue{""}, core.RawValue("  ")} {
		report := findings(t, valueObject(valueElement(0x00100040, core.VRCS, empty)), iod.Options{IOD: "CT Image", CheckValues: true})
		for _, f := range valueFindings(report) {
			if f.Tag == core.NewTag(0x0010, 0x0040) {
				t.Fatalf("Type 2 empty rejected: %+v", f)
			}
		}
	}
	for _, name := range []string{"CT Image", "Secondary Capture Image"} {
		for _, pi := range []string{"RGB", "MONOCHROME2", "VENDOR_PI"} {
			report := findings(t, valueObject(valueElement(0x00280004, core.VRCS, core.StringValue{pi})), iod.Options{IOD: name, CheckValues: true})
			if got := hasFinding(report, iod.InvalidEnumeratedValue, "(0028,0004)"); got != (name == "CT Image" && pi != "MONOCHROME2") {
				t.Fatalf("%s PI %s: enum-error=%v", name, pi, got)
			}
			if got := hasFinding(report, iod.AdditionalDefinedTerm, "(0028,0004)"); got != (pi == "VENDOR_PI") {
				t.Fatalf("%s PI %s: additional=%v", name, pi, got)
			}
			for _, f := range valueFindings(report) {
				if f.Kind == iod.AdditionalDefinedTerm && f.Severity != iod.SeverityWarning {
					t.Fatal("extensible term became error")
				}
			}
		}
	}
}

func TestValueConstraintsConditionalRawTypedAndEndian(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		for _, typed := range []bool{false, true} {
			for _, seg := range []string{"BINARY", "FRACTIONAL", "LABELMAP"} {
				for _, bits := range []uint16{1, 8, 16, 32} {
					var value core.Value = core.Uint16Value{bits}
					if !typed {
						raw := make([]byte, 2)
						order.PutUint16(raw, bits)
						value = core.RawValue(raw)
					}
					obj := valueObject(valueElement(0x00620001, core.VRCS, core.StringValue{seg}), valueElement(0x00280100, core.VRUS, value))
					obj.SetValueByteOrder(order)
					report := findings(t, obj, iod.Options{IOD: "Segmentation", CheckValues: true})
					want := (seg == "BINARY" && bits != 1) || (seg == "FRACTIONAL" && bits != 8) || (seg == "LABELMAP" && bits != 8 && bits != 16)
					if hasFinding(report, iod.InvalidEnumeratedValue, "(0028,0100)") != want {
						t.Fatalf("order=%v typed=%v seg=%s bits=%d: %+v", order, typed, seg, bits, valueFindings(report))
					}
				}
			}
		}
	}
	// A predicate outside its applicable branch must not enforce BINARY's list.
	obj := valueObject(valueElement(0x00620001, core.VRCS, core.StringValue{"LABELMAP"}), valueElement(0x00280100, core.VRUS, core.Uint16Value{16}), valueElement(0x00280101, core.VRUS, core.Uint16Value{16}))
	if report := findings(t, obj, iod.Options{IOD: "Segmentation", CheckValues: true}); hasFinding(report, iod.InvalidEnumeratedValue, "(0028,0101)") {
		t.Fatalf("false branch enforced: %+v", valueFindings(report))
	}
	// Missing/undecodable equality input is manual, not an arbitrary false branch.
	obj.Remove(core.NewTag(0x0062, 0x0001))
	report := findings(t, obj, iod.Options{IOD: "Segmentation", CheckValues: true})
	if !hasFinding(report, iod.ManualValueConstraint, "(0028,0100)") || hasFinding(report, iod.InvalidEnumeratedValue, "(0028,0100)") {
		t.Fatal("unknown predicate was not left manual")
	}
}

func TestValueConstraintsPositionAndNestedSequence(t *testing.T) {
	for _, typ := range []core.StringValue{{"DERIVED", "PRIMARY", "VENDOR_FLAVOR", "VENDOR_CONTRAST"}, {"ORIGINAL", "PRIMARY", "VENDOR_FLAVOR"}, {"DERIVED", "SECONDARY"}, {"DERIVED", "", "VENDOR_FLAVOR"}} {
		report := findings(t, valueObject(valueElement(0x00080008, core.VRCS, typ)), iod.Options{IOD: "Parametric Map", CheckValues: true})
		want := typ[0] != "DERIVED" || typ[1] != "PRIMARY"
		if hasFinding(report, iod.InvalidEnumeratedValue, "(0008,0008)") != want {
			t.Fatalf("Image Type component restriction: %+v", valueFindings(report))
		}
	}
	item := core.DataSet{Elements: []core.Element{valueElement(0x00720520, core.VRCS, core.StringValue{"MIP", "VENDOR_SUBTYPE"}), valueElement(0x00720510, core.VRCS, core.StringValue{"3D_RENDERING"})}}
	obj := valueObject(sequenceElement(0x00720200, item))
	opts := iod.Options{IOD: "Hanging Protocol", CheckValues: true}
	report := findings(t, obj, opts)
	if hasFinding(report, iod.AdditionalDefinedTerm, "(0072,0200)[1]/(0072,0520)") {
		t.Fatal("Value 1 rule leaked into subtype Value 2")
	}
	item.Elements[0].Value = core.StringValue{"VENDOR_MODE", "MIP"}
	obj.Put(sequenceElement(0x00720200, item))
	report = findings(t, obj, opts)
	if !hasFinding(report, iod.AdditionalDefinedTerm, "(0072,0200)[1]/(0072,0520)") {
		t.Fatalf("nested defined term not reported: %+v", valueFindings(report))
	}
}

func TestValueConstraintsDimensionIndexMultiplicitySharedAndPerFrame(t *testing.T) {
	for _, group := range []uint32{0x52009229, 0x52009230} {
		for _, typed := range []bool{false, true} {
			for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
				for _, count := range []int{1, 2, 3} {
					var value core.Value = core.Uint32Value{1}
					nums := make(core.Uint32Value, count)
					for i := range nums {
						nums[i] = uint32(i + 1)
					}
					value = nums
					if !typed {
						raw := make([]byte, 4*count)
						for i, n := range nums {
							order.PutUint32(raw[4*i:], n)
						}
						value = core.RawValue(raw)
					}
					frame := core.DataSet{Elements: []core.Element{valueElement(0x00209157, core.VRUL, value)}}
					functional := core.DataSet{Elements: []core.Element{sequenceElement(0x00209111, frame)}}
					obj := valueObject(sequenceElement(group, functional), sequenceElement(0x00209222, core.DataSet{}, core.DataSet{}))
					obj.SetValueByteOrder(order)
					opts := iod.Options{IOD: "Enhanced CT Image", CheckValues: true}
					report := findings(t, obj, opts)
					path := core.NewTag(uint16(group>>16), uint16(group)).String() + "[1]/(0020,9111)[1]/(0020,9157)"
					if hasFinding(report, iod.InvalidContextMultiplicity, path) != (count != 2) {
						t.Fatalf("group=%x typed=%v endian=%v count=%d: %+v", group, typed, order, count, valueFindings(report))
					}
					obj.Remove(core.NewTag(0x0020, 0x9222))
					report = findings(t, obj, opts)
					if hasFinding(report, iod.InvalidContextMultiplicity, path) || !hasFinding(report, iod.ManualValueConstraint, path) {
						t.Fatal("absent dimensions invented a count")
					}
					obj.Put(sequenceElement(0x00209222))
					report = findings(t, obj, opts)
					if hasFinding(report, iod.InvalidContextMultiplicity, path) {
						t.Fatal("empty dimensions invented a count")
					}
					legacy := findings(t, obj, iod.Options{IOD: "Enhanced CT Image"})
					if len(valueFindings(legacy)) > 0 {
						t.Fatal("functional group traversal changed default")
					}
				}
			}
		}
	}
}

func TestValueConstraintsSupplementalFrameUsefulnessSharedAndPerFrame(t *testing.T) {
	for _, group := range []uint32{0x52009229, 0x52009230} {
		for _, tag := range []uint32{0x0034000C, 0x00340008} {
			for _, typed := range []bool{false, true} {
				for _, term := range []string{"YES", "NO", "PRIVATE_FLAG", ""} {
					var value core.Value = core.StringValue{term}
					if !typed {
						raw := []byte(term)
						if len(raw)%2 != 0 {
							raw = append(raw, ' ')
						}
						value = core.RawValue(raw)
					}
					usefulness := core.DataSet{Elements: []core.Element{valueElement(tag, core.VRCS, value)}}
					functional := core.DataSet{Elements: []core.Element{sequenceElement(0x00340009, usefulness)}}
					obj := valueObject(sequenceElement(group, functional))
					defaultOpts := iod.Options{IOD: "Enhanced CT Image"}
					before := findings(t, obj, defaultOpts)
					report := findings(t, obj, iod.Options{IOD: "Enhanced CT Image", CheckValues: true})
					path := core.NewTag(uint16(group>>16), uint16(group)).String() + "[1]/(0034,0009)[1]/" + core.NewTag(uint16(tag>>16), uint16(tag)).String()
					if hasFinding(report, iod.InvalidEnumeratedValue, path) != (term == "PRIVATE_FLAG") {
						t.Fatalf("group=%x tag=%x typed=%v: %+v", group, tag, typed, valueFindings(report))
					}
					for _, finding := range valueFindings(report) {
						if finding.Source == nil || finding.Source.TableID != "table_C.7.6.16.2.27-1" || finding.Source.Index != -1 || finding.Source.Depth != 1 || finding.Type != "3" || strings.Contains(finding.Message, "PRIVATE_FLAG") {
							t.Fatalf("supplemental provenance/privacy: %+v", finding)
						}
					}
					if again := findings(t, obj, defaultOpts); !reflect.DeepEqual(before, again) {
						t.Fatal("supplemental values changed cached legacy Type tree")
					}
				}
			}
		}
		// Merely present macros/Items do not introduce supplemental Type rules.
		functional := core.DataSet{Elements: []core.Element{sequenceElement(0x00340009, core.DataSet{})}}
		obj := valueObject(sequenceElement(group, functional))
		before := findings(t, obj, iod.Options{IOD: "Enhanced CT Image"})
		after := findings(t, obj, iod.Options{IOD: "Enhanced CT Image", CheckValues: true})
		if !reflect.DeepEqual(before, after) {
			t.Fatal("empty Frame Usefulness macro introduced Type requirements")
		}
	}
}

func TestValueConstraintsEncodingGatePrivacyAndLimit(t *testing.T) {
	for _, value := range []core.Value{core.StringValue{"secret"}, core.RawValue{0xff, 0}, core.StringValue{"M", "F"}} {
		report := findings(t, valueObject(valueElement(0x00100040, core.VRCS, value)), iod.Options{IOD: "CT Image", CheckValues: true})
		if hasFinding(report, iod.InvalidEnumeratedValue, "(0010,0040)") {
			t.Fatal("malformed representation/global VM duplicated as enum error")
		}
		for _, f := range report.Findings {
			if strings.Contains(f.String(), "secret") || strings.Contains(f.Message, "secret") {
				t.Fatal("PHI copied to finding")
			}
		}
	}
	obj := valueObject(valueElement(0x00100040, core.VRCS, core.StringValue{"X"}), valueElement(0x00280004, core.VRCS, core.StringValue{"VENDOR_PI"}))
	unlimited := findings(t, obj, iod.Options{IOD: "CT Image", CheckValues: true})
	limited := findings(t, obj, iod.Options{IOD: "CT Image", CheckValues: true, MaxFindings: 1})
	if len(limited.Findings) != 1 || limited.Truncated != len(unlimited.Findings)-1 {
		t.Fatal("findings bound changed")
	}
}

func TestValueConstraintsCoverageAndInheritedCharset(t *testing.T) {
	coverage := iod.ValueConstraintCoverage()
	if coverage.Compiled < 1100 || len(coverage.Manual) < 190 {
		t.Fatalf("coverage: compiled=%d manual=%d", coverage.Compiled, len(coverage.Manual))
	}
	for _, r := range coverage.Manual {
		if r.Source.SHA256 == "" || r.Source.TableID == "" || r.Reason == "" {
			t.Fatal("manual rule lost provenance")
		}
	}
	// LO Defined Terms on nested Window Center/Width Explanation should not be
	// decoded as default ASCII when its item inherits a declared UTF-8 charset.
	// The qualified CT list remains explicitly manual rather than a global enum.
	obj := valueObject(valueElement(0x00080005, core.VRCS, core.StringValue{"ISO_IR 192"}), valueElement(0x00280004, core.VRCS, core.StringValue{"RGB"}))
	if report := findings(t, obj, iod.Options{IOD: "Secondary Capture Image", CheckValues: true}); len(valueFindings(report)) > 0 {
		t.Fatal("charset polluted ASCII PI rule")
	}
}

func TestValueConstraintsCharsetAdditionalTermAndBound(t *testing.T) {
	trigger := core.NewTag(0x0018, 0x1061)
	for _, value := range []core.Value{core.StringValue{"CAFÉ"}, core.RawValue([]byte("CAFÉ "))} {
		obj := valueObject(valueElement(0x00080005, core.VRCS, core.StringValue{"ISO_IR 192"}), valueElement(0x00181061, core.VRLO, value))
		report := findings(t, obj, iod.Options{IOD: "Nuclear Medicine Image", CheckValues: true})
		if !hasFinding(report, iod.AdditionalDefinedTerm, trigger.String()) {
			t.Fatal("declared UTF-8 term did not remain extensible")
		}
	}
	// Nested Coding Scheme Registry LO inherits UTF-8 from its parent scope.
	nested := core.DataSet{Elements: []core.Element{valueElement(0x00080112, core.VRLO, core.RawValue([]byte("CAFÉ ")))}}
	objNested := valueObject(valueElement(0x00080005, core.VRCS, core.StringValue{"ISO_IR 192"}), sequenceElement(0x00080110, nested))
	reportNested := findings(t, objNested, iod.Options{IOD: "CT Image", CheckValues: true})
	if !hasFinding(reportNested, iod.AdditionalDefinedTerm, "(0008,0110)[1]/(0008,0112)") {
		t.Fatal("nested charset was not inherited")
	}
	// Large primitive values are explicitly manual, without constructing a
	// second enum error or copying their content into the report.
	obj := valueObject(valueElement(0x00100040, core.VRCS, core.RawValue(make([]byte, iod.MaxConstraintValueBytes+1))))
	report := findings(t, obj, iod.Options{IOD: "CT Image", CheckValues: true})
	if hasFinding(report, iod.InvalidEnumeratedValue, "(0010,0040)") || !hasFinding(report, iod.ManualValueConstraint, "(0010,0040)") {
		t.Fatal("oversized primitive was not bounded/manual")
	}
}
