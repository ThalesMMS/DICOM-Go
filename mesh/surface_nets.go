package mesh

import (
	"context"
	"fmt"
	"math/bits"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
	"github.com/ThalesMMS/dicom-go/render"
)

// Multi-label surfaces by SurfaceNets (#1013), after VTK's vtkSurfaceNets3D
// (Schroeder et al., arXiv 2401.14906) and Frisken's SurfaceNets: a vertex
// in every dual cell (the cube between eight voxel centres) whose voxels do
// not all carry one label, and one quad across every voxel face between two
// different labels, joining the vertices of the four cells around it. Every
// boundary between two labels is built once and shared by both, so adjacent
// segments meet without gaps or overlaps. A cell where voxels of one label
// touch only along an edge or at a corner gets one vertex per fan of quads
// around its centre (surface_nets_cells.go), so each label's surface stays a
// manifold (#2026). Written from the published description; no VTK code is
// copied.

// LabelField is a label volume: one label per voxel, 0 for the background,
// read a slice at a time.
type LabelField interface {
	// Dims is the number of samples along x, y and z.
	Dims() [3]int
	// IndexToPatient maps a voxel index to patient LPS millimetres.
	IndexToPatient() render.GeometryAffine
	// ReadLabels fills dst (Dims[0]*Dims[1] labels, x fastest) with slice z.
	// ExtractLabels calls it from several goroutines at once, for different
	// or equal z, so implementations must be safe for concurrent use.
	ReadLabels(ctx context.Context, z int, dst []uint32) error
}

// DenseLabelField is an in-memory label volume, x fastest then y then z.
type DenseLabelField struct {
	Size   [3]int
	Affine render.GeometryAffine
	Labels []uint32
}

func (f *DenseLabelField) Dims() [3]int                          { return f.Size }
func (f *DenseLabelField) IndexToPatient() render.GeometryAffine { return f.Affine }

func (f *DenseLabelField) ReadLabels(_ context.Context, z int, dst []uint32) error {
	n := f.Size[0] * f.Size[1]
	if z < 0 || z >= f.Size[2] || len(dst) < n || len(f.Labels) < (z+1)*n {
		return fmt.Errorf("%w: label slice %d of %v", ErrInvalidField, z, f.Size)
	}
	copy(dst, f.Labels[z*n:(z+1)*n])
	return nil
}

// DefaultSurfaceNetsIterations is the smoothing when
// SurfaceNetsOptions.Iterations is zero.
const DefaultSurfaceNetsIterations = 16

// SurfaceNetsOptions configures ExtractLabels.
type SurfaceNetsOptions struct {
	// Labels restricts the surfaces to these labels, the others reading as
	// background; empty keeps every non-zero label.
	Labels []uint32
	// Iterations of constrained smoothing (zero uses
	// DefaultSurfaceNetsIterations, negative disables it). Without smoothing
	// the vertices are the voxel corners and every label's surface encloses
	// exactly its voxels.
	Iterations int
	// Relaxation is the fraction of the way to the mean of its neighbours a
	// vertex moves per iteration, in (0, 1]; zero uses 0.5.
	Relaxation float64
	// MaxTriangles and MaxBytes bound the output as it is built. Zero uses
	// the defaults in Options, and negative disables the corresponding
	// limit. MaxBytes includes triangle label pairs and the list of labels.
	MaxTriangles int64
	MaxBytes     int64
	// Workers bounds the goroutines building the surface, as
	// Options.Workers (0 uses min(GOMAXPROCS, DefaultMaxWorkers)); the
	// surface does not depend on it.
	Workers int
}

