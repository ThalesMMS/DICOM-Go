package main

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// interpretedRow is a Row with its PS3.16 notation decoded.
type interpretedRow struct {
	Row
	Level             int
	Parent            int // index of the parent row, -1 at NL 0
	Relationship      string
	ByReference       bool
	Include           int
	IncludeIdentifier string // complete alphanumeric include target, otherwise empty
	ConceptKind       string // "", "EV", "DT", "CID" or "$"
	Code              [3]string
	ConceptCID        int
	ConceptStr        string // context group strength: "D", "B" or "" (not recorded)
	Parameter         string
	Min, Max          int
	Formal            *condition // nil when absent or not evaluable
	ValueSetCID       int
	ValueSetStr       string
}

var (
	relationships = map[string]bool{
		"CONTAINS": true, "HAS PROPERTIES": true, "HAS OBS CONTEXT": true, "HAS ACQ CONTEXT": true,
		"HAS CONCEPT MOD": true, "INFERRED FROM": true, "SELECTED FROM": true,
	}
	valueTypes = map[string]bool{
		"TEXT": true, "CODE": true, "NUM": true, "DATETIME": true, "DATE": true, "TIME": true, "UIDREF": true,
		"PNAME": true, "COMPOSITE": true, "IMAGE": true, "WAVEFORM": true, "SCOORD": true, "SCOORD3D": true,
		"TCOORD": true, "CONTAINER": true, "TABLE": true,
	}
	// codedConcept also accepts two slips of the 2026d text: a code triple
	// without its EV prefix (TID 314 row 3), read as EV, and a code meaning
	// without its closing quote (TID 15101 row 2).
	codedConcept   = regexp.MustCompile(`^(EV|DT)?\s*\(\s*([^,()"]+?)\s*,\s*([^,()"]+?)\s*,\s*"([^"]+?)"?\s*\)$`)
	contextGroupRe = regexp.MustCompile(`^([DB]?)CID\s+(\d+)\b`)
	// includeRe accepts "BTID" (TID 5002 row 6) as an include, and the
	// normative letter-suffixed sub-templates (TID 10003A-C).
	includeRe   = regexp.MustCompile(`^[BD]?TID\s+(\d+)([A-Z]?)(?:\s|$)`)
	parameterRe = regexp.MustCompile(`^\$([\w-]+)$`)
	// rowValueConcept is a concept name given in prose as another row's
	// value (TID 4023 row 8); it is recorded and does not constrain.
	rowValueConcept = regexp.MustCompile(`^The value of Row \d+$`)
	vmRe            = regexp.MustCompile(`^(\d+)(?:-(\d+|n))?$`)
)

