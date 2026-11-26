package dicomjson

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	// ErrExpectedDatasetArray reports a non-array document at an array decoder.
	ErrExpectedDatasetArray = errors.New("dicomjson: expected dataset array")
	// ErrExpectedDatasetObject reports a non-object dataset, including JSON null.
	ErrExpectedDatasetObject = errors.New("dicomjson: expected dataset object")
	// ErrDuplicateDatasetTag reports repeated tags in the same top-level dataset.
	ErrDuplicateDatasetTag = errors.New("dicomjson: duplicate dataset tag")
)

// Encoder writes objects without retaining the encoded JSON for a whole
// result array. It does not close the writer and is not safe for concurrent use.
type Encoder struct {
	out  io.Writer
	opts Options
}

func NewEncoder(out io.Writer, opts Options) *Encoder { return &Encoder{out: out, opts: opts} }
func (e *Encoder) Encode(obj *object.Object) error    { return e.EncodeContext(context.Background(), obj) }
func (e *Encoder) EncodeContext(ctx context.Context, obj *object.Object) error {
	return EncodeContext(ctx, e.out, obj, e.opts)
}

// EncodeArrayContext calls next synchronously until io.EOF and writes one JSON
// array. It retains only one converted dataset at a time. Limits are cumulative;
// errors leave a prefix and no rollback is attempted. next must observe ctx.
func (e *Encoder) EncodeArrayContext(ctx context.Context, next func() (*object.Object, error)) error {
	budget, err := newConversionBudget(ctx, e.opts.Limits)
	if err != nil {
		return err
	}
	writer := objectStreamWriter(e.out, e.opts, budget)
	if next == nil {
		return fmt.Errorf("dicomjson: nil dataset producer")
	}
	if err := writer.literal("["); err != nil {
		return err
	}
	for count := 0; ; count++ {
		if err := budget.checkContext(); err != nil {
			return err
		}
		obj, err := next()
		if errors.Is(err, io.EOF) {
			return writer.literal("]")
		}
		if err != nil {
			return err
		}
		if count > 0 {
			if err := writer.literal(","); err != nil {
				return err
			}
		}
		if err := encodeObject(writer, obj, e.opts, budget); err != nil {
			return err
		}
	}
}

// EncodeContext observes ctx and the same conversion semantics as MarshalContext.
// Compact/pretty output is byte-equivalent to MarshalContext, without a newline.
// Errors may leave a prefix; the caller owns atomic publication and reader closure.
func EncodeContext(ctx context.Context, out io.Writer, obj *object.Object, opts Options) error {
	budget, err := newConversionBudget(ctx, opts.Limits)
	if err != nil {
		return err
	}
	return encodeObject(objectStreamWriter(out, opts, budget), obj, opts, budget)
}
func objectStreamWriter(out io.Writer, opts Options, budget *conversionBudget) *streamJSONWriter {
	target := &streamJSONWriter{out: out, budget: budget, maximum: opts.Limits.MaxJSONBytes}
	if !opts.Pretty {
		return target
	}
	return &streamJSONWriter{out: &indentJSONWriter{out: target}, budget: budget}
}
func encodeObject(out *streamJSONWriter, obj *object.Object, opts Options, budget *conversionBudget) error {
	if opts.ByteOrder == nil {
		if obj == nil {
			opts.ByteOrder = byteOrderOrDefault(nil)
		} else {
			opts.ByteOrder = obj.ValueByteOrder()
		}
	}
	dataset, err := marshalElementsContext(obj, opts, budget, 0)
	if err != nil {
		return err
	}
	// Conversion above already accounts traversal/binary limits and handles
	// encapsulated pixels/private VR/charset. The writer only emits its result.
	return encodeConvertedDataset(out, dataset, opts)
}
func encodeConvertedDataset(out *streamJSONWriter, dataset map[string]Element, opts Options) error {
	// Keep the conversion budget counters, but avoid charging converted elements
	// twice. A separate emission budget observes the same context/depth only.
	emission, _ := newConversionBudget(out.budget.ctx, Limits{MaxSequenceDepth: out.budget.limits.MaxSequenceDepth})
	originalBudget := out.budget
	out.budget = emission
	defer func() { out.budget = originalBudget }()
	return encodeDataset(out, Dataset(dataset), DatasetOptions{skipValidation: true}, 0)
}

// Decoder reads one dataset at a time. It never buffers a complete result
// array. It is not safe for concurrent use and never closes the reader.
type Decoder struct {
	in   io.Reader
	dict dictionary.DataDictionary
	opts UnmarshalOptions
}

func NewDecoder(in io.Reader, dict dictionary.DataDictionary, opts UnmarshalOptions) *Decoder {
	return &Decoder{in: in, dict: dict, opts: opts}
}

