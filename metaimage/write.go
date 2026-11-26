package metaimage

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/render"
)

// WriteOptions configures Write and WritePlan.
type WriteOptions struct {
	// Compress writes the data as a zlib stream (CompressedData = True).
	Compress bool
	// Fields are extra "Key = Value" lines written before ElementDataFile,
	// sorted by key; keys must not repeat a standard field.
	Fields map[string]string
	// TempDir holds the compressed stream WritePlan spools to learn its
	// size before the header; empty uses the system temporary directory.
	TempDir string
}

// headerSpec is the content of a MetaImage header written by this package.
type headerSpec struct {
	datatype   int16
	dimensions [4]int
	indexToLPS render.GeometryAffine
	// fourthSpacing is the ElementSpacing of a fourth axis (1 when unset).
	fourthSpacing  float64
	compressedSize int64
	options        WriteOptions
}

// header renders the header up to and including ElementDataFile = LOCAL.
// ElementSpacing is the length of each affine column and TransformMatrix row
// i the unit LPS direction of axis i.
func (spec headerSpec) header() (string, error) {
	elementType, ok := elementTypeByDatatype[spec.datatype]
	if !ok {
		return "", fmt.Errorf("%w: datatype %d", ErrUnsupported, spec.datatype)
	}
	d := spec.dimensions
	n := 3
	if d[3] > 1 {
		n = 4
	}
	a := spec.indexToLPS
	var columns [3]render.Vec3
	var spacingValues [3]float64
	for axis := 0; axis < 3; axis++ {
		if d[axis] < 1 {
			return "", fmt.Errorf("%w: size %d of axis %d", ErrMalformedHeader, d[axis], axis)
		}
		columns[axis] = render.Vec3{X: a[axis], Y: a[4+axis], Z: a[8+axis]}
		spacingValues[axis] = columns[axis].Length()
		if !(spacingValues[axis] > 0) || math.IsInf(spacingValues[axis], 0) {
			return "", fmt.Errorf("%w: degenerate direction of axis %d", ErrMalformedHeader, axis)
		}
	}
	var direction []string
	for axis := 0; axis < n; axis++ {
		for j := 0; j < n; j++ {
			value := 0.0
			switch {
			case axis < 3 && j < 3:
				value = a[4*j+axis] / spacingValues[axis]
			case axis == j:
				value = 1
			}
			direction = append(direction, formatNumber(value))
		}
	}
	sizes := []string{strconv.Itoa(d[0]), strconv.Itoa(d[1]), strconv.Itoa(d[2])}
	spacing := []string{formatNumber(spacingValues[0]), formatNumber(spacingValues[1]), formatNumber(spacingValues[2])}
	offset := []string{formatNumber(a[3]), formatNumber(a[7]), formatNumber(a[11])}
	if n == 4 {
		fourth := spec.fourthSpacing
		if !(fourth > 0) || math.IsInf(fourth, 0) {
			fourth = 1
		}
		sizes, spacing, offset = append(sizes, strconv.Itoa(d[3])), append(spacing, formatNumber(fourth)), append(offset, "0")
	}
	var b strings.Builder
	line := func(key, value string) { fmt.Fprintf(&b, "%s = %s\n", key, value) }
	line("ObjectType", "Image")
	line("NDims", strconv.Itoa(n))
	line("BinaryData", "True")
	line("BinaryDataByteOrderMSB", "False")
	line("CompressedData", map[bool]string{true: "True", false: "False"}[spec.options.Compress])
	if spec.options.Compress {
		line("CompressedDataSize", strconv.FormatInt(spec.compressedSize, 10))
	}
	line("TransformMatrix", strings.Join(direction, " "))
	line("Offset", strings.Join(offset, " "))
	line("CenterOfRotation", strings.TrimSpace(strings.Repeat("0 ", n)))
	if n == 3 {
		line("AnatomicalOrientation", anatomicalOrientation(columns))
	}
	line("ElementSpacing", strings.Join(spacing, " "))
	line("DimSize", strings.Join(sizes, " "))
	keys := make([]string, 0, len(spec.options.Fields))
	for key := range spec.options.Fields {
		if key == "" || strings.ContainsAny(key, "=\r\n") || strings.ContainsAny(spec.options.Fields[key], "\r\n") || reservedFields[strings.ToLower(key)] {
			return "", fmt.Errorf("%w: field %q", ErrMalformedHeader, truncate(key))
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		line(key, spec.options.Fields[key])
	}
	line("ElementType", elementType)
	line("ElementDataFile", "LOCAL")
	return b.String(), nil
}

// Write writes v as a single-file MetaImage (.mha): TransformMatrix and
// Offset from the volume's LPS geometry, little-endian elements. A fourth
// dimension is written as NDims = 4 with unit spacing and no rotation of the
// fourth axis. Values are written unscaled; NIfTI scaling makes Write fail.
func Write(w io.Writer, v *nifti.Volume, options WriteOptions) error {
	if v == nil {
		return fmt.Errorf("%w: nil volume", ErrMalformedHeader)
	}
	if v.Scaling.Applied {
		return fmt.Errorf("%w: value scaling", ErrUnsupported)
	}
	spec := headerSpec{datatype: v.Header.Datatype, dimensions: v.Dimensions, indexToLPS: v.Geometry.IndexToLPS, options: options}
	if _, err := spec.header(); err != nil {
		return err
	}
	var payload bytes.Buffer
	if options.Compress {
		compressor := zlib.NewWriter(&payload)
		if err := v.WritePayload(compressor); err != nil {
			return err
		}
		if err := compressor.Close(); err != nil {
			return err
		}
		spec.compressedSize = int64(payload.Len())
	}
	header, err := spec.header()
	if err != nil {
		return err
	}
	buffered := bufio.NewWriter(w)
	if _, err := buffered.WriteString(header); err != nil {
		return err
	}
	if options.Compress {
		if _, err := buffered.Write(payload.Bytes()); err != nil {
			return err
		}
	} else if err := v.WritePayload(buffered); err != nil {
		return err
	}
	return buffered.Flush()
}

// WritePlan writes the volume of a validated NIfTI export plan (one DICOM
// series) as a single-file MetaImage (.mha), with the same pixel pass,
// geometry checks and voxel stream as the NIfTI export: TransformMatrix,
// ElementSpacing and Offset come from the plan's full-precision LPS affine;
// a temporal volume gets NDims = 4 with its time step as fourth spacing.
// MetaImage has no value scaling, so a plan that preserves a non-identity
// slope/intercept (nifti.ScalingPreserveUniform) is refused with
// ErrUnsupported before any pixel is read; an image-grid plan without patient
// geometry is refused too. A compressed file spools its zlib stream to a
// temporary file to write CompressedDataSize first. The report's
// BytesWritten counts the whole file.
func WritePlan(ctx context.Context, w io.Writer, plan *nifti.Plan, options WriteOptions) (report nifti.Report, retErr error) {
	if plan == nil || w == nil {
		return nifti.Report{}, fmt.Errorf("%w: nil plan or destination", ErrMalformedHeader)
	}
	layout := plan.Layout()
	if !layout.IdentityScaling() {
		return nifti.Report{}, fmt.Errorf("%w: value scaling (slope %g, intercept %g)", ErrUnsupported, layout.ScalingSlope, layout.ScalingIntercept)
	}
	if layout.NoSpatialTransform {
		return nifti.Report{}, fmt.Errorf("%w: image grid without patient geometry", ErrUnsupported)
	}
	spec := headerSpec{
		datatype: layout.Datatype, dimensions: layout.Dimensions, indexToLPS: layout.IndexToLPS,
		fourthSpacing: layout.Spacing[3], options: options,
	}
	header, err := spec.header()
	if err != nil {
		return nifti.Report{}, err
	}
	counter := &countingWriter{w: w}
	if !options.Compress {
		buffered := bufio.NewWriterSize(counter, 64<<10)
		if _, err := buffered.WriteString(header); err != nil {
			return nifti.Report{}, err
		}
		report, err := plan.WriteVoxels(ctx, buffered)
		if err != nil {
			return nifti.Report{}, err
		}
		if err := buffered.Flush(); err != nil {
			return nifti.Report{}, err
		}
		report.BytesWritten = counter.n
		return report, nil
	}
	spool, err := os.CreateTemp(options.TempDir, "dicom-metaimage-spool-*")
	if err != nil {
		return nifti.Report{}, err
	}
	defer func() {
		if cleanupErr := errors.Join(spool.Close(), os.Remove(spool.Name())); cleanupErr != nil && retErr == nil {
			report, retErr = nifti.Report{}, cleanupErr
		}
	}()
	spooled := bufio.NewWriterSize(spool, 64<<10)
	compressor := zlib.NewWriter(spooled)
	report, err = plan.WriteVoxels(ctx, compressor)
	if err != nil {
		return nifti.Report{}, err
	}
	if err := errors.Join(compressor.Close(), spooled.Flush()); err != nil {
		return nifti.Report{}, err
	}
	size, err := spool.Seek(0, io.SeekCurrent)
	if err != nil {
		return nifti.Report{}, err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return nifti.Report{}, err
	}
	spec.compressedSize = size
	if header, err = spec.header(); err != nil {
		return nifti.Report{}, err
	}
	if _, err := io.WriteString(counter, header); err != nil {
		return nifti.Report{}, err
	}
	if _, err := io.Copy(counter, spool); err != nil {
		return nifti.Report{}, err
	}
	report.BytesWritten = counter.n
	return report, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
