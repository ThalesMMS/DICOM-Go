// Command nii2seg converts an integer NIfTI labelmap into a binary DICOM
// Segmentation of the DICOM series it was drawn on:
//
//	nii2seg -o seg.dcm labels.nii.gz study/series/
//	nii2seg -labels labels.ctbl -series 1.2.3 -o seg.dcm labels.nii study/
//
// The labelmap (.nii, .nii.gz or a .hdr/.img pair) must lie on the grid of
// the reference series up to an axis permutation, flips and an integer
// offset, such as a crop or a RAS-reoriented copy; no interpolation is done
// (nifti.Volume.SEGDocument). Each non-zero label becomes the segment of the
// same number, named and coloured by the -labels table (3D Slicer, FreeSurfer
// or ITK-SNAP layout) or "Label N" otherwise, so seg2nii gives the labels
// back. The Segmentation is a new series of the reference study and frame of
// reference, and copies the patient and study modules from the first
// reference image, as the IOD requires. Outputs are published through
// temporaries.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/clifile"
	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/seg"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

var errUsage = errors.New("usage error")

type options struct {
	output     string
	labels     string
	series     string
	tolerance  float64
	labelmap   string
	references []string
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet("nii2seg", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.output, "o", "", "output DICOM Segmentation file")
	fs.StringVar(&opts.labels, "labels", "", "label table naming and colouring the labels (Slicer, FreeSurfer or ITK-SNAP)")
	fs.StringVar(&opts.series, "series", "", "Series Instance UID of the reference series when the inputs hold several")
	fs.Float64Var(&opts.tolerance, "tolerance", nifti.DefaultSEGVoxelTolerance, "largest distance, in voxels, between a labelmap voxel and its DICOM voxel")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: nii2seg [-labels table] [-series uid] [-tolerance voxels] -o seg.dcm labelmap.nii[.gz] reference...\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errUsage
	}
	if fs.NArg() < 2 || opts.output == "" || !(opts.tolerance > 0 && opts.tolerance < 0.5) {
		fs.Usage()
		return opts, errUsage
	}
	opts.labelmap, opts.references = fs.Arg(0), fs.Args()[1:]
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
		clidiag.Fprintln(stderr, "nii2seg", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "nii2seg: %d segment(s), %d frame(s)\n", len(report.SegmentNumbers), report.Frames)
	return 0
}

func convert(ctx context.Context, opts options) (nifti.SEGReport, error) {
	volume, err := nifti.ReadFile(ctx, opts.labelmap, nifti.ReadOptions{})
	if err != nil {
		return nifti.SEGReport{}, err
	}
	var table nifti.LabelTable
	if opts.labels != "" {
		source, err := os.Open(opts.labels)
		if err != nil {
			return nifti.SEGReport{}, err
		}
		table, err = nifti.ReadLabelTable(source)
		_ = source.Close()
		if err != nil {
			return nifti.SEGReport{}, err
		}
	}
	paths, err := clifile.SeriesFiles(ctx, opts.references, opts.series)
	if err != nil {
		return nifti.SEGReport{}, err
	}
	files := make([]*object.File, 0, len(paths))
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	for _, path := range paths {
		// Pixel Data stays on disk: only the geometry and the common modules
		// are read.
		file, err := object.OpenFileWithOptions(path, object.ReadFileOptions{InlineValueBytesThreshold: 64 << 10})
		if err != nil {
			return nifti.SEGReport{}, err
		}
		files = append(files, file)
	}
	references, err := nifti.SEGReferences(files)
	if err != nil {
		return nifti.SEGReport{}, err
	}
	first := files[0]
	segOptions := nifti.SEGOptions{References: references, Segments: definitions(volume, table), VoxelTolerance: opts.tolerance}
	segOptions.StudyInstanceUID, _ = first.GetUID(core.NewTag(0x0020, 0x000D))
	segOptions.FrameOfReferenceUID, _ = first.GetUID(core.NewTag(0x0020, 0x0052))
	if segOptions.SOPInstanceUID, err = core.NewUID(); err != nil {
		return nifti.SEGReport{}, err
	}
	if segOptions.SeriesInstanceUID, err = core.NewUID(); err != nil {
		return nifti.SEGReport{}, err
	}
	document, report, err := volume.SEGDocument(segOptions)
	if err != nil {
		return nifti.SEGReport{}, err
	}
	document.Common = object.DerivedCommon{Source: first.Dataset}
	out, err := seg.Write(document)
	if err != nil {
		return nifti.SEGReport{}, err
	}
	inputs := append([]string{opts.labelmap}, paths...)
	return report, clifile.WritePart10(opts.output, out, inputs...)
}

// definitions gives every label of the volume the segment of its own
// number, with the table's name and colour when it has them.
func definitions(volume *nifti.Volume, table nifti.LabelTable) map[int]seg.Segment {
	named := table.Segments()
	present := map[int]bool{}
	for i := 0; i < volume.Len(); i++ {
		value := volume.Value(i)
		if value >= 1 && value <= math.MaxUint16 && value == math.Trunc(value) {
			present[int(value)] = true
		}
	}
	labels := make([]int, 0, len(present))
	for label := range present {
		labels = append(labels, label)
	}
	sort.Ints(labels)
	out := make(map[int]seg.Segment, len(labels))
	for _, label := range labels {
		segment, ok := named[label]
		if !ok {
			segment = seg.Segment{
				Label: fmt.Sprintf("Label %d", label), AlgorithmType: seg.AlgorithmManual,
				SegmentedPropertyCategory: seg.GenericROICategoryCode(), SegmentedPropertyType: seg.GenericROITypeCode(),
			}
		}
		segment.Number = label
		out[label] = segment
	}
	return out
}
