package sr

import "context"

// resolveTemplateLevel materializes only the rows of one actual content parent.
// Children stay in the frozen graph. Each admitted visit consumes the same
// operation budget as matching, before adding an output row or include frame.
func (registry *TemplateRegistry) resolveTemplateLevel(ctx context.Context, rows []TemplateRow, scope string, steps *int, limit int) ([]TemplateRow, []string, error) {
	type levelFrame struct {
		rows        []TemplateRow
		next        int
		scope       string
		constraints []TemplateRow
	}
	stack := []levelFrame{{rows: rows, scope: scope}}
	var out []TemplateRow
	var scopes []string
	seen := map[string]struct{}{}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		frame := &stack[len(stack)-1]
		if frame.next == len(frame.rows) {
			stack = stack[:len(stack)-1]
			continue
		}
		if *steps >= limit {
			return nil, nil, ErrResourceLimitExceeded
		}
		*steps++
		row := frame.rows[frame.next]
		frame.next++
		if row.Include != nil {
			if len(stack) > maxTemplateIncludeDepth || !templateIncludeScopeFits(frame.scope, row.RuleID) {
				return nil, nil, ErrResourceLimitExceeded
			}
			included, ok := registry.templates[*row.Include]
			if !ok {
				return nil, nil, ErrResourceLimitExceeded
			}
			constraints := append(append([]TemplateRow(nil), frame.constraints...), row)
			stack = append(stack, levelFrame{rows: included.Rows, scope: scopedTemplateRuleID(frame.scope, row.RuleID), constraints: constraints})
			continue
		}
		if len(out) >= maxRegistryRows || !templateIncludeScopeFits(frame.scope, row.RuleID) {
			return nil, nil, ErrResourceLimitExceeded
		}
		// A shallow copy retains immutable, registry-owned selector/child slices.
		resolved := row
		resolved.RuleID = scopedTemplateRuleID(frame.scope, row.RuleID)
		if _, duplicate := seen[resolved.RuleID]; duplicate {
			return nil, nil, ErrResourceLimitExceeded
		}
		for i := len(frame.constraints) - 1; i >= 0; i-- {
			var err error
			resolved, err = constrainTemplateRow(frame.constraints[i], resolved)
			if err != nil {
				return nil, nil, err
			}
		}
		seen[resolved.RuleID] = struct{}{}
		out = append(out, resolved)
		scopes = append(scopes, frame.scope)
	}
	return out, scopes, nil
}

func constrainTemplateRow(include, row TemplateRow) (TemplateRow, error) {
	switch {
	case len(include.RelationshipTypes) == 0:
	case !include.IncludeFillsRelationship:
		row.RelationshipTypes = include.RelationshipTypes
	case len(row.RelationshipTypes) == 0:
		row.RelationshipTypes = include.RelationshipTypes
		if include.ReferenceMode != 0 {
			row.ReferenceMode = include.ReferenceMode
		}
	}
	cardinality, err := composeTemplateCardinality(include.Cardinality, row.Cardinality)
	if err != nil {
		return TemplateRow{}, err
	}
	row.Cardinality = cardinality
	if include.Requirement == RequirementConditional && row.Requirement == RequirementConditional && len(include.ConditionID)+len(row.ConditionID)+1 > maxRegistryString {
		return TemplateRow{}, ErrResourceLimitExceeded
	}
	applyTemplateIncludeRequirement(include, &row)
	return row, nil
}

// validateTemplateIncludeGraph retains default cycle rejection. The sole
// exception is an explicit self-edge reached after an ordinary by-value row.
// Such an edge can recur only when validation consumes another content item.
func validateTemplateIncludeGraph(templates map[TemplateKey]TemplateDefinition) error {
	adjacency := make(map[TemplateKey][]TemplateKey, len(templates))
	indegree := make(map[TemplateKey]int, len(templates))
	for key := range templates {
		indegree[key] = 0
	}
	includeCount := 0
	for key, definition := range templates {
		type entry struct {
			rows        []TemplateRow
			next, depth int
			nonByValue  bool
		}
		stack := []entry{{rows: definition.Rows}}
		for len(stack) > 0 {
			frame := &stack[len(stack)-1]
			if frame.next == len(frame.rows) {
				stack = stack[:len(stack)-1]
				continue
			}
			row := frame.rows[frame.next]
			frame.next++
			if row.Include == nil {
				if row.AllowRecursiveInclude {
					return newTemplateRegistryError(TemplateRegistryCodeInvalidDefinition)
				}
				if len(row.Children) > 0 {
					stack = append(stack, entry{rows: row.Children, depth: frame.depth + 1, nonByValue: frame.nonByValue || row.ReferenceMode != ReferenceModeByValue})
				}
				continue
			}
			includeCount++
			if includeCount > maxRegistryIncludes {
				return newTemplateRegistryError(TemplateRegistryCodeResourceLimit)
			}
			if _, exists := templates[*row.Include]; !exists {
				return newTemplateRegistryError(TemplateRegistryCodeMissingInclude)
			}
			if row.AllowRecursiveInclude {
				if *row.Include != key || frame.depth == 0 || frame.nonByValue || row.ReferenceMode != ReferenceModeByValue {
					return newTemplateRegistryError(TemplateRegistryCodeIncludeCycle)
				}
				continue
			}
			adjacency[key] = append(adjacency[key], *row.Include)
			indegree[*row.Include]++
		}
	}
	if graphHasCycle(adjacency, cloneTemplateIndegree(indegree)) {
		return newTemplateRegistryError(TemplateRegistryCodeIncludeCycle)
	}
	// Longest path in the remaining DAG, without enumerating paths or rows.
	depth := make(map[TemplateKey]int, len(templates))
	var queue []TemplateKey
	for key, count := range indegree {
		if count == 0 {
			queue = append(queue, key)
		}
	}
	for head := 0; head < len(queue); head++ {
		key := queue[head]
		for _, target := range adjacency[key] {
			if depth[key]+1 > depth[target] {
				depth[target] = depth[key] + 1
			}
			if depth[target] > maxTemplateIncludeDepth {
				return newTemplateRegistryError(TemplateRegistryCodeResourceLimit)
			}
			indegree[target]--
			if indegree[target] == 0 {
				queue = append(queue, target)
			}
		}
	}
	return nil
}

func cloneTemplateIndegree(in map[TemplateKey]int) map[TemplateKey]int {
	out := make(map[TemplateKey]int, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func templateIncludeScopeFits(scope, ruleID string) bool {
	if scope == "" {
		return len(ruleID) <= maxRuleIDLength
	}
	return len(scope)+1+len(ruleID) <= maxRuleIDLength
}
