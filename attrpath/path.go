// Package attrpath addresses nested attributes with paths such as
// "ReferencedSeriesSequence[0].SeriesInstanceUID",
// "(0008,1115)[*].(0020,000E)" or "(0029,\"SIEMENS CSA HEADER\",08)", and
// finds, inserts, modifies and removes the attributes they select, in the
// manner of DCMTK dcmodify. The syntax and semantics are implemented from
// that tool's documented behaviour; no DCMTK code is used.
//
// A path is a sequence of segments separated by ".". Each segment names an
// attribute by keyword, by tag "(gggg,eeee)" or, for a private attribute, by
// group, private creator and element offset "(gggg,\"creator\",xx)". Every
// segment but the last selects items of a sequence and needs an item index
// "[n]" (zero-based) or the wildcard "[*]" (every existing item). The last
// segment may carry an index too, which then selects the item itself.
package attrpath

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
)

var (
	// ErrSyntax reports a path that cannot be parsed.
	ErrSyntax = errors.New("dicom/attrpath: invalid path")
	// ErrNotFound reports a path that selects nothing.
	ErrNotFound = errors.New("dicom/attrpath: attribute not found")
	// ErrNotSequence reports an item index on an attribute that is not a
	// sequence.
	ErrNotSequence = errors.New("dicom/attrpath: attribute is not a sequence")
	// ErrValue reports a value that does not suit the attribute's VR.
	ErrValue = errors.New("dicom/attrpath: invalid value")
	// ErrUnknownVR reports a new attribute whose VR is not known.
	ErrUnknownVR = errors.New("dicom/attrpath: unknown VR")
	// ErrLimit reports a path or edit beyond the configured limits.
	ErrLimit = errors.New("dicom/attrpath: limit exceeded")
)

const (
	// MaxPathLength bounds the path text.
	MaxPathLength = 4096
	// MaxSegments bounds the nesting depth of a path.
	MaxSegments = 64
	// MaxItemIndex bounds an item index.
	MaxItemIndex = 1 << 20
)

// Segment is one step of a path.
type Segment struct {
	// Tag is the attribute tag. For a private attribute named by creator the
	// element holds only the offset (0x00xx); the block comes from the
	// creator reservation of each data set.
	Tag core.Tag
	// Creator is the private creator of a "(gggg,\"creator\",xx)" segment.
	Creator string
	// Keyword is set when the segment was written as a keyword.
	Keyword string
	// HasItem reports an item selector; Item is its index and AnyItem the
	// wildcard.
	HasItem bool
	Item    int
	AnyItem bool
}

// Private reports a segment addressed by private creator.
func (s Segment) Private() bool { return s.Creator != "" }

// String renders the segment in path syntax.
func (s Segment) String() string {
	var b strings.Builder
	switch {
	case s.Private():
		fmt.Fprintf(&b, "(%04X,%q,%02X)", s.Tag.Group, s.Creator, s.Tag.Element&0xff)
	case s.Keyword != "":
		b.WriteString(s.Keyword)
	default:
		fmt.Fprintf(&b, "(%04X,%04X)", s.Tag.Group, s.Tag.Element)
	}
	switch {
	case s.AnyItem:
		b.WriteString("[*]")
	case s.HasItem:
		fmt.Fprintf(&b, "[%d]", s.Item)
	}
	return b.String()
}

// Path is a parsed attribute path.
type Path []Segment

// String renders the path in path syntax.
func (p Path) String() string {
	parts := make([]string, len(p))
	for i, s := range p {
		parts[i] = s.String()
	}
	return strings.Join(parts, ".")
}

// SelectsItem reports a path whose last segment selects items.
func (p Path) SelectsItem() bool { return len(p) > 0 && p[len(p)-1].HasItem }

// Wildcard reports a path with a "[*]" selector.
func (p Path) Wildcard() bool {
	for _, s := range p {
		if s.AnyItem {
			return true
		}
	}
	return false
}

// MustParse is Parse with the standard dictionary that panics on error, for
// constant paths.
func MustParse(text string) Path {
	p, err := Parse(text, nil)
	if err != nil {
		panic(err)
	}
	return p
}

