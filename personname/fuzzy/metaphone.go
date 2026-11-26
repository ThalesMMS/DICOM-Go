package fuzzy

// Metaphone is the original Metaphone of Lawrence Philips (1990), tuned for
// English names. Codes use the consonant symbols 0BFHJKLMNPRSTWXY, where 0 is
// "th" and X is "sh"/"ch"; a vowel appears only as the first symbol. Duplicate
// adjacent letters other than C are dropped first. The code length is not
// bounded.
type Metaphone struct{}

func (Metaphone) Name() string { return NameMetaphone }

func (Metaphone) Encode(word string) string {
	letters := dedupeLettersExceptC(foldLetters(word))
	if len(letters) == 0 {
		return ""
	}
	switch {
	case len(letters) > 1 && (hasPrefix(letters, "AE") || hasPrefix(letters, "GN") || hasPrefix(letters, "KN") || hasPrefix(letters, "PN") || hasPrefix(letters, "WR")):
		letters = letters[1:]
	case letters[0] == 'X':
		letters[0] = 'S'
	case hasPrefix(letters, "WH"):
		letters = append([]byte{'W'}, letters[2:]...)
	}
	code := make([]byte, 0, len(letters)+2)
	last := len(letters) - 1
	for i, letter := range letters {
		previous, next, afterNext := letterAt(letters, i-1), letterAt(letters, i+1), letterAt(letters, i+2)
		switch letter {
		case 'A', 'E', 'I', 'O', 'U':
			if i == 0 {
				code = append(code, letter)
			}
		case 'B':
			if !(previous == 'M' && i == last) {
				code = append(code, 'B')
			}
		case 'C':
			switch {
			case next == 'I' && afterNext == 'A':
				code = append(code, 'X')
			case next == 'H' && previous == 'S':
				code = append(code, 'K')
			case next == 'H':
				code = append(code, 'X')
			case oneOf(next, "IEY") && previous == 'S':
			case oneOf(next, "IEY"):
				code = append(code, 'S')
			default:
				code = append(code, 'K')
			}
		case 'D':
			if next == 'G' && oneOf(afterNext, "EIY") {
				code = append(code, 'J')
			} else {
				code = append(code, 'T')
			}
		case 'G':
			switch {
			case next == 'H' && i+2 <= last && !isMetaphoneVowel(afterNext):
			case next == 'N' && (i+1 == last || (i+3 == last && afterNext == 'E' && letterAt(letters, i+3) == 'D')):
			case previous == 'D' && oneOf(next, "EIY"):
			case oneOf(next, "EIY"):
				code = append(code, 'J')
			default:
				code = append(code, 'K')
			}
		case 'H':
			switch {
			case oneOf(previous, "CGPST"):
			case isMetaphoneVowel(previous) && !isMetaphoneVowel(next):
			default:
				code = append(code, 'H')
			}
		case 'K':
			if previous != 'C' {
				code = append(code, 'K')
			}
		case 'P':
			if next == 'H' {
				code = append(code, 'F')
			} else {
				code = append(code, 'P')
			}
		case 'Q':
			code = append(code, 'K')
		case 'S':
			if next == 'H' || (next == 'I' && oneOf(afterNext, "OA")) {
				code = append(code, 'X')
			} else {
				code = append(code, 'S')
			}
		case 'T':
			switch {
			case next == 'I' && oneOf(afterNext, "OA"):
				code = append(code, 'X')
			case next == 'H':
				code = append(code, '0')
			case next == 'C' && afterNext == 'H':
			default:
				code = append(code, 'T')
			}
		case 'V':
			code = append(code, 'F')
		case 'W', 'Y':
			if isMetaphoneVowel(next) {
				code = append(code, letter)
			}
		case 'X':
			code = append(code, 'K', 'S')
		case 'Z':
			code = append(code, 'S')
		default: // F, J, L, M, N, R
			code = append(code, letter)
		}
	}
	return string(code)
}

func dedupeLettersExceptC(letters []byte) []byte {
	out := letters[:0]
	for i, letter := range letters {
		if i > 0 && letter == letters[i-1] && letter != 'C' {
			continue
		}
		out = append(out, letter)
	}
	return out
}

func hasPrefix(letters []byte, prefix string) bool {
	return len(letters) >= len(prefix) && string(letters[:len(prefix)]) == prefix
}

func isMetaphoneVowel(letter byte) bool {
	return oneOf(letter, "AEIOU")
}
