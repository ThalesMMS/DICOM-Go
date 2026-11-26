package sr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/codes"
	"github.com/ThalesMMS/dicom-go/object"
)

func catalogFixtureReport(groups ...MeasurementGroup) *MeasurementReport {
	return &MeasurementReport{
		SOPClassUID:    Comprehensive3DSRStorage,
		SOPInstanceUID: "1.2.826.0.1.3680043.9.7433.1265.1",
		ContentDate:    "20261007",
		ContentTime:    "120000",
		Groups:         groups,
	}
}

func catalogFixtureImage(instance string) ImageReference {
	return ImageReference{
		StudyInstanceUID: "1.2.826.0.1.3680043.9.7433.1265.6", SeriesInstanceUID: "1.2.826.0.1.3680043.9.7433.1265.8",
		SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "1.2.826.0.1.3680043.9.7433.1265." + instance,
	}
}

func catalogFixtureMeasurement(derivation CodedEntry) ReportMeasurement {
	return ReportMeasurement{
		ConceptName: CodedEntry{CodeValue: "121206", CodingSchemeDesignator: "DCM", CodeMeaning: "Distance"},
		Value:       12, Units: MillimeterUnit(), Derivation: derivation,
		Image:   catalogFixtureImage("4"),
		Spatial: SpatialReference{FrameOfReferenceUID: "1.2.826.0.1.3680043.9.7433.1265.5", GraphicType: GraphicTypePoint3D, Coordinates: []Point3D{{X: 1, Y: 2, Z: 3}}},
	}
}

func catalogFixtureGroup1501() MeasurementGroup {
	return MeasurementGroup{
		Tracking:     TrackingIdentifier{Identifier: "lesion", UID: "1.2.826.0.1.3680043.9.7433.1265.2"},
		Finding:      CodedEntry{CodeValue: "52988006", CodingSchemeDesignator: "SCT", CodeMeaning: "Lesion"},
		FindingSites: []CodedEntry{{CodeValue: "39607008", CodingSchemeDesignator: "SCT", CodeMeaning: "Lung"}},
		Measurements: []ReportMeasurement{
			catalogFixtureMeasurement(CodedEntry{}),
			catalogFixtureMeasurement(CodedEntry{CodeValue: "255560000", CodingSchemeDesignator: "SCT", CodeMeaning: "Mean"}),
		},
	}
}

func catalogFixtureGroup1411() MeasurementGroup {
	return MeasurementGroup{
		Tracking: TrackingIdentifier{Identifier: "segment", UID: "1.2.826.0.1.3680043.9.7433.1265.9"},
		ReferencedSegment: SegmentReference{
			StudyInstanceUID: "1.2.826.0.1.3680043.9.7433.1265.6", SeriesInstanceUID: "1.2.826.0.1.3680043.9.7433.1265.7",
			SOPClassUID: "1.2.840.10008.5.1.4.1.1.66.4", SOPInstanceUID: "1.2.826.0.1.3680043.9.7433.1265.3", SegmentNumber: 1,
		},
		SourceSeriesUID: "1.2.826.0.1.3680043.9.7433.1265.8",
		Measurements:    []ReportMeasurement{{ConceptName: CodedEntry{CodeValue: "118565006", CodingSchemeDesignator: "SCT", CodeMeaning: "Volume"}, Value: 3, Units: CodedEntry{CodeValue: "mm3", CodingSchemeDesignator: "UCUM", CodeMeaning: "cubic millimeter"}}},
	}
}

func writtenCatalogDocument(t *testing.T, report *MeasurementReport) *Document {
	t.Helper()
	file, err := WriteMeasurementReport(report)
	if err != nil {
		t.Fatalf("WriteMeasurementReport: %v", err)
	}
	document, err := ReadDocument(file.Dataset)
	if err != nil {
		t.Fatalf("ReadDocument: %v", err)
	}
	return document
}

