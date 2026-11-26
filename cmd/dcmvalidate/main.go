// Command dcmvalidate checks DICOM Part 10 files against their Information
// Object Definition and the encoding rules, in the manner of dcm4che
// dcmvalidate and DCMTK dciodvfy:
//
//	dcmvalidate image.dcm
//	dcmvalidate -iod "Enhanced CT Image" -json a.dcm b.dcm
//
// Two checks run on each file. The IOD check (package validation/iod) finds
// missing or empty Type 1, 1C, 2 and 2C attributes of the Modules of the IOD
// the SOP Class names (or -iod), at every sequence depth. The encoding check
// (package validation) finds value representation, length, multiplicity,
// structure and File Meta consistency problems. Findings name attributes and
// modules but never print a value, so the report carries no patient data.
// Conditions that could not be evaluated are informational and listed only
// with -info.
//
// The exit status is 0 when no file has an error finding (warnings only
// with -strict), 1 when one has or cannot be read, and 2 on usage errors.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/clifile"
	"github.com/ThalesMMS/dicom-go/validation"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

var errUsage = errors.New("usage error")

type options struct {
	iod              string
	maxFindings      int
	json             bool
	strict           bool
	noEncoding       bool
	info             bool
	values           bool
	functionalGroups bool
	files            []string
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	opts := options{maxFindings: 1000}
	fs := flag.NewFlagSet("dcmvalidate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.iod, "iod", "", `IOD to check against, as named in PS3.3 without "IOD" (default: from the SOP Class UID)`)
	fs.IntVar(&opts.maxFindings, "max-findings", opts.maxFindings, "report at most this many findings per check and file")
	fs.BoolVar(&opts.json, "json", false, "write one JSON report per file instead of text")
	fs.BoolVar(&opts.strict, "strict", false, "also fail on warnings")
	fs.BoolVar(&opts.noEncoding, "iod-only", false, "skip the encoding check")
	fs.BoolVar(&opts.info, "info", false, "also list informational findings, such as conditions left for manual review")
	fs.BoolVar(&opts.functionalGroups, "functional-groups", false, "also check bounded Enhanced CT/MR/SEG functional group structure")
	fs.BoolVar(&opts.values, "values", false, "also check scoped PS3.3 enumerated values, extensible defined terms and contextual VM")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: dcmvalidate [-iod name] [-iod-only] [-values] [-functional-groups] [-info] [-strict] [-json] [-max-findings n] file...\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errUsage
	}
	if fs.NArg() == 0 || opts.maxFindings < 1 {
		fs.Usage()
		return opts, errUsage
	}
	opts.files = fs.Args()
	return opts, nil
}

// finding is one problem of either check, without attribute values.
type finding struct {
	Check    string                `json:"check"`
	Severity string                `json:"severity"`
	Code     string                `json:"code"`
	Path     string                `json:"path,omitempty"`
	Module   string                `json:"module,omitempty"`
	Type     string                `json:"type,omitempty"`
	Message  string                `json:"message,omitempty"`
	Rule     string                `json:"rule,omitempty"`
	Source   *iod.ConstraintSource `json:"source,omitempty"`
}

type fileReport struct {
	File            string    `json:"file"`
	IOD             string    `json:"iod,omitempty"`
	StandardVersion string    `json:"standard_version,omitempty"`
	Errors          int       `json:"errors"`
	Warnings        int       `json:"warnings"`
	Truncated       bool      `json:"truncated,omitempty"`
	Findings        []finding `json:"findings"`
	Failure         string    `json:"failure,omitempty"`
	info            bool
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	status := 0
	for _, path := range opts.files {
		report, err := validateFile(context.Background(), path, opts)
		if err != nil {
			clidiag.Fprintln(stderr, "dcmvalidate", fmt.Errorf("%s: %w", path, err))
			report = fileReport{File: path, Failure: string(clidiag.Classify(err)), Findings: []finding{}}
			status = 1
			if !opts.json {
				continue
			}
		}
		if report.Errors > 0 || (opts.strict && report.Warnings > 0) {
			status = 1
		}
		if opts.json {
			encoded, _ := json.Marshal(report)
			_, _ = fmt.Fprintln(stdout, string(encoded))
			continue
		}
		printText(stdout, report)
	}
	return status
}

