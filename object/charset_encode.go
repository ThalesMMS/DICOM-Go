package object

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
)

// UnrepresentablePolicy selects what ConvertTextToCharacterSet does with a
// character the target character set cannot represent.
type UnrepresentablePolicy uint8

const (
	// UnrepresentableFail fails the call (the default); nothing is lost.
	UnrepresentableFail UnrepresentablePolicy = iota
	// UnrepresentableTransliterate replaces the character with a similar
	// looking representable text (dcmconv --transliterate), or "?".
	UnrepresentableTransliterate
	// UnrepresentableDiscard drops the character (dcmconv --discard-illegal).
	UnrepresentableDiscard
)

// Reasons reported by ConvertTextToCharacterSet.
const (
	CharsetTransliterated  CharsetReason = "characters-transliterated"
	CharsetDiscarded       CharsetReason = "characters-discarded"
	CharsetUnrepresentable CharsetReason = "unrepresentable-character"
)

// ErrUnrepresentableText reports text the target character set cannot hold
// under UnrepresentableFail.
var ErrUnrepresentableText = errors.New("dicom: text cannot be represented in the target character set")

// CharsetEncodeOptions configures ConvertTextToCharacterSet.
type CharsetEncodeOptions struct {
	// Unrepresentable handles characters outside the target repertoire.
	Unrepresentable UnrepresentablePolicy
	// Decode configures the first step, decoding the source text; see
	// ConvertTextToUTF8.
	Decode CharsetConversionOptions
}

// CharsetEncodeReport summarizes ConvertTextToCharacterSet. It counts
// characters and names element paths, never their text.
type CharsetEncodeReport struct {
	CharsetConversionReport
	EncodedValues            int
	TransliteratedCharacters int
	DiscardedCharacters      int
}

