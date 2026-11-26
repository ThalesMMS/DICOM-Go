package dimsecli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/ThalesMMS/dicom-go/attrpath"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/nofollow"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const (
	MaxQueryBytes      = int64(4 << 20)
	MaxQueryInputBytes = int64(16 << 20)
	MaxQueryFiles      = 16
	MaxQueryKeys       = 1024
	MaxQueryKeyBytes   = 1 << 20
	MaxQueryElements   = 4096
	MaxQueryItems      = 1024
	MaxQueryDepth      = 16
)

var ErrQueryLimit = errors.New("query resource limit exceeded")

// QueryFlags composes bounded command input using existing DICOM primitives.
// Files are independent operations; keys edit each file in argument order.
type QueryFlags struct {
	Keys      []string
	Files     []string
	RawSyntax string
	keyBytes  int
}

type queryListFlag struct {
	q    *QueryFlags
	file bool
}

func (v queryListFlag) String() string {
	if v.q == nil {
		return ""
	}
	if v.file {
		return strings.Join(v.q.Files, ",")
	}
	return strings.Join(v.q.Keys, ",")
}
func (v queryListFlag) Set(s string) error {
	if v.file {
		return v.q.AddFile(s)
	}
	if len(v.q.Keys) >= MaxQueryKeys || len(s) > MaxQueryKeyBytes-v.q.keyBytes {
		return ErrQueryLimit
	}
	if _, _, err := ParseQueryKey(s); err != nil {
		return err
	}
	v.q.Keys = append(v.q.Keys, s)
	v.q.keyBytes += len(s)
	return nil
}
func (q *QueryFlags) Register(fs *flag.FlagSet) {
	fs.Var(queryListFlag{q: q}, "k", "query Keyword[=value], (gggg,eeee)[=value], or item path (repeatable)")
	fs.Var(queryListFlag{q: q, file: true}, "query-file", "Part 10, DICOM JSON or raw Identifier file (repeatable, at most 16)")
	fs.StringVar(&q.RawSyntax, "query-file-syntax", "explicit-le", "raw query dataset syntax: explicit-le, implicit-le, explicit-be")
}
func (q *QueryFlags) AddFile(s string) error {
	if len(q.Files) >= MaxQueryFiles || len(s) == 0 || len(s) > 4096 {
		return ErrQueryLimit
	}
	q.Files = append(q.Files, s)
	return nil
}
func (q QueryFlags) HasInput() bool { return len(q.Keys) > 0 || len(q.Files) > 0 }
func (q QueryFlags) Validate() error {
	if len(q.Files) > MaxQueryFiles || len(q.Keys) > MaxQueryKeys {
		return ErrQueryLimit
	}
	if _, err := querySyntax(q.RawSyntax); err != nil {
		return err
	}
	text := 0
	for _, k := range q.Keys {
		if len(k) > MaxQueryKeyBytes-text {
			return ErrQueryLimit
		}
		text += len(k)
		if _, _, err := ParseQueryKey(k); err != nil {
			return err
		}
	}
	return nil
}
func querySyntax(s string) (transfer.Syntax, error) {
	switch s {
	case "", "explicit-le":
		return transfer.ExplicitVRLittleEndian, nil
	case "implicit-le":
		return transfer.ImplicitVRLittleEndian, nil
	case "explicit-be":
		return transfer.ExplicitVRBigEndian, nil
	}
	return transfer.Syntax{}, errors.New("-query-file-syntax must be explicit-le, implicit-le or explicit-be")
}

func ParseQueryKey(s string) (attrpath.Path, string, error) {
	key, value, has := strings.Cut(s, "=")
	if !has {
		value = ""
	}
	key = strings.TrimSpace(key)
	if len(key) == 9 && key[4] == ',' {
		key = "(" + key + ")"
	}
	p, err := attrpath.Parse(key, nil)
	if err != nil {
		return nil, "", errors.New("invalid query key path")
	}
	if len(p) > MaxQueryDepth || p.SelectsItem() {
		return nil, "", ErrQueryLimit
	}
	for _, seg := range p {
		if seg.HasItem && seg.Item > 255 {
			return nil, "", ErrQueryLimit
		}
		if seg.Tag.Group == 0 || seg.Tag.Group == 2 || isQueryPixelData(seg.Tag) {
			return nil, "", errors.New("command, File Meta and Pixel Data are not query keys")
		}
	}
	return p, value, nil
}

