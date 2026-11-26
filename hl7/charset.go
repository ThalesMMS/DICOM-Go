package hl7

import (
	"fmt"
	"strings"
	"unicode/utf8"

	textencoding "golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

// CharacterSets lists the MSH-18 values (HL7 table 0211) the package
// decodes and encodes. "" is ASCII. UTF-16 and UTF-32 are not supported:
// MLLP framing bytes cannot be told apart in them.
var CharacterSets = []string{
	"ASCII", "8859/1", "8859/2", "8859/3", "8859/4", "8859/5", "8859/6",
	"8859/7", "8859/8", "8859/9", "8859/15", "UNICODE UTF-8",
	"ISO IR87", "GB 18030-2000", "KS X 1001", "BIG-5",
}

func encodingFor(charset string) (textencoding.Encoding, bool, error) {
	switch strings.ToUpper(strings.TrimSpace(charset)) {
	case "", "ASCII":
		return nil, false, nil
	case "UNICODE UTF-8", "UTF-8":
		return nil, true, nil
	case "8859/1":
		return charmap.ISO8859_1, false, nil
	case "8859/2":
		return charmap.ISO8859_2, false, nil
	case "8859/3":
		return charmap.ISO8859_3, false, nil
	case "8859/4":
		return charmap.ISO8859_4, false, nil
	case "8859/5":
		return charmap.ISO8859_5, false, nil
	case "8859/6":
		return charmap.ISO8859_6, false, nil
	case "8859/7":
		return charmap.ISO8859_7, false, nil
	case "8859/8":
		return charmap.ISO8859_8, false, nil
	case "8859/9":
		return charmap.ISO8859_9, false, nil
	case "8859/15":
		return charmap.ISO8859_15, false, nil
	case "ISO IR87":
		return japanese.ISO2022JP, false, nil
	case "GB 18030-2000":
		return simplifiedchinese.GB18030, false, nil
	case "KS X 1001":
		return korean.EUCKR, false, nil
	case "BIG-5":
		return traditionalchinese.Big5, false, nil
	default:
		return nil, false, fmt.Errorf("%w: unsupported MSH-18 value", ErrCharset)
	}
}

// decodeCharacterSet converts a message to UTF-8. ASCII refuses bytes above
// 7FH; UTF-8 must be valid.
func decodeCharacterSet(charset string, data []byte) (string, error) {
	encoding, utf8Set, err := encodingFor(charset)
	if err != nil {
		return "", err
	}
	if utf8Set {
		if !utf8.Valid(data) {
			return "", fmt.Errorf("%w: invalid UTF-8", ErrCharset)
		}
		return string(data), nil
	}
	if encoding == nil {
		for i, c := range data {
			if c > 0x7F {
				return "", fmt.Errorf("%w: byte %d is not ASCII", ErrCharset, i)
			}
		}
		return string(data), nil
	}
	decoded, err := encoding.NewDecoder().Bytes(data)
	if err != nil {
		return "", fmt.Errorf("%w: invalid bytes for the declared set", ErrCharset)
	}
	return string(decoded), nil
}

// encodeCharacterSet converts UTF-8 text to the message's character set.
func encodeCharacterSet(charset, text string) ([]byte, error) {
	encoding, utf8Set, err := encodingFor(charset)
	if err != nil {
		return nil, err
	}
	if utf8Set {
		return []byte(text), nil
	}
	if encoding == nil {
		for i := 0; i < len(text); i++ {
			if text[i] > 0x7F {
				return nil, fmt.Errorf("%w: a character is not ASCII; declare MSH-18", ErrCharset)
			}
		}
		return []byte(text), nil
	}
	encoded, err := encoding.NewEncoder().Bytes([]byte(text))
	if err != nil {
		return nil, fmt.Errorf("%w: a character cannot be represented in the declared set", ErrCharset)
	}
	return encoded, nil
}
