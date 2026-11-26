package encoding

import (
	"bytes"
	"errors"
	"testing"
)

func TestISO2022NormativePersonNameFixtures(t *testing.T) {
	tests := []struct {
		name  string
		codes []string
		text  string
		raw   []byte
	}{
		{
			name:  "DICOM PS3.5 H.3.1 Japanese",
			codes: []string{"", "ISO 2022 IR 87"},
			text:  "Yamada^Tarou=山田^太郎=やまだ^たろう",
			raw:   []byte("Yamada^Tarou=\x1b$B;3ED\x1b(B^\x1b$BB@O:\x1b(B=\x1b$B$d$^$@\x1b(B^\x1b$B$?$m$&\x1b(B"),
		},
		{
			name:  "DICOM PS3.5 H.3.2 Japanese with initial IR 13",
			codes: []string{"ISO 2022 IR 13", "ISO 2022 IR 87"},
			text:  "ﾔﾏﾀﾞ^ﾀﾛｳ=山田^太郎=やまだ^たろう",
			raw: []byte("\xd4\xcf\xc0\xde^\xc0\xdb\xb3=\x1b$B;3ED\x1b(J^\x1b$BB@O:\x1b(J=" +
				"\x1b$B$d$^$@\x1b(J^\x1b$B$?$m$&\x1b(J"),
		},
		{
			name:  "DICOM PS3.5 I.2 Korean",
			codes: []string{"", "ISO 2022 IR 149"},
			text:  "Hong^Gildong=洪^吉洞=홍^길동",
			raw: []byte("Hong^Gildong=\x1b$)C\xfb\xf3^\x1b$)C\xd1\xce\xd4\xd7=" +
				"\x1b$)C\xc8\xab^\x1b$)C\xb1\xe6\xb5\xbf"),
		},
		{
			name:  "DICOM PS3.5 K.2 Chinese",
			codes: []string{"", "ISO 2022 IR 58"},
			text:  "Zhang^XiaoDong=张^小东=",
			raw:   []byte("Zhang^XiaoDong=\x1b$)A\xd5\xc5^\x1b$)A\xd0\xa1\xb6\xab="),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			characterSet, err := ParseCharacterSet(test.codes...)
			if err != nil {
				t.Fatalf("ParseCharacterSet() error = %v", err)
			}
			decoded, err := characterSet.DecodePersonName(test.raw)
			if err != nil {
				t.Fatalf("DecodePersonName() error = %v", err)
			}
			if decoded != test.text {
				t.Fatalf("DecodePersonName() = %q, want %q", decoded, test.text)
			}
			encoded, err := characterSet.EncodePersonName(test.text)
			if err != nil {
				t.Fatalf("EncodePersonName() error = %v", err)
			}
			if !bytes.Equal(encoded, test.raw) {
				t.Fatalf("EncodePersonName() = % X, want % X", encoded, test.raw)
			}
		})
	}
}

func TestISO2022IndependentPydicomJapaneseIR159Fixture(t *testing.T) {
	// Golden bytes are from pydicom 3.0.1's independently implemented
	// test_japanese_multi_byte_personname fixture:
	// https://github.com/pydicom/pydicom/blob/v3.0.1/tests/test_charset.py
	// The character 鷗 is from JIS X 0212 (ISO-IR 159), while the surrounding
	// characters use IR 87.
	characterSet, err := ParseCharacterSet("ISO 2022 IR 6", "ISO 2022 IR 87", "ISO 2022 IR 159")
	if err != nil {
		t.Fatalf("ParseCharacterSet() error = %v", err)
	}
	text := "Mori^Ogai=森^鷗外=もり^おうがい"
	raw := []byte(
		"Mori^Ogai=\x1b$B?9\x1b(B^\x1b$(Dl?\x1b$B30\x1b(B=" +
			"\x1b$B$b$j\x1b(B^\x1b$B$*$&$,$$\x1b(B",
	)

	decoded, err := characterSet.DecodePersonName(raw)
	if err != nil {
		t.Fatalf("DecodePersonName() error = %v", err)
	}
	if decoded != text {
		t.Fatalf("DecodePersonName() = %q, want %q", decoded, text)
	}
	encoded, err := characterSet.EncodePersonName(text)
	if err != nil {
		t.Fatalf("EncodePersonName() error = %v", err)
	}
	if !bytes.Equal(encoded, raw) {
		t.Fatalf("EncodePersonName() = % X, want % X", encoded, raw)
	}
}