func readCatalogDocument(t *testing.T, path string) *Document {
	t.Helper()
	file, err := object.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	document, err := ReadDocument(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func validationErrors(report ValidationReport) []string {
	var out []string
	for _, finding := range report.Findings {
		if finding.Severity == DiagnosticError {
			out = append(out, finding.Code+"@"+finding.Path.String()+" "+finding.RuleID)
		}
	}
	return out
}

// TestStandardTID1500ValidatesTheDocumentsOfTheCurrentDefinition is the
// acceptance check of the catalog: the generated TID 1500 accepts and
// rejects the same documents as CurrentMeasurementReportTemplate, the
// structure WriteMeasurementReport writes, and also accepts the reports
// written by DCMTK's and dcmqi's TID 1500 implementations.
func TestStandardTID1500ValidatesTheDocumentsOfTheCurrentDefinition(t *testing.T) {
	current := CurrentMeasurementReportTemplate()
	currentRegistry, err := NewTemplateRegistry([]TemplateDefinition{current}, nil)
	if err != nil {
		t.Fatal(err)
	}
	standardRegistry, err := NewStandardTemplateRegistry(1500)
	if err != nil {
		t.Fatalf("NewStandardTemplateRegistry(1500): %v", err)
	}
	tid1500, _ := StandardTemplateKey(1500)
	options := DefaultTemplateValidationOptions()
	currentValidator, err := NewTemplateValidator(currentRegistry, options)
	if err != nil {
		t.Fatal(err)
	}
	standardValidator, err := NewTemplateValidator(standardRegistry, options)
	if err != nil {
		t.Fatal(err)
	}
	personObserved := catalogFixtureReport(catalogFixtureGroup1501(), catalogFixtureGroup1411())
	personObserved.Observers = []Observer{{PersonName: "Reader^Synthetic", OrganizationName: "Synthetic"}, DefaultObserver()}
	personObserved.ImageLibrary = []ImageReference{catalogFixtureImage("4"), catalogFixtureImage("10")}
	withoutDerivation := catalogFixtureGroup1501()
	withoutDerivation.Measurements = withoutDerivation.Measurements[:1]
	onlyLanguage := writtenCatalogDocument(t, catalogFixtureReport())
	onlyLanguage.Content = onlyLanguage.Content[:1]
	tests := []struct {
		name     string
		document *Document
		valid    bool
		// currentRejects marks a valid document that the current
		// definition, a non-extensible description of an older writer
		// output, rejects: it describes neither the Derivation modifier,
		// nor a person observer before the device observer, nor the rows
		// other implementations write.
		currentRejects bool
	}{
		{name: "report without groups", document: writtenCatalogDocument(t, catalogFixtureReport()), valid: true},
		{name: "TID 1501 group", document: writtenCatalogDocument(t, catalogFixtureReport(withoutDerivation)), valid: true},
		{name: "TID 1411 group", document: writtenCatalogDocument(t, catalogFixtureReport(catalogFixtureGroup1411())), valid: true},
		{name: "three TID 1501 groups and a TID 1411 group", document: writtenCatalogDocument(t, catalogFixtureReport(withoutDerivation, withoutDerivation, catalogFixtureGroup1411(), withoutDerivation)), valid: true},
		{name: "TID 1501 group with derivation", document: writtenCatalogDocument(t, catalogFixtureReport(catalogFixtureGroup1501())), valid: true, currentRejects: true},
		{name: "both groups, person and device observers, image library", document: writtenCatalogDocument(t, personObserved), valid: true, currentRejects: true},
		{name: "DCMTK report", document: readCatalogDocument(t, filepath.Join("testdata", "dcmtk_tid1500.dcm")), valid: true},
		{name: "dcmqi report", document: readCatalogDocument(t, filepath.Join("..", "testdata", "upstream", "dcmqi", "tid1500.dcm")), valid: true, currentRejects: true},
		{name: "empty document", document: &Document{}, valid: false},
		{name: "language only", document: onlyLanguage, valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			currentReport, err := currentValidator.Validate(context.Background(), test.document, current.Key)
			if err != nil && !errors.Is(err, ErrTemplateValidation) {
				t.Fatal(err)
			}
			standardReport, err := standardValidator.Validate(context.Background(), test.document, tid1500)
			if err != nil && !errors.Is(err, ErrTemplateValidation) {
				t.Fatal(err)
			}
			wantCurrent := test.valid && !test.currentRejects
			if got := len(validationErrors(currentReport)) == 0; got != wantCurrent {
				t.Fatalf("current definition valid = %t, want %t: %v", got, wantCurrent, validationErrors(currentReport))
			}
			if got := len(validationErrors(standardReport)) == 0; got != test.valid {
				t.Fatalf("TID 1500 valid = %t, want %t: %v", got, test.valid, validationErrors(standardReport))
			}
			if test.valid && !strings.HasSuffix(test.name, "report") && len(standardReport.Findings) != 0 {
				t.Fatalf("TID 1500 findings for a report written here: %+v", standardReport.Findings)
			}
		})
	}
}

func TestStandardTemplateCatalogBuildsAndDescribesItself(t *testing.T) {
	ids := StandardTemplateIDs()
	if len(ids) == 0 || StandardTemplateEdition == "" || StandardTemplateCatalogSHA256 == "" {
		t.Fatalf("catalog %v, edition %q", ids, StandardTemplateEdition)
	}
	// Every numeric definition builds without transitive row expansion.
	for _, id := range ids {
		key, ok := StandardTemplateKey(id)
		if !ok || key.MappingResource != "DCMR" || key.Version != StandardTemplateEdition {
			t.Fatalf("StandardTemplateKey(%d) = %+v, %t", id, key, ok)
		}
		registry, err := NewStandardTemplateRegistry(id)
		if err != nil {
			t.Fatalf("NewStandardTemplateRegistry(%d): %v", id, err)
		}
		definition, ok := registry.LookupTemplate(key)
		if !ok || definition.Provenance.Checksum != StandardTemplateCatalogSHA256 || !strings.Contains(definition.Provenance.Source, "TID") {
			t.Fatalf("TID %d definition provenance %+v", id, definition.Provenance)
		}
		info, _ := LookupStandardTemplate(id)
		if info.EvaluatedConditions > info.ConditionalRows {
			t.Fatalf("TID %d: %+v", id, info)
		}
	}
	for _, id := range []int{4004, 4006, 4102, 4125} {
		if info, _ := LookupStandardTemplate(id); len(info.RecursiveIncludeRows) != 1 {
			t.Fatalf("TID %d recursive include rows %v, want one", id, info.RecursiveIncludeRows)
		}
	}
	registry, err := NewStandardTemplateRegistry(1500)
	if err != nil {
		t.Fatal(err)
	}
	info, ok := LookupStandardTemplate(1500)
	if !ok || info.Name != "Measurement Report" || info.Root != "Yes" || !info.Extensible || info.EvaluatedConditions != 3 {
		t.Fatalf("TID 1500 = %+v", info)
	}
	tid1500, _ := StandardTemplateKey(1500)
	definition, _ := registry.LookupTemplate(tid1500)
	if len(definition.Rows) == 0 || definition.Rows[0].RuleID != "TID1500:2" {
		t.Fatalf("TID 1500 rows start with %+v; want the children of the root CONTAINER", definition.Rows[0])
	}
	if _, ok := StandardTemplateKey(99999); ok {
		t.Fatal("StandardTemplateKey of an unknown TID")
	}
	if _, _, err := StandardTemplates(99999); err == nil {
		t.Fatal("StandardTemplates of an unknown TID")
	}
}

// TestStandardTemplateConceptCodesMatchTheTerminology checks every EV/DT
// concept name of the catalog against the PS3.16 terminology of the codes
// package: same code, and same meaning up to case. PS3.16 itself is not
// consistent here, so the template rows that differ in the catalog edition
// are listed in testdata/standard_template_meaning_discrepancies.tsv; the
// list must match exactly, so an extraction error or a new edition shows.
func TestStandardTemplateConceptCodesMatchTheTerminology(t *testing.T) {
	known := map[string]bool{}
	data, err := os.ReadFile(filepath.Join("testdata", "standard_template_meaning_discrepancies.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			known[line] = true
		}
	}
	for _, template := range standardTemplateCatalog {
		for _, row := range template.rows {
			if row.concept != "EV" && row.concept != "DT" {
				continue
			}
			terminology := "-"
			if term, ok := codes.Lookup(row.code.Scheme, row.code.Value); ok {
				if strings.EqualFold(term.Meaning, row.code.Meaning) {
					continue
				}
				terminology = term.Meaning
			}
			line := strings.Join([]string{strconv.Itoa(template.id), row.label, row.code.Scheme, row.code.Value, row.code.Meaning, terminology}, "\t")
			if !known[line] {
				t.Errorf("TID %d row %s: %s, terminology meaning %q, not a listed discrepancy", template.id, row.label, row.code, terminology)
			}
			delete(known, line)
		}
	}
	for line := range known {
		t.Errorf("listed discrepancy not in the catalog: %q", line)
	}
}

func TestStandardTemplateConditionsUseSiblingRowPresence(t *testing.T) {
	condition := catalogConditionFunc("1500", "6", &catalogCondition{op: "and", args: []*catalogCondition{
		{op: "absent", rows: []string{"10"}}, {op: "absent", rows: []string{"12"}},
	}})
	tests := []struct {
		name   string
		ruleID string
		counts map[string]int
		want   bool
		err    bool
	}{
		{name: "siblings absent", ruleID: "TID1500:6", counts: map[string]int{"TID1500:6": 0}, want: true},
		{name: "sibling present", ruleID: "TID1500:6", counts: map[string]int{"TID1500:10": 1}, want: false},
		{name: "included scope", ruleID: "TID9:1.TID1500:6", counts: map[string]int{"TID9:1.TID1500:12": 2, "TID1500:10": 0}, want: false},
		{name: "other scope ignored", ruleID: "TID9:1.TID1500:6", counts: map[string]int{"TID1500:12": 2}, want: true},
		{name: "rows of an included sibling", ruleID: "TID1500:6", counts: map[string]int{"TID1500:12.TID4019:1": 1}, want: false},
		{name: "another row", ruleID: "TID1500:7", err: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := condition(context.Background(), TemplateConditionContext{RuleID: test.ruleID, RowCounts: test.counts})
			if (err != nil) != test.err || got != test.want {
				t.Fatalf("condition = %t, %v; want %t, error %t", got, err, test.want, test.err)
			}
		})
	}
}

