package sr

import (
	"bufio"
	"fmt"
	"html"
	"io"
	"strconv"
	"strings"
)

// Presentation block kinds describe layout without replacing SR value types.
const (
	PresentationSection   = "section"
	PresentationNarrative = "narrative"
	PresentationItem      = "item"
	PresentationInline    = "inline"
	PresentationReference = "reference"
)

// PresentationBlock is a content item in a semantic, preorder presentation.
// Path and Depth retain the same ordinals as RenderedItem. Values are plain
// text; consumers must escape them for their output medium.
type PresentationBlock struct {
	Path, Kind, Relationship string
	Depth                    int
	ValueType                ValueType
	Concept, Value           string
	ReferenceTo              string
	// InNarrative marks an immediate non-container CONTAINS child of a
	// CONTINUOUS container, including a by-reference relationship. Consumers
	// can group these blocks without interpreting the document tree again.
	InNarrative bool
	// Continuity is present only on containers. It applies to their immediate
	// CONTAINS children, never to context relationships or grandchildren.
	Continuity string
}

// PresentationDocument is a bounded semantic view of an SR. The implicit root
// has path "1"; its continuity is independent of every nested container.
// Truncated counts omitted content items, excluding invalid reference payloads.
type PresentationDocument struct {
	Title      string
	Header     []RenderField
	Blocks     []PresentationBlock
	Continuity string
	Truncated  int
}

// RenderPresentation preserves document order, hierarchy and relationships,
// while distinguishing continuous narrative from separate containers. Missing
// or unknown continuity uses separate layout and remains unchanged in the model.
// MaxItems/MaxPoints have the existing RenderOptions defaults. Content nesting
// beyond 64 levels fails with ErrResourceLimitExceeded, including omitted items;
// malformed children/payload of by-reference relationships are never expanded.
// This presentation does not validate the document, fetch references or attest
// authorship, cryptographic signatures or trust.
func RenderPresentation(doc *Document, opts RenderOptions) (PresentationDocument, error) {
	return renderPresentation(doc, opts, nil)
}

// RenderPresentationWithMetadataSnapshot uses the existing guarded short/Long/
// URN code presentation. A stale snapshot fails before returning any model.
// A nil snapshot selects RenderPresentation without additional code metadata.
func RenderPresentationWithMetadataSnapshot(doc *Document, snapshot *TemplateMetadataSnapshot, opts RenderOptions) (PresentationDocument, error) {
	if snapshot == nil {
		return RenderPresentation(doc, opts)
	}
	if err := snapshot.Check(doc); err != nil {
		return PresentationDocument{}, err
	}
	return renderPresentation(doc, opts, snapshot.index.code)
}

type presentationFrame struct {
	items      []ContentItem
	index      int
	prefix     string
	continuity string
}

func renderPresentation(doc *Document, opts RenderOptions, contexts map[string]ContentItemCodeContexts) (PresentationDocument, error) {
	if doc == nil {
		return PresentationDocument{}, ErrNilDocument
	}
	if opts.MaxItems <= 0 {
		opts.MaxItems = 100_000
	}
	if opts.MaxPoints <= 0 {
		opts.MaxPoints = 32
	}
	r := renderer{opts: opts, contexts: contexts}
	header := renderHeader(doc, opts, contexts)
	for i := len(opts.Header); i < len(header.Header); i++ {
		if header.Header[i].Label == "Verification" {
			header.Header[i].Label = "Document verification"
		}
	}
	if contexts == nil {
		if title := r.code(doc.Title); title != "" {
			header.Title = title
		}
	}
	out := PresentationDocument{Title: header.Title, Header: header.Header, Continuity: doc.ContinuityOfContent}
	stack := []presentationFrame{{items: doc.Content, prefix: "1.", continuity: doc.ContinuityOfContent}}
	for len(stack) > 0 {
		frame := &stack[len(stack)-1]
		if frame.index == len(frame.items) {
			stack = stack[:len(stack)-1]
			continue
		}
		item := &frame.items[frame.index]
		frame.index++
		path := frame.prefix + strconv.Itoa(frame.index)
		byReference := len(item.ReferencedContentItemIdentifier) > 0
		if len(out.Blocks) == opts.MaxItems {
			out.Truncated++
		} else {
			block := PresentationBlock{Path: path, Depth: len(stack) - 1, Kind: PresentationItem,
				Relationship: strings.ToLower(item.RelationshipType), ValueType: item.ValueType,
				InNarrative: frame.continuity == "CONTINUOUS" && item.RelationshipType == RelationshipContains &&
					(byReference || item.ValueType != ValueContainer)}
			switch {
			case byReference:
				block.Kind, block.ReferenceTo = PresentationReference, item.ReferencedContentItemIdentifier.String()
				// Do not expose malformed target payload on the relationship node.
				block.ValueType = ""
			case item.ValueType == ValueContainer:
				block.Kind, block.Continuity = PresentationSection, item.ContinuityOfContent
				if item.ContinuityOfContent == "CONTINUOUS" {
					block.Kind = PresentationNarrative
				}
			case block.InNarrative:
				block.Kind = PresentationInline
			}
			if !byReference {
				block.Concept, block.Value = r.code(item.ConceptName), r.value(*item)
				if contexts != nil {
					context := contexts[path]
					block.Concept = contextCode(item.ConceptName, context.ConceptName, opts.ShowCodes)
					block.Value = r.contextValue(*item, context)
				}
			}
			out.Blocks = append(out.Blocks, block)
		}
		if !byReference && len(item.Children) > 0 {
			if len(stack) >= 64 {
				return PresentationDocument{}, fmt.Errorf("%w: SR presentation depth", ErrResourceLimitExceeded)
			}
			continuity := ""
			if item.ValueType == ValueContainer {
				continuity = item.ContinuityOfContent
			}
			stack = append(stack, presentationFrame{items: item.Children, prefix: path + ".", continuity: continuity})
		}
	}
	return out, nil
}

