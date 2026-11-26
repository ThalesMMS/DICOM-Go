// Package hl7 parses, builds and acknowledges HL7 version 2 messages: the
// ER7 ("pipe and hat") encoding with its delimiters and escape sequences,
// MSH-18 character sets decoded to UTF-8, original-mode acknowledgments
// (AA, AE, AR), typed ADT admissions, registrations, demographic updates, identifier
// corrections and merges mapped to patient and visit attributes, and the IHE PDQ (QBP^Q22) and PIX (QBP^Q23)
// queries with their responses. Package net/mllp carries the messages.
//
// The package is a clean-room implementation from the HL7 v2.5.1
// specification (chapters 2 and 3) and the IHE IT Infrastructure Technical
// Framework (ITI-9, ITI-21, ITI-30). Errors name segments, fields and
// positions, never field values, which carry patient data.
package hl7

import "strings"

// Delimiters are a message's encoding characters, from MSH-1 and MSH-2.
type Delimiters struct {
	Field        byte
	Component    byte
	Repetition   byte
	Escape       byte // zero when MSH-2 declares none
	Subcomponent byte // zero when MSH-2 declares none
	// Truncation is the HL7 v2.7 truncation character, zero when absent.
	Truncation byte
}

// DefaultDelimiters are "|^~\&".
var DefaultDelimiters = Delimiters{Field: '|', Component: '^', Repetition: '~', Escape: '\\', Subcomponent: '&'}

// EncodingCharacters returns MSH-2.
func (d Delimiters) EncodingCharacters() string {
	var b strings.Builder
	b.WriteByte(d.Component)
	b.WriteByte(d.Repetition)
	if d.Escape != 0 {
		b.WriteByte(d.Escape)
		if d.Subcomponent != 0 {
			b.WriteByte(d.Subcomponent)
			if d.Truncation != 0 {
				b.WriteByte(d.Truncation)
			}
		}
	}
	return b.String()
}

// Component is a component's subcomponents, unescaped.
type Component []string

// Repetition is one occurrence of a field: its components.
type Repetition []Component

// Field is a field's repetitions.
type Field []Repetition

// Segment is one segment: a three-character name and its fields. Fields[0]
// is field 1; for MSH, field 1 is the field separator and field 2 the
// encoding characters, both literal.
type Segment struct {
	Name   string
	Fields []Field
}

// Message is a parsed HL7 v2 message. Values are UTF-8 and unescaped.
type Message struct {
	Delimiters Delimiters
	Segments   []Segment
}

// Sub returns subcomponent n (1-based), or "".
func (c Component) Sub(n int) string {
	if n < 1 || n > len(c) {
		return ""
	}
	return c[n-1]
}

// String returns the first subcomponent.
func (c Component) String() string { return c.Sub(1) }

// Component returns component n (1-based), or nil.
func (r Repetition) Component(n int) Component {
	if n < 1 || n > len(r) {
		return nil
	}
	return r[n-1]
}

// String returns component n's first subcomponent.
func (r Repetition) String(n int) string { return r.Component(n).Sub(1) }

// First returns the first repetition, or nil.
func (f Field) First() Repetition {
	if len(f) == 0 {
		return nil
	}
	return f[0]
}

// String returns the first repetition's first component's first
// subcomponent.
func (f Field) String() string { return f.First().String(1) }

// IsEmpty reports whether the field has no value at all.
func (f Field) IsEmpty() bool {
	for _, repetition := range f {
		for _, component := range repetition {
			for _, sub := range component {
				if sub != "" {
					return false
				}
			}
		}
	}
	return true
}

// Null is the HL7 null value: a field sent as "" asks the receiver to
// delete the value it holds, unlike an empty field, which leaves it as is.
const Null = `""`

// IsNull reports whether the field is the HL7 null value "".
func (f Field) IsNull() bool {
	return len(f) == 1 && len(f[0]) == 1 && len(f[0][0]) == 1 && f[0][0][0] == Null
}

// Field returns field n (1-based), or nil.
func (s Segment) Field(n int) Field {
	if n < 1 || n > len(s.Fields) {
		return nil
	}
	return s.Fields[n-1]
}

// Value returns field n, component c, subcomponent sub of the first
// repetition (all 1-based).
func (s Segment) Value(n, c, sub int) string {
	return s.Field(n).First().Component(c).Sub(sub)
}

// SetField stores field n (1-based), growing the segment as needed.
func (s *Segment) SetField(n int, field Field) {
	if n < 1 {
		return
	}
	for len(s.Fields) < n {
		s.Fields = append(s.Fields, nil)
	}
	s.Fields[n-1] = field
}

// SetValue stores a single-valued field n.
func (s *Segment) SetValue(n int, value string) {
	s.SetField(n, Text(value))
}

// Text is a single-valued field.
func Text(value string) Field {
	return Field{Repetition{Component{value}}}
}

