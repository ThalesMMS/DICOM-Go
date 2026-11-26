package roi

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math"
)

// Livewire (#1018), after VTK's vtkDijkstraImageGeodesicPath and
// vtkDijkstraImageContourLineInterpolator: the contour between two anchors
// is the least-cost 8-connected path over the pixels, whose cost is low on
// strong edges (high gradient magnitude) plus a length term. A LivewireTree
// holds the shortest paths from one anchor to every pixel, so following the
// pointer is a trace back, not a new search. Written from the documented
// method; no VTK code is copied.

// ErrLivewire reports an unusable livewire request.
var ErrLivewire = errors.New("roi: invalid livewire request")

// LivewireOptions weights the path cost, as VTK's image, edge-length and
// curvature weights.
type LivewireOptions struct {
	// ImageWeight scales the image cost, 1 - |gradient| / max |gradient|
	// of the pixel entered; zero uses 1.
	ImageWeight float64
	// LengthWeight scales the step length in mm; zero uses 0.1, negative
	// disables it.
	LengthWeight float64
	// CurvatureWeight penalises turns (1 - cos of the turning angle) at the
	// cost of a slightly larger search state; zero disables it.
	CurvatureWeight float64
	// ColSpacing and RowSpacing are the pixel spacing in mm; zero uses 1.
	ColSpacing, RowSpacing float64
}

// LivewireCostMap is the per-pixel image cost of one slice.
type LivewireCostMap struct {
	Columns, Rows int
	cost          []float64
	opts          LivewireOptions
	maxBytes      int64 // nonzero only for the opt-in bounded Canny map
}

// NewLivewireCostMap computes the image cost of a columns x rows slice read
// through value (ok false marks a pixel outside the image, which paths
// avoid). The gradient is the central difference in mm, one-sided at the
// border.
func NewLivewireCostMap(columns, rows int, value func(x, y int) (float64, bool), opts LivewireOptions) (*LivewireCostMap, error) {
	if columns <= 0 || rows <= 0 || value == nil {
		return nil, fmt.Errorf("%w: %dx%d slice", ErrLivewire, columns, rows)
	}
	if opts.ImageWeight == 0 {
		opts.ImageWeight = 1
	}
	if opts.LengthWeight == 0 {
		opts.LengthWeight = 0.1
	}
	if opts.ColSpacing <= 0 {
		opts.ColSpacing = 1
	}
	if opts.RowSpacing <= 0 {
		opts.RowSpacing = 1
	}
	values := make([]float64, columns*rows)
	valid := make([]bool, columns*rows)
	for y := 0; y < rows; y++ {
		for x := 0; x < columns; x++ {
			v, ok := value(x, y)
			if ok && !math.IsNaN(v) && !math.IsInf(v, 0) {
				values[y*columns+x], valid[y*columns+x] = v, true
			}
		}
	}
	magnitude := make([]float64, len(values))
	largest := 0.0
	at := func(x, y int) (float64, bool) {
		if x < 0 || y < 0 || x >= columns || y >= rows || !valid[y*columns+x] {
			return 0, false
		}
		return values[y*columns+x], true
	}
	derivative := func(x, y, dx, dy int, spacing float64) float64 {
		center, _ := at(x, y)
		plus, hasPlus := at(x+dx, y+dy)
		minus, hasMinus := at(x-dx, y-dy)
		switch {
		case hasPlus && hasMinus:
			return (plus - minus) / (2 * spacing)
		case hasPlus:
			return (plus - center) / spacing
		case hasMinus:
			return (center - minus) / spacing
		}
		return 0
	}
	for y := 0; y < rows; y++ {
		for x := 0; x < columns; x++ {
			if !valid[y*columns+x] {
				continue
			}
			gx := derivative(x, y, 1, 0, opts.ColSpacing)
			gy := derivative(x, y, 0, 1, opts.RowSpacing)
			m := math.Hypot(gx, gy)
			magnitude[y*columns+x] = m
			largest = math.Max(largest, m)
		}
	}
	cost := make([]float64, len(values))
	for i := range cost {
		switch {
		case !valid[i]:
			cost[i] = math.Inf(1)
		case largest > 0:
			cost[i] = 1 - magnitude[i]/largest
		default:
			cost[i] = 1
		}
	}
	return &LivewireCostMap{Columns: columns, Rows: rows, cost: cost, opts: opts}, nil
}

