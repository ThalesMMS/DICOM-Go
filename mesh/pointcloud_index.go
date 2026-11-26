package mesh

import "math"

type pointCloudNode struct {
	point, left, right int32
	axis               uint8
}
type pointCloudTree struct {
	points [][3]float32
	nodes  []pointCloudNode
	root   int32
}
type pointCloudNeighbor struct {
	id        int32
	distance2 float64
}

// The balanced tree uses deterministic in-place selection. Every comparison
// is charged, so adversarial quickselect and degenerate nearest queries are
// bounded by MaxWork. No unbounded auxiliary candidate lists are allocated.
func buildPointCloudTree(points [][3]float32, run *pointCloudRun) (*pointCloudTree, error) {
	t := &pointCloudTree{points: points, nodes: make([]pointCloudNode, len(points)), root: -1}
	order := make([]int32, len(points))
	for i := range order {
		if err := run.step(); err != nil {
			return nil, err
		}
		order[i] = int32(i)
	}
	var selectMedian func(int, int, int, uint8) error
	less := func(a, b int32, axis uint8) bool {
		x, y := points[a][axis], points[b][axis]
		return x < y || (x == y && a < b)
	}
	selectMedian = func(lo, hi, mid int, axis uint8) error {
		for lo < hi {
			pivot := order[(lo+hi)/2]
			i, j := lo, hi
			for i <= j {
				for {
					if err := run.step(); err != nil {
						return err
					}
					if !less(order[i], pivot, axis) {
						break
					}
					i++
				}
				for {
					if err := run.step(); err != nil {
						return err
					}
					if !less(pivot, order[j], axis) {
						break
					}
					j--
				}
				if i <= j {
					order[i], order[j] = order[j], order[i]
					i++
					j--
				}
			}
			if mid <= j {
				hi = j
			} else if mid >= i {
				lo = i
			} else {
				break
			}
		}
		return nil
	}
	next := int32(0)
	var build func(int, int, uint8) (int32, error)
	build = func(lo, hi int, axis uint8) (int32, error) {
		if lo >= hi {
			return -1, nil
		}
		if err := run.step(); err != nil {
			return -1, err
		}
		mid := lo + (hi-lo)/2
		if err := selectMedian(lo, hi-1, mid, axis); err != nil {
			return -1, err
		}
		id := next
		next++
		t.nodes[id] = pointCloudNode{point: order[mid], axis: axis}
		left, err := build(lo, mid, (axis+1)%3)
		if err != nil {
			return -1, err
		}
		right, err := build(mid+1, hi, (axis+1)%3)
		if err != nil {
			return -1, err
		}
		t.nodes[id].left, t.nodes[id].right = left, right
		return id, nil
	}
	root, err := build(0, len(points), 0)
	t.root = root
	return t, err
}

func pointCloudDistance2(a [3]float64, b [3]float32) float64 {
	x, y, z := a[0]-float64(b[0]), a[1]-float64(b[1]), a[2]-float64(b[2])
	return x*x + y*y + z*z
}
func cloudNeighborWorse(a, b pointCloudNeighbor) bool {
	return a.distance2 > b.distance2 || (a.distance2 == b.distance2 && a.id > b.id)
}

func (t *pointCloudTree) nearest(q [3]float64, skip, k int, heap []pointCloudNeighbor, run *pointCloudRun) ([]pointCloudNeighbor, error) {
	var visit func(int32) error
	visit = func(id int32) error {
		if id < 0 {
			return nil
		}
		if err := run.step(); err != nil {
			return err
		}
		n := t.nodes[id]
		p := t.points[n.point]
		if int(n.point) != skip {
			v := pointCloudNeighbor{n.point, pointCloudDistance2(q, p)}
			if len(heap) < k {
				heap = append(heap, v)
				for i := len(heap) - 1; i > 0; {
					if err := run.step(); err != nil {
						return err
					}
					parent := (i - 1) / 2
					if !cloudNeighborWorse(heap[i], heap[parent]) {
						break
					}
					heap[i], heap[parent] = heap[parent], heap[i]
					i = parent
				}
			} else if cloudNeighborWorse(heap[0], v) {
				heap[0] = v
				for i := 0; 2*i+1 < len(heap); {
					if err := run.step(); err != nil {
						return err
					}
					child := 2*i + 1
					if child+1 < len(heap) && cloudNeighborWorse(heap[child+1], heap[child]) {
						child++
					}
					if !cloudNeighborWorse(heap[child], heap[i]) {
						break
					}
					heap[i], heap[child] = heap[child], heap[i]
					i = child
				}
			}
		}
		d := q[n.axis] - float64(p[n.axis])
		near, far := n.left, n.right
		if d > 0 {
			near, far = far, near
		}
		if err := visit(near); err != nil {
			return err
		}
		if len(heap) < k || d*d <= heap[0].distance2 {
			return visit(far)
		}
		return nil
	}
	err := visit(t.root)
	return heap, err
}

func (t *pointCloudTree) within(q [3]float64, radius float64, active []bool, run *pointCloudRun) (bool, error) {
	var visit func(int32) (bool, error)
	visit = func(id int32) (bool, error) {
		if id < 0 {
			return false, nil
		}
		if err := run.step(); err != nil {
			return false, err
		}
		n := t.nodes[id]
		p := t.points[n.point]
		if active[n.point] && math.Sqrt(pointCloudDistance2(q, p)) < radius {
			return true, nil
		}
		d := q[n.axis] - float64(p[n.axis])
		near, far := n.left, n.right
		if d > 0 {
			near, far = far, near
		}
		found, err := visit(near)
		if found || err != nil {
			return found, err
		}
		if math.Abs(d) < radius {
			return visit(far)
		}
		return false, nil
	}
	return visit(t.root)
}