// interpret decodes the rows of t.
func interpret(t Template) ([]interpretedRow, error) {
	out := make([]interpretedRow, len(t.Rows))
	labels := map[string]int{}
	var parents []int // parents[level] = index of the last row at that level
	for i, row := range t.Rows {
		r := interpretedRow{Row: row, Parent: -1}
		if row.Label == "" || strings.ContainsAny(row.Label, ".:\t ") {
			return nil, fmt.Errorf("row %d: label %q", i+1, row.Label)
		}
		if _, duplicate := labels[row.Label]; duplicate {
			return nil, fmt.Errorf("row %s: duplicate label", row.Label)
		}
		labels[row.Label] = i
		if strings.Trim(row.NL, ">") != "" {
			return nil, fmt.Errorf("row %s: NL %q", row.Label, row.NL)
		}
		r.Level = len(row.NL)
		if r.Level > len(parents) {
			return nil, fmt.Errorf("row %s: NL %d skips a level", row.Label, r.Level)
		}
		parents = append(parents[:r.Level], i)
		if r.Level > 0 {
			r.Parent = parents[r.Level-1]
		}
		rel := row.Rel
		if trimmed, ok := strings.CutPrefix(rel, "R-"); ok {
			rel, r.ByReference = trimmed, true
		}
		if rel != "" && !relationships[rel] {
			return nil, fmt.Errorf("row %s: relationship %q", row.Label, row.Rel)
		}
		r.Relationship = rel
		if err := r.decodeConcept(); err != nil {
			return nil, fmt.Errorf("row %s: %w", row.Label, err)
		}
		match := vmRe.FindStringSubmatch(row.VM)
		switch {
		case row.VM == "n":
			r.Min, r.Max = 1, 0
		case match == nil:
			return nil, fmt.Errorf("row %s: VM %q", row.Label, row.VM)
		default:
			r.Min, _ = strconv.Atoi(match[1])
			r.Max = r.Min
			if match[2] == "n" {
				r.Max = 0
			} else if match[2] != "" {
				r.Max, _ = strconv.Atoi(match[2])
			}
			if r.Max != 0 && r.Max < r.Min {
				return nil, fmt.Errorf("row %s: VM %q", row.Label, row.VM)
			}
		}
		switch row.Req {
		case "M", "U":
			if row.Formal != "" {
				return nil, fmt.Errorf("row %s: formal condition on a %s row", row.Label, row.Req)
			}
		case "MC", "UC":
		default:
			return nil, fmt.Errorf("row %s: Req Type %q", row.Label, row.Req)
		}
		if match := contextGroupRe.FindStringSubmatch(row.ValueSet); match != nil {
			r.ValueSetStr = match[1]
			r.ValueSetCID, _ = strconv.Atoi(match[2])
		}
		out[i] = r
	}
	if len(out) > 0 && out[0].Level != 0 {
		return nil, errors.New("first row is not at NL 0")
	}
	for i := range out {
		if out[i].Formal == nil && out[i].Row.Formal == "" {
			continue
		}
		formal, err := parseFormal(out[i].Row.Formal)
		if err != nil {
			return nil, fmt.Errorf("row %s: formal condition %q: %w", out[i].Label, out[i].Row.Formal, err)
		}
		for _, label := range formal.labels() {
			if _, ok := labels[label]; !ok {
				return nil, fmt.Errorf("row %s: condition names unknown row %s", out[i].Label, label)
			}
		}
		if formal.evaluable(func(label string) bool { return out[labels[label]].Parent == out[i].Parent }) {
			out[i].Formal = formal
		}
	}
	return out, nil
}

func (r *interpretedRow) decodeConcept() error {
	concept := strings.TrimSpace(r.Concept)
	if r.VT == "INCLUDE" {
		match := includeRe.FindStringSubmatch(concept)
		if match == nil {
			return fmt.Errorf("INCLUDE of %q", r.Concept)
		}
		id, identifier, err := parseTemplateIdentifier(match[1] + match[2])
		if err != nil {
			return err
		}
		r.Include, r.IncludeIdentifier = id, identifier
		return nil
	}
	if !valueTypes[r.VT] {
		return fmt.Errorf("value type %q", r.VT)
	}
	if concept == "" {
		return nil
	}
	if match := codedConcept.FindStringSubmatch(concept); match != nil {
		r.ConceptKind = match[1]
		if r.ConceptKind == "" {
			r.ConceptKind = "EV"
		}
		r.Code = [3]string{match[2], match[3], match[4]}
		return nil
	}
	if match := contextGroupRe.FindStringSubmatch(concept); match != nil {
		r.ConceptKind, r.ConceptStr = "CID", match[1]
		r.ConceptCID, _ = strconv.Atoi(match[2])
		return nil
	}
	if match := parameterRe.FindStringSubmatch(concept); match != nil {
		r.ConceptKind, r.Parameter = "$", match[1]
		return nil
	}
	if rowValueConcept.MatchString(concept) {
		return nil
	}
	return fmt.Errorf("concept name %q", r.Concept)
}

// condition is a formal condition:
//
//	present(L,...)  any of the rows is present
//	absent(L,...)   none of the rows is present
//	xor(L,...)      none of the other rows is present (the row is then
//	                required; that two are not present together is not
//	                checked)
//	and(C,...), or(C,...), not(C)
//	value(L,(v, s, "m")), concept(L,(v, s, "m"))  the coded value or concept
//	                name of row L; recorded but not evaluable
//
// L is a row label of the same template. A condition is evaluable when it
// uses no value or concept term and names only rows with the same parent
// as the conditional row, whose presence is then known to the validator.
type condition struct {
	Op   string
	Rows []string
	Code [3]string
	Args []*condition
}

