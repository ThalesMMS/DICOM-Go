package roi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
)

// ContourInterpolationMethod is explicit: existing callers of InterpolateMask
// retain their bounding-box/signed-distance behavior.
type ContourInterpolationMethod string

const (
	ContourSignedDistance ContourInterpolationMethod = "signed-distance"
	ContourMorphological  ContourInterpolationMethod = "morphological-geodesic"
)

type ContourEndPolicy string

const (
	ContourReportUnmatched ContourEndPolicy = "report-unmatched"
	ContourCloseUnmatched  ContourEndPolicy = "close-unmatched"
)

var (
	ErrContourInterpolation = errors.New("roi: invalid contour interpolation")
	ErrContourLimit         = errors.New("roi: contour interpolation resource limit")
)

// ContourInterpolationRequest describes one label on the declared slice axis
// (Axis=2). Manual masks are immutable for the call, including empty masks.
// Geometry must be an orthogonal common lattice; irregular slice positions and
// anisotropic in-plane spacing are accepted. Tilt/shear requires resampling.
// MaxTranslationMM bounds centroid alignment of mutually nearest components
// without overlap. Zero disables that alignment; equal candidates are ambiguous.
// EndPolicy defaults to reporting unmatched components. Optional closing erodes
// them to empty inside the annotated interval within EndLengthMM/MaxEndSlices.
// It never extrapolates beyond the first/last manual slice.
type ContourInterpolationRequest struct {
	Method                       ContourInterpolationMethod
	Label                        uint16
	Axis                         int
	Geometry                     Grid
	Manual                       map[int]*RasterMask
	MaxTranslationMM             float64
	EndPolicy                    ContourEndPolicy
	EndLengthMM                  float64
	MaxEndSlices                 int
	MaxSlices, MaxComponents     int
	MaxVoxels, MaxBytes, MaxWork uint64
}

// ContourRelation records the explicit component correspondence in a gap.
// Component indices are stable row-major discovery indices, starting at zero.
type ContourRelation struct {
	FirstSlice, LastSlice           int
	FirstComponents, LastComponents []int
	Kind                            string // one-to-one, branch, merge, complex, unmatched
	Translation                     [2]int // integer column/row shift of the first endpoint
}

// ContourInterpolationResult owns all masks. Unresolved gaps have diagnostics
// but no generated masks: consumers must not apply a partial result as complete.
type ContourInterpolationResult struct {
	Label      uint16
	Axis       int
	Method     ContourInterpolationMethod
	Manual     map[int]*RasterMask
	Generated  map[int]*RasterMask
	Relations  []ContourRelation
	Unresolved []int // first slice of each unresolved gap
	Work       uint64
}

type contourBudget struct {
	ctx         context.Context
	used, limit uint64
}

func (b *contourBudget) step(n uint64) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if n > b.limit-b.used {
		return ErrContourLimit
	}
	b.used += n
	return nil
}

