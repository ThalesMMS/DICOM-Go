package nrrd

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/render"
)

var typeByDatatype = map[int16]string{
	nifti.DatatypeInt8: "int8", nifti.DatatypeUint8: "uint8", nifti.DatatypeInt16: "int16", nifti.DatatypeUint16: "uint16",
	nifti.DatatypeInt32: "int32", nifti.DatatypeUint32: "uint32", nifti.DatatypeFloat32: "float", nifti.DatatypeFloat64: "double",
}

var bytesByDatatype = map[int16]int{
	nifti.DatatypeInt8: 1, nifti.DatatypeUint8: 1, nifti.DatatypeInt16: 2, nifti.DatatypeUint16: 2,
	nifti.DatatypeInt32: 4, nifti.DatatypeUint32: 4, nifti.DatatypeFloat32: 4, nifti.DatatypeFloat64: 8,
}

// WriteOptions configures Write.
type WriteOptions struct {
	// Raw writes uncompressed data; the default is gzip.
	Raw bool
	// KeyValues are written as key:=value pairs, sorted by key. Keys must
	// not contain ":=" or line breaks.
	KeyValues map[string]string
	// FourthAxisKind is the kind of a fourth (non-spatial) axis; the
	// default is "list".
	FourthAxisKind string
}

// headerSpec is the content of an NRRD header written by this package.
type headerSpec struct {
	datatype   int16
	dimensions [4]int
	indexToLPS render.GeometryAffine
	options    WriteOptions
	// listFirst puts a fourth (non-spatial) axis first, so its samples are
	// interleaved, as 3D Slicer writes segmentation layers.
	listFirst bool
}

// header renders the NRRD0004 header, including the blank line that ends it.
func (spec headerSpec) header() (string, error) {
	typ, ok := typeByDatatype[spec.datatype]
	if !ok {
		return "", fmt.Errorf("%w: datatype %d", ErrUnsupported, spec.datatype)
	}
	for key, value := range spec.options.KeyValues {
		if key == "" || strings.Contains(key, ":=") || strings.ContainsAny(key, "\r\n") || strings.ContainsAny(value, "\r") {
			return "", fmt.Errorf("%w: key/value %q", ErrMalformedHeader, truncate(key))
		}
	}
	d := spec.dimensions
	for axis := 0; axis < 3; axis++ {
		if d[axis] < 1 {
			return "", fmt.Errorf("%w: size %d of axis %d", ErrMalformedHeader, d[axis], axis)
		}
	}
	dimension := 3
	if d[3] > 1 {
		dimension = 4
	}
	a := spec.indexToLPS
	var b strings.Builder
	b.WriteString("NRRD0004\n# Complete NRRD file format specification at:\n# http://teem.sourceforge.net/nrrd/format.html\n")
	fmt.Fprintf(&b, "type: %s\ndimension: %d\nspace: left-posterior-superior\n", typ, dimension)
	sizes := []string{strconv.Itoa(d[0]), strconv.Itoa(d[1]), strconv.Itoa(d[2])}
	directions := make([]string, 3)
	kinds := []string{"domain", "domain", "domain"}
	for column := 0; column < 3; column++ {
		directions[column] = "(" + formatNumber(a[column]) + "," + formatNumber(a[4+column]) + "," + formatNumber(a[8+column]) + ")"
	}
	if dimension == 4 {
		kind := spec.options.FourthAxisKind
		if kind == "" {
			kind = "list"
		}
		if spec.listFirst {
			sizes = append([]string{strconv.Itoa(d[3])}, sizes...)
			directions = append([]string{"none"}, directions...)
			kinds = append([]string{kind}, kinds...)
		} else {
			sizes = append(sizes, strconv.Itoa(d[3]))
			directions = append(directions, "none")
			kinds = append(kinds, kind)
		}
	}
	fmt.Fprintf(&b, "sizes: %s\nspace directions: %s\nkinds: %s\n", strings.Join(sizes, " "), strings.Join(directions, " "), strings.Join(kinds, " "))
	if bytesByDatatype[spec.datatype] > 1 {
		b.WriteString("endian: little\n")
	}
	encoding := "gzip"
	if spec.options.Raw {
		encoding = "raw"
	}
	fmt.Fprintf(&b, "encoding: %s\nspace origin: (%s,%s,%s)\n", encoding, formatNumber(a[3]), formatNumber(a[7]), formatNumber(a[11]))
	keys := make([]string, 0, len(spec.options.KeyValues))
	for key := range spec.options.KeyValues {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&b, "%s:=%s\n", escapeKeyValue(key), escapeKeyValue(spec.options.KeyValues[key]))
	}
	b.WriteString("\n")
	return b.String(), nil
}

