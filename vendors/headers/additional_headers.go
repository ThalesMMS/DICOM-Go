package headers

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// ErrUnsupportedHeader reports a well-framed private format/type/encoding
// whose interpretation is not known. It never silently guesses a layout.
var ErrUnsupportedHeader = errors.New("dicom/vendors: unsupported private header variant")

type headerBudget struct {
	limits                  Limits
	entries, bytes, decoded int
}

func newHeaderBudget(l Limits) *headerBudget { return &headerBudget{limits: l.normalized()} }
func (b *headerBudget) take(entries, raw, decoded int) error {
	if entries < 0 || raw < 0 || decoded < 0 || entries > b.limits.MaxEntries-b.entries || raw > b.limits.MaxBytes-b.bytes || decoded > b.limits.MaxDecodedBytes-b.decoded {
		return ErrLimit
	}
	b.entries += entries
	b.bytes += raw
	b.decoded += decoded
	return nil
}
func (b *headerBudget) line(lines *[]string, s string) error {
	if err := b.take(1, 0, len(s)); err != nil {
		return err
	}
	*lines = append(*lines, s)
	return nil
}
func privateSequence(o *object.Object, tag core.Tag, b *headerBudget) ([]*object.Object, error) {
	el, ok := o.Get(tag)
	if !ok {
		return nil, ErrNotFound
	}
	sq, ok := el.Value.(core.SequenceValue)
	if !ok {
		return nil, fmt.Errorf("%w: expected sequence", ErrInvalid)
	}
	// Check the complete resident subtree before GetSequence builds any facades.
	if err := checkHeaderSequence(sq, 0, b); err != nil {
		return nil, err
	}
	items, ok := o.GetSequence(tag)
	if !ok {
		return nil, ErrInvalid
	}
	return items, nil
}
func checkHeaderSequence(sq core.SequenceValue, depth int, b *headerBudget) error {
	if depth >= 64 {
		return fmt.Errorf("%w: sequence depth", ErrLimit)
	}
	if err := b.take(len(sq.Items), 0, 0); err != nil {
		return err
	}
	for _, item := range sq.Items {
		if err := b.take(len(item.Elements), 0, 0); err != nil {
			return err
		}
		for _, el := range item.Elements {
			if nested, ok := el.Value.(core.SequenceValue); ok {
				if err := checkHeaderSequence(nested, depth+1, b); err != nil {
					return err
				}
				continue
			}
			n, err := primitiveHeaderBytes(el, b.limits.MaxBytes-b.bytes)
			if err != nil {
				return err
			}
			if err := b.take(0, n, 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// Measure supported resident primitives without calling arbitrary Value
// methods or narrowing their size to core.Length (uint32). Header lengths
// cannot hide larger values; unresolved/opaque representations are refused.
func primitiveHeaderBytes(el core.Element, remaining int) (int, error) {
	if remaining < 0 {
		return 0, ErrLimit
	}
	n, count, width := 0, 0, 1
	switch v := el.Value.(type) {
	case core.RawValue:
		count = len(v)
	case core.StringValue:
		if len(v) > 0 {
			n = len(v) - 1 // encoded multiplicity separators
		}
		if n > remaining {
			return 0, ErrLimit
		}
		for _, part := range v {
			if len(part) > remaining-n {
				return 0, ErrLimit
			}
			n += len(part)
		}
		if n%2 != 0 {
			if n == remaining {
				return 0, ErrLimit
			}
			n++ // even DICOM text padding
		}
	case core.Uint16Value:
		count, width = len(v), 2
	case core.Int16Value:
		count, width = len(v), 2
	case core.Uint32Value:
		count, width = len(v), 4
	case core.Int32Value:
		count, width = len(v), 4
	case core.Float32Value:
		count, width = len(v), 4
	case core.TagValue:
		count, width = len(v), 4
	case core.Uint64Value:
		count, width = len(v), 8
	case core.Int64Value:
		count, width = len(v), 8
	case core.Float64Value:
		count, width = len(v), 8
	default:
		return 0, fmt.Errorf("%w: unresolved or unsupported primitive value", ErrInvalid)
	}
	if count > remaining/width {
		return 0, ErrLimit
	}
	if count != 0 {
		n = count * width // multiplication is bounded by remaining
	}
	if el.Header.HasLength() {
		if el.Header.Length == core.UndefinedLength {
			return 0, fmt.Errorf("%w: undefined primitive value length", ErrInvalid)
		}
		if uint64(el.Header.Length) > uint64(remaining) {
			return 0, ErrLimit
		}
		if stored := int(el.Header.Length); stored > n {
			n = stored // narrow only after checking against the int-sized budget
		}
	}
	return n, nil
}

func privateTagRequired(o *object.Object, group uint16, creator string, offset uint8) (core.Tag, error) {
	tag, err := o.PrivateTag(group, creator, offset)
	if err != nil || !o.Has(tag) {
		return core.Tag{}, ErrNotFound
	}
	return tag, nil
}

// DecodeToshibaCT3 decodes CT Private Data 2: two implicit little endian
// (0000,0000)/(0000,0001) UL headers, then a Part 10 file or an explicit VR
// little endian raw data set. Other raw syntaxes are refused rather than guessed.
func DecodeToshibaCT3(raw []byte, limits Limits) (*object.Object, error) {
	limits = limits.normalized()
	if len(raw) > limits.MaxBytes {
		return nil, ErrLimit
	}
	if len(raw) < 24 {
		return nil, ErrInvalid
	}
	for i, element := range []uint16{0, 1} {
		p := raw[i*12:]
		if binary.LittleEndian.Uint16(p) != 0 || binary.LittleEndian.Uint16(p[2:]) != element || binary.LittleEndian.Uint32(p[4:]) != 4 {
			return nil, fmt.Errorf("%w: CT3 wrapper", ErrInvalid)
		}
	}
	if binary.LittleEndian.Uint32(raw[8:]) != 12 || uint64(binary.LittleEndian.Uint32(raw[20:])) != uint64(len(raw)-24) {
		return nil, fmt.Errorf("%w: CT3 length", ErrInvalid)
	}
	return decodeHeaderDataSet(raw[24:], limits)
}

// ToshibaCT3Of locates the CT3 blob using its private creator.
func ToshibaCT3Of(o *object.Object, l Limits) (*object.Object, error) {
	l = l.normalized()
	raw, _, err := privateValue(o, 0x7005, []string{"TOSHIBA_MEC_CT3"}, 0x10, l)
	if err != nil {
		return nil, err
	}
	return DecodeToshibaCT3(raw, l)
}
func decodeHeaderDataSet(raw []byte, l Limits) (*object.Object, error) {
	syntax := transfer.ExplicitVRLittleEndian
	if len(raw) >= 132 && string(raw[128:132]) == "DICM" {
		// Bound and validate File Meta tokens before the object reader can
		// materialize primitives or silently discard unexpected containers.
		metaEntries, err := preflightHeaderFileMeta(raw, l)
		if err != nil {
			return nil, err
		}
		header, err := object.ReadPart10Header(bytes.NewReader(raw), object.ReadFileOptions{MaxElementBytes: int64(l.MaxBytes), MaxTotalBytes: int64(l.MaxBytes), MaxElements: l.MaxEntries, MaxFragments: l.MaxEntries, MaxSequenceDepth: 128})
		if err != nil {
			return nil, headerDataSetError(err)
		}
		if header.DataSetOffset < 0 || header.DataSetOffset > int64(len(raw)) {
			return nil, ErrInvalid
		}
		syntax = header.TransferSyntax
		raw = raw[int(header.DataSetOffset):]
		l.MaxEntries -= metaEntries
		if l.MaxEntries <= 0 {
			return nil, ErrLimit
		}
		if syntax.Deflated {
			// Inflate into an owned finite buffer before both token preflight and parse.
			// A ByteReader keeps any bytes after the DEFLATE stream visible.
			source := bytes.NewReader(raw)
			reader := flate.NewReader(source)
			limit := l.MaxDecodedBytes
			if limit > l.MaxBytes {
				limit = l.MaxBytes
			}
			decoded, err := io.ReadAll(io.LimitReader(reader, int64(limit)))
			var probe [1]byte
			extra, endErr := reader.Read(probe[:])
			closeErr := reader.Close()
			if extra > 0 {
				return nil, ErrLimit
			}
			if err != nil || endErr != io.EOF || closeErr != nil {
				return nil, fmt.Errorf("%w: embedded deflate", ErrInvalid)
			}
			if source.Len() != 0 {
				// PS3.5 A.5 requires exactly one NUL after an odd-length bitstream.
				remaining := source.Len()
				streamBytes := len(raw) - remaining
				pad, padErr := source.ReadByte()
				if remaining != 1 || streamBytes%2 != 1 || padErr != nil || pad != 0 {
					return nil, fmt.Errorf("%w: embedded deflate trailing bytes", ErrInvalid)
				}
			}
			raw = decoded
			syntax = transfer.ExplicitVRLittleEndian
		}
	}
	if len(raw) > l.MaxBytes {
		return nil, ErrLimit
	}
	if err := preflightHeaderDataSet(raw, syntax, l); err != nil {
		return nil, err
	}
	o, err := object.ReadDataSetWithOptions(bytes.NewReader(raw), syntax, object.ReadFileOptions{MaxElementBytes: int64(l.MaxBytes), MaxTotalBytes: int64(l.MaxBytes), MaxElements: l.MaxEntries, MaxFragments: l.MaxEntries, MaxSequenceDepth: 128})
	if err != nil {
		return nil, headerDataSetError(err)
	}
	return o, nil
}

// DecodePMTF reverses the complete byte string and reads an explicit VR little
// endian data set. It does not apply DICOM byte-order swapping to individual words.
func DecodePMTF(raw []byte, l Limits) (*object.Object, error) {
	l = l.normalized()
	if len(raw) > l.MaxBytes {
		return nil, ErrLimit
	}
	if len(raw) == 0 || len(raw)%2 != 0 {
		return nil, ErrInvalid
	}
	rev := make([]byte, len(raw))
	for i, v := range raw {
		rev[len(raw)-1-i] = v
	}
	if err := preflightHeaderDataSet(rev, transfer.ExplicitVRLittleEndian, l); err != nil {
		return nil, err
	}
	o, err := object.ReadDataSetWithOptions(bytes.NewReader(rev), transfer.ExplicitVRLittleEndian, object.ReadFileOptions{MaxElementBytes: int64(l.MaxBytes), MaxTotalBytes: int64(l.MaxBytes), MaxElements: l.MaxEntries, MaxFragments: l.MaxEntries, MaxSequenceDepth: 128})
	if err != nil {
		return nil, headerDataSetError(err)
	}
	return o, nil
}

// PMTFOf locates a PMTF/MEC sequence at offset 01 (or Toshiba offset 02),
// then decodes the offset 90 blob in each item. The item must reserve the same
// creator independently; an outer creator block is never assumed inside it.
func PMTFOf(o *object.Object, creator string, offset uint8, l Limits) ([]*object.Object, error) {
	if creator != "PMTF INFORMATION DATA" && creator != "CANON_MEC_MR3" && creator != "TOSHIBA_MEC_MR3" {
		return nil, ErrUnsupportedHeader
	}
	if offset != 1 && (creator != "TOSHIBA_MEC_MR3" || offset != 2) {
		return nil, ErrUnsupportedHeader
	}
	tag, err := privateTagRequired(o, 0x0029, creator, offset)
	if err != nil {
		return nil, err
	}
	b := newHeaderBudget(l)
	items, err := privateSequence(o, tag, b)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrInvalid
	}
	var out []*object.Object
	for _, item := range items {
		raw, _, err := privateValue(item, 0x0029, []string{creator}, 0x90, b.limits)
		if err != nil {
			return nil, fmt.Errorf("%w: PMTF item", err)
		}
		remaining := b.limits
		remaining.MaxEntries -= b.entries
		if remaining.MaxEntries <= 0 {
			return nil, ErrLimit
		}
		parsed, err := DecodePMTF(raw, remaining)
		if err != nil {
			return nil, err
		}
		if err := chargeHeaderObject(parsed, 0, b); err != nil {
			return nil, err
		}
		out = append(out, parsed)
	}
	return out, nil
}
func chargeHeaderObject(o *object.Object, depth int, b *headerBudget) error {
	return chargeHeaderElements(o.Elements(), depth, b)
}
func chargeHeaderElements(els []core.Element, depth int, b *headerBudget) error {
	if depth >= 64 {
		return ErrLimit
	}
	if err := b.take(len(els), 0, 0); err != nil {
		return err
	}
	for _, el := range els {
		if sq, ok := el.Value.(core.SequenceValue); ok {
			if err := b.take(len(sq.Items), 0, 0); err != nil {
				return err
			}
			for _, ds := range sq.Items {
				if err := chargeHeaderElements(ds.Elements, depth+1, b); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func headerObjectLines(o *object.Object, b *headerBudget, prefix string, depth int, lines *[]string) error {
	if depth >= 64 {
		return ErrLimit
	}
	for _, el := range o.Elements() {
		if err := b.take(1, 0, 0); err != nil {
			return err
		}
		p := prefix + el.Tag().String()
		if sq, ok := el.Value.(core.SequenceValue); ok {
			if err := b.take(len(sq.Items), 0, 0); err != nil {
				return err
			}
			for i, item := range func() []*object.Object { v, _ := o.GetSequence(el.Tag()); return v }() {
				if err := headerObjectLines(item, b, p+"["+strconv.Itoa(i)+"].", depth+1, lines); err != nil {
					return err
				}
			}
			continue
		}
		value := ""
		if el.VR().IsStringLike() {
			var err error
			value, err = o.LookupString(el.Tag())
			if err != nil {
				return fmt.Errorf("%w: embedded text", ErrInvalid)
			}
		} else {
			if raw, ok := el.Value.(core.RawValue); ok {
				n := len(raw)
				if n > 32 {
					n = 32
				}
				value = fmt.Sprintf("%x", raw[:n])
				if n < len(raw) {
					value += "..."
				}
			} else {
				value = fmt.Sprintf("%v", el.Value)
			}
		}
		if err := b.line(lines, p+" "+el.VR().String()+" ["+value+"]"); err != nil {
			return err
		}
	}
	return nil
}

// DecodeCanonPAS validates UTF-8 XML, preserving the original text. It accepts
// one document/root and no DTD/entity declarations, and performs no network IO.
func DecodeCanonPAS(raw []byte, l Limits) (string, error) {
	b := newHeaderBudget(l)
	if err := b.take(0, len(raw), 0); err != nil {
		return "", err
	}
	if len(raw) > 0 && raw[len(raw)-1] == 0 {
		raw = raw[:len(raw)-1]
	}
	if !utf8.Valid(raw) {
		return "", fmt.Errorf("%w: PAS requires UTF-8", ErrInvalid)
	}
	d := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})))
	depth, roots := 0, 0
	declaration := false
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("%w: PAS XML", ErrInvalid)
		}
		if err := b.take(1, 0, 0); err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if err := b.take(len(t.Attr), 0, 0); err != nil {
				return "", err
			}
			if depth == 0 {
				roots++
			}
			depth++
			if depth > 64 {
				return "", ErrLimit
			}
		case xml.EndElement:
			depth--
		case xml.ProcInst:
			if strings.EqualFold(t.Target, "xml") {
				if t.Target != "xml" || declaration || depth != 0 || roots != 0 {
					return "", ErrInvalid
				}
				declaration = true
			}
		case xml.Directive:
			return "", fmt.Errorf("%w: PAS DTD/directive", ErrUnsupportedHeader)
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(t)) != "" {
				return "", ErrInvalid
			}
		}
	}
	if depth != 0 || roots != 1 {
		return "", ErrInvalid
	}
	if err := b.take(0, 0, len(raw)); err != nil {
		return "", err
	}
	return string(raw), nil
}

