// Command dcmodify performs ordered, bounded edits on DICOM Part 10 files.
// Files are published individually only after every edit succeeds.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ThalesMMS/dicom-go/attrpath"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/curated"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/clireadopts"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

var errUsage = errors.New("usage error")

type options struct {
	readPolicy                                  clireadopts.Options
	edits                                       []string
	jobs                                        []rawAction
	conditionText                               []string
	textBytes                                   int
	scriptPath, output, original, system, input string
	inputs                                      []string
	inPlace, private, ignoreMissing, createFile bool
	maxBatch                                    int
	maxValueFile, maxValueSources               int64
	editLimits                                  attrpath.EditLimits
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet("dcmodify", flag.ContinueOnError)
	// flag's default invalid-value diagnostic echoes user input. Usage is
	// rendered explicitly, without those potentially sensitive arguments.
	fs.SetOutput(io.Discard)
	opts.readPolicy.Register(fs, true)
	for _, x := range [][2]string{{"i", "insert"}, {"m", "modify"}, {"e", "remove"}, {"e-if-present", "remove-if-present"}} {
		fs.Var(actionFlag{kind: x[1], opts: &opts}, x[0], "ordered edit with attribute path")
	}
	fs.StringVar(&opts.scriptPath, "script", "", "bounded file with insert/modify/remove/create edits, after command-line actions")
	fs.StringVar(&opts.output, "o", "", "output file, with exactly one input")
	fs.BoolVar(&opts.inPlace, "in-place", false, "replace each input after all its edits succeed")
	fs.BoolVar(&opts.private, "curated-private", false, "resolve private creators with the curated catalog")
	fs.StringVar(&opts.original, "original-attributes", "", "record previous values: correct or coerce")
	fs.StringVar(&opts.system, "modifying-system", "dicom-go dcmodify", "Modifying System for Original Attributes")
	registerParity(fs, &opts)
	fs.Usage = func() {
		fmt.Fprintln(stderr, `Usage: dcmodify [ordered edits] [-iff condition] (-o out | -in-place) input...

Encoded value files: even bytes, binary source little endian, target VR/charset known.
Recovery is off by default; editing does not validate a complete IOD.
Flags:`)
		fs.SetOutput(stderr)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)
	}
	if err := fs.Parse(normalizeCreateFlag(args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errUsage
	}
	if err := opts.readPolicy.Validate(); err != nil {
		return opts, errUsage
	}
	switch strings.ToLower(opts.original) {
	case "", "correct", "coerce":
	default:
		return opts, errUsage
	}
	if fs.NArg() < 1 || (opts.output == "") == !opts.inPlace || len(opts.jobs) == 0 && opts.scriptPath == "" {
		return opts, errUsage
	}
	if opts.maxBatch <= 0 || fs.NArg() > opts.maxBatch || fs.NArg() > 1 && !opts.inPlace || opts.maxValueFile <= 0 || opts.maxValueSources <= 0 {
		return opts, errUsage
	}
	l := opts.editLimits
	for _, n := range []int{l.MaxDepth, l.MaxElements, l.MaxItems, l.MaxVisits, l.MaxMatches, l.MaxPathBytes, l.MaxTouches, l.MaxCreatedItems} {
		if n <= 0 {
			return opts, errUsage
		}
	}
	if l.MaxValueBytes <= 0 {
		return opts, errUsage
	}
	opts.inputs = append([]string(nil), fs.Args()...)
	opts.input = opts.inputs[0]
	return opts, nil
}

func run(args []string, stdout, stderr io.Writer) int {
	return runContext(context.Background(), args, stdout, stderr, core.NewUID)
}
func runContext(ctx context.Context, args []string, stdout, stderr io.Writer, newUID func() (string, error)) int {
	opts, err := parseArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "dcmodify: invalid options or edit syntax")
		return 2
	}
	var dict dictionary.DataDictionary = std.Dictionary
	if opts.private {
		catalog, e := curated.New()
		if e != nil {
			fmt.Fprintln(stderr, "dcmodify: dictionary unavailable")
			return 1
		}
		dict = dictionary.Chain{catalog, std.Dictionary}
	}
	jobs, conditions, err := compileActions(opts, dict)
	if err != nil {
		fmt.Fprintln(stderr, "dcmodify: "+safeCLIError(err))
		return 2
	}
	if err := preflightInputs(opts, jobs); err != nil {
		fmt.Fprintln(stderr, "dcmodify: input or output selection rejected")
		return 1
	}
	values, err := snapshotValues(ctx, opts, jobs)
	if err != nil {
		fmt.Fprintln(stderr, "dcmodify: "+safeCLIError(err))
		return 1
	}
	changed, skipped := 0, 0
	for i, input := range opts.inputs {
		perFile := opts
		perFile.input = input
		n, skip, e := executeFile(ctx, perFile, jobs, conditions, values, dict, stderr, newUID)
		if e != nil {
			fmt.Fprintf(stderr, "dcmodify: file %d: %s\n", i+1, safeCLIError(e))
			return 1
		}
		changed += n
		if skip {
			skipped++
		}
	}
	if len(opts.inputs) == 1 {
		fmt.Fprintf(stdout, "dcmodify: %d attribute(s) or item(s) changed\n", changed)
	} else {
		fmt.Fprintf(stdout, "dcmodify: %d file(s) processed, %d skipped, %d attribute(s) or item(s) changed\n", len(opts.inputs), skipped, changed)
	}
	return 0
}

func buildScript(opts options, dict dictionary.DataDictionary) (attrpath.Script, error) {
	var b strings.Builder
	for _, edit := range opts.edits {
		if len(edit)+1 > attrpath.MaxScriptBytes-b.Len() {
			return nil, attrpath.ErrLimit
		}
		b.WriteString(edit)
		b.WriteByte('\n')
	}
	if opts.scriptPath != "" {
		data, err := readScript(opts.scriptPath, attrpath.MaxScriptBytes-b.Len())
		if err != nil {
			return nil, err
		}
		b.Write(data)
	}
	return attrpath.ParseScript(b.String(), dict)
}

// Preserve the existing internal adapter used by regression tests.
func modifyFile(opts options, script attrpath.Script, dict dictionary.DataDictionary) (int, error) {
	return modifyFileWithDiagnostics(opts, script, dict, io.Discard)
}
func modifyFileWithDiagnostics(opts options, script attrpath.Script, dict dictionary.DataDictionary, diagnostics io.Writer) (int, error) {
	jobs := make([]action, 0, len(script))
	for _, edit := range script {
		jobs = append(jobs, action{kind: "edit", edit: edit})
	}
	n, _, err := executeFile(context.Background(), opts, jobs, nil, nil, dict, diagnostics, core.NewUID)
	return n, err
}
