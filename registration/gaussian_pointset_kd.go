package registration

import (
	"context"
	"github.com/ThalesMMS/dicom-go/render"
)

// newGaussianKDTree reuses the existing kdTree/kdNode representation, with
// context-aware deterministic construction for an already owned snapshot.
// The ICP constructor/search remain unchanged.
func newGaussianKDTree(ctx context.Context, points []render.Vec3) (*kdTree, error) {
	tree := &kdTree{points: points, nodes: make([]kdNode, 0, len(points))}
	indices := make([]int, len(points))
	scratch := make([]int, len(points))
	for i := range indices {
		if i%64 == 0 {
			if e := ctx.Err(); e != nil {
				return nil, e
			}
		}
		indices[i] = i
	}
	var build func([]int, int) (int, error)
	build = func(ids []int, depth int) (int, error) {
		if e := ctx.Err(); e != nil {
			return -1, e
		}
		if len(ids) == 0 {
			return -1, nil
		}
		axis := depth % 3
		if e := gaussianSortIndices(ctx, points, ids, scratch[:len(ids)], axis); e != nil {
			return -1, e
		}
		middle := len(ids) / 2
		node := len(tree.nodes)
		tree.nodes = append(tree.nodes, kdNode{point: ids[middle], axis: axis, left: -1, right: -1})
		left, e := build(ids[:middle], depth+1)
		if e != nil {
			return -1, e
		}
		right, e := build(ids[middle+1:], depth+1)
		if e != nil {
			return -1, e
		}
		tree.nodes[node].left, tree.nodes[node].right = left, right
		return node, nil
	}
	if _, e := build(indices, 0); e != nil {
		return nil, e
	}
	return tree, nil
}
func gaussianSortIndices(ctx context.Context, points []render.Vec3, ids, scratch []int, axis int) error {
	less := func(a, b int) bool {
		av, bv := coordinate(points[a], axis), coordinate(points[b], axis)
		return av < bv || av == bv && a < b
	}
	for width := 1; width < len(ids); width *= 2 {
		for start := 0; start < len(ids); start += 2 * width {
			mid, end := min(start+width, len(ids)), min(start+2*width, len(ids))
			a, b := start, mid
			for at := start; at < end; at++ {
				if at%64 == 0 {
					if e := ctx.Err(); e != nil {
						return e
					}
				}
				if a < mid && (b >= end || less(ids[a], ids[b])) {
					scratch[at] = ids[a]
					a++
				} else {
					scratch[at] = ids[b]
					b++
				}
			}
		}
		for i := range ids {
			if i%64 == 0 {
				if e := ctx.Err(); e != nil {
					return e
				}
			}
			ids[i] = scratch[i]
		}
	}
	return ctx.Err()
}

type gaussianNeighbor struct {
	index int
	d2    float64
}

// gaussianNearest bounds traversal and K storage; ties use original point index.
func (tree *kdTree) gaussianNearest(ctx context.Context, q render.Vec3, k int, labels []uint32, label uint32, out []gaussianNeighbor, visits *uint64, limit uint64) ([]gaussianNeighbor, error) {
	var search func(int) error
	search = func(node int) error {
		if node < 0 {
			return nil
		}
		if *visits >= limit {
			return ErrPointSetWorkLimit
		}
		if *visits%32 == 0 {
			if e := ctx.Err(); e != nil {
				return e
			}
		}
		*visits++
		n := tree.nodes[node]
		point := tree.points[n.point]
		d := q.Sub(point)
		if !finiteVec(d) {
			return ErrGaussianPointSet
		}
		d2 := d.Dot(d)
		if !finite(d2) {
			return ErrGaussianPointSet
		}
		if labels == nil || labels[n.point] == label {
			at := len(out)
			for at > 0 && (d2 < out[at-1].d2 || d2 == out[at-1].d2 && n.point < out[at-1].index) {
				at--
			}
			if at < k {
				if len(out) < k {
					out = append(out, gaussianNeighbor{})
				}
				copy(out[at+1:], out[at:len(out)-1])
				out[at] = gaussianNeighbor{n.point, d2}
			}
		}
		diff := coordinate(q, n.axis) - coordinate(point, n.axis)
		plane := diff * diff
		if !finite(plane) {
			return ErrGaussianPointSet
		}
		near, far := n.left, n.right
		if diff > 0 {
			near, far = far, near
		}
		if e := search(near); e != nil {
			return e
		}
		if len(out) < k || plane <= out[len(out)-1].d2 {
			return search(far)
		}
		return nil
	}
	if e := search(0); e != nil {
		return nil, e
	}
	if len(out) != k {
		return nil, ErrGaussianPointSet
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	return out, nil
}