// CanonPASOf locates PAS Reproduct Information (700d,xx19).
func CanonPASOf(o *object.Object, l Limits) (string, error) {
	l = l.normalized()
	raw, _, err := privateValue(o, 0x700d, []string{"CANON_MEC_MR3^10"}, 0x19, l)
	if err != nil {
		return "", err
	}
	return DecodeCanonPAS(raw, l)
}

func headerDataSetError(err error) error {
	if errors.Is(err, parser.ErrMaxDepthExceeded) || errors.Is(err, parser.ErrMaxElementsExceeded) || errors.Is(err, parser.ErrMaxElementBytesExceeded) || errors.Is(err, parser.ErrMaxTotalBytesExceeded) || errors.Is(err, parser.ErrMaxFragmentsExceeded) {
		return fmt.Errorf("%w: embedded dataset", ErrLimit)
	}
	return fmt.Errorf("%w: embedded dataset: %v", ErrInvalid, err)
}

// Count all tokens, including empty sequence/item containers, before building
// the dataset. MaxElements alone counts primitives and cannot bound empty items.
func preflightHeaderDataSet(raw []byte, syntax transfer.Syntax, l Limits) error {
	_, err := preflightHeaderTokens(raw, syntax, l, false)
	return err
}

// File Meta is explicit little endian and contains only primitive group 0002
// elements. Group length bounds its byte region; syntax resolution remains in
// the existing object reader after this bounded token validation.
func preflightHeaderFileMeta(raw []byte, l Limits) (int, error) {
	if len(raw) < 144 || binary.LittleEndian.Uint16(raw[132:]) != 2 ||
		binary.LittleEndian.Uint16(raw[134:]) != 0 || string(raw[136:138]) != "UL" ||
		binary.LittleEndian.Uint16(raw[138:]) != 4 {
		return 0, fmt.Errorf("%w: embedded File Meta group length", ErrInvalid)
	}
	end := uint64(144) + uint64(binary.LittleEndian.Uint32(raw[140:]))
	if end > uint64(len(raw)) {
		return 0, fmt.Errorf("%w: embedded File Meta length", ErrInvalid)
	}
	return preflightHeaderTokens(raw[132:int(end)], transfer.ExplicitVRLittleEndian, l, true)
}

func preflightHeaderTokens(raw []byte, syntax transfer.Syntax, l Limits, metaOnly bool) (int, error) {
	r := parser.NewReader(bytes.NewReader(raw), syntax, parser.ReaderOptions{MaxElementBytes: int64(l.MaxBytes), MaxTotalBytes: int64(l.MaxBytes), MaxSequenceDepth: 128, MaxElements: l.MaxEntries, MaxFragments: l.MaxEntries})
	for n := 0; ; n++ {
		token, err := r.Next()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return 0, headerDataSetError(err)
		}
		if n >= l.MaxEntries {
			return 0, ErrLimit
		}
		if metaOnly && (token.Kind != parser.TokenElement || token.Element.Tag().Group != 2) {
			return 0, fmt.Errorf("%w: embedded File Meta must contain primitive group 0002 elements", ErrInvalid)
		}
	}
}