func TestIncludeFillsRelationshipKeepsIncludedRelationships(t *testing.T) {
	base := TemplateKey{MappingResource: "TEST", Identifier: "base", Version: "1"}
	including := TemplateKey{MappingResource: "TEST", Identifier: "including", Version: "1"}
	registry, err := NewTemplateRegistry([]TemplateDefinition{
		{Key: base, Rows: []TemplateRow{
			{RuleID: "modifier", RelationshipTypes: []string{RelationshipHasConceptMod}, ValueTypes: []ValueType{ValueCode}, Requirement: RequirementOptional},
			{RuleID: "value", ValueTypes: []ValueType{ValueNum}, ReferenceMode: ReferenceModeByValue, Requirement: RequirementOptional},
		}},
		{Key: including, Rows: []TemplateRow{{
			RuleID: "include", Include: &base, IncludeFillsRelationship: true,
			RelationshipTypes: []string{RelationshipContains}, ReferenceMode: ReferenceModeByValue,
		}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	validator, err := NewTemplateValidator(registry, TemplateValidationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	document := &Document{Content: []ContentItem{
		{RelationshipType: RelationshipHasConceptMod, ValueType: ValueCode},
		{RelationshipType: RelationshipContains, ValueType: ValueNum},
	}}
	if report, err := validator.Validate(context.Background(), document, including); err != nil || len(report.Findings) != 0 {
		t.Fatalf("Validate = %+v, %v; want clean", report, err)
	}
	document.Content[0].RelationshipType = RelationshipContains
	if report, _ := validator.Validate(context.Background(), document, including); len(report.Findings) == 0 {
		t.Fatal("the include's relationship replaced the included row's own")
	}
}

// TestTemplateValidatorTellsEqualRowsApartByTheirChildren: two rows that
// describe a CONTAINER equally are told apart by their child rows.
func TestTemplateValidatorTellsEqualRowsApartByTheirChildren(t *testing.T) {
	key := TemplateKey{MappingResource: "TEST", Identifier: "groups", Version: "1"}
	group := func(rule string, required ValueType) TemplateRow {
		return TemplateRow{
			RuleID: rule, RelationshipTypes: []string{RelationshipContains}, ValueTypes: []ValueType{ValueContainer},
			Requirement: RequirementOptional, Cardinality: Cardinality{Min: 1},
			Children: []TemplateRow{
				{RuleID: rule + "-text", RelationshipTypes: []string{RelationshipContains}, ValueTypes: []ValueType{ValueText}, Requirement: RequirementOptional},
				{RuleID: rule + "-required", RelationshipTypes: []string{RelationshipContains}, ValueTypes: []ValueType{required}, Requirement: RequirementRequired},
			},
		}
	}
	registry, err := NewTemplateRegistry([]TemplateDefinition{{Key: key, Rows: []TemplateRow{group("a", ValueCode), group("b", ValueNum)}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	validator, err := NewTemplateValidator(registry, TemplateValidationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, valueType := range []ValueType{ValueCode, ValueNum} {
		document := &Document{Content: []ContentItem{{RelationshipType: RelationshipContains, ValueType: ValueContainer, Children: []ContentItem{
			{RelationshipType: RelationshipContains, ValueType: ValueText},
			{RelationshipType: RelationshipContains, ValueType: valueType},
		}}}}
		if report, err := validator.Validate(context.Background(), document, key); err != nil || len(report.Findings) != 0 {
			t.Fatalf("group with %s = %+v, %v; want clean", valueType, report, err)
		}
	}
}
