package sr

import (
	"bufio"
	"errors"
	"fmt"
	"html"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/ThalesMMS/dicom-go/dcmtime"
	"github.com/ThalesMMS/dicom-go/dictionary/uid"
	"github.com/ThalesMMS/dicom-go/personname"
)

// RenderOptions configures RenderItems, RenderText and RenderHTML.
type RenderOptions struct {
	// Header lists caller-supplied header fields (patient, study...) shown
	// before the document's own; labels and values are escaped like the
	// content.
	Header []RenderField
	// ShowCodes appends code values and coding schemes to code meanings.
	ShowCodes bool
	// MaxItems bounds the rendered content items (default 100000); the
	// rest are summarized as truncated.
	MaxItems int
	// MaxPoints bounds the coordinates shown per SCOORD, SCOORD3D or TCOORD
	// item (default 32); the rest are counted.
	MaxPoints int
}

// RenderField is one header line.
type RenderField struct {
	Label, Value string
}

// RenderedItem is one content item ready for display.
type RenderedItem struct {
	// Path is the item's position counting the root as 1 ("1.2.3" is the
	// third child of the second child of the root), the Referenced Content
	// Item Identifier of by-reference relationships and of dsrdump +Pn.
	Path  string
	Depth int
	// Relationship is the relationship type in lower case ("contains").
	Relationship string
	ValueType    ValueType
	// Concept is the concept name meaning (with its code under ShowCodes).
	Concept string
	// Value is the formatted value; empty for containers.
	Value string
	// ReferenceTo is the target path of a by-reference relationship.
	ReferenceTo string
	// Continuity is the container's Continuity of Content, when present.
	Continuity string
}

// RenderedDocument is a document ready for display.
type RenderedDocument struct {
	Title  string
	Header []RenderField
	Items  []RenderedItem
	// Truncated counts the content items beyond RenderOptions.MaxItems.
	Truncated int
}

// ErrNilDocument reports a nil document given to a renderer.
var ErrNilDocument = errors.New("dicom/sr: nil document")

// RenderItems formats doc for display: its title, header and every content
// item in document order with its path, relationship, concept and value.
// Values are plain text; RenderText and RenderHTML escape them for their
// output.
func RenderItems(doc *Document, opts RenderOptions) (RenderedDocument, error) {
	return renderItems(doc, opts, nil)
}

func renderItems(doc *Document, opts RenderOptions, contexts map[string]ContentItemCodeContexts) (RenderedDocument, error) {
	if doc == nil {
		return RenderedDocument{}, ErrNilDocument
	}
	if opts.MaxItems <= 0 {
		opts.MaxItems = 100_000
	}
	if opts.MaxPoints <= 0 {
		opts.MaxPoints = 32
	}
	out := renderHeader(doc, opts, contexts)
	r := renderer{opts: opts, out: &out, contexts: contexts}
	r.items(doc.Content, "1.", 0)
	return out, nil
}

func renderHeader(doc *Document, opts RenderOptions, contexts map[string]ContentItemCodeContexts) RenderedDocument {
	out := RenderedDocument{Title: doc.Title.CodeMeaning}
	if out.Title == "" {
		out.Title = "Structured Report"
	}
	if contexts != nil {
		out.Title = contextCode(doc.Title, contexts["1"].ConceptName, opts.ShowCodes)
		if out.Title == "" {
			out.Title = "Structured Report"
		}
	}
	out.Header = append(out.Header, opts.Header...)
	if class := sopClassName(doc.SOPClassUID); class != "" {
		out.Header = append(out.Header, RenderField{"Document type", class})
	}
	if doc.CompletionFlag != "" {
		out.Header = append(out.Header, RenderField{"Completion", doc.CompletionFlag})
	}
	if doc.VerificationFlag != "" {
		out.Header = append(out.Header, RenderField{"Verification", doc.VerificationFlag})
	}
	if doc.CompletionFlagDescription != "" {
		out.Header = append(out.Header, RenderField{"Completion description", doc.CompletionFlagDescription})
	}
	r := renderer{opts: opts}
	for i, observer := range doc.VerifyingObservers {
		if i >= opts.MaxItems {
			out.Header = append(out.Header, RenderField{"Verifying observers omitted", strconv.Itoa(len(doc.VerifyingObservers) - i)})
			break
		}
		label := "Verifying observer " + strconv.Itoa(i+1)
		for _, field := range []RenderField{
			{label, personname.Display(observer.Name)},
			{label + " organization", observer.Organization},
			{label + " date/time", formatDateTime(observer.DateTime)},
			{label + " identification", r.code(observer.Code)},
		} {
			if field.Value != "" {
				out.Header = append(out.Header, field)
			}
		}
	}
	for i, predecessor := range doc.PredecessorDocuments {
		if i >= opts.MaxItems {
			out.Header = append(out.Header, RenderField{"Predecessor documents omitted", strconv.Itoa(len(doc.PredecessorDocuments) - i)})
			break
		}
		label := "Predecessor document " + strconv.Itoa(i+1)
		for _, field := range []RenderField{
			{label, reference(predecessor.SOPClassUID, predecessor.SOPInstanceUID)},
			{label + " study UID", predecessor.StudyInstanceUID},
			{label + " series UID", predecessor.SeriesInstanceUID},
		} {
			if field.Value != "" {
				out.Header = append(out.Header, field)
			}
		}
	}
	if doc.ContentDate != "" || doc.ContentTime != "" {
		out.Header = append(out.Header, RenderField{"Content date/time", strings.TrimSpace(dcmtime.FormatDate(doc.ContentDate) + " " + dcmtime.FormatTime(doc.ContentTime))})
	}
	if doc.SOPInstanceUID != "" {
		out.Header = append(out.Header, RenderField{"SOP Instance UID", doc.SOPInstanceUID})
	}
	return out
}

