package netconf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

type ioFailure struct{ cause error }

func (e ioFailure) Error() string   { return "dicom netconf: I/O failure" }
func (e ioFailure) Unwrap() []error { return []error{ErrInvalid, e.cause} }
func ioError(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return ioFailure{err}
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// Parse borrows r. The schema pass checks raw scalar spans, duplicate keys,
// nesting and cumulative counts/backing before exact-capacity model arrays.
// Cancellation cannot interrupt an arbitrary reader blocked inside Read.
func Parse(ctx context.Context, r io.Reader, options Options) (_ *Device, err error) {
	defer func() { err = fieldError("json", err) }()
	o, err := normalized(options)
	if err != nil {
		return nil, fieldError("limits", err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil {
		return nil, ErrInvalid
	}
	raw, err := io.ReadAll(io.LimitReader(contextReader{ctx, r}, o.Limits.MaxJSONBytes+1))
	if err != nil {
		return nil, ioError(err)
	}
	if int64(len(raw)) > o.Limits.MaxJSONBytes {
		return nil, ErrResourceLimit
	}
	if !utf8.Valid(raw) {
		return nil, ErrInvalid
	}
	s := scanner{ctx: ctx, raw: raw, l: o.Limits, b: budget{max: o.Limits.MaxCompiledBytes}}
	if err = s.object(0, 1); err != nil {
		return nil, fieldError("json", err)
	}
	s.ws()
	if s.at != len(raw) {
		return nil, ErrInvalid
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	d := decodeDevice(bytes.TrimSpace(raw))
	if err = Validate(ctx, d, o); err != nil {
		return nil, err
	}
	for i := range d.ApplicationEntities {
		d.ApplicationEntities[i].AETitle = ul.NormalizeAETitle(d.ApplicationEntities[i].AETitle)
	}
	return d, nil
}
func ReadFile(ctx context.Context, path string, options Options) (_ *Device, err error) {
	defer func() { err = fieldError("configurationFile", err) }()
	o, err := normalized(options)
	if err != nil {
		return nil, err
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ioError(err)
	}
	defer f.Close()
	return Parse(ctx, f, o)
}

type field struct {
	name     string
	kind     int
	required bool
}

// Scalar kinds: -1 string, -2 bool, -3 integer; object kinds 0..3;
// array kinds 10..14: connections, AEs, capabilities, refs, syntaxes.
var schemas = [...][6]field{
	{{"schemaVersion", -3, true}, {"deviceName", -1, true}, {"description", -1, false}, {"installed", -2, false}, {"connections", 10, true}, {"applicationEntities", 11, true}},
	{{"id", -1, true}, {"hostname", -1, true}, {"port", -3, true}, {"installed", -2, false}, {"tlsRequired", -2, true}},
	{{"aeTitle", -1, true}, {"connectionIDs", 13, true}, {"associationInitiator", -2, true}, {"associationAcceptor", -2, true}, {"installed", -2, false}, {"transferCapabilities", 12, true}},
	{{"sopClassUID", -1, true}, {"role", -1, true}, {"transferSyntaxUIDs", 14, true}},
}

type scanner struct {
	ctx            context.Context
	raw            []byte
	at             int
	l              Limits
	b              budget
	caps, syntaxes int
}

func (s *scanner) ws() {
	for s.at < len(s.raw) {
		switch s.raw[s.at] {
		case ' ', '\t', '\r', '\n':
			s.at++
		default:
			return
		}
	}
}
func (s *scanner) take(c byte) bool {
	s.ws()
	if s.at < len(s.raw) && s.raw[s.at] == c {
		s.at++
		return true
	}
	return false
}
func (s *scanner) scalar(kind int) ([]byte, error) {
	s.ws()
	start := s.at
	if start >= len(s.raw) {
		return nil, ErrInvalid
	}
	if kind == -1 {
		if s.raw[s.at] != '"' {
			return nil, ErrInvalid
		}
		s.at++
		for s.at < len(s.raw) {
			c := s.raw[s.at]
			s.at++
			if s.at-start > s.l.MaxScalarBytes {
				return nil, ErrResourceLimit
			}
			if c == '"' {
				break
			}
			if c == '\\' {
				if s.at >= len(s.raw) {
					return nil, ErrInvalid
				}
				s.at++
			}
		}
		if s.raw[s.at-1] != '"' {
			return nil, ErrInvalid
		}
	} else {
		for s.at < len(s.raw) {
			c := s.raw[s.at]
			if c == ',' || c == ']' || c == '}' || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
				break
			}
			s.at++
			if s.at-start > s.l.MaxScalarBytes {
				return nil, ErrResourceLimit
			}
		}
	}
	raw := s.raw[start:s.at]
	if err := s.b.add(len(raw)); err != nil {
		return nil, err
	}
	switch kind {
	case -1:
		if !json.Valid(raw) {
			return nil, ErrInvalid
		}
	case -2:
		if string(raw) != "true" && string(raw) != "false" {
			return nil, ErrInvalid
		}
	case -3:
		var value int
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, ErrInvalid
		}
	}
	return raw, nil
}
func (s *scanner) object(kind, depth int) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if depth > s.l.MaxDepth {
		return ErrResourceLimit
	}
	if err := s.b.add(256); err != nil {
		return err
	}
	if !s.take('{') {
		return ErrInvalid
	}
	var seen uint8
	if !s.take('}') {
		for {
			key, err := s.scalar(-1)
			if err != nil {
				return err
			}
			if len(key) > 128 {
				return ErrResourceLimit
			}
			var name string
			_ = json.Unmarshal(key, &name)
			index := -1
			for i, f := range schemas[kind] {
				if f.name == name && name != "" {
					index = i
					break
				}
			}
			if index < 0 || seen&(1<<index) != 0 {
				return ErrInvalid
			}
			seen |= 1 << index
			if !s.take(':') {
				return ErrInvalid
			}
			f := schemas[kind][index]
			if f.kind < 0 {
				value, scalarErr := s.scalar(f.kind)
				if scalarErr != nil {
					return scalarErr
				}
				if f.kind == -1 {
					limit := s.l.MaxNameBytes
					switch kind {
					case 0:
						if f.name == "description" {
							limit = s.l.MaxDescriptionBytes
						}
					case 1:
						if f.name == "hostname" {
							limit = s.l.MaxHostnameBytes
						}
					case 2:
						limit = ul.MaxAETitleLength
					case 3:
						limit = 64
						if f.name == "role" {
							if decodedStringBytes(value) != 3 {
								return ErrInvalid
							}
							limit = 3
						}
					}
					if decodedStringBytes(value) > limit {
						return ErrResourceLimit
					}
				}
			} else {
				if err = s.array(f.kind, depth+1); err != nil {
					return err
				}
			}
			if s.take('}') {
				break
			}
			if !s.take(',') {
				return ErrInvalid
			}
		}
	}
	for i, f := range schemas[kind] {
		if f.required && seen&(1<<i) == 0 {
			return ErrInvalid
		}
	}
	return nil
}
func (s *scanner) array(kind, depth int) error {
	if depth > s.l.MaxDepth {
		return ErrResourceLimit
	}
	if !s.take('[') {
		return ErrInvalid
	}
	count := 0
	max := s.l.MaxConnections
	child := -1
	switch kind {
	case 10:
		child = 1
	case 11:
		child = 2
		max = s.l.MaxAEs
	case 12:
		child = 3
		max = s.l.MaxCapabilitiesPerAE
	case 14:
		max = s.l.MaxTransferSyntaxesPerCapability
	}
	if s.take(']') {
		return nil
	}
	for {
		count++
		if count > max {
			return ErrResourceLimit
		}
		if kind == 12 {
			s.caps++
			if s.caps > s.l.MaxCapabilities {
				return ErrResourceLimit
			}
		}
		if kind == 14 {
			s.syntaxes++
			if s.syntaxes > s.l.MaxTransferSyntaxes {
				return ErrResourceLimit
			}
		}
		if err := s.b.add(80); err != nil {
			return err
		}
		if child < 0 {
			value, err := s.scalar(-1)
			if err != nil {
				return err
			}
			limit := s.l.MaxNameBytes
			if kind == 14 {
				limit = 64
			}
			if decodedStringBytes(value) > limit {
				return ErrResourceLimit
			}
		} else {
			if err := s.object(child, depth+1); err != nil {
				return err
			}
		}
		if s.take(']') {
			return nil
		}
		if !s.take(',') {
			return ErrInvalid
		}
	}
}

// Count encoding/json's decoded UTF-8 bytes from an already valid string token,
// including surrogate replacement/pairs. No decoded string is allocated in the
// admission pass; actual model strings are copied only after all bounds pass.
func decodedStringBytes(raw []byte) int {
	n := 0
	for i := 1; i < len(raw)-1; i++ {
		if raw[i] != '\\' {
			n++
			continue
		}
		i++
		if raw[i] != 'u' {
			n++
			continue
		}
		r := hex16(raw[i+1 : i+5])
		i += 4
		switch {
		case r >= 0xd800 && r <= 0xdbff:
			if i+6 < len(raw) && raw[i+1] == '\\' && raw[i+2] == 'u' {
				low := hex16(raw[i+3 : i+7])
				if low >= 0xdc00 && low <= 0xdfff {
					n += 4
					i += 6
					continue
				}
			}
			n += 3
		case r >= 0xdc00 && r <= 0xdfff:
			n += 3
		case r < 0x80:
			n++
		case r < 0x800:
			n += 2
		default:
			n += 3
		}
	}
	return n
}
func hex16(raw []byte) uint16 {
	var n uint16
	for _, c := range raw {
		n <<= 4
		switch {
		case c >= '0' && c <= '9':
			n += uint16(c - '0')
		case c >= 'a' && c <= 'f':
			n += uint16(c - 'a' + 10)
		default:
			n += uint16(c - 'A' + 10)
		}
	}
	return n
}

// These span operations run only after the schema pass. They retain no tree of
// RawMessages: at most six borrowed spans per object and exact result arrays.
func skipValue(raw []byte, at int) int {
	for at < len(raw) && (raw[at] == ' ' || raw[at] == '\t' || raw[at] == '\r' || raw[at] == '\n') {
		at++
	}
	switch raw[at] {
	case '"':
		at++
		for {
			c := raw[at]
			at++
			if c == '"' {
				return at
			}
			if c == '\\' {
				at++
			}
		}
	case '{', '[':
		close := byte('}')
		if raw[at] == '[' {
			close = ']'
		}
		at++
		for {
			for raw[at] == ' ' || raw[at] == '\t' || raw[at] == '\r' || raw[at] == '\n' || raw[at] == ',' || raw[at] == ':' {
				at++
			}
			if raw[at] == close {
				return at + 1
			}
			at = skipValue(raw, at)
		}
	default:
		for at < len(raw) && raw[at] != ',' && raw[at] != ']' && raw[at] != '}' && raw[at] != ' ' && raw[at] != '\t' && raw[at] != '\r' && raw[at] != '\n' {
			at++
		}
		return at
	}
}
func objectSpans(raw []byte, kind int) [6][]byte {
	var out [6][]byte
	at := 1
	for {
		for raw[at] == ' ' || raw[at] == '\t' || raw[at] == '\r' || raw[at] == '\n' || raw[at] == ',' {
			at++
		}
		if raw[at] == '}' {
			return out
		}
		end := skipValue(raw, at)
		var name string
		_ = json.Unmarshal(raw[at:end], &name)
		at = end
		for raw[at] != ':' {
			at++
		}
		at++
		for raw[at] == ' ' || raw[at] == '\t' || raw[at] == '\r' || raw[at] == '\n' {
			at++
		}
		end = skipValue(raw, at)
		for i, f := range schemas[kind] {
			if name == f.name {
				out[i] = raw[at:end]
				break
			}
		}
		at = end
	}
}
func arrayCount(raw []byte) int {
	n := 0
	for at := 1; at < len(raw); {
		for raw[at] == ' ' || raw[at] == '\t' || raw[at] == '\r' || raw[at] == '\n' || raw[at] == ',' {
			at++
		}
		if raw[at] == ']' {
			return n
		}
		n++
		at = skipValue(raw, at)
	}
	return n
}
func eachSpan(raw []byte, fn func(int, []byte)) {
	i := 0
	for at := 1; at < len(raw); {
		for raw[at] == ' ' || raw[at] == '\t' || raw[at] == '\r' || raw[at] == '\n' || raw[at] == ',' {
			at++
		}
		if raw[at] == ']' {
			return
		}
		end := skipValue(raw, at)
		fn(i, raw[at:end])
		i++
		at = end
	}
}
func str(raw []byte) string {
	var v string
	if raw != nil {
		_ = json.Unmarshal(raw, &v)
	}
	return v
}
func boolean(raw []byte) bool { return string(raw) == "true" }
func optionalBool(raw []byte) *bool {
	if raw == nil {
		return nil
	}
	v := boolean(raw)
	return &v
}
func stringsArray(raw []byte) []string {
	out := make([]string, arrayCount(raw))
	eachSpan(raw, func(i int, v []byte) { out[i] = str(v) })
	return out
}
func decodeDevice(raw []byte) *Device {
	f := objectSpans(raw, 0)
	version, _ := strconv.Atoi(string(f[0]))
	d := &Device{SchemaVersion: version, DeviceName: str(f[1]), Description: str(f[2]), Installed: optionalBool(f[3]), Connections: make([]Connection, arrayCount(f[4])), ApplicationEntities: make([]ApplicationEntity, arrayCount(f[5]))}
	eachSpan(f[4], func(i int, v []byte) {
		c := objectSpans(v, 1)
		port, _ := strconv.Atoi(string(c[2]))
		d.Connections[i] = Connection{ID: str(c[0]), Hostname: str(c[1]), Port: port, Installed: optionalBool(c[3]), TLSRequired: boolean(c[4])}
	})
	eachSpan(f[5], func(i int, v []byte) {
		a := objectSpans(v, 2)
		ae := ApplicationEntity{AETitle: str(a[0]), ConnectionIDs: stringsArray(a[1]), AssociationInitiator: boolean(a[2]), AssociationAcceptor: boolean(a[3]), Installed: optionalBool(a[4]), TransferCapabilities: make([]TransferCapability, arrayCount(a[5]))}
		eachSpan(a[5], func(j int, v []byte) {
			t := objectSpans(v, 3)
			ae.TransferCapabilities[j] = TransferCapability{SOPClassUID: str(t[0]), Role: Role(str(t[1])), TransferSyntaxUIDs: stringsArray(t[2])}
		})
		d.ApplicationEntities[i] = ae
	})
	return d
}

// Marshal returns deterministic JSON after validation and encoded-size admission.
func Marshal(ctx context.Context, d *Device, options Options) (_ []byte, err error) {
	defer func() { err = fieldError("serialization", err) }()
	o, err := normalized(options)
	if err != nil {
		return nil, err
	}
	if err = Validate(ctx, d, o); err != nil {
		return nil, err
	}
	if err = serializationAdmission(ctx, d, o.Limits); err != nil {
		return nil, err
	}
	size := jsonDeviceSize(d)
	if size > o.Limits.MaxJSONBytes {
		return nil, ErrResourceLimit
	}
	if ctx != nil {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
	}
	// A detached header view canonicalizes padding and empty capability arrays;
	// it never changes the caller's model or copies its already admitted payload.
	view := *d
	view.ApplicationEntities = make([]ApplicationEntity, len(d.ApplicationEntities))
	copy(view.ApplicationEntities, d.ApplicationEntities)
	for i := range view.ApplicationEntities {
		a := &view.ApplicationEntities[i]
		a.AETitle = ul.NormalizeAETitle(a.AETitle)
		if a.TransferCapabilities == nil {
			a.TransferCapabilities = []TransferCapability{}
		}
	}
	var output bytes.Buffer
	output.Grow(int(size) + 1)
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err = encoder.Encode(&view); err != nil {
		return nil, ErrInvalid
	}
	if ctx != nil {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
	}
	return output.Bytes()[:output.Len()-1], nil
}
func Write(ctx context.Context, w io.Writer, d *Device, options Options) error {
	raw, err := Marshal(ctx, d, options)
	if err != nil {
		return err
	}
	if ctx != nil {
		if err = ctx.Err(); err != nil {
			return err
		}
	}
	if w == nil {
		return ErrInvalid
	}
	n, err := w.Write(raw)
	if err != nil {
		return ioError(err)
	}
	if n != len(raw) {
		return ioError(io.ErrShortWrite)
	}
	return nil
}

// Exact string sizes match encoding/json with HTML escaping disabled. No encoder
// or serialization buffer is created during this admission pass.
func jsonStringSize(s string) int64 {
	n := int64(2)
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			n += 2
		case r == '\b' || r == '\f' || r == '\n' || r == '\r' || r == '\t':
			n += 2
		case r < 32 || r == 0x2028 || r == 0x2029:
			n += 6
		default:
			n += int64(utf8.RuneLen(r))
		}
	}
	return n
}
func boolSize(v bool) int64 {
	if v {
		return 4
	}
	return 5
}
func installedSize(v *bool) int64 {
	if v == nil {
		return 0
	}
	return int64(len(",\"installed\":")) + boolSize(*v)
}
func listSize(values []string) int64 {
	n := int64(2)
	for i, v := range values {
		if i > 0 {
			n++
		}
		n += jsonStringSize(v)
	}
	return n
}
func jsonDeviceSize(d *Device) int64 {
	n := int64(len("{\"schemaVersion\":1,\"deviceName\":")) + jsonStringSize(d.DeviceName) + installedSize(d.Installed)
	if d.Description != "" {
		n += int64(len(",\"description\":")) + jsonStringSize(d.Description)
	}
	n += int64(len(",\"connections\":["))
	for i, c := range d.Connections {
		if i > 0 {
			n++
		}
		n += int64(len("{\"id\":")) + jsonStringSize(c.ID) + int64(len(",\"hostname\":")) + jsonStringSize(c.Hostname) + int64(len(",\"port\":")) + int64(len(strconv.Itoa(c.Port))) + installedSize(c.Installed) + int64(len(",\"tlsRequired\":")) + boolSize(c.TLSRequired) + 1
	}
	n += int64(len("],\"applicationEntities\":["))
	for i, a := range d.ApplicationEntities {
		if i > 0 {
			n++
		}
		n += int64(len("{\"aeTitle\":")) + jsonStringSize(ul.NormalizeAETitle(a.AETitle)) + int64(len(",\"connectionIDs\":")) + listSize(a.ConnectionIDs) + int64(len(",\"associationInitiator\":")) + boolSize(a.AssociationInitiator) + int64(len(",\"associationAcceptor\":")) + boolSize(a.AssociationAcceptor) + installedSize(a.Installed) + int64(len(",\"transferCapabilities\":["))
		for j, t := range a.TransferCapabilities {
			if j > 0 {
				n++
			}
			n += int64(len("{\"sopClassUID\":")) + jsonStringSize(t.SOPClassUID) + int64(len(",\"role\":")) + jsonStringSize(string(t.Role)) + int64(len(",\"transferSyntaxUIDs\":")) + listSize(t.TransferSyntaxUIDs) + 1
		}
		n += 2
	}
	return n + 2
}

