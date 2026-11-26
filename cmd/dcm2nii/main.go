// Command dcm2nii writes a DICOM image series as a NIfTI-1 volume:
//
//	dcm2nii -o ct.nii.gz study/series/
//	dcm2nii -series 1.2.3 -scaling float32 -sidecar ct.json -o ct.nii study/
//
// The conversion is package nifti's (PlanVolume and Write): the slices are
// ordered along their normal and must form one regular grid (or, with
// -resample, a grid render can regularize by linear interpolation); the
// affine maps voxel indices to RAS+ millimetres; stored values keep one
// uniform rescale in scl_slope/scl_inter, or are rescaled to float32 or
// float64 with -scaling. VOI, MONOCHROME1 and other presentation transforms
// are not applied. Directories are searched recursively; when the inputs
// hold several series, -series selects one. -sidecar writes the closed,
// PHI-free JSON sidecar of the export. A .gz output (or -gz) is
// gzip-compressed. Outputs are published through temporaries.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/clifile"
	"github.com/ThalesMMS/dicom-go/nifti"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

var errUsage = errors.New("usage error")

var scalings = map[string]nifti.ScalingPolicy{
	"preserve": nifti.ScalingPreserveUniform,
	"float32":  nifti.ScalingApplyFloat32,
	"float64":  nifti.ScalingApplyFloat64,
}

var singleSlices = map[string]nifti.SingleSlicePolicy{
	"reject":     nifti.SingleSliceReject,
	"patient":    nifti.SingleSlicePatient,
	"image-grid": nifti.SingleSliceImageGrid,
}

type options struct {
	output      string
	series      string
	gzip        bool
	scaling     nifti.ScalingPolicy
	resample    bool
	singleSlice nifti.SingleSlicePolicy
	sidecar     string
	inputs      []string
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	var opts options
	var scaling, singleSlice string
	fs := flag.NewFlagSet("dcm2nii", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.output, "o", "", "output NIfTI file (.nii or .nii.gz)")
	fs.StringVar(&opts.series, "series", "", "Series Instance UID to convert when the inputs hold several series")
	fs.BoolVar(&opts.gzip, "gz", false, "gzip-compress the output (implied by a .gz output name)")
	fs.StringVar(&scaling, "scaling", "preserve", "stored values: preserve (one uniform rescale in scl_slope/scl_inter), float32 or float64 (rescaled values)")
	fs.BoolVar(&opts.resample, "resample", false, "linearly resample a series whose slices render can regularize (uneven spacing, gantry tilt)")
	fs.StringVar(&singleSlice, "single-slice", "reject", "one-frame series: reject, patient (needs patient geometry) or image-grid (no patient space)")
	fs.StringVar(&opts.sidecar, "sidecar", "", "also write the PHI-free JSON sidecar to this file")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: dcm2nii [-series uid] [-scaling preserve|float32|float64] [-resample] [-single-slice policy] [-gz] [-sidecar out.json] -o out.nii[.gz] input...\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errUsage
	}
	var okScaling, okSingle bool
	opts.scaling, okScaling = scalings[scaling]
	opts.singleSlice, okSingle = singleSlices[singleSlice]
	if fs.NArg() == 0 || opts.output == "" || !okScaling || !okSingle {
		fs.Usage()
		return opts, errUsage
	}
	opts.gzip = opts.gzip || strings.HasSuffix(strings.ToLower(opts.output), ".gz")
	opts.inputs = fs.Args()
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
	report, err := convert(context.Background(), opts)
	if err != nil {
		clidiag.Fprintln(stderr, "dcm2nii", err)
		return 1
	}
	d := report.Dimensions
	summary := fmt.Sprintf("dcm2nii: %dx%dx%d", d[0], d[1], d[2])
	if d[3] > 1 {
		summary += fmt.Sprintf("x%d", d[3])
	}
	summary += fmt.Sprintf(" voxels, %s, scaling %s", report.Sidecar.Datatype, report.ScalingPolicy)
	if report.Resampled {
		summary += ", resampled"
	}
	if len(report.Warnings) > 0 {
		summary += ", warnings: " + strings.Join(report.Warnings, ",")
	}
	_, _ = fmt.Fprintln(stdout, summary)
	return 0
}

func convert(ctx context.Context, opts options) (nifti.Report, error) {
	paths, err := clifile.SeriesFiles(ctx, opts.inputs, opts.series)
	if err != nil {
		return nifti.Report{}, err
	}
	options := nifti.DefaultOptions()
	options.Scaling = opts.scaling
	options.SingleSlice = opts.singleSlice
	if opts.resample {
		options.Geometry = nifti.GeometryResampleLinear
	}
	if opts.gzip {
		options.Compression = nifti.CompressionGZIP
	}
	plan, err := nifti.PlanVolume(ctx, nifti.NewPathSource(paths), options)
	if err != nil {
		return nifti.Report{}, err
	}
	var report nifti.Report
	err = clifile.WriteAtomic(opts.output, func(w io.Writer) error {
		var writeErr error
		report, writeErr = plan.Write(ctx, w)
		return writeErr
	}, paths...)
	if err != nil {
		return nifti.Report{}, err
	}
	if opts.sidecar != "" {
		encoded, err := report.Sidecar.Marshal(1 << 20)
		if err != nil {
			return nifti.Report{}, err
		}
		if err := clifile.WriteAtomic(opts.sidecar, func(w io.Writer) error { _, err := w.Write(encoded); return err }, paths...); err != nil {
			return nifti.Report{}, err
		}
	}
	return report, nil
}