func TestISO2022IndependentPydicomMixedRepertoireFixture(t *testing.T) {
	characterSet, err := ParseCharacterSet("ISO 2022 IR 13", "ISO 2022 IR 87", "ISO 2022 IR 159")
	if err != nil {
		t.Fatalf("ParseCharacterSet() error = %v", err)
	}
	text := "あaｱア齩"
	// pydicom 3.0.1 emits an explicit IR 13 designation before the Katakana.
	// It is redundant because IR 13 is initially designated in G1, but valid.
	pydicomRaw := []byte("\x1b$B$\"\x1b(Ja\x1b)I\xb1\x1b$B%\"\x1b$(DmN\x1b(J")
	canonicalRaw := []byte("\x1b$B$\"\x1b(Ja\xb1\x1b$B%\"\x1b$(DmN\x1b(J")

	decoded, err := characterSet.Decode(pydicomRaw)
	if err != nil {
		t.Fatalf("Decode(pydicom fixture) error = %v", err)
	}
	if decoded != text {
		t.Fatalf("Decode(pydicom fixture) = %q, want %q", decoded, text)
	}
	encoded, err := characterSet.Encode(text)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if !bytes.Equal(encoded, canonicalRaw) {
		t.Fatalf("Encode() = % X, want canonical % X", encoded, canonicalRaw)
	}
}

func TestISO2022IndependentPydicomSingleByteExtensionFixture(t *testing.T) {
	characterSet, err := ParseCharacterSet("", "ISO 2022 IR 13")
	if err != nil {
		t.Fatalf("ParseCharacterSet() error = %v", err)
	}
	text := "==ﾔﾏﾀﾞ^ﾀﾛｳ"
	raw := []byte("==\x1b)I\xd4\xcf\xc0\xde^\x1b)I\xc0\xdb\xb3")

	decoded, err := characterSet.DecodePersonName(raw)
	if err != nil {
		t.Fatalf("DecodePersonName() error = %v", err)
	}
	if decoded != text {
		t.Fatalf("DecodePersonName() = %q, want %q", decoded, text)
	}
	encoded, err := characterSet.EncodePersonName(text)
	if err != nil {
		t.Fatalf("EncodePersonName() error = %v", err)
	}
	if !bytes.Equal(encoded, raw) {
		t.Fatalf("EncodePersonName() = % X, want % X", encoded, raw)
	}
}

func TestISO2022ResetsAtValueAndLineBoundaries(t *testing.T) {
	characterSet, err := ParseCharacterSet("", "ISO 2022 IR 149")
	if err != nil {
		t.Fatalf("ParseCharacterSet() error = %v", err)
	}
	raw := []byte("\x1b$)C\xc8\xab\\\x1b$)C\xb1\xe6\r\n\x1b$)C\xb5\xbf")

	decoded, err := characterSet.Decode(raw)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if decoded != "홍\\길\r\n동" {
		t.Fatalf("Decode() = %q", decoded)
	}
	encoded, err := characterSet.Encode(decoded)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if !bytes.Equal(encoded, raw) {
		t.Fatalf("Encode() = % X, want % X", encoded, raw)
	}
}

func TestISO2022RestoresNonDefaultInitialG1(t *testing.T) {
	characterSet, err := ParseCharacterSet("ISO 2022 IR 100", "ISO 2022 IR 126")
	if err != nil {
		t.Fatalf("ParseCharacterSet() error = %v", err)
	}
	text := "Buc=Δ"
	raw := []byte("Buc=\x1b-F\xc4\x1b-A")

	encoded, err := characterSet.EncodePersonName(text)
	if err != nil {
		t.Fatalf("EncodePersonName() error = %v", err)
	}
	if !bytes.Equal(encoded, raw) {
		t.Fatalf("EncodePersonName() = % X, want % X", encoded, raw)
	}
	decoded, err := characterSet.DecodePersonName(raw)
	if err != nil {
		t.Fatalf("DecodePersonName() error = %v", err)
	}
	if decoded != text {
		t.Fatalf("DecodePersonName() = %q, want %q", decoded, text)
	}
	if _, err := characterSet.DecodePersonName(raw[:len(raw)-3]); !errors.Is(err, ErrInvalidCodeExtension) {
		t.Fatalf("DecodePersonName(missing restore) error = %v, want ErrInvalidCodeExtension", err)
	}
}

