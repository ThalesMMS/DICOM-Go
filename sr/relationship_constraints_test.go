package sr

import (
	"errors"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/uid"
)

func TestRelationshipTablesCoverEverySRIOD(t *testing.T) {
	if len(relationshipIODs) != 23 {
		t.Fatalf("%d relationship tables, want the 23 SR IODs of PS3.3 A.35", len(relationshipIODs))
	}
	for _, iod := range relationshipIODs {
		entry, ok := uid.Dictionary.ByUID(iod.sopClassUID)
		if !ok || entry.Type != uid.SOPClass {
			t.Errorf("%s: %s is not a SOP Class", iod.name, iod.sopClassUID)
		}
		constraint, ok := RelationshipConstraintFor(" " + iod.sopClassUID + "\x00")
		if !ok || constraint.Table != iod.table || constraint.Edition != RelationshipConstraintsEdition || constraint.IODName != iod.name {
			t.Errorf("RelationshipConstraintFor(%s) = %+v, %v", iod.sopClassUID, constraint, ok)
		}
	}
	if _, ok := RelationshipConstraintFor("1.2.840.10008.5.1.4.1.1.2"); ok {
		t.Fatal("CT Image Storage has relationship constraints")
	}
}

func TestRelationshipAllowedFollowsTables(t *testing.T) {
	const (
		procedureLog      = "1.2.840.10008.5.1.4.1.1.88.40"
		mammographyCAD    = "1.2.840.10008.5.1.4.1.1.88.50"
		waveformAnnotated = "1.2.840.10008.5.1.4.1.1.88.77"
	)
	tests := []struct {
		name         string
		sopClassUID  string
		source       ValueType
		relationship string
		target       ValueType
		byReference  bool
		want         bool
	}{
		{"Basic Text has no NUM", BasicTextSRStorage, ValueContainer, RelationshipContains, ValueNum, false, false},
		{"Enhanced SR has NUM", EnhancedSRStorage, ValueContainer, RelationshipContains, ValueNum, false, true},
		{"Comprehensive SR has no SCOORD3D", ComprehensiveSRStorage, ValueContainer, RelationshipContains, ValueSCoord3D, false, false},
		{"Comprehensive 3D SR has SCOORD3D", Comprehensive3DSRStorage, ValueContainer, RelationshipContains, ValueSCoord3D, false, true},
		{"any type HAS CONCEPT MOD CODE", ComprehensiveSRStorage, ValueSCoord, RelationshipHasConceptMod, ValueCode, false, true},
		{"CONCEPT MOD target is TEXT or CODE", ComprehensiveSRStorage, ValueText, RelationshipHasConceptMod, ValueNum, false, false},
		{"KOS concept modifier from CONTAINER only", KeyObjectSelectionDocumentStorage, ValueText, RelationshipHasConceptMod, ValueCode, false, false},
		{"KOS image acquisition context", KeyObjectSelectionDocumentStorage, ValueImage, RelationshipHasAcqContext, ValueNum, false, true},
		{"Procedure Log properties not from CONTAINER", procedureLog, ValueContainer, RelationshipHasProperties, ValueText, false, false},
		{"Procedure Log properties from any other type", procedureLog, ValueImage, RelationshipHasProperties, ValueText, false, true},
		{"Extensible allows any non-CONTAINS", ExtensibleSRStorage, ValueImage, RelationshipHasObsContext, ValueTable, false, true},
		{"Extensible CONTAINS from CONTAINER only", ExtensibleSRStorage, ValueText, RelationshipContains, ValueText, false, false},
		{"Basic Text has no by-reference", BasicTextSRStorage, ValueText, RelationshipInferredFrom, ValueText, true, false},
		{"Comprehensive by-reference INFERRED FROM", ComprehensiveSRStorage, ValueText, RelationshipInferredFrom, ValueImage, true, true},
		{"Comprehensive no by-reference CONCEPT MOD", ComprehensiveSRStorage, ValueText, RelationshipHasConceptMod, ValueCode, true, false},
		{"Mammography CAD by-reference SELECTED FROM", mammographyCAD, ValueSCoord, RelationshipSelectedFrom, ValueImage, true, true},
		{"Mammography CAD no by-reference OBS CONTEXT", mammographyCAD, ValueCode, RelationshipHasObsContext, ValueText, true, false},
		{"Waveform Annotation by-reference SELECTED FROM", waveformAnnotated, ValueTCoord, RelationshipSelectedFrom, ValueWaveform, true, true},
		{"Waveform Annotation no by-reference PROPERTIES", waveformAnnotated, ValueCode, RelationshipHasProperties, ValueText, true, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, known := RelationshipAllowed(test.sopClassUID, test.source, test.relationship, test.target, test.byReference)
			if !known || got != test.want {
				t.Fatalf("RelationshipAllowed = %v (known %v), want %v", got, known, test.want)
			}
		})
	}
	if _, known := RelationshipAllowed("1.2.3", ValueText, RelationshipContains, ValueText, false); known {
		t.Fatal("unknown SOP Class reported as constrained")
	}
}

