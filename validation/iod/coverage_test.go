package iod

import (
	"testing"
)

// TestConditionCoverage is the coverage report of the PS3.3 conditions the
// validator evaluates (docs/VALIDATION.md records the numbers). Before the
// formalization of names without tags, AT Values, coded Items, more value
// wordings and measurable facts, the 2026d rules evaluated 1,099 of 1,997
// Attribute conditions (55.0%), 10 of 13 Include conditions and 36 of 328
// Module conditions (11.0%); 716 of 1,494 distinct texts (47.9%). The floors
// keep the gain from regressing.
func TestConditionCoverage(t *testing.T) {
	coverage := ConditionCoverage()
	t.Logf("Attributes %d/%d (%.1f%%), Includes %d/%d, Modules %d/%d (%.1f%%), distinct texts %d/%d (%.1f%%)",
		coverage.Attributes.Evaluated, coverage.Attributes.Total, 100*coverage.Attributes.Fraction(),
		coverage.Includes.Evaluated, coverage.Includes.Total,
		coverage.Modules.Evaluated, coverage.Modules.Total, 100*coverage.Modules.Fraction(),
		coverage.Distinct.Evaluated, coverage.Distinct.Total, 100*coverage.Distinct.Fraction())
	for _, floor := range []struct {
		name  string
		count ConditionCount
		min   int
	}{
		{"Attributes", coverage.Attributes, 1253},
		{"Type 1C", coverage.Type1C, 1152},
		{"Type 2C", coverage.Type2C, 101},
		{"Includes", coverage.Includes, 10},
		{"Modules", coverage.Modules, 120},
		{"distinct texts", coverage.Distinct, 826},
	} {
		if floor.count.Evaluated < floor.min {
			t.Errorf("%s: %d evaluated conditions, want at least %d", floor.name, floor.count.Evaluated, floor.min)
		}
	}
	if coverage.Attributes.Total != coverage.Type1C.Total+coverage.Type2C.Total ||
		coverage.Attributes.Evaluated != coverage.Type1C.Evaluated+coverage.Type2C.Evaluated {
		t.Fatal("per-type condition coverage does not sum to attribute coverage")
	}
	if coverage.Distinct.Total-coverage.Distinct.Evaluated != len(coverage.Unevaluated) {
		t.Fatalf("%d unevaluated texts listed, want %d", len(coverage.Unevaluated), coverage.Distinct.Total-coverage.Distinct.Evaluated)
	}
	for i := 1; i < len(coverage.Unevaluated); i++ {
		if coverage.Unevaluated[i].Rows > coverage.Unevaluated[i-1].Rows {
			t.Fatal("unevaluated conditions are not ordered by frequency")
		}
	}
}