// LabelSurface is the multi-label surface of a label volume.
type LabelSurface struct {
	// Mesh holds the shared vertices in patient LPS millimetres and every
	// boundary once; its Normals are nil because a shared boundary faces
	// both of its labels (LabelMesh gives one label's oriented surface).
	Mesh *Mesh
	// Pairs[i] is {lower, higher} label of triangle i; the triangle winds
	// counter-clockwise seen from the lower label, so its right-hand normal
	// points out of the higher label (out of every segment into the
	// background, label 0).
	Pairs [][2]uint32
	// Labels lists the non-zero labels with a surface, ascending.
	Labels []uint32
	// UnresolvedCells counts the dual cells where some label's surface is
	// still not a manifold: a vertex around which the label's triangles form
	// more than one fan, or an edge four of its triangles share. Voxels of
	// one label that touch only along an edge or at a corner get separate
	// vertices, so this is zero for a single label (or any labels that do not
	// touch one another); it counts the junctions of three or more labels
	// where two parts of one label are joined only through the boundaries of
	// the others. Each label's surface remains closed and consistently wound.
	UnresolvedCells int
}

// LabelMesh returns the closed surface of one label, with its triangles
// wound counter-clockwise seen from outside (outward right-hand normals, the
// Mesh convention) and area-weighted vertex normals. Vertices shared with
// other labels are copied, so the result stands alone. Nil when the label
// has no surface.
func (s *LabelSurface) LabelMesh(label uint32) *Mesh {
	if s == nil || s.Mesh == nil || label == 0 {
		return nil
	}
	remap := make([]int32, len(s.Mesh.Vertices))
	for i := range remap {
		remap[i] = -1
	}
	out := &Mesh{}
	vertex := func(i uint32) uint32 {
		if remap[i] < 0 {
			remap[i] = int32(len(out.Vertices))
			out.Vertices = append(out.Vertices, s.Mesh.Vertices[i])
		}
		return uint32(remap[i])
	}
	for i, pair := range s.Pairs {
		t := s.Mesh.Triangles[i]
		switch label {
		case pair[1]:
			out.Triangles = append(out.Triangles, [3]uint32{vertex(t[0]), vertex(t[1]), vertex(t[2])})
		case pair[0]:
			out.Triangles = append(out.Triangles, [3]uint32{vertex(t[0]), vertex(t[2]), vertex(t[1])})
		}
	}
	if len(out.Triangles) == 0 {
		return nil
	}
	out.Normals = make([][3]float32, len(out.Vertices))
	faceNormals(out, nil)
	return out
}

