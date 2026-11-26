package nrrd

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/render"
)

// Warning codes of a read (stable, PHI-free).
const (
	// WarningNonAnatomicalSpace: the space had no anatomical orientation
	// (scanner-xyz, 3D-right-handed…) or was absent, and was read as LPS
	// because ReadOptions.AllowNonAnatomicalSpace was set.
	WarningNonAnatomicalSpace = "non-anatomical-space-read-as-lps"
	// WarningMeasurementFrameIgnored: a measurement frame (for vector or
	// tensor data) was present and not applied.
	WarningMeasurementFrameIgnored = "measurement-frame-ignored"
	// WarningSpaceUnits: the space units were not millimetres.
	WarningSpaceUnits = "space-units-not-mm"
)

// ReadOptions configures Read and ReadFile.
type ReadOptions struct {
	Limits nifti.ReadLimits
	// AllowNonAnatomicalSpace reads a file whose space is not anatomical
	// (scanner-xyz, 3D-right-handed, 3D-left-handed) or that has only
	// per-axis spacings, as if it were LPS, and reports
	// WarningNonAnatomicalSpace. Such a grid must not be matched to DICOM.
	AllowNonAnatomicalSpace bool
}

// Image is a decoded NRRD file.
type Image struct {
	Header Header
	// Volume holds the voxels in DICOM LPS geometry; a non-spatial axis
	// (vector components, Slicer segmentation layers, time) becomes its
	// fourth dimension. A second non-spatial axis is refused with
	// ErrMultipleNonSpatialAxes.
	Volume *nifti.Volume
	// NonSpatialAxis is the NRRD axis that became the fourth dimension, or
	// -1.
	NonSpatialAxis int
	Warnings       []string
}

// Read decodes an NRRD file with attached data from r. A detached header
// (data file) needs ReadFile.
func Read(ctx context.Context, r io.Reader, options ReadOptions) (*Image, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	buffered := bufio.NewReaderSize(r, 64<<10)
	header, err := ParseHeader(buffered)
	if err != nil {
		return nil, err
	}
	if header.DataFile != "" {
		return nil, fmt.Errorf("%w: detached data file; use ReadFile", ErrUnsupported)
	}
	return decode(ctx, header, buffered, nil, options)
}

