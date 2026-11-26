package iod

import (
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/iodmodules"
	"github.com/ThalesMMS/dicom-go/object"
)

const (
	ctStorageUID       = "1.2.840.10008.5.1.4.1.1.2"
	enhancedCTUID      = "1.2.840.10008.5.1.4.1.1.2.1"
	legacyEnhancedCT   = "1.2.840.10008.5.1.4.1.1.2.2"
	mrStorageUID       = "1.2.840.10008.5.1.4.1.1.4"
	enhancedMRUID      = "1.2.840.10008.5.1.4.1.1.4.1"
	segStorageUID      = "1.2.840.10008.5.1.4.1.1.66.4"
	labelMapStorageUID = "1.2.840.10008.5.1.4.1.1.66.7"
)

func prioritySOP(uid string, elements ...core.Element) *object.Object {
	return dataset(append([]core.Element{str(0x0008, 0x0016, core.VRUI, uid)}, elements...)...)
}

func prioritySequence(group, tag uint16, items ...core.DataSet) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: core.NewTag(group, tag), VR: core.VRSQ}, Value: core.SequenceValue{Items: items}}
}

func priorityValidation(t *testing.T, obj *object.Object, opts Options, rule, path string, want ConditionState) Report {
	t.Helper()
	report, err := Validate(obj, opts)
	if err != nil {
		t.Fatal(err)
	}
	context, err := ValidateWithExternalContext(obj, opts, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range context.Conditions {
		if check.Rule != rule || check.Path != path {
			continue
		}
		wantMode := "local"
		if want == ConditionUnknown {
			wantMode = "pending"
		}
		if check.State != want || check.Mode != wantMode || check.Source.Source.Edition != StandardVersion ||
			check.Source.Source.SHA256 != iodmodules.SourceSHA256 {
			t.Fatalf("condition state/source: %+v; want state=%v mode=%s", check, want, wantMode)
		}
		return report
	}
	t.Fatalf("condition not found for %s %s", rule, path)
	return Report{}
}

func priorityFind(t *testing.T, report Report, path string, want Kind, additional ...Kind) {
	t.Helper()
	var got []Kind
	for _, finding := range report.Findings {
		if finding.Path == path {
			got = append(got, finding.Kind)
		}
	}
	if want == "" {
		if len(got) != 0 {
			t.Fatalf("%s unexpected findings %v", path, got)
		}
	} else if expected := append([]Kind{want}, additional...); !slices.Equal(got, expected) {
		t.Fatalf("%s findings %v, want %v", path, got, expected)
	}
}

func TestPriorityCTEnergyWeighting(t *testing.T) {
	weight := str(0x0018, 0x9353, core.VRFL, "0.5")
	additional := prioritySequence(0x0018, 0x9360, core.DataSet{})
	coded := func(code, scheme string) core.Element {
		return prioritySequence(0x0008, 0x9215, core.DataSet{Elements: []core.Element{
			str(0x0008, 0x0100, core.VRSH, code), str(0x0008, 0x0102, core.VRSH, scheme),
		}})
	}
	for _, tc := range []struct {
		name   string
		fields []core.Element
		state  ConditionState
		root   Kind
		nested Kind
	}{
		{"weighted-code-missing", []core.Element{additional, coded("113097", "DCM")}, ConditionTrue, MissingType1C, MissingType1C},
		{"weighted-code-present", []core.Element{weight, prioritySequence(0x0018, 0x9360, core.DataSet{Elements: []core.Element{weight}}), coded("113097", "DCM")}, ConditionTrue, "", ""},
		{"weighted-code-empty", []core.Element{str(0x0018, 0x9353, core.VRFL), additional, coded("113097", "DCM")}, ConditionTrue, EmptyType1C, MissingType1C},
		{"other-scheme", []core.Element{additional, coded("113097", "OTHER")}, ConditionFalse, "", ""},
		{"no-code-sequence", []core.Element{additional}, ConditionFalse, "", ""},
		{"unresolved-code", []core.Element{additional, coded("113097", "")}, ConditionUnknown, ManualCondition, ManualCondition},
		{"malformed-sequence", []core.Element{additional, str(0x0008, 0x9215, core.VRLO, "113097")}, ConditionUnknown, ManualCondition, ManualCondition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := prioritySOP(ctStorageUID, tc.fields...)
			report := priorityValidation(t, obj, Options{}, "table_C.8-3/row-53/index-52", "(0018,9353)", tc.state)
			priorityFind(t, report, "(0018,9353)", tc.root)
			report = priorityValidation(t, obj, Options{}, "table_C.8-3/row-62/index-61", "(0018,9360)[1]/(0018,9353)", tc.state)
			priorityFind(t, report, "(0018,9360)[1]/(0018,9353)", tc.nested)
		})
	}
}

