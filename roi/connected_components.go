package roi

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// Connected-component labelling and seeded filling over run-length masks
// (#1094). Components are labelled with a two-pass union-find over the
// per-row runs of RasterMask, the same run ("line") formulation ITK's
// ConnectedComponentImageFilter uses: one union per overlapping pair of runs
// in neighbouring rows, then a single relabelling sweep. Time and memory are
// linear in the number of runs rather than voxels, and no per-voxel map or
// dense visited buffer is allocated. Seeded filling is a span (scanline) fill
// that only visits the filled component and its border. This is a clean-room
// implementation of the published run-based algorithm; no ITK code is copied.

// Connectivity3D selects the voxel neighbourhood used to join voxels into a
// component: 6 (faces), 18 (faces and edges) or 26 (faces, edges and corners).
type Connectivity3D int

const (
	// Connectivity6 joins voxels that share a face.
	Connectivity6 Connectivity3D = 6
	// Connectivity18 joins voxels that share a face or an edge.
	Connectivity18 Connectivity3D = 18
	// Connectivity26 joins voxels that share a face, an edge or a corner.
	Connectivity26 Connectivity3D = 26
)

// ErrInvalidConnectivity reports a connectivity other than 6, 18 or 26.
var ErrInvalidConnectivity = errors.New("roi: connectivity must be 6, 18 or 26")

// ErrInvalidFillVolume reports fill dimensions that cannot describe a volume.
var ErrInvalidFillVolume = errors.New("roi: invalid flood fill volume")

func (c Connectivity3D) validate() error {
	switch c {
	case Connectivity6, Connectivity18, Connectivity26:
		return nil
	}
	return fmt.Errorf("%w: got %d", ErrInvalidConnectivity, int(c))
}

// runAdjacency describes which neighbouring rows join runs and by how much
// each neighbour run's span is widened (0 = the runs must share a column,
// 1 = diagonal contact counts).
type runAdjacency struct {
	inPlane     int  // widening for row y-1 of the same slice
	throughRow  int  // widening for row y of the previous slice
	diagonal    bool // whether rows y±1 of the previous slice are neighbours
	diagonalExt int  // widening for those diagonal rows
}

func (c Connectivity3D) adjacency() runAdjacency {
	switch c {
	case Connectivity18:
		return runAdjacency{inPlane: 1, throughRow: 1, diagonal: true}
	case Connectivity26:
		return runAdjacency{inPlane: 1, throughRow: 1, diagonal: true, diagonalExt: 1}
	default:
		return runAdjacency{}
	}
}

// runVolume indexes the runs of a stack of masks in (slice, row, start)
// order without copying them: run r of row y of slice ordinal s has the global
// index rowOff[rowBase[s]+y] + i.
type runVolume struct {
	z       []int // ascending slice indices
	masks   []*RasterMask
	rowBase []int
	rowOff  []int
}

func newRunVolume(z []int, masks []*RasterMask) *runVolume {
	v := &runVolume{z: z, masks: masks, rowBase: make([]int, len(masks))}
	totalRows := 0
	for s, mask := range masks {
		v.rowBase[s] = totalRows
		totalRows += len(mask.runs)
	}
	v.rowOff = make([]int, totalRows+1)
	n := 0
	for s, mask := range masks {
		base := v.rowBase[s]
		for y, runs := range mask.runs {
			v.rowOff[base+y] = n
			n += len(runs)
		}
	}
	v.rowOff[totalRows] = n
	return v
}

func segmentationRunVolume(seg *Segmentation3D) *runVolume {
	z := seg.Slices()
	masks := make([]*RasterMask, len(z))
	for i, slice := range z {
		masks[i] = seg.masks[slice]
	}
	return newRunVolume(z, masks)
}

func (v *runVolume) runCount() int { return v.rowOff[len(v.rowOff)-1] }

// row returns the runs of row y of slice ordinal s and the global index of
// the first one; rows outside the mask are empty.
func (v *runVolume) row(s, y int) ([]MaskRun, int) {
	mask := v.masks[s]
	if y < 0 || y >= len(mask.runs) {
		return nil, 0
	}
	return mask.runs[y], v.rowOff[v.rowBase[s]+y]
}

// unionFind keeps every parent index at or below its child, so the root of a
// set is its first run in scan order.
type unionFind []int

func (u unionFind) find(i int) int {
	for u[i] != i {
		u[i] = u[u[i]]
		i = u[i]
	}
	return i
}

func (u unionFind) union(a, b int) {
	a, b = u.find(a), u.find(b)
	switch {
	case a < b:
		u[b] = a
	case b < a:
		u[a] = b
	}
}

