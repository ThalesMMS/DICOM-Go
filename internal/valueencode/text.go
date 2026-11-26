package valueencode

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
)

var (
	dsValuePattern = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)
	isValuePattern = regexp.MustCompile(`^[+-]?[0-9]+$`)
)

// ValidateString checks an authored core.StringValue against its VR: the VR
// must be textual, IS/DS components must satisfy PS3.5 6.2 syntax, length
// and (for IS) the signed 32-bit range, and UI components must be UIDs per
// PS3.5 9.1 (digits and dots, no empty or leading-zero component, at most 64
// bytes). Empty components are empty values.
// Values read from a stream are RawValue and are not checked here, so legacy
// objects keep their bytes.
func ValidateString(vr core.VR, value core.StringValue) error {
	if !vr.IsStringLike() {
		return fmt.Errorf("%w: text value is incompatible with VR %s", core.ErrValueNotRepresentable, vr)
	}
	if !vr.UsesTextValueDelimiter() && len(value) > 1 {
		// LT, ST, UT and UR hold one value in which a backslash is literal;
		// several components would be joined into a different single value.
		return fmt.Errorf("%w: VR %s holds one value, got %d", core.ErrValueNotRepresentable, vr, len(value))
	}
	if vr == core.VRUI {
		return validateUIDs(value)
	}
	if vr != core.VRIS && vr != core.VRDS {
		return nil
	}
	// Components are joined with the backslash delimiter on output, so a
	// component that already contains backslashes carries several values.
	index := 0
	for _, component := range value {
		for _, number := range strings.Split(component, "\\") {
			if strings.Trim(number, " ") != "" {
				if err := ValidateNumberString(vr, number); err != nil {
					return fmt.Errorf("%w: %s value %d: %v", core.ErrValueNotRepresentable, vr, index, err)
				}
			}
			index++
		}
	}
	return nil
}

// validateUIDs checks every UI component. Trailing NUL or space padding is
// tolerated because the writer pads UI with NUL; diagnostics give the index
// and length only.
func validateUIDs(value core.StringValue) error {
	index := 0
	for _, component := range value {
		for _, uid := range strings.Split(component, "\\") {
			uid = strings.TrimRight(uid, "\x00 ")
			if uid != "" && !core.IsValidUID(uid) {
				return fmt.Errorf("%w: UI value %d (%d bytes) is not a valid UID", core.ErrValueNotRepresentable, index, len(uid))
			}
			index++
		}
	}
	return nil
}

// ValidateNumberString validates one IS or DS component, including its
// leading/trailing space padding.
func ValidateNumberString(vr core.VR, value string) error {
	maxLength := 16
	pattern := dsValuePattern
	if vr == core.VRIS {
		maxLength = 12
		pattern = isValuePattern
	}
	// Diagnostics report sizes and rules, never the value itself.
	if len(value) > maxLength {
		return fmt.Errorf("%s value is %d bytes, want at most %d", vr, len(value), maxLength)
	}
	trimmed := strings.Trim(value, " ")
	if !pattern.MatchString(trimmed) {
		return fmt.Errorf("%d-byte value does not match %s syntax", len(value), vr)
	}
	if vr == core.VRIS {
		if _, err := strconv.ParseInt(trimmed, 10, 32); err != nil {
			return fmt.Errorf("IS value is outside the signed 32-bit range")
		}
	}
	return nil
}
