package core

import (
	"bytes"
	"strings"
)

// paddingCutset is the trailing padding read from every text VR: UI is
// padded with NUL (PS3.5 9.1) and the other text VRs with spaces, but some
// writers pad AE, AS, CS, DA, DS, DT, IS, LO, LT, PN, SH, ST, TM, UC, UR and
// UT values with NUL too.
const paddingCutset = " \x00"

// NormalizeUID trims trailing DICOM UID padding spaces and NUL bytes. It leaves
// leading and interior bytes unchanged.
func NormalizeUID(uid string) string {
	return strings.TrimRight(uid, paddingCutset)
}

// TrimTextValueBytes removes trailing DICOM padding from a value of the
// element VR: any trailing run of spaces and NUL bytes, so a value padded with
// one or more NULs compares equal to the unpadded text, as in DCMTK. Leading
// and interior bytes are kept. The element's raw bytes are not changed, and
// the validation package reports NUL padding of a VR padded with spaces.
func TrimTextValueBytes(vr VR, data []byte) []byte {
	return bytes.TrimRight(data, paddingCutset)
}

// TrimTextValue removes trailing DICOM padding according to the element VR,
// with the rules of TrimTextValueBytes.
func TrimTextValue(vr VR, s string) string {
	return strings.TrimRight(s, paddingCutset)
}

// CleanTextValue removes embedded NUL bytes, applies VR-specific trailing
// padding trim, and trims surrounding whitespace for display-oriented text.
func CleanTextValue(vr VR, s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	return strings.TrimSpace(TrimTextValue(vr, s))
}

// SplitTextMultiplicity splits an already decoded DICOM textual value by the
// value multiplicity delimiter and trims each component's trailing padding.
func SplitTextMultiplicity(vr VR, s string) []string {
	s = TrimTextValue(vr, s)
	if s == "" {
		return nil
	}
	if !vr.UsesTextValueDelimiter() {
		return []string{s}
	}
	parts := strings.Split(s, "\\")
	for i := range parts {
		parts[i] = TrimTextValue(vr, parts[i])
	}
	return parts
}

// UsesTextValueDelimiter reports whether a backslash separates values for the
// VR. Single-valued text VRs preserve backslashes as text.
func (vr VR) UsesTextValueDelimiter() bool {
	switch vr {
	case VRLT, VRST, VRUT, VRUR:
		return false
	default:
		return true
	}
}
