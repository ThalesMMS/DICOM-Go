package main

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// Table is the relationship content constraints of one PS3.3 edition.
type Table struct {
	Edition string
	IODs    []IOD
}

// IOD is one SR Document IOD and its Table A.35.x-2.
type IOD struct {
	SOPClassUID string
	Table       string // for example "A.35.3-2"
	Name        string // IOD name without the "IOD" suffix
	// ByReference is "none", "all", "all-except:REL,..." or "only:REL,...".
	ByReference string
	Rules       []Rule
}

// Rule is one table row. Value type and relationship fields hold the
// normalized terms: a comma-separated list, "*" for any, or "*-X" for any
// except X.
type Rule struct {
	Sources      string
	Relationship string
	Targets      string
}

var valueTypes = map[string]bool{
	"CONTAINER": true, "TEXT": true, "CODE": true, "NUM": true, "DATETIME": true, "DATE": true,
	"TIME": true, "UIDREF": true, "PNAME": true, "COMPOSITE": true, "IMAGE": true, "WAVEFORM": true,
	"SCOORD": true, "SCOORD3D": true, "TCOORD": true, "TABLE": true,
}

var relationships = map[string]bool{
	"CONTAINS": true, "HAS OBS CONTEXT": true, "HAS ACQ CONTEXT": true, "HAS CONCEPT MOD": true,
	"HAS PROPERTIES": true, "INFERRED FROM": true, "SELECTED FROM": true,
}

func validTerm(term string, known map[string]bool) error {
	if term == "*" {
		return nil
	}
	if except, ok := strings.CutPrefix(term, "*-"); ok {
		if !known[except] {
			return fmt.Errorf("unknown term %q", except)
		}
		return nil
	}
	for _, value := range strings.Split(term, ",") {
		if !known[value] {
			return fmt.Errorf("unknown term %q", value)
		}
	}
	return nil
}

func validByReference(policy string) error {
	switch policy {
	case "none", "all":
		return nil
	}
	for _, prefix := range []string{"all-except:", "only:"} {
		if list, ok := strings.CutPrefix(policy, prefix); ok {
			for _, relationship := range strings.Split(list, ",") {
				if !relationships[relationship] {
					return fmt.Errorf("by-reference policy %q: unknown relationship %q", policy, relationship)
				}
			}
			return nil
		}
	}
	return fmt.Errorf("unknown by-reference policy %q", policy)
}

func (table Table) validate() error {
	if table.Edition == "" {
		return fmt.Errorf("no edition")
	}
	if len(table.IODs) == 0 {
		return fmt.Errorf("no IOD")
	}
	seen := map[string]bool{}
	for _, iod := range table.IODs {
		if iod.SOPClassUID == "" || seen[iod.SOPClassUID] {
			return fmt.Errorf("IOD %q: missing or duplicate SOP Class UID %q", iod.Name, iod.SOPClassUID)
		}
		seen[iod.SOPClassUID] = true
		if err := validByReference(iod.ByReference); err != nil {
			return fmt.Errorf("IOD %q: %w", iod.Name, err)
		}
		if len(iod.Rules) == 0 {
			return fmt.Errorf("IOD %q: no rule", iod.Name)
		}
		for _, rule := range iod.Rules {
			for _, check := range []struct {
				term  string
				known map[string]bool
			}{{rule.Sources, valueTypes}, {rule.Relationship, relationships}, {rule.Targets, valueTypes}} {
				if err := validTerm(check.term, check.known); err != nil {
					return fmt.Errorf("IOD %q: %w", iod.Name, err)
				}
			}
		}
	}
	return nil
}

const tsvHeader = `# SR relationship content constraints
# Source: DICOM PS3.3 %s, Section A.35, Tables A.35.x-2 "Relationship Content
# Constraints" and the "Relationship Constraints" text of each SR Document IOD
# Extracted by cmd/srrelgen -extract from the PS3.3 chtml pages on %s
# Format:
#   edition<TAB>PS3.3 edition
#   iod<TAB>SOP Class UID<TAB>Table<TAB>By-reference policy<TAB>IOD name
#   rel<TAB>Source value types<TAB>Relationship type<TAB>Target value types
# Terms are comma-separated lists; "*" is any, "*-X" any except X.
# By-reference policy: none | all | all-except:REL,... | only:REL,...
`

func formatTSV(table Table, date string) []byte {
	var out bytes.Buffer
	fmt.Fprintf(&out, tsvHeader, table.Edition, date)
	fmt.Fprintf(&out, "edition\t%s\n", table.Edition)
	for _, iod := range table.IODs {
		fmt.Fprintf(&out, "iod\t%s\t%s\t%s\t%s\n", iod.SOPClassUID, iod.Table, iod.ByReference, iod.Name)
		for _, rule := range iod.Rules {
			fmt.Fprintf(&out, "rel\t%s\t%s\t%s\n", rule.Sources, rule.Relationship, rule.Targets)
		}
	}
	return out.Bytes()
}

func parseTSV(data []byte) (Table, error) {
	var table Table
	for number, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		fail := func(format string, args ...any) (Table, error) {
			return Table{}, fmt.Errorf("line %d: %s", number+1, fmt.Sprintf(format, args...))
		}
		switch fields[0] {
		case "edition":
			if len(fields) != 2 || table.Edition != "" {
				return fail("bad edition row")
			}
			table.Edition = fields[1]
		case "iod":
			if len(fields) != 5 {
				return fail("iod row has %d fields, want 5", len(fields))
			}
			table.IODs = append(table.IODs, IOD{SOPClassUID: fields[1], Table: fields[2], ByReference: fields[3], Name: fields[4]})
		case "rel":
			if len(fields) != 4 || len(table.IODs) == 0 {
				return fail("bad rel row")
			}
			iod := &table.IODs[len(table.IODs)-1]
			iod.Rules = append(iod.Rules, Rule{Sources: fields[1], Relationship: fields[2], Targets: fields[3]})
		default:
			return fail("unknown row kind %q", fields[0])
		}
	}
	if err := table.validate(); err != nil {
		return Table{}, err
	}
	return table, nil
}

// sortIODs orders IODs by table number (A.35.1-2, A.35.2-2, ..., A.35.10-2).
func sortIODs(iods []IOD) {
	number := func(table string) int {
		var major, minor int
		_, _ = fmt.Sscanf(table, "A.35.%d-%d", &major, &minor)
		return major
	}
	sort.SliceStable(iods, func(i, j int) bool { return number(iods[i].Table) < number(iods[j].Table) })
}
