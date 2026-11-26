package iod

import (
	"sort"

	"github.com/ThalesMMS/dicom-go/internal/iodmodules"
)

// ConditionCount counts PS3.3 conditions and how many of them the validator
// evaluates through its grammar or an audited source-bound rule. An evaluator
// can still require manual review when an operand in the dataset is unknown.
type ConditionCount struct {
	Total, Evaluated int
}

// Fraction is Evaluated / Total, 0 when there is no condition.
func (c ConditionCount) Fraction() float64 {
	if c.Total == 0 {
		return 0
	}
	return float64(c.Evaluated) / float64(c.Total)
}

func (c *ConditionCount) add(evaluated bool) {
	c.Total++
	if evaluated {
		c.Evaluated++
	}
}

// Coverage is the share of the conditions of the generated PS3.3 rules
// (StandardVersion) that the validator evaluates.
type Coverage struct {
	// Attributes counts the 1C and 2C rows of every Module and Macro table,
	// Includes the conditional Include rows and Modules the C Modules of
	// every IOD. Distinct counts the distinct condition texts of all three.
	Attributes, Includes, Modules, Distinct ConditionCount
	// Type1C and Type2C split the Attributes total by attribute type.
	Type1C, Type2C ConditionCount
	// Unevaluated lists the distinct condition texts that are not
	// evaluated in at least one source row, most frequent first (ties in
	// text order), with the number of unevaluated rows that use each.
	Unevaluated []ConditionUse
}

// ConditionUse is a condition text and the number of rows that use it.
type ConditionUse struct {
	Text string
	Rows int
}

// ConditionCoverage measures the conditions the validator evaluates over
// every table and IOD of the generated rules.
func ConditionCoverage() Coverage {
	var coverage Coverage
	uses := map[string]int{}
	note := func(text string, source iodmodules.RuleSource) bool {
		_, parsed := parseCondition(text)
		supported := parsed || priorityConditions[source.Rule] != nil
		if _, seen := uses[text]; !seen {
			uses[text] = 0
		}
		if !supported {
			uses[text]++
		}
		return supported
	}
	for _, table := range iodmodules.RuleTables() {
		rows, _ := iodmodules.Rows(table)
		for index, row := range rows {
			if row.Condition == "" {
				continue
			}
			source, _ := iodmodules.RowSource(table, index)
			switch {
			case row.Include != "":
				coverage.Includes.add(note(row.Condition, source))
			case row.Type == "1C", row.Type == "2C":
				supported := note(row.Condition, source)
				coverage.Attributes.add(supported)
				if row.Type == "1C" {
					coverage.Type1C.add(supported)
				} else {
					coverage.Type2C.add(supported)
				}
			}
		}
	}
	for _, name := range iodmodules.IODs() {
		modules, _ := iodmodules.Modules(name)
		for index, use := range modules {
			if use.Usage == "C" && use.Condition != "" {
				source, _ := iodmodules.ModuleSource(name, index)
				coverage.Modules.add(note(use.Condition, source))
			}
		}
	}
	for text, count := range uses {
		coverage.Distinct.add(count == 0)
		if count != 0 {
			coverage.Unevaluated = append(coverage.Unevaluated, ConditionUse{Text: text, Rows: count})
		}
	}
	sort.Slice(coverage.Unevaluated, func(i, j int) bool {
		a, b := coverage.Unevaluated[i], coverage.Unevaluated[j]
		if a.Rows != b.Rows {
			return a.Rows > b.Rows
		}
		return a.Text < b.Text
	})
	return coverage
}
