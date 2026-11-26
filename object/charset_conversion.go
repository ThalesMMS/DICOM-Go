package object

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
)

// ErrCharsetConversion reports text that ConvertTextToUTF8 could not convert
// under the chosen policy.
var ErrCharsetConversion = errors.New("dicom: text cannot be converted to UTF-8")

// CharsetConversionPolicy selects what happens to a scope whose text cannot
// be decoded, even with the fallbacks that TextOptions authorizes.
type CharsetConversionPolicy uint8

const (
	// CharsetConversionAtomic fails the whole call; nothing is converted.
	CharsetConversionAtomic CharsetConversionPolicy = iota
	// CharsetConversionKeepScope keeps that data set or item, and everything
	// that inherits its character set, byte-for-byte with its original
	// declaration. An item whose parent was converted gets its original
	// declaration written explicitly, so no UTF-8 declaration ever covers
	// unconverted bytes.
	CharsetConversionKeepScope
)

// CharsetReason is the fixed vocabulary of conversion diagnostics.
type CharsetReason string

const (
	CharsetFallbackUndeclared  CharsetReason = "fallback-undeclared"
	CharsetFallbackUndecodable CharsetReason = "fallback-undecodable"
	CharsetFallbackUnsupported CharsetReason = "fallback-unsupported-declaration"
	CharsetUndecodable         CharsetReason = "undecodable-text"
	CharsetUnsupported         CharsetReason = "unsupported-declaration"
	CharsetScopeKept           CharsetReason = "scope-kept"
	CharsetScopePinned         CharsetReason = "scope-kept-declaration-pinned"
	CharsetDeferredText        CharsetReason = "deferred-text-value"
)

// CharsetDiagnostic records one decision. Path is structural, for example
// "(0008,1115)[0].(0010,0010)"; Declared is the scope's declaration reduced to
// at most 64 characters of [A-Za-z0-9 _-] (others become '?'). No value text
// is ever included.
type CharsetDiagnostic struct {
	Path     string
	Reason   CharsetReason
	Declared string
}

// CharsetConversionError is returned under CharsetConversionAtomic.
type CharsetConversionError struct {
	Path     string
	Reason   CharsetReason
	Declared string
}

func (e *CharsetConversionError) Error() string {
	return fmt.Sprintf("%s: %s: %s (declared %q)", ErrCharsetConversion, e.Path, e.Reason, e.Declared)
}

func (e *CharsetConversionError) Unwrap() error { return ErrCharsetConversion }

// CharsetConversionReport summarizes a conversion.
type CharsetConversionReport struct {
	ConvertedValues      int
	FallbackValues       int
	KeptScopes           int
	Diagnostics          []CharsetDiagnostic
	DiagnosticsTruncated bool
}

// CharsetConversionOptions configures ConvertTextToUTF8. Fallbacks come from
// the object's TextOptions, so reading and converting follow one policy.
type CharsetConversionOptions struct {
	Policy CharsetConversionPolicy
	// MaxDiagnostics caps the report; 0 means 256. Counts stay exact.
	MaxDiagnostics int
}

var tagSpecificCharacterSetConversion = core.NewTag(0x0008, 0x0005)

const utf8Declaration = "ISO_IR 192"