// ReadFile reads an .nrrd file, or an .nhdr header with its detached data
// file. The data file must be a relative path inside the header's directory
// (no absolute paths or "..", and no file lists).
func ReadFile(ctx context.Context, path string, options ReadOptions) (*Image, error) {
	if ctx == nil {
		ctx = context.Background()
	}
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
	if header.DataFile == "" {
		return decode(ctx, header, buffered, nil, options)
	}
	name := header.DataFile
	if strings.HasPrefix(strings.ToUpper(name), "LIST") || strings.ContainsAny(name, "%") || strings.Contains(name, " ") {
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

var datatypes = map[string]int16{
	"int8": nifti.DatatypeInt8, "uint8": nifti.DatatypeUint8, "int16": nifti.DatatypeInt16, "uint16": nifti.DatatypeUint16,
	"int32": nifti.DatatypeInt32, "uint32": nifti.DatatypeUint32, "float": nifti.DatatypeFloat32, "double": nifti.DatatypeFloat64,
}

// layout is the placement of the NRRD axes in the volume.
type layout struct {
	spatial    []int // NRRD axes carrying i, j (and k)
	nonSpatial int   // NRRD axis carrying t, or -1
	dims       [4]int
	indexToLPS render.GeometryAffine
	warnings   []string
}

func planLayout(h Header, options ReadOptions) (layout, error) {
	l := layout{nonSpatial: -1}
	anatomical := false
	var signs [3]float64
	if s, ok := spaceAxes[h.Space]; ok {
		anatomical, signs = true, s
	}
	if h.Space != "" && !anatomical {
		if strings.HasSuffix(h.Space, "-time") {
			return layout{}, fmt.Errorf("%w: space %q with a time axis", ErrUnsupported, h.Space)
		}
		if !options.AllowNonAnatomicalSpace {
			return layout{}, fmt.Errorf("%w: space %q", ErrNoPatientSpace, h.Space)
		}
	}
	if !anatomical {
		signs = [3]float64{1, 1, 1}
	}
	if h.SpaceDim != 0 && h.SpaceDim != 3 {
		return layout{}, fmt.Errorf("%w: space dimension %d", ErrUnsupported, h.SpaceDim)
	}
	var columns [3]render.Vec3
	if h.Directions != nil {
		for axis, direction := range h.Directions {
			if direction == nil {
				if l.nonSpatial >= 0 {
					return layout{}, fmt.Errorf("%w: axes %d and %d", ErrMultipleNonSpatialAxes, l.nonSpatial, axis)
				}
				l.nonSpatial = axis
				continue
			}
			if len(l.spatial) == 3 {
				return layout{}, fmt.Errorf("%w: more than three spatial axes", ErrUnsupported)
			}
			columns[len(l.spatial)] = render.Vec3{X: signs[0] * direction[0], Y: signs[1] * direction[1], Z: signs[2] * direction[2]}
			l.spatial = append(l.spatial, axis)
		}
	} else {
		if h.Space != "" || !options.AllowNonAnatomicalSpace {
			return layout{}, fmt.Errorf("%w: no space directions", ErrNoPatientSpace)
		}
		for axis := range h.Sizes {
			kind := h.Kinds[axis]
			if kind != "" && kind != "domain" && kind != "space" {
				if l.nonSpatial >= 0 {
					return layout{}, fmt.Errorf("%w: axes %d and %d", ErrMultipleNonSpatialAxes, l.nonSpatial, axis)
				}
				l.nonSpatial = axis
				continue
			}
			if len(l.spatial) == 3 {
				return layout{}, fmt.Errorf("%w: more than three spatial axes", ErrUnsupported)
			}
			spacing := h.Spacings[axis]
			if math.IsNaN(spacing) {
				spacing = 1
			}
			var column render.Vec3
			switch len(l.spatial) {
			case 0:
				column.X = spacing
			case 1:
				column.Y = spacing
			default:
				column.Z = spacing
			}
			columns[len(l.spatial)] = column
			l.spatial = append(l.spatial, axis)
		}
	}
	if !anatomical {
		l.warnings = append(l.warnings, WarningNonAnatomicalSpace)
	}
	switch len(l.spatial) {
	case 2:
		normal := columns[0].Cross(columns[1]).Normalize()
		if normal.Length() == 0 {
			return layout{}, fmt.Errorf("%w: degenerate space directions", ErrMalformedHeader)
		}
		columns[2] = normal
	case 3:
	default:
		return layout{}, fmt.Errorf("%w: %d spatial axes", ErrUnsupported, len(l.spatial))
	}
	if l.nonSpatial > 0 && l.nonSpatial < len(h.Sizes)-1 {
		return layout{}, fmt.Errorf("%w: non-spatial axis between spatial axes", ErrUnsupported)
	}
	for index, axis := range l.spatial {
		l.dims[index] = h.Sizes[axis]
	}
	if len(l.spatial) == 2 {
		l.dims[2] = 1
	}
	l.dims[3] = 1
	if l.nonSpatial >= 0 {
		l.dims[3] = h.Sizes[l.nonSpatial]
	}
	var origin render.Vec3
	if h.Origin != nil {
		origin = render.Vec3{X: signs[0] * h.Origin[0], Y: signs[1] * h.Origin[1], Z: signs[2] * h.Origin[2]}
	}
	for row, get := range []func(render.Vec3) float64{
		func(v render.Vec3) float64 { return v.X },
		func(v render.Vec3) float64 { return v.Y },
		func(v render.Vec3) float64 { return v.Z },
	} {
		for column := 0; column < 3; column++ {
			l.indexToLPS[4*row+column] = get(columns[column])
		}
		l.indexToLPS[4*row+3] = get(origin)
	}
	l.indexToLPS[15] = 1
	for _, unit := range h.SpaceUnits {
		if unit != "mm" && unit != "" {
			l.warnings = append(l.warnings, WarningSpaceUnits)
			break
		}
	}
	if _, ok := h.Fields["measurement frame"]; ok {
		l.warnings = append(l.warnings, WarningMeasurementFrameIgnored)
	}
	return l, nil
}

func decode(ctx context.Context, h Header, data *bufio.Reader, fileSize *int64, options ReadOptions) (*Image, error) {
	datatype, ok := datatypes[h.Type]
	if !ok {
		return nil, fmt.Errorf("%w: type %s", ErrUnsupported, h.Type)
	}
	width := h.bytesPerSample()
	binaryEncoding := h.Encoding != "text"
	if width > 1 && binaryEncoding && h.Endian == "" {
		return nil, fmt.Errorf("%w: no endian for a %d-byte type", ErrMalformedHeader, width)
	}
	l, err := planLayout(h, options)
	if err != nil {
		return nil, err
	}
	limits := options.Limits
	if limits.MaxVoxels == 0 || limits.MaxPayloadBytes == 0 {
		defaults := nifti.DefaultReadLimits()
		if limits.MaxVoxels == 0 {
			limits.MaxVoxels = defaults.MaxVoxels
		}
		if limits.MaxPayloadBytes == 0 {
			limits.MaxPayloadBytes = defaults.MaxPayloadBytes
		}
	}
	voxels := uint64(1)
	for _, size := range h.Sizes {
		if voxels > limits.MaxVoxels/uint64(size) {
			return nil, fmt.Errorf("%w: voxel count exceeds %d", ErrLimitExceeded, limits.MaxVoxels)
		}
		voxels *= uint64(size)
	}
	byteLimit := min(limits.MaxPayloadBytes, uint64(math.MaxInt))
	if voxels > byteLimit/uint64(width) {
		return nil, fmt.Errorf("%w: payload exceeds %d bytes", ErrLimitExceeded, byteLimit)
	}
	payloadBytes := voxels * uint64(width)
	for line := 0; line < h.LineSkip; line++ {
		if _, err := data.ReadString('\n'); err != nil {
			return nil, ErrTruncatedData
		}
	}
	var stream io.Reader = data
	if h.ByteSkip == -1 {
		if h.Encoding != "raw" || fileSize == nil {
			return nil, fmt.Errorf("%w: byte skip -1 needs raw data in a detached file", ErrUnsupported)
		}
		skip := *fileSize - int64(payloadBytes)
		if skip < 0 {
			return nil, ErrTruncatedData
		}
		if _, err := io.CopyN(io.Discard, data, skip); err != nil {
			return nil, ErrTruncatedData
		}
	}
	switch h.Encoding {
	case "gzip":
		decompressor, err := gzip.NewReader(data)
		if err != nil {
			return nil, fmt.Errorf("%w: gzip: %v", ErrMalformedHeader, err)
		}
		defer decompressor.Close()
		stream = decompressor
	case "bzip2":
		stream = bzip2.NewReader(data)
	case "hex":
		stream = &hexReader{r: data}
	}
	if h.ByteSkip > 0 {
		if _, err := io.CopyN(io.Discard, stream, int64(h.ByteSkip)); err != nil {
			return nil, ErrTruncatedData
		}
	}
	payloadStream := stream
	bigEndian := h.Endian == "big"
	if h.Encoding == "text" {
		payload, err := decodeText(ctx, stream, h.Type, int(voxels))
		if err != nil {
			return nil, err
		}
		stream, bigEndian = bytes.NewReader(payload), false
	}
	if l.nonSpatial == 0 && l.dims[3] > 1 {
		// Samples are interleaved (component fastest); the volume stores
		// each component as a whole 3D grid.
		interleaved := make([]byte, payloadBytes)
		if _, err := io.ReadFull(stream, interleaved); err != nil {
			return nil, ErrTruncatedData
		}
		stream = bytes.NewReader(deinterleave(interleaved, l.dims[3], width))
	}
	volume, err := nifti.NewVolume(ctx, io.LimitReader(stream, int64(payloadBytes)), nifti.NewVolumeOptions{
		Datatype:   datatype,
		Dimensions: l.dims,
		IndexToLPS: l.indexToLPS,
		BigEndian:  bigEndian,
		Limits:     limits,
	})
	if err != nil {
		return nil, err
	}
	if h.Encoding == "gzip" || h.Encoding == "bzip2" {
		// Both the voxel limit and deinterleaving can hide the end of the
		// compressed stream. Verify the trailer without draining extra voxels.
		var probe [1]byte
		count, err := io.ReadFull(payloadStream, probe[:])
		switch {
		case count != 0:
			return nil, fmt.Errorf("%w: %s payload exceeds declared dimensions", ErrMalformedHeader, h.Encoding)
		case errors.Is(err, io.ErrUnexpectedEOF):
			return nil, fmt.Errorf("%w: %s payload: %w", ErrTruncatedData, h.Encoding, err)
		case !errors.Is(err, io.EOF):
			return nil, fmt.Errorf("nrrd: %s payload: %w", h.Encoding, err)
		}
	}
	return &Image{Header: h, Volume: volume, NonSpatialAxis: l.nonSpatial, Warnings: append(l.warnings, volume.Warnings...)}, nil
}

func deinterleave(data []byte, components, width int) []byte {
	out := make([]byte, len(data))
	points := len(data) / (components * width)
	for p := 0; p < points; p++ {
		for c := 0; c < components; c++ {
			copy(out[(c*points+p)*width:(c*points+p+1)*width], data[(p*components+c)*width:])
		}
	}
	return out
}

// decodeText parses whitespace-separated numbers into little-endian samples.
func decodeText(ctx context.Context, r io.Reader, typ string, count int) ([]byte, error) {
	width := map[string]int{"int8": 1, "uint8": 1, "int16": 2, "uint16": 2, "int32": 4, "uint32": 4, "float": 4, "double": 8}[typ]
	out := make([]byte, 0, count*width)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	scanner.Split(bufio.ScanWords)
	for n := 0; n < count; n++ {
		if n&65535 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if !scanner.Scan() {
			return nil, ErrTruncatedData
		}
		word := scanner.Text()
		switch typ {
		case "float", "double":
			value, err := strconv.ParseFloat(word, 64)
			if err != nil {
				return nil, fmt.Errorf("%w: text sample %d", ErrMalformedHeader, n)
			}
			if typ == "float" {
				out = binary.LittleEndian.AppendUint32(out, math.Float32bits(float32(value)))
			} else {
				out = binary.LittleEndian.AppendUint64(out, math.Float64bits(value))
			}
		default:
			value, err := strconv.ParseInt(word, 10, 64)
			if err != nil || !inRange(typ, value) {
				return nil, fmt.Errorf("%w: text sample %d", ErrMalformedHeader, n)
			}
			switch width {
			case 1:
				out = append(out, byte(value))
			case 2:
				out = binary.LittleEndian.AppendUint16(out, uint16(value))
			default:
				out = binary.LittleEndian.AppendUint32(out, uint32(value))
			}
		}
	}
	return out, nil
}

func inRange(typ string, v int64) bool {
	switch typ {
	case "int8":
		return v >= math.MinInt8 && v <= math.MaxInt8
	case "uint8":
		return v >= 0 && v <= math.MaxUint8
	case "int16":
		return v >= math.MinInt16 && v <= math.MaxInt16
	case "uint16":
		return v >= 0 && v <= math.MaxUint16
	case "int32":
		return v >= math.MinInt32 && v <= math.MaxInt32
	default:
		return v >= 0 && v <= math.MaxUint32
	}
}

// hexReader decodes pairs of hexadecimal digits, skipping whitespace.
type hexReader struct {
	r    *bufio.Reader
	high int
}

func (h *hexReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c, err := h.r.ReadByte()
		if err != nil {
			if n > 0 {
				return n, nil
			}
			return 0, err
		}
		var v byte
		switch {
		case c >= '0' && c <= '9':
			v = c - '0'
		case c >= 'a' && c <= 'f':
			v = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			v = c - 'A' + 10
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			continue
		default:
			return n, fmt.Errorf("%w: hex data", ErrMalformedHeader)
		}
		if h.high == 0 {
			h.high = int(v) + 1
			continue
		}
		p[n] = byte(h.high-1)<<4 | v
		h.high = 0
		n++
	}
	return n, nil
}
