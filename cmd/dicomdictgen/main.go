package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/ThalesMMS/dicom-go/internal/clidiag"
)

const (
	defaultInputPath  = "internal/standard/dicom.dic"
	defaultOutputPath = "dictionary/std/std_gen.go"
)

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
		clidiag.Fprintln(stderr, "dicomdictgen", err)
		return 1
	}

	entries, err := parseFile(opts.input)
	if err != nil {
		clidiag.Fprintln(stderr, "dicomdictgen", err)
		return 1
	}

	header, err := scanHeader(opts.input)
	if err != nil {
		clidiag.Fprintln(stderr, "dicomdictgen", err)
		return 1
	}

	source, err := generateSource(opts.input, opts.commit, entries, header)
	if err != nil {
		clidiag.Fprintln(stderr, "dicomdictgen", err)
		return 1
	}

	if err := writeGeneratedFile(opts.output, source); err != nil {
		clidiag.Fprintln(stderr, "dicomdictgen", err)
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "generated %s from %s (%d entries)\n", opts.output, opts.input, len(entries))
	return 0
}

var (
	errUsage      = errors.New("usage error")
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

type options struct {
	input  string
	output string
	commit string
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	opts := options{
		input:  defaultInputPath,
		output: defaultOutputPath,
	}

	fs := flag.NewFlagSet("dicomdictgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.input, "input", defaultInputPath, "path to the versioned dicom.dic source file")
	fs.StringVar(&opts.output, "output", defaultOutputPath, "path to the generated Go output file")
	fs.StringVar(&opts.commit, "commit", "", "DCMTK commit the dicom.dic source was vendored from (recorded in the source URL)")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: %s [flags]\n\nGenerate dictionary/std from a DCMTK dicom.dic source file.\n\nFlags:\n", fs.Name())
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errUsage
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return opts, errUsage
	}
	if opts.commit != "" && !commitPattern.MatchString(opts.commit) {
		_, _ = fmt.Fprintf(stderr, "-commit must be a full 40-hex commit, got %q\n", opts.commit)
		return opts, errUsage
	}

	return opts, nil
}
