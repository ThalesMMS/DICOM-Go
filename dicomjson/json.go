package dicomjson

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type Element struct {
	VR           string `json:"vr"`
	Value        []any  `json:"Value,omitempty"`
	InlineBinary string `json:"InlineBinary,omitempty"`
	BulkDataURI  string `json:"BulkDataURI,omitempty"`
}

var ErrMaxMaterializedValueBytesExceeded = errors.New("dicomjson: maximum materialized value bytes exceeded")

type PersonNameComponents struct {
	Alphabetic  string `json:"Alphabetic,omitempty"`
	Ideographic string `json:"Ideographic,omitempty"`
	Phonetic    string `json:"Phonetic,omitempty"`
}

type Options struct {
	// BulkDataReferenceURIFunc receives source metadata before payload replay.
	// Only recorded deferred values eligible for BulkDataURI are offered.
	// Empty URI uses the byte callback/InlineBinary/Value fallback.
	BulkDataReferenceURIFunc func(context.Context, object.BulkDataReference) (string, error)
	// MaxMaterializedValueBytes bounds each deferred fallback before allocation.
	// Zero preserves unlimited historical behavior; negative values are invalid.
	MaxMaterializedValueBytes int64
	root                      *object.Object
	path                      []parser.BulkDataPathStep
	inlineAlreadyBudgeted     bool
	Pretty                    bool
	OmitGroupLength           bool
	// BulkDataURIFunc receives the complete encoded Value Field bytes, including
	// fragment item headers and the delimiter for encapsulated Pixel Data.
	// OW, OF, OD, OL and OV bytes are in the byte order InlineBinary would
	// have: Little Endian unless SourceByteOrderInlineBinary is set.
	BulkDataURIFunc func(tag core.Tag, vr core.VR, data []byte) string
	// PixelDataBulkDataURIFunc can offload Pixel Data without first collecting
	// its complete value field in memory. The callback may open the value stream
	// zero or more times and must close every reader it opens. Returning an empty
	// URI falls back to the existing byte-slice callback or InlineBinary path.
	// Native OW Pixel Data streams in the byte order InlineBinary would have:
	// Little Endian (by sample for Bits Allocated 32 or 64) unless
	// SourceByteOrderInlineBinary is set; encapsulated Pixel Data is unchanged.
	PixelDataBulkDataURIFunc func(tag core.Tag, vr core.VR, open func() (io.ReadCloser, error)) (string, error)
	// ByteOrder is used when interpreting raw bytes for numeric VRs and AT.
	// When nil, the object's ValueByteOrder is used.
	ByteOrder binary.ByteOrder
	// SourceByteOrderInlineBinary writes the InlineBinary of OW, OF, OD, OL
	// and OV values in ByteOrder, the object's own value bytes, which Unmarshal
	// with the same ByteOrder and SourceByteOrderInlineBinary restores. By
	// default, when ByteOrder is Big Endian, these values are written in Little
	// Endian, as PS3.18 section F.2.7 requires: bytes are reversed in units of
	// the VR width, except OW Pixel Data with Bits Allocated 32 or 64 in the
	// same data set or item, which is reversed at the sample width, by the
	// rules of pixeldata.TranscodeDataSet. Unmarshal with a Big Endian
	// ByteOrder reverses them back (#1982). BulkDataURIFunc and
	// PixelDataBulkDataURIFunc receive the bytes in the same order. The
	// object's own values are never modified: a value held by the object is
	// reversed into a copy, after MaxInlineBinaryBytes has accepted it.
	SourceByteOrderInlineBinary bool
	// Deprecated: InlineBinary is Little Endian by default; this field has no
	// effect. Use SourceByteOrderInlineBinary to keep the object's byte order.
	LittleEndianInlineBinary bool
	// Limits bounds traversal and output resources. Zero values preserve the
	// historical unlimited behavior.
	Limits Limits
}

// UnmarshalOptions configures DICOM JSON to object conversion.
type UnmarshalOptions struct {
	TextOptions object.TextOptions
	// ByteOrder is used when encoding numeric VRs and AT from JSON Value arrays.
	// It defaults to binary.LittleEndian. When it is Big Endian, the Little
	// Endian InlineBinary of OW, OF, OD, OL and OV values is reversed into it,
	// as Options describes for Marshal.
	ByteOrder binary.ByteOrder
	// SourceByteOrderInlineBinary keeps InlineBinary bytes as they are, for
	// documents written with Options.SourceByteOrderInlineBinary.
	SourceByteOrderInlineBinary bool
	// TransferSyntax identifies the source value encoding. When it is
	// encapsulated, Pixel Data InlineBinary is reconstructed as a
	// core.FragmentSequence instead of a defined-length raw value.
	TransferSyntax transfer.Syntax
	// Limits bounds traversal and decoded resources. Zero values preserve the
	// historical unlimited behavior.
	Limits Limits
}

func DefaultOptions() Options {
	return Options{
		Pretty:          true,
		OmitGroupLength: true,
	}
}

// DefaultUnmarshalOptions returns the default DICOM JSON unmarshal options.
func DefaultUnmarshalOptions() UnmarshalOptions {
	return UnmarshalOptions{ByteOrder: binary.LittleEndian}
}

func Marshal(obj *object.Object, opts Options) ([]byte, error) {
	return MarshalContext(context.Background(), obj, opts)
}

// MarshalContext converts an object to DICOM JSON while observing ctx and the
// resource limits in opts.
func MarshalContext(ctx context.Context, obj *object.Object, opts Options) ([]byte, error) {
	budget, err := newConversionBudget(ctx, opts.Limits)
	if err != nil {
		return nil, err
	}
	if opts.ByteOrder == nil {
		if obj == nil {
			opts.ByteOrder = binary.LittleEndian
		} else {
			opts.ByteOrder = obj.ValueByteOrder()
		}
	}
	m, err := marshalElementsContext(obj, opts, budget, 0)
	if err != nil {
		return nil, err
	}
	encoded, err := marshalJSONWithLimit(m, opts.Pretty, opts.Limits.MaxJSONBytes)
	if err != nil {
		return nil, err
	}
	if err := budget.checkJSONBytes(len(encoded)); err != nil {
		return nil, err
	}
	return encoded, nil
}

func marshalJSONWithLimit(value any, pretty bool, limit int64) ([]byte, error) {
	if limit == 0 || limit == math.MaxInt64 {
		if pretty {
			return json.MarshalIndent(value, "", "  ")
		}
		return json.Marshal(value)
	}

	writer := &limitedJSONWriter{limit: limit + 1}
	encoder := json.NewEncoder(writer)
	if pretty {
		encoder.SetIndent("", "  ")
	}
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	encoded := writer.buffer.Bytes()
	if len(encoded) > 0 && encoded[len(encoded)-1] == '\n' {
		encoded = encoded[:len(encoded)-1]
	}
	return encoded, nil
}

type limitedJSONWriter struct {
	buffer bytes.Buffer
	limit  int64
}

func (writer *limitedJSONWriter) Write(data []byte) (int, error) {
	if int64(writer.buffer.Len()) > writer.limit-int64(len(data)) {
		return 0, fmt.Errorf("%w: limit %d", ErrMaxJSONBytesExceeded, writer.limit-1)
	}
	return writer.buffer.Write(data)
}

func MarshalCompact(obj *object.Object) ([]byte, error) {
	return Marshal(obj, Options{OmitGroupLength: true, Pretty: false})
}

func MarshalPretty(obj *object.Object) ([]byte, error) {
	return Marshal(obj, Options{OmitGroupLength: true, Pretty: true})
}

func MarshalObject(obj *object.Object) ([]byte, error) {
	return Marshal(obj, DefaultOptions())
}

func MarshalObjectWithOptions(obj *object.Object, opts Options) ([]byte, error) {
	return Marshal(obj, opts)
}

func Unmarshal(data []byte, dict dictionary.DataDictionary) (*object.Object, error) {
	return UnmarshalWithOptions(data, dict, DefaultUnmarshalOptions())
}

