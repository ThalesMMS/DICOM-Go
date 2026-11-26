// Command seg2nii converts a DICOM Segmentation into an integer NIfTI
// labelmap:
//
//	seg2nii -o labels.nii.gz seg.dcm
//	seg2nii -labels labels.ctbl -o labels.nii seg.dcm study/series/
//	seg2nii -split -o segment.nii.gz seg.dcm     segment-1.nii.gz, segment-2.nii.gz, ...
//
// Each voxel holds the number of the segment covering it (0 where none
// does). BINARY and LABELMAP segmentations are read as stored, the label
// map's background (Pixel Padding Value, 0 by default) written as 0; FRACTIONAL
// ones are inside where the value reaches -threshold of the maximum. The
// grid is that of the reference series when one is given (files or
// directories of the source images, -series selecting one), otherwise the
// Segmentation's own planes, which must lie on regularly spaced slices; a
// single plane is 1 mm thick. Slices run along RowDir x ColDir, as dcm2nii
// writes them, so a labelmap of a series lies on dcm2nii's volume of it.
// Overlapping segments cannot share one labelmap: -split writes one binary
// volume per segment instead. -labels writes a 3D Slicer colour table with
// each segment's label and recommended display colour. Outputs are published
// through temporaries.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/clifile"
	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/render"
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
	threshold  float64
	split      bool
	gzip       bool
	input      string
	references []string
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	opts := options{threshold: 0.5}
	fs := flag.NewFlagSet("seg2nii", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.output, "o", "", "output NIfTI file (.nii or .nii.gz); with -split, the name the segment numbers are added to")
	fs.StringVar(&opts.labels, "labels", "", "also write a 3D Slicer colour table of the segments to this file")
	fs.StringVar(&opts.series, "series", "", "Series Instance UID of the reference series when the inputs hold several")
	fs.Float64Var(&opts.threshold, "threshold", opts.threshold, "fraction (0, 1] of the maximum at which a FRACTIONAL value is inside")
	fs.BoolVar(&opts.split, "split", false, "write one binary volume per segment, for overlapping segments")
	fs.BoolVar(&opts.gzip, "gz", false, "gzip-compress the output (implied by a .gz output name)")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: seg2nii [-split] [-labels table] [-threshold f] [-gz] -o out.nii[.gz] seg.dcm [reference...]\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errUsage
	}
	if fs.NArg() < 1 || opts.output == "" || !(opts.threshold > 0 && opts.threshold <= 1) || (opts.series != "" && fs.NArg() < 2) {
		fs.Usage()
		return opts, errUsage
	}
	opts.gzip = opts.gzip || strings.HasSuffix(strings.ToLower(opts.output), ".gz")
	opts.input, opts.references = fs.Arg(0), fs.Args()[1:]
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
	summary, err := convert(context.Background(), opts)
	if err != nil {
		clidiag.Fprintln(stderr, "seg2nii", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "seg2nii: "+summary)
	return 0
}

// grid is a regular voxel grid in patient space.
type grid struct {
	columns, rows, slices int
	origin                render.Vec3
	normal                render.Vec3
	first, spacing        float64
	indexToLPS            render.GeometryAffine
	// bySOP places frames without a plane position through the reference
	// image they were derived from.
	bySOP map[string]int
}

// sliceOf places a plane on the grid.
func (g grid) sliceOf(plane render.SliceGeometry) (int, bool) {
	if plane.Rows != g.rows || plane.Columns != g.columns {
		return 0, false
	}
	position := (plane.Origin.Dot(g.normal) - g.first) / g.spacing
	k := int(math.Round(position))
	if k < 0 || k >= g.slices || math.Abs(position-float64(k)) > 0.1 {
		return 0, false
	}
	// The plane's first pixel must lie on the grid's slice axis.
	expected := g.origin.Add(g.normal.Scale(float64(k) * g.spacing))
	if plane.Origin.Sub(expected).Length() > 0.1*g.spacing {
		return 0, false
	}
	return k, true
}

