package nifti

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/ThalesMMS/dicom-go/render"
)

var (
	ErrNotNIfTI              = errors.New("dicom/nifti: not a NIfTI-1 or NIfTI-2 stream")
	ErrMalformedHeader       = errors.New("dicom/nifti: malformed NIfTI header")
	ErrUnsupportedDatatype   = errors.New("dicom/nifti: unsupported NIfTI datatype")
	ErrUnsupportedDimensions = errors.New("dicom/nifti: unsupported NIfTI dimensions")
	ErrNoSpatialTransform    = errors.New("dicom/nifti: no usable spatial transform")
	ErrInvalidTransform      = errors.New("dicom/nifti: invalid spatial transform")
	ErrNonOrthogonalSForm    = errors.New("dicom/nifti: non-orthogonal sform")
	ErrTruncatedData         = errors.New("dicom/nifti: truncated NIfTI data")
	ErrPairedFile            = errors.New("dicom/nifti: header and image are separate files")
)

// ReadErrorCode is a stable, PHI-free classification of a read failure.
type ReadErrorCode string

const (
	ReadCodeFormat     ReadErrorCode = "format"
	ReadCodeHeader     ReadErrorCode = "header"
	ReadCodeDatatype   ReadErrorCode = "datatype"
	ReadCodeDimensions ReadErrorCode = "dimensions"
	ReadCodeGeometry   ReadErrorCode = "geometry"
	ReadCodeLimit      ReadErrorCode = "limit"
	ReadCodeTruncated  ReadErrorCode = "truncated"
	ReadCodeIO         ReadErrorCode = "io"
	ReadCodeCanceled   ReadErrorCode = "canceled"
)

// ReadError reports a NIfTI read failure. Error() names only the code, a
// stable header field name and, for datatype failures, the datatype number;
// it never echoes paths or header text. The sentinel and any I/O cause remain
// reachable through errors.Is/errors.As.
type ReadError struct {
	Code     ReadErrorCode
	Field    string
	Datatype int16
	Err      error
}

func (e *ReadError) Error() string {
	if e == nil {
		return "dicom/nifti: read failed"
	}
	message := "dicom/nifti: read " + string(e.Code)
	if e.Field != "" {
		message += " (" + e.Field + ")"
	}
	if e.Code == ReadCodeDatatype {
		message += fmt.Sprintf(" datatype %d", e.Datatype)
	}
	return message
}

