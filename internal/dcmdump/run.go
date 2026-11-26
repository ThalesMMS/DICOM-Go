package dcmdump

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	clidiag "github.com/ThalesMMS/dicom-go/internal/clidiag/file"
	"github.com/ThalesMMS/dicom-go/internal/clireadopts"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const defaultMaxValueLen = 64

func Run(args []string, stdout, stderr io.Writer) int {
	opts, path, err := parseArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		if errors.Is(err, errUsage) {
			return 2
		}
		clidiag.Fprintln(stderr, "dcmdump", err)
		return 2
	}

	ctx := context.Background()
	if opts.writeValues != "" || opts.showOffsets {
		opts.sourceInfo, err = dumpSourceInfo(path)
		if err != nil {
			clidiag.Fprintln(stderr, "dcmdump", err)
			return 1
		}
	}
	file, replay, dataPath, inflated, wrapped, cleanup, err := openFileForDumpInput(ctx, path, opts)
	if err != nil {
		clidiag.Fprintln(stderr, "dcmdump", err)
		return 1
	}
	defer cleanup()
	if opts.sourceInfo != nil {
		after, statErr := dumpSourceInfo(path)
		if statErr != nil || !sameDumpSource(opts.sourceInfo, after) {
			clidiag.Fprintln(stderr, "dcmdump", errors.New("dump source changed during read"))
			return 1
		}
	}
	report, err := clireadopts.CollectFileReplay(ctx, file, opts.readPolicy, opts.readPolicy.Apply(object.ReadFileOptions{}), replay)
	if err == nil {
		err = addInflatedWrapperReport(&report, wrapped, opts.readPolicy.DiagnosticCap())
	}
	if err == nil {
		err = report.WriteTo(stderr, "dcmdump")
	}
	if err != nil {
		clidiag.Fprintln(stderr, "dcmdump", err)
		return 1
	}
	if resolution, ok := file.TransferSyntaxResolution(); ok && resolution.Inferred() {
		_, _ = fmt.Fprintf(
			stderr,
			"dcmdump: warning: inferred transfer syntax %s (%s), source=%s, confidence=%.3f\n",
			resolution.Syntax.UID,
			resolution.Syntax.Name,
			resolution.Source,
			resolution.Confidence,
		)
	}

	if opts.decodePrivateHeaders {
		if err := dumpPrivateHeaders(ctx, stdout, file.Dataset); err != nil {
			clidiag.Fprintln(stderr, "dcmdump", err)
			return 1
		}
		return 0
	}
	if opts.json {
		if err := dumpFileJSON(stdout, file); err != nil {
			clidiag.Fprintln(stderr, "dcmdump", err)
			return 1
		}
		return 0
	}
	if len(opts.searchTags) > 0 || len(opts.searchPaths) > 0 || opts.showPaths || opts.writeValues != "" {
		if err := runSelectedDump(ctx, stdout, path, dataPath, file, opts, inflated); err != nil {
			clidiag.Fprintln(stderr, "dcmdump", err)
			return 1
		}
		return 0
	}
	if opts.showOffsets {
		if err := dumpFileTextWithOffsetsConfigured(stdout, path, file, opts.maxValueLen, opts.readPolicy.Apply(object.ReadFileOptions{})); err != nil {
			clidiag.Fprintln(stderr, "dcmdump", err)
			return 1
		}
		return 0
	}

	formatter := NewFormatter(stdout, opts.maxValueLen)
	if err := formatter.DumpFile(file); err != nil {
		clidiag.Fprintln(stderr, "dcmdump", err)
		return 1
	}
	return 0
}

var errUsage = errors.New("usage error")

type options struct {
	privateHeaderOptions
	sourceInfo              os.FileInfo
	readPolicy              clireadopts.Options
	searchTags, searchPaths []string
	showPaths               bool
	writeValues             string
	maxExportFiles          int
	maxExportBytes          int64
	json                    bool
	maxValueLen             int
	showOffsets             bool
	recoverTransferSyntax   bool
}

