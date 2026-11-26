// Command srtemplategen maintains the catalog of PS3.16 SR templates (the
// template tables of Annex A) used by sr.StandardTemplates.
//
// With -extract it reads a local copy of the official DocBook (part16.xml,
// saved from https://dicom.nema.org/medical/dicom/current/source/docbook/part16/)
// and writes internal/standard/sr_templates.tsv, recording the edition, the
// SHA-256 of the DocBook and the extraction date. Without it, it turns that
// TSV into sr/template_catalog_gen.go (go generate in sr/). The command has no
// network client and depends on the standard library only.
//
//	go run ./cmd/srtemplategen -extract part16.xml -date YYYY-MM-DD
//	go run ./cmd/srtemplategen
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

const (
	defaultInputPath  = "internal/standard/sr_templates.tsv"
	defaultOutputPath = "sr/template_catalog_gen.go"
)

var errUsage = errors.New("usage error")

type options struct {
	extract, date, input, output string
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
		source, err := os.Open(opts.extract)
		if err != nil {
			return fail(stderr, err)
		}
		catalog, err := Extract(source, opts.date)
		_ = source.Close()
		if err != nil {
			return fail(stderr, err)
		}
		if err := os.WriteFile(opts.output, formatTSV(catalog), 0o644); err != nil {
			return fail(stderr, err)
		}
		_, _ = fmt.Fprintf(stdout, "extracted %s from %s (%d templates, PS3.16 %s)\n", opts.output, opts.extract, len(catalog.Templates), catalog.Edition)
		return 0
	}
	data, err := os.ReadFile(opts.input)
	if err != nil {
		return fail(stderr, err)
	}
	catalog, err := parseTSV(data)
	if err != nil {
		return fail(stderr, err)
	}
	source, err := generateSource(opts.input, data, catalog)
	if err != nil {
		return fail(stderr, err)
	}
	if err := os.WriteFile(opts.output, source, 0o644); err != nil {
		return fail(stderr, err)
	}
	_, _ = fmt.Fprintf(stdout, "generated %s from %s (%d templates, PS3.16 %s, %s)\n", opts.output, opts.input, len(catalog.Templates), catalog.Edition, catalog.Source)
	return 0
}

func fail(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "srtemplategen: %v\n", err)
	return 1
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("srtemplategen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opts options
	fs.StringVar(&opts.extract, "extract", "", "local part16.xml to extract; writes the TSV to -output")
	fs.StringVar(&opts.date, "date", "", "extraction date recorded in the TSV header (YYYY-MM-DD), required with -extract")
	fs.StringVar(&opts.input, "input", defaultInputPath, "catalog TSV")
	fs.StringVar(&opts.output, "output", defaultOutputPath, "output file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return options{}, err
		}
		return options{}, errUsage
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "srtemplategen: unexpected arguments %v\n", fs.Args())
		return options{}, errUsage
	}
	if opts.extract != "" {
		if opts.date == "" {
			_, _ = fmt.Fprintln(stderr, "srtemplategen: -extract needs -date")
			return options{}, errUsage
		}
		if opts.output == defaultOutputPath {
			opts.output = defaultInputPath
		}
	}
	return opts, nil
}