// Parse parses a path. Keywords are resolved with dict (the standard
// dictionary when nil).
func Parse(text string, dict dictionary.DataDictionary) (Path, error) {
	if dict == nil {
		dict = std.Dictionary
	}
	if len(text) > MaxPathLength {
		return nil, fmt.Errorf("%w: path longer than %d bytes", ErrLimit, MaxPathLength)
	}
	p := &parser{text: text, dict: dict}
	var path Path
	for {
		if len(path) == MaxSegments {
			return nil, fmt.Errorf("%w: more than %d segments", ErrLimit, MaxSegments)
		}
		segment, err := p.segment()
		if err != nil {
			return nil, err
		}
		path = append(path, segment)
		if p.done() {
			break
		}
		if !p.consume('.') {
			return nil, p.errorf("expected '.'")
		}
	}
	for i, s := range path[:len(path)-1] {
		if !s.HasItem {
			return nil, fmt.Errorf("%w: segment %d (%s) needs an item index", ErrSyntax, i+1, s)
		}
	}
	return path, nil
}

type parser struct {
	text string
	pos  int
	dict dictionary.DataDictionary
}

func (p *parser) done() bool { return p.pos >= len(p.text) }

func (p *parser) peek() byte {
	if p.done() {
		return 0
	}
	return p.text[p.pos]
}

func (p *parser) consume(c byte) bool {
	if p.peek() == c && !p.done() {
		p.pos++
		return true
	}
	return false
}

func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf("%w: %s at offset %d", ErrSyntax, fmt.Sprintf(format, args...), p.pos)
}

func (p *parser) segment() (Segment, error) {
	var s Segment
	var err error
	if p.peek() == '(' {
		s, err = p.tag()
	} else {
		s, err = p.keyword()
	}
	if err != nil {
		return s, err
	}
	if p.consume('[') {
		s.HasItem = true
		if p.consume('*') {
			s.AnyItem = true
		} else {
			start := p.pos
			for !p.done() && p.peek() >= '0' && p.peek() <= '9' {
				p.pos++
			}
			digits := p.text[start:p.pos]
			if digits == "" || (len(digits) > 1 && digits[0] == '0') {
				return s, p.errorf("item index must be a non-negative decimal number")
			}
			n, convErr := strconv.Atoi(digits)
			if convErr != nil || n > MaxItemIndex {
				return s, fmt.Errorf("%w: item index above %d", ErrLimit, MaxItemIndex)
			}
			s.Item = n
		}
		if !p.consume(']') {
			return s, p.errorf("expected ']'")
		}
	}
	return s, nil
}

func (p *parser) hex(digits int) (uint16, error) {
	if p.pos+digits > len(p.text) {
		return 0, p.errorf("expected %d hexadecimal digits", digits)
	}
	value, err := strconv.ParseUint(p.text[p.pos:p.pos+digits], 16, 16)
	if err != nil || strings.ContainsAny(p.text[p.pos:p.pos+digits], "+-xX") {
		return 0, p.errorf("expected %d hexadecimal digits", digits)
	}
	p.pos += digits
	return uint16(value), nil
}

// tag parses "(gggg,eeee)" or "(gggg,\"creator\",xx)".
func (p *parser) tag() (Segment, error) {
	p.consume('(')
	group, err := p.hex(4)
	if err != nil {
		return Segment{}, err
	}
	if !p.consume(',') {
		return Segment{}, p.errorf("expected ','")
	}
	var s Segment
	if p.consume('"') {
		end := strings.IndexByte(p.text[p.pos:], '"')
		if end < 0 {
			return s, p.errorf("unterminated private creator")
		}
		creator := p.text[p.pos : p.pos+end]
		p.pos += end + 1
		id, err := dictionary.PrivateCreatorID(creator)
		if err != nil || id == "" {
			return s, p.errorf("invalid private creator")
		}
		if group%2 == 0 || group <= 0x0007 || group == 0xFFFF {
			return s, p.errorf("group %04X cannot hold private attributes", group)
		}
		if !p.consume(',') {
			return s, p.errorf("expected ','")
		}
		offset, err := p.hex(2)
		if err != nil {
			return s, err
		}
		s.Tag, s.Creator = core.NewTag(group, offset), id
	} else {
		element, err := p.hex(4)
		if err != nil {
			return s, err
		}
		s.Tag = core.NewTag(group, element)
	}
	if !p.consume(')') {
		return s, p.errorf("expected ')'")
	}
	if s.Tag.Group == 0xFFFE {
		return s, p.errorf("item and delimitation tags cannot be addressed")
	}
	return s, nil
}

func (p *parser) keyword() (Segment, error) {
	start := p.pos
	for !p.done() {
		c := p.peek()
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' {
			p.pos++
			continue
		}
		break
	}
	word := p.text[start:p.pos]
	if word == "" {
		p.pos = start
		return Segment{}, p.errorf("expected a keyword or tag")
	}
	entry, ok := p.dict.ByKeyword(word)
	if !ok {
		p.pos = start
		return Segment{}, p.errorf("unknown keyword %q", word)
	}
	return Segment{Tag: entry.Tag, Keyword: entry.Keyword}, nil
}
