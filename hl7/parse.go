package hl7

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrSyntax reports a message that is not well-formed ER7.
	ErrSyntax = errors.New("hl7: malformed message")
	// ErrLimit reports a message beyond a ParseOptions limit.
	ErrLimit = errors.New("hl7: message exceeds a parse limit")
	// ErrCharset reports an unsupported MSH-18 or bytes invalid in it.
	ErrCharset = errors.New("hl7: character set")
)

// ParseError locates a parse failure without quoting the message.
type ParseError struct {
	// Segment is the 1-based segment position, zero before the first.
	Segment int
	// Name is the segment ID when known; segment IDs are not patient data.
	Name string
	// Field is the 1-based field number, zero when not field-specific.
	Field int
	Err   error
}

func (e *ParseError) Error() string {
	location := ""
	switch {
	case e.Segment > 0 && e.Field > 0:
		location = fmt.Sprintf(" at segment %d (%s) field %d", e.Segment, e.Name, e.Field)
	case e.Segment > 0:
		location = fmt.Sprintf(" at segment %d (%s)", e.Segment, e.Name)
	}
	return fmt.Sprintf("%v%s", e.Err, location)
}

func (e *ParseError) Unwrap() error { return e.Err }

// ParseOptions bound a parse. Zero values take the defaults.
type ParseOptions struct {
	// MaxBytes bounds the encoded message; default 4 MiB.
	MaxBytes int
	// MaxSegments bounds the segments; default 10000.
	MaxSegments int
	// MaxFields bounds the fields of one segment; default 1000.
	MaxFields int
	// MaxRepetitions bounds the repetitions of one field; default 1000.
	MaxRepetitions int
	// DefaultCharacterSet applies when MSH-18 is empty: "" is ASCII, which
	// refuses bytes above 7FH; "UNICODE UTF-8" accepts UTF-8 senders that
	// leave MSH-18 empty.
	DefaultCharacterSet string
}

const (
	defaultMaxBytes       = 4 << 20
	defaultMaxSegments    = 10000
	defaultMaxFields      = 1000
	defaultMaxRepetitions = 1000
)

func (o ParseOptions) withDefaults() ParseOptions {
	if o.MaxBytes <= 0 {
		o.MaxBytes = defaultMaxBytes
	}
	if o.MaxSegments <= 0 {
		o.MaxSegments = defaultMaxSegments
	}
	if o.MaxFields <= 0 {
		o.MaxFields = defaultMaxFields
	}
	if o.MaxRepetitions <= 0 {
		o.MaxRepetitions = defaultMaxRepetitions
	}
	return o
}

// Parse decodes one ER7 message with the default options.
func Parse(data []byte) (*Message, error) {
	return ParseWithOptions(data, ParseOptions{})
}

// ParseWithOptions decodes one ER7 message. Segments end with a carriage
// return; a line feed or CR LF is accepted too. The MSH-18 character set is
// read from the raw bytes first and the whole message decoded to UTF-8
// before it is split, so multi-byte sets whose bytes collide with the
// delimiters are safe.
func ParseWithOptions(data []byte, options ParseOptions) (*Message, error) {
	options = options.withDefaults()
	if len(data) > options.MaxBytes {
		return nil, &ParseError{Err: fmt.Errorf("%w: %d bytes, limit %d", ErrLimit, len(data), options.MaxBytes)}
	}
	delimiters, err := readDelimiters(data)
	if err != nil {
		return nil, err
	}
	charset, err := rawCharacterSet(data, delimiters)
	if err != nil {
		return nil, err
	}
	declared := charset
	if charset == "" {
		charset = options.DefaultCharacterSet
	}
	text, err := decodeCharacterSet(charset, data)
	if err != nil {
		return nil, &ParseError{Segment: 1, Name: "MSH", Field: 18, Err: err}
	}
	text = strings.ReplaceAll(text, "\r\n", "\r")
	text = strings.ReplaceAll(text, "\n", "\r")
	lines := strings.Split(text, "\r")
	message := &Message{Delimiters: delimiters}
	for _, line := range lines {
		if line == "" {
			continue
		}
		if len(message.Segments) >= options.MaxSegments {
			return nil, &ParseError{Segment: len(message.Segments) + 1, Err: fmt.Errorf("%w: more than %d segments", ErrLimit, options.MaxSegments)}
		}
		segment, err := parseSegment(line, delimiters, charset, len(message.Segments) == 0, options)
		if err != nil {
			err.Segment = len(message.Segments) + 1
			return nil, err
		}
		message.Segments = append(message.Segments, segment)
	}
	if len(message.Segments) == 0 {
		return nil, &ParseError{Err: fmt.Errorf("%w: no segments", ErrSyntax)}
	}
	// A multi-byte character before MSH-18 whose second byte is a
	// delimiter would move MSH-18 once decoded: refuse the ambiguity.
	if decoded := strings.TrimSpace(message.Header().CharacterSet); decoded != declared {
		return nil, &ParseError{Segment: 1, Name: "MSH", Field: 18, Err: fmt.Errorf("%w: MSH-18 reads differently once decoded", ErrCharset)}
	}
	return message, nil
}

