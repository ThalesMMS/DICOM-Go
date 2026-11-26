package registration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/render"
)

// ITK transform files (#1093, #1252), written independently from the
// formats of ITK's TxtTransformIO ("#Insight Transform File V1.0", the text
// .tfm and .txt files of Slicer and elastix), MatlabTransformIO (the MATLAB
// Level 4 .mat files of ANTs) and HDF5TransformIO (the .h5 and .hdf5 files
// of ANTs and Slicer, see ReadITKHDF5).
// Besides the linear transforms, B-spline (BSpline) and displacement field
// (DisplacementFieldTransform) transforms are read from text and HDF5
// files, alone or in composites; MATLAB files hold linear transforms only.
//
// A transform in these ITK files has ITK's direction, the one of this
// package: it maps fixed-image points to moving-image points, in LPS
// patient coordinates, with the centre of rotation in the fixed
// parameters. DICOMMatrix gives the Frame of Reference Transformation
// Matrix, which maps the other way.

// ITKTextHeader starts every ITK text transform file.
const ITKTextHeader = "#Insight Transform File V1.0"

// utf8BOM is skipped before the text header, as editors may write one.
var utf8BOM = string(rune(0xFEFF))

// ErrTransformFile reports a transform file that cannot be read or a
// transform that cannot be written.
var ErrTransformFile = errors.New("dicom/registration: transform file")

// maxTransformFileParameters bounds the parameters read for one linear
// transform, which has at most 15; maxDenseTransformParameters those of a
// B-spline or displacement field (2²⁴ values, 128 MiB of float64).
const (
	maxTransformFileParameters  = 64
	maxDenseTransformParameters = 1 << 24
)

// maxTextLine bounds one line of a text transform file; a displacement
// field's parameters are written on one line.
const maxTextLine = 1 << 29

// parameterLimit is the largest parameter vector read for a transform.
func parameterLimit(t interface{ Kind() Kind }) int {
	if t != nil && (t.Kind() == KindBSpline || t.Kind() == KindDisplacementField) {
		return maxDenseTransformParameters
	}
	return maxTransformFileParameters
}

// ReadTransformFile reads an ITK text, MATLAB, HDF5 or linear MINC XFM file.
// The format is recognized from the content. The result is a Composite when
// the file holds a composition. MINC transforms retain their encoded mapping
// direction between the two physical spaces, converted from RAS to LPS
// millimetres. The caller identifies which space is fixed or moving before
// using a MINC transform for registration.
func ReadTransformFile(data []byte) (Transform, error) {
	return ReadTransformFileContext(context.Background(), data, TransformFileReadOptions{})
}

// TransformFileReadOptions bounds the total parameter values decoded across
// all components, including replaced text parameter lines. Zero uses 2^24.
// This bounds numeric payloads, not the caller's encoded bytes or process RSS.
type TransformFileReadOptions struct {
	MaxParameters int
}

