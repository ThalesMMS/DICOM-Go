package roi

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/ThalesMMS/dicom-go/imagefilter"
)

// Splitting touching objects of a binary mask (#1105) with the classic
// distance-map watershed: the exact Euclidean distance (mm) of every mask
// voxel to the background is a relief with one dome per round object and a
// saddle at each neck where objects touch. The extended maxima of that
// relief (peaks whose dynamic exceeds HeightMM) are the markers, and the
// watershed of the negated distance, confined to the mask, gives every mask
// voxel to one marker. Written from the published method (Beucher and
// Meyer; Soille, "Morphological Image Analysis", ch. 9) on top of this
// package's distance transform, h-maxima, regional maxima and watershed.

// SplitOptions configures SplitTouchingObjects.
type SplitOptions struct {
	// HeightMM is the h of the h-maxima of the distance map, in mm: two
	// domes become separate regions only when the neck between them is at
	// least HeightMM narrower (in distance to the boundary) than the lower
	// dome's centre. Larger values merge more; it must be positive. One or
	// two voxel sizes is a usual choice.
	HeightMM float64
	// Connectivity is the neighbourhood of markers and flooding: 6 (the
	// zero value), 18 or 26.
	Connectivity Connectivity3D
	// MaxVoxels bounds the voxels of the mask's bounding box (plus one voxel
	// of margin); 0 means DefaultMaxMorphologyVoxels.
	MaxVoxels int
}

// SplitResult is a mask split into regions.
type SplitResult struct {
	// Labels holds, on the caller's grid, the region of every mask voxel
	// (1 = the largest region, 2 the next, …; equal sizes in scan order of
	// their markers) and 0 outside the mask.
	Labels *LabelVolume
	// RegionVoxels[k-1] is the voxel count of region k.
	RegionVoxels []int
}

// Regions returns the number of regions.
func (r *SplitResult) Regions() int { return len(r.RegionVoxels) }

// SplitTouchingObjects splits the set voxels of mask into regions, one per
// roughly convex object, separating objects that touch through a neck (for
// example two nodules in contact). grid must have the mask's dimensions;
// the mask's X, Y and Z coordinates (mm) give the distances. The work runs
// on the mask's bounding box. Every mask voxel receives a region: each
// connected component has at least one marker.
func SplitTouchingObjects(ctx context.Context, grid Grid, mask *VolumeMask, opts SplitOptions) (*SplitResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !(opts.HeightMM > 0) || math.IsInf(opts.HeightMM, 0) {
		return nil, fmt.Errorf("%w: split height %v mm must be positive and finite", ErrInvalidMorphology, opts.HeightMM)
	}
	gopts, err := GrayscaleOptions{Connectivity: opts.Connectivity, MaxVoxels: opts.MaxVoxels}.normalize()
	if err != nil {
		return nil, err
	}
	if err := mask.validate(); err != nil {
		return nil, err
	}
	if grid.Columns != mask.Columns || grid.Rows != mask.Rows || grid.Slices != mask.Slices {
		return nil, fmt.Errorf("%w: grid %dx%dx%d, mask %dx%dx%d", ErrInvalidMorphology,
			grid.Columns, grid.Rows, grid.Slices, mask.Columns, mask.Rows, mask.Slices)
	}
	full, err := NewLabelVolume(grid)
	if err != nil {
		return nil, err
	}
	result := &SplitResult{Labels: full}
	box, ok := volumeMaskBounds(mask)
	if !ok {
		return result, nil
	}
	for axis, n := range [3]int{mask.Columns, mask.Rows, mask.Slices} {
		box.Min[axis], box.Max[axis] = max(box.Min[axis]-1, 0), min(box.Max[axis]+1, n)
	}
	sub := cropVolumeMask(mask, box)
	if n := len(sub.Voxels); n > gopts.MaxVoxels {
		return nil, fmt.Errorf("%w: the mask's bounding box has %d voxels, budget %d", ErrMorphologyTooLarge, n, gopts.MaxVoxels)
	}
	distance, err := sub.DistanceTransform(ctx, false)
	if err != nil {
		return nil, err
	}
	// The relief is the distance inside the mask and a level far below
	// every mask voxel outside it, so h-maxima never joins two components
	// through the background and every component keeps a maximum.
	relief := &imagefilter.Volume{
		Dims:      [3]int{sub.Columns, sub.Rows, sub.Slices},
		SpacingMM: [3]float64{axisStep(sub.X), axisStep(sub.Y), axisStep(sub.Z)},
		Data:      make([]float32, len(sub.Voxels)),
	}
	peak := 0.0
	for i, in := range sub.Voxels {
		if in {
			peak = math.Max(peak, distance[i])
		}
	}
	if math.IsInf(peak, 0) {
		// The mask fills its whole box up to the volume border: there is no
		// background to measure from, so it is one region.
		for i, in := range mask.Voxels {
			if in {
				full.Labels[i] = 1
			}
		}
		result.RegionVoxels = []int{mask.Count()}
		return result, nil
	}
	outside := float32(-(peak + opts.HeightMM + 1))
	for i, in := range sub.Voxels {
		if in {
			relief.Data[i] = float32(distance[i])
		} else {
			relief.Data[i] = outside
		}
	}
	peaks, err := ExtendedMaxima(ctx, relief, opts.HeightMM, gopts)
	if err != nil {
		return nil, err
	}
	for i, in := range sub.Voxels {
		peaks.Voxels[i] = peaks.Voxels[i] && in
	}
	peaks.X, peaks.Y, peaks.Z = sub.X, sub.Y, sub.Z
	subGrid := grid.crop(box)
	markers, count, err := LabelComponents(ctx, subGrid, peaks, gopts.Connectivity)
	if err != nil {
		return nil, err
	}
	for i, v := range relief.Data {
		relief.Data[i] = -v
	}
	flood, err := WatershedFromMarkers(ctx, relief, markers, WatershedOptions{
		Connectivity: gopts.Connectivity, Mask: sub, MaxVoxels: gopts.MaxVoxels,
	})
	if err != nil {
		return nil, err
	}
	if flood.Unreached != 0 {
		return nil, fmt.Errorf("%w: %d mask voxels were not reached by any marker", ErrInvalidMorphology, flood.Unreached)
	}
	// Number the regions by decreasing size, then paste them into the grid.
	sizes := make([]int, count+1)
	for _, l := range flood.Labels.Labels {
		sizes[l]++
	}
	order := make([]int, count)
	for i := range order {
		order[i] = i + 1
	}
	sort.SliceStable(order, func(a, b int) bool { return sizes[order[a]] > sizes[order[b]] })
	rank := make([]uint16, count+1)
	result.RegionVoxels = make([]int, count)
	for k, l := range order {
		rank[l] = uint16(k + 1)
		result.RegionVoxels[k] = sizes[l]
	}
	sx, sy := sub.Columns, sub.Rows
	for z := 0; z < sub.Slices; z++ {
		for y := 0; y < sy; y++ {
			src := flood.Labels.Labels[(z*sy+y)*sx : (z*sy+y+1)*sx]
			dst := full.Labels[((box.Min[2]+z)*mask.Rows+box.Min[1]+y)*mask.Columns+box.Min[0]:]
			for x, l := range src {
				dst[x] = rank[l]
			}
		}
	}
	return result, nil
}

