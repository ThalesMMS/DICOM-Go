package sr

//go:generate go run ../cmd/srtemplategen -input ../internal/standard/sr_templates.tsv -output template_catalog_gen.go

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ThalesMMS/dicom-go/codes"
)

// StandardTemplateMappingResource is the Mapping Resource of the PS3.16
// templates.
const StandardTemplateMappingResource = "DCMR"

// catalogTemplate is one generated PS3.16 template (see cmd/srtemplategen).
type catalogTemplate struct {
	id         int
	identifier string // set for normative letter-suffixed identifiers
	name       string
	extensible bool
	order      string // "Significant", "Non-Significant" or "" (not recorded)
	root       string // "Yes", "No" or "" (not recorded)
	rows       []catalogRow
}

// catalogRow is one template table row, in table order.
type catalogRow struct {
	label             string
	level             int
	relationship      string
	byReference       bool
	valueType         ValueType
	include           int
	includeIdentifier string
	concept           string // "", "EV", "DT", "CID" or "$"
	code              codes.Code
	conceptCID        int
	conceptStrength   string // "D", "B" or "" (not recorded)
	parameter         string
	min, max          int
	requirement       string // M, MC, U or UC
	condition         string
	formal            string
	evaluate          *catalogCondition
	valueSet          string
	valueSetCID       int
	valueSetStrength  string
}

// catalogCondition is an evaluable formal condition: present, absent and
// xor over sibling row labels, combined with and, or and not.
type catalogCondition struct {
	op   string
	rows []string
	args []*catalogCondition
}

func (condition *catalogCondition) eval(count func(label string) int) bool {
	switch condition.op {
	case "present":
		for _, label := range condition.rows {
			if count(label) > 0 {
				return true
			}
		}
		return false
	case "absent", "xor":
		for _, label := range condition.rows {
			if count(label) > 0 {
				return false
			}
		}
		return true
	case "and":
		for _, arg := range condition.args {
			if !arg.eval(count) {
				return false
			}
		}
		return true
	case "or":
		for _, arg := range condition.args {
			if arg.eval(count) {
				return true
			}
		}
		return false
	case "not":
		return !condition.args[0].eval(count)
	}
	return false
}

// StandardTemplateInfo describes one template of the catalog.
type StandardTemplateInfo struct {
	ID         int // zero for an alphanumeric identifier
	Identifier string
	Name       string // empty when the curated catalog does not record it
	// Extensible, Order ("Significant", "Non-Significant" or "" when not
	// recorded) and Root ("Yes", "No" or "") are the template's attributes.
	Extensible bool
	Order      string
	Root       string
	Rows       int
	// ConditionalRows counts the MC and UC rows; EvaluatedConditions the MC
	// rows whose condition the validator evaluates. The other conditional
	// rows are validated as optional rows.
	ConditionalRows     int
	EvaluatedConditions int
	// UnresolvedIncludes lists the included TIDs absent from the catalog;
	// their INCLUDE rows are left out of the definitions, so the content
	// they describe is unknown content (a warning in an extensible
	// template).
	UnresolvedIncludes           []int
	UnresolvedIncludeIdentifiers []string
	// RecursiveIncludeRows lists normative self-includes represented as
	// productive references. Validation descends only with actual content.
	RecursiveIncludeRows []string
}

// StandardTemplateKey preserves the numeric catalog API.
func StandardTemplateKey(id int) (TemplateKey, bool) {
	if id <= 0 {
		return TemplateKey{}, false
	}
	return StandardTemplateKeyByIdentifier(strconv.Itoa(id))
}

// StandardTemplateKeyByIdentifier uses an exact canonical normative identifier.
// Numeric identifiers have no leading zero; a suffix is one uppercase ASCII letter.
func StandardTemplateKeyByIdentifier(identifier string) (TemplateKey, bool) {
	if _, ok := catalogTemplateByIdentifier(identifier); !ok {
		return TemplateKey{}, false
	}
	return TemplateKey{MappingResource: StandardTemplateMappingResource, Identifier: identifier, Version: StandardTemplateEdition}, true
}

// StandardTemplateIDs returns numeric TIDs in ascending order, preserving the legacy API.
func StandardTemplateIDs() []int {
	out := make([]int, 0, len(standardTemplateCatalog))
	for _, template := range standardTemplateCatalog {
		if template.id > 0 {
			out = append(out, template.id)
		}
	}
	sort.Ints(out)
	return out
}