func (r ContourInterpolationRequest) admitted() (ContourInterpolationRequest, int, []int, error) {
	bad := func() (ContourInterpolationRequest, int, []int, error) { return r, 0, nil, ErrContourInterpolation }
	if r.Label == 0 || r.Axis != 2 || (r.Method != ContourSignedDistance && r.Method != ContourMorphological) || len(r.Manual) < 2 || math.IsNaN(r.MaxTranslationMM) || math.IsInf(r.MaxTranslationMM, 0) || r.MaxTranslationMM < 0 {
		return bad()
	}
	if r.EndPolicy == "" {
		r.EndPolicy = ContourReportUnmatched
	}
	if r.EndPolicy != ContourReportUnmatched && r.EndPolicy != ContourCloseUnmatched {
		return bad()
	}
	if r.EndPolicy == ContourCloseUnmatched && (r.Method != ContourMorphological || !(r.EndLengthMM > 0) || math.IsInf(r.EndLengthMM, 0) || r.MaxEndSlices < 1 || r.MaxEndSlices > 4096) {
		return bad()
	}
	if err := r.Geometry.Validate(); err != nil {
		return bad()
	}
	g := r.Geometry
	row, col, normal := g.RowDir.Normalize(), g.ColDir.Normalize(), g.SliceDir.Normalize()
	if math.Abs(row.Dot(col)) > 1e-6 || row.Cross(col).Dot(normal) < 1-1e-6 {
		return bad()
	}
	pixels, err := RasterMaskPixelCount(g.Columns, g.Rows)
	if err != nil {
		return bad()
	}
	if r.MaxSlices == 0 {
		r.MaxSlices = 512
	}
	if r.MaxComponents == 0 {
		r.MaxComponents = 64
	}
	if r.MaxVoxels == 0 {
		r.MaxVoxels = 16 << 20
	}
	if r.MaxBytes == 0 {
		r.MaxBytes = 256 << 20
	}
	if r.MaxWork == 0 {
		r.MaxWork = 128 << 20
	}
	if r.MaxSlices < 2 || r.MaxSlices > 4096 || r.MaxComponents < 1 || r.MaxComponents > 256 || len(r.Manual) > r.MaxSlices {
		return r, 0, nil, ErrContourLimit
	}
	indices := make([]int, 0, len(r.Manual))
	for z, m := range r.Manual {
		if z < 0 || z >= g.Slices || m == nil || m.Columns != g.Columns || m.Rows != g.Rows {
			return bad()
		}
		indices = append(indices, z)
	}
	sort.Ints(indices)
	span := indices[len(indices)-1] - indices[0] + 1
	// Conservative output run storage plus dense labels, graph queues/distances,
	// component pixel lists and correspondence metadata. Admission precedes copies.
	if span > r.MaxSlices || uint64(pixels) > r.MaxVoxels/uint64(span) || uint64(pixels) > r.MaxBytes/(uint64(span)*32+256) {
		return r, 0, nil, ErrContourLimit
	}
	if math.IsInf(float64(pixels)*math.Max(g.ColSpacing, g.RowSpacing), 0) {
		return bad()
	}
	firstPosition := g.SliceOffset(indices[0])
	lastPosition := g.SliceOffset(indices[len(indices)-1])
	if math.IsInf(firstPosition, 0) || math.IsInf(lastPosition, 0) || math.IsInf(lastPosition-firstPosition, 0) || !(lastPosition > firstPosition) {
		return bad()
	}
	metadata := uint64(span)*uint64(r.MaxComponents)*256 + uint64(r.MaxComponents)*uint64(r.MaxComponents)*16
	storage := uint64(pixels) * (uint64(span)*32 + 256)
	if metadata > r.MaxBytes || storage > r.MaxBytes-metadata {
		return r, 0, nil, ErrContourLimit
	}
	return r, pixels, indices, nil
}

// InterpolateContours computes a detached proposal. Errors (including cancel)
// return no result and never modify inputs. The morphological method propagates
// conditional four-neighbour dilations inside the endpoints from their common
// seed, using physical edge lengths. It is an independent bounded algorithm,
// not a claim of general pixel equivalence to ITK's recursive interpolator.
func InterpolateContours(ctx context.Context, request ContourInterpolationRequest) (ContourInterpolationResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ContourInterpolationResult{}, err
	}
	r, pixels, indices, err := request.admitted()
	if err != nil {
		return ContourInterpolationResult{}, err
	}
	b := contourBudget{ctx: ctx, limit: r.MaxWork}
	out := ContourInterpolationResult{Label: r.Label, Axis: r.Axis, Method: r.Method, Manual: map[int]*RasterMask{}, Generated: map[int]*RasterMask{}}
	for _, z := range indices {
		if err := b.step(uint64(pixels)); err != nil {
			return ContourInterpolationResult{}, err
		}
		out.Manual[z] = r.Manual[z].Clone()
	}
	for gap := 1; gap < len(indices); gap++ {
		first, last := indices[gap-1], indices[gap]
		if last-first < 2 {
			continue
		}
		if r.Method == ContourSignedDistance {
			for z := first + 1; z < last; z++ {
				if err := b.step(uint64(pixels) * 32); err != nil {
					return ContourInterpolationResult{}, err
				}
				t := (r.Geometry.SliceOffset(z) - r.Geometry.SliceOffset(first)) / (r.Geometry.SliceOffset(last) - r.Geometry.SliceOffset(first))
				out.Generated[z] = InterpolateMask(r.Manual[first], r.Manual[last], t)
			}
			continue
		}
		generated, relations, ok, err := morphologicalGap(&b, r, first, last, pixels)
		if err != nil {
			return ContourInterpolationResult{}, err
		}
		out.Relations = append(out.Relations, relations...)
		if !ok {
			out.Unresolved = append(out.Unresolved, first)
			continue
		}
		for z, m := range generated {
			out.Generated[z] = m
		}
	}
	if err := ctx.Err(); err != nil {
		return ContourInterpolationResult{}, err
	}
	out.Work = b.used
	return out, nil
}

