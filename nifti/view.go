package nifti

import (
	"context"
	"encoding/binary"
	"errors"
	"image"
	"math"

	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/render"
)

var ErrGPUScalarProfile = errors.New("dicom/nifti: GPU float32 display requires finite representable samples")

// SliceImage presents a native file slice only; it does not reslice a volume.
// Scaled values remain untouched. NaN and infinities are black in the display
// raster and remain available through Stored/Value for quantitative inspection.
func (v *Volume) SliceImage(ctx context.Context, k, t int, window render.WindowLevel) (*image.Gray, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if v == nil || k < 0 || k >= v.Dimensions[2] || t < 0 || t >= v.Dimensions[3] {
		return nil, ErrUnsupportedDimensions
	}
	if !finite(window.Center) || !finite(window.Width) || window.Width <= 0 || window.LUT != nil {
		return nil, ErrUnsupportedScaling
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	nx, ny := v.Dimensions[0], v.Dimensions[1]
	if nx <= 0 || ny <= 0 || v.bytesPerVoxel <= 0 {
		return nil, ErrUnsupportedDimensions
	}
	n, ok := checkedMul(uint64(nx), uint64(ny))
	if !ok || n > uint64(math.MaxInt) {
		return nil, ErrLimitExceeded
	}
	planes, ok := checkedMul(uint64(t), uint64(v.Dimensions[2]))
	if !ok || planes > math.MaxUint64-uint64(k) {
		return nil, ErrUnsupportedDimensions
	}
	startVoxel, ok := checkedMul(planes+uint64(k), n)
	if !ok || startVoxel > uint64(v.expected/v.bytesPerVoxel) || n > uint64(v.expected/v.bytesPerVoxel)-startVoxel {
		return nil, ErrUnsupportedDimensions
	}
	start := int(startVoxel)
	img := image.NewGray(image.Rect(0, 0, nx, ny))
	mapper := display.VOILUT{Center: window.Center, Width: window.Width, Function: window.Function}
	for y := 0; y < ny; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for x := 0; x < nx; x++ {
			value := v.Value(start + y*nx + x)
			if finite(value) {
				img.Pix[y*img.Stride+x] = mapper.WindowByte(value)
			}
		}
	}
	return img, nil
}

// GPUInput prepares one scalar timepoint in the existing GPU float32 modality
// transport. This is a display representation, never a replacement for the
// original source samples. Finite float64 values round to float32; nonfinite or
// overflowing values refuse the entire representation without filling holes.
// maxBytes bounds the returned allocation, independent of Read's payload cap.
func (v *Volume) GPUInput(ctx context.Context, t int, generation, maxBytes uint64) (render.VolumeInput, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if v == nil || t < 0 || t >= v.Dimensions[3] {
		return render.VolumeInput{}, ErrUnsupportedDimensions
	}
	if err := ctx.Err(); err != nil {
		return render.VolumeInput{}, err
	}
	nx, ny, nz := v.Dimensions[0], v.Dimensions[1], v.Dimensions[2]
	if nx <= 0 || ny <= 0 || nz <= 0 || uint64(nx) > math.MaxUint32 || uint64(ny) > math.MaxUint32 || uint64(nz) > math.MaxUint32 || v.bytesPerVoxel <= 0 {
		return render.VolumeInput{}, ErrUnsupportedDimensions
	}
	n, ok := checkedMul(uint64(nx), uint64(ny))
	if !ok {
		return render.VolumeInput{}, ErrLimitExceeded
	}
	n, ok = checkedMul(n, uint64(nz))
	if !ok || n > uint64(v.expected/v.bytesPerVoxel) || uint64(t) >= uint64(v.expected/v.bytesPerVoxel)/n {
		return render.VolumeInput{}, ErrUnsupportedDimensions
	}
	if n > uint64(math.MaxInt)/4 || maxBytes == 0 || n > maxBytes/4 {
		return render.VolumeInput{}, ErrLimitExceeded
	}
	inverse, ok := v.Geometry.IndexToLPS.Inverse()
	if !ok {
		return render.VolumeInput{}, ErrInvalidTransform
	}
	d := render.VolumeDescriptor{ContractVersion: render.VolumeSnapshotContractVersion, HeaderSize: render.VolumeSnapshotHeaderSizeV1,
		VolumeGeneration: generation, Derivation: render.VolumeDerivationNormalized,
		Dimensions: [3]uint32{uint32(nx), uint32(ny), uint32(nz)}, Components: 1,
		ScalarFormat: render.VolumeScalarF32ModalityLE, SampleDomain: render.VolumeSampleDomainModality,
		RowStrideBytes: uint64(nx) * 4, SliceStrideBytes: uint64(nx) * uint64(ny) * 4, ByteLength: n * 4,
		RescaleSlope: 1, SpacingMM: v.Geometry.Spacing, IndexToPatientLPS: v.Geometry.IndexToLPS, PatientLPSToIndex: inverse}
	if err := render.ValidateVolumeDescriptor(d); err != nil {
		return render.VolumeInput{}, err
	}
	payload := make([]byte, int(n)*4)
	start := v.Index(0, 0, 0, t)
	for i := 0; i < int(n); i++ {
		if i&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return render.VolumeInput{}, err
			}
		}
		value := v.Value(start + i)
		f := float32(value)
		if !finite(value) || math.IsInf(float64(f), 0) {
			return render.VolumeInput{}, ErrGPUScalarProfile
		}
		binary.LittleEndian.PutUint32(payload[i*4:], math.Float32bits(f))
	}
	return render.VolumeInput{Descriptor: d, Payload: payload}, nil
}
