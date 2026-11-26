// Command pap2dcm converts a Papyrus 3.0 file into one DICOM instance per
// image, as GDCM "gdcmpap3 --split" does, but with the repairs of
// papyrus.Convert (reported on stdout):
//
//	pap2dcm study.pa3 outdir
//
// The instances are written as outdir/IMG0001.dcm, IMG0002.dcm, ...; the
// directory is created when missing and existing files are never replaced.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/papyrus"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

var errUsage = errors.New("usage error")

type options struct {
	convert       papyrus.Options
	input, output string
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet("pap2dcm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.convert.FallbackSOPClassUID, "sop-class", "", "SOP Class of images without a current standard image storage class (default Secondary Capture)")
	fs.IntVar(&opts.convert.MaxImages, "max-images", 0, "refuse files with more images (default 10000)")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: pap2dcm [flags] in.pa3 outdir\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errUsage
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return opts, errUsage
	}
	opts.input, opts.output = fs.Arg(0), fs.Arg(1)
	return opts, nil
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if err := convert(opts, stdout); err != nil {
		clidiag.Fprintln(stderr, "pap2dcm", err)
		return 1
	}
	return 0
}

func convert(opts options, stdout io.Writer) error {
	source, err := object.OpenFile(opts.input)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	images, report, err := papyrus.Convert(context.Background(), source, opts.convert)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(opts.output, 0o755); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "pap2dcm: %d images, transfer syntax %s\n", len(images), report.TransferSyntaxUID)
	for _, warning := range report.Warnings {
		_, _ = fmt.Fprintf(stdout, "pap2dcm: warning: %s\n", warning)
	}
	for i, image := range images {
		name := fmt.Sprintf("IMG%04d.dcm", i+1)
		if err := writeNew(filepath.Join(opts.output, name), image); err != nil {
			return err
		}
		r := report.Images[i]
		class := r.SOPClassUID
		if r.SourceSOPClassUID != "" {
			class += fmt.Sprintf(" (was %q)", r.SourceSOPClassUID)
		}
		_, _ = fmt.Fprintf(stdout, "pap2dcm: %s SOP Class %s; %d inherited, %d added, %d removed attributes\n", name, class, len(r.Inherited), len(r.Added), len(r.Removed))
		for _, change := range r.UIDs {
			_, _ = fmt.Fprintf(stdout, "pap2dcm: %s %s %s UID %q replaced by %s\n", name, change.Tag, change.Reason, change.Old, change.New)
		}
	}
	return nil
}

// writeNew writes file to path, which must not exist, removing a partial
// file on error.
func writeNew(path string, file *object.File) (err error) {
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	return object.WriteFile(out, file)
}