func relationshipItem(valueType ValueType, relationship string, children ...ContentItem) ContentItem {
	return ContentItem{ValueType: valueType, RelationshipType: relationship, Children: children}
}

func TestValidateRelationshipsReportsTypedFindings(t *testing.T) {
	document := &Document{
		SOPClassUID: BasicTextSRStorage,
		Content: []ContentItem{
			relationshipItem(ValueText, RelationshipContains,
				relationshipItem(ValueCode, RelationshipHasProperties),
				relationshipItem(ValueNum, RelationshipInferredFrom), // no NUM in Basic Text SR
			),
			relationshipItem(ValueCode, RelationshipContains,
				relationshipItem(ValueText, RelationshipInferredFrom), // CODE is no INFERRED FROM source
			),
			relationshipItem(ValueText, ""),
			relationshipItem(ValueText, "IS RELATED TO"),
			relationshipItem("FUTURE", RelationshipContains),
			{ReferencedContentItemIdentifier: ContentItemIdentifier{1, 1}, RelationshipType: RelationshipInferredFrom},
		},
	}
	report, err := ValidateRelationships(document, RelationshipOptions{Mode: ValidationModeWarn})
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		code, path                   string
		source, relationship, target string
		severity                     DiagnosticSeverity
	}
	want := []row{
		{RelationshipCodeForbidden, "1.1.2", "TEXT", RelationshipInferredFrom, "NUM", DiagnosticWarning},
		{RelationshipCodeForbidden, "1.2.1", "CODE", RelationshipInferredFrom, "TEXT", DiagnosticWarning},
		{RelationshipCodeMissing, "1.3", "CONTAINER", "", "TEXT", DiagnosticWarning},
		{RelationshipCodeUnknown, "1.4", "CONTAINER", "IS RELATED TO", "TEXT", DiagnosticWarning},
		{RelationshipCodeUnknownValueType, "1.5", "CONTAINER", RelationshipContains, "FUTURE", DiagnosticWarning},
	}
	if len(report.Findings) != len(want) {
		t.Fatalf("findings = %+v", report.Findings)
	}
	for i, w := range want {
		f := report.Findings[i]
		got := row{f.Code, f.Path.String(), string(f.SourceValueType), f.RelationshipType, string(f.TargetValueType), f.Severity}
		if got != w || f.SOPClassUID != BasicTextSRStorage || f.RuleID != "PS3.3-"+RelationshipConstraintsEdition+".Table.A.35.1-2" {
			t.Errorf("finding %d = %+v, want %+v", i, f, w)
		}
	}

	report, err = ValidateRelationships(document, DefaultRelationshipOptions())
	if !errors.Is(err, ErrRelationshipConstraint) || !report.HasErrors() {
		t.Fatalf("strict: err=%v", err)
	}
	if !strings.Contains(err.Error(), "1.1.2 (TEXT INFERRED FROM NUM, Basic Text SR)") {
		t.Fatalf("strict error %q lacks the first finding", err)
	}
	for _, finding := range report.Findings {
		if (finding.Code == RelationshipCodeUnknownValueType) != (finding.Severity == DiagnosticWarning) {
			t.Fatalf("strict severity of %s = %s", finding.Code, finding.Severity)
		}
	}

	report, _ = ValidateRelationships(document, RelationshipOptions{Mode: ValidationModeWarn, MaxFindings: 2})
	if len(report.Findings) != 2 || !report.Truncated {
		t.Fatalf("MaxFindings: %d findings, truncated %v", len(report.Findings), report.Truncated)
	}
	deep := relationshipItem(ValueText, RelationshipHasProperties)
	for i := 0; i < 4; i++ {
		deep = relationshipItem(ValueText, RelationshipHasProperties, deep)
	}
	deep.RelationshipType = RelationshipContains
	report, err = ValidateRelationships(&Document{SOPClassUID: BasicTextSRStorage, Content: []ContentItem{deep}}, RelationshipOptions{MaxDepth: 3})
	if !errors.Is(err, ErrRelationshipConstraint) || report.Findings[0].Code != RelationshipCodeResourceLimit {
		t.Fatalf("MaxDepth: err=%v findings=%+v", err, report.Findings)
	}

	if _, err := ValidateRelationships(&Document{SOPClassUID: "1.2.3"}, DefaultRelationshipOptions()); !errors.Is(err, ErrUnconstrainedSOPClass) {
		t.Fatalf("unknown SOP Class: %v", err)
	}
	if _, err := ValidateRelationships(nil, DefaultRelationshipOptions()); !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("nil document: %v", err)
	}
}

