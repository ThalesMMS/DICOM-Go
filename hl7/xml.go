package hl7

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// XMLOptions bound the structural XML representation. Zero values use defaults.
// Negative limits are refused with ErrLimit.
// This representation is dicom-go's decoded Message tree, not HL7 v2 XML ITS.
type XMLOptions struct {
	MaxBytes       int // Input and output bytes; default 8 MiB.
	MaxNodes       int // XML tokens / encoded tree nodes; default 100000.
	MaxSegments    int // Default 10000.
	MaxFields      int // Per segment, default 1000.
	MaxRepetitions int // Per field, default 1000.
}

// ErrXML reports an unsupported or malformed structural XML document.
var ErrXML = errors.New("hl7: malformed structural XML")

func (o XMLOptions) invalid() bool {
	return o.MaxBytes < 0 || o.MaxNodes < 0 || o.MaxSegments < 0 || o.MaxFields < 0 || o.MaxRepetitions < 0
}

func (o XMLOptions) defaults() XMLOptions {
	if o.MaxBytes <= 0 {
		o.MaxBytes = 8 << 20
	}
	if o.MaxNodes <= 0 {
		o.MaxNodes = 100000
	}
	if o.MaxSegments <= 0 {
		o.MaxSegments = defaultMaxSegments
	}
	if o.MaxFields <= 0 {
		o.MaxFields = defaultMaxFields
	}
	if o.MaxRepetitions <= 0 {
		o.MaxRepetitions = defaultMaxRepetitions
	}
	return o
}

func xmlDelimiters(field, encoding string) (Delimiters, error) {
	if len(field) != 1 {
		return Delimiters{}, ErrXML
	}
	d, err := readDelimiters([]byte("MSH" + field + encoding + field))
	if err != nil {
		return Delimiters{}, ErrXML
	}
	return d, nil
}

func validateXMLMessage(m *Message, o XMLOptions) error {
	if m == nil || len(m.Segments) == 0 || m.Segments[0].Name != "MSH" {
		return ErrXML
	}
	d, err := xmlDelimiters(string(m.Delimiters.Field), m.Delimiters.EncodingCharacters())
	if err != nil || d != m.Delimiters {
		return ErrXML
	}
	if len(m.Segments) > o.MaxSegments {
		return ErrLimit
	}
	nodes, textBytes := o.MaxNodes, o.MaxBytes
	charge := func(n int) bool {
		if n > nodes {
			return false
		}
		nodes -= n
		return true
	}
	if !charge(2) {
		return ErrLimit
	}
	for i, s := range m.Segments {
		if !validSegmentName(s.Name) || (i > 0 && s.Name == "MSH") {
			return ErrXML
		}
		if len(s.Fields) > o.MaxFields || !charge(2) {
			return ErrLimit
		}
		for _, f := range s.Fields {
			if len(f) > o.MaxRepetitions || !charge(2) {
				return ErrLimit
			}
			for _, r := range f {
				if !charge(2) {
					return ErrLimit
				}
				for _, c := range r {
					if !charge(2) {
						return ErrLimit
					}
					for _, sub := range c {
						if !charge(3) || len(sub) > textBytes {
							return ErrLimit
						}
						textBytes -= len(sub)
						if !utf8.ValidString(sub) {
							return ErrXML
						}
						for _, r := range sub {
							if r != 9 && r != 10 && r != 13 && (r < 0x20 || r > 0x10ffff || r >= 0xd800 && r <= 0xdfff || r == 0xfffe || r == 0xffff) {
								return ErrXML
							}
						}
					}
				}
			}
		}
	}
	msh := m.Segments[0]
	if len(msh.Fields) < 2 || msh.Field(1).String() != string(d.Field) || msh.Field(2).String() != d.EncodingCharacters() {
		return ErrXML
	}
	for _, n := range []int{1, 2} {
		f := msh.Field(n)
		if len(f) != 1 || len(f[0]) != 1 || len(f[0][0]) != 1 {
			return ErrXML
		}
	}
	return nil
}