// unionOverlapping joins every run of cur with the runs of other that lie
// within ext columns of it. Both lists are sorted, so one sweep suffices.
func (u unionFind) unionOverlapping(cur []MaskRun, curBase int, other []MaskRun, otherBase, ext int) {
	i, j := 0, 0
	for i < len(cur) && j < len(other) {
		if cur[i].Start < other[j].End+ext && other[j].Start < cur[i].End+ext {
			u.union(curBase+i, otherBase+j)
		}
		if cur[i].End < other[j].End {
			i++
		} else {
			j++
		}
	}
}

// label assigns every run a component label, numbered in the order of each
// component's first run (slice, then row, then column), and returns the
// labels with the component count.
func (v *runVolume) label(adj runAdjacency) ([]int, int) {
	u := make(unionFind, v.runCount())
	for i := range u {
		u[i] = i
	}
	for s := range v.masks {
		prev := -1
		if s > 0 && v.z[s-1] == v.z[s]-1 {
			prev = s - 1
		}
		for y := range v.masks[s].runs {
			cur, base := v.row(s, y)
			if len(cur) == 0 {
				continue
			}
			// Runs are kept merged, but join touching runs defensively.
			for i := 1; i < len(cur); i++ {
				if cur[i].Start <= cur[i-1].End {
					u.union(base+i-1, base+i)
				}
			}
			above, aboveBase := v.row(s, y-1)
			u.unionOverlapping(cur, base, above, aboveBase, adj.inPlane)
			if prev < 0 {
				continue
			}
			back, backBase := v.row(prev, y)
			u.unionOverlapping(cur, base, back, backBase, adj.throughRow)
			if adj.diagonal {
				back, backBase = v.row(prev, y-1)
				u.unionOverlapping(cur, base, back, backBase, adj.diagonalExt)
				back, backBase = v.row(prev, y+1)
				u.unionOverlapping(cur, base, back, backBase, adj.diagonalExt)
			}
		}
	}
	// Every parent precedes its child, so one forward sweep turns parents
	// into labels: a root opens a new label, anything else copies its
	// (already relabelled) parent's label.
	count := 0
	for r := range u {
		if p := u[r]; p == r {
			u[r] = count
			count++
		} else {
			u[r] = u[p]
		}
	}
	return u, count
}

// emit writes the runs of every labelled component whose target is not -1
// into output target[label] (target nil = identity). newMask returns the mask
// of output t for slice ordinal s and is called once per (t, s) pair that
// receives a run; runs are clipped to columns×rows and output rows share one
// exactly sized backing array per output.
func (v *runVolume) emit(labels []int, target []int, outputs, columns, rows int, newMask func(t, s int) *RasterMask) {
	counts := make([]int, outputs)
	for _, l := range labels {
		if target != nil {
			l = target[l]
		}
		if l >= 0 {
			counts[l]++
		}
	}
	slabs := make([][]MaskRun, outputs)
	for t := range slabs {
		slabs[t] = make([]MaskRun, 0, counts[t])
	}
	current := make([]*RasterMask, outputs)
	currentSlice := make([]int, outputs)
	rowKey := make([]int, outputs)
	rowStart := make([]int, outputs)
	for t := range currentSlice {
		currentSlice[t] = -1
		rowKey[t] = -1
	}
	for s, mask := range v.masks {
		for y := range mask.runs {
			runs, base := v.row(s, y)
			key := v.rowBase[s] + y
			for i, run := range runs {
				t := labels[base+i]
				if target != nil {
					t = target[t]
				}
				if t < 0 {
					continue
				}
				if currentSlice[t] != s {
					current[t] = newMask(t, s)
					currentSlice[t] = s
				}
				if y >= rows {
					continue
				}
				start, end := clampSpan(run.Start, run.End, columns)
				if start >= end {
					continue
				}
				if rowKey[t] != key {
					rowKey[t] = key
					rowStart[t] = len(slabs[t])
				}
				slab := slabs[t]
				if n := len(slab); n > rowStart[t] && start <= slab[n-1].End {
					slab[n-1].End = max(slab[n-1].End, end)
				} else {
					slab = append(slab, MaskRun{Start: start, End: end})
				}
				slabs[t] = slab
				current[t].runs[y] = slab[rowStart[t]:len(slab):len(slab)]
			}
		}
	}
}