func parseArgs(args []string, stderr io.Writer) (options, string, error) {
	opts := options{maxValueLen: defaultMaxValueLen}

	fs := flag.NewFlagSet("dcmdump", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts.privateHeaderOptions.register(fs)
	opts.readPolicy.Register(fs, true)
	fs.BoolVar(&opts.showPaths, "show-paths", false, "prefix text attributes with concrete numeric dataset/item paths")
	fs.StringVar(&opts.writeValues, "write-values", "", "export selected primitive source value spans to an existing directory; requires a selector")
	fs.IntVar(&opts.maxExportFiles, "max-export-files", 10000, "maximum exported primitive values (positive)")
	fs.Int64Var(&opts.maxExportBytes, "max-export-bytes", 1<<30, "maximum cumulative exported value bytes (positive)")
	add := func(target *[]string) func(string) error {
		return func(value string) error {
			if len(*target) >= 256 || len(value) > 4096 {
				return errors.New("selector limit exceeded")
			}
			*target = append(*target, value)
			return nil
		}
	}
	fs.Func("search-tag", "select this attribute at any depth by keyword or numeric tag (repeatable; text only)", add(&opts.searchTags))
	fs.Func("search-path", "select dataset attribute path with zero-based [n] or [*] ancestors (repeatable; text only)", add(&opts.searchPaths))
	fs.BoolVar(&opts.json, "json", false, "write combined file metadata and dataset as JSON")
	fs.IntVar(&opts.maxValueLen, "max-value", defaultMaxValueLen, "maximum bytes/characters to render for each value preview")
	fs.BoolVar(&opts.showOffsets, "show-offsets", false, "prefix text dump lines with the byte offset of each element header")
	fs.BoolVar(&opts.recoverTransferSyntax, "recover-transfer-syntax", false, "opt in to bounded, diagnostic transfer syntax recovery for malformed or raw files")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: %s [flags] <file.dcm>\n\nFlags:\n", fs.Name())
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, "", err
		}
		return opts, "", errUsage
	}
	if err := opts.readPolicy.Validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return opts, "", errUsage
	}
	if opts.decodePrivateHeaders && (opts.json || len(opts.searchTags) > 0 || len(opts.searchPaths) > 0 || opts.showPaths || opts.writeValues != "" || opts.showOffsets) {
		fmt.Fprintln(stderr, "-decode-additional-private-headers cannot be combined with JSON, selection, export or offsets")
		return opts, "", errUsage
	}
	if opts.maxExportFiles <= 0 || opts.maxExportBytes <= 0 || opts.writeValues != "" && len(opts.searchTags)+len(opts.searchPaths) == 0 || opts.json && (len(opts.searchTags)+len(opts.searchPaths) > 0 || opts.showPaths || opts.writeValues != "") {
		fmt.Fprintln(stderr, "invalid text selection/export policy")
		return opts, "", errUsage
	}
	if _, err := parseSelection(opts.searchTags, opts.searchPaths); err != nil {
		fmt.Fprintln(stderr, err)
		return opts, "", errUsage
	}
	if opts.maxValueLen <= 0 {
		_, _ = fmt.Fprintln(stderr, "-max-value must be greater than zero")
		fs.Usage()
		return opts, "", errUsage
	}
	if opts.recoverTransferSyntax && opts.showOffsets {
		_, _ = fmt.Fprintln(stderr, "-recover-transfer-syntax cannot be combined with -show-offsets")
		fs.Usage()
		return opts, "", errUsage
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return opts, "", errUsage
	}

	return opts, fs.Arg(0), nil
}

func openFileForDump(path string, recoverTransferSyntax bool) (*object.File, error) {
	if recoverTransferSyntax {
		file, _, err := object.OpenFileWithTransferSyntaxRecovery(
			path,
			object.ReadFileOptions{DeferPixelData: true},
			object.TransferSyntaxRecoveryOptions{
				AllowMissingPreamble:          true,
				AllowMissingFileMeta:          true,
				AllowMissingTransferSyntaxUID: true,
				AllowUnknownTransferSyntaxUID: true,
				AllowDeclaredMismatch:         true,
			},
		)
		return file, err
	}
	file, err := object.OpenFileWithOptions(path, object.ReadFileOptions{
		DeferPixelData: true,
	})
	if errors.Is(err, object.ErrDeferredValueRequiresSeekable) {
		return object.OpenFile(path)
	}
	return file, err
}

func dumpFileTextWithOffsets(w io.Writer, path string, file *object.File, maxValueLen int) error {
	return dumpFileTextWithOffsetsConfigured(w, path, file, maxValueLen, object.ReadFileOptions{})
}

