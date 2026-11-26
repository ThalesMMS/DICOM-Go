// Package dicomconv implements the DICOM Part 10 <-> DICOM JSON (PS3.18
// Annex F) and Native DICOM Model XML (PS3.19 Annex A) conversions behind
// cmd/dcm2json, cmd/json2dcm, cmd/dcm2xml and cmd/xml2dcm.
package dicomconv

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dicomxml"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Format is the text encoding of a conversion.
type Format int

const (
	JSON Format = iota
	XML
)

var (
	// ErrBulkDataURI reports a BulkDataURI that is not a relative reference
	// inside the document's directory.
	ErrBulkDataURI = errors.New("dicomconv: BulkDataURI must be a relative path inside the document directory")
	// ErrTransferSyntax reports a missing or unusable transfer syntax.
	ErrTransferSyntax = errors.New("dicomconv: transfer syntax")
)

// EncodeOptions controls DICOM -> text conversion.
type EncodeOptions struct {
	Compact bool
	// WithMeta includes the File Meta Information (group 0002), so that the
	// transfer syntax and encapsulated Pixel Data survive a round trip.
	WithMeta bool
	// BulkDir, when set with BulkThreshold > 0, writes binary values of at
	// least BulkThreshold bytes to files in BulkDir and references them with
	// a BulkDataURI relative to BaseDir (the output document's directory).
	BulkDir       string
	BulkThreshold int
	BaseDir       string
	// BulkFile, when set with BulkThreshold > 0 and without BulkDir, appends
	// binary values of at least BulkThreshold bytes to this one file and
	// references each with an absolute file: BulkDataURI carrying its offset
	// and length (object.FileBulkDataURI), which object.FileBulkDataResolver
	// reads back.
	BulkFile string
	// KeepCharacterSet keeps the source Specific Character Set declaration.
	// By default text is converted to UTF-8 and declared ISO_IR 192, as the
	// JSON and XML documents themselves are UTF-8 (DCMTK dcm2json does the
	// same); a reader that trusts the original declaration would otherwise
	// mis-decode the text.
	KeepCharacterSet bool
	// PrivateRecovered, when set, makes XML output apply
	// dicomxml.Options.RecoverPrivate and receives each recovery.
	PrivateRecovered func(dicomxml.PrivateRecovery)
	// TextRecovered, when set, makes XML output apply
	// dicomxml.Options.RecoverText and receives each recovery.
	TextRecovered func(dicomxml.TextRecovery)
}

// Encode converts a Part 10 file to JSON or XML.
func Encode(ctx context.Context, file *object.File, format Format, opts EncodeOptions) ([]byte, error) {
	dataset := file.Dataset
	if !opts.KeepCharacterSet {
		converted, _, err := dataset.ConvertTextToUTF8(ctx, object.CharsetConversionOptions{})
		if err != nil {
			return nil, err
		}
		dataset = converted
	}
	if opts.WithMeta && file.Meta != nil {
		order := dataset.ValueByteOrder()
		elements := append(append([]core.Element{}, file.Meta.Elements()...), dataset.Elements()...)
		dataset = object.FromElements(elements, std.Dictionary)
		dataset.SetValueByteOrder(order)
	}
	bulk, err := newBulkWriter(opts)
	if err != nil {
		return nil, err
	}
	if bulk != nil && bulk.file != nil {
		defer func() { _ = bulk.file.Close() }()
	}
	var out []byte
	switch format {
	case JSON:
		jsonOpts := dicomjson.DefaultOptions()
		jsonOpts.Pretty = !opts.Compact
		if bulk != nil {
			jsonOpts.BulkDataURIFunc = bulk.uri
		}
		out, err = dicomjson.MarshalContext(ctx, dataset, jsonOpts)
	case XML:
		xmlOpts := dicomxml.DefaultOptions()
		xmlOpts.Pretty = !opts.Compact
		xmlOpts.Dictionary = std.Dictionary
		xmlOpts.RecoverPrivate = opts.PrivateRecovered != nil
		xmlOpts.PrivateRecovered = opts.PrivateRecovered
		xmlOpts.RecoverText = opts.TextRecovered != nil
		xmlOpts.TextRecovered = opts.TextRecovered
		if bulk != nil {
			xmlOpts.BulkDataURIFunc = bulk.uri
		}
		out, err = dicomxml.MarshalContext(ctx, dataset, xmlOpts)
	default:
		return nil, fmt.Errorf("dicomconv: unknown format %d", format)
	}
	if err == nil && bulk != nil {
		err = bulk.err
	}
	if err == nil && bulk != nil && bulk.file != nil {
		err = bulk.file.Close()
	}
	return out, err
}

type bulkWriter struct {
	dir, prefix string
	threshold   int
	count       int
	err         error
	// file, path and offset hold the single-file (file: URI) mode.
	file   *os.File
	path   string
	offset int64
}

