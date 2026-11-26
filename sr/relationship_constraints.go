package sr

import (
	"errors"
	"fmt"
)

//go:generate go run ../cmd/srrelgen -input ../internal/standard/sr_relationships.tsv -output relationship_constraints_gen.go

// relationshipTerm matches value types or relationship types: a list, any, or
// any except one.
type relationshipTerm struct {
	any           bool
	except        string
	valueTypes    []ValueType
	relationships []string
}

func (term relationshipTerm) matchesValueType(value ValueType) bool {
	if term.any {
		return string(value) != term.except
	}
	for _, candidate := range term.valueTypes {
		if candidate == value {
			return true
		}
	}
	return false
}

func (term relationshipTerm) matchesRelationship(relationship string) bool {
	if term.any {
		return relationship != term.except
	}
	for _, candidate := range term.relationships {
		if candidate == relationship {
			return true
		}
	}
	return false
}

// relationshipRule is one row of a Table A.35.x-2.
type relationshipRule struct {
	sources      relationshipTerm
	relationship relationshipTerm
	targets      relationshipTerm
}

// relationshipIOD is the relationship content constraints of one SR IOD.
type relationshipIOD struct {
	sopClassUID string
	name        string
	table       string
	// byReference is "none", "all", "all-except" or "only", qualified by
	// byReferenceRelationships.
	byReference              string
	byReferenceRelationships []string
	rules                    []relationshipRule
}

var relationshipIODsByClass = func() map[string]*relationshipIOD {
	out := make(map[string]*relationshipIOD, len(relationshipIODs))
	for index := range relationshipIODs {
		out[relationshipIODs[index].sopClassUID] = &relationshipIODs[index]
	}
	return out
}()

func lookupRelationshipIOD(sopClassUID string) *relationshipIOD {
	return relationshipIODsByClass[clean(sopClassUID)]
}

func (iod *relationshipIOD) allows(source ValueType, relationship string, target ValueType) bool {
	for _, rule := range iod.rules {
		if rule.sources.matchesValueType(source) && rule.relationship.matchesRelationship(relationship) && rule.targets.matchesValueType(target) {
			return true
		}
	}
	return false
}

// allowsByReference reports whether relationship may be conveyed by-reference.
func (iod *relationshipIOD) allowsByReference(relationship string) bool {
	listed := false
	for _, candidate := range iod.byReferenceRelationships {
		listed = listed || candidate == relationship
	}
	switch iod.byReference {
	case "all":
		return true
	case "all-except":
		return !listed
	case "only":
		return listed
	default:
		return false
	}
}

func (iod *relationshipIOD) ruleID() string {
	return "PS3.3-" + RelationshipConstraintsEdition + ".Table." + iod.table
}

// RelationshipConstraint identifies the PS3.3 relationship content
// constraints that apply to an SR SOP Class.
type RelationshipConstraint struct {
	SOPClassUID string
	IODName     string
	// Table is the PS3.3 table, such as "A.35.3-2".
	Table   string
	Edition string
}

// RelationshipConstraintFor returns the constraints PS3.3 defines for an SR
// SOP Class; ok is false for SOP Classes without an A.35 table.
func RelationshipConstraintFor(sopClassUID string) (RelationshipConstraint, bool) {
	iod := lookupRelationshipIOD(sopClassUID)
	if iod == nil {
		return RelationshipConstraint{}, false
	}
	return RelationshipConstraint{SOPClassUID: iod.sopClassUID, IODName: iod.name, Table: iod.table, Edition: RelationshipConstraintsEdition}, true
}

// RelationshipAllowed reports whether the IOD of sopClassUID allows a source
// Content Item of one value type to have a target of another with
// relationship, conveyed by-value or by-reference (PS3.3 Tables A.35.x-2 and
// the IOD's relationship constraints). known is false for SOP Classes without
// an A.35 table, which are not constrained.
func RelationshipAllowed(sopClassUID string, source ValueType, relationship string, target ValueType, byReference bool) (allowed, known bool) {
	iod := lookupRelationshipIOD(sopClassUID)
	if iod == nil {
		return false, false
	}
	if byReference && !iod.allowsByReference(relationship) {
		return false, true
	}
	return iod.allows(source, relationship, target), true
}

var (
	// ErrRelationshipConstraint reports a content tree that violates the
	// relationship content constraints of its SR IOD.
	ErrRelationshipConstraint = errors.New("dicom/sr: relationship content constraint violated")
	// ErrUnconstrainedSOPClass reports a SOP Class with no PS3.3 A.35
	// relationship table, which ValidateRelationships cannot check.
	ErrUnconstrainedSOPClass = errors.New("dicom/sr: no relationship content constraints for SOP Class")
)

const (
	// RelationshipCodeForbidden: the source/relationship/target combination
	// is not in the IOD's table.
	RelationshipCodeForbidden = "sr.relationship.forbidden"
	// RelationshipCodeMissing: a non-root Content Item has no Relationship Type.
	RelationshipCodeMissing = "sr.relationship.missing"
	// RelationshipCodeUnknown: the Relationship Type is not a defined term.
	RelationshipCodeUnknown = "sr.relationship.unknown"
	// RelationshipCodeUnknownValueType: the Value Type is not a defined term
	// of this edition. It is always a warning, since a later edition may
	// define it.
	RelationshipCodeUnknownValueType = "sr.relationship.unknown_value_type"
	// RelationshipCodeResourceLimit: the tree exceeds the validation limits.
	RelationshipCodeResourceLimit = "sr.relationship.resource_limit"
)

