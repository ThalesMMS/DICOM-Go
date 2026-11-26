package dicomxml

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/object"
)

const xmlNamespace = "http://www.w3.org/XML/1998/namespace"

func Marshal(obj *object.Object, opts Options) ([]byte, error) {
	return MarshalContext(context.Background(), obj, opts)
}

func MarshalContext(ctx context.Context, obj *object.Object, opts Options) ([]byte, error) {
	var out bytes.Buffer
	if err := EncodeContext(ctx, &out, obj, opts); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func Encode(w io.Writer, obj *object.Object, opts Options) error {
	return EncodeContext(context.Background(), w, obj, opts)
}

// EncodeContext writes each attribute directly. On failure, the writer may
// contain an incomplete document; callers must discard that output.
func EncodeContext(ctx context.Context, w io.Writer, obj *object.Object, opts Options) error {
	if w == nil {
		return &Error{Op: "encode", Err: fmt.Errorf("%w: nil writer", ErrInvalidXML)}
	}
	b, err := newBudget(ctx, opts.Limits)
	if err != nil {
		return &Error{Op: "encode", Err: err}
	}
	if err = b.check(); err != nil {
		return &Error{Op: "encode", Err: err}
	}
	if opts.Dictionary == nil {
		opts.Dictionary = std.Dictionary
	}
	if opts.ByteOrder == nil {
		opts.ByteOrder = binary.LittleEndian
		if obj != nil {
			opts.ByteOrder = obj.ValueByteOrder()
		}
	}
	if opts.RecoverPrivate && opts.PrivateRecovered == nil {
		return &Error{Op: "encode", Err: fmt.Errorf("%w: RecoverPrivate requires PrivateRecovered to report recoveries", ErrInvalidXML)}
	}
	if opts.RecoverText && opts.TextRecovered == nil {
		return &Error{Op: "encode", Err: fmt.Errorf("%w: RecoverText requires TextRecovered to report recoveries", ErrInvalidXML)}
	}
	lw := &limitedWriter{writer: w, remaining: b.limits.MaxXMLBytes, ctx: b.ctx}
	if _, err = io.WriteString(lw, xml.Header); err != nil {
		return &Error{Op: "encode", Err: err}
	}
	enc := xml.NewEncoder(lw)
	if opts.Pretty {
		enc.Indent("", "  ")
	}
	root := xml.StartElement{Name: xml.Name{Local: "NativeDicomModel"}, Attr: []xml.Attr{
		{Name: xml.Name{Local: "xmlns"}, Value: Namespace},
		{Name: xml.Name{Space: xmlNamespace, Local: "space"}, Value: "preserve"},
	}}
	if err = enc.EncodeToken(root); err != nil {
		return &Error{Op: "encode", Path: "/NativeDicomModel", Err: err}
	}
	writer := nativeEncoder{enc: enc, opts: opts, b: b, root: obj}
	if err = writer.dataset(obj, 0, "/NativeDicomModel"); err != nil {
		return err
	}
	if err = enc.EncodeToken(root.End()); err != nil {
		return &Error{Op: "encode", Path: "/NativeDicomModel", Err: err}
	}
	if err = enc.Flush(); err != nil {
		return &Error{Op: "encode", Err: err}
	}
	return nil
}

type limitedWriter struct {
	writer    io.Writer
	remaining int64
	ctx       context.Context
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.ctx != nil {
		if err := w.ctx.Err(); err != nil {
			return 0, err
		}
	}
	if int64(len(p)) > w.remaining {
		return 0, fmt.Errorf("%w: limit reached", ErrMaxXMLBytesExceeded)
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

type nativeEncoder struct {
	enc  *xml.Encoder
	opts Options
	b    *budget
	root *object.Object
}

func (e *nativeEncoder) dataset(obj *object.Object, depth int, path string) error {
	if err := e.b.addElements(0, depth); err != nil {
		return &Error{Op: "encode", Path: path, Err: err}
	}
	elements := obj.SortedElements()
	count := 0
	for _, elem := range elements {
		if !elem.Tag().IsGroupLength() {
			count++
		}
	}
	if err := e.b.addElements(count, depth); err != nil {
		return &Error{Op: "encode", Path: path, Err: err}
	}
	creators, err := e.privateCreators(obj, elements, path)
	if err != nil {
		return err
	}
	for _, elem := range elements {
		if elem.Tag().IsGroupLength() {
			continue
		}
		if err := e.b.check(); err != nil {
			return &Error{Op: "encode", Path: path, Err: err}
		}
		creator := ""
		if dictionary.IsPrivateDataTag(elem.Tag()) {
			creator, err = dataElementCreator(elem.Tag(), obj, creators, e.opts, path)
			if err != nil {
				return err
			}
			if creator == "" {
				continue
			}
		}
		var replacement *string
		if c, ok := creators[elem.Tag()]; ok {
			replacement = c.value
		}
		if err := e.element(obj, elem, creator, replacement, depth, path); err != nil {
			return err
		}
	}
	return nil
}

type privateCreator struct {
	id    string
	value *string
	first uint16
}

func (e *nativeEncoder) privateCreators(obj *object.Object, elements []core.Element, path string) (map[core.Tag]privateCreator, error) {
	out := map[core.Tag]privateCreator{}
	type creatorGroup struct {
		id    string
		group uint16
	}
	firsts := map[creatorGroup]uint16{}
	for _, elem := range elements {
		tag := elem.Tag()
		if !dictionary.IsPrivateCreatorTag(tag) {
			continue
		}
		if err := e.b.check(); err != nil {
			return nil, &Error{Op: "encode", Path: path, Err: err}
		}
		if _, ok := elem.Value.(core.BulkDataValue); ok {
			continue
		}
		raw := ""
		rewrite := false
		switch elem.VR() {
		case core.VRLO:
			values, err := obj.LookupStrings(tag)
			if err != nil {
				return nil, &Error{Op: "encode", Path: attributePath(path, tag), Err: err}
			}
			if len(values) != 1 {
				continue
			}
			raw = values[0]
		case core.VRUN:
			data, ok := elem.RawBytes()
			if !ok {
				if elem.Value != nil || !elem.Header.HasLength() || elem.EncodedLength() > 64 {
					continue
				}
				var b bytes.Buffer
				if err := e.copyValue(obj, elem, &limitedWriter{writer: &b, remaining: 96, ctx: e.b.ctx}); err != nil {
					continue
				}
				data = b.Bytes()
			}
			if len(data) > 96 {
				continue
			}
			raw, rewrite = string(data), true
		default:
			continue
		}
		id, err := dictionary.PrivateCreatorID(raw)
		recovered := false
		if err != nil && e.opts.RecoverPrivate {
			if trimmed := strings.TrimRight(raw, " \x00"); trimmed != raw {
				id, err = dictionary.PrivateCreatorID(trimmed)
				recovered = err == nil
			}
		}
		if err != nil {
			continue
		}
		c := privateCreator{id: id}
		// LO retains its original decoded spelling, matching the creator Value.
		if elem.VR() == core.VRLO && !recovered {
			c.id = raw
		}
		if rewrite || recovered {
			c.value = &id
		}
		key := creatorGroup{c.id, tag.Group}
		if first, ok := firsts[key]; ok {
			c.first = first
		} else {
			firsts[key] = tag.Element
		}
		out[tag] = c
		if recovered {
			e.opts.PrivateRecovered(PrivateRecovery{Kind: PrivateCreatorNULTrimmed, Path: attributePath(path, tag), Tag: tag, Creator: id})
		}
	}
	return out, nil
}

func attributePath(parent string, tag core.Tag) string {
	return parent + "/DicomAttribute[@tag='" + tag.HexString() + "']"
}

func dataElementCreator(tag core.Tag, dataset *object.Object, creators map[core.Tag]privateCreator, opts Options, parentPath string) (string, error) {
	path := attributePath(parentPath, tag)
	creator, ok := creators[core.NewTag(tag.Group, tag.Element>>8)]
	switch {
	case !ok:
		if !opts.RecoverPrivate {
			return "", &Error{Op: "encode", Path: path, Err: fmt.Errorf("%w: private data element has no valid private creator", ErrInvalidXML)}
		}
		opts.PrivateRecovered(PrivateRecovery{Kind: PrivateElementOmitted, Path: path, Tag: tag})
		return "", nil
	case creator.first != 0:
		if !opts.RecoverPrivate {
			return "", &Error{Op: "encode", Path: path, Err: fmt.Errorf("%w: private creator %q reserves blocks %02X and %02X of group %04X, which the Native DICOM Model cannot tell apart", ErrInvalidXML, creator.id, creator.first, tag.Element>>8, tag.Group)}
		}
		if dataset.Has(core.NewTag(tag.Group, creator.first<<8|tag.Element&0xff)) {
			opts.PrivateRecovered(PrivateRecovery{Kind: PrivateElementOmitted, Path: path, Tag: tag, Creator: creator.id})
			return "", nil
		}
		opts.PrivateRecovered(PrivateRecovery{Kind: PrivateElementMerged, Path: path, Tag: tag, Creator: creator.id})
	}
	return creator.id, nil
}

func (e *nativeEncoder) element(obj *object.Object, elem core.Element, creator string, replacement *string, depth int, parentPath string) error {
	tag, vr := elem.Tag(), elem.VR()
	path := attributePath(parentPath, tag)
	fail := func(err error) error {
		if _, ok := err.(*Error); ok {
			return err
		}
		return &Error{Op: "encode", Path: path, Err: err}
	}
	if authored, ok := elem.Value.(core.StringValue); ok {
		if err := valueencode.ValidateString(vr, authored); err != nil {
			return fail(err)
		}
	}
	if replacement != nil {
		vr = core.VRLO
	}
	if _, ok := elem.Value.(core.SequenceValue); ok && vr == core.VRUN {
		vr = core.VRSQ
	}
	start := xml.StartElement{Name: xml.Name{Local: "DicomAttribute"}}
	xmlTag := tag.HexString()
	if creator != "" {
		xmlTag = fmt.Sprintf("%04X00%02X", tag.Group, tag.Element&0xff)
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "privateCreator"}, Value: creator})
	}
	start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "tag"}, Value: xmlTag})
	if vr != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "vr"}, Value: vr.String()})
	}
	if entry, ok := e.opts.Dictionary.ByTag(tag); ok && entry.Keyword != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "keyword"}, Value: entry.Keyword})
	}
	if err := checkXMLAttrs(start.Attr); err != nil {
		return fail(err)
	}
	if err := e.enc.EncodeToken(start); err != nil {
		return fail(err)
	}
	switch value := elem.Value.(type) {
	case core.BulkDataValue:
		if value.URI != "" {
			if err := e.bulk(value.URI, path); err != nil {
				return err
			}
		}
	default:
		switch {
		case replacement != nil:
			if err := e.textValues(tag, vr, []string{*replacement}, path); err != nil {
				return err
			}
		case vr == core.VRSQ:
			seq, ok := elem.Value.(core.SequenceValue)
			if !ok {
				return fail(fmt.Errorf("expected SequenceValue, got %T", elem.Value))
			}
			// Admission precedes GetSequence, which creates item facades/cache entries.
			if len(seq.Items) > e.b.limits.MaxSequenceItems-e.b.items {
				return fail(ErrMaxItemsExceeded)
			}
			if len(seq.Items) > 0 {
				if err := e.b.addElements(0, depth+1); err != nil {
					return fail(err)
				}
			}
			items, ok := obj.GetSequence(tag)
			if !ok {
				return fail(fmt.Errorf("expected SequenceValue"))
			}
			for i, item := range items {
				if err := e.b.addItem(depth + 1); err != nil {
					return fail(err)
				}
				itemStart := xml.StartElement{Name: xml.Name{Local: "Item"}, Attr: []xml.Attr{{Name: xml.Name{Local: "number"}, Value: strconv.Itoa(i + 1)}}}
				if err := e.enc.EncodeToken(itemStart); err != nil {
					return fail(err)
				}
				if err := e.dataset(item, depth+1, fmt.Sprintf("%s/Item[%d]", path, i+1)); err != nil {
					return err
				}
				if err := e.enc.EncodeToken(itemStart.End()); err != nil {
					return fail(err)
				}
			}
		case vr.IsStringLike():
			values, err := obj.LookupStrings(tag)
			if err != nil {
				return fail(err)
			}
			if err = e.textValues(tag, vr, values, path); err != nil {
				return err
			}
		default:
			if err := e.binaryOrNumeric(obj, elem, path); err != nil {
				return fail(err)
			}
		}
	}
	if err := e.enc.EncodeToken(start.End()); err != nil {
		return fail(err)
	}
	return nil
}