func newBulkWriter(opts EncodeOptions) (*bulkWriter, error) {
	if opts.BulkFile != "" && opts.BulkDir != "" {
		return nil, fmt.Errorf("%w: -bulk-dir and -bulk-file are mutually exclusive", ErrBulkDataURI)
	}
	if opts.BulkFile != "" && opts.BulkThreshold > 0 {
		path, err := filepath.Abs(opts.BulkFile)
		if err != nil {
			return nil, err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		return &bulkWriter{threshold: opts.BulkThreshold, file: file, path: path}, nil
	}
	if opts.BulkDir == "" || opts.BulkThreshold <= 0 {
		return nil, nil
	}
	if err := os.MkdirAll(opts.BulkDir, 0o755); err != nil {
		return nil, err
	}
	base := opts.BaseDir
	if base == "" {
		base = "."
	}
	rel, err := filepath.Rel(base, opts.BulkDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("%w: the bulk directory must be inside the output directory", ErrBulkDataURI)
	}
	return &bulkWriter{dir: opts.BulkDir, prefix: filepath.ToSlash(rel), threshold: opts.BulkThreshold}, nil
}

func (b *bulkWriter) uri(tag core.Tag, vr core.VR, data []byte) string {
	if b.err != nil || len(data) < b.threshold {
		return ""
	}
	if b.file != nil {
		uri, err := object.FileBulkDataURI(b.path, b.offset, int64(len(data)))
		if err == nil {
			_, err = b.file.Write(data)
		}
		if err != nil {
			b.err = err
			return ""
		}
		b.offset += int64(len(data))
		return uri
	}
	b.count++
	name := fmt.Sprintf("%04X%04X-%d.bin", tag.Group, tag.Element, b.count)
	if err := os.WriteFile(filepath.Join(b.dir, name), data, 0o644); err != nil {
		b.err = err
		return ""
	}
	return path.Join(b.prefix, name)
}

// DecodeOptions controls text -> DICOM conversion.
type DecodeOptions struct {
	// BaseDir resolves relative BulkDataURIs; empty rejects BulkDataURIs.
	BaseDir string
	// TransferSyntaxUID is used when the document carries no File Meta
	// Information; empty means Explicit VR Little Endian.
	TransferSyntaxUID string
	// CharacterSet, when set, re-encodes text into this Specific Character
	// Set (for example "ISO_IR 192"), with Unrepresentable deciding what
	// happens to characters it cannot hold.
	CharacterSet    string
	Unrepresentable object.UnrepresentablePolicy
	// MaxBulkBytes bounds each resolved BulkData file (default 1 GiB).
	MaxBulkBytes int64
}

// Decode converts JSON or XML to a Part 10 file.
func Decode(ctx context.Context, data []byte, format Format, opts DecodeOptions) (*object.File, error) {
	syntax, err := documentTransferSyntax(data, format, opts.TransferSyntaxUID)
	if err != nil {
		return nil, err
	}
	var dataset *object.Object
	switch format {
	case JSON:
		if data, err = inlineJSONBulkData(data, opts); err != nil {
			return nil, err
		}
		jsonOpts := dicomjson.DefaultUnmarshalOptions()
		jsonOpts.TransferSyntax = syntax
		dataset, err = dicomjson.UnmarshalContext(ctx, data, std.Dictionary, jsonOpts)
	case XML:
		xmlOpts := dicomxml.DefaultUnmarshalOptions()
		xmlOpts.TransferSyntax = syntax
		xmlOpts.BulkDataPolicy = dicomxml.RejectBulkData
		if opts.BaseDir != "" {
			xmlOpts.BulkDataPolicy = dicomxml.ResolveBulkData
			xmlOpts.BulkDataResolver = func(_ context.Context, ref dicomxml.BulkDataReference) (io.ReadCloser, error) {
				resolved, err := resolveBulkPath(opts.BaseDir, ref.URI)
				if err != nil {
					return nil, err
				}
				if err := checkBulkSize(resolved, bulkLimit(opts)); err != nil {
					return nil, err
				}
				return os.Open(resolved)
			}
		}
		dataset, err = dicomxml.UnmarshalContext(ctx, data, std.Dictionary, xmlOpts)
	default:
		return nil, fmt.Errorf("dicomconv: unknown format %d", format)
	}
	if err != nil {
		return nil, err
	}
	var metaElements, dataElements []core.Element
	for _, element := range dataset.Elements() {
		if element.Tag().Group == 0x0002 {
			if element.Tag().Element != 0x0000 {
				metaElements = append(metaElements, element)
			}
			continue
		}
		dataElements = append(dataElements, element)
	}
	body := object.FromElements(dataElements, std.Dictionary)
	body.SetValueByteOrder(dataset.ValueByteOrder())
	if opts.CharacterSet != "" {
		converted, _, err := body.ConvertTextToCharacterSet(ctx, opts.CharacterSet, object.CharsetEncodeOptions{Unrepresentable: opts.Unrepresentable})
		if err != nil {
			return nil, err
		}
		body = converted
	}
	file := &object.File{Dataset: body, TransferSyntax: syntax}
	if len(metaElements) != 0 {
		file.Meta = object.FromElements(metaElements, std.Dictionary)
	}
	return file, nil
}

// documentTransferSyntax reads (0002,0010) from the document, else uses the
// fallback UID.
func documentTransferSyntax(data []byte, format Format, fallback string) (transfer.Syntax, error) {
	uid := strings.TrimSpace(fallback)
	switch format {
	case JSON:
		var probe map[string]struct {
			Value []any `json:"Value"`
		}
		if err := json.Unmarshal(data, &probe); err == nil {
			if element, ok := probe["00020010"]; ok && len(element.Value) == 1 {
				if value, ok := element.Value[0].(string); ok {
					uid = value
				}
			}
		}
	case XML:
		if value, ok := xmlTransferSyntax(data); ok {
			uid = value
		}
	}
	if uid == "" {
		return transfer.ExplicitVRLittleEndian, nil
	}
	syntax, ok := transfer.DefaultRegistry.Get(transfer.NormalizeUID(uid))
	if !ok || !syntax.Supported {
		return transfer.Syntax{}, fmt.Errorf("%w %q is not supported for writing", ErrTransferSyntax, uid)
	}
	return syntax, nil
}

// xmlTransferSyntax finds the first Value of the (0002,0010) attribute
// without decoding the document, whose encapsulated Pixel Data needs the
// transfer syntax first.
func xmlTransferSyntax(data []byte) (string, bool) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.CharsetReader = dicomxml.CharsetReader
	inside, inValue := false, false
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", false
		}
		switch element := token.(type) {
		case xml.StartElement:
			if element.Name.Local == "DicomAttribute" {
				inside = false
				for _, attr := range element.Attr {
					if attr.Name.Local == "tag" && strings.EqualFold(attr.Value, "00020010") {
						inside = true
					}
				}
			}
			inValue = inside && element.Name.Local == "Value"
		case xml.CharData:
			if inValue {
				return strings.TrimSpace(string(element)), true
			}
		case xml.EndElement:
			if element.Name.Local == "DicomAttribute" {
				inside = false
			}
			inValue = false
		}
	}
}

