package roi

// 3D curve skeletons for centrelines (#1106). Thin follows Lee, Kashyap and
// Chu, "Building skeleton models via 3-D medial surface/axis thinning
// algorithms" (CVGIP 1994), as implemented by ITK's
// BinaryThinningImageFilter3D (ITK 35b196e13c,
// Modules/Filtering/Thickness3D, Apache-2.0, Copyright NumFOCUS): six
// directional sub-iterations delete border voxels that are not arc ends, keep
// the Euler characteristic (Lee's octant table, reproduced below) and are
// simple, with a sequential re-check against parallel deletion. Modifications:
// the input is a VolumeMask, only foreground voxels are visited, the simple
// point test counts 26-connected components by a search over the 26
// neighbours (equivalent to ITK's octree labelling, since two 26-adjacent
// neighbours always share an octant), and the work is cancellable. Pruning
// and path extraction are written from their definitions (graph spurs and
// geodesics over the 26-connected skeleton).

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math"
)

// ErrSkeletonPath reports that no centreline joins the requested points.
var ErrSkeletonPath = errors.New("roi: no skeleton path")

// MaxSkeletonVoxels bounds the masks Thin accepts.
const MaxSkeletonVoxels = 1 << 28

// eulerLUT is Lee et al.'s table of Euler characteristic changes per octant
// configuration, indexed by the octant bits below (bit 0, the centre, is
// always set, so only odd entries are used).
var eulerLUT = func() [256]int8 {
	var lut [256]int8
	odd := [128]int8{
		1, -1, -1, 1, -3, -1, -1, 1, -1, 1, 1, -1, 3, 1, 1, -1,
		-3, -1, 3, 1, 1, -1, 3, 1, -1, 1, 1, -1, 3, 1, 1, -1,
		-3, 3, -1, 1, 1, 3, -1, 1, -1, 1, 1, -1, 3, 1, 1, -1,
		1, 3, 3, 1, 5, 3, 3, 1, -1, 1, 1, -1, 3, 1, 1, -1,
		-7, -1, -1, 1, -3, -1, -1, 1, -1, 1, 1, -1, 3, 1, 1, -1,
		-3, -1, 3, 1, 1, -1, 3, 1, -1, 1, 1, -1, 3, 1, 1, -1,
		-3, 3, -1, 1, 1, 3, -1, 1, -1, 1, 1, -1, 3, 1, 1, -1,
		1, 3, 3, 1, 5, 3, 3, 1, -1, 1, 1, -1, 3, 1, 1, -1,
	}
	for i, v := range odd {
		lut[2*i+1] = v
	}
	return lut
}()

// eulerOctants lists, per octant of the 3×3×3 neighbourhood (index
// x + 3y + 9z), the neighbours giving bits 7 down to 1 of its LUT index.
var eulerOctants = [8][7]int{
	{24, 25, 15, 16, 21, 22, 12}, // SWU
	{26, 23, 17, 14, 25, 22, 16}, // SEU
	{18, 21, 9, 12, 19, 22, 10},  // NWU
	{20, 23, 19, 22, 11, 14, 10}, // NEU
	{6, 15, 7, 16, 3, 12, 4},     // SWB
	{8, 7, 17, 16, 5, 4, 14},     // SEB
	{0, 9, 3, 12, 1, 10, 4},      // NWB
	{2, 1, 11, 10, 5, 4, 14},     // NEB
}

// neighbourAdjacency lists, for each of the 27 neighbourhood positions, the
// other non-centre positions 26-adjacent to it.
var neighbourAdjacency = func() [27][]int {
	var adj [27][]int
	for a := 0; a < 27; a++ {
		if a == 13 {
			continue
		}
		ax, ay, az := a%3, a/3%3, a/9
		for b := 0; b < 27; b++ {
			if b == a || b == 13 {
				continue
			}
			bx, by, bz := b%3, b/3%3, b/9
			if absInt(ax-bx) <= 1 && absInt(ay-by) <= 1 && absInt(az-bz) <= 1 {
				adj[a] = append(adj[a], b)
			}
		}
	}
	return adj
}()