func UnmarshalWithTextOptions(data []byte, dict dictionary.DataDictionary, opts object.TextOptions) (*object.Object, error) {
	return UnmarshalWithOptions(data, dict, UnmarshalOptions{TextOptions: opts})
}

// UnmarshalWithOptions converts DICOM JSON into an object using explicit
// conversion options.
func UnmarshalWithOptions(data []byte, dict dictionary.DataDictionary, opts UnmarshalOptions) (*object.Object, error) {
	return UnmarshalContext(context.Background(), data, dict, opts)
}

// UnmarshalContext converts DICOM JSON into an object while observing ctx and
// the resource limits in opts.
func UnmarshalContext(ctx context.Context, data []byte, dict dictionary.DataDictionary, opts UnmarshalOptions) (*object.Object, error) {
	budget, err := newConversionBudget(ctx, opts.Limits)
	if err != nil {
		return nil, err
	}
	if err := budget.checkJSONBytes(len(data)); err != nil {
		return nil, err
	}
	if opts.ByteOrder == nil && opts.TransferSyntax.ByteOrder != nil {
		opts.ByteOrder = opts.TransferSyntax.ByteOrder
	}
	opts.ByteOrder = byteOrderOrDefault(opts.ByteOrder)
	ds, err := unmarshalDataSetContext(data, dict, opts, budget)
	if err != nil {
		return nil, err
	}
	obj := object.FromElementsWithTextOptions(ds.Elements, dict, opts.TextOptions)
	obj.SetValueByteOrder(opts.ByteOrder)
	return obj, nil
}

func byteOrderOrDefault(order binary.ByteOrder) binary.ByteOrder {
	if order == nil {
		return binary.LittleEndian
	}
	return order
}

func base64DecodedLength(encoded string) int {
	encodedLength := 0
	padding := 0
	for i := 0; i < len(encoded); i++ {
		if encoded[i] == '\r' || encoded[i] == '\n' {
			continue
		}
		encodedLength++
	}
	for i := len(encoded) - 1; i >= 0; i-- {
		switch encoded[i] {
		case '\r', '\n':
			continue
		case '=':
			padding++
			continue
		}
		break
	}
	decodedLength := base64.StdEncoding.DecodedLen(encodedLength) - padding
	if decodedLength < 0 {
		return 0
	}
	return decodedLength
}

func parseEncapsulatedPixelDataValue(data []byte, syntax transfer.Syntax, dict dictionary.DataDictionary, budget *conversionBudget) (core.FragmentSequence, error) {
	if err := budget.checkContext(); err != nil {
		return core.FragmentSequence{}, err
	}
	order := byteOrderOrDefault(syntax.ByteOrder)
	syntax.ByteOrder = order

	headerLength := 8
	if syntax.ExplicitVR {
		headerLength = 12
	}
	encoded := make([]byte, headerLength+len(data))
	order.PutUint16(encoded[0:2], core.TagPixelData.Group)
	order.PutUint16(encoded[2:4], core.TagPixelData.Element)
	if syntax.ExplicitVR {
		copy(encoded[4:6], core.VROB.String())
		order.PutUint32(encoded[8:12], uint32(core.UndefinedLength))
	} else {
		order.PutUint32(encoded[4:8], uint32(core.UndefinedLength))
	}
	copy(encoded[headerLength:], data)

	reader := parser.NewReader(bytes.NewReader(encoded), syntax, parser.ReaderOptions{
		Dictionary:          dict,
		MaxPixelDataBytes:   int64(len(data)),
		MaxFragments:        budget.parserFragmentLimit(),
		StrictReservedBytes: true,
		OddLengthPolicy:     parser.RejectOddLength,
	})
	dataset, err := reader.ReadDataSet()
	if err != nil {
		if errors.Is(err, parser.ErrMaxFragmentsExceeded) {
			return core.FragmentSequence{}, fmt.Errorf("%w: %v", ErrMaxSequenceItemsExceeded, err)
		}
		return core.FragmentSequence{}, fmt.Errorf("decode encapsulated Pixel Data Value Field: %w", err)
	}
	if err := budget.checkContext(); err != nil {
		return core.FragmentSequence{}, err
	}
	if len(dataset.Elements) != 1 || dataset.Elements[0].Header.Tag != core.TagPixelData {
		return core.FragmentSequence{}, fmt.Errorf("decode encapsulated Pixel Data Value Field: expected exactly one Pixel Data element")
	}
	fragments, ok := dataset.Elements[0].Value.(core.FragmentSequence)
	if !ok {
		return core.FragmentSequence{}, fmt.Errorf("decode encapsulated Pixel Data Value Field: got %T, want core.FragmentSequence", dataset.Elements[0].Value)
	}
	if err := budget.addSequenceItems(len(fragments.Fragments)); err != nil {
		return core.FragmentSequence{}, err
	}
	return fragments, nil
}

func marshalElementsContext(obj *object.Object, opts Options, budget *conversionBudget, depth int) (map[string]Element, error) {
	out := map[string]Element{}
	if opts.MaxMaterializedValueBytes < 0 {
		return nil, fmt.Errorf("%w: MaxMaterializedValueBytes must not be negative", ErrInvalidLimits)
	}
	if depth == 0 {
		opts.root = obj
		opts.path = nil
	}
	if obj == nil {
		return out, nil
	}
	elements := obj.SortedElements()
	elementCount := len(elements)
	if opts.OmitGroupLength {
		for _, elem := range elements {
			if elem.Tag().IsGroupLength() {
				elementCount--
			}
		}
	}
	if err := budget.enterDataSet(depth, elementCount); err != nil {
		return nil, err
	}

	for _, elem := range elements {
		if err := budget.checkContext(); err != nil {
			return nil, err
		}
		if opts.OmitGroupLength && elem.Tag().IsGroupLength() {
			continue
		}

		entry, err := marshalElementContext(obj, elem, opts, budget, depth)
		if err != nil {
			return nil, err
		}
		out[elem.Tag().HexString()] = entry
	}
	return out, nil
}

