package main

import (
	"bytes"
	"fmt"
	"go/format"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/iodmodules"
)

var listTitle = regexp.MustCompile(`^(Enumerated Values|Defined Terms)(?: for Value ([1-9][0-9]*))?:?$`)
var conditionalTitle = regexp.MustCompile(`^(Enumerated Values|Defined Terms) if (.+?):?$`)
var titleAtom = regexp.MustCompile(`^.+?\(([0-9A-Fa-f]{4}),([0-9A-Fa-f]{4})\)(?: Value ([1-9][0-9]*))? is (.+)$`)
var presentAtom = regexp.MustCompile(`^.+?\(([0-9A-Fa-f]{4}),([0-9A-Fa-f]{4})\)$`)
var titleValue = regexp.MustCompile(`^[A-Z0-9_ ]+$`)
var termRelation = regexp.MustCompile(`(?i)\b(if|when|otherwise)\b`)
var numericRange = regexp.MustCompile(`(?i)^[+-]?[0-9]+(?:\.[0-9]+)? (?:or|through|to) [+-]?[0-9]+(?:\.[0-9]+)?$`)
var qualifiedLead = regexp.MustCompile(`^(?:For .+:|When .+(?:Enumerated Values|Defined Terms).*)$`)

func direct(n node, name string) []node {
	var out []node
	for _, child := range n.Nodes {
		if child.XMLName.Local == name {
			out = append(out, child)
		}
	}
	return out
}
func firstText(n node, name string) string {
	for _, c := range direct(n, name) {
		return c.orderedText()
	}
	return ""
}
func packedTag(g, e string) uint32 { v, _ := strconv.ParseUint(g+e, 16, 32); return uint32(v) }

// titlePredicate compiles only the tiny title grammar audited in the pinned
// DocBook. It deliberately does not accept arbitrary conditions or descriptions.
func titlePredicate(text string) (iodmodules.ValuePredicate, bool) {
	if strings.Contains(text, " and ") {
		p := iodmodules.ValuePredicate{Operator: "and"}
		for _, part := range strings.Split(text, " and ") {
			c, ok := titlePredicate(part)
			if !ok {
				return iodmodules.ValuePredicate{}, false
			}
			p.Children = append(p.Children, c)
		}
		return p, true
	}
	if body, ok := strings.CutSuffix(text, " is present"); ok {
		p := iodmodules.ValuePredicate{Operator: "or"}
		for _, part := range strings.Split(body, " or ") {
			m := presentAtom.FindStringSubmatch(part)
			if m == nil {
				return iodmodules.ValuePredicate{}, false
			}
			p.Children = append(p.Children, iodmodules.ValuePredicate{Operator: "present", Tag: packedTag(m[1], m[2])})
		}
		if len(p.Children) == 1 {
			return p.Children[0], true
		}
		return p, true
	}
	m := titleAtom.FindStringSubmatch(text)
	if m == nil {
		return iodmodules.ValuePredicate{}, false
	}
	p := iodmodules.ValuePredicate{Operator: "one-of", Tag: packedTag(m[1], m[2])}
	p.Position, _ = strconv.Atoi(m[3])
	for _, value := range strings.Split(strings.ReplaceAll(m[4], ", ", " or "), " or ") {
		value = strings.TrimSpace(value)
		if !titleValue.MatchString(value) {
			return iodmodules.ValuePredicate{}, false
		}
		p.Values = append(p.Values, value)
	}
	return p, len(p.Values) > 0
}

