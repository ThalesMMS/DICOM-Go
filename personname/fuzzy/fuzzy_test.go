package fuzzy

import (
	"reflect"
	"testing"
)

// Published American Soundex vectors: D. E. Knuth, The Art of Computer
// Programming vol. 3, 6.4 (Euler ... Lissajous, Robert, Rupert, Rubin,
// Tymczak, Ashcraft, Pfister) and the U.S. National Archives Soundex coding
// guide (Washington, Lee, Gutierrez, Jackson, VanDeusen, Ashcraft, Tymczak,
// Pfister).
func TestSoundexPublishedVectors(t *testing.T) {
	tests := []struct{ word, want string }{
		{"Euler", "E460"}, {"Ellery", "E460"},
		{"Gauss", "G200"}, {"Ghosh", "G200"},
		{"Hilbert", "H416"}, {"Heilbronn", "H416"},
		{"Knuth", "K530"}, {"Kant", "K530"},
		{"Lloyd", "L300"}, {"Ladd", "L300"},
		{"Lukasiewicz", "L222"}, {"Lissajous", "L222"},
		{"Robert", "R163"}, {"Rupert", "R163"}, {"Rubin", "R150"},
		{"Ashcraft", "A261"}, {"Ashcroft", "A261"},
		{"Tymczak", "T522"}, {"Pfister", "P236"}, {"Honeyman", "H555"},
		{"Washington", "W252"}, {"Lee", "L000"}, {"Gutierrez", "G362"},
		{"Jackson", "J250"}, {"VanDeusen", "V532"},
	}
	for _, tc := range tests {
		if got := (Soundex{}).Encode(tc.word); got != tc.want {
			t.Errorf("Soundex(%q) = %q, want %q", tc.word, got, tc.want)
		}
	}
}

func TestSoundexLengthFoldingAndName(t *testing.T) {
	tests := []struct {
		encoder Soundex
		word    string
		want    string
	}{
		{Soundex{Length: 9}, "Lukasiewicz", "L22200000"},
		{Soundex{Length: 6}, "Washington", "W25235"},
		{Soundex{}, "josé", "J200"},
		{Soundex{}, "Müller", "M460"},
		{Soundex{}, "O'Hara", "O600"},
		{Soundex{}, "1234", ""},
		{Soundex{}, "", ""},
	}
	for _, tc := range tests {
		if got := tc.encoder.Encode(tc.word); got != tc.want {
			t.Errorf("%s(%q) = %q, want %q", tc.encoder.Name(), tc.word, got, tc.want)
		}
	}
	if got := (Soundex{}).Name(); got != "soundex" {
		t.Fatalf("Name = %q", got)
	}
	if got := (Soundex{Length: 9}).Name(); got != "soundex-9" {
		t.Fatalf("Name = %q", got)
	}
}

// Published Metaphone examples from the Elixir Text library documentation
// (Thompson, Phillips, Knight, Wright and the knight/night, Wright/Right
// matches and Smith/Schmidt non-match), plus rule examples from the Philips
// rule list (MB, GN, DGE, SCH, TCH, TIO, PH, initial X and WH).
func TestMetaphonePublishedVectors(t *testing.T) {
	tests := []struct{ word, want string }{
		{"Thompson", "0MPSN"},
		{"Phillips", "FLPS"},
		{"Knight", "NT"},
		{"Night", "NT"},
		{"Wright", "RT"},
		{"Right", "RT"},
		{"Smith", "SM0"},
		{"Schmidt", "SKMTT"},
		{"Dumb", "TM"},
		{"Sign", "SN"},
		{"Signed", "SNT"},
		{"Edge", "EJ"},
		{"School", "SKL"},
		{"Witch", "WX"},
		{"Nation", "NXN"},
		{"Xavier", "SFR"},
		{"White", "WT"},
		{"Aeschylus", "ESKLS"},
		{"Ciara", "XR"},
		{"Science", "SNS"},
		{"Jack", "JK"},
		{"Agnes", "AKNS"},
		{"Hugh", "HK"},
	}
	for _, tc := range tests {
		if got := (Metaphone{}).Encode(tc.word); got != tc.want {
			t.Errorf("Metaphone(%q) = %q, want %q", tc.word, got, tc.want)
		}
	}
	if (Metaphone{}).Encode("Smith") == (Metaphone{}).Encode("Schmidt") {
		t.Fatal("Smith and Schmidt must not share a Metaphone key")
	}
}