func marshalElementContext(obj *object.Object, elem core.Element, opts Options, budget *conversionBudget, depth int) (Element, error) {
	if err := budget.checkContext(); err != nil {
		return Element{}, err
	}
	entry := Element{VR: elem.VR().String()}
	// freshRaw reports a raw value this call read into a buffer of its own,
	// which may be reversed in place; other raw values belong to the object.
	freshRaw := false

	switch value := elem.Value.(type) {
	case core.BulkDataValue:
		entry.BulkDataURI = value.URI
		return entry, nil
	}
	if elem.Value == nil && parser.BulkDataVR(elem.VR()) && (elem.EncodedLength().IsUndefined() || elem.EncodedLength() > 0) {
		path := append(append([]parser.BulkDataPathStep(nil), opts.path...), parser.BulkDataPathStep{Tag: elem.Tag(), ItemIndex: -1})
		ref, eligible, err := opts.root.DeferredBulkDataReference(path)
		if err != nil {
			return Element{}, fmt.Errorf("dicomjson: reference %s: %w", elem.Tag(), err)
		}
		if eligible {
			if opts.BulkDataReferenceURIFunc != nil {
				uri, err := opts.BulkDataReferenceURIFunc(budget.ctx, ref)
				if err != nil {
					return Element{}, fmt.Errorf("dicomjson: offload reference %s: %w", elem.Tag(), err)
				}
				if err := budget.checkContext(); err != nil {
					return Element{}, err
				}
				if uri != "" {
					entry.BulkDataURI = uri
					return entry, nil
				}
			}
			// Keep Pixel Data's existing streaming callback before fallback replay.
			if elem.Tag() == core.TagPixelData && opts.PixelDataBulkDataURIFunc != nil {
				width := inlineByteOrderWidth(obj, elem, opts)
				open := func() (io.ReadCloser, error) {
					return streamingValueReader(func(w io.Writer) error {
						return writeLittleEndianUnits(w, width, func(w io.Writer) error {
							// Normalize as the existing pixel callback does (legacy
							// and CP-246) before the Little Endian conversion.
							normalized := &referenceNormalizingWriter{writer: w, width: ref.NormalizationWidth}
							_, err := opts.root.CopyBulkDataValueTo(budget.ctx, path, normalized)
							if err == nil && len(normalized.pending) != 0 {
								err = fmt.Errorf("dicomjson: incomplete numeric byte-order unit")
							}
							return err
						})
					}), nil
				}
				uri, err := opts.PixelDataBulkDataURIFunc(elem.Tag(), elem.VR(), open)
				if err != nil {
					return Element{}, fmt.Errorf("dicomjson: offload Pixel Data: %w", err)
				}
				if err := budget.checkContext(); err != nil {
					return Element{}, err
				}
				if uri != "" {
					entry.BulkDataURI = uri
					return entry, nil
				}
				opts.PixelDataBulkDataURIFunc = nil
			}
			if opts.MaxMaterializedValueBytes > 0 && ref.Length > opts.MaxMaterializedValueBytes {
				return Element{}, fmt.Errorf("%w: deferred value %s has %d bytes, limit %d", ErrMaxMaterializedValueBytesExceeded, elem.Tag(), ref.Length, opts.MaxMaterializedValueBytes)
			}
			if opts.BulkDataURIFunc == nil && !elem.VR().IsStringLike() && !marshalsRawAsJSONValue(elem.VR()) {
				if err := budget.addInlineBinary(ref.Length); err != nil {
					return Element{}, err
				}
				// The replayed value below must not be charged a second time.
				opts.inlineAlreadyBudgeted = true
			}
			raw, err := materializeReference(opts.root, path, ref, budget)
			if err != nil {
				return Element{}, err
			}
			elem.Value = core.RawValue(raw)
			// The buffer was just read from the source: no one else holds it.
			freshRaw = true
		}
	}
	if authored, ok := elem.Value.(core.StringValue); ok {
		if err := valueencode.ValidateString(elem.VR(), authored); err != nil {
			return Element{}, fmt.Errorf("dicomjson: marshal %s: %w", elem.Tag(), err)
		}
	}

	if creator, ok := unknownPrivateCreator(obj, elem, opts.ByteOrder); ok {
		entry.VR = core.VRLO.String()
		entry.Value = []any{creator}
		return entry, nil
	}
	_, isSequence := elem.Value.(core.SequenceValue)
	switch {
	case elem.VR() == core.VRPN:
		values, err := obj.DecodeElementStrings(elem)
		if err != nil {
			return Element{}, err
		}
		entry.Value = personNamesToAny(values)
	case elem.VR() == core.VRSQ || elem.VR() == core.VRUN && isSequence:
		// A private sequence read in Implicit VR (or explicitly as UN, per
		// CP-246) stays UN in the object but was parsed as items; its items
		// are exported, so its VR is SQ.
		entry.VR = core.VRSQ.String()
		items, ok := obj.GetSequence(elem.Tag())
		if !ok {
			return Element{}, fmt.Errorf("dicomjson: marshal %s VR SQ: expected SequenceValue, got %T", elem.Tag(), elem.Value)
		}
		if err := budget.addSequenceItems(len(items)); err != nil {
			return Element{}, err
		}
		entry.Value = make([]any, 0, len(items))
		for index, item := range items {
			if err := budget.checkContext(); err != nil {
				return Element{}, err
			}
			nestedOpts := opts
			nestedOpts.path = append(append([]parser.BulkDataPathStep(nil), opts.path...), parser.BulkDataPathStep{Tag: elem.Tag(), ItemIndex: index})
			nested, err := marshalElementsContext(item, nestedOpts, budget, depth+1)
			if err != nil {
				return Element{}, err
			}
			entry.Value = append(entry.Value, nested)
		}
	case elem.VR().IsStringLike():
		values, err := obj.DecodeElementStrings(elem)
		if err != nil {
			return Element{}, err
		}
		entry.Value = stringsToAny(values)
	default:
		if fragments, ok := elem.Value.(core.FragmentSequence); ok {
			if err := budget.addSequenceItems(len(fragments.Fragments)); err != nil {
				return Element{}, err
			}
		}
		if elem.Tag() == core.TagPixelData && opts.PixelDataBulkDataURIFunc != nil {
			open, ok, err := pixelDataBulkReader(obj, elem, opts.ByteOrder, inlineByteOrderWidth(obj, elem, opts))
			if err != nil {
				return Element{}, err
			}
			if ok {
				uri, err := opts.PixelDataBulkDataURIFunc(elem.Tag(), elem.VR(), open)
				if err != nil {
					return Element{}, fmt.Errorf("dicomjson: offload Pixel Data: %w", err)
				}
				if uri != "" {
					entry.BulkDataURI = uri
					return entry, nil
				}
			}
		}
		raw, ok := elem.RawBytes()
		inlineBudgeted := opts.inlineAlreadyBudgeted
		if !ok {
			if opts.BulkDataURIFunc == nil && !marshalsRawAsJSONValue(elem.VR()) {
				if size, known, err := inlineValueFieldLength(elem); err != nil {
					return Element{}, err
				} else if known {
					if err := budget.addInlineBinary(size); err != nil {
						return Element{}, err
					}
					inlineBudgeted = true
				}
			}
			var err error
			raw, err = materializeRawValue(obj, elem, opts.ByteOrder)
			if err != nil {
				return Element{}, err
			}
			if err := budget.checkContext(); err != nil {
				return Element{}, err
			}
			freshRaw = true
		}
		switch elem.VR() {
		case core.VRAT:
			values, err := marshalAttributeTagsContext(elem.Tag(), raw, opts.ByteOrder, budget)
			if err != nil {
				return Element{}, err
			}
			entry.Value = values
			return entry, nil
		case core.VRFL, core.VRFD, core.VRSS, core.VRUS, core.VRSL, core.VRUL, core.VRSV, core.VRUV:
			values, err := marshalNumericValuesContext(elem.Tag(), elem.VR(), raw, opts.ByteOrder, budget)
			if err != nil {
				return Element{}, err
			}
			entry.Value = values
			return entry, nil
		}
		if len(raw) == 0 {
			return entry, nil
		}
		if opts.BulkDataURIFunc == nil && !inlineBudgeted {
			// Refuse an oversized value before converting it.
			if err := budget.addInlineBinary(int64(len(raw))); err != nil {
				return Element{}, err
			}
			inlineBudgeted = true
		}
		if width := inlineByteOrderWidth(obj, elem, opts); width > 1 {
			if freshRaw {
				valueencode.ReverseUnitsInPlace(raw, width)
			} else {
				// The object's own bytes are never modified.
				raw = valueencode.ReverseFixedWidth(raw, width)
			}
		}
		if opts.BulkDataURIFunc != nil {
			if uri := opts.BulkDataURIFunc(elem.Tag(), elem.VR(), raw); uri != "" {
				entry.BulkDataURI = uri
				return entry, nil
			}
		}
		if !inlineBudgeted {
			if err := budget.addInlineBinary(int64(len(raw))); err != nil {
				return Element{}, err
			}
		}
		entry.InlineBinary = base64.StdEncoding.EncodeToString(raw)
	}

	return entry, nil
}

// inlineByteOrderWidth returns the unit by which the value of elem is
// reversed into the Little Endian order of InlineBinary and of the bulk data
// callbacks, or 1 when its bytes are kept: with SourceByteOrderInlineBinary,
// for a Little Endian object, for encapsulated Pixel Data and for VRs other
// than OW, OF, OD, OL and OV.
func inlineByteOrderWidth(obj *object.Object, elem core.Element, opts Options) int {
	if opts.SourceByteOrderInlineBinary || opts.ByteOrder != binary.BigEndian {
		return 1
	}
	if _, encapsulated := elem.Value.(core.FragmentSequence); encapsulated {
		return 1
	}
	return littleEndianInlineWidth(obj, elem, opts.ByteOrder)
}