func TestISO2022RestoresNonDefaultInitialG1AtEveryBoundary(t *testing.T) {
	characterSet, err := ParseCharacterSet("ISO 2022 IR 100", "ISO 2022 IR 126")
	if err != nil {
		t.Fatalf("ParseCharacterSet() error = %v", err)
	}
	tests := []struct {
		name       string
		personName bool
		text       string
	}{
		{name: "value and controls", text: "Δ\\Δ\tΔ\rΔ\nΔ\fΔ"},
		{name: "PN delimiters", personName: true, text: "=Δ^Δ=Δ"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var (
				raw []byte
				err error
			)
			if test.personName {
				raw, err = characterSet.EncodePersonName(test.text)
			} else {
				raw, err = characterSet.Encode(test.text)
			}
			if err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
			for offset := 0; offset < len(raw); offset++ {
				if !isISO2022ResetByte(raw[offset], test.personName, true) {
					continue
				}
				if offset == 0 {
					continue
				}
				if offset < 3 || !bytes.Equal(raw[offset-3:offset], []byte("\x1b-A")) {
					t.Fatalf("boundary at %d lacks initial G1 restore in % X", offset, raw)
				}
			}
			var decoded string
			if test.personName {
				decoded, err = characterSet.DecodePersonName(raw)
			} else {
				decoded, err = characterSet.Decode(raw)
			}
			if err != nil || decoded != test.text {
				t.Fatalf("Decode() = (%q, %v), want (%q, nil)", decoded, err, test.text)
			}
		})
	}
}

func TestISO2022MalformedInputReportsBoundedOffset(t *testing.T) {
	characterSet, err := ParseCharacterSet("", "ISO 2022 IR 87")
	if err != nil {
		t.Fatalf("ParseCharacterSet() error = %v", err)
	}
	tests := []struct {
		name   string
		raw    []byte
		offset int
		pn     bool
	}{
		{name: "lone escape", raw: []byte("AB\x1b"), offset: 2},
		{name: "truncated escape", raw: []byte("AB\x1b$("), offset: 2},
		{name: "unknown escape", raw: []byte("AB\x1b%G"), offset: 2},
		{name: "undeclared escape", raw: []byte("AB\x1b$)C"), offset: 2},
		{name: "forbidden locking shift", raw: []byte("AB\x0e"), offset: 2},
		{name: "forbidden C1", raw: []byte("AB\x80"), offset: 2},
		{name: "truncated pair", raw: []byte("\x1b$B!"), offset: 3},
		{name: "undefined JIS X 0208 pair", raw: []byte("\x1b$B~~\x1b(B"), offset: 3},
		{name: "truncated escape before delimiter", raw: []byte("AB\x1b$^"), offset: 2, pn: true},
		{name: "forbidden shift in", raw: []byte("AB\x0f"), offset: 2},
		{name: "forbidden single shift 2", raw: []byte("AB\x8e"), offset: 2},
		{name: "forbidden single shift 3", raw: []byte("AB\x8f"), offset: 2},
		{name: "forbidden G2 designation", raw: []byte("AB\x1b*B"), offset: 2},
		{name: "forbidden G3 designation", raw: []byte("AB\x1b+B"), offset: 2},
		{name: "missing restore before PN delimiter", raw: []byte("=\x1b$B;3^"), offset: 6, pn: true},
		{name: "missing restore at end", raw: []byte("\x1b$B;3"), offset: 5},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var decodeErr error
			if test.pn {
				_, decodeErr = characterSet.DecodePersonName(test.raw)
			} else {
				_, decodeErr = characterSet.Decode(test.raw)
			}
			if !errors.Is(decodeErr, ErrInvalidCodeExtension) {
				t.Fatalf("error = %v, want ErrInvalidCodeExtension", decodeErr)
			}
			var detail *CodeExtensionError
			if !errors.As(decodeErr, &detail) {
				t.Fatalf("error = %v, want *CodeExtensionError", decodeErr)
			}
			if detail.Offset != test.offset {
				t.Fatalf("offset = %d, want %d", detail.Offset, test.offset)
			}
			if len(detail.Sequence) > 4 {
				t.Fatalf("sequence length = %d, want <= 4", len(detail.Sequence))
			}
		})
	}
}

func TestISO2022RequiresG1RedesignationAfterPNDelimiter(t *testing.T) {
	characterSet, err := ParseCharacterSet("", "ISO 2022 IR 149")
	if err != nil {
		t.Fatalf("ParseCharacterSet() error = %v", err)
	}
	_, err = characterSet.DecodePersonName([]byte("=\x1b$)C\xc8\xab^\xb1\xe6"))
	var detail *CodeExtensionError
	if !errors.As(err, &detail) || detail.Offset != 8 {
		t.Fatalf("DecodePersonName() error = %v, want CodeExtensionError at byte 8", err)
	}
}

func TestCodeExtensionErrorNilMessage(t *testing.T) {
	var decodeErr *CodeExtensionError
	if got := decodeErr.Error(); got != ErrInvalidCodeExtension.Error() {
		t.Fatalf("nil CodeExtensionError.Error() = %q", got)
	}
}

