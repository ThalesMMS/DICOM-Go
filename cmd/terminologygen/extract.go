package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// node is a DocBook element with its children in document order: strings
// for character data, *node for elements.
type node struct {
	name     string
	attrs    map[string]string
	children []any
}

func (n *node) text() string {
	var b strings.Builder
	n.appendText(&b)
	return collapse(b.String())
}

func (n *node) appendText(b *strings.Builder) {
	for _, child := range n.children {
		switch c := child.(type) {
		case string:
			b.WriteString(c)
		case *node:
			c.appendText(b)
		}
	}
}

func (n *node) elements(name string) []*node {
	var out []*node
	for _, child := range n.children {
		if c, ok := child.(*node); ok && c.name == name {
			out = append(out, c)
		}
	}
	return out
}

// find returns the descendants named name, in document order.
func (n *node) find(name string) []*node {
	var out []*node
	var walk func(*node)
	walk = func(current *node) {
		for _, child := range current.children {
			if c, ok := child.(*node); ok {
				if c.name == name {
					out = append(out, c)
				}
				walk(c)
			}
		}
	}
	walk(n)
	return out
}

func (n *node) hasItalic() bool {
	for _, emphasis := range n.find("emphasis") {
		if emphasis.attrs["role"] == "italic" {
			return true
		}
	}
	return false
}

func collapse(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, " ", " ")), " ")
}

// readElement decodes the element start opened, with its descendants.
func readElement(decoder *xml.Decoder, start xml.StartElement) (*node, error) {
	current := &node{name: start.Name.Local, attrs: attrMap(start.Attr)}
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch t := token.(type) {
		case xml.StartElement:
			child, err := readElement(decoder, t)
			if err != nil {
				return nil, err
			}
			current.children = append(current.children, child)
		case xml.CharData:
			current.children = append(current.children, string(t))
		case xml.EndElement:
			return current, nil
		}
	}
}

func attrMap(attrs []xml.Attr) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		out[attr.Name.Local] = attr.Value
	}
	return out
}

// Term is one coded concept.
type Term struct {
	Scheme, Value, Meaning string
	Retired                bool
}

func (t Term) key() string { return t.Scheme + "\x00" + t.Value }

// ContextGroup is one CID of PS3.16 Annex B.
type ContextGroup struct {
	ID                  int
	Keyword, Name       string
	Extensible          bool
	Version, UID        string
	Retired             bool
	Includes            []int
	Codes               []Term
	WithoutMemberTables bool
}

// Extraction is everything the generators need from part16.xml.
type Extraction struct {
	Edition, SHA256 string
	DCM             []Term // Annex D, document order
	Others          []Term // non-DCM codes of the CIDs and templates
	ContextGroups   []ContextGroup
}

// codedTriplet matches EV/DT (value, scheme, "meaning") in template text.
var codedTriplet = regexp.MustCompile(`\b(?:EV|DT)\s*\(\s*([^,()"]+?)\s*,\s*([^,()"]+?)\s*,\s*"([^"]+)"\s*\)`)

// Extract reads the PS3.16 DocBook.
func Extract(r io.Reader) (Extraction, error) {
	hash := sha256.New()
	decoder := xml.NewDecoder(io.TeeReader(r, hash))
	decoder.Strict = true
	var out Extraction
	var paraDepth int
	var para strings.Builder
	cidMeanings := map[string]Term{}
	templateMeanings := map[string][]Term{}
	var order []string
	noteTerm := func(term Term, fromCID bool) {
		if term.Scheme == "DCM" || term.Scheme == "" || term.Value == "" {
			return
		}
		key := term.key()
		if _, seen := cidMeanings[key]; !seen && len(templateMeanings[key]) == 0 {
			order = append(order, key)
		}
		if fromCID {
			if _, seen := cidMeanings[key]; !seen {
				cidMeanings[key] = term
			}
			return
		}
		templateMeanings[key] = append(templateMeanings[key], term)
	}
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, err
		}
		switch t := token.(type) {
		case xml.StartElement:
			id := attrMap(t.Attr)["id"]
			switch {
			case t.Name.Local == "subtitle" && out.Edition == "":
				element, err := readElement(decoder, t)
				if err != nil {
					return out, err
				}
				out.Edition = strings.TrimSpace(strings.TrimPrefix(strings.SplitN(element.text(), " - ", 2)[0], "DICOM "))
			case t.Name.Local == "section" && strings.HasPrefix(id, "sect_CID_"):
				section, err := readElement(decoder, t)
				if err != nil {
					return out, err
				}
				group, err := contextGroup(section)
				if err != nil {
					return out, fmt.Errorf("%s: %w", id, err)
				}
				for _, term := range group.Codes {
					noteTerm(term, true)
				}
				out.ContextGroups = append(out.ContextGroups, group)
			case t.Name.Local == "table" && id == "table_D-1":
				table, err := readElement(decoder, t)
				if err != nil {
					return out, err
				}
				out.DCM, err = dcmTerms(table)
				if err != nil {
					return out, fmt.Errorf("table D-1: %w", err)
				}
			case t.Name.Local == "para":
				if paraDepth == 0 {
					para.Reset()
				}
				paraDepth++
			}
		case xml.CharData:
			if paraDepth > 0 {
				para.Write(t)
			}
		case xml.EndElement:
			if t.Name.Local == "para" && paraDepth > 0 {
				paraDepth--
				if paraDepth == 0 {
					for _, match := range codedTriplet.FindAllStringSubmatch(collapse(para.String()), -1) {
						noteTerm(Term{Scheme: match[2], Value: match[1], Meaning: match[3]}, false)
					}
				}
			}
		}
	}
	out.SHA256 = hex.EncodeToString(hash.Sum(nil))
	if out.Edition == "" || len(out.DCM) == 0 || len(out.ContextGroups) == 0 {
		return out, errors.New("not a PS3.16 DocBook: edition, Annex D or CIDs missing")
	}
	for _, key := range order {
		if term, ok := cidMeanings[key]; ok {
			out.Others = append(out.Others, term)
			continue
		}
		out.Others = append(out.Others, mostFrequent(templateMeanings[key]))
	}
	sort.SliceStable(out.Others, func(i, j int) bool {
		if out.Others[i].Scheme != out.Others[j].Scheme {
			return out.Others[i].Scheme < out.Others[j].Scheme
		}
		return false
	})
	sort.Slice(out.ContextGroups, func(i, j int) bool { return out.ContextGroups[i].ID < out.ContextGroups[j].ID })
	return out, nil
}