// ReadTransformFileContext reads the same formats as ReadTransformFile with
// cancellation at input reads and an aggregate parameter budget. Cancellation
// is also checked after decoding; no partially assembled transform is returned.
func ReadTransformFileContext(ctx context.Context, data []byte, options TransformFileReadOptions) (Transform, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := options.MaxParameters
	if limit == 0 {
		limit = maxDenseTransformParameters
	}
	if limit < 1 || limit > maxDenseTransformParameters {
		return nil, fmt.Errorf("%w: invalid total parameter budget", ErrTransformFile)
	}
	if body, ok := mincTransformBody(data); ok {
		if len(data) > maxMINCFileBytes {
			return nil, fmt.Errorf("%w: MINC XFM exceeds %d encoded bytes", ErrTransformFile, maxMINCFileBytes)
		}
		transform, err := readMINCTransform(&transformContextReader{ctx: ctx, Reader: bytes.NewReader(body)}, limit)
		if canceled := ctx.Err(); canceled != nil {
			return nil, canceled
		}
		return transform, err
	}
	var (
		records   []itkParameters
		composite bool
		err       error
	)
	input := &transformContextReader{ctx: ctx, Reader: bytes.NewReader(data)}
	if isHDF5(data) {
		records, composite, err = readITKHDF5Bounded(input, int64(len(data)), 3, limit)
	} else if bytes.HasPrefix(bytes.TrimLeft(data, " \t\r\n"+utf8BOM), []byte("#")) {
		records, composite, err = readITKTextBounded(input, 3, limit)
	} else {
		records, err = readITKMatlab(input, 3)
		for _, t := range records {
			limit -= len(t.Parameters())
			if limit < 0 {
				err = fmt.Errorf("%w: total parameter budget", ErrTransformFile)
				break
			}
		}
	}
	if canceled := ctx.Err(); canceled != nil {
		return nil, canceled
	}
	if err != nil {
		return nil, err
	}
	transforms, err := assembleITK3D(records, composite)
	if err != nil {
		return nil, err
	}
	if len(transforms) != 1 {
		return nil, fmt.Errorf("%w: %d transforms without a composite; the file must hold one transform", ErrTransformFile, len(transforms))
	}
	return transforms[0], nil
}

type transformContextReader struct {
	ctx context.Context
	*bytes.Reader
}

func (r *transformContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func (r *transformContextReader) ReadAt(p []byte, off int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.ReadAt(p, off)
}

// ReadITKText reads an ITK text transform file. A composite transform, when
// it opens the file, gathers the transforms that follow; ITK lists them in
// queue order, the last applied first, so the Composite holds them reversed.
// Components in other files (ComponentTransformFile) are not read.
func ReadITKText(r io.Reader) ([]Transform, error) {
	records, composite, err := readITKText(r, 3)
	if err != nil {
		return nil, err
	}
	return assembleITK3D(records, composite)
}

func readITKText(r io.Reader, dimension int) ([]itkParameters, bool, error) {
	return readITKTextBounded(r, dimension, maxDenseTransformParameters)
}

func readITKTextBounded(r io.Reader, dimension, remaining int) ([]itkParameters, bool, error) {
	scanner := bufio.NewScanner(r)
	lineLimit := maxTextLine
	if dimension == 2 {
		lineLimit = 64 * 1024
	}
	scanner.Buffer(make([]byte, 4096), lineLimit)
	var (
		transforms                []itkParameters
		composite                 bool
		current                   itkParameters
		parameters, fixed         []float64
		haveParameters, haveFixed bool
		lineNumber                int
		recordsRead               int
		sawHeader                 bool
	)
	finish := func() error {
		if current == nil {
			return nil
		}
		if current.Kind() == KindComposite {
			current = nil
			return nil
		}
		if !haveParameters || !haveFixed {
			return fmt.Errorf("%w: %s needs Parameters and FixedParameters", ErrTransformFile, current.Kind())
		}
		if err := setITKParameters(current, parameters, fixed); err != nil {
			return err
		}
		transforms = append(transforms, current)
		current, parameters, fixed, haveParameters, haveFixed = nil, nil, nil, false, false
		return nil
	}
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), utf8BOM))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if lineNumber == 1 || !sawHeader {
				if !strings.HasPrefix(line, "#Insight Transform File") {
					return nil, false, fmt.Errorf("%w: not an ITK text transform file", ErrTransformFile)
				}
				if line != ITKTextHeader {
					return nil, false, fmt.Errorf("%w: unsupported version %q", ErrTransformFile, line)
				}
				sawHeader = true
			}
			continue
		}
		if !sawHeader {
			return nil, false, fmt.Errorf("%w: not an ITK text transform file", ErrTransformFile)
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, false, fmt.Errorf("%w: line %d: tags must be delimited by ':'", ErrTransformFile, lineNumber)
		}
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		switch name {
		case "Transform":
			if recordsRead >= maxHDF5Transforms {
				return nil, false, fmt.Errorf("%w: transform count", ErrTransformFile)
			}
			recordsRead++
			if err := finish(); err != nil {
				return nil, false, err
			}
			t, err := newITKParameters(value, dimension)
			if err != nil {
				return nil, false, fmt.Errorf("%w: line %d: %w", ErrTransformFile, lineNumber, err)
			}
			if t.Kind() == KindComposite {
				if len(transforms) > 0 || composite {
					return nil, false, fmt.Errorf("%w: line %d: a composite transform must open the file", ErrTransformFile, lineNumber)
				}
				composite = true
			}
			current = t
		case "Parameters", "FixedParameters":
			if current == nil {
				return nil, false, fmt.Errorf("%w: line %d: %s before a Transform", ErrTransformFile, lineNumber, name)
			}
			if current.Kind() == KindComposite {
				return nil, false, fmt.Errorf("%w: line %d: a composite transform has no parameters of its own", ErrTransformFile, lineNumber)
			}
			limit := maxTransformFileParameters
			if name == "Parameters" {
				limit = min(parameterLimit(current), remaining)
			}
			values, err := parseITKVector(value, limit)
			if err != nil {
				return nil, false, fmt.Errorf("%w: line %d: %w", ErrTransformFile, lineNumber, err)
			}
			if name == "Parameters" {
				remaining -= len(values)
				parameters, haveParameters = values, true
			} else {
				fixed, haveFixed = values, true
			}
		case "ComponentTransformFile":
			return nil, false, fmt.Errorf("%w: line %d: components in other files are not read", ErrTransformFile, lineNumber)
		default:
			return nil, false, fmt.Errorf("%w: line %d: unknown tag %q", ErrTransformFile, lineNumber, name)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrTransformFile, err)
	}
	if !sawHeader {
		return nil, false, fmt.Errorf("%w: not an ITK text transform file", ErrTransformFile)
	}
	if err := finish(); err != nil {
		return nil, false, err
	}
	if len(transforms) == 0 {
		if composite {
			return nil, false, fmt.Errorf("%w: empty composite transform", ErrTransformFile)
		}
		return nil, false, fmt.Errorf("%w: no transform", ErrTransformFile)
	}
	if dimension == 2 && len(transforms)+boolInt(composite) > maxHDF5Transforms {
		return nil, false, fmt.Errorf("%w: 2D transform count", ErrTransformFile)
	}
	return transforms, composite, nil
}

