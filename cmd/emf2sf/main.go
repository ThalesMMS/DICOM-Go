// Command emf2sf converts an Enhanced (or Legacy Converted Enhanced) CT, MR
// or PET multi-frame image into classic single-frame images, in the manner
// of dcm4che emf2sf, and with -reverse converts classic images of one series
// into one Legacy Converted Enhanced image:
//
//	emf2sf -o outdir enhanced.dcm                  outdir/enhanced-0001.dcm, ...
//	emf2sf -frames 1,5-8 -o outdir enhanced.dcm    selected frames (1-based)
//	emf2sf -reverse -o legacy.dcm slice*.dcm
//
// The conversions are those of package multiframe (ConvertToClassic and
// ConvertToLegacyEnhanced): functional groups become the attributes of each
// classic image and back, every result is a new series with new UIDs, Pixel
// Data is decoded and written native in Explicit VR Little Endian, and the
// Contributing Equipment Sequence records dicom-go as the converter. The
// inputs are never replaced and outputs are published through temporaries.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/clifile"
	"github.com/ThalesMMS/dicom-go/multiframe"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

var errUsage = errors.New("usage error")

var equipment = multiframe.ContributingEquipment{Manufacturer: "dicom-go", ManufacturerModelName: "emf2sf"}

type options struct {
	output    string
	frames    string
	seriesUID string
	reverse   bool
	inputs    []string
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet("emf2sf", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.output, "o", "", "output directory (or, with -reverse, output file)")
	fs.StringVar(&opts.frames, "frames", "", "frames to convert, 1-based, as a list of numbers and ranges such as 1,5-8 (default: all)")
	fs.StringVar(&opts.seriesUID, "series-uid", "", "Series Instance UID of the result (default: a new UID)")
	fs.BoolVar(&opts.reverse, "reverse", false, "convert classic images of one series into one Legacy Converted Enhanced image")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: emf2sf [-frames list] [-series-uid uid] -o outdir enhanced.dcm\n       emf2sf -reverse [-series-uid uid] -o out.dcm classic.dcm...\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errUsage
	}
	valid := opts.output != "" && fs.NArg() >= 1
	if opts.reverse {
		valid = valid && opts.frames == ""
	} else {
		valid = valid && fs.NArg() == 1
	}
	if opts.seriesUID != "" && !core.IsValidUID(opts.seriesUID) {
		_, _ = fmt.Fprintf(stderr, "emf2sf: -series-uid %q is not a valid UID\n", opts.seriesUID)
		return opts, errUsage
	}
	if !valid {
		fs.Usage()
		return opts, errUsage
	}
	opts.inputs = fs.Args()
	return opts, nil
}

// parseFrames reads "1,5-8" as zero-based frame indices.
func parseFrames(text string) ([]int, error) {
	if text == "" {
		return nil, nil
	}
	var indices []int
	for _, part := range strings.Split(text, ",") {
		first, last, isRange := strings.Cut(strings.TrimSpace(part), "-")
		from, err := strconv.Atoi(first)
		to := from
		if err == nil && isRange {
			to, err = strconv.Atoi(last)
		}
		if err != nil || from < 1 || to < from {
			return nil, fmt.Errorf("-frames %q: %q is not a frame number or range", text, part)
		}
		for frame := from; frame <= to; frame++ {
			indices = append(indices, frame-1)
		}
	}
	return indices, nil
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	frames, err := parseFrames(opts.frames)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "emf2sf: %v\n", err)
		return 2
	}
	if opts.seriesUID == "" {
		if opts.seriesUID, err = core.NewUID(); err != nil {
			clidiag.Fprintln(stderr, "emf2sf", err)
			return 1
		}
	}
	var summary string
	if opts.reverse {
		summary, err = toEnhanced(opts)
	} else {
		summary, err = toClassic(opts, frames)
	}
	if err != nil {
		clidiag.Fprintln(stderr, "emf2sf", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "emf2sf: "+summary)
	return 0
}

func toClassic(opts options, frames []int) (string, error) {
	input := opts.inputs[0]
	info, err := os.Stat(opts.output)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("-o %s is not a directory", filepath.Base(opts.output))
	}
	file, err := clifile.ReadPart10(input)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	registry, err := builtin.NewRegistry()
	if err != nil {
		return "", err
	}
	classic, report, err := multiframe.ConvertToClassic(context.Background(), file, multiframe.Options{
		FrameIndices:           frames,
		SeriesInstanceUID:      opts.seriesUID,
		SOPInstanceUIDForFrame: func(int) (string, error) { return core.NewUID() },
		DecoderRegistry:        registry,
		Equipment:              equipment,
	})
	if err != nil {
		return "", err
	}
	stem := strings.TrimSuffix(filepath.Base(input), filepath.Ext(input))
	for i, out := range classic {
		number := i + 1
		if frames != nil {
			number = frames[i] + 1
		}
		target := filepath.Join(opts.output, fmt.Sprintf("%s-%04d.dcm", stem, number))
		if err := clifile.WritePart10(target, out, input); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%d of %d frame(s) written as single-frame images", report.ConvertedFrames, report.SourceFrames), nil
}

func toEnhanced(opts options) (string, error) {
	sources := make([]*object.File, 0, len(opts.inputs))
	defer func() {
		for _, file := range sources {
			_ = file.Close()
		}
	}()
	for _, path := range opts.inputs {
		file, err := clifile.ReadPart10(path)
		if err != nil {
			return "", err
		}
		sources = append(sources, file)
	}
	registry, err := builtin.NewRegistry()
	if err != nil {
		return "", err
	}
	sopUID, err := core.NewUID()
	if err != nil {
		return "", err
	}
	result, report, err := multiframe.ConvertToLegacyEnhanced(context.Background(), sources, multiframe.LegacyOptions{
		SOPInstanceUID: sopUID, SeriesInstanceUID: opts.seriesUID, DecoderRegistry: registry, Equipment: equipment,
	})
	if err != nil {
		return "", err
	}
	if err := clifile.WritePart10(opts.output, result, opts.inputs...); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d frame(s) written as one Legacy Converted Enhanced image, ordered by %s", report.Frames, report.OrderedBy), nil
}