func constraintList(vl node, source iodmodules.ConstraintSource, rule string) iodmodules.ValueConstraint {
	c := iodmodules.ValueConstraint{Source: source, Rule: rule}
	title := firstText(vl, "title")
	kind := ""
	if m := listTitle.FindStringSubmatch(title); m != nil {
		kind = m[1]
		c.Position, _ = strconv.Atoi(m[2])
	} else if m := conditionalTitle.FindStringSubmatch(title); m != nil {
		kind = m[1]
		var ok bool
		c.Predicate, ok = titlePredicate(strings.TrimSuffix(m[2], ":"))
		if !ok {
			c.Kind = "manual"
			c.Reason = "uncompiled title condition"
			return c
		}
	} else {
		c.Kind = "manual"
		c.Reason = "unproved title qualifier"
		return c
	}
	c.Kind = map[string]string{"Enumerated Values": "enumerated", "Defined Terms": "defined"}[kind]
	for _, entry := range direct(vl, "varlistentry") {
		for _, term := range direct(entry, "term") {
			value := term.orderedText()
			if value == "" || len(value) > 256 || strings.Contains(value, "\\") || numericRange.MatchString(value) {
				c.Kind = "manual"
				c.Reason = "non-atomic literal term"
				return c
			}
			for _, r := range value {
				if r < 0x20 || r > 0x7e {
					c.Kind = "manual"
					c.Reason = "unproved literal normalization"
					return c
				}
			}
			c.Values = append(c.Values, value)
		}
	}
	if len(c.Values) == 0 {
		c.Kind = "manual"
		c.Reason = "empty source list"
	}
	if source.Tag == 0 {
		c.Kind = "manual"
		c.Reason = "repeating or non-attribute row"
	}
	return c
}