// writeLittleEndianUnits streams what write produces to w with the bytes of
// every width-sized unit reversed, in bounded chunks, and a trailing partial
// unit unchanged. A width of 1 or less passes the bytes through.
func writeLittleEndianUnits(w io.Writer, width int, write func(io.Writer) error) error {
	if width <= 1 {
		return write(w)
	}
	reversing := &referenceNormalizingWriter{writer: w, width: width}
	if err := write(reversing); err != nil {
		return err
	}
	if len(reversing.pending) == 0 {
		return nil
	}
	written, err := w.Write(reversing.pending)
	if err == nil && written != len(reversing.pending) {
		err = io.ErrShortWrite
	}
	return err
}

// littleEndianInlineWidth returns the unit whose bytes are reversed to give
// the Little Endian form of a Big Endian OW, OF, OD, OL or OV value of obj, or
// 1 for any other VR. Bits Allocated is read in order, the byte order the
// values are interpreted in, which may differ from the object's own.
func littleEndianInlineWidth(obj *object.Object, elem core.Element, order binary.ByteOrder) int {
	bits := int64(0)
	if elem.VR() == core.VROW && elem.Tag() == core.TagPixelData {
		// An item never inherits its parent's Bits Allocated.
		if allocated, ok := obj.Get(valueencode.BitsAllocatedTag); ok {
			bits = valueencode.BitsAllocated(allocated, order)
		}
	}
	return valueencode.LittleEndianUnitWidth(elem.Tag(), elem.VR(), bits)
}

// unknownPrivateCreator resolves a Private Creator whose VR was not known
// when it was read (Implicit VR without a private dictionary, or UN). PS3.5
// 7.8.1 fixes a creator's VR as LO, so its value field is read as LO in the
// Default Character Repertoire, as dictionary.PrivateCreatorID requires. A
// value that is not a valid creator (control characters, NUL, non-ASCII,
// backslash) stays UN and InlineBinary.
func unknownPrivateCreator(obj *object.Object, elem core.Element, order binary.ByteOrder) (string, bool) {
	if elem.VR() != core.VRUN || !dictionary.IsPrivateCreatorTag(elem.Tag()) {
		return "", false
	}
	raw, ok := elem.RawBytes()
	if !ok {
		if elem.Value != nil || !elem.Header.HasLength() || elem.EncodedLength() > 64 {
			return "", false
		}
		var err error
		if raw, err = materializeRawValue(obj, elem, order); err != nil {
			return "", false
		}
	}
	creator, err := dictionary.PrivateCreatorID(string(raw))
	if err != nil {
		return "", false
	}
	return creator, true
}

func marshalsRawAsJSONValue(vr core.VR) bool {
	switch vr {
	case core.VRAT, core.VRFL, core.VRFD, core.VRSS, core.VRUS, core.VRSL, core.VRUL, core.VRSV, core.VRUV:
		return true
	default:
		return false
	}
}

func inlineValueFieldLength(elem core.Element) (int64, bool, error) {
	switch value := elem.Value.(type) {
	case core.FragmentSequence:
		if len(value.OffsetTable)%4 != 0 {
			return 0, false, fmt.Errorf("dicomjson: Basic Offset Table length %d is not a multiple of 4", len(value.OffsetTable))
		}
		total := int64(16 + len(value.OffsetTable)) // Basic Offset Table item and sequence delimiter.
		for _, fragment := range value.Fragments {
			fragmentLength := int64(len(fragment))
			if fragmentLength%2 != 0 {
				fragmentLength++
			}
			if fragmentLength > math.MaxUint32 {
				return 0, false, fmt.Errorf("dicomjson: fragment length %d exceeds uint32", fragmentLength)
			}
			if total > math.MaxInt64-8-fragmentLength {
				return 0, false, fmt.Errorf("dicomjson: encapsulated Pixel Data Value Field length overflows int64")
			}
			total += 8 + fragmentLength
		}
		return total, true, nil
	case nil:
		if elem.Header.HasLength() && elem.EncodedLength() != core.UndefinedLength {
			return int64(elem.EncodedLength()), true, nil
		}
	}
	return 0, false, nil
}

// pixelDataBulkReader opens the Pixel Data value for PixelDataBulkDataURIFunc,
// with native values reversed by width into Little Endian while they stream.
func pixelDataBulkReader(obj *object.Object, elem core.Element, order binary.ByteOrder, width int) (func() (io.ReadCloser, error), bool, error) {
	if raw, ok := elem.RawBytes(); ok {
		if width > 1 {
			return func() (io.ReadCloser, error) {
				return streamingValueReader(func(w io.Writer) error {
					return writeLittleEndianUnits(w, width, func(w io.Writer) error {
						_, err := w.Write(raw)
						return err
					})
				}), nil
			}, true, nil
		}
		return func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(raw)), nil
		}, true, nil
	}
	switch value := elem.Value.(type) {
	case core.FragmentSequence:
		if len(value.OffsetTable)%4 != 0 {
			return nil, false, fmt.Errorf("dicomjson: Basic Offset Table length %d is not a multiple of 4", len(value.OffsetTable))
		}
		return func() (io.ReadCloser, error) {
			return streamingValueReader(func(w io.Writer) error {
				return writeFragmentSequenceValueField(w, value, order)
			}), nil
		}, true, nil
	case nil:
		return func() (io.ReadCloser, error) {
			return streamingValueReader(func(w io.Writer) error {
				return writeLittleEndianUnits(w, width, func(w io.Writer) error {
					_, err := obj.CopyValueTo(elem.Tag(), w)
					return err
				})
			}), nil
		}, true, nil
	default:
		return nil, false, nil
	}
}

func streamingValueReader(write func(io.Writer) error) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		writer.CloseWithError(write(writer))
	}()
	return reader
}

// materializeReference allocates once only after all declared-size limits pass.
func materializeReference(root *object.Object, path []parser.BulkDataPathStep, ref object.BulkDataReference, budget *conversionBudget) ([]byte, error) {
	if err := budget.checkContext(); err != nil {
		return nil, err
	}
	if ref.Length < 0 || uint64(ref.Length) > uint64(^uint(0)>>1) {
		return nil, fmt.Errorf("dicomjson: deferred value is too large to materialize")
	}
	data := make([]byte, int(ref.Length))
	writer := &referenceSliceWriter{data: data}
	n, err := root.CopyBulkDataValueTo(budget.ctx, path, writer)
	if err != nil {
		return nil, fmt.Errorf("dicomjson: marshal deferred %s: %w", ref.Tag, err)
	}
	if n != ref.Length || writer.offset != len(data) {
		return nil, fmt.Errorf("dicomjson: deferred source length mismatch")
	}
	if ref.NormalizationWidth > 1 {
		width := ref.NormalizationWidth
		if width > 1 && len(data)%width == 0 {
			for start := 0; start < len(data); start += width {
				for a, b := start, start+width-1; a < b; a, b = a+1, b-1 {
					data[a], data[b] = data[b], data[a]
				}
			}
		}
	}
	if err := budget.checkContext(); err != nil {
		return nil, err
	}
	return data, nil
}

type referenceSliceWriter struct {
	data   []byte
	offset int
}

// A source replay can supply arbitrarily sized writes. Split them before
// copying/reversing, and retain at most one partial numeric unit across writes.
type referenceNormalizingWriter struct {
	writer  io.Writer
	width   int
	pending []byte
}

func (w *referenceNormalizingWriter) Write(p []byte) (int, error) {
	if w.width <= 1 {
		return w.writer.Write(p)
	}
	total := len(p)
	for len(p) > 0 {
		n := min(len(p), 32<<10)
		data := append(w.pending, p[:n]...)
		whole := len(data) - len(data)%w.width
		if whole > 0 {
			encoded := valueencode.ReverseFixedWidth(data[:whole], w.width)
			written, err := w.writer.Write(encoded)
			if err != nil {
				return 0, err
			}
			if written != len(encoded) {
				return 0, io.ErrShortWrite
			}
		}
		w.pending = append([]byte(nil), data[whole:]...)
		p = p[n:]
	}
	return total, nil
}