func (v *runVolume) segmentations(seg *Segmentation3D, labels, target []int, outputs int) []*Segmentation3D {
	out := make([]*Segmentation3D, outputs)
	for t := range out {
		out[t] = NewSegmentation3D(seg.Geometry, seg.Columns, seg.Rows)
	}
	v.emit(labels, target, outputs, seg.Columns, seg.Rows, func(t, s int) *RasterMask {
		return out[t].Mask(v.z[s])
	})
	return out
}

func labelSegmentation(seg *Segmentation3D, connectivity Connectivity3D) (*runVolume, []int, int, error) {
	if err := connectivity.validate(); err != nil {
		return nil, nil, 0, err
	}
	if seg == nil {
		return nil, nil, 0, nil
	}
	v := segmentationRunVolume(seg)
	labels, count := v.label(connectivity.adjacency())
	return v, labels, count, nil
}

// ConnectedComponents labels the set pixels of a mask into connected regions
// (8-connected when eightConnected, else 4-connected), returned in row-major
// discovery order: components are ordered by their first pixel in row-major
// order. It runs a union-find over the mask's runs, in time linear in the
// number of runs.
func ConnectedComponents(mask *RasterMask, eightConnected bool) []*RasterMask {
	if mask == nil {
		return nil
	}
	v := newRunVolume([]int{0}, []*RasterMask{mask})
	adj := runAdjacency{}
	if eightConnected {
		adj.inPlane = 1
	}
	labels, count := v.label(adj)
	if count == 0 {
		return nil
	}
	out := make([]*RasterMask, count)
	for i := range out {
		out[i] = NewRasterMask(mask.Columns, mask.Rows)
	}
	v.emit(labels, nil, count, mask.Columns, mask.Rows, func(t, _ int) *RasterMask { return out[t] })
	return out
}

// ConnectedComponents3D labels the set voxels of a segmentation into 6-connected
// components, each returned as its own Segmentation3D carrying the same geometry.
// Components are ordered by their first voxel in (slice, row, column) order;
// slices are adjacent only when their indices differ by one. Voxels of masks
// larger than the segmentation still connect, but are clipped from the output.
// The labelling is a union-find over the per-row runs (see
// ConnectedComponents3DWithConnectivity).
func ConnectedComponents3D(seg *Segmentation3D) []*Segmentation3D {
	comps, _ := ConnectedComponents3DWithConnectivity(seg, Connectivity6)
	return comps
}

// ConnectedComponents3DWithConnectivity is ConnectedComponents3D with a
// selectable 6-, 18- or 26-connectivity. Labelling is linear in the number of
// runs; the returned segmentations themselves still hold one row table per
// component and slice, so on noisy masks with many islands prefer
// ConnectedComponent3DAt, LargestConnectedComponents3D or
// RemoveSmallConnectedComponents3D, which return a single segmentation.
func ConnectedComponents3DWithConnectivity(seg *Segmentation3D, connectivity Connectivity3D) ([]*Segmentation3D, error) {
	v, labels, count, err := labelSegmentation(seg, connectivity)
	if err != nil || count == 0 {
		return nil, err
	}
	return v.segmentations(seg, labels, nil, count), nil
}

// ConnectedComponent3DAt returns the component of seg that contains voxel
// (x, y, slice) under the given connectivity, or nil when that voxel is not
// set. It equals the matching element of ConnectedComponents3DWithConnectivity
// without building the other components.
func ConnectedComponent3DAt(seg *Segmentation3D, x, y, slice int, connectivity Connectivity3D) (*Segmentation3D, error) {
	v, labels, count, err := labelSegmentation(seg, connectivity)
	if err != nil || count == 0 {
		return nil, err
	}
	s := sort.SearchInts(v.z, slice)
	if s == len(v.z) || v.z[s] != slice {
		return nil, nil
	}
	runs, base := v.row(s, y)
	i := sort.Search(len(runs), func(i int) bool { return runs[i].End > x })
	if i == len(runs) || runs[i].Start > x || x < 0 || x >= seg.Columns || y >= seg.Rows {
		return nil, nil
	}
	return v.selectComponents(seg, labels, count, func(label int) bool { return label == labels[base+i] }), nil
}

// LargestConnectedComponents3D returns one segmentation holding the n largest
// components (by voxel count; equal sizes keep the earlier component in
// ConnectedComponents3D order). n <= 0 returns an empty segmentation.
func LargestConnectedComponents3D(seg *Segmentation3D, n int, connectivity Connectivity3D) (*Segmentation3D, error) {
	v, labels, count, err := labelSegmentation(seg, connectivity)
	if err != nil || seg == nil {
		return nil, err
	}
	sizes := v.componentMeasure(seg, labels, count, func(int) float64 { return 1 })
	order := make([]int, count)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return sizes[order[a]] > sizes[order[b]] })
	keep := make([]bool, count)
	for i := 0; i < n && i < count; i++ {
		keep[order[i]] = true
	}
	return v.selectComponents(seg, labels, count, func(label int) bool { return keep[label] }), nil
}

