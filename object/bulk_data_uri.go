package object

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
)

const (
	// DefaultMaxBulkDataValueBytes bounds one resolved BulkDataURI value when
	// a limit is left at zero.
	DefaultMaxBulkDataValueBytes = int64(1 << 30)
	// DefaultMaxBulkDataTotalBytes bounds all values resolved by one
	// ResolveBulkData call when ResolveBulkDataOptions.MaxTotalBytes is zero.
	DefaultMaxBulkDataTotalBytes = int64(2 << 30)
)

var (
	// ErrBulkDataURI reports a BulkDataURI that a resolver does not accept:
	// an unsupported scheme, a malformed reference, or a file outside the
	// resolver's root. Diagnostics never include the URI itself.
	ErrBulkDataURI = errors.New("dicom: unsupported or invalid BulkDataURI")
	// ErrBulkDataTooLarge reports a referenced value larger than the
	// configured limit.
	ErrBulkDataTooLarge = errors.New("dicom: Bulk Data value exceeds the configured limit")
)

// BulkDataURIResolver opens the bytes a BulkDataURI references (PS3.18
// Annex F.2.6 and PS3.19 A.1.5). maxBytes is the largest value the caller
// accepts; a resolver fails with ErrBulkDataTooLarge, as early as it can,
// rather than return a larger value. The returned Size is exact and the caller
// owns and closes a non-nil Reader, also when an error is returned.
type BulkDataURIResolver interface {
	ResolveBulkDataURI(ctx context.Context, uri string, maxBytes int64) (BulkDataSource, error)
}

// BulkDataURIResolverFunc adapts a function to BulkDataURIResolver.
type BulkDataURIResolverFunc func(ctx context.Context, uri string, maxBytes int64) (BulkDataSource, error)

// ResolveBulkDataURI calls f.
func (f BulkDataURIResolverFunc) ResolveBulkDataURI(ctx context.Context, uri string, maxBytes int64) (BulkDataSource, error) {
	return f(ctx, uri, maxBytes)
}

// BulkDataURIResolvers dispatches on the lowercase URI scheme, for example
// "file" to a FileBulkDataResolver and "http" and "https" to a DICOMweb
// client. The empty key handles relative references, which DICOMweb servers
// commonly return. An unmapped scheme fails with ErrBulkDataURI.
type BulkDataURIResolvers map[string]BulkDataURIResolver

// ResolveBulkDataURI resolves uri with the resolver registered for its scheme.
func (r BulkDataURIResolvers) ResolveBulkDataURI(ctx context.Context, uri string, maxBytes int64) (BulkDataSource, error) {
	parsed, err := url.Parse(strings.TrimSpace(uri))
	if err != nil {
		return BulkDataSource{}, ErrBulkDataURI
	}
	resolver, ok := r[strings.ToLower(parsed.Scheme)]
	if !ok || resolver == nil {
		return BulkDataSource{}, fmt.Errorf("%w: no resolver for the URI scheme", ErrBulkDataURI)
	}
	return resolver.ResolveBulkDataURI(ctx, uri, maxBytes)
}

// FileBulkDataURI returns an absolute file: BulkDataURI for length bytes at
// offset in path, in the file:///path?offset=N&length=M form that
// FileBulkDataResolver reads. A negative length omits offset and length and
// references the whole file.
func FileBulkDataURI(path string, offset, length int64) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	slashed := filepath.ToSlash(absolute)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	u := url.URL{Scheme: "file", Path: slashed}
	if length >= 0 {
		if offset < 0 {
			return "", fmt.Errorf("%w: negative offset", ErrBulkDataURI)
		}
		u.RawQuery = "offset=" + strconv.FormatInt(offset, 10) + "&length=" + strconv.FormatInt(length, 10)
	}
	return u.String(), nil
}

// FileBulkDataResolver resolves file: BulkDataURIs. The optional offset and
// length query parameters select a byte range of the file (both are decimal
// byte counts; without length the range runs to the end of the file). Only
// local files inside Root are read: symbolic links are evaluated before the
// check, and URIs naming a remote host are rejected.
type FileBulkDataResolver struct {
	// Root confines resolution to this directory and is required.
	Root string
	// MaxBytes bounds each value; zero selects DefaultMaxBulkDataValueBytes.
	// The smaller of MaxBytes and the caller's maxBytes applies.
	MaxBytes int64
}