func (w *referenceSliceWriter) Write(p []byte) (int, error) {
	if len(p) > len(w.data)-w.offset {
		return 0, io.ErrShortBuffer
	}
	n := copy(w.data[w.offset:], p)
	w.offset += n
	return n, nil
}

func materializeRawValue(obj *object.Object, elem core.Element, order binary.ByteOrder) ([]byte, error) {
	switch value := elem.Value.(type) {
	case core.Uint16Value, core.Int16Value, core.Uint32Value, core.Int32Value,
		core.Uint64Value, core.Int64Value, core.Float32Value, core.Float64Value, core.TagValue:
		if err := valueencode.ValidateNumeric(elem.VR(), value); err != nil {
			return nil, fmt.Errorf("dicomjson: marshal %s: %w", elem.Tag(), err)
		}
		raw, _, err := valueencode.Numeric(value, order)
		return raw, err
	case core.FragmentSequence:
		if elem.Tag() != core.TagPixelData {
			return nil, fmt.Errorf("dicomjson: marshal %s: FragmentSequence is only valid for Pixel Data", elem.Tag())
		}
		if elem.VR() != core.VROB && elem.VR() != core.VROW {
			return nil, fmt.Errorf("dicomjson: marshal %s: FragmentSequence requires OB or OW VR", elem.Tag())
		}
		return fragmentSequenceValueField(value, order)
	case nil:
		if elem.Header.HasLength() && elem.EncodedLength() == 0 {
			return nil, nil
		}
		var valueField bytes.Buffer
		if _, err := obj.CopyValueTo(elem.Tag(), &valueField); err != nil {
			return nil, fmt.Errorf("dicomjson: marshal deferred %s: %w", elem.Tag(), err)
		}
		return valueField.Bytes(), nil
	default:
		if length, ok := elem.CalculatedLength(); ok && length == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("dicomjson: marshal %s VR %s: unsupported value type %T", elem.Tag(), elem.VR(), elem.Value)
	}
}

