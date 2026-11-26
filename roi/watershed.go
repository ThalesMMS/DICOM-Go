package roi

import (
	"context"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/imagefilter"
)

// Marker-controlled morphological watershed (#1105).
//
// Ported from ITK (Insight Toolkit), commit
// 35b196e13c366e0386389a71c936a0436c568cea, Apache-2.0, Copyright NumFOCUS
// (see THIRD_PARTY_NOTICES.md and testdata/upstream/itk/LICENSE.ITK, which
// holds ITK's LICENSE and NOTICE): WatershedFromMarkers follows
// itkMorphologicalWatershedFromMarkersImageFilter.hxx, with Beucher's
// flooding when no watershed line is marked and Meyer's when it is. Both
// flood the relief from the markers through a hierarchical queue: the
// voxel of lowest level is taken first, voxels of equal level in the order
// they were queued, and a voxel not above the level being flooded joins the
// current level.
//
// Modifications: rewritten in Go over imagefilter.Volume and LabelVolume;
// ITK's map of per-level FIFO queues becomes one binary heap ordered by
// (level, insertion sequence), which pops in the same order; the 6/18/26
// connectivity of roi.Connectivity3D replaces ITK's FullyConnected flag; an
// optional mask confines the flood (voxels outside it are never labelled);
// the result reports the voxels no marker reached and the watershed-line
// voxels; voxel budgets and context cancellation bound the call.

// WatershedOptions configures WatershedFromMarkers.
type WatershedOptions struct {
	// Connectivity is the flooding neighbourhood: 6 (the zero value, ITK's
	// default), 18 or 26 (ITK's FullyConnected).
	Connectivity Connectivity3D
	// MarkWatershedLine leaves the voxels where two basins meet at label 0
	// (Meyer's algorithm). Without it every reached voxel joins a basin
	// (Beucher's algorithm), so touching regions stay in contact.
	MarkWatershedLine bool
	// Mask, when set, confines the flood to its set voxels; it must have the
	// relief's dimensions and hold every marker voxel.
	Mask *VolumeMask
	// MaxVoxels bounds the image size; 0 means DefaultMaxMorphologyVoxels.
	MaxVoxels int
}

// WatershedResult is the labelled flood.
type WatershedResult struct {
	// Labels holds, on the markers' grid, the marker label of every basin
	// voxel and 0 for the background, watershed lines and unreached voxels.
	Labels *LabelVolume
	// LineVoxels counts the watershed-line voxels (MarkWatershedLine only).
	LineVoxels int
	// Unreached counts the voxels (inside Mask when set) that no marker's
	// flood reached, for example a mask component without a marker.
	Unreached int
}

// WatershedFromMarkers floods relief from markers (label 0 = no marker) and
// returns each voxel's basin. Low relief values flood first, so the basins
// grow along valleys and meet on the ridges; to split bright objects, flood
// the negated image (or the negated distance map, see SplitTouchingObjects).
// markers.Grid must have relief's dimensions; it is copied to the result.
func WatershedFromMarkers(ctx context.Context, relief *imagefilter.Volume, markers *LabelVolume, opts WatershedOptions) (*WatershedResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	gopts, err := GrayscaleOptions{Connectivity: opts.Connectivity, MaxVoxels: opts.MaxVoxels}.normalize()
	if err != nil {
		return nil, err
	}
	if err := checkMorphologyVolume(relief, gopts.MaxVoxels, "relief"); err != nil {
		return nil, err
	}
	dims := relief.Dims
	if markers == nil || markers.Grid.Columns != dims[0] || markers.Grid.Rows != dims[1] ||
		markers.Grid.Slices != dims[2] || len(markers.Labels) != len(relief.Data) {
		return nil, fmt.Errorf("%w: markers do not match the relief dimensions %v", ErrInvalidMorphology, dims)
	}
	var blocked []bool
	if opts.Mask != nil {
		if err := opts.Mask.validate(); err != nil {
			return nil, fmt.Errorf("%w: mask: %w", ErrInvalidMorphology, err)
		}
		if opts.Mask.Columns != dims[0] || opts.Mask.Rows != dims[1] || opts.Mask.Slices != dims[2] {
			return nil, fmt.Errorf("%w: mask dimensions differ from the relief %v", ErrInvalidMorphology, dims)
		}
		blocked = make([]bool, len(relief.Data))
		for i, in := range opts.Mask.Voxels {
			if !in {
				if markers.Labels[i] != 0 {
					return nil, fmt.Errorf("%w: marker voxel %d lies outside the mask", ErrInvalidMorphology, i)
				}
				blocked[i] = true
			}
		}
	}
	w := watershed{
		nb: newNeighbourhood(dims, gopts.Connectivity), level: relief.Data, blocked: blocked,
		labels: append([]uint16(nil), markers.Labels...),
	}
	result := &WatershedResult{Labels: &LabelVolume{Grid: markers.Grid, Labels: w.labels}}
	if opts.MarkWatershedLine {
		if err := w.meyer(ctx, markers.Labels); err != nil {
			return nil, err
		}
		for i, done := range w.status {
			switch {
			case w.isBlocked(i):
			case !done:
				result.Unreached++
			case w.labels[i] == 0:
				result.LineVoxels++
			}
		}
		return result, nil
	}
	if err := w.beucher(ctx); err != nil {
		return nil, err
	}
	for i, l := range w.labels {
		if l == 0 && !w.isBlocked(i) {
			result.Unreached++
		}
	}
	return result, nil
}

