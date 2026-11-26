// Package pathtemplate formats portable relative paths from DICOM attributes.
// It performs no filesystem operations. Literals are validated, attributes are
// sanitized as single components, and the complete result is validated again.
// Templates are immutable and have finite input, expansion and output budgets.
//
// Syntax is {keyword} or {eightHexDigitTag}, with optional zero-based [index].
// Modifiers are sha256 (also hash), md5, and date with an optional signed day
// offset: {StudyDate,date,+1d}. {{ and }} encode literal braces. This grammar
// and digest encoding are intentionally distinct from dcm4che MessageFormat.
package pathtemplate
