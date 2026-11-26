package sr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func recursiveTestDefinition() TemplateDefinition {
	key := TemplateKey{MappingResource: "TEST", Identifier: "recursive", Version: "1"}
	return TemplateDefinition{Key: key, Rows: []TemplateRow{{RuleID: "node", ValueTypes: []ValueType{ValueContainer}, ReferenceMode: ReferenceModeByValue,
		Cardinality: Cardinality{Min: 1, Max: 1}, Requirement: RequirementRequired,
		Children: []TemplateRow{{RuleID: "nested", Include: &key, AllowRecursiveInclude: true, Requirement: RequirementOptional,
			ReferenceMode: ReferenceModeByValue, RelationshipTypes: []string{RelationshipContains}, IncludeFillsRelationship: true}}}}}
}

func TestProductiveRecursiveIncludesAndEmptyContent(t *testing.T) {
	definition := recursiveTestDefinition()
	registry, err := NewTemplateRegistry([]TemplateDefinition{definition}, nil)
	if err != nil {
		t.Fatal(err)
	}
	validator, _ := NewTemplateValidator(registry, TemplateValidationOptions{})
	node := ContentItem{ValueType: ValueContainer, RelationshipType: RelationshipContains}
	for i := 0; i < 4; i++ {
		node = ContentItem{ValueType: ValueContainer, RelationshipType: RelationshipContains, Children: []ContentItem{node}}
	}
	doc := &Document{Content: []ContentItem{node}}
	if report, err := validator.Validate(context.Background(), doc, definition.Key); err != nil || len(report.Findings) != 0 {
		t.Fatalf("finite nesting: %v %#v", err, report)
	}
	// No child content means no speculative descendant expansion. At the root
	// exactly two steps describe the missing required node.
	tiny, _ := NewTemplateValidator(registry, TemplateValidationOptions{MaxSteps: 2})
	report, err := tiny.Validate(context.Background(), &Document{}, definition.Key)
	if !errors.Is(err, ErrTemplateValidation) || errors.Is(err, ErrResourceLimitExceeded) {
		t.Fatalf("empty content: %v %#v", err, report)
	}
	assertTemplateFinding(t, report, TemplateCodeCardinality)
	// A real terminal node permits the optional nested occurrence to be absent.
	if report, err := validator.Validate(context.Background(), &Document{Content: []ContentItem{{ValueType: ValueContainer}}}, definition.Key); err != nil {
		t.Fatalf("finite terminal: %v %#v", err, report)
	}
	mutated := node
	mutated.Children[0].RelationshipType = RelationshipInferredFrom
	report, err = validator.Validate(context.Background(), &Document{Content: []ContentItem{mutated}}, definition.Key)
	if !errors.Is(err, ErrTemplateValidation) {
		t.Fatalf("invalid finite recursion: %v", err)
	}
	assertTemplateFinding(t, report, TemplateCodeUnknownContent)
}

func TestRecursiveIncludeDefinitionsRefuseNonproductiveCycles(t *testing.T) {
	base := recursiveTestDefinition()
	other := TemplateKey{MappingResource: "TEST", Identifier: "other", Version: "1"}
	tests := []struct {
		name        string
		definitions []TemplateDefinition
	}{
		{"unmarked self", []TemplateDefinition{{Key: base.Key, Rows: []TemplateRow{{RuleID: "self", Include: &base.Key}}}}},
		{"root marked self", []TemplateDefinition{{Key: base.Key, Rows: []TemplateRow{{RuleID: "self", Include: &base.Key, AllowRecursiveInclude: true}}}}},
		{"mutual", []TemplateDefinition{{Key: base.Key, Rows: []TemplateRow{{RuleID: "a", Include: &other}}}, {Key: other, Rows: []TemplateRow{{RuleID: "b", Include: &base.Key}}}}},
	}
	byref := cloneTemplateDefinition(base)
	byref.Rows[0].ReferenceMode = ReferenceModeByReference
	tests = append(tests, struct {
		name        string
		definitions []TemplateDefinition
	}{"reference ancestor", []TemplateDefinition{byref}})
	byref = cloneTemplateDefinition(base)
	byref.Rows[0].Children[0].ReferenceMode = ReferenceModeByReference
	tests = append(tests, struct {
		name        string
		definitions []TemplateDefinition
	}{"reference recursive edge", []TemplateDefinition{byref}})
	for _, mode := range []ReferenceMode{0, ReferenceModeEither} {
		for _, ancestor := range []bool{true, false} {
			definition := cloneTemplateDefinition(base)
			if ancestor {
				definition.Rows[0].ReferenceMode = mode
			} else {
				definition.Rows[0].Children[0].ReferenceMode = mode
			}
			tests = append(tests, struct {
				name        string
				definitions []TemplateDefinition
			}{fmt.Sprintf("unproven by-value mode %d ancestor %t", mode, ancestor), []TemplateDefinition{definition}})
		}
	}
	foreign := cloneTemplateDefinition(base)
	foreign.Rows[0].Children[0].Include = &other
	tests = append(tests, struct {
		name        string
		definitions []TemplateDefinition
	}{"marked foreign edge", []TemplateDefinition{foreign, {Key: other}}})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewTemplateRegistry(test.definitions, nil)
			var typed *TemplateRegistryError
			if !errors.As(err, &typed) || typed.Code != TemplateRegistryCodeIncludeCycle {
				t.Fatalf("cycle: %v", err)
			}
		})
	}
}

