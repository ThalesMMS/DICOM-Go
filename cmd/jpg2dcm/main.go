// Command jpg2dcm encapsulates a JPEG file in a Secondary Capture or VL
// Photographic Image object without re-encoding it, in the manner of DCMTK
// img2dcm:
//
//	jpg2dcm -vlp -study-from study.dcm photo.jpg photo.dcm
//
// APPn segments other than the JFIF and Adobe headers, COM segments and bytes
// after EOI are removed unless -keep-appn or -keep-com is given; the summary
// names what was removed by marker type only.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/encapimage"
	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/object"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

var errUsage = errors.New("usage error")

type options struct {
	jpeg      encapimage.JPEGOptions
	vlp       bool
	studyFrom string
	input     string
	output    string
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet("jpg2dcm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&opts.vlp, "vlp", false, "write VL Photographic Image instead of Secondary Capture")
	fs.StringVar(&opts.studyFrom, "study-from", "", "copy patient and study attributes from this DICOM file")
	fs.StringVar(&opts.jpeg.PatientName, "patient-name", "", "Patient's Name")
	fs.StringVar(&opts.jpeg.PatientID, "patient-id", "", "Patient ID")
	fs.StringVar(&opts.jpeg.StudyInstanceUID, "study-uid", "", "Study Instance UID (generated when empty)")
	fs.StringVar(&opts.jpeg.SeriesInstanceUID, "series-uid", "", "Series Instance UID (generated when empty)")
	fs.StringVar(&opts.jpeg.SeriesDescription, "series-description", "", "Series Description")
	fs.StringVar(&opts.jpeg.InstanceNumber, "instance-number", "", "Instance Number (default 1)")
	fs.BoolVar(&opts.jpeg.KeepAPPn, "keep-appn", false, "keep every APPn segment (Exif, XMP, ... may hold personal data)")
	fs.BoolVar(&opts.jpeg.KeepComments, "keep-com", false, "keep COM segments")
	fs.BoolVar(&opts.jpeg.DiscardICCProfile, "discard-icc", false, "do not copy an embedded ICC profile to ICC Profile (0028,2000)")
	fs.BoolVar(&opts.jpeg.AllowRetiredProgressive, "allow-progressive", false, "encapsulate progressive JPEG with the retired transfer syntax 1.2.840.10008.1.2.4.55")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: jpg2dcm [-vlp] [-study-from file.dcm] [flags] in.jpg out.dcm\n\nFlags:\n")
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
	if opts.vlp {
		opts.jpeg.SOPClassUID = encapimage.VLPhotographicImageStorage
	}
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
	report, err := convert(opts)
	if err != nil {
		clidiag.Fprintln(stderr, "jpg2dcm", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "jpg2dcm: %s process, transfer syntax %s, %s\n", report.Process, report.TransferSyntaxUID, report.PhotometricInterpretation)
	for _, removed := range report.RemovedSegments {
		name := removed.Marker
		if removed.Identifier != "" {
			name += " " + removed.Identifier
		}
		_, _ = fmt.Fprintf(stdout, "jpg2dcm: removed %d %s segment(s), %d bytes\n", removed.Count, name, removed.Bytes)
	}
	if report.TrailingBytesRemoved > 0 {
		_, _ = fmt.Fprintf(stdout, "jpg2dcm: removed %d bytes after EOI\n", report.TrailingBytesRemoved)
	}
	if report.ICCProfileCopied {
		_, _ = fmt.Fprintln(stdout, "jpg2dcm: ICC profile copied to ICC Profile (0028,2000)")
	}
	return 0
}

func convert(opts options) (encapimage.Report, error) {
	if opts.studyFrom != "" {
		if err := copyStudy(&opts.jpeg, opts.studyFrom); err != nil {
			return encapimage.Report{}, err
		}
	}
	data, err := os.ReadFile(opts.input)
	if err != nil {
		return encapimage.Report{}, err
	}
	file, report, err := encapimage.FromJPEG(data, opts.jpeg)
	if err != nil {
		return report, err
	}
	temp, err := os.CreateTemp(filepath.Dir(opts.output), ".jpg2dcm-*")
	if err != nil {
		return report, err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if err := object.WriteFile(temp, file); err != nil {
		_ = temp.Close()
		return report, err
	}
	if err := temp.Close(); err != nil {
		return report, err
	}
	return report, os.Rename(temp.Name(), opts.output)
}

// copyStudy fills the patient and study attributes the options leave empty
// from an existing object, as img2dcm -study-from does.
func copyStudy(opts *encapimage.JPEGOptions, path string) error {
	source, err := object.OpenFile(path)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	// The output is UTF-8; text in another character set is converted first.
	ds, _, err := source.Dataset.ConvertTextToUTF8(context.Background(), object.CharsetConversionOptions{})
	if err != nil {
		return err
	}
	fields := []struct {
		tag   core.Tag
		value *string
	}{
		{core.NewTag(0x0010, 0x0010), &opts.PatientName},
		{core.NewTag(0x0010, 0x0020), &opts.PatientID},
		{core.NewTag(0x0010, 0x0030), &opts.PatientBirthDate},
		{core.NewTag(0x0010, 0x0040), &opts.PatientSex},
		{core.NewTag(0x0020, 0x000D), &opts.StudyInstanceUID},
		{core.NewTag(0x0008, 0x0020), &opts.StudyDate},
		{core.NewTag(0x0008, 0x0030), &opts.StudyTime},
		{core.NewTag(0x0020, 0x0010), &opts.StudyID},
		{core.NewTag(0x0008, 0x1030), &opts.StudyDescription},
		{core.NewTag(0x0008, 0x0050), &opts.AccessionNumber},
		{core.NewTag(0x0008, 0x0090), &opts.ReferringPhysicianName},
	}
	for _, field := range fields {
		if *field.value != "" {
			continue
		}
		if value, ok := ds.GetString(field.tag); ok {
			*field.value = value
		}
	}
	return nil
}