func (e *ReadError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func readError(code ReadErrorCode, field string, sentinel, cause error) error {
	err := sentinel
	if cause != nil {
		err = fmt.Errorf("%w: %w", sentinel, cause)
	}
	return &ReadError{Code: code, Field: field, Err: err}
}

// ReadLimits bounds the resources a reader commits to an untrusted file.
type ReadLimits struct {
	// MaxVoxels bounds nx*ny*nz*nt; the default equals the exporter's.
	MaxVoxels uint64
	// MaxPayloadBytes bounds the decoded voxel payload held in memory; the
	// default equals the exporter's MaxUncompressedBytes. Memory is committed
	// in chunks as bytes arrive, so a header that lies about its size cannot
	// force a large allocation.
	MaxPayloadBytes uint64
	// MaxHeaderBytes bounds vox_offset: the header plus skipped extensions.
	MaxHeaderBytes uint64
}

// DefaultReadLimits mirrors DefaultLimits for voxel count and payload size and
// allows 64 MiB of header extensions.
func DefaultReadLimits() ReadLimits {
	limits := DefaultLimits()
	return ReadLimits{
		MaxVoxels:       limits.MaxVoxels,
		MaxPayloadBytes: limits.MaxUncompressedBytes,
		MaxHeaderBytes:  64 << 20,
	}
}

// ReadOptions configures Read, ReadPair and ReadFile. The zero value applies
// the automatic qform/sform precedence and the default limits.
type ReadOptions struct {
	Limits ReadLimits
	Xform  XformPolicy
	// SFormPermissive accepts a non-orthogonal sform when no valid qform is
	// preferred, replacing its directions by the nearest orthonormal frame
	// (ITK's SFORM_PERMISSIVE). Geometry.SFormCorrected reports it.
	SFormPermissive bool
	// PermissiveMaxDeviationDegrees bounds that correction; zero selects
	// DefaultPermissiveMaxDeviationDegrees.
	PermissiveMaxDeviationDegrees float64
	// AllowMissingTransform reads a file whose qform_code and sform_code are
	// both zero with the NIfTI method-1 grid diag(pixdim) at the origin.
	// Such a grid has no patient position and must not be matched to DICOM.
	AllowMissingTransform bool
}

func normalizedReadOptions(options ReadOptions) (ReadOptions, error) {
	defaults := DefaultReadLimits()
	if options.Limits.MaxVoxels == 0 {
		options.Limits.MaxVoxels = defaults.MaxVoxels
	}
	if options.Limits.MaxPayloadBytes == 0 {
		options.Limits.MaxPayloadBytes = defaults.MaxPayloadBytes
	}
	if options.Limits.MaxHeaderBytes == 0 {
		options.Limits.MaxHeaderBytes = defaults.MaxHeaderBytes
	}
	if options.Xform > XformSForm {
		return ReadOptions{}, readError(ReadCodeGeometry, "xform", ErrInvalidTransform, nil)
	}
	if deviation := options.PermissiveMaxDeviationDegrees; deviation < 0 || math.IsNaN(deviation) || deviation > 90 {
		return ReadOptions{}, readError(ReadCodeGeometry, "sform", ErrInvalidTransform, nil)
	}
	return options, nil
}

// Scaling is the scl_slope/scl_inter transform of a volume. NIfTI applies it
// only when scl_slope is finite and non-zero; a non-finite intercept is read
// as zero, as nifti_clib does.
type Scaling struct {
	Slope     float64
	Intercept float64
	Applied   bool
}

func headerScaling(header Header) Scaling {
	if !finite(header.SclSlope) || header.SclSlope == 0 {
		return Scaling{Slope: 1}
	}
	intercept := header.SclInter
	if !finite(intercept) {
		intercept = 0
	}
	return Scaling{
		Slope:     header.SclSlope,
		Intercept: intercept,
		Applied:   header.SclSlope != 1 || intercept != 0,
	}
}

// Volume is a decoded NIfTI scalar volume. Voxels are stored in file order
// (i fastest, then j, k, t) and normalized to little-endian.
type Volume struct {
	Header     Header
	Geometry   Geometry
	Dimensions [4]int // nx, ny, nz, nt
	Scaling    Scaling
	// Warnings holds stable, PHI-free codes, including Geometry.Warnings.
	Warnings []string

	bytesPerVoxel int
	expected      int
	// chunks hold the payload in payloadChunkBytes pieces: memory tracks the
	// bytes that actually arrived and a large volume never needs a second,
	// contiguous copy. The chunk size is a multiple of every voxel width.
	chunks [][]byte
}

const payloadChunkBytes = 8 << 20

// Len is the number of voxels (nx*ny*nz*nt).
func (v *Volume) Len() int { return v.expected / v.bytesPerVoxel }

// Index returns the linear voxel index of (i, j, k, t).
func (v *Volume) Index(i, j, k, t int) int {
	d := v.Dimensions
	return i + d[0]*(j+d[1]*(k+d[2]*t))
}

// Stored returns the unscaled on-disk value of voxel index, in [0, Len()), as
// float64. Float NaN and infinities are preserved. An index out of range
// panics like a slice access.
func (v *Volume) Stored(index int) float64 {
	offset := index * v.bytesPerVoxel
	chunk := v.chunks[offset/payloadChunkBytes]
	offset %= payloadChunkBytes
	raw := chunk[offset : offset+v.bytesPerVoxel]
	switch v.Header.Datatype {
	case DatatypeUint8:
		return float64(raw[0])
	case DatatypeInt8:
		return float64(int8(raw[0]))
	case DatatypeInt16:
		return float64(int16(binary.LittleEndian.Uint16(raw)))
	case DatatypeUint16:
		return float64(binary.LittleEndian.Uint16(raw))
	case DatatypeInt32:
		return float64(int32(binary.LittleEndian.Uint32(raw)))
	case DatatypeUint32:
		return float64(binary.LittleEndian.Uint32(raw))
	case DatatypeFloat32:
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(raw)))
	default:
		return math.Float64frombits(binary.LittleEndian.Uint64(raw))
	}
}