func readDelimiters(data []byte) (Delimiters, error) {
	return readHeaderDelimiters(data, "MSH")
}

func readHeaderDelimiters(data []byte, header string) (Delimiters, error) {
	if len(data) < 6 || !bytes.HasPrefix(data, []byte(header)) {
		return Delimiters{}, &ParseError{Err: fmt.Errorf("%w: the message does not start with %s", ErrSyntax, header)}
	}
	d := Delimiters{Field: data[3]}
	// MSH-2 ends at the next field separator, or with the segment.
	end := bytes.IndexAny(data[4:], string([]byte{d.Field, '\r', '\n'}))
	if end < 0 {
		end = len(data) - 4
	}
	encoding := data[4 : 4+end]
	if len(encoding) < 2 || len(encoding) > 5 {
		return Delimiters{}, &ParseError{Segment: 1, Name: header, Field: 2, Err: fmt.Errorf("%w: %d encoding characters", ErrSyntax, len(encoding))}
	}
	d.Component, d.Repetition = encoding[0], encoding[1]
	if len(encoding) > 2 {
		d.Escape = encoding[2]
	}
	if len(encoding) > 3 {
		d.Subcomponent = encoding[3]
	}
	if len(encoding) > 4 {
		d.Truncation = encoding[4]
	}
	seen := map[byte]bool{}
	for _, c := range append([]byte{d.Field}, encoding...) {
		if seen[c] || c < 0x21 || c > 0x7E || isAlphanumeric(c) {
			return Delimiters{}, &ParseError{Segment: 1, Name: header, Field: 2, Err: fmt.Errorf("%w: invalid or repeated delimiter", ErrSyntax)}
		}
		seen[c] = true
	}
	return d, nil
}

func isAlphanumeric(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// rawCharacterSet reads MSH-18's first value from the undecoded MSH line.
func rawCharacterSet(data []byte, d Delimiters) (string, error) {
	line := data
	if end := bytes.IndexAny(line, "\r\n"); end >= 0 {
		line = line[:end]
	}
	const charsetField = 18
	raw := string(line)
	declared, err := characterSetField(splitSegment(raw, d, true, charsetField), d)
	if strings.IndexFunc(raw, func(r rune) bool { return r >= 0x80 || r == '\x1b' }) >= 0 {
		// In a legacy multibyte header a trail byte can look like the
		// escape delimiter. Recover the plain-split candidate only if
		// decoding it places the same declaration back in MSH-18.
		plain, plainErr := characterSetField(strings.SplitN(raw, string(d.Field), charsetField+1), d)
		if plainErr == nil && plain != "" && plain != declared {
			decoded, decodeErr := decodeCharacterSet(plain, line)
			if decodeErr == nil {
				actual, actualErr := characterSetField(splitSegment(decoded, d, true, charsetField), d)
				if actualErr == nil && actual == plain {
					return plain, nil
				}
			}
		}
	}
	return declared, err
}

func characterSetField(fields []string, d Delimiters) (string, error) {
	// fields[0] is "MSH", fields[1] MSH-2, so MSH-n is fields[n-1].
	if len(fields) < 18 {
		return "", nil
	}
	value := fields[17]
	if i := strings.IndexByte(value, d.Repetition); i >= 0 {
		value = value[:i]
	}
	if i := strings.IndexByte(value, d.Component); i >= 0 {
		value = value[:i]
	}
	// MSH-18 is a code of table 0211: printable ASCII without escapes.
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c > 0x7E || c < 0x20 || (d.Escape != 0 && c == d.Escape) || (d.Subcomponent != 0 && c == d.Subcomponent) {
			return "", &ParseError{Segment: 1, Name: "MSH", Field: 18, Err: fmt.Errorf("%w: MSH-18 is not a printable code", ErrCharset)}
		}
	}
	return strings.TrimSpace(value), nil
}