type contourComponent struct {
	pixels []int
	x, y   float64
}

func contourComponents(b *contourBudget, m *RasterMask, maxComponents, pixels int) ([]contourComponent, []int, error) {
	labels := make([]int, pixels)
	for i := range labels {
		labels[i] = -1
	}
	out := []contourComponent{}
	for start := 0; start < pixels; start++ {
		if start%1024 == 0 {
			if err := b.step(1024); err != nil {
				return nil, nil, err
			}
		}
		if labels[start] >= 0 || !m.Get(start%m.Columns, start/m.Columns) {
			continue
		}
		if len(out) >= maxComponents {
			return nil, nil, ErrContourLimit
		}
		c := contourComponent{pixels: []int{start}}
		labels[start] = len(out)
		for head := 0; head < len(c.pixels); head++ {
			if head%1024 == 0 {
				if err := b.step(1024); err != nil {
					return nil, nil, err
				}
			}
			p := c.pixels[head]
			x, y := p%m.Columns, p/m.Columns
			c.x += float64(x)
			c.y += float64(y)
			for _, q := range contourNeighbours(p, m.Columns, m.Rows) {
				if q >= 0 && labels[q] < 0 && m.Get(q%m.Columns, q/m.Columns) {
					labels[q] = len(out)
					c.pixels = append(c.pixels, q)
				}
			}
		}
		c.x /= float64(len(c.pixels))
		c.y /= float64(len(c.pixels))
		out = append(out, c)
	}
	return out, labels, nil
}
func contourNeighbours(p, columns, rows int) [4]int {
	n := [4]int{-1, -1, -1, -1}
	x, y := p%columns, p/columns
	if x > 0 {
		n[0] = p - 1
	}
	if x+1 < columns {
		n[1] = p + 1
	}
	if y > 0 {
		n[2] = p - columns
	}
	if y+1 < rows {
		n[3] = p + columns
	}
	return n
}