type renderer struct {
	opts     RenderOptions
	out      *RenderedDocument
	contexts map[string]ContentItemCodeContexts
}

func (r *renderer) items(items []ContentItem, prefix string, depth int) {
	for i, item := range items {
		path := prefix + strconv.Itoa(i+1)
		if len(r.out.Items) >= r.opts.MaxItems {
			r.out.Truncated++
			if len(item.ReferencedContentItemIdentifier) == 0 {
				r.out.Truncated += countItems(item.Children)
			}
			continue
		}
		rendered := RenderedItem{
			Path:         path,
			Depth:        depth,
			Relationship: strings.ToLower(item.RelationshipType),
			ValueType:    item.ValueType,
		}
		if target := item.ReferencedContentItemIdentifier.String(); target != "" {
			rendered.ReferenceTo = target
			rendered.ValueType = ""
		} else {
			if item.ValueType == ValueContainer {
				rendered.Continuity = item.ContinuityOfContent
			}
			rendered.Concept = r.code(item.ConceptName)
			rendered.Value = r.value(item)
			if r.contexts != nil {
				contexts := r.contexts[path]
				rendered.Concept = contextCode(item.ConceptName, contexts.ConceptName, r.opts.ShowCodes)
				rendered.Value = r.contextValue(item, contexts)
			}
		}
		r.out.Items = append(r.out.Items, rendered)
		if rendered.ReferenceTo == "" {
			r.items(item.Children, path+".", depth+1)
		}
	}
}

func countItems(items []ContentItem) int {
	n := len(items)
	for _, item := range items {
		if len(item.ReferencedContentItemIdentifier) == 0 {
			n += countItems(item.Children)
		}
	}
	return n
}

func (r *renderer) code(code CodedEntry) string {
	meaning := strings.TrimSpace(code.CodeMeaning)
	if meaning == "" {
		meaning = strings.TrimSpace(code.CodeValue)
	}
	if r.opts.ShowCodes && code.CodeValue != "" {
		meaning += " (" + code.CodeValue + ", " + code.CodingSchemeDesignator + ")"
	}
	return meaning
}

