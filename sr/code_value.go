package sr

import (
	"strings"

	"github.com/ThalesMMS/dicom-go/codes"
	"unicode/utf8"
)

// maxShortCodeValue is the length limit of Code Value (0008,0100), an SH.
const maxShortCodeValue = 16

// NewCode returns a coded concept whose code value is placed in the
// attribute the Code Sequence Macro selects (PS3.3 Table 8.8-1): URN Code
// Value (0008,0120) for a URN or URL, Long Code Value (0008,0119) for a value
// longer than 16 characters, and Code Value (0008,0100) otherwise. The long
// and URN forms are carried by the returned CodeContext, which
// ContentItemCodeContexts passes to the template writer.
func NewCode(value, designator, meaning string) (CodedEntry, CodeContext) {
	code := CodedEntry{CodingSchemeDesignator: designator, CodeMeaning: meaning}
	var context CodeContext
	switch {
	case isURNOrURL(value):
		context.URNCodeValue = value
	case utf8.RuneCountInString(value) > maxShortCodeValue:
		context.LongCodeValue = value
	default:
		code.CodeValue = value
	}
	return code, context
}

// ValidCode reports whether code and context form a Code Sequence item the
// Code Sequence Macro (PS3.3 Table 8.8-1) permits: exactly one code value, in
// Code Value (at most 16 characters, not a URN or URL), Long Code Value (more
// than 16 characters, not a URN or URL) or URN Code Value; a single value
// (no backslash); a Coding Scheme Designator, which a URN code may omit only
// together with the Coding Scheme Version; and a Code Meaning.
func ValidCode(code CodedEntry, context CodeContext) bool {
	key := codeKeyForEntry(code, context)
	if !validCodeContextShape(context) || !validCodeKey(key) || code.CodeMeaning == "" {
		return false
	}
	for _, value := range []string{key.CodeValue, key.LongCodeValue} {
		if value != "" && isURNOrURL(value) {
			return false
		}
	}
	for _, value := range []string{key.CodeValue, key.LongCodeValue, key.CodingSchemeDesignator, code.CodeMeaning} {
		if strings.Contains(value, `\`) {
			return false
		}
	}
	return true
}

// isURNOrURL reports a URN ("urn:" scheme, RFC 8141) or a URL with an
// authority ("scheme://", RFC 3986).
func isURNOrURL(value string) bool {
	if len(value) > 4 && strings.EqualFold(value[:4], "urn:") {
		return true
	}
	scheme, rest, found := strings.Cut(value, "://")
	if !found || scheme == "" || rest == "" {
		return false
	}
	for i := 0; i < len(scheme); i++ {
		c := scheme[i]
		letter := 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
		if !letter && (i == 0 || !('0' <= c && c <= '9' || c == '+' || c == '-' || c == '.')) {
			return false
		}
	}
	return true
}

// FromCode is the coded entry of a PS3.16 code, such as dcm.Finding or
// sct.FindingSite from the codes packages. Codes whose value is longer than
// 16 characters or a URN need NewCode for their Code Context.
func FromCode(c codes.Code) CodedEntry {
	return CodedEntry{CodeValue: c.Value, CodingSchemeDesignator: c.Scheme, CodeMeaning: c.Meaning}
}

// PS3.16 Context Groups whose Code Meanings the measurement concepts take.
const (
	cidLinearMeasurement = 7470
	cidAreaMeasurement   = 7471
)

// fromContextGroup is the coded entry of c with the Code Meaning Context
// Group id gives it, or c's own when the group does not list it.
func fromContextGroup(id int, c codes.Code) CodedEntry {
	if group, ok := codes.ContextGroupByID(id); ok {
		if term, ok := group.Member(c); ok {
			return FromCode(term.Code)
		}
	}
	return FromCode(c)
}