// selectedRuleTables uses the same first-caption and Include closure as
// generateRules. Each restriction is attached to its actual emitted row index.
func selectedRuleTables(b *book) map[string]*table {
	result := map[string]*table{}
	for _, id := range b.order {
		t := b.tables[id]
		if strings.HasSuffix(t.caption, " Module Attributes") || strings.HasSuffix(t.caption, " Macro Attributes") {
			name := b.tableName(id)
			if result[name] == nil {
				result[name] = t
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, id := range b.order {
			name := b.tableName(id)
			if name == "" || result[name] != nil {
				continue
			}
			found := false
			for _, t := range result {
				for _, r := range b.rows(t.id) {
					if r.include == name {
						found = true
						break
					}
				}
				if found {
					break
				}
			}
			if found {
				result[name] = b.tables[id]
				changed = true
			}
		}
	}
	return result
}

// sourceParentPath proves a supplemental row's ancestor chain from explicit
// source depth/tag rows. Every ancestor must be a dictionary SQ; an Include or
// opaque/missing ancestor cannot silently become a path.
func sourceParentPath(t *table, ordinal, depth int) ([]uint32, bool) {
	var parents []uint32
	for _, r := range t.rows[:ordinal] {
		if len(r.cells) == 0 {
			continue
		}
		label := r.cells[0].orderedText()
		d := 0
		for strings.HasPrefix(label, ">") {
			d++
			label = strings.TrimSpace(label[1:])
		}
		if d > len(parents) {
			parents = nil
			continue
		}
		var tag uint32
		if !strings.HasPrefix(label, "Include") && len(r.cells) >= 3 && attributeType(r.cells[2].orderedText()) != "" {
			if m := tagPattern.FindStringSubmatch(r.cells[1].orderedText()); m != nil {
				tag = packedTag(m[1], m[2])
			}
		}
		parents = append(parents[:d], tag)
	}
	if depth > len(parents) {
		return nil, false
	}
	path := append([]uint32(nil), parents[:depth]...)
	for _, tag := range path {
		entry, ok := std.Dictionary.ByTag(core.NewTag(uint16(tag>>16), uint16(tag)))
		if !ok || entry.VR != core.VRSQ {
			return nil, false
		}
	}
	return path, true
}

func constraintsForTable(b *book, t *table, edition string) map[int][]iodmodules.ValueConstraint {
	out := map[int][]iodmodules.ValueConstraint{}
	index := 0
	for ordinal, r := range t.rows {
		if len(r.cells) == 0 {
			continue
		}
		label := strings.TrimLeft(r.cells[0].orderedText(), "> ")
		// Bind to the legacy row projection exactly, including its mixed-text
		// emission decisions. Source ordering is used only for constraint text.
		emittedLabel := r.cells[0].text()
		depth := 0
		for strings.HasPrefix(emittedLabel, ">") {
			depth++
			emittedLabel = strings.TrimSpace(emittedLabel[1:])
		}
		if strings.HasPrefix(emittedLabel, "Include") {
			// The legacy projection also treats names beginning "Includes" as
			// Include directives. Preserve its output, but expose typed rows
			// lost by that preexisting ambiguity as supplemental value-only rows.
			if len(r.cells) >= 4 && attributeType(r.cells[2].orderedText()) != "" {
				if m := tagPattern.FindStringSubmatch(r.cells[1].orderedText()); m != nil {
					source := iodmodules.ConstraintSource{Table: b.tableName(t.id), TableID: t.id, Row: ordinal + 1, Index: -1, Depth: uint8(depth), Tag: packedTag(m[1], m[2]), Edition: edition, SHA256: b.digest}
					path, proved := sourceParentPath(t, ordinal, depth)
					for i, vl := range direct(r.cells[3], "variablelist") {
						title := firstText(vl, "title")
						if strings.HasPrefix(title, "Enumerated Values") || strings.HasPrefix(title, "Defined Terms") {
							c := constraintList(vl, source, fmt.Sprintf("%s/row-%d/list-%d", t.id, ordinal+1, i+1))
							c.ParentPath = path
							c.AttributeType = attributeType(r.cells[2].orderedText())
							for _, sibling := range r.cells[3].Nodes {
								if sibling.XMLName.Local == "variablelist" && sibling.ordered == vl.ordered {
									break
								}
								if sibling.XMLName.Local == "para" && qualifiedLead.MatchString(sibling.orderedText()) {
									c.Kind = "manual"
									c.Reason = "preceding prose qualifies the list scope"
								}
							}
							if !proved {
								c.Kind = "manual"
								c.Reason = "supplemental source nesting could not be proved"
							}
							out[-1] = append(out[-1], c)
							for _, entry := range direct(vl, "varlistentry") {
								if termRelation.MatchString(firstText(entry, "listitem")) {
									out[-1] = append(out[-1], iodmodules.ValueConstraint{Kind: "manual", ParentPath: path, AttributeType: c.AttributeType, Rule: c.Rule + "/relations", Reason: "term semantics may impose additional contextual relations", Source: source})
									break
								}
							}
						}
					}
				}
			}
			for _, link := range r.cells[0].links() {
				if strings.HasPrefix(link, "table_") && b.tableName(link) != "" {
					index++
				}
			}
			continue
		}
		if len(r.cells) < 3 {
			continue
		}
		current := index
		index++
		if len(r.cells) < 4 {
			continue
		}
		tag := uint32(0)
		if m := tagPattern.FindStringSubmatch(r.cells[1].text()); m != nil {
			tag = packedTag(m[1], m[2])
		}
		source := iodmodules.ConstraintSource{Table: b.tableName(t.id), TableID: t.id, Row: ordinal + 1, Index: current, Depth: uint8(depth), Tag: tag, Edition: edition, SHA256: b.digest}
		desc := r.cells[3]
		lists := direct(desc, "variablelist")
		appendList := func(vl node, source iodmodules.ConstraintSource, rule string) {
			c := constraintList(vl, source, rule)
			// A plain list title can still be qualified by preceding prose.
			// Preserve the two audited forms as manual scope, not a global set.
			for _, sibling := range desc.Nodes {
				if sibling.XMLName.Local == "variablelist" && sibling.ordered == vl.ordered {
					break
				}
				if sibling.XMLName.Local == "para" && qualifiedLead.MatchString(sibling.orderedText()) {
					c.Kind = "manual"
					c.Reason = "preceding prose qualifies the list scope"
				}
			}
			if attributeType(r.cells[2].orderedText()) == "" {
				c.Kind = "manual"
				c.Reason = "row without Attribute Type"
			}
			out[current] = append(out[current], c)
			for _, entry := range direct(vl, "varlistentry") {
				if termRelation.MatchString(firstText(entry, "listitem")) {
					out[current] = append(out[current], iodmodules.ValueConstraint{Kind: "manual", Rule: rule + "/relations", Reason: "term semantics may impose additional contextual relations", Source: source})
					break
				}
			}
		}
		for i, vl := range lists {
			title := firstText(vl, "title")
			if strings.HasPrefix(title, "Enumerated Values") || strings.HasPrefix(title, "Defined Terms") {
				appendList(vl, source, fmt.Sprintf("%s/row-%d/list-%d", t.id, ordinal+1, i+1))
			}
		}
		for _, para := range direct(desc, "para") {
			text := para.orderedText()
			// These two PI references were audited against section identity and scope.
			for _, link := range para.links() {
				known := (t.id == "table_C.8-3" && tag == 0x00280004 && link == "sect_C.8.2.1.1.3") || (t.id == "table_C.7-11c" && tag == 0x00280004 && link == "sect_C.7.6.3.1.2")
				if known {
					s := b.sections[link]
					if s == nil || s.title != label {
						out[current] = append(out[current], iodmodules.ValueConstraint{Kind: "manual", Rule: link, Reason: "referenced section identity not proved", Source: source})
						continue
					}
					sourced := source
					sourced.Section = link
					for i, vl := range s.lists {
						title := firstText(vl, "title")
						if strings.HasPrefix(title, "Enumerated Values") || strings.HasPrefix(title, "Defined Terms") {
							appendList(vl, sourced, fmt.Sprintf("%s/%s/list-%d", t.id, link, i+1))
						}
					}
				} else if strings.HasPrefix(link, "sect_") && (strings.Contains(text, "specialization") || strings.Contains(text, "Enumerated Values") || strings.Contains(text, "Defined Terms") || label == "Photometric Interpretation") {
					sourced := source
					sourced.Section = link
					out[current] = append(out[current], iodmodules.ValueConstraint{Kind: "manual", Rule: fmt.Sprintf("%s/row-%d/ref-%s", t.id, ordinal+1, link), Reason: "referenced value constraint scope not proved", Source: sourced})
				}
			}
			if t.id == "table_C.7.6.16-3" && tag == 0x00209157 && strings.Contains(text, "The number of Values is equal to the number of Items of the Dimension Index Sequence") {
				out[current] = append(out[current], iodmodules.ValueConstraint{Kind: "count", CountTag: 0x00209222, Rule: fmt.Sprintf("%s/row-%d/dimension-index-count", t.id, ordinal+1), Source: source})
			} else if strings.Contains(text, "number of Values") || strings.Contains(text, "Value Multiplicity") || strings.Contains(text, "same number of Values") {
				out[current] = append(out[current], iodmodules.ValueConstraint{Kind: "manual", Rule: fmt.Sprintf("%s/row-%d/multiplicity", t.id, ordinal+1), Reason: "multiplicity prose not compiled", Source: source})
			}
		}
	}
	return out
}

func generateConstraints(b *book) ([]byte, error) {
	fields := strings.Fields(b.subtitle)
	if len(fields) < 3 {
		return nil, fmt.Errorf("iodmodulegen: invalid subtitle for constraints")
	}
	edition := fields[1] + " " + fields[2]
	tables := selectedRuleTables(b)
	names := make([]string, 0, len(tables))
	for name := range tables {
		names = append(names, name)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "// Code generated by cmd/iodmodulegen from the %s DocBook; DO NOT EDIT.\n\npackage iodmodules\n\n", b.subtitle)
	fmt.Fprintf(&buf, "const ConstraintSourceEdition = %q\nconst ConstraintSourceSHA256 = %q\n\nvar valueConstraints = map[string]map[int][]ValueConstraint{\n", edition, b.digest)
	for _, name := range names {
		constraints := constraintsForTable(b, tables[name], edition)
		if len(constraints) == 0 {
			continue
		}
		fmt.Fprintf(&buf, "%q: {\n", name)
		indexes := make([]int, 0, len(constraints))
		for index := range constraints {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		for _, index := range indexes {
			fmt.Fprintf(&buf, "%d: {\n", index)
			for _, c := range constraints[index] {
				fmt.Fprintf(&buf, "%#v,\n", c)
			}
			buf.WriteString("},\n")
		}
		buf.WriteString("},\n")
	}
	buf.WriteString("}\n")
	// %#v supplies explicit struct field names and escaped strings. These types
	// belong to the generated package, so remove only their package qualifier.
	return format.Source(bytes.ReplaceAll(buf.Bytes(), []byte("iodmodules."), nil))
}
