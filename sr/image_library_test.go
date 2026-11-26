package sr

import (
	"bytes"
	"context"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

func ilCode(value, scheme string) CodedEntry { return CodedEntry{value, scheme, value} }
func ilModality(value string) ContentItem {
	return ContentItem{ValueType: ValueCode, RelationshipType: RelationshipHasAcqContext, ConceptName: ilCode("121139", "DCM"), Code: ilCode(value, "DCM")}
}
func ilNum(concept, scheme, unit string, value float64) ContentItem {
	return ContentItem{ValueType: ValueNum, RelationshipType: RelationshipHasAcqContext, ConceptName: ilCode(concept, scheme), Measurement: &Measurement{value, ilCode(unit, "UCUM")}}
}
func ilText(concept, value string) ContentItem {
	return ContentItem{ValueType: ValueText, RelationshipType: RelationshipHasAcqContext, ConceptName: ilCode(concept, "DCM"), Text: value}
}
func ilEntry(id string, descriptors ...ContentItem) ImageLibraryEntry {
	return ImageLibraryEntry{Image: ImageReference{StudyInstanceUID: "1.2.3.4", SeriesInstanceUID: "1.2.3.5", SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: id, Frames: []int{1}}, Descriptors: descriptors}
}
func ilReport(groups ...ImageLibraryGroup) *MeasurementReport {
	return &MeasurementReport{SOPClassUID: Comprehensive3DSRStorage, SOPInstanceUID: "1.2.3.41", StudyInstanceUID: "1.2.3.4", SeriesInstanceUID: "1.2.3.42", ContentDate: "20261010", ContentTime: "120000", ImageLibraryGroups: groups}
}
func ilRoundTrip(t *testing.T, r *MeasurementReport) *MeasurementReport {
	t.Helper()
	file, err := WriteMeasurementReport(r)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err = object.WriteFile(&b, file); err != nil {
		t.Fatal(err)
	}
	f, err := object.ReadFile(bytes.NewReader(b.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	result, err := ReadMeasurementReport(f.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// Values, concept identities and units below are independently transcribed
// from PS3.16 2026d TID1602–1607, not computed from production catalog rows.
func TestImageLibraryModalitiesRoundTrip(t *testing.T) {
	view := ContentItem{ValueType: ValueCode, RelationshipType: RelationshipHasAcqContext, ConceptName: ilCode("111031", "DCM"), Code: ilCode("R-10242", "SRT"), Children: []ContentItem{{ValueType: ValueCode, RelationshipType: RelationshipHasConceptMod, ConceptName: ilCode("111032", "DCM"), Code: ilCode("R-102D2", "SRT")}}}
	groups := []ImageLibraryGroup{
		{Entries: []ImageLibraryEntry{ilEntry("1.2.3.101", ilModality("DX"), view, ilNum("111026", "DCM", "mm", .3), ilText("111044", "L"))}},
		{Descriptors: []ContentItem{ilModality("CT")}, Entries: []ImageLibraryEntry{ilEntry("1.2.3.102", ilNum("112225", "DCM", "mm", 1.5), ContentItem{ValueType: ValueCode, RelationshipType: RelationshipHasAcqContext, ConceptName: ilCode("113820", "DCM"), Code: ilCode("116152004", "SCT")})}},
		{Entries: []ImageLibraryEntry{ilEntry("1.2.3.103", ilModality("MR"), ilText("128230", "SyntheticPulse"), ilNum("130542", "DCM", "T", 3), ilNum("113240", "DCM", "s/mm2", 900), ilNum("113240", "DCM", "s/mm2", 1000))}},
		{Descriptors: []ContentItem{ilModality("PT")}, Entries: []ImageLibraryEntry{ilEntry("1.2.3.104", ilNum("304283002", "SCT", "s", 6586), ilNum("123006", "DCM", "Bq", 12345), ilNum("14749-6", "LN", "mmol/l", 4.5), ContentItem{ValueType: ValueDate, RelationshipType: RelationshipHasAcqContext, ConceptName: ilCode("127857", "DCM"), Date: "20261010"}, ContentItem{ValueType: ValueTime, RelationshipType: RelationshipHasAcqContext, ConceptName: ilCode("127858", "DCM"), Time: "120000"})}},
	}
	r := ilRoundTrip(t, ilReport(groups...))
	if len(r.ImageLibraryGroups) != 4 || len(r.ImageLibrary) != 4 {
		t.Fatalf("groups/legacy: %d %v", len(r.ImageLibraryGroups), r.ImageLibrary)
	}
	for i, g := range groups {
		got := r.ImageLibraryGroups[i]
		if !reflect.DeepEqual(g.Descriptors, got.Descriptors) {
			t.Fatalf("group %d context differs", i)
		}
		if !reflect.DeepEqual(g.Entries[0].Descriptors, got.Entries[0].Descriptors) {
			t.Fatalf("group %d entry descriptors differ", i)
		}
		if !reflect.DeepEqual(g.Entries[0].Image, got.Entries[0].Image) {
			t.Fatalf("group %d references differ", i)
		}
	}
	// The specialized re-emission uses the preserved subtree snapshot.
	second := ilRoundTrip(t, r)
	if len(second.ImageLibraryGroups) != 4 {
		t.Fatal("re-emission dropped groups")
	}
}
func TestImageLibraryLegacyAndAmbiguity(t *testing.T) {
	legacy := ilReport()
	legacy.ImageLibraryGroups = nil
	legacy.ImageLibrary = []ImageReference{ilEntry("1.2.3.106").Image}
	got := ilRoundTrip(t, legacy)
	if len(got.ImageLibrary) != 1 || got.ImageLibraryGroups != nil {
		t.Fatal("legacy field changed")
	}
	legacy.ImageLibraryGroups = []ImageLibraryGroup{{Entries: []ImageLibraryEntry{ilEntry("1.2.3.107", ilModality("CT"))}}}
	if _, err := WriteMeasurementReport(legacy); !errors.Is(err, ErrImageLibrary) {
		t.Fatal("ambiguous old/new accepted", err)
	}
}
func TestImageLibraryValidationCausal(t *testing.T) {
	tests := []struct {
		name  string
		descs []ContentItem
		code  string
	}{
		{"wrong-units", []ContentItem{ilModality("CT"), ilNum("112225", "DCM", "cm", 1)}, "units"},
		{"modality", []ContentItem{ilModality("MR"), {ValueType: ValueCode, RelationshipType: RelationshipHasAcqContext, ConceptName: ilCode("113820", "DCM"), Code: ilCode("113804", "DCM")}}, "modality-applicability"},
		{"singleton", []ContentItem{ilModality("CT"), ilNum("112225", "DCM", "mm", 1), ilNum("112225", "DCM", "mm", 2)}, "duplicate-singleton"},
		{"wrong-type", []ContentItem{ilModality("CT"), ilText("112225", "secret-value")}, "value-type"},
		{"nan", []ContentItem{ilModality("CT"), ilNum("112225", "DCM", "mm", math.NaN())}, "numeric-value"},
		{"glucose-needs-date-time", []ContentItem{ilModality("PT"), ilNum("14749-6", "LN", "mmol/l", 4)}, "pet-glucose-date-time"},
	}
	for _, c := range tests {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateImageLibrary(context.Background(), nil, []ImageLibraryGroup{{Entries: []ImageLibraryEntry{ilEntry("1.2.3.108", c.descs...)}}}, ImageLibraryOptions{})
			var typed *ImageLibraryError
			if !errors.As(err, &typed) || typed.Code != c.code {
				t.Fatalf("got %v want %s", err, c.code)
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatal("descriptor leaked")
			}
		})
	}
	// Shared spacing concepts are valid for both projection and CT templates.
	for _, modality := range []string{"CT", "MR", "PT", "DX"} {
		g := ImageLibraryGroup{Entries: []ImageLibraryEntry{ilEntry("1.2.3.109", ilModality(modality), ilNum("111026", "DCM", "mm", .2))}}
		if err := ValidateImageLibrary(context.Background(), nil, []ImageLibraryGroup{g}, ImageLibraryOptions{}); err != nil {
			t.Fatal(modality, err)
		}
	}
}
func TestImageLibraryMetadataOrderUnknownAndAuthorizedEdit(t *testing.T) {
	unknown := ilNum("EXTENSION", "99SYNTH", "mm", 1)
	unknown.Children = []ContentItem{ilText("EXT-NESTED", "synthetic nested")}
	unknown.Children[0].ConceptName.CodingSchemeDesignator = "99SYNTH"
	groups := []ImageLibraryGroup{{Order: []int{1, -1}, Descriptors: []ContentItem{ilModality("CT")}, Entries: []ImageLibraryEntry{ilEntry("1.2.3.111", ilNum("112225", "DCM", "mm", .123), unknown)}}}
	metadata := TemplateValidationMetadata{CodeContexts: []ContentItemCodeContexts{{Path: "1.1.1.2", ConceptName: CodeContext{CodingSchemeVersion: "synthetic-v1"}}}, TemplateIdentifications: []TemplateIdentification{{Path: "1.1", MappingResource: "DCMR", Identifier: "1601"}, {Path: "1", MappingResource: "DCMR", Identifier: "1600"}}}
	numeric := NumericRepresentationMetadata{Entries: []NumericRepresentationEntry{{Path: "1.1.1.1", Representation: NumericRepresentation{DecimalString: ".12300", HasDecimalString: true, FloatingPoint: .123, HasFloatingPoint: true}}, {Path: "1.1.1.2", Representation: NumericRepresentation{DecimalString: "1 ", HasDecimalString: true}}}}
	binding, err := BindImageLibraryMetadata(nil, groups, metadata, numeric, ImageLibraryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	report := ilReport(groups...)
	report.ImageLibraryMetadata = binding
	got := ilRoundTrip(t, report)
	if !reflect.DeepEqual(got.ImageLibraryGroups[0].Order, []int{1, -1}) {
		t.Fatal("interleaving changed")
	}
	if got.ImageLibraryGroups[0].Entries[0].Descriptors[1].Measurement.Value != unknown.Measurement.Value || got.ImageLibraryGroups[0].Entries[0].Descriptors[1].Children[0].Text != unknown.Children[0].Text {
		t.Fatal("unknown lost")
	}
	if !reflect.DeepEqual(got.ImageLibraryMetadata.Metadata(), metadata) || !reflect.DeepEqual(got.ImageLibraryMetadata.NumericMetadata(), numeric) {
		t.Fatalf("sidecars changed: metadata got=%#v want=%#v; numeric got=%#v want=%#v", got.ImageLibraryMetadata.Metadata(), metadata, got.ImageLibraryMetadata.NumericMetadata(), numeric)
	}
	got.ImageLibraryGroups[0].Entries[0].Descriptors[1].Children[0].Text = "authorized changed text"
	if _, err := WriteMeasurementReport(got); err == nil {
		t.Fatal("stale association accepted")
	}
	got.ImageLibraryMetadata, err = BindImageLibraryMetadataWithOrder(got.ImageLibraryDescriptors, got.ImageLibraryGroups, got.ImageLibraryOrder, got.ImageLibraryMetadata.Metadata(), got.ImageLibraryMetadata.NumericMetadata(), ImageLibraryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ilRoundTrip(t, got).ImageLibraryGroups[0].Entries[0].Descriptors[1].Children[0].Text != "authorized changed text" {
		t.Fatal("rebind failed")
	}
}
func TestImageLibraryBoundsAndPromotion(t *testing.T) {
	common := ilNum("112225", "DCM", "mm", 1)
	report := ilReport(ImageLibraryGroup{Entries: []ImageLibraryEntry{ilEntry("1.2.3.112", ilModality("CT"), common), ilEntry("1.2.3.113", ilModality("CT"), common)}})
	groups, err := PromoteImageLibraryCommonDescriptors(context.Background(), report, ImageLibraryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups[0].Descriptors) != 2 || len(groups[0].Entries[0].Descriptors) != 0 || len(report.ImageLibraryGroups[0].Entries[0].Descriptors) != 2 {
		t.Fatal("promotion not detached")
	}
	report.ImageLibraryGroups[0].Entries[1].Descriptors[1].Measurement = &Measurement{2, ilCode("mm", "UCUM")}
	groups, err = PromoteImageLibraryCommonDescriptors(context.Background(), report, ImageLibraryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups[0].Descriptors) != 1 || len(groups[0].Entries[0].Descriptors) != 1 {
		t.Fatal("different values promoted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ValidateImageLibrary(ctx, nil, report.ImageLibraryGroups, ImageLibraryOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
	if err := ValidateImageLibrary(context.Background(), nil, report.ImageLibraryGroups, ImageLibraryOptions{MaxItems: 1}); !errors.Is(err, ErrResourceLimitExceeded) {
		t.Fatal("item limit ignored", err)
	}
	report.ImageLibraryGroups[0].Entries[0].Descriptors[0] = ilText("PRIVATE", "long-value")
	if err := ValidateImageLibrary(context.Background(), nil, report.ImageLibraryGroups, ImageLibraryOptions{MaxValueBytes: 1}); !errors.Is(err, ErrResourceLimitExceeded) {
		t.Fatal("byte limit ignored", err)
	}
}
func TestImageLibraryPETObservationCondition(t *testing.T) {
	glucose := ilNum("14749-6", "LN", "mmol/l", 4)
	glucose.ValueElements = []core.Element{strElem(core.NewTag(0x0040, 0xa032), core.VRDT, "20261010120000")}
	g := ImageLibraryGroup{Entries: []ImageLibraryEntry{ilEntry("1.2.3.114", ilModality("PT"), glucose)}}
	if err := ValidateImageLibrary(context.Background(), nil, []ImageLibraryGroup{g}, ImageLibraryOptions{}); err != nil {
		t.Fatal(err)
	}
	glucose.ValueElements[0] = strElem(core.NewTag(0x0040, 0xa032), core.VRDT, "invalid")
	if err := ValidateImageLibrary(context.Background(), nil, []ImageLibraryGroup{g}, ImageLibraryOptions{}); err == nil {
		t.Fatal("invalid observation suppressed conditional rows")
	}
}

func TestImageLibraryCommonOverrideAndRootSummary(t *testing.T) {
	group := ImageLibraryGroup{Descriptors: []ContentItem{ilModality("CT"), ilNum("111026", "DCM", "mm", 1)}, Entries: []ImageLibraryEntry{ilEntry("1.2.3.151", ilModality("MR"), ilNum("111026", "DCM", "mm", 2), ilNum("130542", "DCM", "T", 3))}}
	report := ilReport(group)
	report.ImageLibraryDescriptors = []ContentItem{ilModality("CT"), ilModality("MR"), {ValueType: ValueCode, RelationshipType: RelationshipHasAcqContext, ConceptName: ilCode("123014", "DCM"), Code: ilCode("123037004", "SCT")}, ilText("123014", "Synthetic region"), ilNum("131565", "DCM", "{series}", 2)}
	if err := ValidateImageLibrary(context.Background(), report.ImageLibraryDescriptors, report.ImageLibraryGroups, ImageLibraryOptions{}); err != nil {
		t.Fatal(err)
	}
	effective, err := effectiveLibraryDescriptors(group.Descriptors, group.Entries[0].Descriptors)
	if err != nil {
		t.Fatal(err)
	}
	if libraryModality(effective) != "MR" {
		t.Fatal("common modality did not yield to entry")
	}
	for _, d := range effective {
		if d.ConceptName.CodeValue == "111026" && d.Measurement.Value != 2 {
			t.Fatal("common spacing did not yield to entry")
		}
	}
	round := ilRoundTrip(t, report)
	if len(round.ImageLibraryDescriptors) != 5 || round.ImageLibraryGroups[0].Descriptors[1].Measurement.Value != 1 || round.ImageLibraryGroups[0].Entries[0].Descriptors[1].Measurement.Value != 2 {
		t.Fatal("override lost explicit facts")
	}
	// Root summaries are not inherited as acquisition context. With no group/
	// entry modality, an MR-only attribute cannot borrow the root summary MR.
	invalid := ilReport(ImageLibraryGroup{Entries: []ImageLibraryEntry{ilEntry("1.2.3.152", ilNum("130542", "DCM", "T", 3))}})
	invalid.ImageLibraryDescriptors = []ContentItem{ilModality("MR")}
	if err := ValidateImageLibrary(context.Background(), invalid.ImageLibraryDescriptors, invalid.ImageLibraryGroups, ImageLibraryOptions{}); err == nil {
		t.Fatal("root modality summary incorrectly inherited")
	}
}
func TestImageLibraryInstanceGroupUniqueness(t *testing.T) {
	entry := ilEntry("1.2.3.153", ilModality("CT"))
	groups := []ImageLibraryGroup{{Entries: []ImageLibraryEntry{entry}}, {Entries: []ImageLibraryEntry{entry}}}
	err := ValidateImageLibrary(context.Background(), nil, groups, ImageLibraryOptions{})
	var typed *ImageLibraryError
	if !errors.As(err, &typed) || typed.Code != "instance-in-multiple-groups" || typed.Path != "1.2.1" {
		t.Fatal("group uniqueness", err)
	}
	entry2 := entry
	entry2.Image.Frames = []int{2}
	if err := ValidateImageLibrary(context.Background(), nil, []ImageLibraryGroup{{Entries: []ImageLibraryEntry{entry, entry2}}}, ImageLibraryOptions{}); err != nil {
		t.Fatal("same group frame references refused", err)
	}
}
func TestImageLibraryPETConditionRequiresExactIdentity(t *testing.T) {
	date := ilText("127857", "synthetic-date")
	date.ConceptName.CodingSchemeDesignator = "99PRIVATE"
	time := ilText("127858", "synthetic-time")
	time.ConceptName.CodingSchemeDesignator = "99PRIVATE"
	g := ImageLibraryGroup{Entries: []ImageLibraryEntry{ilEntry("1.2.3.154", ilModality("PT"), ilNum("14749-6", "LN", "mmol/l", 4), date, time)}}
	err := ValidateImageLibrary(context.Background(), nil, []ImageLibraryGroup{g}, ImageLibraryOptions{})
	var typed *ImageLibraryError
	if !errors.As(err, &typed) || typed.Code != "pet-glucose-date-time" {
		t.Fatal("private extensions satisfied DCM condition", err)
	}
}
func TestImageLibraryPromotionDeepDetach(t *testing.T) {
	descriptor := ilNum("112225", "DCM", "mm", 1)
	descriptor.Children = []ContentItem{ilText("NESTED", "synthetic")}
	descriptor.ValueElements = []core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x7777, 0x1001), VR: core.VROB}, Value: core.RawValue{1, 2}}}
	a := ilEntry("1.2.3.155", ilModality("CT"), descriptor)
	a.Header.ValueElements = []core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x7777, 0x1002), VR: core.VRLO}, Value: core.StringValue{"header"}}}
	b := ilEntry("1.2.3.156", ilModality("CT"), descriptor)
	report := ilReport(ImageLibraryGroup{Entries: []ImageLibraryEntry{a, b}})
	groups, err := PromoteImageLibraryCommonDescriptors(context.Background(), report, ImageLibraryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	promoted := &groups[0].Descriptors[1]
	promoted.Measurement.Value = 99
	promoted.Children[0].Text = "edited"
	promoted.ValueElements[0].Value.(core.RawValue)[0] = 9
	groups[0].Entries[0].Header.ValueElements[0].Value.(core.StringValue)[0] = "edited-header"
	groups[0].Entries[0].Image.Frames[0] = 2
	original := report.ImageLibraryGroups[0].Entries[0]
	if original.Descriptors[1].Measurement.Value != 1 || original.Descriptors[1].Children[0].Text != "synthetic" || original.Descriptors[1].ValueElements[0].Value.(core.RawValue)[0] != 1 || original.Header.ValueElements[0].Value.(core.StringValue)[0] != "header" || original.Image.Frames[0] != 1 {
		t.Fatal("promotion result aliases source")
	}
}