// StandardTemplateIdentifiers returns all numeric and alphanumeric catalog identifiers.
// The generated order is by numeric prefix, then suffix.
func StandardTemplateIdentifiers() []string {
	out := make([]string, len(standardTemplateCatalog))
	for i := range standardTemplateCatalog {
		out[i] = standardTemplateCatalog[i].canonicalIdentifier()
	}
	return out
}

func LookupStandardTemplate(id int) (StandardTemplateInfo, bool) {
	if id <= 0 {
		return StandardTemplateInfo{}, false
	}
	return LookupStandardTemplateByIdentifier(strconv.Itoa(id))
}

// LookupStandardTemplateByIdentifier describes an exact normative identifier.
func LookupStandardTemplateByIdentifier(identifier string) (StandardTemplateInfo, bool) {
	template, ok := catalogTemplateByIdentifier(identifier)
	if !ok {
		return StandardTemplateInfo{}, false
	}
	info := StandardTemplateInfo{ID: template.id, Identifier: identifier, Name: template.name, Extensible: template.extensible,
		Order: template.order, Root: template.root, Rows: len(template.rows)}
	for _, row := range template.rows {
		if row.requirement == "MC" || row.requirement == "UC" {
			info.ConditionalRows++
		}
		if row.requirement == "MC" && row.evaluate != nil {
			info.EvaluatedConditions++
		}
		include := row.canonicalInclude()
		if include == identifier {
			info.RecursiveIncludeRows = append(info.RecursiveIncludeRows, row.label)
		}
		if include != "" {
			if _, known := catalogTemplateByIdentifier(include); !known {
				info.UnresolvedIncludeIdentifiers = append(info.UnresolvedIncludeIdentifiers, include)
				if row.include > 0 {
					info.UnresolvedIncludes = append(info.UnresolvedIncludes, row.include)
				}
			}
		}
	}
	return info, true
}

func (template *catalogTemplate) canonicalIdentifier() string {
	if template.identifier != "" {
		return template.identifier
	}
	return strconv.Itoa(template.id)
}
func (row *catalogRow) canonicalInclude() string {
	if row.includeIdentifier != "" {
		return row.includeIdentifier
	}
	if row.include > 0 {
		return strconv.Itoa(row.include)
	}
	return ""
}

var standardTemplateIndex = sync.OnceValue(func() map[string]*catalogTemplate {
	out := make(map[string]*catalogTemplate, len(standardTemplateCatalog))
	for i := range standardTemplateCatalog {
		template := &standardTemplateCatalog[i]
		out[template.canonicalIdentifier()] = template
	}
	return out
})

func canonicalStandardTemplateIdentifier(value string) bool {
	if len(value) == 0 || len(value) > maxRegistryString || value[0] < '1' || value[0] > '9' {
		return false
	}
	end := len(value)
	if value[end-1] >= 'A' && value[end-1] <= 'Z' {
		end--
	}
	for i := 0; i < end; i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	number, err := strconv.Atoi(value[:end])
	return end > 0 && err == nil && number > 0
}
func catalogTemplateByIdentifier(identifier string) (*catalogTemplate, bool) {
	if !canonicalStandardTemplateIdentifier(identifier) {
		return nil, false
	}
	template, ok := standardTemplateIndex()[identifier]
	return template, ok
}
func catalogTemplateByID(id int) (*catalogTemplate, bool) {
	if id <= 0 {
		return nil, false
	}
	return catalogTemplateByIdentifier(strconv.Itoa(id))
}

// StandardTemplates returns the PS3.16 templates ids and every template they
// include, as definitions for NewTemplateRegistry, with the context groups
// their enforced value sets need. The definitions come from the generated
// catalog (StandardTemplateEdition, StandardTemplateSource) and record it as
// their provenance. How the catalog's notation becomes TemplateRows:
//
//   - row 1 of a root template (Root: Yes), the document's root CONTAINER,
//     is not a row: its children are the definition's rows;
//   - a Rel with Parent becomes RelationshipTypes, by-reference for an "R-"
//     relationship and by-value otherwise; an INCLUDE row passes its
//     relationship only to the included rows without one
//     (IncludeFillsRelationship);
//   - an EV or DT concept name constrains the concept name; a concept name
//     from a context group constrains it to the members only for a DCID of a
//     non-extensible group, and $parameters do not constrain it;
//   - a DCID value set of a CODE row is enforced only for a non-extensible
//     group; BCID, extensible and unspecified ("CID n") value sets, units
//     and $parameters are recorded but not enforced;
//   - M is required, U optional; MC is conditional when its formal
//     condition is evaluable (row presence of siblings) and optional
//     otherwise; UC is optional;
//   - INCLUDE rows outside the catalog are recorded as unresolved. Normative
//     self-includes remain productive recursive references; the registry
//     resolves includes only at an actual content parent during validation.
//
// A clean validation therefore means the document matches the catalog's
// evaluable constraints, not full PS3.16 conformance.
func StandardTemplates(ids ...int) ([]TemplateDefinition, []ContextGroupDefinition, error) {
	if len(ids) > maxRegistryDefinitions {
		return nil, nil, newTemplateRegistryError(TemplateRegistryCodeResourceLimit)
	}
	identifiers := make([]string, len(ids))
	for i, id := range ids {
		identifiers[i] = strconv.Itoa(id)
	}
	return StandardTemplatesByIdentifier(identifiers...)
}

