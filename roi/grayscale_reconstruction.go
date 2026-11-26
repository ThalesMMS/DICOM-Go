package roi

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/imagefilter"
)

// Grayscale reconstruction and the filters built on it (#1105): geodesic
// reconstruction by dilation and by erosion, regional maxima and minima,
// h-maxima and h-minima (with h-convex, h-concave and extended extrema) and
// grayscale hole filling, over dense float32 volumes (imagefilter.Volume).
//
// Ported from ITK (Insight Toolkit), commit
// 35b196e13c366e0386389a71c936a0436c568cea, Apache-2.0, Copyright NumFOCUS
// (see THIRD_PARTY_NOTICES.md and testdata/upstream/itk/LICENSE.ITK, which
// holds ITK's LICENSE and NOTICE):
//
//   - reconstruction follows itkReconstructionImageFilter.hxx (Vincent's
//     hybrid algorithm: a raster pass, an anti-raster pass that queues the
//     voxels able to propagate further, then a FIFO flood);
//   - regional extrema follow itkValuedRegionalExtremaImageFilter.hxx (a
//     voxel with a strictly higher neighbour floods its plateau as
//     non-maximal) and itkRegionalMaximaImageFilter.hxx;
//   - h-extrema, h-convex/concave and hole filling follow the marker
//     constructions of itkHMaximaImageFilter.hxx, itkHMinimaImageFilter.hxx,
//     itkHConvexImageFilter.hxx, itkHConcaveImageFilter.hxx and
//     itkGrayscaleFillholeImageFilter.hxx.
//
// Modifications: rewritten in Go over x-fastest float32 volumes with
// explicit neighbour offsets instead of padded images and shaped
// neighbourhood iterators; reconstruction by erosion runs as reconstruction
// by dilation of the negated images; float samples are compared exactly
// (ITK's NotAlmostEquals tolerance is not needed because the marker never
// exceeds the mask); the 6/18/26 connectivity of roi.Connectivity3D replaces
// ITK's FullyConnected flag; an axis of one sample is not an image border
// (a one-slice volume is filled as a 2-D image); voxel budgets and context
// cancellation bound every call; regional extrema are returned as a
// VolumeMask instead of a foreground/background image, and a flat image is
// one regional maximum and minimum (ITK's FlatIsMaxima default).

var (
	// ErrInvalidMorphology reports a grayscale morphology or watershed
	// request whose images, markers, parameters or options cannot be used.
	ErrInvalidMorphology = errors.New("roi: invalid grayscale morphology request")
	// ErrMorphologyTooLarge reports an image beyond the voxel budget.
	ErrMorphologyTooLarge = errors.New("roi: grayscale morphology exceeds the voxel budget")
)

// DefaultMaxMorphologyVoxels bounds the voxels of one grayscale morphology,
// reconstruction or watershed call (a 512x512x256 volume) unless
// GrayscaleOptions.MaxVoxels or WatershedOptions.MaxVoxels sets another
// budget. Reconstruction needs about 8 bytes per voxel beyond its inputs
// (16 for reconstruction by erosion), plus its queue; a watershed about 15.
const DefaultMaxMorphologyVoxels = 1 << 26

// GrayscaleOptions configures the grayscale morphology and reconstruction
// filters.
type GrayscaleOptions struct {
	// Connectivity is the neighbourhood of flat zones and propagation: 6
	// (faces; the zero value, ITK's default), 18 or 26 (ITK's
	// FullyConnected). A one-slice volume uses the in-plane part: 4 or 8.
	Connectivity Connectivity3D
	// MaxVoxels bounds the image size; 0 means DefaultMaxMorphologyVoxels.
	MaxVoxels int
}

