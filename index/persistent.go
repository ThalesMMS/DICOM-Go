package index

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/recordstore"
)

var ErrPersistentIndex = errors.New("dicom index: persistent metadata is inconsistent or unavailable")

// PersistentRecord binds detached metadata to one immutable Part 10 file.
// File verification and archive retention remain the caller's responsibility.
// Attributes holds explicitly selected primitive values, keyed by 8-digit tags;
// Record.Selected cannot be persisted because its Elements have interface values.
// Patient/descriptive metadata can contain PHI; select those profiles explicitly.
type PersistentRecord struct {
	Record     Record            `json:"record"`
	Attributes map[string]string `json:"attributes,omitempty"`
	FileBytes  int64             `json:"file_bytes"`
	SHA256     [32]byte          `json:"sha256"`
}

type PersistentStoreOptions struct {
	MaxRecords     int
	MaxRecordBytes int64
	MaxTotalBytes  int64
}

// PersistentStore is a bounded, exclusively owned local metadata directory.
// Records are immutable; Close releases its process lock. Open replays and
// validates every record. Returned records own their maps/pointers/slices.
// It does not own data files, implement query matching or repair archive data.
type PersistentStore struct {
	store          *recordstore.Store
	maxRecordBytes int64
}
type persistentEnvelope struct {
	Version int              `json:"version"`
	Value   PersistentRecord `json:"value"`
}

func OpenPersistentStore(ctx context.Context, dir string, opts PersistentStoreOptions) (*PersistentStore, error) {
	if opts.MaxRecords == 0 {
		opts.MaxRecords = 10_000
	}
	if opts.MaxRecordBytes == 0 {
		opts.MaxRecordBytes = 256 << 10
	}
	if opts.MaxTotalBytes == 0 {
		opts.MaxTotalBytes = 256 << 20
	}
	s, err := recordstore.Open(ctx, dir, recordstore.Options{MaxRecords: opts.MaxRecords, MaxRecordBytes: opts.MaxRecordBytes, MaxTotalBytes: opts.MaxTotalBytes})
	if err != nil {
		return nil, persistentError(err)
	}
	p := &PersistentStore{store: s, maxRecordBytes: opts.MaxRecordBytes}
	if _, err := p.Records(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return p, nil
}

func persistentError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, recordstore.ErrLimit) {
		return ErrResourceLimit
	}
	return ErrPersistentIndex
}

func persistentKey(uid string) string {
	h := sha256.Sum256([]byte(uid))
	return hex.EncodeToString(h[:]) + ".json"
}

func validPersistent(r PersistentRecord) bool {
	v := r.Record
	if r.FileBytes < 1 || r.SHA256 == [32]byte{} || v.Selected != nil || len(r.Attributes) > 128 {
		return false
	}
	for _, uid := range []string{v.Instance.SOPClassUID, v.Instance.SOPInstanceUID, v.Study.InstanceUID, v.Series.InstanceUID, v.FileMeta.TransferSyntaxUID} {
		if !core.IsValidUID(uid) {
			return false
		}
	}
	if v.FileMeta.MediaStorageSOPClassUID != v.Instance.SOPClassUID || v.FileMeta.MediaStorageSOPInstanceUID != v.Instance.SOPInstanceUID || v.Source.Size != r.FileBytes {
		return false
	}
	for key, value := range r.Attributes {
		if len(key) != 8 || strings.ToUpper(key) != key || len(value) > 1024 {
			return false
		}
		if _, err := hex.DecodeString(key); err != nil {
			return false
		}
	}
	return true
}

func decodePersistent(key string, data []byte) (PersistentRecord, error) {
	if !strictPersistentJSON(data) {
		return PersistentRecord{}, ErrPersistentIndex
	}
	var env persistentEnvelope
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&env); err != nil {
		return PersistentRecord{}, ErrPersistentIndex
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF || env.Version != 1 || !validPersistent(env.Value) || persistentKey(env.Value.Record.Instance.SOPInstanceUID) != key {
		return PersistentRecord{}, ErrPersistentIndex
	}
	// encoding/json accepts short/long input for fixed arrays; a digest must
	// contain exactly 32 bytes rather than silently zero-pad or truncate.
	var shape struct{ Value struct{ SHA256 []byte } }
	if json.Unmarshal(data, &shape) != nil || len(shape.Value.SHA256) != 32 {
		return PersistentRecord{}, ErrPersistentIndex
	}
	if persistentAdmission(env.Value, int64(len(data))) != nil {
		return PersistentRecord{}, ErrPersistentIndex
	}
	return env.Value, nil
}