// RenderPresentationText writes the semantic presentation with indented
// sections, separate values and continuous CONTAINS values on the same line.
// Terminal and bidirectional controls share RenderText's escaping contract.
func RenderPresentationText(w io.Writer, doc *Document, opts RenderOptions) error {
	presentation, err := RenderPresentation(doc, opts)
	if err != nil {
		return err
	}
	return renderPresentationText(w, presentation)
}

// RenderPresentationTextWithMetadataSnapshot guards code context before writing.
func RenderPresentationTextWithMetadataSnapshot(w io.Writer, doc *Document, snapshot *TemplateMetadataSnapshot, opts RenderOptions) error {
	presentation, err := RenderPresentationWithMetadataSnapshot(doc, snapshot, opts)
	if err != nil {
		return err
	}
	return renderPresentationText(w, presentation)
}

func renderPresentationText(w io.Writer, presentation PresentationDocument) error {
	out := bufio.NewWriter(w)
	fmt.Fprintln(out, textSafe(presentation.Title))
	for _, field := range presentation.Header {
		fmt.Fprintf(out, "%s: %s\n", textSafe(field.Label), textSafe(field.Value))
	}
	fmt.Fprintln(out)
	inline := false
	for i, block := range presentation.Blocks {
		isInline := block.InNarrative
		if inline && (!isInline || i == 0 || block.Depth != presentation.Blocks[i-1].Depth) {
			fmt.Fprintln(out)
			inline = false
		}
		indent := strings.Repeat("  ", block.Depth)
		value := textSafe(presentationValue(block))
		if isInline {
			if inline {
				fmt.Fprint(out, " ")
			} else {
				fmt.Fprint(out, indent)
			}
			fmt.Fprint(out, strings.ReplaceAll(value, "\n", "\n"+indent))
			inline = true
			continue
		}
		if block.Kind == PresentationSection || block.Kind == PresentationNarrative {
			if block.Continuity != "" {
				value += " [" + textSafe(block.Continuity) + "]"
			}
		}
		if block.Relationship != "" && block.Relationship != "contains" {
			value = "[" + textSafe(block.Relationship) + "] " + value
		}
		fmt.Fprintln(out, indent+strings.ReplaceAll(value, "\n", "\n"+indent))
	}
	if inline {
		fmt.Fprintln(out)
	}
	if presentation.Truncated > 0 {
		fmt.Fprintf(out, "... %d more content items\n", presentation.Truncated)
	}
	return out.Flush()
}

func presentationValue(block PresentationBlock) string {
	if block.ReferenceTo != "" {
		return "Reference to " + block.ReferenceTo
	}
	value := block.Concept
	if block.Value != "" {
		if value != "" {
			value += ": "
		}
		value += block.Value
	}
	return value
}

const presentationStyle = `body{font-family:sans-serif;max-width:70em;margin:2em auto;padding:0 1em;line-height:1.5}` +
	`table.header td{padding:0 1em 0 0;vertical-align:top}section{margin:1em 0}section h2{font-size:1.2em}` +
	`.children{margin-left:1.4em}.item{margin:.3em 0}.narrative{line-height:1.7}.rel{color:#666;font-size:smaller}` +
	`.flow{margin:1em 0}.flow>.item{display:inline}` +
	`.value{white-space:pre-wrap}.concept{font-weight:bold}`

