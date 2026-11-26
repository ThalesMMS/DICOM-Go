package imagefilter

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// ErrInvalidVolume reports a volume, spacing or option the dense volume
// filters cannot use.
var ErrInvalidVolume = errors.New("imagefilter: invalid volume")

// Volume is a dense scalar volume of float32 samples on a regular grid, the
// input and output of the recursive Gaussian filters.
//
// The layout is the one used across dicom-go for voxel data: Dims holds the
// columns (x), rows (y) and slices (z) — render.Volume's Cols, Rows and Depth
// and roi.VolumeMask's Columns, Rows and Slices — and Data is x-fastest, the
// sample (x, y, z) being Data[(z*Dims[1]+y)*Dims[0]+x], as in
// render.VolumeSnapshot's packed modality rows, render.SmoothGaussian3D and
// roi.VolumeMask.Voxels. SpacingMM is the distance in millimetres between
// neighbouring samples along x, y and z (render.Volume's ColSpacing,
// RowSpacing and SliceSpacing, or render.VolumeDescriptor.SpacingMM). Only
// the grid spacing matters to the filters: the origin and the axis
// directions stay with the caller, and irregular slice positions must be
// regularized (render.Volume.RegularGrid) before filtering.
//
// Samples are float32, like the modality values of a render.VolumeSnapshot,
// so a volume costs 4 bytes per voxel; the filters compute each line in
// float64 and round only when storing. A 2-D image is a Volume with
// Dims[2] == 1; its z spacing is then ignored and may be 0.
type Volume struct {
	Dims      [3]int
	SpacingMM [3]float64
	Data      []float32
}

// NewVolume returns a zero-filled volume. It panics if a dimension is not
// positive or the voxel count overflows int, like make.
func NewVolume(dims [3]int, spacingMM [3]float64) *Volume {
	n, ok := voxelCount(dims)
	if !ok {
		panic(fmt.Sprintf("imagefilter: invalid volume dimensions %v", dims))
	}
	return &Volume{Dims: dims, SpacingMM: spacingMM, Data: make([]float32, n)}
}

// Index returns the position of sample (x, y, z) in Data.
func (v *Volume) Index(x, y, z int) int { return (z*v.Dims[1]+y)*v.Dims[0] + x }

// At returns sample (x, y, z).
func (v *Volume) At(x, y, z int) float32 { return v.Data[v.Index(x, y, z)] }

// Clone returns a deep copy of v.
func (v *Volume) Clone() *Volume {
	out := *v
	out.Data = append([]float32(nil), v.Data...)
	return &out
}

// Validate reports whether v can be filtered: positive dimensions, Data
// holding exactly one sample per voxel, finite samples, and a positive
// finite spacing along every axis with more than one sample.
func (v *Volume) Validate() error {
	if v == nil {
		return fmt.Errorf("%w: nil volume", ErrInvalidVolume)
	}
	n, ok := voxelCount(v.Dims)
	if !ok || len(v.Data) != n {
		return fmt.Errorf("%w: %d samples for dimensions %v", ErrInvalidVolume, len(v.Data), v.Dims)
	}
	for axis, s := range v.SpacingMM {
		if v.Dims[axis] > 1 && !(s > 0 && s <= math.MaxFloat64) {
			return fmt.Errorf("%w: spacing %v mm along axis %d", ErrInvalidVolume, s, axis)
		}
	}
	for i, x := range v.Data {
		if x != x || math.IsInf(float64(x), 0) {
			return fmt.Errorf("%w: non-finite sample at index %d", ErrInvalidVolume, i)
		}
	}
	return nil
}

func voxelCount(dims [3]int) (int, bool) {
	n := 1
	for _, d := range dims {
		if d <= 0 || n > math.MaxInt/d {
			return 0, false
		}
		n *= d
	}
	return n, true
}

// ModalityF32Writer streams a volume as tightly packed little-endian float32
// samples, x-fastest. render.VolumeSnapshot satisfies it; the interface keeps
// this package free of a dependency on render, so render can use the filters.
type ModalityF32Writer interface {
	WriteModalityF32Context(ctx context.Context, dst io.Writer) error
}

// VolumeFromModalityF32 reads a volume of dims samples from src, for example
// a render.VolumeSnapshot with the Dimensions and SpacingMM of its
// Descriptor:
//
//	d, _ := snapshot.Descriptor()
//	vol, err := imagefilter.VolumeFromModalityF32(ctx, snapshot,
//		[3]int{int(d.Dimensions[0]), int(d.Dimensions[1]), int(d.Dimensions[2])}, d.SpacingMM)
func VolumeFromModalityF32(ctx context.Context, src ModalityF32Writer, dims [3]int, spacingMM [3]float64) (*Volume, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if src == nil {
		return nil, fmt.Errorf("%w: nil source", ErrInvalidVolume)
	}
	n, ok := voxelCount(dims)
	if !ok {
		return nil, fmt.Errorf("%w: dimensions %v", ErrInvalidVolume, dims)
	}
	w := &f32Decoder{dst: make([]float32, n)}
	if err := src.WriteModalityF32Context(ctx, w); err != nil {
		return nil, err
	}
	if w.n != n || w.pending != 0 {
		return nil, fmt.Errorf("%w: source wrote %d samples for dimensions %v", ErrInvalidVolume, w.n, dims)
	}
	return &Volume{Dims: dims, SpacingMM: spacingMM, Data: w.dst}, nil
}

// f32Decoder decodes little-endian float32 samples written in chunks of any
// size.
type f32Decoder struct {
	dst     []float32
	n       int
	partial [4]byte
	pending int
}

func (w *f32Decoder) Write(p []byte) (int, error) {
	written := len(p)
	for len(p) > 0 {
		if w.pending > 0 || len(p) < 4 {
			c := copy(w.partial[w.pending:], p)
			w.pending += c
			p = p[c:]
			if w.pending < 4 {
				break
			}
			if err := w.put(binary.LittleEndian.Uint32(w.partial[:])); err != nil {
				return 0, err
			}
			w.pending = 0
			continue
		}
		if err := w.put(binary.LittleEndian.Uint32(p)); err != nil {
			return 0, err
		}
		p = p[4:]
	}
	return written, nil
}

func (w *f32Decoder) put(bits uint32) error {
	if w.n >= len(w.dst) {
		return fmt.Errorf("%w: source wrote more than %d samples", ErrInvalidVolume, len(w.dst))
	}
	w.dst[w.n] = math.Float32frombits(bits)
	w.n++
	return nil
}

// VolumeFromMask converts an x-fastest binary mask (roi.VolumeMask.Voxels,
// with Columns, Rows and Slices as dims) into a volume of 1 inside and 0
// outside, for example to smooth a segmentation into a soft mask. The caller
// supplies the spacing; roi.VolumeMask keeps per-axis coordinates, whose
// steps must be uniform for the filters to be meaningful.
func VolumeFromMask(dims [3]int, spacingMM [3]float64, voxels []bool) (*Volume, error) {
	n, ok := voxelCount(dims)
	if !ok || len(voxels) != n {
		return nil, fmt.Errorf("%w: mask of %d voxels for dimensions %v", ErrInvalidVolume, len(voxels), dims)
	}
	v := &Volume{Dims: dims, SpacingMM: spacingMM, Data: make([]float32, n)}
	for i, in := range voxels {
		if in {
			v.Data[i] = 1
		}
	}
	return v, nil
}