func strictPersistentJSON(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(data))
	tokens := 0
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 64 || tokens > 100_000 {
			return false
		}
		token, err := d.Token()
		tokens++
		if err != nil {
			return false
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				tokens++
				name, ok := key.(string)
				// All field names of this closed schema and hexadecimal attribute
				// keys are ASCII. Reject Unicode case aliases that encoding/json
				// would otherwise fold to an existing ASCII struct field.
				for _, r := range name {
					if r > 127 {
						return false
					}
				}
				folded := strings.ToLower(name)
				if err != nil || !ok || seen[folded] || len(seen) >= 128 {
					return false
				}
				seen[folded] = true
				if !value(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for d.More() {
				if !value(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	if !value(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

func (p *PersistentStore) Insert(ctx context.Context, value PersistentRecord) (bool, error) {
	if !validPersistent(value) {
		return false, ErrPersistentIndex
	}
	if err := persistentAdmission(value, p.maxRecordBytes); err != nil {
		return false, err
	}
	data, err := json.Marshal(persistentEnvelope{Version: 1, Value: value})
	if err != nil {
		return false, ErrPersistentIndex
	}
	created, err := p.store.Create(ctx, persistentKey(value.Record.Instance.SOPInstanceUID), data)
	return created, persistentError(err)
}

func (p *PersistentStore) Records(ctx context.Context) ([]PersistentRecord, error) {
	keys, err := p.store.Keys(ctx)
	if err != nil {
		return nil, persistentError(err)
	}
	rows := make([]PersistentRecord, 0, len(keys))
	for _, key := range keys {
		data, err := p.store.Read(ctx, key)
		if err != nil {
			return nil, persistentError(err)
		}
		row, err := decodePersistent(key, data)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (p *PersistentStore) Close() error { return persistentError(p.store.Close()) }

// Count the existing encoder's output for this closed metadata shape before
// allocating the record buffer. Only bounded floating-point scalars are encoded
// during admission. The counters bound logical work/output, not process RSS.
func persistentAdmission(value PersistentRecord, limit int64) error {
	r := value.Record
	if len(r.Missing) > DefaultMaxDiagnostics || len(r.Diagnostics) > DefaultMaxDiagnostics {
		return ErrResourceLimit
	}
	if r.Geometry != nil && (len(r.Geometry.PixelSpacing) > 64 || len(r.Geometry.ImagePositionPatient) > 64 || len(r.Geometry.ImageOrientationPatient) > 64) {
		return ErrResourceLimit
	}
	budget := limit
	return jsonAdmission(reflect.ValueOf(persistentEnvelope{Version: 1, Value: value}), &budget)
}

func consumeJSON(budget *int64, n int64) error {
	if n > *budget {
		return ErrResourceLimit
	}
	*budget -= n
	return nil
}
func stringJSONSize(s string) (int64, error) {
	if !utf8.ValidString(s) {
		return 0, ErrPersistentIndex
	}
	n := int64(2)
	for _, r := range s {
		switch r {
		case '"', '\\', '\b', '\f', '\n', '\r', '\t':
			n += 2
		case '<', '>', '&', 0x2028, 0x2029:
			n += 6
		default:
			if r < 32 {
				n += 6
			} else {
				n += int64(utf8.RuneLen(r))
			}
		}
	}
	return n, nil
}
func emptyJSON(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64, reflect.Interface, reflect.Pointer:
		return v.IsZero()
	}
	return false
}
func jsonAdmission(v reflect.Value, budget *int64) error {
	if *budget < 0 {
		return ErrResourceLimit
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return consumeJSON(budget, 4)
		}
		return jsonAdmission(v.Elem(), budget)
	case reflect.String:
		if int64(v.Len()) > *budget {
			return ErrResourceLimit
		}
		n, err := stringJSONSize(v.String())
		if err != nil {
			return err
		}
		return consumeJSON(budget, n)
	case reflect.Bool:
		if v.Bool() {
			return consumeJSON(budget, 4)
		}
		return consumeJSON(budget, 5)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return consumeJSON(budget, int64(len(strconv.FormatInt(v.Int(), 10))))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return consumeJSON(budget, int64(len(strconv.FormatUint(v.Uint(), 10))))
	case reflect.Float32, reflect.Float64:
		data, err := json.Marshal(v.Interface())
		if err != nil {
			return ErrPersistentIndex
		}
		return consumeJSON(budget, int64(len(data)))
	case reflect.Struct:
		if err := consumeJSON(budget, 2); err != nil {
			return err
		}
		fields := 0
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")
			if tag[0] == "-" || !f.IsExported() {
				continue
			}
			omit := false
			for _, part := range tag[1:] {
				if part == "omitempty" {
					omit = true
				}
			}
			child := v.Field(i)
			if omit && emptyJSON(child) {
				continue
			}
			name := tag[0]
			if name == "" {
				name = f.Name
			}
			n, err := stringJSONSize(name)
			if err != nil {
				return err
			}
			if fields > 0 {
				n++
			}
			fields++
			if err := consumeJSON(budget, n+1); err != nil {
				return err
			}
			if err := jsonAdmission(child, budget); err != nil {
				return err
			}
		}
		return nil
	case reflect.Array, reflect.Slice:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return consumeJSON(budget, 4)
		}
		if err := consumeJSON(budget, 2); err != nil {
			return err
		}
		for i := 0; i < v.Len(); i++ {
			if i > 0 {
				if err := consumeJSON(budget, 1); err != nil {
					return err
				}
			}
			if err := jsonAdmission(v.Index(i), budget); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		if v.IsNil() {
			return consumeJSON(budget, 4)
		}
		if v.Type().Key().Kind() != reflect.String {
			return ErrPersistentIndex
		}
		if err := consumeJSON(budget, 2); err != nil {
			return err
		}
		iter := v.MapRange()
		i := 0
		for iter.Next() {
			n, err := stringJSONSize(iter.Key().String())
			if err != nil {
				return err
			}
			if i > 0 {
				n++
			}
			i++
			if err := consumeJSON(budget, n+1); err != nil {
				return err
			}
			if err := jsonAdmission(iter.Value(), budget); err != nil {
				return err
			}
		}
		return nil
	default:
		return ErrPersistentIndex
	}
}