func (e *nativeEncoder) bulk(uri, path string) error {
	attrs := []xml.Attr{{Name: xml.Name{Local: "uri"}, Value: uri}}
	if strings.HasPrefix(strings.ToLower(uri), "urn:uuid:") {
		attrs[0] = xml.Attr{Name: xml.Name{Local: "uuid"}, Value: uri[9:]}
	}
	if err := checkXMLAttrs(attrs); err != nil {
		return &Error{Op: "encode", Path: path + "/BulkData", Err: err}
	}
	if err := emptyElement(e.enc, "BulkData", attrs); err != nil {
		return &Error{Op: "encode", Path: path, Err: err}
	}
	return nil
}

func (e *nativeEncoder) textValues(tag core.Tag, vr core.VR, values []string, path string) error {
	if len(values) == 1 && values[0] == "" {
		return nil
	}
	for i, value := range values {
		if err := e.b.checkValue(len(value)); err != nil {
			return &Error{Op: "encode", Path: path, Err: err}
		}
		if vr == core.VRPN {
			pnPath := fmt.Sprintf("%s/PersonName[%d]", path, i+1)
			start := xml.StartElement{Name: xml.Name{Local: "PersonName"}, Attr: []xml.Attr{{Name: xml.Name{Local: "number"}, Value: strconv.Itoa(i + 1)}}}
			if err := e.enc.EncodeToken(start); err != nil {
				return &Error{Op: "encode", Path: path, Err: err}
			}
			groups := strings.SplitN(value, "=", 3)
			names := []string{"Alphabetic", "Ideographic", "Phonetic"}
			if i == len(values)-1 {
				for j := len(groups) - 1; j >= 0; j-- {
					if groups[j] != "" {
						groups[j] = trimNULPadding(groups[j], e.opts, tag, pnPath+"/"+names[j])
						break
					}
				}
			}
			for j, group := range groups {
				if group != "" {
					if err := encodePersonNameGroup(e.enc, names[j], group, pnPath+"/"+names[j]); err != nil {
						return err
					}
				}
			}
			if err := e.enc.EncodeToken(start.End()); err != nil {
				return &Error{Op: "encode", Path: path, Err: err}
			}
		} else {
			valuePath := fmt.Sprintf("%s/Value[%d]", path, i+1)
			isCreator := dictionary.IsPrivateCreatorTag(tag)
			if i == len(values)-1 && !isCreator {
				value = trimNULPadding(value, e.opts, tag, valuePath)
			}
			if err := checkXMLText(value); err != nil {
				if isCreator {
					err = fmt.Errorf("private creator: %w", err)
				}
				return &Error{Op: "encode", Path: valuePath, Err: err}
			}
			if err := e.numberedValue(value, i+1); err != nil {
				return &Error{Op: "encode", Path: path, Err: err}
			}
		}
	}
	return nil
}