func isQueryPixelData(tag core.Tag) bool {
	return tag == core.NewTag(0x7fe0, 0x0010) || tag == core.NewTag(0x7fe0, 0x0008) || tag == core.NewTag(0x7fe0, 0x0009)
}

// Load owns fully resident identifiers; no deferred provider escapes the call.
// It bounds actual reads, not merely the size observed by stat.
func (q QueryFlags) Load(ctx context.Context) ([]*object.Object, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := q.Validate(); err != nil {
		return nil, err
	}
	if len(q.Files) == 0 {
		return []*object.Object{object.New(std.Dictionary)}, nil
	}
	syntax, _ := querySyntax(q.RawSyntax)
	result := make([]*object.Object, 0, len(q.Files))
	total := int64(0)
	for _, path := range q.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f, err := nofollow.OpenFile(path)
		if err != nil {
			return nil, errors.New("open query file failed")
		}
		info, statErr := f.Stat()
		if statErr != nil || !info.Mode().IsRegular() {
			f.Close()
			return nil, errors.New("query file must be regular")
		}
		remaining := MaxQueryInputBytes - total
		limit := MaxQueryBytes
		if remaining < limit {
			limit = remaining
		}
		data, readErr := io.ReadAll(io.LimitReader(queryContextReader{ctx: ctx, r: f}, limit+1))
		closeErr := f.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, errors.New("close query file failed")
		}
		if int64(len(data)) > limit {
			return nil, ErrQueryLimit
		}
		total += int64(len(data))
		obj, err := decodeQuery(ctx, data, syntax)
		if err != nil {
			return nil, queryDecodeError{cause: err}
		}
		if _, err := ValidateQuery(ctx, obj); err != nil {
			return nil, err
		}
		result = append(result, obj)
	}
	return result, nil
}

// Decoder causes may include input values. Keep them available for typed
// cancellation/limit classification without exposing them in CLI diagnostics.
type queryDecodeError struct{ cause error }

func (queryDecodeError) Error() string   { return "query file decoding failed" }
func (e queryDecodeError) Unwrap() error { return e.cause }

func decodeQuery(ctx context.Context, data []byte, syntax transfer.Syntax) (*object.Object, error) {
	trim := bytes.TrimSpace(data)
	if len(trim) > 0 && (trim[0] == '{' || trim[0] == '[') {
		if trim[0] == '[' {
			var arr []json.RawMessage
			if err := json.Unmarshal(trim, &arr); err != nil || len(arr) != 1 {
				return nil, errors.New("query JSON array must contain one Identifier")
			}
			trim = arr[0]
		}
		return dicomjson.UnmarshalContext(ctx, trim, std.Dictionary, dicomjson.UnmarshalOptions{Limits: dicomjson.Limits{MaxJSONBytes: MaxQueryBytes, MaxInlineBinaryBytes: MaxQueryBytes, MaxTotalBinaryBytes: MaxQueryBytes, MaxElements: MaxQueryElements, MaxSequenceItems: MaxQueryItems, MaxSequenceDepth: MaxQueryDepth}})
	}
	opts := object.ReadFileOptions{MaxTotalBytes: MaxQueryBytes, MaxElementBytes: MaxQueryBytes, MaxElements: MaxQueryElements, MaxSequenceDepth: MaxQueryDepth, MaxPixelDataBytes: MaxQueryBytes, MaxFragments: MaxQueryItems}
	reader := queryContextReader{ctx: ctx, r: bytes.NewReader(data)}
	if len(data) >= 132 && string(data[128:132]) == "DICM" {
		f, err := object.ReadFileWithOptions(reader, opts)
		if err != nil {
			return nil, err
		}
		return f.Dataset, nil
	}
	return object.ReadDataSetWithOptions(reader, syntax, opts)
}

