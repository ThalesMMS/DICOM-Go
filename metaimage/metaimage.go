// Package metaimage reads and writes MetaImage files (.mha with inline data,
// .mhd with a separate raw file) into the library's scalar volume
// (nifti.Volume) in DICOM patient (LPS) coordinates. It is implemented from
// the MetaIO file format description as ITK writes it (TransformMatrix row i
// is the LPS direction of axis i, Offset the LPS position of the first
// voxel); no MetaIO code is used.
//
// A file with FileFormatVersion = 1 keeps the image geometry in
// ElementOrigin and ElementDirection, and Offset and TransformMatrix are the
// object-to-parent transform of a SpatialObject scene. As ITK's MetaImageIO
// reads such a file, the element geometry becomes the volume geometry and
// the object-to-parent transform is not applied (WarningObjectTransformIgnored
// reports one that is not the identity). Other versions are refused; Write
// always writes version 0.
package metaimage

import (
	"bufio"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/render"
)

var (
	// ErrNotMetaImage reports a header without MetaImage fields.
	ErrNotMetaImage = errors.New("metaimage: not a MetaImage header")
	// ErrMalformedHeader reports a field that cannot be parsed or
	// contradicts another.
	ErrMalformedHeader = errors.New("metaimage: malformed header")
	// ErrUnsupported reports a valid MetaImage feature this package does not
	// read (multi-channel or 64-bit elements, file lists, NDims above 4…).
	ErrUnsupported = errors.New("metaimage: unsupported feature")
	// ErrLimitExceeded reports a header or payload beyond the read limits.
	ErrLimitExceeded = errors.New("metaimage: limit exceeded")
	// ErrTruncatedData reports a payload shorter than the header declares.
	ErrTruncatedData = errors.New("metaimage: truncated data")
)

// MaxHeaderBytes bounds the header text read before the payload.
const MaxHeaderBytes = 1 << 20

// WarningOrientationIgnored: AnatomicalOrientation disagreed with the
// direction matrix, which was used (as ITK does).
const WarningOrientationIgnored = "anatomical-orientation-ignored"

// WarningObjectTransformIgnored: a FileFormatVersion 1 file had a
// non-identity object-to-parent transform (Offset, TransformMatrix) besides
// its ElementOrigin and ElementDirection; the element geometry was used and
// the object transform was not applied (as ITK does).
const WarningObjectTransformIgnored = "object-to-parent-transform-ignored"

// Header is a parsed MetaImage header: Fields keeps every key (keys in their
// file spelling, order in Keys) with its raw value.
type Header struct {
	Fields map[string]string
	Keys   []string

	// FileFormatVersion is the MetaIO file format version: 0 (absent) or 1.
	FileFormatVersion int

	NDims       int
	DimSize     []int
	ElementType string
	Spacing     []float64
	// Offset and Direction are the image geometry: in version 0 the first
	// present of ImagePosition, Origin, Position and Offset, and of
	// TransformMatrix, Rotation and Orientation; in version 1
	// ElementOrigin and ElementDirection, each falling back to
	// ImagePosition or Offset and to TransformMatrix when absent, as ITK
	// reads them.
	Offset         []float64
	Direction      []float64 // NDims×NDims, row i = direction of axis i
	BigEndian      bool
	Compressed     bool
	CompressedSize int64 // 0 when absent
	HeaderSize     int64 // bytes to skip in the data file; -1: data at its end
	DataFile       string

	// objectTransformIgnored: a version 1 non-identity object-to-parent
	// transform that the element geometry replaced.
	objectTransformIgnored bool
}

var datatypes = map[string]int16{
	"MET_CHAR": nifti.DatatypeInt8, "MET_UCHAR": nifti.DatatypeUint8,
	"MET_SHORT": nifti.DatatypeInt16, "MET_USHORT": nifti.DatatypeUint16,
	"MET_INT": nifti.DatatypeInt32, "MET_UINT": nifti.DatatypeUint32,
	"MET_FLOAT": nifti.DatatypeFloat32, "MET_DOUBLE": nifti.DatatypeFloat64,
}

var elementBytes = map[string]int{
	"MET_CHAR": 1, "MET_UCHAR": 1, "MET_SHORT": 2, "MET_USHORT": 2,
	"MET_INT": 4, "MET_UINT": 4, "MET_FLOAT": 4, "MET_DOUBLE": 8,
}