type xmlBoundedWriter struct {
	bytes.Buffer
	remaining int
}

func (w *xmlBoundedWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, ErrLimit
	}
	w.remaining -= len(p)
	return w.Buffer.Write(p)
}

// MarshalXMLMessage encodes a decoded Message with ordered structural nodes.
// It preserves delimiter declarations and text values, not ER7 escape spelling.
func MarshalXMLMessage(m *Message, options XMLOptions) ([]byte, error) {
	if options.invalid() {
		return nil, ErrLimit
	}
	o := options.defaults()
	if err := validateXMLMessage(m, o); err != nil {
		return nil, err
	}
	w := &xmlBoundedWriter{remaining: o.MaxBytes}
	e := xml.NewEncoder(w)
	start := func(name string, attrs ...xml.Attr) error {
		return e.EncodeToken(xml.StartElement{Name: xml.Name{Local: name}, Attr: attrs})
	}
	end := func(name string) error { return e.EncodeToken(xml.EndElement{Name: xml.Name{Local: name}}) }
	root := []xml.Attr{{Name: xml.Name{Local: "version"}, Value: "1"}, {Name: xml.Name{Local: "field"}, Value: string(m.Delimiters.Field)}, {Name: xml.Name{Local: "encoding"}, Value: m.Delimiters.EncodingCharacters()}}
	if err := start("hl7-message", root...); err != nil {
		return nil, err
	}
	for _, s := range m.Segments {
		if err := start("segment", xml.Attr{Name: xml.Name{Local: "name"}, Value: s.Name}); err != nil {
			return nil, err
		}
		for _, f := range s.Fields {
			if err := start("field"); err != nil {
				return nil, err
			}
			for _, r := range f {
				if err := start("repetition"); err != nil {
					return nil, err
				}
				for _, c := range r {
					if err := start("component"); err != nil {
						return nil, err
					}
					for _, sub := range c {
						if err := start("subcomponent"); err != nil {
							return nil, err
						}
						if err := e.EncodeToken(xml.CharData(sub)); err != nil {
							return nil, ErrXML
						}
						if err := end("subcomponent"); err != nil {
							return nil, err
						}
					}
					if err := end("component"); err != nil {
						return nil, err
					}
				}
				if err := end("repetition"); err != nil {
					return nil, err
				}
			}
			if err := end("field"); err != nil {
				return nil, err
			}
		}
		if err := end("segment"); err != nil {
			return nil, err
		}
	}
	if err := end("hl7-message"); err != nil {
		return nil, err
	}
	if err := e.Flush(); err != nil {
		if errors.Is(err, ErrLimit) {
			return nil, ErrLimit
		}
		return nil, ErrXML
	}
	return w.Bytes(), nil
}

func xmlAttrs(s xml.StartElement, keys ...string) (map[string]string, error) {
	if s.Name.Space != "" {
		return nil, ErrXML
	}
	out := make(map[string]string, len(keys))
	for _, a := range s.Attr {
		if a.Name.Space != "" {
			return nil, ErrXML
		}
		allowed := false
		for _, key := range keys {
			if a.Name.Local == key {
				allowed = true
			}
		}
		if !allowed {
			return nil, ErrXML
		}
		if _, ok := out[a.Name.Local]; ok {
			return nil, ErrXML
		}
		out[a.Name.Local] = a.Value
	}
	if len(out) != len(keys) {
		return nil, ErrXML
	}
	return out, nil
}

