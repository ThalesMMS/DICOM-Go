package encoding

import "testing"

func TestDecodeReplacingKeepsDecodableText(t *testing.T) {
	utf8Set, err := ParseCharacterSet("ISO_IR 192")
	if err != nil {
		t.Fatal(err)
	}
	korean, err := ParseCharacterSet("", "ISO 2022 IR 149")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		decode   func([]byte) (string, bool)
		raw      string
		want     string
		replaced bool
	}{
		{"valid UTF-8 is untouched", utf8Set.DecodeReplacing, "Müller^José", "Müller^José", false},
		{"one invalid byte", utf8Set.DecodeReplacing, "M\xfcller^Jos\xc3\xa9", "M\uFFFDller^José", true},
		{"a run of invalid bytes is one marker", utf8Set.DecodeReplacing, "AB\xff\xfe\xfdCD", "AB\uFFFDCD", true},
		{"invalid bytes at both ends", utf8Set.DecodeReplacing, "\xffAB\xfe", "\uFFFDAB\uFFFD", true},
		{"person name groups stay apart", utf8Set.DecodePersonNameReplacing, "Wang^XiaoDong=\xff\xfe^\xe5\xb0\x8f", "Wang^XiaoDong=\uFFFD^小", true},
		{"single value", utf8Set.DecodeSingleValueReplacing, "a\\b\xff", "a\\b\uFFFD", true},
		{"valid ISO 2022 is untouched", korean.DecodePersonNameReplacing, "Hong^Gildong=\x1b$)C\xc8\xab^\x1b$)C\xb1\xe6\xb5\xbf", "Hong^Gildong=홍^길동", false},
		{"broken ISO 2022 keeps the default repertoire", korean.DecodePersonNameReplacing, "Hong^Gildong=\xc8\xab^\xb1\xe6", "Hong^Gildong=\uFFFD^\uFFFD", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, replaced := tc.decode([]byte(tc.raw))
			if got != tc.want || replaced != tc.replaced {
				t.Fatalf("decode(%q) = %q, %t; want %q, %t", tc.raw, got, replaced, tc.want, tc.replaced)
			}
		})
	}
}

func TestReadDefaultRepertoireReplacingFollowsG0Designations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		raw      string
		want     string
		replaced bool
	}{
		{"ASCII", "Yamada^Tarou", "Yamada^Tarou", false},
		// The single-valued "ISO 2022 IR 87" that Orthanc declares in
		// JapaneseKanji mode: JIS X 0208 in G0 between ESC $ B and ESC ( B.
		{"JIS X 0208 in G0", "Yamada^Tarou=\x1b$B;3ED\x1b(B^\x1b$BB@O:\x1b(B=\x1b$B$d$^$@\x1b(B^\x1b$B$?$m$&\x1b(B",
			"Yamada^Tarou=\uFFFD^\uFFFD=\uFFFD^\uFFFD", true},
		// KS X 1001 in G1: only the high bytes are lost.
		{"KS X 1001 in G1", "Hong^Gildong=\x1b$)C\xc8\xab^\xb1\xe6\xb5\xbf", "Hong^Gildong=\uFFFD^\uFFFD", true},
		{"JIS X 0201 Katakana in G0", "\x1b(I1\x1b(J^x", "\uFFFD^x", true},
		{"a value left in a multi-byte set", "ab\x1b$Bcd", "ab\uFFFD", true},
		{"a truncated escape sequence", "ab\x1b", "ab", false},
		{"controls and high bytes", "a\x01b\x80\xffc\r\n", "a\uFFFDb\uFFFDc\r\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, replaced := ReadDefaultRepertoireReplacing([]byte(tc.raw))
			if got != tc.want || replaced != tc.replaced {
				t.Fatalf("ReadDefaultRepertoireReplacing(%q) = %q, %t; want %q, %t", tc.raw, got, replaced, tc.want, tc.replaced)
			}
		})
	}
}
