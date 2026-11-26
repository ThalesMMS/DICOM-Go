package iodmodules

// ConstraintSource locates a normative source row, independently of Include
// expansion. Row is 1-based in DocBook; Index is 0-based in Rows(Table), or
// -1 for a supplemental value-only source row absent from the legacy projection.
type ConstraintSource struct {
	Table, TableID  string
	Row, Index      int
	Depth           uint8
	Tag             uint32
	Section         string
	Edition, SHA256 string
}

// ValuePredicate is a compiled, bounded title condition. No prose is parsed by
// the validator. An empty Operator means unconditional.
type ValuePredicate struct {
	Operator string
	Tag      uint32
	Position int
	Values   []string
	Children []ValuePredicate
}

// ValueConstraint is a closed enumeration, extensible defined terms, or a
// contextual VM rule. Manual records describe limitations, not executable rules.
type ValueConstraint struct {
	Kind      string // enumerated, defined, count, manual
	Position  int    // 1-based; 0 checks every component
	Values    []string
	Predicate ValuePredicate
	CountTag  uint32 // root sequence whose Item count constrains VM
	// ParentPath binds a supplemental row through explicit source Sequence
	// nesting, without adding legacy Type requirements.
	ParentPath    []uint32
	AttributeType string
	Rule          string // stable source identifier, never an attribute value
	Reason        string // fixed manual-review reason
	Source        ConstraintSource
}

// Constraints returns the constraints attached to one generated row. The
// returned data is immutable, like Rows; callers must not modify its slices.
func Constraints(table string, index int) []ValueConstraint {
	return valueConstraints[table][index]
}

// ConstraintTables returns the generated constraints for coverage inspection.
// Treat all returned data as immutable.
func ConstraintTables() map[string]map[int][]ValueConstraint { return valueConstraints }