func TestISO2022IR58UsesGB2312Punctuation(t *testing.T) {
	// These cells follow the pinned DCMTK GB 2312 row 1 fixture.
	const raw = "\x1b$)A\xa1\xa4\xa1\xaa"
	const punctuation = "\u30fb\u2015"
	const gbkPunctuation = "\u00b7\u2014"
	charset, err := ParseCharacterSet("", "ISO 2022 IR 58")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := charset.Decode([]byte(raw)); err != nil || got != punctuation {
		t.Errorf("Decode() = %U, %v; want %U", []rune(got), err, []rune(punctuation))
	}
	if got, err := charset.Encode(punctuation); err != nil || string(got) != raw {
		t.Errorf("Encode() = % X, %v; want % X", got, err, raw)
	}
	for _, value := range gbkPunctuation {
		if _, err := charset.Encode(string(value)); !errors.Is(err, ErrUnrepresentableCharacter) {
			t.Errorf("Encode(%U) error = %v; want ErrUnrepresentableCharacter", value, err)
		}
	}

	for _, term := range []string{"GBK", "GB18030"} {
		t.Run(term, func(t *testing.T) {
			standalone, err := ParseCharacterSet(term)
			if err != nil {
				t.Fatal(err)
			}
			cells := raw[4:]
			if got, err := standalone.Decode([]byte(cells)); err != nil || got != gbkPunctuation {
				t.Errorf("Decode() = %U, %v; want %U", []rune(got), err, []rune(gbkPunctuation))
			}
			if got, err := standalone.Encode(gbkPunctuation); err != nil || string(got) != cells {
				t.Errorf("Encode() = % X, %v; want % X", got, err, cells)
			}
		})
	}
}

// TestISO2022TwoByteG0DelimiterValuedFirstByte pins #2030: while JIS X 0208
// or JIS X 0212 is invoked in G0, a kanji whose first byte is 3D ('='), 5E
// ('^') or 5C ('\') is a character, not a delimiter that would demand the
// initial repertoire first.
func TestISO2022TwoByteG0DelimiterValuedFirstByte(t *testing.T) {
	tests := []struct {
		name  string
		codes []string
		pn    bool
		text  string
		raw   string
	}{
		{
			// 春 is JIS X 0208 3D55 ("=U"); ま is 245E, whose second byte is '^'.
			name:  "IR 87 PN with 春",
			codes: []string{"", "ISO 2022 IR 87"},
			pn:    true,
			text:  "Yamada^Haruko=山田^春子=やまだ^はるこ",
			raw:   "Yamada^Haruko=\x1b$B;3ED\x1b(B^\x1b$B=U;R\x1b(B=\x1b$B$d$^$@\x1b(B^\x1b$B$O$k$3\x1b(B",
		},
		{
			// 棔 is JIS X 0208 5C21 ("\!").
			name:  "IR 87 LO with 5C first byte",
			codes: []string{"", "ISO 2022 IR 87"},
			text:  "棔子\\CT",
			raw:   "\x1b$B\\!;R\x1b(B\\CT",
		},
		{
			// 彯, 誶 and 蠺 are JIS X 0212 3D21, 5E21 and 5C21.
			name:  "IR 159 PN with 3D, 5E and 5C first bytes",
			codes: []string{"", "ISO 2022 IR 159"},
			pn:    true,
			text:  "A^B=彯^誶蠺",
			raw:   "A^B=\x1b$(D=!\x1b(B^\x1b$(D^!\\!\x1b(B",
		},
		{
			name:  "IR 159 LO with 5C first byte",
			codes: []string{"", "ISO 2022 IR 87", "ISO 2022 IR 159"},
			text:  "蠺\\山",
			raw:   "\x1b$(D\\!\x1b(B\\\x1b$B;3\x1b(B",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			characterSet, err := ParseCharacterSet(test.codes...)
			if err != nil {
				t.Fatalf("ParseCharacterSet() error = %v", err)
			}
			decode, decodeReplacing, encode := characterSet.Decode, characterSet.DecodeReplacing, characterSet.Encode
			if test.pn {
				decode, decodeReplacing, encode = characterSet.DecodePersonName, characterSet.DecodePersonNameReplacing, characterSet.EncodePersonName
			}
			decoded, err := decode([]byte(test.raw))
			if err != nil || decoded != test.text {
				t.Fatalf("decode() = %q, %v; want %q", decoded, err, test.text)
			}
			if replacing, replaced := decodeReplacing([]byte(test.raw)); replaced || replacing != test.text {
				t.Fatalf("decode replacing = %q, replaced %t; want %q", replacing, replaced, test.text)
			}
			encoded, err := encode(test.text)
			if err != nil || string(encoded) != test.raw {
				t.Fatalf("encode() = % X, %v; want % X", encoded, err, test.raw)
			}
			if again, err := decode(encoded); err != nil || again != test.text {
				t.Fatalf("decode(encode()) = %q, %v; want %q", again, err, test.text)
			}
		})
	}
}