// newGrid builds the regular grid through planes of one orientation and
// pixel size; spacing is used when there is a single position.
func newGrid(planes []render.SliceGeometry, singleSpacing float64) (grid, error) {
	if len(planes) == 0 {
		return grid{}, errors.New("the segmentation has no plane geometry; give the reference series")
	}
	reference := planes[0]
	g := grid{columns: reference.Columns, rows: reference.Rows, normal: reference.RowDir.Cross(reference.ColDir).Normalize()}
	var positions []float64
	for _, plane := range planes {
		if plane.Rows != g.rows || plane.Columns != g.columns || plane.RowDir.Sub(reference.RowDir).Length() > 1e-4 ||
			plane.ColDir.Sub(reference.ColDir).Length() > 1e-4 || math.Abs(plane.RowSpacing-reference.RowSpacing) > 1e-4 ||
			math.Abs(plane.ColSpacing-reference.ColSpacing) > 1e-4 {
			return grid{}, errors.New("the planes differ in size, orientation or pixel spacing")
		}
		positions = append(positions, plane.Origin.Dot(g.normal))
	}
	sort.Float64s(positions)
	unique := positions[:1]
	for _, p := range positions[1:] {
		if p-unique[len(unique)-1] > 1e-3 {
			unique = append(unique, p)
		}
	}
	g.first, g.spacing = unique[0], singleSpacing
	if len(unique) > 1 {
		g.spacing = math.Inf(1)
		for i := 1; i < len(unique); i++ {
			g.spacing = math.Min(g.spacing, unique[i]-unique[i-1])
		}
	}
	if !(g.spacing > 0) || math.IsInf(g.spacing, 0) {
		g.spacing = 1
	}
	g.slices = int(math.Round((unique[len(unique)-1]-g.first)/g.spacing)) + 1
	if g.slices > 1<<15 || g.slices*g.rows*g.columns > 1<<30 {
		return grid{}, errors.New("the planes do not lie on a regular slice grid of a supported size")
	}
	for _, plane := range planes {
		if plane.Origin.Dot(g.normal) == g.first {
			g.origin = plane.Origin
			break
		}
	}
	i, j, k := reference.RowDir.Scale(reference.ColSpacing), reference.ColDir.Scale(reference.RowSpacing), g.normal.Scale(g.spacing)
	g.indexToLPS = render.GeometryAffine{i.X, j.X, k.X, g.origin.X, i.Y, j.Y, k.Y, g.origin.Y, i.Z, j.Z, k.Z, g.origin.Z, 0, 0, 0, 1}
	for _, plane := range planes {
		if _, ok := g.sliceOf(plane); !ok {
			return grid{}, errors.New("the planes do not lie on a regular slice grid")
		}
	}
	return g, nil
}

