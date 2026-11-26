package nifti_test

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/render"
)

// synthHeader describes a handcrafted third-party-style NIfTI header. Every
// value is written exactly as given so tests can reproduce what other writers
// (nibabel, ITK, dcm2niix, FSL, etc.) put on disk, including inconsistent
// headers.
type synthHeader struct {
	version   int // 1 (default) or 2
	bigEndian bool
	paired    bool
	dims      []int64 // nx, ny, nz[, nt, ...]; dim[0] = len(dims)
	datatype  int16
	bitpix    int16 // zero derives it from datatype
	pixdim    [8]float64
	voxOffset float64 // negative selects the natural offset
	slope     float64
	inter     float64
	qformCode int
	sformCode int
	quatern   [3]float64
	qoffset   [3]float64
	srow      [3][4]float64
	xyztUnits uint8
	magic     string // overrides the magic when non-empty
	extension []byte // appended after a set extender flag (single file)
}

func defaultSynth() synthHeader {
	return synthHeader{
		version:   1,
		dims:      []int64{2, 1, 2},
		datatype:  nifti.DatatypeUint8,
		pixdim:    [8]float64{1, 1, 1, 1, 1, 1, 1, 1},
		voxOffset: -1,
		xyztUnits: 2,
	}
}

func synthByteOrder(spec synthHeader) binary.ByteOrder {
	if spec.bigEndian {
		return binary.BigEndian
	}
	return binary.LittleEndian
}

func synthDatatypeWidth(datatype int16) int {
	switch datatype {
	case nifti.DatatypeUint8, nifti.DatatypeInt8:
		return 1
	case nifti.DatatypeInt16, nifti.DatatypeUint16:
		return 2
	case nifti.DatatypeInt32, nifti.DatatypeUint32, nifti.DatatypeFloat32:
		return 4
	case nifti.DatatypeFloat64:
		return 8
	case 32, 1024, 1280:
		return 8
	case 128:
		return 3
	default:
		return 1
	}
}

// synthHeaderBytes encodes the header only (348 or 540 bytes).
func synthHeaderBytes(spec synthHeader) []byte {
	order := synthByteOrder(spec)
	bitpix := spec.bitpix
	if bitpix == 0 {
		bitpix = int16(8 * synthDatatypeWidth(spec.datatype))
	}
	var dim [8]int64
	dim[0] = int64(len(spec.dims))
	for index := 1; index < 8; index++ {
		dim[index] = 1
	}
	copy(dim[1:], spec.dims)
	voxOffset := spec.voxOffset
	if voxOffset < 0 {
		switch {
		case spec.paired:
			voxOffset = 0
		case spec.version == 2:
			voxOffset = 544 + float64(len(spec.extension))
		default:
			voxOffset = 352 + float64(len(spec.extension))
		}
	}
	if spec.version == 2 {
		header := make([]byte, 540)
		order.PutUint32(header[0:], 540)
		magic := "n+2\x00\r\n\x1a\n"
		if spec.paired {
			magic = "ni2\x00\r\n\x1a\n"
		}
		if spec.magic != "" {
			magic = spec.magic
		}
		copy(header[4:12], magic)
		order.PutUint16(header[12:], uint16(spec.datatype))
		order.PutUint16(header[14:], uint16(bitpix))
		for index, value := range dim {
			order.PutUint64(header[16+8*index:], uint64(value))
		}
		putF64 := func(offset int, value float64) { order.PutUint64(header[offset:], math.Float64bits(value)) }
		for index, value := range spec.pixdim {
			putF64(104+8*index, value)
		}
		order.PutUint64(header[168:], uint64(int64(voxOffset)))
		putF64(176, spec.slope)
		putF64(184, spec.inter)
		order.PutUint32(header[344:], uint32(int32(spec.qformCode)))
		order.PutUint32(header[348:], uint32(int32(spec.sformCode)))
		for index, value := range spec.quatern {
			putF64(352+8*index, value)
		}
		for index, value := range spec.qoffset {
			putF64(376+8*index, value)
		}
		for row := range spec.srow {
			for column := range spec.srow[row] {
				putF64(400+32*row+8*column, spec.srow[row][column])
			}
		}
		order.PutUint32(header[500:], uint32(spec.xyztUnits))
		return header
	}
	header := make([]byte, 348)
	order.PutUint32(header[0:], 348)
	for index, value := range dim {
		order.PutUint16(header[40+2*index:], uint16(int16(value)))
	}
	putF32 := func(offset int, value float64) {
		order.PutUint32(header[offset:], math.Float32bits(float32(value)))
	}
	order.PutUint16(header[70:], uint16(spec.datatype))
	order.PutUint16(header[72:], uint16(bitpix))
	for index, value := range spec.pixdim {
		putF32(76+4*index, value)
	}
	putF32(108, voxOffset)
	putF32(112, spec.slope)
	putF32(116, spec.inter)
	header[123] = spec.xyztUnits
	order.PutUint16(header[252:], uint16(int16(spec.qformCode)))
	order.PutUint16(header[254:], uint16(int16(spec.sformCode)))
	for index, value := range spec.quatern {
		putF32(256+4*index, value)
	}
	for index, value := range spec.qoffset {
		putF32(268+4*index, value)
	}
	for row := range spec.srow {
		for column := range spec.srow[row] {
			putF32(280+16*row+4*column, spec.srow[row][column])
		}
	}
	magic := "n+1\x00"
	if spec.paired {
		magic = "ni1\x00"
	}
	if spec.magic != "" {
		magic = spec.magic
	}
	copy(header[344:348], magic)
	return header
}

