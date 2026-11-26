package sr

import (
	"bytes"
	"encoding/binary"
	"errors"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func contextTestReport1508(segment bool) *MeasurementReport {
	g := MeasurementGroup{Template: "1501", Tracking: TrackingIdentifier{UID: "1.2.826.0.1.3680043.10.543.1508.4", Identifier: "Tracked synthetic target"}, Finding: measurementContextConcept("FIND", "99TEST", "Finding"), FindingSites: []CodedEntry{measurementContextConcept("SITE", "99TEST", "Site")}, Measurements: []ReportMeasurement{{ConceptName: measurementContextConcept("VALUE", "99TEST", "Value"), Value: .125, Units: measurementContextConcept("mm", "UCUM", "millimeter"), Derivation: measurementContextConcept("121401", "DCM", "Derivation"), Image: ImageReference{StudyInstanceUID: "1.2.826.0.1.3680043.10.543.1508.2", SeriesInstanceUID: "1.2.826.0.1.3680043.10.543.1508.5", SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "1.2.826.0.1.3680043.10.543.1508.6", Frames: []int{1}}}}}
	if segment {
		g.Template = "1411"
		g.ReferencedSegment = SegmentReference{StudyInstanceUID: "1.2.826.0.1.3680043.10.543.1508.2", SeriesInstanceUID: "1.2.826.0.1.3680043.10.543.1508.7", SOPClassUID: "1.2.840.10008.5.1.4.1.1.66.4", SOPInstanceUID: "1.2.826.0.1.3680043.10.543.1508.8", SegmentNumber: 7}
		g.SourceSeriesUID = "1.2.826.0.1.3680043.10.543.1508.5"
	}
	return &MeasurementReport{SOPClassUID: Comprehensive3DSRStorage, SOPInstanceUID: "1.2.826.0.1.3680043.10.543.1508.1", StudyInstanceUID: "1.2.826.0.1.3680043.10.543.1508.2", SeriesInstanceUID: "1.2.826.0.1.3680043.10.543.1508.3", ContentDate: "20261009", ContentTime: "120000", Groups: []MeasurementGroup{g}}
}
func contextTestCode1508(value string) *MeasurementContextCode {
	return &MeasurementContextCode{Code: measurementContextConcept(value, "99TEST", value)}
}
func contextTestRWVM1508(suffix string) *MeasurementReferenceEvidence {
	return &MeasurementReferenceEvidence{StudyInstanceUID: "1.2.826.0.1.3680043.10.543.1508.2", SeriesInstanceUID: "1.2.826.0.1.3680043.10.543.1508.9", SOPClassUID: MeasurementRWVMStorage, SOPInstanceUID: "1.2.826.0.1.3680043.10.543.1508." + suffix}
}
func contextTestContext1508() MeasurementReportContext {
	numeric := NumericRepresentation{DecimalString: " 0.12500 ", HasDecimalString: true, HasFloatingPoint: true, FloatingPoint: .125, HasRational: true, RationalNumerator: 1, RationalDenominator: 8}
	order := NumericRepresentation{DecimalString: "2", HasDecimalString: true}
	offset := NumericRepresentation{DecimalString: "-1.25", HasDecimalString: true, HasRational: true, RationalNumerator: -5, RationalDenominator: 4}
	long, context := NewCode("synthetic-method-long-value", "99TEST", "Synthetic method")
	context.CodingSchemeVersion = "2026"
	return MeasurementReportContext{ImagingAlgorithm: &MeasurementAlgorithm{Name: "Container algorithm", Version: "1"}, QualitativeAlgorithm: &MeasurementAlgorithm{Name: "Qualitative algorithm", Version: "2"}, Qualitative: []MeasurementQualitativeEvaluation{{ConceptName: *contextTestCode1508("ASSESS"), ValueType: ValueText, Text: "Qualitative report answer"}}, Groups: []MeasurementGroupContext{{ActivitySession: "Explicit session", TimePoint: &MeasurementTimePoint{SubjectIdentifier: "Subject TP", ProtocolIdentifier: "Protocol TP", TimePoint: "Visit 2", Types: []MeasurementContextCode{*contextTestCode1508("FOLLOWUP")}, Order: &order, TemporalOffset: &offset, EventType: contextTestCode1508("EVENT")}, Method: &MeasurementContextCode{Code: long, Context: context}, FindingSites: []MeasurementFindingSite{{Site: *contextTestCode1508("SITE"), Laterality: &MeasurementContextCode{Code: measurementContextConcept("7771000", "SCT", "Left")}, TopographicalModifier: contextTestCode1508("TOPO")}}, Algorithm: &MeasurementAlgorithm{Name: "Group algorithm", NameCode: contextTestCode1508("ALGO"), Version: "3", Manufacturer: "Synthetic manufacturer", Parameters: []string{"p=1", "q=2"}, Family: contextTestCode1508("FAMILY")}, RealWorldValueMap: contextTestRWVM1508("10"), Qualitative: []MeasurementQualitativeEvaluation{{ConceptName: *contextTestCode1508("QUAL"), ValueType: ValueCode, Code: *contextTestCode1508("YES"), Modifiers: []MeasurementQualitativeModifier{{ConceptName: *contextTestCode1508("MOD"), Value: *contextTestCode1508("MODVAL")}}}}, Measurements: []MeasurementNumericContext{{Numeric: &numeric, Method: contextTestCode1508("OVERRIDE"), FindingSites: []MeasurementFindingSite{{Site: *contextTestCode1508("OTHER")}}, Algorithm: &MeasurementAlgorithm{Name: "Measurement algorithm", Version: "4", Parameters: []string{"measurement=1"}}, RealWorldValueMap: contextTestRWVM1508("11")}}}}}
}
func contextTestBuild1508(t *testing.T, segment bool) *DetailedMeasurementReport {
	t.Helper()
	r, err := BuildMeasurementReportWithContext(contextTestReport1508(segment), contextTestContext1508(), MeasurementContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func contextTestItem1508(t *testing.T, obj *object.Object, path string) *object.Object {
	t.Helper()
	parts := strings.Split(path, ".")
	current := obj
	for _, part := range parts[1:] {
		n := 0
		for _, c := range part {
			n = n*10 + int(c-'0')
		}
		items, ok := current.GetSequence(tagContentSequence)
		if !ok || n < 1 || n > len(items) {
			t.Fatal("fixture path")
		}
		current = items[n-1]
	}
	return current
}

// Rebuild nested sequence values rather than mutating cached item facades.
func contextTestMutate1508(t *testing.T, obj *object.Object, path string, fn func(*[]core.Element)) *object.Object {
	t.Helper()
	parts := strings.Split(path, ".")
	ordinals := []int{}
	for _, part := range parts[1:] {
		n := 0
		for _, c := range part {
			n = n*10 + int(c-'0')
		}
		ordinals = append(ordinals, n-1)
	}
	var edit func([]core.Element, int) []core.Element
	edit = func(elements []core.Element, depth int) []core.Element {
		out := append([]core.Element(nil), elements...)
		if depth == len(ordinals) {
			fn(&out)
			return out
		}
		index := elementIndex(out, tagContentSequence)
		sq := out[index].Value.(core.SequenceValue)
		sq.Items = append([]core.DataSet(nil), sq.Items...)
		sq.Items[ordinals[depth]].Elements = edit(sq.Items[ordinals[depth]].Elements, depth+1)
		out[index].Value = sq
		return out
	}
	result := object.FromElements(edit(obj.Elements(), 0), nil)
	result.SetValueByteOrder(obj.ValueByteOrder())
	return result
}
func contextTestProjection1508(t *testing.T, r *DetailedMeasurementReport, segment bool) {
	t.Helper()
	report, err := r.Report()
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.Context()
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Groups) != 1 || len(report.Groups[0].Measurements) != 1 || report.Groups[0].Measurements[0].Value != .125 || report.Groups[0].Measurements[0].Units.CodeValue != "mm" {
		t.Fatalf("numeric projection: %#v", report.Groups)
	}
	if segment && report.Groups[0].ReferencedSegment.SegmentNumber != 7 {
		t.Fatal("lost segment")
	}
	if report.Groups[0].Tracking.Identifier != "Tracked synthetic target" || report.Groups[0].Measurements[0].Image.Frames[0] != 1 {
		t.Fatal("lost legacy reference/tracking")
	}
	g := c.Groups[0]
	if c.ImagingAlgorithm.Name != "Container algorithm" || c.QualitativeAlgorithm.Name != "Qualitative algorithm" || len(c.Qualitative) != 1 || g.ActivitySession != "Explicit session" || g.TimePoint.TimePoint != "Visit 2" || g.Method.Context.LongCodeValue != "synthetic-method-long-value" || g.Method.Context.CodingSchemeVersion != "2026" || g.FindingSites[0].Laterality.Code.CodeValue != "7771000" || g.Algorithm.Version != "3" || len(g.Algorithm.Parameters) != 2 || g.RealWorldValueMap.SOPInstanceUID == g.Measurements[0].RealWorldValueMap.SOPInstanceUID || g.Measurements[0].Method.Code.CodeValue != "OVERRIDE" || g.Measurements[0].Algorithm.Version != "4" || g.Qualitative[0].Modifiers[0].Value.Code.CodeValue != "MODVAL" {
		t.Fatalf("context: %#v", c)
	}
	if g.TimePoint.Order.DecimalString != "2" && g.TimePoint.Order.DecimalString != "2 " {
		t.Fatal("time point NUM projected as measurement or changed")
	}
	if math.Float64bits(g.Measurements[0].Numeric.FloatingPoint) != math.Float64bits(.125) || g.Measurements[0].Numeric.RationalDenominator != 8 || strings.Trim(g.Measurements[0].Numeric.DecimalString, " ") != "0.12500" {
		t.Fatal("numeric provenance lost")
	}
}
func TestMeasurementContext1508ScopesAndRoundTrips(t *testing.T) {
	for _, segment := range []bool{false, true} {
		t.Run(map[bool]string{false: "1501", true: "1411"}[segment], func(t *testing.T) {
			r := contextTestBuild1508(t, segment)
			contextTestProjection1508(t, r, segment)
			obj, err := r.Dataset()
			if err != nil {
				t.Fatal(err)
			}
			c, _ := r.Context()
			group := contextTestItem1508(t, obj, c.Groups[0].Path)
			children, _ := group.GetSequence(tagContentSequence)
			seenOffset := false
			for _, item := range children {
				concept, ok := readCode(item, tagConceptNameCodeSeq)
				if ok && concept.CodeValue == "128740" {
					rel, _ := item.GetString(tagRelationshipType)
					if rel != RelationshipHasObsContext {
						t.Fatal("time point relationship")
					}
					seenOffset = true
				}
			}
			if !seenOffset {
				t.Fatal("missing offset")
			}
			for _, syntax := range []transfer.Syntax{transfer.ImplicitVRLittleEndian, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
				var encoded bytes.Buffer
				if err := object.WriteFile(&encoded, &object.File{Dataset: obj, TransferSyntax: syntax}); err != nil {
					t.Fatal(err)
				}
				parsed, err := object.ReadFile(bytes.NewReader(encoded.Bytes()))
				if err != nil {
					t.Fatal(err)
				}
				after, err := ReadMeasurementReportWithContext(parsed.Dataset, MeasurementContextOptions{})
				if err != nil {
					t.Fatalf("%s: %v", syntax.UID, err)
				}
				contextTestProjection1508(t, after, segment)
				rewritten, err := after.Dataset()
				if err != nil {
					t.Fatal(err)
				}
				again, err := ReadMeasurementReportWithContext(rewritten, MeasurementContextOptions{})
				if err != nil {
					t.Fatal(err)
				}
				contextTestProjection1508(t, again, segment)
			}
		})
	}
}
func TestMeasurementContext1508PreservesUnmodeledCanonicalContent(t *testing.T) {
	r := contextTestBuild1508(t, false)
	obj, err := r.Dataset()
	if err != nil {
		t.Fatal(err)
	}
	unknown := ContentItem{ValueType: ValueContainer, RelationshipType: RelationshipContains, ConceptName: derivedImagingMeasurementsConcept, ContinuityOfContent: "SEPARATE", Children: []ContentItem{{ValueType: ValueText, RelationshipType: RelationshipContains, ConceptName: measurementContextConcept("CUSTOM", "99TEST", "Extension"), Text: "Unknown derived section remains"}}}
	ds, err := contentItemDataSet(unknown)
	if err != nil {
		t.Fatal(err)
	}
	source := contextTestMutate1508(t, obj, "1", func(elements *[]core.Element) {
		idx := elementIndex(*elements, tagContentSequence)
		sq := (*elements)[idx].Value.(core.SequenceValue)
		sq.Items = append(append([]core.DataSet(nil), sq.Items...), ds)
		(*elements)[idx].Value = sq
	})
	before := source.Elements()
	view, err := ReadMeasurementReportWithContext(source, MeasurementContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := view.Validation()
	if len(v.Unsupported) == 0 {
		t.Fatal("missing unsupported finding")
	}
	out, err := view.Dataset()
	if err != nil {
		t.Fatal(err)
	}
	read, err := ReadDocument(out)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read.Content[len(read.Content)-1], unknown) {
		t.Fatal("derived content lost or reconstructed")
	}
	if !reflect.DeepEqual(source.Elements(), before) {
		t.Fatal("source mutated")
	}
}
func TestMeasurementContext1508ExactIdentityAndNoInventedContext(t *testing.T) {
	base, err := BuildMeasurementReportWithContext(contextTestReport1508(false), MeasurementReportContext{}, MeasurementContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := base.Context()
	if c.ImagingAlgorithm != nil || c.Groups[0].TimePoint != nil || c.Groups[0].Method != nil || len(c.Qualitative) > 0 {
		t.Fatal("invented context")
	}
	obj, _ := base.Dataset()
	source := contextTestMutate1508(t, obj, c.Groups[0].Path, func(elements *[]core.Element) {
		n := core.DataSet{Elements: []core.Element{strElem(tagValueType, core.VRCS, "CODE"), strElem(tagRelationshipType, core.VRCS, RelationshipContains), codeSequence(tagConceptNameCodeSeq, measurementContextConcept("OTHER", "99OTHER", "Measurement Method")), codeSequence(tagConceptCodeSeq, measurementContextConcept("ANSWER", "99TEST", "Answer"))}}
		idx := elementIndex(*elements, tagContentSequence)
		sq := (*elements)[idx].Value.(core.SequenceValue)
		sq.Items = append(append([]core.DataSet(nil), sq.Items...), n)
		(*elements)[idx].Value = sq
	})
	view, err := ReadMeasurementReportWithContext(source, MeasurementContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := view.Context()
	if after.Groups[0].Method != nil || len(after.Groups[0].Qualitative) != 1 {
		t.Fatal("meaning fallback invented method")
	}
}
func TestMeasurementContext1508DefensiveCopiesStaleAndExplicitRebind(t *testing.T) {
	r := contextTestBuild1508(t, true)
	c, _ := r.Context()
	c.Groups[0].Algorithm.Parameters[0] = "changed"
	c.Groups[0].FindingSites[0].Laterality.Code.CodeValue = "changed"
	c.Groups[0].Measurements[0].Numeric.DecimalString = "changed"
	report, _ := r.Report()
	report.Groups[0].Measurements[0].Image.Frames[0] = 99
	e, _ := r.Evidence()
	e[0].SOPInstanceUID = "changed"
	refs := r.MetadataSnapshot().MeasurementReferences()
	refs.Entries[0].Segments[0] = 99
	contextTestProjection1508(t, r, true)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if _, err := r.Context(); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	doc := r.Document()
	doc.Content[0].Code.CodeMeaning = "Edited language"
	if out, err := r.Dataset(); out != nil || !errors.Is(err, ErrMetadataSnapshot) {
		t.Fatal("stale output allowed")
	}
	if _, err := r.Report(); !errors.Is(err, ErrMetadataSnapshot) {
		t.Fatal("stale projection allowed")
	}
	s, err := BindMeasurementContextMetadata(doc, r.MetadataSnapshot().Metadata(), r.MetadataSnapshot().NumericRepresentations(), r.MetadataSnapshot().MeasurementReferences(), MetadataSnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	evidence, _ := admitMeasurementEvidence(r.evidence, r.options)
	rebound, err := BindMeasurementReportWithContext(doc, s, evidence, MeasurementContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = rebound.Dataset(); err != nil {
		t.Fatal(err)
	}
}
func TestMeasurementContext1508ReferenceMacroGuardAndBudgets(t *testing.T) {
	doc := &Document{SOPClassUID: ComprehensiveSRStorage, Title: measurementContextConcept("ROOT", "99TEST", "Root"), Content: []ContentItem{{ValueType: ValueImage, RelationshipType: RelationshipContains, Image: ImageReference{SOPClassUID: "1.2.3", SOPInstanceUID: "1.2.4"}}, {ValueType: ValueImage, RelationshipType: RelationshipContains, Image: ImageReference{SOPClassUID: "1.2.3", SOPInstanceUID: "1.2.4"}}}}
	refs := MeasurementReferenceMetadata{Entries: []MeasurementReferenceMetadataEntry{{Path: "1.1", SOPClassUID: "1.2.3", SOPInstanceUID: "1.2.4", Segments: []uint16{1}}, {Path: "1.2", SOPClassUID: "1.2.3", SOPInstanceUID: "1.2.4", Segments: []uint16{2}}}}
	if _, err := BindMeasurementContextMetadata(doc, TemplateValidationMetadata{}, NumericRepresentationMetadata{}, refs, MetadataSnapshotOptions{}); !errors.Is(err, ErrMetadataSnapshot) {
		t.Fatal("ambiguous identical IMAGE binding allowed")
	}
	refs.Entries[1].Segments[0] = 1
	s, err := BindMeasurementContextMetadata(doc, TemplateValidationMetadata{}, NumericRepresentationMetadata{}, refs, MetadataSnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Check(doc); err != nil {
		t.Fatal(err)
	}
	if _, err := BindMeasurementContextMetadata(doc, TemplateValidationMetadata{}, NumericRepresentationMetadata{}, refs, MetadataSnapshotOptions{MaxItems: 3}); !errors.Is(err, ErrResourceLimitExceeded) {
		t.Fatal("combined metadata quota missing")
	}
	refs.Entries[0].SOPInstanceUID = "1.2.9"
	if _, err := BindMeasurementContextMetadata(doc, TemplateValidationMetadata{}, NumericRepresentationMetadata{}, refs, MetadataSnapshotOptions{}); !errors.Is(err, ErrMeasurementContext) {
		t.Fatal("wrong SOP association allowed")
	}
}
func TestMeasurementContext1508LegacyProjectionControl(t *testing.T) {
	// This control records the legacy gap; it is not used for preservation output.
	r := contextTestBuild1508(t, false)
	obj, err := r.Dataset()
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := ReadMeasurementReport(obj)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy.Groups[0].Measurements) != 3 {
		t.Fatalf("legacy gap control changed: got %d", len(legacy.Groups[0].Measurements))
	}
	modern, err := ReadMeasurementReportWithContext(obj, MeasurementContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	report, _ := modern.Report()
	if len(report.Groups[0].Measurements) != 1 {
		t.Fatal("observation NUM still misclassified")
	}
}
func TestMeasurementContext1508RefusalsAndSourceIntact(t *testing.T) {
	r := contextTestBuild1508(t, false)
	obj, _ := r.Dataset()
	c, _ := r.Context()
	gp := c.Groups[0].Path
	mp := c.Groups[0].Measurements[0].Path
	// Generic path conversion avoids assumptions about the number of context rows.
	pathFor := func(parent, code, scheme string) string {
		items, _ := contextTestItem1508(t, obj, parent).GetSequence(tagContentSequence)
		for i, item := range items {
			name, _ := readCode(item, tagConceptNameCodeSeq)
			if name.CodeValue == code && name.CodingSchemeDesignator == scheme {
				p := parent
				ordinal := i + 1
				digits := ""
				for ordinal > 0 {
					digits = string(rune('0'+ordinal%10)) + digits
					ordinal /= 10
				}
				return p + "." + digits
			}
		}
		t.Fatalf("missing fixture code %s", code)
		return ""
	}
	method := pathFor(gp, "370129005", "SCT")
	tp := pathFor(gp, "C2348792", "UMLS")
	order := pathFor(gp, "126073", "DCM")
	algoVersion := pathFor(gp, "111003", "DCM")
	rwvm := pathFor(mp, "126100", "DCM")
	site := pathFor(gp, "363698007", "SCT")
	cases := []struct {
		name, path string
		change     func(*[]core.Element)
	}{
		{"wrong-value-type", method, func(e *[]core.Element) {
			replaceOrAppendElement(e, strElem(tagValueType, core.VRCS, "TEXT"))
			replaceOrAppendElement(e, strElem(tagTextValue, core.VRUT, "PRIVATE_VALUE"))
		}},
		{"wrong-relationship", method, func(e *[]core.Element) {
			replaceOrAppendElement(e, strElem(tagRelationshipType, core.VRCS, RelationshipContains))
		}},
		{"missing-time-point", tp, func(e *[]core.Element) { replaceOrAppendElement(e, strElem(tagTextValue, core.VRUT, "")) }},
		{"missing-algorithm-version", algoVersion, func(e *[]core.Element) { replaceOrAppendElement(e, strElem(tagTextValue, core.VRUT, "")) }},
		{"bad-VM", method, func(e *[]core.Element) {
			replaceOrAppendElement(e, core.Element{Header: core.ElementHeader{Tag: tagValueType, VR: core.VRCS}, Value: core.StringValue{"CODE", "TEXT"}})
		}},
		{"wrong-code-VR", method, func(e *[]core.Element) {
			idx := elementIndex(*e, tagConceptCodeSeq)
			sq := (*e)[idx].Value.(core.SequenceValue)
			sq.Items = append([]core.DataSet(nil), sq.Items...)
			attrs := append([]core.Element(nil), sq.Items[0].Elements...)
			replaceOrAppendElement(&attrs, strElem(tagLongCodeValue, core.VRLO, "PRIVATE_VALUE"))
			sq.Items[0].Elements = attrs
			(*e)[idx].Value = sq
		}},
		{"empty-NUM", mp, func(e *[]core.Element) { replaceOrAppendElement(e, seqElement(tagMeasuredValueSeq)) }},
		{"wrong-fixed-unit", order, func(e *[]core.Element) {
			idx := elementIndex(*e, tagMeasuredValueSeq)
			sq := (*e)[idx].Value.(core.SequenceValue)
			sq.Items = append([]core.DataSet(nil), sq.Items...)
			attrs := append([]core.Element(nil), sq.Items[0].Elements...)
			replaceOrAppendElement(&attrs, codeSequence(tagMeasurementUnitsCode, measurementContextConcept("mm", "UCUM", "millimeter")))
			sq.Items[0].Elements = attrs
			(*e)[idx].Value = sq
		}},
		{"wrong-RWVM-relationship", rwvm, func(e *[]core.Element) {
			replaceOrAppendElement(e, strElem(tagRelationshipType, core.VRCS, RelationshipContains))
		}},
		{"duplicate-laterality", site, func(e *[]core.Element) {
			idx := elementIndex(*e, tagContentSequence)
			sq := (*e)[idx].Value.(core.SequenceValue)
			sq.Items = append(append([]core.DataSet(nil), sq.Items...), sq.Items[0])
			(*e)[idx].Value = sq
		}},
		{"duplicate-method", gp, func(e *[]core.Element) {
			idx := elementIndex(*e, tagContentSequence)
			sq := (*e)[idx].Value.(core.SequenceValue)
			for _, it := range sq.Items {
				o := object.FromElements(it.Elements, nil)
				name, _ := readCode(o, tagConceptNameCodeSeq)
				if name.CodeValue == "370129005" {
					sq.Items = append(append([]core.DataSet(nil), sq.Items...), it)
					break
				}
			}
			(*e)[idx].Value = sq
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := contextTestMutate1508(t, obj, tc.path, tc.change)
			before := source.Elements()
			view, err := ReadMeasurementReportWithContext(source, MeasurementContextOptions{})
			if view != nil || err == nil {
				t.Fatal("invalid recognized profile admitted")
			}
			if strings.Contains(err.Error(), "PRIVATE_VALUE") {
				t.Fatal("error leaked content")
			}
			if !reflect.DeepEqual(source.Elements(), before) {
				t.Fatal("source mutated on refusal")
			}
		})
	}
	for _, options := range []MeasurementContextOptions{{MaxEvidence: 1}, {MaxEvidenceItems: 1}, {MaxStringBytes: 1}, {MaxContextFindings: -1}, {Read: ReadOptions{MaxDepth: 1}}, {Snapshot: MetadataSnapshotOptions{MaxValueBytes: 1}}} {
		if _, err := ReadMeasurementReportWithContext(obj, options); !errors.Is(err, ErrResourceLimitExceeded) {
			t.Fatalf("quota not enforced: %v", err)
		}
	}
}
func TestMeasurementContext1508AuthoringContradictions(t *testing.T) {
	for _, mutate := range []func(*MeasurementReportContext){
		func(c *MeasurementReportContext) { c.Groups = append(c.Groups, MeasurementGroupContext{}) },
		func(c *MeasurementReportContext) {
			c.Groups[0].Measurements = append(c.Groups[0].Measurements, MeasurementNumericContext{})
		},
		func(c *MeasurementReportContext) { c.Groups[0].FindingSites[0].Site.Code.CodeValue = "CONFLICT" },
		func(c *MeasurementReportContext) { c.Groups[0].Algorithm.Version = "" },
		func(c *MeasurementReportContext) { c.Groups[0].TimePoint.TimePoint = "" },
		func(c *MeasurementReportContext) { c.Groups[0].TimePoint.EventType = nil },
		func(c *MeasurementReportContext) { c.Groups[0].Measurements[0].Numeric.FloatingPoint = 2 },
		func(c *MeasurementReportContext) { c.Groups[0].RealWorldValueMap.SOPClassUID = "1.2.3" },
		func(c *MeasurementReportContext) { c.Groups[0].RealWorldValueMap.SeriesInstanceUID = "" },
		func(c *MeasurementReportContext) {
			c.Groups[0].Measurements[0].RealWorldValueMap.SOPInstanceUID = c.Groups[0].RealWorldValueMap.SOPInstanceUID
			c.Groups[0].Measurements[0].RealWorldValueMap.SeriesInstanceUID = "1.2.9"
		},
		func(c *MeasurementReportContext) {
			c.Qualitative[0].Modifiers = []MeasurementQualitativeModifier{{ConceptName: *contextTestCode1508("MOD"), Value: *contextTestCode1508("ANSWER")}}
		},
	} {
		c := contextTestContext1508()
		mutate(&c)
		if r, err := BuildMeasurementReportWithContext(contextTestReport1508(false), c, MeasurementContextOptions{}); r != nil || err == nil {
			t.Fatal("authoring contradiction accepted")
		}
	}
}
func TestMeasurementContext1508USByteOrderAndGenericSnapshot(t *testing.T) {
	r := contextTestBuild1508(t, true)
	obj, _ := r.Dataset()
	c, _ := r.Context()
	group := contextTestItem1508(t, obj, c.Groups[0].Path)
	items, _ := group.GetSequence(tagContentSequence)
	segmentPath := ""
	for idx, it := range items {
		name, _ := readCode(it, tagConceptNameCodeSeq)
		if name.CodeValue == "121191" {
			ordinal := idx + 1
			digits := ""
			for ordinal > 0 {
				digits = string(rune('0'+ordinal%10)) + digits
				ordinal /= 10
			}
			segmentPath = c.Groups[0].Path + "." + digits
		}
	}
	source := contextTestMutate1508(t, obj, segmentPath, func(e *[]core.Element) {
		idx := elementIndex(*e, tagRefSOPSequence)
		sq := (*e)[idx].Value.(core.SequenceValue)
		sq.Items = append([]core.DataSet(nil), sq.Items...)
		attrs := append([]core.Element(nil), sq.Items[0].Elements...)
		replaceOrAppendElement(&attrs, core.NewRawElement(tagReferencedSegmentNumber, core.VRUS, []byte{0, 7}))
		sq.Items[0].Elements = attrs
		(*e)[idx].Value = sq
	})
	source.SetValueByteOrder(binary.BigEndian)
	read, err := ReadMeasurementReportWithContext(source, MeasurementContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	contextTestProjection1508(t, read, true)
	generic, err := read.Document().DatasetWithMetadataSnapshot(read.MetadataSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := contextTestItem1508(t, generic, segmentPath).GetSequence(tagRefSOPSequence)
	values, _ := ref[0].GetInts(tagReferencedSegmentNumber)
	if !reflect.DeepEqual(values, []int64{7}) {
		t.Fatalf("generic snapshot lost US: %#v", values)
	}
	before, _, err := ReadDocumentWithNumericRepresentations(source, ReadOptions{}, MetadataSnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := BindNumericRepresentations(before.Document, before.TemplateMetadata, read.MetadataSnapshot().NumericRepresentations(), MetadataSnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := before.Document.DatasetWithMetadataSnapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ = contextTestItem1508(t, legacy, segmentPath).GetSequence(tagRefSOPSequence)
	if ref[0].Has(tagReferencedSegmentNumber) {
		t.Fatal("legacy snapshot output changed")
	}
	refs := read.MetadataSnapshot().MeasurementReferences()
	refs.Entries[0].Segments = []uint16{7, 8}
	s, err = BindMeasurementContextMetadata(read.Document(), read.MetadataSnapshot().Metadata(), read.MetadataSnapshot().NumericRepresentations(), refs, MetadataSnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	generic, err = read.Document().DatasetWithMetadataSnapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ = contextTestItem1508(t, generic, segmentPath).GetSequence(tagRefSOPSequence)
	values, _ = ref[0].GetInts(tagReferencedSegmentNumber)
	if !reflect.DeepEqual(values, []int64{7, 8}) {
		t.Fatal("ordered segment VM lost")
	}
}
func TestMeasurementContext1508NoCallbacksAndEvidenceConflicts(t *testing.T) {
	r := contextTestBuild1508(t, false)
	obj, _ := r.Dataset()
	c, _ := r.Context()
	var calls atomic.Int32
	bad := contextTestMutate1508(t, obj, c.Groups[0].Path, func(e *[]core.Element) {
		replaceOrAppendElement(e, core.Element{Header: core.ElementHeader{Tag: tagTextValue, VR: core.VRUT}, Value: numericCallbackValue{calls: &calls}})
	})
	if _, err := ReadMeasurementReportWithContext(bad, MeasurementContextOptions{}); err == nil || calls.Load() != 0 {
		t.Fatal("resident custom callback invoked/admitted")
	}
	evidence, _ := r.Evidence()
	duplicate := evidence[0]
	duplicate.SeriesInstanceUID = "1.2.9"
	evidence = append(evidence, duplicate)
	if _, err := BindMeasurementReportWithContext(r.Document(), r.MetadataSnapshot(), evidence, MeasurementContextOptions{}); !errors.Is(err, ErrMeasurementContext) {
		t.Fatal("conflicting SOP evidence admitted")
	}
}
func TestMeasurementContext1508IndependentSourceRows(t *testing.T) {
	legacy := contextTestReport1508(false)
	file, err := WriteMeasurementReport(legacy)
	if err != nil {
		t.Fatal(err)
	}
	// Independent direct source rows: the new context author is not involved.
	groupsPath := ""
	roots, _ := file.Dataset.GetSequence(tagContentSequence)
	for idx, it := range roots {
		code, _ := readCode(it, tagConceptNameCodeSeq)
		if code.CodeValue == "126010" && code.CodingSchemeDesignator == "DCM" {
			groupsPath = "1." + strconv.Itoa(idx+1) + ".1"
		}
	}
	if groupsPath == "" {
		t.Fatal("missing independent fixture container")
	}
	source := contextTestMutate1508(t, file.Dataset, groupsPath, func(e *[]core.Element) {
		idx := elementIndex(*e, tagContentSequence)
		sq := (*e)[idx].Value.(core.SequenceValue)
		rows := []core.DataSet{
			{Elements: []core.Element{strElem(tagValueType, core.VRCS, "TEXT"), strElem(tagRelationshipType, core.VRCS, "HAS OBS CONTEXT"), codeSequence(tagConceptNameCodeSeq, measurementContextConcept("C2348792", "UMLS", "Time Point")), strElem(tagTextValue, core.VRUT, "Independent Visit")}},
			{Elements: []core.Element{strElem(tagValueType, core.VRCS, "NUM"), strElem(tagRelationshipType, core.VRCS, "HAS OBS CONTEXT"), codeSequence(tagConceptNameCodeSeq, measurementContextConcept("126073", "DCM", "Time Point Order")), seqElement(tagMeasuredValueSeq, core.DataSet{Elements: []core.Element{codeSequence(tagMeasurementUnitsCode, measurementContextConcept("1", "UCUM", "no units")), strElem(tagNumericValue, core.VRDS, "4")}})}},
			{Elements: []core.Element{strElem(tagValueType, core.VRCS, "CODE"), strElem(tagRelationshipType, core.VRCS, "HAS CONCEPT MOD"), codeSequence(tagConceptNameCodeSeq, measurementContextConcept("370129005", "SCT", "Measurement Method")), codeSequence(tagConceptCodeSeq, measurementContextConcept("INDEPENDENT", "99TEST", "Independent Method"))}},
		}
		sq.Items = append(append([]core.DataSet(nil), sq.Items...), rows...)
		(*e)[idx].Value = sq
	})
	view, err := ReadMeasurementReportWithContext(source, MeasurementContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	report, _ := view.Report()
	c, _ := view.Context()
	if len(report.Groups[0].Measurements) != 1 || c.Groups[0].TimePoint.TimePoint != "Independent Visit" || c.Groups[0].TimePoint.Order.DecimalString != "4" || c.Groups[0].Method.Code.CodeValue != "INDEPENDENT" {
		t.Fatal("independent source rows projected incorrectly")
	}
	output, err := view.Dataset()
	if err != nil {
		t.Fatal(err)
	}
	again, err := ReadMeasurementReportWithContext(output, MeasurementContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	round, _ := again.Context()
	if !reflect.DeepEqual(c, round) {
		t.Fatal("independent source context changed")
	}
}
func TestMeasurementContext1508URNAndUnicode(t *testing.T) {
	c := contextTestContext1508()
	code, ctx := NewCode("urn:synthetic:method:1508", "", "Método sintético")
	c.Groups[0].Method = &MeasurementContextCode{Code: code, Context: ctx}
	c.Qualitative[0].Text = "Avaliação qualitativa: ação, ausência e precisão"
	r, err := BuildMeasurementReportWithContext(contextTestReport1508(false), c, MeasurementContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	obj, err := r.Dataset()
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := object.WriteFile(&b, &object.File{Dataset: obj, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	file, err := object.ReadFile(bytes.NewReader(b.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	after, err := ReadMeasurementReportWithContext(file.Dataset, MeasurementContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := after.Context()
	if got.Groups[0].Method.Context.URNCodeValue != ctx.URNCodeValue || got.Groups[0].Method.Code.CodeMeaning != code.CodeMeaning || got.Qualitative[0].Text != c.Qualitative[0].Text {
		t.Fatal("URN/Unicode context changed")
	}
}
func TestMeasurementContext1508QualitativeOnlyAndRetainedCharset(t *testing.T) {
	r, err := BuildMeasurementReportWithContext(&MeasurementReport{SOPClassUID: ComprehensiveSRStorage, SOPInstanceUID: "1.2.826.0.1.3680043.10.543.1508.50", ContentDate: "20261009", ContentTime: "120000"}, MeasurementReportContext{Qualitative: []MeasurementQualitativeEvaluation{{ConceptName: *contextTestCode1508("ANSWER"), ValueType: ValueText, Text: "Qualitative-only explicit answer"}}}, MeasurementContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Remove the empty authored Imaging Measurements to exercise a read source
	// that genuinely has only the qualitative container.
	obj, _ := r.Dataset()
	source := contextTestMutate1508(t, obj, "1", func(e *[]core.Element) {
		idx := elementIndex(*e, tagContentSequence)
		sq := (*e)[idx].Value.(core.SequenceValue)
		out := []core.DataSet{}
		for _, ds := range sq.Items {
			o := object.FromElements(ds.Elements, nil)
			code, _ := readCode(o, tagConceptNameCodeSeq)
			if code.CodeValue != "126010" {
				out = append(out, ds)
			}
		}
		sq.Items = out
		(*e)[idx].Value = sq
	})
	source.Put(strElem(core.NewTag(0x0008, 0x0005), core.VRCS, "ISO_IR 100"))
	view, err := ReadMeasurementReportWithContext(source, MeasurementContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	report, _ := view.Report()
	c, _ := view.Context()
	if len(report.Groups) != 0 || len(c.Qualitative) != 1 {
		t.Fatal("qualitative-only content lost")
	}
	out, err := view.Dataset()
	if err != nil {
		t.Fatal(err)
	}
	charset, _ := out.GetString(core.NewTag(0x0008, 0x0005))
	if charset != "ISO_IR 100" {
		t.Fatal("source charset changed")
	}
}
func TestMeasurementContext1508MalformedSegmentMacro(t *testing.T) {
	r := contextTestBuild1508(t, true)
	obj, _ := r.Dataset()
	refMetadata := r.MetadataSnapshot().MeasurementReferences()
	path := refMetadata.Entries[0].Path
	for _, value := range []core.Element{strElem(tagReferencedSegmentNumber, core.VRIS, "PRIVATE_VALUE"), core.NewRawElement(tagReferencedSegmentNumber, core.VRUS, []byte{1}), core.NewRawElement(tagReferencedSegmentNumber, core.VRUS, []byte{0, 0}), {Header: core.ElementHeader{Tag: tagReferencedSegmentNumber, VR: core.VRUS}, Value: core.Uint16Value{}}} {
		source := contextTestMutate1508(t, obj, path, func(e *[]core.Element) {
			idx := elementIndex(*e, tagRefSOPSequence)
			sq := (*e)[idx].Value.(core.SequenceValue)
			sq.Items = append([]core.DataSet(nil), sq.Items...)
			attrs := append([]core.Element(nil), sq.Items[0].Elements...)
			replaceOrAppendElement(&attrs, value)
			sq.Items[0].Elements = attrs
			(*e)[idx].Value = sq
		})
		before := source.Elements()
		if view, err := ReadMeasurementReportWithContext(source, MeasurementContextOptions{}); view != nil || !errors.Is(err, ErrMeasurementContext) || strings.Contains(err.Error(), "PRIVATE_VALUE") {
			t.Fatal("bad segment macro not refused safely")
		}
		if !reflect.DeepEqual(source.Elements(), before) {
			t.Fatal("segment source changed")
		}
	}
}
func TestMeasurementContext1508LongNumericCodesAndReferenceIndex(t *testing.T) {
	r := contextTestBuild1508(t, false)
	doc := r.Document()
	c, _ := r.Context()
	path := c.Groups[0].Measurements[0].Path
	walker := newReferenceWalker(doc.Content, ContentItemIdentifier{1}, 1)
	var item *ContentItem
	for {
		entry, ok := walker.next()
		if !ok {
			break
		}
		if entry.path.String() == path {
			item = entry.item
			break
		}
		walker.push(entry.item.Children, entry.path, entry.depth+1)
	}
	name, nameContext := NewCode("synthetic-long-measurement-code", "99TEST", "Long measurement")
	unit, unitContext := NewCode("synthetic-long-unit-code", "99UNIT", "Long unit")
	item.ConceptName = name
	item.Measurement.Units = unit
	metadata := r.MetadataSnapshot().Metadata()
	found := false
	for i := range metadata.CodeContexts {
		if metadata.CodeContexts[i].Path == path {
			metadata.CodeContexts[i].ConceptName = nameContext
			metadata.CodeContexts[i].MeasurementUnits = unitContext
			found = true
		}
	}
	if !found {
		metadata.CodeContexts = append(metadata.CodeContexts, ContentItemCodeContexts{Path: path, ConceptName: nameContext, MeasurementUnits: unitContext})
	}
	numeric := r.MetadataSnapshot().NumericRepresentations()
	for i := range numeric.Entries {
		if numeric.Entries[i].Path == path {
			numeric.Entries[i].Representation.DecimalString = ".1"
		}
	}
	snapshot, err := BindMeasurementContextMetadata(doc, metadata, numeric, r.MetadataSnapshot().MeasurementReferences(), MetadataSnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	view, err := BindMeasurementReportWithContext(doc, snapshot, r.evidence, MeasurementContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	obj, err := view.Dataset()
	if err != nil {
		t.Fatal(err)
	}
	read, err := ReadMeasurementReportWithContext(obj, MeasurementContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := read.Context()
	if got.Groups[0].Measurements[0].ConceptName.Context.LongCodeValue != nameContext.LongCodeValue || got.Groups[0].Measurements[0].Units.Context.LongCodeValue != unitContext.LongCodeValue {
		t.Fatal("numeric long code lost")
	}
	options := DefaultReadOptions()
	options.ResolveReferences = true
	result, _, err := ReadDocumentWithNumericRepresentations(obj, options, MetadataSnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.References == nil {
		t.Fatal("existing numeric API lost reference index")
	}
	// The old API's resolver must see FD preference, not a stale DS approximation.
	if _, err := result.References.Edges(); err != nil {
		t.Fatal("numeric reference index stale after FD selection", err)
	}
}

func TestMeasurementContext1508ReviewAdmission(t *testing.T) {
	r := contextTestBuild1508(t, false)
	obj, err := r.Dataset()
	if err != nil {
		t.Fatal(err)
	}
	c, _ := r.Context()
	gp := c.Groups[0].Path
	mp := c.Groups[0].Measurements[0].Path
	find := func(parent string, code string) string {
		items, _ := contextTestItem1508(t, obj, parent).GetSequence(tagContentSequence)
		for i, it := range items {
			v, _ := readCode(it, tagConceptNameCodeSeq)
			if v.CodeValue == code {
				return parent + "." + strconv.Itoa(i+1)
			}
		}
		t.Fatal("missing fixture row")
		return ""
	}
	image := find(mp, sourceOfMeasurementConcept.CodeValue)
	rwvm := find(gp, contextRWVM.CodeValue)
	for _, target := range []struct{ name, path string }{{"image", image}, {"composite", rwvm}} {
		for _, kind := range []string{"class-vm2", "instance-vm2", "class-wrong-vr", "instance-wrong-vr", "sq2", "invalid-uid", "raw-vm2"} {
			t.Run(target.name+"/"+kind, func(t *testing.T) {
				bad := contextTestMutate1508(t, obj, target.path, func(attrs *[]core.Element) {
					idx := elementIndex(*attrs, tagRefSOPSequence)
					sq := (*attrs)[idx].Value.(core.SequenceValue)
					sq.Items = append([]core.DataSet(nil), sq.Items...)
					fields := append([]core.Element(nil), sq.Items[0].Elements...)
					if kind == "sq2" {
						sq.Items = append(sq.Items, sq.Items[0])
					} else {
						tag := tagRefSOPClassUID
						if strings.HasPrefix(kind, "instance") {
							tag = tagRefSOPInstanceUID
						}
						old := fields[elementIndex(fields, tag)]
						value := old.Value.(core.StringValue)[0]
						switch kind {
						case "class-vm2", "instance-vm2":
							old.Value = core.StringValue{value, "1.2.826.0.1.3680043.10.543.1508.999"}
						case "class-wrong-vr", "instance-wrong-vr":
							old.Header.VR = core.VRLO
						case "invalid-uid":
							old.Value = core.StringValue{"1..2"}
						case "raw-vm2":
							old.Value = core.RawValue([]byte(value + "\\1.2.3"))
						}
						replaceOrAppendElement(&fields, old)
						sq.Items[0].Elements = fields
					}
					(*attrs)[idx].Value = sq
				})
				before := append([]core.Element(nil), bad.Elements()...)
				got, err := ReadMeasurementReportWithContext(bad, MeasurementContextOptions{})
				if got != nil || !errors.Is(err, ErrMeasurementContext) {
					t.Fatalf("ambiguous reference admitted: facade=%t err=%v", got != nil, err)
				}
				if !reflect.DeepEqual(before, bad.Elements()) {
					t.Fatal("source changed on refusal")
				}
			})
		}
	}
	t.Run("raw-string-admission-before-consumer", func(t *testing.T) {
		o, _ := normalizeMeasurementContextOptions(MeasurementContextOptions{MaxStringBytes: 8})
		for _, vr := range []core.VR{core.VRUT, core.VRUI, core.VRUC} {
			err := admitMeasurementResident([]core.Element{{Header: core.ElementHeader{Tag: tagTextValue, VR: vr}, Value: core.RawValue([]byte("123456789"))}}, o)
			if !errors.Is(err, ErrResourceLimitExceeded) {
				t.Fatalf("raw %s limit not admitted before consumer: %v", vr, err)
			}
		}
		err := admitMeasurementResident([]core.Element{{Header: core.ElementHeader{Tag: tagTextValue, VR: core.VRUT}, Value: core.StringValue{"123456789"}}}, o)
		if !errors.Is(err, ErrResourceLimitExceeded) {
			t.Fatal("string control")
		}
	})
	t.Run("person-name-vm-and-vr", func(t *testing.T) {
		report := contextTestReport1508(false)
		report.Observers = []Observer{{PersonName: "Synthetic^Observer"}}
		view, err := BuildMeasurementReportWithContext(report, MeasurementReportContext{}, MeasurementContextOptions{})
		if err != nil {
			t.Fatal(err)
		}
		source, err := view.Dataset()
		if err != nil {
			t.Fatal(err)
		}
		observerPath := ""
		for i, it := range view.Document().Content {
			if it.ConceptName == personObserverNameConcept {
				observerPath = "1." + strconv.Itoa(i+1)
			}
		}
		if observerPath == "" {
			t.Fatal("person observer fixture")
		}
		for _, wrongVR := range []bool{false, true} {
			bad := contextTestMutate1508(t, source, observerPath, func(attrs *[]core.Element) {
				e := (*attrs)[elementIndex(*attrs, tagPersonNameValue)]
				if wrongVR {
					e.Header.VR = core.VRLO
				} else {
					e.Value = core.StringValue{"Synthetic^Observer", "Second^Observer"}
				}
				replaceOrAppendElement(attrs, e)
			})
			if _, err := ReadMeasurementReportWithContext(bad, MeasurementContextOptions{}); !errors.Is(err, ErrMeasurementContext) {
				t.Fatalf("ambiguous PN admitted: %v", err)
			}
		}
	})
	t.Run("read-ignores-bind-charset-override", func(t *testing.T) {
		if _, err := ReadMeasurementReportWithContext(obj, MeasurementContextOptions{CharacterSet: []string{"invalid-character-set"}}); err != nil {
			t.Fatalf("Read used Bind-only override: %v", err)
		}
	})
}

func TestMeasurementContext1508RWVMSeam(t *testing.T) {
	r := contextTestBuild1508(t, true)
	v, err := r.Validation()
	if err != nil {
		t.Fatal(err)
	}
	c, _ := r.Context()
	mp := c.Groups[0].Measurements[0].Path
	found := false
	for _, f := range v.Unsupported {
		if f.Code == MeasurementContextRWVMExtension && strings.HasPrefix(f.Path, mp+".") {
			found = true
		}
	}
	if !found {
		t.Fatal("1411 per-NUM extension was not disclosed")
	}
	if v.Relationships.HasErrors() {
		t.Fatal("generic IOD constraint relaxed")
	}
	obj, err := r.Dataset()
	if err != nil {
		t.Fatal(err)
	}
	items, _ := contextTestItem1508(t, obj, mp).GetSequence(tagContentSequence)
	path := ""
	for i, it := range items {
		v, _ := readCode(it, tagConceptNameCodeSeq)
		if v.CodeValue == contextRWVM.CodeValue {
			path = mp + "." + strconv.Itoa(i+1)
		}
	}
	if path == "" {
		t.Fatal("missing RWVM")
	}
	bad := contextTestMutate1508(t, obj, path, func(attrs *[]core.Element) {
		replaceOrAppendElement(attrs, strElem(tagRelationshipType, core.VRCS, RelationshipContains))
	})
	if _, err := ReadMeasurementReportWithContext(bad, MeasurementContextOptions{}); !errors.Is(err, ErrMeasurementContext) {
		t.Fatalf("NUM CONTAINS COMPOSITE admitted: %v", err)
	}
}

func TestMeasurementContext1508SegmentSOPClasses(t *testing.T) {
	for _, class := range []string{"1.2.840.10008.5.1.4.1.1.66.4", "1.2.840.10008.5.1.4.1.1.66.5", "1.2.840.10008.5.1.4.1.1.66.7"} {
		t.Run(class, func(t *testing.T) {
			report := contextTestReport1508(true)
			report.Groups[0].ReferencedSegment.SOPClassUID = class
			view, err := BuildMeasurementReportWithContext(report, contextTestContext1508(), MeasurementContextOptions{})
			if err != nil {
				t.Fatal(err)
			}
			data, err := view.Dataset()
			if err != nil {
				t.Fatal(err)
			}
			read, err := ReadMeasurementReportWithContext(data, MeasurementContextOptions{})
			if err != nil {
				t.Fatal(err)
			}
			got, err := read.Report()
			if err != nil {
				t.Fatal(err)
			}
			if got.Groups[0].ReferencedSegment.SOPClassUID != class || got.Groups[0].ReferencedSegment.SegmentNumber != 7 {
				t.Fatal("lost exact segmentation class/reference")
			}
		})
	}
	for _, class := range []string{"1.2.840.10008.5.1.4.1.1.2", "1.2.840.10008.5.1.4.1.1.66.8"} {
		report := contextTestReport1508(true)
		report.Groups[0].ReferencedSegment.SOPClassUID = class
		if _, err := BuildMeasurementReportWithContext(report, contextTestContext1508(), MeasurementContextOptions{}); !errors.Is(err, ErrMeasurementContext) {
			t.Fatalf("unrelated/out-of-profile segment class admitted: %v", err)
		}
	}
}
func TestMeasurementContext1508OpaqueOutputBoundary(t *testing.T) {
	r := contextTestBuild1508(t, false)
	obj, err := r.Dataset()
	if err != nil {
		t.Fatal(err)
	}
	c, _ := r.Context()
	path := c.Groups[0].Measurements[0].Path
	tag := core.NewTag(0x7777, 0x1010)
	for _, big := range []bool{false, true} {
		raw := []byte{7, 0}
		if big {
			raw = []byte{0, 7}
		}
		source := contextTestMutate1508(t, obj, path, func(attrs *[]core.Element) { *attrs = append(*attrs, core.NewRawElement(tag, core.VRUS, raw)) })
		if big {
			source.SetValueByteOrder(binary.BigEndian)
		}
		before := append([]core.Element(nil), source.Elements()...)
		read, err := ReadMeasurementReportWithContext(source, MeasurementContextOptions{})
		if err != nil {
			t.Fatal(err)
		}
		validation, err := read.Validation()
		if err != nil {
			t.Fatal(err)
		}
		seen := false
		for _, f := range validation.Unsupported {
			if f.Code == MeasurementContextOpaqueValues && f.Path == path {
				seen = true
			}
		}
		if !seen {
			t.Fatal("opaque byte limitation not disclosed")
		}
		result, err := read.Dataset()
		if big {
			if result != nil || !errors.Is(err, ErrMeasurementContext) {
				t.Fatalf("big endian opaque output not refused: %v", err)
			}
			if file, err := read.File(); file != nil || !errors.Is(err, ErrMeasurementContext) {
				t.Fatal("File bypassed opaque output refusal")
			}
			evidence, err := read.Evidence()
			if err != nil {
				t.Fatal(err)
			}
			rebound, err := BindMeasurementReportWithContext(read.Document(), read.MetadataSnapshot(), evidence, MeasurementContextOptions{ValueElementsBigEndian: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rebound.Dataset(); !errors.Is(err, ErrMeasurementContext) {
				t.Fatal("explicit Bind lost source endian contract")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			e, present := contextTestItem1508(t, result, path).Get(tag)
			if !present || !bytes.Equal(e.Value.(core.RawValue), raw) {
				t.Fatal("opaque LE bytes changed")
			}
		}
		if !reflect.DeepEqual(before, source.Elements()) {
			t.Fatal("opaque output changed source")
		}
		// Source raw values remain in the canonical document even when output refuses.
		w := newReferenceWalker(read.Document().Content, ContentItemIdentifier{1}, 1)
		retained := false
		for {
			entry, ok := w.next()
			if !ok {
				break
			}
			if entry.path.String() == path {
				for _, e := range entry.item.ValueElements {
					if e.Tag() == tag {
						retained = bytes.Equal(e.Value.(core.RawValue), raw)
					}
				}
			}
			w.push(entry.item.Children, entry.path, entry.depth+1)
		}
		if !retained {
			t.Fatal("opaque raw values lost in canonical tree")
		}
	}
}

func TestMeasurementContext1508NestedSOPOptionalRefusal(t *testing.T) {
	r := contextTestBuild1508(t, false)
	source, err := r.Dataset()
	if err != nil {
		t.Fatal(err)
	}
	c, _ := r.Context()
	mp := c.Groups[0].Measurements[0].Path
	items, _ := contextTestItem1508(t, source, mp).GetSequence(tagContentSequence)
	path := ""
	for i, it := range items {
		v, _ := readCode(it, tagConceptNameCodeSeq)
		if v.CodeValue == sourceOfMeasurementConcept.CodeValue {
			path = mp + "." + strconv.Itoa(i+1)
		}
	}
	if path == "" {
		t.Fatal("missing IMAGE fixture")
	}
	for _, tag := range []core.Tag{core.NewTag(0x0008, 0x1199), core.NewTag(0x0008, 0x114B), core.NewTag(0x0088, 0x0200)} {
		bad := contextTestMutate1508(t, source, path, func(attrs *[]core.Element) {
			idx := elementIndex(*attrs, tagRefSOPSequence)
			sq := (*attrs)[idx].Value.(core.SequenceValue)
			sq.Items = append([]core.DataSet(nil), sq.Items...)
			fields := append([]core.Element(nil), sq.Items[0].Elements...)
			fields = append(fields, seqElement(tag, core.DataSet{Elements: []core.Element{strElem(tagRefSOPClassUID, core.VRUI, "1.2.3"), strElem(tagRefSOPInstanceUID, core.VRUI, "1.2.3.4")}}))
			sq.Items[0].Elements = fields
			(*attrs)[idx].Value = sq
		})
		before := append([]core.Element(nil), bad.Elements()...)
		if got, err := ReadMeasurementReportWithContext(bad, MeasurementContextOptions{}); got != nil || !errors.Is(err, ErrMeasurementContext) {
			t.Fatalf("unmodeled macro silently admitted: %v", err)
		}
		if !reflect.DeepEqual(before, bad.Elements()) {
			t.Fatal("source optional macro changed on refusal")
		}
	}
}