// mostFrequent is the meaning a template code has most often, the first on
// a tie.
func mostFrequent(terms []Term) Term {
	counts := map[string]int{}
	best := terms[0]
	for _, term := range terms {
		counts[term.Meaning]++
		if counts[term.Meaning] > counts[best.Meaning] {
			best = term
		}
	}
	return best
}

// dcmTerms reads table D-1: Code Value, Code Meaning, Definition, Notes;
// italic values are retired.
func dcmTerms(table *node) ([]Term, error) {
	var out []Term
	for _, body := range table.elements("tbody") {
		for _, row := range body.elements("tr") {
			cells := row.elements("td")
			if len(cells) < 2 {
				continue
			}
			value, meaning := cells[0].text(), cells[1].text()
			if value == "" || meaning == "" {
				return nil, fmt.Errorf("row without value or meaning: %q %q", value, meaning)
			}
			out = append(out, Term{Scheme: "DCM", Value: value, Meaning: meaning, Retired: cells[0].hasItalic()})
		}
	}
	return out, nil
}

var includeLink = regexp.MustCompile(`^sect_CID_(\d+)$`)

func contextGroup(section *node) (ContextGroup, error) {
	var group ContextGroup
	id, err := strconv.Atoi(strings.TrimPrefix(section.attrs["id"], "sect_CID_"))
	if err != nil {
		return group, err
	}
	group.ID = id
	if titles := section.elements("title"); len(titles) > 0 {
		group.Name = titles[0].text()
	}
	group.Retired = strings.Contains(strings.ToLower(group.Name), "(retired)")
	for _, list := range section.elements("variablelist") {
		for _, entry := range list.elements("varlistentry") {
			terms, items := entry.elements("term"), entry.elements("listitem")
			if len(terms) == 0 || len(items) == 0 {
				continue
			}
			value := items[0].text()
			switch strings.TrimSuffix(terms[0].text(), ":") {
			case "Keyword":
				group.Keyword = value
			case "Type":
				group.Extensible = strings.EqualFold(value, "Extensible")
			case "Version":
				group.Version = value
			case "UID":
				group.UID = value
			}
		}
	}
	tables := section.elements("table")
	if len(tables) == 0 {
		group.WithoutMemberTables = true
		return group, nil
	}
	for _, table := range tables {
		if err := readMemberTable(table, &group); err != nil {
			return group, err
		}
	}
	return group, nil
}

// readMemberTable adds the rows of one CID table: coded concepts and
// Include rows.
func readMemberTable(table *node, group *ContextGroup) error {
	columns := map[string]int{}
	for _, head := range table.elements("thead") {
		for _, row := range head.elements("tr") {
			for i, cell := range row.elements("th") {
				columns[cell.text()] = i
			}
		}
	}
	schemeColumn, hasScheme := columns["Coding Scheme Designator"]
	meaningColumn, hasMeaning := columns["Code Meaning"]
	valueColumn, hasValue := columns["Code Value"]
	if !hasValue {
		valueColumn, hasValue = columns["Long Code Value"]
	}
	if !hasValue {
		valueColumn, hasValue = columns["URN Code Value"]
	}
	for _, body := range table.elements("tbody") {
		for _, row := range body.elements("tr") {
			cells := row.elements("td")
			if len(cells) == 1 {
				for _, xref := range cells[0].find("xref") {
					if match := includeLink.FindStringSubmatch(xref.attrs["linkend"]); match != nil && strings.Contains(cells[0].text(), "Include") {
						included, _ := strconv.Atoi(match[1])
						group.Includes = append(group.Includes, included)
					}
				}
				continue
			}
			if !hasScheme || !hasValue || !hasMeaning {
				continue
			}
			if max(schemeColumn, valueColumn, meaningColumn) >= len(cells) {
				continue
			}
			term := Term{
				Scheme: cells[schemeColumn].text(), Value: cells[valueColumn].text(), Meaning: cells[meaningColumn].text(),
				Retired: cells[valueColumn].hasItalic(),
			}
			if term.Scheme == "" || term.Value == "" || term.Meaning == "" {
				continue
			}
			group.Codes = append(group.Codes, term)
		}
	}
	return nil
}
