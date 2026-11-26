package hl7

import (
	"errors"
	"fmt"
	"strings"
)

// String encodes the message as ER7 text in UTF-8, segments ended by
// carriage returns. Values are escaped and trailing empty components,
// repetitions and fields are omitted.
func (m *Message) String() string {
	text, _ := m.encode()
	return text
}

// Marshal encodes the message as ER7 bytes in its MSH-18 character set.
func (m *Message) Marshal() ([]byte, error) {
	text, err := m.encode()
	if err != nil {
		return nil, err
	}
	charset := strings.TrimSpace(m.Header().CharacterSet)
	encoded, err := encodeCharacterSet(charset, text)
	if err != nil {
		return nil, err
	}
	if err := m.checkEncodedCharacterSet(encoded, charset); err != nil {
		return nil, err
	}
	return encoded, nil
}

func (m *Message) checkEncodedCharacterSet(encoded []byte, charset string) error {
	// A multi-byte character before MSH-18 whose bytes include a delimiter
	// would hide MSH-18 from a receiver: refuse to send it.
	d := m.Delimiters
	if d.Field == 0 || d.Component == 0 || d.Repetition == 0 {
		d = DefaultDelimiters
	}
	if raw, err := rawCharacterSet(encoded, d); err != nil || raw != charset {
		return fmt.Errorf("%w: a character before MSH-18 encodes with a delimiter byte", ErrCharset)
	}
	return nil
}

func (m *Message) encode() (string, error) {
	var b strings.Builder
	if err := m.writeER7(&b); err != nil {
		return "", err
	}
	return b.String(), nil
}

type er7Writer interface {
	WriteByte(byte) error
	WriteString(string) (int, error)
}

func (m *Message) writeER7(b er7Writer) error {
	if m == nil || len(m.Segments) == 0 || m.Segments[0].Name != "MSH" {
		return fmt.Errorf("%w: the first segment must be MSH", ErrSyntax)
	}
	d := m.Delimiters
	if d.Field == 0 || d.Component == 0 || d.Repetition == 0 {
		d = DefaultDelimiters
	}
	for index, segment := range m.Segments {
		if !validSegmentName(segment.Name) {
			return &ParseError{Segment: index + 1, Err: fmt.Errorf("%w: invalid segment ID", ErrSyntax)}
		}
		if index > 0 && segment.Name == "MSH" {
			return &ParseError{Segment: index + 1, Name: "MSH", Err: errors.New("hl7: MSH after the first segment")}
		}
		if _, err := b.WriteString(segment.Name); err != nil {
			return err
		}
		fields := segment.Fields
		if segment.Name == "MSH" {
			if err := b.WriteByte(d.Field); err != nil {
				return err
			}
			if _, err := b.WriteString(d.EncodingCharacters()); err != nil {
				return err
			}
			if len(fields) > 2 {
				fields = fields[2:]
			} else {
				fields = nil
			}
		}
		fields = trimFields(fields)
		for _, field := range fields {
			if err := b.WriteByte(d.Field); err != nil {
				return err
			}
			if err := writeField(b, field, d); err != nil {
				return err
			}
		}
		if err := b.WriteByte('\r'); err != nil {
			return err
		}
	}
	return nil
}

func trimFields(fields []Field) []Field {
	for len(fields) > 0 && fields[len(fields)-1].IsEmpty() {
		fields = fields[:len(fields)-1]
	}
	return fields
}

func writeField(b er7Writer, field Field, d Delimiters) error {
	repetitions := field
	for len(repetitions) > 0 && Field(repetitions[len(repetitions)-1:]).IsEmpty() {
		repetitions = repetitions[:len(repetitions)-1]
	}
	for i, repetition := range repetitions {
		if i > 0 {
			if err := b.WriteByte(d.Repetition); err != nil {
				return err
			}
		}
		components := repetition
		for len(components) > 0 && componentEmpty(components[len(components)-1]) {
			components = components[:len(components)-1]
		}
		for j, component := range components {
			if j > 0 {
				if err := b.WriteByte(d.Component); err != nil {
					return err
				}
			}
			subs := component
			for len(subs) > 0 && subs[len(subs)-1] == "" {
				subs = subs[:len(subs)-1]
			}
			for k, sub := range subs {
				if k > 0 && d.Subcomponent != 0 {
					if err := b.WriteByte(d.Subcomponent); err != nil {
						return err
					}
				}
				if err := writeEscaped(b, sub, d); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func componentEmpty(c Component) bool {
	for _, sub := range c {
		if sub != "" {
			return false
		}
	}
	return true
}

// writeEscaped is unescape's inverse for delimiters; line breaks become
// \.br\ and other control characters \Xhh\. Raw runs and escape pieces
// go to the shared writer without materializing a second escaped string.
func writeEscaped(b er7Writer, value string, d Delimiters) error {
	if d.Escape == 0 {
		_, err := b.WriteString(value)
		return err
	}
	rawStart := 0
	for i := 0; i < len(value); i++ {
		if i-rawStart >= 2048 {
			if _, err := b.WriteString(value[rawStart:i]); err != nil {
				return err
			}
			rawStart = i
		}
		c := value[i]
		code := ""
		switch {
		case c == d.Escape:
			code = "E"
		case c == d.Field:
			code = "F"
		case c == d.Component:
			code = "S"
		case c == d.Repetition:
			code = "R"
		case d.Subcomponent != 0 && c == d.Subcomponent:
			code = "T"
		case d.Truncation != 0 && c == d.Truncation:
			code = "P"
		case c == '\n':
			code = ".br"
		case c < 0x20 || c == 0x7F:
			const hex = "0123456789ABCDEF"
			code = string([]byte{'X', hex[c>>4], hex[c&15]})
		default:
			continue
		}
		if _, err := b.WriteString(value[rawStart:i]); err != nil {
			return err
		}
		if err := b.WriteByte(d.Escape); err != nil {
			return err
		}
		if _, err := b.WriteString(code); err != nil {
			return err
		}
		if err := b.WriteByte(d.Escape); err != nil {
			return err
		}
		rawStart = i + 1
	}
	_, err := b.WriteString(value[rawStart:])
	return err
}