// ResolveBulkDataURI opens the referenced byte range.
func (r FileBulkDataResolver) ResolveBulkDataURI(ctx context.Context, uri string, maxBytes int64) (BulkDataSource, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return BulkDataSource{}, err
		}
	}
	if strings.TrimSpace(r.Root) == "" {
		return BulkDataSource{}, fmt.Errorf("%w: FileBulkDataResolver.Root is required", ErrBulkDataURI)
	}
	if r.MaxBytes < 0 || maxBytes < 0 {
		return BulkDataSource{}, fmt.Errorf("dicom: Bulk Data limits cannot be negative")
	}
	limit := effectiveBulkDataLimit(r.MaxBytes, maxBytes)
	path, offset, length, err := parseFileBulkDataURI(uri)
	if err != nil {
		return BulkDataSource{}, err
	}
	root, err := filepath.EvalSymlinks(r.Root)
	if err != nil {
		return BulkDataSource{}, fmt.Errorf("%w: resolver root is unavailable", ErrBulkDataURI)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return BulkDataSource{}, fmt.Errorf("%w: referenced file is unavailable", ErrBulkDataURI)
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return BulkDataSource{}, fmt.Errorf("%w: referenced file is outside the resolver root", ErrBulkDataURI)
	}
	file, err := os.Open(resolved)
	if err != nil {
		return BulkDataSource{}, fmt.Errorf("%w: referenced file cannot be opened", ErrBulkDataURI)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return BulkDataSource{}, fmt.Errorf("%w: referenced path is not a regular file", ErrBulkDataURI)
	}
	if offset > info.Size() {
		_ = file.Close()
		return BulkDataSource{}, fmt.Errorf("%w: offset is past the end of the file", ErrBulkDataURI)
	}
	if length < 0 {
		length = info.Size() - offset
	}
	if length > info.Size()-offset {
		_ = file.Close()
		return BulkDataSource{}, fmt.Errorf("%w: range is past the end of the file", ErrBulkDataURI)
	}
	if length > limit {
		_ = file.Close()
		return BulkDataSource{}, ErrBulkDataTooLarge
	}
	return BulkDataSource{Reader: sectionReadCloser{Reader: io.NewSectionReader(file, offset, length), Closer: file}, Size: length}, nil
}

type sectionReadCloser struct {
	io.Reader
	io.Closer
}

func parseFileBulkDataURI(uri string) (string, int64, int64, error) {
	parsed, err := url.Parse(strings.TrimSpace(uri))
	if err != nil || !strings.EqualFold(parsed.Scheme, "file") || parsed.Opaque != "" || parsed.User != nil || parsed.Fragment != "" {
		return "", 0, 0, ErrBulkDataURI
	}
	if parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost") {
		return "", 0, 0, fmt.Errorf("%w: remote file hosts are not supported", ErrBulkDataURI)
	}
	path := parsed.Path
	if runtime.GOOS == "windows" && len(path) >= 3 && path[0] == '/' && path[2] == ':' {
		path = path[1:]
	}
	path = filepath.FromSlash(path)
	if path == "" || !filepath.IsAbs(path) {
		return "", 0, 0, fmt.Errorf("%w: file URIs must be absolute", ErrBulkDataURI)
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return "", 0, 0, ErrBulkDataURI
	}
	offset, length := int64(0), int64(-1)
	for name, values := range query {
		if len(values) != 1 {
			return "", 0, 0, fmt.Errorf("%w: repeated query parameter", ErrBulkDataURI)
		}
		value, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || value < 0 {
			return "", 0, 0, fmt.Errorf("%w: offset and length must be non-negative integers", ErrBulkDataURI)
		}
		switch name {
		case "offset":
			offset = value
		case "length":
			length = value
		default:
			return "", 0, 0, fmt.Errorf("%w: unknown query parameter", ErrBulkDataURI)
		}
	}
	return filepath.Clean(path), offset, length, nil
}

func effectiveBulkDataLimit(configured, requested int64) int64 {
	limit := configured
	if limit <= 0 {
		limit = DefaultMaxBulkDataValueBytes
	}
	if requested > 0 && requested < limit {
		limit = requested
	}
	return limit
}

// ResolveBulkDataOptions bounds ResolveBulkData.
type ResolveBulkDataOptions struct {
	// MaxValueBytes bounds each value; zero selects
	// DefaultMaxBulkDataValueBytes.
	MaxValueBytes int64
	// MaxTotalBytes bounds the sum of all resolved values; zero selects
	// DefaultMaxBulkDataTotalBytes.
	MaxTotalBytes int64
}

