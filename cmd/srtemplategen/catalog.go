package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Catalog is the content of internal/standard/sr_templates.tsv: the template
// tables of PS3.16 Annex A in the standard's own notation.
type Catalog struct {
	Edition   string // PS3.16 edition, such as 2026d
	Source    string // "part16.xml" when extracted, "curated" otherwise
	SHA256    string // SHA-256 of part16.xml, "-" when curated
	Extracted string // extraction date, "-" when curated
	Templates []Template
}

// Template is one TID with its table rows.
type Template struct {
	ID         int    // positive for a numeric TID; zero for an alphanumeric TID
	Identifier string // complete alphanumeric TID, otherwise empty
	Name       string
	Type       string // Extensible or Non-Extensible
	Order      string // Significant, Non-Significant or empty when not recorded
	Root       string // Yes, No or empty when not recorded
	Rows       []Row
}

var templateIdentifierRe = regexp.MustCompile(`^([0-9]+)([A-Z]?)$`)

// parseTemplateIdentifier keeps numeric IDs compatible with the integer
// catalog. The pinned edition additionally uses one uppercase suffix letter.
func parseTemplateIdentifier(value string) (int, string, error) {
	match := templateIdentifierRe.FindStringSubmatch(value)
	if match == nil {
		return 0, "", fmt.Errorf("invalid TID %q", value)
	}
	number, err := strconv.Atoi(match[1])
	if err != nil || number <= 0 {
		return 0, "", fmt.Errorf("invalid TID %q", value)
	}
	if match[2] == "" {
		return number, "", nil
	}
	identifier := strconv.Itoa(number) + match[2]
	if identifier != value {
		return 0, "", fmt.Errorf("noncanonical TID %q", value)
	}
	return 0, identifier, nil
}

func (t Template) identifier() string {
	if t.Identifier != "" {
		return t.Identifier
	}
	return strconv.Itoa(t.ID)
}