func TestPriorityEnhancedCTAcquisition(t *testing.T) {
	image := func(value string) core.Element {
		return str(0x0008, 0x0008, core.VRCS, value, "PRIMARY", "AXIAL", "NONE")
	}
	for _, tc := range []struct {
		name, uid string
		imageType string
		present   bool
		state     ConditionState
		kind      Kind
	}{
		{"original-missing", enhancedCTUID, "ORIGINAL", false, ConditionTrue, MissingType1C},
		{"mixed-present", enhancedCTUID, "MIXED", true, ConditionTrue, ""},
		{"derived", enhancedCTUID, "DERIVED", false, ConditionFalse, ""},
		{"legacy-exemption", legacyEnhancedCT, "ORIGINAL", false, ConditionFalse, ""},
		{"unknown-type", enhancedCTUID, "", false, ConditionUnknown, ManualCondition},
		{"unknown-class", "", "ORIGINAL", false, ConditionUnknown, ManualCondition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fields []core.Element
			if tc.imageType != "" {
				fields = append(fields, image(tc.imageType))
			}
			if tc.present {
				fields = append(fields, str(0x0008, 0x002a, core.VRDT, "20260101120000"), str(0x0018, 0x9073, core.VRFD, "12"))
			}
			obj := prioritySOP(tc.uid, fields...)
			opts := Options{}
			if tc.uid == "" {
				opts.IOD = "Enhanced CT Image"
			}
			report := priorityValidation(t, obj, opts, "table_C.8-114/row-5/index-4", "(0008,002A)", tc.state)
			priorityFind(t, report, "(0008,002A)", tc.kind)
			report = priorityValidation(t, obj, opts, "table_C.8-114/row-6/index-5", "(0018,9073)", tc.state)
			priorityFind(t, report, "(0018,9073)", tc.kind)
		})
	}
}

func TestPriorityMRTimeConditions(t *testing.T) {
	scan := func(values ...string) core.Element { return str(0x0018, 0x0020, core.VRCS, values...) }
	variant := func(values ...string) core.Element { return str(0x0018, 0x0021, core.VRCS, values...) }
	options := func(values ...string) core.Element { return str(0x0018, 0x0022, core.VRCS, values...) }
	for _, profile := range []struct {
		name, rule     string
		tag            uint16
		trueConditions [][]core.Element
		falseFields    []core.Element
		unknownFields  []core.Element
	}{
		{"repetition", "table_C.8-4/row-11/index-10", 0x0080,
			[][]core.Element{{scan("EP"), variant("SK")}, {scan("SE")}}, []core.Element{scan("EP"), variant("NONE")}, []core.Element{scan("EP", "GR"), variant("NONE")}},
		{"inversion", "table_C.8-4/row-14/index-13", 0x0082,
			[][]core.Element{{scan("SE", "IR")}}, []core.Element{scan("SE")}, []core.Element{scan("OTHER")}},
		{"trigger", "table_C.8-4/row-15/index-14", 0x1060,
			[][]core.Element{{options("CG", "FC")}, {options("PPG")}}, []core.Element{options("RG", "FC")}, []core.Element{options("UNDEFINED")}},
	} {
		t.Run(profile.name, func(t *testing.T) {
			path := core.NewTag(0x0018, profile.tag).String()
			for _, fields := range profile.trueConditions {
				report := priorityValidation(t, prioritySOP(mrStorageUID, fields...), Options{}, profile.rule, path, ConditionTrue)
				priorityFind(t, report, path, MissingType2C)
				for _, value := range []core.Element{str(0x0018, profile.tag, core.VRDS, "12"), str(0x0018, profile.tag, core.VRDS)} {
					obj := prioritySOP(mrStorageUID, append(slices.Clone(fields), value)...)
					report = priorityValidation(t, obj, Options{}, profile.rule, path, ConditionTrue)
					priorityFind(t, report, path, "")
				}
			}
			report := priorityValidation(t, prioritySOP(mrStorageUID, profile.falseFields...), Options{}, profile.rule, path, ConditionFalse)
			priorityFind(t, report, path, "")
			for _, obj := range []*object.Object{prioritySOP(mrStorageUID), prioritySOP(mrStorageUID, profile.unknownFields...)} {
				report = priorityValidation(t, obj, Options{}, profile.rule, path, ConditionUnknown)
				priorityFind(t, report, path, ManualCondition)
			}
		})
	}
}

