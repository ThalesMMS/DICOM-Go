package attrpath

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/object"
)

// Operation is one kind of scripted edit.
type Operation string

const (
	// OpInsert inserts or replaces (dcmodify -i).
	OpInsert Operation = "insert"
	// OpModify replaces existing attributes only (dcmodify -m).
	OpModify Operation = "modify"
	// OpRemove removes attributes or items (dcmodify -e).
	OpRemove Operation = "remove"
	// OpCreate makes sure a path exists, with empty new attributes.
	OpCreate Operation = "create"
)

// Edit is one scripted operation. IfPresent turns a path that selects
// nothing into a no-op for modify and remove.
type Edit struct {
	Op        Operation
	Path      Path
	Value     string
	IfPresent bool
}

// Script is an ordered list of edits.
type Script []Edit

const (
	// MaxScriptBytes bounds script text.
	MaxScriptBytes = 1 << 20
	// MaxScriptEdits bounds the number of edits in a script.
	MaxScriptEdits = 4096
)

// ParseScript reads one edit per line:
//
//	insert ReferencedSeriesSequence[0].SeriesInstanceUID=1.2.3
//	modify PatientName=ANONYMOUS
//	modify-if-present (0010,1000)=
//	remove (0029,"SIEMENS CSA HEADER",10)
//	remove-if-present OtherPatientIDsSequence
//	create ContentSequence[0]
//
// Blank lines and lines starting with "#" are ignored. The value is the
// text after the first "=" outside a quoted private creator, kept as
// written; remove takes no value.
func ParseScript(text string, dict dictionary.DataDictionary) (Script, error) {
	if len(text) > MaxScriptBytes {
		return nil, fmt.Errorf("%w: script longer than %d bytes", ErrLimit, MaxScriptBytes)
	}
	var script Script
	for number, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if len(script) == MaxScriptEdits {
			return nil, fmt.Errorf("%w: more than %d edits", ErrLimit, MaxScriptEdits)
		}
		edit, err := parseEdit(strings.TrimLeft(line, " \t"), dict)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", number+1, err)
		}
		script = append(script, edit)
	}
	return script, nil
}

func parseEdit(line string, dict dictionary.DataDictionary) (Edit, error) {
	word, rest, _ := strings.Cut(line, " ")
	var edit Edit
	switch word {
	case "insert", "modify", "remove", "create":
		edit.Op = Operation(word)
	case "modify-if-present", "remove-if-present":
		edit.Op, edit.IfPresent = Operation(strings.TrimSuffix(word, "-if-present")), true
	default:
		return edit, fmt.Errorf("%w: unknown operation %q", ErrSyntax, word)
	}
	rest = strings.TrimLeft(rest, " \t")
	pathText, value, hasValue := cutOutsideQuotes(rest, '=')
	pathText = strings.TrimSpace(pathText)
	if edit.Op == OpRemove && hasValue {
		return edit, fmt.Errorf("%w: remove takes no value", ErrSyntax)
	}
	if edit.Op == OpModify && !hasValue {
		return edit, fmt.Errorf("%w: modify needs path=value", ErrSyntax)
	}
	path, err := Parse(pathText, dict)
	if err != nil {
		return edit, err
	}
	edit.Path, edit.Value = path, value
	return edit, nil
}

// cutOutsideQuotes splits at the first sep outside double quotes.
func cutOutsideQuotes(s string, sep byte) (before, after string, found bool) {
	quoted := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			quoted = !quoted
		case sep:
			if !quoted {
				return s[:i], s[i+1:], true
			}
		}
	}
	return s, "", false
}

// String renders the script in the ParseScript syntax.
func (s Script) String() string {
	var b strings.Builder
	for _, edit := range s {
		b.WriteString(string(edit.Op))
		if edit.IfPresent {
			b.WriteString("-if-present")
		}
		b.WriteByte(' ')
		b.WriteString(edit.Path.String())
		if edit.Op != OpRemove && (edit.Value != "" || edit.Op == OpModify) {
			b.WriteByte('=')
			b.WriteString(edit.Value)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Apply runs the edits in order and returns how many attributes or items
// they touched. It stops at the first failing edit; earlier edits stay
// applied, so callers wanting atomicity apply the script to a clone.
func (s Script) Apply(dataset *object.Object, opts Options) (int, error) {
	total := 0
	for i, edit := range s {
		var n int
		var err error
		switch edit.Op {
		case OpInsert:
			n, err = Insert(dataset, edit.Path, edit.Value, opts)
		case OpModify:
			n, err = Modify(dataset, edit.Path, edit.Value, opts)
		case OpRemove:
			n, err = Remove(dataset, edit.Path)
		case OpCreate:
			n, err = Create(dataset, edit.Path, opts)
		default:
			err = fmt.Errorf("%w: unknown operation %q", ErrSyntax, edit.Op)
		}
		if err != nil {
			if edit.IfPresent && errors.Is(err, ErrNotFound) {
				continue
			}
			return total, fmt.Errorf("edit %d (%s %s): %w", i+1, edit.Op, edit.Path, err)
		}
		total += n
	}
	return total, nil
}
