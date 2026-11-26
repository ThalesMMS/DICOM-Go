package hl7

import (
	"bytes"
	"errors"
	"runtime"
	"strings"
	"testing"
)

func boundedAuthoredMessage(charset, value string) *Message {
	m := NewMessage(MessageHeader{Type: MessageType{Code: "ADT", Trigger: "A08"}, ControlID: "SYNTHETIC", Version: "2.5.1", CharacterSet: charset})
	m.Add("PID").SetValue(3, value)
	return m
}

func TestMarshalWithLimitMatchesCharacterSetBytesAndBoundaries(t *testing.T) {
	values := map[string]string{
		"ASCII": "SYNTHETIC", "8859/1": "é", "8859/2": "Ł", "8859/3": "Ħ",
		"8859/4": "Ā", "8859/5": "Ж", "8859/6": "ش", "8859/7": "Ω",
		"8859/8": "ש", "8859/9": "Ğ", "8859/15": "€", "UNICODE UTF-8": "𐍈",
		"ISO IR87": "日本", "GB 18030-2000": "中𐍈", "KS X 1001": "한", "BIG-5": "許",
	}
	for _, charset := range append([]string{"", "UTF-8"}, CharacterSets...) {
		t.Run(charset, func(t *testing.T) {
			value := values[charset]
			if value == "" {
				value = "SYNTHETIC"
			}
			m := boundedAuthoredMessage(charset, strings.Repeat(value, 1500))
			want, err := m.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			got, err := m.MarshalWithLimit(len(want))
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("encoded boundary or charset residue differs: %v", err)
			}
			if got, err := m.MarshalWithLimit(len(want) - 1); !errors.Is(err, ErrLimit) || got != nil {
				t.Fatalf("one byte below encoded boundary: %v / %d bytes", err, len(got))
			}
		})
	}
}

func TestMarshalWithLimitPreservesEscapesAndAuthoredStructure(t *testing.T) {
	for _, d := range []Delimiters{DefaultDelimiters, {Field: '#', Component: '$', Repetition: '*', Escape: '@', Subcomponent: '%', Truncation: '!'}, {Field: '|', Component: '^', Repetition: '~'}} {
		t.Run(d.EncodingCharacters(), func(t *testing.T) {
			m := boundedAuthoredMessage("UNICODE UTF-8", "SYNTHETIC")
			m.Delimiters = d
			shared := Text("field|component^repeat~escape\\sub&truncate!\n\r\x00\x7f𐍈")
			m.Add("ZXY").Fields = []Field{shared, shared, nil, {nil, {{"", "inner", ""}}, nil}, nil}
			want, err := m.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			got, err := m.MarshalWithLimit(len(want))
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("escaping/aliases/trailing empties changed: %v", err)
			}
			if got, err := m.MarshalWithLimit(len(want) - 1); !errors.Is(err, ErrLimit) || got != nil {
				t.Fatalf("structure output budget: %v", err)
			}
		})
	}
}

func TestMarshalWithLimitRefusesInvalidInputWithoutPartialOutput(t *testing.T) {
	for _, limit := range []int{0, -1} {
		if p, err := boundedAuthoredMessage("", "SYNTHETIC").MarshalWithLimit(limit); !errors.Is(err, ErrLimit) || p != nil {
			t.Fatalf("nonpositive limit: %v", err)
		}
	}
	var absent *Message
	if p, err := absent.MarshalWithLimit(1024); !errors.Is(err, ErrSyntax) || p != nil {
		t.Fatal(err)
	}
	for _, input := range []struct {
		charset string
		value   string
	}{{"ASCII", "SECRET-é"}, {"8859/1", "SECRET-𐍈"}, {"unsupported", "SECRET"}} {
		if p, err := boundedAuthoredMessage(input.charset, input.value).MarshalWithLimit(1024); !errors.Is(err, ErrCharset) || p != nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("charset failure/partial output/value diagnostic: %v", err)
		}
	}
	m := boundedAuthoredMessage("BIG-5", "SYNTHETIC")
	// Big5 許 is B3 5C. A custom 5C field delimiter before MSH-18
	// makes raw charset discovery ambiguous and must be refused.
	m.Delimiters = Delimiters{Field: '\\', Component: '^', Repetition: '~', Escape: '|', Subcomponent: '&'}
	m.Segments[0].SetValue(3, "許")
	if p, err := m.MarshalWithLimit(1024); !errors.Is(err, ErrCharset) || p != nil {
		t.Fatalf("ambiguous encoded header: %v", err)
	}
}

func TestMarshalWithLimitDoesNotMaterializeLargeAuthoredOutput(t *testing.T) {
	// Allocate caller input before measuring serializer workspace. This is a
	// bounded-allocation counterexample to whole-message materialization,
	// not a throughput benchmark or a general RSS guarantee.
	value := strings.Repeat("|\\\nSYNTHETIC", 1<<17)
	m := boundedAuthoredMessage("UNICODE UTF-8", value)
	shared := Text(value)
	m.Add("ZXY").Fields = []Field{shared, shared, shared}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	p, err := m.MarshalWithLimit(128)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrLimit) || p != nil {
		t.Fatalf("large authored output not refused: %v", err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 512<<10 {
		t.Fatalf("small output quota materialized large input: allocated %d bytes", allocated)
	}
	runtime.KeepAlive(m)
}

func TestMarshalWithLimitBoundsAuthoredCharsetNormalization(t *testing.T) {
	charset := strings.Repeat("secret", 1<<17)
	m := boundedAuthoredMessage(charset, "SYNTHETIC")
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	p, err := m.MarshalWithLimit(1)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrCharset) || p != nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("authored charset admission/diagnostic: %v", err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 512<<10 {
		t.Fatalf("charset normalization materialized authored input: allocated %d bytes", allocated)
	}
	runtime.KeepAlive(m)
}