func dumpFileTextWithOffsetsConfigured(w io.Writer, path string, file *object.File, maxValueLen int, read object.ReadFileOptions) error {
	if file == nil {
		return fmt.Errorf("dicom: nil file")
	}
	if file.TransferSyntax.Deflated {
		return fmt.Errorf("dcmdump: --show-offsets is not available for deflated transfer syntax %q", file.TransferSyntax.UID)
	}

	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()

	header, err := object.ReadPart10Header(source, read)
	if err != nil {
		return err
	}

	formatter := NewFormatterWithOptions(w, maxValueLen, FormatterOptions{ShowOffsets: true})
	if _, err := fmt.Fprintf(w, "Transfer Syntax: %s (%s)\n\n# File Meta\n", file.TransferSyntax.Name, file.TransferSyntax.UID); err != nil {
		return err
	}
	metaSyntax := transfer.ExplicitVRLittleEndian
	if read.AllowImplicitVRFileMeta {
		var vr [2]byte
		if _, err := source.ReadAt(vr[:], 136); err == nil {
			if _, err := core.ParseVR(string(vr[:])); err != nil {
				metaSyntax = transfer.ImplicitVRLittleEndian
			}
		}
	}
	metaOptions := parserReadOptions(read, 132)
	metaOptions.BulkDataDescriptor = nil
	metaReader := parser.NewReader(io.NewSectionReader(source, 132, header.DataSetOffset-132), metaSyntax, metaOptions)
	if err := formatter.DumpTokenStream(metaReader, metaSyntax, 0); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "\n%s\n# Dataset\n", sectionSeparator); err != nil {
		return err
	}
	if _, err := source.Seek(header.DataSetOffset, io.SeekStart); err != nil {
		return err
	}
	datasetOptions := parserReadOptions(read, header.DataSetOffset)
	datasetOptions.BulkDataDescriptor = nil
	datasetReader := parser.NewReader(source, file.TransferSyntax, datasetOptions)
	return formatter.DumpTokenStream(datasetReader, file.TransferSyntax, 0)
}

func dumpFileMetaWithOffsets(formatter *Formatter, r *bufio.Reader) (int64, error) {
	readerOpts := parser.ReaderOptions{
		Dictionary: std.Dictionary,
		BaseOffset: 132,
	}
	metaReader := parser.NewReader(r, transfer.ExplicitVRLittleEndian, readerOpts)

	offset := metaReader.Position()
	tok, err := metaReader.Next()
	if err != nil {
		return 0, err
	}
	if tok.Kind != parser.TokenElement || tok.Element.Tag() != core.NewTag(0x0002, 0x0000) {
		return 0, object.ErrInvalidFileMetaGroupLength
	}
	raw, ok := tok.Element.RawBytes()
	if !ok || len(raw) != 4 {
		return 0, object.ErrInvalidFileMetaGroupLengthValue
	}
	if err := formatter.dumpElementWithOffset(tok.Element, 0, transfer.ExplicitVRLittleEndian, offset); err != nil {
		return 0, err
	}

	metaLen := binary.LittleEndian.Uint32(raw)
	rest := io.LimitReader(r, int64(metaLen))
	restReader := parser.NewReader(rest, transfer.ExplicitVRLittleEndian, parser.ReaderOptions{
		Dictionary: std.Dictionary,
		BaseOffset: metaReader.Position(),
	})
	if err := formatter.DumpTokenStream(restReader, transfer.ExplicitVRLittleEndian, 0); err != nil {
		return 0, err
	}
	return restReader.Position(), nil
}

type fileJSON struct {
	TransferSyntax transferSyntaxJSON `json:"transferSyntax"`
	FileMeta       map[string]any     `json:"fileMeta"`
	DataSet        map[string]any     `json:"dataSet"`
}

