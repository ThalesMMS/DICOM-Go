package sr

import (
	"context"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/codes/sct"
)

// TestStandardContextGroupsValidateCodes: a template row constrained to
// PS3.16 CID 244 (Laterality, non-extensible) accepts the members of the
// CID 247 it includes and refuses other codes; CID 7464 brings its CID 3488;
// a group defined by reference is refused.
func TestStandardContextGroupsValidateCodes(t *testing.T) {
	groups, err := StandardContextGroups(244, 7464)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, group := range groups {
		ids = append(ids, group.Key.Identifier)
	}
	if len(ids) != 4 || ids[0] != "247" || ids[1] != "244" || ids[2] != "3488" || ids[3] != "7464" {
		t.Fatalf("definitions %v, want the includes before their groups", ids)
	}
	laterality, _ := StandardContextGroupKey(244)
	if laterality != (ContextGroupKey{MappingResource: "DCMR", Identifier: "244", Version: "20030108"}) || groups[1].Extensible || !groups[3].Extensible {
		t.Fatalf("CID 244 key %+v, definitions %+v", laterality, groups)
	}
	key := TemplateKey{MappingResource: "TEST", Identifier: "laterality", Version: "1"}
	registry, err := NewTemplateRegistry([]TemplateDefinition{{Key: key, Rows: []TemplateRow{{
		RuleID: "laterality", RelationshipTypes: []string{RelationshipHasConceptMod}, ReferenceMode: ReferenceModeByValue,
		ValueTypes: []ValueType{ValueCode}, ContextGroup: &laterality, Cardinality: Cardinality{Min: 1, Max: 1}, Requirement: RequirementRequired,
	}}}}, groups)
	if err != nil {
		t.Fatal(err)
	}
	validator, err := NewTemplateValidator(registry, TemplateValidationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	document := func(code CodedEntry) *Document {
		return &Document{Content: []ContentItem{{
			RelationshipType: RelationshipHasConceptMod, ValueType: ValueCode,
			ConceptName: CodedEntry{CodeValue: "272741003", CodingSchemeDesignator: "SCT", CodeMeaning: "Laterality"}, Code: code,
		}}}
	}
	if report, err := validator.Validate(context.Background(), document(FromCode(sct.Right)), key); err != nil || len(report.Findings) != 0 {
		t.Fatalf("Right: %+v, %v", report, err)
	}
	if _, err := validator.Validate(context.Background(), document(FromCode(sct.FindingSite)), key); !errors.Is(err, ErrTemplateValidation) {
		t.Fatalf("a non-member of a non-extensible CID: %v", err)
	}
	if _, err := StandardContextGroups(82); !errors.Is(err, ErrContextGroupByReference) {
		t.Fatalf("CID 82: %v", err)
	}
	if _, err := StandardContextGroups(999999); err == nil {
		t.Fatal("unknown CID accepted")
	}
}