func fragmentSequenceValueField(value core.FragmentSequence, order binary.ByteOrder) ([]byte, error) {
	if len(value.OffsetTable)%4 != 0 {
		return nil, fmt.Errorf("dicomjson: Basic Offset Table length %d is not a multiple of 4", len(value.OffsetTable))
	}
	var out bytes.Buffer
	if err := writeFragmentSequenceValueField(&out, value, order); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func writeFragmentSequenceValueField(out io.Writer, value core.FragmentSequence, order binary.ByteOrder) error {
	if err := writeFragmentItem(out, order, value.OffsetTable, false); err != nil {
		return err
	}
	for _, fragment := range value.Fragments {
		if err := writeFragmentItem(out, order, fragment, true); err != nil {
			return err
		}
	}
	return writeItemHeader(out, order, core.TagSequenceDelimitationItem, 0)
}

func writeFragmentItem(out io.Writer, order binary.ByteOrder, data []byte, pad bool) error {
	length := uint64(len(data))
	if pad && length%2 == 1 {
		length++
	}
	if length > uint64(^uint32(0)) {
		return fmt.Errorf("dicomjson: fragment length %d exceeds uint32", length)
	}
	if err := writeItemHeader(out, order, core.TagItem, uint32(length)); err != nil {
		return err
	}
	if _, err := out.Write(data); err != nil {
		return err
	}
	if pad && len(data)%2 == 1 {
		if _, err := out.Write([]byte{0}); err != nil {
			return err
		}
	}
	return nil
}

func writeItemHeader(out io.Writer, order binary.ByteOrder, tag core.Tag, length uint32) error {
	var header [8]byte
	order.PutUint16(header[0:2], tag.Group)
	order.PutUint16(header[2:4], tag.Element)
	order.PutUint32(header[4:8], length)
	_, err := out.Write(header[:])
	return err
}

func unmarshalDataSetContext(data []byte, dict dictionary.DataDictionary, opts UnmarshalOptions, budget *conversionBudget) (core.DataSet, error) {
	if err := budget.checkContext(); err != nil {
		return core.DataSet{}, err
	}
	reader := json.NewDecoder(bytes.NewReader(data))
	raw, err := decodeDatasetObject[json.RawMessage](reader, budget, "$")
	if err != nil {
		return core.DataSet{}, fmt.Errorf("dicomjson: decode dataset JSON: %w", err)
	}
	if err := ensureJSONEOF(reader); err != nil {
		return core.DataSet{}, fmt.Errorf("dicomjson: decode dataset JSON: %w", err)
	}
	return unmarshalDataSetMapContext(raw, "", dict, opts, budget, 0)
}

func unmarshalDataSetMapContext(raw map[string]json.RawMessage, path string, dict dictionary.DataDictionary, opts UnmarshalOptions, budget *conversionBudget, depth int) (core.DataSet, error) {
	if err := budget.enterDataSet(depth, len(raw)); err != nil {
		return core.DataSet{}, err
	}
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	elements := make([]core.Element, 0, len(keys))
	var inline []int
	for _, tagStr := range keys {
		if err := budget.checkContext(); err != nil {
			return core.DataSet{}, err
		}
		tagPath := joinJSONPath(path, tagStr)
		tag, err := parseJSONTag(tagStr, tagPath)
		if err != nil {
			return core.DataSet{}, err
		}
		elem, fromInline, err := unmarshalElementContext(tag, raw[tagStr], tagPath, dict, opts, budget, depth)
		if err != nil {
			return core.DataSet{}, err
		}
		if fromInline {
			inline = append(inline, len(elements))
		}
		elements = append(elements, elem)
	}
	if len(inline) > 0 && !opts.SourceByteOrderInlineBinary && opts.ByteOrder == binary.BigEndian {
		toInlineByteOrder(elements, inline, opts.ByteOrder)
	}
	return core.DataSet{Elements: elements}, nil
}

// toInlineByteOrder reverses the Little Endian InlineBinary values of
// elements at the given indexes into the Big Endian order of the data set,
// by the units Marshal reversed them in. The Bits Allocated of OW Pixel Data
// is the one of the same data set or item.
func toInlineByteOrder(elements []core.Element, inline []int, order binary.ByteOrder) {
	bits := int64(0)
	for _, elem := range elements {
		if elem.Tag() == valueencode.BitsAllocatedTag {
			bits = valueencode.BitsAllocated(elem, order)
			break
		}
	}
	for _, index := range inline {
		elem := &elements[index]
		raw, ok := elem.RawBytes()
		if !ok {
			continue
		}
		if width := valueencode.LittleEndianUnitWidth(elem.Tag(), elem.VR(), bits); width > 1 {
			elem.Value = core.RawValue(valueencode.ReverseFixedWidth(raw, width))
		}
	}
}

// unmarshalElementContext decodes one element; fromInline reports a raw
// value decoded from InlineBinary.
func unmarshalElementContext(tag core.Tag, raw json.RawMessage, path string, dict dictionary.DataDictionary, opts UnmarshalOptions, budget *conversionBudget, depth int) (core.Element, bool, error) {
	fromInline := false
	elem, err := unmarshalElementValueContext(tag, raw, path, dict, opts, budget, depth, &fromInline)
	if err != nil {
		return core.Element{}, false, err
	}
	return elem, fromInline, nil
}

func unmarshalElementValueContext(tag core.Tag, raw json.RawMessage, path string, dict dictionary.DataDictionary, opts UnmarshalOptions, budget *conversionBudget, depth int, fromInline *bool) (core.Element, error) {
	if err := budget.checkContext(); err != nil {
		return core.Element{}, err
	}
	var fields map[string]json.RawMessage
	if err := decodeJSON(raw, &fields); err != nil {
		return core.Element{}, pathError(path, "decode element JSON: %v", err)
	}

	for key := range fields {
		switch key {
		case "vr", "Value", "InlineBinary", "BulkDataURI":
		default:
			return core.Element{}, pathError(joinJSONPath(path, key), "unrecognized data element field %q", key)
		}
	}

	vrRaw, ok := fields["vr"]
	if !ok {
		return core.Element{}, pathError(joinJSONPath(path, "vr"), "missing VR field")
	}
	var vrText string
	if err := decodeJSON(vrRaw, &vrText); err != nil {
		return core.Element{}, pathError(joinJSONPath(path, "vr"), "decode VR: %v", err)
	}
	vr, err := core.ParseVR(vrText)
	if err != nil {
		return core.Element{}, pathError(joinJSONPath(path, "vr"), "%v", err)
	}

	_, hasValue := fields["Value"]
	_, hasInline := fields["InlineBinary"]
	_, hasBulk := fields["BulkDataURI"]
	switch {
	case hasValue && hasInline:
		return core.Element{}, pathError(path, "\"Value\" conflicts with \"InlineBinary\"")
	case hasValue && hasBulk:
		return core.Element{}, pathError(path, "\"Value\" conflicts with \"BulkDataURI\"")
	case hasInline && hasBulk:
		return core.Element{}, pathError(path, "\"InlineBinary\" conflicts with \"BulkDataURI\"")
	}

	elem := core.Element{
		Header: core.ElementHeader{
			Tag: tag,
			VR:  vr,
		},
	}

	switch {
	case hasBulk:
		if vr == core.VRSQ {
			return core.Element{}, pathError(joinJSONPath(path, "BulkDataURI"), "BulkDataURI is not valid for SQ")
		}
		var uri string
		if err := decodeJSON(fields["BulkDataURI"], &uri); err != nil {
			return core.Element{}, pathError(joinJSONPath(path, "BulkDataURI"), "decode BulkDataURI: %v", err)
		}
		elem.Value = core.BulkDataValue{URI: uri}
		return elem, nil
	case hasInline:
		if vr == core.VRSQ {
			return core.Element{}, pathError(joinJSONPath(path, "InlineBinary"), "InlineBinary is not valid for SQ")
		}
		var encoded string
		if err := decodeJSON(fields["InlineBinary"], &encoded); err != nil {
			return core.Element{}, pathError(joinJSONPath(path, "InlineBinary"), "decode InlineBinary: %v", err)
		}
		decodedLength := base64DecodedLength(encoded)
		if err := budget.addInlineBinary(int64(decodedLength)); err != nil {
			return core.Element{}, fmt.Errorf("dicomjson: %w at %s", err, joinJSONPath(path, "InlineBinary"))
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return core.Element{}, pathError(joinJSONPath(path, "InlineBinary"), "inline binary data is not valid base64")
		}
		if len(data) != decodedLength {
			return core.Element{}, pathError(joinJSONPath(path, "InlineBinary"), "inline binary decoded length %d differs from expected %d", len(data), decodedLength)
		}
		if tag == core.TagPixelData && !opts.TransferSyntax.Encapsulated &&
			(bytes.HasPrefix(data, []byte{0xfe, 0xff, 0x00, 0xe0}) || bytes.HasPrefix(data, []byte{0xff, 0xfe, 0xe0, 0x00})) {
			return core.Element{}, fmt.Errorf("dicomjson: decode Pixel Data at %s: %w: encapsulated InlineBinary requires an encapsulated transfer syntax", joinJSONPath(path, "InlineBinary"), object.ErrPixelDataEncodingMismatch)
		}
		if tag == core.TagPixelData && opts.TransferSyntax.Encapsulated {
			fragments, err := parseEncapsulatedPixelDataValue(data, opts.TransferSyntax, dict, budget)
			if err != nil {
				return core.Element{}, fmt.Errorf("dicomjson: decode encapsulated Pixel Data at %s: %w", joinJSONPath(path, "InlineBinary"), err)
			}
			elem.Header.Length = core.UndefinedLength
			elem.Header.LengthSet = true
			elem.Value = fragments
		} else {
			elem.Value = core.RawValue(core.CloneBytes(data))
			*fromInline = true
		}
		return elem, nil
	case !hasValue:
		elem.Value = emptyValueForVR(vr)
		return elem, nil
	}

	valuePath := joinJSONPath(path, "Value")
	switch vr {
	case core.VRSQ:
		value, err := unmarshalSequenceContext(fields["Value"], path, valuePath, dict, opts, budget, depth)
		if err != nil {
			return core.Element{}, err
		}
		elem.Value = value
	case core.VRPN:
		value, err := unmarshalPersonNamesContext(fields["Value"], valuePath, vr, budget)
		if err != nil {
			return core.Element{}, err
		}
		elem.Value = value
	case core.VRDS, core.VRIS:
		value, err := unmarshalNumberStringsContext(fields["Value"], valuePath, vr, budget)
		if err != nil {
			return core.Element{}, err
		}
		elem.Value = value
	case core.VRAT:
		value, err := unmarshalAttributeTagsContext(fields["Value"], valuePath, vr, opts.ByteOrder, budget)
		if err != nil {
			return core.Element{}, err
		}
		elem.Value = value
	case core.VRUN:
		return core.Element{}, valueTypePathError(valuePath, vr, fields["Value"], "can't parse JSON Value in UN; use InlineBinary or BulkDataURI")
	case core.VRFL, core.VROF, core.VRFD, core.VROD, core.VRSS, core.VRUS, core.VROW, core.VRSL, core.VRUL, core.VROL, core.VRSV, core.VRUV, core.VROV, core.VROB:
		value, err := encodeNumericValuesContext(vr, fields["Value"], valuePath, opts.ByteOrder, budget)
		if err != nil {
			return core.Element{}, err
		}
		elem.Value = core.RawValue(value)
	case core.VRAE, core.VRAS, core.VRCS, core.VRDA, core.VRDT, core.VRLO, core.VRLT, core.VRSH, core.VRST, core.VRUT, core.VRUR, core.VRTM, core.VRUC, core.VRUI:
		value, err := unmarshalStringValuesContext(fields["Value"], valuePath, vr, budget)
		if err != nil {
			return core.Element{}, err
		}
		elem.Value = value
	default:
		return core.Element{}, valueTypePathError(valuePath, vr, fields["Value"], "unsupported VR %s", vr)
	}

	return elem, nil
}

func unmarshalStringValuesContext(raw json.RawMessage, path string, vr core.VR, budget *conversionBudget) (core.StringValue, error) {
	items, err := decodeValueArray(raw, path, vr)
	if err != nil {
		return nil, err
	}
	delimited := vr.UsesTextValueDelimiter()
	if !delimited && len(items) > 1 {
		// LT, ST, UT and UR are single-valued: a backslash inside the one value
		// is literal, so several entries cannot be represented.
		return nil, pathError(path, "VR %s holds one value, got %d", vr, len(items))
	}
	values := make([]string, len(items))
	for i := range items {
		if err := budget.checkContext(); err != nil {
			return nil, err
		}
		itemPath := indexJSONPath(path, i)
		value, err := decodeNullableStringValue(items[i], itemPath, vr)
		if err != nil {
			return nil, err
		}
		if delimited && strings.Contains(value, "\\") {
			// Each JSON entry is one value; a backslash would split it into
			// several values when the data set is encoded.
			return nil, pathError(itemPath, "VR %s value contains the backslash value delimiter", vr)
		}
		values[i] = value
	}
	return core.StringValue(values), nil
}

func unmarshalNumberStringsContext(raw json.RawMessage, path string, vr core.VR, budget *conversionBudget) (core.StringValue, error) {
	items, err := decodeValueArray(raw, path, vr)
	if err != nil {
		return nil, err
	}
	values := make([]string, len(items))
	for i := range items {
		if err := budget.checkContext(); err != nil {
			return nil, err
		}
		itemPath := indexJSONPath(path, i)
		token, err := decodeNullableNumberOrStringValue(items[i], itemPath, vr)
		if err != nil {
			return nil, err
		}
		if !isJSONNull(items[i]) {
			if err := validateNumberString(token, vr); err != nil {
				return nil, valueTypePathError(itemPath, vr, items[i], "%v", err)
			}
		}
		values[i] = token
	}
	return core.StringValue(values), nil
}

func validateNumberString(value string, vr core.VR) error {
	return valueencode.ValidateNumberString(vr, value)
}

func unmarshalPersonNamesContext(raw json.RawMessage, path string, vr core.VR, budget *conversionBudget) (core.StringValue, error) {
	items, err := decodeValueArray(raw, path, vr)
	if err != nil {
		return nil, err
	}
	values := make([]string, len(items))
	for i := range items {
		if err := budget.checkContext(); err != nil {
			return nil, err
		}
		itemPath := indexJSONPath(path, i)
		if isJSONNull(items[i]) {
			continue
		}
		if jsonValueType(items[i]) != "object" {
			return nil, valueTypePathError(itemPath, vr, items[i], "expected person name object")
		}
		var pn PersonNameComponents
		if err := decodeJSON(items[i], &pn); err != nil {
			return nil, valueTypePathError(itemPath, vr, items[i], "decode person name: %v", err)
		}
		if strings.Contains(pn.Alphabetic+pn.Ideographic+pn.Phonetic, "\\") {
			return nil, pathError(itemPath, "VR PN value contains the backslash value delimiter")
		}
		values[i] = joinPersonName(pn)
	}
	return core.StringValue(values), nil
}

func unmarshalSequenceContext(raw json.RawMessage, path, valuePath string, dict dictionary.DataDictionary, opts UnmarshalOptions, budget *conversionBudget, depth int) (core.SequenceValue, error) {
	items, err := decodeValueArray(raw, valuePath, core.VRSQ)
	if err != nil {
		return core.SequenceValue{}, err
	}
	if err := budget.addSequenceItems(len(items)); err != nil {
		return core.SequenceValue{}, err
	}
	seq := core.SequenceValue{Items: make([]core.DataSet, 0, len(items))}
	for i := range items {
		if err := budget.checkContext(); err != nil {
			return core.SequenceValue{}, err
		}
		itemPath := indexJSONPath(path, i)
		var dataset map[string]json.RawMessage
		if err := decodeJSON(items[i], &dataset); err != nil {
			return core.SequenceValue{}, pathError(itemPath, "decode sequence item: %v", err)
		}
		ds, err := unmarshalDataSetMapContext(dataset, itemPath, dict, opts, budget, depth+1)
		if err != nil {
			return core.SequenceValue{}, err
		}
		seq.Items = append(seq.Items, ds)
	}
	return seq, nil
}

func unmarshalAttributeTagsContext(raw json.RawMessage, path string, vr core.VR, order binary.ByteOrder, budget *conversionBudget) (core.RawValue, error) {
	items, err := decodeValueArray(raw, path, vr)
	if err != nil {
		return nil, err
	}
	data := make([]byte, 0, len(items)*4)
	for i := range items {
		if err := budget.checkContext(); err != nil {
			return nil, err
		}
		itemPath := indexJSONPath(path, i)
		value, err := decodeRequiredStringValue(items[i], itemPath, vr)
		if err != nil {
			return nil, err
		}
		tag, err := parseJSONTag(value, itemPath)
		if err != nil {
			return nil, err
		}
		var buf [4]byte
		order.PutUint16(buf[0:2], tag.Group)
		order.PutUint16(buf[2:4], tag.Element)
		data = append(data, buf[:]...)
	}
	return core.RawValue(data), nil
}

func encodeNumericValuesContext(vr core.VR, raw json.RawMessage, path string, order binary.ByteOrder, budget *conversionBudget) ([]byte, error) {
	items, err := decodeValueArray(raw, path, vr)
	if err != nil {
		return nil, err
	}
	data := make([]byte, 0, len(items)*8)
	for i := range items {
		if err := budget.checkContext(); err != nil {
			return nil, err
		}
		itemPath := indexJSONPath(path, i)
		switch vr {
		case core.VRFL, core.VROF:
			v, err := parseFloat32(items[i], itemPath, vr)
			if err != nil {
				return nil, err
			}
			var buf [4]byte
			order.PutUint32(buf[:], math.Float32bits(v))
			data = append(data, buf[:]...)
		case core.VRFD, core.VROD:
			v, err := parseFloat64(items[i], itemPath, vr)
			if err != nil {
				return nil, err
			}
			var buf [8]byte
			order.PutUint64(buf[:], math.Float64bits(v))
			data = append(data, buf[:]...)
		case core.VRSS:
			v, err := parseInt(items[i], itemPath, 16, vr)
			if err != nil {
				return nil, err
			}
			var buf [2]byte
			order.PutUint16(buf[:], uint16(int16(v)))
			data = append(data, buf[:]...)
		case core.VRUS, core.VROW:
			v, err := parseUint(items[i], itemPath, 16, vr)
			if err != nil {
				return nil, err
			}
			var buf [2]byte
			order.PutUint16(buf[:], uint16(v))
			data = append(data, buf[:]...)
		case core.VRSL:
			v, err := parseInt(items[i], itemPath, 32, vr)
			if err != nil {
				return nil, err
			}
			var buf [4]byte
			order.PutUint32(buf[:], uint32(int32(v)))
			data = append(data, buf[:]...)
		case core.VRUL, core.VROL:
			v, err := parseUint(items[i], itemPath, 32, vr)
			if err != nil {
				return nil, err
			}
			var buf [4]byte
			order.PutUint32(buf[:], uint32(v))
			data = append(data, buf[:]...)
		case core.VRSV:
			v, err := parseInt(items[i], itemPath, 64, vr)
			if err != nil {
				return nil, err
			}
			var buf [8]byte
			order.PutUint64(buf[:], uint64(v))
			data = append(data, buf[:]...)
		case core.VRUV, core.VROV:
			v, err := parseUint(items[i], itemPath, 64, vr)
			if err != nil {
				return nil, err
			}
			var buf [8]byte
			order.PutUint64(buf[:], v)
			data = append(data, buf[:]...)
		case core.VROB:
			v, err := parseUint(items[i], itemPath, 8, vr)
			if err != nil {
				return nil, err
			}
			data = append(data, byte(v))
		default:
			return nil, valueTypePathError(path, vr, raw, "unsupported numeric VR %s", vr)
		}
	}
	return data, nil
}

func emptyValueForVR(vr core.VR) core.Value {
	switch vr {
	case core.VRSQ:
		return core.SequenceValue{}
	case core.VRPN, core.VRAE, core.VRAS, core.VRCS, core.VRDA, core.VRDS, core.VRDT, core.VRIS, core.VRLO, core.VRLT, core.VRSH, core.VRST, core.VRTM, core.VRUC, core.VRUI, core.VRUR, core.VRUT:
		return core.StringValue(nil)
	default:
		return core.RawValue(nil)
	}
}

func stringsToAny(values []string) []any {
	if len(values) == 0 {
		return nil
	}
	if len(values) == 1 && values[0] == "" {
		return nil
	}
	out := make([]any, 0, len(values))
	for _, value := range values {
		if value == "" {
			out = append(out, nil)
			continue
		}
		out = append(out, value)
	}
	return out
}

func personNamesToAny(values []string) []any {
	if len(values) == 0 {
		return nil
	}
	if len(values) == 1 && values[0] == "" {
		return nil
	}
	out := make([]any, 0, len(values))
	for _, value := range values {
		if value == "" {
			out = append(out, nil)
			continue
		}
		out = append(out, splitPersonName(value))
	}
	return out
}

func splitPersonName(value string) PersonNameComponents {
	parts := strings.SplitN(value, "=", 3)
	pn := PersonNameComponents{}
	if len(parts) > 0 && parts[0] != "" {
		pn.Alphabetic = parts[0]
	}
	if len(parts) > 1 && parts[1] != "" {
		pn.Ideographic = parts[1]
	}
	if len(parts) > 2 && parts[2] != "" {
		pn.Phonetic = parts[2]
	}
	return pn
}

func joinPersonName(pn PersonNameComponents) string {
	switch {
	case pn.Ideographic == "" && pn.Phonetic == "":
		return pn.Alphabetic
	case pn.Ideographic != "" && pn.Phonetic == "":
		return pn.Alphabetic + "=" + pn.Ideographic
	case pn.Ideographic == "" && pn.Phonetic != "":
		return pn.Alphabetic + "==" + pn.Phonetic
	default:
		return pn.Alphabetic + "=" + pn.Ideographic + "=" + pn.Phonetic
	}
}

func marshalAttributeTagsContext(tag core.Tag, raw []byte, order binary.ByteOrder, budget *conversionBudget) ([]any, error) {
	if len(raw)%4 != 0 {
		return nil, invalidRawValueLengthError(tag, core.VRAT, len(raw), 4)
	}
	values := make([]any, 0, len(raw)/4)
	for len(raw) > 0 {
		if err := budget.checkContext(); err != nil {
			return nil, err
		}
		value := core.NewTag(
			order.Uint16(raw[0:2]),
			order.Uint16(raw[2:4]),
		)
		values = append(values, value.HexString())
		raw = raw[4:]
	}
	return values, nil
}

func marshalNumericValuesContext(tag core.Tag, vr core.VR, raw []byte, order binary.ByteOrder, budget *conversionBudget) ([]any, error) {
	width := numericValueWidth(vr)
	if width == 0 {
		return nil, fmt.Errorf("dicomjson: cannot marshal tag %s VR %s as JSON Value", tag, vr)
	}
	if len(raw)%width != 0 {
		return nil, invalidRawValueLengthError(tag, vr, len(raw), width)
	}

	values := make([]any, 0, len(raw)/width)
	for len(raw) > 0 {
		if err := budget.checkContext(); err != nil {
			return nil, err
		}
		switch vr {
		case core.VRFL:
			values = append(values, jsonFloatValue(float64(math.Float32frombits(order.Uint32(raw[:4])))))
		case core.VRFD:
			values = append(values, jsonFloatValue(math.Float64frombits(order.Uint64(raw[:8]))))
		case core.VRSS:
			values = append(values, int64(int16(order.Uint16(raw[:2]))))
		case core.VRUS:
			values = append(values, uint64(order.Uint16(raw[:2])))
		case core.VRSL:
			values = append(values, int64(int32(order.Uint32(raw[:4]))))
		case core.VRUL:
			values = append(values, uint64(order.Uint32(raw[:4])))
		case core.VRSV:
			values = append(values, int64(order.Uint64(raw[:8])))
		case core.VRUV:
			values = append(values, order.Uint64(raw[:8]))
		}
		raw = raw[width:]
	}
	return values, nil
}

func jsonFloatValue(value float64) any {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "Infinity"
	case math.IsInf(value, -1):
		return "-Infinity"
	default:
		return value
	}
}