// RemoveSmallConnectedComponents3D returns one segmentation without the
// components whose volume is below minVolumeMM3. Volume uses the same
// per-slice patient-space spacing and thickness as Statistics().VolumeMM3.
func RemoveSmallConnectedComponents3D(seg *Segmentation3D, minVolumeMM3 float64, connectivity Connectivity3D) (*Segmentation3D, error) {
	v, labels, count, err := labelSegmentation(seg, connectivity)
	if err != nil || seg == nil {
		return nil, err
	}
	volumes := v.componentMeasure(seg, labels, count, func(slice int) float64 {
		colSp, rowSp := sliceInPlaneSpacing(seg.Geometry, slice)
		return colSp * rowSp * sliceThickness(seg.Geometry, slice)
	})
	return v.selectComponents(seg, labels, count, func(label int) bool { return volumes[label] >= minVolumeMM3 }), nil
}

// componentMeasure sums perVoxel(slice) over the in-bounds voxels of every
// component.
func (v *runVolume) componentMeasure(seg *Segmentation3D, labels []int, count int, perVoxel func(slice int) float64) []float64 {
	out := make([]float64, count)
	for s, mask := range v.masks {
		weight := perVoxel(v.z[s])
		for y := range mask.runs {
			if y >= seg.Rows {
				break
			}
			runs, base := v.row(s, y)
			for i, run := range runs {
				start, end := clampSpan(run.Start, run.End, seg.Columns)
				if start < end {
					out[labels[base+i]] += float64(end-start) * weight
				}
			}
		}
	}
	return out
}

// selectComponents emits the components accepted by keep into a single
// segmentation.
func (v *runVolume) selectComponents(seg *Segmentation3D, labels []int, count int, keep func(label int) bool) *Segmentation3D {
	target := make([]int, count)
	for label := range target {
		target[label] = -1
		if keep(label) {
			target[label] = 0
		}
	}
	out := v.segmentations(seg, labels, target, 1)[0]
	out.Prune()
	return out
}

// FloodFill returns the 4-connected region of pixels reachable from (seedX,seedY)
// for which inside reports true (region growing). It is the basis of the fill
// tool and seeded threshold growing. It is a span fill: memory is proportional
// to the filled region's runs and inside is only evaluated on the region and
// its border.
func FloodFill(columns, rows, seedX, seedY int, inside func(x, y int) bool) *RasterMask {
	mask := NewRasterMask(columns, rows)
	if inside == nil || seedX < 0 || seedY < 0 || seedX >= columns || seedY >= rows || !inside(seedX, seedY) {
		return mask
	}
	f := spanFiller{
		columns: columns, rows: rows, slices: 1,
		inside:     func(x, y, _ int) bool { return inside(x, y) },
		neighbours: fourNeighbourRows,
		masks:      map[int]*RasterMask{0: mask},
	}
	f.fill(seedX, seedY, 0)
	return mask
}

// FloodFill3D grows the region of voxels connected to (seedX, seedY,
// seedSlice) under the given connectivity for which inside reports true, in
// a columns×rows×slices volume. It returns the filled mask of every slice the
// region touches; the result equals thresholding the whole volume with inside
// and keeping the ConnectedComponents3DWithConnectivity component holding the
// seed, but inside is only evaluated on the region and its border. A seed
// outside the volume or for which inside is false yields an empty map.
func FloodFill3D(columns, rows, slices, seedX, seedY, seedSlice int, connectivity Connectivity3D, inside func(x, y, slice int) bool) (map[int]*RasterMask, error) {
	if err := connectivity.validate(); err != nil {
		return nil, err
	}
	if _, err := RasterMaskPixelCount(columns, rows); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidFillVolume, err)
	}
	if slices <= 0 {
		return nil, fmt.Errorf("%w: %d slices", ErrInvalidFillVolume, slices)
	}
	out := map[int]*RasterMask{}
	if inside == nil || seedX < 0 || seedY < 0 || seedSlice < 0 || seedX >= columns || seedY >= rows || seedSlice >= slices || !inside(seedX, seedY, seedSlice) {
		return out, nil
	}
	f := spanFiller{
		columns: columns, rows: rows, slices: slices,
		inside:     inside,
		neighbours: fillNeighbourRows(connectivity),
		masks:      out,
	}
	f.fill(seedX, seedY, seedSlice)
	return out, nil
}