func (c *condition) labels() []string {
	out := append([]string(nil), c.Rows...)
	for _, arg := range c.Args {
		out = append(out, arg.labels()...)
	}
	return out
}

func (c *condition) evaluable(sibling func(string) bool) bool {
	switch c.Op {
	case "value", "concept":
		return false
	}
	for _, label := range c.Rows {
		if !sibling(label) {
			return false
		}
	}
	for _, arg := range c.Args {
		if !arg.evaluable(sibling) {
			return false
		}
	}
	return true
}

var labelRe = regexp.MustCompile(`^[0-9]+[a-z]?$`)

// parseFormal parses the formal condition notation.
func parseFormal(text string) (*condition, error) {
	p := &formalParser{text: text}
	c, err := p.condition()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.text) {
		return nil, fmt.Errorf("trailing %q", p.text[p.pos:])
	}
	return c, nil
}

type formalParser struct {
	text string
	pos  int
}

func (p *formalParser) skipSpace() {
	for p.pos < len(p.text) && p.text[p.pos] == ' ' {
		p.pos++
	}
}

func (p *formalParser) expect(b byte) error {
	p.skipSpace()
	if p.pos >= len(p.text) || p.text[p.pos] != b {
		return fmt.Errorf("expected %q at %d", b, p.pos)
	}
	p.pos++
	return nil
}

func (p *formalParser) word() string {
	p.skipSpace()
	start := p.pos
	for p.pos < len(p.text) && (p.text[p.pos] >= 'a' && p.text[p.pos] <= 'z' || p.text[p.pos] >= '0' && p.text[p.pos] <= '9') {
		p.pos++
	}
	return p.text[start:p.pos]
}

func (p *formalParser) condition() (*condition, error) {
	op := p.word()
	if err := p.expect('('); err != nil {
		return nil, err
	}
	c := &condition{Op: op}
	switch op {
	case "present", "absent", "xor":
		for {
			label := p.word()
			if !labelRe.MatchString(label) {
				return nil, fmt.Errorf("row label %q", label)
			}
			c.Rows = append(c.Rows, label)
			p.skipSpace()
			if p.pos < len(p.text) && p.text[p.pos] == ',' {
				p.pos++
				continue
			}
			break
		}
	case "and", "or", "not":
		for {
			arg, err := p.condition()
			if err != nil {
				return nil, err
			}
			c.Args = append(c.Args, arg)
			p.skipSpace()
			if op != "not" && p.pos < len(p.text) && p.text[p.pos] == ',' {
				p.pos++
				continue
			}
			break
		}
	case "value", "concept":
		label := p.word()
		if !labelRe.MatchString(label) {
			return nil, fmt.Errorf("row label %q", label)
		}
		c.Rows = []string{label}
		if err := p.expect(','); err != nil {
			return nil, err
		}
		p.skipSpace()
		end := strings.IndexByte(p.text[p.pos:], ')')
		if end < 0 {
			return nil, errors.New("unterminated code")
		}
		match := codedConcept.FindStringSubmatch("EV " + p.text[p.pos:p.pos+end+1])
		if match == nil {
			return nil, fmt.Errorf("code %q", p.text[p.pos:p.pos+end+1])
		}
		c.Code = [3]string{match[2], match[3], match[4]}
		p.pos += end + 1
	default:
		return nil, fmt.Errorf("operator %q", op)
	}
	if err := p.expect(')'); err != nil {
		return nil, err
	}
	return c, nil
}

// String renders c in the formal notation.
func (c *condition) String() string {
	switch c.Op {
	case "value", "concept":
		return fmt.Sprintf("%s(%s,(%s, %s, %q))", c.Op, c.Rows[0], c.Code[0], c.Code[1], c.Code[2])
	case "and", "or", "not":
		args := make([]string, len(c.Args))
		for i, arg := range c.Args {
			args[i] = arg.String()
		}
		return c.Op + "(" + strings.Join(args, ",") + ")"
	}
	return c.Op + "(" + strings.Join(c.Rows, ",") + ")"
}