func (r *renderer) value(item ContentItem) string {
	switch item.ValueType {
	case ValueText:
		return item.Text
	case ValueCode:
		return r.code(item.Code)
	case ValueNum:
		if item.Measurement == nil {
			if item.NumericValueQualifier.Empty() {
				return "no value"
			}
			return r.code(item.NumericValueQualifier)
		}
		units := item.Measurement.Units
		unit := units.CodeMeaning
		if strings.EqualFold(units.CodingSchemeDesignator, "UCUM") && units.CodeValue != "" {
			unit = units.CodeValue
			if unit == "1" || unit == "{ratio}" {
				unit = ""
			}
		}
		return strings.TrimSpace(strconv.FormatFloat(item.Measurement.Value, 'g', -1, 64) + " " + unit)
	case ValueDateTime:
		return formatDateTime(item.DateTime)
	case ValueDate:
		return dcmtime.FormatDate(item.Date)
	case ValueTime:
		return dcmtime.FormatTime(item.Time)
	case ValueUIDRef:
		return item.UID
	case ValuePName:
		return personname.Display(item.PersonName)
	case ValueImage:
		value := reference(item.Image.SOPClassUID, item.Image.SOPInstanceUID)
		if len(item.Image.Frames) > 0 {
			frames := make([]string, len(item.Image.Frames))
			for i, frame := range item.Image.Frames {
				frames[i] = strconv.Itoa(frame)
			}
			value += ", frames " + strings.Join(frames, ",")
		}
		return value
	case ValueComposite:
		return reference(item.Composite.SOPClassUID, item.Composite.SOPInstanceUID)
	case ValueWaveform:
		value := reference(item.Waveform.SOPClassUID, item.Waveform.SOPInstanceUID)
		if n := len(item.Waveform.Channels); n > 0 {
			value += fmt.Sprintf(", %d channels", n)
		}
		return value
	case ValueSCoord:
		points := make([]string, 0, len(item.Spatial.Coordinates))
		for _, p := range item.Spatial.Coordinates {
			points = append(points, fmt.Sprintf("(%g,%g)", p.X, p.Y))
		}
		return strings.TrimSpace(item.Spatial.GraphicType + " " + r.points(points))
	case ValueSCoord3D:
		points := make([]string, 0, len(item.Spatial3D.Coordinates))
		for _, p := range item.Spatial3D.Coordinates {
			points = append(points, fmt.Sprintf("(%g,%g,%g)", p.X, p.Y, p.Z))
		}
		value := strings.TrimSpace(item.Spatial3D.GraphicType + " " + r.points(points))
		if item.Spatial3D.FrameOfReferenceUID != "" {
			value += " in " + item.Spatial3D.FrameOfReferenceUID
		}
		return value
	case ValueTCoord:
		var points []string
		for _, p := range item.Temporal.SamplePositions {
			points = append(points, strconv.FormatUint(uint64(p), 10))
		}
		for _, p := range item.Temporal.TimeOffsets {
			points = append(points, strconv.FormatFloat(p, 'g', -1, 64)+" s")
		}
		for _, p := range item.Temporal.DateTimes {
			points = append(points, formatDateTime(p))
		}
		return strings.TrimSpace(item.Temporal.RangeType + " " + r.points(points))
	case ValueContainer:
		return ""
	default:
		return "(" + string(item.ValueType) + " value not rendered)"
	}
}

// points joins coordinates, counting those beyond MaxPoints.
func (r *renderer) points(points []string) string {
	if len(points) > r.opts.MaxPoints {
		return strings.Join(points[:r.opts.MaxPoints], " ") + fmt.Sprintf(" ... (%d points)", len(points))
	}
	return strings.Join(points, " ")
}

func reference(class, instance string) string {
	name := sopClassName(class)
	if name == "" {
		name = class
	}
	return strings.TrimSpace(name + " " + instance)
}

func sopClassName(class string) string {
	if entry, ok := uid.Dictionary.ByUID(class); ok {
		return entry.Name
	}
	return ""
}

// formatDateTime formats a DT value as its date and time.
func formatDateTime(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 8 {
		return dcmtime.FormatDate(value)
	}
	return strings.TrimSpace(dcmtime.FormatDate(value[:8]) + " " + dcmtime.FormatTime(value[8:]))
}

// RenderText writes doc as indented plain text: the title, the header and
// one line per content item ("contains CODE Finding: Lesion"). Control
// characters of the content are written as escapes, so the text cannot
// drive a terminal.
func RenderText(w io.Writer, doc *Document, opts RenderOptions) error {
	rendered, err := RenderItems(doc, opts)
	if err != nil {
		return err
	}
	return renderText(w, rendered)
}

func renderText(w io.Writer, rendered RenderedDocument) error {
	out := bufio.NewWriter(w)
	fmt.Fprintln(out, textSafe(rendered.Title))
	for _, field := range rendered.Header {
		fmt.Fprintf(out, "%s: %s\n", textSafe(field.Label), textSafe(field.Value))
	}
	fmt.Fprintln(out)
	for _, item := range rendered.Items {
		indent := strings.Repeat("  ", item.Depth)
		line := item.Path + " " + item.Relationship
		switch {
		case item.ReferenceTo != "":
			line += " -> " + item.ReferenceTo
		default:
			line += " " + string(item.ValueType)
			if item.Concept != "" {
				line += " " + textSafe(item.Concept)
			}
			if item.Value != "" {
				// Continuation lines of multi-line text keep the indent.
				value := strings.ReplaceAll(textSafe(item.Value), "\n", "\n"+indent+"    ")
				line += ": " + value
			}
		}
		fmt.Fprintln(out, indent+strings.TrimSpace(line))
	}
	if rendered.Truncated > 0 {
		fmt.Fprintf(out, "... %d more content items\n", rendered.Truncated)
	}
	return out.Flush()
}