func TestPrioritySupplementalPaletteModule(t *testing.T) {
	module := "Supplemental Palette Color Lookup Table"
	firstDescriptor := core.NewTag(0x0028, 0x1101)
	for _, profile := range []struct{ uid, rule string }{
		{enhancedCTUID, "table_A.38-1/row-20/index-19"},
		{enhancedMRUID, "table_A.36-1/row-21/index-20"},
	} {
		for _, tc := range []struct {
			presentation string
			state        ConditionState
			reason       string
		}{
			{"COLOR", ConditionTrue, "condition true"}, {"MIXED", ConditionTrue, "condition true"},
			{"MONOCHROME", ConditionFalse, "condition false"}, {"UNKNOWN", ConditionUnknown, "condition unknown"},
			{"", ConditionUnknown, "condition unknown"},
		} {
			t.Run(profile.uid+"/"+tc.presentation, func(t *testing.T) {
				var fields []core.Element
				if tc.presentation != "" {
					fields = append(fields, str(0x0008, 0x9205, core.VRCS, tc.presentation))
				}
				report := priorityValidation(t, prioritySOP(profile.uid, fields...), Options{}, profile.rule, "module/"+module, tc.state)
				priorityModule(t, report, module, tc.state == ConditionTrue, tc.reason)
				if tc.state != ConditionTrue {
					priorityFind(t, report, firstDescriptor.String(), "")
					return
				}
				priorityFind(t, report, firstDescriptor.String(), MissingType1C, MissingType1)
				for _, tag := range []uint16{0x1101, 0x1102, 0x1103, 0x1201, 0x1202, 0x1203} {
					fields = append(fields, str(0x0028, tag, core.VRUS, "1"))
				}
				report = priorityValidation(t, prioritySOP(profile.uid, fields...), Options{}, profile.rule, "module/"+module, ConditionTrue)
				priorityFind(t, report, firstDescriptor.String(), "")
			})
		}
	}
}

func priorityModule(t *testing.T, report Report, name string, checked bool, reason string) {
	t.Helper()
	for _, module := range report.Modules {
		if module.Module == name {
			if module.Checked != checked || module.Reason != reason {
				t.Fatalf("module = %+v, want checked %v with reason %q", module, checked, reason)
			}
			return
		}
	}
	t.Fatalf("module %q missing", name)
}

func TestPrioritySEGDerivationModules(t *testing.T) {
	frames := str(0x0028, 0x0008, core.VRIS, "2")
	group := core.DataSet{Elements: []core.Element{prioritySequence(0x0008, 0x9124)}}
	emptyGroup := core.DataSet{}
	malformedGroup := core.DataSet{Elements: []core.Element{str(0x0008, 0x9124, core.VRLO, "not a sequence")}}
	shared := func(ds core.DataSet) core.Element { return prioritySequence(0x5200, 0x9229, ds) }
	perFrame := func(ds ...core.DataSet) core.Element { return prioritySequence(0x5200, 0x9230, ds...) }
	for _, tc := range []struct {
		name       string
		fields     []core.Element
		frameOfRef ConditionState
		common     ConditionState
	}{
		{"absent", []core.Element{shared(emptyGroup)}, ConditionTrue, ConditionFalse},
		{"shared", []core.Element{shared(group)}, ConditionFalse, ConditionTrue},
		{"per-frame", []core.Element{shared(emptyGroup), perFrame(group, group)}, ConditionFalse, ConditionTrue},
		{"partial", []core.Element{shared(emptyGroup), perFrame(group, emptyGroup)}, ConditionUnknown, ConditionTrue},
		{"no-shared", nil, ConditionUnknown, ConditionUnknown},
		{"truncated-per-frame", []core.Element{shared(emptyGroup), perFrame(group)}, ConditionUnknown, ConditionUnknown},
		{"malformed", []core.Element{shared(malformedGroup)}, ConditionUnknown, ConditionUnknown},
		{"in-both-placements", []core.Element{shared(group), perFrame(group, emptyGroup)}, ConditionUnknown, ConditionUnknown},
		{"root-sequence-is-not-a-group", []core.Element{shared(emptyGroup), prioritySequence(0x0008, 0x9124)}, ConditionTrue, ConditionFalse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, uid := range []string{segStorageUID, labelMapStorageUID} {
				fields := append([]core.Element{frames}, tc.fields...)
				obj := prioritySOP(uid, fields...)
				report := priorityValidation(t, obj, Options{}, segFrameOfReferenceRule, "module/Frame of Reference", tc.frameOfRef)
				reasons := map[ConditionState]string{ConditionTrue: "condition true", ConditionFalse: "condition false", ConditionUnknown: "condition unknown"}
				priorityModule(t, report, "Frame of Reference", tc.frameOfRef == ConditionTrue, reasons[tc.frameOfRef])
				if tc.frameOfRef == ConditionTrue {
					priorityFind(t, report, "(0020,0052)", MissingType1)
					obj = prioritySOP(uid, append(fields, str(0x0020, 0x0052, core.VRUI, "1.2.3"))...)
					r := priorityValidation(t, obj, Options{}, segFrameOfReferenceRule, "module/Frame of Reference", ConditionTrue)
					priorityFind(t, r, "(0020,0052)", "")
				}
				report = priorityValidation(t, obj, Options{}, "table_A.51-1/row-23/index-22", "module/Common Instance Reference", tc.common)
				priorityModule(t, report, "Common Instance Reference", tc.common == ConditionTrue, reasons[tc.common])
			}
		})
	}
}