// Cost is the image cost of pixel (x, y), +Inf outside the image.
func (c *LivewireCostMap) Cost(x, y int) float64 {
	if c == nil || x < 0 || y < 0 || x >= c.Columns || y >= c.Rows {
		return math.Inf(1)
	}
	return c.cost[y*c.Columns+x]
}

var livewireSteps = [8][2]int{{1, 0}, {1, 1}, {0, 1}, {-1, 1}, {-1, 0}, {-1, -1}, {0, -1}, {1, -1}}

// LivewireTree is the shortest-path tree from one anchor.
type LivewireTree struct {
	costs  *LivewireCostMap
	anchor [2]int
	// parent[state] is the previous state, -1 at the anchor; a state is a
	// pixel, times 8 incoming directions with curvature.
	parent   []int32
	distance []float64
	states   int
}

type livewireItem struct {
	state    int32
	distance float64
}

type livewireQueue []livewireItem

func (q livewireQueue) Len() int           { return len(q) }
func (q livewireQueue) Less(i, j int) bool { return q[i].distance < q[j].distance }
func (q livewireQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *livewireQueue) Push(x any)        { *q = append(*q, x.(livewireItem)) }
func (q *livewireQueue) Pop() any {
	old := *q
	x := old[len(old)-1]
	*q = old[:len(old)-1]
	return x
}