func TestImageLibraryPromotionIgnoredHeaderTopologyBounds(t *testing.T) {
	for _, kind := range []string{"depth", "cycle", "items", "sequence"} {
		t.Run(kind, func(t *testing.T) {
			report := ilReport(ImageLibraryGroup{Entries: []ImageLibraryEntry{ilEntry("1.2.3.157", ilModality("CT"))}})
			opts := ImageLibraryOptions{MaxItems: 16, MaxDepth: 8}
			header := &report.ImageLibraryGroups[0].Header
			switch kind {
			case "depth":
				cursor := header
				for i := 0; i < 10; i++ {
					cursor.Children = []ContentItem{{}}
					cursor = &cursor.Children[0]
				}
			case "cycle":
				children := make([]ContentItem, 1)
				children[0].Children = children
				header.Children = children
			case "items":
				header.Children = make([]ContentItem, 17)
			case "sequence":
				elements := make([]core.Element, 1)
				elements[0] = core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x7777, 0x2010), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: elements}}}}
				header.Children = []ContentItem{{ValueElements: elements}}
			}
			if _, err := PromoteImageLibraryCommonDescriptors(context.Background(), report, opts); !errors.Is(err, ErrResourceLimitExceeded) {
				t.Fatal("ignored retained topology was not bounded before hash/clone", err)
			}
		})
	}
}

