package hl7

import (
	"bytes"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const charsetTestHeader = `MSH|^~\&|REG|NORTH|VIEW|SOUTH|20261115100500||ADT^A08^ADT_A01|CHARSET-17|P|2.5.1`

func TestER7DeclaredCharacterSetsUseIndependentWireVectors(t *testing.T) {
	// The octets are independent of the Go serializer. Include a Big5 trail
	// byte equal to a field delimiter and a stateful ISO 2022 sequence.
	vectors := []struct {
		charset string
		want    string
		octets  string
	}{
		{"ASCII", "T", "\x54"},
		{"8859/1", "ñ", "\xf1"},
		{"8859/2", "ą", "\xb1"},
		{"8859/3", "ċ", "\xe5"},
		{"8859/4", "Ū", "\xde"},
		{"8859/5", "Ю", "\xce"},
		{"8859/6", "م", "\xe5"},
		{"8859/7", "β", "\xe2"},
		{"8859/8", "ש", "\xf9"},
		{"8859/9", "İ", "\xdd"},
		{"8859/15", "Œ", "\xbc"},
		{"UNICODE UTF-8", "界", "\xe7\x95\x8c"},
		{"ISO IR87", "木", "\x1b$BLZ\x1b(B"},
		{"GB 18030-2000", "𠀀", "\x95\x32\x82\x36"},
		{"KS X 1001", "한", "\xc7\xd1"},
		{"BIG-5", "吜", "\xca\x7c"},
	}
	for _, vector := range vectors {
		t.Run(vector.charset, func(t *testing.T) {
			header := charsetTestHeader + "||||||" + vector.charset
			canonical := message(header, "ZXY|FIRST^"+vector.octets+"&AUX~LAST|AFTER")
			escaped := `\X` + strings.ToUpper(hex.EncodeToString([]byte(vector.octets))) + `\`
			for _, input := range []struct {
				name string
				wire []byte
			}{
				{"native", canonical},
				{"hex-escaped", message(header, "ZXY|FIRST^"+escaped+"&AUX~LAST|AFTER")},
			} {
				t.Run(input.name, func(t *testing.T) {
					m, err := Parse(input.wire)
					if err != nil {
						t.Fatal(err)
					}
					z, ok := m.Segment("ZXY")
					want := Field{{{"FIRST"}, {vector.want, "AUX"}}, {{"LAST"}}}
					if !ok || len(z.Fields) != 2 || !reflect.DeepEqual(z.Field(1), want) || z.Field(2).String() != "AFTER" {
						t.Fatalf("decoded segment = %+v, want ZXY-1 %v and ZXY-2 AFTER", z, want)
					}
					if got := m.Header().CharacterSet; got != vector.charset {
						t.Fatalf("MSH-18 = %q, want %q", got, vector.charset)
					}
					got, err := m.Marshal()
					if err != nil || !bytes.Equal(got, canonical) {
						t.Fatalf("encoded = %x, %v, want %x", got, err, canonical)
					}
				})
			}
		})
	}
}

func TestER7CharacterSetDeclarationPrecedenceAndRefusal(t *testing.T) {
	const secondary = "UNSUPPORTED-ALTERNATE"
	const marker = "SYNTHETIC-DO-NOT-REPORT"
	for _, tc := range []struct {
		name          string
		declarations  string
		defaultSet    string
		value         string
		wantValue     string
		wantHeaderSet string
	}{
		{"primary over default", "8859/7", "UNICODE UTF-8", "\xe2", "β", "8859/7"},
		{"only first declaration selects decoder", "8859/7~" + secondary, "ASCII", "\xe2", "β", "8859/7"},
		{"default applies to missing declaration", "", "8859/7", "\xe2", "β", ""},
		{"unknown primary overrides supported secondary and default", marker + "~8859/7", "8859/7", "\xe2", "", ""},
		{"explicit ASCII cannot fall back", "ASCII", "8859/7", "\xe2", "", ""},
		{"invalid UTF-8 cannot fall back", "UNICODE UTF-8", "8859/7", "\xe2", "", ""},
		{"field content resembling an escape is no declaration", `\X383835392F37\`, "8859/7", "\xe2", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := charsetTestHeader
			if tc.declarations != "" {
				header += "||||||" + tc.declarations
			}
			m, err := ParseWithOptions(message(header, "ZXY|"+tc.value+marker), ParseOptions{DefaultCharacterSet: tc.defaultSet})
			if tc.wantValue == "" {
				var parseErr *ParseError
				if m != nil || !errors.Is(err, ErrCharset) || !errors.As(err, &parseErr) || parseErr.Segment != 1 ||
					parseErr.Name != "MSH" || parseErr.Field != 18 || strings.Contains(err.Error(), marker) {
					t.Fatalf("character set refusal did not identify MSH-18 without exposing content: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			z, ok := m.Segment("ZXY")
			if !ok {
				t.Fatal("ZXY segment missing after successful parse")
			}
			if got := z.Field(1).String(); got != tc.wantValue+marker {
				t.Fatalf("ZXY-1 = %q, want %q", got, tc.wantValue+marker)
			}
			msh, _ := m.Segment("MSH")
			if got := m.Header().CharacterSet; got != tc.wantHeaderSet {
				t.Fatalf("MSH-18 = %q, want %q", got, tc.wantHeaderSet)
			}
			if strings.Contains(tc.declarations, "~") {
				values := msh.Field(18)
				if len(values) != 2 || values[1].String(1) != secondary {
					t.Fatalf("alternate declaration lost: %v", values)
				}
			}
		})
	}
}