func TestPrioritySEGAttributeConditions(t *testing.T) {
	segType := func(value string) core.Element { return str(0x0062, 0x0001, core.VRCS, value) }
	frameTypes := []struct {
		value string
		state ConditionState
		kind  Kind
	}{{"FRACTIONAL", ConditionTrue, MissingType1C}, {"BINARY", ConditionFalse, ""}, {"UNKNOWN", ConditionUnknown, ManualCondition}}
	for _, tag := range []struct {
		rule    string
		element core.Element
	}{
		{"table_C.8.20-2/row-13/index-12", str(0x0062, 0x0010, core.VRCS, "PROBABILITY")},
		{"table_C.8.20-2/row-14/index-13", str(0x0062, 0x000e, core.VRUS, "255")},
	} {
		path := tag.element.Tag().String()
		for _, tc := range frameTypes {
			report := priorityValidation(t, prioritySOP(segStorageUID, segType(tc.value)), Options{}, tag.rule, path, tc.state)
			priorityFind(t, report, path, tc.kind)
			if tc.state == ConditionTrue {
				report := priorityValidation(t, prioritySOP(segStorageUID, segType(tc.value), tag.element), Options{}, tag.rule, path, ConditionTrue)
				priorityFind(t, report, path, "")
			}
		}
	}
	algorithmType := func(value string) core.Element { return str(0x0062, 0x0008, core.VRCS, value) }
	segments := prioritySequence(0x0062, 0x0002, core.DataSet{Elements: []core.Element{algorithmType("AUTOMATIC")}},
		core.DataSet{Elements: []core.Element{algorithmType("MANUAL")}}, core.DataSet{},
		core.DataSet{Elements: []core.Element{algorithmType("SEMIAUTOMATIC"), str(0x0062, 0x0009, core.VRLO, "SYNTHETIC")}})
	obj := prioritySOP(segStorageUID, segments, algorithmType("AUTOMATIC"))
	for _, tc := range []struct {
		path  string
		state ConditionState
		kind  Kind
	}{
		{"(0062,0002)[1]/(0062,0009)", ConditionTrue, MissingType1C}, {"(0062,0002)[2]/(0062,0009)", ConditionFalse, ""},
		{"(0062,0002)[3]/(0062,0009)", ConditionUnknown, ManualCondition}, {"(0062,0002)[4]/(0062,0009)", ConditionTrue, ""},
	} {
		report := priorityValidation(t, obj, Options{}, "table_C.8.20-2/row-18/index-17", tc.path, tc.state)
		priorityFind(t, report, tc.path, tc.kind)
	}
}

