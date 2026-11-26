package hl7

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func xmlFixture(t *testing.T) *Message {
	t.Helper()
	m := NewMessage(MessageHeader{Type: MessageType{Code: "ADT", Trigger: "A08"}, ControlID: "synthetic-1", Version: "2.5.1", CharacterSet: "UNICODE UTF-8"})
	m.Delimiters = Delimiters{Field: '*', Component: '$', Repetition: '%', Escape: '!', Subcomponent: '?'}
	m.Segments[0].SetValue(1, "*")
	m.Segments[0].SetValue(2, "$%!?")
	s := m.Add("PID")
	s.SetField(1, Field{Repetition{Component{"a<b & c", "é"}, Component{"* $ % ! ?"}}, Repetition{Component{"second", ""}}})
	s.SetValue(3, Null)
	encoded, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	m, err = Parse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestStructuralXMLRoundTrip(t *testing.T) {
	m := xmlFixture(t)
	data, err := MarshalXMLMessage(m, XMLOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("a<b")) {
		t.Fatal("XML text was not escaped")
	}
	got, err := ParseXMLMessage(data, XMLOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m, got) {
		t.Fatal("ordered structural tree changed")
	}
	a, _ := m.Marshal()
	b, _ := got.Marshal()
	if !bytes.Equal(a, b) {
		t.Fatal("canonical ER7 changed")
	}
}

func TestStructuralXMLRefusals(t *testing.T) {
	good, err := MarshalXMLMessage(xmlFixture(t), XMLOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{
		`<!DOCTYPE hl7-message [<!ENTITY secret "sensitive-value">]>` + string(good),
		strings.Replace(string(good), `version="1"`, `version="2"`, 1),
		strings.Replace(string(good), `<hl7-message`, `<hl7-message xmlns="secret"`, 1),
		strings.Replace(string(good), `<segment`, `<segment extra="sensitive-value"`, 1),
		strings.Replace(string(good), `<field>`, `<unknown>`, 1),
		strings.Replace(string(good), `<component>`, `<component>sensitive-value`, 1),
		strings.Replace(string(good), `field="*"`, `field="|"`, 1),
		string(good) + string(good),
		`<?untrusted sensitive-value?>` + string(good),
		string(good[:len(good)-1]),
	}
	for i, data := range cases {
		t.Run(string(rune('A'+i)), func(t *testing.T) {
			_, err := ParseXMLMessage([]byte(data), XMLOptions{})
			if err == nil {
				t.Fatal("unsupported document accepted")
			}
			if strings.Contains(err.Error(), "sensitive-value") {
				t.Fatal("diagnostic exposed content")
			}
		})
	}
}

func TestStructuralXMLLimits(t *testing.T) {
	m := xmlFixture(t)
	good, err := MarshalXMLMessage(m, XMLOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []XMLOptions{{MaxBytes: 1}, {MaxNodes: 1}, {MaxSegments: 1}, {MaxFields: 1}, {MaxRepetitions: 1}} {
		if _, err := MarshalXMLMessage(m, o); !errors.Is(err, ErrLimit) {
			t.Fatalf("encode limit not enforced: %v", err)
		}
		if _, err := ParseXMLMessage(good, o); !errors.Is(err, ErrLimit) {
			t.Fatalf("decode limit not enforced: %v", err)
		}
	}
}

func TestStructuralXMLRefusesUnrepresentableText(t *testing.T) {
	for _, value := range []string{"bad\x01", string([]byte{0xff}), "bad\uffff"} {
		m := xmlFixture(t)
		m.Segments[1].SetValue(2, value)
		if _, err := MarshalXMLMessage(m, XMLOptions{}); !errors.Is(err, ErrXML) {
			t.Fatalf("invalid XML text accepted: %v", err)
		}
	}
	m := xmlFixture(t)
	m.Segments[0].Fields[0] = Components("*", "extra")
	if _, err := MarshalXMLMessage(m, XMLOptions{}); !errors.Is(err, ErrXML) {
		t.Fatal("ambiguous MSH delimiter field accepted")
	}
}

func TestStructuralXMLEmptyNodesAndDeclaration(t *testing.T) {
	m := xmlFixture(t)
	m.Segments[1].Fields = append(m.Segments[1].Fields, nil, Field{nil}, Field{Repetition{nil}}, Text("line\r\n\tend"))
	data, err := MarshalXMLMessage(m, XMLOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseXMLMessage(append([]byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"), data...), XMLOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m, got) {
		t.Fatal("empty nodes or whitespace text changed")
	}
}

func TestStructuralXMLNegativeLimitsAreRefused(t *testing.T) {
	m := xmlFixture(t)
	good, err := MarshalXMLMessage(m, XMLOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		options XMLOptions
	}{{"MaxBytes", XMLOptions{MaxBytes: -1}}, {"MaxNodes", XMLOptions{MaxNodes: -1}}, {"MaxSegments", XMLOptions{MaxSegments: -1}}, {"MaxFields", XMLOptions{MaxFields: -1}}, {"MaxRepetitions", XMLOptions{MaxRepetitions: -1}}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := MarshalXMLMessage(m, tc.options); !errors.Is(err, ErrLimit) {
				t.Errorf("negative encode limit accepted: %v", err)
			}
			if _, err := ParseXMLMessage(good, tc.options); !errors.Is(err, ErrLimit) {
				t.Errorf("negative decode limit accepted: %v", err)
			}
		})
	}
}
