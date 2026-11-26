package dicomjson

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// ErrMaxValuesExceeded reports the represented attribute/Value-entry budget.
var ErrMaxValuesExceeded = errors.New("dicomjson: maximum represented value count exceeded")

// DatasetOptions configures encoding of an already represented DICOM JSON
// dataset. The encoder validates VR-specific values without converting through
// a binary DICOM object. Limits apply cumulatively to one encoding operation.
type DatasetOptions struct {
	// MaxValues bounds cumulative attributes and Value entries before generic
	// typed slices are normalized. Zero is unlimited; negative values are invalid.
	MaxValues int
	// TransferSyntax enables validation of encapsulated Pixel Data InlineBinary.
	TransferSyntax transfer.Syntax
	Limits         Limits
	skipValidation bool
	validateOnly   bool
	// ValidateElement is an optional application policy check, called before
	// encoding each attribute. Depth is zero at the root. It must not mutate the
	// dataset. Returning an error stops encoding; the writer may contain a prefix.
	ValidateElement func(tag string, element Element, depth int) error
}

// EncodeDatasetContext writes compact DICOM JSON, with the same bytes as
// json.Marshal(dataset) for admitted non-nil datasets, and no trailing newline.
// A nil dataset is written as an empty JSON object.
// It does not close out. Errors may leave a prefix; stage output when atomic
// publication is required. Callers must not mutate dataset during encoding.
func EncodeDatasetContext(ctx context.Context, out io.Writer, dataset Dataset, opts DatasetOptions) error {
	if opts.MaxValues < 0 {
		return ErrInvalidLimits
	}
	budget, err := newConversionBudget(ctx, opts.Limits)
	if err != nil {
		return err
	}
	if dataset == nil {
		dataset = Dataset{}
	}
	writer := &streamJSONWriter{out: out, budget: budget, maximum: opts.Limits.MaxJSONBytes}
	return encodeDataset(writer, dataset, opts, 0)
}

// ValidateDatasetContext validates the represented dataset with the same
// semantics and traversal limits as EncodeDatasetContext, without publishing.
func ValidateDatasetContext(ctx context.Context, dataset Dataset, opts DatasetOptions) error {
	return EncodeDatasetContext(ctx, io.Discard, dataset, opts)
}

type streamJSONWriter struct {
	out               io.Writer
	budget            *conversionBudget
	maximum           int64
	written           int64
	representedValues int
}

func (w *streamJSONWriter) Write(p []byte) (int, error) {
	if err := w.budget.checkContext(); err != nil {
		return 0, err
	}
	if w.out == nil {
		return 0, fmt.Errorf("dicomjson: nil writer")
	}
	if w.maximum > 0 && int64(len(p)) > w.maximum-w.written {
		return 0, ErrMaxJSONBytesExceeded
	}
	n, err := w.out.Write(p)
	w.written += int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}
func (w *streamJSONWriter) literal(s string) error { _, err := io.WriteString(w, s); return err }
func (w *streamJSONWriter) value(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("dicomjson: invalid JSON value: %w", err)
	}
	_, err = w.Write(data)
	return err
}

// representedElement preserves generic-map field presence and key order.
// This matters for byte compatibility: encoding/json sorts map keys, while
// Element struct fields retain their declaration order and omitempty policy.
type representedElement struct {
	element Element
	fields  map[string]any
}

func representedDataSet(value any) (Dataset, map[string]any, error) {
	switch item := value.(type) {
	case Dataset:
		return item, nil, nil
	case map[string]Element:
		return Dataset(item), nil, nil
	case map[string]any:
		return nil, item, nil
	case nil:
		return nil, nil, nil // JSON null was historically an empty SQ item.
	default:
		return nil, nil, fmt.Errorf("dicomjson: invalid sequence item")
	}
}

