package fuzzy

import "strconv"

// Soundex is American Soundex: the first letter followed by digits for the
// following consonants (BFPV 1, CGJKQSXZ 2, DT 3, L 4, MN 5, R 6). Adjacent
// letters with the same digit are coded once, including the first letter, and
// so are letters with the same digit separated only by H or W; a vowel (or Y)
// between them codes both. The code is zero-padded or truncated to Length
// characters, four when Length is zero.
type Soundex struct {
	Length int
}

func (s Soundex) length() int {
	if s.Length <= 0 {
		return 4
	}
	return s.Length
}

// Name returns "soundex" for the standard four-character code and
// "soundex-<n>" for other lengths.
func (s Soundex) Name() string {
	if s.length() == 4 {
		return NameSoundex
	}
	return NameSoundex + "-" + strconv.Itoa(s.length())
}

func (s Soundex) Encode(word string) string {
	letters := foldLetters(word)
	if len(letters) == 0 {
		return ""
	}
	length := s.length()
	code := make([]byte, 0, length)
	code = append(code, letters[0])
	last := soundexDigit(letters[0])
	for _, letter := range letters[1:] {
		if len(code) == length {
			break
		}
		switch letter {
		case 'H', 'W':
			continue
		}
		digit := soundexDigit(letter)
		if digit == 0 {
			last = 0
			continue
		}
		if digit != last {
			code = append(code, digit)
		}
		last = digit
	}
	for len(code) < length {
		code = append(code, '0')
	}
	return string(code)
}

func soundexDigit(letter byte) byte {
	switch letter {
	case 'B', 'F', 'P', 'V':
		return '1'
	case 'C', 'G', 'J', 'K', 'Q', 'S', 'X', 'Z':
		return '2'
	case 'D', 'T':
		return '3'
	case 'L':
		return '4'
	case 'M', 'N':
		return '5'
	case 'R':
		return '6'
	default:
		return 0
	}
}
