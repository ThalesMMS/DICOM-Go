package encapdoc

import (
	"context"
	"strings"
)

const (
	cdaLinkPath     = cdaBodyPath + "//section/text//linkHtml/@href"
	cdaFootnotePath = cdaBodyPath + "//section/text//footnoteRef/@IDREF"
	cdaEntryPath    = cdaBodyPath + "//section/entry//reference/@value"
)

type cdaNarrativeAnchor struct {
	section   *cdaXMLNode
	footnote  bool
	narrative bool
}

type cdaLocalReference struct {
	from    int
	target  string
	path    string
	section *cdaXMLNode // Non-nil for entry references to narrative in this section.
}

type cdaNarrativeGraph struct {
	ctx        context.Context
	report     *CDAValidationReport
	byID       map[string]int
	anchors    []cdaNarrativeAnchor
	references []cdaLocalReference
}

func (r *CDAValidationReport) validateNarrativeLinks(ctx context.Context, body *cdaXMLNode) error {
	g := cdaNarrativeGraph{
		ctx:     ctx,
		report:  r,
		byID:    make(map[string]int),
		anchors: make([]cdaNarrativeAnchor, 1), // Index zero denotes no enclosing anchor.
	}
	if err := g.sections(body); err != nil {
		return err
	}
	return g.validateReferences()
}

func (g *cdaNarrativeGraph) sections(parent *cdaXMLNode) error {
	for _, part := range parent.parts {
		if err := g.ctx.Err(); err != nil {
			return err
		}
		if part.node == nil || !part.node.is("component") {
			continue
		}
		for _, child := range part.node.parts {
			if child.node != nil && child.node.is("section") {
				if err := g.section(child.node); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (g *cdaNarrativeGraph) section(section *cdaXMLNode) error {
	owner := g.addAnchor(section, section, false)
	for _, part := range section.parts {
		if err := g.ctx.Err(); err != nil {
			return err
		}
		if part.node == nil {
			continue
		}
		switch {
		case part.node.is("text"):
			if err := g.narrative(part.node, section, owner); err != nil {
				return err
			}
		case part.node.is("entry"):
			if err := g.entry(part.node, section, false); err != nil {
				return err
			}
		}
	}
	return g.sections(section)
}

func (g *cdaNarrativeGraph) addAnchor(node, section *cdaXMLNode, narrative bool) int {
	id := node.attribute("ID")
	if id == "" {
		return 0
	}
	if g.byID[id] != 0 {
		g.report.add("duplicate-narrative-id", cdaBodyPath+"//@ID")
		g.byID[id] = -1
		return 0
	}
	index := len(g.anchors)
	g.byID[id] = index
	g.anchors = append(g.anchors, cdaNarrativeAnchor{section: section, footnote: node.is("footnote"), narrative: narrative})
	return index
}

func (g *cdaNarrativeGraph) narrative(node, section *cdaXMLNode, owner int) error {
	if err := g.ctx.Err(); err != nil {
		return err
	}
	if anchor := g.addAnchor(node, section, true); anchor != 0 {
		owner = anchor
	}
	switch {
	case node.is("linkHtml"):
		if target, local := strings.CutPrefix(node.attribute("href"), "#"); local {
			g.references = append(g.references, cdaLocalReference{from: owner, target: target, path: cdaLinkPath})
		}
	case node.is("footnoteRef"):
		g.references = append(g.references, cdaLocalReference{from: owner, target: node.attribute("IDREF"), path: cdaFootnotePath})
	}
	for _, part := range node.parts {
		if part.node != nil && part.node.name.Space == cdaNamespace {
			if err := g.narrative(part.node, section, owner); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *cdaNarrativeGraph) entry(node, section *cdaXMLNode, textReference bool) error {
	if err := g.ctx.Err(); err != nil {
		return err
	}
	if node.is("observationMedia") || node.is("regionOfInterest") {
		g.addAnchor(node, section, false)
	}
	if textReference && node.is("reference") {
		if target, local := strings.CutPrefix(node.attribute("value"), "#"); local {
			g.references = append(g.references, cdaLocalReference{target: target, path: cdaEntryPath, section: section})
		}
	}
	for _, part := range node.parts {
		if part.node != nil && part.node.name.Space == cdaNamespace {
			if err := g.entry(part.node, section, node.is("originalText") || node.is("text")); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *cdaNarrativeGraph) validateReferences() error {
	edges := make([][]int, len(g.anchors))
	incoming := make([]int, len(g.anchors))
	for _, ref := range g.references {
		if err := g.ctx.Err(); err != nil {
			return err
		}
		to := g.byID[ref.target]
		switch {
		case to == 0:
			g.report.add("dangling-narrative-reference", ref.path)
		case to < 0:
			g.report.add("ambiguous-narrative-reference", ref.path)
		case ref.path == cdaFootnotePath && !g.anchors[to].footnote:
			g.report.add("wrong-narrative-target", ref.path)
		case ref.section != nil && (!g.anchors[to].narrative || g.anchors[to].section != ref.section):
			g.report.add("wrong-narrative-target", ref.path)
		case ref.from != 0:
			edges[ref.from] = append(edges[ref.from], to)
			incoming[to]++
		}
	}

	// Remove all vertices with no incoming edges. Any remaining vertex
	// requires a cycle, even when the cycle reaches other vertices.
	queue := make([]int, 0, len(g.anchors))
	for index, count := range incoming {
		if count == 0 {
			queue = append(queue, index)
		}
	}
	for head := 0; head < len(queue); head++ {
		if err := g.ctx.Err(); err != nil {
			return err
		}
		for _, to := range edges[queue[head]] {
			incoming[to]--
			if incoming[to] == 0 {
				queue = append(queue, to)
			}
		}
	}
	if len(queue) != len(g.anchors) {
		g.report.add("cyclic-narrative-reference", cdaBodyPath)
	}
	return nil
}