// Apply bounds created items across edits, in addition to attrpath's per-edit
// cap, and checks serialized Identifier size before a caller can dial.
func (q QueryFlags) Apply(ctx context.Context, obj *object.Object) error {
	if err := q.Validate(); err != nil {
		return err
	}
	for _, k := range q.Keys {
		items, err := ValidateQuery(ctx, obj)
		if err != nil {
			return err
		}
		p, v, _ := ParseQueryKey(k)
		// Reserve a conservative upper bound before attrpath can create items.
		needed := 0
		for _, s := range p {
			if s.HasItem && !s.AnyItem {
				needed += s.Item + 1
			}
		}
		if needed > MaxQueryItems-items {
			return ErrQueryLimit
		}
		if _, err := attrpath.Insert(obj, p, v, attrpath.Options{MaxCreatedItems: MaxQueryItems, SkipValueValidation: true}); err != nil {
			return errors.New("query key edit failed")
		}
	}
	// Queries loaded by this helper are resident. Replacing SQ values through
	// Put keeps object COW ownership while ordering each item for wire emission.
	for _, e := range obj.Elements() {
		if v, ok := e.Value.(core.SequenceValue); ok {
			e.Value = sortedQuerySequence(v)
			obj.Put(e)
		}
	}
	_, err := ValidateQuery(ctx, obj)
	return err
}

type queryContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r queryContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

type queryCountWriter struct {
	ctx context.Context
	n   int64
}

func (w *queryCountWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > MaxQueryBytes-w.n {
		return 0, ErrQueryLimit
	}
	w.n += int64(len(p))
	return len(p), nil
}

// ValidateQuery refuses unavailable payloads without invoking providers and
// checks structure before the writer's pure size/streaming emission.
func ValidateQuery(ctx context.Context, obj *object.Object) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if obj == nil {
		return 0, errors.New("nil query Identifier")
	}
	elements, items := 0, 0
	var walk func(core.DataSet, int) error
	walk = func(ds core.DataSet, depth int) error {
		if depth > MaxQueryDepth {
			return ErrQueryLimit
		}
		for _, e := range ds.Elements {
			if err := ctx.Err(); err != nil {
				return err
			}
			elements++
			if elements > MaxQueryElements {
				return ErrQueryLimit
			}
			if e.Tag().Group == 0 || e.Tag().Group == 2 || isQueryPixelData(e.Tag()) {
				return errors.New("command, File Meta and Pixel Data are not query keys")
			}
			if e.Value == nil {
				return errors.New("query payload is unavailable")
			}
			switch v := e.Value.(type) {
			case core.SequenceValue:
				items += len(v.Items)
				if items > MaxQueryItems {
					return ErrQueryLimit
				}
				for _, item := range v.Items {
					if err := walk(item, depth+1); err != nil {
						return err
					}
				}
			case core.RawValue, core.StringValue, core.Uint16Value, core.Int16Value, core.Uint32Value, core.Int32Value, core.Uint64Value, core.Int64Value, core.Float32Value, core.Float64Value, core.TagValue:
			default:
				return errors.New("bulk, deferred and unknown values are not query keys")
			}
		}
		return nil
	}
	if obj.Len() > MaxQueryElements {
		return 0, ErrQueryLimit
	}
	if err := walk(obj.ToDataSet(), 0); err != nil {
		return 0, err
	}
	w := &queryCountWriter{ctx: ctx}
	if err := object.WriteDataSet(w, obj, transfer.ExplicitVRLittleEndian); err != nil {
		return 0, fmt.Errorf("query Identifier serialization failed: %w", err)
	}
	return items, nil
}

func sortedQuerySequence(v core.SequenceValue) core.SequenceValue {
	items := make([]core.DataSet, len(v.Items))
	for i, item := range v.Items {
		items[i] = item
		items[i].Elements = append([]core.Element(nil), item.Elements...)
		for j, e := range items[i].Elements {
			if nested, ok := e.Value.(core.SequenceValue); ok {
				items[i].Elements[j].Value = sortedQuerySequence(nested)
			}
		}
		sort.Slice(items[i].Elements, func(a, b int) bool { return items[i].Elements[a].Tag().Less(items[i].Elements[b].Tag()) })
	}
	return core.SequenceValue{Items: items}
}