// textSafe keeps printable text, tabs and newlines and writes other control
// characters (including CR and ANSI escapes) as \x escapes.
func textSafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case unicode.IsControl(r) || r == '\u2028' || r == '\u2029' ||
			unicode.Is(unicode.Bidi_Control, r):
			fmt.Fprintf(&b, "\\x%02x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

const htmlStyle = `body{font-family:sans-serif;margin:1.5em;line-height:1.4}` +
	`table.header td{padding:0 1em 0 0;vertical-align:top}` +
	`ul{list-style:none;padding-left:1.4em;margin:0}` +
	`.rel{color:#666;font-size:smaller}.vt{color:#666;font-size:smaller}.concept{font-weight:bold}` +
	`.value{white-space:pre-wrap}`

// RenderHTML writes doc as a standalone HTML5 page: the title, a header
// table and the content tree as nested lists, each item anchored by its path
// ("item-1.2") and each by-reference relationship linked to its target. All
// document content is HTML-escaped and never placed in an attribute or a
// URL; the page has no scripts or external resources.
func RenderHTML(w io.Writer, doc *Document, opts RenderOptions) error {
	rendered, err := RenderItems(doc, opts)
	if err != nil {
		return err
	}
	return renderHTML(w, rendered)
}

func renderHTML(w io.Writer, rendered RenderedDocument) error {
	out := bufio.NewWriter(w)
	e := func(s string) string { return html.EscapeString(textSafe(s)) }
	fmt.Fprintf(out, "<!DOCTYPE html>\n<html>\n<head>\n<meta charset=\"utf-8\">\n<title>%s</title>\n<style>%s</style>\n</head>\n<body>\n", e(rendered.Title), htmlStyle)
	fmt.Fprintf(out, "<h1>%s</h1>\n", e(rendered.Title))
	if len(rendered.Header) > 0 {
		fmt.Fprintln(out, `<table class="header">`)
		for _, field := range rendered.Header {
			fmt.Fprintf(out, "<tr><td>%s</td><td>%s</td></tr>\n", e(field.Label), e(field.Value))
		}
		fmt.Fprintln(out, "</table>")
	}
	// Each item's children are a list inside its <li>.
	if len(rendered.Items) > 0 {
		fmt.Fprintln(out, "<ul>")
	}
	for i, item := range rendered.Items {
		// Paths are generated from ordinals: digits and dots only.
		fmt.Fprintf(out, `<li id="item-%s"><span class="rel">%s</span> `, item.Path, e(item.Relationship))
		if item.ReferenceTo != "" {
			fmt.Fprintf(out, `<a href="#item-%s">%s</a>`, pathOnly(item.ReferenceTo), e(item.ReferenceTo))
		} else {
			fmt.Fprintf(out, `<span class="vt">%s</span>`, e(string(item.ValueType)))
			if item.Concept != "" {
				fmt.Fprintf(out, ` <span class="concept">%s</span>`, e(item.Concept))
			}
			if item.Value != "" {
				fmt.Fprintf(out, `: <span class="value">%s</span>`, e(item.Value))
			}
		}
		next := -1
		if i+1 < len(rendered.Items) {
			next = rendered.Items[i+1].Depth
		}
		if next > item.Depth {
			fmt.Fprint(out, "\n<ul>\n")
			continue
		}
		fmt.Fprintln(out, "</li>")
		for depth := item.Depth; depth > next; depth-- {
			fmt.Fprintln(out, "</ul>")
			if depth > 0 {
				fmt.Fprintln(out, "</li>")
			}
		}
	}
	if rendered.Truncated > 0 {
		fmt.Fprintf(out, "<p>%d more content items</p>\n", rendered.Truncated)
	}
	fmt.Fprintln(out, "</body>\n</html>")
	return out.Flush()
}

// pathOnly keeps the digits and dots of a content item path.
func pathOnly(path string) string {
	return strings.Map(func(r rune) rune {
		if r == '.' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, path)
}