func (o GrayscaleOptions) normalize() (GrayscaleOptions, error) {
	if o.Connectivity == 0 {
		o.Connectivity = Connectivity6
	}
	if err := o.Connectivity.validate(); err != nil {
		return o, err
	}
	switch {
	case o.MaxVoxels == 0:
		o.MaxVoxels = DefaultMaxMorphologyVoxels
	case o.MaxVoxels < 0:
		return o, fmt.Errorf("%w: negative voxel budget %d", ErrInvalidMorphology, o.MaxVoxels)
	}
	return o, nil
}

// checkMorphologyVolume validates v and its size against the budget.
func checkMorphologyVolume(v *imagefilter.Volume, maxVoxels int, name string) error {
	if err := v.Validate(); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrInvalidMorphology, name, err)
	}
	if n := len(v.Data); n > maxVoxels || n > math.MaxInt32 {
		return fmt.Errorf("%w: %s has %d voxels, budget %d", ErrMorphologyTooLarge, name, n, maxVoxels)
	}
	return nil
}

// checkImage validates src and its size against opts before a filter
// allocates its working images.
func checkImage(src *imagefilter.Volume, opts GrayscaleOptions) error {
	opts, err := opts.normalize()
	if err != nil {
		return err
	}
	return checkMorphologyVolume(src, opts.MaxVoxels, "image")
}

// nbOffset is one neighbour: its displacement and its step in the x-fastest
// sample array.
type nbOffset struct {
	dx, dy, dz int
	step       int
}

// neighbourhood lists the neighbours of a connectivity on a grid, without
// the displacements along axes of one sample. prev holds the neighbours
// that precede a voxel in raster order and next those that follow it.
type neighbourhood struct {
	dims       [3]int
	all        []nbOffset
	prev, next []nbOffset
}

func newNeighbourhood(dims [3]int, c Connectivity3D) neighbourhood {
	n := neighbourhood{dims: dims}
	for dz := -1; dz <= 1; dz++ {
		for dy := -1; dy <= 1; dy++ {
			for dx := -1; dx <= 1; dx++ {
				order := absInt(dx) + absInt(dy) + absInt(dz)
				if order == 0 || order > connectivityOrder(c) ||
					dims[0] == 1 && dx != 0 || dims[1] == 1 && dy != 0 || dims[2] == 1 && dz != 0 {
					continue
				}
				o := nbOffset{dx: dx, dy: dy, dz: dz, step: (dz*dims[1]+dy)*dims[0] + dx}
				n.all = append(n.all, o)
				if o.step < 0 {
					n.prev = append(n.prev, o)
				} else {
					n.next = append(n.next, o)
				}
			}
		}
	}
	return n
}