// ConvertTextToUTF8 returns a copy of o whose text is UTF-8 and declared
// ISO_IR 192. Every raw value of a VR that uses Specific Character Set (LO,
// LT, PN, SH, ST, UC, UT, except Private Creators) is decoded with its scope's
// declaration, or the inherited one, and re-encoded; PN component groups and
// ISO 2022 escapes are decoded first. Other VRs, binary values and Private
// Creators are not touched, and o is not modified: writing o still preserves
// the original bytes and declarations.
//
// Values the scope's character set rejects use FallbackCharacterSet only in
// the cases o's TextOptions authorizes; each use is reported. A value that
// still cannot be decoded fails the call (CharsetConversionAtomic) or keeps
// its scope unchanged (CharsetConversionKeepScope). The result is verified by
// decoding every converted scope strictly as UTF-8 before it is returned.
func (o *Object) ConvertTextToUTF8(ctx context.Context, opts CharsetConversionOptions) (*Object, CharsetConversionReport, error) {
	if o == nil {
		return nil, CharsetConversionReport{}, fmt.Errorf("dicom: nil object")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.MaxDiagnostics <= 0 {
		opts.MaxDiagnostics = 256
	}
	c := &charsetConverter{ctx: ctx, text: o.text, opts: opts}
	root := charsetScope{charset: dicomenc.DefaultCharacterSet}
	if o.inheritedCharsetSet {
		root = charsetScope{charset: o.inheritedCharset, err: o.inheritedCharsetErr, declared: o.inheritedCharsetDeclared}
	}
	elements, _, err := c.dataSet(o.Elements(), "", root, false, true)
	if err != nil {
		return nil, c.report, err
	}
	if err := verifyScope(ctx, elements, "", root, c.keptScopes); err != nil {
		return nil, c.report, err
	}
	out := FromElementsWithTextOptions(elements, o.dict, TextOptions{})
	out.SetValueByteOrder(o.ValueByteOrder())
	out.valueProvider = o.valueProvider
	return out, c.report, nil
}

type charsetScope struct {
	charset  dicomenc.SpecificCharacterSet
	err      error
	declared bool
	terms    []string // the declaration's values, for pinning and diagnostics
}

type charsetConverter struct {
	ctx        context.Context
	text       TextOptions
	opts       CharsetConversionOptions
	report     CharsetConversionReport
	keptScopes map[string]bool
}

func (c *charsetConverter) note(path string, reason CharsetReason, scope charsetScope) {
	if len(c.report.Diagnostics) < c.opts.MaxDiagnostics {
		c.report.Diagnostics = append(c.report.Diagnostics, CharsetDiagnostic{Path: path, Reason: reason, Declared: sanitizeCharsetTerms(scope.terms)})
	} else {
		c.report.DiagnosticsTruncated = true
	}
}

// fail applies the policy to a scope that cannot be converted: an error, or
// errKeepScope for the caller to keep the scope.
func (c *charsetConverter) fail(path string, reason CharsetReason, scope charsetScope) error {
	if c.opts.Policy == CharsetConversionAtomic {
		return &CharsetConversionError{Path: path, Reason: reason, Declared: sanitizeCharsetTerms(scope.terms)}
	}
	c.note(path, reason, scope)
	return errKeepScope
}

var errKeepScope = errors.New("keep scope")

func sanitizeCharsetTerms(terms []string) string {
	joined := strings.Join(terms, `\`)
	var b strings.Builder
	for _, r := range joined {
		if b.Len() >= 64 {
			break
		}
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == ' ', r == '_', r == '-', r == '\\':
			b.WriteRune(r)
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
}

// scopeOf resolves a data set's own declaration, or inherits parent.
func scopeOf(elements []core.Element, parent charsetScope) (charsetScope, bool) {
	for _, element := range elements {
		if element.Tag() != tagSpecificCharacterSetConversion {
			continue
		}
		values, err := decodeTextValuesWithCodec(element, dicomenc.DefaultCharacterSet)
		if err != nil {
			return charsetScope{err: fmt.Errorf("%w: declaration is not text", dicomenc.ErrInvalidCharsetDeclaration), declared: true}, true
		}
		scope := charsetScope{terms: values}
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				scope.declared = true
			}
		}
		scope.charset, scope.err = dicomenc.ParseCharacterSet(values...)
		return scope, true
	}
	return parent, false
}

// dataSet converts one data set. parentConverted says whether the enclosing
// scope now declares UTF-8; top marks the root, which always declares it when
// converted. It returns the elements and whether this scope was converted.
func (c *charsetConverter) dataSet(elements []core.Element, path string, parent charsetScope, parentConverted, top bool) ([]core.Element, bool, error) {
	scope, own := scopeOf(elements, parent)
	out := append([]core.Element(nil), elements...)
	fallback := CharsetReason("")
	charset := scope.charset
	if scope.err != nil {
		if !c.text.AllowUnsupportedCharsetFallback {
			return c.keep(elements, path, path, CharsetUnsupported, scope, own, parent, parentConverted)
		}
		charset, fallback = c.text.FallbackCharacterSet, CharsetFallbackUnsupported
	}
	converted := 0
	type fallbackUse struct {
		path   string
		reason CharsetReason
	}
	var fallbacks []fallbackUse
	for i, element := range out {
		if err := c.ctx.Err(); err != nil {
			return nil, false, err
		}
		if !element.VR().UsesSpecificCharacterSet() || dictionary.IsPrivateCreatorTag(element.Tag()) {
			continue
		}
		elementPath := unPath(path, element.Tag())
		var raw core.RawValue
		switch value := element.Value.(type) {
		case core.RawValue:
			raw = value
		case nil:
			// A deferred value cannot be converted in memory.
			return c.keep(elements, path, elementPath, CharsetDeferredText, scope, own, parent, parentConverted)
		default:
			continue // authored strings are encoded by the writer
		}
		text, err := decodeWholeText(element.VR(), raw, charset)
		reason := fallback
		if err != nil && fallback == "" {
			authorized := scope.declared && c.text.CharsetFallback&FallbackUndecodable != 0 ||
				!scope.declared && c.text.CharsetFallback&FallbackUndeclared != 0
			if authorized {
				text, err = decodeWholeText(element.VR(), raw, c.text.FallbackCharacterSet)
				reason = CharsetFallbackUndecodable
				if !scope.declared {
					reason = CharsetFallbackUndeclared
				}
			}
		}
		if err != nil {
			return c.keep(elements, path, elementPath, CharsetUndecodable, scope, own, parent, parentConverted)
		}
		if reason != "" {
			fallbacks = append(fallbacks, fallbackUse{elementPath, reason})
		}
		value := core.RawValue(element.VR().PadToEvenLength([]byte(text)))
		element.Value = value
		element.Header.Length = core.Length(len(value))
		out[i] = element
		converted++
	}
	// This scope converts; its items inherit UTF-8 unless they declare their
	// own character set.
	for i, element := range out {
		sequence, ok := element.Value.(core.SequenceValue)
		if !ok {
			continue
		}
		elementPath := unPath(path, element.Tag())
		items := make([]core.DataSet, len(sequence.Items))
		for j, item := range sequence.Items {
			convertedItem, _, err := c.dataSet(item.Elements, fmt.Sprintf("%s[%d]", elementPath, j), scope, true, false)
			if err != nil {
				return nil, false, err
			}
			items[j] = item
			items[j].Elements = convertedItem
		}
		element.Value = core.SequenceValue{Items: items}
		out[i] = element
	}
	for _, use := range fallbacks {
		c.note(use.path, use.reason, scope)
	}
	c.report.FallbackValues += len(fallbacks)
	c.report.ConvertedValues += converted
	if own || top || !parentConverted {
		out = setDeclaration(out, []string{utf8Declaration})
	}
	return out, true, nil
}

// keep applies the policy to an unconvertible scope. Under KeepScope the
// original elements are returned; when the parent now declares UTF-8 and the
// scope inherited its declaration, the original declaration is pinned.
func (c *charsetConverter) keep(elements []core.Element, scopePath, path string, reason CharsetReason, scope charsetScope, own bool, parent charsetScope, parentConverted bool) ([]core.Element, bool, error) {
	if err := c.fail(path, reason, scope); !errors.Is(err, errKeepScope) {
		return nil, false, err
	}
	c.report.KeptScopes++
	if c.keptScopes == nil {
		c.keptScopes = make(map[string]bool)
	}
	c.keptScopes[scopePath] = true
	if own || !parentConverted {
		return elements, false, nil
	}
	c.note(path, CharsetScopePinned, parent)
	return setDeclaration(append([]core.Element(nil), elements...), parent.terms), false, nil
}

// setDeclaration replaces or inserts (0008,0005) in tag order. Empty terms
// declare the default repertoire explicitly.
func setDeclaration(elements []core.Element, terms []string) []core.Element {
	value := strings.Join(terms, `\`)
	element := core.Element{
		Header: core.ElementHeader{Tag: tagSpecificCharacterSetConversion, VR: core.VRCS},
		Value:  core.RawValue(core.VRCS.PadToEvenLength([]byte(value))),
	}
	element.Header.Length = core.Length(len(element.Value.(core.RawValue)))
	for i, existing := range elements {
		if existing.Tag() == tagSpecificCharacterSetConversion {
			elements[i] = element
			return elements
		}
	}
	return append(elements, element)
}

// decodeWholeText decodes a raw text value, delimiters included, to Unicode.
func decodeWholeText(vr core.VR, raw core.RawValue, charset dicomenc.SpecificCharacterSet) (string, error) {
	trimmed := core.TrimTextValueBytes(vr, raw.Bytes())
	switch {
	case vr == core.VRPN:
		return charset.DecodePersonName(trimmed)
	case !vr.UsesTextValueDelimiter():
		return charset.DecodeSingleValue(trimmed)
	default:
		return charset.Decode(trimmed)
	}
}

// verifyScope decodes every text value of every UTF-8 scope strictly, so no
// ISO_IR 192 declaration can cover bytes that were not converted. Kept scopes
// keep their own declaration and are not re-checked.
func verifyScope(ctx context.Context, elements []core.Element, path string, parent charsetScope, keptScopes map[string]bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if keptScopes[path] {
		return nil
	}
	scope, _ := scopeOf(elements, parent)
	if scope.err != nil {
		return &CharsetConversionError{Path: path, Reason: CharsetUnsupported, Declared: sanitizeCharsetTerms(scope.terms)}
	}
	utf8 := len(scope.terms) == 1 && strings.TrimSpace(scope.terms[0]) == utf8Declaration
	for _, element := range elements {
		if err := ctx.Err(); err != nil {
			return err
		}
		elementPath := unPath(path, element.Tag())
		if sequence, ok := element.Value.(core.SequenceValue); ok {
			for j, item := range sequence.Items {
				if err := verifyScope(ctx, item.Elements, fmt.Sprintf("%s[%d]", elementPath, j), scope, keptScopes); err != nil {
					return err
				}
			}
			continue
		}
		raw, ok := element.Value.(core.RawValue)
		if !ok || !utf8 || !element.VR().UsesSpecificCharacterSet() || dictionary.IsPrivateCreatorTag(element.Tag()) {
			continue
		}
		if _, err := decodeWholeText(element.VR(), raw, scope.charset); err != nil {
			return &CharsetConversionError{Path: elementPath, Reason: CharsetUndecodable, Declared: utf8Declaration}
		}
	}
	return nil
}