// ParseHeader reads "Key = Value" lines up to and including ElementDataFile,
// which ends the header; r is left at the first payload byte of a LOCAL
// file.
func ParseHeader(r *bufio.Reader) (Header, error) {
	h := Header{Fields: map[string]string{}}
	read := 0
	for {
		line, err := r.ReadString('\n')
		read += len(line)
		if read > MaxHeaderBytes {
			return Header{}, fmt.Errorf("%w: header longer than %d bytes", ErrLimitExceeded, MaxHeaderBytes)
		}
		if err != nil && !(errors.Is(err, io.EOF) && line != "") {
			if errors.Is(err, io.EOF) {
				break
			}
			return Header{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			if errors.Is(err, io.EOF) {
				break
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return Header{}, fmt.Errorf("%w: line %q", ErrMalformedHeader, truncate(line))
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		canonical := strings.ToLower(key)
		if _, dup := h.Fields[canonical]; dup {
			return Header{}, fmt.Errorf("%w: key %q repeated", ErrMalformedHeader, truncate(key))
		}
		h.Fields[canonical] = value
		h.Keys = append(h.Keys, key)
		if canonical == "elementdatafile" {
			break
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	if _, ok := h.Fields["elementdatafile"]; !ok {
		if len(h.Fields) == 0 {
			return Header{}, ErrNotMetaImage
		}
		return Header{}, fmt.Errorf("%w: no ElementDataFile", ErrMalformedHeader)
	}
	return h, h.parse()
}

// parseVersion1Geometry reads the image geometry of a FileFormatVersion 1
// header as ITK's MetaImageIO does: ElementOrigin and ElementDirection, each
// falling back to the object-to-parent Offset or TransformMatrix when absent
// (the version 0 aliases Origin, Position, Rotation and Orientation are not
// read). The object-to-parent transform is not composed with the element
// geometry; a non-identity one is reported.
func (h *Header) parseVersion1Geometry(floats func(int, ...string) ([]float64, bool, error)) error {
	n := h.NDims
	objectOffset, hasObjectOffset, err := floats(n, "Offset")
	if err != nil {
		return err
	}
	objectMatrix, hasObjectMatrix, err := floats(n*n, "TransformMatrix")
	if err != nil {
		return err
	}
	origin, hasOrigin, err := floats(n, "ElementOrigin")
	if err != nil {
		return err
	}
	// MetaImage reads ImagePosition into the offset before it converts a
	// version 1 header, so ImagePosition replaces Offset and ElementOrigin
	// replaces both (#2041).
	imagePosition, hasImagePosition, err := floats(n, "ImagePosition")
	if err != nil {
		return err
	}
	direction, hasDirection, err := floats(n*n, "ElementDirection")
	if err != nil {
		return err
	}
	switch {
	case hasOrigin:
		h.Offset = origin
		if hasObjectOffset && !allZero(objectOffset) {
			h.objectTransformIgnored = true
		}
	case hasImagePosition:
		h.Offset = imagePosition
	case hasObjectOffset:
		h.Offset = objectOffset
	default:
		h.Offset = make([]float64, n)
	}
	switch {
	case hasDirection:
		h.Direction = direction
		if hasObjectMatrix && !slices.Equal(objectMatrix, identity(n)) {
			h.objectTransformIgnored = true
		}
	case hasObjectMatrix:
		h.Direction = objectMatrix
	default:
		h.Direction = identity(n)
	}
	return nil
}

func identity(n int) []float64 {
	out := make([]float64, n*n)
	for i := 0; i < n; i++ {
		out[i*n+i] = 1
	}
	return out
}

func allZero(values []float64) bool {
	for _, v := range values {
		if v != 0 {
			return false
		}
	}
	return true
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

func (h *Header) parse() error {
	field := func(names ...string) (string, bool) {
		for _, name := range names {
			if value, ok := h.Fields[strings.ToLower(name)]; ok {
				return value, true
			}
		}
		return "", false
	}
	if objectType, ok := field("ObjectType"); ok && !strings.EqualFold(objectType, "Image") {
		return fmt.Errorf("%w: ObjectType %q", ErrUnsupported, truncate(objectType))
	}
	ndims, ok := field("NDims")
	if !ok {
		return fmt.Errorf("%w: no NDims", ErrMalformedHeader)
	}
	n, err := strconv.Atoi(ndims)
	if err != nil || n < 1 {
		return fmt.Errorf("%w: NDims %q", ErrMalformedHeader, truncate(ndims))
	}
	if n > 4 {
		return fmt.Errorf("%w: NDims %d", ErrUnsupported, n)
	}
	h.NDims = n
	if version, ok := field("FileFormatVersion"); ok {
		v, err := strconv.Atoi(version)
		if err != nil || v < 0 {
			return fmt.Errorf("%w: FileFormatVersion %q", ErrMalformedHeader, truncate(version))
		}
		if v > 1 {
			return fmt.Errorf("%w: FileFormatVersion %d", ErrUnsupported, v)
		}
		h.FileFormatVersion = v
	}
	ints := func(name string, count int) ([]int, error) {
		value, ok := field(name)
		if !ok {
			return nil, fmt.Errorf("%w: no %s", ErrMalformedHeader, name)
		}
		parts := strings.Fields(value)
		if len(parts) != count {
			return nil, fmt.Errorf("%w: %s has %d values, want %d", ErrMalformedHeader, name, len(parts), count)
		}
		out := make([]int, count)
		for i, part := range parts {
			v, err := strconv.Atoi(part)
			if err != nil || v < 1 {
				return nil, fmt.Errorf("%w: %s %q", ErrMalformedHeader, name, truncate(value))
			}
			out[i] = v
		}
		return out, nil
	}
	floats := func(count int, names ...string) ([]float64, bool, error) {
		value, ok := field(names...)
		if !ok {
			return nil, false, nil
		}
		parts := strings.Fields(value)
		if len(parts) != count {
			return nil, true, fmt.Errorf("%w: %s has %d values, want %d", ErrMalformedHeader, names[0], len(parts), count)
		}
		out := make([]float64, count)
		for i, part := range parts {
			v, err := strconv.ParseFloat(part, 64)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, true, fmt.Errorf("%w: %s %q", ErrMalformedHeader, names[0], truncate(value))
			}
			out[i] = v
		}
		return out, true, nil
	}
	if h.DimSize, err = ints("DimSize", n); err != nil {
		return err
	}
	elementType, ok := field("ElementType")
	if !ok {
		return fmt.Errorf("%w: no ElementType", ErrMalformedHeader)
	}
	h.ElementType = strings.ToUpper(elementType)
	if _, ok := datatypes[h.ElementType]; !ok {
		return fmt.Errorf("%w: ElementType %s", ErrUnsupported, truncate(elementType))
	}
	if channels, ok := field("ElementNumberOfChannels"); ok && strings.TrimSpace(channels) != "1" {
		return fmt.Errorf("%w: %s channels", ErrUnsupported, truncate(channels))
	}
	spacing, found, err := floats(n, "ElementSpacing")
	if err != nil {
		return err
	}
	if !found {
		if spacing, found, err = floats(n, "ElementSize"); err != nil {
			return err
		}
	}
	if !found {
		spacing = make([]float64, n)
		for i := range spacing {
			spacing[i] = 1
		}
	}
	for _, s := range spacing {
		if s <= 0 {
			return fmt.Errorf("%w: ElementSpacing must be positive", ErrMalformedHeader)
		}
	}
	h.Spacing = spacing
	if h.FileFormatVersion == 0 {
		// MetaIO reads Offset, then Position, then Origin, and MetaImage then
		// ImagePosition, each replacing the one before (#2041).
		if h.Offset, found, err = floats(n, "ImagePosition", "Origin", "Position", "Offset"); err != nil {
			return err
		}
		if !found {
			h.Offset = make([]float64, n)
		}
		if h.Direction, found, err = floats(n*n, "TransformMatrix", "Rotation", "Orientation"); err != nil {
			return err
		}
		if !found {
			h.Direction = identity(n)
		}
	} else if err := h.parseVersion1Geometry(floats); err != nil {
		return err
	}
	boolean := func(names ...string) (bool, error) {
		value, ok := field(names...)
		if !ok {
			return false, nil
		}
		switch strings.ToLower(value) {
		case "true", "1":
			return true, nil
		case "false", "0":
			return false, nil
		}
		return false, fmt.Errorf("%w: %s %q", ErrMalformedHeader, names[0], truncate(value))
	}
	if h.BigEndian, err = boolean("BinaryDataByteOrderMSB", "ElementByteOrderMSB"); err != nil {
		return err
	}
	if h.Compressed, err = boolean("CompressedData"); err != nil {
		return err
	}
	if binary, ok := field("BinaryData"); ok && strings.EqualFold(binary, "False") {
		return fmt.Errorf("%w: ASCII element data", ErrUnsupported)
	}
	if value, ok := field("CompressedDataSize"); ok {
		if h.CompressedSize, err = strconv.ParseInt(value, 10, 64); err != nil || h.CompressedSize < 0 {
			return fmt.Errorf("%w: CompressedDataSize", ErrMalformedHeader)
		}
	}
	if value, ok := field("HeaderSize"); ok {
		if h.HeaderSize, err = strconv.ParseInt(value, 10, 64); err != nil || h.HeaderSize < -1 {
			return fmt.Errorf("%w: HeaderSize", ErrMalformedHeader)
		}
	}
	h.DataFile, _ = field("ElementDataFile")
	return nil
}

// ReadOptions configures Read and ReadFile.
type ReadOptions struct {
	Limits nifti.ReadLimits
}

// Image is a decoded MetaImage file.
type Image struct {
	Header   Header
	Volume   *nifti.Volume
	Warnings []string
}

// Read decodes a MetaImage with inline data (ElementDataFile = LOCAL).
func Read(ctx context.Context, r io.Reader, options ReadOptions) (*Image, error) {
	buffered := bufio.NewReaderSize(r, 64<<10)
	header, err := ParseHeader(buffered)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(header.DataFile, "LOCAL") {
		return nil, fmt.Errorf("%w: separate data file; use ReadFile", ErrUnsupported)
	}
	return decode(ctx, header, buffered, nil, options)
}

// ReadFile reads an .mha file, or an .mhd header with its data file, which
// must be a relative path inside the header's directory (no absolute paths,
// "..", LIST or file patterns).
func ReadFile(ctx context.Context, path string, options ReadOptions) (*Image, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	buffered := bufio.NewReaderSize(file, 64<<10)
	header, err := ParseHeader(buffered)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(header.DataFile, "LOCAL") {
		return decode(ctx, header, buffered, nil, options)
	}
	name := header.DataFile
	if strings.HasPrefix(strings.ToUpper(name), "LIST") || strings.Contains(name, "%") || strings.Contains(name, " ") {
		return nil, fmt.Errorf("%w: data file lists and patterns", ErrUnsupported)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.VolumeName(clean) != "" {
		return nil, fmt.Errorf("%w: data file %q outside the header directory", ErrMalformedHeader, truncate(name))
	}
	data, err := os.Open(filepath.Join(filepath.Dir(path), clean))
	if err != nil {
		return nil, err
	}
	defer data.Close()
	info, err := data.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	return decode(ctx, header, bufio.NewReaderSize(data, 64<<10), &size, options)
}

func decode(ctx context.Context, h Header, data *bufio.Reader, fileSize *int64, options ReadOptions) (*Image, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	limits := options.Limits
	defaults := nifti.DefaultReadLimits()
	if limits.MaxVoxels == 0 {
		limits.MaxVoxels = defaults.MaxVoxels
	}
	if limits.MaxPayloadBytes == 0 {
		limits.MaxPayloadBytes = defaults.MaxPayloadBytes
	}
	voxels := uint64(1)
	for _, size := range h.DimSize {
		if voxels > limits.MaxVoxels/uint64(size) {
			return nil, fmt.Errorf("%w: voxel count exceeds %d", ErrLimitExceeded, limits.MaxVoxels)
		}
		voxels *= uint64(size)
	}
	width := uint64(elementBytes[h.ElementType])
	byteLimit := min(limits.MaxPayloadBytes, uint64(math.MaxInt))
	if voxels > byteLimit/width {
		return nil, fmt.Errorf("%w: payload exceeds %d bytes", ErrLimitExceeded, byteLimit)
	}
	payloadBytes := int64(voxels * width)
	skip := h.HeaderSize
	if skip == -1 {
		if fileSize == nil || h.Compressed {
			return nil, fmt.Errorf("%w: HeaderSize -1 needs uncompressed data in a separate file", ErrUnsupported)
		}
		if skip = *fileSize - payloadBytes; skip < 0 {
			return nil, ErrTruncatedData
		}
	}
	if skip > 0 {
		if _, err := io.CopyN(io.Discard, data, skip); err != nil {
			return nil, ErrTruncatedData
		}
	}
	var stream io.Reader = data
	if h.Compressed {
		source := io.Reader(data)
		if h.CompressedSize > 0 {
			source = io.LimitReader(data, h.CompressedSize)
		}
		decompressor, err := zlib.NewReader(source)
		if err != nil {
			return nil, fmt.Errorf("%w: zlib: %v", ErrMalformedHeader, err)
		}
		defer decompressor.Close()
		stream = decompressor
	}
	n := h.NDims
	spatial := min(n, 3)
	dims := [4]int{1, 1, 1, 1}
	copy(dims[:], h.DimSize)
	var columns [3]render.Vec3
	for axis := 0; axis < spatial; axis++ {
		var d [3]float64
		for j := 0; j < spatial; j++ {
			d[j] = h.Direction[axis*n+j] * h.Spacing[axis]
		}
		columns[axis] = render.Vec3{X: d[0], Y: d[1], Z: d[2]}
	}
	switch spatial {
	case 1:
		columns[1] = render.Vec3{Y: 1}
		columns[2] = render.Vec3{Z: 1}
	case 2:
		columns[2] = columns[0].Cross(columns[1]).Normalize()
	}
	var origin render.Vec3
	o := append(h.Offset[:spatial:spatial], make([]float64, 3-spatial)...)
	origin = render.Vec3{X: o[0], Y: o[1], Z: o[2]}
	var affine render.GeometryAffine
	for column := 0; column < 3; column++ {
		affine[column] = columns[column].X
		affine[4+column] = columns[column].Y
		affine[8+column] = columns[column].Z
	}
	affine[3], affine[7], affine[11], affine[15] = origin.X, origin.Y, origin.Z, 1
	volume, err := nifti.NewVolume(ctx, io.LimitReader(stream, payloadBytes), nifti.NewVolumeOptions{
		Datatype: datatypes[h.ElementType], Dimensions: dims, IndexToLPS: affine, BigEndian: h.BigEndian, Limits: limits,
	})
	if err != nil {
		return nil, err
	}
	if h.Compressed {
		// NewVolume saw EOF at the voxel limit. Require the zlib reader itself
		// to reach EOF so it checks the trailer before we expose any voxels.
		var probe [1]byte
		count, err := io.ReadFull(stream, probe[:])
		switch {
		case count != 0:
			return nil, fmt.Errorf("%w: zlib payload exceeds declared dimensions", ErrMalformedHeader)
		case errors.Is(err, io.ErrUnexpectedEOF):
			return nil, fmt.Errorf("%w: zlib payload: %w", ErrTruncatedData, err)
		case !errors.Is(err, io.EOF):
			return nil, fmt.Errorf("metaimage: zlib payload: %w", err)
		}
	}
	image := &Image{Header: h, Volume: volume, Warnings: volume.Warnings}
	if h.objectTransformIgnored {
		image.Warnings = append(image.Warnings, WarningObjectTransformIgnored)
	}
	if orientation, ok := h.Fields["anatomicalorientation"]; ok && n == 3 {
		if want := anatomicalOrientation(columns); !strings.EqualFold(orientation, want) && !strings.EqualFold(orientation, "???") {
			image.Warnings = append(image.Warnings, WarningOrientationIgnored)
		}
	}
	return image, nil
}

// anatomicalOrientation is the MetaIO orientation code of three LPS
// direction columns: for each axis, the anatomical side its index starts
// from (R for an axis increasing towards the left, and so on), after the
// dominant component.
func anatomicalOrientation(columns [3]render.Vec3) string {
	var b strings.Builder
	for _, c := range columns {
		v := [3]float64{c.X, c.Y, c.Z}
		best := 0
		for i := 1; i < 3; i++ {
			if math.Abs(v[i]) > math.Abs(v[best]) {
				best = i
			}
		}
		from := [3][2]byte{{'R', 'L'}, {'A', 'P'}, {'I', 'S'}}[best]
		if v[best] >= 0 {
			b.WriteByte(from[0])
		} else {
			b.WriteByte(from[1])
		}
	}
	return b.String()
}

var elementTypeByDatatype = map[int16]string{
	nifti.DatatypeInt8: "MET_CHAR", nifti.DatatypeUint8: "MET_UCHAR", nifti.DatatypeInt16: "MET_SHORT", nifti.DatatypeUint16: "MET_USHORT",
	nifti.DatatypeInt32: "MET_INT", nifti.DatatypeUint32: "MET_UINT", nifti.DatatypeFloat32: "MET_FLOAT", nifti.DatatypeFloat64: "MET_DOUBLE",
}

// reservedFields are the fields Write sets itself.
var reservedFields = map[string]bool{
	"objecttype": true, "ndims": true, "binarydata": true, "binarydatabyteordermsb": true, "elementbyteordermsb": true,
	"compresseddata": true, "compresseddatasize": true, "transformmatrix": true, "rotation": true, "orientation": true,
	"offset": true, "origin": true, "position": true, "centerofrotation": true, "anatomicalorientation": true,
	"elementspacing": true, "elementsize": true, "dimsize": true, "elementtype": true, "elementdatafile": true,
	"headersize": true, "elementnumberofchannels": true,
	// Write produces version 0 geometry; version 1 fields would contradict it.
	"fileformatversion": true, "elementorigin": true, "elementdirection": true,
}

func formatNumber(v float64) string {
	if v == 0 {
		return "0"
	}
	return strconv.FormatFloat(v, 'g', 17, 64)
}