func validSegmentName(name string) bool {
	if len(name) != 3 || name[0] < 'A' || name[0] > 'Z' {
		return false
	}
	for i := 1; i < 3; i++ {
		c := name[i]
		if !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func parseSegment(line string, d Delimiters, charset string, first bool, options ParseOptions) (Segment, *ParseError) {
	parts := splitSegment(line, d, first, options.MaxFields)
	name := parts[0]
	if !validSegmentName(name) {
		return Segment{}, &ParseError{Err: fmt.Errorf("%w: invalid segment ID", ErrSyntax)}
	}
	if first != (name == "MSH") {
		if first {
			return Segment{}, &ParseError{Name: name, Err: fmt.Errorf("%w: the first segment is not MSH", ErrSyntax)}
		}
		return Segment{}, &ParseError{Name: name, Err: fmt.Errorf("%w: MSH after the first segment", ErrSyntax)}
	}
	return parseSegmentFields(parts, d, charset, first, options)
}

func splitSegment(line string, d Delimiters, header bool, maxFields int) []string {
	// One more field than the limit lets the caller report ErrLimit.
	n := min(maxFields, len(line)) + 1
	if !header {
		// Include the segment name, which is not counted as a field.
		return splitOutsideEscapes(line, d.Field, d.Escape, n+1)
	}
	// MSH-2, FHS-2 and BHS-2 contain the literal escape character.
	parts := strings.SplitN(line, string(d.Field), 3)
	if len(parts) == 3 {
		return append(parts[:2], splitOutsideEscapes(parts[2], d.Field, d.Escape, n-2)...)
	}
	return parts
}

// splitOutsideEscapes splits ER7 structure while leaving paired escape
// sequences for unescape to interpret at the leaves. An unmatched escape
// character stays literal and does not suppress following separators.
func splitOutsideEscapes(text string, separator, escape byte, n int) []string {
	if n == 0 || n == 1 || escape == 0 || strings.IndexByte(text, escape) < 0 {
		return strings.SplitN(text, string(separator), n)
	}
	var parts []string
	start := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case escape:
			if end := strings.IndexByte(text[i+1:], escape); end >= 0 {
				i += end + 1
			}
		case separator:
			parts = append(parts, text[start:i])
			start = i + 1
			if len(parts)+1 == n {
				return append(parts, text[start:])
			}
		}
	}
	return append(parts, text[start:])
}

