package roi

import (
	"math"
	"sort"
)

// Exact Feret diameter (largest distance between voxel centres) without
// the quadratic all-pairs search over boundary voxels.
//
// The two farthest centres are vertices of the convex hull of the centres,
// and a hull vertex cannot lie strictly between two other centres on any
// axis line. So the candidates are the voxels that are the first or last of
// their x row, y column and z column at once, which is a small subset of
// the boundary. A k-d tree over the candidates then answers, for each one,
// "is any candidate farther than the best pair so far?" by pruning the
// nodes whose box's farthest corner is closer than the best distance.

const feretLeafSize = 8

type feretNode struct {
	lo, hi      [3]float64
	start, end  int
	left, right int // -1 for leaves
}

type feretTree struct {
	pts   [][3]float64
	idx   []int
	nodes []feretNode
	best  float64 // squared distance
	a, b  int
}

// feretDiameter returns the Feret diameter and its end points in local
// coordinates.
func (g *shapeGrid) feretDiameter(runs []shapeRun, lo, hi [3]int) (float64, [3]float64, [3]float64) {
	pts := g.feretCandidates(runs, lo, hi)
	if len(pts) == 1 {
		return 0, pts[0], pts[0]
	}
	t := &feretTree{pts: pts, idx: make([]int, len(pts))}
	for i := range t.idx {
		t.idx[i] = i
	}
	t.seed()
	t.build(0, len(pts))
	for i := range pts {
		t.search(0, i)
	}
	return math.Sqrt(t.best), pts[t.a], pts[t.b]
}

// feretCandidates returns the centres of the voxels extreme along their x
// row and their y and z columns.
func (g *shapeGrid) feretCandidates(runs []shapeRun, lo, hi [3]int) [][3]float64 {
	nx, ny, nz := hi[0]-lo[0]+1, hi[1]-lo[1]+1, hi[2]-lo[2]+1
	yMin, yMax := make([]int32, nx*nz), make([]int32, nx*nz)
	zMin, zMax := make([]int32, nx*ny), make([]int32, nx*ny)
	for i := range yMin {
		yMin[i], yMax[i] = math.MaxInt32, -1
	}
	for i := range zMin {
		zMin[i], zMax[i] = math.MaxInt32, -1
	}
	for _, r := range runs {
		y, z := int32(r.y), int32(r.z)
		for x := r.x0; x <= r.x1; x++ {
			iy := (r.z-lo[2])*nx + (x - lo[0])
			yMin[iy], yMax[iy] = min(yMin[iy], y), max(yMax[iy], y)
			iz := (r.y-lo[1])*nx + (x - lo[0])
			zMin[iz], zMax[iz] = min(zMin[iz], z), max(zMax[iz], z)
		}
	}
	extreme := func(x, y, z int) bool {
		iy := (z-lo[2])*nx + (x - lo[0])
		if int32(y) != yMin[iy] && int32(y) != yMax[iy] {
			return false
		}
		if g.dim == 2 {
			return true
		}
		iz := (y-lo[1])*nx + (x - lo[0])
		return int32(z) == zMin[iz] || int32(z) == zMax[iz]
	}
	var pts [][3]float64
	add := func(x, y, z int) {
		if extreme(x, y, z) {
			p := [3]float64{g.coord[0][x], g.coord[1][y], g.coord[2][z]}
			if g.dim == 2 {
				p[2] = 0
			}
			pts = append(pts, p)
		}
	}
	for i := 0; i < len(runs); {
		j := i + 1
		for j < len(runs) && runs[j].y == runs[i].y && runs[j].z == runs[i].z {
			j++
		}
		first, last := runs[i].x0, runs[j-1].x1
		add(first, runs[i].y, runs[i].z)
		if last != first {
			add(last, runs[i].y, runs[i].z)
		}
		i = j
	}
	return pts
}

// seed starts from the best pair among the extremes along the 13 lattice
// directions, which is usually the answer or close to it.
func (t *feretTree) seed() {
	var ext []int
	for dx := -1; dx <= 1; dx++ {
		for dy := -1; dy <= 1; dy++ {
			for dz := -1; dz <= 1; dz++ {
				if dx < 0 || dx == 0 && (dy < 0 || dy == 0 && dz <= 0) {
					continue // one of each ± pair, no zero vector
				}
				lo, hi := 0, 0
				for i, p := range t.pts {
					v := float64(dx)*p[0] + float64(dy)*p[1] + float64(dz)*p[2]
					if v < float64(dx)*t.pts[lo][0]+float64(dy)*t.pts[lo][1]+float64(dz)*t.pts[lo][2] {
						lo = i
					}
					if v > float64(dx)*t.pts[hi][0]+float64(dy)*t.pts[hi][1]+float64(dz)*t.pts[hi][2] {
						hi = i
					}
				}
				ext = append(ext, lo, hi)
			}
		}
	}
	t.best, t.a, t.b = -1, ext[0], ext[0]
	for i, p := range ext {
		for _, q := range ext[i+1:] {
			if d := dist2(t.pts[p], t.pts[q]); d > t.best {
				t.best, t.a, t.b = d, p, q
			}
		}
	}
}

func dist2(p, q [3]float64) float64 {
	dx, dy, dz := p[0]-q[0], p[1]-q[1], p[2]-q[2]
	return dx*dx + dy*dy + dz*dz
}

func (t *feretTree) build(start, end int) int {
	node := feretNode{start: start, end: end, left: -1, right: -1}
	node.lo, node.hi = t.pts[t.idx[start]], t.pts[t.idx[start]]
	for _, i := range t.idx[start+1 : end] {
		for k := 0; k < 3; k++ {
			node.lo[k] = math.Min(node.lo[k], t.pts[i][k])
			node.hi[k] = math.Max(node.hi[k], t.pts[i][k])
		}
	}
	pos := len(t.nodes)
	t.nodes = append(t.nodes, node)
	if end-start > feretLeafSize {
		axis := 0
		for k := 1; k < 3; k++ {
			if node.hi[k]-node.lo[k] > node.hi[axis]-node.lo[axis] {
				axis = k
			}
		}
		sub := t.idx[start:end]
		sort.Slice(sub, func(i, j int) bool { return t.pts[sub[i]][axis] < t.pts[sub[j]][axis] })
		mid := start + (end-start)/2
		left := t.build(start, mid)
		right := t.build(mid, end)
		t.nodes[pos].left, t.nodes[pos].right = left, right
	}
	return pos
}

// farthest2 is the squared distance from p to the farthest corner of the
// node's box, an upper bound for every point inside it.
func (n *feretNode) farthest2(p [3]float64) float64 {
	s := 0.0
	for k := 0; k < 3; k++ {
		d := math.Max(math.Abs(p[k]-n.lo[k]), math.Abs(p[k]-n.hi[k]))
		s += d * d
	}
	return s
}

func (t *feretTree) search(node, i int) {
	n := &t.nodes[node]
	p := t.pts[i]
	if n.farthest2(p) <= t.best {
		return
	}
	if n.left < 0 {
		for _, j := range t.idx[n.start:n.end] {
			if d := dist2(p, t.pts[j]); d > t.best {
				t.best, t.a, t.b = d, min(i, j), max(i, j)
			}
		}
		return
	}
	first, second := n.left, n.right
	if t.nodes[second].farthest2(p) > t.nodes[first].farthest2(p) {
		first, second = second, first
	}
	t.search(first, i)
	t.search(second, i)
}