// Thin returns the curve skeleton of the mask: a one-voxel-thick,
// 26-connected set of voxels along the medial axis that keeps the topology
// (components, tunnels and cavities) of the input.
func (m *VolumeMask) Thin(ctx context.Context) (*VolumeMask, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	px, py, pz := m.Columns+2, m.Rows+2, m.Slices+2
	if float64(px)*float64(py)*float64(pz) > MaxSkeletonVoxels {
		return nil, fmt.Errorf("%w: %d×%d×%d voxels exceed the skeleton limit", ErrInvalidVolumeMask, m.Columns, m.Rows, m.Slices)
	}
	// A one-voxel background border makes every neighbourhood in range.
	image := make([]byte, px*py*pz)
	var foreground []int
	for z := 0; z < m.Slices; z++ {
		for y := 0; y < m.Rows; y++ {
			for x := 0; x < m.Columns; x++ {
				if m.Voxels[m.index(x, y, z)] {
					p := ((z+1)*py+y+1)*px + x + 1
					image[p] = 1
					foreground = append(foreground, p)
				}
			}
		}
	}
	var offsets [27]int
	for i := range offsets {
		offsets[i] = (i/9-1)*px*py + (i/3%3-1)*px + (i%3 - 1)
	}
	// Border directions in ITK's order: N (y-1), S (y+1), E (x+1), W (x-1),
	// U (z+1), B (z-1).
	borders := [6]int{-px, px, 1, -1, px * py, -px * py}
	var neighbourhood [27]byte
	gather := func(p int) {
		for i, offset := range offsets {
			neighbourhood[i] = image[p+offset]
		}
	}
	var candidates []int
	for unchanged := 0; unchanged < 6; {
		unchanged = 0
		for _, border := range borders {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			candidates = candidates[:0]
			for _, p := range foreground {
				if image[p] != 1 || image[p+border] != 0 {
					continue
				}
				gather(p)
				neighbours := -1
				for _, v := range neighbourhood {
					neighbours += int(v)
				}
				if neighbours == 1 || !eulerInvariant(&neighbourhood) || !simplePoint(&neighbourhood) {
					continue
				}
				candidates = append(candidates, p)
			}
			changed := false
			for _, p := range candidates {
				image[p] = 0
				gather(p)
				if simplePoint(&neighbourhood) {
					changed = true
				} else {
					image[p] = 1
				}
			}
			if !changed {
				unchanged++
			}
			kept := foreground[:0]
			for _, p := range foreground {
				if image[p] == 1 {
					kept = append(kept, p)
				}
			}
			foreground = kept
		}
	}
	out := &VolumeMask{Columns: m.Columns, Rows: m.Rows, Slices: m.Slices, X: m.X, Y: m.Y, Z: m.Z, Voxels: make([]bool, len(m.Voxels))}
	for _, p := range foreground {
		x, y, z := p%px-1, p/px%py-1, p/(px*py)-1
		out.Voxels[m.index(x, y, z)] = true
	}
	return out, nil
}

// eulerInvariant reports whether deleting the centre keeps the Euler
// characteristic of the neighbourhood.
func eulerInvariant(n *[27]byte) bool {
	sum := 0
	for _, octant := range eulerOctants {
		index := 1
		for bit, position := range octant {
			if n[position] == 1 {
				index |= 1 << (7 - bit)
			}
		}
		sum += int(eulerLUT[index])
	}
	return sum == 0
}

// simplePoint reports whether the foreground of the 26-neighbourhood,
// without the centre, forms at most one 26-connected component.
func simplePoint(n *[27]byte) bool {
	var seen [27]bool
	var stack [27]int
	components := 0
	for start := 0; start < 27; start++ {
		if start == 13 || n[start] != 1 || seen[start] {
			continue
		}
		components++
		if components > 1 {
			return false
		}
		top := 0
		stack[top] = start
		seen[start] = true
		for top >= 0 {
			current := stack[top]
			top--
			for _, next := range neighbourAdjacency[current] {
				if n[next] == 1 && !seen[next] {
					seen[next] = true
					top++
					stack[top] = next
				}
			}
		}
	}
	return true
}

// skeletonGraph is the 26-connected graph of skeleton voxels with edge
// lengths in mm.
type skeletonGraph struct {
	mask  *VolumeMask
	nodes []int       // voxel index per node
	node  map[int]int // voxel index -> node
	adj   [][]skeletonEdge
}

type skeletonEdge struct {
	to     int
	length float64
}

func newSkeletonGraph(m *VolumeMask) *skeletonGraph {
	g := &skeletonGraph{mask: m, node: map[int]int{}}
	for i, v := range m.Voxels {
		if v {
			g.node[i] = len(g.nodes)
			g.nodes = append(g.nodes, i)
		}
	}
	g.adj = make([][]skeletonEdge, len(g.nodes))
	for n, voxel := range g.nodes {
		x, y, z := voxel%m.Columns, voxel/m.Columns%m.Rows, voxel/(m.Columns*m.Rows)
		for dz := -1; dz <= 1; dz++ {
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					nx, ny, nz := x+dx, y+dy, z+dz
					if dx == 0 && dy == 0 && dz == 0 || nx < 0 || ny < 0 || nz < 0 || nx >= m.Columns || ny >= m.Rows || nz >= m.Slices {
						continue
					}
					if other, ok := g.node[m.index(nx, ny, nz)]; ok {
						g.adj[n] = append(g.adj[n], skeletonEdge{to: other, length: math.Sqrt(
							sq(m.X[nx]-m.X[x]) + sq(m.Y[ny]-m.Y[y]) + sq(m.Z[nz]-m.Z[z]))})
					}
				}
			}
		}
	}
	return g
}