func morphologicalGap(b *contourBudget, r ContourInterpolationRequest, first, last, pixels int) (map[int]*RasterMask, []ContourRelation, bool, error) {
	a, al, err := contourComponents(b, r.Manual[first], r.MaxComponents, pixels)
	if err != nil {
		return nil, nil, false, err
	}
	c, cl, err := contourComponents(b, r.Manual[last], r.MaxComponents, pixels)
	if err != nil {
		return nil, nil, false, err
	}
	edges := make([][]bool, len(a))
	for i := range edges {
		edges[i] = make([]bool, len(c))
	}
	for p := 0; p < pixels; p++ {
		if p%1024 == 0 {
			if err := b.step(1024); err != nil {
				return nil, nil, false, err
			}
		}
		if al[p] >= 0 && cl[p] >= 0 {
			edges[al[p]][cl[p]] = true
		}
	}
	shifts := map[[2]int][2]int{}
	if err := b.step(uint64(len(a)) * uint64(len(c)) * 4); err != nil {
		return nil, nil, false, err
	}
	linkedA, linkedC := make([]bool, len(a)), make([]bool, len(c))
	for i := range a {
		for j, yes := range edges[i] {
			if yes {
				linkedA[i] = true
				linkedC[j] = true
			}
		}
	}
	nearest := func(from []contourComponent, to []contourComponent, i int, linked []bool) int {
		best, distance, tie := -1, math.Inf(1), false
		for j := range to {
			if linked[j] {
				continue
			}
			d := math.Hypot((from[i].x-to[j].x)*r.Geometry.ColSpacing, (from[i].y-to[j].y)*r.Geometry.RowSpacing)
			if d < distance-1e-9 {
				best, distance, tie = j, d, false
			} else if math.Abs(d-distance) <= 1e-9 {
				tie = true
			}
		}
		if tie || distance > r.MaxTranslationMM {
			return -1
		}
		return best
	}
	nearA, nearC := make([]int, len(a)), make([]int, len(c))
	for i := range a {
		nearA[i] = nearest(a, c, i, linkedC)
	}
	for j := range c {
		nearC[j] = nearest(c, a, j, linkedA)
	}
	for i := range a {
		if linkedA[i] || r.MaxTranslationMM == 0 {
			continue
		}
		j := nearA[i]
		if j < 0 || nearC[j] != i {
			continue
		}
		edges[i][j] = true
		shifts[[2]int{i, j}] = [2]int{int(math.Round(c[j].x - a[i].x)), int(math.Round(c[j].y - a[i].y))}
	}
	left, right := make([]bool, pixels), make([]bool, pixels)
	ld, rd := make([]float64, pixels), make([]float64, pixels)
	positions, nodes := make([]int, pixels), make([]int, pixels)
	seenA, seenC := make([]bool, len(a)), make([]bool, len(c))
	relations := []ContourRelation{}
	complete := true
	generated := map[int]*RasterMask{}
	for z := first + 1; z < last; z++ {
		generated[z] = NewRasterMask(r.Geometry.Columns, r.Geometry.Rows)
	}
	for i := range a {
		if seenA[i] {
			continue
		}
		aa, cc := []int{i}, []int{}
		seenA[i] = true
		for head := 0; head < len(aa)+len(cc); head++ {
			// Fixed-point connected bipartite groups, bounded by component admission.
			if err := b.step(uint64(len(aa))*uint64(len(c)) + uint64(len(cc))*uint64(len(a))); err != nil {
				return nil, nil, false, err
			}
			changed := false
			for _, k := range aa {
				for j, yes := range edges[k] {
					if yes && !seenC[j] {
						seenC[j] = true
						cc = append(cc, j)
						changed = true
					}
				}
			}
			for _, j := range cc {
				for k := range a {
					if edges[k][j] && !seenA[k] {
						seenA[k] = true
						aa = append(aa, k)
						changed = true
					}
				}
			}
			if !changed {
				break
			}
		}
		relation := ContourRelation{FirstSlice: first, LastSlice: last, FirstComponents: aa, LastComponents: cc, Kind: "one-to-one"}
		switch {
		case len(cc) == 0:
			relation.Kind = "unmatched"
			if r.EndPolicy == ContourReportUnmatched {
				complete = false
			} else {
				relation.Kind = "closing"
			}
		case len(aa) > 1 && len(cc) > 1:
			relation.Kind = "complex"
		case len(aa) > 1:
			relation.Kind = "merge"
		case len(cc) > 1:
			relation.Kind = "branch"
		}
		if len(aa) == 1 && len(cc) == 1 {
			relation.Translation = shifts[[2]int{aa[0], cc[0]}]
		}
		relations = append(relations, relation)
		if len(cc) == 0 {
			if r.EndPolicy == ContourCloseUnmatched {
				if err := closeContourEnd(b, r, a[aa[0]], first, last, generated, left, right, ld, positions, nodes); err != nil {
					return nil, nil, false, err
				}
			}
			continue
		}
		clear(left)
		clear(right)
		dx, dy := relation.Translation[0], relation.Translation[1]
		for _, k := range aa {
			for _, p := range a[k].pixels {
				x, y := p%r.Geometry.Columns+dx, p/r.Geometry.Columns+dy
				if x >= 0 && x < r.Geometry.Columns && y >= 0 && y < r.Geometry.Rows {
					left[y*r.Geometry.Columns+x] = true
				} else {
					complete = false
				}
			}
		}
		for _, j := range cc {
			for _, p := range c[j].pixels {
				right[p] = true
			}
		}
		lmax, err := contourDilationDistanceInto(b, left, right, r.Geometry, ld, positions, nodes)
		if err != nil {
			return nil, nil, false, err
		}
		rmax, err := contourDilationDistanceInto(b, right, left, r.Geometry, rd, positions, nodes)
		if err != nil {
			return nil, nil, false, err
		}
		if math.IsInf(lmax, 1) || math.IsInf(rmax, 1) {
			complete = false
			continue
		}
		for z := first + 1; z < last; z++ {
			t := (r.Geometry.SliceOffset(z) - r.Geometry.SliceOffset(first)) / (r.Geometry.SliceOffset(last) - r.Geometry.SliceOffset(first))
			if err := b.step(uint64(pixels)); err != nil {
				return nil, nil, false, err
			}
			shiftX, shiftY := int(math.Round((1-t)*float64(dx))), int(math.Round((1-t)*float64(dy)))
			contourEmitRuns(generated[z], shiftX, shiftY, func(p int) bool {
				return (left[p] && right[p]) || (left[p] && ld[p] <= (1-t)*lmax) || (right[p] && rd[p] <= t*rmax)
			})
		}
	}
	for j := range c {
		if !seenC[j] {
			kind := "unmatched"
			if r.EndPolicy == ContourCloseUnmatched {
				kind = "closing"
				if err := closeContourEnd(b, r, c[j], last, first, generated, left, right, ld, positions, nodes); err != nil {
					return nil, nil, false, err
				}
			} else {
				complete = false
			}
			relations = append(relations, ContourRelation{FirstSlice: first, LastSlice: last, LastComponents: []int{j}, Kind: kind})
		}
	}
	return generated, relations, complete, nil
}