// DecodeContext reads exactly one JSON dataset and rejects trailing JSON values.
func (d *Decoder) DecodeContext(ctx context.Context) (*object.Object, error) {
	reader, budget, err := d.reader(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := decodeDatasetObject[json.RawMessage](reader, budget, "$")
	if err != nil {
		return nil, err
	}
	if err := checkDecoderOffset(reader, budget); err != nil {
		return nil, err
	}
	opts := d.opts
	if opts.ByteOrder == nil && opts.TransferSyntax.ByteOrder != nil {
		opts.ByteOrder = opts.TransferSyntax.ByteOrder
	}
	opts.ByteOrder = byteOrderOrDefault(opts.ByteOrder)
	ds, err := unmarshalDataSetMapContext(raw, "", d.dict, opts, budget, 0)
	if err != nil {
		return nil, err
	}
	if err := ensureJSONEOF(reader); err != nil {
		return nil, err
	}
	obj := object.FromElementsWithTextOptions(ds.Elements, d.dict, opts.TextOptions)
	obj.SetValueByteOrder(opts.ByteOrder)
	return obj, nil
}
func (d *Decoder) Decode() (*object.Object, error) { return d.DecodeContext(context.Background()) }

// DecodeArrayContext invokes handle synchronously for each fully validated
// object. Earlier callbacks remain consumed if a later dataset, limit, callback
// or trailing JSON fails. No goroutines or callback replay are used.
func (d *Decoder) DecodeArrayContext(ctx context.Context, handle func(*object.Object) error) error {
	if handle == nil {
		return fmt.Errorf("dicomjson: nil dataset handler")
	}
	reader, budget, err := d.reader(ctx)
	if err != nil {
		return err
	}
	opts := d.opts
	if opts.ByteOrder == nil && opts.TransferSyntax.ByteOrder != nil {
		opts.ByteOrder = opts.TransferSyntax.ByteOrder
	}
	opts.ByteOrder = byteOrderOrDefault(opts.ByteOrder)
	return decodeArray(reader, budget, func(raw map[string]json.RawMessage) error {
		ds, err := unmarshalDataSetMapContext(raw, "", d.dict, opts, budget, 0)
		if err != nil {
			return err
		}
		obj := object.FromElementsWithTextOptions(ds.Elements, d.dict, opts.TextOptions)
		obj.SetValueByteOrder(opts.ByteOrder)
		return handle(obj)
	})
}

// DecodeDatasetsContext decodes the package's represented Dataset shape. Number
// tokens remain json.Number, and VR validation/limits are applied before each
// callback. It shares DecodeArrayContext's explicit partial-consumption contract.
func (d *Decoder) DecodeDatasetsContext(ctx context.Context, handle func(Dataset) error) error {
	if handle == nil {
		return fmt.Errorf("dicomjson: nil dataset handler")
	}
	reader, budget, err := d.reader(ctx)
	if err != nil {
		return err
	}
	return decodeArray(reader, budget, func(generic map[string]any) error {
		writer := &streamJSONWriter{out: io.Discard, budget: budget}
		if err := encodeRepresentedDataset(writer, nil, generic, DatasetOptions{TransferSyntax: d.opts.TransferSyntax, validateOnly: true}, 0); err != nil {
			return err
		}
		dataset := make(Dataset, len(generic))
		for tag, value := range generic {
			element, err := representedAttribute(value, 0)
			if err != nil {
				return err
			}
			dataset[tag] = element.element
		}
		return handle(dataset)
	})
}

// DecodeRawDatasetsContext reads a represented dataset array with the same
// partial-consumption policy as DecodeDatasetsContext. It enforces JSON bytes,
// cumulative top-level elements, and unique top-level tag identity, and leaves
// element fields to json decoding into Element. Numbers remain json.Number.
func (d *Decoder) DecodeRawDatasetsContext(ctx context.Context, handle func(Dataset) error) error {
	if handle == nil {
		return fmt.Errorf("dicomjson: nil dataset handler")
	}
	reader, budget, err := d.reader(ctx)
	if err != nil {
		return err
	}
	return decodeArray(reader, budget, func(raw map[string]Element) error {
		if err := budget.enterDataSet(0, len(raw)); err != nil {
			return err
		}
		return handle(Dataset(raw))
	})
}
func (d *Decoder) reader(ctx context.Context) (*json.Decoder, *conversionBudget, error) {
	budget, err := newConversionBudget(ctx, d.opts.Limits)
	if err != nil {
		return nil, nil, err
	}
	if err := budget.checkContext(); err != nil {
		return nil, nil, err
	}
	if d.in == nil {
		return nil, nil, fmt.Errorf("dicomjson: nil reader")
	}
	reader := json.NewDecoder(&streamJSONReader{in: d.in, budget: budget, maximum: d.opts.Limits.MaxJSONBytes})
	reader.UseNumber()
	return reader, budget, nil
}
func checkDecoderOffset(reader *json.Decoder, budget *conversionBudget) error {
	if err := budget.checkContext(); err != nil {
		return err
	}
	if budget.limits.MaxJSONBytes > 0 && reader.InputOffset() > budget.limits.MaxJSONBytes {
		return ErrMaxJSONBytesExceeded
	}
	return nil
}

func decodeArray[T any](reader *json.Decoder, budget *conversionBudget, handle func(map[string]T) error) error {
	token, err := reader.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('[') {
		return ErrExpectedDatasetArray
	}
	for index := 0; reader.More(); index++ {
		if err := budget.checkContext(); err != nil {
			return err
		}
		raw, err := decodeDatasetObject[T](reader, budget, indexJSONPath("", index))
		if err != nil {
			return err
		}
		if err := checkDecoderOffset(reader, budget); err != nil {
			return err
		}
		if err := handle(raw); err != nil {
			return err
		}
	}
	token, err = reader.Token()
	if err != nil {
		return err
	}
	if token != json.Delim(']') {
		return fmt.Errorf("dicomjson: invalid dataset array")
	}
	return ensureJSONEOF(reader)
}

func decodeDatasetObject[T any](reader *json.Decoder, budget *conversionBudget, path string) (map[string]T, error) {
	token, err := reader.Token()
	if err != nil {
		return nil, err
	}
	if token != json.Delim('{') {
		return nil, fmt.Errorf("%w at %s", ErrExpectedDatasetObject, path)
	}
	dataset := make(map[string]T)
	tags := make(map[string]struct{})
	for reader.More() {
		if err := budget.checkContext(); err != nil {
			return nil, err
		}
		token, err := reader.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("%w at %s", ErrExpectedDatasetObject, path)
		}
		tag := strings.ToUpper(strings.TrimSpace(key))
		if _, exists := tags[tag]; exists {
			if len(tag) == 8 {
				if _, err := strconv.ParseUint(tag, 16, 32); err == nil {
					path = joinJSONPath(path, tag)
				}
			}
			return nil, fmt.Errorf("%w at %s", ErrDuplicateDatasetTag, path)
		}
		if max := budget.limits.MaxElements; max > 0 && len(dataset) >= max-budget.elements {
			return nil, fmt.Errorf("%w: got more than %d", ErrMaxElementsExceeded, max)
		}
		var value T
		if err := reader.Decode(&value); err != nil {
			return nil, err
		}
		dataset[key] = value
		tags[tag] = struct{}{}
	}
	if _, err := reader.Token(); err != nil {
		return nil, err
	}
	return dataset, nil
}