// StandardTemplatesByIdentifier preserves normative alphanumeric TIDs. Returned
// definitions retain INCLUDE references; construction does not expand their combinations.
func StandardTemplatesByIdentifier(identifiers ...string) ([]TemplateDefinition, []ContextGroupDefinition, error) {
	if len(identifiers) > maxRegistryDefinitions {
		return nil, nil, newTemplateRegistryError(TemplateRegistryCodeResourceLimit)
	}
	var definitions []TemplateDefinition
	groupIDs := map[int]bool{}
	added := map[string]bool{}
	var add func(string) error
	add = func(identifier string) error {
		if added[identifier] {
			return nil
		}
		template, ok := catalogTemplateByIdentifier(identifier)
		if !ok {
			return newTemplateRegistryError(TemplateRegistryCodeMissingInclude)
		}
		added[identifier] = true
		definition, includes, groups := template.definition()
		definitions = append(definitions, definition)
		for _, group := range groups {
			groupIDs[group] = true
		}
		for _, include := range includes {
			if err := add(include); err != nil {
				return err
			}
		}
		return nil
	}
	for _, identifier := range identifiers {
		if err := add(identifier); err != nil {
			return nil, nil, err
		}
	}
	var groups []int
	for id := range groupIDs {
		groups = append(groups, id)
	}
	sort.Ints(groups)
	contextGroups, err := StandardContextGroups(groups...)
	if err != nil {
		return nil, nil, err
	}
	return definitions, contextGroups, nil
}

func NewStandardTemplateRegistry(ids ...int) (*TemplateRegistry, error) {
	templates, groups, err := StandardTemplates(ids...)
	if err != nil {
		return nil, err
	}
	return NewTemplateRegistry(templates, groups)
}

// NewStandardTemplateRegistryByIdentifier builds the same immutable registry using exact identifiers.
func NewStandardTemplateRegistryByIdentifier(identifiers ...string) (*TemplateRegistry, error) {
	templates, groups, err := StandardTemplatesByIdentifier(identifiers...)
	if err != nil {
		return nil, err
	}
	return NewTemplateRegistry(templates, groups)
}

// definition converts the template, returning the TIDs it includes and the
// context groups its enforced value sets use.
func (template *catalogTemplate) definition() (TemplateDefinition, []string, []int) {
	key, _ := StandardTemplateKeyByIdentifier(template.canonicalIdentifier())
	definition := TemplateDefinition{
		Key: key, Extensible: template.extensible, OrderSignificant: template.order == "Significant",
		Provenance: DefinitionProvenance{
			Source:   fmt.Sprintf("DICOM PS3.16 %s TID %s (%s)", StandardTemplateEdition, template.canonicalIdentifier(), StandardTemplateSource),
			Checksum: StandardTemplateCatalogSHA256,
		},
	}
	var includes []string
	var groups []int
	top := template.rowsOf(0, len(template.rows), 0, &includes, &groups)
	definition.Rows = top
	if template.root == "Yes" && len(top) == 1 && len(top[0].ValueTypes) == 1 && top[0].ValueTypes[0] == ValueContainer {
		definition.Rows = top[0].Children
	}
	return definition, includes, groups
}

// rowsOf converts the rows [start, end) at NL level with their descendants.
func (template *catalogTemplate) rowsOf(start, end, level int, includes *[]string, groups *[]int) []TemplateRow {
	var out []TemplateRow
	for index := start; index < end; {
		next := index + 1
		for next < end && template.rows[next].level > level {
			next++
		}
		if converted, ok := template.row(&template.rows[index], includes, groups); ok {
			if converted.Include == nil {
				converted.Children = template.rowsOf(index+1, next, level+1, includes, groups)
			}
			out = append(out, converted)
		}
		index = next
	}
	return out
}