func closeContourEnd(b *contourBudget, r ContourInterpolationRequest, c contourComponent, from, to int, generated map[int]*RasterMask, mask, seed []bool, d []float64, positions, nodes []int) error {
	clear(mask)
	clear(seed)
	for _, p := range c.pixels {
		mask[p] = true
	}
	for _, p := range c.pixels {
		for _, q := range contourNeighbours(p, r.Geometry.Columns, r.Geometry.Rows) {
			if q < 0 || !mask[q] {
				seed[p] = true
			}
		}
	}
	maximum, err := contourDilationDistanceInto(b, mask, seed, r.Geometry, d, positions, nodes)
	if err != nil {
		return err
	}
	direction := 1
	if from > to {
		direction = -1
	}
	for z := from + direction; z != to; z += direction {
		delta := int(math.Abs(float64(z - from)))
		physical := math.Abs(r.Geometry.SliceOffset(z) - r.Geometry.SliceOffset(from))
		if delta > r.MaxEndSlices || physical >= r.EndLengthMM {
			continue
		}
		if err := b.step(uint64(len(mask))); err != nil {
			return err
		}
		// Boundary-centre erosion in physical units. At the declared length all
		// pixels disappear, even a one-pixel branch with zero in-plane radius.
		threshold := (maximum + math.Min(r.Geometry.RowSpacing, r.Geometry.ColSpacing)) * physical / r.EndLengthMM
		contourEmitRuns(generated[z], 0, 0, func(p int) bool { return mask[p] && d[p] >= threshold })
	}
	return nil
}