func numericValueWidth(vr core.VR) int {
	switch vr {
	case core.VRSS, core.VRUS:
		return 2
	case core.VRFL, core.VRSL, core.VRUL:
		return 4
	case core.VRFD, core.VRSV, core.VRUV:
		return 8
	default:
		return 0
	}
}

func invalidRawValueLengthError(tag core.Tag, vr core.VR, got, width int) error {
	return fmt.Errorf("dicomjson: cannot marshal tag %s VR %s: raw value length %d is not a multiple of %d", tag, vr, got, width)
}

func parseJSONTag(tagStr, path string) (core.Tag, error) {
	trimmed := strings.TrimSpace(tagStr)
	if len(trimmed) != 8 || strings.ContainsRune(trimmed, ',') {
		return core.Tag{}, pathError(path, "invalid tag %q", tagStr)
	}
	tag, err := core.ParseTag(trimmed)
	if err != nil {
		return core.Tag{}, pathError(path, "%v", err)
	}
	return tag, nil
}

func decodeValueArray(raw json.RawMessage, path string, vr core.VR) ([]json.RawMessage, error) {
	var items []json.RawMessage
	if err := decodeJSON(raw, &items); err != nil {
		return nil, valueTypePathError(path, vr, raw, "expected JSON array: %v", err)
	}
	return items, nil
}

