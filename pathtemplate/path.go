package pathtemplate

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

func forbidden(r rune) bool {
	return !unicode.IsGraphic(r) || strings.ContainsRune(`<>:"/\|?*`, r)
}

func deviceName(name string) bool {
	base := name
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	base = strings.ToUpper(strings.TrimRight(base, " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT") {
		suffix := base[3:]
		return len(suffix) == 1 && suffix[0] >= '0' && suffix[0] <= '9' || suffix == "¹" || suffix == "²" || suffix == "³"
	}
	return false
}

// SanitizeAttribute returns one portable component, never a path. Unsafe
// characters become underscores, terminal dots/spaces are removed, empty/dot
// components become "_", and device names are prefixed with "_". It is lossy
// and does not guarantee uniqueness. Default attribute/component budgets apply.
func SanitizeAttribute(value string) (string, error) {
	return sanitizeAttribute(value, 4096, 255)
}

func sanitizeAttribute(value string, maxInput, maxComponent int) (string, error) {
	if len(value) > maxInput {
		return "", ErrLimitExceeded
	}
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("%w: invalid UTF-8", ErrValue)
	}
	var b strings.Builder
	for _, r := range value {
		if forbidden(r) {
			r = '_'
		}
		b.WriteRune(r)
	}
	result := strings.TrimRightFunc(b.String(), func(r rune) bool { return r == '.' || unicode.IsSpace(r) })
	if result == "" || result == "." || result == ".." {
		result = "_"
	}
	if deviceName(result) {
		result = "_" + result
	}
	if len(result) > maxComponent {
		return "", ErrLimitExceeded
	}
	return result, nil
}

// ValidateRelativePath validates a slash-separated relative path under the
// default output/component budgets. It rejects traversal, empty components,
// absolute/drive/UNC/device paths and Windows-incompatible names. It never
// cleans, joins, resolves or accesses a filesystem path.
func ValidateRelativePath(value string) error {
	return validatePath(value, 4096, 255, 64)
}

func validatePath(value string, maxOutput, maxComponent, maxComponents int) error {
	if len(value) > maxOutput || strings.Count(value, "/") >= maxComponents {
		return ErrLimitExceeded
	}
	if value == "" || !utf8.ValidString(value) {
		return ErrUnsafePath
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." || deviceName(component) {
			return ErrUnsafePath
		}
		if len(component) > maxComponent {
			return ErrLimitExceeded
		}
		last, _ := utf8.DecodeLastRuneInString(component)
		if last == '.' || unicode.IsSpace(last) {
			return ErrUnsafePath
		}
		for _, r := range component {
			if forbidden(r) {
				return ErrUnsafePath
			}
		}
	}
	return nil
}
