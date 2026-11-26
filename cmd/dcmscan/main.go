// Command dcmscan reads chosen attributes from every DICOM file below the
// given files and directories, in the manner of GDCM gdcmscanner, and
// prints them as a table:
//
//	dcmscan -t Modality -t SeriesInstanceUID studies/
//	dcmscan -t "(0020,000D)" -t ReferencedSeriesSequence/SeriesInstanceUID -format json a.dcm dir/
//
// Only the metadata is read (package index): the reader stops at the
// top-level Pixel Data and never reads it, so large trees scan at the cost of
// their headers. Directories are walked without following links, with
// bounded parallel readers. Nested attributes are named by their sequence
// path with "/"; -all collects the values of every matching item.
//
// The output has one row per DICOM file, ordered by path: TSV with a header
// (multiple values joined by "\") or, with -format json, one JSON object per
// line. The values printed are exactly those the -t flags request and may
// contain patient data. Files below a directory that are not DICOM are
// counted on standard error, never named. The exit status is 0 on success, 1
// when a named file cannot be read or a directory scan fails, and 2 on usage
// errors.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/index"
	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/clifile"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

var errUsage = errors.New("usage error")

type tagList []string

func (l *tagList) String() string { return strings.Join(*l, ",") }

func (l *tagList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

type column struct {
	name     string
	path     []core.Tag
	tag      core.Tag
	selector string
}

type options struct {
	columns  []column
	format   string
	all      bool
	workers  int
	maxFiles int
	roots    []string
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	opts := options{format: "tsv"}
	var tags tagList
	fs := flag.NewFlagSet("dcmscan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Var(&tags, "t", "attribute to print, as a keyword or (gggg,eeee); nested ones as Sequence/.../Attribute (repeatable)")
	fs.StringVar(&opts.format, "format", opts.format, "output format: tsv or json")
	fs.BoolVar(&opts.all, "all", false, "print the values of every matching sequence item, not only the first")
	fs.IntVar(&opts.workers, "workers", 0, fmt.Sprintf("parallel readers per directory (0: the number of CPUs, at most %d)", index.MaxScanWorkers))
	fs.IntVar(&opts.maxFiles, "max-files", 100_000, "fail a directory with more files than this")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: dcmscan -t tag [-t tag...] [-format tsv|json] [-all] [-workers n] path...\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errUsage
	}
	if fs.NArg() == 0 || len(tags) == 0 || (opts.format != "tsv" && opts.format != "json") || opts.workers < 0 || opts.maxFiles < 1 {
		fs.Usage()
		return opts, errUsage
	}
	for i, spec := range tags {
		parts := strings.Split(spec, "/")
		col := column{name: spec, selector: "c" + strconv.Itoa(i)}
		for j, part := range parts {
			tag, err := clifile.ParseTag(strings.TrimSpace(part))
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "dcmscan: -t %q: %v\n", spec, err)
				return opts, errUsage
			}
			if j == len(parts)-1 {
				col.tag = tag
			} else {
				col.path = append(col.path, tag)
			}
		}
		opts.columns = append(opts.columns, col)
	}
	opts.roots = fs.Args()
	return opts, nil
}

// readOptions selects the requested attributes and nothing else.
func readOptions(opts options) index.Options {
	read := index.DefaultOptions()
	read.Profile = index.ProfileNone
	occurrence := index.OccurrenceFirst
	if opts.all {
		occurrence = index.OccurrenceAll
	}
	for _, col := range opts.columns {
		read.Selectors = append(read.Selectors, index.Selector{ID: col.selector, Path: col.path, Tag: col.tag, Occurrence: occurrence})
	}
	return read
}

type row struct {
	path   string
	values [][]string
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	rows, skipped, err := scan(context.Background(), opts)
	sort.Slice(rows, func(i, j int) bool { return rows[i].path < rows[j].path })
	write(stdout, opts, rows)
	if skipped > 0 {
		_, _ = fmt.Fprintf(stderr, "dcmscan: %d DICOM file(s), %d other file(s) skipped\n", len(rows), skipped)
	}
	if err != nil {
		clidiag.Fprintln(stderr, "dcmscan", err)
		return 1
	}
	return 0
}