// Admit the same canonical schema/scalar/backing bounds as Parse before the
// serialization buffer exists. This also covers caller-created models whose
// custom decoded string limits exceed the raw escaped-token limit.
func serializationAdmission(ctx context.Context, d *Device, l Limits) error {
	depth := 4
	for _, a := range d.ApplicationEntities {
		if len(a.TransferCapabilities) > 0 {
			depth = 6
			break
		}
	}
	if depth > l.MaxDepth {
		return ErrResourceLimit
	}
	b := budget{max: l.MaxCompiledBytes}
	scalar := func(n int64) error {
		if n > int64(l.MaxScalarBytes) {
			return ErrResourceLimit
		}
		return b.add(int(n))
	}
	text := func(s string) error { return scalar(jsonStringSize(s)) }
	object := func(kind int, hasOptional func(string) bool) error {
		if err := b.add(256); err != nil {
			return err
		}
		for _, f := range schemas[kind] {
			if f.name != "" && (f.required || hasOptional(f.name)) {
				if err := text(f.name); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := object(0, func(k string) bool {
		return k == "description" && d.Description != "" || k == "installed" && d.Installed != nil
	}); err != nil {
		return err
	}
	if err := scalar(1); err != nil {
		return err
	}
	if err := text(d.DeviceName); err != nil {
		return err
	}
	if d.Description != "" {
		if err := text(d.Description); err != nil {
			return err
		}
	}
	if d.Installed != nil {
		if err := scalar(boolSize(*d.Installed)); err != nil {
			return err
		}
	}
	for _, c := range d.Connections {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if err := b.add(80); err != nil {
			return err
		}
		if err := object(1, func(k string) bool { return k == "installed" && c.Installed != nil }); err != nil {
			return err
		}
		for _, s := range []string{c.ID, c.Hostname} {
			if err := text(s); err != nil {
				return err
			}
		}
		if err := scalar(int64(len(strconv.Itoa(c.Port)))); err != nil {
			return err
		}
		if err := scalar(boolSize(c.TLSRequired)); err != nil {
			return err
		}
		if c.Installed != nil {
			if err := scalar(boolSize(*c.Installed)); err != nil {
				return err
			}
		}
	}
	for _, a := range d.ApplicationEntities {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if err := b.add(80); err != nil {
			return err
		}
		if err := object(2, func(k string) bool { return k == "installed" && a.Installed != nil }); err != nil {
			return err
		}
		if err := text(ul.NormalizeAETitle(a.AETitle)); err != nil {
			return err
		}
		for _, v := range []bool{a.AssociationInitiator, a.AssociationAcceptor} {
			if err := scalar(boolSize(v)); err != nil {
				return err
			}
		}
		if a.Installed != nil {
			if err := scalar(boolSize(*a.Installed)); err != nil {
				return err
			}
		}
		for _, id := range a.ConnectionIDs {
			if err := b.add(80); err != nil {
				return err
			}
			if err := text(id); err != nil {
				return err
			}
		}
		for _, t := range a.TransferCapabilities {
			if err := b.add(80); err != nil {
				return err
			}
			if err := object(3, func(string) bool { return false }); err != nil {
				return err
			}
			if err := text(t.SOPClassUID); err != nil {
				return err
			}
			if err := text(string(t.Role)); err != nil {
				return err
			}
			for _, ts := range t.TransferSyntaxUIDs {
				if err := b.add(80); err != nil {
					return err
				}
				if err := text(ts); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