func TestImageLibraryRichReadLegacyProjectionCompatibility(t *testing.T) {
	source := ilReport(ImageLibraryGroup{Descriptors: []ContentItem{ilModality("CT")}, Entries: []ImageLibraryEntry{ilEntry("1.2.3.158", ilNum("112225", "DCM", "mm", 2)), ilEntry("1.2.3.159", ilNum("112225", "DCM", "mm", 3))}})
	got := ilRoundTrip(t, source)
	if len(got.ImageLibrary) != 2 || len(got.ImageLibraryGroups) != 1 || !reflect.DeepEqual(got.ImageLibrary[0], got.ImageLibraryGroups[0].Entries[0].Image) {
		t.Fatal("rich read lost legacy projection")
	}
	again := ilRoundTrip(t, got)
	if !reflect.DeepEqual(got.ImageLibrary, again.ImageLibrary) || !reflect.DeepEqual(got.ImageLibraryGroups, again.ImageLibraryGroups) || !reflect.DeepEqual(got.ImageLibraryMetadata.Metadata(), again.ImageLibraryMetadata.Metadata()) {
		t.Fatal("dual read projection emitted duplicate/lost structured library")
	}
	got.ImageLibrary[0].Frames[0] = 2
	if got.ImageLibraryGroups[0].Entries[0].Image.Frames[0] != 1 {
		t.Fatal("flat projection aliases structured frames")
	}
	if _, err := WriteMeasurementReport(got); err == nil {
		t.Fatal("flat-only divergent edit accepted")
	}
	got = ilRoundTrip(t, source)
	got.ImageLibraryGroups[0].Entries[0].Image.SOPInstanceUID = "1.2.3.160"
	if _, err := WriteMeasurementReport(got); err == nil {
		t.Fatal("group-only divergent reference accepted")
	}
	got = ilRoundTrip(t, source)
	got.ImageLibraryGroups[0].Order = []int{2, 1, -1}
	if _, err := WriteMeasurementReport(got); err == nil {
		t.Fatal("group-only reordered projection accepted")
	}
	manual := ilReport(source.ImageLibraryGroups...)
	manual.ImageLibrary = cloneLibrarySlice(again.ImageLibrary)
	if _, err := WriteMeasurementReport(manual); err == nil {
		t.Fatal("manual equivalent dual authoring accepted")
	}
}