func TestDocumentWritingEnforcesRelationshipConstraints(t *testing.T) {
	document := &Document{
		SOPClassUID: BasicTextSRStorage, SOPInstanceUID: "1.2.826.0.1.3680043.10.543.999.20",
		Content: []ContentItem{{
			ValueType: ValueNum, RelationshipType: RelationshipContains,
			Measurement: &Measurement{Value: 1, Units: CodedEntry{CodeValue: "mm", CodingSchemeDesignator: "UCUM", CodeMeaning: "millimeter"}},
		}},
	}
	if _, err := document.Dataset(); !errors.Is(err, ErrInvalidDocument) || !errors.Is(err, ErrRelationshipConstraint) {
		t.Fatalf("Dataset() error = %v", err)
	}
	document.SOPClassUID = EnhancedSRStorage
	dataset, err := document.Dataset()
	if err != nil {
		t.Fatal(err)
	}

	// Reading reports what writing refuses, without failing by default.
	dataset.Put(strElem(tagSOPClassUID, core.VRUI, BasicTextSRStorage))
	options := DefaultReadOptions()
	options.ValidateRelationships = true
	result, err := ReadDocumentWithOptions(dataset, options)
	if err != nil || len(result.Report.Findings) != 1 || result.Report.Findings[0].Code != RelationshipCodeForbidden || result.Report.Findings[0].Severity != DiagnosticWarning {
		t.Fatalf("read: err=%v findings=%+v", err, result.Report.Findings)
	}
	options.RelationshipMode = ValidationModeStrict
	if _, err := ReadDocumentWithOptions(dataset, options); !errors.Is(err, ErrRelationshipConstraint) {
		t.Fatalf("strict read: %v", err)
	}
	dataset.Put(strElem(tagSOPClassUID, core.VRUI, "1.2.826.0.1.3680043.10.543.999.21"))
	if result, err := ReadDocumentWithOptions(dataset, options); err != nil || len(result.Report.Findings) != 0 {
		t.Fatalf("unconstrained SOP Class: err=%v findings=%+v", err, result.Report.Findings)
	}
}

// TestByReferenceRelationshipsUseTheTables: ResolveReferences applies the
// IOD's by-reference policy and table, with typed findings.
func TestByReferenceRelationshipsUseTheTables(t *testing.T) {
	const mammographyCAD = "1.2.840.10008.5.1.4.1.1.88.50"
	document := &Document{
		SOPClassUID: mammographyCAD,
		Content: []ContentItem{
			relationshipItem(ValueCode, RelationshipContains,
				ContentItem{ReferencedContentItemIdentifier: ContentItemIdentifier{1, 2}, RelationshipType: RelationshipInferredFrom},
				ContentItem{ReferencedContentItemIdentifier: ContentItemIdentifier{1, 3}, RelationshipType: RelationshipHasObsContext},
				ContentItem{ReferencedContentItemIdentifier: ContentItemIdentifier{1, 4}, RelationshipType: RelationshipInferredFrom},
			),
			relationshipItem(ValueNum, RelationshipContains),
			relationshipItem(ValueText, RelationshipContains),
			relationshipItem(ValueDate, RelationshipContains),
		},
	}
	options := DefaultReferenceOptions()
	options.Mode = ValidationModeWarn
	_, report, err := ResolveReferences(document, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 2 {
		t.Fatalf("findings = %+v", report.Findings)
	}
	forbidden, incompatible := report.Findings[0], report.Findings[1]
	if forbidden.Code != ReferenceCodeForbiddenRelation || forbidden.Path.String() != "1.1.2" || forbidden.RelationshipType != RelationshipHasObsContext {
		t.Fatalf("by-reference HAS OBS CONTEXT: %+v", forbidden)
	}
	if incompatible.Code != ReferenceCodeIncompatible || incompatible.SourceValueType != ValueCode || incompatible.TargetValueType != ValueDate ||
		incompatible.SOPClassUID != mammographyCAD || incompatible.RuleID != "PS3.3-"+RelationshipConstraintsEdition+".Table.A.35.5-2" {
		t.Fatalf("CODE INFERRED FROM DATE: %+v", incompatible)
	}
}

func TestKeyObjectSelectionFindingsMeetTheKOSTable(t *testing.T) {
	refs := []ImageReference{{StudyInstanceUID: "1.2.9102", SeriesInstanceUID: "1.2.9103", SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "1.2.9125.1"}}
	document := NewKeyObjectSelection("1.2.9125.9", refs, map[string]string{"1.2.9125.1": "finding"})
	if report, err := ValidateRelationships(document, DefaultRelationshipOptions()); err != nil || len(report.Findings) != 0 {
		t.Fatalf("KOS with findings: err=%v findings=%+v", err, report.Findings)
	}
	if child := document.Content[0].Children[0]; child.RelationshipType != RelationshipHasAcqContext {
		t.Fatalf("finding relationship %q", child.RelationshipType)
	}
	// Documents written before still yield their findings.
	document.Content[0].Children[0].RelationshipType = RelationshipContains
	if got := KeyObjectSelectionFindings(document)["1.2.9125.1"]; got != "finding" {
		t.Fatalf("legacy finding = %q", got)
	}
}