func representedAttribute(value any, remaining int) (representedElement, error) {
	switch v := value.(type) {
	case Element:
		return representedElement{element: v}, nil
	case *Element:
		if v != nil {
			return representedElement{element: *v}, nil
		}
	case map[string]any:
		e := Element{}
		for key, value := range v {
			switch key {
			case "vr":
				text, ok := value.(string)
				if !ok {
					return representedElement{}, fmt.Errorf("dicomjson: invalid VR field")
				}
				e.VR = text
			case "Value":
				if value != nil {
					values, ok := value.([]any)
					if !ok {
						slice := reflect.ValueOf(value)
						if slice.Kind() != reflect.Slice || slice.Type().Elem().Kind() == reflect.Uint8 {
							return representedElement{}, fmt.Errorf("dicomjson: invalid Value array")
						}
						if remaining != 0 && (remaining < 0 && slice.Len() > 0 || slice.Len() > remaining && remaining > 0) {
							return representedElement{}, ErrMaxValuesExceeded
						}
						values = make([]any, slice.Len())
						for i := range values {
							values[i] = slice.Index(i).Interface()
						}
					}
					e.Value = values
				}
			case "InlineBinary":
				if value != nil {
					text, ok := value.(string)
					if !ok {
						return representedElement{}, fmt.Errorf("dicomjson: invalid InlineBinary")
					}
					e.InlineBinary = text
				}
			case "BulkDataURI":
				if value != nil {
					text, ok := value.(string)
					if !ok {
						return representedElement{}, fmt.Errorf("dicomjson: invalid BulkDataURI")
					}
					e.BulkDataURI = text
				}
			default:
				return representedElement{}, fmt.Errorf("dicomjson: unrecognized element field")
			}
		}
		if _, ok := v["vr"]; !ok {
			return representedElement{}, fmt.Errorf("dicomjson: missing VR field")
		}
		return representedElement{element: e, fields: v}, nil
	}
	return representedElement{}, fmt.Errorf("dicomjson: invalid data element")
}