// connectivityOrder is the largest number of axes a neighbour may move
// along: 1 for 6-, 2 for 18- and 3 for 26-connectivity.
func connectivityOrder(c Connectivity3D) int {
	switch c {
	case Connectivity18:
		return 2
	case Connectivity26:
		return 3
	default:
		return 1
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// coords returns the (x, y, z) position of sample i.
func (n *neighbourhood) coords(i int) (int, int, int) {
	plane := n.dims[0] * n.dims[1]
	z := i / plane
	r := i - z*plane
	y := r / n.dims[0]
	return r - y*n.dims[0], y, z
}

// inside reports whether the neighbour o of voxel (x, y, z) lies in the grid.
func (n *neighbourhood) inside(x, y, z int, o nbOffset) bool {
	return uint(x+o.dx) < uint(n.dims[0]) && uint(y+o.dy) < uint(n.dims[1]) && uint(z+o.dz) < uint(n.dims[2])
}

// ReconstructionByDilation returns the grayscale reconstruction by dilation
// of marker under mask: marker dilated geodesically inside mask until
// stable, so every regional maximum of mask that no marker reaches is
// lowered to the level at which the marker enters it. marker must not
// exceed mask anywhere; the two volumes must have the same dimensions.
func ReconstructionByDilation(ctx context.Context, marker, mask *imagefilter.Volume, opts GrayscaleOptions) (*imagefilter.Volume, error) {
	return reconstruction(ctx, marker, mask, opts, false)
}

// ReconstructionByErosion returns the grayscale reconstruction by erosion
// of marker above mask, the dual of ReconstructionByDilation: marker must
// not be below mask anywhere.
func ReconstructionByErosion(ctx context.Context, marker, mask *imagefilter.Volume, opts GrayscaleOptions) (*imagefilter.Volume, error) {
	return reconstruction(ctx, marker, mask, opts, true)
}

func reconstruction(ctx context.Context, marker, mask *imagefilter.Volume, opts GrayscaleOptions, erosion bool) (*imagefilter.Volume, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	opts, err := opts.normalize()
	if err != nil {
		return nil, err
	}
	if err := checkMorphologyVolume(marker, opts.MaxVoxels, "marker"); err != nil {
		return nil, err
	}
	if err := checkMorphologyVolume(mask, opts.MaxVoxels, "mask"); err != nil {
		return nil, err
	}
	if marker.Dims != mask.Dims {
		return nil, fmt.Errorf("%w: marker dimensions %v differ from mask %v", ErrInvalidMorphology, marker.Dims, mask.Dims)
	}
	out := marker.Clone()
	limit := mask.Data
	if erosion {
		limit = make([]float32, len(mask.Data))
		for i, v := range mask.Data {
			limit[i] = -v
			out.Data[i] = -out.Data[i]
		}
	}
	for i, v := range out.Data {
		if v > limit[i] {
			if erosion {
				return nil, fmt.Errorf("%w: marker below mask at sample %d", ErrInvalidMorphology, i)
			}
			return nil, fmt.Errorf("%w: marker above mask at sample %d", ErrInvalidMorphology, i)
		}
	}
	if err := reconstructByDilation(ctx, out.Data, limit, newNeighbourhood(out.Dims, opts.Connectivity)); err != nil {
		return nil, err
	}
	if erosion {
		for i := range out.Data {
			out.Data[i] = -out.Data[i]
		}
	}
	return out, nil
}

// reconstructByDilation reconstructs j (the marker, overwritten) under
// mask with Vincent's hybrid algorithm; j <= mask on entry.
func reconstructByDilation(ctx context.Context, j, mask []float32, nb neighbourhood) error {
	nx, ny, nz := nb.dims[0], nb.dims[1], nb.dims[2]
	// Raster pass: propagate from the preceding neighbours, clamped by the
	// mask.
	for z := 0; z < nz; z++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		for y := 0; y < ny; y++ {
			i := (z*ny + y) * nx
			for x := 0; x < nx; x, i = x+1, i+1 {
				v := j[i]
				for _, o := range nb.prev {
					if nb.inside(x, y, z, o) && j[i+o.step] > v {
						v = j[i+o.step]
					}
				}
				j[i] = min(v, mask[i])
			}
		}
	}
	// Anti-raster pass: propagate from the following neighbours and queue
	// every voxel that can still raise one of them.
	var queue []int32
	for z := nz - 1; z >= 0; z-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		for y := ny - 1; y >= 0; y-- {
			i := (z*ny+y)*nx + nx - 1
			for x := nx - 1; x >= 0; x, i = x-1, i-1 {
				v := j[i]
				for _, o := range nb.next {
					if nb.inside(x, y, z, o) && j[i+o.step] > v {
						v = j[i+o.step]
					}
				}
				v = min(v, mask[i])
				j[i] = v
				for _, o := range nb.next {
					if q := i + o.step; nb.inside(x, y, z, o) && j[q] < v && j[q] < mask[q] {
						queue = append(queue, int32(i))
						break
					}
				}
			}
		}
	}
	// FIFO flood of what the two passes could not reach.
	for head, steps := 0, 0; head < len(queue); steps++ {
		if steps&0xffff == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			if head > len(queue)/2 {
				queue = append(queue[:0], queue[head:]...)
				head = 0
			}
		}
		p := int(queue[head])
		head++
		v := j[p]
		x, y, z := nb.coords(p)
		for _, o := range nb.all {
			q := p + o.step
			if nb.inside(x, y, z, o) && j[q] < v && j[q] < mask[q] {
				j[q] = min(v, mask[q])
				queue = append(queue, int32(q))
			}
		}
	}
	return nil
}