// Published Kölner Phonetik vectors: Wikipedia (de) "Kölner Phonetik"
// (Wikipedia, Müller-Lüdenscheidt, Heinz, Classen and Heinz Classen coded as
// one word) and H. J. Postel's Breschnew example; Meier/Maier/Mayer/Mayr are
// the article's spellings that must share a code.
func TestKoelnerPhonetikPublishedVectors(t *testing.T) {
	tests := []struct{ word, want string }{
		{"Wikipedia", "3412"},
		{"Müller-Lüdenscheidt", "65752682"},
		{"Breschnew", "17863"},
		{"Heinz", "068"},
		{"Classen", "4586"},
		{"Heinz Classen", "068586"},
		{"Meier", "67"}, {"Maier", "67"}, {"Mayer", "67"}, {"Mayr", "67"},
		{"Müller", "657"}, {"Mueller", "657"},
		{"Schmidt", "862"}, {"Schmitt", "862"},
		{"Xaver", "4837"},
		{"Philipp", "351"},
		{"Strauß", "8278"},
		{"", ""},
		{"H", ""},
	}
	for _, tc := range tests {
		if got := (KoelnerPhonetik{}).Encode(tc.word); got != tc.want {
			t.Errorf("KoelnerPhonetik(%q) = %q, want %q", tc.word, got, tc.want)
		}
	}
}

func TestWordsAndKeysApplyToEveryPersonNameComponent(t *testing.T) {
	words := Words("O'Hara-Smith^José María^^Dr.^Jr.=ヤマダ")
	want := []string{"OHara", "Smith", "José", "María", "Dr", "Jr", "ヤマダ"}
	if !reflect.DeepEqual(words, want) {
		t.Fatalf("Words = %q, want %q", words, want)
	}
	keys := Keys(Soundex{}, "SMITH^JOHN^SMYTHE")
	if !reflect.DeepEqual(keys, []string{"S530", "J500"}) {
		t.Fatalf("Keys = %q", keys)
	}
	if Keys(nil, "SMITH") != nil {
		t.Fatal("Keys with nil encoder returned keys")
	}
}

func TestMatch(t *testing.T) {
	tests := []struct {
		name      string
		encoder   FuzzyStr
		query     string
		candidate string
		want      bool
	}{
		{"soundex spelling", Soundex{}, "SMYTH^JON", "Smith^John^Q", true},
		{"word order", Soundex{}, "John Smith", "SMITH^JOHN", true},
		{"subset of words", Soundex{}, "SMITH", "SMITH^JOHN", true},
		{"missing word", Soundex{}, "SMITH^MARY", "SMITH^JOHN", false},
		{"no encodable query", Soundex{}, "^^", "SMITH", false},
		{"ideographic only", Soundex{}, "ヤマダ", "ヤマダ", false},
		{"metaphone", Metaphone{}, "KNIGHT", "Night^Alan", true},
		{"koelner", KoelnerPhonetik{}, "Meier^Hans", "MAYR^HANS", true},
		{"koelner miss", KoelnerPhonetik{}, "Meier", "Müller", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.encoder, tc.query, tc.candidate); got != tc.want {
				t.Fatalf("Match(%q, %q) = %v, want %v", tc.query, tc.candidate, got, tc.want)
			}
		})
	}
}

func TestByName(t *testing.T) {
	for _, name := range Algorithms() {
		encoder, ok := ByName(name)
		if !ok || encoder.Name() != name {
			t.Fatalf("ByName(%q) = %v, %v", name, encoder, ok)
		}
	}
	if _, ok := ByName("phonem"); ok {
		t.Fatal("ByName accepted an unknown algorithm")
	}
}