func parseITKVector(value string, limit int) ([]float64, error) {
	scanner := bufio.NewScanner(strings.NewReader(value))
	scanner.Split(bufio.ScanWords)
	values := make([]float64, 0, min(limit, 64))
	for scanner.Scan() {
		if len(values) >= limit {
			return nil, fmt.Errorf("more than %d values", limit)
		}
		field := scanner.Text()
		v, err := strconv.ParseFloat(field, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("value %q is not a finite number", field)
		}
		values = append(values, v)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

// newITKTransform creates the transform an ITK type name such as
// "AffineTransform_double_3_3" names. MatrixOffsetTransformBase, written by
// older ANTs versions, reads as an affine transform. ITK names a
// BSplineTransform whose spline order is not 3 with the order as a fifth
// field, such as "BSplineTransform_double_3_3_1"; only order 3 (cubic, the
// four-field name) is implemented, and another order is refused by name.
func newITKTransform(typeName string) (Transform, error) {
	parts := strings.Split(typeName, "_")
	splineOrder := strconv.Itoa(bsplineOrder)
	if len(parts) == 5 && parts[0] == string(KindBSpline) {
		parts, splineOrder = parts[:4], parts[4]
	}
	if len(parts) != 4 || (parts[1] != "double" && parts[1] != "float") {
		return nil, fmt.Errorf("%w: transform type %q", ErrUnsupportedTransform, typeName)
	}
	if parts[2] != "3" || parts[3] != "3" {
		return nil, fmt.Errorf("%w: %s is not three-dimensional", ErrUnsupportedTransform, typeName)
	}
	if splineOrder != strconv.Itoa(bsplineOrder) {
		return nil, fmt.Errorf("%w: %s has B-spline order %q; only order %d (cubic) is supported", ErrUnsupportedTransform, typeName, splineOrder, bsplineOrder)
	}
	switch parts[0] {
	case string(KindTranslation):
		return NewTranslation3D(), nil
	case string(KindScaleVersor3D):
		return NewScaleVersor3D(), nil
	case string(KindScaleSkewVersor3D):
		return NewScaleSkewVersor3D(), nil
	case string(KindEuler3D):
		return NewEuler3D(), nil
	case string(KindVersorRigid3D):
		return NewVersorRigid3D(), nil
	case string(KindSimilarity3D):
		return NewSimilarity3D(), nil
	case string(KindAffine), "MatrixOffsetTransformBase":
		return NewAffine(), nil
	case string(KindComposite):
		return NewComposite()
	case string(KindBSpline):
		return NewBSpline([3]int{4, 4, 4}, render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3())
	case string(KindDisplacementField):
		return NewDisplacementFieldTransform(&DisplacementField{
			Size: [3]int{1, 1, 1}, Spacing: render.Vec3{X: 1, Y: 1, Z: 1}, Direction: Identity3(), Vectors: make([]render.Vec3, 1),
		})
	}
	return nil, fmt.Errorf("%w: %s (only supported built-in three-dimensional transforms are read)", ErrUnsupportedTransform, parts[0])
}

func itkTypeName(t Transform) (string, error) {
	switch t.Kind() {
	case KindTranslation, KindScaleVersor3D, KindScaleSkewVersor3D, KindEuler3D, KindVersorRigid3D, KindSimilarity3D, KindAffine, KindComposite, KindBSpline, KindDisplacementField:
		return string(t.Kind()) + "_double_3_3", nil
	}
	return "", fmt.Errorf("%w: %s", ErrUnsupportedTransform, t.Kind())
}

// WriteITKText writes t as an ITK text transform file. A Composite is
// written as ITK writes one, its components in queue order (the last
// applied first); nested composites are not written.
func WriteITKText(w io.Writer, t Transform) error {
	if t == nil {
		return fmt.Errorf("%w: no transform", ErrTransformFile)
	}
	var b strings.Builder
	b.WriteString(ITKTextHeader + "\n")
	components := []Transform{t}
	if c, ok := t.(*Composite); ok {
		if c.Len() == 0 {
			return fmt.Errorf("%w: empty composite transform", ErrTransformFile)
		}
		components = []Transform{c}
		for i := c.Len() - 1; i >= 0; i-- {
			if c.Transform(i).Kind() == KindComposite {
				return fmt.Errorf("%w: nested composite transform", ErrTransformFile)
			}
			components = append(components, c.Transform(i))
		}
	}
	for i, component := range components {
		name, err := itkTypeName(component)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrTransformFile, err)
		}
		fmt.Fprintf(&b, "#Transform %d\nTransform: %s\n", i, name)
		if component.Kind() == KindComposite {
			continue
		}
		fmt.Fprintf(&b, "Parameters: %s\nFixedParameters: %s\n", formatITKVector(component.Parameters()), formatITKVector(component.FixedParameters()))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func formatITKVector(values []float64) string {
	fields := make([]string, len(values))
	for i, v := range values {
		fields[i] = strconv.FormatFloat(v, 'g', -1, 64)
	}
	return strings.Join(fields, " ")
}

// MATLAB Level 4 matrix header type (MOPT): M the byte order (0 little
// endian, 1 big endian), O zero, P the precision (0 double, 1 single), T
// zero for a full numeric matrix.
const (
	matlabBigEndian = 1000
	matlabSingle    = 10
)

// ReadITKMatlab reads a MATLAB Level 4 transform file as ITK writes it: per
// transform, a column vector of parameters named after the transform type,
// then one of fixed parameters. Both byte orders and both precisions are
// read.
func ReadITKMatlab(r io.Reader) ([]Transform, error) {
	records, err := readITKMatlab(r, 3)
	if err != nil {
		return nil, err
	}
	return assembleITK3D(records, false)
}

func readITKMatlab(r io.Reader, dimension int) ([]itkParameters, error) {
	var transforms []itkParameters
	for {
		name, parameters, err := readMatlabVector(r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		_, fixed, err := readMatlabVector(r)
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: %s has no fixed parameters", ErrTransformFile, name)
		}
		if err != nil {
			return nil, err
		}
		t, err := newITKParameters(name, dimension)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrTransformFile, err)
		}
		if t.Kind() == KindComposite {
			return nil, fmt.Errorf("%w: composite transforms are not read from MATLAB files", ErrTransformFile)
		}
		if err := setITKParameters(t, parameters, fixed); err != nil {
			return nil, err
		}
		if len(transforms) >= maxHDF5Transforms {
			return nil, fmt.Errorf("%w: transform count", ErrTransformFile)
		}
		transforms = append(transforms, t)
	}
	if len(transforms) == 0 {
		return nil, fmt.Errorf("%w: no transform", ErrTransformFile)
	}
	return transforms, nil
}