// Tree computes the shortest paths from anchor to every pixel (Dijkstra).
// The context is checked every few thousand pixels.
func (c *LivewireCostMap) Tree(ctx context.Context, anchor [2]int) (*LivewireTree, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil || anchor[0] < 0 || anchor[1] < 0 || anchor[0] >= c.Columns || anchor[1] >= c.Rows || math.IsInf(c.Cost(anchor[0], anchor[1]), 1) {
		return nil, fmt.Errorf("%w: anchor %v", ErrLivewire, anchor)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pixels := c.Columns * c.Rows
	directions := 1
	if c.opts.CurvatureWeight > 0 {
		directions = 8
	}
	if c.maxBytes > 0 && pixels > (16<<20)/directions {
		return nil, fmt.Errorf("%w: livewire state ceiling", ErrLivewire)
	}
	states := pixels * directions
	queueLimit := 0
	if c.maxBytes > 0 {
		var err error
		queueLimit, err = c.cannyTreeAdmission(states, directions)
		if err != nil {
			return nil, err
		}
	}
	t := &LivewireTree{costs: c, anchor: anchor, parent: make([]int32, states), distance: make([]float64, states), states: directions}
	for i := range t.distance {
		if i%4096 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		t.distance[i] = math.Inf(1)
		t.parent[i] = -1
	}
	var steps [8]float64
	for k, s := range livewireSteps {
		steps[k] = math.Hypot(float64(s[0])*c.opts.ColSpacing, float64(s[1])*c.opts.RowSpacing)
	}
	start := int32((anchor[1]*c.Columns + anchor[0]) * directions)
	queue := livewireQueue{}
	push := func(item livewireItem) error {
		if queueLimit > 0 {
			if len(queue) >= queueLimit {
				return fmt.Errorf("%w: livewire queue", ErrLivewire)
			}
			if len(queue) == cap(queue) {
				capacity := min(queueLimit, max(16, cap(queue)*2))
				next := make(livewireQueue, len(queue), capacity)
				copy(next, queue)
				queue = next
			}
		}
		heap.Push(&queue, item)
		return nil
	}
	for d := 0; d < directions; d++ {
		t.distance[int(start)+d] = 0
		if err := push(livewireItem{state: start + int32(d)}); err != nil {
			return nil, err
		}
	}
	done := make([]bool, states)
	settled := 0
	for pops := 0; queue.Len() > 0; pops++ {
		if pops%4096 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		item := heap.Pop(&queue).(livewireItem)
		state := int(item.state)
		if done[state] {
			continue
		}
		done[state] = true
		settled++
		if settled%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		pixel, incoming := state/directions, state%directions
		x, y := pixel%c.Columns, pixel/c.Columns
		for k, s := range livewireSteps {
			nx, ny := x+s[0], y+s[1]
			if nx < 0 || ny < 0 || nx >= c.Columns || ny >= c.Rows {
				continue
			}
			image := c.cost[ny*c.Columns+nx]
			if math.IsInf(image, 1) {
				continue
			}
			step := c.opts.ImageWeight * image
			if c.opts.LengthWeight > 0 {
				step += c.opts.LengthWeight * steps[k]
			}
			next := (ny*c.Columns + nx) * directions
			if directions == 8 {
				next += k
				if t.parent[state] >= 0 {
					// Turning angle between the incoming and outgoing steps.
					turn := math.Abs(float64(k - incoming))
					if turn > 4 {
						turn = 8 - turn
					}
					step += c.opts.CurvatureWeight * (1 - math.Cos(turn*math.Pi/4))
				}
			}
			if done[next] {
				continue
			}
			d := item.distance + step
			if c.maxBytes > 0 && (math.IsNaN(d) || math.IsInf(d, 0)) {
				return nil, fmt.Errorf("%w: path distance overflow", ErrLivewire)
			}
			if d < t.distance[next] {
				t.distance[next] = d
				t.parent[next] = int32(state)
				if err := push(livewireItem{state: int32(next), distance: d}); err != nil {
					return nil, err
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return t, nil
}

// Anchor is the tree's source pixel.
func (t *LivewireTree) Anchor() [2]int { return t.anchor }

// PathTo returns the least-cost path from the anchor to target as pixel
// centres (x, y), anchor first; nil when target is unreachable.
func (t *LivewireTree) PathTo(target [2]int) [][2]float64 {
	c := t.costs
	if target[0] < 0 || target[1] < 0 || target[0] >= c.Columns || target[1] >= c.Rows {
		return nil
	}
	pixel := target[1]*c.Columns + target[0]
	best, bestState := math.Inf(1), -1
	for d := 0; d < t.states; d++ {
		if s := pixel*t.states + d; t.distance[s] < best {
			best, bestState = t.distance[s], s
		}
	}
	if bestState < 0 {
		return nil
	}
	var reversed [][2]float64
	for s := bestState; s >= 0; s = int(t.parent[s]) {
		p := s / t.states
		reversed = append(reversed, [2]float64{float64(p % c.Columns), float64(p / c.Columns)})
		if t.parent[s] < 0 {
			break
		}
	}
	path := make([][2]float64, len(reversed))
	for i, p := range reversed {
		path[len(reversed)-1-i] = p
	}
	return path
}

// Distance is the path cost from the anchor to target (+Inf when
// unreachable).
func (t *LivewireTree) Distance(target [2]int) float64 {
	c := t.costs
	if target[0] < 0 || target[1] < 0 || target[0] >= c.Columns || target[1] >= c.Rows {
		return math.Inf(1)
	}
	best := math.Inf(1)
	pixel := target[1]*c.Columns + target[0]
	for d := 0; d < t.states; d++ {
		best = math.Min(best, t.distance[pixel*t.states+d])
	}
	return best
}

// LivewirePath is the least-cost path between two pixels (one search).
func (c *LivewireCostMap) LivewirePath(ctx context.Context, from, to [2]int) ([][2]float64, error) {
	tree, err := c.Tree(ctx, from)
	if err != nil {
		return nil, err
	}
	path := tree.PathTo(to)
	if path == nil {
		return nil, fmt.Errorf("%w: %v is unreachable from %v", ErrLivewire, to, from)
	}
	return path, nil
}