func (e *nativeEncoder) numberedValue(value string, number int) error {
	if err := e.b.checkValue(len(value)); err != nil {
		return err
	}
	start := xml.StartElement{Name: xml.Name{Local: "Value"}, Attr: []xml.Attr{{Name: xml.Name{Local: "number"}, Value: strconv.Itoa(number)}}}
	if err := e.enc.EncodeToken(start); err != nil {
		return err
	}
	if value != "" {
		if err := e.enc.EncodeToken(xml.CharData(value)); err != nil {
			return err
		}
	}
	return e.enc.EncodeToken(start.End())
}

func encodePersonNameGroup(enc *xml.Encoder, name, value, path string) error {
	start := xml.StartElement{Name: xml.Name{Local: name}}
	if err := enc.EncodeToken(start); err != nil {
		return &Error{Op: "encode", Path: path, Err: err}
	}
	parts := strings.SplitN(value, "^", 5)
	names := []string{"FamilyName", "GivenName", "MiddleName", "NamePrefix", "NameSuffix"}
	for i, part := range parts {
		if part == "" {
			continue
		}
		if err := checkXMLText(part); err != nil {
			return &Error{Op: "encode", Path: path + "/" + names[i], Err: err}
		}
		if err := textElement(enc, names[i], part); err != nil {
			return &Error{Op: "encode", Path: path, Err: err}
		}
	}
	if err := enc.EncodeToken(start.End()); err != nil {
		return &Error{Op: "encode", Path: path, Err: err}
	}
	return nil
}