// ExtractLabels builds the SurfaceNets surface of every label of field (see
// SurfaceNetsOptions.Labels). The grid is padded with background, so
// surfaces touching the boundary close half a voxel outside it. The field
// is read slice by slice by up to SurfaceNetsOptions.Workers goroutines,
// each visiting only the cells of a row between its non-zero voxels; the
// vertex and quad arrays grow with the surface.
func ExtractLabels(ctx context.Context, field LabelField, opts SurfaceNetsOptions) (*LabelSurface, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if field == nil {
		return nil, fmt.Errorf("%w: nil label field", ErrInvalidField)
	}
	dims := field.Dims()
	if err := validateFieldDimensions(dims); err != nil {
		return nil, err
	}
	frame, err := newAffineFrame(field.IndexToPatient())
	if err != nil {
		return nil, err
	}
	relaxation := opts.Relaxation
	if relaxation == 0 {
		relaxation = 0.5
	}
	if !(relaxation > 0 && relaxation <= 1) {
		return nil, fmt.Errorf("%w: relaxation %v", ErrInvalidField, opts.Relaxation)
	}
	iterations := opts.Iterations
	if iterations == 0 {
		iterations = DefaultSurfaceNetsIterations
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = min(parallel.DefaultWorkers(), DefaultMaxWorkers)
	}
	nets := newSurfaceNets(dims, opts)
	if err := nets.build(ctx, field, workers); err != nil {
		return nil, err
	}
	if err := nets.smooth(ctx, max(iterations, 0), relaxation); err != nil {
		return nil, err
	}
	surface := &LabelSurface{
		Mesh:            &Mesh{Vertices: make([][3]float32, len(nets.position)), Triangles: nets.triangles},
		Pairs:           nets.pairs,
		Labels:          nets.labelList(),
		UnresolvedCells: nets.unresolved,
	}
	for i, p := range nets.position {
		surface.Mesh.Vertices[i] = frame.point(p[0], p[1], p[2])
	}
	if frame.det < 0 {
		// A left-handed index-to-patient frame mirrors the winding.
		for i, t := range surface.Mesh.Triangles {
			surface.Mesh.Triangles[i] = [3]uint32{t[0], t[2], t[1]}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return surface, nil
}

type surfaceNets struct {
	nx, ny, nz int
	opts       SurfaceNetsOptions
	keep       map[uint32]bool
	// position is each vertex in voxel index coordinates, cell its dual
	// cell's lowest voxel and kind the number of labels around its fan.
	position   [][3]float64
	cell       [][3]int32
	kind       []uint8
	quads      [][4]uint32
	triangles  [][3]uint32
	pairs      [][2]uint32
	seen       map[uint32]bool
	unresolved int
}

func newSurfaceNets(dims [3]int, opts SurfaceNetsOptions) *surfaceNets {
	n := &surfaceNets{nx: dims[0], ny: dims[1], nz: dims[2], opts: opts, seen: map[uint32]bool{}}
	if len(opts.Labels) != 0 {
		n.keep = map[uint32]bool{}
		for _, label := range opts.Labels {
			n.keep[label] = true
		}
	}
	return n
}

func (n *surfaceNets) labelList() []uint32 {
	labels := make([]uint32, 0, len(n.seen))
	for label := range n.seen {
		labels = append(labels, label)
	}
	slices.Sort(labels)
	return labels
}

// build constructs the dual cell planes k = -1 .. nz-1 (cell k spans voxel
// slices k and k+1): the vertices of plane k, then the quads of the voxel
// edges whose four cells are known. Contiguous runs of planes are built in
// parallel, each plane into its own output with vertices numbered within
// the plane, and the outputs are joined in plane order, so the surface is
// the same for any number of workers.
func (n *surfaceNets) build(ctx context.Context, field LabelField, workers int) error {
	planes := make([]netPlane, n.nz+1)
	budgetOptions := Options{MaxTriangles: n.opts.MaxTriangles, MaxBytes: n.opts.MaxBytes}
	var (
		vertices, triangles atomic.Int64
		mu                  sync.Mutex
	)
	err := forEachSlab(ctx, len(planes), workers, func() slabFunc {
		w := newNetWorker(n, field)
		return func(ctx context.Context, p0, p1 int) error {
			defer func() {
				mu.Lock()
				for label := range w.seen {
					n.seen[label] = true
				}
				mu.Unlock()
			}()
			// Plane p is k = p-1; its quads need the vertices of plane p-1.
			if p0 > 0 {
				if err := w.plane(ctx, p0-2, nil); err != nil {
					return err
				}
			}
			for p := p0; p < p1; p++ {
				if err := ctx.Err(); err != nil {
					return err
				}
				out := &planes[p]
				if err := w.plane(ctx, p-1, out); err != nil {
					return err
				}
				// The label list is counted once the planes are joined.
				v := vertices.Add(int64(len(out.cells)))
				t := triangles.Add(2 * int64(len(out.quads)))
				if err := checkOutputBudget(budgetOptions, v, t, meshBytes(v, t, false)+8*t); err != nil {
					return err
				}
			}
			return nil
		}
	})
	if err != nil {
		return err
	}
	v, t := vertices.Load(), triangles.Load()
	if err := checkOutputBudget(budgetOptions, v, t, meshBytes(v, t, false)+8*t+4*int64(len(n.seen))); err != nil {
		return err
	}
	n.join(planes, int(v), int(t/2))
	return ctx.Err()
}

// netPlane is the output of one dual cell plane: the cells (i, j) of its
// vertices and their kinds, its quads with corners numbered within the
// plane (or within the previous plane where bit r of previous is set), the
// lower and higher label of each quad, and its non-manifold cells.
type netPlane struct {
	cells    [][2]int32
	kind     []uint8
	quads    [][4]uint32
	previous []uint8
	pairs    [][2]uint32
	defects  []netDefect
}

type netDefect struct {
	i, j int32
	fans cellFans
}

// quad adds the face between voxel a and its neighbour b along the positive
// axis; v0..v3 wind counter-clockwise about that axis, so the right-hand
// normal points from a into b. It is flipped to point out of the higher
// label.
func (p *netPlane) quad(a, b uint32, v [4]uint32, previous uint8) {
	if a < b {
		v[1], v[3] = v[3], v[1]
		previous = previous&0b0101 | previous>>2&0b0010 | previous<<2&0b1000
	}
	p.quads = append(p.quads, v)
	p.previous = append(p.previous, previous)
	p.pairs = append(p.pairs, [2]uint32{min(a, b), max(a, b)})
}

// join numbers the vertices of the planes in order and gathers their
// vertices and quads.
func (n *surfaceNets) join(planes []netPlane, vertices, quads int) {
	n.position = make([][3]float64, 0, vertices)
	n.cell = make([][3]int32, 0, vertices)
	n.kind = make([]uint8, 0, vertices)
	n.quads = make([][4]uint32, 0, quads)
	n.triangles = make([][3]uint32, 0, 2*quads)
	n.pairs = make([][2]uint32, 0, 2*quads)
	var defects cellDefects
	var base, previousBase uint32
	for p := range planes {
		plane := &planes[p]
		k := int32(p - 1)
		for i, c := range plane.cells {
			n.position = append(n.position, [3]float64{float64(c[0]) + 0.5, float64(c[1]) + 0.5, float64(k) + 0.5})
			n.cell = append(n.cell, [3]int32{c[0], c[1], k})
			n.kind = append(n.kind, plane.kind[i])
		}
		for q, local := range plane.quads {
			var v [4]uint32
			for r := range v {
				if plane.previous[q]>>r&1 != 0 {
					v[r] = previousBase + local[r]
				} else {
					v[r] = base + local[r]
				}
			}
			n.quads = append(n.quads, v)
			n.triangles = append(n.triangles, [3]uint32{v[0], v[1], v[2]}, [3]uint32{v[0], v[2], v[3]})
			n.pairs = append(n.pairs, plane.pairs[q], plane.pairs[q])
		}
		for _, d := range plane.defects {
			defects.add(int(d.i), int(d.j), int(k), d.fans)
		}
		previousBase, base = base, base+uint32(len(plane.cells))
		*plane = netPlane{}
	}
	n.unresolved = defects.count()
}

// rowSpan bounds the non-zero voxels of a row: first is one before the
// first, last the last (first > last for an empty row). The dual cells i
// in [first, last] of a row are the only ones that can hold a vertex, as
// in vtkSurfaceNets3D's trimming: the rows are padded with background, so
// the cells beyond the non-zero voxels of their four rows are background.
type rowSpan struct{ first, last int }

// netWorker holds one worker's padded label slices, their row spans and
// the vertex numbering of its last two planes.
type netWorker struct {
	n      *surfaceNets
	field  LabelField
	raw    []uint32
	labels [4][]uint32
	spans  [4][]rowSpan
	loaded [4]int
	// ids holds the first vertex (within its plane) of each cell of the
	// last two planes and fans the fan of each of its cube edges.
	ids  [2][]uint32
	fans [2][]uint64
	seen map[uint32]bool
}

func newNetWorker(n *surfaceNets, field LabelField) *netWorker {
	px, py := n.nx+2, n.ny+2
	cx, cy := n.nx+1, n.ny+1
	w := &netWorker{n: n, field: field, raw: make([]uint32, n.nx*n.ny), loaded: [4]int{-3, -3, -3, -3}, seen: map[uint32]bool{}}
	for i := range w.labels {
		w.labels[i] = make([]uint32, px*py)
		w.spans[i] = make([]rowSpan, n.ny)
	}
	for i := range w.ids {
		w.ids[i] = make([]uint32, cx*cy)
		w.fans[i] = make([]uint64, cx*cy)
	}
	return w
}

// slice loads voxel slice z into slot z&3, with a background border;
// slices -1 and nz are all background.
func (w *netWorker) slice(ctx context.Context, z int) ([]uint32, []rowSpan, error) {
	n := w.n
	slot := z & 3
	buffer, spans := w.labels[slot], w.spans[slot]
	if w.loaded[slot] == z {
		return buffer, spans, nil
	}
	w.loaded[slot] = -3
	clear(buffer)
	for y := range spans {
		spans[y] = rowSpan{n.nx, -2}
	}
	if z >= 0 && z < n.nz {
		if err := w.field.ReadLabels(ctx, z, w.raw); err != nil {
			return nil, nil, err
		}
		px := n.nx + 2
		for y := 0; y < n.ny; y++ {
			row := buffer[(y+1)*px+1 : (y+1)*px+1+n.nx]
			first, last := -1, -1
			for x, label := range w.raw[y*n.nx : (y+1)*n.nx] {
				if n.keep != nil && !n.keep[label] {
					label = 0
				}
				row[x] = label
				if label != 0 {
					w.seen[label] = true
					if first < 0 {
						first = x
					}
					last = x
				}
			}
			if first >= 0 {
				spans[y] = rowSpan{first - 1, last}
			}
		}
	}
	w.loaded[slot] = z
	return buffer, spans, nil
}

// plane numbers the vertices of cell plane k, and with out also records
// them and the quads of the voxel edges whose cells it completes.
func (w *netWorker) plane(ctx context.Context, k int, out *netPlane) error {
	n := w.n
	px, cx := n.nx+2, n.nx+1
	var layers [4][]uint32
	var spans [4][]rowSpan
	for i := range layers {
		var err error
		if layers[i], spans[i], err = w.slice(ctx, k-1+i); err != nil {
			return err
		}
	}
	lo, hi := layers[1], layers[2]
	// span is the row span of padded row y of layer 1 or 2.
	span := func(layer, y int) rowSpan {
		if y < 0 || y >= n.ny {
			return rowSpan{n.nx, -2}
		}
		return spans[layer][y]
	}
	// voxel reads padded voxel (x, y, k-1+layer), background beyond.
	voxel := func(x, y, layer int) uint32 {
		if x < -1 || x > n.nx || y < -1 || y > n.ny {
			return 0
		}
		return layers[layer][(y+1)*px+x+1]
	}
	ids, fans := w.ids[(k+1)&1], w.fans[(k+1)&1]
	previousIDs, previousFans := w.ids[k&1], w.fans[k&1]
	// Vertices of cell plane k: cell (i, j) holds voxels i..i+1, j..j+1
	// of slices k and k+1, at padded offsets i+1..i+2, j+1..j+2. Cells
	// outside the row spans are background and keep stale ids: no quad
	// uses them.
	var next uint32
	for j := -1; j < n.ny; j++ {
		a, b, c, d := span(1, j), span(1, j+1), span(2, j), span(2, j+1)
		for i := min(a.first, b.first, c.first, d.first); i <= max(a.last, b.last, c.last, d.last); i++ {
			o := (j+1)*px + i + 1
			corners := [8]uint32{lo[o], lo[o+1], lo[o+px], lo[o+px+1], hi[o], hi[o+1], hi[o+px], hi[o+px+1]}
			if uniform(&corners) {
				continue
			}
			fan := analyzeCell(&corners, func(f int) [4]uint32 {
				return outerLayer(&netSquares[f], voxel, i, j)
			})
			cell := (j+1)*cx + i + 1
			ids[cell], fans[cell] = next, fan.fan
			next += uint32(fan.count)
			if out == nil {
				continue
			}
			for g := 0; g < fan.count; g++ {
				out.cells = append(out.cells, [2]int32{int32(i), int32(j)})
				out.kind = append(out.kind, fan.kind[g])
			}
			if fan.tangled || fan.pinched != 0 {
				out.defects = append(out.defects, netDefect{int32(i), int32(j), fan})
			}
		}
	}
	if out == nil {
		return nil
	}
	// at is the vertex of cube edge e's fan in cell (i, j).
	at := func(ids []uint32, fans []uint64, i, j, e int) uint32 {
		c := (j+1)*cx + i + 1
		return ids[c] + uint32(fans[c]>>(4*e))&15
	}
	label := func(buffer []uint32, x, y int) uint32 { return buffer[(y+1)*px+x+1] }
	// z-edges between voxel slices k and k+1: cells (x-1..x, y-1..y, k),
	// where the voxel pair is cube edge 11, 10, 8 and 9.
	for y := 0; y < n.ny; y++ {
		a, b := span(1, y), span(2, y)
		for x := min(a.first, b.first) + 1; x <= max(a.last, b.last); x++ {
			if a, b := label(lo, x, y), label(hi, x, y); a != b {
				out.quad(a, b, [4]uint32{at(ids, fans, x-1, y-1, 11), at(ids, fans, x, y-1, 10), at(ids, fans, x, y, 8), at(ids, fans, x-1, y, 9)}, 0)
			}
		}
	}
	if k < 0 {
		return nil
	}
	// x- and y-edges of voxel slice k: cells of planes k-1 and k.
	for y := 0; y < n.ny; y++ {
		s := span(1, y)
		for x := s.first; x <= s.last; x++ {
			if a, b := label(lo, x, y), label(lo, x+1, y); a != b {
				out.quad(a, b, [4]uint32{at(previousIDs, previousFans, x, y-1, 3), at(previousIDs, previousFans, x, y, 2), at(ids, fans, x, y, 0), at(ids, fans, x, y-1, 1)}, 0b0011)
			}
		}
	}
	for y := -1; y < n.ny; y++ {
		a, b := span(1, y), span(1, y+1)
		for x := min(a.first, b.first) + 1; x <= max(a.last, b.last); x++ {
			if a, b := label(lo, x, y), label(lo, x, y+1); a != b {
				out.quad(a, b, [4]uint32{at(previousIDs, previousFans, x-1, y, 7), at(ids, fans, x-1, y, 5), at(ids, fans, x, y, 4), at(previousIDs, previousFans, x, y, 6)}, 0b1001)
			}
		}
	}
	return nil
}

// uniform reports whether the eight voxels of a cell carry one label.
func uniform(v *[8]uint32) bool {
	for _, label := range v[1:] {
		if label != v[0] {
			return false
		}
	}
	return true
}

// outerLayer returns, in ring order, the voxels behind square f of cell
// (i, j) in the neighbouring cell; voxel reads voxel (x, y) of layer 0..3
// (slices k-1..k+2).
func outerLayer(f *netSquare, voxel func(x, y, layer int) uint32, i, j int) [4]uint32 {
	var out [4]uint32
	axis := bits.TrailingZeros8(f.axis)
	for r, c := range f.ring {
		d := [3]int{int(c & 1), int(c >> 1 & 1), int(c >> 2 & 1)}
		d[axis] = 2
		if f.side == 0 {
			d[axis] = -1
		}
		out[r] = voxel(i+d[0], j+d[1], 1+d[2])
	}
	return out
}

// cellDefects collects the cells where a label's surface is not a manifold:
// tangled cells, and both cells of a square whose two pairs of quads are
// joined in one fan on either side (the edge then has four of the label's
// triangles).
type cellDefects struct {
	cells   map[[3]int32]bool
	squares map[[4]int32]int
}

func (d *cellDefects) add(i, j, k int, fan cellFans) {
	if !fan.tangled && fan.pinched == 0 {
		return
	}
	if d.cells == nil {
		d.cells, d.squares = map[[3]int32]bool{}, map[[4]int32]int{}
	}
	cell := [3]int32{int32(i), int32(j), int32(k)}
	if fan.tangled {
		d.cells[cell] = true
	}
	for f := range netSquares {
		if fan.pinched&(1<<f) == 0 {
			continue
		}
		// A square is named by its lower cell and axis.
		axis := f / 2
		low := cell
		if f%2 == 0 {
			low[axis]--
		}
		d.squares[[4]int32{low[0], low[1], low[2], int32(axis)}]++
	}
}

func (d *cellDefects) count() int {
	for square, sides := range d.squares {
		if sides == 2 {
			low := [3]int32{square[0], square[1], square[2]}
			high := low
			high[square[3]]++
			d.cells[low], d.cells[high] = true, true
		}
	}
	return len(d.cells)
}

// smooth relaxes every vertex toward the mean of its quad neighbours with at
// least as many labels in their cells (junction curves smooth along
// themselves, so the boundaries between three or more labels keep their
// place), and keeps it inside its dual cell.
func (n *surfaceNets) smooth(ctx context.Context, iterations int, relaxation float64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if iterations == 0 || len(n.position) == 0 {
		return nil
	}
	edges := make([]uint64, 0, 4*len(n.quads))
	for quadIndex, q := range n.quads {
		if quadIndex&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		for i := 0; i < 4; i++ {
			a, b := q[i], q[(i+1)%4]
			edges = append(edges, uint64(min(a, b))<<32|uint64(max(a, b)))
		}
	}
	slices.Sort(edges)
	edges = slices.Compact(edges)
	if err := ctx.Err(); err != nil {
		return err
	}
	start := make([]int32, len(n.position)+1)
	for _, e := range edges {
		start[e>>32+1]++
		start[uint32(e)+1]++
	}
	for i := 1; i < len(start); i++ {
		start[i] += start[i-1]
	}
	neighbours := make([]uint32, start[len(start)-1])
	fill := append([]int32(nil), start[:len(start)-1]...)
	for _, e := range edges {
		a, b := uint32(e>>32), uint32(e)
		if n.kind[b] >= n.kind[a] {
			neighbours[fill[a]] = b
			fill[a]++
		}
		if n.kind[a] >= n.kind[b] {
			neighbours[fill[b]] = a
			fill[b]++
		}
	}
	next := make([][3]float64, len(n.position))
	// Each iteration is a shrinking step toward the neighbours' mean and an
	// inflating step away from it (Taubin's lambda|mu), so the smoothing does
	// not shrink the surfaces the way plain Laplacian relaxation does.
	for step := 0; step < 2*iterations; step++ {
		factor := relaxation
		if step%2 == 1 {
			factor = -relaxation - taubinPassBand*relaxation*relaxation
		}
		for v, p := range n.position {
			if v&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			count := fill[v] - start[v]
			if count == 0 {
				next[v] = p
				continue
			}
			var mean [3]float64
			for _, u := range neighbours[start[v]:fill[v]] {
				q := n.position[u]
				mean[0] += q[0]
				mean[1] += q[1]
				mean[2] += q[2]
			}
			c := n.cell[v]
			for axis := 0; axis < 3; axis++ {
				moved := p[axis] + factor*(mean[axis]/float64(count)-p[axis])
				next[v][axis] = min(max(moved, float64(c[axis])), float64(c[axis])+1)
			}
		}
		n.position, next = next, n.position
	}
	return ctx.Err()
}

// taubinPassBand sets the inflating factor mu = -(lambda + k*lambda^2) of
// each shrinking factor lambda; k = 0.1 keeps mu slightly larger than lambda
// (0.525 for 0.5), below the frequencies of the voxel staircase.
const taubinPassBand = 0.1