func sq(v float64) float64 { return v * v }

// neighbourComponents counts the 26-connected components formed by a
// node's remaining neighbours among themselves.
func (g *skeletonGraph) neighbourComponents(n int, removed []bool) int {
	var neighbours []int
	for _, e := range g.adj[n] {
		if !removed[e.to] {
			neighbours = append(neighbours, e.to)
		}
	}
	components := 0
	seen := make([]bool, len(neighbours))
	for start := range neighbours {
		if seen[start] {
			continue
		}
		components++
		stack := []int{start}
		seen[start] = true
		for len(stack) > 0 {
			current := neighbours[stack[len(stack)-1]]
			stack = stack[:len(stack)-1]
			for _, e := range g.adj[current] {
				if i, member := indexOf(neighbours, e.to); member && !seen[i] {
					seen[i] = true
					stack = append(stack, i)
				}
			}
		}
	}
	return components
}

func indexOf(values []int, v int) (int, bool) {
	for i, value := range values {
		if value == v {
			return i, true
		}
	}
	return -1, false
}

func (g *skeletonGraph) degree(n int, removed []bool) int {
	d := 0
	for _, e := range g.adj[n] {
		if !removed[e.to] {
			d++
		}
	}
	return d
}

// removeRedundant deletes, one at a time, voxels that are not ends and
// whose neighbours stay connected without them: the corners of the
// staircase a 26-connected curve makes. What remains has interior voxels of
// degree two, so degree three or more marks a real branch point.
func (g *skeletonGraph) removeRedundant(ctx context.Context, removed []bool) error {
	for changed := true; changed; {
		if err := ctx.Err(); err != nil {
			return err
		}
		changed = false
		for n := range g.nodes {
			if !removed[n] && g.degree(n, removed) >= 2 && g.neighbourComponents(n, removed) == 1 {
				removed[n] = true
				changed = true
			}
		}
	}
	return nil
}

// PruneSkeleton removes the spurs of a skeleton: branches running from an
// end voxel to a branch point that are shorter than minBranchMM. It first
// drops redundant staircase voxels (keeping 26-connectivity and the ends),
// and repeats until no spur is left, so spurs of spurs go too; a skeleton
// that is a single curve is never shortened.
func PruneSkeleton(ctx context.Context, skeleton *VolumeMask, minBranchMM float64) (*VolumeMask, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := skeleton.validate(); err != nil {
		return nil, err
	}
	if minBranchMM < 0 || math.IsNaN(minBranchMM) || math.IsInf(minBranchMM, 0) {
		return nil, fmt.Errorf("%w: branch length must be finite and not negative", ErrInvalidVolumeMask)
	}
	g := newSkeletonGraph(skeleton)
	removed := make([]bool, len(g.nodes))
	for {
		if err := g.removeRedundant(ctx, removed); err != nil {
			return nil, err
		}
		var spurs [][]int
		for n := range g.nodes {
			if !removed[n] && g.degree(n, removed) == 1 {
				if branch, ok := g.spur(n, removed, minBranchMM); ok {
					spurs = append(spurs, branch)
				}
			}
		}
		if len(spurs) == 0 {
			break
		}
		for _, branch := range spurs {
			for _, n := range branch {
				removed[n] = true
			}
		}
	}
	out := skeleton.Clone()
	for n, voxel := range g.nodes {
		if removed[n] {
			out.Voxels[voxel] = false
		}
	}
	return out, nil
}

// spur walks from end node n along degree-two nodes and returns the branch
// (without the branch point) when a branch point is reached within limit.
func (g *skeletonGraph) spur(n int, removed []bool, limit float64) ([]int, bool) {
	branch := []int{n}
	previous, current := -1, n
	length := 0.0
	for {
		next, step := -1, 0.0
		for _, e := range g.adj[current] {
			if !removed[e.to] && e.to != previous {
				next, step = e.to, e.length
				break
			}
		}
		if next < 0 {
			return nil, false
		}
		length += step
		if length >= limit {
			return nil, false
		}
		switch g.degree(next, removed) {
		case 1:
			return nil, false // the other end of a lone curve
		case 2:
			branch = append(branch, next)
			previous, current = current, next
		default:
			return branch, true
		}
	}
}

// SkeletonPathOptions selects the ends of a centreline.
type SkeletonPathOptions struct {
	// From and To, when set, are points in the mask's mm coordinates; the
	// path joins the skeleton voxels nearest to them. Otherwise the path is
	// the longest geodesic of the largest skeleton component.
	From, To *[3]float64
}