// Natural identifier order preserves numeric relative order, with suffixes
// immediately after their numeric prefix: 10003, 10003A, ... then 10004.
func sortTemplates(templates []Template) {
	sort.SliceStable(templates, func(i, j int) bool {
		a, b := templates[i].identifier(), templates[j].identifier()
		an := strings.TrimRight(a, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
		bn := strings.TrimRight(b, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
		if len(an) != len(bn) {
			return len(an) < len(bn)
		}
		if an != bn {
			return an < bn
		}
		return a[len(an):] < b[len(bn):]
	})
}

// Row is one row of a template table. Formal is the machine-readable form of
// Condition (see parseFormal); it is derived from Condition on extraction and
// may be empty when the condition is not formalized.
type Row struct {
	Label, NL, Rel, VT, Concept, VM, Req, Condition, Formal, ValueSet string
}

const (
	sourceCurated  = "curated"
	sourceDocBook  = "part16.xml"
	tsvColumnsTID  = 6
	tsvColumnsRow  = 12
	headerSummary  = "# DICOM PS3.16 SR templates (Annex A template tables), input of cmd/srtemplategen."
	headerRecords  = "# Records:\n#   T  TID  Name  Type (Extensible|Non-Extensible)  Order (Significant|Non-Significant|empty)  Root (Yes|No|empty)\n#   R  TID  Row  NL  Rel with Parent  VT  Concept Name  VM  Req Type  Condition  Formal Condition  Value Set Constraint\n"
	headerCuration = `# Curation: rows of the templates dicom-go writes and reads (TID 1500 and the templates it includes, TID 2010),
#   transcribed in PS3.16 notation and cross-checked with DICOM-Swift f60fe313 (same copyright holder, Apache-2.0,
#   recorded in THIRD_PARTY_PROVENANCE.json). Every EV/DT concept code is checked against the PS3.16 2026d terminology
#   of the codes package. Template names, Order and the BCID/DCID strength of value sets are not recorded where the
#   curation did not establish them; regenerate from part16.xml (cmd/srtemplategen -extract) for the full catalog.
`
)

// formatTSV writes the canonical TSV of c.
func formatTSV(c Catalog) []byte {
	var out bytes.Buffer
	fmt.Fprintf(&out, "%s\n# Edition: %s\n# Source: %s\n# Source SHA-256: %s\n# Extracted: %s\n", headerSummary, c.Edition, c.Source, c.SHA256, c.Extracted)
	if c.Source == sourceCurated {
		out.WriteString(headerCuration)
	}
	out.WriteString(headerRecords)
	for _, t := range c.Templates {
		fmt.Fprintf(&out, "T\t%s\t%s\t%s\t%s\t%s\n", t.identifier(), t.Name, t.Type, t.Order, t.Root)
	}
	for _, t := range c.Templates {
		for _, r := range t.Rows {
			fmt.Fprintf(&out, "R\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.identifier(), r.Label, r.NL, r.Rel, r.VT, r.Concept, r.VM, r.Req, r.Condition, r.Formal, r.ValueSet)
		}
	}
	return out.Bytes()
}

// parseTSV reads the TSV and checks every cell's notation (see check).
func parseTSV(data []byte) (Catalog, error) {
	var c Catalog
	index := map[string]int{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if strings.HasPrefix(text, "#") {
			for _, field := range []struct {
				prefix string
				value  *string
			}{
				{"# Edition: ", &c.Edition}, {"# Source: ", &c.Source},
				{"# Source SHA-256: ", &c.SHA256}, {"# Extracted: ", &c.Extracted},
			} {
				if value, ok := strings.CutPrefix(text, field.prefix); ok {
					*field.value = strings.TrimSpace(value)
				}
			}
			continue
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		cells := strings.Split(text, "\t")
		switch cells[0] {
		case "T":
			if len(cells) != tsvColumnsTID {
				return c, fmt.Errorf("line %d: %d template cells, want %d", line, len(cells), tsvColumnsTID)
			}
			id, identifier, err := parseTemplateIdentifier(cells[1])
			if err != nil {
				return c, fmt.Errorf("line %d: invalid TID %q", line, cells[1])
			}
			t := Template{ID: id, Identifier: identifier, Name: cells[2], Type: cells[3], Order: cells[4], Root: cells[5]}
			key := t.identifier()
			if _, duplicate := index[key]; duplicate {
				return c, fmt.Errorf("line %d: duplicate TID %s", line, key)
			}
			index[key] = len(c.Templates)
			c.Templates = append(c.Templates, t)
		case "R":
			if len(cells) != tsvColumnsRow {
				return c, fmt.Errorf("line %d: %d row cells, want %d", line, len(cells), tsvColumnsRow)
			}
			id, identifier, err := parseTemplateIdentifier(cells[1])
			if err != nil {
				return c, fmt.Errorf("line %d: invalid TID %q", line, cells[1])
			}
			key := (Template{ID: id, Identifier: identifier}).identifier()
			position, ok := index[key]
			if !ok {
				return c, fmt.Errorf("line %d: row of undeclared TID %s", line, key)
			}
			c.Templates[position].Rows = append(c.Templates[position].Rows, Row{
				Label: cells[2], NL: cells[3], Rel: cells[4], VT: cells[5], Concept: cells[6], VM: cells[7],
				Req: cells[8], Condition: cells[9], Formal: cells[10], ValueSet: cells[11],
			})
		default:
			return c, fmt.Errorf("line %d: unknown record %q", line, cells[0])
		}
	}
	if err := scanner.Err(); err != nil {
		return c, err
	}
	if c.Edition == "" || (c.Source != sourceCurated && c.Source != sourceDocBook) || c.SHA256 == "" || c.Extracted == "" {
		return c, fmt.Errorf("TSV header lacks edition, source, source SHA-256 or extraction date")
	}
	sortTemplates(c.Templates)
	return c, check(c)
}

// check validates the notation of every template and row: the generator
// refuses what it cannot interpret instead of guessing.
func check(c Catalog) error {
	var errs []error
	seen := map[string]bool{}
	for _, t := range c.Templates {
		key := t.identifier()
		if seen[key] {
			errs = append(errs, fmt.Errorf("duplicate TID %s", key))
		}
		seen[key] = true
		if err := checkTemplate(t); err != nil {
			errs = append(errs, fmt.Errorf("TID %s: %w", key, err))
		}
	}
	return errors.Join(errs...)
}

func checkTemplate(t Template) error {
	id, identifier, err := parseTemplateIdentifier(t.identifier())
	if err != nil || id != t.ID || identifier != t.Identifier {
		return errors.New("inconsistent template identifier")
	}
	if t.Type != "Extensible" && t.Type != "Non-Extensible" {
		return fmt.Errorf("type %q", t.Type)
	}
	if t.Order != "" && t.Order != "Significant" && t.Order != "Non-Significant" {
		return fmt.Errorf("order %q", t.Order)
	}
	if t.Root != "" && t.Root != "Yes" && t.Root != "No" {
		return fmt.Errorf("root %q", t.Root)
	}
	if len(t.Rows) == 0 {
		return errors.New("no rows")
	}
	_, err = interpret(t)
	return err
}