// Value returns voxel index with scl_slope/scl_inter applied. NaN stays NaN.
func (v *Volume) Value(index int) float64 {
	stored := v.Stored(index)
	if !v.Scaling.Applied {
		return stored
	}
	return stored*v.Scaling.Slope + v.Scaling.Intercept
}

// SliceValues appends the scaled values of slice k at time point t, row by
// row, to dst and returns it.
func (v *Volume) SliceValues(k, t int, dst []float64) []float64 {
	start := v.Index(0, 0, k, t)
	count := v.Dimensions[0] * v.Dimensions[1]
	for index := start; index < start+count; index++ {
		dst = append(dst, v.Value(index))
	}
	return dst
}

// SliceGeometries returns the DICOM LPS geometry of every slice k in file
// order.
func (v *Volume) SliceGeometries() []render.SliceGeometry {
	out := make([]render.SliceGeometry, v.Dimensions[2])
	for k := range out {
		out[k] = v.Geometry.SliceGeometry(k, v.Dimensions[0], v.Dimensions[1])
	}
	return out
}

// VolumeGeometry classifies the slices with the render guardrails used by the
// rest of the library. A left-handed file yields ReversedInput.
func (v *Volume) VolumeGeometry(tolerances render.GeometryTolerances) render.VolumeGeometry {
	return render.BuildVolumeGeometry(v.SliceGeometries(), tolerances)
}

// Read decodes a single-file NIfTI-1 or NIfTI-2 volume (.nii) from r,
// transparently decompressing gzip (.nii.gz). A header whose magic declares a
// .hdr/.img pair returns ErrPairedFile; use ReadPair or ReadFile.
func Read(ctx context.Context, r io.Reader, options ReadOptions) (*Volume, error) {
	options, err := normalizedReadOptions(options)
	if err != nil {
		return nil, err
	}
	stream, closeStream, err := openMaybeGZIP(r)
	if err != nil {
		return nil, err
	}
	defer closeStream()
	header, raw, err := readHeaderBytes(stream)
	if err != nil {
		return nil, err
	}
	if header.Paired {
		return nil, readError(ReadCodeFormat, "magic", ErrPairedFile, nil)
	}
	volume, dataOffset, err := prepareVolume(header, options, false)
	if err != nil {
		return nil, err
	}
	consumed := int64(len(raw))
	if dataOffset >= consumed+4 {
		var extender [4]byte
		if _, err := io.ReadFull(stream, extender[:]); err != nil {
			return nil, ioReadError(ctx, "extension", err)
		}
		volume.Header.Extended = extender[0] != 0
		consumed += 4
	}
	if err := discard(ctx, stream, dataOffset-consumed); err != nil {
		return nil, err
	}
	if err := volume.readPayload(ctx, stream); err != nil {
		return nil, err
	}
	return volume, nil
}

// ReadPair decodes a NIfTI .hdr/.img pair (either may be gzip-compressed).
// vox_offset is the voxel offset inside the image stream.
func ReadPair(ctx context.Context, headerReader, imageReader io.Reader, options ReadOptions) (*Volume, error) {
	options, err := normalizedReadOptions(options)
	if err != nil {
		return nil, err
	}
	headerStream, closeHeader, err := openMaybeGZIP(headerReader)
	if err != nil {
		return nil, err
	}
	defer closeHeader()
	header, _, err := readHeaderBytes(headerStream)
	if err != nil {
		return nil, err
	}
	if !header.Paired {
		return nil, readError(ReadCodeFormat, "magic", ErrNotNIfTI, nil)
	}
	volume, dataOffset, err := prepareVolume(header, options, true)
	if err != nil {
		return nil, err
	}
	imageStream, closeImage, err := openMaybeGZIP(imageReader)
	if err != nil {
		return nil, err
	}
	defer closeImage()
	if err := discard(ctx, imageStream, dataOffset); err != nil {
		return nil, err
	}
	if err := volume.readPayload(ctx, imageStream); err != nil {
		return nil, err
	}
	return volume, nil
}