// RelationshipOptions bounds ValidateRelationships and selects strict or
// warn mode.
type RelationshipOptions struct {
	Mode        ValidationMode
	MaxDepth    int
	MaxItems    int
	MaxFindings int
}

// DefaultRelationshipOptions returns bounded strict validation defaults.
func DefaultRelationshipOptions() RelationshipOptions {
	return RelationshipOptions{Mode: ValidationModeStrict, MaxDepth: maxContentTreeDepth, MaxItems: 100_000, MaxFindings: 1_024}
}

var knownValueTypes = map[ValueType]bool{
	ValueContainer: true, ValueText: true, ValueCode: true, ValueImage: true, ValueNum: true,
	ValueDateTime: true, ValueDate: true, ValueTime: true, ValueUIDRef: true, ValuePName: true,
	ValueComposite: true, ValueWaveform: true, ValueSCoord: true, ValueSCoord3D: true, ValueTCoord: true,
	ValueTable: true,
}

var knownRelationships = map[string]bool{
	RelationshipContains: true, RelationshipHasObsContext: true, RelationshipHasAcqContext: true,
	RelationshipHasConceptMod: true, RelationshipHasProperties: true, RelationshipInferredFrom: true,
	RelationshipSelectedFrom: true,
}

// ValidateRelationships checks every by-value relationship of the content tree
// against the relationship content constraints of the document's SR IOD
// (PS3.3 Tables A.35.x-2). The root is the CONTAINER source of the top-level
// items. By-reference relationships are checked by ResolveReferences, which
// applies the same tables. Findings name the item path, the value types, the
// relationship and the table, and never include values.
//
// A SOP Class without an A.35 table returns ErrUnconstrainedSOPClass. In
// strict mode a violation also returns an error wrapping
// ErrRelationshipConstraint.
func ValidateRelationships(document *Document, options RelationshipOptions) (ValidationReport, error) {
	defaults := DefaultRelationshipOptions()
	if options.Mode == 0 {
		options.Mode = defaults.Mode
	}
	if options.MaxDepth == 0 {
		options.MaxDepth = defaults.MaxDepth
	}
	if options.MaxItems == 0 {
		options.MaxItems = defaults.MaxItems
	}
	if options.MaxFindings == 0 {
		options.MaxFindings = defaults.MaxFindings
	}
	if (options.Mode != ValidationModeStrict && options.Mode != ValidationModeWarn) || options.MaxDepth < 1 || options.MaxItems < 1 || options.MaxFindings < 1 {
		return ValidationReport{}, fmt.Errorf("%w: invalid options", ErrRelationshipConstraint)
	}
	if document == nil {
		return ValidationReport{}, fmt.Errorf("%w: document is nil", ErrInvalidDocument)
	}
	iod := lookupRelationshipIOD(document.SOPClassUID)
	if iod == nil {
		return ValidationReport{}, fmt.Errorf("%w: %q", ErrUnconstrainedSOPClass, clean(document.SOPClassUID))
	}
	var report ValidationReport
	add := func(code string, path ContentItemIdentifier, source ValueType, relationship string, target ValueType, message string) {
		if len(report.Findings) >= options.MaxFindings {
			report.Truncated = true
			return
		}
		severity := DiagnosticError
		if (options.Mode == ValidationModeWarn && code != RelationshipCodeResourceLimit) || code == RelationshipCodeUnknownValueType {
			severity = DiagnosticWarning
		}
		report.Findings = append(report.Findings, DiagnosticFinding{
			Path: path.Clone(), RuleID: iod.ruleID(), Code: code, Severity: severity, Message: message,
			SOPClassUID: iod.sopClassUID, SourceValueType: source, RelationshipType: relationship, TargetValueType: target,
		})
	}
	items := 0
	var walk func(parent ContentItemIdentifier, source ValueType, children []ContentItem, depth int) bool
	walk = func(parent ContentItemIdentifier, source ValueType, children []ContentItem, depth int) bool {
		if depth > options.MaxDepth {
			add(RelationshipCodeResourceLimit, parent, source, "", "", "content tree exceeds the validation depth limit")
			return false
		}
		for index := range children {
			item := &children[index]
			items++
			path := appendPath(parent, uint32(index+1))
			if items > options.MaxItems {
				add(RelationshipCodeResourceLimit, path, source, "", "", "content tree exceeds the validation item limit")
				return false
			}
			if len(item.ReferencedContentItemIdentifier) > 0 {
				continue // by-reference: checked by ResolveReferences
			}
			relationship := clean(item.RelationshipType)
			switch {
			case !knownValueTypes[item.ValueType]:
				add(RelationshipCodeUnknownValueType, path, source, relationship, item.ValueType, "content item value type is not a defined term")
			case relationship == "":
				add(RelationshipCodeMissing, path, source, "", item.ValueType, "content item has no relationship type")
			case !knownRelationships[relationship]:
				add(RelationshipCodeUnknown, path, source, relationship, item.ValueType, "relationship type is not a defined term")
			case !iod.allows(source, relationship, item.ValueType):
				add(RelationshipCodeForbidden, path, source, relationship, item.ValueType, "relationship is not allowed between these value types by the IOD")
			}
			if !walk(path, item.ValueType, item.Children, depth+1) {
				return false
			}
		}
		return true
	}
	walk(ContentItemIdentifier{1}, ValueContainer, document.Content, 1)
	if options.Mode == ValidationModeStrict && report.HasErrors() {
		finding := report.Findings[0]
		return report, fmt.Errorf("%w: %s at %s (%s %s %s, %s)", ErrRelationshipConstraint, finding.Code, finding.Path,
			finding.SourceValueType, finding.RelationshipType, finding.TargetValueType, iod.name)
	}
	return report, nil
}
