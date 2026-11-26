package encoding

import (
	"bytes"
	"errors"
	"strings"
)

// ReplacementCharacter stands for each run of bytes that a replacing decode
// could not read.
const ReplacementCharacter = '�'

// DecodeReplacing decodes a multi-valued text value like Decode. Where the
// value has bytes this character set cannot decode, it returns the text it
// can read with one ReplacementCharacter for each undecodable run, and true.
// It never fails: a reader that must not lose a value entirely, such as one
// that presents query results, uses it after strict decoding failed.
func (s SpecificCharacterSet) DecodeReplacing(text []byte) (string, bool) {
	if decoded, err := s.Decode(text); err == nil {
		return decoded, false
	}
	if s.iso2022 != nil {
		return ReadDefaultRepertoireReplacing(text)
	}
	return decodeReplacingWith(s.valueCodec().Decode, text)
}

// DecodeSingleValueReplacing is DecodeReplacing for a VR whose backslash byte
// is text rather than a value-multiplicity delimiter (ST, LT, UT, and UR).
func (s SpecificCharacterSet) DecodeSingleValueReplacing(text []byte) (string, bool) {
	if decoded, err := s.DecodeSingleValue(text); err == nil {
		return decoded, false
	}
	if s.iso2022 != nil {
		return ReadDefaultRepertoireReplacing(text)
	}
	decode := s.valueCodec().Decode
	if codec, ok := s.valueCodec().(interface{ DecodeSingleValue([]byte) (string, error) }); ok {
		decode = codec.DecodeSingleValue
	}
	return decodeReplacingWith(decode, text)
}

// DecodePersonNameReplacing is DecodeReplacing for a PN value: each component
// group is read with its own codec, so a group that decodes stays intact.
func (s SpecificCharacterSet) DecodePersonNameReplacing(text []byte) (string, bool) {
	if decoded, err := s.DecodePersonName(text); err == nil {
		return decoded, false
	}
	if s.iso2022 != nil {
		return ReadDefaultRepertoireReplacing(text)
	}
	groups := bytes.Split(text, []byte{'='})
	decoded := make([]string, len(groups))
	replaced := false
	for i, group := range groups {
		var groupReplaced bool
		decoded[i], groupReplaced = decodeReplacingWith(s.personNameCodec(i).Decode, group)
		replaced = replaced || groupReplaced
	}
	return strings.Join(decoded, "="), replaced
}

// decodeReplacingWith decodes text with a stateless codec, replacing each byte
// the codec reports at a DecodeError offset and resuming after it. A codec
// that reports no offset falls back to the default-repertoire reading.
func decodeReplacingWith(decode func([]byte) (string, error), text []byte) (string, bool) {
	var output strings.Builder
	output.Grow(len(text))
	replaced, lastReplaced := false, false
	for rest := text; len(rest) > 0; {
		decoded, err := decode(rest)
		if err == nil {
			output.WriteString(decoded)
			break
		}
		var decodeErr *DecodeError
		if !errors.As(err, &decodeErr) || decodeErr.Offset < 0 || decodeErr.Offset >= len(rest) {
			return ReadDefaultRepertoireReplacing(text)
		}
		prefix, err := decode(rest[:decodeErr.Offset])
		if err != nil {
			return ReadDefaultRepertoireReplacing(text)
		}
		output.WriteString(prefix)
		if prefix != "" || !lastReplaced {
			output.WriteRune(ReplacementCharacter)
		}
		replaced, lastReplaced = true, true
		rest = rest[decodeErr.Offset+1:]
	}
	return output.String(), replaced
}

// ReadDefaultRepertoireReplacing reads text in the default repertoire
// (ISO 646), which every DICOM character set shares, for a value whose
// declaration is invalid or unsupported or whose Code Extension cannot be
// followed. Printable ASCII, TAB, LF, FF and CR stay as they are. ISO 2022
// escape sequences carry no characters and are dropped. Every other byte, and
// every byte that follows an escape sequence designating a set other than
// ASCII or JIS X 0201 Roman into G0 until an escape sequence designates one of
// them again, is undecodable: each run of them becomes one
// ReplacementCharacter. No character set is guessed.
func ReadDefaultRepertoireReplacing(text []byte) (string, bool) {
	var output strings.Builder
	output.Grow(len(text))
	replaced, inRun, g0Text := false, false, true
	undecodable := func() {
		if !inRun {
			output.WriteRune(ReplacementCharacter)
		}
		replaced, inRun = true, true
	}
	for offset := 0; offset < len(text); {
		value := text[offset]
		if value == 0x1b {
			size := escapeSequenceSize(text[offset:])
			sequence := string(text[offset+1 : offset+size])
			switch {
			case sequence == "(B" || sequence == "(J":
				g0Text = true
			case strings.HasPrefix(sequence, "(") || sequence == "$@" || sequence == "$B" || strings.HasPrefix(sequence, "$("):
				g0Text = false
			}
			offset += size
			continue
		}
		if g0Text && (value >= 0x20 && value <= 0x7e || value == '\t' || value == '\n' || value == '\f' || value == '\r') {
			output.WriteByte(value)
			inRun = false
		} else {
			undecodable()
		}
		offset++
	}
	return output.String(), replaced
}

// escapeSequenceSize returns the length of the ISO 2022 escape sequence at the
// start of text: ESC, intermediate bytes 20-2F and one final byte 30-7E. A
// truncated sequence ends at the end of text.
func escapeSequenceSize(text []byte) int {
	size := 1
	for size < len(text) && text[size] >= 0x20 && text[size] <= 0x2f {
		size++
	}
	if size < len(text) && text[size] >= 0x30 && text[size] <= 0x7e {
		size++
	}
	return size
}