// RegionalMaxima returns the regional maxima of src: the flat zones
// (connected voxels of equal value) with no strictly higher neighbour. A
// flat image is one regional maximum. The result has src's dimensions and
// uniform spacing.
func RegionalMaxima(ctx context.Context, src *imagefilter.Volume, opts GrayscaleOptions) (*VolumeMask, error) {
	return regionalExtrema(ctx, src, opts, false)
}

// RegionalMinima returns the regional minima of src: the flat zones with no
// strictly lower neighbour.
func RegionalMinima(ctx context.Context, src *imagefilter.Volume, opts GrayscaleOptions) (*VolumeMask, error) {
	return regionalExtrema(ctx, src, opts, true)
}

func regionalExtrema(ctx context.Context, src *imagefilter.Volume, opts GrayscaleOptions, minima bool) (*VolumeMask, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	opts, err := opts.normalize()
	if err != nil {
		return nil, err
	}
	if err := checkMorphologyVolume(src, opts.MaxVoxels, "image"); err != nil {
		return nil, err
	}
	nb := newNeighbourhood(src.Dims, opts.Connectivity)
	data := src.Data
	// beyond reports whether a is strictly more extreme than b.
	beyond := func(a, b float32) bool { return a > b }
	if minima {
		beyond = func(a, b float32) bool { return a < b }
	}
	excluded := make([]bool, len(data))
	var stack []int32
	for i := range data {
		if i&0xffff == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if excluded[i] {
			continue
		}
		v := data[i]
		x, y, z := nb.coords(i)
		higher := false
		for _, o := range nb.all {
			if nb.inside(x, y, z, o) && beyond(data[i+o.step], v) {
				higher = true
				break
			}
		}
		if !higher {
			continue
		}
		// The flat zone of i has a more extreme neighbour: exclude all of it.
		excluded[i] = true
		stack = append(stack[:0], int32(i))
		for len(stack) > 0 {
			p := int(stack[len(stack)-1])
			stack = stack[:len(stack)-1]
			px, py, pz := nb.coords(p)
			for _, o := range nb.all {
				if q := p + o.step; nb.inside(px, py, pz, o) && !excluded[q] && data[q] == v {
					excluded[q] = true
					stack = append(stack, int32(q))
				}
			}
		}
	}
	out := NewVolumeMask(src.Dims[0], src.Dims[1], src.Dims[2], src.SpacingMM)
	for i, e := range excluded {
		out.Voxels[i] = !e
	}
	return out, nil
}

// checkHeight validates an h-extrema height.
func checkHeight(h float64) error {
	if !(h >= 0) || math.IsInf(h, 0) || h > math.MaxFloat32 {
		return fmt.Errorf("%w: height %v", ErrInvalidMorphology, h)
	}
	return nil
}

// shifted returns src with h added to every sample, as float32; for h >= 0
// the result is never below src and for h <= 0 never above it.
func shifted(src *imagefilter.Volume, h float64) *imagefilter.Volume {
	out := &imagefilter.Volume{Dims: src.Dims, SpacingMM: src.SpacingMM, Data: make([]float32, len(src.Data))}
	for i, v := range src.Data {
		out.Data[i] = float32(float64(v) + h)
	}
	return out
}

// HMaxima suppresses the regional maxima of src whose dynamic (height above
// the saddle that joins them to a higher one) is at most h: it is the
// reconstruction by dilation of src-h under src. Every remaining maximum is
// lowered by h; h is in the image's units and must be finite and not
// negative.
func HMaxima(ctx context.Context, src *imagefilter.Volume, h float64, opts GrayscaleOptions) (*imagefilter.Volume, error) {
	if err := checkHeight(h); err != nil {
		return nil, err
	}
	if err := checkImage(src, opts); err != nil {
		return nil, err
	}
	return ReconstructionByDilation(ctx, shifted(src, -h), src, opts)
}