// Emit sorted row runs, avoiding per-voxel insertion/allocation of masks.
func contourEmitRuns(mask *RasterMask, shiftX, shiftY int, on func(int) bool) {
	for y := 0; y < mask.Rows; y++ {
		for x := 0; x < mask.Columns; {
			if !on(y*mask.Columns + x) {
				x++
				continue
			}
			start := x
			x++
			for x < mask.Columns && on(y*mask.Columns+x) {
				x++
			}
			mask.SetRun(y-shiftY, start-shiftX, x-shiftX)
		}
	}
}

// Indexed heap keeps at most one entry per voxel: unlike repeated relaxation
// queues, its workspace bound does not depend on image content or spacing.
type contourHeap struct {
	nodes, positions []int
	distance         []float64
}

func (h *contourHeap) swap(a, b int) {
	h.nodes[a], h.nodes[b] = h.nodes[b], h.nodes[a]
	h.positions[h.nodes[a]] = a
	h.positions[h.nodes[b]] = b
}
func (h *contourHeap) up(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if h.distance[h.nodes[p]] <= h.distance[h.nodes[i]] {
			break
		}
		h.swap(i, p)
		i = p
	}
}
func (h *contourHeap) update(p int) {
	i := h.positions[p]
	if i < 0 {
		i = len(h.nodes)
		h.nodes = append(h.nodes, p)
		h.positions[p] = i
	}
	h.up(i)
}
func (h *contourHeap) pop() int {
	p := h.nodes[0]
	last := len(h.nodes) - 1
	h.swap(0, last)
	h.nodes = h.nodes[:last]
	h.positions[p] = -1
	for i := 0; 2*i+1 < len(h.nodes); {
		j := 2*i + 1
		if j+1 < len(h.nodes) && h.distance[h.nodes[j+1]] < h.distance[h.nodes[j]] {
			j++
		}
		if h.distance[h.nodes[i]] <= h.distance[h.nodes[j]] {
			break
		}
		h.swap(i, j)
		i = j
	}
	return p
}
func contourDilationDistance(b *contourBudget, mask, seed []bool, g Grid) ([]float64, float64, error) {
	d := make([]float64, len(mask))
	maximum, err := contourDilationDistanceInto(b, mask, seed, g, d, make([]int, len(mask)), make([]int, len(mask)))
	return d, maximum, err
}

func contourDilationDistanceInto(b *contourBudget, mask, seed []bool, g Grid, d []float64, positions, nodes []int) (float64, error) {
	h := contourHeap{nodes: nodes[:0], positions: positions, distance: d}
	for p := range d {
		if p%1024 == 0 {
			if err := b.step(1024); err != nil {
				return 0, err
			}
		}
		d[p] = math.Inf(1)
		positions[p] = -1
		if mask[p] && seed[p] {
			d[p] = 0
			h.update(p)
		}
	}
	count := uint64(0)
	for len(h.nodes) > 0 {
		if count%256 == 0 {
			if err := b.step(256 * 4); err != nil {
				return 0, err
			}
		}
		count++
		p := h.pop()
		for edge, q := range contourNeighbours(p, g.Columns, g.Rows) {
			if q < 0 || !mask[q] {
				continue
			}
			cost := g.ColSpacing
			if edge >= 2 {
				cost = g.RowSpacing
			}
			candidate := d[p] + cost
			if candidate < d[q] {
				d[q] = candidate
				h.update(q)
			}
		}
	}
	maximum := float64(0)
	for p, on := range mask {
		if p%1024 == 0 {
			if err := b.step(1024); err != nil {
				return 0, err
			}
		}
		if on {
			maximum = math.Max(maximum, d[p])
		}
	}
	return maximum, nil
}

// ValidateComplete reports unresolved correspondences before a consumer enters
// an application transaction. Diagnostics remain available for review/rejection.
func (r ContourInterpolationResult) ValidateComplete() error {
	if len(r.Unresolved) > 0 {
		return fmt.Errorf("%w: %d unresolved contour gaps", ErrContourInterpolation, len(r.Unresolved))
	}
	return nil
}