type streamJSONReader struct {
	in            io.Reader
	budget        *conversionBudget
	maximum, read int64
	exceeded      bool
	pending       error
}

func (r *streamJSONReader) Read(p []byte) (int, error) {
	if r.pending != nil {
		return 0, r.pending
	}
	if err := r.budget.checkContext(); err != nil {
		return 0, err
	}
	if r.exceeded {
		return 0, ErrMaxJSONBytesExceeded
	}
	if len(p) == 0 {
		return 0, nil
	}
	remaining := int64(0)
	if r.maximum > 0 {
		remaining = r.maximum - r.read
		// Only add the probe byte after proving remaining is smaller than a slice
		// length. MaxInt64 is a valid ceiling and must never enter remaining+1.
		if remaining < int64(len(p)) {
			p = p[:remaining+1]
		}
	}
	n, err := r.in.Read(p)
	if r.maximum > 0 && int64(n) > remaining {
		r.exceeded = true
		return n, ErrMaxJSONBytesExceeded
	}
	r.read += int64(n)
	if err == nil {
		err = r.budget.checkContext()
	}
	// encoding/json may consume a complete value and discard a reader error
	// returned alongside bytes. Preserve it for the trailing-input check.
	if err != nil && err != io.EOF {
		r.pending = err
	}
	return n, err
}

// indentJSONWriter formats compact chunks without materializing JSON output.
// Its state survives chunk boundaries inside escaped string literals.
type indentJSONWriter struct {
	out                      io.Writer
	depth                    int
	quoted, escaped, pending bool
	last                     byte
}

func (w *indentJSONWriter) Write(p []byte) (int, error) {
	var b []byte
	newline := func() {
		b = append(b, '\n')
		for i := 0; i < w.depth; i++ {
			b = append(b, ' ', ' ')
		}
	}
	for _, c := range p {
		if w.quoted {
			b = append(b, c)
			if w.escaped {
				w.escaped = false
			} else if c == '\\' {
				w.escaped = true
			} else if c == '"' {
				w.quoted = false
			}
			w.last = c
			continue
		}
		if w.pending && c != '}' && c != ']' {
			newline()
			w.pending = false
		}
		switch c {
		case '{', '[':
			b = append(b, c)
			w.depth++
			w.pending = true
		case '}', ']':
			w.depth--
			if w.pending {
				w.pending = false
			} else {
				newline()
			}
			b = append(b, c)
		case ',':
			b = append(b, c)
			newline()
		case ':':
			b = append(b, c, ' ')
		default:
			b = append(b, c)
			if c == '"' {
				w.quoted = true
			}
		}
		w.last = c
	}
	n, err := w.out.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
