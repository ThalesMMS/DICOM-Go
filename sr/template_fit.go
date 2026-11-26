package sr

import "context"

// registryLookups carries the per-validation indexes that row matching
// needs.
type registryLookups struct {
	targets      map[string]*ContentItem
	codeContexts map[string]ContentItemCodeContexts
}

// templateRowCounts maps the compiled RuleID of each row of one parent to
// the number of children it matched.
func templateRowCounts(rows []TemplateRow, counts []int) map[string]int {
	out := make(map[string]int, len(rows))
	for index, row := range rows {
		out[row.RuleID] += counts[index]
	}
	return out
}

// bestFittingRow chooses, among the rows of frame that match child with the
// static selection score best (templateRowStaticScore), the one whose child
// rows describe child's children best: most children matched, fewest left
// unmatched and fewest required child rows missing. A row already at its
// maximum count loses to any other; between equal fits the selection score
// (which prefers rows below their minimum count) decides, then table order.
func (validator *TemplateValidator) bestFittingRow(
	ctx context.Context,
	document *Document,
	frame templateValidationFrame,
	counts []int,
	best int,
	child *ContentItem,
	childPath ContentItemIdentifier,
	lookups registryLookups,
	steps *int,
) (int, error) {
	chosen, chosenFull, chosenFit, chosenScore := -1, false, 0, 0
	for rowIndex, row := range frame.rows {
		*steps++
		if *steps > validator.options.MaxSteps {
			break
		}
		matches, _ := templateRowMatches(row, *child, lookups.codeContexts[childPath.String()], validator.registry, lookups.targets, lookups.codeContexts, validator.options)
		if !matches || templateRowStaticScore(row) != best {
			continue
		}
		full := row.Cardinality.Max > 0 && counts[rowIndex] >= row.Cardinality.Max
		fit, err := validator.childRowsFit(ctx, document, row, frame.childScopes[rowIndex], child, childPath, lookups, steps)
		if err != nil {
			return -1, err
		}
		score := templateRowSelectionScore(row, counts[rowIndex])
		better := chosen < 0 ||
			(!full && chosenFull) ||
			(full == chosenFull && (fit > chosenFit || (fit == chosenFit && score > chosenScore)))
		if better {
			chosen, chosenFull, chosenFit, chosenScore = rowIndex, full, fit, score
		}
	}
	return chosen, nil
}

// childRowsFit scores how well row.Children describe item.Children, one
// level deep.
func (validator *TemplateValidator) childRowsFit(
	ctx context.Context,
	document *Document,
	row TemplateRow,
	scope string,
	item *ContentItem,
	itemPath ContentItemIdentifier,
	lookups registryLookups,
	steps *int,
) (int, error) {
	// A relationship reference selects its target, but does not own another
	// occurrence of that target's children.
	if len(item.ReferencedContentItemIdentifier) != 0 {
		return 0, nil
	}
	rows, _, err := validator.registry.resolveTemplateLevel(ctx, row.Children, scope, steps, validator.options.MaxSteps)
	if err != nil {
		return 0, err
	}
	counts := make([]int, len(rows))
	matched, unmatched := 0, 0
	for childIndex := range item.Children {
		child := &item.Children[childIndex]
		childPath := appendPath(itemPath, uint32(childIndex+1))
		chosen, chosenScore := -1, 0
		for rowIndex, candidate := range rows {
			*steps++
			if *steps > validator.options.MaxSteps {
				return matched - unmatched, ErrResourceLimitExceeded
			}
			matches, _ := templateRowMatches(candidate, *child, lookups.codeContexts[childPath.String()], validator.registry, lookups.targets, lookups.codeContexts, validator.options)
			if !matches {
				continue
			}
			score := templateRowSelectionScore(candidate, counts[rowIndex])
			if chosen < 0 || score > chosenScore {
				chosen, chosenScore = rowIndex, score
			}
		}
		if chosen < 0 {
			unmatched++
			continue
		}
		counts[chosen]++
		matched++
	}
	missing := 0
	var rowCounts map[string]int
	for rowIndex, candidate := range rows {
		if *steps >= validator.options.MaxSteps {
			return 0, ErrResourceLimitExceeded
		}
		*steps++
		required := candidate.Requirement == RequirementRequired
		if candidate.Requirement == RequirementConditional && candidate.Condition != nil {
			if rowCounts == nil {
				rowCounts = templateRowCounts(rows, counts)
			}
			active, err := callTemplateCondition(candidate.Condition, ctx, TemplateConditionContext{
				SOPClassUID: document.SOPClassUID, ParentPath: itemPath.String(),
				ParentValueType: item.ValueType, ChildCount: len(item.Children),
				RuleID: candidate.RuleID, RowCounts: rowCounts,
			})
			required = err == nil && active
		}
		if required && counts[rowIndex] < max(candidate.Cardinality.Min, 1) {
			missing++
		}
	}
	return matched - unmatched - 2*missing, nil
}
