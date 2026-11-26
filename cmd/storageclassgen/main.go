// Command storageclassgen maintains the catalog of Storage SOP Classes
// (#1046) used by DIMSE reception, C-GET and storescp.
//
// With -extract it reads the PS3.4 chtml pages of an edition (sect_B.5.html,
// sect_B.6.html and sect_GG.3.html, saved from dicom.nema.org) together with
// the vendored PS3.6 Table A-1 (internal/standard/uids.tsv) and writes the
// vendored TSV internal/standard/storage_sop_classes.tsv: Tables B.5-1
// (current), B.6-1 (retired but listed) and GG.3-1 (non-patient objects),
// plus the PS3.6 Storage SOP Classes that PS3.4 does not list (retired ones,
// and those specified outside the standard such as DICOS and DICONDE). Every
// class is given a category and, when its IOD fixes one, the Modality; a
// name the rules do not recognize stops the extraction instead of being
// guessed. Without -extract it turns that TSV into dictionary/uid/storage_gen.go.
// It depends on the standard library only.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

const (
	defaultInputPath  = "internal/standard/storage_sop_classes.tsv"
	defaultOutputPath = "dictionary/uid/storage_gen.go"
	defaultUIDsPath   = "internal/standard/uids.tsv"
)

var errUsage = errors.New("usage error")

type options struct {
	extract string
	uids    string
	date    string
	input   string
	output  string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		if errors.Is(err, errUsage) {
			return 2
		}
		return fail(stderr, err)
	}
	if opts.extract != "" {
		uids, err := os.ReadFile(opts.uids)
		if err != nil {
			return fail(stderr, err)
		}
		table, err := extractDir(opts.extract, uids)
		if err != nil {
			return fail(stderr, err)
		}
		if err := os.WriteFile(opts.output, formatTSV(table, opts.date), 0o644); err != nil {
			return fail(stderr, err)
		}
		_, _ = fmt.Fprintf(stdout, "extracted %s from %s (%d SOP Classes, PS3.4 %s)\n", opts.output, opts.extract, len(table.Classes), table.Edition)
		return 0
	}
	data, err := os.ReadFile(opts.input)
	if err != nil {
		return fail(stderr, err)
	}
	table, err := parseTSV(data)
	if err != nil {
		return fail(stderr, err)
	}
	source, err := generateSource(opts.input, table)
	if err != nil {
		return fail(stderr, err)
	}
	if err := os.WriteFile(opts.output, source, 0o644); err != nil {
		return fail(stderr, err)
	}
	_, _ = fmt.Fprintf(stdout, "generated %s from %s (%d SOP Classes, PS3.4 %s)\n", opts.output, opts.input, len(table.Classes), table.Edition)
	return 0
}

func fail(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "storageclassgen: %v\n", err)
	return 1
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("storageclassgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opts options
	fs.StringVar(&opts.extract, "extract", "", "directory with the PS3.4 chtml pages sect_B.5.html, sect_B.6.html and sect_GG.3.html; writes the TSV to -output")
	fs.StringVar(&opts.uids, "uids", defaultUIDsPath, "vendored PS3.6 Table A-1 (with -extract)")
	fs.StringVar(&opts.date, "date", "", "extraction date recorded in the TSV header (YYYY-MM-DD)")
	fs.StringVar(&opts.input, "input", defaultInputPath, "vendored TSV")
	fs.StringVar(&opts.output, "output", defaultOutputPath, "output file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return options{}, err
		}
		return options{}, errUsage
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "storageclassgen: unexpected arguments %v\n", fs.Args())
		return options{}, errUsage
	}
	if opts.extract != "" {
		if opts.date == "" {
			_, _ = fmt.Fprintln(stderr, "storageclassgen: -extract needs -date")
			return options{}, errUsage
		}
		if opts.output == defaultOutputPath {
			opts.output = defaultInputPath
		}
	}
	return opts, nil
}
