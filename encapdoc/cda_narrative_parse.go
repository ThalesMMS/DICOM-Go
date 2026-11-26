package encapdoc

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
)

const cdaNamespace = "urn:hl7-org:v3"

type cdaXMLPart struct {
	node *cdaXMLNode
	text string
}

type cdaXMLNode struct {
	name  xml.Name
	attrs []xml.Attr
	parts []cdaXMLPart
}

func (node *cdaXMLNode) is(name string) bool {
	return node.name.Space == cdaNamespace && node.name.Local == name
}

func (node *cdaXMLNode) attribute(name string) string {
	for _, attr := range node.attrs {
		if attr.Name.Space == "" && attr.Name.Local == name {
			return attr.Value
		}
	}
	return ""
}

func normalizeCDANarrativeLimits(l CDANarrativeLimits) (CDANarrativeLimits, error) {
	d := DefaultCDANarrativeLimits()
	if l.MaxBytes == 0 {
		l.MaxBytes = d.MaxBytes
	}
	if l.MaxDepth == 0 {
		l.MaxDepth = d.MaxDepth
	}
	if l.MaxTokens == 0 {
		l.MaxTokens = d.MaxTokens
	}
	if l.MaxItems == 0 {
		l.MaxItems = d.MaxItems
	}
	if l.MaxTextBytes == 0 {
		l.MaxTextBytes = d.MaxTextBytes
	}
	if l.MaxBytes < 1 || l.MaxBytes > 256<<20 || l.MaxDepth < 1 || l.MaxDepth > 64 ||
		l.MaxTokens < 1 || l.MaxTokens > 1_000_000 || l.MaxItems < 1 || l.MaxItems > 1_000_000 ||
		l.MaxTextBytes < 1 || l.MaxTextBytes > 64<<20 {
		return CDANarrativeLimits{}, ErrInvalidDocument
	}
	return l, nil
}

func parseCDANarrative(ctx context.Context, data []byte, declaredIdentifier string, limits CDANarrativeLimits) (CDANarrative, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return CDANarrative{}, err
	}
	limits, err := normalizeCDANarrativeLimits(limits)
	if err != nil {
		return CDANarrative{}, err
	}
	info, err := InspectPayload(ctx, MIMETypeCDA, data, PayloadLimits{MaxBytes: limits.MaxBytes, MaxDepth: limits.MaxDepth, MaxRecords: limits.MaxTokens})
	if err != nil {
		return CDANarrative{}, err
	}
	if err := compareCDAIdentifiers(declaredIdentifier, info.HL7InstanceIdentifier); err != nil {
		return CDANarrative{}, err
	}
	root, err := readCDANarrativeXML(ctx, data, limits, false)
	if err != nil {
		return CDANarrative{}, err
	}
	if root == nil {
		return CDANarrative{}, ErrInvalidDocument
	}
	p := cdaNarrativeParser{ctx: ctx, limits: limits, out: CDANarrative{HL7InstanceIdentifier: info.HL7InstanceIdentifier}}
	if err = p.chargeText(info.HL7InstanceIdentifier); err != nil {
		return CDANarrative{}, err
	}
	titles, bodies := 0, 0
	for _, part := range root.parts {
		if part.node == nil {
			continue
		}
		node := part.node
		switch {
		case node.is("title"):
			titles++
			if titles > 1 {
				return CDANarrative{}, ErrInvalidDocument
			}
			err = p.audit(node)
			if err == nil {
				p.out.Title, err = p.inlineText(node)
			}
			if err == nil {
				err = p.chargeText(p.out.Title)
			}
		case node.is("component"):
			for _, child := range node.parts {
				if child.node != nil && child.node.is("nonXMLBody") {
					return CDANarrative{}, fmt.Errorf("%w: nonXML body", ErrCDANarrativeUnsupported)
				}
				if child.node != nil && child.node.is("structuredBody") {
					bodies++
					if bodies > 1 {
						return CDANarrative{}, ErrInvalidDocument
					}
					err = p.sections(child.node, 0)
					if err != nil {
						break
					}
				}
			}
		}
		if err != nil {
			return CDANarrative{}, err
		}
	}
	if !p.readable {
		return CDANarrative{}, ErrCDANarrativeAbsent
	}
	if err := p.format(); err != nil {
		return CDANarrative{}, err
	}
	return p.out, nil
}