// synthVoxels encodes values in the spec's datatype and byte order.
func synthVoxels(spec synthHeader, values []float64) []byte {
	order := synthByteOrder(spec)
	width := synthDatatypeWidth(spec.datatype)
	out := make([]byte, width*len(values))
	for index, value := range values {
		raw := out[index*width : (index+1)*width]
		switch spec.datatype {
		case nifti.DatatypeUint8:
			raw[0] = uint8(value)
		case nifti.DatatypeInt8:
			raw[0] = uint8(int8(value))
		case nifti.DatatypeInt16:
			order.PutUint16(raw, uint16(int16(value)))
		case nifti.DatatypeUint16:
			order.PutUint16(raw, uint16(value))
		case nifti.DatatypeInt32:
			order.PutUint32(raw, uint32(int32(value)))
		case nifti.DatatypeUint32:
			order.PutUint32(raw, uint32(value))
		case nifti.DatatypeFloat32:
			order.PutUint32(raw, math.Float32bits(float32(value)))
		case nifti.DatatypeFloat64:
			order.PutUint64(raw, math.Float64bits(value))
		}
	}
	return out
}

// synthFile builds a complete single-file .nii stream.
func synthFile(spec synthHeader, values []float64) []byte {
	var out bytes.Buffer
	out.Write(synthHeaderBytes(spec))
	extender := make([]byte, 4)
	if len(spec.extension) > 0 {
		extender[0] = 1
	}
	// 348+4 (NIfTI-1) or 540+4 (NIfTI-2) bytes reach the natural offset.
	out.Write(extender)
	out.Write(spec.extension)
	out.Write(synthVoxels(spec, values))
	return out.Bytes()
}

func gzipBytes(tb testing.TB, payload []byte) []byte {
	tb.Helper()
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	if _, err := writer.Write(payload); err != nil {
		tb.Fatalf("gzip write: %v", err)
	}
	if err := writer.Close(); err != nil {
		tb.Fatalf("gzip close: %v", err)
	}
	return out.Bytes()
}

// axisAngleQuaternion returns (b, c, d) and the rotation matrix of a rotation
// of angle radians about a unit axis; a = cos(angle/2) is non-negative for
// angles in [0, pi].
func axisAngleQuaternion(axis render.Vec3, angle float64) ([3]float64, [3][3]float64) {
	axis = axis.Normalize()
	s := math.Sin(angle / 2)
	a := math.Cos(angle / 2)
	b, c, d := s*axis.X, s*axis.Y, s*axis.Z
	r := [3][3]float64{
		{a*a + b*b - c*c - d*d, 2 * (b*c - a*d), 2 * (b*d + a*c)},
		{2 * (b*c + a*d), a*a + c*c - b*b - d*d, 2 * (c*d - a*b)},
		{2 * (b*d - a*c), 2 * (c*d + a*b), a*a + d*d - c*c - b*b},
	}
	return [3]float64{b, c, d}, r
}

// sformRows returns srow_x/y/z of R*diag(spacing) + offset.
func sformRows(r [3][3]float64, spacing [3]float64, offset [3]float64) [3][4]float64 {
	var rows [3][4]float64
	for row := 0; row < 3; row++ {
		for column := 0; column < 3; column++ {
			rows[row][column] = r[row][column] * spacing[column]
		}
		rows[row][3] = offset[row]
	}
	return rows
}

func affineNear(a, b render.GeometryAffine, tolerance float64) bool {
	for index := range a {
		if math.Abs(a[index]-b[index]) > tolerance {
			return false
		}
	}
	return true
}

func vecNear(a, b render.Vec3, tolerance float64) bool {
	return a.Sub(b).Length() <= tolerance
}

// directionColumns returns the unit column directions of an index affine.
func directionColumns(a render.GeometryAffine) [3]render.Vec3 {
	var out [3]render.Vec3
	for column := 0; column < 3; column++ {
		out[column] = render.Vec3{X: a[column], Y: a[4+column], Z: a[8+column]}.Normalize()
	}
	return out
}

// rotationAngleDegrees is the angle of R1*R2^T, the metric ITK's
// itkNiftiReadWriteDirectionTest uses (commit 71449a713c).
func rotationAngleDegrees(first, second [3]render.Vec3) float64 {
	trace := 0.0
	for index := 0; index < 3; index++ {
		trace += first[index].Dot(second[index])
	}
	cosine := math.Max(-1, math.Min(1, (trace-1)/2))
	return math.Acos(cosine) * 180 / math.Pi
}

func orthonormal(columns [3]render.Vec3, tolerance float64) bool {
	for first := 0; first < 3; first++ {
		if math.Abs(columns[first].Length()-1) > tolerance {
			return false
		}
		for second := first + 1; second < 3; second++ {
			if math.Abs(columns[first].Dot(columns[second])) > tolerance {
				return false
			}
		}
	}
	return true
}