// volumeMaskBounds returns the index box of the set voxels.
func volumeMaskBounds(m *VolumeMask) (IndexBox, bool) {
	box := IndexBox{Min: [3]int{m.Columns, m.Rows, m.Slices}}
	found := false
	for z, i := 0, 0; z < m.Slices; z++ {
		for y := 0; y < m.Rows; y++ {
			for x := 0; x < m.Columns; x, i = x+1, i+1 {
				if !m.Voxels[i] {
					continue
				}
				found = true
				for axis, v := range [3]int{x, y, z} {
					box.Min[axis] = min(box.Min[axis], v)
					box.Max[axis] = max(box.Max[axis], v+1)
				}
			}
		}
	}
	return box, found
}

// cropVolumeMask returns the voxels and coordinates of box.
func cropVolumeMask(m *VolumeMask, box IndexBox) *VolumeMask {
	out := &VolumeMask{
		Columns: box.Max[0] - box.Min[0], Rows: box.Max[1] - box.Min[1], Slices: box.Max[2] - box.Min[2],
		X: append([]float64(nil), m.X[box.Min[0]:box.Max[0]]...),
		Y: append([]float64(nil), m.Y[box.Min[1]:box.Max[1]]...),
		Z: append([]float64(nil), m.Z[box.Min[2]:box.Max[2]]...),
	}
	out.Voxels = make([]bool, out.Columns*out.Rows*out.Slices)
	for z := 0; z < out.Slices; z++ {
		for y := 0; y < out.Rows; y++ {
			src := m.Voxels[m.index(box.Min[0], box.Min[1]+y, box.Min[2]+z):]
			copy(out.Voxels[(z*out.Rows+y)*out.Columns:(z*out.Rows+y+1)*out.Columns], src[:out.Columns])
		}
	}
	return out
}

// axisStep returns the mean spacing of ascending coordinates, 1 for a
// single one. Only the positivity matters to the morphology filters.
func axisStep(c []float64) float64 {
	if len(c) < 2 {
		return 1
	}
	return (c[len(c)-1] - c[0]) / float64(len(c)-1)
}