// resolveBulkPath maps a relative BulkDataURI to an existing file inside base.
// Symbolic links (and, on Windows, junctions) in base and in the referenced
// path are evaluated before the containment check, as
// object.FileBulkDataResolver does, so a link inside base cannot reach a file
// outside it. The returned path is the evaluated one.
func resolveBulkPath(base, uri string) (string, error) {
	if uri == "" || strings.Contains(uri, ":") || strings.HasPrefix(uri, "/") || strings.Contains(uri, `\`) {
		return "", ErrBulkDataURI
	}
	clean := path.Clean(uri)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", ErrBulkDataURI
	}
	root, err := filepath.Abs(base)
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		return "", fmt.Errorf("%w: the document directory is unavailable", ErrBulkDataURI)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(clean)))
	if err != nil {
		return "", fmt.Errorf("%w: the referenced file is unavailable", ErrBulkDataURI)
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: the referenced file is outside the document directory", ErrBulkDataURI)
	}
	return resolved, nil
}

// bulkLimit is the most bytes one resolved BulkData file may hold:
// opts.MaxBulkBytes, 1 GiB when it is not positive.
func bulkLimit(opts DecodeOptions) int64 {
	if opts.MaxBulkBytes <= 0 {
		return 1 << 30
	}
	return opts.MaxBulkBytes
}

// checkBulkSize refuses a resolved BulkData file larger than limit, for the
// JSON and the XML paths alike.
func checkBulkSize(path string, limit int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() > limit {
		return fmt.Errorf("%w: a bulk data file exceeds %d bytes", ErrBulkDataURI, limit)
	}
	return nil
}

// inlineJSONBulkData replaces every BulkDataURI with the InlineBinary of the
// file it names, so that the JSON decoder rebuilds the value as it would
// from inline data (encapsulated Pixel Data included).
func inlineJSONBulkData(data []byte, opts DecodeOptions) ([]byte, error) {
	if !bytes.Contains(data, []byte(`"BulkDataURI"`)) {
		return data, nil
	}
	if opts.BaseDir == "" {
		return nil, fmt.Errorf("%w: no document directory to resolve against", ErrBulkDataURI)
	}
	limit := bulkLimit(opts)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	var walk func(any) error
	walk = func(node any) error {
		switch value := node.(type) {
		case map[string]any:
			if uri, ok := value["BulkDataURI"].(string); ok {
				resolved, err := resolveBulkPath(opts.BaseDir, uri)
				if err != nil {
					return err
				}
				if err := checkBulkSize(resolved, limit); err != nil {
					return err
				}
				content, err := os.ReadFile(resolved)
				if err != nil {
					return err
				}
				delete(value, "BulkDataURI")
				value["InlineBinary"] = base64.StdEncoding.EncodeToString(content)
			}
			for _, child := range value {
				if err := walk(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range value {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(document); err != nil {
		return nil, err
	}
	return json.Marshal(document)
}

// ReadPart10 reads a DICOM file from path.
func ReadPart10(path string) (*object.File, error) {
	source, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = source.Close() }()
	return object.ReadFile(source)
}

// WritePart10 writes file to path through a temporary file in the same
// directory.
func WritePart10(target string, file *object.File) error {
	if err := file.RebuildFileMeta(); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".dicomconv-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if err := object.WriteFile(temp, file); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), target)
}