// checkXMLText rejects text that XML 1.0 (PS3.19) cannot represent: C0
// controls other than TAB, LF and CR (NUL above all), U+FFFE, U+FFFF,
// surrogates and invalid UTF-8. encoding/xml would otherwise silently write
// U+FFFD in their place. The error names the character and its byte offset,
// not the value.
func checkXMLText(s string) error {
	for i, r := range s {
		if r == utf8.RuneError {
			if _, size := utf8.DecodeRuneInString(s[i:]); size == 1 {
				return fmt.Errorf("%w: invalid UTF-8 at byte %d", ErrInvalidXML, i)
			}
			continue
		}
		if !isXMLChar(r) {
			return fmt.Errorf("%w: character %U at byte %d cannot be represented in XML 1.0", ErrInvalidXML, r, i)
		}
	}
	return nil
}

// isXMLChar reports whether r matches the XML 1.0 Char production.
func isXMLChar(r rune) bool {
	return r == 0x09 || r == 0x0A || r == 0x0D ||
		r >= 0x20 && r <= 0xD7FF ||
		r >= 0xE000 && r <= 0xFFFD ||
		r >= 0x10000 && r <= 0x10FFFF
}

// checkXMLAttrs applies checkXMLText to attribute values. privateCreator
// values already exclude control characters (dictionary.PrivateCreatorID);
// keyword and BulkData uri may come from a caller's dictionary or function.
func checkXMLAttrs(attrs []xml.Attr) error {
	for _, attr := range attrs {
		if err := checkXMLText(attr.Value); err != nil {
			return fmt.Errorf("%s attribute: %w", attr.Name.Local, err)
		}
	}
	return nil
}

// trimNULPadding applies RecoverText to the text that ends a Value Field:
// when value is unrepresentable only for trailing NUL padding, it returns
// value without its trailing NULs and spaces and reports the recovery;
// otherwise it returns value unchanged for checkXMLText to reject.
func trimNULPadding(value string, opts Options, tag core.Tag, path string) string {
	if !opts.RecoverText || checkXMLText(value) == nil {
		return value
	}
	trimmed := strings.TrimRight(value, " \x00")
	if !strings.Contains(value[len(trimmed):], "\x00") || checkXMLText(trimmed) != nil {
		return value
	}
	opts.TextRecovered(TextRecovery{Kind: TextNULPaddingTrimmed, Path: path, Tag: tag})
	return trimmed
}

func textElement(enc *xml.Encoder, name, value string) error {
	start := xml.StartElement{Name: xml.Name{Local: name}}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	if value != "" {
		if err := enc.EncodeToken(xml.CharData(value)); err != nil {
			return err
		}
	}
	return enc.EncodeToken(start.End())
}

func emptyElement(enc *xml.Encoder, name string, attrs []xml.Attr) error {
	start := xml.StartElement{Name: xml.Name{Local: name}, Attr: attrs}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	return enc.EncodeToken(start.End())
}