// SkeletonPath orders skeleton voxels into a centreline: the shortest path
// in mm along the 26-connected skeleton between the chosen ends. It returns
// voxel indices (x, y, z) from the first end to the second.
func SkeletonPath(ctx context.Context, skeleton *VolumeMask, options SkeletonPathOptions) ([][3]int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := skeleton.validate(); err != nil {
		return nil, err
	}
	g := newSkeletonGraph(skeleton)
	if len(g.nodes) == 0 {
		return nil, fmt.Errorf("%w: the skeleton is empty", ErrSkeletonPath)
	}
	var start, end int
	switch {
	case options.From != nil && options.To != nil:
		start, end = g.nearest(*options.From), g.nearest(*options.To)
	case options.From != nil || options.To != nil:
		return nil, fmt.Errorf("%w: set both ends or neither", ErrInvalidVolumeMask)
	default:
		// Two sweeps: the farthest node from any node of the largest
		// component, then the farthest from that one.
		seed := g.largestComponentNode()
		dist, _, err := g.dijkstra(ctx, seed)
		if err != nil {
			return nil, err
		}
		start = farthest(dist)
		dist, _, err = g.dijkstra(ctx, start)
		if err != nil {
			return nil, err
		}
		end = farthest(dist)
	}
	dist, previous, err := g.dijkstra(ctx, start)
	if err != nil {
		return nil, err
	}
	if math.IsInf(dist[end], 1) {
		return nil, fmt.Errorf("%w: the ends lie on different skeleton components", ErrSkeletonPath)
	}
	var reversed [][3]int
	m := skeleton
	for n := end; ; n = previous[n] {
		voxel := g.nodes[n]
		reversed = append(reversed, [3]int{voxel % m.Columns, voxel / m.Columns % m.Rows, voxel / (m.Columns * m.Rows)})
		if n == start {
			break
		}
	}
	path := make([][3]int, len(reversed))
	for i, p := range reversed {
		path[len(reversed)-1-i] = p
	}
	return path, nil
}

func (g *skeletonGraph) nearest(p [3]float64) int {
	best, bestDistance := 0, math.Inf(1)
	m := g.mask
	for n, voxel := range g.nodes {
		x, y, z := voxel%m.Columns, voxel/m.Columns%m.Rows, voxel/(m.Columns*m.Rows)
		if d := sq(m.X[x]-p[0]) + sq(m.Y[y]-p[1]) + sq(m.Z[z]-p[2]); d < bestDistance {
			best, bestDistance = n, d
		}
	}
	return best
}

func (g *skeletonGraph) largestComponentNode() int {
	label := make([]int, len(g.nodes))
	best, bestSize := 0, 0
	next := 0
	for start := range g.nodes {
		if label[start] != 0 {
			continue
		}
		next++
		size := 0
		stack := []int{start}
		label[start] = next
		for len(stack) > 0 {
			current := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			size++
			for _, e := range g.adj[current] {
				if label[e.to] == 0 {
					label[e.to] = next
					stack = append(stack, e.to)
				}
			}
		}
		if size > bestSize {
			best, bestSize = start, size
		}
	}
	return best
}

func farthest(dist []float64) int {
	best, bestDistance := 0, -1.0
	for n, d := range dist {
		if !math.IsInf(d, 1) && d > bestDistance {
			best, bestDistance = n, d
		}
	}
	return best
}

func (g *skeletonGraph) dijkstra(ctx context.Context, source int) ([]float64, []int, error) {
	dist := make([]float64, len(g.nodes))
	previous := make([]int, len(g.nodes))
	for i := range dist {
		dist[i] = math.Inf(1)
		previous[i] = -1
	}
	dist[source] = 0
	queue := skeletonQueue{{node: source}}
	for pops := 0; len(queue) > 0; pops++ {
		if pops&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
		}
		item := heap.Pop(&queue).(skeletonItem)
		if item.dist > dist[item.node] {
			continue
		}
		for _, e := range g.adj[item.node] {
			if d := item.dist + e.length; d < dist[e.to] {
				dist[e.to] = d
				previous[e.to] = item.node
				heap.Push(&queue, skeletonItem{node: e.to, dist: d})
			}
		}
	}
	return dist, previous, nil
}

type skeletonItem struct {
	node int
	dist float64
}

type skeletonQueue []skeletonItem

func (q skeletonQueue) Len() int           { return len(q) }
func (q skeletonQueue) Less(i, j int) bool { return q[i].dist < q[j].dist }
func (q skeletonQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *skeletonQueue) Push(v any)        { *q = append(*q, v.(skeletonItem)) }
func (q *skeletonQueue) Pop() any {
	old := *q
	last := old[len(old)-1]
	*q = old[:len(old)-1]
	return last
}
