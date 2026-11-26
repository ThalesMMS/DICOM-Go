package fuzzy

// KoelnerPhonetik is the Kölner Phonetik (Cologne phonetics), tuned for German
// names. Each letter is coded 0–8 from at most one neighbouring letter, H is
// not coded and other characters are ignored; then repeated adjacent digits
// are collapsed and every 0 except a leading one is removed. Ä, Ö and Ü code
// as vowels and ß as S. The code length is not bounded.
type KoelnerPhonetik struct{}

func (KoelnerPhonetik) Name() string { return NameKoelnerPhonetik }

func (KoelnerPhonetik) Encode(word string) string {
	letters := foldLetters(word)
	if len(letters) == 0 {
		return ""
	}
	raw := make([]byte, 0, len(letters)+2)
	for i, letter := range letters {
		previous, next := letterAt(letters, i-1), letterAt(letters, i+1)
		switch letter {
		case 'A', 'E', 'I', 'J', 'O', 'U', 'Y':
			raw = append(raw, '0')
		case 'H':
		case 'B':
			raw = append(raw, '1')
		case 'P':
			if next == 'H' {
				raw = append(raw, '3')
			} else {
				raw = append(raw, '1')
			}
		case 'D', 'T':
			if oneOf(next, "CSZ") {
				raw = append(raw, '8')
			} else {
				raw = append(raw, '2')
			}
		case 'F', 'V', 'W':
			raw = append(raw, '3')
		case 'G', 'K', 'Q':
			raw = append(raw, '4')
		case 'C':
			switch {
			case i == 0 && oneOf(next, "AHKLOQRUX"):
				raw = append(raw, '4')
			case i == 0:
				raw = append(raw, '8')
			case oneOf(previous, "SZ"):
				raw = append(raw, '8')
			case oneOf(next, "AHKOQUX"):
				raw = append(raw, '4')
			default:
				raw = append(raw, '8')
			}
		case 'X':
			if oneOf(previous, "CKQ") {
				raw = append(raw, '8')
			} else {
				raw = append(raw, '4', '8')
			}
		case 'L':
			raw = append(raw, '5')
		case 'M', 'N':
			raw = append(raw, '6')
		case 'R':
			raw = append(raw, '7')
		case 'S', 'Z':
			raw = append(raw, '8')
		}
	}
	if len(raw) == 0 {
		return ""
	}
	code := make([]byte, 0, len(raw))
	for i, digit := range raw {
		if i > 0 && digit == raw[i-1] {
			continue
		}
		if digit == '0' && len(code) > 0 {
			continue
		}
		code = append(code, digit)
	}
	return string(code)
}
