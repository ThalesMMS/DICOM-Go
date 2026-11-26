package telemetry

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// EscapePeerString returns s in a form that is safe to write to a log line,
// terminal, or user interface: printable characters are kept, a backslash is
// doubled, and control characters (C0, DEL, C1), invalid UTF-8 bytes, and
// other non-printable runes such as bidirectional overrides are replaced by
// Go-style escapes (\x00, \n, \u202e). Use it for strings a network peer
// chose, such as AE titles and UIDs, before they reach an operator. A string
// that needs no escaping is returned unchanged without allocating.
//
// The result is for display only; compare and look up the unescaped value.
func EscapePeerString(s string) string {
	clean := true
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !peerRuneSafe(r, size) {
			clean = false
			break
		}
		i += size
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	const hex = "0123456789abcdef"
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			b.WriteString(`\x`)
			b.WriteByte(hex[s[i]>>4])
			b.WriteByte(hex[s[i]&0x0f])
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x80 && !peerRuneSafe(r, size):
			b.WriteString(`\x`)
			b.WriteByte(hex[r>>4])
			b.WriteByte(hex[r&0x0f])
		case !peerRuneSafe(r, size) && r <= 0xffff:
			b.WriteString(`\u`)
			for shift := 12; shift >= 0; shift -= 4 {
				b.WriteByte(hex[(r>>uint(shift))&0x0f])
			}
		case !peerRuneSafe(r, size):
			b.WriteString(`\U`)
			for shift := 28; shift >= 0; shift -= 4 {
				b.WriteByte(hex[(r>>uint(shift))&0x0f])
			}
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

func peerRuneSafe(r rune, size int) bool {
	if r == utf8.RuneError && size == 1 {
		return false
	}
	if r == '\\' {
		return false
	}
	if r >= 0x20 && r < 0x7f {
		return true
	}
	return r >= 0x80 && unicode.IsPrint(r)
}