// ResolveBulkData replaces every core.BulkDataValue in obj, including values
// nested in sequence items, with the bytes resolver returns for its URI. Each
// resolved value becomes a core.RawValue holding the Value Field as the
// reference supplied it (Little Endian for DICOMweb, PS3.18 8.7.3.5), with a
// defined length. The function does not synthesize encapsulated Pixel Data;
// keep such references and use NewBulkDataWriteResolver to stream them
// unchanged into a Part 10 file instead.
//
// Values are read fully into memory under the per-value and total limits.
// On error obj keeps the values resolved before the failure; the error names
// the tag but never the URI.
func ResolveBulkData(ctx context.Context, obj *Object, resolver BulkDataURIResolver, opts ResolveBulkDataOptions) error {
	if obj == nil {
		return nil
	}
	if resolver == nil {
		return fmt.Errorf("%w: a resolver is required", ErrBulkDataURI)
	}
	if opts.MaxValueBytes < 0 || opts.MaxTotalBytes < 0 {
		return fmt.Errorf("dicom: Bulk Data limits cannot be negative")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	state := &bulkDataResolution{
		ctx:      ctx,
		resolver: resolver,
		maxValue: effectiveBulkDataLimit(opts.MaxValueBytes, 0),
		maxTotal: opts.MaxTotalBytes,
	}
	if state.maxTotal == 0 {
		state.maxTotal = DefaultMaxBulkDataTotalBytes
	}
	for _, elem := range obj.SortedElements() {
		resolved, changed, err := state.element(elem)
		if err != nil {
			return err
		}
		if changed {
			obj.Put(resolved)
		}
	}
	return nil
}

type bulkDataResolution struct {
	ctx      context.Context
	resolver BulkDataURIResolver
	maxValue int64
	maxTotal int64
	total    int64
}

func (s *bulkDataResolution) element(elem core.Element) (core.Element, bool, error) {
	if err := s.ctx.Err(); err != nil {
		return elem, false, err
	}
	switch value := elem.Value.(type) {
	case core.BulkDataValue:
		data, err := s.read(value)
		if err != nil {
			return elem, false, fmt.Errorf("dicom: resolve Bulk Data for %s: %w", elem.Tag(), err)
		}
		if err := validateResolvedBulkDataLength(elem.VR(), len(data)); err != nil {
			return elem, false, fmt.Errorf("dicom: resolve Bulk Data for %s: %w", elem.Tag(), err)
		}
		elem.Value = core.RawValue(data)
		elem.Header.Length = core.Length(len(data))
		elem.Header.LengthSet = true
		return elem, true, nil
	case core.SequenceValue:
		var items []core.DataSet
		for itemIndex, item := range value.Items {
			var elements []core.Element
			for elementIndex, nested := range item.Elements {
				resolved, changed, err := s.element(nested)
				if err != nil {
					return elem, false, err
				}
				if !changed {
					continue
				}
				if elements == nil {
					elements = append([]core.Element(nil), item.Elements...)
				}
				elements[elementIndex] = resolved
			}
			if elements == nil {
				continue
			}
			if items == nil {
				items = append([]core.DataSet(nil), value.Items...)
			}
			items[itemIndex].Elements = elements
		}
		if items == nil {
			return elem, false, nil
		}
		elem.Value = core.SequenceValue{Items: items}
		return elem, true, nil
	}
	return elem, false, nil
}

func (s *bulkDataResolution) read(value core.BulkDataValue) ([]byte, error) {
	limit := s.maxValue
	if remaining := s.maxTotal - s.total; remaining < limit {
		limit = remaining
	}
	if limit <= 0 {
		return nil, ErrBulkDataTooLarge
	}
	source, err := s.resolver.ResolveBulkDataURI(s.ctx, value.URI, limit)
	if source.Reader != nil {
		defer func() { _ = source.Reader.Close() }()
	}
	if err != nil {
		return nil, err
	}
	if source.Reader == nil {
		return nil, fmt.Errorf("%w: resolver returned a nil Reader", ErrBulkDataURI)
	}
	if source.Size < 0 {
		return nil, fmt.Errorf("%w: resolver returned a negative size", ErrBulkDataURI)
	}
	if source.Size > limit {
		return nil, ErrBulkDataTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(contextBulkReader{ctx: s.ctx, reader: source.Reader}, source.Size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != source.Size {
		return nil, fmt.Errorf("dicom: Bulk Data source returned %d bytes, declared %d", len(data), source.Size)
	}
	s.total += source.Size
	return data, nil
}

// NewBulkDataWriteResolver adapts a BulkDataURIResolver to the
// BulkDataResolver of WriteFileOptions, so that a Part 10 writer streams each
// referenced value without holding it in memory. maxBytes bounds each value;
// zero selects DefaultMaxBulkDataValueBytes.
func NewBulkDataWriteResolver(ctx context.Context, resolver BulkDataURIResolver, maxBytes int64) BulkDataResolver {
	if ctx == nil {
		ctx = context.Background()
	}
	limit := effectiveBulkDataLimit(maxBytes, 0)
	return func(value core.BulkDataValue) (BulkDataSource, error) {
		if resolver == nil {
			return BulkDataSource{}, fmt.Errorf("%w: a resolver is required", ErrBulkDataURI)
		}
		if err := ctx.Err(); err != nil {
			return BulkDataSource{}, err
		}
		source, err := resolver.ResolveBulkDataURI(ctx, value.URI, limit)
		if err != nil {
			return source, err
		}
		if source.Size < 0 || source.Size > limit {
			return source, ErrBulkDataTooLarge
		}
		if source.Reader != nil {
			source.Reader = sectionReadCloser{Reader: contextBulkReader{ctx: ctx, reader: source.Reader}, Closer: source.Reader}
		}
		return source, nil
	}
}

type contextBulkReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextBulkReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func validateResolvedBulkDataLength(vr core.VR, size int) error {
	width := 1
	switch vr {
	case core.VRAT, core.VRFL, core.VROF, core.VRSL, core.VRUL, core.VROL:
		width = 4
	case core.VRFD, core.VROD, core.VRSV, core.VRUV, core.VROV:
		width = 8
	case core.VRSS, core.VRUS, core.VROW:
		width = 2
	}
	if size%width != 0 {
		return fmt.Errorf("dicom: resolved %s value length %d is not a multiple of %d", vr, size, width)
	}
	return nil
}