// deriveFormal returns the formal form of the condition text of row label,
// or "" for a condition text it does not recognize. Only the plain row
// presence wordings are recognized; everything else stays unevaluated.
func deriveFormal(label, text string) string {
	text = strings.TrimSpace(text)
	switch {
	case xorText.MatchString(text):
		rows, _ := rowList(xorText.FindStringSubmatch(text)[1])
		return "xor(" + strings.Join(rows, ",") + ")"
	case absentText.MatchString(text):
		// "Rows 10 and 12 are absent": none of them is present. An "or"
		// list (some of them absent) has no formal form.
		rows, connective := rowList(absentText.FindStringSubmatch(text)[1])
		if connective == "or" {
			return ""
		}
		return "absent(" + strings.Join(rows, ",") + ")"
	case presentText.MatchString(text):
		// present(...) is "any of them is present"; "Rows 3 and 4 are
		// present" needs each of them.
		rows, connective := rowList(presentText.FindStringSubmatch(text)[1])
		if connective == "and" && len(rows) > 1 {
			terms := make([]string, len(rows))
			for i, row := range rows {
				terms[i] = "present(" + row + ")"
			}
			return "and(" + strings.Join(terms, ",") + ")"
		}
		return "present(" + strings.Join(rows, ",") + ")"
	case atLeastOneText.MatchString(text):
		rows, _ := rowList(atLeastOneText.FindStringSubmatch(text)[1])
		var others []string
		for _, row := range rows {
			if row != label {
				others = append(others, row)
			}
		}
		if len(others) == 0 {
			return ""
		}
		return "absent(" + strings.Join(others, ",") + ")"
	}
	return ""
}

// rowList splits a row list ("10, 12", "Row 10 and Row 12", "6 or 7") into
// its labels and names its connective: "and" (also for a comma list),
// "or", "mixed", or "" for a single row.
func rowList(list string) ([]string, string) {
	list = rowWord.ReplaceAllString(strings.TrimSpace(list), "")
	rows := rowSeparator.Split(strings.TrimSpace(list), -1)
	hasOr := orWord.MatchString(list)
	hasAnd := andWord.MatchString(list) || strings.Contains(list, ",")
	switch {
	case len(rows) == 1:
		return rows, ""
	case hasOr && hasAnd:
		return rows, "mixed"
	case hasOr:
		return rows, "or"
	default:
		return rows, "and"
	}
}

var (
	// rowListRe is a list of row labels, each optionally preceded by "Row"
	// ("10, 12", "Row 10 and Row 12").
	rowListRe = `((?:rows?\s+)?[0-9]+[a-z]?(?:\s*(?:,|and|or)\s*(?:rows?\s+)?[0-9]+[a-z]?)*)`
	// conditionLead introduces a row-presence condition: "Required if",
	// "Shall be present if", "IF" or "IFF".
	conditionLead  = `(?:(?:required|shall be present) if|IFF?)\s+`
	xorText        = regexp.MustCompile(`(?i)^XOR\s+(?:with\s+)?` + rowListRe + `\.?$`)
	absentText     = regexp.MustCompile(`(?i)^` + conditionLead + rowListRe + `\s+(?:(?:is|are)\s+(?:absent|not present)|not present|(?:does|do) not exist)\.?$`)
	presentText    = regexp.MustCompile(`(?i)^` + conditionLead + rowListRe + `\s+(?:is|are)\s+present\.?$`)
	atLeastOneText = regexp.MustCompile(`(?i)^at least one of ` + rowListRe + `\s+shall be present\.?$`)
	rowSeparator   = regexp.MustCompile(`(?i)\s*(?:,|\band\b|\bor\b)\s*`)
	rowWord        = regexp.MustCompile(`(?i)\brows?\s+`)
	orWord         = regexp.MustCompile(`(?i)\bor\b`)
	andWord        = regexp.MustCompile(`(?i)\band\b`)
)