type watershed struct {
	nb      neighbourhood
	level   []float32
	blocked []bool
	labels  []uint16
	status  []bool // Meyer: queued or processed
	queue   floodQueue
}

func (w *watershed) isBlocked(i int) bool { return w.blocked != nil && w.blocked[i] }

// beucher floods without watershed lines: every marker voxel with an
// unlabelled neighbour seeds the queue, and a popped voxel gives its label
// to all its unlabelled neighbours.
func (w *watershed) beucher(ctx context.Context) error {
	nb := &w.nb
	for i, l := range w.labels {
		if l == 0 {
			continue
		}
		x, y, z := nb.coords(i)
		for _, o := range nb.all {
			if q := i + o.step; nb.inside(x, y, z, o) && w.labels[q] == 0 && !w.isBlocked(q) {
				w.queue.push(w.level[i], i)
				break
			}
		}
	}
	for steps := 0; len(w.queue.items) > 0; steps++ {
		if steps&0xffff == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		current, p := w.queue.pop()
		label := w.labels[p]
		x, y, z := nb.coords(p)
		for _, o := range nb.all {
			q := p + o.step
			if !nb.inside(x, y, z, o) || w.labels[q] != 0 || w.isBlocked(q) {
				continue
			}
			w.labels[q] = label
			w.queue.push(max(w.level[q], current), q)
		}
	}
	return nil
}

// meyer floods with watershed lines: the neighbours of the markers seed the
// queue, and a popped voxel takes the label of its labelled neighbours only
// when they all agree; otherwise it stays 0 (a line) and does not spread.
func (w *watershed) meyer(ctx context.Context, markers []uint16) error {
	nb := &w.nb
	w.status = make([]bool, len(w.labels))
	for i := range w.status {
		if w.isBlocked(i) || markers[i] != 0 {
			w.status[i] = true
		}
	}
	for i, l := range markers {
		if l == 0 {
			continue
		}
		x, y, z := nb.coords(i)
		for _, o := range nb.all {
			if q := i + o.step; nb.inside(x, y, z, o) && !w.status[q] {
				w.status[q] = true
				w.queue.push(w.level[q], q)
			}
		}
	}
	for steps := 0; len(w.queue.items) > 0; steps++ {
		if steps&0xffff == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		current, p := w.queue.pop()
		x, y, z := nb.coords(p)
		var label uint16
		collision := false
		for _, o := range nb.all {
			if !nb.inside(x, y, z, o) {
				continue
			}
			if l := w.labels[p+o.step]; l != 0 {
				if label != 0 && l != label {
					collision = true
					break
				}
				label = l
			}
		}
		if collision || label == 0 {
			continue
		}
		w.labels[p] = label
		for _, o := range nb.all {
			if q := p + o.step; nb.inside(x, y, z, o) && !w.status[q] {
				w.status[q] = true
				w.queue.push(max(w.level[q], current), q)
			}
		}
	}
	return nil
}