func TestLazyIncludeBudgetsAndHostileContent(t *testing.T) {
	definitions := make([]TemplateDefinition, 30)
	for i := range definitions {
		definitions[i].Key = TemplateKey{MappingResource: "TEST", Identifier: fmt.Sprint(i), Version: "1"}
	}
	definitions[0].Rows = []TemplateRow{{RuleID: "leaf", ValueTypes: []ValueType{ValueText}}}
	for i := 1; i < len(definitions); i++ {
		key := definitions[i-1].Key
		definitions[i].Rows = []TemplateRow{{RuleID: "left", Include: &key}, {RuleID: "right", Include: &key}}
	}
	registry, err := NewTemplateRegistry(definitions, nil)
	if err != nil {
		t.Fatal("raw graph refused instead of avoiding expansion", err)
	}
	// The graph denotes over half a billion current-level leaves. Admission
	// must stop after the declared visits, before allocating those leaves.
	steps := 0
	rows, _, err := registry.resolveTemplateLevel(context.Background(), definitions[29].Rows, "", &steps, 40)
	if !errors.Is(err, ErrResourceLimitExceeded) || rows != nil || steps != 40 {
		t.Fatalf("fanout admission: %d %d %v", len(rows), steps, err)
	}
	validator, _ := NewTemplateValidator(registry, TemplateValidationOptions{MaxSteps: 40})
	report, err := validator.Validate(context.Background(), &Document{}, definitions[29].Key)
	if !errors.Is(err, ErrResourceLimitExceeded) {
		t.Fatalf("fanout typed validation: %v", err)
	}
	assertTemplateFinding(t, report, TemplateCodeResourceLimit)
	recursive := recursiveTestDefinition()
	registry, err = NewTemplateRegistry([]TemplateDefinition{recursive}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cycle := []ContentItem{{ValueType: ValueContainer}}
	cycle[0].Children = cycle
	for _, opts := range []TemplateValidationOptions{{MaxDepth: 3}, {MaxItems: 3}, {MaxSteps: 1}} {
		v, _ := NewTemplateValidator(registry, opts)
		report, err := v.Validate(context.Background(), &Document{Content: cycle}, recursive.Key)
		if !errors.Is(err, ErrResourceLimitExceeded) {
			t.Fatalf("cyclic content: %v %#v", err, report)
		}
	}
	empty := TemplateDefinition{Key: recursive.Key}
	registry, _ = NewTemplateRegistry([]TemplateDefinition{empty}, nil)
	v, _ := NewTemplateValidator(registry, TemplateValidationOptions{MaxFindings: 1})
	report, err = v.Validate(context.Background(), &Document{Content: []ContentItem{{ValueType: ValueText}, {ValueType: ValueText}, {ValueType: ValueText}}}, empty.Key)
	if !errors.Is(err, ErrResourceLimitExceeded) || !report.Truncated || len(report.Findings) != 1 {
		t.Fatalf("findings budget: %v %#v", err, report)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := v.Validate(cancelled, &Document{}, empty.Key); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func standardTestCode(cid int) CodedEntry {
	members, _ := nonExtensibleContextGroupMembers(cid)
	if len(members) > 0 {
		return CodedEntry{CodeValue: members[0].CodeValue, CodingSchemeDesignator: members[0].CodingSchemeDesignator, CodeMeaning: "synthetic"}
	}
	return CodedEntry{CodeValue: "SYN", CodingSchemeDesignator: "99TEST", CodeMeaning: "synthetic"}
}
func standardTestItem(code, scheme, relationship string) ContentItem {
	return ContentItem{ValueType: ValueCode, ConceptName: CodedEntry{CodeValue: code, CodingSchemeDesignator: scheme, CodeMeaning: "synthetic"}, RelationshipType: relationship}
}
func cadFindingFixture(tid int, depth int, relationship string) ContentItem {
	finding := standardTestItem("111015", "DCM", relationship)
	cid := 6016
	if tid == 4006 {
		finding.ConceptName.CodeValue = "111059"
		cid = 6014
	}
	if tid == 4102 {
		cid = 6101
	}
	if tid == 4125 {
		cid = 6201
	}
	finding.Code = standardTestCode(cid)
	intent := standardTestItem("111056", "DCM", RelationshipHasConceptMod)
	intent.Code = standardTestCode(6034)
	finding.Children = append(finding.Children, intent)
	algorithmRel := RelationshipHasObsContext
	if tid == 4004 || tid == 4006 {
		algorithmRel = RelationshipHasProperties
	}
	if tid != 4006 {
		if tid != 4004 {
			finding.Children = append(finding.Children, ContentItem{ValueType: ValueText, ConceptName: CodedEntry{CodeValue: "111001", CodingSchemeDesignator: "DCM"}, RelationshipType: algorithmRel, Text: "synthetic"}, ContentItem{ValueType: ValueText, ConceptName: CodedEntry{CodeValue: "111003", CodingSchemeDesignator: "DCM"}, RelationshipType: algorithmRel, Text: "1"})
		}
		typ := standardTestItem("111016", "DCM", RelationshipHasProperties)
		typ.Code = standardTestCode(6035)
		scope := standardTestItem("111057", "DCM", RelationshipHasProperties)
		scope.Code = standardTestCode(6036)
		finding.Children = append(finding.Children, typ, scope)
	}
	if tid == 4004 || tid == 4006 {
		finding.Children = append(finding.Children, ContentItem{ValueType: ValueText, ConceptName: CodedEntry{CodeValue: "111001", CodingSchemeDesignator: "DCM"}, RelationshipType: algorithmRel, Text: "synthetic"}, ContentItem{ValueType: ValueText, ConceptName: CodedEntry{CodeValue: "111003", CodingSchemeDesignator: "DCM"}, RelationshipType: algorithmRel, Text: "1"})
	}
	if depth > 0 {
		finding.Children = append(finding.Children, cadFindingFixture(tid, depth-1, RelationshipInferredFrom))
	}
	return finding
}

func TestStandardFiniteCADRecursiveIncludes(t *testing.T) {
	for _, tid := range []int{4004, 4006, 4102, 4125} {
		t.Run(fmt.Sprint(tid), func(t *testing.T) {
			registry, err := NewStandardTemplateRegistry(tid)
			if err != nil {
				t.Fatal(err)
			}
			key, _ := StandardTemplateKey(tid)
			v, _ := NewTemplateValidator(registry, TemplateValidationOptions{})
			doc := &Document{Content: []ContentItem{cadFindingFixture(tid, 2, RelationshipContains)}}
			report, err := v.Validate(context.Background(), doc, key)
			if err != nil || len(report.Findings) != 0 {
				t.Fatalf("nested CAD: %v %#v", err, report.Findings)
			}
			// Remove a mandatory included algorithm name in the deepest finding. This
			// must diagnose that actual path, rather than accept it as unknown content.
			deepest := &doc.Content[0]
			for i := 0; i < 2; i++ {
				deepest = &deepest.Children[len(deepest.Children)-1]
			}
			for i := range deepest.Children {
				if deepest.Children[i].ConceptName.CodeValue == "111001" {
					deepest.Children = append(deepest.Children[:i], deepest.Children[i+1:]...)
					break
				}
			}
			report, err = v.Validate(context.Background(), doc, key)
			if !errors.Is(err, ErrTemplateValidation) {
				t.Fatalf("missing recursive property: %v", err)
			}
			found := false
			for _, f := range report.Findings {
				if f.Code == TemplateCodeCardinality && len(f.Path) > 2 && strings.Contains(f.RuleID, fmt.Sprintf("TID%d:", tid)) {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing scoped path: %#v", report.Findings)
			}
		})
	}
}

func TestStandardAlphanumericAndCardiovascularTemplates(t *testing.T) {
	ids := StandardTemplateIdentifiers()
	if len(ids) != 407 || len(StandardTemplateIDs()) != 404 {
		t.Fatalf("catalog identities: %d %d", len(ids), len(StandardTemplateIDs()))
	}
	all, err := NewStandardTemplateRegistryByIdentifier(ids...)
	if err != nil {
		t.Fatal("combined unexpanded catalog", err)
	}
	if len(all.templates) != 407 {
		t.Fatalf("raw definitions %d", len(all.templates))
	}
	for _, identifier := range []string{"10003A", "10003B", "10003C"} {
		key, ok := StandardTemplateKeyByIdentifier(identifier)
		info, found := LookupStandardTemplateByIdentifier(identifier)
		if !ok || !found || key.Identifier != identifier || info.Identifier != identifier || info.ID != 0 {
			t.Fatalf("alpha identity %s: %#v %#v", identifier, key, info)
		}
		v, _ := NewTemplateValidator(all, TemplateValidationOptions{})
		doc := &Document{}
		if identifier == "10003B" {
			item := standardTestItem("113733", "DCM", RelationshipContains)
			item.ValueType = ValueNum
			item.Measurement = &Measurement{Value: 80, Units: CodedEntry{CodeValue: "kV", CodingSchemeDesignator: "UCUM"}}
			exposure := standardTestItem("113736", "DCM", RelationshipContains)
			exposure.ValueType = ValueNum
			exposure.Measurement = &Measurement{Value: 1, Units: CodedEntry{CodeValue: "uA.s", CodingSchemeDesignator: "UCUM"}}
			doc.Content = []ContentItem{item, exposure}
		}
		if report, err := v.Validate(context.Background(), doc, key); err != nil || len(report.Findings) != 0 {
			t.Fatalf("alpha fixture %s: %v %#v", identifier, err, report)
		}
	}
	for _, bad := range []string{"010003A", "10003a", "10003AA", "10003 A", "0", "-1500", " 1500"} {
		if _, ok := StandardTemplateKeyByIdentifier(bad); ok {
			t.Fatalf("noncanonical identifier %q", bad)
		}
	}
	definitions, _, err := StandardTemplates(10003)
	if err != nil {
		t.Fatal(err)
	}
	includes := map[string]bool{}
	for _, d := range definitions {
		if strings.HasPrefix(d.Key.Identifier, "10003") {
			includes[d.Key.Identifier] = true
		}
	}
	for _, id := range []string{"10003A", "10003B", "10003C"} {
		if !includes[id] {
			t.Fatalf("unresolved alpha %s", id)
		}
	}
	for _, tid := range []int{3900, 3902} {
		key, _ := StandardTemplateKey(tid)
		v, _ := NewTemplateValidator(all, TemplateValidationOptions{})
		doc := &Document{}
		analysis := standardTestItem("111004", "DCM", RelationshipHasConceptMod)
		analysis.Code = CodedEntry{CodeValue: "122605", CodingSchemeDesignator: "DCM"}
		if tid == 3902 {
			doc.Content = []ContentItem{{ValueType: ValueContainer, ConceptName: CodedEntry{CodeValue: "59776-5", CodingSchemeDesignator: "LN"}, Children: []ContentItem{analysis}}}
		} else {
			procedure := standardTestItem("121058", "DCM", RelationshipHasConceptMod)
			procedure.Code = CodedEntry{CodeValue: "SYN", CodingSchemeDesignator: "99TEST"}
			language := standardTestItem("121049", "DCM", RelationshipHasConceptMod)
			language.Code = standardTestCode(5000)
			doc.Content = []ContentItem{procedure, language}
		}
		if report, err := v.Validate(context.Background(), doc, key); err != nil || len(report.Findings) != 0 {
			t.Fatalf("small TID%d: %v %#v", tid, err, report)
		}
	}
	// Neither callers nor simultaneous validators can mutate the frozen graph.
	got, _ := all.LookupTemplate(TemplateKey{MappingResource: "DCMR", Identifier: "4006", Version: StandardTemplateEdition})
	got.Rows[0].RuleID = "mutated"
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, _ := StandardTemplateKey(4006)
			v, _ := NewTemplateValidator(all, TemplateValidationOptions{})
			if _, err := v.Validate(context.Background(), &Document{Content: []ContentItem{cadFindingFixture(4006, 1, RelationshipContains)}}, key); err != nil {
				t.Errorf("concurrent immutable: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestTemplateGraphAdmissionBeforeOwnedCopies(t *testing.T) {
	key := TemplateKey{MappingResource: "TEST", Identifier: "admission", Version: "1"}
	assertLimit := func(t *testing.T, templates []TemplateDefinition, groups []ContextGroupDefinition) {
		t.Helper()
		registry, err := NewTemplateRegistry(templates, groups)
		var typed *TemplateRegistryError
		if registry != nil || !errors.As(err, &typed) || typed.Code != TemplateRegistryCodeResourceLimit {
			t.Fatalf("admission: registry=%v err=%v", registry != nil, err)
		}
	}
	t.Run("root rows", func(t *testing.T) {
		// Empty row IDs would be invalid if visited. The count must refuse before
		// copying/enqueuing this already caller-owned root slice.
		assertLimit(t, []TemplateDefinition{{Key: key, Rows: make([]TemplateRow, maxRegistryRows+1)}}, nil)
	})
	t.Run("children and pending roots", func(t *testing.T) {
		assertLimit(t, []TemplateDefinition{{Key: key, Rows: []TemplateRow{{RuleID: "pending"},
			{RuleID: "parent", Children: make([]TemplateRow, maxRegistryRows-1)}}}}, nil)
	})
	t.Run("remaining rows", func(t *testing.T) {
		total := maxRegistryRows - 1
		if err := validateTemplateRows(make([]TemplateRow, 2), &total); !errors.Is(err, ErrResourceLimitExceeded) || total != maxRegistryRows-1 {
			t.Fatalf("remaining admission: total=%d err=%v", total, err)
		}
	})
	t.Run("remaining context codes", func(t *testing.T) {
		first := ContextGroupKey{MappingResource: "TEST", Identifier: "first", Version: "1"}
		second := ContextGroupKey{MappingResource: "TEST", Identifier: "second", Version: "1"}
		assertLimit(t, nil, []ContextGroupDefinition{
			{Key: first, Codes: []CodeKey{{CodeValue: "one", CodingSchemeDesignator: "99TEST"}}},
			{Key: second, Codes: make([]CodeKey, maxRegistryCodes)},
		})
	})
	t.Run("aggregate context includes", func(t *testing.T) {
		first := ContextGroupKey{MappingResource: "TEST", Identifier: "first", Version: "1"}
		second := ContextGroupKey{MappingResource: "TEST", Identifier: "second", Version: "1"}
		// The second group is individually within its ceiling. The aggregate
		// rejects before duplicating its includes or inspecting missing keys.
		assertLimit(t, nil, []ContextGroupDefinition{
			{Key: first, Includes: make([]ContextGroupKey, maxRegistryIncludes/2+1)},
			{Key: second, Includes: make([]ContextGroupKey, maxRegistryIncludes/2)},
		})
	})
}

func TestIncludedByReferenceTargetChildrenHaveOneOwner(t *testing.T) {
	key := TemplateKey{MappingResource: "TEST", Identifier: "main", Version: "1"}
	included := TemplateKey{MappingResource: "TEST", Identifier: "target", Version: "1"}
	registry, err := NewTemplateRegistry([]TemplateDefinition{
		{Key: key, Rows: []TemplateRow{
			{RuleID: "owned", Include: &included, IncludeFillsRelationship: true, RelationshipTypes: []string{RelationshipContains}, ReferenceMode: ReferenceModeByValue},
			{RuleID: "reference", Include: &included, IncludeFillsRelationship: true, RelationshipTypes: []string{RelationshipInferredFrom}, ReferenceMode: ReferenceModeByReference},
		}},
		{Key: included, Rows: []TemplateRow{{RuleID: "container", ValueTypes: []ValueType{ValueContainer}, Requirement: RequirementRequired, Cardinality: Cardinality{Min: 1, Max: 1},
			Children: []TemplateRow{{RuleID: "property", ValueTypes: []ValueType{ValueText}, ReferenceMode: ReferenceModeByValue, RelationshipTypes: []string{RelationshipHasProperties}, Requirement: RequirementRequired, Cardinality: Cardinality{Min: 1, Max: 1}}}}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	validator, _ := NewTemplateValidator(registry, TemplateValidationOptions{})
	doc := &Document{Content: []ContentItem{
		{ValueType: ValueContainer, RelationshipType: RelationshipContains, Children: []ContentItem{{ValueType: ValueText, RelationshipType: RelationshipHasProperties, Text: "synthetic"}}},
		{RelationshipType: RelationshipInferredFrom, ReferencedContentItemIdentifier: ContentItemIdentifier{1, 1}},
	}}
	if report, err := validator.Validate(context.Background(), doc, key); err != nil || len(report.Findings) != 0 {
		t.Fatalf("one owned target subtree: %v %#v", err, report)
	}
	doc.Content[0].Children = nil
	report, err := validator.Validate(context.Background(), doc, key)
	if !errors.Is(err, ErrTemplateValidation) || len(report.Findings) != 1 || report.Findings[0].Code != TemplateCodeCardinality || report.Findings[0].Path.String() != "1.1" {
		t.Fatalf("missing owned property once: %v %#v", err, report.Findings)
	}
}