// ReadFile reads path by extension: .nii and .nii.gz as single files; .hdr,
// .hdr.gz, .img or .img.gz as a pair whose companion shares the base name
// (either compression). Errors never include the path.
func ReadFile(ctx context.Context, path string, options ReadOptions) (*Volume, error) {
	lower := strings.ToLower(path)
	base, pair := "", false
	for _, suffix := range []string{".hdr.gz", ".img.gz", ".hdr", ".img"} {
		if strings.HasSuffix(lower, suffix) {
			base, pair = path[:len(path)-len(suffix)], true
			break
		}
	}
	if !pair {
		file, err := os.Open(path)
		if err != nil {
			return nil, readError(ReadCodeIO, "open", ErrInvalidSource, err)
		}
		defer file.Close()
		return Read(ctx, file, options)
	}
	headerFile, err := openCompanion(base, ".hdr")
	if err != nil {
		return nil, err
	}
	defer headerFile.Close()
	imageFile, err := openCompanion(base, ".img")
	if err != nil {
		return nil, err
	}
	defer imageFile.Close()
	return ReadPair(ctx, headerFile, imageFile, options)
}

func openCompanion(base, extension string) (*os.File, error) {
	dir, name := filepath.Split(base)
	for _, candidate := range []string{extension, extension + ".gz", strings.ToUpper(extension), strings.ToUpper(extension) + ".GZ"} {
		file, err := os.Open(filepath.Join(dir, name+candidate))
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, readError(ReadCodeIO, "open", ErrInvalidSource, err)
		}
	}
	return nil, readError(ReadCodeIO, "open", ErrInvalidSource, os.ErrNotExist)
}

func openMaybeGZIP(r io.Reader) (io.Reader, func(), error) {
	if r == nil {
		return nil, func() {}, readError(ReadCodeIO, "reader", ErrInvalidSource, nil)
	}
	buffered := bufio.NewReaderSize(r, 64<<10)
	signature, err := buffered.Peek(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, func() {}, readError(ReadCodeIO, "reader", ErrInvalidSource, err)
	}
	if len(signature) == 2 && signature[0] == 0x1f && signature[1] == 0x8b {
		decompressor, err := gzip.NewReader(buffered)
		if err != nil {
			return nil, func() {}, readError(ReadCodeFormat, "gzip", ErrNotNIfTI, err)
		}
		return decompressor, func() { _ = decompressor.Close() }, nil
	}
	return buffered, func() {}, nil
}

func readHeaderBytes(stream io.Reader) (Header, []byte, error) {
	raw := make([]byte, nifti2HeaderSize)
	if _, err := io.ReadFull(stream, raw[:nifti1HeaderSize]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return Header{}, nil, readError(ReadCodeTruncated, "header", ErrTruncatedData, nil)
		}
		return Header{}, nil, readError(ReadCodeIO, "header", ErrInvalidSource, err)
	}
	raw = raw[:nifti1HeaderSize]
	sizeLE := binary.LittleEndian.Uint32(raw)
	sizeBE := binary.BigEndian.Uint32(raw)
	if sizeLE == nifti2HeaderSize || sizeBE == nifti2HeaderSize {
		raw = raw[:nifti2HeaderSize]
		if _, err := io.ReadFull(stream, raw[nifti1HeaderSize:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return Header{}, nil, readError(ReadCodeTruncated, "header", ErrTruncatedData, nil)
			}
			return Header{}, nil, readError(ReadCodeIO, "header", ErrInvalidSource, err)
		}
	}
	header, err := ParseHeader(raw)
	if err != nil {
		return Header{}, nil, err
	}
	return header, raw, nil
}

