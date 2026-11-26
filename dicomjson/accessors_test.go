package dicomjson

import "testing"

func TestElementStringsExtractsTextNumbersAndPersonNames(t *testing.T) {
	dataset := Dataset{
		"00100010": {VR: "PN", Value: []any{map[string]any{"Alphabetic": "DOE^JANE"}}},
		"00201208": {VR: "IS", Value: []any{float64(7)}},
		"00080061": {VR: "CS", Value: []any{"CT", "MR"}},
	}

	if got := ElementString(dataset, "00100010"); got != "DOE^JANE" {
		t.Fatalf("ElementString(PN) = %q", got)
	}
	if got := ElementString(dataset, "00201208"); got != "7" {
		t.Fatalf("ElementString(number) = %q", got)
	}
	got := ElementStrings(dataset, "00080061")
	if len(got) != 2 || got[0] != "CT" || got[1] != "MR" {
		t.Fatalf("ElementStrings(multivalue) = %#v", got)
	}
}

func TestElementStringKeepsEveryPersonNameComponentGroup(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value map[string]any
		want  string
	}{
		{"three groups", map[string]any{"Alphabetic": "Yamada^Tarou", "Ideographic": "山田^太郎", "Phonetic": "やまだ^たろう"}, "Yamada^Tarou=山田^太郎=やまだ^たろう"},
		{"alphabetic and ideographic", map[string]any{"Alphabetic": "Wang^XiaoDong", "Ideographic": "王^小東"}, "Wang^XiaoDong=王^小東"},
		{"ideographic only", map[string]any{"Ideographic": "柳^明"}, "=柳^明"},
		{"phonetic only", map[string]any{"Phonetic": "やまだ^たろう"}, "==やまだ^たろう"},
		{"alphabetic and phonetic", map[string]any{"Alphabetic": "Yamada^Tarou", "Phonetic": "やまだ^たろう"}, "Yamada^Tarou==やまだ^たろう"},
		{"no group", map[string]any{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataset := Dataset{"00100010": {VR: "PN", Value: []any{tc.value}}}
			if got := ElementString(dataset, "00100010"); got != tc.want {
				t.Fatalf("ElementString(PN) = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestElementStringsNormalizesTagLookupAndSkipsBlankValues(t *testing.T) {
	dataset := Dataset{
		"00080050": {VR: "SH", Value: []any{"", " ACC-1 "}},
	}

	got := ElementStrings(dataset, "00080050")
	if len(got) != 1 || got[0] != "ACC-1" {
		t.Fatalf("ElementStrings() = %#v, want [ACC-1]", got)
	}
}

// A person name keeps every component group in the DICOM PN form, so a
// QIDO-RS match shows the same name as a C-FIND match.
func TestElementStringKeepsPersonNameComponentGroups(t *testing.T) {
	for _, test := range []struct {
		value map[string]any
		want  string
	}{
		{map[string]any{"Alphabetic": "Wang^XiaoDong", "Ideographic": "王^小東"}, "Wang^XiaoDong=王^小東"},
		{map[string]any{"Alphabetic": "Hong^Gildong", "Ideographic": "洪^吉洞", "Phonetic": "홍^길동"}, "Hong^Gildong=洪^吉洞=홍^길동"},
		{map[string]any{"Alphabetic": "Yamada^Tarou", "Phonetic": "やまだ^たろう"}, "Yamada^Tarou==やまだ^たろう"},
		{map[string]any{"Ideographic": "山田^太郎"}, "=山田^太郎"},
		{map[string]any{}, ""},
	} {
		dataset := Dataset{"00100010": {VR: "PN", Value: []any{test.value}}}
		if got := ElementString(dataset, "00100010"); got != test.want {
			t.Errorf("ElementString(%v) = %q, want %q", test.value, got, test.want)
		}
	}
}