// ConvertTextToCharacterSet returns a copy of o whose text is encoded in the
// character set target (a Specific Character Set declaration such as
// "ISO_IR 100", "" for the default repertoire, or ISO 2022 terms separated
// by backslashes). The text of each VR that uses Specific Character Set
// (LO, LT, PN, SH, ST, UC, UT, except Private Creators) is decoded as
// ConvertTextToUTF8 does and re-encoded; declarations are rewritten. UIDs,
// other VRs and binary values are never touched, and o is not modified.
//
// Characters target cannot represent fail the call (UnrepresentableFail),
// are approximated (UnrepresentableTransliterate: an explicit table for
// symbols, ligatures and letters without a decomposition, then the
// compatibility decomposition without diacritics, then "?") or dropped
// (UnrepresentableDiscard). Scopes that the decode policy kept unconverted
// keep their original text and declaration.
func (o *Object) ConvertTextToCharacterSet(ctx context.Context, target string, opts CharsetEncodeOptions) (*Object, CharsetEncodeReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	terms := strings.Split(target, `\`)
	if strings.TrimSpace(target) == "" {
		terms = nil
	}
	charset, err := dicomenc.ParseCharacterSet(terms...)
	if err != nil {
		return nil, CharsetEncodeReport{}, err
	}
	utf8, decodeReport, err := o.ConvertTextToUTF8(ctx, opts.Decode)
	report := CharsetEncodeReport{CharsetConversionReport: decodeReport}
	if err != nil {
		return nil, report, err
	}
	e := &charsetEncoder{ctx: ctx, charset: charset, terms: terms, policy: opts.Unrepresentable, report: &report, max: opts.Decode.MaxDiagnostics, known: map[rune]bool{}}
	if e.max <= 0 {
		e.max = 256
	}
	elements, err := e.dataSet(utf8.Elements(), "", true, true)
	if err != nil {
		return nil, report, err
	}
	out := FromElementsWithTextOptions(elements, o.dict, TextOptions{})
	out.SetValueByteOrder(o.ValueByteOrder())
	out.valueProvider = o.valueProvider
	return out, report, nil
}

type charsetEncoder struct {
	ctx     context.Context
	charset dicomenc.SpecificCharacterSet
	terms   []string
	policy  UnrepresentablePolicy
	report  *CharsetEncodeReport
	max     int
	known   map[rune]bool
}

func (e *charsetEncoder) note(path string, reason CharsetReason) {
	if len(e.report.Diagnostics) >= e.max {
		e.report.DiagnosticsTruncated = true
		return
	}
	e.report.Diagnostics = append(e.report.Diagnostics, CharsetDiagnostic{Path: path, Reason: reason, Declared: sanitizeCharsetTerms(e.terms)})
}

// dataSet re-encodes the UTF-8 scopes of one data set. inheritedUTF8 says
// whether the scope inherits a converted (UTF-8) declaration.
func (e *charsetEncoder) dataSet(elements []core.Element, path string, inheritedUTF8, top bool) ([]core.Element, error) {
	utf8 := inheritedUTF8
	own := false
	for _, element := range elements {
		if element.Tag() == tagSpecificCharacterSetConversion {
			own = true
			values, _ := decodeTextValuesWithCodec(element, dicomenc.DefaultCharacterSet)
			utf8 = len(values) == 1 && strings.EqualFold(strings.TrimSpace(values[0]), utf8Declaration)
		}
	}
	out := append([]core.Element(nil), elements...)
	for i, element := range out {
		if err := e.ctx.Err(); err != nil {
			return nil, err
		}
		elementPath := unPath(path, element.Tag())
		if sequence, ok := element.Value.(core.SequenceValue); ok {
			items := make([]core.DataSet, len(sequence.Items))
			for j, item := range sequence.Items {
				converted, err := e.dataSet(item.Elements, fmt.Sprintf("%s[%d]", elementPath, j), utf8, false)
				if err != nil {
					return nil, err
				}
				items[j] = item
				items[j].Elements = converted
			}
			element.Value = core.SequenceValue{Items: items}
			out[i] = element
			continue
		}
		if !element.VR().UsesSpecificCharacterSet() || dictionary.IsPrivateCreatorTag(element.Tag()) {
			continue
		}
		// Raw values are UTF-8 once their scope was converted; decoded
		// string values (from DICOM JSON or XML, for example) always are.
		var source string
		switch value := element.Value.(type) {
		case core.RawValue:
			if !utf8 {
				continue
			}
			source = strings.TrimRight(string(value), " \x00")
		case core.StringValue:
			source = strings.Join(value, `\`)
		default:
			continue
		}
		text, err := e.text(source, elementPath)
		if err != nil {
			return nil, err
		}
		text = trimTrailingSpaces(element.VR(), text)
		encoded, err := encodeTextValue(element.VR(), text, e.charset)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrUnrepresentableText, elementPath, err)
		}
		value := core.RawValue(element.VR().PadToEvenLength(encoded))
		element.Value = value
		element.Header.Length = core.Length(len(value))
		out[i] = element
		e.report.EncodedValues++
	}
	if utf8 && (own || top) {
		if len(e.terms) == 0 {
			kept := out[:0]
			for _, element := range out {
				if element.Tag() != tagSpecificCharacterSetConversion {
					kept = append(kept, element)
				}
			}
			out = kept
		} else {
			out = setDeclaration(out, e.terms)
		}
	}
	return out, nil
}

// text applies the policy to the characters of one value.
func (e *charsetEncoder) text(value, path string) (string, error) {
	var b strings.Builder
	transliterated, discarded := 0, 0
	for _, r := range value {
		if e.representable(r) {
			b.WriteRune(r)
			continue
		}
		switch e.policy {
		case UnrepresentableTransliterate:
			b.WriteString(e.approximate(r, 0))
			transliterated++
		case UnrepresentableDiscard:
			discarded++
		default:
			e.note(path, CharsetUnrepresentable)
			return "", fmt.Errorf("%w: %s", ErrUnrepresentableText, path)
		}
	}
	if transliterated > 0 {
		e.report.TransliteratedCharacters += transliterated
		e.note(path, CharsetTransliterated)
	}
	if discarded > 0 {
		e.report.DiscardedCharacters += discarded
		e.note(path, CharsetDiscarded)
	}
	return b.String(), nil
}

func (e *charsetEncoder) representable(r rune) bool {
	if r < 0x80 {
		return true // every DICOM repertoire keeps ASCII in G0
	}
	if ok, seen := e.known[r]; seen {
		return ok
	}
	encoded, err := e.charset.Encode(string(r))
	ok := err == nil
	if ok {
		decoded, err := e.charset.Decode(encoded)
		ok = err == nil && decoded == string(r)
	}
	e.known[r] = ok
	return ok
}

func (e *charsetEncoder) representableString(s string) bool {
	for _, r := range s {
		if !e.representable(r) {
			return false
		}
	}
	return s != ""
}

// approximate returns representable text similar to r: the table entry,
// else the compatibility decomposition without combining marks (each part
// approximated in turn), else "?".
func (e *charsetEncoder) approximate(r rune, depth int) string {
	if replacement, ok := transliterations[r]; ok && e.representableString(replacement) {
		return replacement
	}
	for _, candidate := range preferredTransliterations[r] {
		if e.representableString(candidate) {
			return candidate
		}
	}
	if depth < 3 {
		var b strings.Builder
		changed := false
		for _, part := range norm.NFKD.String(string(r)) {
			if unicode.Is(unicode.Mn, part) {
				changed = true
				continue
			}
			if part == r {
				continue
			}
			changed = true
			if e.representable(part) {
				b.WriteRune(part)
			} else {
				b.WriteString(e.approximate(part, depth+1))
			}
		}
		if changed && b.Len() > 0 && !strings.Contains(b.String(), "?") {
			return b.String()
		}
	}
	return "?"
}

// trimTrailingSpaces removes the trailing spaces of each value, which DICOM
// treats as padding and which discarded or replaced characters can leave.
func trimTrailingSpaces(vr core.VR, text string) string {
	if !vr.UsesTextValueDelimiter() {
		return strings.TrimRight(text, " ")
	}
	values := strings.Split(text, `\`)
	for i := range values {
		values[i] = strings.TrimRight(values[i], " ")
	}
	return strings.Join(values, `\`)
}

// encodeTextValue encodes a whole (possibly multi-valued) text value with the
// codec its VR needs.
func encodeTextValue(vr core.VR, text string, charset dicomenc.SpecificCharacterSet) ([]byte, error) {
	switch {
	case vr == core.VRPN:
		return charset.EncodePersonName(text)
	case !vr.UsesTextValueDelimiter():
		return charset.EncodeSingleValue(text)
	default:
		return charset.Encode(text)
	}
}

// transliterations approximate characters that have no compatibility
// decomposition, or whose decomposition reads badly, following the choices
// DCMTK's oficonv makes for them.
var transliterations = map[rune]string{
	'Æ': "AE", 'æ': "ae", 'Ø': "O", 'ø': "o", 'Œ': "OE", 'œ': "oe", 'ß': "ss",
	'Ð': "D", 'ð': "d", 'Đ': "D", 'đ': "d", 'Þ': "Th", 'þ': "th", 'Ł': "L", 'ł': "l",
	'Ħ': "H", 'ħ': "h", 'ı': "i", 'Ŋ': "N", 'ŋ': "n", 'ĸ': "q", 'Ŧ': "T", 'ŧ': "t", 'ƒ': "f",
	'‛': "'", '“': `"`, '”': `"`, '„': `"`, '‟': `"`,
	'‹': "<", '›': ">", '«': "<<", '»': ">>",
	'‐': "-", '‑': "-", '‒': "-", '–': "-", '—': "-", '―': "-", '−': "-",
	'…': "...", '•': "o", '·': ".", '′': "'", '″': `"`, '⁄': "/",
	'€': "EUR", '£': "lb", '¥': "yen", '¢': "c", '©': "(c)", '®': "(R)", '™': "TM",
	'°': "^0", '±': "+/-", '×': "x", '÷': ":", '¬': "!", '¦': "|", '§': "S", '¶': "P",
	'¡': "!", '¿': "?", '¤': "?", 'µ': "u",
	' ': " ", ' ': " ", ' ': " ", ' ': " ", '​': "",
	'①': "(1)", '②': "(2)", '③': "(3)", '④': "(4)", '⑤': "(5)", '⑥': "(6)", '⑦': "(7)", '⑧': "(8)", '⑨': "(9)", '⑩': "(10)",
}

// preferredTransliterations lists candidates in order, the closest looking
// first, for characters whose best approximation depends on the target.
var preferredTransliterations = map[rune][]string{
	'‘': {"`", "'"},
	'’': {"´", "'"},
	'‚': {"‘", "`", "'"},
}