// HMinima suppresses the regional minima of src whose depth is at most h:
// the reconstruction by erosion of src+h above src.
func HMinima(ctx context.Context, src *imagefilter.Volume, h float64, opts GrayscaleOptions) (*imagefilter.Volume, error) {
	if err := checkHeight(h); err != nil {
		return nil, err
	}
	if err := checkImage(src, opts); err != nil {
		return nil, err
	}
	return ReconstructionByErosion(ctx, shifted(src, h), src, opts)
}

// HConvex returns src - HMaxima(src, h): the domes of height up to h at
// every regional maximum (h at the maxima whose dynamic exceeds h).
func HConvex(ctx context.Context, src *imagefilter.Volume, h float64, opts GrayscaleOptions) (*imagefilter.Volume, error) {
	hmax, err := HMaxima(ctx, src, h, opts)
	if err != nil {
		return nil, err
	}
	for i, v := range src.Data {
		hmax.Data[i] = v - hmax.Data[i]
	}
	return hmax, nil
}

// HConcave returns HMinima(src, h) - src: the basins of depth up to h at
// every regional minimum.
func HConcave(ctx context.Context, src *imagefilter.Volume, h float64, opts GrayscaleOptions) (*imagefilter.Volume, error) {
	hmin, err := HMinima(ctx, src, h, opts)
	if err != nil {
		return nil, err
	}
	for i, v := range src.Data {
		hmin.Data[i] -= v
	}
	return hmin, nil
}

// ExtendedMaxima returns the regional maxima of HMaxima(src, h): one flat
// zone per peak whose dynamic exceeds h, the usual markers of bright
// objects (for example PET hotspots or distance-map centres). h = 0 gives
// RegionalMaxima.
func ExtendedMaxima(ctx context.Context, src *imagefilter.Volume, h float64, opts GrayscaleOptions) (*VolumeMask, error) {
	hmax, err := HMaxima(ctx, src, h, opts)
	if err != nil {
		return nil, err
	}
	return RegionalMaxima(ctx, hmax, opts)
}

// ExtendedMinima returns the regional minima of HMinima(src, h).
func ExtendedMinima(ctx context.Context, src *imagefilter.Volume, h float64, opts GrayscaleOptions) (*VolumeMask, error) {
	hmin, err := HMinima(ctx, src, h, opts)
	if err != nil {
		return nil, err
	}
	return RegionalMinima(ctx, hmin, opts)
}

// GrayscaleFillHoles fills the regional minima of src that do not touch the
// image border (dark holes enclosed by brighter voxels) up to the level of
// their lowest surrounding saddle: the reconstruction by erosion of a marker
// equal to src on the border and to the image maximum elsewhere. An axis of
// one sample does not form a border, so a one-slice volume is filled as a
// 2-D image.
func GrayscaleFillHoles(ctx context.Context, src *imagefilter.Volume, opts GrayscaleOptions) (*imagefilter.Volume, error) {
	if err := checkImage(src, opts); err != nil {
		return nil, err
	}
	peak := float32(math.Inf(-1))
	for _, v := range src.Data {
		peak = max(peak, v)
	}
	marker := &imagefilter.Volume{Dims: src.Dims, SpacingMM: src.SpacingMM, Data: make([]float32, len(src.Data))}
	nx, ny, nz := src.Dims[0], src.Dims[1], src.Dims[2]
	border := func(i, n int) bool { return n > 1 && (i == 0 || i == n-1) }
	for z, i := 0, 0; z < nz; z++ {
		for y := 0; y < ny; y++ {
			for x := 0; x < nx; x, i = x+1, i+1 {
				if border(x, nx) || border(y, ny) || border(z, nz) {
					marker.Data[i] = src.Data[i]
				} else {
					marker.Data[i] = peak
				}
			}
		}
	}
	return ReconstructionByErosion(ctx, marker, src, opts)
}
