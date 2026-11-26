// Command iodmodulegen projects the Module and IOD tables of the DICOM PS3.3
// DocBook into the compact data used by package internal/iodmodules: the
// top-level Attributes of every Module and Macro (Include rows resolved) and
// the Modules of every IOD with their usage.
//
// The input is a local copy of the official DocBook (part03.xml). The command
// has no network client: updating the generated data is a separate, auditable
// step that requires an explicitly supplied source file.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"flag"
	"fmt"
	"go/format"
	"html"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

func main() {
	in := flag.String("in", "", "path to the official PS3.3 DocBook (part03.xml)")
	bodyParts := flag.String("bodyparts", "", "path to the official PS3.16 Annex L page (chapter_L.html), to generate the Body Part Examined codes instead")
	pkg := flag.String("package", "iodmodules", "package of the generated Go file")
	out := flag.String("out", "", "path of the generated Go file")
	rulesOut := flag.String("rules", "", "path of the generated Module and Macro rows (rules_generated.go), with -in")
	sourcesOut := flag.String("sources", "", "path of the additive normative row/module source sidecar, with -in")
	constraintsOut := flag.String("constraints", "", "path of the generated contextual value constraints, with -in")
	flag.Parse()
	if *bodyParts != "" && *out != "" {
		page, err := os.ReadFile(*bodyParts)
		if err == nil {
			var code []byte
			if code, err = generateBodyParts(page, *pkg); err == nil {
				err = os.WriteFile(*out, code, 0o644)
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: iodmodulegen -in part03.xml -out modules_generated.go | -bodyparts chapter_L.html -package p -out bodyparts_generated.go")
		os.Exit(2)
	}
	source, err := os.Open(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer source.Close()
	book, err := parse(source)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code, err := generate(book)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, code, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *rulesOut != "" {
		rules, err := generateRules(book)
		if err == nil {
			err = os.WriteFile(*rulesOut, rules, 0o644)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if *sourcesOut != "" {
		sources, err := generateRuleSources(book)
		if err == nil {
			err = os.WriteFile(*sourcesOut, sources, 0o644)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if *constraintsOut != "" {
		constraints, err := generateConstraints(book)
		if err == nil {
			err = os.WriteFile(*constraintsOut, constraints, 0o644)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

// node is a generic DocBook element.
type node struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Nodes   []node     `xml:",any"`
	Text    string     `xml:",chardata"`
	ordered string
}

// UnmarshalXML preserves mixed-content order for literal constraint terms.
// Text retains the legacy concatenated chardata used by the existing tables.
func (n *node) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	n.XMLName, n.Attrs = start.Name, start.Attr
	var ordered strings.Builder
	for {
		t, err := d.Token()
		if err != nil {
			return err
		}
		switch t := t.(type) {
		case xml.CharData:
			n.Text += string(t)
			ordered.Write(t)
		case xml.StartElement:
			var child node
			if err := d.DecodeElement(&child, &t); err != nil {
				return err
			}
			n.Nodes = append(n.Nodes, child)
			ordered.WriteString(child.ordered)
		case xml.EndElement:
			n.ordered = ordered.String()
			return nil
		}
	}
}

func (n node) orderedText() string { return strings.Join(strings.Fields(n.ordered), " ") }

func (n node) attr(local string) string {
	for _, a := range n.Attrs {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

// text is the element's text with whitespace collapsed.
func (n node) text() string {
	var b strings.Builder
	var walk func(node)
	walk = func(n node) {
		b.WriteString(n.Text)
		for _, c := range n.Nodes {
			b.WriteString(" ")
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

// links are the xref targets inside the element.
func (n node) links() []string {
	var out []string
	var walk func(node)
	walk = func(n node) {
		if n.XMLName.Local == "xref" {
			out = append(out, n.attr("linkend"))
		}
		for _, c := range n.Nodes {
			walk(c)
		}
	}
	walk(n)
	return out
}

func (n node) find(local string) []node {
	var out []node
	var walk func(node)
	walk = func(n node) {
		if n.XMLName.Local == local {
			out = append(out, n)
			return
		}
		for _, c := range n.Nodes {
			walk(c)
		}
	}
	walk(n)
	return out
}

type row struct {
	cells []node
}

type table struct {
	id, caption, section string
	rows                 []row
}

type book struct {
	subtitle string
	tables   map[string]*table // by id
	order    []string
	digest   string
	sections map[string]*section
}

type section struct {
	id, title string
	lists     []node
}

func parse(r io.Reader) (*book, error) {
	hash := sha256.New()
	decoder := xml.NewDecoder(io.TeeReader(r, hash))
	b := &book{tables: map[string]*table{}, sections: map[string]*section{}}
	var stack []string
	var sections []*section
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if end, ok := token.(xml.EndElement); ok {
			if end.Name.Local == "section" && len(sections) > 0 {
				sections = sections[:len(sections)-1]
			}
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "section":
			id := ""
			for _, a := range start.Attr {
				if a.Name.Local == "id" {
					id = a.Value
				}
			}
			s := &section{id: id}
			b.sections[id] = s
			sections = append(sections, s)
			stack = append(stack, "section")
		case "title", "variablelist":
			var n node
			if err := decoder.DecodeElement(&n, &start); err != nil {
				return nil, err
			}
			if len(sections) > 0 && len(stack) > 0 && stack[len(stack)-1] == "section" {
				s := sections[len(sections)-1]
				if start.Name.Local == "title" {
					s.title = n.orderedText()
				} else {
					s.lists = append(s.lists, n)
				}
			}
		case "subtitle":
			var n node
			if err := decoder.DecodeElement(&n, &start); err != nil {
				return nil, err
			}
			if b.subtitle == "" {
				b.subtitle = n.text()
			}
		case "table":
			var n node
			if err := decoder.DecodeElement(&n, &start); err != nil {
				return nil, err
			}
			t := &table{id: n.attr("id")}
			if len(sections) > 0 {
				t.section = sections[len(sections)-1].id
			}
			if captions := n.find("caption"); len(captions) > 0 {
				t.caption = captions[0].text()
			}
			for _, body := range n.find("tbody") {
				for _, tr := range body.find("tr") {
					t.rows = append(t.rows, row{cells: tr.find("td")})
				}
			}
			if t.id != "" {
				b.tables[t.id] = t
				b.order = append(b.order, t.id)
			}
		default:
			stack = append(stack, start.Name.Local)
		}
	}
	b.digest = hex.EncodeToString(hash.Sum(nil))
	return b, nil
}

var tagPattern = regexp.MustCompile(`^\(([0-9A-Fa-f]{4}),([0-9A-Fa-f]{4})\)$`)

// validType matches the Attribute Types of PS3.5 7.4. Tables of other
// shapes (the DIMSE-N print tables, for example) have no Type column; their
// third cell is not a Type and is dropped.
var validType = regexp.MustCompile(`^(1|1C|2|2C|3)$`)

func attributeType(cell string) string {
	if validType.MatchString(cell) {
		return cell
	}
	return ""
}

type attribute struct {
	tag, typ string
}

// ruleRow is one row of a Module or Macro table: an Attribute at a nesting
// depth (0 for top level, 1 for the Items of the Sequence above, ...) or an
// Include of another table at that depth. Tag is empty for Attributes
// without a plain tag, such as repeating groups, whose subtree is opaque.
type ruleRow struct {
	depth                        int
	tag, typ, include, condition string
}

// maxConditionLength bounds the condition text kept for a 1C or 2C row.
const maxConditionLength = 400

// condition extracts the condition of a 1C or 2C Attribute from its
// description: the text from "Required" (or "Shall be present"), bounded.
func condition(description string) string {
	i := strings.Index(description, "Required")
	if j := strings.Index(description, "Shall be present"); i < 0 || (j >= 0 && j < i) {
		i = j
	}
	if i < 0 {
		return ""
	}
	text := description[i:]
	if len(text) > maxConditionLength {
		cut := maxConditionLength
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	return text
}

// tableName is a table's caption without the trailing " Attributes".
func (b *book) tableName(id string) string {
	if t := b.tables[id]; t != nil {
		return strings.TrimSuffix(t.caption, " Attributes")
	}
	return ""
}

// rows returns every row of a Module or Macro table, keeping Includes as
// references.
func (b *book) rows(id string) []ruleRow {
	t := b.tables[id]
	var out []ruleRow
	for _, r := range t.rows {
		if len(r.cells) == 0 {
			continue
		}
		first := r.cells[0].text()
		depth := 0
		for strings.HasPrefix(first, ">") {
			depth++
			first = strings.TrimSpace(first[1:])
		}
		if strings.HasPrefix(first, "Include") {
			// "Include <table> if and only if Value Type (0040,A040) is NUM"
			// keeps its condition in the Include cell.
			cond := ""
			if i := strings.Index(first, " if "); i >= 0 {
				cond = "Required if " + strings.TrimPrefix(first[i+len(" if "):], "and only if ")
			}
			for _, link := range r.cells[0].links() {
				if name := b.tableName(link); strings.HasPrefix(link, "table_") && name != "" {
					out = append(out, ruleRow{depth: depth, include: name, condition: condition(cond)})
				}
			}
			continue
		}
		if len(r.cells) < 3 {
			continue
		}
		row := ruleRow{depth: depth, typ: attributeType(r.cells[2].text())}
		if m := tagPattern.FindStringSubmatch(r.cells[1].text()); m != nil {
			row.tag = strings.ToUpper(m[1] + m[2])
		}
		if (row.typ == "1C" || row.typ == "2C") && len(r.cells) > 3 {
			row.condition = condition(r.cells[3].text())
		}
		out = append(out, row)
	}
	return out
}

// topLevel returns the table's depth-0 Attributes, expanding depth-0 Include
// rows recursively.
func (b *book) topLevel(id string, seen map[string]bool) []attribute {
	if seen[id] {
		return nil
	}
	seen[id] = true
	defer delete(seen, id)
	t := b.tables[id]
	if t == nil {
		return nil
	}
	var out []attribute
	for _, r := range t.rows {
		if len(r.cells) == 0 {
			continue
		}
		first := r.cells[0].text()
		if strings.HasPrefix(first, ">") {
			continue
		}
		if strings.HasPrefix(first, "Include") {
			for _, link := range r.cells[0].links() {
				if strings.HasPrefix(link, "table_") {
					out = append(out, b.topLevel(link, seen)...)
				}
			}
			continue
		}
		if len(r.cells) < 3 {
			continue
		}
		m := tagPattern.FindStringSubmatch(r.cells[1].text())
		if m == nil {
			continue // repeating groups such as (60xx,3000)
		}
		out = append(out, attribute{tag: strings.ToUpper(m[1] + m[2]), typ: attributeType(r.cells[2].text())})
	}
	return out
}

func generate(b *book) ([]byte, error) {
	subtitleFields := strings.Fields(b.subtitle)
	if len(subtitleFields) < 3 {
		return nil, fmt.Errorf("iodmodulegen: subtitle %q must contain at least three fields", b.subtitle)
	}
	modules := map[string][]attribute{}
	iods := map[string][][3]string{}
	for _, id := range b.order {
		t := b.tables[id]
		switch {
		case strings.HasSuffix(t.caption, " Module Attributes"), strings.HasSuffix(t.caption, " Macro Attributes"):
			name := strings.TrimSuffix(t.caption, " Attributes")
			// A Module of repeating groups only (Overlay Plane) is kept
			// empty, so that it still resolves.
			if _, dup := modules[name]; !dup {
				modules[name] = b.topLevel(id, map[string]bool{})
			}
		case strings.HasSuffix(t.caption, " IOD Modules"):
			name := strings.TrimSuffix(t.caption, " IOD Modules")
			var uses [][3]string
			for _, r := range t.rows {
				cells := make([]string, 0, len(r.cells))
				for _, c := range r.cells {
					if text := c.text(); text != "" {
						cells = append(cells, text)
					}
				}
				// IE | Module | Usage, or Module | Usage under a spanned IE.
				if len(cells) < 2 {
					continue
				}
				module, usage := cells[len(cells)-2], cells[len(cells)-1]
				cond := ""
				if firstWord(usage) == "C" {
					cond = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(usage, "C"), " -"))
				}
				uses = append(uses, [3]string{module, usage, cond})
			}
			if len(uses) > 0 {
				iods[name] = uses
			}
		}
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "// Code generated by cmd/iodmodulegen from the %s DocBook; DO NOT EDIT.\n\n", b.subtitle)
	buf.WriteString("package iodmodules\n\n")
	fmt.Fprintf(&buf, "const (\n\tStandardVersion = %q\n\tSourceSHA256 = %q\n)\n\n", subtitleFields[1]+" "+subtitleFields[2], b.digest)
	buf.WriteString("var moduleAttributes = map[string][]Attribute{\n")
	names := make([]string, 0, len(modules))
	for name := range modules {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&buf, "\t%q: {", name)
		for _, a := range modules[name] {
			fmt.Fprintf(&buf, "{0x%s, %q}, ", a.tag, a.typ)
		}
		buf.WriteString("},\n")
	}
	buf.WriteString("}\n\nvar iodModules = map[string][]ModuleUsage{\n")
	names = names[:0]
	for name := range iods {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&buf, "\t%q: {", name)
		for _, use := range iods[name] {
			fmt.Fprintf(&buf, "{%q, %q, %q}, ", use[0], firstWord(use[1]), use[2])
		}
		buf.WriteString("},\n")
	}
	buf.WriteString("}\n")
	return format.Source(buf.Bytes())
}

// generateRules writes every row of every Module and Macro table, for the
// IOD validator (validation/iod).
func generateRules(b *book) ([]byte, error) {
	tables := map[string][]ruleRow{}
	for _, id := range b.order {
		t := b.tables[id]
		if !strings.HasSuffix(t.caption, " Module Attributes") && !strings.HasSuffix(t.caption, " Macro Attributes") {
			continue
		}
		if name := b.tableName(id); tables[name] == nil {
			tables[name] = b.rows(id)
		}
	}
	// Tables reached only through Include rows (captions without Module or
	// Macro) are added too, so that every Include resolves.
	for changed := true; changed; {
		changed = false
		for _, id := range b.order {
			name := b.tableName(id)
			if name == "" || tables[name] != nil {
				continue
			}
			for _, rows := range tables {
				for _, r := range rows {
					if r.include == name {
						tables[name] = b.rows(id)
						changed = true
					}
				}
				if tables[name] != nil {
					break
				}
			}
		}
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "// Code generated by cmd/iodmodulegen from the %s DocBook; DO NOT EDIT.\n\n", b.subtitle)
	buf.WriteString("package iodmodules\n\n")
	buf.WriteString("var moduleRows = map[string][]Row{\n")
	names := make([]string, 0, len(tables))
	for name := range tables {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&buf, "\t%q: {\n", name)
		for _, r := range tables[name] {
			tag := "0"
			if r.tag != "" {
				tag = "0x" + r.tag
			}
			fmt.Fprintf(&buf, "\t\t{%d, %s, %q, %q, %q},\n", r.depth, tag, r.typ, r.include, r.condition)
		}
		buf.WriteString("\t},\n")
	}
	buf.WriteString("}\n")
	return format.Source(buf.Bytes())
}

// firstWord keeps the usage letter (M, U or C) of a usage cell.
func firstWord(usage string) string {
	if fields := strings.Fields(usage); len(fields) > 0 {
		return fields[0]
	}
	return usage
}

var (
	htmlRow   = regexp.MustCompile(`(?s)<tr[^>]*>(.*?)</tr>`)
	htmlCell  = regexp.MustCompile(`(?s)<td[^>]*>(.*?)</td>`)
	htmlTag   = regexp.MustCompile(`<[^>]+>`)
	htmlTitle = regexp.MustCompile(`(?s)<title>(.*?)</title>`)
)

// generateBodyParts projects PS3.16 Annex L: each Body Part Examined
// Defined Term with the first anatomic region code listed for it.
func generateBodyParts(page []byte, pkg string) ([]byte, error) {
	title := ""
	if m := htmlTitle.FindSubmatch(page); m != nil {
		title = strings.Join(strings.Fields(html.UnescapeString(string(m[1]))), " ")
	}
	if !strings.Contains(title, "Body Part Examined") {
		return nil, fmt.Errorf("not the PS3.16 Annex L page: %q", title)
	}
	version := ""
	if m := regexp.MustCompile(`PS3\.16 (\d{4}[a-z]?)`).FindSubmatch(page); m != nil {
		version = "PS3.16 " + string(m[1])
	}
	type code struct{ scheme, value, meaning string }
	parts := map[string]code{}
	for _, r := range htmlRow.FindAllSubmatch(page, -1) {
		var cells []string
		for _, c := range htmlCell.FindAllSubmatch(r[1], -1) {
			cells = append(cells, strings.Join(strings.Fields(html.UnescapeString(htmlTag.ReplaceAllString(string(c[1]), " "))), " "))
		}
		if len(cells) < 4 || cells[3] == "" || cells[0] == "" || cells[1] == "" {
			continue
		}
		if _, seen := parts[cells[3]]; !seen {
			parts[cells[3]] = code{cells[0], cells[1], cells[2]}
		}
	}
	if len(parts) < 100 {
		return nil, fmt.Errorf("only %d Body Part Examined terms found", len(parts))
	}
	terms := make([]string, 0, len(parts))
	for term := range parts {
		terms = append(terms, term)
	}
	sort.Strings(terms)
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "// Code generated by cmd/iodmodulegen from DICOM %s Annex L; DO NOT EDIT.\n\n", version)
	fmt.Fprintf(&buf, "package %s\n\n", pkg)
	buf.WriteString("// bodyPartRegions maps each Body Part Examined Defined Term of PS3.16\n// Annex L to its anatomic region code.\n")
	buf.WriteString("var bodyPartRegions = map[string][3]string{\n")
	for _, term := range terms {
		c := parts[term]
		fmt.Fprintf(&buf, "\t%q: {%q, %q, %q},\n", term, c.scheme, c.value, c.meaning)
	}
	buf.WriteString("}\n")
	return format.Source(buf.Bytes())
}
