package iodmodules

// RuleSource is additive provenance for a legacy projected rule. Index refers
// to Rows(Table), or to Modules(Table) for an IOD usage. CrossReferences retain
// DocBook link targets, including targets whose visible text was empty.
type RuleSource struct {
	Rule            string
	Source          ConstraintSource
	CrossReferences []string
	// ExternalFamily is generated from an explicit audited condition binding.
	// It is not a runtime interpretation of arbitrary normative prose.
	ExternalFamily string
}

var rowSources map[string]map[int]RuleSource
var moduleSources map[string]map[int]RuleSource

func copyRuleSource(s RuleSource) RuleSource {
	s.CrossReferences = append([]string(nil), s.CrossReferences...)
	return s
}

// RowSource returns the normative origin of one projected attribute/include.
func RowSource(table string, index int) (RuleSource, bool) {
	s, ok := rowSources[table][index]
	return copyRuleSource(s), ok
}

// ModuleSource returns the origin of one IOD module usage.
func ModuleSource(iod string, index int) (RuleSource, bool) {
	s, ok := moduleSources[iod][index]
	return copyRuleSource(s), ok
}