// The temporary XML tree is bounded by source bytes, tokens and depth. It is
// released after returning the detached narrative; no source/decoder is retained.
func readCDANarrativeXML(ctx context.Context, data []byte, limits CDANarrativeLimits, rejectDuplicateAttrs bool) (*cdaXMLNode, error) {
	decoder := xml.NewDecoder(payloadContextReader{ctx: ctx, reader: bytes.NewReader(data)})
	var root *cdaXMLNode
	var stack []*cdaXMLNode
	var attributeNames map[xml.Name]struct{}
	for tokens := 0; ; tokens++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		token, err := decoder.Token()
		if err == io.EOF {
			return root, nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, ErrInvalidDocument
		}
		if tokens >= limits.MaxTokens {
			return nil, ErrEncapsulatedDocumentTooLarge
		}
		switch token := token.(type) {
		case xml.StartElement:
			if len(stack) >= limits.MaxDepth {
				return nil, ErrEncapsulatedDocumentTooLarge
			}
			if rejectDuplicateAttrs && len(token.Attr) > 1 {
				if attributeNames == nil {
					attributeNames = make(map[xml.Name]struct{})
				}
				for _, attr := range token.Attr {
					if _, exists := attributeNames[attr.Name]; exists {
						return nil, ErrInvalidDocument
					}
					attributeNames[attr.Name] = struct{}{}
				}
				clear(attributeNames)
			}
			node := &cdaXMLNode{name: token.Name, attrs: token.Attr}
			if len(stack) == 0 {
				root = node
			} else {
				parent := stack[len(stack)-1]
				parent.parts = append(parent.parts, cdaXMLPart{node: node})
			}
			stack = append(stack, node)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				node := stack[len(stack)-1]
				node.parts = append(node.parts, cdaXMLPart{text: string(token)})
			}
		case xml.Directive:
			return nil, ErrInvalidDocument
		case xml.ProcInst:
			if token.Target != "xml" {
				return nil, fmt.Errorf("%w: XML processing instruction", ErrCDANarrativeUnsupported)
			}
			if tokens != 0 {
				return nil, ErrInvalidDocument
			}
		}
	}
}

type cdaNarrativeParser struct {
	ctx       context.Context
	limits    CDANarrativeLimits
	out       CDANarrative
	items     int
	textBytes int64
	readable  bool
}

func (p *cdaNarrativeParser) chargeText(text string) error {
	if int64(len(text)) > p.limits.MaxTextBytes-p.textBytes {
		return ErrEncapsulatedDocumentTooLarge
	}
	p.textBytes += int64(len(text))
	return nil
}

func (p *cdaNarrativeParser) block(block CDANarrativeBlock) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	items := 1 + len(block.Cells)
	if items > p.limits.MaxItems-p.items {
		return ErrEncapsulatedDocumentTooLarge
	}
	if err := p.chargeText(block.Text); err != nil {
		return err
	}
	for _, cell := range block.Cells {
		if err := p.chargeText(cell); err != nil {
			return err
		}
		p.readable = p.readable || strings.TrimSpace(cell) != ""
	}
	p.items += items
	if block.Kind != CDANarrativeSection && block.Kind != CDANarrativeCaption && strings.TrimSpace(block.Text) != "" {
		p.readable = true
	}
	p.out.Blocks = append(p.out.Blocks, block)
	return nil
}

func (p *cdaNarrativeParser) diagnostic(code string) {
	for i := range p.out.Diagnostics {
		if p.out.Diagnostics[i].Code == code {
			p.out.Diagnostics[i].Count++
			return
		}
	}
	p.out.Diagnostics = append(p.out.Diagnostics, CDANarrativeDiagnostic{Code: code, Count: 1})
}

func (p *cdaNarrativeParser) sections(container *cdaXMLNode, depth int) error {
	for _, part := range container.parts {
		if part.node == nil || !part.node.is("component") {
			continue
		}
		for _, child := range part.node.parts {
			if child.node != nil && child.node.is("section") {
				if err := p.section(child.node, depth); err != nil {
					return err
				}
			}
		}
	}
	return p.ctx.Err()
}