// prepareVolume validates everything the header alone decides (datatype,
// dimensions, limits, voxel offset and geometry) before any voxel byte is
// read, and returns the data offset within the voxel stream.
func prepareVolume(header Header, options ReadOptions, paired bool) (*Volume, int64, error) {
	volume := &Volume{Header: header, Scaling: headerScaling(header)}
	bytesPerVoxel, ok := datatypeBytes(header.Datatype)
	if !ok {
		return nil, 0, &ReadError{Code: ReadCodeDatatype, Field: "datatype", Datatype: header.Datatype, Err: ErrUnsupportedDatatype}
	}
	volume.bytesPerVoxel = bytesPerVoxel
	if header.BitPix != int16(8*bytesPerVoxel) {
		volume.Warnings = append(volume.Warnings, WarningBitPixMismatch)
	}
	for index := 5; index < len(header.Dim); index++ {
		if header.Dim[index] != 1 {
			return nil, 0, readError(ReadCodeDimensions, "dim", ErrUnsupportedDimensions, nil)
		}
	}
	voxels := uint64(1)
	for index := 0; index < 4; index++ {
		dimension := header.Dim[index+1]
		volume.Dimensions[index] = int(dimension)
		var ok bool
		voxels, ok = checkedMul(voxels, uint64(dimension))
		if !ok || voxels > options.Limits.MaxVoxels {
			return nil, 0, readError(ReadCodeLimit, "dim", ErrLimitExceeded, nil)
		}
	}
	payload, fits := checkedMul(voxels, uint64(bytesPerVoxel))
	if !fits || payload > options.Limits.MaxPayloadBytes || payload > uint64(math.MaxInt) {
		return nil, 0, readError(ReadCodeLimit, "payload", ErrLimitExceeded, nil)
	}

	offset := header.VoxelOffset
	if !finite(offset) || offset < 0 || offset != math.Trunc(offset) || offset > float64(options.Limits.MaxHeaderBytes) {
		if finite(offset) && offset > float64(options.Limits.MaxHeaderBytes) {
			return nil, 0, readError(ReadCodeLimit, "vox_offset", ErrLimitExceeded, nil)
		}
		return nil, 0, readError(ReadCodeHeader, "vox_offset", ErrMalformedHeader, nil)
	}
	dataOffset := int64(offset)
	if !paired && dataOffset < int64(header.Size()) {
		// Legacy writers left vox_offset at zero; the voxels of a single
		// file cannot start inside the header itself.
		dataOffset = int64(header.Size()) + 4
		volume.Warnings = append(volume.Warnings, WarningVoxelOffsetAdjusted)
	}

	geometry, err := resolveGeometry(header, options)
	if err != nil {
		return nil, 0, err
	}
	volume.Geometry = geometry
	volume.Warnings = append(volume.Warnings, geometry.Warnings...)
	volume.expected = int(payload)
	return volume, dataOffset, nil
}

func discard(ctx context.Context, stream io.Reader, count int64) error {
	for count > 0 {
		if err := ctx.Err(); err != nil {
			return readError(ReadCodeCanceled, "extension", err, nil)
		}
		chunk := min(count, 1<<20)
		copied, err := io.CopyN(io.Discard, stream, chunk)
		count -= copied
		if err != nil {
			return ioReadError(ctx, "vox_offset", err)
		}
	}
	return nil
}

func (v *Volume) readPayload(ctx context.Context, stream io.Reader) error {
	for remaining := v.expected; remaining > 0; {
		if err := ctx.Err(); err != nil {
			v.chunks = nil
			return readError(ReadCodeCanceled, "data", err, nil)
		}
		chunk := make([]byte, min(remaining, payloadChunkBytes))
		if _, err := io.ReadFull(stream, chunk); err != nil {
			v.chunks = nil
			return ioReadError(ctx, "data", err)
		}
		if v.Header.BigEndian {
			swapToLittleEndian(chunk, v.bytesPerVoxel)
		}
		v.chunks = append(v.chunks, chunk)
		remaining -= len(chunk)
	}
	var probe [1]byte
	count, err := io.ReadFull(stream, probe[:])
	switch {
	case count > 0:
		v.Warnings = append(v.Warnings, WarningTrailingData)
	case err != nil && !errors.Is(err, io.EOF):
		// A gzip checksum or framing error surfaces only at end of stream.
		v.chunks = nil
		return ioReadError(ctx, "data", err)
	}
	return nil
}

func ioReadError(ctx context.Context, field string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return readError(ReadCodeCanceled, field, ctxErr, nil)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return readError(ReadCodeTruncated, field, ErrTruncatedData, nil)
	}
	return readError(ReadCodeIO, field, ErrInvalidSource, err)
}

func swapToLittleEndian(data []byte, width int) {
	if width == 1 {
		return
	}
	for offset := 0; offset+width <= len(data); offset += width {
		for low, high := offset, offset+width-1; low < high; low, high = low+1, high-1 {
			data[low], data[high] = data[high], data[low]
		}
	}
}