// ParseXMLMessage decodes only the version-1 structural format emitted above.
// It refuses namespaces, DTDs, unknown shape and mixed text without quoting data.
func ParseXMLMessage(data []byte, options XMLOptions) (*Message, error) {
	if options.invalid() {
		return nil, ErrLimit
	}
	o := options.defaults()
	if len(data) > o.MaxBytes {
		return nil, ErrLimit
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	remaining := o.MaxNodes
	var stack []string
	var m *Message
	var text strings.Builder
	finished, seenToken := false, false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			if !finished || len(stack) != 0 {
				return nil, ErrXML
			}
			break
		}
		if err != nil {
			return nil, ErrXML
		}
		if remaining == 0 {
			return nil, ErrLimit
		}
		remaining--
		switch t := token.(type) {
		case xml.ProcInst:
			if seenToken || t.Target != "xml" {
				return nil, ErrXML
			}
			seenToken = true
		case xml.StartElement:
			seenToken = true
			if finished || t.Name.Space != "" {
				return nil, ErrXML
			}
			parent := ""
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			switch t.Name.Local {
			case "hl7-message":
				if parent != "" || m != nil {
					return nil, ErrXML
				}
				a, err := xmlAttrs(t, "version", "field", "encoding")
				if err != nil || a["version"] != "1" {
					return nil, ErrXML
				}
				d, err := xmlDelimiters(a["field"], a["encoding"])
				if err != nil {
					return nil, ErrXML
				}
				m = &Message{Delimiters: d}
			case "segment":
				if parent != "hl7-message" {
					return nil, ErrXML
				}
				a, err := xmlAttrs(t, "name")
				if err != nil || !validSegmentName(a["name"]) {
					return nil, ErrXML
				}
				if len(m.Segments) >= o.MaxSegments {
					return nil, ErrLimit
				}
				m.Segments = append(m.Segments, Segment{Name: a["name"]})
			case "field":
				if parent != "segment" {
					return nil, ErrXML
				}
				if _, err := xmlAttrs(t); err != nil {
					return nil, err
				}
				s := &m.Segments[len(m.Segments)-1]
				if len(s.Fields) >= o.MaxFields {
					return nil, ErrLimit
				}
				s.Fields = append(s.Fields, nil)
			case "repetition":
				if parent != "field" {
					return nil, ErrXML
				}
				if _, err := xmlAttrs(t); err != nil {
					return nil, err
				}
				s := &m.Segments[len(m.Segments)-1]
				f := &s.Fields[len(s.Fields)-1]
				if len(*f) >= o.MaxRepetitions {
					return nil, ErrLimit
				}
				*f = append(*f, nil)
			case "component":
				if parent != "repetition" {
					return nil, ErrXML
				}
				if _, err := xmlAttrs(t); err != nil {
					return nil, err
				}
				s := &m.Segments[len(m.Segments)-1]
				f := s.Fields[len(s.Fields)-1]
				r := &f[len(f)-1]
				*r = append(*r, nil)
			case "subcomponent":
				if parent != "component" {
					return nil, ErrXML
				}
				if _, err := xmlAttrs(t); err != nil {
					return nil, err
				}
				text.Reset()
			default:
				return nil, ErrXML
			}
			stack = append(stack, t.Name.Local)
		case xml.EndElement:
			if len(stack) == 0 || t.Name.Space != "" || stack[len(stack)-1] != t.Name.Local {
				return nil, ErrXML
			}
			if t.Name.Local == "subcomponent" {
				s := &m.Segments[len(m.Segments)-1]
				f := s.Fields[len(s.Fields)-1]
				r := f[len(f)-1]
				c := &r[len(r)-1]
				*c = append(*c, text.String())
			}
			stack = stack[:len(stack)-1]
			if t.Name.Local == "hl7-message" {
				finished = true
			}
		case xml.CharData:
			if len(stack) > 0 && stack[len(stack)-1] == "subcomponent" {
				text.Write(t)
			} else if len(bytes.TrimSpace(t)) != 0 {
				return nil, ErrXML
			}
		default:
			return nil, ErrXML
		}
	}
	if err := validateXMLMessage(m, o); err != nil {
		return nil, err
	}
	return m, nil
}