type transferSyntaxJSON struct {
	UID        string  `json:"uid"`
	Name       string  `json:"name"`
	ExplicitVR bool    `json:"explicitVR"`
	Endianness string  `json:"endianness"`
	Inferred   bool    `json:"inferred,omitempty"`
	Source     string  `json:"source,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

func dumpFileJSON(w io.Writer, file *object.File) error {
	meta, err := marshalJSONObject(file.Meta)
	if err != nil {
		return err
	}
	dataset, err := marshalJSONObject(file.Dataset)
	if err != nil {
		return err
	}

	transferSyntax := transferSyntaxJSON{
		UID:        file.TransferSyntax.UID,
		Name:       file.TransferSyntax.Name,
		ExplicitVR: file.TransferSyntax.ExplicitVR,
		Endianness: endiannessForSyntax(file.TransferSyntax),
	}
	if resolution, ok := file.TransferSyntaxResolution(); ok && resolution.Inferred() {
		transferSyntax.Inferred = true
		transferSyntax.Source = string(resolution.Source)
		transferSyntax.Confidence = resolution.Confidence
	}

	data, err := json.MarshalIndent(fileJSON{
		TransferSyntax: transferSyntax,
		FileMeta:       meta,
		DataSet:        dataset,
	}, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func marshalJSONObject(obj *object.Object) (map[string]any, error) {
	if obj == nil {
		return map[string]any{}, nil
	}
	data, err := dicomjson.MarshalObject(obj)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	for _, elem := range obj.SortedElements() {
		if elem.VR() == core.VRUI {
			entry, ok := out[elem.Tag().HexString()].(map[string]any)
			if ok {
				addUIDNames(entry, elem.StringValues())
			}
		}
	}
	return out, nil
}

func addUIDNames(entry map[string]any, values []string) {
	if len(values) == 0 {
		return
	}

	names := make([]any, 0, len(values))
	for _, value := range values {
		name := nameForUID(value)
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return
	}
	entry["ValueNames"] = names
}

func endiannessForSyntax(syntax transfer.Syntax) string {
	if syntax.IsLittleEndian() {
		return "little"
	}
	return "big"
}

func openFileForDumpInput(ctx context.Context, path string, opts options) (*object.File, clireadopts.MetaReplay, string, bool, bool, func(), error) {
	noop := func() {}
	source, err := os.Open(path)
	if err != nil {
		return nil, clireadopts.MetaReplay{}, "", false, false, noop, err
	}
	if opts.sourceInfo != nil {
		info, statErr := source.Stat()
		if statErr != nil || !sameDumpSource(opts.sourceInfo, info) {
			source.Close()
			return nil, clireadopts.MetaReplay{}, "", false, false, noop, errors.New("dump source changed before read")
		}
	}
	replay, err := clireadopts.FileMetaReplay(source)
	if err != nil {
		source.Close()
		return nil, clireadopts.MetaReplay{}, "", false, false, noop, err
	}
	read := opts.readPolicy.Apply(object.ReadFileOptions{DeferPixelData: true})
	if opts.writeValues != "" {
		read.BulkDataDescriptor = &parser.BulkDataDescriptor{Rules: []parser.BulkDataRule{{Threshold: 0}}}
	}
	var file *object.File
	parse := func() (*object.File, error) {
		if _, err := source.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		if opts.recoverTransferSyntax {
			file, _, err := object.ReadFileWithTransferSyntaxRecovery(source, read, object.TransferSyntaxRecoveryOptions{AllowMissingPreamble: true, AllowMissingFileMeta: true, AllowMissingTransferSyntaxUID: true, AllowUnknownTransferSyntaxUID: true, AllowDeclaredMismatch: true})
			return file, err
		}
		return object.ReadFileWithOptions(source, read)
	}
	file, err = parse()
	if errors.Is(err, object.ErrDeferredValueRequiresSeekable) {
		if opts.writeValues != "" {
			f, spool, wrapped, cleanup, e := openInflatedForDump(ctx, source, read)
			if e != nil {
				source.Close()
				return nil, clireadopts.MetaReplay{}, "", false, false, noop, e
			}
			return f, replay, spool, true, wrapped, func() { cleanup(); _ = source.Close() }, nil
		}
		read.DeferPixelData = false
		file, err = parse()
	}
	if err != nil {
		source.Close()
		return nil, clireadopts.MetaReplay{}, "", false, false, noop, err
	}
	return file, replay, path, false, false, func() { _ = file.Close(); _ = source.Close() }, nil
}

// A Windows path Stat may defer loading its file ID until os.SameFile runs.
// Snapshot through an open handle so replacement cannot retarget that lazy
// identity, even when the new file has exactly the same size and timestamps.
func dumpSourceInfo(path string) (os.FileInfo, error) {
	f, err := object.OpenSourceFile(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return info, nil
}