func (p *cdaNarrativeParser) section(node *cdaXMLNode, depth int) error {
	title := ""
	titles, texts := 0, 0
	for _, part := range node.parts {
		if part.node != nil && part.node.is("title") {
			titles++
			if titles > 1 {
				return ErrInvalidDocument
			}
			if err := p.audit(part.node); err != nil {
				return err
			}
			var err error
			title, err = p.inlineText(part.node)
			if err != nil {
				return err
			}
		}
	}
	if err := p.block(CDANarrativeBlock{Kind: CDANarrativeSection, Depth: depth, Text: title}); err != nil {
		return err
	}
	for _, part := range node.parts {
		if part.node == nil {
			continue
		}
		switch {
		case part.node.is("text"):
			texts++
			if texts > 1 {
				return ErrInvalidDocument
			}
			if err := p.audit(part.node); err != nil {
				return err
			}
			if err := p.flow(part.node, depth+1); err != nil {
				return err
			}
		case part.node.is("component"):
			for _, child := range part.node.parts {
				if child.node != nil && child.node.is("section") {
					if err := p.section(child.node, depth+1); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (p *cdaNarrativeParser) flow(node *cdaXMLNode, depth int) error {
	text := p.textBuilder()
	flush := func() error {
		if value := text.value(); value != "" {
			if err := p.block(CDANarrativeBlock{Kind: CDANarrativeParagraph, Depth: depth, Text: value}); err != nil {
				return err
			}
		}
		text = p.textBuilder()
		return nil
	}
	for _, part := range node.parts {
		if part.node == nil {
			if err := text.write(part.text); err != nil {
				return err
			}
			continue
		}
		child := part.node
		if child.is("paragraph") || child.is("caption") || child.is("list") || child.is("table") {
			if err := flush(); err != nil {
				return err
			}
			var err error
			switch {
			case child.is("list"):
				err = p.list(child, depth)
			case child.is("table"):
				err = p.table(child, depth)
			default:
				kind := CDANarrativeParagraph
				if child.is("caption") {
					kind = CDANarrativeCaption
				}
				var value string
				value, err = p.inlineText(child)
				if err == nil && value != "" {
					err = p.block(CDANarrativeBlock{Kind: kind, Depth: depth, Text: value})
				}
			}
			if err != nil {
				return err
			}
		} else if err := p.inline(child, text); err != nil {
			return err
		}
	}
	return flush()
}

func (p *cdaNarrativeParser) list(node *cdaXMLNode, depth int) error {
	listType := node.attribute("listType")
	if listType != "" && listType != "ordered" && listType != "unordered" {
		return fmt.Errorf("%w: list type", ErrCDANarrativeUnsupported)
	}
	ordered := listType == "ordered"
	if err := p.block(CDANarrativeBlock{Kind: CDANarrativeList, Depth: depth, Ordered: ordered}); err != nil {
		return err
	}
	number := 0
	for _, part := range node.parts {
		if part.node == nil {
			if strings.TrimSpace(part.text) != "" {
				return ErrInvalidDocument
			}
			continue
		}
		child := part.node
		switch {
		case child.is("caption"):
			value, err := p.inlineText(child)
			if err != nil {
				return err
			}
			if err := p.block(CDANarrativeBlock{Kind: CDANarrativeCaption, Depth: depth + 1, Text: value}); err != nil {
				return err
			}
		case child.is("item"):
			number++
			if err := p.listItem(child, depth+1, ordered, number); err != nil {
				return err
			}
		default:
			p.diagnostic("unsupported-element-omitted")
		}
	}
	return nil
}

func (p *cdaNarrativeParser) listItem(node *cdaXMLNode, depth int, ordered bool, number int) error {
	cut := len(node.parts)
	for i, part := range node.parts {
		if part.node != nil && (part.node.is("paragraph") || part.node.is("list") || part.node.is("table")) {
			cut = i
			break
		}
	}
	value, err := p.inlineText(&cdaXMLNode{name: node.name, parts: node.parts[:cut]})
	if err != nil {
		return err
	}
	if err := p.block(CDANarrativeBlock{Kind: CDANarrativeListItem, Depth: depth, Ordered: ordered, Number: number, Text: value}); err != nil {
		return err
	}
	if cut < len(node.parts) {
		return p.flow(&cdaXMLNode{name: node.name, parts: node.parts[cut:]}, depth+1)
	}
	return nil
}

func (p *cdaNarrativeParser) table(node *cdaXMLNode, depth int) error {
	if err := p.block(CDANarrativeBlock{Kind: CDANarrativeTable, Depth: depth}); err != nil {
		return err
	}
	return p.tableParts(node, depth+1)
}

func (p *cdaNarrativeParser) tableParts(node *cdaXMLNode, depth int) error {
	for _, part := range node.parts {
		if part.node == nil {
			if strings.TrimSpace(part.text) != "" {
				return ErrInvalidDocument
			}
			continue
		}
		child := part.node
		switch {
		case child.is("caption"):
			value, err := p.inlineText(child)
			if err != nil {
				return err
			}
			if err := p.block(CDANarrativeBlock{Kind: CDANarrativeCaption, Depth: depth, Text: value}); err != nil {
				return err
			}
		case child.is("thead") || child.is("tbody") || child.is("tfoot"):
			if err := p.tableParts(child, depth); err != nil {
				return err
			}
		case child.is("tr"):
			var cells []string
			var rowBytes int64
			for _, cell := range child.parts {
				if cell.node == nil {
					if strings.TrimSpace(cell.text) != "" {
						return ErrInvalidDocument
					}
					continue
				}
				if !cell.node.is("th") && !cell.node.is("td") {
					return fmt.Errorf("%w: table cell", ErrCDANarrativeUnsupported)
				}
				if 1+len(cells) >= p.limits.MaxItems-p.items {
					return ErrEncapsulatedDocumentTooLarge
				}
				for _, name := range []string{"rowspan", "colspan"} {
					if value := cell.node.attribute(name); value != "" && value != "1" {
						return fmt.Errorf("%w: table span", ErrCDANarrativeUnsupported)
					}
				}
				value, err := p.inlineText(cell.node)
				if err != nil {
					return err
				}
				if int64(len(value)) > p.limits.MaxTextBytes-p.textBytes-rowBytes {
					return ErrEncapsulatedDocumentTooLarge
				}
				rowBytes += int64(len(value))
				cells = append(cells, value)
			}
			if err := p.block(CDANarrativeBlock{Kind: CDANarrativeTableRow, Depth: depth, Cells: cells}); err != nil {
				return err
			}
		case child.is("col") || child.is("colgroup"):
			p.diagnostic("table-layout-ignored")
		default:
			p.diagnostic("unsupported-element-omitted")
		}
	}
	return nil
}

func (p *cdaNarrativeParser) inlineText(node *cdaXMLNode) (string, error) {
	text := p.textBuilder()
	if err := p.inlineContents(node, text); err != nil {
		return "", err
	}
	return text.value(), nil
}

func (p *cdaNarrativeParser) inlineContents(node *cdaXMLNode, text *cdaTextBuilder) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	for _, part := range node.parts {
		if part.node == nil {
			if err := text.write(part.text); err != nil {
				return err
			}
		} else if err := p.inline(part.node, text); err != nil {
			return err
		}
	}
	return nil
}

func (p *cdaNarrativeParser) inline(node *cdaXMLNode, text *cdaTextBuilder) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	if node.name.Space != cdaNamespace {
		p.diagnostic("unsupported-element-omitted")
		return nil
	}
	switch node.name.Local {
	case "content", "caption":
		switch revised := node.attribute("revised"); revised {
		case "delete":
			p.diagnostic("deleted-content-suppressed")
			return nil
		case "insert":
			inner, err := p.inlineText(node)
			if err != nil || inner == "" {
				return err
			}
			if err := text.write("[inserted: "); err != nil {
				return err
			}
			if err := text.write(inner); err != nil {
				return err
			}
			return text.write("]")
		case "":
			return p.inlineContents(node, text)
		default:
			return fmt.Errorf("%w: revision marker", ErrCDANarrativeUnsupported)
		}
	case "br":
		return text.lineBreak()
	case "sub", "sup", "footnote":
		inner, err := p.inlineText(node)
		if err != nil || inner == "" {
			return err
		}
		prefix := "_("
		if node.name.Local == "sup" {
			prefix = "^("
		} else if node.name.Local == "footnote" {
			prefix = " [footnote: "
		}
		if err := text.write(prefix); err != nil {
			return err
		}
		if err := text.write(inner); err != nil {
			return err
		}
		suffix := ")"
		if node.name.Local == "footnote" {
			suffix = "]"
		}
		return text.write(suffix)
	case "linkHtml":
		p.diagnostic("local-link-inert")
		return p.inlineContents(node, text)
	case "footnoteRef":
		p.diagnostic("footnote-reference-unresolved")
		return nil
	case "renderMultiMedia", "script", "iframe", "object", "img":
		return fmt.Errorf("%w: multimedia or active content", ErrCDANarrativeUnsupported)
	case "paragraph":
		// Paragraphs inside simple table cells remain explicit hard lines.
		if err := text.lineBreak(); err != nil {
			return err
		}
		if err := p.inlineContents(node, text); err != nil {
			return err
		}
		return text.lineBreak()
	case "list", "table":
		return fmt.Errorf("%w: nested block in inline content", ErrCDANarrativeUnsupported)
	default:
		p.diagnostic("unsupported-element-omitted")
		return nil
	}
}

// Audit even omitted/deleted subtrees before returning any readable model.
func (p *cdaNarrativeParser) audit(node *cdaXMLNode) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	switch node.name.Local {
	case "renderMultiMedia", "script", "iframe", "object", "img", "style", "video", "audio", "reference":
		return fmt.Errorf("%w: multimedia or active content", ErrCDANarrativeUnsupported)
	}
	if node.attribute("styleCode") != "" {
		p.diagnostic("style-hints-ignored")
	}
	for _, attr := range node.attrs {
		switch attr.Name.Local {
		case "href", "src", "background", "codeBase":
			if !node.is("linkHtml") || attr.Name.Local != "href" || !strings.HasPrefix(attr.Value, "#") {
				return fmt.Errorf("%w: external resource", ErrCDANarrativeUnsupported)
			}
		}
	}
	for _, part := range node.parts {
		if part.node != nil {
			if err := p.audit(part.node); err != nil {
				return err
			}
		}
	}
	return nil
}

type cdaTextBuilder struct {
	ctx   context.Context
	limit int64
	text  strings.Builder
	last  rune
}

func (p *cdaNarrativeParser) textBuilder() *cdaTextBuilder {
	return &cdaTextBuilder{ctx: p.ctx, limit: p.limits.MaxTextBytes}
}

func (text *cdaTextBuilder) append(value string) error {
	if int64(len(value)) > text.limit-int64(text.text.Len()) {
		return ErrEncapsulatedDocumentTooLarge
	}
	text.text.WriteString(value)
	return nil
}

func (text *cdaTextBuilder) write(value string) error {
	checked := -4096
	for i, r := range value {
		if i-checked >= 4096 {
			if err := text.ctx.Err(); err != nil {
				return err
			}
			checked = i
		}
		var part string
		switch {
		case unicode.Is(unicode.Bidi_Control, r) || unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t':
			part = fmt.Sprintf("\\x%02x", r)
		case unicode.IsSpace(r):
			if text.last == ' ' || text.last == '\n' || text.text.Len() == 0 {
				continue
			}
			part, r = " ", ' '
		default:
			part = string(r)
		}
		if err := text.append(part); err != nil {
			return err
		}
		text.last = r
	}
	return nil
}

func (text *cdaTextBuilder) lineBreak() error {
	if err := text.append("\n"); err != nil {
		return err
	}
	text.last = '\n'
	return nil
}

func (text *cdaTextBuilder) value() string {
	lines := strings.Split(text.text.String(), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func (p *cdaNarrativeParser) format() error {
	var text strings.Builder
	for _, block := range p.out.Blocks {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		value := block.Text
		switch block.Kind {
		case CDANarrativeList, CDANarrativeTable:
			continue
		case CDANarrativeListItem:
			marker := "•"
			if block.Ordered {
				marker = strconv.Itoa(block.Number) + "."
			}
			value = strings.TrimSpace(marker + " " + value)
		case CDANarrativeTableRow:
			cells := make([]string, len(block.Cells))
			for i, cell := range block.Cells {
				if strings.ContainsAny(cell, "|\n\"\\") {
					cell = strconv.Quote(cell)
				}
				cells[i] = cell
			}
			value = strings.Join(cells, " | ")
		}
		if value == "" {
			continue
		}
		indent := strings.Repeat("  ", block.Depth)
		for _, line := range strings.Split(value, "\n") {
			if int64(len(indent)+len(line)+1) > p.limits.MaxTextBytes-int64(text.Len()) {
				return ErrEncapsulatedDocumentTooLarge
			}
			text.WriteString(indent)
			text.WriteString(line)
			text.WriteByte('\n')
		}
	}
	p.out.PlainText = strings.TrimSpace(text.String())
	return nil
}
