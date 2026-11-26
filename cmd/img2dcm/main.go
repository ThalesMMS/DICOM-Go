// Command img2dcm imports raster samples or encapsulates existing image/video
// payloads into a new DICOM image. It does not encode or decode video.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/ThalesMMS/dicom-go/encapimage"
	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/clifile"
)

var errUsage = errors.New("usage error")

type options struct {
	image                 encapimage.ImageOptions
	vlp, newSC            bool
	studyFrom, seriesFrom string
	inputs                []string
	output                string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(runContext(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
func parseArgs(args []string, stderr io.Writer) (options, error) {
	var o options
	var format string
	var region encapimage.AnatomicRegion
	fs := flag.NewFlagSet("img2dcm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&region.CodeValue, "region-code", "", "video: anatomic Code Value")
	fs.StringVar(&region.CodingSchemeDesignator, "region-scheme", "", "video: Coding Scheme Designator")
	fs.StringVar(&region.CodeMeaning, "region-meaning", "", "video: Code Meaning")
	fs.StringVar(&format, "format", "auto", "auto, png, bmp, pnm, pgx, raw, jpeg, j2k, jls, video")
	fs.BoolVar(&o.newSC, "new-sc", false, "choose the Secondary Capture family (the default)")
	fs.BoolVar(&o.vlp, "vlp", false, "write one unsigned 8-bit VL Photographic image")
	fs.StringVar(&o.studyFrom, "study-from", "", "inherit patient and study modules from this Part 10 file")
	fs.StringVar(&o.seriesFrom, "series-from", "", "inherit patient, study and series identity from this Part 10 file")
	fs.StringVar(&o.image.PatientName, "patient-name", "", "Patient's Name")
	fs.StringVar(&o.image.PatientID, "patient-id", "", "Patient ID")
	fs.StringVar(&o.image.StudyInstanceUID, "study-uid", "", "Study Instance UID (generated when empty)")
	fs.StringVar(&o.image.SeriesInstanceUID, "series-uid", "", "Series Instance UID (generated when empty)")
	fs.StringVar(&o.image.SOPInstanceUID, "instance-uid", "", "new SOP Instance UID (generated when empty)")
	fs.StringVar(&o.image.SeriesDescription, "series-description", "", "Series Description")
	fs.StringVar(&o.image.InstanceNumber, "instance-number", "", "Instance Number (default 1)")
	fs.StringVar(&o.image.BurnedInAnnotation, "burned-in", "", "YES or NO (default conservatively YES)")
	fs.BoolVar(&o.image.KeepAPPn, "keep-appn", false, "JPEG only: retain APPn markers")
	fs.BoolVar(&o.image.KeepComments, "keep-com", false, "JPEG only: retain COM markers")
	fs.BoolVar(&o.image.DiscardICCProfile, "discard-icc", false, "JPEG only: discard embedded ICC profile")
	fs.BoolVar(&o.image.AllowRetiredProgressive, "allow-progressive", false, "JPEG only: allow retired progressive transfer syntax")
	fs.BoolVar(&o.image.J2KLossless, "j2k-lossless", false, "declare full reversible J2K codestream lossless; headers are checked")
	fs.Int64Var(&o.image.Limits.MaxInputBytes, "max-input-bytes", 256<<20, "maximum combined image input bytes (1 GiB hard ceiling)")
	fs.Int64Var(&o.image.Limits.MaxDecodedBytes, "max-decoded-bytes", 512<<20, "maximum admitted native sample span; not an RSS limit")
	fs.Int64Var(&o.image.Limits.MaxPixels, "max-pixels", 16<<20, "maximum pixels per frame")
	fs.IntVar(&o.image.Limits.MaxFrames, "max-frames", 1024, "maximum frames (100000 hard ceiling)")
	fs.IntVar(&o.image.Raw.Rows, "rows", 0, "raw only: rows")
	fs.IntVar(&o.image.Raw.Columns, "columns", 0, "raw only: columns")
	fs.IntVar(&o.image.Raw.Frames, "frames", 0, "raw only: frames per file (default 1)")
	fs.IntVar(&o.image.Raw.Samples, "samples", 0, "raw only: 1 or 3 interleaved samples (default 1)")
	fs.IntVar(&o.image.Raw.BitsAllocated, "bits-allocated", 0, "raw only: 8 or 16")
	fs.IntVar(&o.image.Raw.BitsStored, "bits-stored", 0, "raw only: precision (default BitsAllocated)")
	fs.BoolVar(&o.image.Raw.Signed, "signed", false, "raw only: signed samples with canonical sign extension")
	fs.StringVar(&o.image.Raw.ByteOrder, "endian", "", "raw only: little (default) or big")
	fs.Int64Var(&o.image.Raw.Offset, "offset", 0, "raw only: explicit skipped prefix bytes")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: img2dcm [flags] input [input ...] output.dcm\nFlags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return o, err
		}
		return o, errUsage
	}
	if fs.NArg() < 2 {
		fs.Usage()
		return o, errUsage
	}
	o.inputs = fs.Args()[:fs.NArg()-1]
	o.output = fs.Arg(fs.NArg() - 1)
	o.image.Format = encapimage.Format(strings.ToLower(format))
	switch o.image.Format {
	case encapimage.Auto, encapimage.PNG, encapimage.BMP, encapimage.PNM, encapimage.PGX, encapimage.Raw, encapimage.JPEG, encapimage.J2K, encapimage.JLS, encapimage.Video:
	default:
		return o, errUsage
	}
	if region != (encapimage.AnatomicRegion{}) {
		o.image.AnatomicRegion = &region
	}
	if o.vlp && o.newSC || o.studyFrom != "" && o.seriesFrom != "" {
		return o, errUsage
	}
	if o.vlp {
		o.image.SOPClassUID = encapimage.VLPhotographicImageStorage
	}
	if o.image.Limits.MaxInputBytes <= 0 || o.image.Limits.MaxInputBytes > 1<<30 || o.image.Limits.MaxDecodedBytes <= 0 || o.image.Limits.MaxPixels <= 0 || o.image.Limits.MaxFrames <= 0 || o.image.Limits.MaxFrames > 100000 {
		return o, errUsage
	}
	if o.image.Format != encapimage.Raw && o.image.Raw != (encapimage.RawOptions{}) {
		return o, errUsage
	}
	if o.image.Format == encapimage.Raw && (o.image.Raw.Rows <= 0 || o.image.Raw.Columns <= 0 || (o.image.Raw.BitsAllocated != 8 && o.image.Raw.BitsAllocated != 16)) {
		return o, errUsage
	}
	if o.image.J2KLossless && o.image.Format != encapimage.Auto && o.image.Format != encapimage.J2K {
		return o, errUsage
	}
	if o.newSC && o.image.Format == encapimage.Video {
		return o, errUsage
	}
	return o, nil
}
func run(args []string, stdout, stderr io.Writer) int {
	return runContext(context.Background(), args, stdout, stderr)
}
func runContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parseArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	rep, err := convert(ctx, o)
	if err != nil {
		clidiag.Fprintln(stderr, "img2dcm", err)
		return 1
	}
	fmt.Fprintf(stdout, "img2dcm: %dx%d, %d frame(s), %d bits, transfer syntax %s, encapsulated=%t, decoded=%t\n", rep.Columns, rep.Rows, rep.Frames, rep.BitsStored, rep.TransferSyntaxUID, rep.Encapsulated, rep.Decoded)
	return 0
}
func convert(ctx context.Context, o options) (encapimage.ImportReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return encapimage.ImportReport{}, err
	}
	sources := append([]string(nil), o.inputs...)
	sourcePath := o.studyFrom
	if o.seriesFrom != "" {
		sourcePath = o.seriesFrom
	}
	if sourcePath != "" {
		sources = append(sources, sourcePath)
	}
	for _, p := range sources {
		if samePath(o.output, p) {
			return encapimage.ImportReport{}, clifile.ErrOutputIsInput
		}
	}
	if len(o.inputs) > o.image.Limits.MaxFrames {
		return encapimage.ImportReport{}, encapimage.ErrImportLimit
	}
	inputs := make([][]byte, len(o.inputs))
	var total int64
	for i, p := range o.inputs {
		data, err := readInput(ctx, p, o.image.Limits.MaxInputBytes-total)
		if err != nil {
			return encapimage.ImportReport{}, err
		}
		inputs[i] = data
		total += int64(len(data))
	}
	if sourcePath != "" {
		source, closeSource, err := readSource(ctx, sourcePath)
		if err != nil {
			return encapimage.ImportReport{}, err
		}
		defer closeSource()
		if o.seriesFrom != "" {
			o.image.SeriesSource = source.Dataset
		} else {
			ds, _, err := source.Dataset.ConvertTextToUTF8(ctx, objectCharsetOptions())
			if err != nil {
				return encapimage.ImportReport{}, err
			}
			o.image.Common.Source = ds
		}
	}
	file, rep, err := encapimage.FromImages(ctx, inputs, o.image)
	if err != nil {
		return rep, err
	}
	if o.newSC && rep.Format == encapimage.Video {
		return rep, encapimage.ErrUnsupportedSOPClass
	}
	if err := ctx.Err(); err != nil {
		return rep, err
	}
	if err = file.RebuildFileMeta(); err != nil {
		return rep, err
	}
	err = clifile.WriteAtomic(o.output, func(w io.Writer) error { return writePart10Context(ctx, w, file) }, sources...)
	return rep, err
}