func encodeDataset(w *streamJSONWriter, dataset Dataset, opts DatasetOptions, depth int) error {
	return encodeRepresentedDataset(w, dataset, nil, opts, depth)
}
func encodeRepresentedDataset(w *streamJSONWriter, dataset Dataset, generic map[string]any, opts DatasetOptions, depth int) error {
	size := len(dataset)
	if generic != nil {
		size = len(generic)
	}
	if err := w.budget.enterDataSet(depth, size); err != nil {
		return err
	}
	if dataset == nil && generic == nil {
		if opts.validateOnly {
			return nil
		}
		return w.literal("null")
	}
	keys := make([]string, 0, size)
	if generic != nil {
		for k := range generic {
			keys = append(keys, k)
		}
	} else {
		for k := range dataset {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if !opts.validateOnly {
		if err := w.literal("{"); err != nil {
			return err
		}
	}
	for index, key := range keys {
		if err := w.budget.checkContext(); err != nil {
			return err
		}
		tag, err := parseJSONTag(key, key)
		if err != nil {
			return err
		}
		if opts.MaxValues > 0 && w.representedValues >= opts.MaxValues {
			return ErrMaxValuesExceeded
		}
		w.representedValues++
		view := representedElement{element: dataset[key]}
		if generic != nil {
			remaining := 0
			if opts.MaxValues > 0 {
				remaining = opts.MaxValues - w.representedValues
				if remaining == 0 {
					remaining = -1
				}
			}
			view, err = representedAttribute(generic[key], remaining)
			if err != nil {
				return err
			}
		}
		if opts.ValidateElement != nil {
			if err := opts.ValidateElement(key, view.element, depth); err != nil {
				return err
			}
		}
		if !opts.validateOnly && index > 0 {
			if err := w.literal(","); err != nil {
				return err
			}
		}
		if !opts.validateOnly {
			if err := w.value(key); err != nil {
				return err
			}
			if err := w.literal(":"); err != nil {
				return err
			}
		}
		if err := encodeRepresentedElement(w, tag, view, opts, depth); err != nil {
			return err
		}
	}
	if opts.validateOnly {
		return nil
	}
	return w.literal("}")
}

func encodeRepresentedElement(w *streamJSONWriter, tag core.Tag, view representedElement, opts DatasetOptions, depth int) error {
	e := view.element
	if opts.MaxValues > 0 && len(e.Value) > opts.MaxValues-w.representedValues {
		return ErrMaxValuesExceeded
	}
	w.representedValues += len(e.Value)
	vr, err := core.ParseVR(e.VR)
	if err != nil {
		return err
	}
	hasValue, hasInline, hasBulk := len(e.Value) > 0, e.InlineBinary != "", e.BulkDataURI != ""
	if view.fields != nil {
		_, hasValue = view.fields["Value"]
		_, hasInline = view.fields["InlineBinary"]
		_, hasBulk = view.fields["BulkDataURI"]
	}
	count := 0
	for _, b := range []bool{hasValue, hasInline, hasBulk} {
		if b {
			count++
		}
	}
	if count > 1 {
		return fmt.Errorf("dicomjson: conflicting element representations")
	}
	if vr == core.VRSQ && (hasInline || hasBulk) {
		return fmt.Errorf("dicomjson: binary representation is invalid for SQ")
	}
	if hasInline && !opts.skipValidation {
		if err := w.budget.addInlineBinary(int64(base64DecodedLength(e.InlineBinary))); err != nil {
			return err
		}
		reader := base64.NewDecoder(base64.StdEncoding, &streamJSONReader{in: strings.NewReader(e.InlineBinary), budget: w.budget})
		// Verify the Pixel Data envelope without materializing the binary value.
		var prefix [4]byte
		n, readErr := io.ReadFull(reader, prefix[:])
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return fmt.Errorf("dicomjson: invalid InlineBinary: %w", readErr)
		}
		if tag == core.TagPixelData && !opts.TransferSyntax.Encapsulated && n == 4 && (prefix == [4]byte{0xfe, 0xff, 0x00, 0xe0} || prefix == [4]byte{0xff, 0xfe, 0xe0, 0x00}) {
			return object.ErrPixelDataEncodingMismatch
		}
		if _, err := io.Copy(io.Discard, reader); err != nil {
			return fmt.Errorf("dicomjson: invalid InlineBinary: %w", err)
		}

		if tag == core.TagPixelData && opts.TransferSyntax.Encapsulated {
			data, err := base64.StdEncoding.DecodeString(e.InlineBinary)
			if err != nil {
				return fmt.Errorf("dicomjson: invalid InlineBinary")
			}
			if _, err := parseEncapsulatedPixelDataValue(data, opts.TransferSyntax, nil, w.budget); err != nil {
				return err
			}
		}
	}
	if opts.validateOnly {
		if !hasValue {
			return nil
		}
		if vr != core.VRSQ {
			return validateRepresentedValues(w.budget, vr, e.Value)
		}
		if err := w.budget.addSequenceItems(len(e.Value)); err != nil {
			return err
		}
		for _, value := range e.Value {
			ds, generic, err := representedDataSet(value)
			if err != nil {
				return err
			}
			if err := encodeRepresentedDataset(w, ds, generic, opts, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := w.literal("{"); err != nil {
		return err
	}
	fields := []string{"vr"}
	if hasValue {
		fields = append(fields, "Value")
	}
	if hasInline {
		fields = append(fields, "InlineBinary")
	}
	if hasBulk {
		fields = append(fields, "BulkDataURI")
	}
	if view.fields != nil {
		sort.Strings(fields)
	}
	for i, field := range fields {
		if i > 0 {
			if err := w.literal(","); err != nil {
				return err
			}
		}
		if err := w.value(field); err != nil {
			return err
		}
		if err := w.literal(":"); err != nil {
			return err
		}
		if field == "Value" {
			if view.fields != nil && e.Value == nil {
				if err := w.literal("null"); err != nil {
					return err
				}
				continue
			}
			if vr == core.VRSQ {
				if err := w.budget.addSequenceItems(len(e.Value)); err != nil {
					return err
				}
				if err := w.literal("["); err != nil {
					return err
				}
				for j, value := range e.Value {
					if j > 0 {
						if err := w.literal(","); err != nil {
							return err
						}
					}
					ds, generic, err := representedDataSet(value)
					if err != nil {
						return err
					}
					if err := encodeRepresentedDataset(w, ds, generic, opts, depth+1); err != nil {
						return err
					}
				}
				if err := w.literal("]"); err != nil {
					return err
				}
			} else {
				if !opts.skipValidation {
					if err := validateRepresentedValues(w.budget, vr, e.Value); err != nil {
						return err
					}
				}
				if err := w.value(e.Value); err != nil {
					return err
				}
			}
		} else {
			value := any(e.VR)
			if field == "InlineBinary" {
				value = e.InlineBinary
			}
			if field == "BulkDataURI" {
				value = e.BulkDataURI
			}
			if view.fields != nil {
				value = view.fields[field]
			}
			if err := w.value(value); err != nil {
				return err
			}
		}
	}
	return w.literal("}")
}

func validateRepresentedValues(budget *conversionBudget, vr core.VR, values []any) error {
	switch vr {
	case core.VRSQ:
		return fmt.Errorf("dicomjson: unexpected SQ scalar")
	case core.VRUN:
		return fmt.Errorf("dicomjson: UN requires binary representation")
	case core.VRPN:
		for _, value := range values {
			if err := budget.checkContext(); err != nil {
				return err
			}
			if value == nil {
				continue
			}
			var pn PersonNameComponents
			switch v := value.(type) {
			case PersonNameComponents:
				pn = v
			case *PersonNameComponents:
				if v == nil {
					continue
				}
				pn = *v
			case map[string]any:
				for key, component := range v {
					if key != "Alphabetic" && key != "Ideographic" && key != "Phonetic" {
						return fmt.Errorf("dicomjson: invalid person name field")
					}
					text, ok := component.(string)
					if !ok {
						return fmt.Errorf("dicomjson: invalid person name component")
					}
					switch key {
					case "Alphabetic":
						pn.Alphabetic = text
					case "Ideographic":
						pn.Ideographic = text
					case "Phonetic":
						pn.Phonetic = text
					}
				}
			default:
				return fmt.Errorf("dicomjson: PN requires object values")
			}
			if strings.Contains(pn.Alphabetic+pn.Ideographic+pn.Phonetic, "\\") {
				return fmt.Errorf("dicomjson: PN value delimiter")
			}
		}
	case core.VRDS, core.VRIS:
		for _, value := range values {
			if err := budget.checkContext(); err != nil {
				return err
			}
			if value == nil {
				continue
			}
			var token string
			switch v := value.(type) {
			case string:
				token = v
			case json.Number:
				token = v.String()
			default:
				data, err := json.Marshal(value)
				if err != nil {
					return err
				}
				token = string(data)
			}
			if err := validateNumberString(token, vr); err != nil {
				return err
			}
		}
	case core.VRAT:
		for _, value := range values {
			text, ok := value.(string)
			if !ok {
				return fmt.Errorf("dicomjson: AT requires tag strings")
			}
			if _, err := parseJSONTag(text, ""); err != nil {
				return err
			}
		}
	case core.VRFL, core.VROF, core.VRFD, core.VROD, core.VRSS, core.VRUS, core.VROW, core.VRSL, core.VRUL, core.VROL, core.VRSV, core.VRUV, core.VROV, core.VROB:
		// Reuse the numeric parser's exact integer widths, float syntax and special
		// values. This temporary is bounded by one element, never a dataset/array.
		raw, err := json.Marshal(values)
		if err != nil {
			return err
		}
		_, err = encodeNumericValuesContext(vr, raw, "", byteOrderOrDefault(nil), budget)
		return err
	default:
		if !vr.IsStringLike() {
			return fmt.Errorf("dicomjson: unsupported VR")
		}
		if !vr.UsesTextValueDelimiter() && len(values) > 1 {
			return fmt.Errorf("dicomjson: single-valued VR")
		}
		for _, value := range values {
			if err := budget.checkContext(); err != nil {
				return err
			}
			if value == nil {
				continue
			}
			text, ok := value.(string)
			if !ok {
				return fmt.Errorf("dicomjson: text VR requires strings")
			}
			if vr.UsesTextValueDelimiter() && strings.Contains(text, "\\") {
				return fmt.Errorf("dicomjson: text value delimiter")
			}
		}
	}
	return nil
}
