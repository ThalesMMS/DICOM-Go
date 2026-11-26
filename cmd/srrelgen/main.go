// Command srrelgen maintains the SR relationship content constraints of the
// PS3.3 SR Document IODs (Section A.35, Tables A.35.x-2).
//
// With -extract it reads the PS3.3 chtml section pages of an edition
// (sect_A.35*.html, saved from dicom.nema.org) and writes the vendored TSV
// internal/standard/sr_relationships.tsv. Without it, it turns that TSV into
// sr/relationship_constraints_gen.go. It depends on the standard library only,
// so it builds while the generated file is missing or stale.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

const (
	defaultInputPath  = "internal/standard/sr_relationships.tsv"
	defaultOutputPath = "sr/relationship_constraints_gen.go"
)

var errUsage = errors.New("usage error")

type options struct {
	extract string
	edition string
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
		table, err := extractDir(opts.extract, opts.edition)
		if err != nil {
			return fail(stderr, err)
		}
		if err := os.WriteFile(opts.output, formatTSV(table, opts.date), 0o644); err != nil {
			return fail(stderr, err)
		}
		_, _ = fmt.Fprintf(stdout, "extracted %s from %s (%d IODs, PS3.3 %s)\n", opts.output, opts.extract, len(table.IODs), table.Edition)
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
	_, _ = fmt.Fprintf(stdout, "generated %s from %s (%d IODs, PS3.3 %s)\n", opts.output, opts.input, len(table.IODs), table.Edition)
	return 0
}

func fail(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "srrelgen: %v\n", err)
	return 1
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("srrelgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opts options
	fs.StringVar(&opts.extract, "extract", "", "directory with the PS3.3 chtml pages sect_A.35*.html; writes the TSV to -output")
	fs.StringVar(&opts.edition, "edition", "", "PS3.3 edition of the extracted pages (default: read from the pages)")
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
		_, _ = fmt.Fprintf(stderr, "srrelgen: unexpected arguments %v\n", fs.Args())
		return options{}, errUsage
	}
	if opts.extract != "" && opts.output == defaultOutputPath {
		opts.output = defaultInputPath
	}
	return opts, nil
}