func TestPriorityConditionInventory(t *testing.T) {
	found := map[string]bool{}
	attributes, modules := 0, 0
	for _, table := range iodmodules.RuleTables() {
		rows, _ := iodmodules.Rows(table)
		for index, row := range rows {
			source, _ := iodmodules.RowSource(table, index)
			if priorityConditions[source.Rule] == nil {
				continue
			}
			if row.Type != "1C" && row.Type != "2C" || row.Condition == "" {
				t.Fatalf("bound nonconditional row %+v", source)
			}
			found[source.Rule] = true
			attributes++
			t.Logf("%s %s %s %s %s", row.Type, table, source.Rule, source.Source.Section, row.Condition)
		}
	}
	for _, iod := range iodmodules.IODs() {
		uses, _ := iodmodules.Modules(iod)
		for index, use := range uses {
			source, _ := iodmodules.ModuleSource(iod, index)
			if priorityConditions[source.Rule] == nil {
				continue
			}
			if use.Usage != "C" || use.Condition == "" {
				t.Fatalf("bound unconditional module %+v", source)
			}
			found[source.Rule] = true
			modules++
			t.Logf("C %s %s %s %s", iod, use.Module, source.Rule, use.Condition)
		}
	}
	if len(found) != len(priorityConditions) || attributes != 10 || modules != 4 {
		t.Fatalf("audited binding inventory: %d found of %d, attributes=%d modules=%d", len(found), len(priorityConditions), attributes, modules)
	}
}

func TestPriorityRulesLeaveOtherIODEvaluationsUnchanged(t *testing.T) {
	for _, name := range iodmodules.IODs() {
		uses, _ := iodmodules.Modules(name)
		for index, use := range uses {
			source, _ := iodmodules.ModuleSource(name, index)
			if priorityConditions[source.Rule] != nil {
				continue
			}
			checkGenericConditionPreserved(t, use.Condition, source)
		}
	}
	for _, table := range iodmodules.RuleTables() {
		rows, _ := iodmodules.Rows(table)
		for index, row := range rows {
			source, _ := iodmodules.RowSource(table, index)
			if priorityConditions[source.Rule] != nil {
				continue
			}
			checkGenericConditionPreserved(t, row.Condition, source)
		}
	}
}

func checkGenericConditionPreserved(t *testing.T, text string, source iodmodules.RuleSource) {
	t.Helper()
	for _, obj := range []*object.Object{
		dataset(),
		dataset(str(0x0008, 0x0016, core.VRUI, ctStorageUID), str(0x0008, 0x0008, core.VRCS, "ORIGINAL", "PRIMARY"),
			str(0x0028, 0x0004, core.VRCS, "MONOCHROME2"), str(0x0028, 0x0008, core.VRIS, "2")),
	} {
		want, forbidden := ConditionUnknown, false
		if grammar, known := parseCondition(text); known {
			want, forbidden = conditionState(grammar.eval(obj, obj)), grammar.forbidden
		}
		got, gotForbidden := datasetCondition(text, source, obj, obj)
		if got != want || forbidden != gotForbidden {
			t.Fatalf("other rule changed %s", source.Rule)
		}
	}
}

func TestPrioritySEGUnknownAndBoundedConditions(t *testing.T) {
	for _, frames := range []string{"", "0", "-1", "4097", "invalid"} {
		fields := []core.Element{str(0x0028, 0x0008, core.VRIS, frames), prioritySequence(0x5200, 0x9229, core.DataSet{})}
		obj := prioritySOP(segStorageUID, fields...)
		priorityValidation(t, obj, Options{}, segFrameOfReferenceRule, "module/Frame of Reference", ConditionUnknown)
	}
	root := prioritySOP(segStorageUID, str(0x0062, 0x0001, core.VRCS, "FRACTIONAL"))
	root.Put(str(0x0062, 0x0010, core.VRCS))
	report := priorityValidation(t, root, Options{}, "table_C.8.20-2/row-13/index-12", "(0062,0010)", ConditionTrue)
	priorityFind(t, report, "(0062,0010)", EmptyType1C)
	root.Put(str(0x0062, 0x0001, core.VRCS, "UNRESOLVED"))
	report = priorityValidation(t, root, Options{}, "table_C.8.20-2/row-13/index-12", "(0062,0010)", ConditionUnknown)
	priorityFind(t, report, "(0062,0010)", ManualCondition)

	// A known requirement still allows the specified optional Frame of
	// Reference module to be checked when a shared derivation group is used.
	group := core.DataSet{Elements: []core.Element{prioritySequence(0x0008, 0x9124)}}
	root = prioritySOP(segStorageUID, str(0x0028, 0x0008, core.VRIS, "1"),
		prioritySequence(0x5200, 0x9229, group), str(0x0020, 0x0052, core.VRUI, "1.2.3"))
	report = priorityValidation(t, root, Options{}, segFrameOfReferenceRule, "module/Frame of Reference", ConditionFalse)
	priorityModule(t, report, "Frame of Reference", true, "present ((0020,0052))")
}
