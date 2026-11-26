// Package fuzzy provides phonetic keys for fuzzy semantic matching of Person
// Name (PN) values, as negotiated for C-FIND (PS3.4 C.5.1.1.1) or requested by
// the QIDO-RS fuzzymatching parameter (PS3.18 8.3.4.1).
//
// A FuzzyStr encodes one word. Keys and Match apply an encoder to every word of
// every component (family, given, middle, prefix, suffix) of every component
// group of a PN value, so word order and component placement do not affect the
// result. The encoders are clean-room implementations of the published
// algorithm descriptions: American Soundex (Knuth, The Art of Computer
// Programming vol. 3; U.S. National Archives coding rules), Metaphone
// (L. Philips, "Hanging on the Metaphone", Computer Language 7(12), 1990) and
// Kölner Phonetik (H. J. Postel, IBM-Nachrichten 19, 1969).
package fuzzy

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// FuzzyStr maps one person-name word to a phonetic key. Words that sound
// alike map to the same key. Encode returns "" for a word with no letters the
// algorithm can encode.
type FuzzyStr interface {
	// Name is a stable identifier of the algorithm and its parameters, suitable
	// for recording which encoder produced persisted keys.
	Name() string
	Encode(word string) string
}

// Algorithm names accepted by ByName.
const (
	NameSoundex         = "soundex"
	NameMetaphone       = "metaphone"
	NameKoelnerPhonetik = "koelner-phonetik"
)

// Algorithms lists the names accepted by ByName.
func Algorithms() []string {
	return []string{NameSoundex, NameMetaphone, NameKoelnerPhonetik}
}

// ByName returns the encoder registered under name: four-character Soundex,
// Metaphone or Kölner Phonetik.
func ByName(name string) (FuzzyStr, bool) {
	switch name {
	case NameSoundex:
		return Soundex{}, true
	case NameMetaphone:
		return Metaphone{}, true
	case NameKoelnerPhonetik:
		return KoelnerPhonetik{}, true
	default:
		return nil, false
	}
}

// Words splits a PN value into its words. Component groups ('='), components
// ('^') and any other non-letter character separate words; apostrophes are
// dropped without splitting, so O'Hara is the single word OHara.
func Words(value string) []string {
	var words []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			words = append(words, b.String())
			b.Reset()
		}
	}
	for _, r := range value {
		switch {
		case r == '\'' || r == '’':
			continue
		case unicode.IsLetter(r) || unicode.Is(unicode.Mn, r):
			b.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return words
}

// Keys returns the distinct phonetic keys of the words of a PN value, in word
// order. Words the encoder cannot encode contribute no key.
func Keys(encoder FuzzyStr, value string) []string {
	if encoder == nil {
		return nil
	}
	words := Words(value)
	keys := make([]string, 0, len(words))
	seen := make(map[string]struct{}, len(words))
	for _, word := range words {
		key := encoder.Encode(word)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys
}

// Match reports whether candidate sounds like query: query must have at least
// one key and every query key must be a key of candidate. Extra candidate
// words, word order and component placement are ignored, so "SMYTH^JON"
// matches "Smith^John^Q" and "John Smith".
func Match(encoder FuzzyStr, query, candidate string) bool {
	queryKeys := Keys(encoder, query)
	if len(queryKeys) == 0 {
		return false
	}
	candidateKeys := Keys(encoder, candidate)
	have := make(map[string]struct{}, len(candidateKeys))
	for _, key := range candidateKeys {
		have[key] = struct{}{}
	}
	for _, key := range queryKeys {
		if _, ok := have[key]; !ok {
			return false
		}
	}
	return true
}

// foldLetters returns the upper-case ASCII letters of word. Diacritics are
// removed (É→E, Ü→U, Ç→C) and the Latin ligatures and letters without a
// decomposition are spelled out (ß→SS, Æ→AE, Ø→O); other characters are
// dropped.
func foldLetters(word string) []byte {
	out := make([]byte, 0, len(word))
	for _, r := range norm.NFD.String(word) {
		switch r {
		case 'ß':
			out = append(out, 'S', 'S')
			continue
		case 'Æ', 'æ':
			out = append(out, 'A', 'E')
			continue
		case 'Œ', 'œ':
			out = append(out, 'O', 'E')
			continue
		case 'Ø', 'ø':
			r = 'O'
		case 'Ł', 'ł':
			r = 'L'
		case 'Đ', 'đ':
			r = 'D'
		case 'Þ', 'þ':
			out = append(out, 'T', 'H')
			continue
		case 'ı':
			r = 'I'
		}
		r = unicode.ToUpper(r)
		if r >= 'A' && r <= 'Z' {
			out = append(out, byte(r))
		}
	}
	return out
}

func letterAt(word []byte, index int) byte {
	if index < 0 || index >= len(word) {
		return 0
	}
	return word[index]
}

func oneOf(letter byte, set string) bool {
	return letter != 0 && strings.IndexByte(set, letter) >= 0
}