// floodItem is one queued voxel: its flooding level and its insertion
// sequence, which keeps equal levels first in, first out.
type floodItem struct {
	level float32
	seq   uint32
	index int32
}

// floodQueue is a binary min-heap of flood items by (level, seq).
type floodQueue struct {
	items []floodItem
	seq   uint32
}

func (a floodItem) before(b floodItem) bool {
	return a.level < b.level || a.level == b.level && a.seq < b.seq
}

func (q *floodQueue) push(level float32, index int) {
	item := floodItem{level: level, seq: q.seq, index: int32(index)}
	q.seq++
	q.items = append(q.items, item)
	i := len(q.items) - 1
	for i > 0 {
		parent := (i - 1) / 2
		if !item.before(q.items[parent]) {
			break
		}
		q.items[i] = q.items[parent]
		i = parent
	}
	q.items[i] = item
}

func (q *floodQueue) pop() (float32, int) {
	top := q.items[0]
	last := q.items[len(q.items)-1]
	q.items = q.items[:len(q.items)-1]
	n := len(q.items)
	i := 0
	for n > 0 {
		child := 2*i + 1
		if child >= n {
			break
		}
		if right := child + 1; right < n && q.items[right].before(q.items[child]) {
			child = right
		}
		if !q.items[child].before(last) {
			break
		}
		q.items[i] = q.items[child]
		i = child
	}
	if n > 0 {
		q.items[i] = last
	}
	return top.level, int(top.index)
}

// ErrTooManyLabels reports more connected components than a 16-bit label
// volume can number.
var ErrTooManyLabels = fmt.Errorf("%w: more than %d components", ErrInvalidMorphology, math.MaxUint16)

// LabelComponents numbers the connected components of mask 1, 2, … in scan
// order (slice, row, column of each component's first voxel) on a label
// volume on grid, which must have the mask's dimensions: the usual way to
// turn binary markers (for example ExtendedMaxima) into watershed markers.
// It returns the volume and the component count, or ErrTooManyLabels
// beyond 65535 components. The labelling reuses the run-based union-find of
// ConnectedComponents3D.
func LabelComponents(ctx context.Context, grid Grid, mask *VolumeMask, connectivity Connectivity3D) (*LabelVolume, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if connectivity == 0 {
		connectivity = Connectivity6
	}
	if err := connectivity.validate(); err != nil {
		return nil, 0, err
	}
	if err := mask.validate(); err != nil {
		return nil, 0, err
	}
	if grid.Columns != mask.Columns || grid.Rows != mask.Rows || grid.Slices != mask.Slices {
		return nil, 0, fmt.Errorf("%w: grid %dx%dx%d, mask %dx%dx%d", ErrInvalidMorphology,
			grid.Columns, grid.Rows, grid.Slices, mask.Columns, mask.Rows, mask.Slices)
	}
	if err := grid.Validate(); err != nil {
		return nil, 0, err
	}
	nx, ny := mask.Columns, mask.Rows
	z := make([]int, mask.Slices)
	masks := make([]*RasterMask, mask.Slices)
	for s := range masks {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		z[s] = s
		masks[s] = NewRasterMask(nx, ny)
		for y := 0; y < ny; y++ {
			row := mask.Voxels[(s*ny+y)*nx : (s*ny+y+1)*nx]
			start := -1
			for x := 0; x <= nx; x++ {
				on := x < nx && row[x]
				if on && start < 0 {
					start = x
				} else if !on && start >= 0 {
					masks[s].SetRun(y, start, x)
					start = -1
				}
			}
		}
	}
	runs := newRunVolume(z, masks)
	labels, count := runs.label(connectivity.adjacency())
	if count > math.MaxUint16 {
		return nil, 0, ErrTooManyLabels
	}
	out := &LabelVolume{Grid: grid, Labels: make([]uint16, len(mask.Voxels))}
	for s := range masks {
		for y := 0; y < ny; y++ {
			row, base := runs.row(s, y)
			for i, run := range row {
				l := uint16(labels[base+i] + 1)
				values := out.Labels[(s*ny+y)*nx : (s*ny+y+1)*nx]
				for x := run.Start; x < run.End; x++ {
					values[x] = l
				}
			}
		}
	}
	return out, count, ctx.Err()
}