// readMatlabVector reads one Level 4 matrix, which must be a real column
// vector; io.EOF means the file ended before a new header.
func readMatlabVector(r io.Reader) (string, []float64, error) {
	var header [20]byte
	n, err := io.ReadFull(r, header[:])
	if n == 0 && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
		return "", nil, io.EOF
	}
	if err != nil {
		return "", nil, fmt.Errorf("%w: truncated MATLAB header", ErrTransformFile)
	}
	var order binary.ByteOrder = binary.LittleEndian
	kind := int32(order.Uint32(header[0:4]))
	if kind < 0 || kind >= matlabBigEndian {
		order = binary.BigEndian
		kind = int32(order.Uint32(header[0:4]))
		if kind < matlabBigEndian || kind >= 2*matlabBigEndian {
			return "", nil, fmt.Errorf("%w: not a MATLAB Level 4 file (type %d)", ErrTransformFile, kind)
		}
	}
	precision := kind % 100
	if kind%1000 >= 100 || (precision != 0 && precision != matlabSingle) {
		return "", nil, fmt.Errorf("%w: MATLAB type %d is not a double or single numeric matrix", ErrTransformFile, kind)
	}
	rows := int32(order.Uint32(header[4:8]))
	cols := int32(order.Uint32(header[8:12]))
	imaginary := int32(order.Uint32(header[12:16]))
	nameLength := int32(order.Uint32(header[16:20]))
	if cols != 1 || rows < 0 || rows > maxTransformFileParameters {
		return "", nil, fmt.Errorf("%w: a %d × %d matrix, only parameter vectors are read", ErrTransformFile, rows, cols)
	}
	if imaginary != 0 {
		return "", nil, fmt.Errorf("%w: complex parameters", ErrTransformFile)
	}
	if nameLength < 1 || nameLength > 256 {
		return "", nil, fmt.Errorf("%w: variable name of %d bytes", ErrTransformFile, nameLength)
	}
	nameBytes := make([]byte, nameLength)
	if _, err := io.ReadFull(r, nameBytes); err != nil {
		return "", nil, fmt.Errorf("%w: truncated MATLAB variable name", ErrTransformFile)
	}
	name := string(bytes.TrimRight(nameBytes, "\x00"))
	size := 8
	if precision == matlabSingle {
		size = 4
	}
	data := make([]byte, int(rows)*size)
	if _, err := io.ReadFull(r, data); err != nil {
		return "", nil, fmt.Errorf("%w: truncated MATLAB data for %s", ErrTransformFile, name)
	}
	values := make([]float64, rows)
	for i := range values {
		if size == 8 {
			values[i] = math.Float64frombits(order.Uint64(data[8*i:]))
		} else {
			values[i] = float64(math.Float32frombits(order.Uint32(data[4*i:])))
		}
		if math.IsNaN(values[i]) || math.IsInf(values[i], 0) {
			return "", nil, fmt.Errorf("%w: non-finite value in %s", ErrTransformFile, name)
		}
	}
	return name, values, nil
}