func (template *catalogTemplate) row(row *catalogRow, includes *[]string, groups *[]int) (TemplateRow, bool) {
	ruleID := catalogRuleID(template.canonicalIdentifier(), row.label)
	out := TemplateRow{
		RuleID:      ruleID,
		Cardinality: Cardinality{Min: row.min, Max: row.max},
	}
	if row.relationship != "" {
		out.RelationshipTypes = []string{row.relationship}
	}
	if row.byReference {
		out.ReferenceMode = ReferenceModeByReference
	} else {
		out.ReferenceMode = ReferenceModeByValue
	}
	switch row.requirement {
	case "M":
		out.Requirement = RequirementRequired
	case "MC":
		out.Requirement = RequirementOptional
		if row.evaluate != nil {
			out.Requirement = RequirementConditional
			out.ConditionID = row.formal
			out.Condition = catalogConditionFunc(template.canonicalIdentifier(), row.label, row.evaluate)
		}
	default:
		out.Requirement = RequirementOptional
	}
	if include := row.canonicalInclude(); include != "" {
		if _, known := catalogTemplateByIdentifier(include); !known {
			return TemplateRow{}, false
		}
		key, _ := StandardTemplateKeyByIdentifier(include)
		out.Include = &key
		out.IncludeFillsRelationship = true
		if row.relationship == "" {
			out.ReferenceMode = 0
		}
		out.AllowRecursiveInclude = include == template.canonicalIdentifier()
		*includes = append(*includes, include)
		return out, true
	}
	out.ValueTypes = []ValueType{row.valueType}
	switch row.concept {
	case "EV", "DT":
		out.ConceptNames = []CodeKey{catalogCodeKey(row.code)}
	case "CID":
		if row.conceptStrength == "D" {
			if members, ok := nonExtensibleContextGroupMembers(row.conceptCID); ok {
				out.ConceptNames = members
			}
		}
	}
	if row.valueSetStrength == "D" && row.valueType == ValueCode {
		if group, ok := codes.ContextGroupByID(row.valueSetCID); ok && !group.Extensible && !group.DefinedByReference {
			if key, ok := StandardContextGroupKey(row.valueSetCID); ok {
				out.ContextGroup = &key
				*groups = append(*groups, row.valueSetCID)
			}
		}
	}
	return out, true
}

// catalogRuleID is the RuleID of a catalog row, "TID1500:6"; include
// scoping prefixes it with the RuleIDs of the INCLUDE rows.
func catalogRuleID(id string, label string) string {
	return "TID" + id + ":" + label
}

// catalogConditionFunc evaluates condition over the rows of TID id that
// share the parent of row label, in the compiled (include-scoped) RuleIDs of
// the validated parent.
func catalogConditionFunc(id string, label string, condition *catalogCondition) TemplateCondition {
	own := catalogRuleID(id, label)
	return func(_ context.Context, input TemplateConditionContext) (bool, error) {
		components := strings.Split(input.RuleID, ".")
		position := -1
		for index := len(components) - 1; index >= 0; index-- {
			if components[index] == own {
				position = index
				break
			}
		}
		if position < 0 {
			return false, fmt.Errorf("dicom/sr: condition of %s evaluated for another row", own)
		}
		prefix := strings.Join(components[:position], ".")
		if prefix != "" {
			prefix += "."
		}
		return condition.eval(func(sibling string) int {
			target := prefix + catalogRuleID(id, sibling)
			total := 0
			for ruleID, count := range input.RowCounts {
				if ruleID == target || strings.HasPrefix(ruleID, target+".") {
					total += count
				}
			}
			return total
		}), nil
	}
}

func catalogCodeKey(code codes.Code) CodeKey {
	if len([]rune(code.Value)) > 16 {
		return CodeKey{LongCodeValue: code.Value, CodingSchemeDesignator: code.Scheme}
	}
	return CodeKey{CodeValue: code.Value, CodingSchemeDesignator: code.Scheme}
}

// nonExtensibleContextGroupMembers lists the codes of a non-extensible
// context group with the groups it includes.
func nonExtensibleContextGroupMembers(id int) ([]CodeKey, bool) {
	group, ok := codes.ContextGroupByID(id)
	if !ok || group.Extensible || group.DefinedByReference {
		return nil, false
	}
	var out []CodeKey
	seen := map[int]bool{}
	var walk func(int)
	walk = func(id int) {
		if seen[id] {
			return
		}
		seen[id] = true
		group, ok := codes.ContextGroupByID(id)
		if !ok {
			return
		}
		for _, term := range group.Codes {
			out = append(out, catalogCodeKey(codes.Code{Value: term.Value, Scheme: term.Scheme}))
		}
		for _, included := range group.Includes {
			walk(included)
		}
	}
	walk(id)
	return out, len(out) > 0
}