func decodeNullableStringValue(raw json.RawMessage, path string, vr core.VR) (string, error) {
	if isJSONNull(raw) {
		return "", nil
	}
	return decodeRequiredStringValue(raw, path, vr)
}

func decodeRequiredString(raw json.RawMessage, path string) (string, error) {
	var value string
	if err := decodeJSON(raw, &value); err != nil {
		return "", pathError(path, "expected string: %v", err)
	}
	return value, nil
}

func decodeRequiredStringValue(raw json.RawMessage, path string, vr core.VR) (string, error) {
	if jsonValueType(raw) != "string" {
		return "", valueTypePathError(path, vr, raw, "expected string")
	}
	var value string
	if err := decodeJSON(raw, &value); err != nil {
		return "", valueTypePathError(path, vr, raw, "expected string: %v", err)
	}
	return value, nil
}

func decodeNullableNumberOrStringValue(raw json.RawMessage, path string, vr core.VR) (string, error) {
	if isJSONNull(raw) {
		return "", nil
	}
	return decodeRequiredNumberOrStringValue(raw, path, vr)
}

func decodeRequiredNumberOrStringValue(raw json.RawMessage, path string, vr core.VR) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", valueTypePathError(path, vr, raw, "empty JSON value")
	}
	if trimmed[0] == '"' {
		return decodeRequiredString(raw, path)
	}
	valueType := jsonValueType(raw)
	if valueType != "number" {
		return "", valueTypePathError(path, vr, raw, "expected number or string")
	}
	var number json.Number
	if err := decodeJSON(raw, &number); err != nil {
		return "", valueTypePathError(path, vr, raw, "expected number or string: %v", err)
	}
	return number.String(), nil
}

func parseInt(raw json.RawMessage, path string, bits int, vr core.VR) (int64, error) {
	token, err := decodeRequiredNumberOrStringValue(raw, path, vr)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseInt(token, 10, bits)
	if err != nil {
		return 0, valueTypePathError(path, vr, raw, "parse signed integer: %v", err)
	}
	return value, nil
}

func parseUint(raw json.RawMessage, path string, bits int, vr core.VR) (uint64, error) {
	token, err := decodeRequiredNumberOrStringValue(raw, path, vr)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseUint(token, 10, bits)
	if err != nil {
		return 0, valueTypePathError(path, vr, raw, "parse unsigned integer: %v", err)
	}
	return value, nil
}

func parseFloat32(raw json.RawMessage, path string, vr core.VR) (float32, error) {
	value, err := parseFloatToken(raw, path, 32, vr)
	if err != nil {
		return 0, err
	}
	return float32(value), nil
}

func parseFloat64(raw json.RawMessage, path string, vr core.VR) (float64, error) {
	return parseFloatToken(raw, path, 64, vr)
}

func parseFloatToken(raw json.RawMessage, path string, bits int, vr core.VR) (float64, error) {
	token, err := decodeRequiredNumberOrStringValue(raw, path, vr)
	if err != nil {
		return 0, err
	}
	switch strings.ToLower(token) {
	case "nan":
		return math.NaN(), nil
	case "inf", "+inf", "infinity", "+infinity":
		return math.Inf(1), nil
	case "-inf", "-infinity":
		return math.Inf(-1), nil
	}
	value, err := strconv.ParseFloat(token, bits)
	if err != nil {
		return 0, valueTypePathError(path, vr, raw, "parse float: %v", err)
	}
	return value, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func decodeJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := ensureJSONEOF(dec); err != nil {
		return err
	}
	return nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing data")
		}
		return err
	}
	return nil
}

func joinJSONPath(prefix, suffix string) string {
	if prefix == "" {
		return suffix
	}
	return prefix + "/" + suffix
}

func indexJSONPath(path string, index int) string {
	return fmt.Sprintf("%s[%d]", path, index)
}

func jsonValueType(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "empty"
	}
	switch trimmed[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return "number"
	default:
		return "invalid"
	}
}

func valueTypePathError(path string, vr core.VR, raw json.RawMessage, format string, args ...any) error {
	return pathError(path, "VR %s Value received %s: %s", vr, jsonValueType(raw), fmt.Sprintf(format, args...))
}

func pathError(path, format string, args ...any) error {
	return fmt.Errorf("dicomjson: %s at %s", fmt.Sprintf(format, args...), path)
}
