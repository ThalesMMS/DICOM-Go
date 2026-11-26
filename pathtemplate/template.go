package pathtemplate

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dcmtime"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	ErrSyntax        = errors.New("dicom pathtemplate: invalid template syntax")
	ErrModifier      = errors.New("dicom pathtemplate: unsupported modifier")
	ErrMissing       = errors.New("dicom pathtemplate: attribute/component is absent or empty")
	ErrMultiplicity  = errors.New("dicom pathtemplate: ambiguous value multiplicity")
	ErrValue         = errors.New("dicom pathtemplate: unsupported or invalid attribute value")
	ErrUnsafePath    = errors.New("dicom pathtemplate: unsafe relative path")
	ErrLimitExceeded = errors.New("dicom pathtemplate: resource limit exceeded")
)

// MissingPolicy controls absent attributes, unavailable VM indexes and empty
// selected components. Invalid/deferred values never become missing values.
type MissingPolicy uint8

const (
	MissingError MissingPolicy = iota
	MissingSubstitute
)

// VMPolicy chooses how an unindexed placeholder handles multiple values.
type VMPolicy uint8

const (
	VMError VMPolicy = iota
	VMFirst
)

// Options selects explicit fallback/VM policies and finite byte/count budgets.
// Every zero limit selects its documented default; negative limits are invalid.
type Options struct {
	// Dictionary resolves keywords at compile time. Nil uses std.Dictionary.
	Dictionary dictionary.DataDictionary
	Missing    MissingPolicy
	// Replacement applies only under MissingSubstitute. Empty selects "_".
	// It is sanitized as attribute data, not interpreted as a path/template.
	Replacement       string
	Multiplicity      VMPolicy
	MaxTemplateBytes  int // default 4096
	MaxOutputBytes    int // default 4096
	MaxAttributeBytes int // default 4096, before decoding and after selection
	MaxExpansions     int // default 128, placeholders; no recursive expansion
	MaxComponentBytes int // default 255 UTF-8 bytes
	MaxComponents     int // default 64
}

type part struct {
	literal string
	field   *field
}
type field struct {
	tag      core.Tag
	index    int // -1 is unindexed
	modifier string
	days     int
}

// Template is immutable. Compile resolves keywords and copies relevant options;
// later dictionary or option changes do not change the compiled template.
type Template struct {
	parts       []part
	opts        Options
	replacement string
}

// Compile accepts {selector}, {selector[index]}, {selector,sha256|hash|md5},
// {selector,date}, or {selector,date,+Nd|-Nd}. Selectors are keywords or eight
// hexadecimal tag digits. The date modifier requires complete DA and returns
// YYYYMMDD after a calendar-day shift. Other modifiers/grammars are errors.
// {{ and }} represent literal braces. Only literal '/' creates directories.
// Literal paths are validated without sanitization, then final paths are
// validated again after each attribute has been sanitized independently.
func Compile(pattern string, opts Options) (*Template, error) {
	limits := []*int{&opts.MaxTemplateBytes, &opts.MaxOutputBytes, &opts.MaxAttributeBytes, &opts.MaxExpansions, &opts.MaxComponentBytes, &opts.MaxComponents}
	defaults := []int{4096, 4096, 4096, 128, 255, 64}
	for i, value := range limits {
		if *value < 0 {
			return nil, ErrLimitExceeded
		}
		if *value == 0 {
			*value = defaults[i]
		}
	}
	if opts.Missing > MissingSubstitute || opts.Multiplicity > VMFirst {
		return nil, ErrSyntax
	}
	if len(pattern) > opts.MaxTemplateBytes {
		return nil, ErrLimitExceeded
	}
	if !utf8.ValidString(pattern) {
		return nil, ErrSyntax
	}
	dict := opts.Dictionary
	if dict == nil {
		dict = std.Dictionary
	}
	if opts.Replacement == "" {
		opts.Replacement = "_"
	}
	replacement, err := sanitizeAttribute(opts.Replacement, opts.MaxAttributeBytes, opts.MaxComponentBytes)
	if err != nil {
		return nil, err
	}
	// A dictionary is used only to resolve compile-time selectors, not retained.
	opts.Dictionary, opts.Replacement = nil, ""
	t := &Template{opts: opts, replacement: replacement}
	var literal, skeleton strings.Builder
	flush := func() {
		if literal.Len() != 0 {
			value := literal.String()
			t.parts = append(t.parts, part{literal: value})
			skeleton.WriteString(value)
			literal.Reset()
		}
	}
	expansions := 0
	for i := 0; i < len(pattern); {
		switch pattern[i] {
		case '{':
			if i+1 < len(pattern) && pattern[i+1] == '{' {
				literal.WriteByte('{')
				i += 2
				continue
			}
			flush()
			end := strings.IndexByte(pattern[i+1:], '}')
			if end < 0 {
				return nil, ErrSyntax
			}
			end += i + 1
			expansions++
			if expansions > opts.MaxExpansions {
				return nil, ErrLimitExceeded
			}
			f, err := parseField(pattern[i+1:end], dict)
			if err != nil {
				return nil, err
			}
			t.parts = append(t.parts, part{field: &f})
			skeleton.WriteByte('x')
			i = end + 1
		case '}':
			if i+1 >= len(pattern) || pattern[i+1] != '}' {
				return nil, ErrSyntax
			}
			literal.WriteByte('}')
			i += 2
		default:
			literal.WriteByte(pattern[i])
			i++
		}
	}
	flush()
	if err := validatePath(skeleton.String(), opts.MaxOutputBytes, opts.MaxComponentBytes, opts.MaxComponents); err != nil {
		return nil, err
	}
	return t, nil
}