func validateFile(ctx context.Context, path string, opts options) (fileReport, error) {
	file, err := clifile.ReadPart10(path)
	if err != nil {
		return fileReport{}, err
	}
	defer func() { _ = file.Close() }()
	report := fileReport{File: path, Findings: []finding{}, info: opts.info}
	var iodReport iod.Report
	iodOptions := iod.Options{IOD: opts.iod, MaxFindings: opts.maxFindings, CheckValues: opts.values}
	if opts.functionalGroups {
		iodReport, err = iod.ValidateWithFunctionalGroups(ctx, file.Dataset, iodOptions, iod.FunctionalGroupOptions{IOD: opts.iod, MaxFindings: opts.maxFindings})
	} else {
		iodReport, err = iod.Validate(file.Dataset, iodOptions)
	}
	if err != nil {
		return fileReport{}, err
	}
	report.IOD, report.StandardVersion = iodReport.IOD, iodReport.StandardVersion
	report.Truncated = iodReport.Truncated > 0
	for _, f := range iodReport.Findings {
		report.add(finding{Check: "iod", Severity: string(f.Severity), Code: string(f.Kind), Path: f.Path, Module: f.Module, Type: f.Type, Rule: f.Rule, Source: f.Source, Message: f.Message})
	}
	if !opts.noEncoding {
		order := file.TransferSyntax.ByteOrder
		if order == nil {
			order = binary.LittleEndian
		}
		meta := core.DataSet{}
		if file.Meta != nil {
			meta = file.Meta.ToDataSet()
		}
		encoding, err := validation.ValidateFile(ctx, meta, file.Dataset.ToDataSet(), file.TransferSyntax, validation.Options{
			Dictionary: std.Dictionary, MaxFindings: opts.maxFindings, ByteOrder: order,
		})
		if err != nil {
			return fileReport{}, err
		}
		report.Truncated = report.Truncated || encoding.Truncated
		for _, f := range encoding.Findings {
			path := f.Path.String()
			if path == "" {
				path = f.Tag.String()
			}
			report.add(finding{Check: "encoding", Severity: string(f.Severity), Code: string(f.Code), Path: path, Message: f.Message})
		}
	}
	return report, nil
}

func (r *fileReport) add(f finding) {
	switch f.Severity {
	case "error":
		r.Errors++
	case "warning":
		r.Warnings++
	default:
		if !r.info {
			return
		}
	}
	r.Findings = append(r.Findings, f)
}

func printText(w io.Writer, report fileReport) {
	iodName := report.IOD
	if iodName == "" {
		iodName = "unknown IOD"
	}
	truncated := ""
	if report.Truncated {
		truncated = " (truncated)"
	}
	_, _ = fmt.Fprintf(w, "%s: %s: %d error(s), %d warning(s)%s\n", report.File, iodName, report.Errors, report.Warnings, truncated)
	for _, f := range report.Findings {
		line := fmt.Sprintf("  %s %s %s", f.Severity, f.Code, f.Path)
		if f.Check == "iod" {
			line += fmt.Sprintf(" (Type %s, %s)", f.Type, f.Module)
			if f.Rule != "" {
				line += ": " + f.Message + " [rule=" + f.Rule + "]"
			}
			if f.Source != nil {
				line += fmt.Sprintf(" [source=%s %s row=%d index=%d depth=%d tag=%s section=%s sha256=%s]", f.Source.Edition, f.Source.TableID, f.Source.Row, f.Source.Index, f.Source.Depth, f.Source.Tag, f.Source.Section, f.Source.SHA256)
			}
		} else if f.Message != "" {
			line += ": " + f.Message
		}
		_, _ = fmt.Fprintln(w, line)
	}
}