func scan(ctx context.Context, opts options) ([]row, int, error) {
	read := readOptions(opts)
	var rows []row
	skipped := 0
	add := func(path string, record index.Record) {
		r := row{path: path, values: make([][]string, len(opts.columns))}
		order := byteOrder(record.FileMeta.TransferSyntaxUID)
		for i, col := range opts.columns {
			for _, selected := range record.Selected[col.selector] {
				r.values[i] = append(r.values[i], formatValues(selected.Element, order)...)
			}
		}
		rows = append(rows, r)
	}
	for i, root := range opts.roots {
		info, err := os.Stat(root)
		if err != nil {
			return rows, skipped, err
		}
		if !info.IsDir() {
			result, err := index.ReadPath(ctx, root, read)
			if err != nil {
				return rows, skipped, fmt.Errorf("input %d: %w", i+1, err)
			}
			add(root, result.Record)
			continue
		}
		scanOptions := index.DefaultScanOptions()
		scanOptions.ReadOptions = read
		scanOptions.Workers = opts.workers
		scanOptions.MaxFiles = opts.maxFiles
		scanOptions.MaxErrors = opts.maxFiles
		err = index.Scan(ctx, root, scanOptions, func(result index.ScanResult) error {
			if result.Err != nil {
				skipped++
				return nil
			}
			add(result.Path, result.Result.Record)
			return nil
		})
		if err != nil {
			return rows, skipped, fmt.Errorf("input %d: %w", i+1, err)
		}
	}
	return rows, skipped, nil
}

func byteOrder(transferSyntaxUID string) binary.ByteOrder {
	if syntax, ok := transfer.DefaultRegistry.Get(transferSyntaxUID); ok && syntax.ByteOrder != nil {
		return syntax.ByteOrder
	}
	return binary.LittleEndian
}

// formatValues renders the values of a primitive element: text as stored
// without padding, binary numbers decoded, AT as (gggg,eeee), and other
// binary values by their length only.
func formatValues(element core.Element, order binary.ByteOrder) []string {
	vr := element.VR()
	raw, _ := element.RawBytes()
	var out []string
	switch vr {
	case core.VRUS, core.VRSS, core.VRUL, core.VRSL, core.VRUV, core.VRSV, core.VRFL, core.VRFD:
		width := map[core.VR]int{core.VRUS: 2, core.VRSS: 2, core.VRUL: 4, core.VRSL: 4, core.VRFL: 4, core.VRUV: 8, core.VRSV: 8, core.VRFD: 8}[vr]
		for offset := 0; offset+width <= len(raw); offset += width {
			out = append(out, formatNumber(vr, raw[offset:offset+width], order))
		}
	case core.VRAT:
		for offset := 0; offset+4 <= len(raw); offset += 4 {
			out = append(out, core.NewTag(order.Uint16(raw[offset:]), order.Uint16(raw[offset+2:])).String())
		}
	case core.VROB, core.VROW, core.VROF, core.VROD, core.VROL, core.VROV, core.VRUN:
		out = append(out, fmt.Sprintf("<%d bytes>", len(raw)))
	default:
		out = element.StringValues()
	}
	return out
}

func formatNumber(vr core.VR, data []byte, order binary.ByteOrder) string {
	switch vr {
	case core.VRUS:
		return strconv.FormatUint(uint64(order.Uint16(data)), 10)
	case core.VRSS:
		return strconv.FormatInt(int64(int16(order.Uint16(data))), 10)
	case core.VRUL:
		return strconv.FormatUint(uint64(order.Uint32(data)), 10)
	case core.VRSL:
		return strconv.FormatInt(int64(int32(order.Uint32(data))), 10)
	case core.VRUV:
		return strconv.FormatUint(order.Uint64(data), 10)
	case core.VRSV:
		return strconv.FormatInt(int64(order.Uint64(data)), 10)
	case core.VRFL:
		return strconv.FormatFloat(float64(math.Float32frombits(order.Uint32(data))), 'g', -1, 32)
	default:
		return strconv.FormatFloat(math.Float64frombits(order.Uint64(data)), 'g', -1, 64)
	}
}

func write(w io.Writer, opts options, rows []row) {
	if opts.format == "json" {
		for _, r := range rows {
			values := make(map[string][]string, len(opts.columns))
			for i, col := range opts.columns {
				values[col.name] = append([]string{}, r.values[i]...)
			}
			encoded, _ := json.Marshal(struct {
				Path   string              `json:"path"`
				Values map[string][]string `json:"values"`
			}{r.path, values})
			_, _ = fmt.Fprintln(w, string(encoded))
		}
		return
	}
	header := []string{"path"}
	for _, col := range opts.columns {
		header = append(header, cell(col.name))
	}
	_, _ = fmt.Fprintln(w, strings.Join(header, "\t"))
	for _, r := range rows {
		cells := []string{cell(r.path)}
		for _, values := range r.values {
			cells = append(cells, cell(strings.Join(values, `\`)))
		}
		_, _ = fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
}

// cell keeps a TSV cell on one line.
func cell(text string) string {
	return strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(text)
}