func parseField(body string, dict dictionary.DataDictionary) (field, error) {
	if strings.ContainsAny(body, "{} /") {
		return field{}, ErrSyntax
	}
	args := strings.SplitN(body, ",", 4)
	if len(args) == 0 || len(args) > 3 {
		return field{}, ErrSyntax
	}
	selector := args[0]
	f := field{index: -1}
	if i := strings.IndexByte(selector, '['); i >= 0 {
		if !strings.HasSuffix(selector, "]") {
			return field{}, ErrSyntax
		}
		index := selector[i+1 : len(selector)-1]
		if index == "" {
			return field{}, ErrSyntax
		}
		for _, r := range index {
			if r < '0' || r > '9' {
				return field{}, ErrSyntax
			}
		}
		n, err := strconv.ParseUint(index, 10, 31)
		if err != nil {
			return field{}, ErrSyntax
		}
		f.index = int(n)
		selector = selector[:i]
	}
	if selector == "" || strings.ContainsAny(selector, "[]") {
		return field{}, ErrSyntax
	}
	if len(selector) == 8 {
		if tag, err := core.ParseTag(selector); err == nil {
			f.tag = tag
		} else if entry, ok := dict.ByKeyword(selector); ok {
			f.tag = entry.Tag
		} else {
			return field{}, ErrSyntax
		}
	} else if entry, ok := dict.ByKeyword(selector); ok {
		f.tag = entry.Tag
	} else {
		return field{}, ErrSyntax
	}
	if len(args) == 1 {
		return f, nil
	}
	f.modifier = args[1]
	switch f.modifier {
	case "sha256", "hash", "md5":
		if len(args) != 2 {
			return field{}, ErrModifier
		}
	case "date":
		if len(args) == 3 {
			value := args[2]
			if len(value) < 3 || (value[0] != '+' && value[0] != '-') || value[len(value)-1] != 'd' {
				return field{}, ErrModifier
			}
			for _, c := range value[1 : len(value)-1] {
				if c < '0' || c > '9' {
					return field{}, ErrModifier
				}
			}
			days, err := strconv.ParseInt(value[:len(value)-1], 10, 32)
			if err != nil || days < -3652425 || days > 3652425 {
				return field{}, ErrModifier
			}
			f.days = int(days)
		}
	default:
		return field{}, ErrModifier
	}
	return f, nil
}

// Format returns a validated relative slash path, or an empty string on error.
// It never reads deferred payloads or accesses the filesystem. It uses Object's
// charset-aware text APIs, which can populate that Object's internal caches.
// Sharing a Template concurrently is safe; concurrent access to the same Object
// must follow Object's synchronization requirements. Values are never modified.
func (t *Template) Format(src *object.Object) (string, error) {
	if t == nil || t.opts.MaxOutputBytes <= 0 {
		return "", ErrSyntax
	}
	var output strings.Builder
	for _, p := range t.parts {
		value := p.literal
		if p.field != nil {
			selected, vr, missing, err := t.attribute(src, *p.field)
			if err != nil {
				return "", fmt.Errorf("%s: %w", p.field.tag, err)
			}
			if missing {
				if t.opts.Missing != MissingSubstitute {
					return "", fmt.Errorf("%s: %w", p.field.tag, ErrMissing)
				}
				value = t.replacement
			} else {
				selected, err = modify(selected, vr, *p.field)
				if err != nil {
					return "", fmt.Errorf("%s: %w", p.field.tag, err)
				}
				// Digest/date results are expansion output, not source payload.
				value, err = sanitizeAttribute(selected, t.opts.MaxOutputBytes, t.opts.MaxComponentBytes)
				if err != nil {
					return "", fmt.Errorf("%s: %w", p.field.tag, err)
				}
			}
		}
		if len(value) > t.opts.MaxOutputBytes-output.Len() {
			return "", ErrLimitExceeded
		}
		output.WriteString(value)
	}
	result := output.String()
	if err := validatePath(result, t.opts.MaxOutputBytes, t.opts.MaxComponentBytes, t.opts.MaxComponents); err != nil {
		return "", err
	}
	return result, nil
}

func modify(value string, vr core.VR, f field) (string, error) {
	switch f.modifier {
	case "sha256", "hash":
		sum := sha256.Sum256([]byte(value))
		return hex.EncodeToString(sum[:]), nil
	case "md5":
		sum := md5.Sum([]byte(value))
		return hex.EncodeToString(sum[:]), nil
	case "date":
		if vr != core.VRDA || len(value) != 8 {
			return "", ErrValue
		}
		date, err := dcmtime.ParseDate(value)
		if err != nil {
			return "", ErrValue
		}
		date.Time = date.Time.AddDate(0, 0, f.days)
		if date.Time.Year() < 0 || date.Time.Year() > 9999 {
			return "", ErrValue
		}
		return date.DCM(), nil
	default:
		return value, nil
	}
}
