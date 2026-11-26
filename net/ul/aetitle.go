package ul

import (
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/net/telemetry"
)

// AE title handling follows PS3.5 Table 6.2-1 (AE: at most 16 characters of
// the Default Character Repertoire without control characters or backslash;
// leading and trailing spaces are not significant; a value made only of
// spaces shall not be used) and PS3.8 9.3.2 (the A-ASSOCIATE-RQ fields are 16
// bytes padded with spaces).
//
// Some implementations pad the 16-byte PDU field with NUL bytes instead of
// spaces. NormalizeAETitle treats a run of trailing NUL and space bytes as
// padding, so such a peer compares equal to one that pads with spaces. A NUL
// followed by anything other than padding is not padding: it stays in the
// value and makes it invalid, so "SCP\x00EVIL" never collapses to "SCP".

// NormalizeAETitle returns the significant part of an AE title: trailing NUL
// and space padding and leading spaces are removed. Other characters are kept
// as received, including invalid ones; use ValidateAETitle to check them and
// telemetry.EscapePeerString before showing a peer-supplied title.
func NormalizeAETitle(title string) string {
	return strings.TrimLeft(strings.TrimRight(title, " \x00"), " ")
}

// ValidateAETitle reports whether title, once normalized, is a usable AE
// title: 1 to 16 characters from the Default Character Repertoire (0x20 to
// 0x7E) without backslash. Control characters, including an embedded NUL,
// and bytes outside ASCII are rejected. The error wraps ErrInvalidAEtitle and
// quotes the value with control characters escaped.
func ValidateAETitle(title string) error {
	normalized := NormalizeAETitle(title)
	if normalized == "" {
		return fmt.Errorf("%w: empty or all-space value", ErrInvalidAEtitle)
	}
	if len(normalized) > MaxAETitleLength {
		return fmt.Errorf("%w: length %d exceeds %d", ErrInvalidAEtitle, len(normalized), MaxAETitleLength)
	}
	for i := 0; i < len(normalized); i++ {
		c := normalized[i]
		if c < 0x20 || c > 0x7e || c == '\\' {
			return fmt.Errorf("%w: \"%s\" contains byte 0x%02X outside the AE repertoire", ErrInvalidAEtitle, telemetry.EscapePeerString(normalized), c)
		}
	}
	return nil
}

// EqualAETitles reports whether a and b name the same application entity:
// their normalized values are equal. The comparison is case-sensitive, as
// PS3.5 does not fold AE title case; an application that treats titles
// case-insensitively folds case itself.
func EqualAETitles(a, b string) bool {
	return NormalizeAETitle(a) == NormalizeAETitle(b)
}