// writeEncoded writes the header and then the payload produced by payload,
// gzip-compressed unless options.Raw.
func writeEncoded(w io.Writer, header string, raw bool, payload func(io.Writer) error) (int64, error) {
	counter := &countingWriter{w: w}
	buffered := bufio.NewWriterSize(counter, 64<<10)
	if _, err := buffered.WriteString(header); err != nil {
		return counter.n, err
	}
	if raw {
		if err := payload(buffered); err != nil {
			return counter.n, err
		}
		err := buffered.Flush()
		return counter.n, err
	}
	compressor := gzip.NewWriter(buffered)
	if err := payload(compressor); err != nil {
		return counter.n, err
	}
	if err := compressor.Close(); err != nil {
		return counter.n, err
	}
	err := buffered.Flush()
	return counter.n, err
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

// Write writes v as an NRRD0004 file with attached data in the
// left-posterior-superior space: space directions and origin come from the
// volume's LPS geometry, a fourth dimension becomes a last, non-spatial axis,
// and samples are little-endian. The stored values are written unscaled; a
// NIfTI scl_slope/scl_inter is not representable in NRRD and makes Write fail.
func Write(w io.Writer, v *nifti.Volume, options WriteOptions) error {
	if v == nil {
		return fmt.Errorf("%w: nil volume", ErrMalformedHeader)
	}
	if v.Scaling.Applied {
		return fmt.Errorf("%w: value scaling", ErrUnsupported)
	}
	header, err := headerSpec{datatype: v.Header.Datatype, dimensions: v.Dimensions, indexToLPS: v.Geometry.IndexToLPS, options: options}.header()
	if err != nil {
		return err
	}
	_, err = writeEncoded(w, header, options.Raw, v.WritePayload)
	return err
}

// WritePlan writes the volume of a validated NIfTI export plan (one DICOM
// series) as an NRRD0004 file, with the same pixel pass, geometry checks and
// voxel stream as the NIfTI export: space directions and origin come from the
// plan's full-precision LPS affine, a temporal volume gets a last list axis.
// NRRD has no value scaling, so a plan that preserves a non-identity
// slope/intercept (nifti.ScalingPreserveUniform) is refused with
// ErrUnsupported before any pixel is read; choose a floating-point scaling
// policy instead. An image-grid plan without patient geometry gets
// ErrNoPatientSpace. The report's BytesWritten counts the whole file.
func WritePlan(ctx context.Context, w io.Writer, plan *nifti.Plan, options WriteOptions) (nifti.Report, error) {
	if plan == nil || w == nil {
		return nifti.Report{}, fmt.Errorf("%w: nil plan or destination", ErrMalformedHeader)
	}
	layout := plan.Layout()
	if !layout.IdentityScaling() {
		return nifti.Report{}, fmt.Errorf("%w: value scaling (slope %g, intercept %g)", ErrUnsupported, layout.ScalingSlope, layout.ScalingIntercept)
	}
	if layout.NoSpatialTransform {
		return nifti.Report{}, fmt.Errorf("%w: image grid without patient geometry", ErrNoPatientSpace)
	}
	header, err := headerSpec{datatype: layout.Datatype, dimensions: layout.Dimensions, indexToLPS: layout.IndexToLPS, options: options}.header()
	if err != nil {
		return nifti.Report{}, err
	}
	var report nifti.Report
	written, err := writeEncoded(w, header, options.Raw, func(out io.Writer) error {
		var voxelErr error
		report, voxelErr = plan.WriteVoxels(ctx, out)
		return voxelErr
	})
	if err != nil {
		return nifti.Report{}, err
	}
	report.BytesWritten = written
	if !options.Raw {
		report.Compression = nifti.CompressionGZIP
	}
	return report, nil
}

func formatNumber(v float64) string {
	if v == 0 {
		return "0"
	}
	return strconv.FormatFloat(v, 'g', 17, 64)
}