func parseSegmentFields(parts []string, d Delimiters, charset string, header bool, options ParseOptions) (Segment, *ParseError) {
	name := parts[0]
	segment := Segment{Name: name}
	values := parts[1:]
	if header {
		// MSH, FHS and BHS begin with a field separator and literal
		// encoding characters.
		if len(parts) < 2 {
			return Segment{}, &ParseError{Name: name, Field: 2, Err: ErrSyntax}
		}
		segment.Fields = append(segment.Fields, Text(string(d.Field)), Text(parts[1]))
		values = parts[2:]
	}
	if len(segment.Fields)+len(values) > options.MaxFields {
		return Segment{}, &ParseError{Name: name, Err: fmt.Errorf("%w: more than %d fields", ErrLimit, options.MaxFields)}
	}
	for _, raw := range values {
		field, err := parseField(raw, d, charset, options)
		if err != nil {
			return Segment{}, &ParseError{Name: name, Field: len(segment.Fields) + 1, Err: err}
		}
		segment.Fields = append(segment.Fields, field)
	}
	return segment, nil
}

func parseField(raw string, d Delimiters, charset string, options ParseOptions) (Field, error) {
	if raw == "" {
		return nil, nil
	}
	repetitions := splitOutsideEscapes(raw, d.Repetition, d.Escape, min(options.MaxRepetitions, len(raw))+1)
	if len(repetitions) > options.MaxRepetitions {
		return nil, fmt.Errorf("%w: more than %d repetitions", ErrLimit, options.MaxRepetitions)
	}
	field := make(Field, len(repetitions))
	for i, repetition := range repetitions {
		components := splitOutsideEscapes(repetition, d.Component, d.Escape, -1)
		field[i] = make(Repetition, len(components))
		for j, component := range components {
			var subs []string
			if d.Subcomponent != 0 {
				subs = splitOutsideEscapes(component, d.Subcomponent, d.Escape, -1)
			} else {
				subs = []string{component}
			}
			for k := range subs {
				subs[k] = unescape(subs[k], d, charset)
			}
			field[i][j] = subs
		}
	}
	return field, nil
}

// unescape resolves the escape sequences of HL7 v2.5.1 section 2.7: \F\,
// \S\, \T\, \R\ and \E\ become the delimiters, \Xhh..\ the bytes it names
// in the message character set, \.br\ a line break; \H\ and \N\
// (highlighting) are dropped; any other sequence, and an unterminated or
// undecodable one, stays verbatim.
func unescape(value string, d Delimiters, charset string) string {
	if d.Escape == 0 || strings.IndexByte(value, d.Escape) < 0 {
		return value
	}
	var out strings.Builder
	for i := 0; i < len(value); {
		c := value[i]
		if c != d.Escape {
			out.WriteByte(c)
			i++
			continue
		}
		end := strings.IndexByte(value[i+1:], d.Escape)
		if end < 0 {
			out.WriteString(value[i:])
			break
		}
		sequence := value[i+1 : i+1+end]
		raw := value[i : i+2+end]
		i += 2 + end
		switch {
		case sequence == "F":
			out.WriteByte(d.Field)
		case sequence == "S":
			out.WriteByte(d.Component)
		case sequence == "T" && d.Subcomponent != 0:
			out.WriteByte(d.Subcomponent)
		case sequence == "R":
			out.WriteByte(d.Repetition)
		case sequence == "E":
			out.WriteByte(d.Escape)
		case sequence == "P" && d.Truncation != 0:
			out.WriteByte(d.Truncation)
		case sequence == "H" || sequence == "N":
		case sequence == ".br":
			out.WriteByte('\n')
		case len(sequence) > 1 && sequence[0] == 'X' && len(sequence)%2 == 1:
			decoded, ok := decodeHex(sequence[1:], charset)
			if ok {
				out.WriteString(decoded)
			} else {
				out.WriteString(raw)
			}
		default:
			out.WriteString(raw)
		}
	}
	return out.String()
}

func decodeHex(hex, charset string) (string, bool) {
	out := make([]byte, 0, len(hex)/2)
	for i := 0; i+1 < len(hex); i += 2 {
		hi, ok1 := hexValue(hex[i])
		lo, ok2 := hexValue(hex[i+1])
		if !ok1 || !ok2 {
			return "", false
		}
		out = append(out, hi<<4|lo)
	}
	decoded, err := decodeCharacterSet(charset, out)
	if err != nil {
		return "", false
	}
	return decoded, true
}

func hexValue(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}