// fillNeighbour is a neighbouring row (dz, dy) and how far a filled span is
// widened into it (1 when diagonal contact counts).
type fillNeighbour struct{ dz, dy, ext int }

var fourNeighbourRows = []fillNeighbour{{0, -1, 0}, {0, 1, 0}}

func fillNeighbourRows(connectivity Connectivity3D) []fillNeighbour {
	adj := connectivity.adjacency()
	out := []fillNeighbour{
		{0, -1, adj.inPlane}, {0, 1, adj.inPlane},
		{-1, 0, adj.throughRow}, {1, 0, adj.throughRow},
	}
	if adj.diagonal {
		for _, dz := range []int{-1, 1} {
			for _, dy := range []int{-1, 1} {
				out = append(out, fillNeighbour{dz, dy, adj.diagonalExt})
			}
		}
	}
	return out
}

type fillCandidate struct{ z, y, x0, x1 int }

type spanFiller struct {
	columns, rows, slices int
	inside                func(x, y, z int) bool
	neighbours            []fillNeighbour
	masks                 map[int]*RasterMask
	stack                 []fillCandidate

	// Optional bounds used by the region growing family: ctx is polled
	// every fillCancelInterval candidates, maxVoxels (> 0) caps the filled
	// voxels and stop, when it reports true for a newly filled span, ends
	// the fill early. The first failure is kept in err.
	ctx       context.Context
	maxVoxels int
	stop      func(z, y, x0, x1 int) bool
	filled    int
	polls     int
	stopped   bool
	err       error
}

const fillCancelInterval = 1024

// fill scans candidate intervals: every unfilled inside pixel starts a
// maximal inside span, which is filled and pushes the widened interval of
// each neighbouring row. Filled pixels are recognised from the output runs.
func (f *spanFiller) fill(seedX, seedY, seedZ int) {
	if f.err != nil || f.stopped {
		return
	}
	f.stack = append(f.stack[:0], fillCandidate{seedZ, seedY, seedX, seedX + 1})
	for len(f.stack) > 0 {
		if f.ctx != nil {
			if f.polls++; f.polls%fillCancelInterval == 0 {
				if err := f.ctx.Err(); err != nil {
					f.err = fmt.Errorf("%w: %w", ErrRegionGrowCancelled, err)
					return
				}
			}
		}
		c := f.stack[len(f.stack)-1]
		f.stack = f.stack[:len(f.stack)-1]
		mask := f.masks[c.z]
		for x := c.x0; x < c.x1; {
			if mask != nil {
				runs := mask.runs[c.y]
				if i := sort.Search(len(runs), func(i int) bool { return runs[i].End > x }); i < len(runs) && runs[i].Start <= x {
					x = runs[i].End
					continue
				}
			}
			if !f.inside(x, c.y, c.z) {
				x++
				continue
			}
			left, right := x, x+1
			for left > 0 && f.inside(left-1, c.y, c.z) {
				left--
			}
			for right < f.columns && f.inside(right, c.y, c.z) {
				right++
			}
			if mask == nil {
				mask = NewRasterMask(f.columns, f.rows)
				f.masks[c.z] = mask
			}
			mask.runs[c.y] = insertRun(mask.runs[c.y], left, right)
			f.filled += right - left
			if f.maxVoxels > 0 && f.filled > f.maxVoxels {
				f.err = fmt.Errorf("%w: region exceeds %d voxels", ErrRegionGrowResourceLimit, f.maxVoxels)
				return
			}
			if f.stop != nil && f.stop(c.z, c.y, left, right) {
				f.stopped = true
				return
			}
			for _, n := range f.neighbours {
				z, y := c.z+n.dz, c.y+n.dy
				if z < 0 || z >= f.slices || y < 0 || y >= f.rows {
					continue
				}
				f.stack = append(f.stack, fillCandidate{z, y, max(left-n.ext, 0), min(right+n.ext, f.columns)})
			}
			x = right + 1
		}
	}
}

// insertRun adds [x0,x1) to a sorted run list in place, merging with touching
// runs (addRun without reallocating for the common disjoint case).
func insertRun(runs []MaskRun, x0, x1 int) []MaskRun {
	i := sort.Search(len(runs), func(i int) bool { return runs[i].End >= x0 })
	if i < len(runs) && runs[i].Start <= x1 {
		return addRun(runs, x0, x1)
	}
	runs = append(runs, MaskRun{})
	copy(runs[i+1:], runs[i:])
	runs[i] = MaskRun{Start: x0, End: x1}
	return runs
}