// TID1600 is non-root; its first row must see the actual library CONTAINER.
func TestImageLibraryTemplateScopeAndReadProjection(t *testing.T) {
	r := ilReport(ImageLibraryGroup{Descriptors: []ContentItem{ilModality("CT")}, Entries: []ImageLibraryEntry{ilEntry("1.2.3.181", ilNum("112225", "DCM", "mm", 2))}})
	metadata := TemplateValidationMetadata{TemplateIdentifications: []TemplateIdentification{{Path: "1", MappingResource: "DCMR", Identifier: "1600"}}}
	var err error
	r.ImageLibraryMetadata, err = BindImageLibraryMetadata(r.ImageLibraryDescriptors, r.ImageLibraryGroups, metadata, NumericRepresentationMetadata{}, ImageLibraryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before := r.ImageLibraryMetadata.Metadata()
	check := func(report *MeasurementReport) {
		t.Helper()
		got, err := ValidateImageLibraryTemplate(context.Background(), report, ImageLibraryOptions{}, TemplateValidationOptions{Mode: ValidationModeWarn})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range got.Findings {
			if f.Code == TemplateCodeUnknownContent || (f.Code == TemplateCodeCardinality && f.RuleID == "TID1600:1") || f.Code == TemplateCodeTemplateIdentification {
				t.Fatalf("known library scope or metadata misassociated: %+v", f)
			}
		}
	}
	check(r)
	if !reflect.DeepEqual(before, r.ImageLibraryMetadata.Metadata()) {
		t.Fatal("template wrapper mutated binding")
	}
	rich := ilRoundTrip(t, r)
	if len(rich.ImageLibrary) != 1 || !rich.imageLibraryReadProjection {
		t.Fatal("fixture is not a rich reader projection")
	}
	check(rich)
	rich.ImageLibrary[0].Frames = []int{2}
	if _, err := ValidateImageLibraryTemplate(context.Background(), rich, ImageLibraryOptions{}, TemplateValidationOptions{}); !errors.Is(err, ErrImageLibrary) {
		t.Fatal("divergent projection accepted", err)
	}
	manual := ilReport(r.ImageLibraryGroups...)
	manual.ImageLibrary = []ImageReference{manual.ImageLibraryGroups[0].Entries[0].Image}
	if _, err := ValidateImageLibraryTemplate(context.Background(), manual, ImageLibraryOptions{}, TemplateValidationOptions{}); !errors.Is(err, ErrImageLibrary) {
		t.Fatal("manual dual authoring accepted", err)
	}
	file, err := object.OpenFile(filepath.Join("..", "testdata", "upstream", "dcmqi", "tid1500.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	dcmqi, err := ReadMeasurementReport(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if len(dcmqi.ImageLibrary) != 3 {
		t.Fatal("legacy dcmqi references changed")
	}
	if _, err := ValidateImageLibraryTemplate(context.Background(), dcmqi, ImageLibraryOptions{}, TemplateValidationOptions{Mode: ValidationModeWarn}); err != nil {
		t.Fatal("reader projection refused", err)
	}
}

func TestImageLibraryTemplateDescriptorMetadataPath(t *testing.T) {
	r := ilReport(ImageLibraryGroup{Entries: []ImageLibraryEntry{ilEntry("1.2.3.182", ilModality("CT"), ContentItem{ValueType: ValueCode, RelationshipType: RelationshipHasAcqContext, ConceptName: ilCode("111027", "DCM"), Code: ilCode("51440002", "SCT")})}})
	// The NUM/unit domain gate is still enforced before template qualification.
	badUnits := ilReport(ImageLibraryGroup{Entries: []ImageLibraryEntry{ilEntry("1.2.3.183", ilModality("CT"), ilNum("112225", "DCM", "cm", 2))}})
	if _, err := ValidateImageLibraryTemplate(context.Background(), badUnits, ImageLibraryOptions{}, TemplateValidationOptions{}); !errors.Is(err, ErrImageLibrary) {
		t.Fatal("wrong units accepted", err)
	}
	// Syntactically valid encoded metadata declares a different CID for the
	// known Laterality row, whose CID244 is non-extensible and installed in
	// the pinned registry. CID33 Modality remains an unresolved catalog seam.
	// The template must reach TID1602:3 at the relocated descriptor path.
	metadata := TemplateValidationMetadata{CodeContexts: []ContentItemCodeContexts{{Path: "1.1.1.2", Value: CodeContext{ContextIdentifier: "244", MappingResource: "DCMR", ContextGroupVersion: "20030108"}}}}
	var err error
	r.ImageLibraryMetadata, err = BindImageLibraryMetadata(r.ImageLibraryDescriptors, r.ImageLibraryGroups, metadata, NumericRepresentationMetadata{}, ImageLibraryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	valid, err := ValidateImageLibraryTemplate(context.Background(), r, ImageLibraryOptions{}, TemplateValidationOptions{Mode: ValidationModeWarn})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range valid.Findings {
		if f.Code == TemplateCodeCodeContext || f.Code == TemplateCodeUnknownContent {
			t.Fatalf("valid Laterality control refused: %+v", f)
		}
	}
	metadata.CodeContexts[0].Value.ContextIdentifier = "33"
	r.ImageLibraryMetadata, err = BindImageLibraryMetadata(r.ImageLibraryDescriptors, r.ImageLibraryGroups, metadata, NumericRepresentationMetadata{}, ImageLibraryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before := r.ImageLibraryMetadata.Metadata()
	got, err := ValidateImageLibraryTemplate(context.Background(), r, ImageLibraryOptions{}, TemplateValidationOptions{Mode: ValidationModeWarn})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range got.Findings {
		if f.RuleID == "TID1600:4.TID1601:2.TID1602:3" && f.Code == TemplateCodeCodeContext && f.Path.String() == "1.1.1.1.2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("known descriptor metadata did not reach actual row/path: %+v", got)
	}
	if !reflect.DeepEqual(before, r.ImageLibraryMetadata.Metadata()) {
		t.Fatal("template wrapper mutated metadata")
	}
}