// Components is a field of one repetition with the given components.
func Components(values ...string) Field {
	repetition := make(Repetition, len(values))
	for i, value := range values {
		repetition[i] = Component{value}
	}
	return Field{repetition}
}

// Segment returns the first segment named name.
func (m *Message) Segment(name string) (*Segment, bool) {
	if m == nil {
		return nil, false
	}
	for i := range m.Segments {
		if m.Segments[i].Name == name {
			return &m.Segments[i], true
		}
	}
	return nil, false
}

// All returns every segment named name, in order.
func (m *Message) All(name string) []*Segment {
	if m == nil {
		return nil
	}
	var out []*Segment
	for i := range m.Segments {
		if m.Segments[i].Name == name {
			out = append(out, &m.Segments[i])
		}
	}
	return out
}

// Header returns the typed MSH segment.
func (m *Message) Header() MessageHeader {
	msh, _ := m.Segment("MSH")
	if msh == nil {
		return MessageHeader{}
	}
	return headerFrom(msh)
}

// HierarchicDesignator is the HL7 HD data type: an application, facility or
// assigning authority.
type HierarchicDesignator struct {
	NamespaceID     string
	UniversalID     string
	UniversalIDType string
}

func hdFrom(c Repetition) HierarchicDesignator {
	return HierarchicDesignator{NamespaceID: c.String(1), UniversalID: c.String(2), UniversalIDType: c.String(3)}
}

func hdFromComponent(c Component) HierarchicDesignator {
	return HierarchicDesignator{NamespaceID: c.Sub(1), UniversalID: c.Sub(2), UniversalIDType: c.Sub(3)}
}

func (h HierarchicDesignator) field() Field {
	return Components(h.NamespaceID, h.UniversalID, h.UniversalIDType)
}

func (h HierarchicDesignator) component() Component {
	return Component{h.NamespaceID, h.UniversalID, h.UniversalIDType}
}

// MessageType is MSH-9: message code, trigger event and structure, such as
// ADT^A08^ADT_A01.
type MessageType struct {
	Code      string
	Trigger   string
	Structure string
}

// String renders the type as CODE^TRIGGER.
func (t MessageType) String() string {
	if t.Trigger == "" {
		return t.Code
	}
	return t.Code + "^" + t.Trigger
}

// MessageHeader is the typed MSH segment.
type MessageHeader struct {
	SendingApplication   HierarchicDesignator // MSH-3
	SendingFacility      HierarchicDesignator // MSH-4
	ReceivingApplication HierarchicDesignator // MSH-5
	ReceivingFacility    HierarchicDesignator // MSH-6
	DateTime             string               // MSH-7
	Type                 MessageType          // MSH-9
	ControlID            string               // MSH-10
	ProcessingID         string               // MSH-11
	Version              string               // MSH-12
	// CharacterSet is MSH-18's first repetition, "" for the default ASCII.
	CharacterSet string
}

func headerFrom(msh *Segment) MessageHeader {
	messageType := msh.Field(9).First()
	return MessageHeader{
		SendingApplication:   hdFrom(msh.Field(3).First()),
		SendingFacility:      hdFrom(msh.Field(4).First()),
		ReceivingApplication: hdFrom(msh.Field(5).First()),
		ReceivingFacility:    hdFrom(msh.Field(6).First()),
		DateTime:             msh.Field(7).String(),
		Type:                 MessageType{Code: messageType.String(1), Trigger: messageType.String(2), Structure: messageType.String(3)},
		ControlID:            msh.Field(10).String(),
		ProcessingID:         msh.Field(11).String(),
		Version:              msh.Field(12).String(),
		CharacterSet:         msh.Field(18).String(),
	}
}

// NewMessage starts a message with an MSH segment built from header and the
// default delimiters.
func NewMessage(header MessageHeader) *Message {
	msh := Segment{Name: "MSH"}
	msh.SetValue(1, string(DefaultDelimiters.Field))
	msh.SetValue(2, DefaultDelimiters.EncodingCharacters())
	msh.SetField(3, header.SendingApplication.field())
	msh.SetField(4, header.SendingFacility.field())
	msh.SetField(5, header.ReceivingApplication.field())
	msh.SetField(6, header.ReceivingFacility.field())
	msh.SetValue(7, header.DateTime)
	msh.SetField(9, Components(header.Type.Code, header.Type.Trigger, header.Type.Structure))
	msh.SetValue(10, header.ControlID)
	msh.SetValue(11, header.ProcessingID)
	msh.SetValue(12, header.Version)
	if header.CharacterSet != "" {
		msh.SetValue(18, header.CharacterSet)
	}
	return &Message{Delimiters: DefaultDelimiters, Segments: []Segment{msh}}
}

// Add appends a segment and returns it for filling; the pointer is valid
// until the next Add.
func (m *Message) Add(name string) *Segment {
	m.Segments = append(m.Segments, Segment{Name: name})
	return &m.Segments[len(m.Segments)-1]
}