// WriteITKMatlab writes t as ITK's MatlabTransformIO does: little endian,
// double precision, the parameters named after the transform type, then
// the fixed parameters named "fixed". Composite transforms are not written
// (ANTs keeps each stage in a file of its own).
func WriteITKMatlab(w io.Writer, t Transform) error {
	if t == nil {
		return fmt.Errorf("%w: no transform", ErrTransformFile)
	}
	if t.Kind() == KindComposite {
		return fmt.Errorf("%w: composite transforms are not written to MATLAB files", ErrTransformFile)
	}
	if t.Kind() == KindBSpline || t.Kind() == KindDisplacementField {
		return fmt.Errorf("%w: %s transforms are written to text files only", ErrTransformFile, t.Kind())
	}
	name, err := itkTypeName(t)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrTransformFile, err)
	}
	var b bytes.Buffer
	for _, vector := range []struct {
		name   string
		values []float64
	}{{name, t.Parameters()}, {"fixed", t.FixedParameters()}} {
		for _, v := range []int32{0, int32(len(vector.values)), 1, 0, int32(len(vector.name) + 1)} {
			_ = binary.Write(&b, binary.LittleEndian, v)
		}
		b.WriteString(vector.name)
		b.WriteByte(0)
		for _, v := range vector.values {
			_ = binary.Write(&b, binary.LittleEndian, v)
		}
	}
	_, err = w.Write(b.Bytes())
	return err
}