func convert(ctx context.Context, opts options) (string, error) {
	file, err := clifile.ReadPart10(opts.input)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	document, err := seg.Read(file.Dataset)
	if err != nil {
		return "", err
	}
	g, err := documentGrid(ctx, document, opts)
	if err != nil {
		return "", err
	}
	if document.Rows != g.rows || document.Columns != g.columns {
		return "", fmt.Errorf("the segmentation is %dx%d and the reference grid %dx%d", document.Columns, document.Rows, g.columns, g.rows)
	}
	plane := g.columns * g.rows
	layers := map[int][]uint16{} // segment number -> binary volume, for -split
	merged := make([]uint16, plane*g.slices)
	overlaps := 0
	for index, frame := range document.Frames {
		k, ok := -1, false
		if frame.Geometry.Rows > 0 {
			k, ok = g.sliceOf(frame.Geometry)
		}
		if !ok && frame.ReferencedSOPInstanceUID != "" {
			k, ok = g.bySOP[referenceKey(frame.ReferencedSOPInstanceUID, frame.ReferencedFrameNumber)]
		}
		if !ok {
			return "", fmt.Errorf("frame %d does not lie on the grid", index+1)
		}
		for row := 0; row < g.rows; row++ {
			for column := 0; column < g.columns; column++ {
				number, err := pixelSegment(document, frame, column, row, opts.threshold)
				if err != nil {
					return "", fmt.Errorf("frame %d: %w", index+1, err)
				}
				if number == 0 {
					continue
				}
				voxel := k*plane + row*g.columns + column
				if opts.split {
					if layers[number] == nil {
						layers[number] = make([]uint16, len(merged))
					}
					layers[number][voxel] = 1
					continue
				}
				if merged[voxel] != 0 && merged[voxel] != uint16(number) {
					overlaps++
				}
				merged[voxel] = uint16(number)
			}
		}
	}
	if overlaps > 0 {
		return "", fmt.Errorf("segments overlap at %d voxel(s); use -split", overlaps)
	}
	inputs := append([]string{opts.input}, opts.references...)
	compression := nifti.CompressionNone
	if opts.gzip {
		compression = nifti.CompressionGZIP
	}
	written := 0
	if opts.split {
		numbers := make([]int, 0, len(layers))
		for number := range layers {
			numbers = append(numbers, number)
		}
		sort.Ints(numbers)
		for _, number := range numbers {
			if err := writeLabelmap(ctx, splitName(opts.output, number), layers[number], g, compression, inputs); err != nil {
				return "", err
			}
			written++
		}
	} else {
		if err := writeLabelmap(ctx, opts.output, merged, g, compression, inputs); err != nil {
			return "", err
		}
		written = 1
	}
	if opts.labels != "" {
		if err := clifile.WriteAtomic(opts.labels, func(w io.Writer) error { return writeTable(w, document) }, inputs...); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%dx%dx%d voxels, %d segment(s), %d volume(s) written", g.columns, g.rows, g.slices, len(document.Segments), written), nil
}

func documentGrid(ctx context.Context, document *seg.Document, opts options) (grid, error) {
	if len(opts.references) == 0 {
		var planes []render.SliceGeometry
		for _, frame := range document.Frames {
			if frame.Geometry.Rows > 0 {
				planes = append(planes, frame.Geometry)
			}
		}
		return newGrid(planes, document.Geometry.MeanSpacing)
	}
	paths, err := clifile.SeriesFiles(ctx, opts.references, opts.series)
	if err != nil {
		return grid{}, err
	}
	files := make([]*object.File, 0, len(paths))
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	for _, path := range paths {
		file, err := object.OpenFileWithOptions(path, object.ReadFileOptions{InlineValueBytesThreshold: 64 << 10})
		if err != nil {
			return grid{}, err
		}
		files = append(files, file)
	}
	references, err := nifti.SEGReferences(files)
	if err != nil {
		return grid{}, err
	}
	planes := make([]render.SliceGeometry, len(references))
	for i, reference := range references {
		planes[i] = reference.Geometry
	}
	g, err := newGrid(planes, 1)
	if err != nil {
		return grid{}, err
	}
	g.bySOP = map[string]int{}
	for _, reference := range references {
		k, _ := g.sliceOf(reference.Geometry)
		frame := 0
		if len(reference.Image.Frames) > 0 {
			frame = reference.Image.Frames[0]
		}
		g.bySOP[referenceKey(reference.Image.SOPInstanceUID, frame)] = k
	}
	return g, nil
}

func referenceKey(sopInstanceUID string, frame int) string {
	if frame <= 1 {
		frame = 0
	}
	return fmt.Sprintf("%s#%d", sopInstanceUID, frame)
}

// pixelSegment is the segment number frame gives the pixel, 0 for none. A
// label map's background is none; a label-map segment numbered 0 (beside a
// nonzero background) has no NIfTI label and is refused.
func pixelSegment(document *seg.Document, frame seg.Frame, column, row int, threshold float64) (int, error) {
	index := row*document.Columns + column
	switch {
	case frame.LabelMap != nil:
		if index < len(frame.LabelMap) {
			label := frame.LabelMap[index]
			switch {
			case label == document.LabelMapBackground:
				return 0, nil
			case label == 0:
				return 0, fmt.Errorf("label-map segment 0 (background %d) cannot be written to a NIfTI labelmap, where 0 is no segment", document.LabelMapBackground)
			}
			return int(label), nil
		}
	case strings.EqualFold(document.SegmentationType, "FRACTIONAL"):
		if index < len(frame.Fractional) && frame.Fractional[index] > 0 &&
			float64(frame.Fractional[index]) >= threshold*float64(document.MaxFractionalValue) {
			return frame.SegmentNumber, nil
		}
	case frame.Mask != nil:
		if frame.Mask.Get(column, row) {
			return frame.SegmentNumber, nil
		}
	}
	return 0, nil
}

func writeLabelmap(ctx context.Context, path string, labels []uint16, g grid, compression nifti.Compression, inputs []string) error {
	maximum := uint16(0)
	for _, label := range labels {
		maximum = max(maximum, label)
	}
	datatype := nifti.DatatypeUint8
	payload := make([]byte, 0, 2*len(labels))
	if maximum > math.MaxUint8 {
		datatype = nifti.DatatypeUint16
		for _, label := range labels {
			payload = binary.LittleEndian.AppendUint16(payload, label)
		}
	} else {
		for _, label := range labels {
			payload = append(payload, byte(label))
		}
	}
	volume, err := nifti.NewVolume(ctx, bytes.NewReader(payload), nifti.NewVolumeOptions{
		Datatype: datatype, Dimensions: [4]int{g.columns, g.rows, g.slices, 1}, IndexToLPS: g.indexToLPS,
	})
	if err != nil {
		return err
	}
	return clifile.WriteAtomic(path, func(w io.Writer) error { return nifti.WriteVolume(w, volume, compression) }, inputs...)
}

// splitName inserts -<number> before the .nii or .nii.gz extension.
func splitName(path string, number int) string {
	for _, ext := range []string{".nii.gz", ".nii"} {
		if strings.HasSuffix(strings.ToLower(path), ext) {
			return fmt.Sprintf("%s-%d%s", path[:len(path)-len(ext)], number, path[len(path)-len(ext):])
		}
	}
	return fmt.Sprintf("%s-%d", path, number)
}

// writeTable writes "number name R G B A" lines (3D Slicer colour table);
// spaces in names become underscores, which readers turn back.
func writeTable(w io.Writer, document *seg.Document) error {
	background := 0
	if document.SOPClassUID == seg.LabelMapSegmentationStorage {
		background = int(document.LabelMapBackground)
	}
	sorted := append([]seg.Segment(nil), document.Segments...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Number < sorted[j].Number })
	if _, err := fmt.Fprintln(w, "# segment number, label, recommended display colour (RGB, alpha)"); err != nil {
		return err
	}
	for _, segment := range sorted {
		if segment.Number == 0 || segment.Number == background {
			continue
		}
		rgb := seg.DICOMCIELabToRGB(segment.RecommendedDisplayCIELab)
		name := strings.Join(strings.Fields(segment.Label), "_")
		if name == "" {
			name = fmt.Sprintf("Segment_%d", segment.Number)
		}
		if _, err := fmt.Fprintf(w, "%d %s %d %d %d 255\n", segment.Number, name, rgb[0], rgb[1], rgb[2]); err != nil {
			return err
		}
	}
	return nil
}
