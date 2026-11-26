package nifti

import (
	"context"
	"io"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// NewVolumeOptions describes a volume decoded by another format reader
// (NRRD, MetaImage) so it can use Volume's accessors, geometry and SEG
// conversion.
type NewVolumeOptions struct {
	// Datatype is one of the NIfTI datatypes Read supports (DatatypeUint8
	// … DatatypeFloat64).
	Datatype int16
	// Dimensions are nx, ny, nz, nt (zero entries count as 1).
	Dimensions [4]int
	// IndexToLPS maps a zero-based (i, j, k) voxel index to DICOM patient
	// (LPS) millimetres. Its direction columns must be orthogonal.
	IndexToLPS render.GeometryAffine
	// BigEndian reports the byte order of the voxel stream.
	BigEndian bool
	Limits    ReadLimits
}

// NewVolume reads a volume's voxels from r (nx·ny·nz·nt voxels, i fastest)
// and describes it like a NIfTI file with a scanner-anatomical sform, so the
// resulting Volume behaves exactly as if Read had decoded it. A voxel stream
// with trailing bytes gets WarningTrailingData.
func NewVolume(ctx context.Context, r io.Reader, options NewVolumeOptions) (*Volume, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	limits := options.Limits
	defaults := DefaultReadLimits()
	if limits.MaxVoxels == 0 {
		limits.MaxVoxels = defaults.MaxVoxels
	}
	if limits.MaxPayloadBytes == 0 {
		limits.MaxPayloadBytes = defaults.MaxPayloadBytes
	}
	if limits.MaxHeaderBytes == 0 {
		limits.MaxHeaderBytes = defaults.MaxHeaderBytes
	}
	bytesPerVoxel, ok := datatypeBytes(options.Datatype)
	if !ok {
		return nil, &ReadError{Code: ReadCodeDatatype, Field: "datatype", Datatype: options.Datatype, Err: ErrUnsupportedDatatype}
	}
	header := Header{
		Version:     1,
		BigEndian:   options.BigEndian,
		Datatype:    options.Datatype,
		BitPix:      int16(8 * bytesPerVoxel),
		SclSlope:    1,
		SFormCode:   XformScannerAnat,
		XYZTUnits:   uint8(SpatialUnitMillimeter),
		VoxelOffset: float64(nifti1HeaderSize + 4),
	}
	dimensions := 0
	for index, size := range options.Dimensions {
		if size == 0 {
			size = 1
		}
		if size < 0 || size > math.MaxInt32 {
			return nil, readError(ReadCodeDimensions, "dim", ErrUnsupportedDimensions, nil)
		}
		header.Dim[index+1] = int64(size)
		if size > 1 || index < 3 {
			dimensions = index + 1
		}
	}
	for index := 5; index < len(header.Dim); index++ {
		header.Dim[index] = 1
	}
	header.Dim[0] = int64(dimensions)
	ras := affineLPS2RAS(options.IndexToLPS)
	header.PixDim[0] = 1
	for column := 0; column < 3; column++ {
		header.PixDim[column+1] = math.Sqrt(ras[column]*ras[column] + ras[4+column]*ras[4+column] + ras[8+column]*ras[8+column])
	}
	for row := 0; row < 3; row++ {
		for column := 0; column < 4; column++ {
			header.SRow[row][column] = ras[4*row+column]
		}
	}
	volume, _, err := prepareVolume(header, ReadOptions{Limits: limits}, true)
	if err != nil {
		return nil, err
	}
	if err := volume.readPayload(ctx, r); err != nil {
		return nil, err
	}
	return volume, nil
}

// WritePayload writes the voxels (i fastest, then j, k, t) in little-endian
// byte order, as stored.
func (v *Volume) WritePayload(w io.Writer) error {
	if v == nil {
		return ErrInvalidSource
	}
	for _, chunk := range v.chunks {
		if err := writeExact(w, chunk); err != nil {
			return err
		}
	}
	return nil
}

// BytesPerVoxel is the stored size of one voxel.
func (v *Volume) BytesPerVoxel() int {
	if v == nil {
		return 0
	}
	return v.bytesPerVoxel
}