// RenderPresentationHTML writes a standalone semantic HTML5 preview. Only
// generated local item paths become links; document strings remain escaped
// inert text. No scripts, external resources or automatic retrieval are used.
func RenderPresentationHTML(w io.Writer, doc *Document, opts RenderOptions) error {
	presentation, err := RenderPresentation(doc, opts)
	if err != nil {
		return err
	}
	return renderPresentationHTML(w, presentation)
}

// RenderPresentationHTMLWithMetadataSnapshot guards context before output.
func RenderPresentationHTMLWithMetadataSnapshot(w io.Writer, doc *Document, snapshot *TemplateMetadataSnapshot, opts RenderOptions) error {
	presentation, err := RenderPresentationWithMetadataSnapshot(doc, snapshot, opts)
	if err != nil {
		return err
	}
	return renderPresentationHTML(w, presentation)
}

func renderPresentationHTML(w io.Writer, presentation PresentationDocument) error {
	out := bufio.NewWriter(w)
	e := func(s string) string { return html.EscapeString(textSafe(s)) }
	fmt.Fprintf(out, "<!DOCTYPE html>\n<html><head><meta charset=\"utf-8\"><title>%s</title><style>%s</style></head><body>\n", e(presentation.Title), presentationStyle)
	fmt.Fprintf(out, "<h1 id=\"item-1\">%s</h1>\n", e(presentation.Title))
	if len(presentation.Header) > 0 {
		fmt.Fprintln(out, `<table class="header">`)
		for _, field := range presentation.Header {
			fmt.Fprintf(out, "<tr><td>%s</td><td>%s</td></tr>\n", e(field.Label), e(field.Value))
		}
		fmt.Fprintln(out, "</table>")
	}
	r := presentationHTMLWriter{out: out, blocks: presentation.Blocks, escape: e}
	r.level(0, 0)
	if presentation.Truncated > 0 {
		fmt.Fprintf(out, "<p>%d more content items</p>\n", presentation.Truncated)
	}
	fmt.Fprintln(out, "</body></html>")
	return out.Flush()
}

type presentationHTMLWriter struct {
	out    io.Writer
	blocks []PresentationBlock
	escape func(string) string
}

func (r *presentationHTMLWriter) level(index, depth int) int {
	inline := false
	for index < len(r.blocks) && r.blocks[index].Depth == depth {
		block := r.blocks[index]
		isInline := block.InNarrative
		if inline && !isInline {
			fmt.Fprintln(r.out, "</div>")
			inline = false
		}
		switch {
		case block.Kind == PresentationSection || block.Kind == PresentationNarrative:
			fmt.Fprintf(r.out, "<section class=\"%s\" id=\"item-%s\">\n", block.Kind, block.Path)
			if block.Relationship != "" && block.Relationship != "contains" {
				fmt.Fprintf(r.out, "<span class=\"rel\">%s</span>\n", r.escape(block.Relationship))
			}
			if block.Concept != "" {
				fmt.Fprintf(r.out, "<h2>%s</h2>\n", r.escape(block.Concept))
			}
		case isInline:
			if !inline {
				fmt.Fprint(r.out, `<div class="narrative flow" role="paragraph">`)
				inline = true
			} else {
				fmt.Fprint(r.out, " ")
			}
			r.value(block)
		default:
			r.value(block)
			fmt.Fprintln(r.out)
		}
		index++
		if index < len(r.blocks) && r.blocks[index].Depth > depth {
			fmt.Fprintln(r.out, `<div class="children">`)
			index = r.level(index, depth+1)
			fmt.Fprintln(r.out, "</div>")
		}
		if block.Kind == PresentationSection || block.Kind == PresentationNarrative {
			fmt.Fprintln(r.out, "</section>")
		} else {
			fmt.Fprintln(r.out, "</div>")
		}
	}
	if inline {
		fmt.Fprintln(r.out, "</div>")
	}
	return index
}

func (r *presentationHTMLWriter) value(block PresentationBlock) {
	fmt.Fprintf(r.out, "<div class=\"item\" id=\"item-%s\">", block.Path)
	if block.Relationship != "" && block.Relationship != "contains" {
		fmt.Fprintf(r.out, `<span class="rel">%s</span> `, r.escape(block.Relationship))
	}
	if block.ReferenceTo != "" {
		fmt.Fprintf(r.out, `<a href="#item-%s">Reference to %s</a>`, pathOnly(block.ReferenceTo), r.escape(block.ReferenceTo))
	} else {
		if block.Concept != "" {
			fmt.Fprintf(r.out, `<span class="concept">%s</span>`, r.escape(block.Concept))
			if block.Value != "" {
				fmt.Fprint(r.out, ": ")
			}
		}
		if block.Value != "" {
			fmt.Fprintf(r.out, `<span class="value">%s</span>`, r.escape(block.Value))
		}
	}
}
