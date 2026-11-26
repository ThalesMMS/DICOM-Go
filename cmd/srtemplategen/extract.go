package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
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
			if c.name == "xref" {
				b.WriteString(xrefText(c.attrs["linkend"]))
				continue
			}
			c.appendText(b)
		}
	}
}

var xrefTarget = regexp.MustCompile(`^sect_(TID|CID)_(\d+[A-Z]?)$`)

// xrefText renders an empty DocBook cross-reference: the template tables
// write "DTID 301" and "BCID 244" as the strength letter followed by an
// <xref linkend="sect_TID_301"/> or <xref linkend="sect_CID_244"/>. Other
// targets render as nothing, so notation that needs them is refused later.
func xrefText(linkend string) string {
	if match := xrefTarget.FindStringSubmatch(linkend); match != nil {
		if match[1] == "TID" {
			id, identifier, err := parseTemplateIdentifier(match[2])
			if err != nil {
				return ""
			}
			return "TID " + (Template{ID: id, Identifier: identifier}).identifier()
		}
		return match[1] + " " + match[2]
	}
	return ""
}

// cellText is the text of a table cell, its paragraphs joined by "; " (a
// Value Set Constraint cell lists one parameter binding per paragraph).
func (n *node) cellText() string {
	var paragraphs []string
	for _, para := range n.elements("para") {
		if text := para.text(); text != "" {
			paragraphs = append(paragraphs, text)
		}
	}
	if len(paragraphs) == 0 {
		return n.text()
	}
	return strings.Join(paragraphs, "; ")
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

func collapse(s string) string {
	s = strings.NewReplacer(" ", " ", "“", `"`, "”", `"`, "‘", "'", "’", "'").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

func readElement(decoder *xml.Decoder, start xml.StartElement) (*node, error) {
	current := &node{name: start.Name.Local, attrs: map[string]string{}}
	for _, attr := range start.Attr {
		current.attrs[attr.Name.Local] = attr.Value
	}
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

var (
	templateSection = regexp.MustCompile(`^sect_TID_(\d+[A-Z]?)$`)
	editionRe       = regexp.MustCompile(`^\d{4}[a-z]$`)
)

// Extract reads the PS3.16 DocBook (part16.xml): the edition from its
// subtitle and, from each section sect_TID_n, the template's title, its
// Type, Order and Root, and its template table, the one whose header has
// the NL, VT and Req Type columns. Condition texts are formalized when they
// use a recognized row-presence wording (deriveFormal).
func Extract(r io.Reader, date string) (Catalog, error) {
	hash := sha256.New()
	decoder := xml.NewDecoder(io.TeeReader(r, hash))
	decoder.Strict = true
	c := Catalog{Source: sourceDocBook, Extracted: date}
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return c, err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch {
		case start.Name.Local == "subtitle" && c.Edition == "":
			element, err := readElement(decoder, start)
			if err != nil {
				return c, err
			}
			edition := strings.SplitN(element.text(), " - ", 2)[0]
			c.Edition = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(edition, "DICOM "), "PS3.16 "))
		case start.Name.Local == "section":
			id := ""
			for _, attr := range start.Attr {
				if attr.Name.Local == "id" {
					id = attr.Value
				}
			}
			match := templateSection.FindStringSubmatch(id)
			if match == nil {
				if strings.HasPrefix(id, "sect_TID_") {
					return c, fmt.Errorf("invalid template section identifier %q", id)
				}
				continue
			}
			section, err := readElement(decoder, start)
			if err != nil {
				return c, err
			}
			template, ok, err := templateFromSection(section, match[1])
			if err != nil {
				return c, fmt.Errorf("%s: %w", id, err)
			}
			if ok {
				c.Templates = append(c.Templates, template)
			}
		}
	}
	c.SHA256 = hex.EncodeToString(hash.Sum(nil))
	if !editionRe.MatchString(c.Edition) || len(c.Templates) == 0 {
		return c, errors.New("not a PS3.16 DocBook: edition or template tables missing")
	}
	sortTemplates(c.Templates)
	return c, check(c)
}

// optionalTemplateColumns may be missing from a template table: a few
// templates (TID 3480, for instance) list only root-level rows and leave out
// the relationship column.
var optionalTemplateColumns = map[string]bool{"Rel with Parent": true}

var templateColumns = []string{"NL", "Rel with Parent", "VT", "Concept Name", "VM", "Req Type", "Condition", "Value Set Constraint"}

// templateFromSection reads one sect_TID_n section; ok is false for a
// section without a template table (a retired template, for instance).
func templateFromSection(section *node, number string) (Template, bool, error) {
	id, identifier, err := parseTemplateIdentifier(number)
	if err != nil {
		return Template{}, false, err
	}
	t := Template{ID: id, Identifier: identifier}
	if titles := section.elements("title"); len(titles) > 0 {
		t.Name = titles[0].text()
	}
	for _, list := range section.find("variablelist") {
		for _, entry := range list.elements("varlistentry") {
			terms, items := entry.elements("term"), entry.elements("listitem")
			if len(terms) == 0 || len(items) == 0 {
				continue
			}
			value := items[0].text()
			switch strings.TrimSuffix(terms[0].text(), ":") {
			case "Type":
				t.Type = value
			case "Order":
				t.Order = value
			case "Root":
				t.Root = value
			}
		}
	}
	for _, table := range section.find("table") {
		columns := map[string]int{}
		heads := table.find("thead")
		if len(heads) == 0 {
			continue
		}
		for _, row := range heads[0].find("tr") {
			for i, cell := range row.elements("th") {
				columns[cell.text()] = i
			}
		}
		if _, ok := columns["NL"]; !ok {
			continue
		}
		if _, ok := columns["Req Type"]; !ok {
			continue
		}
		for _, name := range templateColumns {
			if _, ok := columns[name]; !ok && !optionalTemplateColumns[name] {
				return t, false, fmt.Errorf("template table without column %q", name)
			}
		}
		for _, body := range table.elements("tbody") {
			for _, tr := range body.elements("tr") {
				cells := tr.elements("td")
				if len(cells) <= columns["Value Set Constraint"] {
					return t, false, fmt.Errorf("row with %d cells", len(cells))
				}
				cell := func(name string) string {
					index, ok := columns[name]
					if !ok {
						return ""
					}
					return cells[index].cellText()
				}
				row := Row{
					Label: cells[0].text(), NL: strings.ReplaceAll(cell("NL"), " ", ""), Rel: cell("Rel with Parent"),
					VT: cell("VT"), Concept: cell("Concept Name"), VM: cell("VM"), Req: cell("Req Type"),
					Condition: cell("Condition"), ValueSet: cell("Value Set Constraint"),
				}
				// "NUMERIC" is a slip of the 2026d text for NUM (TID 3480,
				// 15300, 15301, 15401).
				if row.VT == "NUMERIC" {
					row.VT = "NUM"
				}
				// Only MC and UC rows have a condition; the Condition column
				// of an M or U row is a note (TID 1008 row 5: "XOR Row 6").
				if row.Req == "MC" || row.Req == "UC" {
					row.Formal = deriveFormal(row.Label, row.Condition)
				}
				t.Rows = append(t.Rows, row)
			}
		}
		break
	}
	if len(t.Rows) == 0 {
		return t, false, nil
	}
	return t, true, nil
}