// itkParameters is the private, dimension-independent IO portion. It has no
// point/sampling operations; factories and assemblers retain typed results.
type itkParameters interface {
	Kind() Kind
	Parameters() []float64
	FixedParameters() []float64
	SetParameters([]float64) error
	SetFixedParameters([]float64) error
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func newITKParameters(name string, dim int) (itkParameters, error) {
	if dim == 2 {
		return newITKTransform2D(name)
	}
	return newITKTransform(name)
}
func assembleITK3D(rs []itkParameters, composite bool) ([]Transform, error) {
	ts := make([]Transform, len(rs))
	for i, t := range rs {
		ts[i] = t.(Transform)
	}
	if !composite {
		return ts, nil
	}
	for i, j := 0, len(ts)-1; i < j; i, j = i+1, j-1 {
		ts[i], ts[j] = ts[j], ts[i]
	}
	c, err := NewComposite(ts...)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTransformFile, err)
	}
	return []Transform{c}, nil
}

// Validate dense fixed sizes and exact parameter count before SetFixedParameters
// can allocate a grid/field. This is IO admission, not a sampler change.
func setITKParameters(t itkParameters, p, f []float64) error {
	if t.Kind() == KindBSpline || t.Kind() == KindDisplacementField {
		if err := checkLength(f, 18, "dense fixed parameters"); err != nil {
			return fmt.Errorf("%w: %w", ErrTransformFile, err)
		}
		minimum := 1.0
		if t.Kind() == KindBSpline {
			minimum = 4
		}
		count := uint64(3)
		for _, size := range f[:3] {
			if size < minimum || size > 1<<20 || size != math.Trunc(size) || count > maxDenseTransformParameters/uint64(size) {
				return fmt.Errorf("%w: dense fixed geometry exceeds parameter limit", ErrTransformFile)
			}
			count *= uint64(size)
		}
		if count != uint64(len(p)) {
			return fmt.Errorf("%w: dense geometry requires %d parameters, got%d", ErrTransformFile, count, len(p))
		}
	}
	if err := t.SetFixedParameters(f); err != nil {
		return fmt.Errorf("%w: %s fixed parameters: %w", ErrTransformFile, t.Kind(), err)
	}
	if err := t.SetParameters(p); err != nil {
		return fmt.Errorf("%w: %s parameters: %w", ErrTransformFile, t.Kind(), err)
	}
	return nil
}
