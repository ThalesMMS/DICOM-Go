package registration

import (
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// ErrImage reports an image whose size, samples or geometry cannot be
// used.
var ErrImage = errors.New("registration: invalid image")

// Image is a scalar image for intensity-based registration: float32
// samples with x varying fastest, then y, then z, placed in patient (LPS)
// space by origin, spacing and direction cosines, as a VolumeSnapshot or an
// ITK image is. A 2D image has size 1 along z.
type Image struct {
	size      [3]int
	data      []float32
	origin    render.Vec3
	spacing   render.Vec3
	direction Matrix3
	// indexToPhysical is direction·diag(spacing); physicalToIndex its
	// inverse.
	indexToPhysical, physicalToIndex Matrix3
}

// NewImage wraps data, which it does not copy; the direction columns are
// the unit directions of the x, y and z index axes.
func NewImage(size [3]int, data []float32, origin, spacing render.Vec3, direction Matrix3) (*Image, error) {
	im, err := newImageGeometry(size, origin, spacing, direction)
	if err != nil {
		return nil, err
	}
	if count := size[0] * size[1] * size[2]; count != len(data) || count/size[0]/size[1] != size[2] {
		return nil, fmt.Errorf("%w: %d samples for size %v", ErrImage, len(data), size)
	}
	im.data = data
	return &im, nil
}

// newImageGeometry builds coordinate transforms without allocating samples.
func newImageGeometry(size [3]int, origin, spacing render.Vec3, direction Matrix3) (Image, error) {
	for _, n := range size {
		if n < 1 {
			return Image{}, fmt.Errorf("%w: size %v", ErrImage, size)
		}
	}
	for _, s := range []float64{spacing.X, spacing.Y, spacing.Z} {
		if !(s > 0) || math.IsInf(s, 0) {
			return Image{}, fmt.Errorf("%w: spacing %v", ErrImage, spacing)
		}
	}
	scale := Matrix3{{spacing.X, 0, 0}, {0, spacing.Y, 0}, {0, 0, spacing.Z}}
	indexToPhysical := direction.Mul(scale)
	physicalToIndex, err := indexToPhysical.Inverse()
	if err != nil || !finiteVec(origin) {
		return Image{}, fmt.Errorf("%w: singular direction or non-finite origin", ErrImage)
	}
	return Image{
		size: size, origin: origin, spacing: spacing, direction: direction,
		indexToPhysical: indexToPhysical, physicalToIndex: physicalToIndex,
	}, nil
}

// ImageFromSnapshot copies a single-component volume in modality values
// (rescaled Hounsfield units for CT).
func ImageFromSnapshot(snapshot render.VolumeSnapshot) (*Image, error) {
	descriptor, err := snapshot.Descriptor()
	if err != nil {
		return nil, err
	}
	if descriptor.Components != 1 {
		return nil, fmt.Errorf("%w: %d components", ErrImage, descriptor.Components)
	}
	size := [3]int{int(descriptor.Dimensions[0]), int(descriptor.Dimensions[1]), int(descriptor.Dimensions[2])}
	sliceLength := size[0] * size[1]
	data := make([]float32, sliceLength*size[2])
	for z := 0; z < size[2]; z++ {
		if err := snapshot.ReadModalitySliceF32(uint32(z), data[z*sliceLength:(z+1)*sliceLength]); err != nil {
			return nil, err
		}
	}
	affine := descriptor.IndexToPatientLPS
	spacing := render.Vec3{X: descriptor.SpacingMM[0], Y: descriptor.SpacingMM[1], Z: descriptor.SpacingMM[2]}
	var direction Matrix3
	for row := 0; row < 3; row++ {
		direction[row] = [3]float64{affine[row*4] / spacing.X, affine[row*4+1] / spacing.Y, affine[row*4+2] / spacing.Z}
	}
	return NewImage(size, data, render.Vec3{X: affine[3], Y: affine[7], Z: affine[11]}, spacing, direction)
}

// Downsample averages blocks of factors samples (a partial block at the end
// of an axis averages what it holds) into a coarser image covering the same
// physical extent: spacing × factor, with each sample at the centre of its
// block. Registration of large volumes runs on such an image.
func (im *Image) Downsample(factors [3]int) (*Image, error) {
	var size [3]int
	for axis, f := range factors {
		if f < 1 {
			return nil, fmt.Errorf("%w: downsampling factors %v", ErrImage, factors)
		}
		size[axis] = 1 + (im.size[axis]-1)/f
	}
	if factors == [3]int{1, 1, 1} {
		return im, nil
	}
	data := make([]float32, size[0]*size[1]*size[2])
	for z := 0; z < size[2]; z++ {
		for y := 0; y < size[1]; y++ {
			for x := 0; x < size[0]; x++ {
				var sum float64
				var count int
				for k := z * factors[2]; k < min((z+1)*factors[2], im.size[2]); k++ {
					for j := y * factors[1]; j < min((y+1)*factors[1], im.size[1]); j++ {
						row := (k*im.size[1] + j) * im.size[0]
						for i := x * factors[0]; i < min((x+1)*factors[0], im.size[0]); i++ {
							sum += float64(im.data[row+i])
							count++
						}
					}
				}
				data[(z*size[1]+y)*size[0]+x] = float32(sum / float64(count))
			}
		}
	}
	// Sample (0, 0, 0) is the centre of the first block, (f−1)/2 samples in.
	origin := im.Point(render.Vec3{X: float64(factors[0]-1) / 2, Y: float64(factors[1]-1) / 2, Z: float64(factors[2]-1) / 2})
	spacing := render.Vec3{X: im.spacing.X * float64(factors[0]), Y: im.spacing.Y * float64(factors[1]), Z: im.spacing.Z * float64(factors[2])}
	return NewImage(size, data, origin, spacing, im.direction)
}

// Size is the number of samples along x, y and z.
func (im *Image) Size() [3]int { return im.size }

// Data is the samples, x fastest; the image does not copy them.
func (im *Image) Data() []float32 { return im.data }

// Spacing is the sample spacing in millimetres along x, y and z.
func (im *Image) Spacing() render.Vec3 { return im.spacing }

// Origin is the physical position of sample (0, 0, 0).
func (im *Image) Origin() render.Vec3 { return im.origin }

// Direction holds the unit directions of the index axes as columns.
func (im *Image) Direction() Matrix3 { return im.direction }

// At is sample (x, y, z), which must be inside the image.
func (im *Image) At(x, y, z int) float32 { return im.data[(z*im.size[1]+y)*im.size[0]+x] }

// Point is the physical position of a continuous index.
func (im *Image) Point(index render.Vec3) render.Vec3 {
	return im.origin.Add(im.indexToPhysical.MulVec(index))
}

// ContinuousIndex is the continuous index of a physical point.
func (im *Image) ContinuousIndex(point render.Vec3) render.Vec3 {
	return im.physicalToIndex.MulVec(point.Sub(im.origin))
}

// inside reports whether a continuous index is within half a sample of
// the grid (−0.5 ≤ i < size−0.5 on every axis), the buffer of ITK's
// interpolators; linear interpolation clamps beyond the outer samples.
func (im *Image) inside(index render.Vec3) bool {
	return index.X >= -0.5 && index.X < float64(im.size[0])-0.5 &&
		index.Y >= -0.5 && index.Y < float64(im.size[1])-0.5 &&
		index.Z >= -0.5 && index.Z < float64(im.size[2])-0.5
}

// Interpolate is the linearly interpolated sample at a physical point;
// false more than half a sample outside the grid.
func (im *Image) Interpolate(point render.Vec3) (float64, bool) {
	index := im.ContinuousIndex(point)
	if !im.inside(index) {
		return 0, false
	}
	return im.linearAt(index), true
}

// linearAt interpolates at a continuous index inside the grid, clamping the
// corners so that the last sample of an axis (and a single-sample axis)
// needs no neighbour beyond it.
func (im *Image) linearAt(index render.Vec3) float64 {
	value, _, _ := im.linearAtWithSlices(index)
	return value
}

// linearAtWithSlices reports the z range from the interpolation stencil
// without recalculating the clamped coordinate for provenance.
func (im *Image) linearAtWithSlices(index render.Vec3) (value float64, first, last int) {
	x, fx, nx := linearAxis(index.X, im.size[0])
	y, fy, ny := linearAxis(index.Y, im.size[1])
	z, fz, nz := linearAxis(index.Z, im.size[2])
	if nx == 1 && ny == 1 && nz == 1 {
		return 0 + float64(im.At(x, y, z)), z, z
	}
	xWeight := [2]float64{1 - fx, fx}
	yWeight := [2]float64{1 - fy, fy}
	zWeight := [2]float64{1 - fz, fz}
	width, height := im.size[0], im.size[1]
	for k := range nz {
		for j := range ny {
			row := ((z+k)*height+y+j)*width + x
			for i := range nx {
				weight := xWeight[i] * yWeight[j] * zWeight[k]
				if weight != 0 {
					value += weight * float64(im.data[row+i])
				}
			}
		}
	}
	return value, z, z + nz - 1
}

func linearAxis(index float64, size int) (base int, fraction float64, count int) {
	index = min(max(index, 0), float64(size-1))
	base = int(index)
	fraction = index - float64(base)
	if fraction == 0 {
		return base, 0, 1
	}
	return base, fraction, 2
}
